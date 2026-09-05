package battleengine

import (
	"testing"

	"qqtang/internal/clientdata/sceneelement"
)

func TestPandaPushMovesWholeNativeElementAndHiddenPickupsOneCell(t *testing.T) {
	config := testConfig()
	config.Rules.TickMS = NativeMapElementPushPulseMS
	config.Participants[0].Spawn = Cell{Row: 2, Col: 1}
	config.Participants[1].Spawn = Cell{Row: 0, Col: 4}
	for col := int16(1); col <= 2; col++ {
		config.Grid.Cells[1*int(config.Grid.Width)+int(col)] = Tile{
			Kind: CellBreakable, Durability: 1, MapElementID: 9011, PandaPushable: true,
			ElementWidth: 2, ElementHeight: 1, ElementAnchor: Cell{Row: 1, Col: 1},
		}
	}
	config.Pickups = []Pickup{
		{SceneID: SceneBombCapacitySmall, Cell: Cell{Row: 1, Col: 1}, State: PickupHidden},
		{SceneID: SceneBombPowerSmall, Cell: Cell{Row: 1, Col: 2}, State: PickupHidden},
	}
	engine := mustEngine(t, config)
	engine.installTransformation(&engine.actors[0], mustNativeTransformation(t, 109))
	engine.actors[0].Position = PositionAtCellCenter(Cell{Row: 2, Col: 1})

	var events []Event
	for contact := 1; contact <= 7; contact++ {
		stepEvents, err := engine.Step([]Action{{PlayerID: 1, Move: DirectionUp}})
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, stepEvents...)
		if contact < 7 && hasEvent(stepEvents, EventMapElementMoved, 0) {
			t.Fatalf("panda push confirmed after only %d contact pulses", contact)
		}
	}
	oldAnchor, _ := engine.grid.Cell(Cell{Row: 1, Col: 1})
	newAnchor, _ := engine.grid.Cell(Cell{Row: 0, Col: 1})
	newTail, _ := engine.grid.Cell(Cell{Row: 0, Col: 2})
	if oldAnchor.Kind != CellOpen || !newAnchor.PandaPushable || !newTail.PandaPushable ||
		newAnchor.MapElementID != 9011 || newTail.ElementAnchor != (Cell{Row: 0, Col: 1}) {
		t.Fatalf("pushed grid old=%+v new=%+v/%+v", oldAnchor, newAnchor, newTail)
	}
	if engine.pickups[0].Cell != (Cell{Row: 0, Col: 1}) || engine.pickups[1].Cell != (Cell{Row: 0, Col: 2}) {
		t.Fatalf("hidden pickups did not follow element: %+v", engine.pickups)
	}
	if !hasEvent(events, EventMapElementMoved, 0) || engine.actors[0].Position.Y >= PositionAtCellCenter(Cell{Row: 2, Col: 1}).Y {
		t.Fatalf("panda push events/actor = %+v/%+v", events, engine.actors[0])
	}
}

func TestNormalActorUsesCanMovePushPredicateAndNativeContactCounter(t *testing.T) {
	config := testConfig()
	config.Rules.TickMS = NativeMapElementPushPulseMS
	config.Participants[1].Spawn = Cell{Row: 0, Col: 4}
	config.Grid.Cells[1*int(config.Grid.Width)+2] = Tile{
		Kind: CellSolid, MapElementID: 9012, NormalPushable: true,
		ElementWidth: 1, ElementHeight: 1, ElementAnchor: Cell{Row: 1, Col: 2},
	}
	engine := mustEngine(t, config)
	engine.actors[0].Position = PositionAtCellCenter(Cell{Row: 1, Col: 1})

	for contact := 1; contact <= 6; contact++ {
		events, err := engine.Step([]Action{{PlayerID: 1, Move: DirectionRight}})
		if err != nil {
			t.Fatal(err)
		}
		if hasEvent(events, EventMapElementMoved, 0) || engine.actors[0].Position != PositionAtCellCenter(Cell{Row: 1, Col: 1}) {
			t.Fatalf("contact %d moved early: events=%+v actor=%+v", contact, events, engine.actors[0])
		}
		tile, _ := engine.grid.Cell(Cell{Row: 1, Col: 2})
		if tile.PushCounter != byte(contact*2) {
			t.Fatalf("contact %d counter=%d", contact, tile.PushCounter)
		}
	}
	events, err := engine.Step([]Action{{PlayerID: 1, Move: DirectionRight}})
	if err != nil {
		t.Fatal(err)
	}
	movedAnchor, _ := engine.grid.Cell(Cell{Row: 1, Col: 3})
	if !hasEvent(events, EventMapElementMoved, 0) || movedAnchor.MapElementID != 9012 ||
		movedAnchor.PushCounter != 0 ||
		engine.actors[0].Position.X <= PositionAtCellCenter(Cell{Row: 1, Col: 1}).X {
		t.Fatalf("seventh contact did not confirm normal push: events=%+v moved=%+v actor=%+v", events, movedAnchor, engine.actors[0])
	}
}

