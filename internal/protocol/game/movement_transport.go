package game

import (
	"encoding/binary"
	"fmt"
)

// Gameplay fast-path schemas recovered from QQTMsgData.bin and verified
// against the original QQTEncoder.dll. They are QQTEncoder object schemas,
// not ordinary top-level game-service commands.
const (
	GameplayDataPackageSchema     uint16 = 0x0FBD
	GameplayPackageToPlayerSchema uint16 = 0x10E7
	PlayerMoveCompressedSchema    uint16 = 0x10E9
	PlayerMoveRevisionSchema      uint16 = 0x10EA
	GameplayPackageToServerSchema uint16 = 0x10EB
)

const (
	maxBattleMessageDataLength   = 800
	maxUpstreamBattleMessages    = 255
	maxDownstreamAbsoluteMoves   = 2
	maxDownstreamRevisionMoves   = 16
	maxDownstreamBattleMessages  = 8
	maxPeerDataPackageMessages   = 64
	battleMessageFixedWireSize   = 22
	upstreamPackageFixedWireSize = 7
	downstreamPackageHeaderSize  = 6
	absoluteMoveWireSize         = 9
	revisionMoveWireSize         = 4
)

// GameplayDataPackage is QQT_DATA_PACKAGE / schema 0x0FBD. The original
// peer-receive callback decodes this object directly. Reliable TCP uploads
// carry one or more encoded instances of this object, while the ordinary
// high-rate path carries the same bytes inside QQTPPP Type 2.
type GameplayDataPackage struct {
	PlayerID      uint16
	Time          uint32
	GameID        uint32
	MessageIndexes []uint32
	Messages      []BattleMessageData
}

// BattleMessageData is QQT_MSG_DATA. DataID is a 32-bit schema identifier;
// no narrower semantic name is imposed because the original table permits
// multiple adjacent gameplay event families.
type BattleMessageData struct {
	Time     uint32
	DataID   uint32
	Sequence uint32
	Data     []byte
	GameTime uint32
	Flag     uint32
}

// PlayerMoveCompressed is the absolute movement sample embedded by
// QQT_PACKAGE_TO_PLAYER.
type PlayerMoveCompressed struct {
	Time     uint32
	PosX     uint16
	PosY     uint16
	DirState byte
}

// PlayerMoveRevision is the compact delta sample following absolute samples.
// DeltaPosX and DeltaPosY are signed bytes in QQTMsgData.bin.
type PlayerMoveRevision struct {
	DeltaTime byte
	DeltaPosX int8
	DeltaPosY int8
	DirState  byte
}

// GameplayUpstreamPackage is QQT_PACKAGE_TO_SERVER / schema 0x10EB. Static
// analysis shows the client batches this object near a 1440-byte threshold,
// but the schema itself permits all 255 entries and the codec preserves that
// protocol limit.
type GameplayUpstreamPackage struct {
	PlayerID uint16
	Time     uint32
	Messages []BattleMessageData
}

// GameplayDownstreamPackage is QQT_PACKAGE_TO_PLAYER / schema 0x10E7.
type GameplayDownstreamPackage struct {
	GameID        uint16
	PlayerID      uint16
	FirstIndex    uint16
	AbsoluteMoves []PlayerMoveCompressed
	RevisionMoves []PlayerMoveRevision
	Messages      []BattleMessageData
}

func (packet GameplayDataPackage) MarshalNetworkBinary() ([]byte, error) {
	if packet.PlayerID == 0 || packet.GameID == 0 {
		return nil, fmt.Errorf("0x0FBD player ID and game ID must be non-zero")
	}
	if len(packet.Messages) == 0 || len(packet.Messages) > maxPeerDataPackageMessages {
		return nil, fmt.Errorf("0x0FBD message count %d is outside 1..%d", len(packet.Messages), maxPeerDataPackageMessages)
	}
	if len(packet.MessageIndexes) != len(packet.Messages) {
		return nil, fmt.Errorf("0x0FBD index count %d != message count %d", len(packet.MessageIndexes), len(packet.Messages))
	}
	if err := validateBattleMessages(packet.Messages); err != nil {
		return nil, fmt.Errorf("0x0FBD: %w", err)
	}
	payload := make([]byte, 0, 11+4*len(packet.MessageIndexes)+encodedBattleMessagesSize(packet.Messages))
	payload = binary.BigEndian.AppendUint16(payload, packet.PlayerID)
	payload = binary.BigEndian.AppendUint32(payload, packet.Time)
	payload = binary.BigEndian.AppendUint32(payload, packet.GameID)
	payload = append(payload, byte(len(packet.Messages)))
	for _, index := range packet.MessageIndexes {
		payload = binary.BigEndian.AppendUint32(payload, index)
	}
	return appendBattleMessages(payload, packet.Messages)
}

