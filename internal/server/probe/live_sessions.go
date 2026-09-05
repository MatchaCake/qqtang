package probe

import (
	"encoding/binary"
	"fmt"
	"net"
	"time"

	"qqtang/internal/protocol/game"
)

const authenticatedSessionLeaseLifetime = 15 * time.Second

// Keep the authenticated account identity separate from any one TCP socket so
// a short legacy navigation reconnect can resume without weakening password
// login. Native shop catalog/session sockets are auxiliary connections: their
// normal close never replaces or retires the owning hall socket.
func (server *Server) rememberAuthenticatedSession(session *connectionSession, remote string) {
	if server.authenticatedSessions == nil || session == nil || session.UIN == 0 {
		return
	}
	key := accountAuthGrant{RemoteHost: remoteHost(remote), UIN: session.UIN}
	now := time.Now()
	server.authMu.Lock()
	defer server.authMu.Unlock()
	server.pruneAuthenticatedSessionsLocked(now)
	if session.auxiliary {
		lease, ok := server.authenticatedSessions[key]
		if ok {
			lease.Expires = now.Add(authenticatedSessionLeaseLifetime)
			server.authenticatedSessions[key] = lease
		}
		return
	}
	server.authenticatedSessions[key] = authenticatedSessionLease{
		Expires:            now.Add(authenticatedSessionLeaseLifetime),
		Profile:            clonePlayerProfile(session.Profile),
		SelectedRoleID:     session.selectedRoleID(),
		CurrentMapID:       session.CurrentMapID,
		CurrentGameID:      session.CurrentGameID,
		CurrentStageGameID: session.CurrentStageGameID,
		RoomID:             session.RoomID,
	}
}

func (server *Server) touchAuthenticatedSession(remote string, uin uint32) {
	if server.authenticatedSessions == nil || uin == 0 {
		return
	}
	key := accountAuthGrant{RemoteHost: remoteHost(remote), UIN: uin}
	now := time.Now()
	server.authMu.Lock()
	defer server.authMu.Unlock()
	server.pruneAuthenticatedSessionsLocked(now)
	lease, ok := server.authenticatedSessions[key]
	if !ok {
		return
	}
	lease.Expires = now.Add(authenticatedSessionLeaseLifetime)
	server.authenticatedSessions[key] = lease
}

func (server *Server) authenticatedSession(remote string, uin uint32) (authenticatedSessionLease, bool) {
	if server.authenticatedSessions == nil || uin == 0 {
		return authenticatedSessionLease{}, false
	}
	key := accountAuthGrant{RemoteHost: remoteHost(remote), UIN: uin}
	now := time.Now()
	server.authMu.Lock()
	defer server.authMu.Unlock()
	server.pruneAuthenticatedSessionsLocked(now)
	lease, ok := server.authenticatedSessions[key]
	if !ok {
		return authenticatedSessionLease{}, false
	}
	lease.Expires = now.Add(authenticatedSessionLeaseLifetime)
	lease.Profile = clonePlayerProfile(lease.Profile)
	server.authenticatedSessions[key] = lease
	return lease, true
}

// claimLiveUINFromLease permits only a narrow legacy navigation reconnect. The
// lease is tied to the same account and remote host, expires quickly, and the
// live-session scan still enforces one owning TCP connection per account. A
// normal shop exit does not use this path because closing an auxiliary shop
// socket leaves the hall owner untouched.
func (server *Server) claimLiveUINFromLease(session *connectionSession, remote string, uin uint32) bool {
	if session == nil || uin == 0 {
		return false
	}
	lease, ok := server.authenticatedSession(remote, uin)
	if !ok {
		return false
	}
	server.liveMu.Lock()
	defer server.liveMu.Unlock()
	current := session.liveUIN.Load()
	if current != 0 && current != uin || session.UIN != 0 && session.UIN != uin {
		return false
	}
	for _, active := range server.liveSessions {
		if active != session && active.liveUIN.Load() == uin {
			return false
		}
	}
	session.UIN = uin
	session.replaceProfile(lease.Profile)
	if lease.SelectedRoleID != 0 {
		session.setSelectedRoleID(lease.SelectedRoleID)
	}
	session.CurrentMapID = lease.CurrentMapID
	session.CurrentGameID = lease.CurrentGameID
	session.CurrentStageGameID = lease.CurrentStageGameID
	session.RoomID = lease.RoomID
	session.liveRoomID.Store(uint32(lease.RoomID))
	session.liveUIN.Store(uin)
	session.livePlayerID.Store(uint32(session.Profile.PlayerID))
	session.continuation = false
	return true
}

