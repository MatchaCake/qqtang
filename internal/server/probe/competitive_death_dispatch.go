package probe

import (
	"fmt"
	"time"

	"qqtang/internal/game/match"
	"qqtang/internal/protocol/game"
)

const (
	competitiveNativeConclusionDelay      = 750 * time.Millisecond
	competitiveBossVictoryConclusionDelay = 10 * time.Second
)

type competitiveDeathDispatchResult struct {
	response            []byte
	result              string
	followUp            []byte
	followUpResult      string
	startFollowUp       []byte
	startFollowUpResult string
	startFollowUpDelay  time.Duration
	completeAfterSend   bool
	roomSettlement      *competitiveRoomSettlementCommit
}

func (server *Server) handleCompetitiveDeath(session *connectionSession, data []byte) (competitiveDeathDispatchResult, error) {
	var result competitiveDeathDispatchResult
	response, event, err := game.BuildLocalGameEventRelay(data)
	if err != nil {
		return result, err
	}
	result.response = response
	death, err := game.ParsePlayerDeathEvent(event)
	if err != nil {
		return competitiveDeathDispatchResult{}, err
	}
	battle, err := server.competitiveBattle(session.CurrentGameID)
	if err != nil {
		return competitiveDeathDispatchResult{}, err
	}
	battle.RecordNativeRelay(event.Schema, event.Body)
	var resolution match.CompetitiveResolution
	bossVictory := false
	if battle.HasParticipant(death.PlayerID) {
		if battle.UsesNativeDurability() {
			if !battle.AuthorizesNativeDurabilityDeathReporter(session.Profile.PlayerID, death.PlayerID) {
				return competitiveDeathDispatchResult{}, fmt.Errorf("native-durability death source player %d cannot report player %d", session.Profile.PlayerID, death.PlayerID)
			}
			var authoritative bool
			resolution, authoritative, err = battle.RecordNativeDurabilityDeath(death.PlayerID, death.ClientTime, death.PosX, death.PosY)
			if err != nil {
				return competitiveDeathDispatchResult{}, err
			}
			result.result = fmt.Sprintf("qqt_game_event_ack_native_durability_death_%d_authoritative_%t", death.PlayerID, authoritative)
			if !authoritative {
				return result, nil
			}
		} else {
			itemIDs := make([]uint32, 0, len(death.Items))
			for _, item := range death.Items {
				itemIDs = append(itemIDs, item.ItemID)
			}
			if err = battle.RecordTreasureScatter(death.PlayerID, itemIDs); err != nil {
				return competitiveDeathDispatchResult{}, err
			}
			var graceDeath, allDead bool
			graceDeath, allDead, err = battle.RecordBossVictoryGraceDeath(death.PlayerID)
			if err != nil {
				return competitiveDeathDispatchResult{}, err
			}
			if graceDeath {
				result.result = fmt.Sprintf("qqt_game_event_ack_competitive_boss_victory_grace_death_%d_all_dead_%t", death.PlayerID, allDead)
				return result, nil
			}
			resolution, err = battle.RecordDeath(death.PlayerID)
			result.result = fmt.Sprintf("qqt_game_event_ack_competitive_player_death_%d", death.PlayerID)
			result.followUpResult = fmt.Sprintf("qqt_game_event_notify_competitive_player_death_%d", death.PlayerID)
		}
	} else if battle.HasBossEntity(death.PlayerID) {
		itemIDs := make([]uint32, 0, len(death.Items))
		for _, item := range death.Items {
			itemIDs = append(itemIDs, item.ItemID)
		}
		if err = battle.RecordBossDeathDrops(death.PlayerID, itemIDs); err != nil {
			return competitiveDeathDispatchResult{}, err
		}
		resolution, err = battle.RecordBossDeath(death.PlayerID)
		bossVictory = resolution.NewlyConcluded
		result.result = fmt.Sprintf("qqt_game_event_ack_competitive_boss_death_%d", death.PlayerID)
		result.followUpResult = fmt.Sprintf("qqt_game_event_notify_competitive_boss_death_%d", death.PlayerID)
	} else if battle.HasNativeNPCEntity(death.PlayerID) {
		if !battle.IsArbitrator(session.Profile.PlayerID) {
			return competitiveDeathDispatchResult{}, fmt.Errorf("native NPC death source player %d is not the match arbitrator", session.Profile.PlayerID)
		}
		var fresh bool
		fresh, err = battle.RecordNativeNPCDeath(death.PlayerID)
		result.result = fmt.Sprintf("qqt_game_event_ack_native_npc_death_%d_fresh_%t", death.PlayerID, fresh)
	} else {
		return competitiveDeathDispatchResult{}, fmt.Errorf("competitive death object %d is neither a room participant nor registered native entity", death.PlayerID)
	}
	if err != nil {
		return competitiveDeathDispatchResult{}, err
	}
	// 0x0FA7 is already present in QQTPPP's native Type-2 multicast. The
	// reliable copy is consumed for authoritative durability/death accounting;
	// another TCP peer notification would apply the same death scene twice.
	if !resolution.NewlyConcluded {
		return result, nil
	}
	gameOver := competitiveGameOverData(death.ClientTime, resolution)
	result.startFollowUp, err = game.BuildLocalGameOverNotifyFromDeath(data, session.RoomID, server.nextGameDataSequence(), gameOver)
	if err != nil {
		return competitiveDeathDispatchResult{}, err
	}
	result.startFollowUpResult = fmt.Sprintf("qqt_competitive_game_over_winner_team_%d", resolution.WinnerTeamID)
	result.startFollowUpDelay = competitiveNativeConclusionDelay
	if bossVictory {
		result.startFollowUpDelay = competitiveBossVictoryConclusionDelay
	}
	result.completeAfterSend = true
	result.roomSettlement = &competitiveRoomSettlementCommit{GameOver: gameOver, Battle: battle}
	return result, nil
}

