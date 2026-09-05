package probe

import (
	"fmt"
	"time"

	"qqtang/internal/game/match"
	roomstate "qqtang/internal/game/room"
	"qqtang/internal/protocol/game"
)

// Captured native clients keep the personal GAME_OVER result scene visible for
// about six seconds before returning to room navigation.  A between-stage
// eliminated client does not always emit REQUEST_LEAVE_ROOM at that boundary,
// so close near the end of the visible result window, with enough margin to
// prevent the removed room from flashing back on screen first.
const adventureStageResultSceneGrace = 5 * time.Second

func adventureParticipantIDs(participants []match.AdventureParticipant) map[uint16]struct{} {
	ids := make(map[uint16]struct{}, len(participants))
	for _, participant := range participants {
		ids[participant.PlayerID] = struct{}{}
	}
	return ids
}

func (server *Server) liveRoomSessionForPlayer(roomID, playerID uint16) *connectionSession {
	server.liveMu.RLock()
	for _, session := range server.liveSessions {
		if session.liveRoomID.Load() == uint32(roomID) && session.liveUIN.Load() != 0 && session.livePlayerID.Load() == uint32(playerID) {
			server.liveMu.RUnlock()
			return session
		}
	}
	server.liveMu.RUnlock()
	return nil
}

type preparedAdventureStageElimination struct {
	participant  match.AdventureSettlement
	session      *connectionSession
	sectionID    uint16
	template     []byte
	notification []byte
	settlement   adventureSettlementCommit
}

type adventureStageEliminationPlan struct {
	roomID          uint16
	gameID          uint32
	newArbitratorID uint16
	leaveMapID      uint16
	players         []preparedAdventureStageElimination
}

type appliedAdventureStageElimination struct {
	prepared  preparedAdventureStageElimination
	departure roomstate.Departure
}

// prepareAdventureStageEliminations snapshots all battle-owned result data
// while the outgoing stage still contains the eliminated players. It does not
// lock or mutate any peer session, so REQUEST_GAME_NEXTMAP can safely call it
// while its own dispatcher mutex is held.
func (server *Server) prepareAdventureStageEliminations(triggerPacket []byte, roomID uint16, gameID uint32, clientTime uint32, newArbitratorID uint16, battle *match.AdventureBattle, settled []match.AdventureSettlement) (adventureStageEliminationPlan, error) {
	plan := adventureStageEliminationPlan{roomID: roomID, gameID: gameID, newArbitratorID: newArbitratorID}
	var leaveMapID uint16
	if state, active := server.worldState().Room(roomID); active && state != nil {
		leaveMapID = selectedRoomMapID(state.Snapshot())
	}
	plan.leaveMapID = leaveMapID
	for _, eliminated := range settled {
		playerID := eliminated.Participant.PlayerID
		session := server.liveRoomSessionForPlayer(roomID, playerID)
		if session == nil {
			plan.players = append(plan.players, preparedAdventureStageElimination{participant: eliminated})
			continue
		}
		statistics, err := battle.PlayerStatistics(playerID)
		if err != nil {
			return adventureStageEliminationPlan{}, err
		}
		gameOver := game.GameOverData{
			Time: clientTime, GameMode: game.SettlementGameModeAdventure,
			Results: []game.GameResultData{{
				PlayerID: playerID, Result: game.GameResultLoss,
				Fields: (game.AdventureResultStatistics{
					KillCount: statistics.KillCount, KillScore: statistics.KillScore,
					RescueCount: statistics.RescueCount, RescueScore: statistics.RescueScore,
					RewardCount: statistics.RewardCount, RewardScore: statistics.RewardScore,
				}).GameResultFields(),
			}},
		}
		applyAdventureOutcomeCourageMultiplier(&gameOver)
		settlement, err := adventureSettlementForPlayer(gameOver, playerID)
		if err == nil {
			err = attachAdventureCollectedItems(battle, playerID, &settlement)
		}
		if err != nil {
			return adventureStageEliminationPlan{}, err
		}
		source, err := game.BuildLocalGameOverNotifyFromEvent(triggerPacket, roomID, server.nextGameDataSequence(), gameOver)
		if err != nil {
			return adventureStageEliminationPlan{}, err
		}
		template := session.packetTemplate()
		if len(template) == 0 {
			return adventureStageEliminationPlan{}, fmt.Errorf("eliminated player %d has no packet template", playerID)
		}
		notification, err := game.RebindLocalServerNotification(template, source)
		if err != nil {
			return adventureStageEliminationPlan{}, err
		}
		plan.players = append(plan.players, preparedAdventureStageElimination{
			participant: eliminated, session: session, sectionID: session.routingSectionID(),
			template: template, notification: notification, settlement: settlement,
		})
	}
	return plan, nil
}

