package battleenv

import (
	"fmt"
	"reflect"
	"testing"

	"qqtang/internal/game/battleengine"
)

func TestTrainingAndDeploymentInputsMatchForEightActors(t *testing.T) {
	for _, teams := range []int{2, 8} {
		t.Run(fmt.Sprintf("teams_%d", teams), func(t *testing.T) {
			config := testBatchConfig(1)
			config.Maps[0], config.ParticipantCount, config.TeamCount = benchmarkTrainingMap(), 8, teams
			batch, err := NewBatch(config)
			if err != nil {
				t.Fatal(err)
			}
			for step := 0; step <= 8; step++ {
				batched, err := batch.Observe()
				if err != nil {
					t.Fatal(err)
				}
				episode := &batch.episodes[0]
				for actorIndex, playerID := range episode.playerIDs {
					// Deployment starts from an observer-bounded snapshot; training
					// batches public world features across observers.
					snapshot, err := episode.engine.PolicySnapshot(playerID)
					if err != nil {
						t.Fatal(err)
					}
					observation, err := episode.engine.Observation(playerID)
					if err != nil {
						t.Fatal(err)
					}
					danger, err := snapshot.DangerTimeline(config.DangerHorizonMS)
					if err != nil {
						t.Fatal(err)
					}
					legal, err := snapshot.LegalActionMask(playerID)
					if err != nil {
						t.Fatal(err)
					}
					encoded, err := EncodeActorAtDecision(snapshot, observation, danger, legal, batch.maxHeight, batch.maxWidth, config.DecisionMS)
					if err != nil {
						t.Fatal(err)
					}
					spatial := SpatialChannels * batch.maxHeight * batch.maxWidth
					actions := int(battleengine.DiscreteActionCount)
					if !reflect.DeepEqual(encoded.Spatial, batched.Spatial[actorIndex*spatial:(actorIndex+1)*spatial]) ||
						!reflect.DeepEqual(encoded.ScalarValues, batched.Scalars[actorIndex*ScalarFeatures:(actorIndex+1)*ScalarFeatures]) ||
						!reflect.DeepEqual(encoded.Legal, batched.Legal[actorIndex*actions:(actorIndex+1)*actions]) {
						t.Fatalf("deployment/training input differs at step %d player %d", step, playerID)
					}
				}
				if step == 8 {
					break
				}
				actions := make([]battleengine.ActionID, config.ParticipantCount)
				for index, playerID := range episode.playerIDs {
					mask, err := episode.engine.LegalActionMask(playerID)
					if err != nil {
						t.Fatal(err)
					}
					preferred := battleengine.ActionID(1 + (index+step)%4)
					if mask[preferred] {
						actions[index] = preferred
					}
					if step == 0 && mask[battleengine.ActionPlaceBomb] {
						actions[index] = battleengine.ActionPlaceBomb
					}
				}
				if _, err := batch.Step([][]battleengine.ActionID{actions}); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestEncodeActorMatchesBatchTensor(t *testing.T) {
	batch, err := NewBatch(testBatchConfig(1))
	if err != nil {
		t.Fatal(err)
	}
	batched, err := batch.Observe()
	if err != nil {
		t.Fatal(err)
	}
	episode := &batch.episodes[0]
	playerID := episode.playerIDs[0]
	observation, err := episode.engine.Observation(playerID)
	if err != nil {
		t.Fatal(err)
	}
	danger, err := episode.engine.DangerTimeline(batch.config.DangerHorizonMS)
	if err != nil {
		t.Fatal(err)
	}
	legal, err := episode.engine.LegalActionMask(playerID)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := EncodeActorAtDecision(
		episode.engine, observation, danger, legal,
		batch.maxHeight, batch.maxWidth, batch.config.DecisionMS,
	)
	if err != nil {
		t.Fatal(err)
	}
	spatialCount := SpatialChannels * batch.maxHeight * batch.maxWidth
	if !reflect.DeepEqual(encoded.Spatial, batched.Spatial[:spatialCount]) {
		t.Fatal("single actor spatial encoding differs from batch encoding")
	}
	if !reflect.DeepEqual(encoded.ScalarValues, batched.Scalars[:ScalarFeatures]) {
		t.Fatal("single actor scalar encoding differs from batch encoding")
	}
	if !reflect.DeepEqual(encoded.Legal, batched.Legal[:int(battleengine.DiscreteActionCount)]) {
		t.Fatal("single actor legal encoding differs from batch encoding")
	}
}
