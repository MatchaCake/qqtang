package probe

import (
	"context"
	"fmt"
	"math"

	roomstate "qqtang/internal/game/room"
	"qqtang/internal/protocol/game"
	"qqtang/internal/server/application"
)

func (server *Server) startSessionRoomMatch(session *connectionSession) error {
	state, err := server.sessionRoom(session)
	if err != nil {
		return err
	}
	options, err := server.sessionRoomStartValidationOptions(session, state)
	if err != nil {
		return err
	}
	return server.startSessionRoomMatchWithOptions(session, state, options)
}

func (server *Server) startSessionRoomMatchWithOptions(session *connectionSession, state *roomstate.State, options roomstate.StartValidationOptions) error {
	if session == nil || state == nil {
		return fmt.Errorf("cannot start a room match without session state")
	}
	if err := state.ValidateStartWithOptions(session.Profile.PlayerID, options); err != nil {
		return err
	}
	if _, err := server.worldState().StartMatchWithIDOptions(sessionWorldUIN(session), session.CurrentGameID, options); err != nil {
		return err
	}
	return nil
}

// validateSessionRoomStart owns the account-backed admission checks that the
// pure room state cannot perform. Item 99 is the original client entitlement;
// item 30098 is the local competitive Boss counterpart. Neither is consumed.
func (server *Server) validateSessionRoomStart(session *connectionSession, state *roomstate.State) error {
	if state != nil && session != nil {
		snapshot := state.Snapshot()
		category, categoryErr := snapshot.Settings.GameType.Category()
		if categoryErr != nil {
			return categoryErr
		}
		hasBossCard := profileOwnsActiveUncollectedItem(session.Profile, game.SinglePlayerBossCardItemID)
		hasAICard := server.config.CompetitiveAI.Enabled && profileOwnsActiveUncollectedItem(session.Profile, game.CompetitiveAICardItemID)
		if category == roomstate.CategoryCompetitive && (hasBossCard || hasAICard) {
			eligible, eligibilityErr := server.competitiveControlCardAdmissionEligible(snapshot)
			if eligibilityErr != nil {
				return eligibilityErr
			}
			if eligible {
				// Preparation resolves Boss qualification first, AI fill second and
				// ordinary room topology last. Keep phase/readiness authoritative here
				// without rejecting a roster that one of those two overlays will replace.
				return state.ValidateStartWithOptions(session.Profile.PlayerID, roomstate.StartValidationOptions{DeferCompetitiveTopology: true})
			}
		}
	}
	options, err := server.sessionRoomStartValidationOptions(session, state)
	if err != nil {
		return err
	}
	return state.ValidateStartWithOptions(session.Profile.PlayerID, options)
}

func (server *Server) competitiveControlCardAdmissionEligible(snapshot roomstate.Snapshot) (bool, error) {
	switch snapshot.Settings.Map.Kind {
	case roomstate.MapSelectionRandom:
		// Random competitive selection is resolved exclusively through
		// RandomEligibleOrdinaryCompetitive.
		return true, nil
	case roomstate.MapSelectionFixed:
		if server.mapCatalog == nil {
			return false, fmt.Errorf("competitive room %d has no map catalog", snapshot.RoomID)
		}
		selected, ok := server.mapCatalog.CompetitiveMap(snapshot.Settings.Map.MapID)
		if !ok {
			return false, fmt.Errorf("fixed competitive map %d is not installed or selectable", snapshot.Settings.Map.MapID)
		}
		return competitiveMapAllowsControlCardAdmission(selected), nil
	default:
		return false, fmt.Errorf("unknown competitive map selection kind %d", snapshot.Settings.Map.Kind)
	}
}

func (server *Server) sessionRoomStartValidationOptions(session *connectionSession, state *roomstate.State) (roomstate.StartValidationOptions, error) {
	if session == nil || state == nil {
		return roomstate.StartValidationOptions{}, fmt.Errorf("cannot validate a room start without session state")
	}
	snapshot := state.Snapshot()
	category, err := snapshot.Settings.GameType.Category()
	if err != nil {
		return roomstate.StartValidationOptions{}, err
	}
	if len(snapshot.Members) != 1 {
		return roomstate.StartValidationOptions{}, nil
	}
	switch category {
	case roomstate.CategoryAdventure:
		if !profileOwnsActiveItem(session.Profile, game.SinglePlayerAdventureCardItemID) {
			return roomstate.StartValidationOptions{}, fmt.Errorf("single-player adventure room %d requires item %d", snapshot.RoomID, game.SinglePlayerAdventureCardItemID)
		}
		return roomstate.StartValidationOptions{}, nil
	case roomstate.CategoryCompetitive:
		if !profileOwnsActiveUncollectedItem(session.Profile, game.SinglePlayerBossCardItemID) {
			return roomstate.StartValidationOptions{}, fmt.Errorf("single-player competitive Boss room %d requires item %d", snapshot.RoomID, game.SinglePlayerBossCardItemID)
		}
		return roomstate.StartValidationOptions{AllowSinglePlayerCompetitive: true}, nil
	default:
		return roomstate.StartValidationOptions{}, nil
	}
}

