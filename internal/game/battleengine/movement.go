package battleengine

import "fmt"

func (engine *Engine) moveActor(actorIndex int, direction Direction) ([]Event, bool) {
	actor := &engine.actors[actorIndex]
	direction = engine.transformInput(actor, direction)
	return engine.moveActorInWorldDirection(actorIndex, direction)
}

func (engine *Engine) moveActorInWorldDirection(actorIndex int, direction Direction) ([]Event, bool) {
	actor := &engine.actors[actorIndex]
	actor.Facing = direction
	actor.nativePreviousPosition = actor.Position
	distance := engine.consumeNativeMovementDistance(actor, direction, engine.rules.TickMS)
	if distance == 0 {
		return nil, false
	}
	start := actor.Position
	events := make([]Event, 0, 2)
	interactionAttempted := false
	// Native collision actions use a directional point 22 pixels from the
	// actor centre. Once that point enters the map element's cell, the CMapElem
	// push counter charges before ordinary movement resolution.
	if interactionEvent, interacted := engine.tryActorWorldInteraction(actorIndex, direction); interacted {
		interactionAttempted = true
		if interactionEvent.Kind != 0 {
			events = append(events, interactionEvent)
		}
		if interactionEvent.Kind == 0 {
			return events, false
		}
	}
	moveEvents, moved := engine.moveActorDisplacement(actorIndex, direction, start, distance, !interactionAttempted)
	return append(events, moveEvents...), moved
}

// moveActorDisplacement applies only geometry. Step has already processed the
// native generic push/kick callback before the hit callback and must not charge
// the same contact counter again during the later movement phase.
func (engine *Engine) moveActorDisplacement(actorIndex int, direction Direction, start Position, distance int, allowCorner bool) ([]Event, bool) {
	actor := &engine.actors[actorIndex]
	engine.resolveNativeMovementDisplacement(actor, direction, distance, allowCorner)
	if actor.Position == start {
		return nil, false
	}
	return []Event{{Kind: EventActorMoved, TimeMS: engine.elapsedMS, PlayerID: actor.PlayerID, Position: actor.Position, Cell: actor.Position.Cell()}}, true
}

func (engine *Engine) consumeNativeMovementDistance(actor *Actor, direction Direction, elapsedMS uint32) int {
	if actor == nil || elapsedMS == 0 || direction == DirectionNone {
		return 0
	}
	numerator := uint32(engine.effectiveSpeedPixelsPerSecond(actor, direction))*elapsedMS + actor.moveRemainder
	distance := int(numerator / 1000)
	actor.moveRemainder = numerator % 1000
	if distance > int(NativeMaxMovementPixelsPerUpdate) {
		distance = int(NativeMaxMovementPixelsPerUpdate)
	}
	return distance
}

// resolveNativeMovementDisplacement is the single physical displacement path
// shared by authoritative movement and AI legality probes. It mirrors one
// FUN_005cfc10 update: try the full primary-axis displacement, then its
// walkable cell-centre clamp, then the perpendicular corner branch.
func (engine *Engine) resolveNativeMovementDisplacement(actor *Actor, direction Direction, distance int, allowCorner bool) bool {
	if actor == nil || distance <= 0 {
		return false
	}
	dx, dy, ok := direction.delta()
	if !ok || direction == DirectionNone {
		return false
	}
	start := actor.Position
	candidate := Position{
		X: start.X + dx*int32(distance),
		Y: start.Y + dy*int32(distance),
	}
	blockedCandidate, blocked := engine.nativeMovementSegmentCollision(actor, start, direction, distance)
	if !blocked {
		actor.Position = candidate
		return true
	}
	// FUN_005cfc10 first tests the next centre on the travel axis when the
	// full displacement collides. If that centre is walkable and lies within
	// this update, stop there. Omitting this branch left 20 ms actors a few
	// pixels short of the endpoint produced by the 25 ms native projection.
	partial := nativeMovementDistanceToCenter(start, direction, distance)
	if partial < distance && engine.movementSegmentWalkable(actor, start, direction, partial) {
		actor.Position = Position{X: start.X + dx*int32(partial), Y: start.Y + dy*int32(partial)}
		return true
	}
	if allowCorner {
		// The swept check may stop inside a bomb's three-pixel entry strip
		// even when candidate has already crossed it. Corner classification
		// must use the point that actually blocked movement; reclassifying the
		// far endpoint loses the collision and falsely advertises a dead end.
		engine.applyNativeCornerCorrection(actor, blockedCandidate, direction, distance)
	}
	return actor.Position != start
}

