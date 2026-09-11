package battlereplay

import (
	"testing"

	"qqtang/internal/game/battleengine"
	"qqtang/internal/game/qbv"
	"qqtang/internal/protocol/game"
)

func TestEventAlignedReplayUsesNativeBombReach(t *testing.T) {
	for _, wirePower := range []uint16{2, 3} {
		body, err := (game.PlayerUseBombEvent{
			PlayerID: 1, ClientTime: 3000, BombID: 1,
			Row: 3, Column: 3, Power: wirePower,
		}).MarshalNetworkBinary()
		if err != nil {
			t.Fatal(err)
		}
		grid := battleengine.Grid{Width: 7, Height: 7, Cells: make([]battleengine.Tile, 49)}
		for i := range grid.Cells {
			grid.Cells[i].FlamePassable = true
		}
		config := battleengine.Config{
			Seed: 1, Grid: grid,
			Rules: battleengine.Rules{
				TickMS: 20, RoundDurationMS: 10000, BombFuseMS: 3000,
				FlameDurationMS: 500, TrapDurationMS: 6000, ActorHalfSizePixels: 19,
			},
			Participants: []battleengine.Participant{
				{PlayerID: 1, TeamID: 1, Source: battleengine.ParticipantHuman, Spawn: battleengine.Cell{Row: 0, Col: 0}, SpeedPixelsPerSecond: 180, BombCapacity: 1, BombPower: 1},
				{PlayerID: 2, TeamID: 2, Source: battleengine.ParticipantHuman, Spawn: battleengine.Cell{Row: 6, Col: 6}, SpeedPixelsPerSecond: 180, BombCapacity: 1, BombPower: 1},
			},
		}
		report, err := RunEventAligned(config, qbv.Recording{Events: []qbv.Event{
			{TimeMS: 3000, Schema: game.PlayerUseBomb, Data: body},
			{TimeMS: 6500, Schema: 0xffff},
		}})
		if err != nil {
			t.Fatal(err)
		}
		if len(report.Warnings) != 0 {
			t.Fatalf("replay warnings: %v", report.Warnings)
		}
		found := false
		for _, event := range report.EngineEvents {
			if event.Kind != battleengine.EventBombExploded {
				continue
			}
			found = true
			reach := int16(wirePower - 1)
			if event.BlastRowMin != 3-reach || event.BlastRowMax != 3+reach ||
				event.BlastColMin != 3-reach || event.BlastColMax != 3+reach {
				t.Fatalf("wire power %d exploded rows %d..%d cols %d..%d; want arm reach %d", wirePower, event.BlastRowMin, event.BlastRowMax, event.BlastColMin, event.BlastColMax, reach)
			}
		}
		if !found {
			t.Fatal("recorded bomb did not explode")
		}
	}
}
