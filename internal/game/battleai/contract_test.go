package battleai

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"qqtang/internal/game/battleenv"
)

func TestLoadContractDoesNotPinReplaceableONNXBytes(t *testing.T) {
	directory := t.TempDir()
	modelPath := filepath.Join(directory, "actor.onnx")
	metadataPath := filepath.Join(directory, "actor.onnx.json")
	if err := os.WriteFile(modelPath, []byte("replacement model bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	metadata := fmt.Sprintf(`{"format":"qqtang-decentralized-actor-onnx","contract_version":1,"model_architecture_version":2,"model_variant":"routed-multiplayer-context-v1","tensor_version":%d,"channels":%d,"scalars":%d,"actions":45,"height":13,"width":15,"batch_dynamic":true,"onnx_sha256":"stale provenance only"}`,
		battleenv.TensorSchemaVersion, battleenv.SpatialChannels, battleenv.ScalarFeatures,
	)
	if err := os.WriteFile(metadataPath, []byte(metadata), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadContract(modelPath, metadataPath); err != nil {
		t.Fatal(err)
	}
}

func TestRecurrentContractRequiresVersionTwoAndPositiveMemory(t *testing.T) {
	base := Contract{
		Format: ONNXActorFormat, ContractVersion: 2,
		TensorVersion: battleenv.TensorSchemaVersion,
		Channels:      battleenv.SpatialChannels, Scalars: battleenv.ScalarFeatures, Actions: 45,
		Height: 13, Width: 15, RecurrentHiddenSize: 256,
	}
	if err := base.Validate(); err != nil {
		t.Fatal(err)
	}
	invalidVersion := base
	invalidVersion.ContractVersion = 1
	if err := invalidVersion.Validate(); err == nil {
		t.Fatal("version-one contract accepted recurrent memory")
	}
	missingMemory := base
	missingMemory.RecurrentHiddenSize = 0
	if err := missingMemory.Validate(); err == nil {
		t.Fatal("version-two contract accepted missing recurrent memory")
	}
}

func TestContractTreatsTensorVersionAsModelProvenance(t *testing.T) {
	contract := Contract{
		Format: ONNXActorFormat, ContractVersion: 2,
		TensorVersion: 65_535,
		Channels:      battleenv.SpatialChannels, Scalars: battleenv.ScalarFeatures, Actions: 45,
		Height: 13, Width: 15, RecurrentHiddenSize: 256,
	}
	if err := contract.Validate(); err != nil {
		t.Fatalf("informational tensor version rejected: %v", err)
	}
}
