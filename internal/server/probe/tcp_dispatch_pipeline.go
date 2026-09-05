package probe

import (
	"fmt"
	"time"

	"qqtang/internal/protocol/directory"
	"qqtang/internal/protocol/game"
)

// tcpDispatchOutcome is the complete transport work produced by request
// dispatch. It deliberately contains no socket: protocol selection can be
// tested independently from ordered delivery and connection lifetime.
type tcpDispatchOutcome struct {
	beforeResponse               []byte
	beforeResponseResult         string
	response                     []byte
	result                       string
	followUp                     []byte
	followUpResult               string
	additionalFollowUps          []tcpFollowUpPacket
	startFollowUp                []byte
	startFollowUpResult          string
	startFollowUpDelay           time.Duration
	completeAdventureAfterSend   bool
	completeCompetitiveAfterSend bool
	adventureSettlement          *adventureSettlementCommit
	adventureRoomSettlement      *adventureRoomSettlementCommit
	competitiveRoomSettlement    *competitiveRoomSettlementCommit
	finalVictorySchedule         *adventureFinalVictorySchedule
	postResponse                 func()
	afterFollowUp                func()
	afterStartFollowUp           func()
	closeAfterResponse           bool
	handledWithoutResponse       bool
	keepConnection               bool
}

func newTCPDispatchOutcome(config ListenerConfig, data []byte) tcpDispatchOutcome {
	outcome := tcpDispatchOutcome{
		response:            config.response,
		result:              "preset",
		followUpResult:      "qqt_game_begin",
		startFollowUpResult: "qqt_second_followup",
		keepConnection:      true,
	}
	if config.Response.EchoAfterReceive {
		outcome.response = data
		outcome.result = "echo"
	}
	return outcome
}

func (outcome *tcpDispatchOutcome) available() bool {
	return len(outcome.response) == 0
}

// requiresOrderedDelivery identifies outcomes whose work continues after the
// direct ACK. In particular, an objective can produce GAME_OVER solely in
// startFollowUp; omitting that field here used to mark the battle concluded
// in memory while silently dropping both GAME_OVER and settlement.
func (outcome tcpDispatchOutcome) requiresOrderedDelivery() bool {
	return len(outcome.response) != 0 && (len(outcome.beforeResponse) != 0 || len(outcome.followUp) != 0 ||
		len(outcome.additionalFollowUps) != 0 || len(outcome.startFollowUp) != 0 ||
		outcome.completeAdventureAfterSend || outcome.completeCompetitiveAfterSend ||
		outcome.adventureSettlement != nil || outcome.adventureRoomSettlement != nil ||
		outcome.competitiveRoomSettlement != nil || outcome.finalVictorySchedule != nil)
}

func (outcome *tcpDispatchOutcome) applyProtocol(result protocolMessageResult) {
	if !result.handled {
		return
	}
	outcome.beforeResponse = result.beforeResponse
	outcome.beforeResponseResult = result.beforeResult
	outcome.response = result.response
	outcome.result = result.result
	outcome.followUp = result.followUp
	outcome.followUpResult = result.followUpResult
	outcome.postResponse = result.postResponse
}

func (outcome *tcpDispatchOutcome) applyMatchStart(result matchStartMessageResult) {
	if !result.handled {
		return
	}
	outcome.response = result.response
	outcome.result = result.result
	outcome.followUp = result.followUp
	outcome.followUpResult = result.followUpResult
	outcome.startFollowUp = result.startFollowUp
	outcome.startFollowUpResult = result.startFollowUpResult
	outcome.startFollowUpDelay = result.startFollowUpDelay
	outcome.postResponse = result.postResponse
	outcome.afterFollowUp = result.afterFollowUp
	outcome.afterStartFollowUp = result.afterStartFollowUp
}

func (outcome *tcpDispatchOutcome) applyGameEvent(result gameEventMessageResult) {
	if !result.handled {
		return
	}
	outcome.response = result.response
	outcome.result = result.result
	outcome.followUp = result.followUp
	outcome.followUpResult = result.followUpResult
	outcome.startFollowUp = result.startFollowUp
	outcome.startFollowUpResult = result.startFollowUpResult
	outcome.startFollowUpDelay = result.startFollowUpDelay
	outcome.completeAdventureAfterSend = result.completeAdventureAfterSend
	outcome.completeCompetitiveAfterSend = result.completeCompetitiveAfterSend
	outcome.adventureSettlement = result.adventureSettlement
	outcome.adventureRoomSettlement = result.adventureRoomSettlement
	outcome.competitiveRoomSettlement = result.competitiveRoomSettlement
	outcome.finalVictorySchedule = result.finalVictorySchedule
	outcome.handledWithoutResponse = result.handledWithoutResponse
	outcome.postResponse = result.afterResponse
	outcome.afterFollowUp = result.afterFollowUp
	outcome.afterStartFollowUp = result.afterStartFollowUp
}

