package probe

import (
	"encoding/binary"
	"io"
	"net"
	"path/filepath"
	"testing"
	"time"

	"qqtang/internal/protocol/capture"
	"qqtang/internal/protocol/game"
	"qqtang/internal/server/persistence"
)

func TestRoomChatSendsDedicatedContentNotificationToSelfAndPeer(t *testing.T) {
	const actorUIN uint32 = 1_000_001
	const peerUIN uint32 = 1_000_002
	payload := make([]byte, game.RoomChatHeaderSize+5)
	binary.BigEndian.PutUint32(payload[0:4], actorUIN)
	binary.BigEndian.PutUint32(payload[4:8], 123)
	binary.BigEndian.PutUint16(payload[10:12], 5)
	copy(payload[12:], "hello")
	request := testLocalRoutedPacketWithPayload(t, game.RoomChatCommand, 3, 0xffff, 1, actorUIN, payload)
	peerPayload := append([]byte(nil), payload...)
	binary.BigEndian.PutUint32(peerPayload[0:4], peerUIN)
	peerTemplate := testLocalRoutedPacketWithPayload(t, game.RoomChatCommand, 3, 0xffff, 2, peerUIN, peerPayload)

	actorServer, actorClient := net.Pipe()
	peerServer, peerClient := net.Pipe()
	defer actorServer.Close()
	defer actorClient.Close()
	defer peerServer.Close()
	defer peerClient.Close()
	actor := newChatSession(actorUIN, 1, 1, 1, actorServer, request)
	peer := newChatSession(peerUIN, 2, 1, 1, peerServer, peerTemplate)
	server := newChatTestServer(t, map[net.Conn]*connectionSession{actorServer: actor, peerServer: peer})
	result := server.handleChatMessage(actor, "room-chat", request)
	if !result.handled || len(result.response) == 0 || result.postResponse == nil {
		t.Fatalf("room chat result = handled:%t response:%d post:%t", result.handled, len(result.response), result.postResponse != nil)
	}
	actorRead, peerRead := readChatPacket(actorClient), readChatPacket(peerClient)
	result.postResponse()
	for label, read := range map[string]<-chan chatPacketRead{"self": actorRead, "peer": peerRead} {
		got := <-read
		if got.err != nil {
			t.Fatalf("%s read: %v", label, got.err)
		}
		message, err := game.InspectLocalPacket(got.packet)
		if err != nil {
			t.Fatalf("%s inspect: %v", label, err)
		}
		wireContent := []byte("LocalPlayer\xcb\xb5:hello")
		if message.Command != game.RoomChatNotifyCommand || len(message.Payload) != 6+len(wireContent) || binary.BigEndian.Uint16(message.Payload[0:2]) != 1 || string(message.Payload[6:]) != string(wireContent) {
			t.Fatalf("%s room chat notification = %+v payload=%x", label, message, message.Payload)
		}
	}
}

type chatPacketRead struct {
	packet []byte
	err    error
}

func readChatPacket(connection net.Conn) <-chan chatPacketRead {
	result := make(chan chatPacketRead, 1)
	go func() {
		header := make([]byte, 4)
		if _, err := io.ReadFull(connection, header); err != nil {
			result <- chatPacketRead{err: err}
			return
		}
		length := int(binary.BigEndian.Uint32(header))
		if length < len(header) {
			result <- chatPacketRead{err: io.ErrUnexpectedEOF}
			return
		}
		packet := make([]byte, length)
		copy(packet, header)
		if _, err := io.ReadFull(connection, packet[4:]); err != nil {
			result <- chatPacketRead{err: err}
			return
		}
		result <- chatPacketRead{packet: packet}
	}()
	return result
}

func readChatPackets(connection net.Conn, count int) <-chan chatPacketRead {
	result := make(chan chatPacketRead, count)
	go func() {
		for index := 0; index < count; index++ {
			header := make([]byte, 4)
			if _, err := io.ReadFull(connection, header); err != nil {
				result <- chatPacketRead{err: err}
				return
			}
			length := int(binary.BigEndian.Uint32(header))
			if length < len(header) {
				result <- chatPacketRead{err: io.ErrUnexpectedEOF}
				return
			}
			packet := make([]byte, length)
			copy(packet, header)
			if _, err := io.ReadFull(connection, packet[4:]); err != nil {
				result <- chatPacketRead{err: err}
				return
			}
			result <- chatPacketRead{packet: packet}
		}
	}()
	return result
}

