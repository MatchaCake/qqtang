// Package battleenv exposes the production rule-1 battle engine as a stable,
// batched training environment. It owns feature encoding and reward plumbing,
// never a second implementation of combat rules.
package battleenv

import (
	"fmt"
	"math"
	"sync"

	"qqtang/internal/clientdata/sceneelement"
	"qqtang/internal/game/battleengine"
)

const (
	// Schema 15 separates one-shot damage forecasts from visible flame residue.
	// Shapes and physics are unchanged; danger/refuge values are corrected.
	TensorSchemaVersion uint16 = 15
	SpatialChannels            = 91
	ScalarFeatures             = 182
)

const (
	// Existing channels 20..25 preserve the coarse pickup categories learned
	// by older policies. Schema 14 appends public, self-relative facts that the
	// previous representation discarded: the native attribute amount, the
	// amount that can still take effect before this actor's cap, and the exact
	// fixed held-action identity. These are consequences, not item preferences.
	pickupAttributeAmountChannel = 82
	pickupEffectiveGainChannel   = 83
	pickupActionChannelStart     = 84
)

const (
	CurriculumOrdinary uint8 = iota
	CurriculumDuel
	CurriculumBlockade
	CurriculumChainFinisher
	CurriculumBombEscape
	CurriculumDevelopment
	CurriculumLateDuelReplay
	CurriculumNativePass
)

// TensorBatch is a channel-first, fixed-shape actor input. Layout is
// [environment, participant, channel, row, column] for Spatial and
// [environment, participant, feature] for Scalars.
type TensorBatch struct {
	SchemaVersion    uint16
	EnvCount         int
	ParticipantCount int
	Height           int
	Width            int
	// TeamIDs is episode metadata, not an actor input. It lets training group
	// rewards and opponent policies without assuming equal or contiguous teams.
	TeamIDs []uint8
	// TrainingTeamIDs is per-environment curriculum metadata, not an actor
	// input. Zero leaves learner-team selection symmetric; a non-zero value
	// binds the learner to the role that owns an asymmetric capability lesson.
	TrainingTeamIDs []uint8
	// CurriculumIDs identifies the authoritative environment setup used by
	// each episode. It is learner-only task metadata and never enters the actor
	// observation, so separating course gradients cannot constrain exploration.
	CurriculumIDs []uint8
	Spatial       []float32
	Scalars       []float32
	Legal         []uint8
	Active        []uint8
	// DecisionReady is scheduler metadata, separate from life-state Active.
	// A live actor retains recurrent memory while its held input continues.
	DecisionReady []uint8
}

func newTensorBatch(envCount, participants, height, width int) TensorBatch {
	return TensorBatch{
		SchemaVersion: TensorSchemaVersion,
		EnvCount:      envCount, ParticipantCount: participants, Height: height, Width: width,
		TeamIDs:         make([]uint8, envCount*participants),
		TrainingTeamIDs: make([]uint8, envCount),
		CurriculumIDs:   make([]uint8, envCount),
		Spatial:         make([]float32, envCount*participants*SpatialChannels*height*width),
		Scalars:         make([]float32, envCount*participants*ScalarFeatures),
		Legal:           make([]uint8, envCount*participants*int(battleengine.DiscreteActionCount)),
		Active:          make([]uint8, envCount*participants),
		DecisionReady:   make([]uint8, envCount*participants),
	}
}

func clearTensorBatch(tensors *TensorBatch) {
	if tensors == nil {
		return
	}
	clear(tensors.TeamIDs)
	clear(tensors.TrainingTeamIDs)
	clear(tensors.CurriculumIDs)
	clear(tensors.Spatial)
	clear(tensors.Scalars)
	clear(tensors.Legal)
	clear(tensors.Active)
	clear(tensors.DecisionReady)
}

