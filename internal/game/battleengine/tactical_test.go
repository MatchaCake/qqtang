package battleengine

import "testing"

func TestDangerTimelineUsesProductionChainAndWallRules(t *testing.T) {
	config := testConfig()
	config.Grid = testOpenGrid(7, 3)
	config.Participants[0].Spawn = Cell{Row: 0, Col: 0}
	config.Participants[1].Spawn = Cell{Row: 2, Col: 6}
	engine := mustEngine(t, config)
	engine.grid.Cells[1*7+5] = Tile{Kind: CellBreakable, FlamePassable: false, Durability: 1}
	engine.bombs = []Bomb{
		{ID: 1, OwnerID: 1, Cell: Cell{Row: 1, Col: 1}, Power: 3, ExplodeAtMS: 300},
		{ID: 2, OwnerID: 2, Cell: Cell{Row: 1, Col: 3}, Power: 3, ExplodeAtMS: 900},
	}
	engine.nextBombID = 3

	timeline, err := engine.DangerTimeline(1_000)
	if err != nil {
		t.Fatal(err)
	}
	for _, cell := range []Cell{{Row: 1, Col: 1}, {Row: 1, Col: 2}, {Row: 1, Col: 3}, {Row: 1, Col: 4}, {Row: 1, Col: 5}} {
		if impact, ok := timeline.ImpactAt(cell); !ok || impact != 300 {
			t.Fatalf("impact at %+v = %d/%t, want 300/true", cell, impact, ok)
		}
		if impact, ok := timeline.LastImpactAt(cell); !ok || impact != 300 {
			t.Fatalf("last impact at %+v = %d/%t, want 300/true", cell, impact, ok)
		}
		if clear, ok := timeline.ClearAt(cell); !ok || clear != 301 {
			t.Fatalf("clear at %+v = %d/%t, want 301/true", cell, clear, ok)
		}
		if waves := timeline.WaveCountAt(cell); waves != 1 {
			t.Fatalf("waves at %+v = %d, want 1", cell, waves)
		}
	}
	if impact, ok := timeline.ImpactAt(Cell{Row: 1, Col: 6}); ok {
		t.Fatalf("flame crossed breakable wall at 1,5: %d", impact)
	}
	if engine.grid.Cells[1*7+5].Kind != CellBreakable || len(engine.bombs) != 2 {
		t.Fatal("danger forecast mutated the source engine")
	}
}

func TestDangerTimelineRetainsSeparatedVisibleWaves(t *testing.T) {
	config := testConfig()
	config.Grid = testOpenGrid(5, 3)
	config.Participants[0].Spawn = Cell{Row: 0, Col: 0}
	config.Participants[1].Spawn = Cell{Row: 2, Col: 4}
	engine := mustEngine(t, config)
	engine.bombs = []Bomb{
		{ID: 1, OwnerID: 1, Cell: Cell{Row: 1, Col: 0}, Power: 2, ExplodeAtMS: 300},
		{ID: 2, OwnerID: 2, Cell: Cell{Row: 1, Col: 4}, Power: 2, ExplodeAtMS: 900},
	}
	engine.nextBombID = 3
	timeline, err := engine.DangerTimeline(1_200)
	if err != nil {
		t.Fatal(err)
	}
	cell := Cell{Row: 1, Col: 2}
	if first, ok := timeline.ImpactAt(cell); !ok || first != 300 {
		t.Fatalf("first impact = %d/%t, want 300/true", first, ok)
	}
	if last, ok := timeline.LastImpactAt(cell); !ok || last != 900 {
		t.Fatalf("last impact = %d/%t, want 900/true", last, ok)
	}
	if clear, ok := timeline.ClearAt(cell); !ok || clear != 901 {
		t.Fatalf("clear = %d/%t, want 901/true", clear, ok)
	}
	if waves := timeline.WaveCountAt(cell); waves != 2 {
		t.Fatalf("waves = %d, want 2", waves)
	}
}

