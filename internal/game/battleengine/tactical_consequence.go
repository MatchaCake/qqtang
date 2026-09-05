package battleengine

import "sync"

// TacticalReachabilityHorizonCount is the number of fixed real-time windows
// exposed by TacticalConsequences. They span immediate movement through the
// complete native three-second fuse and flame lifetime.
const TacticalReachabilityHorizonCount = 5

// TacticalMovementCandidateCount matches the native actor direction factor:
// wait, up, right, down and left.  Candidate facts describe the consequence
// of applying that held direction for the caller's complete decision interval
// and then retaining freedom to choose later movement. They are observations
// rather than action masks or policy decisions.
const TacticalMovementCandidateCount = 5

var tacticalReachabilityHorizonsMS = [TacticalReachabilityHorizonCount]uint32{
	500, 1_000, 2_000, 3_000, NativeBombFuseMS + NativeFlameDurationMS,
}

// TacticalConsequences contains deterministic, visibility-safe facts about
// the current world and the counterfactual result of placing one bubble on the
// actor's current cell. It does not choose an action. The hypothetical bubble
// is inserted through the same placement rule and projected by DangerTimeline,
// so chain timing, walls, flying bubbles and flame lifetime stay authoritative.
// Known-danger reachability is a conservative native-physics estimate, not a
// claim that an actor or policy is unconditionally safe: it uses production
// terrain, bomb, capability and sub-cell collision rules, then retains only
// whole-cell routes that survive the bombs and flames already present in the
// snapshot. It deliberately does not predict future opponent actions, model
// decision cadence or replace native movement. These values are observations,
// never evaluation outcomes or hard action masks.
//
// Every area is normalized by the public map cell count. Enemy aggregates are
// additionally averaged across active enemies. This keeps the meaning stable
// across map and team sizes without hiding the absolute player/team context
// already present in the observation schema.
type TacticalConsequences struct {
	CurrentKnownDangerReachable          [TacticalReachabilityHorizonCount]float32
	BombKnownDangerReachable             [TacticalReachabilityHorizonCount]float32
	CurrentDirectionKnownDangerReachable [TacticalMovementCandidateCount][TacticalReachabilityHorizonCount]float32
	BombDirectionKnownDangerReachable    [TacticalMovementCandidateCount][TacticalReachabilityHorizonCount]float32
	CurrentDirectionWholeCellRefugeFound [TacticalMovementCandidateCount]bool
	CurrentDirectionEarliestRefuge       [TacticalMovementCandidateCount]float32
	BombDirectionWholeCellRefugeFound    [TacticalMovementCandidateCount]bool
	BombDirectionEarliestRefuge          [TacticalMovementCandidateCount]float32
	KnownDangerProjectionValid           bool
	BombLegal                            bool
	BombKnownDangerProjectionValid       bool
	BombEarliestWholeCellRefugeEstimate  float32
	EnemyCurrentKnownReachable           [3]float32
	EnemyBombKnownReachable              [3]float32
	EnemyKnownReachReduction             [3]float32
	AllyKnownReachReduction              float32
	NewEnemyThreatRatio                  float32
	NewAllyThreatRatio                   float32
	NewBreakableThreatRatio              float32
	AcceleratedChainRatio                float32
}

type tacticalReachability struct {
	safeRatios  [TacticalReachabilityHorizonCount]float32
	refugeFound bool
	refugeAtMS  uint32
	valid       bool
}

type tacticalMovementCandidateReachabilities struct {
	safeRatios  [TacticalMovementCandidateCount][TacticalReachabilityHorizonCount]float32
	refugeFound [TacticalMovementCandidateCount]bool
	refugeAtMS  [TacticalMovementCandidateCount]uint32
}

type tacticalQueueEntry struct {
	cellIndex int
	arrivalMS uint32
}

type tacticalReachabilityScratch struct {
	arrival []uint32
	blocked []bool
	queue   []tacticalQueueEntry
}

var tacticalReachabilityScratchPool = sync.Pool{
	New: func() any { return &tacticalReachabilityScratch{} },
}

