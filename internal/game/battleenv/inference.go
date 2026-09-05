package battleenv

import (
	"fmt"

	"qqtang/internal/game/battleengine"
)

// EncodedActor is the single-participant deployment form of TensorBatch.
// Its feature semantics are exactly those used by vectorized training.
type EncodedActor struct {
	TensorVersion uint16
	Channels      int
	Scalars       int
	Actions       int
	Height        int
	Width         int
	Spatial       []float32
	ScalarValues  []float32
	Legal         []uint8
	Active        bool
}

// EncodeActor converts one visibility-bounded observation with a public
// rule-derived danger timeline into the exact training tensor schema.
func EncodeActor(
	snapshot *battleengine.Engine,
	observation battleengine.Observation,
	danger battleengine.DangerTimeline,
	legal battleengine.ActionMask,
	height int,
	width int,
) (EncodedActor, error) {
	return EncodeActorAtDecision(
		snapshot, observation, danger, legal, height, width, 0,
	)
}

// EncodeActorAtDecision keeps per-direction consequence features aligned with
// the interval for which deployment holds one learned direction.
func EncodeActorAtDecision(
	snapshot *battleengine.Engine,
	observation battleengine.Observation,
	danger battleengine.DangerTimeline,
	legal battleengine.ActionMask,
	height int,
	width int,
	decisionMS uint32,
) (EncodedActor, error) {
	if snapshot == nil {
		return EncodedActor{}, fmt.Errorf("inference battle snapshot is nil")
	}
	if height < int(observation.Grid.Height) || width < int(observation.Grid.Width) {
		return EncodedActor{}, fmt.Errorf(
			"inference tensor %dx%d is smaller than map %dx%d",
			width, height, observation.Grid.Width, observation.Grid.Height,
		)
	}
	if height <= 0 || width <= 0 {
		return EncodedActor{}, fmt.Errorf("inference tensor dimensions must be positive")
	}
	tensors := newTensorBatch(1, 1, height, width)
	var consequences battleengine.TacticalConsequences
	var err error
	if decisionMS == 0 {
		consequences, err = snapshot.TacticalConsequencesForPlayer(
			observation.PlayerID, danger,
		)
	} else {
		consequences, err = snapshot.TacticalConsequencesForPlayerAtDecision(
			observation.PlayerID, danger, decisionMS,
		)
	}
	if err != nil {
		return EncodedActor{}, fmt.Errorf("inference tactical consequences: %w", err)
	}
	if err := encodeActor(&tensors, 0, 0, observation, danger, consequences, legal); err != nil {
		return EncodedActor{}, err
	}
	return EncodedActor{
		TensorVersion: tensors.SchemaVersion,
		Channels:      SpatialChannels, Scalars: ScalarFeatures,
		Actions: int(battleengine.DiscreteActionCount), Height: height, Width: width,
		Spatial: tensors.Spatial, ScalarValues: tensors.Scalars, Legal: tensors.Legal,
		Active: tensors.Active[0] != 0,
	}, nil
}