// dispatchTCPMessage executes protocol-family adapters in legacy priority
// order. Each stage owns exactly one family; this replaces the former
// command-by-command conditional block in the socket loop.
func (server *Server) dispatchTCPMessage(config ListenerConfig, session *connectionSession, connectionID, local, remote string, data []byte) tcpDispatchOutcome {
	outcome := newTCPDispatchOutcome(config, data)
	server.dispatchDirectoryStage(config, session, connectionID, remote, data, &outcome)
	server.dispatchShopStages(config, session, connectionID, data, &outcome)
	server.dispatchLoginStages(config, session, connectionID, remote, data, &outcome)
	if !outcome.keepConnection {
		return outcome
	}
	if !server.authorizeBusinessDispatch(config, session, connectionID, data, &outcome) {
		return outcome
	}
	server.dispatchLoginUtilityStage(config, session, connectionID, data, &outcome)
	server.dispatchChatStage(config, session, connectionID, data, &outcome)
	server.dispatchFriendStage(config, session, connectionID, data, &outcome)
	server.dispatchKinStage(config, session, connectionID, data, &outcome)
	server.dispatchMarriageStage(config, session, connectionID, data, &outcome)
	server.dispatchItemStatusStage(config, session, connectionID, data, &outcome)
	server.dispatchRoomStage(config, session, connectionID, data, &outcome)
	server.dispatchPeerStage(session, connectionID, data, &outcome)
	server.dispatchMoneyStage(config, session, connectionID, data, &outcome)
	server.dispatchLobbyStage(config, session, connectionID, data, &outcome)
	server.dispatchMatchStartStage(config, session, connectionID, local, remote, data, &outcome)
	server.dispatchGameEventStage(config, session, connectionID, local, remote, data, &outcome)
	server.logUnhandledDynamicPacket(config, connectionID, local, remote, data, outcome)
	return outcome
}

// authorizeBusinessDispatch is the single transport boundary between login
// and stateful game operations. Protocol decoders already bind payload UIN to
// the encrypted envelope; this additionally binds that envelope to the live
// authenticated connection so a valid packet cannot act as another account.
func (server *Server) authorizeBusinessDispatch(config ListenerConfig, session *connectionSession, connectionID string, data []byte, outcome *tcpDispatchOutcome) bool {
	if outcome == nil || !outcome.available() || !config.Response.QQTLoginSuccess {
		return true
	}
	inspection, err := game.InspectLocalPacket(data)
	if err != nil {
		return true
	}
	// The legacy client sends the read-only directory discovery request before
	// it has sent REQUEST_LOGIN. Verified captures show the same route with
	// SectionID 0x00FE on the game/TLS probes and 0x00FD on the HTTP fallback.
	// A listener with QQTDirectoryResponse answers it in dispatchDirectoryStage;
	// other listeners must at least keep the probe connection valid. Closing it
	// here turns normal discovery into a fixed multi-second fallback and can
	// make the client abandon district navigation.
	//
	// Keep the exception narrower than the command number: route, marker and
	// section are part of the recovered directory wire contract. All stateful
	// commands, including REQUEST_LOGIN itself, still pass through their own
	// authentication/capability checks below.
	if isLocalDirectoryDiscovery(inspection) {
		return true
	}
	authorized := session != nil && session.UIN != 0 && inspection.EnvelopeUIN == session.UIN
	if authorized && session.auxiliary {
		// Catalog and purchase messages were consumed by the shop stages above.
		// The only profile message legitimately sent on the remaining type-2
		// connection is the client's equipment/status save.
		authorized = config.Response.QQTShopType2Session && inspection.Command == game.ItemStatusChangeCommand
	} else if authorized {
		authorized = session.liveUIN.Load() == session.UIN
	}
	if authorized {
		return true
	}
	server.log(logEvent{
		Level: "warn", Event: "unauthenticated_business_packet_rejected", ConnectionID: connectionID,
		AccountID: fmt.Sprint(inspection.EnvelopeUIN), MessageID: fmt.Sprintf("0x%04X", inspection.Command),
		Result: "connection_closed", ErrorContext: "packet envelope is not owned by this authenticated connection",
	})
	outcome.response = nil
	outcome.handledWithoutResponse = true
	outcome.keepConnection = false
	return false
}