func TestDangerTimelineDoesNotRearmSpentVisualFlames(t *testing.T) {
	config := testConfig()
	config.Grid = testOpenGrid(5, 3)
	config.Rules.FlameDurationMS = 500
	config.Participants[0].Spawn = Cell{Row: 0, Col: 0}
	config.Participants[1].Spawn = Cell{Row: 2, Col: 4}
	engine := mustEngine(t, config)
	engine.bombs = []Bomb{
		{ID: 1, OwnerID: 1, Cell: Cell{Row: 1, Col: 0}, Power: 2, ExplodeAtMS: 300},
		{ID: 2, OwnerID: 2, Cell: Cell{Row: 1, Col: 4}, Power: 2, ExplodeAtMS: 600},
	}
	engine.nextBombID = 3
	cell := Cell{Row: 1, Col: 2}
	for _, now := range []uint32{300, 400, 600, 700} {
		for engine.elapsedMS <= now {
			if _, err := engine.Step(nil); err != nil {
				t.Fatal(err)
			}
		}
		if !hasFlame(engine.flames, cell) {
			t.Fatalf("missing visible flame at %d", now)
		}
		timeline, err := engine.DangerTimeline(1_200)
		if err != nil {
			t.Fatal(err)
		}
		impact, predicted := timeline.ImpactAt(cell)
		if now < 600 {
			if !predicted || impact != 600 || timeline.WaveCountAt(cell) != 1 {
				t.Fatalf("at %d: first=%d/%v waves=%d, want only the future impact at 600", now, impact, predicted, timeline.WaveCountAt(cell))
			}
			for _, check := range []struct {
				arrival, depart uint32
				safe            bool
			}{{now + 1, 599, true}, {599, 600, false}, {600, 600, false}, {601, 700, true}} {
				index := int(cell.Row)*int(timeline.Width) + int(cell.Col)
				if got := tacticalCellSafeDuring(timeline, cell, check.arrival, check.depart); got != check.safe {
					t.Fatalf("search safety at %d in [%d,%d]=%v want %v", now, check.arrival, check.depart, got, check.safe)
				}
				if got := tacticalIndexSafeDuring(timeline, index, check.arrival, check.depart); got != check.safe {
					t.Fatalf("encoded consequence safety at %d in [%d,%d]=%v want %v", now, check.arrival, check.depart, got, check.safe)
				}
			}
		} else if predicted || timeline.WaveCountAt(cell) != 0 {
			t.Fatalf("at %d: spent visual flame became a new impact %d/%v waves=%d", now, impact, predicted, timeline.WaveCountAt(cell))
		}
	}
}

func TestDangerTimelineMovesThrownBombThreatAndWaitsForLanding(t *testing.T) {
	config := testConfig()
	config.Grid = testOpenGrid(5, 3)
	engine := mustEngine(t, config)
	engine.bombs = []Bomb{{
		ID: 1, OwnerID: 2, Cell: Cell{Row: 1, Col: 3}, Power: 0,
		ExplodeAtMS: 100, FlightUntilMS: NativeBombFlightMS,
	}}
	engine.nextBombID = 2

	timeline, err := engine.DangerTimeline(600)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := timeline.ImpactAt(Cell{Row: 1, Col: 2}); ok {
		t.Fatal("thrown bomb retained danger at its old cell")
	}
	if impact, ok := timeline.ImpactAt(Cell{Row: 1, Col: 3}); !ok || impact != NativeBombFlightMS {
		t.Fatalf("landing danger = %d/%t, want %d/true", impact, ok, NativeBombFlightMS)
	}
}