func (server *Server) hasAuthenticatedSession(uin uint32) bool {
	if server.authenticatedSessions == nil || uin == 0 {
		return false
	}
	now := time.Now()
	server.authMu.Lock()
	defer server.authMu.Unlock()
	server.pruneAuthenticatedSessionsLocked(now)
	for key := range server.authenticatedSessions {
		if key.UIN == uin {
			return true
		}
	}
	return false
}

func (server *Server) updateAuthenticatedSessionProfile(remote string, uin uint32, profile game.PlayerProfile) {
	if server.authenticatedSessions == nil || uin == 0 {
		return
	}
	key := accountAuthGrant{RemoteHost: remoteHost(remote), UIN: uin}
	server.authMu.Lock()
	defer server.authMu.Unlock()
	lease, ok := server.authenticatedSessions[key]
	if !ok || !time.Now().Before(lease.Expires) {
		return
	}
	if profile.GameInfo.RoleID != 0 {
		lease.SelectedRoleID = profile.GameInfo.RoleID
	}
	lease.Profile = clonePlayerProfile(profile)
	if lease.SelectedRoleID != 0 {
		lease.Profile.GameInfo.RoleID = lease.SelectedRoleID
	}
	lease.Expires = time.Now().Add(authenticatedSessionLeaseLifetime)
	server.authenticatedSessions[key] = lease
}

func (server *Server) forgetAuthenticatedSession(remote string, uin uint32) {
	if server.authenticatedSessions == nil || uin == 0 {
		return
	}
	server.authMu.Lock()
	delete(server.authenticatedSessions, accountAuthGrant{RemoteHost: remoteHost(remote), UIN: uin})
	server.authMu.Unlock()
}

func (server *Server) pruneAuthenticatedSessionsLocked(now time.Time) {
	for key, lease := range server.authenticatedSessions {
		if !now.Before(lease.Expires) {
			delete(server.authenticatedSessions, key)
		}
	}
}

func clonePlayerProfile(profile game.PlayerProfile) game.PlayerProfile {
	profile.Inventory = append([]game.ItemInfo(nil), profile.Inventory...)
	return profile
}

func decodeLegacyOnlineHeartbeatUIN(packet []byte) (uint32, bool) {
	if len(packet) != 24 || binary.BigEndian.Uint16(packet[:2]) != uint16(len(packet)) {
		return 0, false
	}
	uin := binary.BigEndian.Uint32(packet[9:13])
	return uin, uin != 0
}

func (session *connectionSession) setUIN(uin uint32) {
	session.UIN = uin
	session.liveUIN.Store(uin)
	if uin == 0 {
		session.livePlayerID.Store(0)
		session.liveSectionID.Store(0)
		session.liveKinIndex.Store(0)
		session.liveRoleID.Store(0)
	} else {
		session.livePlayerID.Store(uint32(session.Profile.PlayerID))
		session.liveSectionID.Store(uint32(session.Profile.SectionID))
		session.liveKinIndex.Store(session.Profile.KinIndex)
		session.liveRoleID.Store(uint32(session.selectedRoleID()))
	}
}

func (session *connectionSession) setRoomID(roomID uint16) {
	session.RoomID = roomID
	session.liveRoomID.Store(uint32(roomID))
}

func (session *connectionSession) notePacket(packet []byte) {
	session.lastPacket.Store(append([]byte(nil), packet...))
}

func (session *connectionSession) packetTemplate() []byte {
	value := session.lastPacket.Load()
	if value == nil {
		return nil
	}
	packet, ok := value.([]byte)
	if !ok {
		return nil
	}
	return append([]byte(nil), packet...)
}

