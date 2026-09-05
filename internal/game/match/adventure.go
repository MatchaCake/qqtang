package match

import (
	"fmt"
	"sort"
	"sync"
)

// AdventureParticipant is the server-owned identity used by one adventure
// battle. The same battle object can be shared by every connection in a room;
// the current local room simply contains one participant.
type AdventureParticipant struct {
	PlayerID  uint16
	RoleID    byte
	TeamID    byte
	DelayTime uint32
	ExtPoint  uint32
}

type AdventureResolution struct {
	Concluded           bool
	NewlyConcluded      bool
	FinalVictoryPending bool
	AllPlayersDead      bool
	Participants        []AdventureParticipant
}

type AdventureFinalPlayerResult struct {
	Participant AdventureParticipant
	Won         bool
}

type AdventureFinalResolution struct {
	NewlyConcluded bool
	Players        []AdventureFinalPlayerResult
}

// AdventurePlayerStatistics is per-match, per-player transient state. It is
// carried across stages of one GameID and discarded when the match ends.
type AdventurePlayerStatistics struct {
	KillCount   uint32
	KillScore   uint32
	RescueCount uint32
	RescueScore uint32
	RewardCount uint32
	RewardScore uint32
}

// AdventureItemPickup is the client-reported identity of one collected map
// item. ClientTime and position are part of the identity because the same
// item definition can legitimately be collected more than once in a stage.
type AdventureItemPickup struct {
	PlayerID   uint16
	ClientTime uint32
	ItemID     uint32
	PosX       uint16
	PosY       uint16
}

type adventureItemPickupKey struct {
	AdventureItemPickup
	MapID uint32
}

// AdventureNPCDeathProgress is the authoritative per-stage NPC-instance
// counter. Object IDs are intentionally absent: repeated monster types are
// distinct instances and each valid death advances the stage once.
type AdventureNPCDeathProgress struct {
	DeathOrdinal     uint32
	ExpectedDeaths   uint32
	Recorded         bool
	CreditedPlayerID uint16
	Credited         bool
	StageCompleted   bool
	NewlyCompleted   bool
}

type AdventureDepartureReason byte

const (
	// AdventureDepartureLeftMatch is an explicit mid-match exit. Whether the
	// room layer also removes the member is a separate transition.
	AdventureDepartureLeftMatch AdventureDepartureReason = iota + 1
	AdventureDepartureDisconnected
)

type AdventureDeparture struct {
	Participant        AdventureParticipant
	Reason             AdventureDepartureReason
	ArbitratorPlayerID uint16
	Remaining          []AdventureParticipant
	Concluded          bool
	NewlyConcluded     bool
}

type AdventureSettlementReason byte

const (
	// AdventureSettlementDiedBeforeNextStage is the legacy adventure rule:
	// when surviving teammates advance, a player already marked dead receives
	// an individual settlement and leaves the room instead of being revived.
	AdventureSettlementDiedBeforeNextStage AdventureSettlementReason = iota + 1
)

type AdventureSettlement struct {
	Participant AdventureParticipant
	Reason      AdventureSettlementReason
}

// AdventureStageTransition is a fan-out plan, not merely a new participant
// list. The room layer sends GAME_NEXTMAP only to Survivors and sends a
// personal GAME_OVER/room-exit sequence to every SettledOut participant.
type AdventureStageTransition struct {
	ArbitratorPlayerID uint16
	Survivors          []AdventureParticipant
	SettledOut         []AdventureSettlement
}

// AdventureBattle owns elimination state independently of any TCP
// connection. This is intentionally room/match state so adding another local
// client does not turn an individual death into an immediate game over.
type AdventureBattle struct {
	mu                  sync.Mutex
	gameID              uint32
	mapID               uint32
	arbitratorID        uint16
	participants        map[uint16]AdventureParticipant
	alive               map[uint16]bool
	statistics          map[uint16]AdventurePlayerStatistics
	collectedItems      map[uint16]map[uint32]uint32
	itemPickups         map[adventureItemPickupKey]struct{}
	itemSeed            uint32
	expectedNPCDeaths   uint32
	npcDeathCount       uint32
	stageCompleted      bool
	finalVictoryPending bool
	finalVictoryPlayers []AdventureFinalPlayerResult
	concluded           bool
}

func NewAdventureBattle(gameID, mapID uint32, arbitratorID uint16, participants []AdventureParticipant) (*AdventureBattle, error) {
	return NewAdventureBattleWithStage(gameID, mapID, 1, 0, arbitratorID, participants)
}