// applyAdventureStageEliminations performs cross-session settlement only after
// the next-map actor's dispatcher mutex has been released. Waiting for a busy
// eliminated connection is safe here and deliberately preferred over dropping
// its settlement or leaving a ghost room member.
func (server *Server) applyAdventureStageEliminations(plan adventureStageEliminationPlan) error {
	applied := make([]appliedAdventureStageElimination, 0, len(plan.players))
	// A single next-map request can settle several dead teammates. Commit every
	// authoritative departure before projecting any of them: otherwise the
	// first eliminated owner is advertised with another soon-to-be-eliminated
	// player as the temporary owner, and that client never observes the final
	// survivor-owned room state.
	for _, eliminated := range plan.players {
		playerID := eliminated.participant.Participant.PlayerID
		session := eliminated.session
		if session == nil {
			departure, err := server.worldState().RemoveMember(plan.roomID, playerID, roomstate.LeaveEliminatedBetweenStages)
			if err != nil {
				return err
			}
			applied = append(applied, appliedAdventureStageElimination{prepared: eliminated, departure: departure})
			continue
		}
		var departure roomstate.Departure
		session.mu.Lock()
		// An explicit leave may have completed while the next-map ACK was being
		// delivered. That path already owns settlement and room cleanup.
		if session.liveRoomID.Load() != uint32(plan.roomID) || session.CurrentGameID != plan.gameID {
			session.mu.Unlock()
			continue
		}
		err := server.commitAdventureSettlement(session, eliminated.settlement, session.connectionID)
		if err == nil {
			departure, err = server.leaveSessionRoom(session, roomstate.LeaveEliminatedBetweenStages)
		}
		if err == nil {
			session.CurrentGameID = 0
			session.CurrentStageGameID = 0
			session.CurrentMapID = 0
			// The authoritative member is gone now so survivors can enter the
			// next stage, while the eliminated native client still owns its result
			// scene. Keep the room correlation so a later native
			// REQUEST_LEAVE_ROOM, when emitted, can be acknowledged idempotently.
			session.detachedRoomID = plan.roomID
		}
		session.mu.Unlock()
		if err != nil {
			return err
		}
		applied = append(applied, appliedAdventureStageElimination{prepared: eliminated, departure: departure})
	}
	if len(applied) == 0 {
		return nil
	}

	var finalOwnerID uint16
	if state, active := server.worldState().Room(plan.roomID); active && state != nil {
		finalOwnerID = state.Snapshot().OwnerID
	}
	sections := make(map[uint16]struct{})
	// The eliminated client needs two distinct facts: its settlement and an
	// eventual local game-scene close. It does not reliably emit
	// REQUEST_LEAVE_ROOM after a between-stage loss, and self-addressed
	// NOTIFY_LEAVE_ROOM only removes member actors without closing rule 12's room
	// scene. QQTSection's 0x0404 ReasonID=4 branch is the native closeRoom(0)
	// transaction and has no owner-kick dialog, but it must not overtake the
	// native result scene.
	for _, item := range applied {
		eliminated := item.prepared
		playerID := eliminated.participant.Participant.PlayerID
		session := eliminated.session
		if session == nil {
			continue
		}
		sections[eliminated.sectionID] = struct{}{}
		if !server.writeTCP(session.connection, session.connectionID, session.localAddress, session.remoteAddress, eliminated.notification, "qqt_adventure_stage_eliminated_game_over") {
			server.log(logEvent{
				Level: "warn", Event: "adventure_stage_player_game_over_write_failed", ConnectionID: session.connectionID,
				AccountID: fmt.Sprint(session.UIN), RoomID: fmt.Sprint(plan.roomID),
				Result: fmt.Sprintf("game_%d_player_%d", plan.gameID, playerID),
			})
			continue
		}
		if !server.writeAdventureSettlementInventoryRefresh(session.connection, session, session.connectionID, session.localAddress, session.remoteAddress, eliminated.template, &eliminated.settlement) {
			server.log(logEvent{
				Level: "warn", Event: "adventure_stage_inventory_refresh_write_failed", ConnectionID: session.connectionID,
				AccountID: fmt.Sprint(session.UIN), RoomID: fmt.Sprint(plan.roomID),
				Result: fmt.Sprintf("game_%d_player_%d", plan.gameID, playerID),
			})
		}
		server.log(logEvent{
			Level: "info", Event: "adventure_stage_player_settled", ConnectionID: session.connectionID,
			AccountID: fmt.Sprint(session.UIN), RoomID: fmt.Sprint(plan.roomID),
			Result: fmt.Sprintf("game_%d_player_%d_reason_%d", plan.gameID, playerID, eliminated.participant.Reason),
		})
		server.scheduleAdventureStageSceneClose(plan.roomID, playerID, session, eliminated.template)
	}

	// Remaining members consume the ordinary room-member departure projection;
	// the eliminated client's whole local scene was closed above and must not be
	// fed a second, partial self-departure transaction.
	for _, item := range applied {
		if err := server.projectAdventureStageDepartureWithOwner(plan.roomID, 0, finalOwnerID, plan.newArbitratorID, plan.leaveMapID, item.departure); err != nil {
			return err
		}
	}
	_, roomActive := server.worldState().Room(plan.roomID)
	for sectionID := range sections {
		server.broadcastRoomLobbyMutation(sectionID, plan.roomID, !roomActive, "qqt_room_lobby_push_stage_eliminated")
	}

	return nil
}