// TacticalConsequencesForPlayers amortizes the current-world reachability
// projection across all requested actors. Each actor still receives its own
// exact placement counterfactual because bubble ownership, capacity, power,
// position and team relationships differ.
func (engine *Engine) TacticalConsequencesForPlayers(
	playerIDs []uint16,
	current DangerTimeline,
) (map[uint16]TacticalConsequences, error) {
	decisionMS := uint32(0)
	if engine != nil {
		decisionMS = engine.rules.TickMS
	}
	return engine.TacticalConsequencesForPlayersAtDecision(
		playerIDs, current, decisionMS,
	)
}

// TacticalConsequencesForPlayersAtDecision computes candidate movement facts
// for the complete interval during which the caller will hold one policy
// decision. Training and live deployment pass the same 100 ms cadence; the
// simpler method above retains one-tick semantics for engine-only callers.
func (engine *Engine) TacticalConsequencesForPlayersAtDecision(
	playerIDs []uint16,
	current DangerTimeline,
	decisionMS uint32,
) (map[uint16]TacticalConsequences, error) {
	result := make(map[uint16]TacticalConsequences, len(playerIDs))
	if engine == nil || len(playerIDs) == 0 {
		return result, nil
	}
	horizons := tacticalHorizonsWithin(current.HorizonMS)
	var activeActorIndices [MaxParticipants]int
	activeCount := 0
	var currentReachability [MaxParticipants]tacticalReachability
	for index := range engine.actors {
		actor := &engine.actors[index]
		if actor.State != ActorActive {
			continue
		}
		activeActorIndices[activeCount] = index
		activeCount++
		currentReachability[index] = engine.tacticalSafeReachability(
			actor.PlayerID, current, horizons, nil,
		)
	}

	for _, playerID := range playerIDs {
		actorIndex := engine.actorIndex(playerID)
		if actorIndex < 0 {
			continue
		}
		self := &engine.actors[actorIndex]
		facts := TacticalConsequences{}
		facts.CurrentKnownDangerReachable = currentReachability[actorIndex].safeRatios
		facts.KnownDangerProjectionValid = currentReachability[actorIndex].valid
		currentCandidates := engine.tacticalMovementCandidateReachability(
			actorIndex, current, horizons, nil, decisionMS,
		)
		facts.CurrentDirectionKnownDangerReachable = currentCandidates.safeRatios
		facts.CurrentDirectionWholeCellRefugeFound = currentCandidates.refugeFound
		facts.CurrentDirectionEarliestRefuge = engine.tacticalCandidateRefugeEstimates(
			currentCandidates, current.HorizonMS,
		)
		facts.EnemyCurrentKnownReachable = engine.aggregateEnemyReachability(actorIndex, &currentReachability)
		if self.State != ActorActive || engine.outcome.Ended {
			result[playerID] = facts
			continue
		}

		bomb, placed := engine.prospectiveBomb(actorIndex, true)
		if !placed {
			result[playerID] = facts
			continue
		}
		facts.BombLegal = true
		counterDanger, err := engine.dangerTimelineWithAdditionalBomb(current.HorizonMS, &bomb)
		if err != nil {
			return nil, err
		}
		var counterReachability [MaxParticipants]tacticalReachability
		for _, activeIndex := range activeActorIndices[:activeCount] {
			activeID := engine.actors[activeIndex].PlayerID
			counterReachability[activeIndex] = engine.tacticalSafeReachability(
				activeID, counterDanger, horizons, &bomb,
			)
		}
		selfCounter := counterReachability[actorIndex]
		facts.BombKnownDangerReachable = selfCounter.safeRatios
		facts.BombKnownDangerProjectionValid = selfCounter.valid
		bombCandidates := engine.tacticalMovementCandidateReachability(
			actorIndex, counterDanger, horizons, &bomb, decisionMS,
		)
		facts.BombDirectionKnownDangerReachable = bombCandidates.safeRatios
		facts.BombDirectionWholeCellRefugeFound = bombCandidates.refugeFound
		facts.BombDirectionEarliestRefuge = engine.tacticalCandidateRefugeEstimates(
			bombCandidates, current.HorizonMS,
		)
		if selfCounter.refugeFound && current.HorizonMS != 0 && selfCounter.refugeAtMS >= engine.elapsedMS {
			facts.BombEarliestWholeCellRefugeEstimate = clampUnitFloat32(
				float32(selfCounter.refugeAtMS-engine.elapsedMS) / float32(current.HorizonMS),
			)
		}
		facts.EnemyBombKnownReachable = engine.aggregateEnemyReachability(actorIndex, &counterReachability)
		for index := range facts.EnemyKnownReachReduction {
			facts.EnemyKnownReachReduction[index] = positiveDifference(
				facts.EnemyCurrentKnownReachable[index], facts.EnemyBombKnownReachable[index],
			)
		}
		facts.AllyKnownReachReduction = engine.aggregateAllyReachabilityReduction(
			actorIndex, &currentReachability, &counterReachability,
		)
		facts.NewEnemyThreatRatio, facts.NewAllyThreatRatio = engine.actorThreatExpansion(
			self, current, counterDanger,
		)
		facts.NewBreakableThreatRatio = engine.breakableThreatExpansion(current, counterDanger)
		facts.AcceleratedChainRatio = engine.acceleratedChainRatio(current, counterDanger)
		result[playerID] = facts
	}
	return result, nil
}