func NewAdventureBattleWithStage(gameID, mapID, itemSeed, expectedNPCDeaths uint32, arbitratorID uint16, participants []AdventureParticipant) (*AdventureBattle, error) {
	if gameID == 0 || mapID == 0 {
		return nil, fmt.Errorf("adventure battle game and map IDs must be non-zero")
	}
	if itemSeed == 0 {
		return nil, fmt.Errorf("adventure battle item seed must be non-zero")
	}
	if len(participants) == 0 || len(participants) > 8 {
		return nil, fmt.Errorf("adventure participant count %d is outside 1..8", len(participants))
	}
	battle := &AdventureBattle{
		gameID: gameID, mapID: mapID, itemSeed: itemSeed, expectedNPCDeaths: expectedNPCDeaths, arbitratorID: arbitratorID,
		participants:   make(map[uint16]AdventureParticipant, len(participants)),
		alive:          make(map[uint16]bool, len(participants)),
		statistics:     make(map[uint16]AdventurePlayerStatistics, len(participants)),
		collectedItems: make(map[uint16]map[uint32]uint32, len(participants)),
		itemPickups:    make(map[adventureItemPickupKey]struct{}),
	}
	for _, participant := range participants {
		if participant.PlayerID == 0 || participant.TeamID == 0 {
			return nil, fmt.Errorf("adventure participant player and team IDs must be non-zero")
		}
		if _, exists := battle.participants[participant.PlayerID]; exists {
			return nil, fmt.Errorf("duplicate adventure player ID %d", participant.PlayerID)
		}
		battle.participants[participant.PlayerID] = participant
		battle.alive[participant.PlayerID] = true
		battle.statistics[participant.PlayerID] = AdventurePlayerStatistics{}
		battle.collectedItems[participant.PlayerID] = make(map[uint32]uint32)
	}
	if _, exists := battle.participants[arbitratorID]; !exists {
		return nil, fmt.Errorf("adventure arbitrator player %d is not a participant", arbitratorID)
	}
	return battle, nil
}

func (battle *AdventureBattle) HasParticipant(playerID uint16) bool {
	if battle == nil || playerID == 0 {
		return false
	}
	battle.mu.Lock()
	defer battle.mu.Unlock()
	_, exists := battle.participants[playerID]
	return exists
}

// Participants returns the players that still belong to the current stage.
// A dead player remains a participant until the next-stage transition settles
// them out, so final team victory can include a teammate who died during the
// last stage without confusing death state with match outcome.
func (battle *AdventureBattle) Participants() []AdventureParticipant {
	if battle == nil {
		return nil
	}
	battle.mu.Lock()
	defer battle.mu.Unlock()
	participants := make([]AdventureParticipant, 0, len(battle.participants))
	for _, participant := range battle.participants {
		participants = append(participants, participant)
	}
	sort.Slice(participants, func(i, j int) bool {
		return participants[i].PlayerID < participants[j].PlayerID
	})
	return participants
}

// IsArbitrator reports whether playerID currently owns native adventure scene
// generation. The value can change between stages or after a participant
// leaves, so callers must query the battle instead of caching the room owner.
func (battle *AdventureBattle) IsArbitrator(playerID uint16) bool {
	if battle == nil || playerID == 0 {
		return false
	}
	battle.mu.Lock()
	defer battle.mu.Unlock()
	return battle.arbitratorID == playerID
}

// ArbitratorPlayerID returns the current owner of native shared-scene rules.
// Requests such as REQUEST_GET_ITEM must be delivered to exactly this client;
// every other participant only consumes the authoritative notification it
// produces.
func (battle *AdventureBattle) ArbitratorPlayerID() uint16 {
	if battle == nil {
		return 0
	}
	battle.mu.Lock()
	defer battle.mu.Unlock()
	return battle.arbitratorID
}

