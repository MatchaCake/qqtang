package probe

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"io"
	"math/big"

	"qqtang/internal/clientdata/sceneelement"
	"qqtang/internal/game/itemeffect"
	"qqtang/internal/game/mapdata"
	"qqtang/internal/game/match"
	roomstate "qqtang/internal/game/room"
	"qqtang/internal/protocol/game"
	"qqtang/internal/server/persistence"
)

func (server *Server) newCompetitiveGameData(session *connectionSession, selectedMap mapdata.CompetitiveMap, field roomstate.CompetitiveFieldType, arbitratorID uint16, participants []match.CompetitiveParticipant, bossEnabled bool) (game.GameBeginData, error) {
	ruleSpec, ok := mapdata.LookupCompetitiveRuleByNativeID(selectedMap.NativeRule)
	if !ok || ruleSpec.RoundDurationMS == 0 {
		return game.GameBeginData{}, fmt.Errorf("competitive map %d has no native round duration for rule %d", selectedMap.ID, selectedMap.NativeRule)
	}
	var entropy [8]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return game.GameBeginData{}, fmt.Errorf("generate competitive seeds: %w", err)
	}
	spawnSeed := binary.BigEndian.Uint32(entropy[0:4])
	itemSeed := binary.BigEndian.Uint32(entropy[4:8])
	if spawnSeed == 0 {
		spawnSeed = 1
	}
	if itemSeed == 0 {
		itemSeed = 1
	}
	players := make([]game.PlayerGameInfo, 0, len(participants))
	for _, participant := range participants {
		var preparedItems []game.GameItemType
		if field == roomstate.CompetitiveFieldItem && participant.Source == match.CompetitiveParticipantHuman {
			profile, err := server.adventureParticipantProfile(session, participant.PlayerID)
			if err != nil {
				return game.GameBeginData{}, err
			}
			preparedItems, err = server.preparedMatchItems(profile, participant.RoleID, itemeffect.ScopeCompetitiveItem)
			if err != nil {
				return game.GameBeginData{}, err
			}
		}
		players = append(players, game.PlayerGameInfo{
			PlayerID: participant.PlayerID, RoleID: participant.RoleID, TeamID: participant.TeamID,
			DelayTime: participant.DelayTime, ExtPoint: participant.Point, NewItems: preparedItems,
		})
	}
	teamSizes := make(map[byte]int)
	largestTeam := 0
	for _, participant := range participants {
		teamSizes[participant.TeamID]++
		if teamSizes[participant.TeamID] > largestTeam {
			largestTeam = teamSizes[participant.TeamID]
		}
	}
	objectiveItems := selectedMap.CompetitiveObjectiveSceneItems(largestTeam, itemSeed)
	wallCapacity := len(selectedMap.HiddenItemCells)
	var rolledWallItems []mapdata.CompetitiveWallItem
	if bossEnabled {
		rolledWallItems = selectedMap.RollFinalCompetitiveBossWallItems(
			itemSeed, len(participants), wallCapacity, wallCapacity/2,
		)
	} else {
		rolledWallItems = selectedMap.RollFinalCompetitiveOrdinaryWallItems(
			itemSeed, len(participants), wallCapacity, wallCapacity/2,
		)
	}
	wallItems := mergeCompetitiveSceneItems(rolledWallItems, objectiveItems)
	return game.NewGameBeginData(game.GameBeginOptions{
		GameID: session.CurrentGameID, MapID: selectedMap.ID,
		SpawnSeed: spawnSeed, ItemSeed: itemSeed, ArbitratorPlayerID: arbitratorID,
		ContinueID: game.NoRemoteContinueFileID, MapHash: selectedMap.MapHash,
		GameTimeMS: game.DefaultCompetitiveGameTimeMS, Players: players,
		// NewItems combines the selected map's ordinary pool with its native
		// rule-owned objective pool. The client combines quantities with
		// ItemSeed and embedded wall positions, so the server still does not
		// invent coordinates.
		NewItems: wallItems,
	})
}

func mergeCompetitiveSceneItems(groups ...[]mapdata.CompetitiveWallItem) []game.GameItemType {
	quantities := make(map[uint32]int)
	order := make([]uint32, 0)
	for _, group := range groups {
		for _, item := range group {
			if item.SceneID == 0 || item.Quantity <= 0 {
				continue
			}
			if _, exists := quantities[item.SceneID]; !exists {
				order = append(order, item.SceneID)
			}
			quantities[item.SceneID] += int(item.Quantity)
		}
	}
	items := make([]game.GameItemType, 0, len(order))
	for _, sceneID := range order {
		quantity := quantities[sceneID]
		if quantity <= 0 || quantity > int(^uint16(0)>>1) {
			continue
		}
		items = append(items, game.GameItemType{ItemID: sceneID, Quantity: int16(quantity)})
	}
	return items
}

