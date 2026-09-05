package probe

import (
	"fmt"
	"testing"

	"qqtang/internal/game/battleengine"
)

func TestCompetitiveAIMovementFramePublishesActualTurnOrigin(t *testing.T) {
	for _, directions := range [][2]battleengine.Direction{
		{battleengine.DirectionRight, battleengine.DirectionUp},
		{battleengine.DirectionUp, battleengine.DirectionLeft},
		{battleengine.DirectionLeft, battleengine.DirectionDown},
		{battleengine.DirectionDown, battleengine.DirectionRight},
		{battleengine.DirectionRight, battleengine.DirectionLeft},
		{battleengine.DirectionDown, battleengine.DirectionUp},
		{battleengine.DirectionRight, battleengine.DirectionNone},
		{battleengine.DirectionNone, battleengine.DirectionRight},
	} {
		t.Run(fmt.Sprintf("%d_to_%d", directions[0], directions[1]), func(t *testing.T) {
			engine, projection := turnTestEngine(t, directions[0])
			before := engine.Clone()
			origin, _ := actorByID(before.Actors(), 20001)
			action := battleengine.Action{PlayerID: 20001, Move: directions[1]}
			if _, err := engine.Step([]battleengine.Action{action}); err != nil {
				t.Fatal(err)
			}
			lastSequence := projection.sequence
			_, moves, err := competitiveAIMovementFrameSamples(before, engine, 20001, action, projection, false)
			if err != nil || len(moves) == 0 {
				t.Fatalf("moves=%+v err=%v", moves, err)
			}
			first := moves[0]
			if first.TimeStamp != before.ElapsedMS() || first.CurrentPosX != uint16(origin.Position.X) || first.CurrentPosY != uint16(origin.Position.Y) {
				t.Fatalf("turn starts at %+v; authority origin at %d is %+v", first, before.ElapsedMS(), origin.Position)
			}
			if first.Sequence != lastSequence+1 || first.WalkAndDirection&0x20 != 0 {
				t.Fatalf("ordinary input boundary must increment sequence without forcing: %+v", first)
			}
			if directions[1] != battleengine.DirectionNone {
				path, err := before.ProjectNativeMovement(20001, directions[1])
				if err != nil || first.EndPosX != uint16(path.End.X) || first.EndPosY != uint16(path.End.Y) {
					t.Fatalf("turn path=%+v want %+v err=%v", first, path, err)
				}
			}
			for i := 1; i < len(moves); i++ {
				if moves[i].TimeStamp <= moves[i-1].TimeStamp || moves[i].Sequence != first.Sequence {
					t.Fatalf("invalid same-input path update: %+v", moves)
				}
			}
		})
	}
}

func TestCompetitiveAITurnPreservesForcedAndAlreadySentCheckpoints(t *testing.T) {
	for _, forced := range []bool{false, true} {
		engine, projection := turnTestEngine(t, battleengine.DirectionRight)
		before := engine.Clone()
		if !forced {
			projection.lastSentAt = before.ElapsedMS()
		}
		action := battleengine.Action{PlayerID: 20001, Move: battleengine.DirectionLeft}
		if _, err := engine.Step([]battleengine.Action{action}); err != nil {
			t.Fatal(err)
		}
		_, moves, err := competitiveAIMovementFrameSamples(before, engine, 20001, action, projection, forced)
		if err != nil || len(moves) != 1 {
			t.Fatalf("forced=%t moves=%+v err=%v", forced, moves, err)
		}
		if moves[0].TimeStamp != engine.ElapsedMS() || (moves[0].WalkAndDirection&0x20 != 0) != forced {
			t.Fatalf("forced=%t incompatible event/duplicate checkpoint: %+v", forced, moves[0])
		}
	}
}

func turnTestEngine(t *testing.T, held battleengine.Direction) (*battleengine.Engine, liveCompetitiveAIMovementProjection) {
	t.Helper()
	human, err := battleengine.ParticipantFromNativeRole(1, 9, 1, battleengine.ParticipantHuman)
	if err != nil {
		t.Fatal(err)
	}
	actor, err := battleengine.ParticipantFromNativeRole(20001, 10, 2, battleengine.ParticipantVirtualAI)
	if err != nil {
		t.Fatal(err)
	}
	human.Spawn, actor.Spawn = battleengine.Cell{Row: 1, Col: 1}, battleengine.Cell{Row: 5, Col: 7}
	rules := battleengine.Rules{TickMS: 20, StartClockMS: 3000, RoundDurationMS: 240000,
		BombFuseMS: battleengine.NativeBombFuseMS, FlameDurationMS: 200, ActorHalfSizePixels: 10}
	for rate := byte(0); rate <= battleengine.MaxNativeSpeedRate; rate++ {
		rules.SpeedPixelsPerSecondByRate[rate], _ = battleengine.NativeSpeedPixelsPerSecond(rate)
	}
	engine, err := battleengine.New(battleengine.Config{
		Grid:         battleengine.Grid{Width: 15, Height: 11, Cells: make([]battleengine.Tile, 165)},
		Rules:        rules,
		Participants: []battleengine.Participant{human, actor},
	})
	if err != nil {
		t.Fatal(err)
	}
	projection := liveCompetitiveAIMovementProjection{}
	action := battleengine.Action{PlayerID: 20001, Move: held}
	for i := 0; i < 5; i++ {
		before := engine.Clone()
		if _, err = engine.Step([]battleengine.Action{action}); err != nil {
			t.Fatal(err)
		}
		projection, _, err = competitiveAIMovementFrameSamples(before, engine, 20001, action, projection, false)
		if err != nil {
			t.Fatal(err)
		}
	}
	return engine, projection
}