func nativeMovementDistanceToCenter(position Position, direction Direction, distance int) int {
	coordinate, sign := position.X, int32(1)
	if direction == DirectionUp || direction == DirectionDown {
		coordinate = position.Y
	}
	if direction == DirectionLeft || direction == DirectionUp {
		sign = -1
	}
	center := coordinate - positiveRemainder(coordinate, CellSizePixels) + CellSizePixels/2
	remaining := (center - coordinate) * sign
	if remaining <= 0 {
		remaining += CellSizePixels
	}
	if remaining < int32(distance) {
		return int(remaining)
	}
	return distance
}

func (engine *Engine) tryActorWorldInteraction(actorIndex int, direction Direction) (Event, bool) {
	actor := &engine.actors[actorIndex]
	capabilities := engine.actorCapabilities(actor, direction)
	_, _, ok := direction.delta()
	if !ok || direction == DirectionNone {
		return Event{}, false
	}
	front, touching := nativeWorldInteractionCell(actor.Position, direction)
	if !touching {
		return Event{}, false
	}
	if capabilities.CanKickBomb {
		if engine.rules.NativeOutcomeAuthority && actor.Source == ParticipantVirtualAI {
			if index := engine.bombAt(front); index >= 0 {
				if target, found := engine.kickBombDestination(front, direction); found {
					return Event{
						Kind: EventBombKickRequested, TimeMS: engine.elapsedMS,
						PlayerID: actor.PlayerID, BombID: engine.bombs[index].ID,
						FromCell: front, Cell: target,
					}, true
				}
			}
		}
		if event, kicked := engine.kickBomb(actor.PlayerID, front, direction); kicked {
			return event, true
		}
	}
	// FUN_0060cac9 selects one of two CMapElem predicates. Normal actors use
	// canMove&2; the panda avatar uses LifeTime>=0. Both feed the same native
	// contact counter and 0xFB2/0xFB3 confirmation path.
	if event, handled := engine.chargeMapElementPush(actor.PlayerID, front, direction, capabilities.CanPushBreakable); handled {
		return event, true
	}
	return Event{}, false
}

func (engine *Engine) canActorWorldInteract(actor *Actor, direction Direction) bool {
	capabilities := engine.actorCapabilities(actor, direction)
	_, _, ok := direction.delta()
	if !ok || direction == DirectionNone {
		return false
	}
	front, touching := nativeWorldInteractionCell(actor.Position, direction)
	if !touching {
		return false
	}
	if capabilities.CanKickBomb && engine.bombAt(front) >= 0 {
		if _, found := engine.kickBombDestination(front, direction); found {
			return true
		}
	}
	return engine.canRequestMapElementPush(front, direction, capabilities.CanPushBreakable)
}

func nativeWorldInteractionCell(position Position, direction Direction) (Cell, bool) {
	point := position
	switch direction {
	case DirectionRight:
		point.X += NativeMapElementContactOffsetPixels
	case DirectionUp:
		point.Y -= NativeMapElementContactOffsetPixels
	case DirectionLeft:
		point.X -= NativeMapElementContactOffsetPixels
	case DirectionDown:
		point.Y += NativeMapElementContactOffsetPixels
	default:
		return Cell{}, false
	}
	cell := point.Cell()
	return cell, cell != position.Cell()
}

func (engine *Engine) kickBomb(playerID uint16, source Cell, direction Direction) (Event, bool) {
	index := engine.bombAt(source)
	if index < 0 {
		return Event{}, false
	}
	target, found := engine.kickBombDestination(source, direction)
	if !found {
		return Event{}, false
	}
	bomb := &engine.bombs[index]
	bomb.Cell = target
	bomb.FlightUntilMS = saturatingAdd(engine.elapsedMS, NativeBombFlightMS)
	return Event{Kind: EventBombKicked, TimeMS: engine.elapsedMS, PlayerID: playerID, BombID: bomb.ID, FromCell: source, Cell: target}, true
}

func (engine *Engine) kickBombDestination(source Cell, direction Direction) (Cell, bool) {
	dx, dy, ok := direction.delta()
	if !ok || direction == DirectionNone {
		return Cell{}, false
	}
	// The native producer probes from eight cells beyond the adjacent bomb
	// back toward it and chooses the farthest cell without a static map
	// element. Dynamic actors and bombs do not participate in this probe.
	for distance := int16(8); distance >= 1; distance-- {
		candidate := Cell{Row: source.Row + int16(dy)*distance, Col: source.Col + int16(dx)*distance}
		tile, inside := engine.grid.Cell(candidate)
		if inside && tile.Kind == CellOpen && !tile.MapElementOccupied {
			return candidate, true
		}
	}
	return Cell{}, false
}

