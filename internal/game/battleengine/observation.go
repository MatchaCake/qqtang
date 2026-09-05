package battleengine

import "fmt"

type ActorObservation struct {
	PlayerID                     uint16
	RoleID                       uint16
	TeamID                       byte
	Source                       ParticipantSource
	Position                     Position
	Cell                         Cell
	State                        ActorState
	Facing                       Direction
	BombCapacity                 byte
	MaxBombCapacity              byte
	BombPower                    byte
	MaxBombPower                 byte
	SpeedRate                    byte
	MaxSpeedRate                 byte
	SpeedPixelsPerSecond         uint16
	ActiveBombs                  byte
	TrapExpiresAt                uint32
	HiddenPickupReachExpiresAt   uint32
	SceneFourEffectExpiresAt     uint32
	OxygenValue                  uint32
	TransformationSceneID        uint32
	AvatarRoleID                 uint16
	TransformationExpiresAt      uint32
	HarmProtectionExpiresAt      uint32
	MatchSugar                   uint32
	Capabilities                 ActorCapabilities
	MovementStatus               MovementStatusKind
	MovementStatusExpiresAt      uint32
	HeldActions                  [NativeBattleActionSlots]HeldActionSlot
	PublicBehavior               PublicBehaviorMemory
	NativePassCollisionValid     bool
	NativePassCollisionCell      Cell
	NativePassCollisionStartedAt uint32
	NativePassCollisionLastAt    uint32
	NativePassActive             bool
	NativePassStartedAt          uint32
	NativePassDurationMS         uint32
}

type Observation struct {
	SchemaVersion uint16
	ActionVersion uint16
	PlayerID      uint16
	// ClockMS is the absolute native game clock used by object deadlines. It
	// includes the 3-second Ready/Go offset; ElapsedMS intentionally does not.
	ClockMS           uint32
	ElapsedMS         uint32
	RemainingMS       uint32
	Grid              Grid
	Actors            []ActorObservation
	Bombs             []Bomb
	Flames            []Flame
	FieldObjects      []FieldObject
	ActionProjectiles []ActionProjectile
	Pickups           []Pickup
	PublicWallItems   PublicWallItemProfile
	Outcome           Outcome
}

// Observation returns the battlefield information available to one player.
// Standard QQTang maps expose terrain, actors and active world objects, but a
// policy must not read the server's hidden wall-item allocation. Hidden pickups
// enter this view only while this observer's native detector reaches them.
func (engine *Engine) Observation(playerID uint16) (Observation, error) {
	observations, err := engine.Observations([]uint16{playerID})
	if err != nil {
		return Observation{}, err
	}
	return observations[0], nil
}

