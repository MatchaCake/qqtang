package battleengine

import (
	"fmt"
	"testing"
)

func TestNativeHitClearsMovementStatusAndFacesDown(t *testing.T) {
	// 005acaab clears actor+3fc through 005adf32(0) before either hit branch;
	// both the normal branch and avatar callback 005f78bf set facing to 3.
	for _, path := range []string{"flame", "field", "verified"} {
		for _, sceneID := range []uint32{0, 101} {
			for _, status := range []MovementStatusKind{MovementStatusSlow, MovementStatusFast, MovementStatusForcedSlide} {
				t.Run(fmt.Sprintf("%s_avatar_%d_status_%d", path, sceneID, status), func(t *testing.T) {
					engine := mustEngine(t, testConfig())
					actor := &engine.actors[0]
					if sceneID != 0 {
						engine.collectPickup(actor, &Pickup{SceneID: sceneID, State: PickupAvailable, Cell: actor.Position.Cell()})
					}
					engine.installMovementStatus(actor, status)
					actor.Facing = DirectionLeft
					actor.moveRemainder = 7
					switch path {
					case "flame":
						engine.flames = []Flame{{Cell: actor.Position.Cell(), OwnerID: 2, ImpactAtMS: engine.elapsedMS}}
						engine.applyFlameHazards()
					case "field":
						engine.fieldObjects = []FieldObject{{ID: 1, ActionID: 41, OwnerID: 2, Cell: actor.Position.Cell()}}
						engine.resolveFieldObjectContacts()
					case "verified":
						if _, changed, err := engine.ApplyVerifiedActorHit(actor.PlayerID, actor.Position, sceneID != 0); err != nil || !changed {
							t.Fatalf("hit changed=%t err=%v", changed, err)
						}
					}
					if actor.MovementStatus != MovementStatusNone || actor.MovementStatusExpiresAt != 0 || actor.moveRemainder != 0 || actor.Facing != DirectionDown {
						t.Fatalf("native hit retained movement state: %+v", *actor)
					}
				})
			}
		}
	}
}

func TestNativeNewAvatarDoesNotInheritNormalBodyProtection(t *testing.T) {
	for _, sceneID := range []uint32{101, 104, 107, 108, 109, 110, 114, 115} {
		t.Run(fmt.Sprint(sceneID), func(t *testing.T) {
			config := testConfig()
			config.Participants[0].Source = ParticipantVirtualAI
			engine := mustEngine(t, config)
			actor := &engine.actors[0]
			engine.collectPickup(actor, &Pickup{SceneID: 101, State: PickupAvailable})
			engine.endTransformation(actor, TransformationEndExpired, 0)
			if !engine.actorHarmProtected(actor) {
				t.Fatal("normal body should receive recovery protection")
			}
			engine.collectPickup(actor, &Pickup{SceneID: sceneID, State: PickupAvailable})
			request, ok, err := engine.BuildVirtualBlastHitRequest(actor.PlayerID, 2, actor.Position)
			if err != nil || !ok || request.SceneID != sceneID || actor.HarmProtectionExpiresAt != 0 {
				t.Fatalf("new avatar inherited embedded normal-body protection: request=%+v ok=%t err=%v actor=%+v", request, ok, err, *actor)
			}
		})
	}
}

func TestNativeMovementTimersRetainZeroUntilNextUpdate(t *testing.T) {
	for _, status := range []MovementStatusKind{MovementStatusSlow, MovementStatusFast} {
		engine := mustEngine(t, testConfig())
		actor := &engine.actors[0]
		engine.installMovementStatus(actor, status)
		for _, now := range []uint32{NativeMovementStatusMS - 1, NativeMovementStatusMS, NativeMovementStatusMS + 1} {
			engine.elapsedMS = now
			engine.expireMovementStatuses()
			want := status
			if now > NativeMovementStatusMS {
				want = MovementStatusNone
			}
			if actor.MovementStatus != want {
				t.Fatalf("status %d at %d: got %d want %d", status, now, actor.MovementStatus, want)
			}
		}
	}
}

func TestNativeTrappedActorCanInitiateDiffGridContact(t *testing.T) {
	// 0060c148 and FA8/FAA arbiters check the target's trapped state. They
	// do not add an active-state condition on the reporting source actor.
	for _, live := range []bool{false, true} {
		for _, rescue := range []bool{false, true} {
			engine := mustEngine(t, testConfig())
			engine.rules.NativeOutcomeAuthority = live
			source, target := &engine.actors[0], &engine.actors[1]
			source.Source, source.State = ParticipantVirtualAI, ActorTrapped
			target.Position, target.State = source.Position, ActorTrapped
			target.nativePreviousPosition = target.Position
			if rescue {
				target.TeamID = source.TeamID
			}
			events := engine.resolveActorContacts()
			if len(events) == 0 || events[0].PlayerID != source.PlayerID || events[0].TargetID != target.PlayerID {
				t.Fatalf("live=%t rescue=%t native contact suppressed: %+v", live, rescue, events)
			}
			if live && (source.State != ActorTrapped || target.State != ActorTrapped) {
				t.Fatal("native request committed before authority echo")
			}
		}
	}
}