// TacticalConsequencesForPlayer is the deployment convenience form.
func (engine *Engine) TacticalConsequencesForPlayer(
	playerID uint16,
	current DangerTimeline,
) (TacticalConsequences, error) {
	all, err := engine.TacticalConsequencesForPlayers([]uint16{playerID}, current)
	if err != nil {
		return TacticalConsequences{}, err
	}
	return all[playerID], nil
}

// TacticalConsequencesForPlayerAtDecision is the single-actor deployment
// convenience form with an explicit held-decision interval.
func (engine *Engine) TacticalConsequencesForPlayerAtDecision(
	playerID uint16,
	current DangerTimeline,
	decisionMS uint32,
) (TacticalConsequences, error) {
	all, err := engine.TacticalConsequencesForPlayersAtDecision(
		[]uint16{playerID}, current, decisionMS,
	)
	if err != nil {
		return TacticalConsequences{}, err
	}
	return all[playerID], nil
}

func tacticalHorizonsWithin(horizonMS uint32) [TacticalReachabilityHorizonCount]uint32 {
	result := tacticalReachabilityHorizonsMS
	for index := range result {
		if result[index] > horizonMS {
			result[index] = horizonMS
		}
	}
	return result
}

func (engine *Engine) tacticalSafeReachability(
	playerID uint16,
	timeline DangerTimeline,
	horizons [TacticalReachabilityHorizonCount]uint32,
	additionalBomb *Bomb,
) tacticalReachability {
	actorIndex := engine.actorIndex(playerID)
	if actorIndex < 0 || engine.actors[actorIndex].State != ActorActive || len(engine.grid.Cells) == 0 {
		return tacticalReachability{}
	}
	actor := engine.actors[actorIndex]
	return engine.tacticalSafeReachabilityFromActor(
		&actor, engine.elapsedMS, timeline, horizons, additionalBomb,
	)
}