func newChatSession(uin uint32, playerID, sectionID, roomID uint16, connection net.Conn, template []byte) *connectionSession {
	profile := game.DefaultPlayerProfile()
	profile.PlayerID = playerID
	profile.SectionID = sectionID
	session := &connectionSession{
		UIN: uin, Profile: profile, RoomID: roomID, connection: connection,
		connectionID: "chat", localAddress: "local", remoteAddress: "remote",
	}
	session.liveUIN.Store(uin)
	session.livePlayerID.Store(uint32(playerID))
	session.liveSectionID.Store(uint32(sectionID))
	session.liveRoomID.Store(uint32(roomID))
	session.notePacket(template)
	return session
}

func newChatTestServer(t *testing.T, sessions map[net.Conn]*connectionSession) *Server {
	t.Helper()
	captureWriter, err := capture.Open(filepath.Join(t.TempDir(), "chat.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = captureWriter.Close() })
	server := &Server{capture: captureWriter, logWriter: io.Discard, liveSessions: sessions}
	for _, session := range sessions {
		if err := server.worldState().EnterLobby(sessionWorldUIN(session), session.Profile); err != nil {
			t.Fatal(err)
		}
	}
	return server
}

func TestSectionChatSendsDedicatedContentNotificationToSelfAndPeer(t *testing.T) {
	const actorUIN uint32 = 1_000_001
	const peerUIN uint32 = 1_000_002
	payload := make([]byte, game.SectionChatHeaderSize+5)
	binary.BigEndian.PutUint32(payload[0:4], actorUIN)
	binary.BigEndian.PutUint32(payload[4:8], 123)
	binary.BigEndian.PutUint16(payload[8:10], game.SectionChatBroadcastPlayerID)
	binary.BigEndian.PutUint32(payload[10:14], game.SectionChatPublicUIN)
	binary.BigEndian.PutUint16(payload[14:16], 5)
	copy(payload[16:], "hello")
	request := testLocalRoutedPacketWithPayload(t, game.SectionChatCommand, 3, 0xffff, 1, actorUIN, payload)
	peerPayload := append([]byte(nil), payload...)
	binary.BigEndian.PutUint32(peerPayload[0:4], peerUIN)
	peerTemplate := testLocalRoutedPacketWithPayload(t, game.SectionChatCommand, 3, 0xffff, 2, peerUIN, peerPayload)

	actorServer, actorClient := net.Pipe()
	peerServer, peerClient := net.Pipe()
	defer actorServer.Close()
	defer actorClient.Close()
	defer peerServer.Close()
	defer peerClient.Close()
	actor := newChatSession(actorUIN, 1, 1, 0, actorServer, request)
	peer := newChatSession(peerUIN, 2, 1, 0, peerServer, peerTemplate)
	server := newChatTestServer(t, map[net.Conn]*connectionSession{actorServer: actor, peerServer: peer})
	// The connection snapshot can lag after reconnect. Lobby-visible identity
	// must still come from the authoritative section member table.
	actor.Profile.PlayerID = 99
	result := server.handleChatMessage(actor, "section-chat", request)
	if !result.handled || len(result.response) == 0 || result.postResponse == nil {
		t.Fatalf("section chat result = handled:%t response:%d post:%t", result.handled, len(result.response), result.postResponse != nil)
	}
	actorRead, peerRead := readChatPacket(actorClient), readChatPacket(peerClient)
	result.postResponse()
	for label, read := range map[string]<-chan chatPacketRead{"self": actorRead, "peer": peerRead} {
		got := <-read
		if got.err != nil {
			t.Fatalf("%s read: %v", label, got.err)
		}
		message, err := game.InspectLocalPacket(got.packet)
		if err != nil {
			t.Fatalf("%s inspect: %v", label, err)
		}
		wireContent := []byte("LocalPlayer\xcb\xb5:hello")
		metadataOffset := 6 + len(wireContent)
		if message.Command != game.SectionChatNotifyCommand || len(message.Payload) != metadataOffset+24 || binary.BigEndian.Uint16(message.Payload[0:2]) != 1 || string(message.Payload[6:metadataOffset]) != string(wireContent) || binary.BigEndian.Uint32(message.Payload[metadataOffset:metadataOffset+4]) != actorUIN {
			t.Fatalf("%s section notification = %+v payload=%x", label, message, message.Payload)
		}
	}
}

func TestSmallBugleConsumesItemAndMarksBroadcast(t *testing.T) {
	const actorUIN uint32 = 1_000_001
	const peerUIN uint32 = 1_000_002
	payload := make([]byte, game.SectionChatHeaderSize+5)
	binary.BigEndian.PutUint32(payload[0:4], actorUIN)
	binary.BigEndian.PutUint32(payload[4:8], 123)
	binary.BigEndian.PutUint16(payload[8:10], game.SectionChatBroadcastPlayerID)
	binary.BigEndian.PutUint32(payload[10:14], game.SectionChatSmallBugleUIN)
	binary.BigEndian.PutUint16(payload[14:16], 5)
	copy(payload[16:], "hello")
	request := testLocalRoutedPacketWithPayload(t, game.SectionChatCommand, 3, 0xffff, 1, actorUIN, payload)
	peerPayload := append([]byte(nil), payload...)
	binary.BigEndian.PutUint32(peerPayload[0:4], peerUIN)
	peerTemplate := testLocalRoutedPacketWithPayload(t, game.SectionChatCommand, 3, 0xffff, 2, peerUIN, peerPayload)

	actorServer, actorClient := net.Pipe()
	peerServer, peerClient := net.Pipe()
	defer actorServer.Close()
	defer actorClient.Close()
	defer peerServer.Close()
	defer peerClient.Close()
	actor := newChatSession(actorUIN, 1, 1, 0, actorServer, request)
	peer := newChatSession(peerUIN, 2, 1, 0, peerServer, peerTemplate)
	server := newChatTestServer(t, map[net.Conn]*connectionSession{actorServer: actor, peerServer: peer})
	store, err := persistence.OpenPlayerStore(filepath.Join(t.TempDir(), "players.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	server.playerStore = store
	actor.Profile.Inventory = []game.ItemInfo{game.NewPermanentItemInfo(game.SmallBugleItemID, 2)}
	if err = store.Save(t.Context(), actorUIN, actor.Profile); err != nil {
		t.Fatal(err)
	}

	result := server.handleChatMessage(actor, "section-bugle", request)
	if !result.handled || len(result.response) == 0 || result.postResponse == nil {
		t.Fatalf("bugle result = handled:%t response:%d post:%t", result.handled, len(result.response), result.postResponse != nil)
	}
	stored, err := store.Load(t.Context(), actorUIN)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.Inventory) != 1 || stored.Inventory[0].ItemID != game.SmallBugleItemID || stored.Inventory[0].NumOfItem != 1 {
		t.Fatalf("stored bugle inventory = %+v", stored.Inventory)
	}

	actorReads := readChatPackets(actorClient, 2)
	peerRead := readChatPacket(peerClient)
	result.postResponse()
	actorInventory := <-actorReads
	if actorInventory.err != nil {
		t.Fatal(actorInventory.err)
	}
	inventoryPacket, err := game.InspectLocalPacket(actorInventory.packet)
	if err != nil {
		t.Fatal(err)
	}
	if inventoryPacket.Command != game.PlayerItemAddNotifyCommand {
		t.Fatalf("bugle inventory command = 0x%04X", inventoryPacket.Command)
	}
	inventory, err := game.ParsePlayerItemAddNotification(inventoryPacket.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(inventory.Items) != 1 || inventory.Items[0].ItemID != game.SmallBugleItemID || inventory.Items[0].NumOfItem != 1 {
		t.Fatalf("bugle inventory notification = %+v", inventory)
	}

	actorChat := make(chan chatPacketRead, 1)
	actorChat <- <-actorReads
	for label, read := range map[string]<-chan chatPacketRead{"self": actorChat, "peer": peerRead} {
		got := <-read
		if got.err != nil {
			t.Fatalf("%s read: %v", label, got.err)
		}
		inspection, inspectErr := game.InspectLocalPacket(got.packet)
		if inspectErr != nil {
			t.Fatal(inspectErr)
		}
		wireContent := []byte("LocalPlayer\xcb\xb5:hello")
		metadataOffset := 6 + len(wireContent)
		if inspection.Command != game.SectionChatNotifyCommand ||
			binary.BigEndian.Uint32(inspection.Payload[metadataOffset+4:metadataOffset+8])&game.SectionChatSmallBugleIdentity == 0 {
			t.Fatalf("%s bugle notification = %+v payload=%x", label, inspection, inspection.Payload)
		}
	}
}

func TestAcrossSectionChatReliesOnResponseForSelfAndNotifiesDestination(t *testing.T) {
	const actorUIN uint32 = 1_000_001
	const peerUIN uint32 = 1_000_002
	payload := make([]byte, game.AcrossSectionChatHeaderSize+5)
	binary.BigEndian.PutUint32(payload[0:4], actorUIN)
	binary.BigEndian.PutUint32(payload[4:8], 123)
	binary.BigEndian.PutUint32(payload[8:12], 1)
	binary.BigEndian.PutUint16(payload[12:14], 2)
	binary.BigEndian.PutUint32(payload[14:18], peerUIN)
	binary.BigEndian.PutUint16(payload[18:20], 5)
	copy(payload[20:], "hello")
	request := testLocalRoutedPacketWithPayload(t, game.ChatAcrossSectionCommand, 3, 0xffff, 1, actorUIN, payload)
	peerPayload := append([]byte(nil), payload...)
	binary.BigEndian.PutUint32(peerPayload[0:4], peerUIN)
	peerTemplate := testLocalRoutedPacketWithPayload(t, game.ChatAcrossSectionCommand, 3, 0xffff, 2, peerUIN, peerPayload)

	actorServer, actorClient := net.Pipe()
	peerServer, peerClient := net.Pipe()
	defer actorServer.Close()
	defer actorClient.Close()
	defer peerServer.Close()
	defer peerClient.Close()
	actor := newChatSession(actorUIN, 1, 1, 0, actorServer, request)
	actor.Profile.Nickname = "糖一"
	peer := newChatSession(peerUIN, 2, 2, 0, peerServer, peerTemplate)
	server := newChatTestServer(t, map[net.Conn]*connectionSession{actorServer: actor, peerServer: peer})
	result := server.handleChatMessage(actor, "across-chat", request)
	if !result.handled || len(result.response) == 0 || result.postResponse == nil {
		t.Fatalf("across chat result = handled:%t response:%d post:%t", result.handled, len(result.response), result.postResponse != nil)
	}
	if err := actorClient.SetReadDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	actorRead := readChatPacket(actorClient)
	peerRead := readChatPacket(peerClient)
	result.postResponse()
	peerMessage := <-peerRead
	if peerMessage.err != nil {
		t.Fatal(peerMessage.err)
	}
	inspection, err := game.InspectLocalPacket(peerMessage.packet)
	if err != nil {
		t.Fatal(err)
	}
	if inspection.Command != game.AcrossSectionChatNotifyCommand || len(inspection.Payload) != 43 || binary.BigEndian.Uint32(inspection.Payload[0:4]) != actorUIN || string(inspection.Payload[6:11]) != "hello" {
		t.Fatalf("across destination notification = %+v payload=%x", inspection, inspection.Payload)
	}
	if actorMessage := <-actorRead; actorMessage.err == nil {
		t.Fatalf("across sender received duplicate server notification: %x", actorMessage.packet)
	} else if networkErr, ok := actorMessage.err.(net.Error); !ok || !networkErr.Timeout() {
		t.Fatalf("across sender read error = %v, want timeout", actorMessage.err)
	}
}