func TestNativeAvatarRecoveryRunsBeforeMovementAfterTimerTurnsNegative(t *testing.T) {
	config := testConfig()
	config.Rules.TickMS = 20
	config.Rules.RoundDurationMS = 120_000
	engine := mustEngine(t, config)
	actor := &engine.actors[0]
	engine.collectPickup(actor, &Pickup{SceneID: 107, State: PickupAvailable})
	expires := actor.TransformationExpiresAt
	engine.elapsedMS = expires
	if events, err := engine.Step(nil); err != nil || actor.TransformationSceneID == 0 {
		t.Fatalf("zero timer must survive this frame: actor=%+v events=%+v err=%v", *actor, events, err)
	}
	origin, recoveryAt := actor.Position, engine.elapsedMS
	events, err := engine.Step([]Action{{PlayerID: actor.PlayerID, Move: DirectionRight}})
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Kind == EventActorTransformationEnded {
			if event.TimeMS != recoveryAt || event.Position != origin {
				t.Fatalf("recovery used next-frame clock/coordinate: %+v want %d/%+v", event, recoveryAt, origin)
			}
			return
		}
	}
	t.Fatal("negative timer did not recover avatar")
}

func TestNativeDueBlastHitsBeforeActorCanMoveAway(t *testing.T) {
	config := testConfig()
	config.Rules.TickMS = 20
	engine := mustEngine(t, config)
	actor := &engine.actors[0]
	actor.Position = Position{X: 79, Y: 60}
	origin := actor.Position
	engine.elapsedMS = 4_000
	engine.bombs = []Bomb{{ID: 1, OwnerID: 2, Cell: Cell{Row: 1, Col: 0}, Power: 1, ExplodeAtMS: 4_000}}
	events, err := engine.Step([]Action{{PlayerID: actor.PlayerID, Move: DirectionRight}})
	if err != nil || actor.State != ActorTrapped || actor.Position != origin {
		t.Fatalf("actor escaped a due native impact by moving first: actor=%+v events=%+v err=%v", *actor, events, err)
	}
	for _, event := range events {
		if event.Kind == EventActorTrapped && event.TimeMS != 4_000 {
			t.Fatalf("impact sampled wrong scene time: %+v", event)
		}
	}
}

func TestNativeBombFuseRequiresNegativeCountdown(t *testing.T) {
	engine := mustEngine(t, testConfig())
	_, placed := engine.placeBomb(0)
	if !placed {
		t.Fatal("place bomb")
	}
	engine.elapsedMS = engine.rules.BombFuseMS
	if events := engine.explodeDueBombs(); len(events) != 0 {
		t.Fatalf("zero native fuse exploded: %+v", events)
	}
	engine.elapsedMS++
	if events := engine.explodeDueBombs(); len(events) == 0 {
		t.Fatal("negative native fuse did not explode")
	}
}

func TestNativeProjectileUsesFlamePassability(t *testing.T) {
	for _, passable := range []bool{false, true} {
		engine := mustEngine(t, testConfig())
		actor := &engine.actors[0]
		actor.Position, actor.Facing = PositionAtCellCenter(Cell{Row: 1, Col: 1}), DirectionRight
		kind := CellOpen
		if passable {
			kind = CellSolid
		}
		engine.grid.Cells[int(engine.grid.Width)+2] = Tile{Kind: kind, FlamePassable: passable, MapElementOccupied: true}
		engine.bombs = []Bomb{{ID: 1, OwnerID: 2, Cell: Cell{Row: 1, Col: 3}, ExplodeAtMS: 4_000}}
		_, id, reachable := engine.nativeActionProjectileTarget(actor)
		if reachable != passable || (passable && id != 1) {
			t.Fatalf("flame passable=%t: target=%d reachable=%t", passable, id, reachable)
		}
	}
}

func TestNativeItemInputBlockedOverStaticMapObject(t *testing.T) {
	for _, live := range []bool{false, true} {
		engine := mustEngine(t, testConfig())
		engine.rules.NativeOutcomeAuthority = live
		actor := &engine.actors[0]
		actor.Source = ParticipantVirtualAI
		cell := actor.Position.Cell()
		engine.grid.Cells[int(cell.Row)*int(engine.grid.Width)+int(cell.Col)] = Tile{Kind: CellOpen, FlamePassable: true, MapElementOccupied: true}
		grantHeldAction(actor, 43, 1)
		if events, used := engine.useHeldAction(0, 43); used || len(events) != 0 || actorHeldActionCount(actor, 43) != 1 {
			t.Fatalf("live=%t native item input passed static object: used=%t events=%+v", live, used, events)
		}
	}
}