// ApplyVerifiedBombMovement reconciles one native 0x139C confirmation. The
// producer already chose the farthest static-open destination, so the mirror
// checks identity/map bounds and applies that absolute result without running
// the Panda contact predicate a second time.
func (engine *Engine) ApplyVerifiedBombMovement(playerID uint16, bombID uint32, source, target Cell) (Event, error) {
	if engine == nil {
		return Event{}, fmt.Errorf("battle engine is nil")
	}
	if engine.actorIndex(playerID) < 0 {
		return Event{}, fmt.Errorf("verified bomb movement player %d is not a participant", playerID)
	}
	index := engine.bombIndexByID(bombID)
	if index < 0 {
		return Event{}, fmt.Errorf("verified bomb movement references unknown bomb %d", bombID)
	}
	if engine.bombs[index].Cell != source {
		return Event{}, fmt.Errorf("verified bomb %d is at %d,%d, not %d,%d", bombID, engine.bombs[index].Cell.Row, engine.bombs[index].Cell.Col, source.Row, source.Col)
	}
	tile, inside := engine.grid.Cell(target)
	if !inside || tile.Kind != CellOpen {
		return Event{}, fmt.Errorf("verified bomb %d target %d,%d is not static-open", bombID, target.Row, target.Col)
	}
	engine.bombs[index].Cell = target
	engine.bombs[index].FlightUntilMS = saturatingAdd(engine.elapsedMS, NativeBombFlightMS)
	return Event{Kind: EventBombKicked, TimeMS: engine.elapsedMS, PlayerID: playerID, BombID: bombID, FromCell: source, Cell: target}, nil
}

func (engine *Engine) chargeMapElementPush(playerID uint16, source Cell, direction Direction, pandaPredicate bool) (Event, bool) {
	moved, footprint, found := engine.pushableMapElement(source, pandaPredicate)
	if !found || !engine.canRequestMapElementPush(source, direction, pandaPredicate) {
		return Event{}, false
	}
	oldPulse := engine.elapsedMS / NativeMapElementPushPulseMS
	newPulse := saturatingAdd(engine.elapsedMS, engine.rules.TickMS) / NativeMapElementPushPulseMS
	pulses := newPulse - oldPulse
	if pulses == 0 {
		return Event{}, true
	}
	next := uint32(moved.PushCounter) + pulses*2
	if next <= NativeMapElementPushThreshold {
		for _, cell := range footprint {
			index := engine.gridIndex(cell)
			engine.grid.Cells[index].PushCounter = byte(next)
		}
		return Event{}, true
	}
	if engine.rules.NativeOutcomeAuthority {
		if actorIndex := engine.actorIndex(playerID); actorIndex >= 0 && engine.actors[actorIndex].Source == ParticipantVirtualAI {
			// The local contact counter decides when to request a push, but only
			// the arbitrator's 0x0FB3 moves the shared map element.
			for _, cell := range footprint {
				engine.grid.Cells[engine.gridIndex(cell)].PushCounter = byte(NativeMapElementPushThreshold + 1)
			}
			dx, dy, _ := direction.delta()
			return Event{
				Kind: EventMapElementMoveRequested, TimeMS: engine.elapsedMS,
				PlayerID: playerID, ObjectID: moved.MapElementID,
				FromCell: moved.ElementAnchor,
				Cell:     Cell{Row: moved.ElementAnchor.Row + int16(dy), Col: moved.ElementAnchor.Col + int16(dx)},
			}, true
		}
	}
	return engine.relocateMapElement(playerID, source, direction, pandaPredicate)
}

func (engine *Engine) relocateMapElement(playerID uint16, source Cell, direction Direction, pandaPredicate bool) (Event, bool) {
	dx, dy, _ := direction.delta()
	moved, footprint, found := engine.pushableMapElement(source, pandaPredicate)
	if !found {
		return Event{}, false
	}
	for _, cell := range footprint {
		target := Cell{Row: cell.Row + int16(dy), Col: cell.Col + int16(dx)}
		if _, inside := engine.grid.Cell(target); !inside {
			return Event{}, false
		}
	}
	oldCells := make(map[Cell]struct{}, len(footprint))
	oldTiles := make([]Tile, len(footprint))
	for index, cell := range footprint {
		oldCells[cell] = struct{}{}
		oldTiles[index], _ = engine.grid.Cell(cell)
		engine.grid.Cells[engine.gridIndex(cell)] = Tile{Kind: CellOpen, FlamePassable: true}
	}
	newAnchor := Cell{Row: moved.ElementAnchor.Row + int16(dy), Col: moved.ElementAnchor.Col + int16(dx)}
	for index, cell := range footprint {
		target := Cell{Row: cell.Row + int16(dy), Col: cell.Col + int16(dx)}
		tile := oldTiles[index]
		tile.ElementAnchor = newAnchor
		tile.PushCounter = 0
		engine.grid.Cells[engine.gridIndex(target)] = tile
	}
	for index := range engine.pickups {
		if _, belongs := oldCells[engine.pickups[index].Cell]; belongs && engine.pickups[index].State != PickupCollected {
			engine.pickups[index].Cell.Row += int16(dy)
			engine.pickups[index].Cell.Col += int16(dx)
		}
	}
	return Event{Kind: EventMapElementMoved, TimeMS: engine.elapsedMS, PlayerID: playerID, ObjectID: moved.MapElementID, FromCell: moved.ElementAnchor, Cell: newAnchor}, true
}

