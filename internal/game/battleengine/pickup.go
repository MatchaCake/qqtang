package battleengine

import (
	"fmt"

	"qqtang/internal/clientdata/sceneelement"
)

const (
	SceneBombCapacitySmall uint32 = 1
	SceneBombPowerSmall    uint32 = 2
	SceneSpeedSmall        uint32 = 3
	SceneFourBubbleEffect  uint32 = 4
	SceneRandomAttribute   uint32 = 5
	SceneBombCapacityLarge uint32 = 6
	SceneBombPowerLarge    uint32 = 7
	SceneSpeedLarge        uint32 = 8
	SceneHiddenPickupReach uint32 = 61
	SceneOxygenValueAdd    uint32 = 62
	SceneFastMovement      uint32 = 47
	SceneOxygenBottle      uint32 = 26
)

func supportedPickupEffect(sceneID uint32) (AttributeKind, byte, bool) {
	switch sceneID {
	case SceneBombCapacitySmall:
		return AttributeBombCapacity, 1, true
	case SceneBombPowerSmall:
		return AttributeBombPower, 1, true
	case SceneSpeedSmall:
		return AttributeSpeedRate, 1, true
	case SceneFourBubbleEffect, SceneRandomAttribute, SceneHiddenPickupReach, SceneOxygenValueAdd, SceneFastMovement, SceneOxygenBottle:
		return AttributeNone, 0, true
	case SceneBombCapacityLarge:
		return AttributeBombCapacity, 8, true
	case SceneBombPowerLarge:
		return AttributeBombPower, 8, true
	case SceneSpeedLarge:
		return AttributeSpeedRate, 8, true
	default:
		if _, ok := sceneelement.NativeBattleActionPickup(sceneelement.ID(sceneID)); ok {
			return AttributeNone, 0, true
		}
		if _, ok := sceneelement.NativeTransformation(sceneelement.ID(sceneID)); ok {
			return AttributeNone, 0, true
		}
		if reward, ok := sceneelement.NativeReward(sceneelement.ID(sceneID)); ok &&
			reward.Kind == sceneelement.RewardMatchSugar && reward.DirectMatchAccumulator {
			return AttributeNone, 0, true
		}
		return AttributeNone, 0, false
	}
}

func validateInitialPickups(grid Grid, rules Rules, participants []Participant, pickups []Pickup) error {
	seen := make(map[Cell]struct{}, len(pickups))
	hasSpeedPickup := false
	requiredSpeedRates := [MaxNativeSpeedRate + 1]bool{}
	for index, pickup := range pickups {
		attribute, _, supported := supportedPickupEffect(pickup.SceneID)
		if !supported {
			return fmt.Errorf("battle pickup %d uses unsupported scene ID %d", index, pickup.SceneID)
		}
		if attribute == AttributeSpeedRate || pickup.SceneID == SceneRandomAttribute {
			hasSpeedPickup = true
		}
		switch pickup.SceneID {
		case 23:
			requiredSpeedRates[10] = true
		case 25:
			requiredSpeedRates[2] = true
		case SceneFastMovement:
			requiredSpeedRates[8] = true
		}
		if transformation, ok := sceneelement.NativeTransformation(sceneelement.ID(pickup.SceneID)); ok {
			for _, rate := range [...]uint8{transformation.SpeedRate, transformation.HorizontalSpeedRate, transformation.VerticalSpeedRate} {
				if rate != 0 {
					requiredSpeedRates[rate] = true
				}
			}
		}
		if pickup.State != PickupHidden && pickup.State != PickupAvailable {
			return fmt.Errorf("battle pickup %d has invalid initial state %d", index, pickup.State)
		}
		tile, ok := grid.Cell(pickup.Cell)
		if !ok {
			return fmt.Errorf("battle pickup %d cell %d,%d is outside the grid", index, pickup.Cell.Row, pickup.Cell.Col)
		}
		if pickup.State == PickupHidden && tile.Kind != CellBreakable {
			return fmt.Errorf("hidden battle pickup %d cell %d,%d is not breakable", index, pickup.Cell.Row, pickup.Cell.Col)
		}
		if pickup.State == PickupAvailable && tile.Kind != CellOpen {
			return fmt.Errorf("available battle pickup %d cell %d,%d is not open", index, pickup.Cell.Row, pickup.Cell.Col)
		}
		if _, duplicate := seen[pickup.Cell]; duplicate {
			return fmt.Errorf("battle pickups repeat cell %d,%d", pickup.Cell.Row, pickup.Cell.Col)
		}
		seen[pickup.Cell] = struct{}{}
	}
	if hasSpeedPickup {
		for _, participant := range participants {
			if participant.SpeedRate == 0 {
				return fmt.Errorf("battle participant %d needs a native speed rate for SceneID 3/8 pickups", participant.PlayerID)
			}
		}
	}
	for rate, required := range requiredSpeedRates {
		if required && rules.SpeedPixelsPerSecondByRate[rate] == 0 {
			return fmt.Errorf("battle pickups require a pixels/second projection for native speed rate %d", rate)
		}
	}
	return nil
}