func encodeActor(tensors *TensorBatch, envIndex, actorIndex int, observation battleengine.Observation, danger battleengine.DangerTimeline, consequences battleengine.TacticalConsequences, legal battleengine.ActionMask) error {
	if tensors == nil {
		return fmt.Errorf("tensor batch is nil")
	}
	selfIndex := -1
	for index, actor := range observation.Actors {
		if actor.PlayerID == observation.PlayerID {
			selfIndex = index
			break
		}
	}
	if selfIndex < 0 {
		return fmt.Errorf("observation player %d is missing", observation.PlayerID)
	}
	self := observation.Actors[selfIndex]
	initialTeamSizes := make(map[uint8]int, 8)
	activeTeamSizes := make(map[uint8]int, 8)
	for _, actor := range observation.Actors {
		initialTeamSizes[actor.TeamID]++
		if actor.State != battleengine.ActorEliminated {
			activeTeamSizes[actor.TeamID]++
		}
	}
	enemyActiveTeams := 0
	largestEnemyTeam := 0
	activeTeams := 0
	for teamID, size := range activeTeamSizes {
		if size <= 0 {
			continue
		}
		activeTeams++
		if teamID == self.TeamID {
			continue
		}
		enemyActiveTeams++
		if size > largestEnemyTeam {
			largestEnemyTeam = size
		}
	}
	coalitionContext := [5]float32{
		clamp01(float32(initialTeamSizes[self.TeamID]) / 8),
		clamp01(float32(activeTeamSizes[self.TeamID]) / 8),
		clamp01(float32(enemyActiveTeams) / 8),
		clamp01(float32(largestEnemyTeam) / 8),
		clamp01(float32(activeTeams) / 8),
	}
	teamOrdinal := 0
	for _, actor := range observation.Actors {
		if actor.TeamID == self.TeamID && actor.PlayerID < self.PlayerID {
			teamOrdinal++
		}
	}
	teamRole := teamOrdinal % 2
	clockMS := observation.ClockMS
	if clockMS == 0 {
		// Keep hand-built fixtures and older replay observations readable. Live
		// Schema-6+ observations always carry the absolute native clock.
		clockMS = observation.ElapsedMS
	}
	spatialBase := ((envIndex * tensors.ParticipantCount) + actorIndex) * SpatialChannels * tensors.Height * tensors.Width
	cellOffset := func(channel int, cell battleengine.Cell) (int, bool) {
		if cell.Row < 0 || cell.Col < 0 || int(cell.Row) >= tensors.Height || int(cell.Col) >= tensors.Width {
			return 0, false
		}
		return spatialBase + (channel*tensors.Height+int(cell.Row))*tensors.Width + int(cell.Col), true
	}
	set := func(channel int, cell battleengine.Cell, value float32) {
		if offset, ok := cellOffset(channel, cell); ok && value > tensors.Spatial[offset] {
			tensors.Spatial[offset] = value
		}
	}
	for row := 0; row < int(observation.Grid.Height); row++ {
		for col := 0; col < int(observation.Grid.Width); col++ {
			cell := battleengine.Cell{Row: int16(row), Col: int16(col)}
			tile, _ := observation.Grid.Cell(cell)
			set(0, cell, 1)
			// Channels 75..79 broadcast only public lobby/scoreboard facts:
			// own initial/alive team size, active enemy-team count, largest
			// active enemy coalition, and total active teams. Without these a
			// deployed solo actor sees seven allied enemies and seven unrelated
			// enemies as the same state even though their team colours are public.
			for index, value := range coalitionContext {
				set(75+index, cell, value)
			}
			// Channels 80..81 are a public, actor-local one-hot team role.
			// PlayerID order is stable in both live and training observations,
			// so alternating the within-team ordinal remains exactly replayable
			// when deployment evaluates one decentralized actor at a time.
			set(80+teamRole, cell, 1)
			switch tile.Kind {
			case battleengine.CellOpen:
				// A native map element may occupy a collision-open grid cell.
				// Channel 1 denotes unoccupied floor suitable for placement.
				// Player movement still follows Kind and may cross an occupied
				// CellOpen tile, matching the native GridAttr traversal bit.
				if !tile.MapElementOccupied {
					set(1, cell, 1)
				}
			case battleengine.CellBreakable:
				set(2, cell, float32(tile.Durability)/4)
			case battleengine.CellSolid:
				set(3, cell, 1)
			}
			if tile.FlamePassable {
				set(4, cell, 1)
			}
			if tile.NormalPushable {
				set(5, cell, 1)
			}
			if tile.PandaPushable {
				set(6, cell, 1)
			}
			if tile.PushCounter != 0 {
				set(63, cell, clamp01(float32(tile.PushCounter)/float32(battleengine.NativeMapElementPushThreshold)))
			}
			if impact, ok := danger.ImpactAt(cell); ok {
				remaining := float32(0)
				if impact > clockMS {
					remaining = float32(impact-clockMS) / float32(danger.HorizonMS)
				}
				set(16, cell, clamp01(1-remaining))
			}
			if impact, ok := danger.LastImpactAt(cell); ok {
				remaining := float32(0)
				if impact > clockMS {
					remaining = float32(impact-clockMS) / float32(danger.HorizonMS)
				}
				set(35, cell, clamp01(1-remaining))
			}
			if clearAt, ok := danger.ClearAt(cell); ok && clearAt > clockMS {
				clearHorizon := float32(danger.HorizonMS + battleengine.NativeFlameDurationMS)
				set(36, cell, clamp01(float32(clearAt-clockMS)/clearHorizon))
			}
			set(37, cell, clamp01(float32(danger.WaveCountAt(cell))/4))
		}
	}
	for _, actor := range observation.Actors {
		channel := 11
		switch {
		case actor.PlayerID == observation.PlayerID && actor.State == battleengine.ActorTrapped:
			channel = 8
		case actor.PlayerID == observation.PlayerID:
			channel = 7
		case actor.TeamID == self.TeamID && actor.State == battleengine.ActorTrapped:
			channel = 10
		case actor.TeamID == self.TeamID:
			channel = 9
		case actor.State == battleengine.ActorTrapped:
			channel = 12
		}
		if actor.State != battleengine.ActorEliminated {
			set(channel, actor.Cell, 1)
			// Channels 72..74 retain the public team colour and its initial and
			// currently active size at every live actor cell. Team size gives a
			// permutation-robust coalition signal while TeamID preserves exact
			// same-team relationships between equally sized enemy coalitions.
			set(72, actor.Cell, clamp01(float32(actor.TeamID)/8))
			set(73, actor.Cell, clamp01(float32(initialTeamSizes[actor.TeamID])/8))
			set(74, actor.Cell, clamp01(float32(activeTeamSizes[actor.TeamID])/8))
			if transformChannel, ok := transformationChannel(actor.TransformationSceneID); ok {
				set(transformChannel, actor.Cell, activeTransformationValue(actor.TransformationExpiresAt, clockMS))
			}
			set(34, actor.Cell, remainingRatio(actor.HarmProtectionExpiresAt, clockMS, battleengine.NativePostTransformationProtectionMS))
			set(41, actor.Cell, actorCellOffset(actor.Position.X, actor.Cell.Col))
			set(42, actor.Cell, actorCellOffset(actor.Position.Y, actor.Cell.Row))
			if actor.Facing >= battleengine.DirectionUp && actor.Facing <= battleengine.DirectionLeft {
				set(42+int(actor.Facing), actor.Cell, 1)
			}
			if actor.MovementStatus >= battleengine.MovementStatusSlow && actor.MovementStatus <= battleengine.MovementStatusFast {
				set(46+int(actor.MovementStatus), actor.Cell, 1)
			}
			set(50, actor.Cell, float32(actor.BombCapacity)/8)
			set(51, actor.Cell, float32(actor.MaxBombCapacity)/8)
			set(52, actor.Cell, float32(actor.BombPower)/10)
			set(53, actor.Cell, float32(actor.MaxBombPower)/10)
			set(54, actor.Cell, float32(actor.SpeedRate)/10)
			set(55, actor.Cell, float32(actor.MaxSpeedRate)/10)
			// Publicly inferred shortcut-bar counts, in stable native ActionID
			// order. No hidden wall allocation is exposed by these channels.
			for index := 0; index < battleengine.NativeBattleActionSlots; index++ {
				actionID, ok := battleengine.NativeUseActionIDAt(index)
				if !ok {
					continue
				}
				for _, slot := range actor.HeldActions {
					if slot.ActionID == actionID {
						set(56+index, actor.Cell, float32(slot.Count)/9)
						break
					}
				}
			}
			// Channels 64..71 are public round-local behavior memory at the
			// observed actor's current cell: recent move/wait/bomb/item rhythm,
			// followed by the corresponding whole-match rates. Bomb and item
			// frequencies are converted from per-20 ms tick fractions to bounded
			// events/second so rare tactical actions retain useful scale.
			for kind := battleengine.PublicBehaviorKind(0); kind < battleengine.PublicBehaviorKindCount; kind++ {
				recent := actor.PublicBehavior.RecentRate(kind)
				match := actor.PublicBehavior.MatchRate(kind)
				if kind == battleengine.PublicBehaviorBomb || kind == battleengine.PublicBehaviorItem {
					recent = clamp01(recent * 50)
					match = clamp01(match * 50)
				}
				set(64+int(kind), actor.Cell, recent)
				set(68+int(kind), actor.Cell, match)
			}
		}
	}
	for _, bomb := range observation.Bombs {
		set(13, bomb.Cell, 1)
		remaining := float32(0)
		if due := bomb.EffectiveExplodeAtMS(); due > clockMS {
			remaining = float32(due-clockMS) / float32(battleengine.NativeBombFuseMS)
		}
		set(14, bomb.Cell, clamp01(remaining))
		relationChannel := 0
		if bomb.OwnerID == observation.PlayerID {
			relationChannel = 38
		} else {
			for _, actor := range observation.Actors {
				if actor.PlayerID != bomb.OwnerID {
					continue
				}
				if actor.TeamID == self.TeamID {
					relationChannel = 39
				} else {
					relationChannel = 40
				}
				break
			}
		}
		if relationChannel != 0 {
			set(relationChannel, bomb.Cell, 1)
		}
	}
	for _, flame := range observation.Flames {
		set(15, flame.Cell, 1)
	}
	for _, object := range observation.FieldObjects {
		switch object.ActionID {
		case 41:
			set(17, object.Cell, 1)
		case 42:
			set(18, object.Cell, 1)
		case 43:
			set(19, object.Cell, 1)
		}
	}
	for _, pickup := range observation.Pickups {
		set(pickupChannel(pickup.SceneID), pickup.Cell, 1)
		setVisiblePickupFacts(set, pickup, self)
	}

	scalarBase := ((envIndex * tensors.ParticipantCount) + actorIndex) * ScalarFeatures
	scalars := tensors.Scalars[scalarBase : scalarBase+ScalarFeatures]
	setScalarFeatures(scalars, observation, self)
	setVisibleTacticalRouteFeatures(scalars, observation, self)
	setTacticalConsequenceFeatures(scalars, consequences)
	legalBase := ((envIndex * tensors.ParticipantCount) + actorIndex) * int(battleengine.DiscreteActionCount)
	for id, allowed := range legal {
		if allowed {
			tensors.Legal[legalBase+id] = 1
		}
	}
	if self.State != battleengine.ActorEliminated && !observation.Outcome.Ended {
		tensors.Active[envIndex*tensors.ParticipantCount+actorIndex] = 1
	}
	return nil
}

