package battleengine

import "testing"

func TestNativePassiveAndHeldPickupsUseDistinctState(t *testing.T) {
	config := testConfig()
	config.Rules.SpeedPixelsPerSecondByRate = nativeSpeedPixelsPerSecondByRate
	config.Pickups = []Pickup{
		{SceneID: SceneOxygenBottle, Cell: config.Participants[0].Spawn, State: PickupAvailable},
		{SceneID: 21, Cell: config.Participants[1].Spawn, State: PickupAvailable},
	}
	engine := mustEngine(t, config)
	if _, err := engine.Step(nil); err != nil {
		t.Fatal(err)
	}
	if got := engine.actors[0].OxygenValue; got != NativeOxygenValueInitial+NativeOxygenValueIncrement {
		t.Fatalf("passive oxygen value = %d", got)
	}
	if got := actorHeldActionCount(&engine.actors[0], 62); got != 0 {
		t.Fatalf("passive oxygen leaked into active slots: %d", got)
	}
	if got := actorHeldActionCount(&engine.actors[1], 41); got != 1 {
		t.Fatalf("scene 21 action-41 count = %d, want 1", got)
	}
	observation, err := engine.Observation(2)
	if err != nil {
		t.Fatal(err)
	}
	if observation.Actors[1].HeldActions[0] != (HeldActionSlot{ActionID: 41, Count: 1}) {
		t.Fatalf("observed held actions = %+v", observation.Actors[1].HeldActions)
	}
}

func TestNativeHeldActionInventoryUsesSevenSlots(t *testing.T) {
	actor := Actor{}
	for actionID := uint8(1); actionID <= NativeBattleActionSlots; actionID++ {
		if !grantHeldAction(&actor, actionID, 1) {
			t.Fatalf("grant action %d failed: %+v", actionID, actor.HeldActions)
		}
	}
	if got := actor.HeldActions[NativeBattleActionSlots-1]; got != (HeldActionSlot{ActionID: 7, Count: 1}) {
		t.Fatalf("seventh native slot = %+v, want action 7", got)
	}
	if grantHeldAction(&actor, 8, 1) {
		t.Fatalf("eighth action unexpectedly fit: %+v", actor.HeldActions)
	}
}

func TestPlacedSlowFieldTriggersOnceAndExpires(t *testing.T) {
	config := testConfig()
	config.Rules.SpeedPixelsPerSecondByRate = nativeSpeedPixelsPerSecondByRate
	engine := mustEngine(t, config)
	grantHeldAction(&engine.actors[0], 43, 1)
	if _, err := engine.Step([]Action{{PlayerID: 1, UseActionID: 43}}); err != nil {
		t.Fatal(err)
	}
	if len(engine.fieldObjects) != 1 || actorHeldActionCount(&engine.actors[0], 43) != 0 {
		t.Fatalf("placed field/slot = %+v/%+v", engine.fieldObjects, engine.actors[0].HeldActions)
	}
	engine.actors[0].Position = PositionAtCellCenter(Cell{Row: 0, Col: 0})
	engine.releaseFieldObjectPassThrough()
	engine.actors[1].Position = PositionAtCellCenter(engine.fieldObjects[0].Cell)
	events := engine.resolveFieldObjectContacts()
	if len(engine.fieldObjects) != 0 || engine.actors[1].MovementStatus != MovementStatusSlow || len(events) < 2 {
		t.Fatalf("slow field result actor=%+v objects=%+v events=%+v", engine.actors[1], engine.fieldObjects, events)
	}
	engine.elapsedMS = engine.actors[1].MovementStatusExpiresAt + 1
	engine.expireMovementStatuses()
	if engine.actors[1].MovementStatus != MovementStatusNone {
		t.Fatalf("slow status did not expire: %+v", engine.actors[1])
	}
}

