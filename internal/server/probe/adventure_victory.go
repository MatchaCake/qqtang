package probe

import (
	"fmt"
	"strconv"
	"time"

	"qqtang/internal/game/match"
	"qqtang/internal/protocol/game"
)

const adventureFinalLootGrace = 10 * time.Second

type adventureFinalVictorySchedule struct {
	gameID     uint32
	roomID     uint16
	clientTime uint32
	battle     *match.AdventureBattle
}

func adventureFinalGameOverData(battle *match.AdventureBattle, resolution match.AdventureFinalResolution, clientTime uint32) (game.GameOverData, error) {
	results := make([]game.GameResultData, 0, len(resolution.Players))
	for _, playerResult := range resolution.Players {
		statistics, err := battle.PlayerStatistics(playerResult.Participant.PlayerID)
		if err != nil {
			return game.GameOverData{}, err
		}
		outcome := game.GameResultLoss
		if playerResult.Won {
			outcome = game.GameResultWin
		}
		results = append(results, game.GameResultData{
			PlayerID: playerResult.Participant.PlayerID,
			Result:   outcome,
			Fields: (game.AdventureResultStatistics{
				KillCount: statistics.KillCount, KillScore: statistics.KillScore,
				RescueCount: statistics.RescueCount, RescueScore: statistics.RescueScore,
				RewardCount: statistics.RewardCount, RewardScore: statistics.RewardScore,
			}).GameResultFields(),
		})
	}
	data := game.GameOverData{Time: clientTime, Results: results, GameMode: game.SettlementGameModeAdventure}
	applyAdventureOutcomeCourageMultiplier(&data)
	return data, nil
}

func (server *Server) scheduleAdventureFinalVictory(schedule adventureFinalVictorySchedule) {
	server.log(logEvent{
		Level: "info", Event: "adventure_final_loot_grace_started",
		RoomID: strconv.FormatUint(uint64(schedule.roomID), 10),
		Result: "game_" + strconv.FormatUint(uint64(schedule.gameID), 10) + "_grace_" + adventureFinalLootGrace.String(),
	})
	server.runAfter(adventureFinalLootGrace, func() {
		server.completeAdventureFinalVictory(schedule)
	})
}

// completeAdventureFinalVictory resolves the room from the current live
// membership rather than from the connection that happened to trigger the
// final objective. The grace timer therefore survives host/arbitrator
// migration and a closed trigger socket.
func (server *Server) completeAdventureFinalVictory(schedule adventureFinalVictorySchedule) {
	err := runRoomActor(server, schedule.roomID, "adventure-final-victory", func() error {
		server.completeAdventureFinalVictoryOnRoomActor(schedule)
		return nil
	})
	if err != nil {
		server.log(logEvent{Level: "error", Event: "adventure_final_victory_actor_failed", RoomID: fmt.Sprint(schedule.roomID), Result: fmt.Sprintf("game_%d", schedule.gameID), ErrorContext: err.Error()})
	}
}

func (server *Server) completeAdventureFinalVictoryOnRoomActor(schedule adventureFinalVictorySchedule) {
	members := server.liveRoomMatchSessions(schedule.roomID, schedule.gameID)
	if len(members) == 0 {
		server.removeAdventureBattle(schedule.gameID)
		return
	}
	resolution, err := schedule.battle.FinalizePendingVictory()
	if err != nil {
		server.log(logEvent{Level: "error", Event: "adventure_final_victory_finalize_failed", RoomID: fmt.Sprint(schedule.roomID), ErrorContext: err.Error()})
		return
	}
	if !resolution.NewlyConcluded {
		return
	}
	gameOver, err := adventureFinalGameOverData(schedule.battle, resolution, schedule.clientTime)
	if err != nil {
		server.log(logEvent{Level: "error", Event: "adventure_final_victory_result_failed", RoomID: fmt.Sprint(schedule.roomID), ErrorContext: err.Error()})
		return
	}
	for _, member := range members {
		template := member.packetTemplate()
		if len(template) == 0 {
			continue
		}
		notification, buildErr := game.BuildLocalGameOverPushForRecipient(template, schedule.roomID, server.nextGameDataSequence(), gameOver)
		if buildErr != nil {
			server.log(logEvent{Level: "error", Event: "adventure_final_victory_encode_failed", ConnectionID: member.connectionID, RoomID: fmt.Sprint(schedule.roomID), ErrorContext: buildErr.Error()})
			continue
		}
		server.writeTCP(member.connection, member.connectionID, member.localAddress, member.remoteAddress, notification, "qqt_adventure_game_over_final_loot_grace_expired")
	}
	var completionErr error
	for _, actor := range members {
		completionErr = server.completeAdventureRoomMatchOnRoomActor(actor, actor.connectionID, adventureRoomSettlementCommit{GameOver: gameOver, Battle: schedule.battle})
		if completionErr == nil {
			return
		}
		if _, activeErr := server.adventureBattle(schedule.gameID); activeErr != nil {
			server.log(logEvent{Level: "error", Event: "adventure_match_completion_failed", ConnectionID: actor.connectionID, RoomID: fmt.Sprint(schedule.roomID), Result: "qqt_adventure_game_over_final_loot_grace_expired_after_cleanup", ErrorContext: completionErr.Error()})
			return
		}
	}
	server.log(logEvent{Level: "error", Event: "adventure_match_completion_failed", RoomID: fmt.Sprint(schedule.roomID), Result: "qqt_adventure_game_over_final_loot_grace_expired", ErrorContext: completionErr.Error()})
}