// ApplyVerifiedMapElementMovement reconciles one arbitrator-confirmed native
// 0x0FB3 movement. Row/Col identify the element's pre-move anchor; the native
// direction supplies the one-cell destination. The client has already charged
// the contact counter, so this boundary applies the move once without charging
// a second simulated counter.
func (engine *Engine) ApplyVerifiedMapElementMovement(playerID uint16, elementID uint32, source Cell, direction Direction) (Event, error) {
	if engine == nil {
		return Event{}, fmt.Errorf("battle engine is nil")
	}
	if engine.outcome.Ended {
		return Event{}, fmt.Errorf("battle has already ended")
	}
	actorIndex := engine.actorIndex(playerID)
	if actorIndex < 0 {
		return Event{}, fmt.Errorf("map-element movement player %d is not a participant", playerID)
	}
	tile, inside := engine.grid.Cell(source)
	if !inside || tile.MapElementID != elementID || tile.ElementAnchor != source {
		return Event{}, fmt.Errorf("map-element %d is not anchored at %d,%d", elementID, source.Row, source.Col)
	}
	if event, moved := engine.relocateMapElement(playerID, source, direction, false); moved {
		return event, nil
	}
	capabilities := engine.actorCapabilities(&engine.actors[actorIndex], direction)
	if capabilities.CanPushBreakable {
		if event, moved := engine.relocateMapElement(playerID, source, direction, true); moved {
			return event, nil
		}
	}
	return Event{}, fmt.Errorf("map-element %d cannot move from %d,%d in direction %d", elementID, source.Row, source.Col, direction)
}

func (engine *Engine) canRequestMapElementPush(source Cell, direction Direction, pandaPredicate bool) bool {
	dx, dy, ok := direction.delta()
	if !ok || direction == DirectionNone {
		return false
	}
	if _, _, found := engine.pushableMapElement(source, pandaPredicate); !found {
		return false
	}
	// FUN_0060cac9 validates exactly the cell one step beyond the contacted
	// map-element cell. It does not treat overlap with the element's old
	// footprint as free. That distinction prevents a 2x1 element from moving
	// along its long axis, matching the original client's producer gate.
	target := Cell{Row: source.Row + int16(dy), Col: source.Col + int16(dx)}
	targetTile, inside := engine.grid.Cell(target)
	if !inside || targetTile.MapElementOccupied || targetTile.MapElementID != 0 || engine.bombAt(target) >= 0 {
		return false
	}
	for _, object := range engine.fieldObjects {
		if object.Cell == target {
			return false
		}
	}
	for _, pickup := range engine.pickups {
		if pickup.Cell == target && pickup.State != PickupCollected {
			return false
		}
	}
	for _, actor := range engine.actors {
		if actor.State != ActorEliminated && actor.Position.Cell() == target {
			return false
		}
	}
	return true
}

func (engine *Engine) pushableMapElement(source Cell, pandaPredicate bool) (Tile, []Cell, bool) {
	sourceTile, inside := engine.grid.Cell(source)
	eligible := sourceTile.NormalPushable
	if pandaPredicate {
		eligible = sourceTile.PandaPushable
	}
	if !inside || !eligible || sourceTile.MapElementID == 0 || sourceTile.ElementWidth == 0 || sourceTile.ElementHeight == 0 {
		return Tile{}, nil, false
	}
	anchor := sourceTile.ElementAnchor
	if source.Row < anchor.Row || source.Col < anchor.Col ||
		source.Row >= anchor.Row+int16(sourceTile.ElementHeight) || source.Col >= anchor.Col+int16(sourceTile.ElementWidth) {
		return Tile{}, nil, false
	}
	footprint := make([]Cell, 0, int(sourceTile.ElementWidth)*int(sourceTile.ElementHeight))
	for row := int16(0); row < int16(sourceTile.ElementHeight); row++ {
		for col := int16(0); col < int16(sourceTile.ElementWidth); col++ {
			cell := Cell{Row: anchor.Row + row, Col: anchor.Col + col}
			tile, present := engine.grid.Cell(cell)
			cellEligible := tile.NormalPushable
			if pandaPredicate {
				cellEligible = tile.PandaPushable
			}
			if !present || !cellEligible || tile.MapElementID != sourceTile.MapElementID ||
				tile.ElementWidth != sourceTile.ElementWidth || tile.ElementHeight != sourceTile.ElementHeight || tile.ElementAnchor != anchor ||
				tile.PushCounter != sourceTile.PushCounter {
				return Tile{}, nil, false
			}
			footprint = append(footprint, cell)
		}
	}
	return sourceTile, footprint, true
}