func TestNativeAuthorityMovementFieldWaitsForVerifiedContact(t *testing.T) {
	for _, test := range []struct {
		name     string
		actionID uint8
		status   MovementStatusKind
	}{
		{name: "forced slide", actionID: 42, status: MovementStatusForcedSlide},
		{name: "slow glue", actionID: 43, status: MovementStatusSlow},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := testConfig()
			config.Rules.NativeOutcomeAuthority = true
			config.Rules.SpeedPixelsPerSecondByRate = nativeSpeedPixelsPerSecondByRate
			config.Participants[1].Source = ParticipantVirtualAI
			engine := mustEngine(t, config)
			target := &engine.actors[1]
			object := FieldObject{
				ID: 9, ActionID: test.actionID, OwnerID: engine.actors[0].PlayerID,
				Cell: target.Position.Cell(),
			}
			engine.fieldObjects = []FieldObject{object}

			events := engine.resolveFieldObjectContacts()
			if len(events) != 1 || events[0].Kind != EventFieldObjectTriggered ||
				events[0].TargetID != target.PlayerID {
				t.Fatalf("native-authority contact request = %+v", events)
			}
			if target.MovementStatus != MovementStatusNone || len(engine.fieldObjects) != 1 {
				t.Fatalf("unverified contact mutated state: actor=%+v objects=%+v", *target, engine.fieldObjects)
			}

			verified, err := engine.ApplyVerifiedFieldObjectContact(target.PlayerID, test.actionID, target.Position)
			if err != nil {
				t.Fatal(err)
			}
			if len(verified) != 2 || verified[0].Kind != EventFieldObjectTriggered ||
				verified[1].Kind != EventMovementStatusStarted {
				t.Fatalf("verified field events = %+v", verified)
			}
			if target.MovementStatus != test.status || len(engine.fieldObjects) != 0 {
				t.Fatalf("verified contact did not commit: actor=%+v objects=%+v", *target, engine.fieldObjects)
			}
		})
	}
}

func TestNativeAuthorityMovementFieldRequiresActorCenterCell(t *testing.T) {
	config := testConfig()
	config.Rules.NativeOutcomeAuthority = true
	config.Participants[1].Source = ParticipantVirtualAI
	engine := mustEngine(t, config)
	target := &engine.actors[1]
	fieldCell := Cell{Row: 1, Col: 2}
	object := FieldObject{
		ID: 9, ActionID: 43, OwnerID: engine.actors[0].PlayerID,
		Cell: fieldCell,
	}
	engine.fieldObjects = []FieldObject{object}

	// The actor centre remains in the adjacent cell while its 19-pixel body
	// overlaps the field beginning at x=80. The native client does not report
	// a contact in this state.
	target.Position = Position{X: 79, Y: 60}
	if !engine.positionOverlapsCell(target.Position, fieldCell) {
		t.Fatal("test setup does not overlap the adjacent field cell")
	}
	if got := target.Position.Cell(); got == fieldCell {
		t.Fatalf("test setup centre cell = %+v, want adjacent cell", got)
	}
	if events := engine.resolveFieldObjectContacts(); len(events) != 0 {
		t.Fatalf("adjacent-centre footprint overlap triggered field: %+v", events)
	}
	if _, err := engine.ApplyVerifiedFieldObjectContact(target.PlayerID, object.ActionID, target.Position); err == nil {
		t.Fatal("adjacent-centre verified contact unexpectedly matched field")
	}
	if target.MovementStatus != MovementStatusNone || len(engine.fieldObjects) != 1 {
		t.Fatalf("adjacent overlap mutated field state: actor=%+v objects=%+v", *target, engine.fieldObjects)
	}

	target.Position = PositionAtCellCenter(fieldCell)
	events := engine.resolveFieldObjectContacts()
	if len(events) != 1 || events[0].Kind != EventFieldObjectTriggered || events[0].TargetID != target.PlayerID {
		t.Fatalf("centre-cell field contact request = %+v", events)
	}
	verified, err := engine.ApplyVerifiedFieldObjectContact(target.PlayerID, object.ActionID, target.Position)
	if err != nil {
		t.Fatal(err)
	}
	if len(verified) != 2 || target.MovementStatus != MovementStatusSlow || len(engine.fieldObjects) != 0 {
		t.Fatalf("centre-cell verified contact did not commit: events=%+v actor=%+v objects=%+v", verified, *target, engine.fieldObjects)
	}
}