func setVisiblePickupFacts(
	set func(channel int, cell battleengine.Cell, value float32),
	pickup battleengine.Pickup,
	self battleengine.ActorObservation,
) {
	var current, maximum, amount byte
	switch pickup.SceneID {
	case battleengine.SceneBombCapacitySmall:
		current, maximum, amount = self.BombCapacity, self.MaxBombCapacity, 1
	case battleengine.SceneBombCapacityLarge:
		current, maximum, amount = self.BombCapacity, self.MaxBombCapacity, 8
	case battleengine.SceneBombPowerSmall:
		current, maximum, amount = self.BombPower, self.MaxBombPower, 1
	case battleengine.SceneBombPowerLarge:
		current, maximum, amount = self.BombPower, self.MaxBombPower, 8
	case battleengine.SceneSpeedSmall:
		current, maximum, amount = self.SpeedRate, self.MaxSpeedRate, 1
	case battleengine.SceneSpeedLarge:
		current, maximum, amount = self.SpeedRate, self.MaxSpeedRate, 8
	}
	if amount != 0 {
		set(pickupAttributeAmountChannel, pickup.Cell, float32(amount)/8)
		gain := byte(0)
		if maximum > current {
			gain = maximum - current
			if gain > amount {
				gain = amount
			}
		}
		set(pickupEffectiveGainChannel, pickup.Cell, float32(gain)/8)
	}

	action, ok := sceneelement.NativeBattleActionPickup(sceneelement.ID(pickup.SceneID))
	if !ok {
		return
	}
	for index := 0; index < battleengine.NativeBattleActionSlots; index++ {
		actionID, known := battleengine.NativeUseActionIDAt(index)
		if known && action.ActionID == actionID {
			set(pickupActionChannelStart+index, pickup.Cell, 1)
			return
		}
	}
}