func TestNormalActorCannotChargeTwoCellElementAlongItsOccupiedLongAxis(t *testing.T) {
	config := testConfig()
	config.Rules.TickMS = NativeMapElementPushPulseMS
	config.Participants[1].Spawn = Cell{Row: 0, Col: 4}
	for col := int16(2); col <= 3; col++ {
		config.Grid.Cells[1*int(config.Grid.Width)+int(col)] = Tile{
			Kind: CellSolid, MapElementOccupied: true, MapElementID: 9008, NormalPushable: true,
			ElementWidth: 2, ElementHeight: 1, ElementAnchor: Cell{Row: 1, Col: 2},
		}
	}
	engine := mustEngine(t, config)
	engine.actors[0].Position = PositionAtCellCenter(Cell{Row: 1, Col: 1})
	for contact := 0; contact < 8; contact++ {
		if _, err := engine.Step([]Action{{PlayerID: 1, Move: DirectionRight}}); err != nil {
			t.Fatal(err)
		}
	}
	anchor, _ := engine.grid.Cell(Cell{Row: 1, Col: 2})
	if anchor.ElementAnchor != (Cell{Row: 1, Col: 2}) || anchor.PushCounter != 0 {
		t.Fatalf("long-axis native producer gate was bypassed: %+v", anchor)
	}
}

func TestMapElementPushDoesNotChargeBeforeNativeTwentyTwoPixelProbeTouches(t *testing.T) {
	config := testConfig()
	config.Rules.TickMS = NativeMapElementPushPulseMS
	config.Participants[1].Spawn = Cell{Row: 0, Col: 4}
	config.Grid.Cells[1*int(config.Grid.Width)+2] = Tile{
		Kind: CellSolid, MapElementID: 9012, NormalPushable: true,
		ElementWidth: 1, ElementHeight: 1, ElementAnchor: Cell{Row: 1, Col: 2},
	}
	engine := mustEngine(t, config)
	engine.actors[0].Position = Position{X: 50, Y: 60}
	if event, handled := engine.tryActorWorldInteraction(0, DirectionRight); handled || event.Kind != 0 {
		t.Fatalf("distant same-cell actor charged push: event=%+v handled=%t", event, handled)
	}
	engine.actors[0].Position.X = 58
	if _, handled := engine.tryActorWorldInteraction(0, DirectionRight); !handled {
		t.Fatal("22-pixel native contact probe did not reach adjacent element")
	}
}

func TestMapElementPushTargetPickupBlocksProducerInTrainingEngine(t *testing.T) {
	config := testConfig()
	config.Rules.TickMS = NativeMapElementPushPulseMS
	config.Participants[1].Spawn = Cell{Row: 0, Col: 4}
	config.Grid.Cells[1*int(config.Grid.Width)+2] = Tile{
		Kind: CellSolid, MapElementID: 9012, NormalPushable: true,
		ElementWidth: 1, ElementHeight: 1, ElementAnchor: Cell{Row: 1, Col: 2},
	}
	config.Pickups = []Pickup{{
		SceneID: SceneBombPowerSmall,
		Cell:    Cell{Row: 1, Col: 3},
		State:   PickupAvailable,
	}}
	engine := mustEngine(t, config)
	engine.actors[0].Position = PositionAtCellCenter(Cell{Row: 1, Col: 1})

	for contact := 0; contact < 8; contact++ {
		events, err := engine.Step([]Action{{PlayerID: 1, Move: DirectionRight}})
		if err != nil {
			t.Fatal(err)
		}
		if hasEvent(events, EventMapElementMoved, 0) {
			t.Fatalf("pickup-blocked target produced a push on contact %d: %+v", contact+1, events)
		}
	}
	source, _ := engine.grid.Cell(Cell{Row: 1, Col: 2})
	if source.PushCounter != 0 || source.ElementAnchor != (Cell{Row: 1, Col: 2}) {
		t.Fatalf("pickup-blocked target charged or moved the element: %+v", source)
	}
}

