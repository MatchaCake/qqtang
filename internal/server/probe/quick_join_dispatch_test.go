package probe

import (
	"encoding/binary"
	"io"
	"testing"

	roomstate "qqtang/internal/game/room"
	"qqtang/internal/protocol/game"
)

func TestEmptyQuickJoinRespondsAndLeavesCreateRoomAvailable(t *testing.T) {
	const uin uint32 = 1_000_001
	server := &Server{logWriter: io.Discard}
	profile := game.DefaultPlayerProfile()
	session := &connectionSession{UIN: uin, Profile: profile}
	if err := server.worldState().EnterLobby(uin, profile); err != nil {
		t.Fatal(err)
	}

	joinPayload := make([]byte, 9)
	binary.BigEndian.PutUint32(joinPayload[0:4], uin)
	joinPayload[8] = byte(roomstate.GameTypeCompetitiveNoItem)
	joinPacket := testLocalRoutedPacketWithPayload(t, game.JoinRoomCommand, 2, 0xFFFF, 1, uin, joinPayload)
	joinResult := server.handleLobbyMessage(ListenerConfig{Response: ResponseConfig{QQTRoomList: true}}, session, "empty-quick-join", joinPacket)
	if !joinResult.handled || len(joinResult.response) == 0 || session.RoomID != 0 {
		t.Fatalf("empty quick join result=%+v room=%d", joinResult, session.RoomID)
	}
	inspection, err := game.InspectLocalPacket(joinResult.response)
	if err != nil {
		t.Fatal(err)
	}
	if inspection.Command != game.JoinRoomCommand || game.EnterRoomResultID(binary.BigEndian.Uint16(inspection.Payload[0:2])) != game.EnterRoomResultRoomUnavailable {
		t.Fatalf("empty quick join response command=0x%04X payload=%X", inspection.Command, inspection.Payload)
	}

	createPayload := make([]byte, 50)
	binary.BigEndian.PutUint32(createPayload[0:4], uin)
	copy(createPayload[8:28], []byte("after-quick-join"))
	createPayload[45] = byte(roomstate.GameTypeCompetitiveNoItem)
	createPacket := testLocalRoutedPacketWithPayload(t, game.CreateRoomCommand, 2, 0xFFFF, 1, uin, createPayload)
	createResult := server.handleLobbyMessage(ListenerConfig{Response: ResponseConfig{QQTCreateRoomSuccess: true}}, session, "create-after-empty-join", createPacket)
	if !createResult.handled || len(createResult.response) == 0 || session.RoomID == 0 {
		t.Fatalf("create after empty quick join result=%+v room=%d", createResult, session.RoomID)
	}
}