// tacticalMovementCandidateReachability exposes what each native direction
// changes after one real engine tick.  It reuses the production displacement,
// corner correction, collision footprint, speed remainder and input transform,
// then runs the same conservative time-expanded reachability used by the
// global facts.  Existing bombs and a prospective newly placed bomb remain
// blocking for later route segments.  World interactions such as pushing a
// map element are deliberately not predicted here; leaving them unchanged is
// conservative and avoids claiming that an unconfirmed interaction succeeded.
func (engine *Engine) tacticalMovementCandidateReachability(
	actorIndex int,
	timeline DangerTimeline,
	horizons [TacticalReachabilityHorizonCount]uint32,
	additionalBomb *Bomb,
	decisionMS uint32,
) tacticalMovementCandidateReachabilities {
	result := tacticalMovementCandidateReachabilities{}
	if engine == nil || actorIndex < 0 || actorIndex >= len(engine.actors) || len(engine.grid.Cells) == 0 {
		return result
	}
	base := &engine.actors[actorIndex]
	if base.State != ActorActive || base.MovementStatus == MovementStatusForcedSlide {
		return result
	}
	if decisionMS == 0 {
		decisionMS = engine.rules.TickMS
	}
	startAtMS := saturatingAdd(engine.elapsedMS, decisionMS)
	directions := [...]Direction{
		DirectionNone, DirectionUp, DirectionRight, DirectionDown, DirectionLeft,
	}
candidates:
	for index, requested := range directions {
		candidate := *base
		worldDirection := DirectionNone
		if requested != DirectionNone {
			worldDirection = engine.transformInput(&candidate, requested)
			candidate.Facing = worldDirection
		}
		remainingMS := decisionMS
		for remainingMS != 0 {
			now := saturatingAdd(engine.elapsedMS, decisionMS-remainingMS)
			// Native hits sample the current position before this frame moves.
			// Do not skip an intervening impact by forecasting only the final
			// position at the next policy decision (including the wait action).
			if !tacticalCellSafeDuring(timeline, candidate.Position.Cell(), now, now) {
				continue candidates
			}
			stepMS := engine.rules.TickMS
			if stepMS == 0 || stepMS > remainingMS {
				stepMS = remainingMS
			}
			if worldDirection != DirectionNone {
				engine.projectNativeMovementStep(&candidate, worldDirection, stepMS)
			}
			remainingMS -= stepMS
		}
		reachability := engine.tacticalSafeReachabilityFromActor(
			&candidate, startAtMS, timeline, horizons, additionalBomb,
		)
		if reachability.valid {
			result.safeRatios[index] = reachability.safeRatios
			result.refugeFound[index] = reachability.refugeFound
			result.refugeAtMS[index] = reachability.refugeAtMS
		}
	}
	return result
}

func (engine *Engine) tacticalCandidateRefugeEstimates(
	candidates tacticalMovementCandidateReachabilities,
	horizonMS uint32,
) [TacticalMovementCandidateCount]float32 {
	result := [TacticalMovementCandidateCount]float32{}
	if engine == nil || horizonMS == 0 {
		return result
	}
	for index, found := range candidates.refugeFound {
		if !found || candidates.refugeAtMS[index] < engine.elapsedMS {
			continue
		}
		result[index] = clampUnitFloat32(
			float32(candidates.refugeAtMS[index]-engine.elapsedMS) / float32(horizonMS),
		)
	}
	return result
}