func competitiveGameOverData(clientTime uint32, resolution match.CompetitiveResolution) game.GameOverData {
	results := make([]game.GameResultData, 0, len(resolution.Results))
	for _, player := range resolution.Results {
		outcome := game.GameResultLoss
		if player.Draw {
			outcome = game.GameResultDraw
		} else if player.Won {
			outcome = game.GameResultWin
		}
		results = append(results, game.GameResultData{
			PlayerID: player.Participant.PlayerID, Result: outcome,
		})
		applyCompetitivePlayerStatistics(&results[len(results)-1], match.CompetitivePlayerStatistics{
			KillCount: player.KillCount, RescueCount: player.RescueCount,
		})
	}
	data := game.GameOverData{Time: clientTime, Results: results, GameMode: game.SettlementGameModeCompetitive}
	addCompetitiveOutcomeBasePoints(&data)
	return data
}

func (server *Server) competitiveKillConclusion(session *connectionSession, trigger []byte, clientTime uint32, battle *match.CompetitiveBattle, resolution match.CompetitiveResolution) ([]byte, string, *competitiveRoomSettlementCommit, error) {
	if !resolution.NewlyConcluded {
		return nil, "", nil, nil
	}
	gameOver := competitiveGameOverData(clientTime, resolution)
	packet, err := game.BuildLocalGameOverNotifyFromEvent(trigger, session.RoomID, server.nextGameDataSequence(), gameOver)
	if err != nil {
		return nil, "", nil, err
	}
	return packet, fmt.Sprintf("qqt_competitive_game_over_winner_team_%d", resolution.WinnerTeamID), &competitiveRoomSettlementCommit{
		GameOver: gameOver,
		Battle:   battle,
	}, nil
}

type competitiveDepartureTransition struct {
	ArbitratorPlayerID  uint16
	Battle              *match.CompetitiveBattle
	GameOver            *game.GameOverData
	DepartingSettlement *competitiveSettlementCommit
}

