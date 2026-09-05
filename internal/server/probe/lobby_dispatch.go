package probe

import (
	"fmt"

	roomstate "qqtang/internal/game/room"
	"qqtang/internal/protocol/game"
)

// handleLobbyMessage owns player/room-list queries and room creation or entry.
// It returns transport work while lobby and room state stay in their domain
// packages.
func (server *Server) handleLobbyMessage(config ListenerConfig, session *connectionSession, id string, data []byte) protocolMessageResult {
	inspection, err := game.InspectLocalPacket(data)
	if err != nil {
		return protocolMessageResult{}
	}
	switch inspection.Command {
	case game.FindFriendCommand, game.RoomListCommand, game.PlayerListCommand,
		game.EnterRoomCommand, game.JoinRoomCommand, game.CreateRoomCommand:
	default:
		return protocolMessageResult{}
	}
	var response []byte
	result := ""
	var beforeResponse []byte
	beforeResult := "qqt_lobby_before_response"
	var followUp []byte
	followUpResult := "qqt_lobby_followup"
	var postResponse func()

	if len(response) == 0 && config.Response.QQTPlayerList {
		if findRequest, findErr := game.DecodeLocalFindFriendRequest(data); findErr == nil {
			var friend game.FindFriendResponse
			friend, findErr = server.findFriendResponse(session, findRequest.FriendUIN)
			if findErr == nil {
				response, findErr = game.BuildLocalFindFriendResponse(data, friend)
			}
			if findErr != nil {
				server.log(logEvent{Level: "error", Event: "find_friend_failed", ConnectionID: id, AccountID: fmt.Sprint(findRequest.UIN), Result: "rejected", ErrorContext: findErr.Error()})
				response = nil
			} else {
				result = fmt.Sprintf("qqt_find_friend_%d", findRequest.FriendUIN)
			}
		}
	}
	if len(response) == 0 && config.Response.QQTRoomList {
		if listRequest, listErr := game.DecodeLocalRoomListRequest(data); listErr == nil {
			var rooms []game.RoomListEntry
			rooms, listErr = server.roomListPage(session, listRequest)
			if listErr == nil {
				response, listErr = game.BuildRoomListResponse(data, rooms)
			}
			if listErr != nil {
				server.log(logEvent{Level: "error", Event: "room_list_failed", ConnectionID: id, AccountID: fmt.Sprint(listRequest.UIN), Result: "rejected", ErrorContext: listErr.Error()})
				response = nil
			} else {
				result = fmt.Sprintf("qqt_room_list_%d_filter_%d_type_%d", len(rooms), listRequest.GameMode, listRequest.GameType)
			}
		}
	}
	if len(response) == 0 && config.Response.QQTPlayerList {
		if listRequest, listErr := game.DecodeLocalPlayerListRequest(data); listErr == nil {
			var players []game.PlayerListEntry
			players, listErr = server.playerListEntries(session, listRequest)
			if listErr == nil {
				response, listErr = game.BuildPlayerListResponse(data, players)
			}
			if listErr == nil {
				result = fmt.Sprintf("qqt_player_list_%d", len(players))
			}
			if listErr != nil {
				server.log(logEvent{Level: "error", Event: "player_list_failed", ConnectionID: id, AccountID: fmt.Sprint(listRequest.UIN), Result: "rejected", ErrorContext: listErr.Error()})
				response = nil
			}
		}
	}
	if len(response) == 0 && config.Response.QQTRoomList {
		if enterRequest, enterErr := game.DecodeLocalEnterRoomRequest(data); enterErr == nil {
			var roomResponse game.EnterRoomResponseOld
			var newlyJoined bool
			roomResponse, newlyJoined, enterErr = server.enterSessionRoom(session, enterRequest)
			if enterErr == nil {
				response, enterErr = game.BuildLocalEnterRoomSuccess(data, roomResponse)
			}
			if enterErr != nil {
				response, _ = game.BuildLocalEnterRoomFailure(data, enterRoomRejectionResult(enterErr), enterRequest.RoomID)
				server.log(logEvent{Level: "warn", Event: "room_enter_rejected", ConnectionID: id, AccountID: fmt.Sprint(enterRequest.UIN), RoomID: fmt.Sprint(enterRequest.RoomID), MessageID: fmt.Sprintf("0x%04X", game.EnterRoomCommand), Result: "rejected", ErrorContext: enterErr.Error()})
			} else {
				result = fmt.Sprintf("qqt_enter_room_%d_players_%d", enterRequest.RoomID, len(roomResponse.Players))
				if !newlyJoined {
					result = fmt.Sprintf("qqt_reenter_retained_room_%d_players_%d", enterRequest.RoomID, len(roomResponse.Players))
				}
				var petCount int
				beforeResponse, petCount, enterErr = server.localPlayerPetsRefresh(session, data)
				if enterErr != nil {
					beforeResponse = nil
					server.log(logEvent{Level: "warn", Event: "room_enter_pet_refresh_failed", ConnectionID: id, AccountID: fmt.Sprint(session.UIN), RoomID: fmt.Sprint(enterRequest.RoomID), ErrorContext: enterErr.Error()})
				} else {
					beforeResult = fmt.Sprintf("qqt_room_enter_pet_list_refresh_%d_before_result", petCount)
				}
				roleRefresh := roomRoleRefreshNotifications(roomResponse, session.Profile.PlayerID)
				for _, player := range roomResponse.Players {
					if player.Player.PlayerID != session.Profile.PlayerID {
						continue
					}
					notification := game.EnterRoomNotificationOld{
						Player: player.Player, RoomID: enterRequest.RoomID, Attach: player.Attach, SpouseUIN: player.SpouseUIN,
					}
					postResponse = func() {
						if newlyJoined {
							server.broadcastRoomNotification(enterRequest.RoomID, session.UIN, "qqt_enter_room_peer_notify", func(recipient []byte) ([]byte, error) {
								return game.BuildLocalEnterRoomNotification(recipient, notification)
							})
						}
						server.sendRoomRoleRefresh(session, data, enterRequest.RoomID, roleRefresh)
						if newlyJoined {
							server.broadcastRoomLobbyMutation(session.Profile.SectionID, enterRequest.RoomID, false, "qqt_room_lobby_push_enter")
						}
					}
					break
				}
			}
		}
	}
	if len(response) == 0 && config.Response.QQTRoomList {
		if joinRequest, joinErr := game.DecodeLocalJoinRoomRequest(data); joinErr == nil {
			var roomID uint16
			var roomResponse game.JoinRoomResponseOld
			roomID, roomResponse, joinErr = server.quickJoinSessionRoom(session, joinRequest)
			if joinErr == nil {
				response, joinErr = game.BuildLocalJoinRoomSuccess(data, roomResponse)
			}
			if joinErr != nil {
				if roomID != 0 && session.RoomID == roomID {
					_, _ = server.leaveSessionRoom(session, roomstate.LeaveVoluntary)
				}
				response, _ = game.BuildLocalJoinRoomFailure(data, enterRoomRejectionResult(joinErr), roomID)
				server.log(logEvent{Level: "warn", Event: "room_quick_join_rejected", ConnectionID: id, AccountID: fmt.Sprint(joinRequest.UIN), RoomID: fmt.Sprint(roomID), MessageID: fmt.Sprintf("0x%04X", game.JoinRoomCommand), Result: "rejected", ErrorContext: joinErr.Error()})
			} else {
				result = fmt.Sprintf("qqt_join_room_%d_players_%d", roomID, len(roomResponse.Players))
				var petCount int
				beforeResponse, petCount, joinErr = server.localPlayerPetsRefresh(session, data)
				if joinErr != nil {
					beforeResponse = nil
					server.log(logEvent{Level: "warn", Event: "room_join_pet_refresh_failed", ConnectionID: id, AccountID: fmt.Sprint(session.UIN), RoomID: fmt.Sprint(roomID), ErrorContext: joinErr.Error()})
				} else {
					beforeResult = fmt.Sprintf("qqt_room_join_pet_list_refresh_%d_before_result", petCount)
				}
				roleRefresh := roomRoleRefreshNotifications(roomResponse.EnterRoomResponseOld, session.Profile.PlayerID)
				for _, player := range roomResponse.Players {
					if player.Player.PlayerID != session.Profile.PlayerID {
						continue
					}
					notification := game.EnterRoomNotificationOld{
						Player: player.Player, RoomID: roomID, Attach: player.Attach, SpouseUIN: player.SpouseUIN,
					}
					postResponse = func() {
						server.broadcastRoomNotification(roomID, session.UIN, "qqt_join_room_peer_notify", func(recipient []byte) ([]byte, error) {
							return game.BuildLocalEnterRoomNotification(recipient, notification)
						})
						server.sendRoomRoleRefresh(session, data, roomID, roleRefresh)
						server.broadcastRoomLobbyMutation(session.Profile.SectionID, roomID, false, "qqt_room_lobby_push_join")
					}
					break
				}
			}
		}
	}
	if len(response) == 0 && config.Response.QQTCreateRoomSuccess {
		if createRequest, requestErr := game.DecodeLocalCreateRoomRequest(data); requestErr == nil {
			roomErr := server.prepareSessionForRoomCreate(session)
			var roomID uint16
			var roomName string
			if roomErr == nil {
				roomName, roomErr = createRequest.RoomNameString()
			}
			if roomErr == nil {
				roomID, roomErr = server.createAllocatedSessionRoom(session, createRequest.GameType, createRequest.ContinueID, roomstate.Properties{
					Name: roomName, Flag: byte(createRequest.Flag), HasPassword: createRequest.HasPassword(), Password: createRequest.Password,
				})
			}
			if roomErr == nil {
				response, roomErr = game.BuildLocalCreateRoomSuccess(data, roomID)
			}
			if roomErr != nil && roomID != 0 {
				_, _ = server.leaveSessionRoom(session, roomstate.LeaveVoluntary)
			}
			if roomErr != nil {
				response = nil
				beforeResponse = nil
				server.log(logEvent{Level: "error", Event: "room_create_failed", ConnectionID: id, Result: "rejected", ErrorContext: roomErr.Error()})
			} else {
				result = fmt.Sprintf("qqt_create_room_success_%d", roomID)
				var petCount int
				beforeResponse, petCount, roomErr = server.localPlayerPetsRefresh(session, data)
				if roomErr != nil {
					beforeResponse = nil
					server.log(logEvent{Level: "warn", Event: "room_create_pet_refresh_failed", ConnectionID: id, AccountID: fmt.Sprint(session.UIN), RoomID: fmt.Sprint(roomID), ErrorContext: roomErr.Error()})
				} else {
					beforeResult = fmt.Sprintf("qqt_room_create_pet_list_refresh_%d_before_result", petCount)
				}
				sectionID := session.Profile.SectionID
				postResponse = func() {
					server.broadcastRoomLobbyMutation(sectionID, roomID, false, "qqt_room_lobby_push_create")
				}
			}
		}
	}

	handled := len(response) != 0 || result != "" || len(beforeResponse) != 0 || len(followUp) != 0 || postResponse != nil
	return protocolMessageResult{
		handled: handled, beforeResponse: beforeResponse, beforeResult: beforeResult,
		response: response, result: result,
		followUp: followUp, followUpResult: followUpResult, postResponse: postResponse,
	}
}
