// Package battlereplay adapts original-client QBV events to the restricted
// deterministic battle engine. It is validation infrastructure, not a second
// live authority path: native observations are compared with engine output and
// never overwrite simulated state.
package battlereplay

import (
	"fmt"
	"sort"

	"qqtang/internal/game/battleengine"
	"qqtang/internal/game/qbv"
	"qqtang/internal/protocol/game"
)

type InputKind byte

const (
	InputMovement InputKind = iota + 1
	InputPlaceBomb
	InputPickup
	InputUseAction
	InputConfirmDeath
)

type Input struct {
	TimeMS       uint32
	RecordIndex  int
	Kind         InputKind
	PlayerID     uint16
	Move         battleengine.Direction
	MoveEnd      battleengine.Position
	BombCell     battleengine.Cell
	BombPower    uint16 // Internal arm reach; native PLAYER_USE_BOMB encodes reach + 1.
	BombProperty byte
	SceneID      uint32
	UseActionID  uint8
}

type ActorCheckpoint struct {
	TimeMS      uint32
	RecordIndex int
	PlayerID    uint16
	Cell        battleengine.Cell
	PositionX   uint16
	PositionY   uint16
}

type Timeline struct {
	Inputs      []Input
	Checkpoints []ActorCheckpoint
	Native      []qbv.NativeEvent
}