func TestNativeTurnAndItemUsesNewFacingBeforeMovement(t *testing.T) {
	config := testConfig()
	config.Rules.NativeOutcomeAuthority = true
	config.Participants[0].Source = ParticipantVirtualAI
	engine := mustEngine(t, config)
	actor := &engine.actors[0]
	actor.Facing = DirectionRight
	origin := actor.Position
	grantHeldAction(actor, 44, 1)
	events, err := engine.Step([]Action{{PlayerID: actor.PlayerID, Move: DirectionDown, UseActionID: 44}})
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Kind == EventBattleActionUseRequested {
			if event.ProjectileDirection != DirectionDown || event.ProjectileTargetCell.Col != origin.Cell().Col || event.Position != origin {
				t.Fatalf("turn/item did not use input-facing at old coordinate: %+v", event)
			}
			return
		}
	}
	t.Fatal("missing directional item request")
}

func TestNativeBlastAvatarHitStopsCurrentInputFrame(t *testing.T) {
	engine := mustEngine(t, testConfig())
	actor := &engine.actors[0]
	engine.collectPickup(actor, &Pickup{SceneID: 101, State: PickupAvailable})
	origin := actor.Position
	engine.bombs = []Bomb{{ID: 1, OwnerID: 2, Cell: actor.Position.Cell(), Power: 0, ExplodeAtMS: engine.elapsedMS}}
	events, err := engine.Step([]Action{{PlayerID: actor.PlayerID, Move: DirectionRight}})
	if err != nil || actor.TransformationSceneID != 0 || actor.State != ActorActive || actor.Position != origin || actor.Facing != DirectionDown {
		t.Fatalf("avatar hit retained movement: actor=%+v events=%+v err=%v", *actor, events, err)
	}
}

func TestNativeMapPushCallbackPrecedesSameFrameBlastHit(t *testing.T) {
	config := testConfig()
	config.Participants[1].Spawn = Cell{Row: 0, Col: 4}
	config.Grid.Cells[int(config.Grid.Width)+2] = Tile{
		Kind: CellSolid, MapElementID: 9012, NormalPushable: true,
		ElementWidth: 1, ElementHeight: 1, ElementAnchor: Cell{Row: 1, Col: 2}, PushCounter: 12,
	}
	engine := mustEngine(t, config)
	actor := &engine.actors[0]
	engine.bombs = []Bomb{{ID: 1, OwnerID: 2, Cell: actor.Position.Cell(), ExplodeAtMS: engine.elapsedMS}}
	events, err := engine.Step([]Action{{PlayerID: actor.PlayerID, Move: DirectionRight}})
	if err != nil || !hasEvent(events, EventMapElementMoved, 0) || actor.State != ActorTrapped {
		t.Fatalf("0060cac9 must precede 0060c63a: actor=%+v events=%+v err=%v", *actor, events, err)
	}
}

func TestNativeKickAndDuckRecoveryRequireNoStaticObject(t *testing.T) {
	engine := mustEngine(t, testConfig())
	engine.grid.Cells[int(engine.grid.Width)+4] = Tile{Kind: CellOpen, FlamePassable: true, MapElementOccupied: true}
	if target, ok := engine.kickBombDestination(Cell{Row: 1, Col: 2}, DirectionRight); !ok || target != (Cell{Row: 1, Col: 3}) {
		t.Fatalf("native 005d865c destination must have no map object: %+v/%t", target, ok)
	}
	actor := &engine.actors[0]
	engine.installTransformation(actor, mustNativeTransformation(t, 104))
	actor.Position = PositionAtCellCenter(Cell{Row: 1, Col: 4})
	engine.elapsedMS = actor.TransformationExpiresAt + 1
	if events := engine.expireTransformations(); len(events) != 0 || actor.TransformationSceneID == 0 {
		t.Fatalf("duck recovered on walkable static object: %+v", events)
	}
}

func TestNativeDangerTimelineIncludesAlreadyDuePreMovementImpact(t *testing.T) {
	engine := mustEngine(t, testConfig())
	engine.elapsedMS = 4_000
	cell := engine.actors[0].Position.Cell()
	engine.bombs = []Bomb{{ID: 1, OwnerID: 2, Cell: cell, ExplodeAtMS: 4_000}}
	timeline, err := engine.DangerTimeline(100)
	if err != nil {
		t.Fatal(err)
	}
	if impact, ok := timeline.ImpactAt(cell); !ok || impact != 4_000 {
		t.Fatalf("due impact delayed until after next move: %d/%t", impact, ok)
	}
	facts := tacticalFactsForTest(t, engine, 1)
	for direction, found := range facts.CurrentDirectionWholeCellRefugeFound {
		if found {
			t.Fatalf("direction %d predicted movement before the current impact", direction)
		}
	}
}
