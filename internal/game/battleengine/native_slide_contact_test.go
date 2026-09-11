package battleengine

import "testing"

func TestNativeBananaUnconfirmedContactDoesNotRestrictVirtualInput(t *testing.T) {
	config := testConfig()
	config.Grid = testOpenGrid(15, 11)
	config.Rules.TickMS = 20
	config.Rules.NativeOutcomeAuthority = true
	config.Rules.SpeedPixelsPerSecondByRate = nativeSpeedPixelsPerSecondByRate
	config.Participants[1].Source = ParticipantVirtualAI
	config.Participants[1].Spawn = Cell{Row: 1, Col: 4}
	engine := mustEngine(t, config)
	actor := &engine.actors[1]
	// Reproduce entering the banana cell while choosing the opposite direction.
	// Until FAD, native contact is only a request: no slide or input quarantine.
	origin := Position{X: 198, Y: 59}
	actor.Position, actor.nativePreviousPosition = origin, Position{X: 204, Y: 59}
	actor.Facing, actor.SpeedRate = DirectionLeft, 8
	engine.fieldObjects = []FieldObject{{ID: 1, OwnerID: 1, ActionID: 42, Cell: origin.Cell()}}
	runtime, err := NewRuntime(engine, map[uint16]Policy{2: PolicyFunc(func(Observation, []Action) (Action, error) {
		return Action{PlayerID: 2, Move: DirectionRight}, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	for step := 0; step < 25; step++ {
		before := actor.Position
		result, err := runtime.StepWithTrace(nil)
		if err != nil || len(result.Actions) != 1 || result.Actions[0].Move != DirectionRight ||
			actor.Facing != DirectionRight || actor.Position.X <= before.X || actor.MovementStatus != MovementStatusNone ||
			len(engine.fieldObjects) != 1 || runtime.VirtualActorSuspended(2) {
			t.Fatalf("step %d unconfirmed contact affected movement: result=%+v actor=%+v err=%v", step, result, *actor, err)
		}
		if step == 0 && !hasEvent(result.Events, EventFieldObjectTriggered, 2) {
			t.Fatal("contact did not emit its native request")
		}
	}
}
