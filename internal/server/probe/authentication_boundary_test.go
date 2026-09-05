package probe

import (
	"encoding/binary"
	"io"
	"testing"

	roomstate "qqtang/internal/game/room"
	"qqtang/internal/protocol/game"
)

func TestLoginUtilityCommandsCannotBypassBusinessAuthentication(t *testing.T) {
	for _, auxiliary := range []bool{false, true} {
		name := "foreign-envelope"
		packetUIN := uint32(1000002)
		if auxiliary {
			name = "shop-auxiliary"
			packetUIN = 1000001
		}
		t.Run(name, func(t *testing.T) {
			payload := make([]byte, 9)
			binary.BigEndian.PutUint32(payload, packetUIN)
			packet := testLocalRoutedPacketWithPayload(t, game.LogoutCommand, 2, 0xffff, 1, packetUIN, payload)
			server := &Server{logWriter: io.Discard, authenticationState: newAuthenticationState()}
			session := &connectionSession{UIN: 1000001, Profile: game.DefaultPlayerProfile(), auxiliary: auxiliary}
			if !auxiliary {
				session.liveUIN.Store(session.UIN)
			}
			outcome := server.dispatchTCPMessage(ListenerConfig{Response: ResponseConfig{QQTLoginSuccess: true, QQTShopType2Session: auxiliary}}, session, "utility-auth", "local", "127.0.0.1:40000", packet)
			if outcome.keepConnection || len(outcome.response) != 0 || session.UIN != 1000001 {
				t.Fatalf("logout utility bypassed authentication: keep=%t response=%d sessionUIN=%d", outcome.keepConnection, len(outcome.response), session.UIN)
			}
		})
	}
}

func TestUnauthenticatedConnectionCannotCreateRoom(t *testing.T) {
	const forgedUIN uint32 = 1_000_001
	payload := make([]byte, 50)
	binary.BigEndian.PutUint32(payload[0:4], forgedUIN)
	copy(payload[8:28], []byte("unauthorized"))
	payload[45] = byte(roomstate.GameTypeAdventure)
	packet := testLocalRoutedPacketWithPayload(t, game.CreateRoomCommand, 2, 0xffff, 1, forgedUIN, payload)
	server := &Server{logWriter: io.Discard}
	session := &connectionSession{Profile: game.DefaultPlayerProfile()}
	outcome := server.dispatchTCPMessage(ListenerConfig{Response: ResponseConfig{
		QQTLoginSuccess: true, QQTCreateRoomSuccess: true,
	}}, session, "unauthenticated-create", "127.0.0.1:18000", "192.0.2.10:40000", packet)
	if outcome.keepConnection || !outcome.handledWithoutResponse || session.RoomID != 0 {
		t.Fatalf("unauthenticated create outcome=%+v room=%d", outcome, session.RoomID)
	}
}

func TestAuthenticatedConnectionCannotUseAnotherEnvelopeUIN(t *testing.T) {
	const sessionUIN uint32 = 1_000_001
	const forgedUIN uint32 = 1_000_002
	payload := make([]byte, 50)
	binary.BigEndian.PutUint32(payload[0:4], forgedUIN)
	copy(payload[8:28], []byte("forged"))
	payload[45] = byte(roomstate.GameTypeAdventure)
	packet := testLocalRoutedPacketWithPayload(t, game.CreateRoomCommand, 2, 0xffff, 1, forgedUIN, payload)
	server := &Server{logWriter: io.Discard}
	session := &connectionSession{UIN: sessionUIN, Profile: game.DefaultPlayerProfile()}
	session.liveUIN.Store(sessionUIN)
	outcome := server.dispatchTCPMessage(ListenerConfig{Response: ResponseConfig{
		QQTLoginSuccess: true, QQTCreateRoomSuccess: true,
	}}, session, "forged-create", "127.0.0.1:18000", "192.0.2.10:40000", packet)
	if outcome.keepConnection || !outcome.handledWithoutResponse || session.RoomID != 0 {
		t.Fatalf("forged create outcome=%+v room=%d", outcome, session.RoomID)
	}
}

func TestUnauthenticatedDirectoryDiscoveryKeepsFreshGameSocketOpen(t *testing.T) {
	for _, sectionID := range []uint16{localDirectoryRequestSectionID, localDirectoryProbeRequestSectionID} {
		const uin uint32 = 1_000_001
		packet := testLocalRoutedPacket(t, localDirectoryRequestCommand, localDirectoryRequestRoute, localDirectoryRequestMarker, sectionID, uin)
		server := &Server{logWriter: io.Discard}
		session := &connectionSession{Profile: game.DefaultPlayerProfile()}
		outcome := server.dispatchTCPMessage(ListenerConfig{Response: ResponseConfig{
			QQTLoginSuccess: true,
		}}, session, "directory-probe", "127.0.0.1:18000", "192.0.2.10:40000", packet)
		if !outcome.keepConnection || len(outcome.response) != 0 || session.UIN != 0 {
			t.Fatalf("directory probe section 0x%04X outcome=%+v session UIN=%d", sectionID, outcome, session.UIN)
		}
	}
}

func TestUnauthenticatedDirectoryLookalikeStillClosesGameSocket(t *testing.T) {
	const uin uint32 = 1_000_001
	packet := testLocalRoutedPacket(t, localDirectoryRequestCommand, localDirectoryRequestRoute+1, localDirectoryRequestMarker, localDirectoryRequestSectionID, uin)
	server := &Server{logWriter: io.Discard}
	session := &connectionSession{Profile: game.DefaultPlayerProfile()}
	outcome := server.dispatchTCPMessage(ListenerConfig{Response: ResponseConfig{
		QQTLoginSuccess: true,
	}}, session, "directory-lookalike", "127.0.0.1:18000", "192.0.2.10:40000", packet)
	if outcome.keepConnection || !outcome.handledWithoutResponse {
		t.Fatalf("directory lookalike outcome=%+v", outcome)
	}
}

func TestFreshGameSocketAnswersVerifiedDirectoryProbeImmediately(t *testing.T) {
	const uin uint32 = 1_000_001
	packet := testLocalRoutedPacket(t, localDirectoryRequestCommand, localDirectoryRequestRoute, localDirectoryRequestMarker, localDirectoryProbeRequestSectionID, uin)
	server := &Server{logWriter: io.Discard, directoryHallPayload: []byte{0x08, 0x16, 0, 0}}
	session := &connectionSession{Profile: game.DefaultPlayerProfile()}
	outcome := server.dispatchTCPMessage(ListenerConfig{Response: ResponseConfig{
		QQTDirectoryResponse: true,
		QQTLoginSuccess:      true,
	}}, session, "directory-game", "127.0.0.1:18000", "192.0.2.10:40000", packet)
	if !outcome.keepConnection || len(outcome.response) == 0 || outcome.result != "qqt_directory_response" {
		t.Fatalf("directory game response outcome=%+v", outcome)
	}
	inspection, err := game.InspectLocalPacket(outcome.response)
	if err != nil {
		t.Fatal(err)
	}
	if inspection.EnvelopeUIN != uin || inspection.Command != localDirectoryRequestCommand ||
		inspection.Route != localDirectoryRequestRoute || inspection.SectionID != localDirectoryProbeRequestSectionID {
		t.Fatalf("directory game response inspection=%+v", inspection)
	}
}