// RecordNPCDeath advances the stage instance count and awards shared courage
// to every participant still carried by this stage. The observed death body
// has no proven killer ID, so the kill is a cooperative stage achievement,
// not an invented last-hit attribution. A player eliminated during this stage
// remains eligible until the next doorway settles that player out.
func (battle *AdventureBattle) RecordNPCDeath(score uint32) (AdventureNPCDeathProgress, error) {
	if battle == nil {
		return AdventureNPCDeathProgress{}, fmt.Errorf("adventure battle is nil")
	}
	battle.mu.Lock()
	defer battle.mu.Unlock()
	if battle.concluded {
		return AdventureNPCDeathProgress{}, fmt.Errorf("adventure battle %d is already concluded", battle.gameID)
	}
	progress := AdventureNPCDeathProgress{
		DeathOrdinal:   battle.npcDeathCount,
		ExpectedDeaths: battle.expectedNPCDeaths,
		StageCompleted: battle.stageCompleted,
	}
	if battle.stageCompleted {
		return progress, nil
	}
	battle.npcDeathCount = saturatingIncrement(battle.npcDeathCount, 1)
	progress.DeathOrdinal = battle.npcDeathCount
	progress.Recorded = true
	for playerID := range battle.participants {
		statistics := battle.statistics[playerID]
		statistics.KillCount = saturatingIncrement(statistics.KillCount, 1)
		statistics.KillScore = saturatingIncrement(statistics.KillScore, score)
		battle.statistics[playerID] = statistics
		if len(battle.participants) == 1 {
			progress.CreditedPlayerID = playerID
			progress.Credited = true
		}
	}
	if battle.expectedNPCDeaths != 0 && battle.npcDeathCount >= battle.expectedNPCDeaths {
		battle.stageCompleted = true
		progress.StageCompleted = true
		progress.NewlyCompleted = true
	}
	return progress, nil
}

func (battle *AdventureBattle) ItemSeed() uint32 {
	if battle == nil {
		return 0
	}
	battle.mu.Lock()
	defer battle.mu.Unlock()
	return battle.itemSeed
}

// CanAdvanceStage reports whether a client doorway request may be accepted.
// Unknown legacy stages (expected count zero) retain the client request as the
// completion authority; mapped stages require the observed instance count.
func (battle *AdventureBattle) CanAdvanceStage() bool {
	if battle == nil {
		return false
	}
	battle.mu.Lock()
	defer battle.mu.Unlock()
	return !battle.concluded && !battle.finalVictoryPending && (battle.expectedNPCDeaths == 0 || battle.stageCompleted)
}

func (battle *AdventureBattle) RecordPlayerRescue(playerID uint16, score uint32) error {
	if battle == nil {
		return fmt.Errorf("adventure battle is nil")
	}
	battle.mu.Lock()
	defer battle.mu.Unlock()
	if battle.concluded {
		return fmt.Errorf("adventure battle %d is already concluded", battle.gameID)
	}
	if _, exists := battle.participants[playerID]; !exists {
		return fmt.Errorf("rescuing player %d is not in adventure battle %d", playerID, battle.gameID)
	}
	statistics := battle.statistics[playerID]
	statistics.RescueCount = saturatingIncrement(statistics.RescueCount, 1)
	statistics.RescueScore = saturatingIncrement(statistics.RescueScore, score)
	battle.statistics[playerID] = statistics
	return nil
}

// RecordPlayerSaved records the confirmed NOTIFY_PLAYER_BE_SAVED transition.
// The same wire event is used for an ordinary bubble rescue and for a dead
// teammate restored by a medical prepared prop. If the destination was still
// alive this only updates rescue statistics; if it was eliminated, it becomes
// eligible for the current stage again. Item consumption remains outside the
// battle state and is owned by the prepared-prop/application transaction.
func (battle *AdventureBattle) RecordPlayerSaved(playerID, destinationPlayerID uint16, score uint32) (bool, error) {
	if battle == nil {
		return false, fmt.Errorf("adventure battle is nil")
	}
	if playerID == 0 || destinationPlayerID == 0 || playerID == destinationPlayerID {
		return false, fmt.Errorf("adventure saved transition requires distinct non-zero players")
	}
	battle.mu.Lock()
	defer battle.mu.Unlock()
	if battle.concluded {
		return false, fmt.Errorf("adventure battle %d is already concluded", battle.gameID)
	}
	if battle.finalVictoryPending {
		return false, fmt.Errorf("adventure battle %d has locked final-stage outcomes", battle.gameID)
	}
	if _, exists := battle.participants[playerID]; !exists {
		return false, fmt.Errorf("rescuing player %d is not in adventure battle %d", playerID, battle.gameID)
	}
	if _, exists := battle.participants[destinationPlayerID]; !exists {
		return false, fmt.Errorf("saved player %d is not in adventure battle %d", destinationPlayerID, battle.gameID)
	}
	statistics := battle.statistics[playerID]
	statistics.RescueCount = saturatingIncrement(statistics.RescueCount, 1)
	statistics.RescueScore = saturatingIncrement(statistics.RescueScore, score)
	battle.statistics[playerID] = statistics
	revived := !battle.alive[destinationPlayerID]
	if revived {
		battle.alive[destinationPlayerID] = true
	}
	return revived, nil
}

