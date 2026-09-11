package battleengine

import (
	"strconv"
	"testing"
)

func TestNativeStaticWallExitUsesWholeMovementEndpoint(t *testing.T) {
	// Native MapID14 static probe 42 starts at (420,20), moves left by two
	// pixels and returns (418,20). Its intermediate leading point is still
	// inside the origin wall; the final leading point is in the open cell.
	grid := testOpenGrid(15, 13)
	grid.Cells[10] = Tile{Kind: CellSolid}
	result, err := ResolveNativeMovementProbe(NativeMovementProbe{
		Grid: grid, Position: Position{X: 420, Y: 20}, Direction: DirectionLeft,
		TickMS: 50, SpeedPixelsPerSecond: 40,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Moved || result.Position != (Position{X: 418, Y: 20}) {
		t.Fatalf("native wall-exit endpoint differs: %+v", result)
	}
}

// Expected outcomes are from the original 005b7999 x86 control-flow replay,
// image SHA256 007d7431bbdc4c260a1b2cda9a7146d6b3524e2b6dc2be691df1336569f71ada.
// See evidence/ai-native-upwall-control-flow-20260906.md for scope and adapters.
func TestNativePassMixedWallCornersFollowOriginalOrder(t *testing.T) {
	tests := []struct {
		name            string
		walls, bombs    []Cell
		position        Position
		active, allowed bool
	}{
		{"bubble-before-wall", []Cell{{Row: 0, Col: 1}}, []Cell{{Row: 1, Col: 1}}, Position{21, 40}, true, true},
		{"wall-before-bubble", []Cell{{Row: 1, Col: 1}}, []Cell{{Row: 0, Col: 1}}, Position{21, 40}, true, false},
		{"inactive-mixed-corners", []Cell{{Row: 0, Col: 1}}, []Cell{{Row: 1, Col: 1}}, Position{21, 40}, false, false},
		{"bubble-on-static-cell", []Cell{{Row: 1, Col: 1}}, []Cell{{Row: 1, Col: 1}}, Position{21, 60}, true, true},
		{"other-forward-corner-blocked", []Cell{{Row: 0, Col: 2}}, []Cell{{Row: 1, Col: 1}}, Position{21, 40}, true, false},
		{"one-bubble-open-exit", nil, []Cell{{Row: 1, Col: 1}}, Position{21, 60}, true, true},
		{"two-bubbles", nil, []Cell{{Row: 1, Col: 1}, {Row: 1, Col: 2}}, Position{21, 60}, true, false},
		{"static-only", []Cell{{Row: 1, Col: 1}}, nil, Position{21, 60}, true, false},
	}
	for rotation, direction := range []Direction{DirectionRight, DirectionUp, DirectionLeft, DirectionDown} {
		for _, test := range tests {
			t.Run(test.name+"/"+strconv.Itoa(int(direction)), func(t *testing.T) {
				rotateCell := func(cell Cell) Cell {
					for range rotation {
						cell = Cell{Row: 9 - cell.Col, Col: cell.Row}
					}
					return cell
				}
				config := testConfig()
				config.Grid = testOpenGrid(10, 10)
				config.Rules.ActorHalfSizePixels = NativeActorHalfSizePixels
				config.Participants[0].Spawn = Cell{Row: 5, Col: 5}
				config.Participants[1].Spawn = Cell{Row: 6, Col: 6}
				for _, cell := range test.walls {
					cell = rotateCell(cell)
					config.Grid.Cells[int(cell.Row)*10+int(cell.Col)] = Tile{Kind: CellSolid}
				}
				engine := mustEngine(t, config)
				for index, cell := range test.bombs {
					engine.bombs = append(engine.bombs, Bomb{ID: uint32(index + 1), Cell: rotateCell(cell), ExplodeAtMS: 10000})
				}
				position := test.position
				for range rotation {
					position = Position{X: position.Y, Y: 399 - position.X}
				}
				actor := &engine.actors[0]
				actor.NativePassActive = test.active
				if got := engine.positionWalkable(actor, position, direction); got != test.allowed {
					t.Fatalf("position=%+v allowed=%t, original x86=%t", position, got, test.allowed)
				}
			})
		}
	}
}

func TestNativeActivePassUsesEndpointInEveryDirection(t *testing.T) {
	for _, direction := range []Direction{DirectionUp, DirectionRight, DirectionDown, DirectionLeft} {
		config := testConfig()
		config.Grid = testOpenGrid(10, 10)
		config.Rules.TickMS = 20
		config.Rules.BombFuseMS = NativeBombFuseMS
		config.Rules.RoundDurationMS = 20000
		config.Rules.ActorHalfSizePixels = NativeActorHalfSizePixels
		config.Participants[0].Spawn = Cell{Row: 4, Col: 4}
		config.Participants[0].SpeedPixelsPerSecond = 180
		dx, dy, _ := direction.delta()
		config.Participants[1].Spawn = Cell{Row: 4 + int16(dy), Col: 4 + int16(dx)}
		config.Participants[1].SpeedPixelsPerSecond = 200
		engine := mustEngine(t, config)
		step := func(actions ...Action) {
			t.Helper()
			if _, err := engine.Step(actions); err != nil {
				t.Fatal(err)
			}
		}
		side := DirectionRight
		if direction == DirectionRight || direction == DirectionLeft {
			side = DirectionDown
		}
		step(Action{PlayerID: 2, Move: side, PlaceBomb: true})
		charged := false
		for range 70 {
			actor := engine.Actors()[0]
			if actor.NativePassCollisionValid && engine.ElapsedMS()-actor.NativePassCollisionStartedAt >= 520 {
				charged = true
				break
			}
			step(Action{PlayerID: 1, Move: direction}, Action{PlayerID: 2, Move: side})
		}
		if !charged {
			t.Fatalf("direction %d never charged", direction)
		}
		step(Action{PlayerID: 1, Move: direction, PlaceBomb: true})
		actor := engine.Actors()[0]
		if !actor.NativePassActive || !nativeMovementAdvancedOnRequestedAxis(Position{X: 180, Y: 180}, actor.Position, direction) {
			t.Fatalf("direction %d failed native placement-and-pass: %+v", direction, actor)
		}
		if !containsAction(mustLegalActions(t, engine, 1), Action{PlayerID: 1, Move: direction}) {
			t.Fatalf("direction %d passage was masked out", direction)
		}
		for range 35 {
			step(Action{PlayerID: 1, Move: direction})
		}
		// Original x86 FUN_005cfc10 at 180 px/s reaches 50.39995 or
		// 309.60022 after these 36 updates, with the pass already expired.
		want := Position{X: 180 + dx*130, Y: 180 + dy*130}
		actor = engine.Actors()[0]
		if actor.Position != want || actor.NativePassActive {
			t.Fatalf("direction %d final = %+v, want %+v after expiry", direction, actor, want)
		}
	}
}

func TestNativeWallEntryFromOrdinaryInputsSurvivesPassExpiry(t *testing.T) {
	for _, place := range []bool{false, true} {
		config := testConfig()
		config.Grid = testOpenGrid(10, 10)
		wall := Cell{Row: 3, Col: 4}
		config.Grid.Cells[34] = Tile{Kind: CellSolid, MapElementOccupied: true}
		config.Rules.TickMS = 20
		config.Rules.BombFuseMS = NativeBombFuseMS
		config.Rules.RoundDurationMS = 20000
		config.Rules.TrapDurationMS = 6000
		config.Rules.ActorHalfSizePixels = NativeActorHalfSizePixels
		config.Participants[0].Spawn = Cell{Row: 4, Col: 3}
		config.Participants[0].SpeedPixelsPerSecond = 80
		config.Participants[1].Spawn = Cell{Row: 4, Col: 4}
		config.Participants[1].SpeedPixelsPerSecond = 200
		engine := mustEngine(t, config)
		explosions := 0
		step := func(actions ...Action) {
			t.Helper()
			events, err := engine.Step(actions)
			if err != nil {
				t.Fatal(err)
			}
			for _, event := range events {
				if event.Kind == EventBombExploded {
					explosions++
				}
			}
		}
		step(Action{PlayerID: 2, Move: DirectionDown, PlaceBomb: true})
		for range 26 {
			step(Action{PlayerID: 1, Move: DirectionRight}, Action{PlayerID: 2, Move: DirectionDown})
		}
		step(Action{PlayerID: 1, Move: DirectionUp})
		step(Action{PlayerID: 1, Move: DirectionUp, PlaceBomb: place})
		for range 11 {
			step(Action{PlayerID: 1, Move: DirectionUp})
		}
		if place && !engine.NeedsImmediatePolicyDecision(1) {
			t.Fatal("active pass must allow the learned turn after its activation window closes")
		}
		for range 13 {
			step(Action{PlayerID: 1, Move: DirectionRight})
		}
		actor := engine.Actors()[0]
		if (actor.Position.Cell() == wall) != place {
			t.Fatalf("place=%t position=%+v, expected wall entry only after successful placement", place, actor.Position)
		}
		if !place {
			continue
		}
		if engine.ElapsedMS() != 1060 || actor.Position != (Position{X: 161, Y: 159}) {
			t.Fatalf("entry differs from native-sequence regression: time=%d actor=%+v", engine.ElapsedMS(), actor)
		}
		for engine.ElapsedMS() < 6000 {
			step()
			actor = engine.Actors()[0]
			if actor.Position.Cell() != wall || actor.State != ActorActive {
				t.Fatalf("wall occupant changed during wait: %+v", actor)
			}
		}
		if actor.NativePassActive || explosions != 2 || len(engine.Bombs()) != 0 {
			t.Fatalf("did not survive actual explosions beyond pass expiry: active=%t explosions=%d", actor.NativePassActive, explosions)
		}
		for range 10 {
			step(Action{PlayerID: 1, Move: DirectionLeft})
		}
		actor = engine.Actors()[0]
		if actor.Position.Cell() == wall || actor.State != ActorActive {
			t.Fatalf("could not leave wall: %+v", actor)
		}
	}
}