func (engine *Engine) tacticalSafeReachabilityFromActor(
	actor *Actor,
	startAtMS uint32,
	timeline DangerTimeline,
	horizons [TacticalReachabilityHorizonCount]uint32,
	additionalBomb *Bomb,
) tacticalReachability {
	result := tacticalReachability{}
	if engine == nil || actor == nil || actor.State != ActorActive || len(engine.grid.Cells) == 0 {
		return result
	}
	if actor.MovementStatus == MovementStatusForcedSlide {
		// Direction input is not authoritative during native forced slide. Keep
		// valid=false instead of encoding an ambiguous zero as "no refuge".
		return result
	}
	start := actor.Position.Cell()
	startIndex, inside := engine.tacticalCellIndex(start)
	if !inside {
		return result
	}
	result.valid = true
	scratch := tacticalReachabilityScratchPool.Get().(*tacticalReachabilityScratch)
	cellTotal := len(engine.grid.Cells)
	if cap(scratch.arrival) < cellTotal {
		scratch.arrival = make([]uint32, cellTotal)
	}
	if cap(scratch.blocked) < cellTotal {
		scratch.blocked = make([]bool, cellTotal)
	}
	arrival := scratch.arrival[:cellTotal]
	blocked := scratch.blocked[:cellTotal]
	for index := range arrival {
		arrival[index] = NoDangerImpact
		blocked[index] = false
	}
	for _, bomb := range engine.bombs {
		if index, ok := engine.tacticalCellIndex(bomb.Cell); ok {
			blocked[index] = true
		}
	}
	if additionalBomb != nil {
		if index, ok := engine.tacticalCellIndex(additionalBomb.Cell); ok {
			blocked[index] = true
		}
	}
	// Placed action fields are contacts, not movement walls. Their eventual
	// effect is deliberately outside this bomb/flame-only projection; treating
	// them as solid would make the reachability fact objectively wrong.
	maximumDeadline := saturatingAdd(engine.elapsedMS, horizons[len(horizons)-1])
	if startAtMS > maximumDeadline || !tacticalIndexSafeDuring(timeline, startIndex, startAtMS, startAtMS) {
		scratch.arrival = arrival
		scratch.blocked = blocked
		scratch.queue = scratch.queue[:0]
		tacticalReachabilityScratchPool.Put(scratch)
		return result
	}
	arrival[startIndex] = startAtMS
	queue := scratch.queue[:0]
	queue = tacticalQueuePush(queue, tacticalQueueEntry{
		cellIndex: startIndex, arrivalMS: startAtMS,
	})
	cardinal := [...]Direction{DirectionUp, DirectionRight, DirectionDown, DirectionLeft}
	var speedByDirection [4]uint32
	var traversesStatic [4]bool
	for index, direction := range cardinal {
		speedByDirection[index] = engine.tacticalMinimumSpeed(actor, direction, maximumDeadline)
		capabilities := engine.actorCapabilities(actor, direction)
		traversesStatic[index] = capabilities.TraverseStaticTerrain &&
			(actor.TransformationExpiresAt == 0 || actor.TransformationExpiresAt >= maximumDeadline)
	}
	for len(queue) != 0 {
		var entry tacticalQueueEntry
		queue, entry = tacticalQueuePop(queue)
		currentIndex, currentAt := entry.cellIndex, entry.arrivalMS
		if arrival[currentIndex] != currentAt {
			continue
		}
		if currentAt > maximumDeadline {
			break
		}
		current := Cell{Row: int16(currentIndex / int(engine.grid.Width)), Col: int16(currentIndex % int(engine.grid.Width))}
		for directionIndex, worldDirection := range cardinal {
			dx, dy, _ := worldDirection.delta()
			next := Cell{Row: current.Row + int16(dy), Col: current.Col + int16(dx)}
			nextIndex, nextInside := engine.tacticalCellIndex(next)
			if !nextInside {
				continue
			}
			tile := engine.grid.Cells[nextIndex]
			if (tile.Kind != CellOpen && !traversesStatic[directionIndex]) ||
				(nextIndex != startIndex && blocked[nextIndex]) {
				continue
			}
			speed := speedByDirection[directionIndex]
			if speed == 0 {
				continue
			}
			travelPixels := uint32(CellSizePixels)
			if currentIndex == startIndex && currentAt == startAtMS {
				travelPixels = tacticalFirstSegmentPixels(actor.Position, next, worldDirection)
				if travelPixels == 0 {
					continue
				}
				// Only the first edge starts from an arbitrary sub-cell position.
				// Later edges connect cell centres: with the native half-size below
				// half a cell, their footprint stays inside the source/destination
				// rows or columns already checked above. Sweeping every later pixel
				// repeats the same tile test and is prohibitively expensive.
				probe := *actor
				if !engine.tacticalStraightSegmentWalkable(&probe, actor.Position, worldDirection, int(travelPixels)) {
					continue
				}
			}
			travelMS := (travelPixels*1_000 + speed - 1) / speed
			if travelMS < engine.rules.TickMS {
				travelMS = engine.rules.TickMS
			}
			nextAt := saturatingAdd(currentAt, travelMS)
			if nextAt > maximumDeadline || !tacticalIndexSafeDuring(timeline, currentIndex, currentAt, nextAt) {
				continue
			}
			guardUntil := saturatingAdd(nextAt, travelMS)
			if guardUntil > maximumDeadline {
				guardUntil = maximumDeadline
			}
			if !tacticalIndexSafeDuring(timeline, nextIndex, nextAt, guardUntil) {
				continue
			}
			if arrival[nextIndex] == NoDangerImpact || nextAt < arrival[nextIndex] {
				arrival[nextIndex] = nextAt
				queue = tacticalQueuePush(queue, tacticalQueueEntry{
					cellIndex: nextIndex, arrivalMS: nextAt,
				})
			}
		}
	}

	cellCount := float32(tacticalReachabilityNormalizationCells(engine.grid, traversesStatic))
	if cellCount == 0 {
		cellCount = 1
	}
	for horizonIndex, horizonMS := range horizons {
		deadline := saturatingAdd(engine.elapsedMS, horizonMS)
		count := 0
		for index, at := range arrival {
			if at == NoDangerImpact || at > deadline {
				continue
			}
			if tacticalIndexSafeDuring(timeline, index, at, deadline) {
				count++
				if horizonIndex == len(horizons)-1 && (!result.refugeFound || at < result.refugeAtMS) {
					result.refugeFound = true
					result.refugeAtMS = at
				}
			}
		}
		result.safeRatios[horizonIndex] = clampUnitFloat32(float32(count) / cellCount)
	}
	scratch.arrival = arrival
	scratch.blocked = blocked
	scratch.queue = queue[:0]
	tacticalReachabilityScratchPool.Put(scratch)
	return result
}