type competitiveMatchActivation struct {
	Candidate   mapdata.CompetitiveBossCandidate
	Active      bool
	SummonItems map[uint32][]mapdata.CompetitiveBossItemRequirement
}

type satisfiedCompetitiveBossCandidate struct {
	Candidate mapdata.CompetitiveBossCandidate
	Plan      map[uint16][]mapdata.CompetitiveBossItemRequirement
}

func inventoryQuantity(inventory []game.ItemInfo, itemID uint16) uint32 {
	for _, item := range inventory {
		if item.ItemID == itemID {
			return item.NumOfItem
		}
	}
	return 0
}

func competitiveBossCandidateSatisfied(candidate mapdata.CompetitiveBossCandidate, participants []match.CompetitiveParticipant, inventories map[uint16][]game.ItemInfo) (bool, error) {
	_, satisfied, err := competitiveBossCandidateConsumptionPlan(candidate, participants, inventories)
	return satisfied, err
}

// competitiveBossCandidateConsumptionPlan chooses the first complete option
// for every active participant, then excludes ownership-only requirements.
// The returned plan is still read-only; one later SQLite transaction owns
// the actual all-player debit.
func competitiveBossCandidateConsumptionPlan(candidate mapdata.CompetitiveBossCandidate, participants []match.CompetitiveParticipant, inventories map[uint16][]game.ItemInfo) (map[uint16][]mapdata.CompetitiveBossItemRequirement, bool, error) {
	switch candidate.Activation {
	case mapdata.CompetitiveBossActivationUnconditional:
		return map[uint16][]mapdata.CompetitiveBossItemRequirement{}, true, nil
	case mapdata.CompetitiveBossActivationAllPlayersOwn:
		if len(candidate.ItemOptions) == 0 {
			return nil, false, fmt.Errorf("Boss candidate %q requires all players to own an empty item option set", candidate.ID)
		}
		plan := make(map[uint16][]mapdata.CompetitiveBossItemRequirement, len(participants))
		for _, participant := range participants {
			inventory, ok := inventories[participant.PlayerID]
			if !ok {
				return nil, false, nil
			}
			var selected []mapdata.CompetitiveBossItemRequirement
			for optionIndex, option := range candidate.ItemOptions {
				if len(option.Requirements) == 0 {
					return nil, false, fmt.Errorf("Boss candidate %q item option %d is empty", candidate.ID, optionIndex)
				}
				optionSatisfied := true
				for _, requirement := range option.Requirements {
					if requirement.ItemID == 0 || requirement.Count == 0 {
						return nil, false, fmt.Errorf("Boss candidate %q has an invalid item requirement %+v", candidate.ID, requirement)
					}
					if inventoryQuantity(inventory, requirement.ItemID) < requirement.Count {
						optionSatisfied = false
						break
					}
				}
				if optionSatisfied {
					selected = append([]mapdata.CompetitiveBossItemRequirement(nil), option.Requirements...)
					break
				}
			}
			if len(selected) == 0 {
				return nil, false, nil
			}
			for _, requirement := range selected {
				if !requirement.OwnershipOnly {
					plan[participant.PlayerID] = append(plan[participant.PlayerID], requirement)
				}
			}
		}
		return plan, true, nil
	default:
		return nil, false, fmt.Errorf("Boss candidate %q has unsupported activation %q", candidate.ID, candidate.Activation)
	}
}

// chooseCompetitiveBossCandidate selects one uniformly from the pool whose
// conditions every active participant satisfies. A map may advertise several
// alternatives, but CompetitiveMaximumActiveBosses still limits the resulting
// round to the one selected entity.
func chooseCompetitiveBossCandidate(pool []satisfiedCompetitiveBossCandidate, random io.Reader) (satisfiedCompetitiveBossCandidate, error) {
	if len(pool) == 0 {
		return satisfiedCompetitiveBossCandidate{}, fmt.Errorf("cannot choose a competitive Boss from an empty pool")
	}
	if len(pool) == 1 {
		return pool[0], nil
	}
	if random == nil {
		return satisfiedCompetitiveBossCandidate{}, fmt.Errorf("competitive Boss selection requires randomness")
	}
	index, err := rand.Int(random, big.NewInt(int64(len(pool))))
	if err != nil {
		return satisfiedCompetitiveBossCandidate{}, fmt.Errorf("select one of %d competitive Boss candidates: %w", len(pool), err)
	}
	return pool[index.Int64()], nil
}

