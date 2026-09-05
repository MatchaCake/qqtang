package probe

import (
	"fmt"
	"sort"
	"time"

	"qqtang/internal/game/match"
	"qqtang/internal/protocol/game"
)

type adventureNextMapResult struct {
	response                 []byte
	result                   string
	followUp                 []byte
	followUpResult           string
	startFollowUp            []byte
	startFollowUpResult      string
	startFollowUpDelay       time.Duration
	completeAfterSend        bool
	settlement               *adventureSettlementCommit
	roomSettlement           *adventureRoomSettlementCommit
	followUpPeerPlayers      map[uint16]struct{}
	startFollowUpPeerPlayers map[uint16]struct{}
	afterResponse            func()
}

func (server *Server) handleAdventureNextMap(session *connectionSession, data []byte) (adventureNextMapResult, error) {
	result := adventureNextMapResult{startFollowUpResult: "qqt_second_followup"}
	response, nextRequest, err := game.BuildLocalGameNextMapSuccess(data)
	result.response = response
	currentMap, currentExists := server.mapCatalog.Map(session.CurrentMapID)
	nextMap, hasNext := server.mapCatalog.Next(session.CurrentMapID)
	validSequence := currentExists && (nextRequest.ContinueID == uint32(currentMap.SequenceID) || nextRequest.ContinueID == game.NoRemoteContinueFileIDWire)
	validTarget := adventureNextMapTargetMatches(nextRequest.NextMapID, session.CurrentMapID, nextMap.ID, hasNext)
	if err == nil && (!currentExists || nextRequest.PlayerID != session.Profile.PlayerID || !validSequence || !validTarget) {
		err = fmt.Errorf("invalid next-map request player=%d continue=%d next_map=%d current_map=%d", nextRequest.PlayerID, nextRequest.ContinueID, nextRequest.NextMapID, session.CurrentMapID)
	}
	var transition match.AdventureStageTransition
	var battle *match.AdventureBattle
	if err == nil {
		battle, err = server.adventureBattle(session.CurrentGameID)
		if err == nil {
			transition, err = battle.PrepareNextStage(nextRequest.PlayerID)
		}
	}
	if err != nil {
		return result, err
	}
	if !hasNext {
		// Reaching the final doorway is a cooperative team victory. A teammate
		// who died during this final stage is still part of the result and wins;
		// only players settled out during an earlier transition are absent from
		// the current battle.
		participants := adventureFinalStageParticipants(transition)
		results := make([]game.GameResultData, 0, len(participants))
		for _, participant := range participants {
			statistics, statisticsErr := battle.PlayerStatistics(participant.PlayerID)
			if statisticsErr != nil {
				return result, statisticsErr
			}
			results = append(results, game.GameResultData{
				PlayerID: participant.PlayerID, Result: game.GameResultWin,
				Fields: (game.AdventureResultStatistics{
					KillCount: statistics.KillCount, KillScore: statistics.KillScore,
					RescueCount: statistics.RescueCount, RescueScore: statistics.RescueScore,
					RewardCount: statistics.RewardCount, RewardScore: statistics.RewardScore,
				}).GameResultFields(),
			})
		}
		gameOver := game.GameOverData{Time: nextRequest.ClientTime, Results: results, GameMode: game.SettlementGameModeAdventure}
		applyAdventureOutcomeCourageMultiplier(&gameOver)
		result.followUp, err = game.BuildLocalGameOverNotifyFromEvent(data, session.RoomID, server.nextGameDataSequence(), gameOver)
		if err != nil {
			return result, err
		}
		result.result = "qqt_adventure_final_doorway_success"
		result.followUpResult = "qqt_adventure_game_over_all_survivors_win"
		result.completeAfterSend = true
		settlement, settlementErr := adventureSettlementForPlayer(gameOver, session.Profile.PlayerID)
		if settlementErr == nil {
			settlementErr = attachAdventureCollectedItems(battle, session.Profile.PlayerID, &settlement)
		}
		if settlementErr != nil {
			return result, settlementErr
		}
		result.settlement = &settlement
		result.roomSettlement = &adventureRoomSettlementCommit{GameOver: gameOver, Battle: battle}
		return result, nil
	}

	// The native client snapshots the previous wire GameID while applying
	// NOTIFY_GAME_NEXTMAP and ignores peer 0x0FBD packages until the incoming
	// stage carries a different identity. Do not reuse the stable match ID.
	nextStageGameID := server.worldState().AllocateGameID()
	nextData, err := server.newAdventureGameData(session, nextStageGameID, nextMap, transition.ArbitratorPlayerID, transition.Survivors)
	if err != nil {
		return result, err
	}
	result.followUp, err = game.BuildLocalGameNextMapNotify(data, session.RoomID, server.nextGameDataSequence(), nextData)
	if err != nil {
		return result, err
	}
	nextRule, hasNextRule := server.adventureStageRule(nextMap.ID)
	if hasNextRule {
		result.startFollowUp, err = game.BuildLocalCreatePVENPCBossNotify(data, session.RoomID, server.nextGameDataSequence(), adventurePVEBossData(nextRule, nextData.ItemSeed))
		if err != nil {
			return result, err
		}
		result.startFollowUpResult = fmt.Sprintf("qqt_adventure_create_pve_npc_boss_map_%d_groups_%d", nextMap.ID, len(nextRule.NPCDropGroups))
		result.startFollowUpDelay = 50 * time.Millisecond
	}
	eliminationPlan, err := server.prepareAdventureStageEliminations(data, session.RoomID, session.CurrentGameID, nextRequest.ClientTime, transition.ArbitratorPlayerID, battle, transition.SettledOut)
	if err != nil {
		return result, err
	}
	if err = battle.AdvanceStageWithRule(nextMap.ID, nextData.ItemSeed, nextRule.ExpectedNPCDeaths, transition.ArbitratorPlayerID, transition.Survivors); err != nil {
		return result, err
	}
	result.followUpResult = fmt.Sprintf("qqt_adventure_next_map_%d", nextMap.ID)
	result.followUpPeerPlayers = adventureParticipantIDs(transition.Survivors)
	result.startFollowUpPeerPlayers = result.followUpPeerPlayers
	server.projectRoomAdventureStageFromLockedSession(session, session.RoomID, session.CurrentGameID, nextStageGameID, nextMap.ID, result.followUpPeerPlayers)
	if len(eliminationPlan.players) != 0 {
		result.afterResponse = func() {
			applyErr := runRoomActor(server, eliminationPlan.roomID, "adventure-stage-elimination", func() error {
				return server.applyAdventureStageEliminations(eliminationPlan)
			})
			if applyErr != nil {
				server.log(logEvent{
					Level: "error", Event: "adventure_stage_elimination_failed", ConnectionID: session.connectionID,
					AccountID: fmt.Sprint(session.UIN), RoomID: fmt.Sprint(eliminationPlan.roomID),
					Result: fmt.Sprintf("game_%d_players_%d", eliminationPlan.gameID, len(eliminationPlan.players)), ErrorContext: applyErr.Error(),
				})
			}
		}
	}
	return result, nil
}

func adventureFinalStageParticipants(transition match.AdventureStageTransition) []match.AdventureParticipant {
	participants := make([]match.AdventureParticipant, 0, len(transition.Survivors)+len(transition.SettledOut))
	participants = append(participants, transition.Survivors...)
	for _, settled := range transition.SettledOut {
		participants = append(participants, settled.Participant)
	}
	sort.Slice(participants, func(i, j int) bool { return participants[i].PlayerID < participants[j].PlayerID })
	return participants
}
