package battleengine

import "testing"

// The refuge has no current blast at its entrance. An older bubble blocks the
// corridor farther out; after it explodes, the actor can cross its former cell
// and turn away from the new bubble's blast. A conservative whole-cell feature
// is not permission to reject this legal placement or label the route unsafe.
func TestNativeRefugePlacementCanWaitForExistingBombToClear(t *testing.T) {
	config := testConfig()
	config.Rules.TickMS = 20
	config.Rules.BombFuseMS = 3_000
	config.Rules.FlameDurationMS = 500
	config.Participants[0].SpeedPixelsPerSecond = 200
	config.Participants[0].BombPower = 2
	config.Participants[1].Spawn = Cell{Row: 2, Col: 4}
	for index := range config.Grid.Cells {
		config.Grid.Cells[index] = Tile{Kind: CellSolid}
	}
	for _, cell := range []Cell{{Row: 1, Col: 1}, {Row: 1, Col: 2}, {Row: 1, Col: 3}, {Row: 0, Col: 3}, {Row: 2, Col: 4}} {
		config.Grid.Cells[int(cell.Row)*int(config.Grid.Width)+int(cell.Col)] = Tile{Kind: CellOpen, FlamePassable: true}
	}
	engine := mustEngine(t, config)
	engine.bombs = []Bomb{{ID: 77, OwnerID: 2, Cell: Cell{Row: 1, Col: 3}, Power: 1, ExplodeAtMS: 600}}
	engine.nextBombID = 78
	current, err := engine.DangerTimeline(3_500)
	if err != nil {
		t.Fatal(err)
	}
	if at, threatened := current.ImpactAt(engine.actors[0].Position.Cell()); threatened {
		t.Fatalf("refuge must be outside the existing bomb's blast, got impact %d", at)
	}
	facts, err := engine.TacticalConsequencesForPlayerAtDecision(1, current, 100)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("before native execution: placement legal=%t, projection valid=%t, direction refuges=%v", facts.BombLegal, facts.BombKnownDangerProjectionValid, facts.BombDirectionWholeCellRefugeFound)
	if !facts.BombLegal {
		t.Fatal("a conservative route estimate must not remove a legal bomb")
	}
	exploded := make(map[uint32]uint32)
	step := func(action Action) {
		t.Helper()
		events, stepErr := engine.Step([]Action{action})
		if stepErr != nil {
			t.Fatal(stepErr)
		}
		for _, event := range events {
			if event.Kind == EventBombExploded {
				exploded[event.BombID] = event.TimeMS
			}
		}
		if engine.actors[0].State != ActorActive {
			t.Fatalf("actor was hit at %d ms at %+v", engine.elapsedMS, engine.actors[0].Position)
		}
	}
	step(Action{PlayerID: 1, PlaceBomb: true})
	for engine.elapsedMS < 620 {
		step(Action{PlayerID: 1})
	}
	for engine.elapsedMS < 1_020 {
		step(Action{PlayerID: 1, Move: DirectionRight})
	}
	if engine.actors[0].Position.Cell() != (Cell{Row: 1, Col: 3}) {
		t.Fatalf("failed to cross the old bubble cell: %+v", engine.actors[0].Position)
	}
	for engine.elapsedMS < 1_220 {
		step(Action{PlayerID: 1, Move: DirectionUp})
	}
	if engine.actors[0].Position.Cell() != (Cell{Row: 0, Col: 3}) {
		t.Fatalf("failed to reach the turned refuge: %+v", engine.actors[0].Position)
	}
	for engine.elapsedMS < 3_520 {
		step(Action{PlayerID: 1})
	}
	// Native fuse expiry is strict: 3000 ms remains armed; the next 20 ms
	// frame detonates the new bubble. The old one was explicitly due at 600.
	if exploded[77] != 600 || exploded[78] != 3_020 || len(engine.bombs) != 0 {
		t.Fatalf("unexpected bomb times or remaining bubbles: %v / %+v", exploded, engine.bombs)
	}
	t.Logf("native execution survived both bubbles: explosions=%v, final position=%+v", exploded, engine.actors[0].Position)
}
