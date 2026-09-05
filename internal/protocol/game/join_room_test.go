package game

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func testJoinRoomPacket(t *testing.T) []byte {
	t.Helper()
	payload := make([]byte, joinRoomRequestSize)
	binary.BigEndian.PutUint32(payload[0:4], 1_000_001)
	binary.BigEndian.PutUint32(payload[4:8], 123)
	payload[8] = 2
	return makeLocalPacketForTest(t, append(buildInnerHeaderForTest(JoinRoomCommand), payload...))
}

func TestJoinRoomRequestAndResponse(t *testing.T) {
	packet := testJoinRoomPacket(t)
	request, err := DecodeLocalJoinRoomRequest(packet)
	if err != nil {
		t.Fatal(err)
	}
	if request.UIN != 1_000_001 || request.ClientTime != 123 || request.GameType != 2 {
		t.Fatalf("join-room request = %+v", request)
	}
	profile := DefaultPlayerProfile()
	response := JoinRoomResponseOld{
		RoomName: "探险房",
		EnterRoomResponseOld: EnterRoomResponseOld{
			RoomID: 7, LocalTeamID: 1, LocalSeatID: 1, RoomOwnerID: 1, GameType: 2,
			SeatStatus: [8]EnterRoomSeatStatus{EnterRoomSeatOccupied, EnterRoomSeatOpen, EnterRoomSeatOpen, EnterRoomSeatOpen, EnterRoomSeatLocked, EnterRoomSeatLocked, EnterRoomSeatLocked, EnterRoomSeatLocked},
			Players:    []EnterRoomPlayer{{Player: PlayerInfoInRoomOldFromProfile(1_000_001, profile, 1, 1, 0)}},
		},
	}
	payload, err := response.MarshalNetworkBinary()
	if err != nil {
		t.Fatal(err)
	}
	enterPayload, err := response.EnterRoomResponseOld.MarshalNetworkBinary()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(payload), len(enterPayload)+RoomNameMaximumBytes-2; got != want {
		t.Fatalf("join-room payload length = %d, want %d", got, want)
	}
	if binary.BigEndian.Uint16(payload[2:4]) != 7 || payload[24] != 0x11 || binary.BigEndian.Uint16(payload[25:27]) != 0 {
		t.Fatalf("join-room fixed header = %X", payload[:31])
	}
	encoded, err := BuildLocalJoinRoomSuccessWithReader(packet, response, bytes.NewReader(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	inspection, err := InspectLocalPacket(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if inspection.Command != JoinRoomCommand || binary.BigEndian.Uint16(inspection.Payload[2:4]) != 7 {
		t.Fatalf("join-room response command 0x%04X payload %X", inspection.Command, inspection.Payload[:6])
	}
}

func TestBuildLocalJoinRoomUnavailableFailure(t *testing.T) {
	packet, err := BuildLocalJoinRoomFailure(testJoinRoomPacket(t), EnterRoomResultRoomUnavailable, 0)
	if err != nil {
		t.Fatal(err)
	}
	inspection, err := InspectLocalPacket(packet)
	if err != nil {
		t.Fatal(err)
	}
	if inspection.Command != JoinRoomCommand || len(inspection.Payload) != joinRoomFailureSize {
		t.Fatalf("join-room failure = command 0x%04X payload %d", inspection.Command, len(inspection.Payload))
	}
	if got := EnterRoomResultID(binary.BigEndian.Uint16(inspection.Payload[0:2])); got != EnterRoomResultRoomUnavailable {
		t.Fatalf("join-room failure result = %d", got)
	}
	if got := binary.BigEndian.Uint16(inspection.Payload[2:4]); got != 0 {
		t.Fatalf("join-room unavailable room ID = %d, want 0", got)
	}
}