// Observations projects one immutable public world snapshot for several
// participants. Returned observations may share their read-only grid and
// world-object storage with each other, but never with the authoritative
// engine. Per-observer pickups, local timers, capabilities and native-pass
// state remain independently projected. This avoids cloning the same world
// once per participant in vectorized training without widening visibility.
func (engine *Engine) Observations(playerIDs []uint16) ([]Observation, error) {
	if engine == nil {
		return nil, fmt.Errorf("battle engine is nil")
	}
	observerIndices := make([]int, len(playerIDs))
	for index, playerID := range playerIDs {
		observerIndices[index] = engine.actorIndex(playerID)
		if observerIndices[index] < 0 {
			return nil, fmt.Errorf("battle observation player %d is not a participant", playerID)
		}
	}
	remaining := uint32(0)
	roundElapsed := engine.RoundElapsedMS()
	if engine.elapsedMS < engine.rules.RoundDurationMS {
		remaining = engine.rules.RoundDurationMS - engine.elapsedMS
	}
	grid := engine.grid.Clone()
	bombs := engine.Bombs()
	flames := engine.Flames()
	fieldObjects := engine.FieldObjects()
	projectiles := engine.ActionProjectiles()
	publicActors := make([]ActorObservation, len(engine.actors))
	for actorIndex, actor := range engine.actors {
		publicActors[actorIndex] = ActorObservation{
			PlayerID: actor.PlayerID, RoleID: actor.RoleID, TeamID: actor.TeamID, Source: actor.Source,
			Position: actor.Position, Cell: actor.Position.Cell(), State: actor.State, Facing: actor.Facing,
			// Attribute pickups, including a random question pickup's resolved
			// result, are carried by the public collection event. Every client can
			// therefore maintain these current values without reading hidden wall
			// allocation or private process state.
			BombCapacity: actor.BombCapacity, MaxBombCapacity: actor.MaxBombCapacity,
			BombPower: actor.BombPower, MaxBombPower: actor.MaxBombPower,
			SpeedRate: actor.SpeedRate, MaxSpeedRate: actor.MaxSpeedRate,
			SpeedPixelsPerSecond:  actor.SpeedPixelsPerSecond,
			ActiveBombs:           byte(engine.activeBombCount(actor.PlayerID)),
			TransformationSceneID: actor.TransformationSceneID, AvatarRoleID: actor.AvatarRoleID,
			TransformationExpiresAt: actor.TransformationExpiresAt,
			HarmProtectionExpiresAt: actor.HarmProtectionExpiresAt,
			MovementStatus:          actor.MovementStatus,
			// Restricted rule-1 starts without private carried battle actions.
			// Later shortcut mutations are public pickup/use/drop/transform
			// events, so opponents can maintain this same inferred ledger.
			HeldActions: actor.HeldActions, PublicBehavior: actor.PublicBehavior,
		}
	}
	result := make([]Observation, len(playerIDs))
	for resultIndex, observerIndex := range observerIndices {
		playerID := playerIDs[resultIndex]
		actors := append([]ActorObservation(nil), publicActors...)
		actor := &engine.actors[observerIndex]
		visible := &actors[observerIndex]
		// Exact timers and collision state remain local. Public attributes and
		// shortcut mutations above are reconstructible from native events.
		visible.TrapExpiresAt = actor.TrapExpiresAt
		visible.HiddenPickupReachExpiresAt = actor.HiddenPickupReachExpiresAt
		visible.SceneFourEffectExpiresAt = actor.SceneFourEffectExpiresAt
		visible.OxygenValue = actor.OxygenValue
		visible.MatchSugar = actor.MatchSugar
		visible.MovementStatusExpiresAt = actor.MovementStatusExpiresAt
		visible.Capabilities = engine.actorCapabilities(actor, actor.Facing)
		visible.NativePassCollisionValid = actor.NativePassCollisionValid
		visible.NativePassCollisionCell = actor.NativePassCollisionCell
		visible.NativePassCollisionStartedAt = actor.NativePassCollisionStartedAt
		visible.NativePassCollisionLastAt = actor.NativePassCollisionLastAt
		visible.NativePassActive = actor.NativePassActive
		visible.NativePassStartedAt = actor.NativePassStartedAt
		visible.NativePassDurationMS = actor.NativePassDurationMS
		result[resultIndex] = Observation{
			SchemaVersion: ObservationSchemaVersion, ActionVersion: ActionSpaceVersion,
			PlayerID: playerID, ClockMS: engine.elapsedMS, ElapsedMS: roundElapsed, RemainingMS: remaining,
			Grid: grid, Actors: actors, Bombs: bombs, Flames: flames, FieldObjects: fieldObjects,
			ActionProjectiles: projectiles, Pickups: engine.observablePickups(actor),
			PublicWallItems: engine.publicWallItems, Outcome: engine.outcome,
		}
	}
	return result, nil
}

func (engine *Engine) observablePickups(observer *Actor) []Pickup {
	result := make([]Pickup, 0, len(engine.pickups))
	detectorActive := engine.hiddenPickupReachActive(observer)
	observerCell := observer.Position.Cell()
	for _, pickup := range engine.pickups {
		switch pickup.State {
		case PickupAvailable:
			result = append(result, pickup)
		case PickupHidden:
			if detectorActive && absoluteCellDelta(observerCell.Row, pickup.Cell.Row) <= 1 && absoluteCellDelta(observerCell.Col, pickup.Cell.Col) <= 1 {
				result = append(result, pickup)
			}
		}
	}
	return result
}