func (server *Server) registerLiveSession(session *connectionSession, connection net.Conn, id, local, remote string) bool {
	session.connection = connection
	session.connectionID = id
	session.localAddress = local
	session.remoteAddress = remote
	session.openedAt = time.Now()
	server.liveMu.Lock()
	defer server.liveMu.Unlock()
	// Close signals done before scanning liveSessions under this same lock.
	// A socket is therefore either in that scan or rejected here, even when
	// its accepted handler was not scheduled until shutdown had begun.
	select {
	case <-server.done:
		return false
	default:
	}
	if server.liveSessions == nil {
		server.liveSessions = make(map[net.Conn]*connectionSession)
	}
	server.liveSessions[connection] = session
	return true
}

// prepareSessionContinuation runs before handleTCPMessage takes session.mu.
// A config-version request is the first identity-bearing frame on both legacy
// replacement hall connections and a type-2 connection to the shop endpoint.
func (server *Server) prepareSessionContinuation(config ListenerConfig, session *connectionSession, connectionID string, packet []byte) {
	if session == nil || !config.Response.QQTLoginSuccess || session.liveUIN.Load() != 0 {
		return
	}
	request, err := game.DecodeLocalConfigFileRequest(packet)
	if err != nil {
		return
	}
	session.notePacket(packet)
	if config.Response.QQTShopType2Session {
		err = server.bindAuxiliaryShopConnection(session, request.UIN)
		if err == nil {
			server.log(logEvent{
				Level: "info", Event: "shop_session_bound", ConnectionID: connectionID,
				AccountID: fmt.Sprint(request.UIN), Result: "config_file_request",
			})
			return
		}
	} else {
		err = server.bindSessionContinuation(session, request.UIN)
	}
	if err != nil {
		event := "session_continuation_rejected"
		if config.Response.QQTShopType2Session {
			event = "shop_session_rejected"
		}
		server.log(logEvent{
			Level: "warn", Event: event, ConnectionID: connectionID,
			AccountID: fmt.Sprint(request.UIN), Result: "config_file_request", ErrorContext: err.Error(),
		})
	}
}

// claimLiveUIN atomically binds one authenticated UIN to one live TCP
// connection. The established session wins; a later duplicate is rejected so
// an in-progress room or match is never displaced by another local client.
func (server *Server) claimLiveUIN(session *connectionSession, uin uint32) bool {
	if session == nil || uin == 0 {
		return false
	}
	if server.hasAuthenticatedSession(uin) {
		return false
	}
	server.liveMu.Lock()
	defer server.liveMu.Unlock()
	current := session.liveUIN.Load()
	if current != 0 && current != uin {
		return false
	}
	for _, active := range server.liveSessions {
		if active != session && active.liveUIN.Load() == uin {
			return false
		}
	}
	session.setUIN(uin)
	return true
}

// bindAuxiliaryShopConnection associates a native type-2 or type-3 shop socket
// with the already authenticated game socket for the same account. It intentionally
// leaves liveUIN at zero: this connection is not a second login, must not
// appear as another lobby player, and must not evict the primary session when
// the shop window closes.
func (server *Server) bindAuxiliaryShopConnection(session *connectionSession, uin uint32) error {
	if session == nil || uin == 0 {
		return fmt.Errorf("shop session requires a non-zero UIN")
	}
	if session.auxiliary {
		if session.UIN != uin {
			return fmt.Errorf("shop session UIN %d cannot be rebound to %d", session.UIN, uin)
		}
		return nil
	}
	server.liveMu.RLock()
	var primary *connectionSession
	for _, active := range server.liveSessions {
		if active != session && active.liveUIN.Load() == uin {
			primary = active
			break
		}
	}
	server.liveMu.RUnlock()
	if primary == nil {
		lease, ok := server.authenticatedSession(session.remoteAddress, uin)
		if !ok {
			return fmt.Errorf("shop UIN %d has no authenticated primary session or online lease", uin)
		}
		session.UIN = uin
		session.replaceProfile(lease.Profile)
		if lease.SelectedRoleID != 0 {
			session.setSelectedRoleID(lease.SelectedRoleID)
		}
		session.auxiliary = true
		return nil
	}
	primaryHost := remoteHost(primary.remoteAddress)
	auxiliaryHost := remoteHost(session.remoteAddress)
	if primaryHost == "" || auxiliaryHost == "" || primaryHost != auxiliaryHost {
		return fmt.Errorf("shop UIN %d auxiliary remote host does not match its authenticated primary session", uin)
	}
	primary.mu.Lock()
	defer primary.mu.Unlock()
	if primary.liveUIN.Load() != uin {
		return fmt.Errorf("shop UIN %d primary session ended during binding", uin)
	}
	session.UIN = uin
	session.replaceProfile(primary.Profile)
	session.setSelectedRoleID(primary.selectedRoleID())
	session.auxiliary = true
	return nil
}