func TestNativeAuthorityVirtualItemUseWaitsForArbitrator(t *testing.T) {
	config := testConfig()
	config.Rules.NativeOutcomeAuthority = true
	config.Participants[0].Source = ParticipantVirtualAI
	engine := mustEngine(t, config)
	actor := &engine.actors[0]
	grantHeldAction(actor, 43, 1)
	position := actor.Position

	events, err := engine.Step([]Action{{PlayerID: actor.PlayerID, UseActionID: 43}})
	if err != nil {
		t.Fatal(err)
	}
	foundRequest := false
	for _, event := range events {
		if event.Kind == EventBattleActionUseRequested && event.PlayerID == actor.PlayerID && event.ActionID == 43 {
			foundRequest = true
		}
	}
	if !foundRequest || actorHeldActionCount(actor, 43) != 1 || len(engine.fieldObjects) != 0 {
		t.Fatalf("virtual item request mutated shared state: events=%+v actor=%+v objects=%+v", events, *actor, engine.fieldObjects)
	}
	if _, err = engine.ApplyVerifiedBattleAction(actor.PlayerID, 43, position, 0); err != nil {
		t.Fatal(err)
	}
	if actorHeldActionCount(actor, 43) != 0 || len(engine.fieldObjects) != 1 {
		t.Fatalf("arbitrator item confirmation did not commit: actor=%+v objects=%+v", *actor, engine.fieldObjects)
	}
}

func TestHeldActionUseDoesNotIntroduceMovementStiffness(t *testing.T) {
	config := testConfig()
	config.Rules.SpeedPixelsPerSecondByRate = nativeSpeedPixelsPerSecondByRate
	engine := mustEngine(t, config)
	actor := &engine.actors[0]
	start := actor.Position
	startCell := start.Cell()
	grantHeldAction(actor, 43, 1)

	if _, err := engine.Step([]Action{{PlayerID: actor.PlayerID, Move: DirectionRight, UseActionID: 43}}); err != nil {
		t.Fatal(err)
	}
	if actor.Position.X <= start.X {
		t.Fatalf("item use suppressed independently held movement: start=%+v actor=%+v", start, *actor)
	}
	if len(engine.fieldObjects) != 1 || engine.fieldObjects[0].Cell != startCell {
		t.Fatalf("item did not snapshot pre-movement cell: start=%+v objects=%+v", startCell, engine.fieldObjects)
	}
	combined := Action{PlayerID: actor.PlayerID, Move: DirectionRight, UseActionID: 43}
	id, ok := combined.ID()
	if !ok {
		t.Fatalf("combined item/movement action has no discrete ID: %+v", combined)
	}
	decoded, ok := ActionFromID(actor.PlayerID, id)
	if !ok || decoded != combined {
		t.Fatalf("combined action round trip = %+v/%v, want %+v", decoded, ok, combined)
	}
}

func TestCaptureFieldWaitsForPostTransformationProtection(t *testing.T) {
	engine := mustEngine(t, testConfig())
	actor := &engine.actors[1]
	actor.Position = PositionAtCellCenter(Cell{Row: 1, Col: 2})
	actor.HarmProtectionExpiresAt = 1_000
	engine.fieldObjects = []FieldObject{{ID: 1, ActionID: 41, OwnerID: 1, Cell: actor.Position.Cell()}}

	if events := engine.resolveFieldObjectContacts(); actor.State != ActorActive || len(events) != 0 || len(engine.fieldObjects) != 1 {
		t.Fatalf("protected actor consumed capture field: actor=%+v objects=%+v events=%+v", *actor, engine.fieldObjects, events)
	}
	engine.elapsedMS = actor.HarmProtectionExpiresAt
	events := engine.resolveFieldObjectContacts()
	if actor.State != ActorTrapped || len(events) != 2 || len(engine.fieldObjects) != 0 {
		t.Fatalf("expired protection did not admit capture field: actor=%+v objects=%+v events=%+v", *actor, engine.fieldObjects, events)
	}
}