func (engine *Engine) gridIndex(cell Cell) int {
	return int(cell.Row)*int(engine.grid.Width) + int(cell.Col)
}

// NativeMovementProjection describes the deterministic path advertised by one
// native PLAYER_MOVE sample. Current position remains authoritative; Corner
// and End are interpolation hints for the remote original client.
type NativeMovementProjection struct {
	Corner    Position
	HasCorner bool
	End       Position
}

// ProjectNativeMovement advances a disposable engine copy through the same
// pixel collision and corner-correction code as the authoritative simulation.
// It deliberately excludes pickups, bomb placement, kicking and map-element
// pushes: those are separate arbitrated transactions and must never be
// predicted by a movement packet.
//
// Client.exe FUN_005cf9b8 repeatedly advances a disposable position in fixed
// 25 ms increments until FUN_005cfc10 can no longer move it. End is therefore
// the complete current collision endpoint, not the coordinate expected at the
// next 150 ms network heartbeat. Dynamic objects present in this snapshot are
// still part of that collision query.
func (engine *Engine) ProjectNativeMovement(playerID uint16, direction Direction) (NativeMovementProjection, error) {
	if engine == nil {
		return NativeMovementProjection{}, fmt.Errorf("battle engine is nil")
	}
	index := engine.actorIndex(playerID)
	if index < 0 {
		return NativeMovementProjection{}, fmt.Errorf("movement projection player %d is not a participant", playerID)
	}
	projectionEngine := engine.Clone()
	actor := &projectionEngine.actors[index]
	result := NativeMovementProjection{End: actor.Position}
	if actor.State != ActorActive || direction == DirectionNone {
		return result, nil
	}
	actor.Facing = direction
	// At the slowest native speed a 25 ms step advances one pixel. A straight
	// path cannot traverse more than both map dimensions plus one correction,
	// so this guard is a fail-safe rather than a prediction horizon.
	maxSteps := (uint32(projectionEngine.grid.Width)+uint32(projectionEngine.grid.Height))*CellSizePixels + 1
	for step := uint32(0); step < maxSteps; step++ {
		before := actor.Position
		movedDirection, moved := projectionEngine.projectNativeMovementStep(actor, direction, NativeMovementProjectionStepMS)
		if !moved {
			break
		}
		if !result.HasCorner && movedDirection != direction {
			result.Corner = before
			result.HasCorner = true
		}
		result.End = actor.Position
	}
	// FUN_005cf9b8 normalizes only the travel-axis endpoint's trailing 9/1
	// pixel to the adjacent multiple of ten after the resolver has stopped.
	// It never retries a blocked update with a shorter displacement.
	coordinate := &result.End.Y
	if direction == DirectionLeft || direction == DirectionRight {
		coordinate = &result.End.X
	}
	switch *coordinate % 10 {
	case 9:
		*coordinate++
	case 1:
		*coordinate--
	}
	return result, nil
}

// projectNativeMovementStep mirrors only moveActorInWorldDirection's physical
// displacement. It is intentionally side-effect free outside the disposable
// projection engine used by ProjectNativeMovement.
func (engine *Engine) projectNativeMovementStep(actor *Actor, direction Direction, elapsedMS uint32) (Direction, bool) {
	if actor == nil {
		return DirectionNone, false
	}
	dx, dy, ok := direction.delta()
	if !ok || direction == DirectionNone {
		return DirectionNone, false
	}
	if elapsedMS == 0 {
		return DirectionNone, false
	}
	distance := engine.consumeNativeMovementDistance(actor, direction, elapsedMS)
	if distance == 0 {
		return DirectionNone, false
	}
	start := actor.Position
	candidate := Position{X: start.X + dx*int32(distance), Y: start.Y + dy*int32(distance)}
	if engine.resolveNativeMovementDisplacement(actor, direction, distance, true) {
		if actor.Position.X == candidate.X && actor.Position.Y == candidate.Y {
			return direction, true
		}
		return movementDirectionBetween(start, actor.Position), true
	}
	return DirectionNone, false
}