func isLocalDirectoryDiscovery(inspection game.LocalPacketInspection) bool {
	return inspection.EnvelopeUIN != 0 &&
		inspection.Command == localDirectoryRequestCommand &&
		inspection.Route == localDirectoryRequestRoute &&
		inspection.Marker == localDirectoryRequestMarker &&
		(inspection.SectionID == localDirectoryRequestSectionID ||
			inspection.SectionID == localDirectoryProbeRequestSectionID)
}

func (server *Server) dispatchFriendStage(config ListenerConfig, session *connectionSession, connectionID string, data []byte, outcome *tcpDispatchOutcome) {
	if !outcome.available() || !config.Response.QQTLoginSuccess {
		return
	}
	result := server.handleFriendMessage(session, connectionID, data)
	if !result.handled {
		return
	}
	outcome.applyProtocol(result)
	outcome.handledWithoutResponse = len(result.response) == 0
}

func (server *Server) dispatchKinStage(config ListenerConfig, session *connectionSession, connectionID string, data []byte, outcome *tcpDispatchOutcome) {
	if !outcome.available() || !config.Response.QQTLoginSuccess {
		return
	}
	result := server.handleKinMessage(session, connectionID, data)
	if !result.handled {
		return
	}
	outcome.applyProtocol(result)
	outcome.handledWithoutResponse = len(result.response) == 0
}

func (server *Server) dispatchMarriageStage(config ListenerConfig, session *connectionSession, connectionID string, data []byte, outcome *tcpDispatchOutcome) {
	if !outcome.available() || !config.Response.QQTLoginSuccess {
		return
	}
	result := server.handleMarriageMessage(session, connectionID, data)
	if !result.handled {
		return
	}
	outcome.applyProtocol(result)
	outcome.handledWithoutResponse = len(result.response) == 0
}

func (server *Server) dispatchDirectoryStage(config ListenerConfig, session *connectionSession, connectionID, remote string, data []byte, outcome *tcpDispatchOutcome) {
	if !config.Response.QQTDirectoryResponse {
		return
	}
	inspection, err := game.InspectLocalPacket(data)
	if err != nil || !isLocalDirectoryDiscovery(inspection) {
		outcome.response, outcome.result = nil, "qqt_directory_request_ignored"
		return
	}
	server.attachAccountAuthNavigation(session, remote, inspection.EnvelopeUIN)
	outcome.response, err = directory.BuildLocalResponse(inspection.EnvelopeUIN, inspection.OuterSequence, server.directoryHallPayload)
	if err != nil {
		server.log(logEvent{Level: "error", Event: "directory_response_build_failed", ConnectionID: connectionID, AccountID: fmt.Sprint(inspection.EnvelopeUIN), Result: "qqt_directory_response", ErrorContext: err.Error()})
		outcome.response = nil
		return
	}
	outcome.result = "qqt_directory_response"
}

func (server *Server) dispatchShopStages(config ListenerConfig, session *connectionSession, connectionID string, data []byte, outcome *tcpDispatchOutcome) {
	if outcome.available() && config.Response.QQTShopCatalog {
		if response, result, handled := server.handleShopCatalogMessage(session, connectionID, data); handled {
			outcome.response, outcome.result = response, result
			outcome.handledWithoutResponse = len(response) == 0
		}
	}
	if outcome.available() && config.Response.QQTShopType2Session {
		if response, result, handled := server.handleShopType2SessionMessage(session, connectionID, data); handled {
			outcome.response, outcome.result = response, result
			outcome.handledWithoutResponse = len(response) == 0
			outcome.closeAfterResponse = len(response) != 0
		}
	}
}

