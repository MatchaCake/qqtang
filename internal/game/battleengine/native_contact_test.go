package battleengine

import (
	"fmt"
	"testing"
)

func TestNativeActorContactUsesPixelDistanceAndSourcePosition(t *testing.T) {
	// Rule-1 initializer 005f4d57 installs 0060c10b -> vtable 007a4548.
	// Its producer 0060c148 requires abs(dx),abs(dy) < double[007a41a8]
	// (41.0), and writes the acting player's coordinates into FA8/FAA.
	for _, live := range []bool{false, true} {
		for _, rescue := range []bool{false, true} {
			for _, delta := range []Position{{X: 40}, {X: -40}, {Y: 40}, {Y: -40}, {X: 40, Y: 40}, {X: 41}, {X: -41}, {Y: 41}, {Y: -41}} {
				t.Run(fmt.Sprintf("live_%t_rescue_%t_delta_%d_%d", live, rescue, delta.X, delta.Y), func(t *testing.T) {
					config := testConfig()
					config.Grid = testOpenGrid(9, 9)
					config.Rules.NativeOutcomeAuthority = live
					config.Participants[1].Source = ParticipantVirtualAI
					engine := mustEngine(t, config)
					target, source := &engine.actors[0], &engine.actors[1]
					target.Position, target.State = Position{X: 180, Y: 180}, ActorTrapped
					source.Position = Position{X: target.Position.X + delta.X, Y: target.Position.Y + delta.Y}
					if rescue {
						source.TeamID = target.TeamID
					}
					wantContact := delta.X > -41 && delta.X < 41 && delta.Y > -41 && delta.Y < 41
					events := engine.resolveActorContacts()
					if !wantContact {
						if len(events) != 0 || target.State != ActorTrapped {
							t.Fatalf("outside native contact: target=%+v events=%+v", *target, events)
						}
						return
					}
					if len(events) == 0 {
						t.Fatalf("native near contact was lost across a cell boundary: source=%+v target=%+v", source.Position, target.Position)
					}
					if live {
						wantKind := EventActorEliminationRequested
						if rescue {
							wantKind = EventActorRescueRequested
						}
						if events[0].Kind != wantKind || events[0].Position != source.Position || target.State != ActorTrapped {
							t.Fatalf("request must carry source coordinates and wait for native authority: %+v", events)
						}
					}
				})
			}
		}
	}
}

func TestNativeRescueNotificationDoesNotMoveTargetToRescuer(t *testing.T) {
	engine := mustEngine(t, testConfig())
	target := &engine.actors[0]
	target.State = ActorTrapped
	position := target.Position
	// FAB consumer 006063bd uses the target ID and 005acc39, and never applies
	// the request's source coordinate to the rescued target.
	_, changed, err := engine.ApplyVerifiedRescue(2, target.PlayerID, Position{X: position.X + 30, Y: position.Y})
	if err != nil || !changed || target.State != ActorActive || target.Position != position {
		t.Fatalf("rescue notification moved target: target=%+v changed=%t err=%v", *target, changed, err)
	}
}