func movementDirectionBetween(start, end Position) Direction {
	switch {
	case end.X > start.X:
		return DirectionRight
	case end.X < start.X:
		return DirectionLeft
	case end.Y > start.Y:
		return DirectionDown
	case end.Y < start.Y:
		return DirectionUp
	default:
		return DirectionNone
	}
}

func (engine *Engine) positionWalkable(actor *Actor, position Position, direction Direction) bool {
	_, collisions, ok := engine.nativeLeadingEdgeCollisions(actor, position, direction)
	if !ok {
		return false
	}
	walkable := true
	touchedTypeOne := false
	for _, collision := range collisions {
		collisionCell, collisionType := collision.cell, collision.kind
		if collisionType == nativeCollisionNone {
			continue
		}
		if collisionType == nativeCollisionDynamicTypeOne && actor.NativePassActive && engine.nativeCellBeyondPassable(actor, collisionCell, direction) {
			continue
		}
		walkable = false
		if collisionType == nativeCollisionDynamicTypeOne && !actor.NativePassActive {
			touchedTypeOne = true
		}
	}
	if touchedTypeOne {
		// FUN_005b7999 does not touch only the corner that classified as a
		// type-1 object. Its inactive branch calls FUN_005d335c for both native
		// leading cells, in the order emitted by FUN_005b7d1b. When the two
		// corners straddle different cells, the remembered cell therefore
		// changes twice on every frame and the strict 500..600 ms charge can
		// never accumulate. Recording only the colliding corner incorrectly
		// made diagonal edge contact sufficient to pass a bubble.
		for _, cell := range nativePassTouchOrder(collisions, direction) {
			engine.recordNativePassCollision(actor, cell)
		}
	}
	return walkable
}

// movementSegmentWalkable preserves the native collision boundary when the
// fixed 20/25 ms simulation step advances more than the boundary's three-pixel
// width. Testing only the final point lets a fast actor jump from just outside
// a bubble to four or more pixels inside it, bypassing the type-1 collision
// predicate without ever activating NativePass. Client.exe reaches the same
// boundary through its higher-frequency render integration; the restricted
// engine therefore sweeps every integer pixel while retaining the native
// all-or-nothing displacement and corner-correction result for the update.
func (engine *Engine) movementSegmentWalkable(actor *Actor, start Position, direction Direction, distance int) bool {
	_, blocked := engine.nativeMovementSegmentCollision(actor, start, direction, distance)
	return !blocked
}

// nativeMovementSegmentCollision retains the first blocking candidate for the
// resolver's perpendicular branch. No coordinate is committed by this probe.
func (engine *Engine) nativeMovementSegmentCollision(actor *Actor, start Position, direction Direction, distance int) (Position, bool) {
	if actor == nil || distance <= 0 {
		return start, true
	}
	dx, dy, ok := direction.delta()
	if !ok || direction == DirectionNone {
		return start, true
	}
	for partial := 1; partial <= distance; partial++ {
		candidate := Position{
			X: start.X + dx*int32(partial),
			Y: start.Y + dy*int32(partial),
		}
		if !engine.positionWalkable(actor, candidate, direction) {
			return candidate, true
		}
	}
	return Position{}, false
}

func nativePassTouchOrder(collisions [2]nativePointCollisionResult, direction Direction) [2]Cell {
	// nativeLeadingEdgePoints is intentionally kept in geometric
	// negative/positive order because corner correction consumes that order.
	// FUN_005b7d1b emits the opposite order for right and up movement.
	if direction == DirectionRight || direction == DirectionUp {
		return [2]Cell{collisions[1].cell, collisions[0].cell}
	}
	return [2]Cell{collisions[0].cell, collisions[1].cell}
}

type nativePointCollisionResult struct {
	cell Cell
	kind nativeCollisionType
}

func (engine *Engine) nativeLeadingEdgeCollisions(actor *Actor, position Position, direction Direction) ([2]Position, [2]nativePointCollisionResult, bool) {
	points, ok := nativeLeadingEdgePoints(position, int32(engine.rules.ActorHalfSizePixels), direction)
	if !ok {
		return [2]Position{}, [2]nativePointCollisionResult{}, false
	}
	var collisions [2]nativePointCollisionResult
	for index, point := range points {
		collisions[index].cell, collisions[index].kind = engine.nativePointCollision(actor, point, direction)
	}
	return points, collisions, true
}