// completeAdventureMatch disposes the short-lived battle while retaining the
// long-lived room, owner, members, and selected next-match configuration.
func (server *Server) completeAdventureMatch(session *connectionSession, connectionID string, settlement *adventureSettlementCommit) error {
	roomID := sessionRoomActorID(session)
	if roomID == 0 {
		return fmt.Errorf("cannot complete adventure without an active room")
	}
	return runRoomActor(server, roomID, "complete-adventure-match", func() error {
		return server.completeAdventureMatchOnRoomActor(session, connectionID, settlement)
	})
}

func (server *Server) completeAdventureMatchOnRoomActor(session *connectionSession, connectionID string, settlement *adventureSettlementCommit) error {
	if session == nil {
		return fmt.Errorf("cannot complete adventure without an active game")
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.CurrentGameID == 0 {
		return fmt.Errorf("cannot complete adventure without an active game")
	}
	gameID := session.CurrentGameID
	if _, err := server.worldState().BeginSettlement(sessionWorldUIN(session), gameID); err != nil {
		return err
	}
	if settlement != nil {
		if err := server.commitAdventureSettlement(session, *settlement, connectionID); err != nil {
			return err
		}
	}
	if _, err := server.worldState().FinishSettlement(sessionWorldUIN(session), gameID); err != nil {
		return err
	}
	server.broadcastRoomLobbyMutation(session.Profile.SectionID, session.RoomID, false, "qqt_room_lobby_push_adventure_settled")
	server.removeAdventureBattle(gameID)
	session.CurrentGameID = 0
	session.CurrentStageGameID = 0
	session.CurrentMapID = 0
	server.log(logEvent{
		Level: "info", Event: "adventure_match_completed_room_retained", ConnectionID: connectionID,
		RoomID: fmt.Sprint(session.RoomID), Result: fmt.Sprintf("game_%d_room_preparing", gameID),
	})
	return nil
}

// completeAdventureRoomMatch commits one authoritative GAME_OVER for every
// connected participant before returning the retained room to preparing.
// Each account receives only its own progression and collected items, while
// all clients consume the same result table.
func (server *Server) completeAdventureRoomMatch(actor *connectionSession, connectionID string, commit adventureRoomSettlementCommit) error {
	roomID := sessionRoomActorID(actor)
	if roomID == 0 {
		return fmt.Errorf("cannot complete adventure room without an active room")
	}
	return runRoomActor(server, roomID, "complete-adventure-room-match", func() error {
		return server.completeAdventureRoomMatchOnRoomActor(actor, connectionID, commit)
	})
}

func (server *Server) completeAdventureRoomMatchOnRoomActor(actor *connectionSession, connectionID string, commit adventureRoomSettlementCommit) error {
	if actor == nil || commit.Battle == nil {
		return fmt.Errorf("cannot complete adventure room without an active game and battle")
	}
	actor.mu.Lock()
	gameID, roomID, actorUIN, sectionID := actor.CurrentGameID, actor.RoomID, sessionWorldUIN(actor), actor.Profile.SectionID
	actor.mu.Unlock()
	if gameID == 0 || roomID == 0 {
		return fmt.Errorf("cannot complete adventure room without an active game and room")
	}
	members := server.liveRoomMatchSessions(roomID, gameID)
	if len(members) == 0 {
		members = []*connectionSession{actor}
	}
	prepared := make([]preparedAdventureRoomSettlement, 0, len(members))
	for _, member := range members {
		member.mu.Lock()
		settlement, settlementErr := adventureSettlementForPlayer(commit.GameOver, member.Profile.PlayerID)
		if settlementErr == nil {
			settlementErr = attachAdventureCollectedItems(commit.Battle, member.Profile.PlayerID, &settlement)
		}
		if settlementErr != nil {
			member.mu.Unlock()
			return settlementErr
		}
		var item preparedAdventureRoomSettlement
		if settlementErr == nil {
			item, settlementErr = server.prepareAdventureRoomSettlement(member, settlement)
		}
		member.mu.Unlock()
		if settlementErr != nil {
			return settlementErr
		}
		prepared = append(prepared, item)
	}
	if _, err := server.worldState().BeginSettlement(actorUIN, gameID); err != nil {
		return err
	}
	persistErr := server.persistAdventureRoomSettlements(prepared)
	if persistErr == nil {
		server.applyAdventureRoomSettlementProfiles(prepared, nil)
	}
	if _, err := server.worldState().FinishSettlement(actorUIN, gameID); err != nil {
		if persistErr != nil {
			return fmt.Errorf("persist adventure room settlement: %v; finish room settlement: %w", persistErr, err)
		}
		return err
	}
	server.removeAdventureBattle(gameID)
	for _, member := range members {
		member.mu.Lock()
		member.CurrentGameID = 0
		member.CurrentStageGameID = 0
		member.CurrentMapID = 0
		member.mu.Unlock()
	}
	server.broadcastRoomLobbyMutation(sectionID, roomID, false, "qqt_room_lobby_push_adventure_settled")
	if persistErr == nil {
		for _, item := range prepared {
			template := item.session.packetTemplate()
			if !server.writeAdventureSettlementInventoryRefresh(
				item.session.connection, item.session, item.session.connectionID,
				item.session.localAddress, item.session.remoteAddress, template, &item.settlement,
			) {
				server.log(logEvent{Level: "warn", Event: "adventure_settlement_inventory_refresh_failed", ConnectionID: item.session.connectionID, RoomID: fmt.Sprint(roomID), Result: fmt.Sprintf("game_%d_player_%d", gameID, item.playerID)})
			}
		}
	}
	completionResult := fmt.Sprintf("game_%d_players_%d_room_preparing", gameID, len(prepared))
	if persistErr != nil {
		completionResult += "_persistence_failed"
	}
	server.log(logEvent{
		Level: "info", Event: "adventure_room_match_completed", ConnectionID: connectionID,
		RoomID: fmt.Sprint(roomID), Result: completionResult,
	})
	return persistErr
}

// completeCompetitiveRoomMatch persists one ordinary competitive outcome per
// participant, retains room membership, and resets readiness for the next
// round. Adventure counters and ExtPoint are never touched here.
func (server *Server) completeCompetitiveRoomMatch(actor *connectionSession, connectionID string, commit competitiveRoomSettlementCommit) error {
	roomID := sessionRoomActorID(actor)
	if roomID == 0 {
		return fmt.Errorf("cannot complete competitive room without an active room")
	}
	return runRoomActor(server, roomID, "complete-competitive-room-match", func() error {
		return server.completeCompetitiveRoomMatchOnRoomActor(actor, connectionID, commit)
	})
}

func (server *Server) completeCompetitiveRoomMatchOnRoomActor(actor *connectionSession, connectionID string, commit competitiveRoomSettlementCommit) error {
	if actor == nil || commit.Battle == nil {
		return fmt.Errorf("cannot complete competitive room without an active game and battle")
	}
	participants := commit.Battle.Participants()
	if len(commit.GameOver.Results) != len(participants) {
		return fmt.Errorf("competitive GAME_OVER has %d results for %d participants", len(commit.GameOver.Results), len(participants))
	}
	for _, participant := range participants {
		if _, err := competitiveSettlementForPlayer(commit.GameOver, participant.PlayerID); err != nil {
			return err
		}
	}
	actor.mu.Lock()
	gameID, roomID, actorUIN, sectionID := actor.CurrentGameID, actor.RoomID, sessionWorldUIN(actor), actor.Profile.SectionID
	actor.mu.Unlock()
	if gameID == 0 || roomID == 0 {
		return fmt.Errorf("cannot complete competitive room without an active game and room")
	}
	members := server.liveRoomMatchSessions(roomID, gameID)
	if len(members) == 0 {
		members = []*connectionSession{actor}
	}
	sceneRewards := commit.Battle.SceneRewards()
	collectedBossItems := commit.Battle.CollectedBossItemsForSettlement()
	prepared := make([]preparedCompetitiveRoomSettlement, 0, len(members))
	for _, member := range members {
		member.mu.Lock()
		settlement, settlementErr := competitiveSettlementForPlayer(commit.GameOver, member.Profile.PlayerID)
		if settlementErr != nil {
			member.mu.Unlock()
			return settlementErr
		}
		reward := sceneRewards[member.Profile.PlayerID]
		settlement.Points = saturatingAddUint32(settlement.Points, reward.Experience)
		settlement.MoneyReward = reward.Money
		settlement.CollectedItems = collectedBossItems[member.Profile.PlayerID]
		var item preparedCompetitiveRoomSettlement
		if settlementErr == nil {
			item, settlementErr = server.prepareCompetitiveRoomSettlement(member, settlement)
		}
		member.mu.Unlock()
		if settlementErr != nil {
			return settlementErr
		}
		prepared = append(prepared, item)
	}
	if _, err := server.worldState().BeginSettlement(actorUIN, gameID); err != nil {
		return err
	}
	persistErr := server.persistCompetitiveRoomSettlements(prepared)
	if persistErr == nil {
		server.applyCompetitiveRoomSettlementProfiles(prepared, nil)
	}
	if _, err := server.worldState().FinishSettlement(actorUIN, gameID); err != nil {
		if persistErr != nil {
			return fmt.Errorf("persist competitive room settlement: %v; finish room settlement: %w", persistErr, err)
		}
		return err
	}
	roomOwnerID := uint16(0)
	if room, active := server.worldState().Room(roomID); active {
		roomOwnerID = room.Snapshot().OwnerID
	}
	server.broadcastCompetitiveAILeaveProjections(roomID, gameID, roomOwnerID, commit.Battle.ArbitratorPlayerID())
	server.removeCompetitiveBattle(gameID)
	for _, member := range members {
		member.mu.Lock()
		member.CurrentGameID = 0
		member.CurrentStageGameID = 0
		member.CurrentMapID = 0
		member.mu.Unlock()
	}
	server.broadcastRoomLobbyMutation(sectionID, roomID, false, "qqt_room_lobby_push_competitive_settled")
	if persistErr == nil {
		server.writeCompetitiveRoomMoneyNotifications(prepared, roomID, gameID)
		for _, item := range prepared {
			template := item.session.packetTemplate()
			if !server.writeCompetitiveSettlementInventoryRefresh(
				item.session.connection, item.session, item.session.connectionID,
				item.session.localAddress, item.session.remoteAddress, template, &item.settlement,
			) {
				server.log(logEvent{Level: "warn", Event: "competitive_settlement_inventory_refresh_failed", ConnectionID: item.session.connectionID, RoomID: fmt.Sprint(roomID), Result: fmt.Sprintf("game_%d_player_%d", gameID, item.playerID)})
			}
		}
	}
	completionResult := fmt.Sprintf("game_%d_players_%d_room_preparing", gameID, len(members))
	if persistErr != nil {
		completionResult += "_persistence_failed"
	}
	server.log(logEvent{
		Level: "info", Event: "competitive_room_match_completed", ConnectionID: connectionID,
		RoomID: fmt.Sprint(roomID), Result: completionResult,
	})
	return persistErr
}

type preparedCompetitiveRoomSettlement struct {
	session      *connectionSession
	playerID     uint16
	uin          uint32
	settlement   competitiveSettlementCommit
	pointEffects competitivePointEffects
	moneyEffects petRewardEffects
	baseProfile  game.PlayerProfile
	profile      game.PlayerProfile
	progression  game.ProgressionChange
}

func (server *Server) prepareCompetitiveRoomSettlement(session *connectionSession, settlement competitiveSettlementCommit) (preparedCompetitiveRoomSettlement, error) {
	if session == nil || session.Profile.PlayerID == 0 {
		return preparedCompetitiveRoomSettlement{}, fmt.Errorf("competitive room settlement has no player session")
	}
	pointEffects, moneyEffects, err := server.applyCompetitiveSessionRewardEffects(
		session, settlement.Result, settlement.Points, settlement.MoneyReward,
	)
	if err != nil {
		return preparedCompetitiveRoomSettlement{}, err
	}
	settlement.MoneyReward = moneyEffects.FinalValue
	return preparedCompetitiveRoomSettlement{
		session: session, playerID: session.Profile.PlayerID, uin: session.UIN,
		settlement: settlement, pointEffects: pointEffects, moneyEffects: moneyEffects, baseProfile: clonePlayerProfile(session.Profile),
	}, nil
}

func (server *Server) persistCompetitiveRoomSettlements(prepared []preparedCompetitiveRoomSettlement) error {
	if len(prepared) == 0 {
		return fmt.Errorf("competitive room settlement has no prepared members")
	}
	if server.playerStore == nil {
		for index := range prepared {
			updated, progression, err := application.ProjectCompetitiveSettlement(prepared[index].baseProfile, application.CompetitiveSettlement{
				Result: prepared[index].settlement.Result, Points: prepared[index].pointEffects.FinalPoints,
				MoneyReward: prepared[index].settlement.MoneyReward, CollectedItems: prepared[index].settlement.CollectedItems,
			})
			if err != nil {
				return err
			}
			prepared[index].profile, prepared[index].progression = updated, progression
		}
		return nil
	}
	players, err := server.players()
	if err != nil {
		return err
	}
	requests := make([]application.CompetitiveSettlementRequest, 0, len(prepared))
	for _, item := range prepared {
		if item.uin == 0 {
			return fmt.Errorf("cannot persist competitive settlement for player %d without a UIN", item.playerID)
		}
		requests = append(requests, application.CompetitiveSettlementRequest{UIN: item.uin, Settlement: application.CompetitiveSettlement{
			Result: item.settlement.Result, Points: item.pointEffects.FinalPoints, MoneyReward: item.settlement.MoneyReward,
			CollectedItems: item.settlement.CollectedItems,
		}})
	}
	results, err := players.ApplyCompetitiveSettlements(context.Background(), requests)
	if err != nil {
		return err
	}
	for index := range prepared {
		result, ok := results[prepared[index].uin]
		if !ok {
			return fmt.Errorf("competitive room settlement omitted UIN %d", prepared[index].uin)
		}
		prepared[index].profile, prepared[index].progression = result.Profile, result.Progression
	}
	return nil
}

func (server *Server) applyCompetitiveRoomSettlementProfiles(prepared []preparedCompetitiveRoomSettlement, lockedActor *connectionSession) {
	for _, item := range prepared {
		if item.session != lockedActor {
			item.session.mu.Lock()
		}
		item.session.replaceProfile(item.profile)
		if item.session != lockedActor {
			item.session.mu.Unlock()
		}
		server.log(logEvent{
			Level: "info", Event: "competitive_profile_result_committed", ConnectionID: item.session.connectionID, AccountID: fmt.Sprint(item.uin),
			Result: fmt.Sprintf("outcome_%d_points_outcome_base_%d_points_pre_effect_%d_equipment_percent_%d_pet_percent_%d_pet_double_chance_%d_pet_double_applied_%t_win_multiplier_%d_final_%d_applied_%d_total_%d_degree_%d_money_base_%d_pet_percent_%d_pet_double_chance_%d_pet_double_applied_%t_money_final_%d_balance_%d", item.settlement.Result, item.settlement.OutcomeBasePoints, item.pointEffects.BasePoints, item.pointEffects.EquipmentPercent, item.pointEffects.PetPercent, item.pointEffects.PetDoubleChance, item.pointEffects.PetDoubleApplied, item.pointEffects.WinMultiplier, item.pointEffects.FinalPoints, item.progression.AppliedPoints, item.profile.GameInfo.Point, item.profile.GameInfo.Degree, item.moneyEffects.BaseValue, item.moneyEffects.Percent, item.moneyEffects.DoubleChance, item.moneyEffects.DoubleApplied, item.settlement.MoneyReward, item.profile.GameInfo.Money),
		})
	}
}

func (server *Server) writeCompetitiveRoomMoneyNotifications(prepared []preparedCompetitiveRoomSettlement, roomID uint16, gameID uint32) {
	for _, item := range prepared {
		if item.settlement.MoneyReward == 0 {
			continue
		}
		template := item.session.packetTemplate()
		if len(template) == 0 {
			server.log(logEvent{Level: "warn", Event: "competitive_money_reward_notify_skipped", ConnectionID: item.session.connectionID, RoomID: fmt.Sprint(roomID), Result: fmt.Sprintf("game_%d_player_%d", gameID, item.playerID), ErrorContext: "player has no packet template"})
			continue
		}
		moneyNotice, err := game.BuildLocalGameMoneyNotificationFromRequest(template, game.GameMoneyNotification{
			ResultID: 0, MoneyType: 0, Money: item.profile.GameInfo.Money, UIN: item.uin,
		})
		if err != nil {
			server.log(logEvent{Level: "warn", Event: "competitive_money_reward_notify_build_failed", ConnectionID: item.session.connectionID, RoomID: fmt.Sprint(roomID), Result: fmt.Sprintf("game_%d_player_%d", gameID, item.playerID), ErrorContext: err.Error()})
			continue
		}
		server.writeTCP(item.session.connection, item.session.connectionID, item.session.localAddress, item.session.remoteAddress, moneyNotice, "qqt_competitive_game_money_reward")
	}
}

type preparedAdventureRoomSettlement struct {
	session             *connectionSession
	playerID            uint16
	uin                 uint32
	settlement          adventureSettlementCommit
	baseAdventurePoints uint32
	courageEffects      petRewardEffects
	materialBonusItems  int
	baseProfile         game.PlayerProfile
	profile             game.PlayerProfile
	progression         game.ProgressionChange
}

func (server *Server) prepareAdventureRoomSettlement(session *connectionSession, settlement adventureSettlementCommit) (preparedAdventureRoomSettlement, error) {
	if session == nil || session.Profile.PlayerID == 0 {
		return preparedAdventureRoomSettlement{}, fmt.Errorf("adventure room settlement has no player session")
	}
	baseAdventurePoints := settlement.AdventurePoints
	itemIDs, petID, err := server.carriedPetEffectItemIDs(session)
	if err != nil {
		return preparedAdventureRoomSettlement{}, err
	}
	courageEffects := server.applyPetRewardEffects(session, itemIDs, petID, "勇气", settlement.AdventurePoints, 0)
	settlement.AdventurePoints = courageEffects.FinalValue
	materialBonusItems := 0
	if len(settlement.CollectedItems) != 0 {
		collected := make(map[uint32]uint32, len(settlement.CollectedItems))
		for itemID, quantity := range settlement.CollectedItems {
			finalQuantity := quantity
			if itemID <= math.MaxUint16 && server.itemKindsByID[uint16(itemID)] == "material" {
				materialEffects := server.applyPetRewardEffects(session, itemIDs, petID, "材料", quantity, itemID)
				finalQuantity = materialEffects.FinalValue
				if finalQuantity != quantity {
					materialBonusItems++
				}
			}
			collected[itemID] = finalQuantity
		}
		settlement.CollectedItems = collected
	}
	return preparedAdventureRoomSettlement{
		session: session, playerID: session.Profile.PlayerID, uin: session.UIN, settlement: settlement,
		baseAdventurePoints: baseAdventurePoints, courageEffects: courageEffects, materialBonusItems: materialBonusItems,
		baseProfile: clonePlayerProfile(session.Profile),
	}, nil
}

func (server *Server) persistAdventureRoomSettlements(prepared []preparedAdventureRoomSettlement) error {
	if len(prepared) == 0 {
		return fmt.Errorf("adventure room settlement has no prepared members")
	}
	if server.playerStore == nil {
		for index := range prepared {
			updated, progression, err := application.ProjectAdventureSettlement(prepared[index].baseProfile, application.AdventureSettlement{
				Result: prepared[index].settlement.Result, AdventurePoints: prepared[index].settlement.AdventurePoints,
				CollectedItems: prepared[index].settlement.CollectedItems,
			})
			if err != nil {
				return err
			}
			prepared[index].profile, prepared[index].progression = updated, progression
		}
		return nil
	}
	players, err := server.players()
	if err != nil {
		return err
	}
	requests := make([]application.AdventureSettlementRequest, 0, len(prepared))
	for _, item := range prepared {
		if item.uin == 0 {
			return fmt.Errorf("cannot persist adventure settlement for player %d without a UIN", item.playerID)
		}
		requests = append(requests, application.AdventureSettlementRequest{UIN: item.uin, Settlement: application.AdventureSettlement{
			Result: item.settlement.Result, AdventurePoints: item.settlement.AdventurePoints, CollectedItems: item.settlement.CollectedItems,
		}})
	}
	results, err := players.ApplyAdventureSettlements(context.Background(), requests)
	if err != nil {
		return err
	}
	for index := range prepared {
		result, ok := results[prepared[index].uin]
		if !ok {
			return fmt.Errorf("adventure room settlement omitted UIN %d", prepared[index].uin)
		}
		prepared[index].profile, prepared[index].progression = result.Profile, result.Progression
	}
	return nil
}

func (server *Server) applyAdventureRoomSettlementProfiles(prepared []preparedAdventureRoomSettlement, lockedActor *connectionSession) {
	for _, item := range prepared {
		if item.session != lockedActor {
			item.session.mu.Lock()
		}
		previousRank := game.AdventureRankForPoints(item.session.Profile.GameInfo.ExtPoint)
		item.session.replaceProfile(item.profile)
		currentRank := game.AdventureRankForPoints(item.session.Profile.GameInfo.ExtPoint)
		if item.session != lockedActor {
			item.session.mu.Unlock()
		}
		server.log(logEvent{
			Level: "info", Event: "adventure_profile_reward_committed", ConnectionID: item.session.connectionID, AccountID: fmt.Sprint(item.uin),
			Result: fmt.Sprintf("outcome_%d_courage_outcome_multiplier_percent_%d_adventure_points_pre_effect_%d_applied_%d_extended_points_%d_level_%d_to_%d_leveled_up_%t", item.settlement.Result, item.settlement.OutcomeMultiplierPercent, item.baseAdventurePoints, item.progression.AppliedPoints, item.profile.GameInfo.ExtPoint, previousRank.Level, currentRank.Level, currentRank.Level > previousRank.Level),
		})
		if item.courageEffects.Percent != 0 || item.courageEffects.DoubleChance != 0 || item.materialBonusItems != 0 {
			server.log(logEvent{
				Level: "info", Event: "adventure_pet_reward_effects", ConnectionID: item.session.connectionID, AccountID: fmt.Sprint(item.uin),
				Result: fmt.Sprintf("courage_base_%d_percent_%d_double_chance_%d_double_applied_%t_final_%d_material_items_modified_%d", item.baseAdventurePoints, item.courageEffects.Percent, item.courageEffects.DoubleChance, item.courageEffects.DoubleApplied, item.settlement.AdventurePoints, item.materialBonusItems),
			})
		}
	}
}

func (server *Server) commitCompetitiveSettlement(session *connectionSession, settlement competitiveSettlementCommit, connectionID string) error {
	if session.UIN == 0 && server.playerStore != nil {
		return fmt.Errorf("cannot persist competitive settlement without a session UIN")
	}
	pointEffects, moneyEffects, pointEffectsErr := server.applyCompetitiveSessionRewardEffects(
		session, settlement.Result, settlement.Points, settlement.MoneyReward,
	)
	if pointEffectsErr != nil {
		return pointEffectsErr
	}
	settlement.MoneyReward = moneyEffects.FinalValue
	updated, progression, err := application.ProjectCompetitiveSettlement(session.Profile, application.CompetitiveSettlement{
		Result: settlement.Result, Points: pointEffects.FinalPoints, MoneyReward: settlement.MoneyReward,
		CollectedItems: settlement.CollectedItems,
	})
	if err != nil {
		return err
	}
	if server.playerStore != nil {
		players, serviceErr := server.players()
		if serviceErr != nil {
			return serviceErr
		}
		updated, progression, err = players.ApplyCompetitiveSettlement(context.Background(), session.UIN, session.Profile, application.CompetitiveSettlement{
			Result: settlement.Result, Points: pointEffects.FinalPoints, MoneyReward: settlement.MoneyReward,
			CollectedItems: settlement.CollectedItems,
		})
		if err != nil {
			return err
		}
	}
	session.replaceProfile(updated)
	server.log(logEvent{
		Level: "info", Event: "competitive_profile_result_committed", ConnectionID: connectionID, AccountID: fmt.Sprint(session.UIN),
		Result: fmt.Sprintf("outcome_%d_points_outcome_base_%d_points_pre_effect_%d_equipment_percent_%d_pet_percent_%d_pet_double_chance_%d_pet_double_applied_%t_win_multiplier_%d_final_%d_applied_%d_total_%d_degree_%d_money_base_%d_pet_percent_%d_pet_double_chance_%d_pet_double_applied_%t_money_final_%d_balance_%d", settlement.Result, settlement.OutcomeBasePoints, pointEffects.BasePoints, pointEffects.EquipmentPercent, pointEffects.PetPercent, pointEffects.PetDoubleChance, pointEffects.PetDoubleApplied, pointEffects.WinMultiplier, pointEffects.FinalPoints, progression.AppliedPoints, updated.GameInfo.Point, updated.GameInfo.Degree, moneyEffects.BaseValue, moneyEffects.Percent, moneyEffects.DoubleChance, moneyEffects.DoubleApplied, settlement.MoneyReward, updated.GameInfo.Money),
	})
	return nil
}

func (server *Server) commitAdventureSettlement(session *connectionSession, settlement adventureSettlementCommit, connectionID string) error {
	if session.UIN == 0 && server.playerStore != nil {
		return fmt.Errorf("cannot persist adventure settlement without a session UIN")
	}
	baseAdventurePoints := settlement.AdventurePoints
	itemIDs, petID, err := server.carriedPetEffectItemIDs(session)
	if err != nil {
		return err
	}
	courageEffects := server.applyPetRewardEffects(session, itemIDs, petID, "勇气", settlement.AdventurePoints, 0)
	settlement.AdventurePoints = courageEffects.FinalValue
	materialBonusItems := 0
	if len(settlement.CollectedItems) != 0 {
		collected := make(map[uint32]uint32, len(settlement.CollectedItems))
		for itemID, quantity := range settlement.CollectedItems {
			finalQuantity := quantity
			if itemID <= math.MaxUint16 && server.itemKindsByID[uint16(itemID)] == "material" {
				materialEffects := server.applyPetRewardEffects(session, itemIDs, petID, "材料", quantity, itemID)
				finalQuantity = materialEffects.FinalValue
				if finalQuantity != quantity {
					materialBonusItems++
				}
			}
			collected[itemID] = finalQuantity
		}
		settlement.CollectedItems = collected
	}
	updated, err := projectAdventureSettlement(session.Profile, settlement)
	if err != nil {
		return err
	}
	previousRank := game.AdventureRankForPoints(session.Profile.GameInfo.ExtPoint)
	currentRank := game.AdventureRankForPoints(updated.GameInfo.ExtPoint)
	var appliedPoints uint32
	if updated.GameInfo.ExtPoint >= session.Profile.GameInfo.ExtPoint {
		appliedPoints = updated.GameInfo.ExtPoint - session.Profile.GameInfo.ExtPoint
	}
	if server.playerStore != nil {
		players, serviceErr := server.players()
		if serviceErr != nil {
			return serviceErr
		}
		var progression game.ProgressionChange
		updated, progression, err = players.ApplyAdventureSettlement(context.Background(), session.UIN, session.Profile, application.AdventureSettlement{
			Result: settlement.Result, AdventurePoints: settlement.AdventurePoints, CollectedItems: settlement.CollectedItems,
		})
		if err != nil {
			return err
		}
		appliedPoints = progression.AppliedPoints
	}
	session.replaceProfile(updated)
	server.log(logEvent{
		Level: "info", Event: "adventure_profile_reward_committed", ConnectionID: connectionID,
		AccountID: fmt.Sprint(session.UIN),
		Result: fmt.Sprintf(
			"outcome_%d_courage_outcome_multiplier_percent_%d_adventure_points_pre_effect_%d_applied_%d_extended_points_%d_level_%d_to_%d_leveled_up_%t",
			settlement.Result,
			settlement.OutcomeMultiplierPercent,
			baseAdventurePoints,
			appliedPoints,
			updated.GameInfo.ExtPoint,
			previousRank.Level,
			currentRank.Level,
			currentRank.Level > previousRank.Level,
		),
	})
	if courageEffects.Percent != 0 || courageEffects.DoubleChance != 0 || materialBonusItems != 0 {
		server.log(logEvent{
			Level: "info", Event: "adventure_pet_reward_effects", ConnectionID: connectionID,
			AccountID: fmt.Sprint(session.UIN),
			Result:    fmt.Sprintf("courage_base_%d_percent_%d_double_chance_%d_double_applied_%t_final_%d_material_items_modified_%d", baseAdventurePoints, courageEffects.Percent, courageEffects.DoubleChance, courageEffects.DoubleApplied, settlement.AdventurePoints, materialBonusItems),
		})
	}
	return nil
}

func projectAdventureSettlement(profile game.PlayerProfile, settlement adventureSettlementCommit) (game.PlayerProfile, error) {
	updated, _, err := application.ProjectAdventureSettlement(profile, application.AdventureSettlement{
		Result: settlement.Result, AdventurePoints: settlement.AdventurePoints, CollectedItems: settlement.CollectedItems,
	})
	return updated, err
}

func saturatingAddUint32(value, increment uint32) uint32 {
	return saturatingAddUint32Limit(value, increment, math.MaxUint32)
}

func saturatingAddUint32Limit(value, increment, maximum uint32) uint32 {
	if value >= maximum || increment > maximum-value {
		return maximum
	}
	return value + increment
}
