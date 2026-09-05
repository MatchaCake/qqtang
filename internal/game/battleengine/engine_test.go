package battleengine

import (
	"reflect"
	"testing"
)

func TestStepIsIndependentOfInputAndParticipantOrder(t *testing.T) {
	config := testConfig()
	config.Participants[0], config.Participants[1] = config.Participants[1], config.Participants[0]
	first := mustEngine(t, config)
	second := mustEngine(t, config)

	firstEvents, err := first.Step([]Action{
		{PlayerID: 2, Move: DirectionLeft},
		{PlayerID: 1, Move: DirectionRight, PlaceBomb: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	secondEvents, err := second.Step([]Action{
		{PlayerID: 1, Move: DirectionRight, PlaceBomb: true},
		{PlayerID: 2, Move: DirectionLeft},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(firstEvents, secondEvents) || !reflect.DeepEqual(first, second) {
		t.Fatalf("canonical steps differ:\nfirst events=%+v\nsecond events=%+v", firstEvents, secondEvents)
	}
	actors := first.Actors()
	if actors[0].PlayerID != 1 || actors[1].PlayerID != 2 {
		t.Fatalf("actors are not canonically sorted: %+v", actors)
	}
}

func TestCloneAndObservationAreDeepCopies(t *testing.T) {
	engine := mustEngine(t, testConfig())
	if _, err := engine.Step([]Action{{PlayerID: 1, PlaceBomb: true}}); err != nil {
		t.Fatal(err)
	}
	clone := engine.Clone()
	clone.grid.Cells[0] = Tile{Kind: CellBreakable, Durability: 1}
	clone.actors[0].NativePassCollisionCell = Cell{Row: 99, Col: 99}
	if engine.grid.Cells[0].Kind == CellBreakable || engine.actors[0].NativePassCollisionCell == (Cell{Row: 99, Col: 99}) {
		t.Fatal("Clone leaked mutable grid or actor state")
	}
	observation, err := engine.Observation(1)
	if err != nil {
		t.Fatal(err)
	}
	observation.Grid.Cells[0] = Tile{Kind: CellBreakable, Durability: 1}
	observation.Actors[0].NativePassCollisionCell = Cell{Row: 99, Col: 99}
	if engine.grid.Cells[0].Kind == CellBreakable || engine.actors[0].NativePassCollisionCell == (Cell{Row: 99, Col: 99}) {
		t.Fatal("Observation leaked mutable state")
	}
}

func TestBatchedObservationsMatchIndependentParticipantViews(t *testing.T) {
	engine := mustEngine(t, testConfig())
	engine.actors[0].HeldActions[0] = HeldActionSlot{ActionID: 64, Count: 2}
	engine.actors[1].NativePassActive = true
	engine.actors[1].NativePassDurationMS = 555

	batched, err := engine.Observations([]uint16{1, 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(batched) != 2 {
		t.Fatalf("batched observation count = %d, want 2", len(batched))
	}
	for index, playerID := range []uint16{1, 2} {
		independent, observeErr := engine.Observation(playerID)
		if observeErr != nil {
			t.Fatal(observeErr)
		}
		if !reflect.DeepEqual(batched[index], independent) {
			t.Fatalf("batched observation for player %d differs from independent projection", playerID)
		}
	}
	if batched[0].Actors[1].NativePassActive || !batched[1].Actors[1].NativePassActive {
		t.Fatalf("batched views leaked or hid observer-private pass state: first=%+v second=%+v", batched[0].Actors[1], batched[1].Actors[1])
	}
}

func TestActorPassableMapElementAllowsMovementButRejectsBombPlacement(t *testing.T) {
	config := testConfig()
	visualWall := Cell{Row: 1, Col: 2}
	config.Grid.Cells[1*int(config.Grid.Width)+2] = Tile{
		Kind: CellOpen, FlamePassable: false, MapElementOccupied: true,
	}
	engine := mustEngine(t, config)

	for step := 0; step < 3; step++ {
		if _, err := engine.Step([]Action{{PlayerID: 1, Move: DirectionRight}}); err != nil {
			t.Fatal(err)
		}
	}
	if got := engine.actors[0].Position.Cell(); got != visualWall {
		t.Fatalf("actor did not enter player-passable map element: got %+v, want %+v", got, visualWall)
	}

	actions, err := engine.LegalActions(1)
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range actions {
		if action.PlaceBomb {
			t.Fatalf("placement was legal on occupied static map element: %+v", action)
		}
	}
	events, err := engine.Step([]Action{{PlayerID: 1, PlaceBomb: true}})
	if err != nil {
		t.Fatal(err)
	}
	if len(engine.bombs) != 0 || hasEvent(events, EventBombPlaced, 1) {
		t.Fatalf("placement succeeded on occupied static map element: bombs=%+v events=%+v", engine.bombs, events)
	}
	if !engine.canProduceNativeMovement(engine.actors[0], DirectionLeft) {
		t.Fatal("actor could not leave player-passable map element")
	}
}

func TestObservationPublishesInferredAttributesButKeepsPrivateTimersHidden(t *testing.T) {
	engine := mustEngine(t, testConfig())
	other := &engine.actors[1]
	other.BombCapacity = 7
	other.BombPower = 8
	other.SpeedRate = 9
	other.HeldActions[0] = HeldActionSlot{ActionID: 64, Count: 2}
	other.NativePassActive = true
	other.NativePassDurationMS = 555

	observation, err := engine.Observation(1)
	if err != nil {
		t.Fatal(err)
	}
	if got := observation.Actors[1]; got.BombCapacity != 7 || got.BombPower != 8 || got.SpeedRate != 9 ||
		got.HeldActions[0] != (HeldActionSlot{ActionID: 64, Count: 2}) || got.NativePassActive || got.NativePassDurationMS != 0 {
		t.Fatalf("public attribute/shortcut ledger or private timer boundary is wrong: %+v", got)
	}
	if self := observation.Actors[0]; self.BombCapacity == 0 || self.BombPower == 0 {
		t.Fatalf("self combat state was hidden: %+v", self)
	}
}

func TestVerifiedMovementCheckpointResetsOnlyLocationDependentState(t *testing.T) {
	engine := mustEngine(t, testConfig())
	actor := &engine.actors[0]
	actor.moveRemainder = 777
	actor.NativePassCollisionValid = true
	actor.NativePassCollisionCell = Cell{Row: 1, Col: 2}
	actor.NativePassCollisionStartedAt = 100
	actor.NativePassCollisionLastAt = 500
	actor.NativePassActive = true
	actor.NativePassStartedAt = 501
	actor.NativePassDurationMS = 222

	sameCell := Position{X: actor.Position.X + 1, Y: actor.Position.Y}
	if err := engine.ApplyVerifiedMovementCheckpoint(actor.PlayerID, sameCell); err != nil {
		t.Fatal(err)
	}
	if actor.Position != sameCell || actor.moveRemainder != 0 || !actor.NativePassActive || !actor.NativePassCollisionValid {
		t.Fatalf("same-cell checkpoint damaged native state: %+v", actor)
	}

	otherCell := PositionAtCellCenter(Cell{Row: 1, Col: 2})
	if err := engine.ApplyVerifiedMovementCheckpoint(actor.PlayerID, otherCell); err != nil {
		t.Fatal(err)
	}
	if actor.Position != otherCell || actor.NativePassActive || actor.NativePassCollisionValid ||
		actor.NativePassCollisionStartedAt != 0 || actor.NativePassCollisionLastAt != 0 ||
		actor.NativePassStartedAt != 0 || actor.NativePassDurationMS != 0 {
		t.Fatalf("cell-changing checkpoint kept stale native state: %+v", actor)
	}
	if err := engine.ApplyVerifiedMovementCheckpoint(actor.PlayerID, Position{X: -1, Y: 0}); err == nil {
		t.Fatal("out-of-map movement checkpoint was accepted")
	}
}

func TestVerifiedBombPlacementPreservesRecordedStackAndPower(t *testing.T) {
	engine := mustEngine(t, testConfig())
	cell := engine.actors[0].Position.Cell()
	first, err := engine.ApplyVerifiedBombPlacement(1, cell, 7, 1)
	if err != nil {
		t.Fatal(err)
	}
	second, err := engine.ApplyVerifiedBombPlacement(1, cell, 7, 0)
	if err != nil {
		t.Fatal(err)
	}
	if first.BombID == second.BombID || len(engine.bombs) != 2 || engine.bombs[0].Cell != cell || engine.bombs[1].Cell != cell ||
		engine.bombs[0].Power != 7 || !engine.bombs[0].SceneFourEffect || engine.bombs[1].SceneFourEffect {
		t.Fatalf("verified bomb stack was not retained: events=%+v/%+v bombs=%+v", first, second, engine.bombs)
	}
}

func TestNativeMovementRejectsBlockedAtomicUpdate(t *testing.T) {
	config := testConfig()
	config.Grid.Cells[1*int(config.Grid.Width)+2] = Tile{Kind: CellSolid}
	config.Participants[0].Spawn = Cell{Row: 1, Col: 1}
	config.Participants[0].SpeedPixelsPerSecond = 400
	engine := mustEngine(t, config)
	start := engine.actors[0].Position
	if _, err := engine.Step([]Action{{PlayerID: 1, Move: DirectionRight}}); err != nil {
		t.Fatal(err)
	}
	actor := engine.actors[0]
	if actor.Position != start || actor.NativePassCollisionValid {
		t.Fatalf("blocked native update = %+v, start=%+v", actor, start)
	}
	for step := 0; step < 8; step++ {
		if _, err := engine.Step([]Action{{PlayerID: 1, Move: DirectionRight}}); err != nil {
			t.Fatal(err)
		}
	}
	if engine.actors[0].NativePassActive || engine.actors[0].Position != start {
		t.Fatalf("static wall charged native dynamic pass: %+v", engine.actors[0])
	}
}

func TestBombPlacementRequiresOpenSceneCell(t *testing.T) {
	engine := mustEngine(t, testConfig())
	wall := engine.actors[0].Position.Cell()
	engine.grid.Cells[engine.gridIndex(wall)] = Tile{Kind: CellSolid}
	legal, err := engine.LegalActions(engine.actors[0].PlayerID)
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range legal {
		if action.PlaceBomb {
			t.Fatalf("wall position exposed placement action: %+v", action)
		}
	}
	events, err := engine.Step([]Action{{PlayerID: engine.actors[0].PlayerID, PlaceBomb: true}})
	if err != nil {
		t.Fatal(err)
	}
	if len(engine.bombs) != 0 || hasEvent(events, EventBombPlaced, engine.actors[0].PlayerID) {
		t.Fatalf("wall position accepted placement: bombs=%+v events=%+v", engine.bombs, events)
	}
}

func TestNativeCornerCorrectionUsesSixPixelCentreTolerance(t *testing.T) {
	tests := []struct {
		name     string
		position Position
		move     Direction
		wall     Cell
		want     Position
	}{
		{name: "right slides down from upper blocked corner", position: Position{X: 60, Y: 38}, move: DirectionRight, wall: Cell{Row: 0, Col: 2}, want: Position{X: 60, Y: 46}},
		{name: "right slides up from lower blocked corner", position: Position{X: 60, Y: 42}, move: DirectionRight, wall: Cell{Row: 1, Col: 2}, want: Position{X: 60, Y: 34}},
		{name: "down slides right from left blocked corner", position: Position{X: 38, Y: 60}, move: DirectionDown, wall: Cell{Row: 2, Col: 0}, want: Position{X: 46, Y: 60}},
		{name: "down slides left from right blocked corner", position: Position{X: 42, Y: 60}, move: DirectionDown, wall: Cell{Row: 2, Col: 1}, want: Position{X: 34, Y: 60}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := testConfig()
			config.Grid = testOpenGrid(5, 5)
			config.Grid.Cells[int(test.wall.Row)*int(config.Grid.Width)+int(test.wall.Col)] = Tile{Kind: CellSolid}
			config.Rules.ActorHalfSizePixels = NativeActorHalfSizePixels
			config.Participants[0].Spawn = Cell{Row: 1, Col: 1}
			config.Participants[0].SpeedPixelsPerSecond = 80
			config.Participants[1].Spawn = Cell{Row: 4, Col: 4}
			engine := mustEngine(t, config)
			engine.actors[0].Position = test.position
			if _, err := engine.Step([]Action{{PlayerID: 1, Move: test.move}}); err != nil {
				t.Fatal(err)
			}
			if got := engine.actors[0].Position; got != test.want {
				t.Fatalf("corner-corrected position=%+v, want %+v", got, test.want)
			}
		})
	}
}

func TestNativeCornerCorrectionDoesNotPullFromMiddleOrTwoBlockedCorners(t *testing.T) {
	config := testConfig()
	config.Grid = testOpenGrid(5, 5)
	config.Grid.Cells[0*5+2] = Tile{Kind: CellSolid}
	config.Grid.Cells[1*5+2] = Tile{Kind: CellSolid}
	config.Rules.ActorHalfSizePixels = NativeActorHalfSizePixels
	config.Participants[1].Spawn = Cell{Row: 4, Col: 4}
	engine := mustEngine(t, config)
	engine.actors[0].Position = Position{X: 60, Y: 40}
	if _, err := engine.Step([]Action{{PlayerID: 1, Move: DirectionRight}}); err != nil {
		t.Fatal(err)
	}
	if got := engine.actors[0].Position; got != (Position{X: 60, Y: 40}) {
		t.Fatalf("two blocked leading corners caused correction: %+v", got)
	}

	config.Grid.Cells[1*5+2] = Tile{Kind: CellOpen, FlamePassable: true}
	engine = mustEngine(t, config)
	engine.actors[0].Position = Position{X: 60, Y: 20}
	if _, err := engine.Step([]Action{{PlayerID: 1, Move: DirectionRight}}); err != nil {
		t.Fatal(err)
	}
	if got := engine.actors[0].Position; got != (Position{X: 60, Y: 20}) {
		t.Fatalf("middle-of-cell collision caused six-pixel correction: %+v", got)
	}
}

func TestNativeCornerCorrectionSixPixelBoundaryIsInclusive(t *testing.T) {
	tests := []struct {
		name       string
		positionY  int32
		wallRow    int
		wantMovedY int32
	}{
		{name: "low edge six corrects", positionY: 46, wallRow: 1, wantMovedY: 38},
		{name: "low edge seven does not", positionY: 47, wallRow: 1, wantMovedY: 47},
		{name: "high edge six corrects", positionY: 34, wallRow: 0, wantMovedY: 42},
		{name: "high edge seven does not", positionY: 33, wallRow: 0, wantMovedY: 33},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := testConfig()
			config.Grid = testOpenGrid(5, 5)
			config.Grid.Cells[test.wallRow*5+2] = Tile{Kind: CellSolid}
			config.Rules.ActorHalfSizePixels = NativeActorHalfSizePixels
			config.Participants[0].SpeedPixelsPerSecond = 80
			config.Participants[1].Spawn = Cell{Row: 4, Col: 4}
			engine := mustEngine(t, config)
			engine.actors[0].Position = Position{X: 60, Y: test.positionY}
			if _, err := engine.Step([]Action{{PlayerID: 1, Move: DirectionRight}}); err != nil {
				t.Fatal(err)
			}
			if got := engine.actors[0].Position; got != (Position{X: 60, Y: test.wantMovedY}) {
				t.Fatalf("position=%+v, want X=60 Y=%d", got, test.wantMovedY)
			}
		})
	}
}

func TestLegalActionsIncludeNativeCornerCorrectionDirection(t *testing.T) {
	config := testConfig()
	config.Grid = testOpenGrid(5, 5)
	config.Grid.Cells[0*5+2] = Tile{Kind: CellSolid}
	config.Rules.ActorHalfSizePixels = NativeActorHalfSizePixels
	config.Participants[0].SpeedPixelsPerSecond = 80
	config.Participants[1].Spawn = Cell{Row: 4, Col: 4}
	engine := mustEngine(t, config)
	engine.actors[0].Position = Position{X: 60, Y: 34}
	actions, err := engine.LegalActions(1)
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range actions {
		if action.Move == DirectionRight && !action.PlaceBomb && action.UseActionID == 0 {
			return
		}
	}
	t.Fatalf("native corner-correction direction missing from legal actions: %+v", actions)
}

func TestLegalActionsUseCompleteNativeTickAtCollisionEdge(t *testing.T) {
	config := testConfig()
	config.Grid = testOpenGrid(5, 5)
	config.Grid.Cells[1*5+2] = Tile{Kind: CellSolid}
	config.Rules.TickMS = 20
	config.Rules.ActorHalfSizePixels = NativeActorHalfSizePixels
	config.Participants[0].SpeedPixelsPerSecond = 204
	config.Participants[1].Spawn = Cell{Row: 4, Col: 4}
	engine := mustEngine(t, config)
	actor := &engine.actors[0]
	actor.Position = Position{X: 100, Y: 100}

	// One pixel still leaves the upper leading edge in row 2, while the real
	// four-pixel update reaches blocked row 1. The former one-pixel legality
	// shortcut exposed Up even though every authoritative update stayed put.
	if !engine.positionWalkable(&Actor{Position: actor.Position}, Position{X: 100, Y: 99}, DirectionUp) {
		t.Fatal("test precondition: one-pixel shortcut is not open")
	}
	if engine.canProduceNativeMovement(*actor, DirectionUp) {
		t.Fatal("full-tick blocked direction was exposed as a productive AI move")
	}
	actions, err := engine.LegalActions(actor.PlayerID)
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range actions {
		if action.Move == DirectionUp {
			t.Fatalf("full-tick blocked Up action leaked through LegalActions: %+v", action)
		}
	}
}

func TestLegalActionsReachWallCenterWithoutWaitingForShorterPhase(t *testing.T) {
	config := testConfig()
	config.Grid = testOpenGrid(5, 5)
	config.Grid.Cells[1*5+2] = Tile{Kind: CellSolid}
	config.Rules.TickMS = 20
	config.Rules.ActorHalfSizePixels = NativeActorHalfSizePixels
	config.Participants[0].SpeedPixelsPerSecond = 176
	config.Participants[1].Spawn = Cell{Row: 4, Col: 4}
	engine := mustEngine(t, config)
	actor := &engine.actors[0]
	actor.Position = Position{X: 57, Y: 60}
	actor.moveRemainder = 800

	probe := *actor
	if distance := engine.consumeNativeMovementDistance(&probe, DirectionRight, config.Rules.TickMS); distance != 4 {
		t.Fatalf("first movement distance=%d, want 4", distance)
	}
	if !engine.resolveNativeMovementDisplacement(&probe, DirectionRight, 4, true) || probe.Position.X != 60 {
		t.Fatalf("four-pixel phase did not clamp at the native wall centre: %+v", probe.Position)
	}
	if distance := engine.consumeNativeMovementDistance(&probe, DirectionRight, config.Rules.TickMS); distance != 3 {
		t.Fatalf("second movement distance=%d, want 3", distance)
	}
	if engine.resolveNativeMovementDisplacement(&probe, DirectionRight, 3, true) || probe.Position.X != 60 {
		t.Fatalf("subsequent phase crossed the wall centre: %+v", probe.Position)
	}
	if !engine.canProduceNativeMovement(*actor, DirectionRight) {
		t.Fatal("productive native centre clamp was rejected by LegalActions")
	}
}

func TestLegalActionCollisionGridMatrixUsesNativeResolver(t *testing.T) {
	newEngine := func() *Engine {
		config := testConfig()
		config.Grid = testOpenGrid(5, 5)
		config.Rules.TickMS = 20
		config.Rules.ActorHalfSizePixels = NativeActorHalfSizePixels
		config.Participants[0].SpeedPixelsPerSecond = 80
		config.Participants[1].Spawn = Cell{Row: 4, Col: 4}
		engine := mustEngine(t, config)
		engine.actors[0].Position = PositionAtCellCenter(Cell{Row: 2, Col: 2})
		return engine
	}
	assertProductive := func(name string, engine *Engine, direction Direction, want bool) {
		t.Helper()
		if got := engine.canProduceNativeMovement(engine.actors[0], direction); got != want {
			t.Fatalf("%s productive=%t, want %t", name, got, want)
		}
	}

	engine := newEngine()
	assertProductive("open cell", engine, DirectionRight, true)

	engine = newEngine()
	engine.actors[0].Position.X = int32(NativeActorHalfSizePixels)
	assertProductive("map boundary", engine, DirectionLeft, false)

	engine = newEngine()
	engine.grid.Cells[engine.gridIndex(Cell{Row: 2, Col: 3})] = Tile{Kind: CellSolid}
	assertProductive("solid cell", engine, DirectionRight, false)

	engine = newEngine()
	engine.grid.Cells[engine.gridIndex(Cell{Row: 2, Col: 3})] = Tile{Kind: CellBreakable, Durability: 1}
	assertProductive("breakable cell", engine, DirectionRight, false)

	engine = newEngine()
	engine.grid.Cells[engine.gridIndex(Cell{Row: 2, Col: 3})] = Tile{Kind: CellOpen, MapElementOccupied: true}
	engine.pickups = []Pickup{{SceneID: SceneSpeedSmall, Cell: Cell{Row: 2, Col: 3}, State: PickupAvailable}}
	assertProductive("player-passable element and pickup", engine, DirectionRight, true)

	engine = newEngine()
	engine.grid.Cells[engine.gridIndex(Cell{Row: 2, Col: 3})] = Tile{
		Kind: CellSolid, MapElementID: 9012, NormalPushable: true,
		ElementWidth: 1, ElementHeight: 1, ElementAnchor: Cell{Row: 2, Col: 3},
	}
	assertProductive("source-confirmed map-element push", engine, DirectionRight, true)

	engine = newEngine()
	engine.bombs = []Bomb{{ID: 1, Cell: Cell{Row: 2, Col: 3}, ExplodeAtMS: 10_000}}
	assertProductive("aligned bubble charge with open exit", engine, DirectionRight, true)
	engine.grid.Cells[engine.gridIndex(Cell{Row: 2, Col: 4})] = Tile{Kind: CellSolid}
	assertProductive("bubble with blocked exit", engine, DirectionRight, false)

	engine = newEngine()
	engine.actors[0].Position.Y = 80
	engine.bombs = []Bomb{{ID: 1, Cell: Cell{Row: 2, Col: 3}, ExplodeAtMS: 10_000}}
	assertProductive("one-corner contact can squeeze into open lane", engine, DirectionRight, true)
	engine.grid.Cells[engine.gridIndex(Cell{Row: 1, Col: 2})] = Tile{Kind: CellSolid}
	assertProductive("one-corner contact with blocked squeeze lane", engine, DirectionRight, false)

	engine = newEngine()
	engine.installTransformation(&engine.actors[0], mustNativeTransformation(t, 109))
	engine.bombs = []Bomb{{ID: 1, Cell: Cell{Row: 2, Col: 3}, ExplodeAtMS: 10_000}}
	assertProductive("source-confirmed bomb kick", engine, DirectionRight, true)
}

func TestLegalActionsPreserveOnlyAlignedProductiveNativePassCharge(t *testing.T) {
	config := testConfig()
	config.Grid = testOpenGrid(5, 3)
	config.Rules.TickMS = 20
	config.Rules.ActorHalfSizePixels = NativeActorHalfSizePixels
	config.Participants[0].Spawn = Cell{Row: 1, Col: 0}
	config.Participants[0].SpeedPixelsPerSecond = 80
	config.Participants[1].Spawn = Cell{Row: 2, Col: 4}
	engine := mustEngine(t, config)
	actor := &engine.actors[0]
	engine.bombs = []Bomb{{ID: 1, Cell: Cell{Row: 1, Col: 1}, ExplodeAtMS: 10_000}}
	if !engine.canProduceNativeMovement(*actor, DirectionRight) {
		t.Fatal("aligned type-1 charge with an open exit was removed from the action space")
	}

	actor.NativePassCollisionValid = true
	actor.NativePassCollisionCell = Cell{Row: 1, Col: 1}
	actor.NativePassCollisionStartedAt = 0
	actor.NativePassCollisionLastAt = 580
	engine.elapsedMS = NativePassChargeMaxMS
	if engine.canProduceNativeMovement(*actor, DirectionRight) {
		t.Fatal("expired same-cell native-pass charge remained productive forever")
	}
}

func TestLegalActionsAreStableAndRespectImmediateCollision(t *testing.T) {
	engine := mustEngine(t, testConfig())
	engine.actors[0].Position = Position{X: 60, Y: 10}
	actions, err := engine.LegalActions(1)
	if err != nil {
		t.Fatal(err)
	}
	want := []Action{
		{PlayerID: 1},
		{PlayerID: 1, Move: DirectionRight},
		{PlayerID: 1, Move: DirectionDown},
		{PlayerID: 1, Move: DirectionLeft},
		{PlayerID: 1, PlaceBomb: true},
		{PlayerID: 1, Move: DirectionRight, PlaceBomb: true},
		{PlayerID: 1, Move: DirectionDown, PlaceBomb: true},
		{PlayerID: 1, Move: DirectionLeft, PlaceBomb: true},
	}
	if !reflect.DeepEqual(actions, want) {
		t.Fatalf("legal actions = %+v, want %+v", actions, want)
	}
}

func TestVirtualActorsNeverBecomeArbitrators(t *testing.T) {
	config := testConfig()
	config.Participants = []Participant{
		testParticipant(1, 1, ParticipantHuman, Cell{Row: 1, Col: 1}),
		testParticipant(2, 2, ParticipantVirtualAI, Cell{Row: 1, Col: 3}),
		testParticipant(3, 2, ParticipantHuman, Cell{Row: 2, Col: 3}),
	}
	engine := mustEngine(t, config)
	if got, want := engine.EligibleArbitratorIDs(), []uint16{1, 3}; !reflect.DeepEqual(got, want) {
		t.Fatalf("eligible arbitrators = %v, want %v", got, want)
	}
	if selected, err := engine.SelectArbitrator(2); err != nil || selected != 1 {
		t.Fatalf("virtual preferred arbitrator selected %d, err=%v", selected, err)
	}
	if selected, err := engine.SelectArbitrator(3); err != nil || selected != 3 {
		t.Fatalf("human preferred arbitrator selected %d, err=%v", selected, err)
	}
	engine.actors[0].State = ActorEliminated
	engine.actors[2].State = ActorEliminated
	if got := engine.EligibleArbitratorIDs(); len(got) != 0 {
		t.Fatalf("AI-only survivors produced arbitrators %v", got)
	}
	if selected, err := engine.SelectArbitrator(2); err == nil || selected != 0 {
		t.Fatalf("AI was selected without a living human: selected=%d err=%v", selected, err)
	}
}

func TestParticipantAndTeamValidation(t *testing.T) {
	config := testConfig()
	config.Participants = config.Participants[:1]
	if _, err := New(config); err == nil {
		t.Fatal("single participant was accepted")
	}
	config = testConfig()
	for id := uint16(3); id <= 9; id++ {
		config.Participants = append(config.Participants, testParticipant(id, byte(id%2+1), ParticipantVirtualAI, Cell{Row: int16(id % 3), Col: int16(id % 5)}))
	}
	if _, err := New(config); err == nil {
		t.Fatal("nine participants were accepted")
	}
	config = testConfig()
	config.Participants[1].TeamID = config.Participants[0].TeamID
	if _, err := New(config); err == nil {
		t.Fatal("single-team battle was accepted")
	}
}

func TestEightParticipantsAndMultipleTeamsAreSupported(t *testing.T) {
	config := testConfig()
	config.Grid = testOpenGrid(8, 2)
	config.Participants = make([]Participant, 0, MaxParticipants)
	for index := 0; index < MaxParticipants; index++ {
		source := ParticipantVirtualAI
		if index == 0 || index == 4 {
			source = ParticipantHuman
		}
		config.Participants = append(config.Participants, testParticipant(
			uint16(index+1), byte(index%4+1), source, Cell{Row: int16(index / 4), Col: int16(index % 4)},
		))
	}
	engine := mustEngine(t, config)
	if got := len(engine.Actors()); got != MaxParticipants {
		t.Fatalf("actor count = %d, want %d", got, MaxParticipants)
	}
	if got, want := engine.EligibleArbitratorIDs(), []uint16{1, 5}; !reflect.DeepEqual(got, want) {
		t.Fatalf("8-player eligible arbitrators = %v, want %v", got, want)
	}
}

func TestMultipleTeamsEndOnlyWhenOneTeamRemains(t *testing.T) {
	config := testConfig()
	config.Participants = []Participant{
		testParticipant(1, 1, ParticipantHuman, Cell{Row: 0, Col: 0}),
		testParticipant(2, 2, ParticipantHuman, Cell{Row: 0, Col: 1}),
		testParticipant(3, 3, ParticipantVirtualAI, Cell{Row: 0, Col: 2}),
		testParticipant(4, 3, ParticipantVirtualAI, Cell{Row: 0, Col: 3}),
	}
	engine := mustEngine(t, config)
	engine.actors[0].State = ActorEliminated
	if event, ended := engine.evaluateTerminal(); ended {
		t.Fatalf("two remaining teams ended prematurely: %+v", event)
	}
	engine.actors[1].State = ActorEliminated
	event, ended := engine.evaluateTerminal()
	if !ended || engine.outcome.Draw || engine.outcome.WinnerTeamID != 3 || event.TeamID != 3 {
		t.Fatalf("multi-team terminal event=%+v outcome=%+v", event, engine.outcome)
	}
}

func TestResetRebuildsInitialState(t *testing.T) {
	config := testConfig()
	engine := mustEngine(t, config)
	if _, err := engine.Step([]Action{{PlayerID: 1, Move: DirectionRight, PlaceBomb: true}}); err != nil {
		t.Fatal(err)
	}
	if err := engine.Reset(config); err != nil {
		t.Fatal(err)
	}
	fresh := mustEngine(t, config)
	if !reflect.DeepEqual(engine, fresh) {
		t.Fatalf("reset state differs from fresh engine:\nreset=%+v\nfresh=%+v", engine, fresh)
	}
}

func TestProjectNativeMovementUsesCollisionResolverWithoutMutatingEngine(t *testing.T) {
	config := testConfig()
	config.Rules.TickMS = 20
	engine := mustEngine(t, config)
	start := engine.actors[0].Position
	projection, err := engine.ProjectNativeMovement(1, DirectionRight)
	if err != nil {
		t.Fatal(err)
	}
	if projection.End.X <= start.X || projection.End.Y != start.Y {
		t.Fatalf("open projection = %+v, want rightward from %+v", projection, start)
	}
	if engine.actors[0].Position != start {
		t.Fatalf("projection mutated authoritative position to %+v", engine.actors[0].Position)
	}

	engine.grid.Cells[engine.gridIndex(Cell{Row: 1, Col: 2})] = Tile{Kind: CellSolid}
	projection, err = engine.ProjectNativeMovement(1, DirectionRight)
	if err != nil {
		t.Fatal(err)
	}
	if projection.End.X != 68 || projection.End.Y != start.Y {
		t.Fatalf("native wall-bounded projection = %+v, want last whole-step end 68,%d", projection, start.Y)
	}
	if projection.HasCorner {
		t.Fatalf("flat wall unexpectedly produced corner %+v", projection.Corner)
	}
	engine.actors[0].SpeedPixelsPerSecond = 40
	projection, err = engine.ProjectNativeMovement(1, DirectionRight)
	if err != nil {
		t.Fatal(err)
	}
	if projection.End.X != 70 {
		t.Fatalf("native trailing-nine normalization = %+v, want X=70", projection)
	}
}

func TestNativeMovementAndProjectionReachSameWallCenter(t *testing.T) {
	// session-20260905-071016, game 3/player 20005: the authority stayed at
	// (338,220), but every moving heartbeat advertised (340,220). The native
	// resolver clamps the final update at a walkable cell centre. Both the
	// authoritative 20 ms update and the 25 ms producer must use that branch.
	config := testConfig()
	config.Grid = testOpenGrid(15, 13)
	config.Rules.TickMS = 20
	config.Rules.ActorHalfSizePixels = NativeActorHalfSizePixels
	config.Grid.Cells[5*15+9] = Tile{Kind: CellSolid}
	config.Participants[0].SpeedPixelsPerSecond = 180
	engine := mustEngine(t, config)
	engine.actors[0].Position = Position{X: 338, Y: 220}
	before := engine.Clone()
	projection, err := engine.ProjectNativeMovement(1, DirectionRight)
	if err != nil {
		t.Fatal(err)
	}
	if projection.End != (Position{X: 340, Y: 220}) || projection.HasCorner {
		t.Fatalf("wall projection = %+v, want centre (340,220)", projection)
	}
	if !reflect.DeepEqual(engine, before) {
		t.Fatal("projection mutated authoritative state")
	}
	for step := 0; step < 8; step++ {
		if _, err := engine.Step([]Action{{PlayerID: 1, Move: DirectionRight}}); err != nil {
			t.Fatal(err)
		}
		if engine.actors[0].Position != projection.End {
			t.Fatalf("authority moved beyond blocked endpoint: %+v", engine.actors[0].Position)
		}
	}
}

func TestNativeBombCornerSweepPreservesFirstCollision(t *testing.T) {
	// The 17:18 live match advertised (540,23) as an endpoint while its 20 ms
	// actor could turn the corner to Y=20. A 25 ms probe crossed the bomb's
	// three-pixel entry strip, then classified its final point as collision-free
	// and lost the perpendicular branch. Exercise the same boundary on all axes.
	for _, tc := range []struct {
		name      string
		direction Direction
		start     Position
		bomb      Cell
		corner    Position
		end       Position
	}{
		{"right", DirectionRight, Position{X: 100, Y: 63}, Cell{Row: 2, Col: 3}, Position{X: 100, Y: 63}, Position{X: 180, Y: 60}},
		{"left", DirectionLeft, Position{X: 100, Y: 63}, Cell{Row: 2, Col: 1}, Position{X: 100, Y: 63}, Position{X: 20, Y: 60}},
		{"down", DirectionDown, Position{X: 63, Y: 100}, Cell{Row: 3, Col: 2}, Position{X: 63, Y: 100}, Position{X: 60, Y: 180}},
		{"up", DirectionUp, Position{X: 63, Y: 100}, Cell{Row: 1, Col: 2}, Position{X: 63, Y: 100}, Position{X: 60, Y: 20}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := testConfig()
			config.Grid = testOpenGrid(5, 5)
			config.Rules.TickMS = 20
			config.Rules.ActorHalfSizePixels = NativeActorHalfSizePixels
			config.Participants[0].SpeedPixelsPerSecond = 232
			engine := mustEngine(t, config)
			engine.actors[0].Position = tc.start
			engine.bombs = []Bomb{{ID: 1, OwnerID: 2, Cell: tc.bomb, ExplodeAtMS: 9000}}
			projection, err := engine.ProjectNativeMovement(1, tc.direction)
			if err != nil {
				t.Fatal(err)
			}
			if !projection.HasCorner || projection.Corner != tc.corner || projection.End != tc.end {
				t.Fatalf("corner projection = %+v, want corner %+v end %+v", projection, tc.corner, tc.end)
			}
			for step := 0; step < 40; step++ {
				engine.moveActorInWorldDirection(0, tc.direction)
			}
			if got := engine.actors[0].Position; got != projection.End {
				t.Fatalf("held physical path ended at %+v, projected %+v", got, projection.End)
			}
		})
	}
}

func testConfig() Config {
	return Config{
		Seed: 1,
		Grid: testOpenGrid(5, 3),
		Rules: Rules{
			TickMS: 100, RoundDurationMS: 10_000, BombFuseMS: 300,
			FlameDurationMS: 200, TrapDurationMS: 500, ActorHalfSizePixels: 10,
		},
		Participants: []Participant{
			testParticipant(1, 1, ParticipantHuman, Cell{Row: 1, Col: 1}),
			testParticipant(2, 2, ParticipantHuman, Cell{Row: 1, Col: 3}),
		},
	}
}

func testOpenGrid(width, height uint16) Grid {
	grid := Grid{Width: width, Height: height, Cells: make([]Tile, int(width)*int(height))}
	for index := range grid.Cells {
		grid.Cells[index].FlamePassable = true
	}
	return grid
}

func testParticipant(playerID uint16, teamID byte, source ParticipantSource, spawn Cell) Participant {
	return Participant{
		PlayerID: playerID, TeamID: teamID, Source: source, Spawn: spawn,
		SpeedPixelsPerSecond: 80, BombCapacity: 2, BombPower: 2,
	}
}

func mustEngine(t *testing.T, config Config) *Engine {
	t.Helper()
	engine, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	return engine
}