// tacticalStraightSegmentWalkable is equivalent to movementSegmentWalkable
// for one straight segment but samples only points where the native collision
// classification can change: the first displaced pixel, every leading-edge
// cell boundary, and the endpoint. Static collision is constant inside a cell;
// type-1 bomb collision is active only at those entry boundaries.
func (engine *Engine) tacticalStraightSegmentWalkable(actor *Actor, start Position, direction Direction, distance int) bool {
	if actor == nil || distance <= 0 {
		return false
	}
	dx, dy, ok := direction.delta()
	if !ok || direction == DirectionNone {
		return false
	}
	half := int32(engine.rules.ActorHalfSizePixels)
	var leading int32
	positive := direction == DirectionRight || direction == DirectionDown
	switch direction {
	case DirectionRight:
		leading = start.X + half
	case DirectionLeft:
		leading = start.X - half
	case DirectionDown:
		leading = start.Y + half
	case DirectionUp:
		leading = start.Y - half
	}
	check := func(partial int) bool {
		candidate := Position{
			X: start.X + dx*int32(partial),
			Y: start.Y + dy*int32(partial),
		}
		return engine.positionWalkable(actor, candidate, direction)
	}
	lastChecked := 0
	if !check(1) {
		return false
	}
	lastChecked = 1
	remainder := int(positiveRemainder(leading, CellSizePixels))
	boundary := remainder + 1
	if positive {
		boundary = int(CellSizePixels) - remainder
		if boundary == 0 {
			boundary = int(CellSizePixels)
		}
	}
	for partial := boundary; partial <= distance; partial += int(CellSizePixels) {
		if partial == lastChecked {
			continue
		}
		if !check(partial) {
			return false
		}
		lastChecked = partial
	}
	return lastChecked == distance || check(distance)
}

func tacticalReachabilityNormalizationCells(grid Grid, traversesStatic [4]bool) int {
	canTraverseStatic := false
	for _, allowed := range traversesStatic {
		canTraverseStatic = canTraverseStatic || allowed
	}
	if canTraverseStatic {
		return len(grid.Cells)
	}
	count := 0
	for _, tile := range grid.Cells {
		if tile.Kind == CellOpen {
			count++
		}
	}
	return count
}