func ParseGameplayDataPackage(payload []byte) (GameplayDataPackage, error) {
	if len(payload) < 11 {
		return GameplayDataPackage{}, fmt.Errorf("0x0FBD payload length %d, want at least 11", len(payload))
	}
	packet := GameplayDataPackage{
		PlayerID: binary.BigEndian.Uint16(payload[0:2]),
		Time:     binary.BigEndian.Uint32(payload[2:6]),
		GameID:   binary.BigEndian.Uint32(payload[6:10]),
	}
	if packet.PlayerID == 0 || packet.GameID == 0 {
		return GameplayDataPackage{}, fmt.Errorf("0x0FBD player ID %d or game ID %d is invalid", packet.PlayerID, packet.GameID)
	}
	count := int(payload[10])
	if count == 0 || count > maxPeerDataPackageMessages {
		return GameplayDataPackage{}, fmt.Errorf("0x0FBD message count %d is outside 1..%d", count, maxPeerDataPackageMessages)
	}
	offset := 11
	if len(payload)-offset < 4*count {
		return GameplayDataPackage{}, fmt.Errorf("0x0FBD message indexes are truncated")
	}
	packet.MessageIndexes = make([]uint32, count)
	for index := range count {
		packet.MessageIndexes[index] = binary.BigEndian.Uint32(payload[offset : offset+4])
		offset += 4
	}
	messages, offset, err := parseBattleMessages(payload, offset, count)
	if err != nil {
		return GameplayDataPackage{}, fmt.Errorf("0x0FBD messages: %w", err)
	}
	if offset != len(payload) {
		return GameplayDataPackage{}, fmt.Errorf("0x0FBD has %d trailing bytes", len(payload)-offset)
	}
	packet.Messages = messages
	return packet, nil
}

func (packet GameplayUpstreamPackage) MarshalNetworkBinary() ([]byte, error) {
	if len(packet.Messages) > maxUpstreamBattleMessages {
		return nil, fmt.Errorf("0x10EB message count %d exceeds %d", len(packet.Messages), maxUpstreamBattleMessages)
	}
	if err := validateBattleMessages(packet.Messages); err != nil {
		return nil, fmt.Errorf("0x10EB: %w", err)
	}
	payload := make([]byte, 0, upstreamPackageFixedWireSize+encodedBattleMessagesSize(packet.Messages))
	payload = binary.BigEndian.AppendUint16(payload, packet.PlayerID)
	payload = binary.BigEndian.AppendUint32(payload, packet.Time)
	payload = append(payload, byte(len(packet.Messages)))
	return appendBattleMessages(payload, packet.Messages)
}

func ParseGameplayUpstreamPackage(payload []byte) (GameplayUpstreamPackage, error) {
	if len(payload) < upstreamPackageFixedWireSize {
		return GameplayUpstreamPackage{}, fmt.Errorf("0x10EB payload length %d, want at least %d", len(payload), upstreamPackageFixedWireSize)
	}
	packet := GameplayUpstreamPackage{
		PlayerID: binary.BigEndian.Uint16(payload[0:2]),
		Time:     binary.BigEndian.Uint32(payload[2:6]),
	}
	messages, offset, err := parseBattleMessages(payload, upstreamPackageFixedWireSize, int(payload[6]))
	if err != nil {
		return GameplayUpstreamPackage{}, fmt.Errorf("0x10EB messages: %w", err)
	}
	if offset != len(payload) {
		return GameplayUpstreamPackage{}, fmt.Errorf("0x10EB has %d trailing bytes", len(payload)-offset)
	}
	packet.Messages = messages
	return packet, nil
}

