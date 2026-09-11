package match

import (
	"fmt"

	"qqtang/internal/clientdata/sceneelement"
)

// CompetitiveSceneRewards is one player's server-side settlement projection
// of native battlefield reward objects. It deliberately excludes rule-owned
// objectives such as treasure gems and ordinary temporary props.
type CompetitiveSceneRewards struct {
	Money      uint32
	Experience uint32
}

type competitiveRewardPickupKey struct {
	PlayerID   uint16
	ClientTime uint32
	ItemID     uint32
	PosX       uint16
	PosY       uint16
}

func addRewardValue(current, delta uint32) (uint32, error) {
	if ^uint32(0)-current < delta {
		return 0, fmt.Errorf("competitive scene reward counter overflow: %d + %d", current, delta)
	}
	return current + delta, nil
}

// RecordBossSceneDrops validates concrete NOTIFY_NPC_DROPITEM output against
// the registered BOSS_INFO.NormalItems inventory. The native arbitrator owns
// random subset selection and positions; the server owns inventory limits.
func (battle *CompetitiveBattle) RecordBossSceneDrops(objectID uint16, itemIDs []uint32) error {
	if battle == nil {
		return fmt.Errorf("competitive battle is nil")
	}
	battle.mu.Lock()
	defer battle.mu.Unlock()
	remaining, registered := battle.bossSceneItems[objectID]
	if !registered {
		return fmt.Errorf("competitive Boss %d has no registered scene-item inventory", objectID)
	}
	wanted := make(map[uint32]uint32, len(itemIDs))
	for _, itemID := range itemIDs {
		if itemID == 0 {
			return fmt.Errorf("competitive Boss %d attempted to drop item ID zero", objectID)
		}
		wanted[itemID]++
	}
	for itemID, quantity := range wanted {
		if remaining[itemID] < quantity {
			return fmt.Errorf("competitive Boss %d drop item %d count %d exceeds remaining inventory %d", objectID, itemID, quantity, remaining[itemID])
		}
	}
	for itemID, quantity := range wanted {
		remaining[itemID] -= quantity
		if _, proven := sceneelement.NativeReward(sceneelement.ID(itemID)); proven || sceneelement.IsPermanentInventoryPickup(itemID) {
			battle.droppedSceneItems[itemID] += quantity
		}
	}
	return nil
}

// RecordBossDeathDrops validates the QQT_GAME_ITEM array embedded in the
// native NOTIFY_PLAYER_DIE for a Boss. The lethal rule callback may emit
// remaining NormalItems and OutfitItems entries and recycle up to ten units
// of each native base attribute as SceneID 1/2/3. Transport validation is
// intentionally permissive about omitted inventory entries; it rejects only
// items or quantities that exceed the advertised source pools.
func (battle *CompetitiveBattle) RecordBossDeathDrops(objectID uint16, itemIDs []uint32) error {
	if battle == nil {
		return fmt.Errorf("competitive battle is nil")
	}
	battle.mu.Lock()
	defer battle.mu.Unlock()
	if alive, registered := battle.bossAlive[objectID]; !registered {
		return fmt.Errorf("competitive Boss %d is not a registered objective", objectID)
	} else if !alive {
		// The same native event can be observed on fast and reliable transports.
		// Once RecordBossDeath committed it, the second copy is idempotent.
		return nil
	}
	normalRemaining := battle.bossSceneItems[objectID]
	deathRemaining := battle.bossDeathItems[objectID]
	wanted := make(map[uint32]uint32, len(itemIDs))
	for _, itemID := range itemIDs {
		if itemID == 0 {
			return fmt.Errorf("competitive Boss %d attempted to drop item ID zero on death", objectID)
		}
		if wanted[itemID] == ^uint32(0) {
			return fmt.Errorf("competitive Boss %d death item %d count overflows", objectID, itemID)
		}
		wanted[itemID]++
	}
	for itemID, quantity := range wanted {
		if itemID >= 1 && itemID <= 3 {
			if quantity > 10 {
				return fmt.Errorf("competitive Boss %d recycled base item %d count %d exceeds native limit 10", objectID, itemID, quantity)
			}
			continue
		}
		available := normalRemaining[itemID]
		if ^uint32(0)-available < deathRemaining[itemID] {
			return fmt.Errorf("competitive Boss %d death item %d inventory overflows", objectID, itemID)
		}
		available += deathRemaining[itemID]
		if available < quantity {
			return fmt.Errorf("competitive Boss %d death item %d count %d exceeds remaining inventory %d", objectID, itemID, quantity, available)
		}
	}
	for itemID, quantity := range wanted {
		if itemID >= 1 && itemID <= 3 {
			continue
		}
		normalUsed := quantity
		if normalUsed > normalRemaining[itemID] {
			normalUsed = normalRemaining[itemID]
		}
		if normalUsed != 0 {
			normalRemaining[itemID] -= normalUsed
		}
		if deathUsed := quantity - normalUsed; deathUsed != 0 {
			deathRemaining[itemID] -= deathUsed
		}
		if _, proven := sceneelement.NativeReward(sceneelement.ID(itemID)); proven || sceneelement.IsPermanentInventoryPickup(itemID) {
			battle.droppedSceneItems[itemID] += quantity
		}
	}
	return nil
}