// bindSessionContinuation associates a replacement lobby connection opened by
// a legacy navigation transition with the still-live authenticated connection.
// The replacement deliberately remains non-owning until the old socket closes,
// so normal duplicate-login protection continues to have exactly one owner.
func (server *Server) bindSessionContinuation(session *connectionSession, uin uint32) error {
	if session == nil || uin == 0 {
		return fmt.Errorf("session continuation requires a non-zero UIN")
	}
	session.mu.Lock()
	if session.liveUIN.Load() == uin {
		session.mu.Unlock()
		return nil
	}
	if session.auxiliary || (session.UIN != 0 && session.UIN != uin) {
		session.mu.Unlock()
		return fmt.Errorf("connection cannot continue UIN %d", uin)
	}
	// Publish the candidate before looking up the old owner. If the old socket
	// closes concurrently, its departure path can promote this connection.
	session.UIN = uin
	session.continuation = true
	session.mu.Unlock()
	server.liveMu.RLock()
	var primary *connectionSession
	for _, active := range server.liveSessions {
		if active != session && active.liveUIN.Load() == uin {
			primary = active
			break
		}
	}
	server.liveMu.RUnlock()
	if primary == nil {
		if session.liveUIN.Load() == uin {
			return nil
		}
		lease, ok := server.authenticatedSession(session.remoteAddress, uin)
		if !ok {
			server.rollbackSessionContinuation(session, uin)
			return fmt.Errorf("UIN %d has no authenticated connection or online lease to continue", uin)
		}
		session.mu.Lock()
		session.replaceProfile(lease.Profile)
		if lease.SelectedRoleID != 0 {
			session.setSelectedRoleID(lease.SelectedRoleID)
		}
		session.CurrentMapID = lease.CurrentMapID
		session.CurrentGameID = lease.CurrentGameID
		session.CurrentStageGameID = lease.CurrentStageGameID
		session.RoomID = lease.RoomID
		session.liveRoomID.Store(uint32(lease.RoomID))
		session.liveUIN.Store(uin)
		session.livePlayerID.Store(uint32(session.Profile.PlayerID))
		session.continuation = false
		session.mu.Unlock()
		if lease.Profile.SectionID != 0 {
			_ = server.worldState().EnterLobby(uin, lease.Profile)
		}
		server.log(logEvent{
			Level: "info", Event: "session_online_lease_resumed", ConnectionID: session.connectionID,
			AccountID: fmt.Sprint(uin), Result: "legacy_udp_presence",
		})
		return nil
	}
	if remoteHost(primary.remoteAddress) == "" || remoteHost(primary.remoteAddress) != remoteHost(session.remoteAddress) {
		server.rollbackSessionContinuation(session, uin)
		return fmt.Errorf("UIN %d continuation remote host does not match", uin)
	}
	primary.mu.Lock()
	if session.liveUIN.Load() == uin {
		primary.mu.Unlock()
		return nil
	}
	if primary.liveUIN.Load() != uin {
		primary.mu.Unlock()
		server.rollbackSessionContinuation(session, uin)
		return fmt.Errorf("UIN %d authenticated connection ended during continuation", uin)
	}
	profile := clonePlayerProfile(primary.Profile)
	selectedRoleID := primary.selectedRoleID()
	currentMapID := primary.CurrentMapID
	currentGameID := primary.CurrentGameID
	currentStageGameID := primary.CurrentStageGameID
	roomID := primary.RoomID
	primary.mu.Unlock()

	session.mu.Lock()
	defer session.mu.Unlock()
	session.replaceProfile(profile)
	session.setSelectedRoleID(selectedRoleID)
	session.CurrentMapID = currentMapID
	session.CurrentGameID = currentGameID
	session.CurrentStageGameID = currentStageGameID
	session.RoomID = roomID
	session.liveRoomID.Store(uint32(roomID))
	session.continuation = true
	return nil
}