func (engine *Engine) tacticalMinimumSpeed(actor *Actor, direction Direction, deadlineMS uint32) uint32 {
	minimum := uint32(engine.effectiveSpeedPixelsPerSecond(actor, direction))
	consider := func(candidate Actor) {
		speed := uint32(engine.effectiveSpeedPixelsPerSecond(&candidate, direction))
		if speed < minimum {
			minimum = speed
		}
	}
	statusExpires := actor.MovementStatus != MovementStatusNone &&
		actor.MovementStatusExpiresAt != 0 && actor.MovementStatusExpiresAt < deadlineMS
	transformationExpires := actor.TransformationSceneID != 0 &&
		actor.TransformationExpiresAt != 0 && actor.TransformationExpiresAt < deadlineMS
	if statusExpires {
		candidate := *actor
		candidate.MovementStatus = MovementStatusNone
		candidate.MovementStatusExpiresAt = 0
		consider(candidate)
	}
	if transformationExpires {
		candidate := *actor
		candidate.TransformationSceneID = 0
		candidate.AvatarRoleID = 0
		candidate.TransformationExpiresAt = 0
		consider(candidate)
		if statusExpires {
			candidate.MovementStatus = MovementStatusNone
			candidate.MovementStatusExpiresAt = 0
			consider(candidate)
		}
	}
	return minimum
}

func tacticalFirstSegmentPixels(position Position, target Cell, direction Direction) uint32 {
	center := PositionAtCellCenter(target)
	var distance int32
	switch direction {
	case DirectionUp:
		distance = position.Y - center.Y
	case DirectionRight:
		distance = center.X - position.X
	case DirectionDown:
		distance = center.Y - position.Y
	case DirectionLeft:
		distance = position.X - center.X
	}
	if distance <= 0 {
		return 0
	}
	return uint32(distance)
}

func tacticalIndexSafeDuring(timeline DangerTimeline, index int, arrivalMS, departMS uint32) bool {
	if index < 0 || index >= len(timeline.EarliestImpactMS) {
		return true
	}
	impactAt := timeline.EarliestImpactMS[index]
	if impactAt == NoDangerImpact {
		return true
	}
	if index < len(timeline.SafeAfterMS) {
		clearAt := timeline.SafeAfterMS[index]
		if clearAt != NoDangerImpact && clearAt <= arrivalMS {
			return true
		}
	}
	return impactAt > departMS
}

func tacticalQueuePush(queue []tacticalQueueEntry, entry tacticalQueueEntry) []tacticalQueueEntry {
	queue = append(queue, entry)
	index := len(queue) - 1
	for index > 0 {
		parent := (index - 1) / 2
		if queue[parent].arrivalMS <= entry.arrivalMS {
			break
		}
		queue[index] = queue[parent]
		index = parent
	}
	queue[index] = entry
	return queue
}

func tacticalQueuePop(queue []tacticalQueueEntry) ([]tacticalQueueEntry, tacticalQueueEntry) {
	result := queue[0]
	last := queue[len(queue)-1]
	queue = queue[:len(queue)-1]
	if len(queue) == 0 {
		return queue, result
	}
	index := 0
	for {
		left := index*2 + 1
		if left >= len(queue) {
			break
		}
		child := left
		right := left + 1
		if right < len(queue) && queue[right].arrivalMS < queue[left].arrivalMS {
			child = right
		}
		if queue[child].arrivalMS >= last.arrivalMS {
			break
		}
		queue[index] = queue[child]
		index = child
	}
	queue[index] = last
	return queue, result
}

func (engine *Engine) tacticalCellIndex(cell Cell) (int, bool) {
	if cell.Row < 0 || cell.Col < 0 || int(cell.Row) >= int(engine.grid.Height) || int(cell.Col) >= int(engine.grid.Width) {
		return 0, false
	}
	return int(cell.Row)*int(engine.grid.Width) + int(cell.Col), true
}

func (engine *Engine) aggregateEnemyReachability(
	selfIndex int,
	byActor *[MaxParticipants]tacticalReachability,
) [3]float32 {
	result := [3]float32{}
	count := 0
	selected := [...]int{1, 2, 4}
	self := &engine.actors[selfIndex]
	for index := range engine.actors {
		actor := &engine.actors[index]
		if actor.State != ActorActive || actor.TeamID == self.TeamID || !byActor[index].valid {
			continue
		}
		reachability := byActor[index]
		for outputIndex, sourceIndex := range selected {
			result[outputIndex] += reachability.safeRatios[sourceIndex]
		}
		count++
	}
	if count != 0 {
		for index := range result {
			result[index] /= float32(count)
		}
	}
	return result
}

