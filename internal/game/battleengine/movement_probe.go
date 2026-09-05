package battleengine

import "fmt"

// NativeMovementProbe is a test-only projection of one FUN_005cfc10 call.
// It deliberately excludes map-element pushing, bomb kicking, pickups and
// time advancement because those are performed outside the original movement
// resolver. The production runtime never needs to call this API.
type NativeMovementProbe struct {
	Grid                 Grid
	Position             Position
	Direction            Direction
	TickMS               uint32
	SpeedPixelsPerSecond uint16
	ElapsedMS            uint32
	Bombs                []Bomb
	Pass                 NativePassProbeState
}

type NativePassProbeState struct {
	CollisionValid     bool
	CollisionCell      Cell
	CollisionStartedAt uint32
	CollisionLastAt    uint32
	Active             bool
	StartedAt          uint32
	DurationMS         uint32
}

type NativeMovementProbeResult struct {
	Position Position
	Moved    bool
	Pass     NativePassProbeState
}

// ResolveNativeMovementProbe executes only the deterministic whole-update movement
// and corner-correction portion shared with the live engine. It exists so a
// versioned original-client micro-oracle can exercise the exact production
// implementation rather than a second test reimplementation.
func ResolveNativeMovementProbe(probe NativeMovementProbe) (NativeMovementProbeResult, error) {
	if probe.TickMS == 0 || probe.SpeedPixelsPerSecond == 0 {
		return NativeMovementProbeResult{}, fmt.Errorf("native movement probe has zero tick or speed")
	}
	if _, _, ok := probe.Direction.delta(); !ok || probe.Direction == DirectionNone {
		return NativeMovementProbeResult{}, fmt.Errorf("native movement probe has invalid direction %d", probe.Direction)
	}
	grid := probe.Grid.Clone()
	// Pushing is an outer gameplay interaction and must not consume a resolver
	// probe. Preserve collision/flame semantics while removing only its action
	// metadata from the isolated copy.
	for index := range grid.Cells {
		grid.Cells[index].MapElementID = 0
		grid.Cells[index].NormalPushable = false
		grid.Cells[index].PandaPushable = false
		grid.Cells[index].PushCounter = 0
		grid.Cells[index].ElementWidth = 0
		grid.Cells[index].ElementHeight = 0
		grid.Cells[index].ElementAnchor = Cell{}
	}
	spawn, ok := firstOpenProbeCell(grid)
	if !ok {
		return NativeMovementProbeResult{}, fmt.Errorf("native movement probe grid has no open cell")
	}
	config := Config{
		Grid: grid,
		Rules: Rules{
			TickMS: probe.TickMS, RoundDurationMS: StandardRoundTimeMS,
			BombFuseMS: NativeBombFuseMS, FlameDurationMS: NativeFlameDurationMS,
			ActorHalfSizePixels: NativeActorHalfSizePixels,
		},
		Participants: []Participant{
			{PlayerID: 1, TeamID: 1, Source: ParticipantHuman, Spawn: spawn, SpeedPixelsPerSecond: probe.SpeedPixelsPerSecond, BombCapacity: 1, BombPower: 1},
			{PlayerID: 2, TeamID: 2, Source: ParticipantHuman, Spawn: spawn, SpeedPixelsPerSecond: 40, BombCapacity: 1, BombPower: 1},
		},
	}
	engine, err := New(config)
	if err != nil {
		return NativeMovementProbeResult{}, err
	}
	engine.elapsedMS = probe.ElapsedMS
	engine.bombs = append([]Bomb(nil), probe.Bombs...)
	actor := &engine.actors[0]
	actor.Position = probe.Position
	actor.Facing = probe.Direction
	actor.NativePassCollisionValid = probe.Pass.CollisionValid
	actor.NativePassCollisionCell = probe.Pass.CollisionCell
	actor.NativePassCollisionStartedAt = probe.Pass.CollisionStartedAt
	actor.NativePassCollisionLastAt = probe.Pass.CollisionLastAt
	actor.NativePassActive = probe.Pass.Active
	actor.NativePassStartedAt = probe.Pass.StartedAt
	actor.NativePassDurationMS = probe.Pass.DurationMS

	distance64 := uint64(probe.SpeedPixelsPerSecond) * uint64(probe.TickMS) / 1000
	if distance64 > 1<<20 {
		return NativeMovementProbeResult{}, fmt.Errorf("native movement probe distance %d is unreasonable", distance64)
	}
	distance := int(distance64)
	if distance > int(NativeMaxMovementPixelsPerUpdate) {
		distance = int(NativeMaxMovementPixelsPerUpdate)
	}
	if distance == 0 {
		return nativeMovementProbeResult(actor, false), nil
	}
	start := actor.Position
	engine.resolveNativeMovementDisplacement(actor, probe.Direction, distance, true)
	return nativeMovementProbeResult(actor, actor.Position != start), nil
}