// RecordPlayerRelive records the native NOTIFY_PLAYER_RELIVE transition used
// after a medical prepared prop restores a dead adventure participant. Unlike
// NOTIFY_PLAYER_BE_SAVED, this event contains only the revived player and does
// not identify the rescuer, so it changes lifecycle state without inventing a
// rescue-statistics owner.
func (battle *AdventureBattle) RecordPlayerRelive(playerID uint16) (bool, error) {
	if battle == nil {
		return false, fmt.Errorf("adventure battle is nil")
	}
	if playerID == 0 {
		return false, fmt.Errorf("adventure relive transition requires a non-zero player")
	}
	battle.mu.Lock()
	defer battle.mu.Unlock()
	if battle.concluded {
		return false, fmt.Errorf("adventure battle %d is already concluded", battle.gameID)
	}
	if battle.finalVictoryPending {
		return false, fmt.Errorf("adventure battle %d has locked final-stage outcomes", battle.gameID)
	}
	if _, exists := battle.participants[playerID]; !exists {
		return false, fmt.Errorf("relived player %d is not in adventure battle %d", playerID, battle.gameID)
	}
	revived := !battle.alive[playerID]
	if revived {
		battle.alive[playerID] = true
	}
	return revived, nil
}

// RecordPlayerItemPickup accepts both REQUEST_GET_ITEM and the client
// arbitrator's NOTIFY_PLAYER_GET_ITEM form. An exact request/notification
// pair is counted once, while distinct pickups of the same ItemID remain
// independent through ClientTime and position.
func (battle *AdventureBattle) RecordPlayerItemPickup(pickup AdventureItemPickup, score uint32) (bool, error) {
	if battle == nil {
		return false, fmt.Errorf("adventure battle is nil")
	}
	battle.mu.Lock()
	defer battle.mu.Unlock()
	if battle.concluded {
		return false, fmt.Errorf("adventure battle %d is already concluded", battle.gameID)
	}
	if pickup.ItemID == 0 {
		return false, fmt.Errorf("reward item ID must be non-zero")
	}
	if _, exists := battle.participants[pickup.PlayerID]; !exists {
		return false, fmt.Errorf("rewarded player %d is not in adventure battle %d", pickup.PlayerID, battle.gameID)
	}
	key := adventureItemPickupKey{AdventureItemPickup: pickup, MapID: battle.mapID}
	if _, duplicate := battle.itemPickups[key]; duplicate {
		return false, nil
	}
	battle.itemPickups[key] = struct{}{}
	statistics := battle.statistics[pickup.PlayerID]
	statistics.RewardCount = saturatingIncrement(statistics.RewardCount, 1)
	statistics.RewardScore = saturatingIncrement(statistics.RewardScore, score)
	battle.statistics[pickup.PlayerID] = statistics
	items := battle.collectedItems[pickup.PlayerID]
	items[pickup.ItemID] = saturatingIncrement(items[pickup.ItemID], 1)
	return true, nil
}

func (battle *AdventureBattle) CollectedItems(playerID uint16) (map[uint32]uint32, error) {
	if battle == nil {
		return nil, fmt.Errorf("adventure battle is nil")
	}
	battle.mu.Lock()
	defer battle.mu.Unlock()
	items, exists := battle.collectedItems[playerID]
	if !exists {
		return nil, fmt.Errorf("player %d has no collected items in adventure battle %d", playerID, battle.gameID)
	}
	copyOfItems := make(map[uint32]uint32, len(items))
	for itemID, quantity := range items {
		copyOfItems[itemID] = quantity
	}
	return copyOfItems, nil
}

func (battle *AdventureBattle) PlayerStatistics(playerID uint16) (AdventurePlayerStatistics, error) {
	if battle == nil {
		return AdventurePlayerStatistics{}, fmt.Errorf("adventure battle is nil")
	}
	battle.mu.Lock()
	defer battle.mu.Unlock()
	statistics, exists := battle.statistics[playerID]
	if !exists {
		return AdventurePlayerStatistics{}, fmt.Errorf("player %d has no statistics in adventure battle %d", playerID, battle.gameID)
	}
	return statistics, nil
}