// LegalActions returns a stable action mask expansion. The ordering is fixed:
// wait/up/right/down/left without placement, then the same movements with a
// placement request. A blocked placement or movement is omitted.
func (engine *Engine) LegalActions(playerID uint16) ([]Action, error) {
	if engine == nil {
		return nil, fmt.Errorf("battle engine is nil")
	}
	index := engine.actorIndex(playerID)
	if index < 0 {
		return nil, fmt.Errorf("battle legal-action player %d is not a participant", playerID)
	}
	actor := engine.actors[index]
	if engine.outcome.Ended || actor.State == ActorEliminated {
		return []Action{{PlayerID: playerID}}, nil
	}
	if actor.State == ActorTrapped {
		result := []Action{{PlayerID: playerID}}
		tile, inside := engine.grid.Cell(actor.Position.Cell())
		if inside && tile.Kind == CellOpen && !tile.MapElementOccupied && actorHeldActionCount(&actor, 63) != 0 {
			// The combined action holds a direction through self-rescue. Native
			// input skips the trapped actor in the first frame, then consumes
			// the still-held direction on the next frame after the item callback.
			for _, direction := range [...]Direction{DirectionNone, DirectionUp, DirectionRight, DirectionDown, DirectionLeft} {
				if direction == DirectionNone || engine.canProduceNativeMovement(actor, direction) {
					result = append(result, Action{PlayerID: playerID, Move: direction, UseActionID: 63})
				}
			}
		}
		return result, nil
	}
	if actor.MovementStatus == MovementStatusForcedSlide {
		return []Action{{PlayerID: playerID}}, nil
	}
	directions := [...]Direction{DirectionNone, DirectionUp, DirectionRight, DirectionDown, DirectionLeft}
	legalMoves := make([]Direction, 0, len(directions))
	for _, direction := range directions {
		if direction == DirectionNone || engine.canProduceNativeMovement(actor, direction) {
			legalMoves = append(legalMoves, direction)
		}
	}
	result := make([]Action, 0, len(legalMoves)*8)
	for _, direction := range legalMoves {
		result = append(result, Action{PlayerID: playerID, Move: direction})
	}
	cell := actor.Position.Cell()
	tile, inside := engine.grid.Cell(cell)
	canPlace := inside && tile.Kind == CellOpen && !tile.MapElementOccupied &&
		engine.activeBombCount(playerID) < int(engine.actorCapabilities(&actor, actor.Facing).EffectiveBombCapacity) && engine.bombAt(cell) < 0
	if canPlace {
		for _, direction := range legalMoves {
			result = append(result, Action{PlayerID: playerID, Move: direction, PlaceBomb: true})
		}
	}
	for _, actionID := range [...]uint8{41, 42, 43, 44, 46, 64} {
		if inside && tile.Kind == CellOpen && !tile.MapElementOccupied && actorHeldActionCount(&actor, actionID) != 0 {
			for _, direction := range legalMoves {
				result = append(result, Action{PlayerID: playerID, Move: direction, UseActionID: actionID})
			}
		}
	}
	return result, nil
}