func (packet GameplayDownstreamPackage) MarshalNetworkBinary() ([]byte, error) {
	if len(packet.AbsoluteMoves) > maxDownstreamAbsoluteMoves {
		return nil, fmt.Errorf("0x10E7 absolute move count %d exceeds %d", len(packet.AbsoluteMoves), maxDownstreamAbsoluteMoves)
	}
	if len(packet.RevisionMoves) > maxDownstreamRevisionMoves {
		return nil, fmt.Errorf("0x10E7 revision move count %d exceeds %d", len(packet.RevisionMoves), maxDownstreamRevisionMoves)
	}
	if len(packet.Messages) > maxDownstreamBattleMessages {
		return nil, fmt.Errorf("0x10E7 message count %d exceeds %d", len(packet.Messages), maxDownstreamBattleMessages)
	}
	if err := validateBattleMessages(packet.Messages); err != nil {
		return nil, fmt.Errorf("0x10E7: %w", err)
	}
	capacity := downstreamPackageHeaderSize + 3 +
		len(packet.AbsoluteMoves)*absoluteMoveWireSize +
		len(packet.RevisionMoves)*revisionMoveWireSize +
		encodedBattleMessagesSize(packet.Messages)
	payload := make([]byte, 0, capacity)
	payload = binary.BigEndian.AppendUint16(payload, packet.GameID)
	payload = binary.BigEndian.AppendUint16(payload, packet.PlayerID)
	payload = binary.BigEndian.AppendUint16(payload, packet.FirstIndex)
	payload = append(payload, byte(len(packet.AbsoluteMoves)))
	for _, move := range packet.AbsoluteMoves {
		payload = binary.BigEndian.AppendUint32(payload, move.Time)
		payload = binary.BigEndian.AppendUint16(payload, move.PosX)
		payload = binary.BigEndian.AppendUint16(payload, move.PosY)
		payload = append(payload, move.DirState)
	}
	payload = append(payload, byte(len(packet.RevisionMoves)))
	for _, move := range packet.RevisionMoves {
		payload = append(payload, move.DeltaTime, byte(move.DeltaPosX), byte(move.DeltaPosY), move.DirState)
	}
	payload = append(payload, byte(len(packet.Messages)))
	return appendBattleMessages(payload, packet.Messages)
}

func ParseGameplayDownstreamPackage(payload []byte) (GameplayDownstreamPackage, error) {
	if len(payload) < downstreamPackageHeaderSize+3 {
		return GameplayDownstreamPackage{}, fmt.Errorf("0x10E7 payload length %d, want at least %d", len(payload), downstreamPackageHeaderSize+3)
	}
	packet := GameplayDownstreamPackage{
		GameID:     binary.BigEndian.Uint16(payload[0:2]),
		PlayerID:   binary.BigEndian.Uint16(payload[2:4]),
		FirstIndex: binary.BigEndian.Uint16(payload[4:6]),
	}
	offset := downstreamPackageHeaderSize
	absoluteCount := int(payload[offset])
	offset++
	if absoluteCount > maxDownstreamAbsoluteMoves {
		return GameplayDownstreamPackage{}, fmt.Errorf("0x10E7 absolute move count %d exceeds %d", absoluteCount, maxDownstreamAbsoluteMoves)
	}
	if len(payload)-offset < absoluteCount*absoluteMoveWireSize+2 {
		return GameplayDownstreamPackage{}, fmt.Errorf("0x10E7 absolute moves are truncated")
	}
	packet.AbsoluteMoves = make([]PlayerMoveCompressed, 0, absoluteCount)
	for range absoluteCount {
		packet.AbsoluteMoves = append(packet.AbsoluteMoves, PlayerMoveCompressed{
			Time:     binary.BigEndian.Uint32(payload[offset : offset+4]),
			PosX:     binary.BigEndian.Uint16(payload[offset+4 : offset+6]),
			PosY:     binary.BigEndian.Uint16(payload[offset+6 : offset+8]),
			DirState: payload[offset+8],
		})
		offset += absoluteMoveWireSize
	}
	revisionCount := int(payload[offset])
	offset++
	if revisionCount > maxDownstreamRevisionMoves {
		return GameplayDownstreamPackage{}, fmt.Errorf("0x10E7 revision move count %d exceeds %d", revisionCount, maxDownstreamRevisionMoves)
	}
	if len(payload)-offset < revisionCount*revisionMoveWireSize+1 {
		return GameplayDownstreamPackage{}, fmt.Errorf("0x10E7 revision moves are truncated")
	}
	packet.RevisionMoves = make([]PlayerMoveRevision, 0, revisionCount)
	for range revisionCount {
		packet.RevisionMoves = append(packet.RevisionMoves, PlayerMoveRevision{
			DeltaTime: payload[offset],
			DeltaPosX: int8(payload[offset+1]),
			DeltaPosY: int8(payload[offset+2]),
			DirState:  payload[offset+3],
		})
		offset += revisionMoveWireSize
	}
	messageCount := int(payload[offset])
	offset++
	if messageCount > maxDownstreamBattleMessages {
		return GameplayDownstreamPackage{}, fmt.Errorf("0x10E7 message count %d exceeds %d", messageCount, maxDownstreamBattleMessages)
	}
	messages, offset, err := parseBattleMessages(payload, offset, messageCount)
	if err != nil {
		return GameplayDownstreamPackage{}, fmt.Errorf("0x10E7 messages: %w", err)
	}
	if offset != len(payload) {
		return GameplayDownstreamPackage{}, fmt.Errorf("0x10E7 has %d trailing bytes", len(payload)-offset)
	}
	packet.Messages = messages
	return packet, nil
}