func (engine *Engine) revealPickupAt(cell Cell, ownerID uint16, bombID uint32) (Event, bool) {
	for index := range engine.pickups {
		pickup := &engine.pickups[index]
		if pickup.Cell != cell || pickup.State != PickupHidden {
			continue
		}
		pickup.State = PickupAvailable
		return Event{
			Kind: EventPickupRevealed, TimeMS: engine.elapsedMS, PlayerID: ownerID,
			BombID: bombID, Cell: cell, SceneID: pickup.SceneID,
		}, true
	}
	return Event{}, false
}

func (engine *Engine) resolvePickupContacts() []Event {
	events := make([]Event, 0)
	for actorIndex := range engine.actors {
		actor := &engine.actors[actorIndex]
		if actor.State != ActorActive || actor.nativePreviousPosition.Cell() == actor.Position.Cell() || !engine.actorCapabilities(actor, actor.Facing).CanCollectItems {
			continue
		}
		// In a live mixed match, overlap is only an observation. The original
		// arbitrator owns the scene object and 0x0FAD is the sole proof that either
		// a human or a virtual participant collected it. A virtual actor therefore
		// emits an intent while it occupies the item's cell, but neither removes
		// the object nor applies its attribute here. Offline/RL engines keep the
		// deterministic immediate-collection path below.
		if engine.rules.NativeOutcomeAuthority {
			if actor.Source == ParticipantHuman {
				continue
			}
			actorCell := actor.Position.Cell()
			for pickupIndex := range engine.pickups {
				pickup := &engine.pickups[pickupIndex]
				// SceneID 61 is a detector: it exposes nearby hidden contents to
				// Observation but does not collect through the containing wall.
				matches := pickup.State == PickupAvailable && pickup.Cell == actorCell
				if !matches {
					continue
				}
				events = append(events, Event{
					Kind: EventPickupCollectRequested, TimeMS: engine.elapsedMS,
					PlayerID: actor.PlayerID, Cell: pickup.Cell,
					Position: actor.Position, SceneID: pickup.SceneID,
				})
			}
			continue
		}
		// FUN_0060c518 divides the actor's current world coordinate by 40 and
		// resolves the scene object at that exact Row/Col. The +/-19 movement
		// footprint is not pickup reach; using overlap here made training collect
		// an adjacent item before a real client could author 0x0FAC.
		actorCell := actor.Position.Cell()
		for pickupIndex := range engine.pickups {
			pickup := &engine.pickups[pickupIndex]
			if pickup.State != PickupAvailable || pickup.Cell != actorCell {
				continue
			}
			events = append(events, engine.collectPickup(actor, pickup))
		}
	}
	return events
}

