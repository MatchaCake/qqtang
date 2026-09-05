package probe

import (
	"fmt"
	"io"
	"os"
	"sort"
	"sync"
	"testing"
	"time"

	"qqtang/internal/game/battleai"
	"qqtang/internal/game/battleengine"
	"qqtang/internal/game/mapdata"
	"qqtang/internal/game/match"
	"qqtang/internal/protocol/game"
)

// This exercises real map geometry, observations, shared-session ONNX inference,
// world steps and movement projection. There are no native peers or socket/log
// writes: the result is a compute stress measurement, not a server capacity SLA.
func BenchmarkCompetitiveAIMultiRoomWorldStep(b *testing.B) {
	modelPath, clientRoot := os.Getenv("QQTANG_TEST_ONNX_MODEL"), os.Getenv("QQTANG_TEST_CLIENT_ROOT")
	if modelPath == "" || clientRoot == "" {
		b.Skip("set QQTANG_TEST_ONNX_MODEL, QQTANG_TEST_ONNX_RUNTIME and QQTANG_TEST_CLIENT_ROOT")
	}
	catalog, err := mapdata.LoadCatalog(clientRoot)
	if err != nil {
		b.Fatal(err)
	}
	selectedMap, ok := catalog.CompetitiveMapMetadata(905)
	if !ok {
		b.Fatal("native map 905 is absent")
	}
	loaded, err := battleai.LoadDeploymentPolicy(battleai.DeploymentPolicyConfig{
		Backend: battleai.DeploymentBackendONNXRuntime, ModelPath: modelPath,
		MetadataPath: os.Getenv("QQTANG_TEST_ONNX_METADATA"), SharedLibraryPath: os.Getenv("QQTANG_TEST_ONNX_RUNTIME"),
		IntraOpThreads: 1, InterOpThreads: 1,
		NativePolicyConfig: battleai.NativePolicyConfig{
			DangerHorizonMS: 3500, DecisionMS: 100,
			// Optional benchmark comparison using the production search settings.
			EnableSearch: os.Getenv("QQTANG_TEST_AI_SEARCH") == "1",
			Search: battleengine.SearchConfig{
				TopK: 4, HorizonMS: battleengine.NativeBombFuseMS + 200, DangerHorizonMS: 3500,
				PriorWeight: 0.35, EliminationValue: 100, TrapValue: 12,
			},
		},
	})
	if err != nil {
		b.Fatal(err)
	}
	if loaded.Closer != nil {
		b.Cleanup(func() { _ = loaded.Closer.Close() })
	}
	participants := []match.CompetitiveParticipant{{PlayerID: 1, RoleID: 9, TeamID: 1, Source: match.CompetitiveParticipantHuman}}
	for i, role := range []byte{10, 11, 7, 7, 11, 19, 18} {
		participants = append(participants, match.CompetitiveParticipant{
			PlayerID: uint16(20001 + i), RoleID: role, TeamID: byte(i + 2), Source: match.CompetitiveParticipantVirtualAI,
		})
	}
	for _, roomCount := range []int{1, 4, 14} {
		b.Run(fmt.Sprintf("rooms_%02d_ai_%02d", roomCount, roomCount*7), func(b *testing.B) {
			server := &Server{
				competitiveAIRuntime: make(map[uint32]*liveCompetitiveAIRuntime),
				competitiveBattles:   make(map[uint32]*match.CompetitiveBattle), logWriter: io.Discard,
			}
			rooms := make([]*liveCompetitiveAIRuntime, roomCount)
			for i := range rooms {
				gameID := uint32(100 + i)
				rooms[i], err = newLiveCompetitiveAIRuntime(uint16(i+1), selectedMap,
					game.GameBeginData{GameID: gameID, SpawnSeed: 0x2D427A82 + uint32(i), ItemSeed: 0x59922938 + uint32(i)},
					participants, true, loaded.Policy, 100)
				if err != nil {
					b.Fatal(err)
				}
				battle, battleErr := match.NewCompetitiveBattle(gameID, 2, 1, participants)
				if battleErr != nil {
					b.Fatal(battleErr)
				}
				server.competitiveAIRuntime[gameID], server.competitiveBattles[gameID] = rooms[i], battle
			}
			durations := make([]time.Duration, 0, b.N)
			jobs := make([]chan uint32, roomCount)
			errors := make([]error, roomCount)
			var wait sync.WaitGroup
			for i, room := range rooms {
				jobs[i] = make(chan uint32)
				go func(i int, room *liveCompetitiveAIRuntime) {
					for target := range jobs[i] {
						room.mu.Lock()
						errors[i] = room.advanceToLocked(server, target, 1)
						room.mu.Unlock()
						wait.Done()
					}
				}(i, room)
			}
			defer func() {
				for _, job := range jobs {
					close(job)
				}
			}()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				started := time.Now()
				wait.Add(roomCount)
				for _, job := range jobs {
					job <- 3000 + uint32(i+1)*competitiveAIWorldTickMS
				}
				wait.Wait()
				durations = append(durations, time.Since(started))
				for _, stepErr := range errors {
					if stepErr != nil {
						b.Fatal(stepErr)
					}
				}
			}
			b.StopTimer()
			sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
			if len(durations) > 0 {
				b.ReportMetric(float64(durations[(len(durations)-1)*95/100].Nanoseconds())/1e6, "p95-ms/all-rooms")
				b.ReportMetric(float64(durations[len(durations)-1].Nanoseconds())/1e6, "max-ms/all-rooms")
				over := len(durations) - sort.Search(len(durations), func(i int) bool { return durations[i] > 20*time.Millisecond })
				b.ReportMetric(float64(over), "over-20ms-rounds")
			}
			b.ReportMetric(float64(roomCount*7), "AI")
		})
	}
}