// setTacticalConsequenceFeatures appends conservative known-danger
// reachability and place-bomb counterfactuals. They describe evidence, not
// preferences or guaranteed future safety: the learned policy remains
// responsible for exploration and for trading self risk, pressure, wall
// value, chain value and teammate risk.
func setTacticalConsequenceFeatures(values []float32, facts battleengine.TacticalConsequences) {
	if len(values) != ScalarFeatures {
		panic("invalid scalar feature storage")
	}
	copy(values[84:89], facts.CurrentKnownDangerReachable[:])
	copy(values[89:94], facts.BombKnownDangerReachable[:])
	if facts.BombLegal {
		values[94] = 1
	}
	if facts.BombKnownDangerProjectionValid {
		values[95] = 1
	}
	values[96] = clamp01(facts.BombEarliestWholeCellRefugeEstimate)
	copy(values[97:100], facts.EnemyCurrentKnownReachable[:])
	copy(values[100:103], facts.EnemyBombKnownReachable[:])
	copy(values[103:106], facts.EnemyKnownReachReduction[:])
	values[106] = clamp01(facts.AllyKnownReachReduction)
	values[107] = clamp01(facts.NewEnemyThreatRatio)
	values[108] = clamp01(facts.NewAllyThreatRatio)
	values[109] = clamp01(facts.NewBreakableThreatRatio)
	values[110] = clamp01(facts.AcceleratedChainRatio)
	if facts.KnownDangerProjectionValid {
		values[111] = 1
	}
	currentDirectionBase := 112
	bombDirectionBase := currentDirectionBase +
		battleengine.TacticalMovementCandidateCount*battleengine.TacticalReachabilityHorizonCount
	for direction := 0; direction < battleengine.TacticalMovementCandidateCount; direction++ {
		currentStart := currentDirectionBase + direction*battleengine.TacticalReachabilityHorizonCount
		bombStart := bombDirectionBase + direction*battleengine.TacticalReachabilityHorizonCount
		copy(
			values[currentStart:currentStart+battleengine.TacticalReachabilityHorizonCount],
			facts.CurrentDirectionKnownDangerReachable[direction][:],
		)
		copy(
			values[bombStart:bombStart+battleengine.TacticalReachabilityHorizonCount],
			facts.BombDirectionKnownDangerReachable[direction][:],
		)
	}
	currentRefugeFoundBase := bombDirectionBase +
		battleengine.TacticalMovementCandidateCount*battleengine.TacticalReachabilityHorizonCount
	currentRefugeTimeBase := currentRefugeFoundBase + battleengine.TacticalMovementCandidateCount
	bombRefugeFoundBase := currentRefugeTimeBase + battleengine.TacticalMovementCandidateCount
	bombRefugeTimeBase := bombRefugeFoundBase + battleengine.TacticalMovementCandidateCount
	for direction := 0; direction < battleengine.TacticalMovementCandidateCount; direction++ {
		if facts.CurrentDirectionWholeCellRefugeFound[direction] {
			values[currentRefugeFoundBase+direction] = 1
		}
		values[currentRefugeTimeBase+direction] = clamp01(
			facts.CurrentDirectionEarliestRefuge[direction],
		)
		if facts.BombDirectionWholeCellRefugeFound[direction] {
			values[bombRefugeFoundBase+direction] = 1
		}
		values[bombRefugeTimeBase+direction] = clamp01(
			facts.BombDirectionEarliestRefuge[direction],
		)
	}
}