func (server *Server) rollbackSessionContinuation(session *connectionSession, uin uint32) {
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.liveUIN.Load() == 0 && session.continuation && session.UIN == uin {
		session.UIN = 0
		session.continuation = false
	}
}

// promoteSessionContinuation transfers ownership without emitting a false
// disconnect/room-leave when a legacy navigation socket has already identified
// itself as the same account's replacement. Native shop sockets are auxiliary
// and never invoke this ownership transfer.
//
// Do not infer ownership from remote IP, listener and connection age. Local
// multi-client sessions share all three attributes, so that heuristic can bind
// an unrelated client's fresh socket to the departing account and leave a
// blank-profile ghost in the lobby.
func (server *Server) promoteSessionContinuation(departing *connectionSession, connectionID string) bool {
	if departing == nil || departing.liveUIN.Load() == 0 {
		return false
	}
	uin := departing.liveUIN.Load()
	server.liveMu.RLock()
	candidates := make([]*connectionSession, 0, 1)
	for _, active := range server.liveSessions {
		if active != departing && active.liveUIN.Load() == 0 {
			candidates = append(candidates, active)
		}
	}
	server.liveMu.RUnlock()
	var continuation *connectionSession
	for _, candidate := range candidates {
		candidate.mu.Lock()
		matches := candidate.continuation && candidate.UIN == uin && candidate.liveUIN.Load() == 0
		candidate.mu.Unlock()
		if matches {
			continuation = candidate
			break
		}
	}
	if continuation == nil {
		return false
	}
	continuation.mu.Lock()
	if continuation.liveUIN.Load() != 0 || (continuation.UIN != 0 && continuation.UIN != uin) {
		continuation.mu.Unlock()
		return false
	}
	continuation.UIN = uin
	continuation.continuation = true
	continuation.replaceProfile(departing.Profile)
	continuation.setSelectedRoleID(departing.selectedRoleID())
	continuation.CurrentMapID = departing.CurrentMapID
	continuation.CurrentGameID = departing.CurrentGameID
	continuation.CurrentStageGameID = departing.CurrentStageGameID
	continuation.RoomID = departing.RoomID
	continuation.liveRoomID.Store(departing.liveRoomID.Load())
	continuation.liveUIN.Store(uin)
	continuation.livePlayerID.Store(uint32(continuation.Profile.PlayerID))
	continuation.continuation = false
	continuedBy := continuation.connectionID
	continuation.mu.Unlock()
	departing.liveUIN.Store(0)
	departing.livePlayerID.Store(0)
	server.log(logEvent{
		Level: "info", Event: "session_continuation_promoted", ConnectionID: continuedBy,
		AccountID: fmt.Sprint(uin), Result: "replaced_" + connectionID,
	})
	return true
}

func (server *Server) syncPrimarySessionProfile(source *connectionSession, uin uint32, profile game.PlayerProfile) {
	server.liveMu.RLock()
	primaries := make([]*connectionSession, 0, 1)
	for _, active := range server.liveSessions {
		if active != source && active.liveUIN.Load() == uin {
			primaries = append(primaries, active)
		}
	}
	server.liveMu.RUnlock()
	for _, primary := range primaries {
		// The source request is serialized under source.mu.  Locking a second
		// connection here creates a cycle when the primary and an auxiliary
		// socket mutate the same account concurrently.  Publish the committed
		// snapshot and let the target apply it under its own dispatcher lock.
		if primary.liveUIN.Load() == uin {
			cloned := clonePlayerProfile(profile)
			if primary.mu.TryLock() {
				primary.replaceProfile(cloned)
				primary.mu.Unlock()
			} else {
				primary.pendingProfile.Store(&cloned)
			}
		}
	}
	if source != nil {
		server.updateAuthenticatedSessionProfile(source.remoteAddress, uin, profile)
	}
}

func (server *Server) unregisterLiveSession(connection net.Conn) {
	server.liveMu.Lock()
	session := server.liveSessions[connection]
	delete(server.liveSessions, connection)
	uin := uint32(0)
	if session != nil {
		uin = session.liveUIN.Load()
	}
	identityStillLive := false
	if uin != 0 {
		for _, active := range server.liveSessions {
			if active.liveUIN.Load() == uin {
				identityStillLive = true
				break
			}
		}
	}
	server.liveMu.Unlock()
	if uin != 0 && !identityStillLive {
		server.clearRoomPeerUDPIdentity(uin)
	}
}