func (server *Server) dispatchLoginStages(config ListenerConfig, session *connectionSession, connectionID, remote string, data []byte, outcome *tcpDispatchOutcome) {
	if !config.Response.QQTLoginSuccess {
		return
	}
	login := server.handleLoginRequest(session, connectionID, remote, data)
	if login.handled {
		outcome.keepConnection = login.keepConnection
		if !login.keepConnection {
			return
		}
		outcome.response, outcome.result = login.response, login.result
		outcome.followUp, outcome.followUpResult = login.followUp, login.followUpResult
		outcome.additionalFollowUps = login.followUps
		outcome.postResponse = login.postResponse
		outcome.handledWithoutResponse = len(login.response) == 0
	}
	if !outcome.available() {
		return
	}
	// The native config-version probe precedes REQUEST_LOGIN and has no
	// account effects. Logout, pets, crafting and item use are business
	// operations and must pass the common authenticated-envelope boundary.
	if _, err := game.DecodeLocalConfigFileRequest(data); err == nil {
		server.dispatchLoginUtilityStage(config, session, connectionID, data, outcome)
	}
}

func (server *Server) dispatchLoginUtilityStage(config ListenerConfig, session *connectionSession, connectionID string, data []byte, outcome *tcpDispatchOutcome) {
	if !config.Response.QQTLoginSuccess || !outcome.available() {
		return
	}
	utility := server.handleLoginUtilityMessage(session, connectionID, data)
	if !utility.handled {
		return
	}
	outcome.response, outcome.result = utility.response, utility.result
	outcome.beforeResponse, outcome.beforeResponseResult = utility.beforeResponse, utility.beforeResponseResult
	outcome.followUp, outcome.followUpResult = utility.followUp, utility.followUpResult
	outcome.additionalFollowUps = utility.followUps
	outcome.postResponse = utility.postResponse
	outcome.handledWithoutResponse = len(utility.response) == 0
	if config.Response.QQTShopType2Session {
		if _, err := game.DecodeLocalConfigFileRequest(data); err == nil {
			outcome.closeAfterResponse = len(utility.response) != 0
		}
	}
}

func (server *Server) dispatchChatStage(config ListenerConfig, session *connectionSession, connectionID string, data []byte, outcome *tcpDispatchOutcome) {
	if !outcome.available() || !config.Response.QQTLoginSuccess {
		return
	}
	chat := server.handleChatMessage(session, connectionID, data)
	if chat.handled {
		outcome.response, outcome.result = chat.response, chat.result
		outcome.postResponse = chat.postResponse
		outcome.handledWithoutResponse = len(chat.response) == 0
	}
}

func (server *Server) dispatchItemStatusStage(config ListenerConfig, session *connectionSession, connectionID string, data []byte, outcome *tcpDispatchOutcome) {
	if !outcome.available() || !(config.Response.gameBeginEnabled() || config.Response.QQTShopType2Session) {
		return
	}
	request, err := game.DecodeLocalItemStatusChangeRequest(data)
	if err != nil {
		return
	}
	collectionTransition := session != nil && itemStatusChangesTouchCollection(session.Profile.Inventory, request.Items)
	outcome.response, err = game.BuildLocalItemStatusChangeSuccess(data)
	if err == nil {
		err = server.updateSessionItemStatuses(session, request)
	}
	if err != nil {
		outcome.response = nil
		server.log(logEvent{Level: "warn", Event: "item_status_change_rejected", ConnectionID: connectionID, MessageID: fmt.Sprintf("0x%04X", game.ItemStatusChangeCommand), Result: "rejected", ErrorContext: err.Error()})
		return
	}
	changedItems := inventoryItemsForStatusChanges(session.Profile.Inventory, request.Items)
	if collectionTransition && len(changedItems) != 0 {
		// Client.exe's native ChangeItemStatusResponse calls
		// ui_updateMyItems() and ui_updateRecycler() synchronously. Therefore
		// the absolute ITEM_INFO projection must arrive before the success ACK;
		// a later notification updates the native cache but leaves the already
		// rebuilt window stale until it is reopened.
		outcome.beforeResponse, err = buildInventoryItemsRefreshPacket(data, request.UIN, 0, changedItems)
		if err == nil {
			outcome.beforeResponseResult = fmt.Sprintf("qqt_collection_inventory_items_%d_before_result", len(changedItems))
		} else {
			outcome.beforeResponse = nil
			server.log(logEvent{Level: "warn", Event: "item_status_refresh_build_failed", ConnectionID: connectionID, AccountID: fmt.Sprint(request.UIN), Result: "delayed_fallback", ErrorContext: err.Error()})
		}
	}
	if len(outcome.beforeResponse) == 0 {
		outcome.postResponse = func() {
			server.scheduleInventoryStatusRefresh(request.UIN, changedItems)
		}
	}
	outcome.result = fmt.Sprintf("qqt_item_status_changed_%d_items", len(request.Items))
	server.log(logEvent{Level: "info", Event: "item_status_change_applied", ConnectionID: connectionID, AccountID: fmt.Sprint(request.UIN), MessageID: fmt.Sprintf("0x%04X", game.ItemStatusChangeCommand), Result: describeItemStatusChanges(request.Items)})
}