func TestNativeRescueAndRemoteDetonationActions(t *testing.T) {
	engine := mustEngine(t, testConfig())
	actor := &engine.actors[0]
	actor.State = ActorTrapped
	actor.TrappedBy = 2
	grantHeldAction(actor, 63, 1)
	startX := actor.Position.X
	action := Action{PlayerID: 1, Move: DirectionRight, UseActionID: 63}
	legal, err := engine.LegalActions(1)
	if err != nil {
		t.Fatal(err)
	}
	if !containsAction(legal, action) {
		t.Fatalf("trapped rescue/movement action missing from legal actions: %+v", legal)
	}
	events, err := engine.Step([]Action{action})
	if err != nil {
		t.Fatal(err)
	}
	if actor.State != ActorActive || actorHeldActionCount(actor, 63) != 0 || actor.Position.X != startX {
		t.Fatalf("action 63 did not rescue/consume: %+v", *actor)
	}
	foundRescue := false
	for _, event := range events {
		if event.Kind == EventActorRescued && event.PlayerID == actor.PlayerID {
			foundRescue = true
			if event.ActionID != 63 {
				t.Fatalf("action 63 rescue omitted its causal action id: %+v", event)
			}
		}
	}
	if !foundRescue {
		t.Fatalf("action 63 did not emit rescue event: %+v", events)
	}
	// Input handler 006112af skipped the trapped actor before the fork's
	// later item callback. The still-held direction starts on the next frame.
	if _, err := engine.Step([]Action{{PlayerID: 1, Move: DirectionRight}}); err != nil || actor.Position.X <= startX {
		t.Fatalf("held direction did not resume after fork: actor=%+v err=%v", *actor, err)
	}
	engine.bombs = []Bomb{{ID: 1, OwnerID: 1, Cell: Cell{Row: 0, Col: 0}, Power: 1, ExplodeAtMS: 9_000}}
	engine.nextBombID = 2
	grantHeldAction(actor, 64, 1)
	events, err = engine.Step([]Action{{PlayerID: 1, UseActionID: 64}})
	if err != nil {
		t.Fatal(err)
	}
	if len(engine.bombs) != 0 || !hasEvent(events, EventBombExploded, 0) {
		t.Fatalf("action 64 did not detonate owner bomb: bombs=%+v events=%+v", engine.bombs, events)
	}
}

func TestAxeTransformationOwnsActionFortySixLifecycle(t *testing.T) {
	engine := mustEngine(t, testConfig())
	pickup := Pickup{SceneID: 115, Cell: engine.actors[0].Position.Cell(), State: PickupAvailable}
	engine.collectPickup(&engine.actors[0], &pickup)
	if got := actorHeldActionCount(&engine.actors[0], 46); got != 9 {
		t.Fatalf("axe action count = %d, want 9", got)
	}
	engine.endTransformation(&engine.actors[0], TransformationEndExpired, 0)
	if got := actorHeldActionCount(&engine.actors[0], 46); got != 0 {
		t.Fatalf("axe action survived transformation: %d", got)
	}
}

func TestNativeProjectileMissKeepsRayEndpointAndBlockedUseKeepsInventory(t *testing.T) {
	for _, actionID := range []uint8{44, 46} {
		for _, direction := range []Direction{DirectionRight, DirectionUp, DirectionLeft, DirectionDown} {
			config := testConfig()
			config.Grid = testOpenGrid(15, 13)
			config.Rules.NativeOutcomeAuthority = true
			config.Participants[0].Source = ParticipantVirtualAI
			config.Participants[0].Spawn = Cell{Row: 6, Col: 7}
			engine := mustEngine(t, config)
			actor := &engine.actors[0]
			actor.Facing = direction
			grantHeldAction(actor, actionID, 2)
			events, used := engine.useHeldAction(0, actionID)
			if !used || len(events) != 1 || events[0].BombID != 0 || events[0].ProjectileDirection != direction {
				t.Fatalf("action %d direction %d miss request: %+v used=%v", actionID, direction, events, used)
			}
			want := map[Direction]Cell{DirectionRight: {6, 14}, DirectionUp: {0, 7}, DirectionLeft: {6, 0}, DirectionDown: {12, 7}}[direction]
			if events[0].ProjectileTargetCell != want {
				t.Fatalf("action %d direction %d target=%+v, want %+v", actionID, direction, events[0].ProjectileTargetCell, want)
			}
			dx, dy, _ := direction.delta()
			adjacent := Cell{Row: 6 + int16(dy), Col: 7 + int16(dx)}
			engine.grid.Cells[engine.gridIndex(adjacent)] = Tile{Kind: CellSolid}
			if events, used = engine.useHeldAction(0, actionID); used || len(events) != 0 || actorHeldActionCount(actor, actionID) != 2 {
				t.Fatalf("blocked action %d direction %d emitted/consumed: %+v/%v", actionID, direction, events, used)
			}
		}
	}
}

