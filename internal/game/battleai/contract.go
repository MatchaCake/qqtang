// Package battleai connects learned policies to the authoritative battle
// engine without moving combat rules into the inference implementation.
package battleai

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"qqtang/internal/game/battleengine"
	"qqtang/internal/game/battleenv"
)

const (
	ONNXActorFormat   = "qqtang-decentralized-actor-onnx"
	NativeActorFormat = "qqtang-native-actor"
)

// Contract describes the versioned actor-only ONNX boundary. The critic is a
// training concern and is intentionally absent from server inference.
type Contract struct {
	Format                   string  `json:"format"`
	ContractVersion          uint16  `json:"contract_version"`
	ModelArchitectureVersion uint16  `json:"model_architecture_version"`
	ModelVariant             string  `json:"model_variant"`
	TensorVersion            uint16  `json:"tensor_version"`
	Channels                 int     `json:"channels"`
	Scalars                  int     `json:"scalars"`
	Actions                  int     `json:"actions"`
	Height                   int     `json:"height"`
	Width                    int     `json:"width"`
	BatchDynamic             bool    `json:"batch_dynamic"`
	RecurrentHiddenSize      int     `json:"recurrent_hidden_size,omitempty"`
	RecurrentReset           string  `json:"recurrent_reset,omitempty"`
	ONNXSHA256               string  `json:"onnx_sha256,omitempty"`
	MaximumParityError       float64 `json:"maximum_parity_error"`
	MeanParityError          float64 `json:"mean_parity_error"`
}

// LoadContract validates the inference boundary but deliberately does not pin
// the model bytes. Operators can replace an actor and its sidecar without
// rebuilding the server; ONNX Runtime validates the actual graph and tensors.
func LoadContract(modelPath, metadataPath string) (Contract, error) {
	metadata, err := os.ReadFile(metadataPath)
	if err != nil {
		return Contract{}, fmt.Errorf("read AI metadata %s: %w", filepath.Clean(metadataPath), err)
	}
	var contract Contract
	if err := json.Unmarshal(metadata, &contract); err != nil {
		return Contract{}, fmt.Errorf("decode AI metadata: %w", err)
	}
	if err := contract.Validate(); err != nil {
		return Contract{}, err
	}
	if contract.Format != ONNXActorFormat {
		return Contract{}, fmt.Errorf("AI metadata format %q is not ONNX", contract.Format)
	}
	model, err := os.Stat(modelPath)
	if err != nil {
		return Contract{}, fmt.Errorf("read AI model %s: %w", filepath.Clean(modelPath), err)
	}
	if !model.Mode().IsRegular() || model.Size() == 0 {
		return Contract{}, fmt.Errorf("AI model %s is not a non-empty regular file", filepath.Clean(modelPath))
	}
	return contract, nil
}

func (contract Contract) Validate() error {
	switch {
	case contract.Format != ONNXActorFormat && contract.Format != NativeActorFormat:
		return fmt.Errorf("unknown AI model format %q", contract.Format)
	case contract.ContractVersion != 1 && contract.ContractVersion != 2:
		return fmt.Errorf("unsupported AI contract version %d", contract.ContractVersion)
	case contract.ContractVersion == 1 && contract.RecurrentHiddenSize != 0:
		return fmt.Errorf("AI contract version 1 cannot declare recurrent memory")
	case contract.ContractVersion == 2 && contract.RecurrentHiddenSize <= 0:
		return fmt.Errorf("AI contract version 2 requires recurrent memory")
	case contract.Channels != battleenv.SpatialChannels:
		return fmt.Errorf("AI spatial channels %d, server expects %d", contract.Channels, battleenv.SpatialChannels)
	case contract.Scalars != battleenv.ScalarFeatures:
		return fmt.Errorf("AI scalar features %d, server expects %d", contract.Scalars, battleenv.ScalarFeatures)
	case contract.Actions != int(battleengine.DiscreteActionCount):
		return fmt.Errorf("AI actions %d, server expects %d", contract.Actions, battleengine.DiscreteActionCount)
	case contract.Height <= 0 || contract.Width <= 0:
		return fmt.Errorf("AI tensor dimensions must be positive")
	}
	return nil
}