func (engine *Engine) applyNativeCornerCorrection(actor *Actor, blockedCandidate Position, direction Direction, distance int) bool {
	if actor == nil || distance <= 0 {
		return false
	}
	_, collisions, ok := engine.nativeLeadingEdgeCollisions(actor, blockedCandidate, direction)
	if !ok {
		return false
	}
	negativeBlocked := collisions[0].kind != nativeCollisionNone
	positiveBlocked := collisions[1].kind != nativeCollisionNone
	if negativeBlocked == positiveBlocked {
		return false
	}

	coordinate := actor.Position.Y
	negativeDirection, positiveDirection := DirectionUp, DirectionDown
	if direction == DirectionUp || direction == DirectionDown {
		coordinate = actor.Position.X
		negativeDirection, positiveDirection = DirectionLeft, DirectionRight
	}
	remainder := positiveRemainder(coordinate, CellSizePixels)
	correction := DirectionNone
	switch {
	case negativeBlocked && remainder < CellSizePixels/2:
		correction = positiveDirection
	case negativeBlocked && remainder >= CellSizePixels-int32(NativeCornerSlideTolerancePixels) &&
		engine.nativeCornerSideCellPassable(actor, positiveDirection):
		correction = positiveDirection
	case positiveBlocked && remainder >= CellSizePixels/2:
		correction = negativeDirection
	case positiveBlocked && remainder <= int32(NativeCornerSlideTolerancePixels) &&
		engine.nativeCornerSideCellPassable(actor, negativeDirection):
		correction = negativeDirection
	default:
		return false
	}

	dx, dy, _ := correction.delta()
	// The native corner branch also stops at its target centre instead of
	// overshooting it and correcting back on the next fixed frame.
	distance = nativeMovementDistanceToCenter(actor.Position, correction, distance)
	actor.Position.X += dx * int32(distance)
	actor.Position.Y += dy * int32(distance)
	return true
}

func (engine *Engine) nativeCornerSideCellPassable(actor *Actor, direction Direction) bool {
	dx, dy, ok := direction.delta()
	if actor == nil || !ok || direction == DirectionNone {
		return false
	}
	source := actor.Position.Cell()
	side := Cell{Row: source.Row + int16(dy), Col: source.Col + int16(dx)}
	tile, inside := engine.grid.Cell(side)
	if !inside {
		return false
	}
	capabilities := engine.actorCapabilities(actor, direction)
	return capabilities.TraverseStaticTerrain || tile.Kind == CellOpen
}

func positiveRemainder(value int32, modulus int32) int32 {
	remainder := value % modulus
	if remainder < 0 {
		remainder += modulus
	}
	return remainder
}

type nativeCollisionType byte

const (
	nativeCollisionNone nativeCollisionType = iota
	nativeCollisionStatic
	nativeCollisionDynamicTypeOne
)

func nativeLeadingEdgePoints(position Position, half int32, direction Direction) ([2]Position, bool) {
	switch direction {
	case DirectionRight:
		return [2]Position{{X: position.X + half, Y: position.Y - half}, {X: position.X + half, Y: position.Y + half}}, true
	case DirectionUp:
		return [2]Position{{X: position.X - half, Y: position.Y - half}, {X: position.X + half, Y: position.Y - half}}, true
	case DirectionLeft:
		return [2]Position{{X: position.X - half, Y: position.Y - half}, {X: position.X - half, Y: position.Y + half}}, true
	case DirectionDown:
		return [2]Position{{X: position.X - half, Y: position.Y + half}, {X: position.X + half, Y: position.Y + half}}, true
	default:
		return [2]Position{}, false
	}
}

func (engine *Engine) nativePointCollision(actor *Actor, point Position, direction Direction) (Cell, nativeCollisionType) {
	if point.X < 0 || point.Y < 0 {
		return point.Cell(), nativeCollisionStatic
	}
	cell := point.Cell()
	tile, inside := engine.grid.Cell(cell)
	capabilities := engine.actorCapabilities(actor, direction)
	if !inside || (!capabilities.TraverseStaticTerrain && tile.Kind != CellOpen) {
		return cell, nativeCollisionStatic
	}
	if engine.bombAt(cell) >= 0 && nativeDynamicBoundaryBlocks(point, direction) {
		return cell, nativeCollisionDynamicTypeOne
	}
	return cell, nativeCollisionNone
}

