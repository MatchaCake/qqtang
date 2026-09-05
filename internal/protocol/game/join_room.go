package game

import (
	"encoding/binary"
	"fmt"
	"io"
)

const (
	JoinRoomRequestSchema     = 0x041D
	JoinRoomResponseOldSchema = 0x0805
	joinRoomRequestSize       = 9
	joinRoomFailureSize       = 44
)

type JoinRoomResponseOld struct {
	EnterRoomResponseOld
	RoomName string
}

// JoinRoomRequest is the legacy quick-join request. Unlike EnterRoomRequest,
// it identifies only the desired room family; the server chooses a compatible
// preparing room and projects the account's currently selected role.
type JoinRoomRequest struct {
	UIN        uint32
	ClientTime uint32
	GameType   byte
}

func DecodeLocalJoinRoomRequest(packet []byte) (JoinRoomRequest, error) {
	request, err := decodeLocalPacket(packet)
	if err != nil {
		return JoinRoomRequest{}, err
	}
	if request.Command != JoinRoomCommand {
		return JoinRoomRequest{}, fmt.Errorf("join-room command 0x%04X, want 0x%04X", request.Command, JoinRoomCommand)
	}
	payload := request.Plaintext[localInnerHeaderSize:]
	if len(payload) != joinRoomRequestSize {
		return JoinRoomRequest{}, fmt.Errorf("join-room payload length %d, want %d", len(payload), joinRoomRequestSize)
	}
	decoded := JoinRoomRequest{
		UIN:        binary.BigEndian.Uint32(payload[0:4]),
		ClientTime: binary.BigEndian.Uint32(payload[4:8]),
		GameType:   payload[8],
	}
	if decoded.UIN == 0 || decoded.UIN != request.EnvelopeUIN {
		return JoinRoomRequest{}, fmt.Errorf("join-room UIN %d does not match envelope UIN %d", decoded.UIN, request.EnvelopeUIN)
	}
	return decoded, nil
}

func (response JoinRoomResponseOld) MarshalNetworkBinary() ([]byte, error) {
	enterPayload, err := response.EnterRoomResponseOld.MarshalNetworkBinary()
	if err != nil {
		return nil, err
	}
	roomName, err := encodeLegacyGBKText(response.RoomName, RoomNameMaximumBytes)
	if err != nil {
		return nil, fmt.Errorf("RESPONSE_JOIN_ROOM_OLD room name: %w", err)
	}
	roomNameSlot := make([]byte, RoomNameMaximumBytes)
	copy(roomNameSlot, roomName)

	// RESPONSE_JOIN_ROOM_OLD inserts a fixed 20-byte RoomName after RoomID and
	// omits RESPONSE_ENTER_ROOM_OLD's WeddingModeID before PatternPoints. The
	// remaining counted arrays use the same compact network representation.
	weddingOffset, err := response.EnterRoomResponseOld.weddingModeOffset()
	if err != nil {
		return nil, err
	}
	if weddingOffset+2 > len(enterPayload) {
		return nil, fmt.Errorf("RESPONSE_JOIN_ROOM_OLD wedding offset %d exceeds payload length %d", weddingOffset, len(enterPayload))
	}
	payload := make([]byte, 0, len(enterPayload)+RoomNameMaximumBytes-2)
	payload = append(payload, enterPayload[:4]...)
	payload = append(payload, roomNameSlot...)
	payload = append(payload, enterPayload[4:weddingOffset]...)
	payload = append(payload, enterPayload[weddingOffset+2:]...)
	return payload, nil
}

func BuildLocalJoinRoomSuccess(requestPacket []byte, response JoinRoomResponseOld) ([]byte, error) {
	return buildLocalJoinRoomSuccess(requestPacket, response, nil)
}

// BuildLocalJoinRoomFailure emits the complete zero-count
// RESPONSE_JOIN_ROOM_OLD shape. The legacy client keeps the quick-join UI
// request pending until it receives this response, including when no matching
// room exists. The fixed fields differ from RESPONSE_ENTER_ROOM_OLD because
// JOIN_ROOM_OLD includes the 20-byte room name and omits WeddingModeID.
func BuildLocalJoinRoomFailure(requestPacket []byte, resultID EnterRoomResultID, roomID uint16) ([]byte, error) {
	if resultID == EnterRoomResultSuccess {
		return nil, fmt.Errorf("join-room failure result must be non-zero")
	}
	if _, err := DecodeLocalJoinRoomRequest(requestPacket); err != nil {
		return nil, err
	}
	request, err := decodeLocalPacket(requestPacket)
	if err != nil {
		return nil, err
	}
	payload := make([]byte, joinRoomFailureSize)
	binary.BigEndian.PutUint16(payload[0:2], uint16(resultID))
	binary.BigEndian.PutUint16(payload[2:4], roomID)
	return buildLocalResponse(requestPacket, request, JoinRoomCommand, payload, nil)
}

func BuildLocalJoinRoomSuccessWithReader(requestPacket []byte, response JoinRoomResponseOld, entropy io.Reader) ([]byte, error) {
	if entropy == nil {
		return nil, fmt.Errorf("entropy reader is nil")
	}
	return buildLocalJoinRoomSuccess(requestPacket, response, entropy)
}

func buildLocalJoinRoomSuccess(requestPacket []byte, response JoinRoomResponseOld, entropy io.Reader) ([]byte, error) {
	if _, err := DecodeLocalJoinRoomRequest(requestPacket); err != nil {
		return nil, err
	}
	request, err := decodeLocalPacket(requestPacket)
	if err != nil {
		return nil, err
	}
	payload, err := response.MarshalNetworkBinary()
	if err != nil {
		return nil, err
	}
	return buildLocalResponse(requestPacket, request, JoinRoomCommand, payload, entropy)
}
