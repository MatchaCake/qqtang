package probe

import (
	"context"
	"fmt"
	"time"

	"qqtang/internal/protocol/game"
	"qqtang/internal/server/persistence"
)

func (server *Server) handleChatMessage(session *connectionSession, connectionID string, data []byte) protocolMessageResult {
	inspection, err := game.InspectLocalPacket(data)
	if err != nil {
		return protocolMessageResult{}
	}
	switch inspection.Command {
	case game.RoomChatCommand:
		request, decodeErr := game.DecodeLocalRoomChatRequest(data)
		if decodeErr != nil {
			return server.rejectedChat(connectionID, inspection.Command, decodeErr)
		}
		if session.RoomID == 0 {
			return server.rejectedChat(connectionID, inspection.Command, fmt.Errorf("sender is not in a room"))
		}
		if session.Profile.Nickname == "" {
			return server.rejectedChat(connectionID, inspection.Command, fmt.Errorf("sender UIN %d has no nickname", session.UIN))
		}
		response, responseErr := game.BuildLocalRoomChatResponse(data, 0)
		if responseErr != nil {
			return server.rejectedChat(connectionID, inspection.Command, responseErr)
		}
		notification := game.RoomChatNotification{
			SourcePlayerID: session.Profile.PlayerID, DestinationPlayerID: request.DestinationPlayerID,
			Nickname: session.Profile.Nickname, Content: request.Content,
		}
		return protocolMessageResult{
			handled: true, response: response, result: "qqt_room_chat",
			postResponse: func() {
				// The native chat widget is populated by the content-bearing chat
				// notification, not by the two-byte request result. Echo the accepted
				// message to its sender as well as every peer in the room.
				server.sendUINNotification(session.UIN, "qqt_room_chat_self", func(recipient []byte) ([]byte, error) {
					return game.BuildLocalRoomChatNotification(recipient, notification)
				})
				server.broadcastRoomNotification(session.RoomID, session.UIN, "qqt_room_chat_peer", func(recipient []byte) ([]byte, error) {
					return game.BuildLocalRoomChatNotification(recipient, notification)
				})
			},
		}
	case game.SectionChatCommand:
		request, decodeErr := game.DecodeLocalSectionChatRequest(data)
		if decodeErr != nil {
			return server.rejectedChat(connectionID, inspection.Command, decodeErr)
		}
		uin := sessionWorldUIN(session)
		sectionID := session.Profile.SectionID
		if uin == 0 || sectionID == 0 {
			return server.rejectedChat(connectionID, inspection.Command, fmt.Errorf("sender has no section"))
		}
		presence, presenceErr := server.worldState().LobbyPresence(uin, sectionID)
		if presenceErr != nil {
			return server.rejectedChat(connectionID, inspection.Command, presenceErr)
		}
		if presence.Nickname == "" {
			return server.rejectedChat(connectionID, inspection.Command, fmt.Errorf("sender UIN %d has no authoritative lobby nickname", uin))
		}
		if request.DestinationPlayerID != game.SectionChatBroadcastPlayerID {
			return server.rejectedChat(connectionID, inspection.Command, fmt.Errorf(
				"section chat selector player ID is %d, want %d", request.DestinationPlayerID, game.SectionChatBroadcastPlayerID,
			))
		}
		bugle := request.DestinationPlayerUIN == game.SectionChatSmallBugleUIN
		if request.DestinationPlayerUIN != game.SectionChatPublicUIN && !bugle {
			return server.rejectedChat(connectionID, inspection.Command, fmt.Errorf(
				"unknown section chat selector UIN %d", request.DestinationPlayerUIN,
			))
		}
		response, responseErr := game.BuildLocalSectionChatResponse(data, 0)
		if responseErr != nil {
			return server.rejectedChat(connectionID, inspection.Command, responseErr)
		}

		var consumedBugle game.ItemInfo
		bugleConsumed := false
		if bugle {
			players, playersErr := server.players()
			if playersErr == nil {
				var profiles map[uint32]game.PlayerProfile
				profiles, playersErr = players.ConsumeInventoryItems(context.Background(), []persistence.InventoryConsumption{{
					UIN: uin, ItemID: game.SmallBugleItemID, Quantity: 1,
				}})
				if playersErr == nil {
					bugleConsumed = true
					profile := profiles[uin]
					session.replaceProfile(profile)
					server.syncPrimarySessionProfile(session, uin, profile)
					consumedBugle = game.NewPermanentItemInfo(game.SmallBugleItemID, 0)
					for _, item := range profile.Inventory {
						if item.ItemID == game.SmallBugleItemID {
							consumedBugle = item
							break
						}
					}
				}
			}
			if playersErr != nil {
				response, responseErr := game.BuildLocalSectionChatResponse(data, 1)
				if responseErr != nil {
					return server.rejectedChat(connectionID, inspection.Command, responseErr)
				}
				server.log(logEvent{
					Level: "warn", Event: "section_bugle_rejected", ConnectionID: connectionID,
					AccountID: fmt.Sprint(uin), MessageID: fmt.Sprintf("0x%04X", inspection.Command),
					Result: "missing_small_bugle", ErrorContext: playersErr.Error(),
				})
				return protocolMessageResult{handled: true, response: response, result: "qqt_section_bugle_rejected"}
			}
		}
		if _, routeErr := server.worldState().RouteSectionChat(uin, sectionID, request.Content, time.Now()); routeErr != nil {
			if bugleConsumed {
				if refundErr := server.refundSmallBugle(session, uin); refundErr != nil {
					server.log(logEvent{
						Level: "error", Event: "section_bugle_refund_failed", ConnectionID: connectionID,
						AccountID: fmt.Sprint(uin), Result: "route_failed_after_consume",
						ErrorContext: fmt.Sprintf("route: %v; refund: %v", routeErr, refundErr),
					})
				} else {
					server.log(logEvent{
						Level: "warn", Event: "section_bugle_refunded", ConnectionID: connectionID,
						AccountID: fmt.Sprint(uin), Result: "route_failed_after_consume", ErrorContext: routeErr.Error(),
					})
				}
			}
			return server.rejectedChat(connectionID, inspection.Command, routeErr)
		}
		identity := session.Profile.Identity
		if bugle {
			identity |= game.SectionChatSmallBugleIdentity
		}
		notification := game.SectionChatNotification{
			SourcePlayerID: presence.PlayerID, DestinationPlayerID: request.DestinationPlayerID,
			Nickname: presence.Nickname, Content: request.Content, UIN: presence.UIN, Identity: identity,
			Point:     session.Profile.GameInfo.Point,
			KinFlagID: game.KinFlagIDForWire(session.Profile.KinIndex, session.Profile.KinFlagID),
		}
		resultName := "qqt_section_chat"
		if bugle {
			resultName = "qqt_section_bugle"
		}
		return protocolMessageResult{
			handled: true, response: response, result: resultName,
			postResponse: func() {
				if bugle {
					server.sendUINNotification(uin, "qqt_section_bugle_inventory", func(recipient []byte) ([]byte, error) {
						return game.BuildLocalPlayerItemAddNotification(recipient, game.PlayerItemAddNotification{
							UIN: uin, Time: uint32(time.Now().Unix()), SourceUIN: uin, Items: []game.ItemInfo{consumedBugle},
						})
					})
				}
				server.sendUINNotification(uin, resultName+"_self", func(recipient []byte) ([]byte, error) {
					return game.BuildLocalSectionChatNotification(recipient, notification)
				})
				server.broadcastSectionNotification(sectionID, uin, resultName+"_peer", 0, func(recipient []byte) ([]byte, error) {
					return game.BuildLocalSectionChatNotification(recipient, notification)
				})
			},
		}
	case game.ChatAcrossSectionCommand:
		request, decodeErr := game.DecodeLocalAcrossSectionChatRequest(data)
		if decodeErr != nil {
			return server.rejectedChat(connectionID, inspection.Command, decodeErr)
		}
		if request.DestinationPlayerUIN == 0 {
			return server.rejectedChat(connectionID, inspection.Command, fmt.Errorf("cross-section chat has no destination UIN"))
		}
		response, responseErr := game.BuildLocalAcrossSectionChatResponse(data, 0, request.DestinationPlayerUIN)
		if responseErr != nil {
			return server.rejectedChat(connectionID, inspection.Command, responseErr)
		}
		notification := game.AcrossSectionChatNotification{SourceUIN: session.UIN, Content: request.Content, Nickname: session.Profile.Nickname}
		return protocolMessageResult{
			handled: true, response: response, result: "qqt_cross_section_chat",
			postResponse: func() {
				// RESPONSE_CHAT_ACROSS_SECTION renders the retained request for
				// the sender. A second content notification creates the duplicate
				// targetless whisper seen by the original client, so only notify the
				// destination here.
				server.broadcastDirectNotification(session.UIN, request.DestinationPlayerUIN, "qqt_cross_section_chat_peer", func(recipient []byte) ([]byte, error) {
					return game.BuildLocalAcrossSectionChatNotification(recipient, notification)
				})
			},
		}
	default:
		return protocolMessageResult{}
	}
}

func (server *Server) refundSmallBugle(session *connectionSession, uin uint32) error {
	players, err := server.players()
	if err != nil {
		return err
	}
	profile, _, err := players.ExchangeInventoryItems(context.Background(), uin, nil, []game.ItemInfo{
		game.NewPermanentItemInfo(game.SmallBugleItemID, 1),
	})
	if err != nil {
		return err
	}
	session.replaceProfile(profile)
	server.syncPrimarySessionProfile(session, uin, profile)
	return nil
}

func (server *Server) rejectedChat(connectionID string, command uint16, err error) protocolMessageResult {
	server.log(logEvent{Level: "warn", Event: "chat_rejected", ConnectionID: connectionID, MessageID: fmt.Sprintf("0x%04X", command), Result: "rejected", ErrorContext: err.Error()})
	return protocolMessageResult{handled: true}
}
