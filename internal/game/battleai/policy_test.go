package battleai

import (
	"fmt"
	"testing"

	"qqtang/internal/game/battleengine"
	"qqtang/internal/game/battleenv"
)

type fixedRunner struct {
	logits []float32
}

type recurrentTestRunner struct{}

func (recurrentTestRunner) RunActor(_ []float32, _ []float32, _ []uint8) ([]float32, error) {
	return nil, fmt.Errorf("stateless inference was used for a recurrent actor")
}

func (recurrentTestRunner) RunActorRecurrent(
	_ []float32,
	_ []float32,
	_ []uint8,
	memory []float32,
	reset bool,
) ([]float32, []float32, error) {
	value := float32(0)
	if !reset && len(memory) != 0 {
		value = memory[0]
	}
	logits := make([]float32, battleengine.DiscreteActionCount)
	if value == 0 {
		logits[battleengine.ActionMoveRight] = 10
	} else {
		logits[battleengine.ActionMoveLeft] = 10
	}
	next := make([]float32, len(memory))
	if len(next) != 0 {
		next[0] = value + 1
	}
	return logits, next, nil
}

func (runner fixedRunner) RunActor(_ []float32, _ []float32, _ []uint8) ([]float32, error) {
	return append([]float32(nil), runner.logits...), nil
}

func TestNeuralCandidatesUsesStableLegalActionIDs(t *testing.T) {
	grid := battleengine.Grid{Width: 5, Height: 3, Cells: make([]battleengine.Tile, 15)}
	for index := range grid.Cells {
		grid.Cells[index].FlamePassable = true
	}
	engine, err := battleengine.New(battleengine.Config{
		Seed: 1,
		Grid: grid,
		Rules: battleengine.Rules{
			TickMS: 20, RoundDurationMS: 240_000, BombFuseMS: 3_000,
			FlameDurationMS: 600, TrapDurationMS: 6_000, ActorHalfSizePixels: 10,
		},
		Participants: []battleengine.Participant{
			{PlayerID: 1, TeamID: 1, Source: battleengine.ParticipantVirtualAI, Spawn: battleengine.Cell{Row: 1, Col: 1}, SpeedPixelsPerSecond: 80, BombCapacity: 1, BombPower: 1},
			{PlayerID: 2, TeamID: 2, Source: battleengine.ParticipantHuman, Spawn: battleengine.Cell{Row: 1, Col: 3}, SpeedPixelsPerSecond: 80, BombCapacity: 1, BombPower: 1},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	observation, err := engine.Observation(1)
	if err != nil {
		t.Fatal(err)
	}
	legal, err := engine.LegalActions(1)
	if err != nil {
		t.Fatal(err)
	}
	logits := make([]float32, battleengine.DiscreteActionCount)
	logits[battleengine.ActionMoveRight] = 10
	policy := NeuralCandidates{
		Contract: Contract{
			Format: ONNXActorFormat, ContractVersion: 1, ModelArchitectureVersion: 2,
			TensorVersion: battleenv.TensorSchemaVersion,
			Channels:      battleenv.SpatialChannels, Scalars: battleenv.ScalarFeatures,
			Actions: int(battleengine.DiscreteActionCount), Height: 13, Width: 15,
			ONNXSHA256: "0000000000000000000000000000000000000000000000000000000000000000",
		},
		Runner: fixedRunner{logits: logits},
	}
	candidates, err := policy.CandidateActionsWithSnapshot(engine.Clone(), observation, legal, 2)
	if err != nil {
		t.Fatal(err)
	}
	id, ok := candidates[0].Action.ID()
	if !ok || id != battleengine.ActionMoveRight {
		t.Fatalf("top candidate ID = %d/%t, want move right", id, ok)
	}
}

func TestActorPolicyFactoryIsolatesRecurrentMemoryPerVirtualActor(t *testing.T) {
	grid := battleengine.Grid{Width: 5, Height: 3, Cells: make([]battleengine.Tile, 15)}
	for index := range grid.Cells {
		grid.Cells[index].FlamePassable = true
	}
	engine, err := battleengine.New(battleengine.Config{
		Seed: 2,
		Grid: grid,
		Rules: battleengine.Rules{
			TickMS: 20, RoundDurationMS: 240_000, BombFuseMS: 3_000,
			FlameDurationMS: 600, TrapDurationMS: 6_000, ActorHalfSizePixels: 10,
		},
		Participants: []battleengine.Participant{
			{PlayerID: 1, TeamID: 1, Source: battleengine.ParticipantVirtualAI, Spawn: battleengine.Cell{Row: 1, Col: 1}, SpeedPixelsPerSecond: 80, BombCapacity: 1, BombPower: 1},
			{PlayerID: 2, TeamID: 2, Source: battleengine.ParticipantHuman, Spawn: battleengine.Cell{Row: 1, Col: 3}, SpeedPixelsPerSecond: 80, BombCapacity: 1, BombPower: 1},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	observation, err := engine.Observation(1)
	if err != nil {
		t.Fatal(err)
	}
	legal, err := engine.LegalActions(1)
	if err != nil {
		t.Fatal(err)
	}
	contract := Contract{
		Format: ONNXActorFormat, ContractVersion: 2, ModelArchitectureVersion: 2,
		TensorVersion: battleenv.TensorSchemaVersion,
		Channels:      battleenv.SpatialChannels, Scalars: battleenv.ScalarFeatures,
		Actions: int(battleengine.DiscreteActionCount), Height: 13, Width: 15,
		RecurrentHiddenSize: 4,
	}
	templatePolicy, err := buildActorPolicy(contract, recurrentTestRunner{}, NativePolicyConfig{})
	if err != nil {
		t.Fatal(err)
	}
	factory, ok := templatePolicy.(interface {
		NewActorPolicy() battleengine.Policy
	})
	if !ok {
		t.Fatalf("deployment policy %T does not create actor-local policies", templatePolicy)
	}
	left := factory.NewActorPolicy().(battleengine.SnapshotPolicy)
	right := factory.NewActorPolicy().(battleengine.SnapshotPolicy)
	choose := func(policy battleengine.SnapshotPolicy) battleengine.ActionID {
		action, chooseErr := policy.ChooseActionWithSnapshot(engine.Clone(), observation, legal)
		if chooseErr != nil {
			t.Fatal(chooseErr)
		}
		id, valid := action.ID()
		if !valid {
			t.Fatalf("policy returned action without an ID: %+v", action)
		}
		return id
	}
	if got := choose(left); got != battleengine.ActionMoveRight {
		t.Fatalf("left first decision = %d, want right", got)
	}
	if got := choose(left); got != battleengine.ActionMoveLeft {
		t.Fatalf("left remembered decision = %d, want left", got)
	}
	if got := choose(right); got != battleengine.ActionMoveRight {
		t.Fatalf("right inherited left memory: decision = %d", got)
	}
}