func TestChainContactArmsFlyingBombButWaitsForLanding(t *testing.T) {
	config := testConfig()
	config.Grid = testOpenGrid(6, 3)
	config.Rules.TickMS = 100
	engine := mustEngine(t, config)
	engine.elapsedMS = 100
	engine.bombs = []Bomb{
		{ID: 1, OwnerID: 1, Cell: Cell{Row: 1, Col: 1}, Power: 3, ExplodeAtMS: 100},
		{ID: 2, OwnerID: 2, Cell: Cell{Row: 1, Col: 3}, Power: 1, ExplodeAtMS: 900, FlightUntilMS: 400},
	}
	engine.nextBombID = 3
	events, err := engine.Step(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !hasEvent(events, EventBombExploded, 0) || len(engine.bombs) != 1 || engine.bombs[0].ID != 2 || engine.bombs[0].ExplodeAtMS != 100 {
		t.Fatalf("flying chain arm at 100 = events=%+v bombs=%+v", events, engine.bombs)
	}
	for engine.elapsedMS < 400 {
		if _, err := engine.Step(nil); err != nil {
			t.Fatal(err)
		}
	}
	if len(engine.bombs) != 1 {
		t.Fatal("chain-armed bomb exploded before landing")
	}
	events, err = engine.Step(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !hasEvent(events, EventBombExploded, 0) || len(engine.bombs) != 0 {
		t.Fatalf("chain-armed bomb did not explode on landing: %+v/%+v", events, engine.bombs)
	}
}

func TestDangerTimelineRebuildsChainAfterBombThrow(t *testing.T) {
	config := testConfig()
	config.Grid = testOpenGrid(7, 3)
	config.Rules.TickMS = 100
	engine := mustEngine(t, config)
	// Bomb 2 has already been thrown from a remote cell to column 3. Its new
	// location puts it in bomb 1's early blast, so the forecast must rebuild the
	// chain from current map cells instead of retaining the pre-throw schedule.
	engine.bombs = []Bomb{
		{ID: 1, OwnerID: 1, Cell: Cell{Row: 1, Col: 1}, Power: 3, ExplodeAtMS: 100},
		{ID: 2, OwnerID: 2, Cell: Cell{Row: 1, Col: 3}, Power: 1, ExplodeAtMS: 900, FlightUntilMS: 400},
	}
	engine.nextBombID = 3

	timeline, err := engine.DangerTimeline(1_000)
	if err != nil {
		t.Fatal(err)
	}
	if impact, ok := timeline.ImpactAt(Cell{Row: 1, Col: 4}); !ok || impact != 400 {
		t.Fatalf("post-throw chain impact = %d/%t, want 400/true", impact, ok)
	}
}

func TestSafetyMaskRejectsImmediateImpactAndKeepsEscape(t *testing.T) {
	config := testConfig()
	config.Grid = testOpenGrid(5, 3)
	config.Rules.TickMS = 100
	config.Participants[0].Spawn = Cell{Row: 1, Col: 1}
	config.Participants[0].SpeedPixelsPerSecond = 400
	config.Participants[1].Spawn = Cell{Row: 2, Col: 4}
	engine := mustEngine(t, config)
	// Power zero is used only as a focused fixture for the own-cell impact;
	// production placement validation never creates such a bomb.
	engine.bombs = []Bomb{{ID: 1, OwnerID: 2, Cell: Cell{Row: 1, Col: 1}, Power: 0, ExplodeAtMS: 100}}
	engine.nextBombID = 2

	mask, err := engine.SafetyMask(1, 100)
	if err != nil {
		t.Fatal(err)
	}
	if mask[ActionWait] {
		t.Fatal("wait remained safe on an immediate flame impact")
	}
	if !mask[ActionMoveUp] || !mask[ActionMoveDown] {
		t.Fatalf("one-cell escapes were removed: up=%t down=%t", mask[ActionMoveUp], mask[ActionMoveDown])
	}
}

func TestDangerTimelineValidatesArguments(t *testing.T) {
	engine := mustEngine(t, testConfig())
	if _, err := engine.DangerTimeline(0); err == nil {
		t.Fatal("zero danger horizon was accepted")
	}
	if _, err := (*Engine)(nil).DangerTimeline(100); err == nil {
		t.Fatal("nil engine danger forecast was accepted")
	}
}