func (server *Server) liveSessionForConnection(connection net.Conn) *connectionSession {
	server.liveMu.RLock()
	session := server.liveSessions[connection]
	server.liveMu.RUnlock()
	return session
}

// liveRoomRoutingSessions returns the lock-free transport identities currently
// published in one room. A room owns at most one active match, and joining an
// in-match room is rejected by the authoritative room state, so room identity
// is sufficient for the AI runtime's native peer fan-out.
//
// Keep this path independent of session.mu. The AI world loop calls it while
// holding runtime.mu; an in-flight REQUEST_LEAVE_ROOM owns session.mu and must
// acquire runtime.mu to retire the human actor. Waiting for session.mu here
// creates the exact inverse lock order and leaves both the room and live UIN
// permanently retained.
func (server *Server) liveRoomRoutingSessions(roomID uint16) []*connectionSession {
	if roomID == 0 {
		return nil
	}
	server.liveMu.RLock()
	members := make([]*connectionSession, 0, len(server.liveSessions))
	for _, session := range server.liveSessions {
		if session.liveRoomID.Load() == uint32(roomID) && session.liveUIN.Load() != 0 && session.livePlayerID.Load() != 0 {
			members = append(members, session)
		}
	}
	server.liveMu.RUnlock()
	return members
}

// liveRoomMatchSessions is a settlement snapshot and may wait for each current
// participant's dispatcher mutex. Callers must not already own any session
// mutex; settlement entry points intentionally enforce that boundary.
func (server *Server) liveRoomMatchSessions(roomID uint16, gameID uint32) []*connectionSession {
	if roomID == 0 || gameID == 0 {
		return nil
	}
	server.liveMu.RLock()
	candidates := make([]*connectionSession, 0, len(server.liveSessions))
	for _, session := range server.liveSessions {
		if session.liveRoomID.Load() == uint32(roomID) && session.liveUIN.Load() != 0 {
			candidates = append(candidates, session)
		}
	}
	server.liveMu.RUnlock()
	members := make([]*connectionSession, 0, len(candidates))
	for _, session := range candidates {
		session.mu.Lock()
		matches := session.liveRoomID.Load() == uint32(roomID) && session.liveUIN.Load() != 0 && session.CurrentGameID == gameID
		session.mu.Unlock()
		if matches {
			members = append(members, session)
		}
	}
	return members
}

// broadcastRoomNotification sends one independently encrypted notification to
// every other live connection currently projected into the room. Each packet
// is based on that recipient's own recent authenticated envelope, so the outer
// UIN is never copied from the actor connection.
func (server *Server) broadcastRoomNotification(roomID uint16, actorUIN uint32, result string, build func(recipientPacket []byte) ([]byte, error)) {
	server.broadcastRoomNotificationWhere(roomID, actorUIN, result, nil, build)
}

func (server *Server) broadcastRoomNotificationWhere(roomID uint16, actorUIN uint32, result string, include func(*connectionSession) bool, build func(recipientPacket []byte) ([]byte, error)) {
	if roomID == 0 || build == nil {
		return
	}
	server.liveMu.RLock()
	candidates := make([]*connectionSession, 0, len(server.liveSessions))
	for _, session := range server.liveSessions {
		if session.liveRoomID.Load() != uint32(roomID) || session.liveUIN.Load() == actorUIN {
			continue
		}
		candidates = append(candidates, session)
	}
	server.liveMu.RUnlock()
	recipients := make([]*connectionSession, 0, len(candidates))
	for _, session := range candidates {
		// Routing identity is maintained in atomics.  Do not lock a recipient
		// while a request actor may still be serialized: simultaneous room
		// notifications in opposite directions would otherwise form an ABBA
		// cycle.  include callbacks must inspect only atomic/live immutable data.
		included := session.liveRoomID.Load() == uint32(roomID) && session.liveUIN.Load() != 0 && session.liveUIN.Load() != actorUIN && (include == nil || include(session))
		if included {
			recipients = append(recipients, session)
		}
	}
	for _, recipient := range recipients {
		template := recipient.packetTemplate()
		if len(template) == 0 {
			server.log(logEvent{
				Level: "warn", Event: "room_notification_skipped", ConnectionID: recipient.connectionID,
				RoomID: fmt.Sprint(roomID), Result: result, ErrorContext: "recipient has no local packet template",
			})
			continue
		}
		packet, err := build(template)
		if err != nil {
			server.log(logEvent{
				Level: "error", Event: "room_notification_build_failed", ConnectionID: recipient.connectionID,
				RoomID: fmt.Sprint(roomID), Result: result, ErrorContext: err.Error(),
			})
			continue
		}
		server.writeTCP(recipient.connection, recipient.connectionID, recipient.localAddress, recipient.remoteAddress, packet, result)
	}
}