func transformationChannel(sceneID uint32) (int, bool) {
	switch sceneID {
	case 101:
		return 26, true
	case 104:
		return 27, true
	case 107:
		return 28, true
	case 108:
		return 29, true
	case 109:
		return 30, true
	case 110:
		return 31, true
	case 114:
		return 32, true
	case 115:
		return 33, true
	default:
		return 0, false
	}
}

func pickupChannel(sceneID uint32) int {
	switch sceneID {
	case 1, 6:
		return 20
	case 2, 7:
		return 21
	case 3, 8, 47:
		return 22
	case 21, 23, 24, 25, 27, 28:
		return 24
	case 101, 104, 107, 108, 109, 110, 114, 115:
		return 25
	default:
		return 23
	}
}

func setScalarFeatures(values []float32, observation battleengine.Observation, self battleengine.ActorObservation) {
	if len(values) != ScalarFeatures {
		panic("invalid scalar feature storage")
	}
	clockMS := observation.ClockMS
	if clockMS == 0 {
		clockMS = observation.ElapsedMS
	}
	round := float32(observation.ElapsedMS + observation.RemainingMS)
	if round <= 0 {
		round = 1
	}
	values[0] = float32(observation.ElapsedMS) / round
	values[1] = float32(observation.RemainingMS) / round
	values[2] = safeRatio(float32(self.Position.X), float32(observation.Grid.Width*battleengine.CellSizePixels))
	values[3] = safeRatio(float32(self.Position.Y), float32(observation.Grid.Height*battleengine.CellSizePixels))
	values[4] = float32(self.TeamID) / 8
	values[5] = float32(self.RoleID) / 32
	if int(self.State) < 3 {
		values[6+int(self.State)] = 1
	}
	if int(self.Facing) < 5 {
		values[9+int(self.Facing)] = 1
	}
	values[14] = float32(self.BombCapacity) / 8
	values[15] = float32(self.MaxBombCapacity) / 8
	values[16] = float32(self.BombPower) / 10
	values[17] = float32(self.MaxBombPower) / 10
	values[18] = float32(self.SpeedRate) / 10
	values[19] = float32(self.MaxSpeedRate) / 10
	values[20] = float32(self.SpeedPixelsPerSecond) / 520
	values[21] = float32(self.ActiveBombs) / 8
	values[22] = remainingRatio(self.TrapExpiresAt, clockMS, 6_000)
	values[23] = remainingRatio(self.HiddenPickupReachExpiresAt, clockMS, battleengine.NativeHiddenPickupReachMS)
	values[24] = remainingRatio(self.SceneFourEffectExpiresAt, clockMS, battleengine.NativeSceneFourEffectMS)
	values[25] = remainingRatio(self.TransformationExpiresAt, clockMS, 30_000)
	values[26] = remainingRatio(self.HarmProtectionExpiresAt, clockMS, battleengine.NativePostTransformationProtectionMS)
	values[27] = float32(self.OxygenValue) / 10_000
	values[28] = float32(self.MatchSugar) / 10_000
	if int(self.MovementStatus) < 4 {
		values[29+int(self.MovementStatus)] = 1
	}
	values[33] = remainingRatio(self.MovementStatusExpiresAt, clockMS, battleengine.NativeMovementStatusMS)
	for index := 0; index < battleengine.NativeBattleActionSlots; index++ {
		actionID, ok := battleengine.NativeUseActionIDAt(index)
		if !ok {
			continue
		}
		for _, slot := range self.HeldActions {
			if slot.ActionID == actionID {
				values[34+index] = float32(slot.Count) / 9
				break
			}
		}
	}
	if self.NativePassCollisionValid {
		values[41] = 1
	}
	if self.NativePassActive {
		values[42] = 1
	}
	values[43] = nativePassPhaseProgress(self, clockMS)
	for _, actor := range observation.Actors {
		if actor.PlayerID == self.PlayerID || actor.State == battleengine.ActorEliminated {
			continue
		}
		ally := actor.TeamID == self.TeamID
		if ally && actor.State == battleengine.ActorTrapped {
			values[46] += 1.0 / 7
		} else if ally {
			values[44] += 1.0 / 7
		} else if actor.State == battleengine.ActorTrapped {
			values[47] += 1.0 / 7
		} else {
			values[45] += 1.0 / 7
		}
	}
	for category, profile := range observation.PublicWallItems.Categories {
		base := 48 + category*2
		values[base] = clamp01(profile.PresenceProbability)
		values[base+1] = clamp01(profile.ExpectedDensity)
	}
}