// AdvanceStage replaces the live participant/elimination view while carrying
// forward statistics for survivors of the same GameID.
func (battle *AdventureBattle) AdvanceStage(mapID uint32, arbitratorID uint16, participants []AdventureParticipant) error {
	return battle.AdvanceStageWithRule(mapID, 1, 0, arbitratorID, participants)
}

func (battle *AdventureBattle) AdvanceStageWithRule(mapID, itemSeed, expectedNPCDeaths uint32, arbitratorID uint16, participants []AdventureParticipant) error {
	if battle == nil {
		return fmt.Errorf("adventure battle is nil")
	}
	if mapID == 0 || itemSeed == 0 || len(participants) == 0 || len(participants) > 8 {
		return fmt.Errorf("invalid next adventure stage map=%d participants=%d", mapID, len(participants))
	}
	battle.mu.Lock()
	defer battle.mu.Unlock()
	nextParticipants := make(map[uint16]AdventureParticipant, len(participants))
	nextAlive := make(map[uint16]bool, len(participants))
	nextStatistics := make(map[uint16]AdventurePlayerStatistics, len(participants))
	nextCollectedItems := make(map[uint16]map[uint32]uint32, len(participants))
	for _, participant := range participants {
		if participant.PlayerID == 0 || participant.TeamID == 0 {
			return fmt.Errorf("next-stage participant player and team IDs must be non-zero")
		}
		if _, duplicate := nextParticipants[participant.PlayerID]; duplicate {
			return fmt.Errorf("duplicate next-stage player ID %d", participant.PlayerID)
		}
		if _, exists := battle.participants[participant.PlayerID]; !exists {
			return fmt.Errorf("next-stage player %d was not in adventure battle %d", participant.PlayerID, battle.gameID)
		}
		nextParticipants[participant.PlayerID] = participant
		nextAlive[participant.PlayerID] = true
		nextStatistics[participant.PlayerID] = battle.statistics[participant.PlayerID]
		nextCollectedItems[participant.PlayerID] = battle.collectedItems[participant.PlayerID]
	}
	if _, exists := nextParticipants[arbitratorID]; !exists {
		return fmt.Errorf("next-stage arbitrator player %d is not a survivor", arbitratorID)
	}
	battle.mapID = mapID
	battle.itemSeed = itemSeed
	battle.expectedNPCDeaths = expectedNPCDeaths
	battle.npcDeathCount = 0
	battle.stageCompleted = false
	battle.finalVictoryPending = false
	battle.finalVictoryPlayers = nil
	battle.arbitratorID = arbitratorID
	battle.participants = nextParticipants
	battle.alive = nextAlive
	battle.statistics = nextStatistics
	battle.collectedItems = nextCollectedItems
	battle.concluded = false
	return nil
}

func saturatingIncrement(value, increment uint32) uint32 {
	if ^uint32(0)-value < increment {
		return ^uint32(0)
	}
	return value + increment
}

// SelectAdventureArbitrator chooses from the actual room participant set.
// The preferred ID is normally the room owner or the previous stage's living
// arbitrator. If it is unavailable, the stable lowest participant ID is used;
// the caller can replace this fallback when live multiplayer evidence proves a
// latency-based official policy.
func SelectAdventureArbitrator(preferredID uint16, participants []AdventureParticipant) (uint16, error) {
	if len(participants) == 0 {
		return 0, fmt.Errorf("cannot select adventure arbitrator without participants")
	}
	selected := uint16(0)
	for _, participant := range participants {
		if participant.PlayerID == 0 {
			return 0, fmt.Errorf("cannot select adventure arbitrator from zero player ID")
		}
		if participant.PlayerID == preferredID {
			return preferredID, nil
		}
		if selected == 0 || participant.PlayerID < selected {
			selected = participant.PlayerID
		}
	}
	return selected, nil
}