func nativeDynamicBoundaryBlocks(point Position, direction Direction) bool {
	var coordinate int32
	switch direction {
	case DirectionRight:
		coordinate = point.X
	case DirectionDown:
		coordinate = point.Y
	case DirectionLeft:
		coordinate = point.X
	case DirectionUp:
		coordinate = point.Y
	default:
		return false
	}
	remainder := positiveRemainder(coordinate, CellSizePixels)
	if direction == DirectionRight || direction == DirectionDown {
		return remainder < NativeDynamicEntryBoundaryPixels
	}
	return remainder >= CellSizePixels-NativeDynamicEntryBoundaryPixels
}

func (engine *Engine) nativeCellBeyondPassable(actor *Actor, collisionCell Cell, direction Direction) bool {
	dx, dy, ok := direction.delta()
	if !ok || direction == DirectionNone {
		return false
	}
	beyond := Cell{Row: collisionCell.Row + int16(dy), Col: collisionCell.Col + int16(dx)}
	tile, inside := engine.grid.Cell(beyond)
	if !inside {
		return false
	}
	capabilities := engine.actorCapabilities(actor, direction)
	if !capabilities.TraverseStaticTerrain && tile.Kind != CellOpen {
		return false
	}
	// The active branch in FUN_005b7999 advances the collided grid coordinate
	// by one whole cell and verifies that destination cell. Reusing the normal
	// +/-3 entry-boundary probe at its centre would incorrectly treat a second
	// adjacent bubble as open simply because the probe already lies inside it.
	return engine.bombAt(beyond) < 0
}

func (engine *Engine) recordNativePassCollision(actor *Actor, cell Cell) {
	if !actor.NativePassCollisionValid || actor.NativePassCollisionCell != cell {
		actor.NativePassCollisionValid = true
		actor.NativePassCollisionCell = cell
		actor.NativePassCollisionStartedAt = engine.elapsedMS
	}
	actor.NativePassCollisionLastAt = engine.elapsedMS
}

func (engine *Engine) activateNativePassState(actor *Actor) bool {
	if !actor.NativePassCollisionValid {
		return false
	}
	chargedFor := engine.elapsedMS - actor.NativePassCollisionStartedAt
	sinceCollision := engine.elapsedMS - actor.NativePassCollisionLastAt
	if chargedFor <= NativePassChargeMinMS || chargedFor >= NativePassChargeMaxMS || sinceCollision >= NativePassCollisionFreshMS {
		return false
	}
	speed := uint32(engine.effectiveSpeedPixelsPerSecond(actor, actor.Facing))
	if speed == 0 {
		return false
	}
	// FUN_005d33ab does not short-circuit on its active byte. A second
	// successful local placement in the same strict source window therefore
	// restarts StartedAt and DurationMS as well.
	actor.NativePassActive = true
	// FUN_005d33ab writes the current native scene clock immediately when the
	// strict activation predicate succeeds. Do not shift this to the next
	// simulation tick: the original client includes that interval in the
	// one-cell pass duration.
	actor.NativePassStartedAt = engine.elapsedMS
	actor.NativePassDurationMS = uint32(CellSizePixels) * 1000 / speed
	if actor.NativePassDurationMS == 0 {
		actor.NativePassDurationMS = 1
	}
	return true
}

func (engine *Engine) expireNativePassState(actor *Actor) {
	if !actor.NativePassActive || engine.elapsedMS-actor.NativePassStartedAt < actor.NativePassDurationMS {
		return
	}
	// FUN_005d3422 delegates to FUN_005d341a at the exact duration boundary.
	// The native reset is intentionally partial: it clears byte +0 and DWORD
	// +0x0c only. The remembered cell and last-contact time remain, preventing
	// the same type-1 object from immediately starting a second charge. A
	// different cell is what reinitializes the collision window.
	actor.NativePassActive = false
	actor.NativePassCollisionStartedAt = 0
}

func (engine *Engine) positionOverlapsCell(position Position, cell Cell) bool {
	half := int32(engine.rules.ActorHalfSizePixels)
	left, right := position.X-half, position.X+half
	top, bottom := position.Y-half, position.Y+half
	cellLeft := int32(cell.Col) * CellSizePixels
	cellTop := int32(cell.Row) * CellSizePixels
	return right >= cellLeft && left < cellLeft+CellSizePixels && bottom >= cellTop && top < cellTop+CellSizePixels
}

// PositionOverlapsCell exposes the engine's native actor-footprint test to a
// live transaction adapter. It is intentionally read-only: pickup intent is
// still generated only when the actor centre enters the item cell, while a
// rejected/pending request stays attached until the complete footprint exits.
func (engine *Engine) PositionOverlapsCell(position Position, cell Cell) bool {
	if engine == nil {
		return false
	}
	return engine.positionOverlapsCell(position, cell)
}