func (engine *Engine) collectPickup(actor *Actor, pickup *Pickup) Event {
	event := Event{
		Kind: EventPickupCollected, TimeMS: engine.elapsedMS, PlayerID: actor.PlayerID,
		Cell: pickup.Cell, Position: actor.Position, SceneID: pickup.SceneID,
	}
	transformation, isTransformation := sceneelement.NativeTransformation(sceneelement.ID(pickup.SceneID))
	battleAction, isBattleAction := sceneelement.NativeBattleActionPickup(sceneelement.ID(pickup.SceneID))
	reward, isReward := sceneelement.NativeReward(sceneelement.ID(pickup.SceneID))
	switch {
	case isTransformation:
		engine.installTransformation(actor, transformation)
		event.Effect = PickupEffectTransformation
		event.AvatarRoleID = transformation.AvatarRoleID
		event.EffectExpiresAt = actor.TransformationExpiresAt
	case isBattleAction:
		grantHeldAction(actor, battleAction.ActionID, battleAction.Count)
		event.Effect = PickupEffectBattleAction
		event.ActionID = battleAction.ActionID
		event.ActionCount = battleAction.Count
	case isReward && reward.Kind == sceneelement.RewardMatchSugar && reward.DirectMatchAccumulator:
		event.Effect = PickupEffectMatchSugar
		event.RewardBefore = actor.MatchSugar
		actor.MatchSugar = saturatingAdd(actor.MatchSugar, reward.Value)
		event.RewardAfter = actor.MatchSugar
	case pickup.SceneID == SceneFourBubbleEffect:
		expiresAt := saturatingAdd(engine.elapsedMS, NativeSceneFourEffectMS)
		if actor.SceneFourEffectExpiresAt < expiresAt {
			actor.SceneFourEffectExpiresAt = expiresAt
		}
		event.Effect = PickupEffectSceneFour
		event.EffectExpiresAt = actor.SceneFourEffectExpiresAt
	case pickup.SceneID == SceneHiddenPickupReach:
		expiresAt := saturatingAdd(engine.elapsedMS, NativeHiddenPickupReachMS)
		if actor.HiddenPickupReachExpiresAt < expiresAt {
			actor.HiddenPickupReachExpiresAt = expiresAt
		}
		event.Effect = PickupEffectHiddenReach
		event.EffectExpiresAt = actor.HiddenPickupReachExpiresAt
	case pickup.SceneID == SceneOxygenValueAdd:
		event.Effect = PickupEffectOxygen
		event.RewardBefore = actor.OxygenValue
		actor.OxygenValue = saturatingAdd(actor.OxygenValue, NativeOxygenValueIncrement)
		event.RewardAfter = actor.OxygenValue
	case pickup.SceneID == SceneOxygenBottle:
		event.Effect = PickupEffectOxygen
		event.RewardBefore = actor.OxygenValue
		actor.OxygenValue = saturatingAdd(actor.OxygenValue, NativeOxygenValueIncrement)
		event.RewardAfter = actor.OxygenValue
	case pickup.SceneID == SceneFastMovement:
		engine.installMovementStatus(actor, MovementStatusFast)
		event.Effect = PickupEffectMovementStatus
		event.MovementStatus = actor.MovementStatus
		event.EffectExpiresAt = actor.MovementStatusExpiresAt
	case pickup.SceneID == SceneRandomAttribute:
		attribute, delta := nativeRandomAttributeEffect(pickup.Cell)
		event.Attribute = attribute
		recordNativeBasicPickupDelta(actor, attribute, delta)
		event.ValueBefore, event.ValueAfter = engine.applyPickupAttributeDelta(actor, attribute, delta)
	default:
		attribute, amount, _ := supportedPickupEffect(pickup.SceneID)
		event.Attribute = attribute
		recordNativeBasicPickupDelta(actor, attribute, int8(amount))
		event.ValueBefore, event.ValueAfter = engine.applyPickupAttributeDelta(actor, attribute, int8(amount))
	}
	pickup.State = PickupCollected
	return event
}

func recordNativeBasicPickupDelta(actor *Actor, attribute AttributeKind, delta int8) {
	if actor == nil || delta == 0 {
		return
	}
	index := -1
	switch attribute {
	case AttributeBombCapacity:
		index = 0
	case AttributeBombPower:
		index = 1
	case AttributeSpeedRate:
		index = 2
	}
	if index >= 0 {
		actor.nativeBasicPickupDelta[index] += int16(delta)
	}
}

// ApplyVerifiedPickup applies one pickup already authenticated by an external
// native event stream. It is intentionally separate from Step and LegalActions:
// live simulation and RL workers still collect by deterministic collision,
// while QBV differential replay can preserve the original client's confirmed
// transformation/action sequence without snapping the simulated actor to an
// observed coordinate. Callers are responsible for validating the source
// packet and map ownership before using this bridge.
func (engine *Engine) ApplyVerifiedPickup(playerID uint16, sceneID uint32) (Event, error) {
	return engine.ApplyVerifiedPickupAt(playerID, sceneID, Position{})
}