// handleCompetitiveDeparture updates only the battle model.  Room removal
// and NOTIFY_LEAVE_ROOM are projected by the caller before any departure-
// caused GAME_OVER or personal loss settlement is emitted.
func (server *Server) handleCompetitiveDeparture(session *connectionSession, connectionID string) competitiveDepartureTransition {
	if session == nil || session.CurrentGameID == 0 || session.Profile.PlayerID == 0 {
		return competitiveDepartureTransition{}
	}
	battle, err := server.competitiveBattle(session.CurrentGameID)
	if err != nil {
		return competitiveDepartureTransition{}
	}
	// A GAME_OVER producer marks the battle concluded before its ordered TCP
	// sequence is delivered. A player leaving during that gap keeps the chosen
	// outcome and receives an individual settlement because room-wide delivery
	// will no longer include the departed session. In particular, Boss victory
	// remains a win throughout its ten-second presentation grace.
	if resolution, concluded := battle.ConcludedResolution(); concluded {
		if _, _, graceErr := battle.RecordBossVictoryGraceDeparture(session.Profile.PlayerID); graceErr != nil {
			server.log(logEvent{Level: "warn", Event: "competitive_boss_victory_grace_departure_rejected", ConnectionID: connectionID, ErrorContext: graceErr.Error()})
		}
		if current, stillConcluded := battle.ConcludedResolution(); stillConcluded {
			resolution = current
		}
		gameOver := competitiveGameOverData(battle.ElapsedMilliseconds(), resolution)
		settlement, settlementErr := competitiveSettlementForPlayer(gameOver, session.Profile.PlayerID)
		if settlementErr != nil {
			server.log(logEvent{Level: "error", Event: "competitive_concluded_departure_result_failed", ConnectionID: connectionID, ErrorContext: settlementErr.Error()})
			return competitiveDepartureTransition{}
		}
		if reward, ok := battle.SceneRewards()[session.Profile.PlayerID]; ok {
			settlement.Points = saturatingAddUint32(settlement.Points, reward.Experience)
			settlement.MoneyReward = reward.Money
		}
		settlement.CollectedItems = battle.CollectedBossItemsForSettlement()[session.Profile.PlayerID]
		return competitiveDepartureTransition{DepartingSettlement: &settlement}
	}
	resolution, err := battle.RecordDeparture(session.Profile.PlayerID)
	if err != nil {
		server.log(logEvent{Level: "warn", Event: "competitive_departure_rejected", ConnectionID: connectionID, ErrorContext: err.Error()})
		return competitiveDepartureTransition{}
	}
	if runtime := server.competitiveAIRuntimeForGame(session.CurrentGameID); runtime != nil {
		server.recordCompetitiveAIHumanDepartureOnRoomActor(runtime, session.Profile.PlayerID)
	}
	transition := competitiveDepartureTransition{ArbitratorPlayerID: resolution.ArbitratorPlayerID}
	if !resolution.NewlyConcluded {
		settlement := competitiveSettlementCommit{Result: game.GameResultLoss, CollectedItems: battle.CollectedBossItemsForSettlement()[session.Profile.PlayerID]}
		transition.DepartingSettlement = &settlement
		return transition
	}
	gameOver := competitiveGameOverData(battle.ElapsedMilliseconds(), resolution)
	settlement, err := competitiveSettlementForPlayer(gameOver, session.Profile.PlayerID)
	if err != nil {
		server.log(logEvent{Level: "error", Event: "competitive_departure_result_failed", ConnectionID: connectionID, ErrorContext: err.Error()})
		return transition
	}
	if reward, ok := battle.SceneRewards()[session.Profile.PlayerID]; ok {
		settlement.Points = saturatingAddUint32(settlement.Points, reward.Experience)
		settlement.MoneyReward = reward.Money
	}
	settlement.CollectedItems = battle.CollectedBossItemsForSettlement()[session.Profile.PlayerID]
	transition.Battle = battle
	transition.GameOver = &gameOver
	transition.DepartingSettlement = &settlement
	return transition
}

func (server *Server) finishCompetitiveDeparture(session *connectionSession, connectionID string, roomID uint16, gameID uint32, transition competitiveDepartureTransition) {
	if session == nil {
		return
	}
	if transition.DepartingSettlement != nil {
		if err := server.commitCompetitiveSettlement(session, *transition.DepartingSettlement, connectionID); err != nil {
			server.log(logEvent{Level: "error", Event: "competitive_departure_settlement_failed", ConnectionID: connectionID, ErrorContext: err.Error()})
		}
	}
	if transition.Battle == nil || transition.GameOver == nil {
		return
	}
	server.broadcastRoomNotification(roomID, session.UIN, "qqt_competitive_game_over_departure_peer", func(recipientPacket []byte) ([]byte, error) {
		return game.BuildLocalGameOverPushForRecipient(recipientPacket, roomID, server.nextGameDataSequence(), *transition.GameOver)
	})
	members := server.liveRoomMatchSessions(roomID, gameID)
	if len(members) == 0 {
		server.removeCompetitiveBattle(gameID)
		return
	}
	if err := server.completeCompetitiveRoomMatchOnRoomActor(members[0], connectionID, competitiveRoomSettlementCommit{GameOver: *transition.GameOver, Battle: transition.Battle}); err != nil {
		server.log(logEvent{Level: "error", Event: "competitive_departure_completion_failed", ConnectionID: connectionID, ErrorContext: err.Error()})
	}
}
