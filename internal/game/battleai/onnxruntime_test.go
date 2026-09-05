//go:build onnxruntime && cgo

package battleai

import (
	"math"
	"os"
	"testing"
)

func loadONNXRuntimeTestRunner(tb testing.TB, config ONNXRuntimeConfig) *ONNXRuntimeRunner {
	tb.Helper()
	modelPath := os.Getenv("QQTANG_TEST_ONNX_MODEL")
	metadataPath := os.Getenv("QQTANG_TEST_ONNX_METADATA")
	runtimePath := os.Getenv("QQTANG_TEST_ONNX_RUNTIME")
	if modelPath == "" || metadataPath == "" || runtimePath == "" {
		tb.Skip("ONNX Runtime integration paths are not configured")
	}
	runner, err := LoadONNXRuntimeRunner(modelPath, metadataPath, runtimePath, config)
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() {
		if err := runner.Close(); err != nil {
			tb.Errorf("close ONNX Runtime runner: %v", err)
		}
	})
	return runner
}

func TestONNXRuntimeRunnerLoadsContractAndRuns(t *testing.T) {
	runner := loadONNXRuntimeTestRunner(t, ONNXRuntimeConfig{IntraOpThreads: 1, InterOpThreads: 1})
	contract := runner.Contract()
	spatial := make([]float32, contract.Channels*contract.Height*contract.Width)
	scalars := make([]float32, contract.Scalars)
	legal := make([]uint8, contract.Actions)
	for index := range legal {
		legal[index] = 1
	}
	legal[len(legal)-1] = 0
	var (
		logits []float32
		err    error
	)
	if contract.RecurrentHiddenSize > 0 {
		memory := make([]float32, contract.RecurrentHiddenSize)
		var nextMemory []float32
		logits, nextMemory, err = runner.RunActorRecurrent(
			spatial, scalars, legal, memory, true,
		)
		if err == nil && len(nextMemory) != contract.RecurrentHiddenSize {
			t.Fatalf("ONNX actor returned %d recurrent values, want %d", len(nextMemory), contract.RecurrentHiddenSize)
		}
		if err == nil {
			_, continued, continueErr := runner.RunActorRecurrent(
				spatial, scalars, legal, nextMemory, false,
			)
			if continueErr != nil {
				t.Fatal(continueErr)
			}
			_, resetAgain, resetErr := runner.RunActorRecurrent(
				spatial, scalars, legal, nextMemory, true,
			)
			if resetErr != nil {
				t.Fatal(resetErr)
			}
			continuedChanged := false
			for index := range nextMemory {
				if math.Abs(float64(resetAgain[index]-nextMemory[index])) > 1e-5 {
					t.Fatalf("recurrent reset changed zero-state result at %d: %g/%g", index, resetAgain[index], nextMemory[index])
				}
				if math.Abs(float64(continued[index]-nextMemory[index])) > 1e-5 {
					continuedChanged = true
				}
			}
			if !continuedChanged {
				t.Fatal("recurrent ONNX actor did not advance caller-owned memory")
			}
		}
	} else {
		logits, err = runner.RunActor(spatial, scalars, legal)
	}
	if err != nil {
		t.Fatal(err)
	}
	if len(logits) != contract.Actions {
		t.Fatalf("ONNX actor returned %d logits, want %d", len(logits), contract.Actions)
	}
	if logits[len(logits)-1] > -math.MaxFloat32/2 {
		t.Fatalf("ONNX actor did not mask an illegal action: %g", logits[len(logits)-1])
	}
}

func TestDeploymentPolicyUsesRequestedONNXRuntimeBackend(t *testing.T) {
	modelPath := os.Getenv("QQTANG_TEST_ONNX_MODEL")
	metadataPath := os.Getenv("QQTANG_TEST_ONNX_METADATA")
	runtimePath := os.Getenv("QQTANG_TEST_ONNX_RUNTIME")
	if modelPath == "" || metadataPath == "" || runtimePath == "" {
		t.Skip("ONNX Runtime integration paths are not configured")
	}
	loaded, err := LoadDeploymentPolicy(DeploymentPolicyConfig{
		Backend:           DeploymentBackendONNXRuntime,
		ModelPath:         modelPath,
		MetadataPath:      metadataPath,
		SharedLibraryPath: runtimePath,
	})
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Backend != DeploymentBackendONNXRuntime || loaded.Closer == nil ||
		loaded.Contract.ModelVariant == "" {
		t.Fatalf("loaded deployment = %+v", loaded)
	}
	if err := loaded.Closer.Close(); err != nil {
		t.Fatal(err)
	}
}

func BenchmarkONNXRuntimeActor(b *testing.B) {
	for _, test := range []struct {
		name   string
		config ONNXRuntimeConfig
	}{
		{"default_threads", ONNXRuntimeConfig{}},
		{"one_thread", ONNXRuntimeConfig{IntraOpThreads: 1, InterOpThreads: 1}},
		{"two_threads", ONNXRuntimeConfig{IntraOpThreads: 2, InterOpThreads: 1}},
	} {
		b.Run(test.name, func(b *testing.B) {
			runner := loadONNXRuntimeTestRunner(b, test.config)
			contract := runner.Contract()
			spatial := make([]float32, contract.Channels*contract.Height*contract.Width)
			scalars := make([]float32, contract.Scalars)
			legal := make([]uint8, contract.Actions)
			for index := range legal {
				legal[index] = 1
			}
			memory := make([]float32, contract.RecurrentHiddenSize)
			b.ReportAllocs()
			b.ResetTimer()
			for iteration := 0; iteration < b.N; iteration++ {
				if contract.RecurrentHiddenSize > 0 {
					_, next, err := runner.RunActorRecurrent(spatial, scalars, legal, memory, iteration == 0)
					if err != nil {
						b.Fatal(err)
					}
					memory = next
				} else if _, err := runner.RunActor(spatial, scalars, legal); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