func (server *Server) dispatchRoomStage(config ListenerConfig, session *connectionSession, connectionID string, data []byte, outcome *tcpDispatchOutcome) {
	if outcome.available() {
		outcome.applyProtocol(server.handleRoomMessage(config, session, connectionID, data))
	}
}

func (server *Server) dispatchPeerStage(session *connectionSession, connectionID string, data []byte, outcome *tcpDispatchOutcome) {
	if !outcome.available() {
		return
	}
	result := server.handlePeerTransportMessage(session, connectionID, data)
	if result.handled {
		outcome.applyProtocol(result)
		outcome.handledWithoutResponse = len(result.response) == 0
	}
}

func (server *Server) dispatchMoneyStage(config ListenerConfig, session *connectionSession, connectionID string, data []byte, outcome *tcpDispatchOutcome) {
	if !outcome.available() || !config.Response.QQTLoginSuccess {
		return
	}
	inspection, err := game.InspectLocalPacket(data)
	if err != nil || inspection.Command != game.GameMoneyCommand {
		return
	}
	outcome.response, err = game.BuildLocalGameMoneyResponse(data, session.Profile.GameInfo.Money)
	if err != nil {
		outcome.response = nil
		server.log(logEvent{Level: "error", Event: "game_money_failed", ConnectionID: connectionID, AccountID: fmt.Sprint(session.UIN), Result: "rejected", ErrorContext: err.Error()})
		return
	}
	outcome.result = "qqt_game_money"
}

func (server *Server) dispatchLobbyStage(config ListenerConfig, session *connectionSession, connectionID string, data []byte, outcome *tcpDispatchOutcome) {
	if outcome.available() {
		outcome.applyProtocol(server.handleLobbyMessage(config, session, connectionID, data))
	}
}

func (server *Server) dispatchMatchStartStage(config ListenerConfig, session *connectionSession, connectionID, local, remote string, data []byte, outcome *tcpDispatchOutcome) {
	if outcome.available() {
		outcome.applyMatchStart(server.handleMatchStartMessage(config, session, connectionID, local, remote, data))
	}
}

func (server *Server) dispatchGameEventStage(config ListenerConfig, session *connectionSession, connectionID, local, remote string, data []byte, outcome *tcpDispatchOutcome) {
	if outcome.available() && config.Response.QQTGameEventRelay {
		outcome.applyGameEvent(server.handleGameEventMessage(config, session, connectionID, local, remote, data))
	}
}

func (server *Server) logUnhandledDynamicPacket(config ListenerConfig, connectionID, local, remote string, data []byte, outcome tcpDispatchOutcome) {
	if len(outcome.response) != 0 || outcome.handledWithoutResponse || !config.Response.dynamicQQTSession() {
		return
	}
	ignored := logEvent{Level: "info", Event: "dynamic_response_ignored", ConnectionID: connectionID, MessageLength: len(data), Result: "qqt_local_session", ErrorContext: "packet is not a supported local-session request", Network: "tcp", LocalAddress: local, RemoteAddress: remote}
	if inspection, err := game.InspectLocalPacket(data); err == nil {
		ignored.MessageID = fmt.Sprintf("0x%04X", inspection.Command)
		ignored.PayloadLength = len(inspection.Payload)
		ignored.OuterSequence = inspection.OuterSequence
		ignored.RouteSequence = inspection.RouteSequence
		ignored.InnerSequence = inspection.InnerSequence
		ignored.Route = inspection.Route
		ignored.SectionID = inspection.SectionID
		ignored.Result = fmt.Sprintf("qqt_local_session_command_0x%04X_route_%d_section_%d", inspection.Command, inspection.Route, inspection.SectionID)
	} else {
		ignored.ErrorContext += "; inspection failed: " + err.Error()
	}
	server.log(ignored)
}