// setVisibleTacticalRouteFeatures appends four deterministic route candidates
// derived exclusively from the actor's Observation. They are hints rather
// than commands: legality, short-horizon safety and the learned policy still
// decide whether following a candidate is useful. Each group stores a
// U/R/D/L first-step one-hot followed by a normalized path distance. A
// positive distance therefore also acts as target-presence even when the
// actor already occupies the target cell.
func setVisibleTacticalRouteFeatures(values []float32, observation battleengine.Observation, self battleengine.ActorObservation) {
	if len(values) != ScalarFeatures {
		panic("invalid scalar feature storage")
	}
	routes := visibleTacticalRoutes(observation, self)
	for index, route := range routes {
		base := 64 + index*5
		if !route.found {
			continue
		}
		if route.first >= battleengine.DirectionUp && route.first <= battleengine.DirectionLeft {
			values[base+int(route.first-battleengine.DirectionUp)] = 1
		}
		cellCount := int(observation.Grid.Width) * int(observation.Grid.Height)
		values[base+4] = safeRatio(float32(route.distance+1), float32(cellCount+1))
	}
}

type tacticalRoute struct {
	found    bool
	first    battleengine.Direction
	distance int
}

const (
	tacticalRouteEnemy = iota
	tacticalRouteTrappedAlly
	tacticalRoutePickup
	tacticalRouteDemolition
	tacticalRouteCount
)