func (server *Server) broadcastSectionNotification(sectionID uint16, actorUIN uint32, result string, destinationUIN uint32, build func(recipientPacket []byte) ([]byte, error)) {
	if sectionID == 0 || build == nil {
		return
	}
	server.liveMu.RLock()
	candidates := make([]*connectionSession, 0, len(server.liveSessions))
	for _, session := range server.liveSessions {
		uin := session.liveUIN.Load()
		if uin == 0 || uin == actorUIN {
			continue
		}
		if destinationUIN != 0 && uin != destinationUIN {
			continue
		}
		candidates = append(candidates, session)
	}
	server.liveMu.RUnlock()
	recipients := make([]*connectionSession, 0, len(candidates))
	for _, session := range candidates {
		if session.liveUIN.Load() != 0 && session.liveUIN.Load() != actorUIN && session.routingSectionID() == sectionID {
			recipients = append(recipients, session)
		}
	}
	server.sendChatNotifications(recipients, result, build)
}

func (server *Server) broadcastDirectNotification(actorUIN, destinationUIN uint32, result string, build func(recipientPacket []byte) ([]byte, error)) {
	if destinationUIN == actorUIN {
		return
	}
	server.sendUINNotification(destinationUIN, result, build)
}

// sendUINNotification is the profile-subsystem equivalent of a direct push.
// Unlike broadcastDirectNotification it intentionally permits delivery back
// to the current actor, which is required when a list response is followed by
// one FRIEND_INFO update per entry.
func (server *Server) sendUINNotification(destinationUIN uint32, result string, build func(recipientPacket []byte) ([]byte, error)) {
	if destinationUIN == 0 || build == nil {
		return
	}
	server.liveMu.RLock()
	recipients := make([]*connectionSession, 0, 1)
	for _, session := range server.liveSessions {
		if session.liveUIN.Load() == destinationUIN {
			recipients = append(recipients, session)
			break
		}
	}
	server.liveMu.RUnlock()
	server.sendChatNotifications(recipients, result, build)
}

func (server *Server) sendChatNotifications(recipients []*connectionSession, result string, build func(recipientPacket []byte) ([]byte, error)) {
	for _, recipient := range recipients {
		template := recipient.packetTemplate()
		if len(template) == 0 {
			continue
		}
		packet, err := build(template)
		if err != nil {
			server.log(logEvent{Level: "error", Event: "chat_notification_build_failed", ConnectionID: recipient.connectionID, Result: result, ErrorContext: err.Error()})
			continue
		}
		server.writeTCP(recipient.connection, recipient.connectionID, recipient.localAddress, recipient.remoteAddress, packet, result)
	}
}

// projectRoomMatch places every live room member onto the same authoritative
// match identity. Previously only the owner connection received CurrentGameID,
// which made every later event from a second client fail battle lookup.
func (server *Server) projectRoomMatch(roomID uint16, gameID, mapID uint32) {
	server.projectRoomMatchPlayers(roomID, gameID, mapID, nil)
}

// projectRoomMatchFromLockedSession is the in-dispatch variant. The TCP
// dispatcher serializes the actor under actor.mu, so attempting to lock that
// same session again deadlocks after the room has already transitioned to
// PhaseInMatch. That left the lobby advertising an active match while neither
// START_GAME nor GAME_BEGIN could be written to the clients.
func (server *Server) projectRoomMatchFromLockedSession(actor *connectionSession, roomID uint16, gameID, mapID uint32, playerIDs map[uint16]struct{}) {
	server.projectRoomMatchSessions(actor, roomID, gameID, mapID, playerIDs)
}