func TestNormalActorCannotPushPandaLifetimeOnlyElement(t *testing.T) {
	config := testConfig()
	config.Rules.TickMS = NativeMapElementPushPulseMS
	config.Grid.Cells[1*int(config.Grid.Width)+2] = Tile{
		Kind: CellBreakable, Durability: 1, MapElementID: 9011, PandaPushable: true,
		ElementWidth: 1, ElementHeight: 1, ElementAnchor: Cell{Row: 1, Col: 2},
	}
	engine := mustEngine(t, config)
	engine.actors[0].Position = PositionAtCellCenter(Cell{Row: 1, Col: 1})

	for contact := 0; contact < 8; contact++ {
		events, err := engine.Step([]Action{{PlayerID: 1, Move: DirectionRight}})
		if err != nil {
			t.Fatal(err)
		}
		if hasEvent(events, EventMapElementMoved, 0) {
			t.Fatalf("normal actor moved Panda-only element: %+v", events)
		}
	}
	tile, _ := engine.grid.Cell(Cell{Row: 1, Col: 2})
	if tile.MapElementID != 9011 || tile.PushCounter != 0 {
		t.Fatalf("Panda-only element changed under normal actor: %+v", tile)
	}
}

func TestPandaKickMovesBombToFarthestNativeStaticOpening(t *testing.T) {
	config := testConfig()
	config.Participants[1].Spawn = Cell{Row: 0, Col: 4}
	config.Grid.Cells[1*int(config.Grid.Width)+4] = Tile{Kind: CellSolid}
	engine := mustEngine(t, config)
	engine.installTransformation(&engine.actors[0], mustNativeTransformation(t, 109))
	engine.actors[0].Position = Position{X: 69, Y: 60}
	engine.bombs = []Bomb{{ID: 1, OwnerID: 2, Cell: Cell{Row: 1, Col: 2}, Power: 2, ExplodeAtMS: 2_000}}
	engine.nextBombID = 2

	events, err := engine.Step([]Action{{PlayerID: 1, Move: DirectionRight}})
	if err != nil {
		t.Fatal(err)
	}
	if engine.bombs[0].Cell != (Cell{Row: 1, Col: 3}) || engine.bombs[0].ExplodeAtMS != 2_000 ||
		engine.bombs[0].FlightUntilMS != NativeBombFlightMS {
		t.Fatalf("kicked bomb = %+v", engine.bombs[0])
	}
	if !hasEvent(events, EventBombKicked, 0) || engine.actors[0].Position.X <= 69 {
		t.Fatalf("panda kick events/actor = %+v/%+v", events, engine.actors[0])
	}
}

func TestNativeAuthorityVirtualPandaKickWaitsForArbitrator(t *testing.T) {
	config := testConfig()
	config.Rules.NativeOutcomeAuthority = true
	config.Participants[0].Source = ParticipantVirtualAI
	config.Participants[1].Spawn = Cell{Row: 0, Col: 4}
	config.Grid.Cells[1*int(config.Grid.Width)+4] = Tile{Kind: CellSolid}
	engine := mustEngine(t, config)
	engine.installTransformation(&engine.actors[0], mustNativeTransformation(t, 109))
	engine.actors[0].Position = Position{X: 69, Y: 60}
	source := Cell{Row: 1, Col: 2}
	target := Cell{Row: 1, Col: 3}
	engine.bombs = []Bomb{{ID: 1, OwnerID: 2, Cell: source, Power: 2, ExplodeAtMS: 2_000}}
	engine.nextBombID = 2

	events, err := engine.Step([]Action{{PlayerID: 1, Move: DirectionRight}})
	if err != nil {
		t.Fatal(err)
	}
	foundRequest := false
	for _, event := range events {
		if event.Kind == EventBombKickRequested && event.BombID == 1 && event.FromCell == source && event.Cell == target {
			foundRequest = true
		}
	}
	if !foundRequest || engine.bombs[0].Cell != source {
		t.Fatalf("virtual kick request mutated bomb: events=%+v bomb=%+v", events, engine.bombs[0])
	}
	if _, err = engine.ApplyVerifiedBombMovement(1, 1, source, target); err != nil {
		t.Fatal(err)
	}
	if engine.bombs[0].Cell != target {
		t.Fatalf("arbitrator kick confirmation did not commit: %+v", engine.bombs[0])
	}
}