func (server *Server) scheduleAdventureStageSceneClose(roomID, playerID uint16, session *connectionSession, template []byte) {
	if server == nil || session == nil || roomID == 0 || server.done == nil {
		return
	}
	template = append([]byte(nil), template...)
	server.log(logEvent{
		Level: "info", Event: "adventure_stage_scene_close_scheduled", ConnectionID: session.connectionID,
		AccountID: fmt.Sprint(session.UIN), RoomID: fmt.Sprint(roomID),
		Result: fmt.Sprintf("player_%d_grace_%s", playerID, adventureStageResultSceneGrace),
	})
	server.runAfter(adventureStageResultSceneGrace, func() {
		server.closeDetachedAdventureStageScene(roomID, playerID, session, template)
	})
}

func (server *Server) closeDetachedAdventureStageScene(roomID, playerID uint16, session *connectionSession, template []byte) bool {
	if server == nil || session == nil || roomID == 0 {
		return false
	}
	// The native result-scene exit request clears detachedRoomID. A subsequent
	// room create/join does the same. Holding the session lock across the small
	// notification write prevents this delayed close from racing a new scene.
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.detachedRoomID != roomID || session.RoomID != 0 || session.CurrentGameID != 0 {
		server.log(logEvent{
			Level: "info", Event: "adventure_stage_scene_close_cancelled", ConnectionID: session.connectionID,
			AccountID: fmt.Sprint(session.UIN), RoomID: fmt.Sprint(roomID),
			Result: fmt.Sprintf("player_%d_detached_room_%d_current_room_%d_game_%d", playerID, session.detachedRoomID, session.RoomID, session.CurrentGameID),
		})
		return false
	}
	closeScene, err := game.BuildLocalKickOffRoomNotification(template, roomID, game.KickOffRoomNotification{
		ReasonID: game.KickOffRoomReasonCloseGameScene,
		RoomID:   roomID, PlayerUIN: session.UIN,
	})
	if err != nil {
		server.log(logEvent{
			Level: "warn", Event: "adventure_stage_scene_close_build_failed", ConnectionID: session.connectionID,
			AccountID: fmt.Sprint(session.UIN), RoomID: fmt.Sprint(roomID),
			Result: fmt.Sprintf("player_%d", playerID), ErrorContext: err.Error(),
		})
		return false
	}
	if !server.writeTCP(session.connection, session.connectionID, session.localAddress, session.remoteAddress, closeScene, "qqt_adventure_stage_eliminated_close_scene") {
		server.log(logEvent{
			Level: "warn", Event: "adventure_stage_player_close_scene_write_failed", ConnectionID: session.connectionID,
			AccountID: fmt.Sprint(session.UIN), RoomID: fmt.Sprint(roomID),
			Result: fmt.Sprintf("player_%d_reason_%d", playerID, game.KickOffRoomReasonCloseGameScene),
		})
		return false
	}
	session.detachedRoomID = 0
	server.log(logEvent{
		Level: "info", Event: "adventure_stage_scene_closed", ConnectionID: session.connectionID,
		AccountID: fmt.Sprint(session.UIN), RoomID: fmt.Sprint(roomID),
		Result: fmt.Sprintf("player_%d_reason_%d", playerID, game.KickOffRoomReasonCloseGameScene),
	})
	return true
}

func (server *Server) projectAdventureStageDeparture(roomID uint16, actorUIN uint32, newArbitratorID, mapID uint16, departure roomstate.Departure) error {
	if departure.Empty || departure.Member.PlayerID == 0 {
		return nil
	}
	currentOwnerID, err := server.currentRoomOwnerAfterDeparture(roomID, departure)
	if err != nil {
		return err
	}
	return server.projectAdventureStageDepartureWithOwner(roomID, actorUIN, currentOwnerID, newArbitratorID, mapID, departure)
}

func (server *Server) projectAdventureStageDepartureWithOwner(roomID uint16, actorUIN uint32, ownerID, newArbitratorID, mapID uint16, departure roomstate.Departure) error {
	if departure.Empty || departure.Member.PlayerID == 0 {
		return nil
	}
	leave := game.LeaveRoomNotification{
		PlayerID: departure.Member.PlayerID, NewRoomOwnerID: ownerID,
		NewArbitratorID: newArbitratorID, MapID: mapID,
	}
	server.broadcastRoomNotification(roomID, actorUIN, "qqt_adventure_stage_leave_room_peer_notify", func(recipient []byte) ([]byte, error) {
		return game.BuildLocalLeaveRoomNotification(recipient, roomID, leave)
	})
	return nil
}