func (server *Server) projectRoomMatchPlayers(roomID uint16, gameID, mapID uint32, playerIDs map[uint16]struct{}) {
	server.projectRoomMatchSessions(nil, roomID, gameID, mapID, playerIDs)
}

func (server *Server) projectRoomMatchSessions(lockedActor *connectionSession, roomID uint16, gameID, mapID uint32, playerIDs map[uint16]struct{}) {
	if roomID == 0 || gameID == 0 || mapID == 0 {
		return
	}
	server.liveMu.RLock()
	candidates := make([]*connectionSession, 0, len(server.liveSessions))
	for _, session := range server.liveSessions {
		if session.liveRoomID.Load() == uint32(roomID) && session.liveUIN.Load() != 0 {
			candidates = append(candidates, session)
		}
	}
	server.liveMu.RUnlock()
	for _, member := range candidates {
		if member == lockedActor {
			_, selected := playerIDs[member.Profile.PlayerID]
			if playerIDs != nil && !selected {
				continue
			}
			member.CurrentGameID = gameID
			member.CurrentStageGameID = gameID
			member.CurrentMapID = mapID
			continue
		}
		playerID := member.routingPlayerID()
		_, selected := playerIDs[playerID]
		if playerIDs != nil && !selected {
			continue
		}
		member.applyOrEnqueueProjection(sessionProjection{
			kind: sessionProjectionStartMatch, roomID: roomID,
			gameID: gameID, stageGameID: gameID, mapID: mapID,
		})
	}
}

// projectRoomAdventureStage advances only the client-facing gameplay identity.
// The stable match ID remains unchanged so battle lookup, room ownership and
// settlement continue to describe one multi-stage adventure.
func (server *Server) projectRoomAdventureStageFromLockedSession(lockedActor *connectionSession, roomID uint16, matchGameID, stageGameID, mapID uint32, playerIDs map[uint16]struct{}) {
	if roomID == 0 || matchGameID == 0 || stageGameID == 0 || mapID == 0 {
		return
	}
	server.liveMu.RLock()
	candidates := make([]*connectionSession, 0, len(server.liveSessions))
	for _, session := range server.liveSessions {
		if session.liveRoomID.Load() == uint32(roomID) && session.liveUIN.Load() != 0 {
			candidates = append(candidates, session)
		}
	}
	server.liveMu.RUnlock()
	for _, member := range candidates {
		if member == lockedActor {
			_, selected := playerIDs[member.Profile.PlayerID]
			if member.CurrentGameID != matchGameID || playerIDs != nil && !selected {
				continue
			}
			member.CurrentStageGameID = stageGameID
			member.CurrentMapID = mapID
			continue
		}
		playerID := member.routingPlayerID()
		_, selected := playerIDs[playerID]
		if playerIDs != nil && !selected {
			continue
		}
		member.applyOrEnqueueProjection(sessionProjection{
			kind: sessionProjectionAdvanceStage, roomID: roomID,
			gameID: matchGameID, stageGameID: stageGameID, mapID: mapID,
		})
	}
}

// broadcastMatchNotification re-envelopes a server notification for each
// recipient. The payload is identical, while the outer identity and transport
// correlation always come from that recipient's own authenticated packet.
func (server *Server) broadcastMatchNotification(roomID uint16, actorUIN uint32, result string, notification []byte) {
	server.broadcastMatchNotificationToPlayers(roomID, actorUIN, result, notification, nil)
}

func (server *Server) broadcastMatchNotificationToPlayers(roomID uint16, actorUIN uint32, result string, notification []byte, playerIDs map[uint16]struct{}) {
	if len(notification) == 0 {
		return
	}
	server.broadcastRoomNotificationWhere(roomID, actorUIN, result, func(session *connectionSession) bool {
		if playerIDs == nil {
			return true
		}
		_, ok := playerIDs[uint16(session.livePlayerID.Load())]
		return ok
	}, func(recipient []byte) ([]byte, error) {
		return game.RebindLocalServerNotification(recipient, notification)
	})
}