func (battle *AdventureBattle) RecordDeath(playerID uint16) (AdventureResolution, error) {
	if battle == nil {
		return AdventureResolution{}, fmt.Errorf("adventure battle is nil")
	}
	battle.mu.Lock()
	defer battle.mu.Unlock()
	if _, exists := battle.participants[playerID]; !exists {
		return AdventureResolution{}, fmt.Errorf("player %d is not in adventure battle %d", playerID, battle.gameID)
	}
	if battle.concluded {
		return battle.resolution(false), nil
	}
	battle.alive[playerID] = false
	if battle.finalVictoryPending {
		allDead := true
		for participantID := range battle.participants {
			if battle.alive[participantID] {
				allDead = false
				break
			}
		}
		resolution := battle.resolution(false)
		resolution.FinalVictoryPending = true
		resolution.AllPlayersDead = allDead
		return resolution, nil
	}
	for participantID := range battle.participants {
		if battle.alive[participantID] {
			return battle.resolution(false), nil
		}
	}
	battle.concluded = true
	return battle.resolution(true), nil
}

// BeginFinalStageVictory locks the team outcome as soon as the configured
// final-stage NPC count reaches zero while keeping the battle open for the
// post-clear loot grace period. Adventure is cooperative: every participant
// who reached the final stage with the team wins, including a teammate who
// died before the surviving players completed the objective. Players removed
// at an earlier stage are no longer participants and retain that stage's loss.
func (battle *AdventureBattle) BeginFinalStageVictory() (AdventureFinalResolution, error) {
	if battle == nil {
		return AdventureFinalResolution{}, fmt.Errorf("adventure battle is nil")
	}
	battle.mu.Lock()
	defer battle.mu.Unlock()
	if !battle.stageCompleted || battle.expectedNPCDeaths == 0 {
		return AdventureFinalResolution{}, fmt.Errorf("adventure battle %d final stage is not cleared", battle.gameID)
	}
	if battle.concluded {
		return AdventureFinalResolution{}, fmt.Errorf("adventure battle %d is already concluded", battle.gameID)
	}
	if battle.finalVictoryPending {
		return AdventureFinalResolution{Players: append([]AdventureFinalPlayerResult(nil), battle.finalVictoryPlayers...)}, nil
	}
	resolution := AdventureFinalResolution{NewlyConcluded: true}
	for _, participant := range battle.participants {
		resolution.Players = append(resolution.Players, AdventureFinalPlayerResult{
			Participant: participant,
			Won:         true,
		})
	}
	sort.Slice(resolution.Players, func(i, j int) bool {
		return resolution.Players[i].Participant.PlayerID < resolution.Players[j].Participant.PlayerID
	})
	battle.finalVictoryPending = true
	battle.finalVictoryPlayers = append([]AdventureFinalPlayerResult(nil), resolution.Players...)
	return resolution, nil
}

// FinalizePendingVictory ends the grace period exactly once. Outcomes were
// fixed by BeginFinalStageVictory, while statistics and collected items remain
// mutable until this transition succeeds.
func (battle *AdventureBattle) FinalizePendingVictory() (AdventureFinalResolution, error) {
	if battle == nil {
		return AdventureFinalResolution{}, fmt.Errorf("adventure battle is nil")
	}
	battle.mu.Lock()
	defer battle.mu.Unlock()
	if battle.concluded {
		return AdventureFinalResolution{Players: append([]AdventureFinalPlayerResult(nil), battle.finalVictoryPlayers...)}, nil
	}
	if !battle.finalVictoryPending {
		return AdventureFinalResolution{}, fmt.Errorf("adventure battle %d has no pending final victory", battle.gameID)
	}
	battle.finalVictoryPending = false
	battle.concluded = true
	return AdventureFinalResolution{
		NewlyConcluded: true,
		Players:        append([]AdventureFinalPlayerResult(nil), battle.finalVictoryPlayers...),
	}, nil
}

