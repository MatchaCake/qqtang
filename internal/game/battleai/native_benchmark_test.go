package battleai

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func benchmarkNativeActorInput(runner *NativeRunner) ([]float32, []float32, []uint8) {
	plane := runner.Height * runner.Width
	spatial := make([]float32, runner.Channels*plane)
	for index := 0; index < plane; index++ {
		spatial[index] = 1
	}
	spatial[7*plane+2*runner.Width+3] = 1
	spatial[9*plane+3*runner.Width+5] = 1
	spatial[11*plane+7*runner.Width+9] = 1
	if runner.Channels >= 82 {
		for index := 0; index < plane; index++ {
			spatial[80*plane+index] = 1
		}
	}
	scalars := make([]float32, runner.Scalars)
	legal := make([]uint8, runner.Actions)
	for index := range legal {
		legal[index] = 1
	}
	return spatial, scalars, legal
}

// BenchmarkNativeDeploymentModels guards the live 100 ms decision budget.
// It deliberately times only dependency-free Go actor inference; observation
// encoding and tactical search have separate engine coverage.
func BenchmarkNativeDeploymentModels(b *testing.B) {
	models := []struct {
		name string
		path string
	}{
		{
			name: "legacy-native",
			path: filepath.Join("..", "..", "..", "runtime", "model-backups", "qqtang-rule1-selected-v2.qtai"),
		},
		{
			name: "v8-context-champion",
			path: filepath.Join("..", "..", "..", "ai-training", "eval-results", "20260828-v8-deployed.qtai"),
		},
	}
	if candidate := os.Getenv("QQTANG_AI_CANDIDATE_MODEL"); candidate != "" {
		models = append(models, struct {
			name string
			path string
		}{name: "candidate", path: candidate})
	}
	for _, model := range models {
		model := model
		b.Run(model.name, func(b *testing.B) {
			if _, err := os.Stat(model.path); err != nil {
				b.Skipf("deployment model unavailable: %v", err)
			}
			runner, err := LoadNativeRunner(model.path)
			if err != nil {
				b.Fatal(err)
			}
			spatial, scalars, legal := benchmarkNativeActorInput(runner)
			b.ReportAllocs()
			b.ResetTimer()
			for iteration := 0; iteration < b.N; iteration++ {
				if _, err := runner.RunActor(spatial, scalars, legal); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkNativeFullRoomConcurrent measures the live worst case: all seven
// virtual actors become due on one 20 ms danger frame. The production runtime
// evaluates their independent policies concurrently against immutable weights.
func BenchmarkNativeFullRoomConcurrent(b *testing.B) {
	models := []struct {
		name string
		path string
	}{
		{
			name: "legacy-native",
			path: filepath.Join("..", "..", "..", "runtime", "model-backups", "qqtang-rule1-selected-v2.qtai"),
		},
		{
			name: "v8-context-champion",
			path: filepath.Join("..", "..", "..", "ai-training", "eval-results", "20260828-v8-deployed.qtai"),
		},
	}
	if candidate := os.Getenv("QQTANG_AI_CANDIDATE_MODEL"); candidate != "" {
		models = append(models, struct {
			name string
			path string
		}{name: "candidate", path: candidate})
	}
	for _, model := range models {
		model := model
		b.Run(model.name, func(b *testing.B) {
			if _, err := os.Stat(model.path); err != nil {
				b.Skipf("deployment model unavailable: %v", err)
			}
			runner, err := LoadNativeRunner(model.path)
			if err != nil {
				b.Fatal(err)
			}
			spatial, scalars, legal := benchmarkNativeActorInput(runner)
			const actors = 7
			b.ReportMetric(actors, "actors/room")
			b.ReportAllocs()
			b.ResetTimer()
			for iteration := 0; iteration < b.N; iteration++ {
				var wait sync.WaitGroup
				wait.Add(actors)
				for actor := 0; actor < actors; actor++ {
					go func() {
						defer wait.Done()
						if _, runErr := runner.RunActor(spatial, scalars, legal); runErr != nil {
							b.Error(runErr)
						}
					}()
				}
				wait.Wait()
			}
		})
	}
}