// ResolveNativePassTouchProbe executes one FUN_005d335c collision-state touch
// against the same state transition used by live Engine movement. It is kept
// deliberately narrow so passive original-client traces can verify the
// production traversal timer without constructing an otherwise unrelated
// battle.
func ResolveNativePassTouchProbe(state NativePassProbeState, now uint32, cell Cell) NativePassProbeState {
	engine := Engine{elapsedMS: now}
	actor := Actor{}
	applyNativePassProbeState(&actor, state)
	engine.recordNativePassCollision(&actor, cell)
	return nativePassProbeState(&actor)
}

// ResolveNativePassActivateProbe executes one FUN_005d33ab activation check
// against the live engine transition. The speed parameter is intentionally
// explicit: the native micro-oracle validates the strict time predicates and
// timestamp transition independently from role attribute reconstruction.
func ResolveNativePassActivateProbe(state NativePassProbeState, now uint32, speedPixelsPerSecond uint16) NativePassProbeState {
	engine := Engine{elapsedMS: now, rules: Rules{TickMS: 1}}
	actor := Actor{Participant: Participant{SpeedPixelsPerSecond: speedPixelsPerSecond}, Facing: DirectionRight}
	applyNativePassProbeState(&actor, state)
	engine.activateNativePassState(&actor)
	return nativePassProbeState(&actor)
}

func firstOpenProbeCell(grid Grid) (Cell, bool) {
	for row := int16(0); row < int16(grid.Height); row++ {
		for col := int16(0); col < int16(grid.Width); col++ {
			cell := Cell{Row: row, Col: col}
			tile, _ := grid.Cell(cell)
			if tile.Kind == CellOpen {
				return cell, true
			}
		}
	}
	return Cell{}, false
}

func nativeMovementProbeResult(actor *Actor, moved bool) NativeMovementProbeResult {
	return NativeMovementProbeResult{
		Position: actor.Position,
		Moved:    moved,
		Pass:     nativePassProbeState(actor),
	}
}

func applyNativePassProbeState(actor *Actor, state NativePassProbeState) {
	actor.NativePassCollisionValid = state.CollisionValid
	actor.NativePassCollisionCell = state.CollisionCell
	actor.NativePassCollisionStartedAt = state.CollisionStartedAt
	actor.NativePassCollisionLastAt = state.CollisionLastAt
	actor.NativePassActive = state.Active
	actor.NativePassStartedAt = state.StartedAt
	actor.NativePassDurationMS = state.DurationMS
}

func nativePassProbeState(actor *Actor) NativePassProbeState {
	return NativePassProbeState{
		CollisionValid: actor.NativePassCollisionValid, CollisionCell: actor.NativePassCollisionCell,
		CollisionStartedAt: actor.NativePassCollisionStartedAt, CollisionLastAt: actor.NativePassCollisionLastAt,
		Active: actor.NativePassActive, StartedAt: actor.NativePassStartedAt, DurationMS: actor.NativePassDurationMS,
	}
}
