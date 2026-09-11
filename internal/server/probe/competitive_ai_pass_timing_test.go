package probe

import (
	"testing"

	"qqtang/internal/game/battleengine"
	"qqtang/internal/game/match"
	"qqtang/internal/protocol/game"
)

// Feed ordinary inputs through the actual live cadence wrapper. The scripted
// choice is a timing oracle, not a claim that the published network knows it.
func TestCompetitiveAINativePassCadenceCoversContactPhases(t *testing.T) {
	for startX := int32(41); startX <= 60; startX++ {
		live, err := newLiveCompetitiveAIRuntime(7, testCompetitiveAIMap(2),
			game.GameBeginData{GameID: 109, SpawnSeed: 11, ItemSeed: 22},
			[]match.CompetitiveParticipant{
				{PlayerID: 1, RoleID: 1, TeamID: 1, Source: match.CompetitiveParticipantHuman},
				{PlayerID: 20001, RoleID: 2, TeamID: 2, Source: match.CompetitiveParticipantVirtualAI},
			}, false, battleengine.PolicyFunc(func(observation battleengine.Observation, _ []battleengine.Action) (battleengine.Action, error) {
				return battleengine.Action{PlayerID: observation.PlayerID}, nil
			}), 100)
		if err != nil {
			t.Fatal(err)
		}
		engine := live.runtime.EngineSnapshot()
		if err := engine.ApplyVerifiedMovementCheckpoint(20001, battleengine.Position{X: startX, Y: 20}); err != nil {
			t.Fatal(err)
		}
		if _, err := engine.ApplyVerifiedBombPlacement(1, battleengine.Cell{Row: 0, Col: 2}, 1, 0); err != nil {
			t.Fatal(err)
		}
		placed := false
		policy := competitiveAILivePolicy(battleengine.PolicyFunc(func(observation battleengine.Observation, legal []battleengine.Action) (battleengine.Action, error) {
			chosen := battleengine.Action{PlayerID: observation.PlayerID, Move: battleengine.DirectionRight}
			for _, self := range observation.Actors {
				if self.PlayerID == observation.PlayerID && self.NativePassCollisionValid && !placed && observation.ClockMS-self.NativePassCollisionStartedAt > 500 {
					chosen.PlaceBomb = true
					placed = true
				}
			}
			if competitiveAIActionAllowed(legal, chosen) {
				return chosen, nil
			}
			chosen.Move = battleengine.DirectionNone
			if competitiveAIActionAllowed(legal, chosen) {
				return chosen, nil
			}
			return battleengine.Action{PlayerID: observation.PlayerID}, nil
		}), 5, 0).(battleengine.SnapshotPolicy)
		activated, crossed := false, false
		for tick := 0; tick < 90; tick++ {
			observation, err := engine.Observation(20001)
			if err != nil {
				t.Fatal(err)
			}
			legal, err := engine.LegalActions(20001)
			if err != nil {
				t.Fatal(err)
			}
			action, err := policy.ChooseActionWithSnapshot(engine, observation, legal)
			if err != nil {
				t.Fatal(err)
			}
			events, err := engine.Step([]battleengine.Action{action})
			if err != nil {
				t.Fatal(err)
			}
			for _, event := range events {
				if event.Kind == battleengine.EventNativePassStarted && event.PlayerID == 20001 {
					activated = true
				}
			}
			for _, actor := range engine.Actors() {
				if actor.PlayerID == 20001 && actor.Position.Cell().Col > 2 {
					crossed = true
				}
			}
			if crossed {
				break
			}
		}
		if !activated || !crossed {
			t.Fatalf("start_x=%d activated=%v crossed=%v", startX, activated, crossed)
		}
	}
}