// visibleTacticalRoutes performs one stable BFS for all global candidates.
// It never inspects the engine's hidden pickup allocation. Bombs and placed
// field objects are treated as current dynamic blockers, while native
// pushable terrain remains a reachable (possibly slower) route. The duck's
// public traversal capability permits static terrain but still excludes
// dynamic objects and suppresses impossible pickup collection targets.
func visibleTacticalRoutes(observation battleengine.Observation, self battleengine.ActorObservation) [tacticalRouteCount]tacticalRoute {
	var result [tacticalRouteCount]tacticalRoute
	width, height := int(observation.Grid.Width), int(observation.Grid.Height)
	cellCount := width * height
	if width == 0 || height == 0 || len(observation.Grid.Cells) != cellCount {
		return result
	}
	indexOf := func(cell battleengine.Cell) (int, bool) {
		if cell.Row < 0 || cell.Col < 0 || int(cell.Row) >= height || int(cell.Col) >= width {
			return 0, false
		}
		return int(cell.Row)*width + int(cell.Col), true
	}
	startIndex, ok := indexOf(self.Cell)
	if !ok {
		return result
	}

	scratch := acquireTacticalRouteScratch(cellCount)
	defer releaseTacticalRouteScratch(scratch)
	passable := scratch.passable
	for index, tile := range observation.Grid.Cells {
		passable[index] = self.Capabilities.TraverseStaticTerrain ||
			tile.Kind == battleengine.CellOpen ||
			tile.NormalPushable || (self.Capabilities.CanPushBreakable && tile.PandaPushable)
	}
	for _, bomb := range observation.Bombs {
		if index, inside := indexOf(bomb.Cell); inside {
			passable[index] = false
		}
	}
	for _, object := range observation.FieldObjects {
		if index, inside := indexOf(object.Cell); inside {
			passable[index] = false
		}
	}
	// An actor may legally leave the cell of a newly placed bomb or field
	// object. Keep that native pass-through origin in the BFS.
	passable[startIndex] = true

	targets := &scratch.targets
	for _, actor := range observation.Actors {
		if actor.PlayerID == self.PlayerID || actor.State == battleengine.ActorEliminated {
			continue
		}
		index, inside := indexOf(actor.Cell)
		if !inside {
			continue
		}
		if actor.TeamID == self.TeamID {
			if actor.State == battleengine.ActorTrapped {
				(*targets)[tacticalRouteTrappedAlly][index] = true
			}
		} else {
			(*targets)[tacticalRouteEnemy][index] = true
		}
	}
	if self.Capabilities.CanCollectItems {
		for _, pickup := range observation.Pickups {
			if index, inside := indexOf(pickup.Cell); inside {
				(*targets)[tacticalRoutePickup][index] = true
			}
		}
	}
	if !self.Capabilities.TraverseStaticTerrain {
		for index, tile := range observation.Grid.Cells {
			if tile.Kind != battleengine.CellBreakable {
				continue
			}
			row, col := index/width, index%width
			for _, delta := range tacticalDirectionDeltas {
				neighbor := battleengine.Cell{Row: int16(row + delta.row), Col: int16(col + delta.col)}
				if neighborIndex, inside := indexOf(neighbor); inside && passable[neighborIndex] {
					(*targets)[tacticalRouteDemolition][neighborIndex] = true
				}
			}
		}
	}

	first := scratch.first
	distance := scratch.distance
	visited := scratch.visited
	queue := append(scratch.queue, startIndex)
	visited[startIndex] = true
	remaining := tacticalRouteCount
	for head := 0; head < len(queue) && remaining > 0; head++ {
		index := queue[head]
		for kind := 0; kind < tacticalRouteCount; kind++ {
			if !result[kind].found && (*targets)[kind][index] {
				result[kind] = tacticalRoute{found: true, first: first[index], distance: distance[index]}
				remaining--
			}
		}
		row, col := index/width, index%width
		for directionIndex, delta := range tacticalDirectionDeltas {
			nextRow, nextCol := row+delta.row, col+delta.col
			if nextRow < 0 || nextCol < 0 || nextRow >= height || nextCol >= width {
				continue
			}
			next := nextRow*width + nextCol
			if visited[next] || !passable[next] {
				continue
			}
			visited[next] = true
			distance[next] = distance[index] + 1
			if index == startIndex {
				first[next] = battleengine.Direction(directionIndex) + battleengine.DirectionUp
			} else {
				first[next] = first[index]
			}
			queue = append(queue, next)
		}
	}
	return result
}