func encodedBattleMessagesSize(messages []BattleMessageData) int {
	size := 0
	for _, message := range messages {
		size += battleMessageFixedWireSize + len(message.Data)
	}
	return size
}

func validateBattleMessages(messages []BattleMessageData) error {
	for index, message := range messages {
		if len(message.Data) > maxBattleMessageDataLength {
			return fmt.Errorf("message %d data length %d exceeds %d", index, len(message.Data), maxBattleMessageDataLength)
		}
	}
	return nil
}

func appendBattleMessages(payload []byte, messages []BattleMessageData) ([]byte, error) {
	for index, message := range messages {
		if len(message.Data) > maxBattleMessageDataLength {
			return nil, fmt.Errorf("message %d data length %d exceeds %d", index, len(message.Data), maxBattleMessageDataLength)
		}
		payload = binary.BigEndian.AppendUint32(payload, message.Time)
		payload = binary.BigEndian.AppendUint32(payload, message.DataID)
		payload = binary.BigEndian.AppendUint16(payload, uint16(len(message.Data)))
		payload = binary.BigEndian.AppendUint32(payload, message.Sequence)
		payload = append(payload, message.Data...)
		payload = binary.BigEndian.AppendUint32(payload, message.GameTime)
		payload = binary.BigEndian.AppendUint32(payload, message.Flag)
	}
	return payload, nil
}

func parseBattleMessages(payload []byte, offset, count int) ([]BattleMessageData, int, error) {
	messages := make([]BattleMessageData, 0, count)
	for index := range count {
		if len(payload)-offset < battleMessageFixedWireSize {
			return nil, offset, fmt.Errorf("message %d fixed fields are truncated", index)
		}
		dataLength := int(binary.BigEndian.Uint16(payload[offset+8 : offset+10]))
		if dataLength > maxBattleMessageDataLength {
			return nil, offset, fmt.Errorf("message %d data length %d exceeds %d", index, dataLength, maxBattleMessageDataLength)
		}
		messageLength := battleMessageFixedWireSize + dataLength
		if len(payload)-offset < messageLength {
			return nil, offset, fmt.Errorf("message %d length %d exceeds remaining %d", index, messageLength, len(payload)-offset)
		}
		dataStart := offset + 14
		dataEnd := dataStart + dataLength
		messages = append(messages, BattleMessageData{
			Time:     binary.BigEndian.Uint32(payload[offset : offset+4]),
			DataID:   binary.BigEndian.Uint32(payload[offset+4 : offset+8]),
			Sequence: binary.BigEndian.Uint32(payload[offset+10 : offset+14]),
			Data:     append([]byte(nil), payload[dataStart:dataEnd]...),
			GameTime: binary.BigEndian.Uint32(payload[dataEnd : dataEnd+4]),
			Flag:     binary.BigEndian.Uint32(payload[dataEnd+4 : dataEnd+8]),
		})
		offset += messageLength
	}
	return messages, offset, nil
}