func satisfiedCompetitiveBossCandidates(mapID uint32, candidates []mapdata.CompetitiveBossCandidate, participants []match.CompetitiveParticipant, inventories map[uint16][]game.ItemInfo) ([]satisfiedCompetitiveBossCandidate, error) {
	pool := make([]satisfiedCompetitiveBossCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		plan, satisfied, err := competitiveBossCandidateConsumptionPlan(candidate, participants, inventories)
		if err != nil {
			return nil, fmt.Errorf("competitive map %d: %w", mapID, err)
		}
		if satisfied {
			pool = append(pool, satisfiedCompetitiveBossCandidate{Candidate: candidate, Plan: plan})
		}
	}
	return pool, nil
}

// resolveCompetitiveMatchActivation deliberately runs after map selection and
// participant capture. A normal round stays native when no candidate is
// satisfied; when several are satisfied, exactly one is selected uniformly.
func (server *Server) resolveCompetitiveMatchActivation(session *connectionSession, selectedMap mapdata.CompetitiveMap, participants []match.CompetitiveParticipant) (competitiveMatchActivation, error) {
	if len(selectedMap.BossCandidates) == 0 {
		return competitiveMatchActivation{}, nil
	}
	inventories := make(map[uint16][]game.ItemInfo, len(participants))
	playerUINs := make(map[uint16]uint32, len(participants))
	inventories[session.Profile.PlayerID] = append([]game.ItemInfo(nil), session.Profile.Inventory...)
	playerUINs[session.Profile.PlayerID] = sessionWorldUIN(session)
	server.liveMu.RLock()
	peers := make([]*connectionSession, 0, len(participants)-1)
	for _, live := range server.liveSessions {
		if live != nil && live != session && live.liveRoomID.Load() == uint32(session.RoomID) && live.liveUIN.Load() != 0 {
			peers = append(peers, live)
		}
	}
	server.liveMu.RUnlock()
	for _, peer := range peers {
		playerID := peer.routingPlayerID()
		if peer.auxiliary || peer.liveRoomID.Load() != uint32(session.RoomID) || peer.liveUIN.Load() == 0 || playerID == session.Profile.PlayerID {
			continue
		}
		if peer.mu.TryLock() {
			inventories[playerID] = append([]game.ItemInfo(nil), peer.Profile.Inventory...)
			playerUINs[playerID] = peer.liveUIN.Load()
			peer.mu.Unlock()
			continue
		}
		if server.playerStore != nil {
			profile, loadErr := server.playerStore.Load(context.Background(), peer.liveUIN.Load())
			if loadErr != nil {
				return competitiveMatchActivation{}, fmt.Errorf("load competitive Boss inventory for player %d: %w", playerID, loadErr)
			}
			inventories[playerID] = append([]game.ItemInfo(nil), profile.Inventory...)
			playerUINs[playerID] = peer.liveUIN.Load()
			continue
		}
		return competitiveMatchActivation{}, fmt.Errorf("competitive Boss inventory for player %d is busy and no durable store is available", playerID)
	}
	pool, err := satisfiedCompetitiveBossCandidates(selectedMap.ID, selectedMap.BossCandidates, participants, inventories)
	if err != nil {
		return competitiveMatchActivation{}, err
	}
	if len(pool) == 0 {
		return competitiveMatchActivation{}, nil
	}
	activated, err := chooseCompetitiveBossCandidate(pool, rand.Reader)
	if err != nil {
		return competitiveMatchActivation{}, fmt.Errorf("competitive map %d: %w", selectedMap.ID, err)
	}
	summonItems := make(map[uint32][]mapdata.CompetitiveBossItemRequirement, len(activated.Plan))
	for playerID, requirements := range activated.Plan {
		uin := playerUINs[playerID]
		if uin == 0 {
			return competitiveMatchActivation{}, fmt.Errorf("competitive Boss %q player %d has no live UIN", activated.Candidate.ID, playerID)
		}
		summonItems[uin] = append([]mapdata.CompetitiveBossItemRequirement(nil), requirements...)
	}
	return competitiveMatchActivation{Candidate: activated.Candidate, Active: true, SummonItems: summonItems}, nil
}