func (battle *AdventureBattle) PrepareNextStage(requesterID uint16) (AdventureStageTransition, error) {
	if battle == nil {
		return AdventureStageTransition{}, fmt.Errorf("adventure battle is nil")
	}
	battle.mu.Lock()
	defer battle.mu.Unlock()
	if _, exists := battle.participants[requesterID]; !exists {
		return AdventureStageTransition{}, fmt.Errorf("player %d is not in adventure battle %d", requesterID, battle.gameID)
	}
	if !battle.alive[requesterID] {
		return AdventureStageTransition{}, fmt.Errorf("eliminated player %d cannot advance adventure battle %d", requesterID, battle.gameID)
	}
	if battle.concluded {
		return AdventureStageTransition{}, fmt.Errorf("adventure battle %d is already concluded", battle.gameID)
	}
	if battle.finalVictoryPending {
		return AdventureStageTransition{}, fmt.Errorf("adventure battle %d is waiting for final loot grace settlement", battle.gameID)
	}
	// A doorway request does not create missing NPC deaths. Counts for known
	// stages come from the installed battlefield's NPC instances. Keep this
	// check under the same lock as participant and outcome validation.
	if battle.expectedNPCDeaths != 0 && !battle.stageCompleted {
		return AdventureStageTransition{}, fmt.Errorf("adventure battle %d stage %d is not cleared: NPC deaths %d of %d", battle.gameID, battle.mapID, battle.npcDeathCount, battle.expectedNPCDeaths)
	}
	transition := AdventureStageTransition{}
	for playerID, participant := range battle.participants {
		if battle.alive[playerID] {
			transition.Survivors = append(transition.Survivors, participant)
		} else {
			transition.SettledOut = append(transition.SettledOut, AdventureSettlement{
				Participant: participant,
				Reason:      AdventureSettlementDiedBeforeNextStage,
			})
		}
	}
	sort.Slice(transition.Survivors, func(i, j int) bool { return transition.Survivors[i].PlayerID < transition.Survivors[j].PlayerID })
	sort.Slice(transition.SettledOut, func(i, j int) bool {
		return transition.SettledOut[i].Participant.PlayerID < transition.SettledOut[j].Participant.PlayerID
	})
	transition.ArbitratorPlayerID, _ = SelectAdventureArbitrator(battle.arbitratorID, transition.Survivors)
	return transition, nil
}

// RemoveParticipant handles both an explicit match exit and a lost client
// connection. Unlike RecordDeath, removal makes the player ineligible to
// arbitrate immediately. Room membership is owned by the separate room layer.
// Native arbitration belongs to a connected stage client, not to the
// character's current alive flag. A dead participant therefore remains a
// valid arbitrator until it actually leaves or is settled out between stages.
// The battle still concludes when no living participant remains, because no
// teammate is then available to continue the objective or use a medical prop.
func (battle *AdventureBattle) RemoveParticipant(playerID uint16, reason AdventureDepartureReason) (AdventureDeparture, error) {
	if battle == nil {
		return AdventureDeparture{}, fmt.Errorf("adventure battle is nil")
	}
	if reason != AdventureDepartureLeftMatch && reason != AdventureDepartureDisconnected {
		return AdventureDeparture{}, fmt.Errorf("unknown adventure departure reason %d", reason)
	}
	battle.mu.Lock()
	defer battle.mu.Unlock()
	participant, exists := battle.participants[playerID]
	if !exists {
		return AdventureDeparture{}, fmt.Errorf("player %d is not in adventure battle %d", playerID, battle.gameID)
	}
	delete(battle.participants, playerID)
	delete(battle.alive, playerID)
	// Keep the departed player's transient loot until the battle object is
	// disposed.  If this departure also removes the final living avatar, the
	// server completes one room-wide loss transaction before removing the
	// battle and needs the same collected-item snapshot as an ordinary
	// all-player death settlement.

	departure := AdventureDeparture{Participant: participant, Reason: reason}
	var livingCount int
	for remainingID, remaining := range battle.participants {
		departure.Remaining = append(departure.Remaining, remaining)
		if battle.alive[remainingID] {
			livingCount++
		}
	}
	sort.Slice(departure.Remaining, func(i, j int) bool {
		return departure.Remaining[i].PlayerID < departure.Remaining[j].PlayerID
	})
	if battle.concluded {
		departure.Concluded = true
		return departure, nil
	}
	if livingCount == 0 {
		battle.concluded = true
		battle.arbitratorID = 0
		departure.Concluded = true
		departure.NewlyConcluded = true
		return departure, nil
	}
	selected, err := SelectAdventureArbitrator(battle.arbitratorID, departure.Remaining)
	if err != nil {
		return AdventureDeparture{}, err
	}
	battle.arbitratorID = selected
	departure.ArbitratorPlayerID = selected
	return departure, nil
}

func (battle *AdventureBattle) resolution(newlyConcluded bool) AdventureResolution {
	participants := make([]AdventureParticipant, 0, len(battle.participants))
	for _, participant := range battle.participants {
		participants = append(participants, participant)
	}
	sort.Slice(participants, func(i, j int) bool { return participants[i].PlayerID < participants[j].PlayerID })
	return AdventureResolution{
		Concluded: battle.concluded, NewlyConcluded: newlyConcluded,
		Participants: participants,
	}
}