// RecordBossPermanentItemPickup records a normal account item collected from
// the bounded BOSS_INFO.OutfitItems pool. The native arbitrator owns object
// creation, collision, and the final pickup notification; the server only
// deduplicates that notification and persists the resulting ITEM_INFO at
// settlement. A permanent item not present in this match's Boss inventory is
// ignored here so ordinary map props cannot be misclassified as Boss loot.
func (battle *CompetitiveBattle) RecordBossPermanentItemPickup(playerID uint16, clientTime, itemID uint32, posX, posY uint16) (bool, error) {
	if battle == nil {
		return false, fmt.Errorf("competitive battle is nil")
	}
	if !sceneelement.IsPermanentInventoryPickup(itemID) {
		return false, fmt.Errorf("scene item %d is not a native permanent inventory pickup", itemID)
	}
	battle.mu.Lock()
	defer battle.mu.Unlock()
	if battle.bossSceneItemCapacity[itemID] == 0 {
		return false, nil
	}
	if _, ok := battle.participantByID[playerID]; !ok {
		return false, fmt.Errorf("Boss-reward player %d is not a participant", playerID)
	}
	if (battle.concluded && !battle.bossVictoryGrace) || !battle.alive[playerID] {
		return false, fmt.Errorf("Boss-reward player %d is not active", playerID)
	}
	key := competitiveRewardPickupKey{
		PlayerID: playerID, ClientTime: clientTime, ItemID: itemID, PosX: posX, PosY: posY,
	}
	if _, duplicate := battle.rewardPickups[key]; duplicate {
		return false, nil
	}
	if battle.bossSceneItemPickups[itemID] >= battle.bossSceneItemCapacity[itemID] {
		return false, fmt.Errorf("Boss-reward item %d exceeds BOSS_INFO inventory %d", itemID, battle.bossSceneItemCapacity[itemID])
	}
	if battle.collectedItems[playerID] == nil {
		battle.collectedItems[playerID] = make(map[uint32]uint32)
	}
	battle.collectedItems[playerID][itemID]++
	battle.bossSceneItemPickups[itemID]++
	if battle.droppedSceneItems[itemID] > 0 {
		battle.droppedSceneItems[itemID]--
	}
	battle.rewardPickups[key] = struct{}{}
	return true, nil
}