func (server *Server) consumeCompetitiveBossSummonItems(session *connectionSession, activation competitiveMatchActivation) (map[uint32]game.PlayerProfile, error) {
	if !activation.Active || len(activation.SummonItems) == 0 {
		return map[uint32]game.PlayerProfile{}, nil
	}
	requests := make([]persistence.InventoryConsumption, 0)
	for uin, requirements := range activation.SummonItems {
		for _, requirement := range requirements {
			requests = append(requests, persistence.InventoryConsumption{
				UIN: uin, ItemID: requirement.ItemID, Quantity: requirement.Count,
			})
		}
	}
	players, err := server.players()
	if err != nil {
		return nil, err
	}
	profiles, err := players.ConsumeInventoryItems(context.Background(), requests)
	if err != nil {
		return nil, fmt.Errorf("consume competitive Boss %q summon items: %w", activation.Candidate.ID, err)
	}
	for uin, profile := range profiles {
		if uin == sessionWorldUIN(session) {
			session.replaceProfile(profile)
		}
		server.syncPrimarySessionProfile(session, uin, profile)
	}
	return profiles, nil
}

func projectCompetitiveParticipants(overlay mapdata.CompetitiveMatchOverlay, participants []match.CompetitiveParticipant) ([]match.CompetitiveParticipant, match.CompetitiveTeamTopology, error) {
	projected := append([]match.CompetitiveParticipant(nil), participants...)
	if overlay.Kind == mapdata.CompetitiveOverlayNone || overlay.TeamProjection == mapdata.CompetitiveTeamsPreserved {
		return projected, match.CompetitiveTeamsOpposed, nil
	}
	if overlay.TeamProjection != mapdata.CompetitiveTeamsUnified || overlay.UnifiedTeamID == 0 {
		return nil, 0, fmt.Errorf("competitive activation has invalid team projection %q/%d", overlay.TeamProjection, overlay.UnifiedTeamID)
	}
	for index := range projected {
		projected[index].TeamID = overlay.UnifiedTeamID
	}
	return projected, match.CompetitiveTeamsCooperative, nil
}

const (
	competitiveBossNeutralTeamID = byte(7)
	competitiveBossFirstID       = uint16(30_001)
)

type competitiveBossRewardInventory struct {
	NormalItems []game.BossItemInfo
	OutfitItems []game.BossItemInfo
}

func (server *Server) competitiveBossRewardItems(candidateID string, itemSeed uint32) (competitiveBossRewardInventory, error) {
	configured := server.config.CompetitiveBossRewards[candidateID]
	if _, err := buildCompetitiveBossRewardItems(candidateID, configured); err != nil {
		return competitiveBossRewardInventory{}, err
	}
	inventory := competitiveBossRewardInventory{
		NormalItems: make([]game.BossItemInfo, 0, len(configured)),
		OutfitItems: make([]game.BossItemInfo, 0, len(configured)),
	}
	for index, item := range configured {
		chance := item.ChancePercent
		if chance == 0 {
			chance = 100
		}
		if chance < 100 && competitiveBossRewardRoll(itemSeed, candidateID, index, item.ItemID) >= uint32(chance) {
			continue
		}
		wire := game.BossItemInfo{ItemID: item.ItemID, ItemCount: item.ItemCount, DropTime: item.DropTime}
		if sceneelement.IsPermanentInventoryPickup(item.ItemID) {
			inventory.OutfitItems = append(inventory.OutfitItems, wire)
		} else {
			inventory.NormalItems = append(inventory.NormalItems, wire)
		}
	}
	return inventory, nil
}

func competitiveBossRewardRoll(itemSeed uint32, candidateID string, index int, itemID uint32) uint32 {
	hash := fnv.New32a()
	var encoded [12]byte
	binary.BigEndian.PutUint32(encoded[0:4], itemSeed)
	binary.BigEndian.PutUint32(encoded[4:8], uint32(index))
	binary.BigEndian.PutUint32(encoded[8:12], itemID)
	_, _ = hash.Write(encoded[:])
	_, _ = hash.Write([]byte(candidateID))
	return hash.Sum32() % 100
}

