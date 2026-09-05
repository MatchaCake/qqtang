package battleenv

import (
	"fmt"
	"reflect"
	"testing"

	"qqtang/internal/game/battleengine"
)

func TestHoldCompletedPreservesLiveGamesAndReportsTerminalOnce(t *testing.T) {
	for _, reuse := range []bool{false, true} {
		t.Run(fmt.Sprintf("reuse=%v", reuse), func(t *testing.T) {
			config := testBatchConfig(2)
			config.ReuseTensorBuffers = reuse
			reference, err := NewBatch(config)
			if err != nil {
				t.Fatal(err)
			}
			config.HoldCompletedEpisodes = true
			held, err := NewBatch(config)
			if err != nil {
				t.Fatal(err)
			}
			actions := [][]battleengine.ActionID{{battleengine.ActionPlaceBomb, battleengine.ActionWait}, {battleengine.ActionWait, battleengine.ActionWait}}
			ended := false
			for step := 0; step < 120; step++ {
				left, err := reference.Step(actions)
				if err != nil {
					t.Fatal(err)
				}
				right, err := held.Step(actions)
				if err != nil {
					t.Fatal(err)
				}
				if left.Dones[0] != 0 {
					if right.Dones[0] != 1 || right.Dones[1] != 0 {
						t.Fatal("wrong terminal slot")
					}
					// Only the held terminal observation is intentionally inactive;
					// all authoritative results, counters and events must match.
					left.Observation = right.Observation
					if !reflect.DeepEqual(left, right) {
						t.Fatal("first terminal evidence differs")
					}
					if right.CompletedAgentMetrics[0].BombsPlaced != 1 {
						t.Fatal("missing completed metrics")
					}
					ended = true
					break
				}
				if !reflect.DeepEqual(left, right) {
					t.Fatalf("live step %d differs", step)
				}
				actions[0][0] = battleengine.ActionWait
			}
			if !ended {
				t.Fatal("native self-bomb fixture did not terminate")
			}
			if _, err := reference.Step(actions); err == nil {
				t.Fatal("default terminal requirement changed")
			}
			if err := reference.Reset([]int{0}); err != nil {
				t.Fatal(err)
			}
			frozenClock := held.episodes[0].engine.ElapsedMS()
			frozenMetrics := held.Metrics()[0]
			for step := 0; step < 5; step++ {
				// A repeated valid ID on a held slot is an idempotent no-op.
				actions[0][0] = battleengine.ActionMoveUp
				right, err := held.Step(actions)
				if err != nil {
					t.Fatal(err)
				}
				actions[0][0] = battleengine.ActionWait
				if _, err := reference.Step(actions); err != nil {
					t.Fatal(err)
				}
				if right.Dones[0] != 0 || right.Outcomes[0].Ended || len(right.Events[0]) != 0 {
					t.Fatal("terminal result repeated")
				}
				for actor := 0; actor < 2; actor++ {
					if right.Rewards[actor] != 0 || right.SelfEliminations[actor] != 0 || right.CompletedAgentMetrics[actor] != (AgentMetrics{}) {
						t.Fatal("held slot repeated training data")
					}
					if right.Observation.Active[actor] != 0 {
						t.Fatal("held slot has an active actor")
					}
					for action := 0; action < int(battleengine.DiscreteActionCount); action++ {
						want := uint8(0)
						if action == int(battleengine.ActionWait) {
							want = 1
						}
						if right.Observation.Legal[actor*int(battleengine.DiscreteActionCount)+action] != want {
							t.Fatal("invalid held policy row")
						}
					}
				}
				if held.episodes[0].engine.ElapsedMS() != frozenClock || !reflect.DeepEqual(held.Metrics()[0], frozenMetrics) {
					t.Fatal("held world or counters advanced")
				}
				if !reflect.DeepEqual(reference.episodes[1].engine.Actors(), held.episodes[1].engine.Actors()) || !reflect.DeepEqual(reference.Metrics()[1], held.Metrics()[1]) {
					t.Fatal("live neighbouring world changed")
				}
			}
			actions[0][0] = battleengine.DiscreteActionCount
			if _, err := held.Step(actions); err == nil {
				t.Fatal("held slot accepted an out-of-range ID")
			}
			if err := held.Reset([]int{0}); err != nil {
				t.Fatal(err)
			}
			actions[0][0] = battleengine.ActionWait
			fresh, err := held.Step(actions)
			if err != nil {
				t.Fatal(err)
			}
			if fresh.Dones[0] != 0 || fresh.Observation.Active[0] != 1 || held.Metrics()[0].Agents[0].BombsPlaced != 0 {
				t.Fatal("reset did not reactivate a clean game")
			}
		})
	}
}