// RecordNativeSceneReward records a source-proven currency or experience
// pickup. The item must still exist in GAME_BEGIN.NewItems or in the bounded
// aggregate BOSS_INFO NormalItems/OutfitItems inventory. A native 0x116B or
// lethal 0x0FA7 announcement is
// useful evidence that an object reached the scene, but it is not an ordering
// prerequisite: the final client can author/drop/collect through different
// fast and reliable paths, and the server must not deadlock a valid pickup by
// guessing which one arrives first. The request and notification share an
// event key, so receiving both cannot duplicate the settlement reward.
func (battle *CompetitiveBattle) RecordNativeSceneReward(playerID uint16, clientTime, itemID uint32, posX, posY uint16) (bool, error) {
	if battle == nil {
		return false, fmt.Errorf("competitive battle is nil")
	}
	definition, ok := sceneelement.NativeReward(sceneelement.ID(itemID))
	if !ok {
		return false, fmt.Errorf("scene item %d has no proven native settlement reward", itemID)
	}
	if definition.Kind == sceneelement.RewardTreasureScore {
		return false, fmt.Errorf("scene item %d is a treasure objective, not a settlement reward", itemID)
	}

	battle.mu.Lock()
	defer battle.mu.Unlock()
	if _, ok = battle.participantByID[playerID]; !ok {
		return false, fmt.Errorf("scene-reward player %d is not a participant", playerID)
	}
	if (battle.concluded && !battle.bossVictoryGrace) || !battle.alive[playerID] {
		return false, fmt.Errorf("scene-reward player %d is not active", playerID)
	}
	key := competitiveRewardPickupKey{
		PlayerID: playerID, ClientTime: clientTime, ItemID: itemID, PosX: posX, PosY: posY,
	}
	if _, duplicate := battle.rewardPickups[key]; duplicate {
		return false, nil
	}
	fromInitialScene := battle.initialSceneItems[itemID] > 0
	fromBossInventory := battle.bossSceneItemPickups[itemID] < battle.bossSceneItemCapacity[itemID]
	if !fromInitialScene && !fromBossInventory {
		return false, fmt.Errorf("scene-reward item %d exceeds initialized and BOSS_INFO inventory", itemID)
	}
	rewards := battle.sceneRewards[playerID]
	switch definition.Kind {
	case sceneelement.RewardMatchSugar:
		next, err := addRewardValue(rewards.Money, definition.Value)
		if err != nil {
			return false, fmt.Errorf("scene-reward money for player %d: %w", playerID, err)
		}
		rewards.Money = next
	case sceneelement.RewardCompetitiveExperience:
		next, err := addRewardValue(rewards.Experience, definition.Value)
		if err != nil {
			return false, fmt.Errorf("scene-reward experience for player %d: %w", playerID, err)
		}
		rewards.Experience = next
	default:
		return false, fmt.Errorf("scene item %d has unsupported reward kind %d", itemID, definition.Kind)
	}
	// Consume provenance only after every accounting check succeeds. An
	// overflow or unsupported definition must leave the scene object available
	// for a valid retry rather than partially committing the transaction.
	if fromInitialScene {
		battle.initialSceneItems[itemID]--
	} else {
		battle.bossSceneItemPickups[itemID]++
		// This counter is diagnostic landing state only. A missing or reordered
		// 0x116B must not block the bounded BOSS_INFO-backed pickup above.
		if battle.droppedSceneItems[itemID] > 0 {
			battle.droppedSceneItems[itemID]--
		}
	}
	battle.sceneRewards[playerID] = rewards
	battle.rewardPickups[key] = struct{}{}
	return true, nil
}

// SceneRewards returns a defensive per-player snapshot for the single
// authoritative settlement transaction.
func (battle *CompetitiveBattle) SceneRewards() map[uint16]CompetitiveSceneRewards {
	if battle == nil {
		return nil
	}
	battle.mu.Lock()
	defer battle.mu.Unlock()
	result := make(map[uint16]CompetitiveSceneRewards, len(battle.sceneRewards))
	for playerID, rewards := range battle.sceneRewards {
		result[playerID] = rewards
	}
	return result
}

// CollectedBossItems returns a defensive per-player snapshot of permanent
// BOSS_INFO items accepted from the original client's pickup authority.
func (battle *CompetitiveBattle) CollectedBossItems() map[uint16]map[uint32]uint32 {
	if battle == nil {
		return nil
	}
	battle.mu.Lock()
	defer battle.mu.Unlock()
	return cloneCollectedBossItems(battle.collectedItems)
}

// CollectedBossItemsForSettlement returns the permanent Boss items that will
// be persisted. Some native client versions consume a final OutfitItems
// object locally without sending NotifyPlayerGetItem. A single-player Boss
// victory has no ownership ambiguity, so award the remaining permanent items
// that the server already confirmed as dropped. Multiplayer rounds remain
// event-driven because the server cannot infer which participant collected an
// unreported object.
func (battle *CompetitiveBattle) CollectedBossItemsForSettlement() map[uint16]map[uint32]uint32 {
	if battle == nil {
		return nil
	}
	battle.mu.Lock()
	defer battle.mu.Unlock()
	result := cloneCollectedBossItems(battle.collectedItems)
	if battle.objective != CompetitiveObjectiveBoss || !battle.concluded || battle.winnerTeamID == 0 || len(battle.participants) != 1 {
		return result
	}
	playerID := battle.participants[0].PlayerID
	if result[playerID] == nil {
		result[playerID] = make(map[uint32]uint32)
	}
	for itemID, quantity := range battle.droppedSceneItems {
		if quantity == 0 || !sceneelement.IsPermanentInventoryPickup(itemID) {
			continue
		}
		current := result[playerID][itemID]
		if ^uint32(0)-current < quantity {
			result[playerID][itemID] = ^uint32(0)
			continue
		}
		result[playerID][itemID] = current + quantity
	}
	return result
}

func cloneCollectedBossItems(source map[uint16]map[uint32]uint32) map[uint16]map[uint32]uint32 {
	result := make(map[uint16]map[uint32]uint32, len(source))
	for playerID, items := range source {
		cloned := make(map[uint32]uint32, len(items))
		for itemID, quantity := range items {
			cloned[itemID] = quantity
		}
		result[playerID] = cloned
	}
	return result
}