func TestNativeContactsSamplePositionBeforeMovement(t *testing.T) {
	// 005c87b0 runs 005f43b1 (0060c357 / 0060c148) before 005f3810
	// moves the actor; only the later 005b8009 advances the scene clock.
	for _, live := range []bool{false, true} {
		for _, kind := range []string{"pickup", "kill", "field41", "field42", "field43"} {
			t.Run(fmt.Sprintf("live_%t_%s", live, kind), func(t *testing.T) {
				config := testConfig()
				config.Rules.TickMS = 20
				config.Rules.NativeOutcomeAuthority = live
				config.Participants[0].Source = ParticipantVirtualAI
				engine := mustEngine(t, config)
				actor := &engine.actors[0]
				actor.Position = Position{X: 79, Y: 60}
				wantKind := EventPickupCollected
				switch kind {
				case "pickup":
					engine.pickups = []Pickup{{SceneID: 1, Cell: Cell{Row: 1, Col: 2}, State: PickupAvailable}}
					if live {
						wantKind = EventPickupCollectRequested
					}
				case "kill":
					engine.actors[1].State = ActorTrapped
					engine.actors[1].Position = Position{X: 120, Y: 60}
					wantKind = EventActorEliminated
					if live {
						wantKind = EventActorEliminationRequested
					}
				default:
					fieldAction := uint8(41 + kind[len(kind)-1] - '1')
					engine.fieldObjects = []FieldObject{{ID: 1, ActionID: fieldAction, OwnerID: 2, Cell: Cell{Row: 1, Col: 2}}}
					wantKind = EventFieldObjectTriggered
				}
				actions := []Action{{PlayerID: actor.PlayerID, Move: DirectionRight}}
				first, err := engine.Step(actions)
				if err != nil {
					t.Fatal(err)
				}
				for _, event := range first {
					if event.Kind == wantKind {
						t.Fatalf("contact used future movement under the old clock: %+v", event)
					}
				}
				position, clock := actor.Position, engine.elapsedMS
				if position.X <= 79 {
					t.Fatalf("actor did not enter contact: %+v", *actor)
				}
				second, err := engine.Step(actions)
				if err != nil {
					t.Fatal(err)
				}
				found := false
				for _, event := range second {
					if event.Kind != wantKind {
						continue
					}
					found = true
					if event.TimeMS != clock || ((kind != "kill" || live) && event.Position != position) {
						t.Fatalf("contact coordinate and clock disagree: before=%+v@%d event=%+v", position, clock, event)
					}
				}
				if !found {
					t.Fatalf("missing contact at next frame start: %+v", second)
				}
				if kind == "field41" && actor.Position != position {
					t.Fatalf("capture contact allowed another movement step: before=%+v after=%+v", position, actor.Position)
				}
			})
		}
	}
}

func TestNativeContactsRequireDifferentPreviousCoordinateCell(t *testing.T) {
	for _, live := range []bool{false, true} {
		config := testConfig()
		config.Rules.TickMS = 20
		config.Rules.NativeOutcomeAuthority = live
		config.Participants[0].Source = ParticipantVirtualAI
		engine := mustEngine(t, config)
		actor := &engine.actors[0]
		actor.Position = Position{X: 90, Y: 60}
		actor.nativePreviousPosition = Position{X: 89, Y: 60}
		engine.pickups = []Pickup{{SceneID: SceneBombCapacitySmall, Cell: actor.Position.Cell(), State: PickupAvailable}}
		engine.fieldObjects = []FieldObject{{ID: 1, ActionID: 43, OwnerID: 2, Cell: actor.Position.Cell()}}
		engine.actors[1].State, engine.actors[1].Position = ActorTrapped, Position{X: 120, Y: 60}
		events, err := engine.Step([]Action{{PlayerID: 1, Move: DirectionRight}})
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range events {
			if event.Kind != EventActorMoved {
				t.Fatalf("live=%t: same-cell movement incorrectly entered a DiffGrid callback: %+v", live, event)
			}
		}
	}
}

func TestNativePickupEffectPrecedesSameFrameBomb(t *testing.T) {
	config := testConfig()
	config.Rules.TickMS = 20
	config.Participants[0].MaxBombPower = 6
	engine := mustEngine(t, config)
	actor := &engine.actors[0]
	actor.Position, actor.nativePreviousPosition = Position{X: 80, Y: 60}, Position{X: 79, Y: 60}
	engine.pickups = []Pickup{{SceneID: SceneBombPowerSmall, Cell: actor.Position.Cell(), State: PickupAvailable}}
	beforePower := actor.BombPower
	events, err := engine.Step([]Action{{PlayerID: 1, PlaceBomb: true}})
	if err != nil {
		t.Fatal(err)
	}
	if actor.BombPower != beforePower+1 || len(engine.bombs) != 1 || engine.bombs[0].Power != actor.BombPower {
		t.Fatalf("same-frame bomb missed the native pickup effect: actor=%+v bombs=%+v events=%+v", *actor, engine.bombs, events)
	}
}