type tacticalRouteScratch struct {
	passable []bool
	targets  [tacticalRouteCount][]bool
	first    []battleengine.Direction
	distance []int
	visited  []bool
	queue    []int
}

var tacticalRouteScratchPool sync.Pool

func acquireTacticalRouteScratch(cellCount int) *tacticalRouteScratch {
	scratch, _ := tacticalRouteScratchPool.Get().(*tacticalRouteScratch)
	if scratch == nil {
		scratch = &tacticalRouteScratch{}
	}
	scratch.passable = resizeAndClear(scratch.passable, cellCount)
	for index := range scratch.targets {
		scratch.targets[index] = resizeAndClear(scratch.targets[index], cellCount)
	}
	scratch.first = resizeAndClear(scratch.first, cellCount)
	scratch.distance = resizeAndClear(scratch.distance, cellCount)
	scratch.visited = resizeAndClear(scratch.visited, cellCount)
	scratch.queue = scratch.queue[:0]
	if cap(scratch.queue) < cellCount {
		scratch.queue = make([]int, 0, cellCount)
	}
	return scratch
}

func releaseTacticalRouteScratch(scratch *tacticalRouteScratch) {
	if scratch != nil {
		tacticalRouteScratchPool.Put(scratch)
	}
}

func resizeAndClear[T any](values []T, size int) []T {
	if cap(values) < size {
		return make([]T, size)
	}
	values = values[:size]
	clear(values)
	return values
}

var tacticalDirectionDeltas = [...]struct{ row, col int }{
	{row: -1}, {col: 1}, {row: 1}, {col: -1},
}

func remainingRatio(expiresAt, now, duration uint32) float32 {
	if expiresAt <= now || duration == 0 {
		return 0
	}
	return clamp01(float32(expiresAt-now) / float32(duration))
}

// Transformation identity remains observable after the nominal timer while a
// duck is standing over static terrain and native recovery is deferred. The
// 0.25 floor distinguishes an active form from no form; the remaining 0.75
// carries its visible countdown.
func activeTransformationValue(expiresAt, now uint32) float32 {
	return 0.25 + 0.75*remainingRatio(expiresAt, now, 30_000)
}

// nativePassPhaseProgress makes the local type-1 collision state Markov for
// the feed-forward actor. Before activation it is the 0..600 ms aligned
// contact charge; after activation it is progress through the one-cell pass.
// Scalar 42 distinguishes the two phases.
func nativePassPhaseProgress(actor battleengine.ActorObservation, now uint32) float32 {
	if actor.NativePassActive {
		if actor.NativePassDurationMS == 0 || now <= actor.NativePassStartedAt {
			return 0
		}
		return clamp01(float32(now-actor.NativePassStartedAt) / float32(actor.NativePassDurationMS))
	}
	if !actor.NativePassCollisionValid || now <= actor.NativePassCollisionStartedAt {
		return 0
	}
	return clamp01(float32(now-actor.NativePassCollisionStartedAt) / float32(battleengine.NativePassChargeMaxMS))
}

func actorCellOffset(value int32, cell int16) float32 {
	start := int32(cell) * battleengine.CellSizePixels
	return clamp01(float32(value-start) / battleengine.CellSizePixels)
}

func safeRatio(value, denominator float32) float32 {
	if denominator <= 0 {
		return 0
	}
	return clamp01(value / denominator)
}

func clamp01(value float32) float32 {
	return float32(math.Max(0, math.Min(1, float64(value))))
}
