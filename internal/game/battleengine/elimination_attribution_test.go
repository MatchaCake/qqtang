package battleengine

import "testing"

func TestContactEliminationPreservesActualTrapper(t *testing.T) {
	for _, trapper := range []uint16{0, 1, 2, 3} {
		config := testConfig()
		config.Participants = []Participant{
			testParticipant(1, 1, ParticipantHuman, Cell{Row: 1, Col: 1}),
			testParticipant(2, 2, ParticipantHuman, Cell{Row: 1, Col: 2}),
			testParticipant(3, 3, ParticipantHuman, Cell{Row: 2, Col: 4}),
		}
		engine := mustEngine(t, config)
		victim := &engine.actors[1]
		victim.State, victim.TrappedBy, victim.TrappedByBombID = ActorTrapped, trapper, 77
		finisher := &engine.actors[0]
		finisher.nativePreviousPosition = finisher.Position
		finisher.Position = victim.Position
		events := engine.resolveActorContacts()
		found := false
		for _, event := range events {
			if event.Kind != EventActorEliminated {
				continue
			}
			found = true
			if event.PlayerID != 1 || event.TargetID != 2 || event.TrappedByPlayerID != trapper || event.BombID != 77 || event.EliminationCause != EliminationContact {
				t.Fatalf("trapper %d was replaced by the contact killer: %+v", trapper, event)
			}
		}
		if !found || victim.State != ActorEliminated || victim.TrappedBy != 0 {
			t.Fatalf("contact did not finish normally: %+v", events)
		}
	}
}

func TestNaturalAndUnclassifiedEliminationAttribution(t *testing.T) {
	for _, trapper := range []uint16{1, 2} {
		engine := mustEngine(t, testConfig())
		victim := &engine.actors[1]
		victim.State, victim.TrappedBy, victim.TrappedByBombID = ActorTrapped, trapper, 77
		victim.TrapExpiresAt, engine.elapsedMS = 100, 100
		found := false
		for _, event := range engine.expireTraps() {
			if event.Kind == EventActorEliminated {
				found = true
				if event.PlayerID != trapper || event.TrappedByPlayerID != trapper || event.EliminationCause != EliminationTrapDeath {
					t.Fatalf("natural death attribution = %+v", event)
				}
			}
		}
		if !found {
			t.Fatal("missing natural death")
		}
	}
	engine := mustEngine(t, testConfig())
	engine.actors[1].State, engine.actors[1].TrappedBy = ActorTrapped, 2
	events, err := engine.ApplyVerifiedElimination(2, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Kind == EventActorEliminated {
			if event.PlayerID != 1 || event.TrappedByPlayerID != 2 || event.EliminationCause != EliminationUnknown {
				t.Fatalf("reconciled death invented a contact or trapping source: %+v", event)
			}
			return
		}
	}
	t.Fatal("missing reconciled elimination")
}