// ApplyVerifiedPickupAt mirrors the native 0x0FAC/0x0FAD coordinates in
// addition to the item effect. Those coordinates are the actor's world
// position at contact time (confirmed by QBV movement checkpoints), so they
// identify the exact native grid cell at floor(Y/40),floor(X/40). SceneID 61 reveals hidden
// contents but never authenticates collection through a wall. Consuming the matching map entry prevents the
// live engine from offering an item to an AI after a human client already
// removed it. A missing entry still accepts the authenticated effect because
// old recordings and partial map projections may not contain the source item.
func (engine *Engine) ApplyVerifiedPickupAt(playerID uint16, sceneID uint32, position Position) (Event, error) {
	actor, normalizedPosition, err := engine.validateVerifiedPickup(playerID, sceneID, position)
	if err != nil {
		return Event{}, err
	}
	if index := engine.verifiedPickupIndex(actor, sceneID, normalizedPosition); index >= 0 {
		return engine.collectPickup(actor, &engine.pickups[index]), nil
	}
	// A successful native 0x0FAD proves that this cell contained and consumed
	// the requested object. Remove any stale mirror object occupying that exact
	// cell before applying the authenticated effect; leaving it behind makes a
	// virtual participant request a second, nonexistent pickup later.
	engine.retirePickupsAtCell(normalizedPosition.Cell())
	pickup := Pickup{SceneID: sceneID, Cell: normalizedPosition.Cell(), State: PickupAvailable}
	return engine.collectPickup(actor, &pickup), nil
}

// ApplyExistingPickupAt is a strict live-world diagnostic variant. It never
// manufactures a missing scene object; replay ingestion retains compatibility
// with partial recordings only in ApplyVerifiedPickupAt above.
func (engine *Engine) ApplyExistingPickupAt(playerID uint16, sceneID uint32, position Position) (Event, error) {
	actor, normalizedPosition, err := engine.validateVerifiedPickup(playerID, sceneID, position)
	if err != nil {
		return Event{}, err
	}
	index := engine.verifiedPickupIndex(actor, sceneID, normalizedPosition)
	if index < 0 {
		return Event{}, fmt.Errorf("pickup scene %d is not available at player %d position %d,%d", sceneID, playerID, normalizedPosition.X, normalizedPosition.Y)
	}
	return engine.collectPickup(actor, &engine.pickups[index]), nil
}

func (engine *Engine) validateVerifiedPickup(playerID uint16, sceneID uint32, position Position) (*Actor, Position, error) {
	if engine == nil {
		return nil, Position{}, fmt.Errorf("battle engine is nil")
	}
	actorIndex := engine.actorIndex(playerID)
	if actorIndex < 0 {
		return nil, Position{}, fmt.Errorf("verified pickup player %d is not a participant", playerID)
	}
	actor := &engine.actors[actorIndex]
	if actor.State != ActorActive {
		return nil, Position{}, fmt.Errorf("verified pickup player %d is not active", playerID)
	}
	if !engine.actorCapabilities(actor, actor.Facing).CanCollectItems {
		return nil, Position{}, fmt.Errorf("verified pickup player %d current avatar cannot collect items", playerID)
	}
	if _, _, supported := supportedPickupEffect(sceneID); !supported {
		return nil, Position{}, fmt.Errorf("verified pickup uses unsupported scene ID %d", sceneID)
	}
	if position == (Position{}) {
		position = actor.Position
	}
	if position.X < 0 || position.Y < 0 || position.X >= int32(engine.grid.Width)*CellSizePixels || position.Y >= int32(engine.grid.Height)*CellSizePixels {
		return nil, Position{}, fmt.Errorf("verified pickup player %d position %d,%d is outside map", playerID, position.X, position.Y)
	}
	return actor, position, nil
}

func (engine *Engine) verifiedPickupIndex(actor *Actor, sceneID uint32, position Position) int {
	cell := position.Cell()
	for index := range engine.pickups {
		pickup := &engine.pickups[index]
		if pickup.SceneID == sceneID && pickup.State == PickupAvailable && pickup.Cell == cell {
			return index
		}
	}
	return -1
}