// canProduceNativeMovement classifies one held input by replaying the same
// whole-update collision path used by the authoritative engine. The original
// client accepts a key held into a wall and merely animates it; an AI action
// mask must instead answer whether that held input can ever advance along its
// requested axis (or perform a source-confirmed push/kick/pass interaction).
// A one-pixel shortcut is not equivalent when the next native update is four
// or five pixels and was the cause of long walking-in-place runs at cell edges.
func (engine *Engine) canProduceNativeMovement(actor Actor, direction Direction) bool {
	direction = engine.transformInput(&actor, direction)
	_, _, ok := direction.delta()
	if !ok || direction == DirectionNone {
		return false
	}
	if engine.canActorWorldInteract(&actor, direction) {
		return true
	}
	speed := uint32(engine.effectiveSpeedPixelsPerSecond(&actor, direction))
	if speed == 0 || engine.rules.TickMS == 0 {
		return false
	}

	start := actor.Position
	// One cell is the maximum perpendicular travel needed before the native
	// corner branch either reaches an aligned lane or proves a repeated state.
	// Extra steps cover integer speed remainders at the slowest native rate.
	denominator := speed * engine.rules.TickMS
	maxSteps := int((uint32(CellSizePixels)*1000+denominator-1)/denominator) + 8
	if maxSteps < 1 {
		maxSteps = 1
	}
	if maxSteps > 128 {
		maxSteps = 128
	}
	type movementState struct {
		position  Position
		remainder uint32
	}
	var seen [129]movementState
	seen[0] = movementState{position: actor.Position, remainder: actor.moveRemainder}
	seenCount := 1
	for step := 0; step < maxSteps; step++ {
		distance := engine.consumeNativeMovementDistance(&actor, direction, engine.rules.TickMS)
		if distance == 0 {
			continue
		}
		if engine.nativePassChargeCanProgress(&actor, direction, distance) {
			return true
		}
		engine.resolveNativeMovementDisplacement(&actor, direction, distance, true)
		if nativeMovementAdvancedOnRequestedAxis(start, actor.Position, direction) {
			return true
		}
		// A rejected update can still change moveRemainder. For example, a
		// carried remainder may request four pixels now and three on the next
		// tick; the shorter update can reach a valid collision edge. Continue
		// until the complete position+phase state repeats instead of treating
		// the first zero-displacement frame as permanent.
		state := movementState{position: actor.Position, remainder: actor.moveRemainder}
		for index := 0; index < seenCount; index++ {
			if seen[index] == state {
				return false
			}
		}
		seen[seenCount] = state
		seenCount++
	}
	return false
}

func nativeMovementAdvancedOnRequestedAxis(start Position, current Position, direction Direction) bool {
	switch direction {
	case DirectionRight:
		return current.X > start.X
	case DirectionUp:
		return current.Y < start.Y
	case DirectionLeft:
		return current.X < start.X
	case DirectionDown:
		return current.Y > start.Y
	default:
		return false
	}
}

// nativePassChargeCanProgress preserves the only intentional zero-displacement
// movement input. FUN_005b7999 can charge passage only when both leading probes
// resolve to the same type-1 cell; split-corner contact rewrites the remembered
// cell twice and can never reach the strict 500..600 ms placement window.
func (engine *Engine) nativePassChargeCanProgress(actor *Actor, direction Direction, distance int) bool {
	if actor == nil || actor.NativePassActive || distance <= 0 {
		return false
	}
	dx, dy, ok := direction.delta()
	if !ok || direction == DirectionNone {
		return false
	}
	start := actor.Position
	for partial := 1; partial <= distance; partial++ {
		candidate := Position{X: start.X + dx*int32(partial), Y: start.Y + dy*int32(partial)}
		_, collisions, collisionOK := engine.nativeLeadingEdgeCollisions(actor, candidate, direction)
		if !collisionOK {
			return false
		}
		blocked := collisions[0].kind != nativeCollisionNone || collisions[1].kind != nativeCollisionNone
		if !blocked {
			continue
		}
		if collisions[0].kind != nativeCollisionDynamicTypeOne ||
			collisions[1].kind != nativeCollisionDynamicTypeOne ||
			collisions[0].cell != collisions[1].cell ||
			!engine.nativeCellBeyondPassable(actor, collisions[0].cell, direction) {
			return false
		}
		startedAt := engine.elapsedMS
		if actor.NativePassCollisionValid && actor.NativePassCollisionCell == collisions[0].cell {
			startedAt = actor.NativePassCollisionStartedAt
		}
		return engine.elapsedMS-startedAt < NativePassChargeMaxMS
	}
	return false
}