// BuildTimeline converts only source-proven native actions. The lower two
// WalkAndDirection bits use the client's right/up/left/down order; bit 0x10 is
// the moving flag. Absolute positions are retained as checkpoints and are not
// fed back into the simulation.
func BuildTimeline(recording qbv.Recording) (Timeline, error) {
	native, err := qbv.DecodeNativeEvents(recording)
	if err != nil {
		return Timeline{}, err
	}
	timeline := Timeline{Native: native}
	type useActionKey struct {
		PlayerID   uint16
		ClientTime uint32
		ActionID   uint8
	}
	type pickupKey struct {
		PlayerID   uint16
		ClientTime uint32
		SceneID    uint32
	}
	seenUseActions := make(map[useActionKey]struct{})
	seenPickups := make(map[pickupKey]struct{})
	for _, event := range native {
		switch event.Kind {
		case qbv.NativeEventMovement:
			for _, entry := range event.Movement.Entries {
				move, err := nativeMovementDirection(entry.Move)
				if err != nil {
					return Timeline{}, fmt.Errorf("QBV event %d player %d: %w", event.Index, entry.PlayerID, err)
				}
				timeline.Inputs = append(timeline.Inputs, Input{
					TimeMS: event.Record.TimeMS, RecordIndex: event.Index, Kind: InputMovement,
					PlayerID: entry.PlayerID, Move: move,
					MoveEnd: battleengine.Position{X: int32(entry.Move.EndPosX), Y: int32(entry.Move.EndPosY)},
				})
				timeline.Checkpoints = append(timeline.Checkpoints, ActorCheckpoint{
					TimeMS: event.Record.TimeMS, RecordIndex: event.Index, PlayerID: entry.PlayerID,
					Cell:      battleengine.Cell{Row: int16(entry.Move.CurrentPosY / battleengine.CellSizePixels), Col: int16(entry.Move.CurrentPosX / battleengine.CellSizePixels)},
					PositionX: entry.Move.CurrentPosX, PositionY: entry.Move.CurrentPosY,
				})
			}
		case qbv.NativeEventBombPlaced:
			// Match the live transport boundary in Runtime.AcceptHumanBombPlacement.
			// Passing the native wire value straight to the engine adds one cell
			// to every blast arm and can falsely trap recorded survivors.
			if event.BombPlaced.Power <= 1 || event.BombPlaced.Power > 0x100 {
				return Timeline{}, fmt.Errorf("QBV event %d player %d: invalid native wire bomb power %d", event.Index, event.BombPlaced.PlayerID, event.BombPlaced.Power)
			}
			timeline.Inputs = append(timeline.Inputs, Input{
				TimeMS: event.Record.TimeMS, RecordIndex: event.Index, Kind: InputPlaceBomb, PlayerID: event.BombPlaced.PlayerID,
				BombCell:  battleengine.Cell{Row: int16(event.BombPlaced.Row), Col: int16(event.BombPlaced.Column)},
				BombPower: event.BombPlaced.Power - 1, BombProperty: event.BombPlaced.Property,
			})
		case qbv.NativeEventItem:
			// Local QBV stores the outgoing 0x0FAC and the arbitrator's 0x0FAD
			// confirmation. The pair is one verified pickup input, not two
			// collisions. Feeding it into replay keeps downstream transformation
			// and held-action state source-traceable even after an earlier movement
			// divergence; actor-position differences remain separate report data.
			key := pickupKey{PlayerID: event.Item.PlayerID, ClientTime: event.Item.ClientTime, SceneID: event.Item.ItemID}
			if _, duplicate := seenPickups[key]; duplicate {
				continue
			}
			seenPickups[key] = struct{}{}
			timeline.Inputs = append(timeline.Inputs, Input{
				TimeMS: event.Record.TimeMS, RecordIndex: event.Index, Kind: InputPickup,
				PlayerID: event.Item.PlayerID, SceneID: event.Item.ItemID,
			})
		case qbv.NativeEventUseItem:
			// A local QBV may retain both the 0x0FAF request and its 0x0FB0
			// arbitrator confirmation. They represent one key pulse. 0x0FB5 is
			// the resulting affection notification and is never an input.
			if event.Record.Schema == game.NotifyPlayerAffection {
				continue
			}
			actionID := uint8(event.UseItem.ItemID)
			if event.UseItem.ItemID <= 0xff && supportedActionID(actionID) {
				key := useActionKey{PlayerID: event.UseItem.PlayerID, ClientTime: event.UseItem.ClientTime, ActionID: actionID}
				if _, duplicate := seenUseActions[key]; duplicate {
					continue
				}
				seenUseActions[key] = struct{}{}
				timeline.Inputs = append(timeline.Inputs, Input{TimeMS: event.Record.TimeMS, RecordIndex: event.Index, Kind: InputUseAction, PlayerID: event.UseItem.PlayerID, UseActionID: actionID})
			}
		case qbv.NativeEventActorKilled:
			timeline.Inputs = append(timeline.Inputs, Input{TimeMS: event.Record.TimeMS, RecordIndex: event.Index, Kind: InputConfirmDeath, PlayerID: event.Killed.DestinationPlayerID})
		}
	}
	sort.SliceStable(timeline.Inputs, func(i, j int) bool {
		if timeline.Inputs[i].TimeMS != timeline.Inputs[j].TimeMS {
			return timeline.Inputs[i].TimeMS < timeline.Inputs[j].TimeMS
		}
		return timeline.Inputs[i].RecordIndex < timeline.Inputs[j].RecordIndex
	})
	sort.SliceStable(timeline.Checkpoints, func(i, j int) bool {
		if timeline.Checkpoints[i].TimeMS != timeline.Checkpoints[j].TimeMS {
			return timeline.Checkpoints[i].TimeMS < timeline.Checkpoints[j].TimeMS
		}
		return timeline.Checkpoints[i].RecordIndex < timeline.Checkpoints[j].RecordIndex
	})
	return timeline, nil
}

func nativeMovementDirection(movement game.PlayerMoveSequence) (battleengine.Direction, error) {
	// PLAYER_MOVE describes a bounded native path segment, not an indefinitely
	// held keyboard direction. The client may retain bit 0x10 in the terminal
	// sample while CurrentPos has already reached EndPos; that sample stops the
	// segment and must not make an offline replay walk beyond its native target.
	if movement.CurrentPosX == movement.EndPosX && movement.CurrentPosY == movement.EndPosY {
		return battleengine.DirectionNone, nil
	}
	value := movement.WalkAndDirection
	if value&0x10 == 0 {
		return battleengine.DirectionNone, nil
	}
	switch value & 0x03 {
	case 0:
		return battleengine.DirectionRight, nil
	case 1:
		return battleengine.DirectionUp, nil
	case 2:
		return battleengine.DirectionLeft, nil
	case 3:
		return battleengine.DirectionDown, nil
	default:
		return battleengine.DirectionNone, fmt.Errorf("invalid native movement byte 0x%02X", value)
	}
}

func supportedActionID(actionID uint8) bool {
	switch actionID {
	case 41, 42, 43, 44, 46, 63, 64:
		return true
	default:
		return false
	}
}