func buildCompetitiveBossRewardItems(candidateID string, configured []CompetitiveBossRewardConfig) (competitiveBossRewardInventory, error) {
	inventory := competitiveBossRewardInventory{
		NormalItems: make([]game.BossItemInfo, 0, len(configured)),
		OutfitItems: make([]game.BossItemInfo, 0, len(configured)),
	}
	seen := make(map[uint32]struct{}, len(configured))
	for index, item := range configured {
		if item.ItemID == 0 || item.ItemCount == 0 {
			return competitiveBossRewardInventory{}, fmt.Errorf("competitive Boss %q reward[%d] has invalid item/count %d/%d", candidateID, index, item.ItemID, item.ItemCount)
		}
		if _, duplicate := seen[item.ItemID]; duplicate {
			return competitiveBossRewardInventory{}, fmt.Errorf("competitive Boss %q repeats reward scene ID %d", candidateID, item.ItemID)
		}
		if item.ChancePercent > 100 {
			return competitiveBossRewardInventory{}, fmt.Errorf("competitive Boss %q reward[%d] chance %d is outside 0..100", candidateID, index, item.ChancePercent)
		}
		_, nativeReward := sceneelement.NativeReward(sceneelement.ID(item.ItemID))
		permanent := sceneelement.IsPermanentInventoryPickup(item.ItemID)
		if !nativeReward && !permanent {
			return competitiveBossRewardInventory{}, fmt.Errorf("competitive Boss %q reward scene ID %d has no proven native reward or permanent-item constructor", candidateID, item.ItemID)
		}
		seen[item.ItemID] = struct{}{}
		wire := game.BossItemInfo{ItemID: item.ItemID, ItemCount: item.ItemCount, DropTime: item.DropTime}
		if permanent {
			inventory.OutfitItems = append(inventory.OutfitItems, wire)
		} else {
			inventory.NormalItems = append(inventory.NormalItems, wire)
		}
	}
	if len(inventory.NormalItems) > game.CompetitiveBossMaxItems {
		return competitiveBossRewardInventory{}, fmt.Errorf("competitive Boss %q normal reward count %d exceeds %d", candidateID, len(inventory.NormalItems), game.CompetitiveBossMaxItems)
	}
	if len(inventory.OutfitItems) > game.CompetitiveBossMaxOutfits {
		return competitiveBossRewardInventory{}, fmt.Errorf("competitive Boss %q death reward count %d exceeds %d", candidateID, len(inventory.OutfitItems), game.CompetitiveBossMaxOutfits)
	}
	return inventory, nil
}

func (server *Server) competitiveBossStartData(activation competitiveMatchActivation, itemSeed uint32) (game.CreateNPCBoss, bool, error) {
	if !activation.Active || activation.Candidate.Overlay.Kind == mapdata.CompetitiveOverlayNone {
		return game.CreateNPCBoss{}, false, nil
	}
	candidate := activation.Candidate
	if candidate.Overlay.Kind != mapdata.CompetitiveOverlayBoss || candidate.Overlay.BossTemplate != mapdata.CompetitiveBossTemplateSharedNative {
		return game.CreateNPCBoss{}, false, fmt.Errorf("competitive Boss %q has unsupported overlay/template %q/%q", candidate.ID, candidate.Overlay.Kind, candidate.Overlay.BossTemplate)
	}
	if candidate.Entity.RoleID == 0 || candidate.Entity.RoleID > 0xff || candidate.Entity.HP == 0 {
		return game.CreateNPCBoss{}, false, fmt.Errorf("competitive Boss %q has invalid entity profile %+v", candidate.ID, candidate.Entity)
	}
	if err := candidate.Entity.ValidateSkillProgram(); err != nil {
		return game.CreateNPCBoss{}, false, fmt.Errorf("competitive Boss %q: %w", candidate.ID, err)
	}
	aiType, err := candidate.Entity.NativeAIType()
	if err != nil {
		return game.CreateNPCBoss{}, false, fmt.Errorf("competitive Boss %q: %w", candidate.ID, err)
	}
	rewards, err := server.competitiveBossRewardItems(candidate.ID, itemSeed)
	if err != nil {
		return game.CreateNPCBoss{}, false, err
	}
	// The shared native BOSS_INFO receiver can consume several slots, but every
	// currently identified named encounter uses at most one active Boss. Array
	// capacity is not evidence for a multi-Boss round.
	bosses := []game.CompetitiveBossInfo{{
		BossID: competitiveBossFirstID,
		RoleID: byte(candidate.Entity.RoleID), TeamID: competitiveBossNeutralTeamID, AIType: aiType,
		HP: candidate.Entity.HP, Rate: candidate.Entity.Rate, Bubble: candidate.Entity.Bubble, Power: candidate.Entity.Power,
		NormalItems: append([]game.BossItemInfo(nil), rewards.NormalItems...),
		OutfitItems: append([]game.BossItemInfo(nil), rewards.OutfitItems...),
		Skills:      append([]uint32(nil), candidate.Entity.Skills...),
	}}
	// The installed full-model RoleID is candidate-specific. The 500/501
	// entries belong to different rule-1/rule-8 templates and must not be
	// copied into these entities.
	// Label, Time and the other unused arrays stay zero.
	// The receiver resolves the rule-owned spawn cell locally, so Row/Col remain
	// placeholders here.
	return game.CreateNPCBoss{Bosses: bosses}, true, nil
}
