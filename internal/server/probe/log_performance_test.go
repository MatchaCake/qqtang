package probe

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestServerLogPreservesConcurrentJSONRecords(t *testing.T) {
	var output bytes.Buffer
	server := &Server{logWriter: &output}
	var workers sync.WaitGroup
	for i := 0; i < 8; i++ {
		workers.Go(func() {
			for n := 0; n < 100; n++ {
				server.log(logEvent{Level: "debug", Event: "packet_sent", Result: "line\nwith quote\" and 中文"})
			}
		})
	}
	workers.Wait()
	lines := bytes.Split(bytes.TrimSpace(output.Bytes()), []byte{'\n'})
	if len(lines) != 800 {
		t.Fatalf("log records = %d, want 800", len(lines))
	}
	for _, line := range lines {
		var event logEvent
		if err := json.Unmarshal(line, &event); err != nil {
			t.Fatal(err)
		}
		if event.Time.IsZero() || event.Result != "line\nwith quote\" and 中文" {
			t.Fatalf("incomplete record: %+v", event)
		}
	}
}

// Match the launcher setup: stdout redirected to a file, plus server.jsonl.
// The old variant provides a direct allocation/IO comparison on the same disk.
func BenchmarkServerLogFiles(b *testing.B) {
	for _, legacy := range []bool{true, false} {
		name := "direct_encoder"
		if legacy {
			name = "previous_per_line_buffer"
		}
		b.Run(name, func(b *testing.B) {
			root := b.TempDir()
			outputs := make([]io.Writer, 0, 2)
			for _, fileName := range []string{"stdout.log", "server.jsonl"} {
				file, err := os.Create(filepath.Join(root, fileName))
				if err != nil {
					b.Fatal(err)
				}
				b.Cleanup(func() { _ = file.Close() })
				outputs = append(outputs, file)
			}
			server := &Server{logWriter: io.MultiWriter(outputs...)}
			event := logEvent{Level: "debug", Event: "competitive_ai_bomb_placed", RoomID: "7", Result: "game_123_player_20001_cell_3_5_reach_8_wire_power_7"}
			b.ReportAllocs()
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					if legacy {
						entry := event
						entry.Time = time.Now().UTC()
						server.logMu.Lock()
						writer := bufio.NewWriter(server.logWriter)
						_ = json.NewEncoder(writer).Encode(entry)
						_ = writer.Flush()
						server.logMu.Unlock()
					} else {
						server.log(event)
					}
				}
			})
		})
	}
}