// installAuthoritativePickup makes one native scene object the sole live
// pickup at its cell. Native wall-reveal and death-drop messages use one
// object per cell, while the Go mirror can temporarily contain a stale hidden
// or predicted object after geometry diverges.
func (engine *Engine) installAuthoritativePickup(pickup Pickup) {
	keep := -1
	for index := range engine.pickups {
		candidate := &engine.pickups[index]
		if candidate.Cell != pickup.Cell || candidate.State == PickupCollected {
			continue
		}
		if keep < 0 && candidate.SceneID == pickup.SceneID {
			keep = index
			candidate.State = PickupAvailable
			continue
		}
		candidate.State = PickupCollected
	}
	if keep < 0 {
		pickup.State = PickupAvailable
		engine.pickups = append(engine.pickups, pickup)
	}
}

func (engine *Engine) retirePickupsAtCell(cell Cell) {
	for index := range engine.pickups {
		if engine.pickups[index].Cell == cell && engine.pickups[index].State != PickupCollected {
			engine.pickups[index].State = PickupCollected
		}
	}
}

func (engine *Engine) installTransformation(actor *Actor, definition sceneelement.TransformationDefinition) {
	// 005ad175 switches harm protection to the new avatar's own +18 flag,
	// initialized to zero by 005b4d22. The embedded normal body's remaining
	// protection is inactive; a later recovery installs a fresh normal timer.
	actor.HarmProtectionExpiresAt = 0
	if previous, ok := sceneelement.NativeTransformation(sceneelement.ID(actor.TransformationSceneID)); ok && previous.GrantedActionID != 0 {
		removeHeldAction(actor, previous.GrantedActionID)
	}
	actor.TransformationSceneID = uint32(definition.SceneID)
	actor.AvatarRoleID = definition.AvatarRoleID
	actor.TransformationExpiresAt = saturatingAdd(engine.elapsedMS, definition.DurationMS)
	if definition.GrantedActionID != 0 {
		removeHeldAction(actor, definition.GrantedActionID)
		grantHeldAction(actor, definition.GrantedActionID, definition.GrantedActionCount)
	}
}

func (engine *Engine) hiddenPickupReachActive(actor *Actor) bool {
	return actor.HiddenPickupReachExpiresAt != 0 && engine.elapsedMS <= actor.HiddenPickupReachExpiresAt
}

func (engine *Engine) sceneFourEffectActive(actor *Actor) bool {
	return actor.SceneFourEffectExpiresAt != 0 && engine.elapsedMS <= actor.SceneFourEffectExpiresAt
}

func nativeRandomAttributeEffect(cell Cell) (AttributeKind, int8) {
	// Client+0x1CD489 seeds Client+0x522F0 with row*15+col, consumes one
	// value for the attribute modulo three and a second for the sign bit.
	rng := nativeMapRNG{state: uint32(int32(cell.Row)*15 + int32(cell.Col))}
	attribute := [...]AttributeKind{AttributeBombCapacity, AttributeBombPower, AttributeSpeedRate}[rng.next()%3]
	if rng.next()&1 == 1 {
		return attribute, 1
	}
	return attribute, -1
}

func absoluteCellDelta(first, second int16) int16 {
	if first < second {
		return second - first
	}
	return first - second
}

func (engine *Engine) applyPickupAttributeDelta(actor *Actor, attribute AttributeKind, delta int8) (byte, byte) {
	switch attribute {
	case AttributeBombCapacity:
		before := actor.BombCapacity
		actor.BombCapacity = changeAttributeBounded(before, delta, actor.MaxBombCapacity)
		return before, actor.BombCapacity
	case AttributeBombPower:
		before := actor.BombPower
		actor.BombPower = changeAttributeBounded(before, delta, actor.MaxBombPower)
		return before, actor.BombPower
	case AttributeSpeedRate:
		before := actor.SpeedRate
		actor.SpeedRate = changeAttributeBounded(before, delta, actor.MaxSpeedRate)
		actor.SpeedPixelsPerSecond = engine.rules.SpeedPixelsPerSecondByRate[actor.SpeedRate]
		actor.moveRemainder = 0
		return before, actor.SpeedRate
	default:
		return 0, 0
	}
}

func changeAttributeBounded(value byte, delta int8, maximum byte) byte {
	if delta < 0 {
		amount := byte(-delta)
		if amount > value {
			return value
		}
		return value - amount
	}
	return addAttributeCapped(value, byte(delta), maximum)
}

func addAttributeCapped(value, amount, maximum byte) byte {
	if value >= maximum || amount >= maximum-value {
		return maximum
	}
	return value + amount
}