func TestNativeAuthorityVirtualMapPushWaitsForArbitrator(t *testing.T) {
	config := testConfig()
	config.Rules.TickMS = NativeMapElementPushPulseMS
	config.Rules.NativeOutcomeAuthority = true
	config.Participants[0].Source = ParticipantVirtualAI
	config.Participants[1].Spawn = Cell{Row: 0, Col: 4}
	config.Grid.Cells[1*int(config.Grid.Width)+2] = Tile{
		Kind: CellSolid, MapElementID: 9012, NormalPushable: true,
		ElementWidth: 1, ElementHeight: 1, ElementAnchor: Cell{Row: 1, Col: 2},
	}
	engine := mustEngine(t, config)
	engine.actors[0].Position = PositionAtCellCenter(Cell{Row: 1, Col: 1})
	var request Event
	for contact := 0; contact < 7; contact++ {
		events, err := engine.Step([]Action{{PlayerID: 1, Move: DirectionRight}})
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range events {
			if event.Kind == EventMapElementMoveRequested {
				request = event
			}
		}
	}
	oldAnchor, _ := engine.grid.Cell(Cell{Row: 1, Col: 2})
	if request.Kind == 0 || oldAnchor.MapElementID != 9012 {
		t.Fatalf("virtual map request mutated element: request=%+v old=%+v", request, oldAnchor)
	}
	if _, err := engine.ApplyVerifiedMapElementMovement(1, 9012, Cell{Row: 1, Col: 2}, DirectionRight); err != nil {
		t.Fatal(err)
	}
	newAnchor, _ := engine.grid.Cell(Cell{Row: 1, Col: 3})
	if newAnchor.MapElementID != 9012 {
		t.Fatalf("arbitrator map confirmation did not commit: %+v", newAnchor)
	}
}

func TestPandaThrownBombKeepsFuseButCannotExplodeBeforeLanding(t *testing.T) {
	config := testConfig()
	config.Rules.TickMS = 100
	engine := mustEngine(t, config)
	engine.bombs = []Bomb{{ID: 1, OwnerID: 2, Cell: Cell{Row: 1, Col: 2}, Power: 1, ExplodeAtMS: 100}}
	engine.nextBombID = 2
	if _, err := engine.ApplyVerifiedBombMovement(1, 1, Cell{Row: 1, Col: 2}, Cell{Row: 1, Col: 3}); err != nil {
		t.Fatal(err)
	}
	if got := engine.bombs[0].EffectiveExplodeAtMS(); got != NativeBombFlightMS {
		t.Fatalf("effective thrown fuse = %d, want %d", got, NativeBombFlightMS)
	}
	for elapsed := uint32(100); elapsed <= NativeBombFlightMS; elapsed += config.Rules.TickMS {
		events, err := engine.Step(nil)
		if err != nil {
			t.Fatal(err)
		}
		if hasEvent(events, EventBombExploded, 0) || len(engine.bombs) != 1 {
			t.Fatalf("bomb exploded in flight at %d: events=%+v bombs=%+v", engine.elapsedMS, events, engine.bombs)
		}
	}
	events, err := engine.Step(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !hasEvent(events, EventBombExploded, 0) || len(engine.bombs) != 0 {
		t.Fatalf("bomb did not explode on landing: clock=%d events=%+v bombs=%+v", engine.elapsedMS, events, engine.bombs)
	}
}

func TestPandaThrownBombDoesNotResetLongerOriginalFuse(t *testing.T) {
	config := testConfig()
	config.Rules.TickMS = 100
	engine := mustEngine(t, config)
	engine.bombs = []Bomb{{ID: 1, OwnerID: 2, Cell: Cell{Row: 1, Col: 2}, Power: 1, ExplodeAtMS: 900}}
	engine.nextBombID = 2
	if _, err := engine.ApplyVerifiedBombMovement(1, 1, Cell{Row: 1, Col: 2}, Cell{Row: 1, Col: 3}); err != nil {
		t.Fatal(err)
	}
	if got := engine.bombs[0].EffectiveExplodeAtMS(); got != 900 {
		t.Fatalf("long fuse was reset by throw: %d", got)
	}
	for engine.elapsedMS < 900 {
		if _, err := engine.Step(nil); err != nil {
			t.Fatal(err)
		}
	}
	if len(engine.bombs) != 1 {
		t.Fatal("long-fuse thrown bomb exploded early")
	}
	events, err := engine.Step(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !hasEvent(events, EventBombExploded, 0) {
		t.Fatalf("long-fuse thrown bomb did not preserve original deadline: %+v", events)
	}
}

func mustNativeTransformation(t *testing.T, sceneID uint32) sceneelement.TransformationDefinition {
	t.Helper()
	definition, ok := sceneelement.NativeTransformation(sceneelement.ID(sceneID))
	if !ok {
		t.Fatalf("native transformation %d is missing", sceneID)
	}
	return definition
}
