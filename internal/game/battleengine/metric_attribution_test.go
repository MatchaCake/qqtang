package battleengine

import "testing"

// Self-elimination attribution describes the fatal flame's owner, not who
// initiated a chain or whether the victim's decisions were avoidable mistakes.
func TestEnemyTriggeredOwnBombRetainsSelfEliminationAttribution(t *testing.T) {
	config := testConfig()
	config.Grid = testOpenGrid(5, 3)
	config.Participants[0].Spawn = Cell{Row: 0, Col: 4}
	config.Participants[1].Spawn = Cell{Row: 2, Col: 4}
	engine := mustEngine(t, config)
	// Install the fixture at the pre-movement explosion clock.
	engine.elapsedMS = 100
	engine.bombs = []Bomb{
		{ID: 1, OwnerID: 2, Cell: Cell{Row: 0, Col: 0}, Power: 2, ExplodeAtMS: 100},
		{ID: 2, OwnerID: 1, Cell: Cell{Row: 0, Col: 2}, Power: 2, ExplodeAtMS: 9_000},
	}
	engine.nextBombID = 3
	events, err := engine.Step(nil)
	if err != nil {
		t.Fatal(err)
	}
	chained := false
	for _, event := range events {
		if event.Kind == EventBombExploded && event.BombID == 2 {
			chained = event.TriggeredByBombID == 1 && event.PlayerID == 1
		}
	}
	if !chained {
		t.Fatalf("own bomb was not attributed to its owner after enemy trigger: %+v", events)
	}
	victim := &engine.actors[0]
	if victim.State != ActorTrapped || victim.TrappedBy != 1 || victim.TrappedByBombID != 2 {
		t.Fatalf("fatal own-flame trap attribution = %+v", victim)
	}
	// Expire the recorded trap using the normal elimination path.
	victim.TrapExpiresAt = engine.elapsedMS
	deaths := engine.expireTraps()
	for _, event := range deaths {
		if event.Kind == EventActorEliminated && event.TargetID == 1 {
			if event.PlayerID != event.TargetID || event.BombID != 2 {
				t.Fatalf("self-elimination event lost the fatal flame identity: %+v", event)
			}
			return
		}
	}
	t.Fatalf("missing elimination event: %+v", deaths)
}