func TestForcedSlideUsesFacingUntilPositionStops(t *testing.T) {
	config := testConfig()
	config.Rules.TickMS = 10
	config.Rules.SpeedPixelsPerSecondByRate = nativeSpeedPixelsPerSecondByRate
	config.Grid.Cells[1*int(config.Grid.Width)+2] = Tile{Kind: CellSolid}
	engine := mustEngine(t, config)
	actor := &engine.actors[0]
	actor.Position.X = 64
	actor.Facing = DirectionRight
	engine.installMovementStatus(actor, MovementStatusForcedSlide)
	if _, err := engine.Step(nil); err != nil {
		t.Fatal(err)
	}
	if actor.Position.X != 69 || actor.MovementStatus != MovementStatusForcedSlide {
		t.Fatalf("forced slide did not advance to obstacle: %+v", *actor)
	}
	if _, err := engine.Step(nil); err != nil {
		t.Fatal(err)
	}
	if actor.MovementStatus != MovementStatusNone {
		t.Fatalf("forced slide survived an unchanged tick: %+v", *actor)
	}
}

func TestDirectionalActionDetonatesFirstBombAfterNativeFlight(t *testing.T) {
	engine := mustEngine(t, testConfig())
	actor := &engine.actors[0]
	actor.Facing = DirectionRight
	engine.bombs = []Bomb{{ID: 1, OwnerID: 2, Cell: Cell{Row: 1, Col: 2}, Power: 1, ExplodeAtMS: 9_000}}
	engine.nextBombID = 2
	grantHeldAction(actor, 44, 1)
	if _, err := engine.Step([]Action{{PlayerID: 1, UseActionID: 44}}); err != nil {
		t.Fatal(err)
	}
	if len(engine.projectiles) != 1 || len(engine.bombs) != 1 {
		t.Fatalf("directional action did not begin flight: projectile=%+v bombs=%+v", engine.projectiles, engine.bombs)
	}
	for engine.elapsedMS <= nativeActionProjectileDurationMS {
		if _, err := engine.Step(nil); err != nil {
			t.Fatal(err)
		}
	}
	if len(engine.projectiles) != 0 || len(engine.bombs) != 0 {
		t.Fatalf("directional action did not detonate target: projectile=%+v bombs=%+v", engine.projectiles, engine.bombs)
	}
}

func TestDirectionalActionDoesNotFollowKickedBomb(t *testing.T) {
	engine := mustEngine(t, testConfig())
	actor := &engine.actors[0]
	actor.Facing = DirectionRight
	engine.bombs = []Bomb{{ID: 1, OwnerID: 2, Cell: Cell{Row: 1, Col: 2}, Power: 1, ExplodeAtMS: 9_000}}
	engine.nextBombID = 2
	grantHeldAction(actor, 44, 1)
	if _, err := engine.Step([]Action{{PlayerID: 1, UseActionID: 44}}); err != nil {
		t.Fatal(err)
	}
	engine.bombs[0].Cell = Cell{Row: 2, Col: 2}
	for engine.elapsedMS < nativeActionProjectileDurationMS {
		if _, err := engine.Step(nil); err != nil {
			t.Fatal(err)
		}
	}
	if len(engine.projectiles) != 0 || len(engine.bombs) != 1 || engine.bombs[0].ExplodeAtMS != 9_000 {
		t.Fatalf("projectile followed moved bomb: projectile=%+v bombs=%+v", engine.projectiles, engine.bombs)
	}
}

func TestDirectionalActionUsesMapEdgeRangeAndStopsAtWall(t *testing.T) {
	config := testConfig()
	config.Grid = testOpenGrid(12, 3)
	config.Participants[0].Spawn = Cell{Row: 1, Col: 1}
	config.Participants[1].Spawn = Cell{Row: 2, Col: 11}
	engine := mustEngine(t, config)
	actor := &engine.actors[0]
	actor.Facing = DirectionRight
	engine.bombs = []Bomb{{ID: 1, OwnerID: 2, Cell: Cell{Row: 1, Col: 10}, Power: 1, ExplodeAtMS: 9_000}}
	if got := engine.firstBombInFacingRay(actor); got != 1 {
		t.Fatalf("map-edge projectile target = %d, want 1", got)
	}
	engine.grid.Cells[1*12+6] = Tile{Kind: CellSolid}
	if got := engine.firstBombInFacingRay(actor); got != 0 {
		t.Fatalf("wall-blocked projectile target = %d, want 0", got)
	}
}