func (engine *Engine) aggregateAllyReachabilityReduction(
	selfIndex int,
	current *[MaxParticipants]tacticalReachability,
	counterfactual *[MaxParticipants]tacticalReachability,
) float32 {
	total := float32(0)
	count := 0
	self := &engine.actors[selfIndex]
	for index := range engine.actors {
		actor := &engine.actors[index]
		if actor.PlayerID == self.PlayerID || actor.State != ActorActive || actor.TeamID != self.TeamID {
			continue
		}
		before := current[index]
		after := counterfactual[index]
		if !before.valid || !after.valid {
			continue
		}
		total += positiveDifference(before.safeRatios[4], after.safeRatios[4])
		count++
	}
	if count == 0 {
		return 0
	}
	return total / float32(count)
}

func (engine *Engine) actorThreatExpansion(
	self *Actor,
	current DangerTimeline,
	counterfactual DangerTimeline,
) (enemyRatio float32, allyRatio float32) {
	enemies, allies := 0, 0
	newEnemyThreats, newAllyThreats := 0, 0
	for index := range engine.actors {
		actor := &engine.actors[index]
		if actor.PlayerID == self.PlayerID || actor.State != ActorActive {
			continue
		}
		expanded := timelineThreatExpanded(current, counterfactual, actor.Position.Cell())
		if actor.TeamID == self.TeamID {
			allies++
			if expanded {
				newAllyThreats++
			}
		} else {
			enemies++
			if expanded {
				newEnemyThreats++
			}
		}
	}
	if enemies != 0 {
		enemyRatio = float32(newEnemyThreats) / float32(enemies)
	}
	if allies != 0 {
		allyRatio = float32(newAllyThreats) / float32(allies)
	}
	return enemyRatio, allyRatio
}

func (engine *Engine) breakableThreatExpansion(current, counterfactual DangerTimeline) float32 {
	total, expanded := 0, 0
	for index, tile := range engine.grid.Cells {
		if tile.Kind != CellBreakable {
			continue
		}
		total++
		cell := Cell{Row: int16(index / int(engine.grid.Width)), Col: int16(index % int(engine.grid.Width))}
		if timelineThreatExpanded(current, counterfactual, cell) {
			expanded++
		}
	}
	if total == 0 {
		return 0
	}
	return float32(expanded) / float32(total)
}

func (engine *Engine) acceleratedChainRatio(current, counterfactual DangerTimeline) float32 {
	if len(engine.bombs) == 0 {
		return 0
	}
	accelerated := 0
	for _, bomb := range engine.bombs {
		before := bomb.EffectiveExplodeAtMS()
		if impact, ok := current.ImpactAt(bomb.Cell); ok && impact < before {
			before = impact
		}
		after := bomb.EffectiveExplodeAtMS()
		if impact, ok := counterfactual.ImpactAt(bomb.Cell); ok && impact < after {
			after = impact
		}
		if after < before {
			accelerated++
		}
	}
	return float32(accelerated) / float32(len(engine.bombs))
}

func timelineThreatExpanded(current, counterfactual DangerTimeline, cell Cell) bool {
	afterImpact, afterKnown := counterfactual.ImpactAt(cell)
	if !afterKnown {
		return false
	}
	beforeImpact, beforeKnown := current.ImpactAt(cell)
	if !beforeKnown || afterImpact < beforeImpact {
		return true
	}
	afterClear, afterClearKnown := counterfactual.ClearAt(cell)
	beforeClear, beforeClearKnown := current.ClearAt(cell)
	if afterClearKnown && (!beforeClearKnown || afterClear > beforeClear) {
		return true
	}
	return counterfactual.WaveCountAt(cell) > current.WaveCountAt(cell)
}

func positiveDifference(before, after float32) float32 {
	if after >= before {
		return 0
	}
	return before - after
}

func clampUnitFloat32(value float32) float32 {
	if value < 0 {
		return 0
	}
	if value > 1 {
		return 1
	}
	return value
}
