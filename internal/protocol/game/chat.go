package game

import (
	"encoding/binary"
	"fmt"
	"io"
)

const (
	ChatAcrossSectionRequestSchema  uint32 = 0x03F4
	ChatAcrossSectionResponseSchema uint32 = 0x07DC
	SectionChatRequestSchema        uint32 = 0x03F5
	SectionChatResponseSchema       uint32 = 0x07DD
	RoomChatRequestSchema           uint32 = 0x03F6
	RoomChatResponseSchema          uint32 = 0x07DE
	SectionChatNotifySchema         uint32 = 0x03FD
	RoomChatNotifySchema            uint32 = 0x03FE
	AcrossSectionChatNotifySchema   uint32 = 0x0420

	ChatContentMaximum           = 500
	RoomChatNotifyContentMaximum = 1024
	ChatNicknameSlotSize         = 32
	SectionChatNicknameMaximum   = 19
	RoomChatHeaderSize           = 12
	SectionChatHeaderSize        = 16
	AcrossSectionChatHeaderSize  = 20
	RoomChatRequestSize          = 512
	SectionChatRequestSize       = 516
	AcrossSectionChatRequestSize = 520

	// QQTSection's Python UI uses the otherwise destination-shaped fields of
	// REQUEST_SECTION_CHAT as channel selectors.  Ordinary lobby chat and a
	// small-bugle broadcast both use PlayerID 0xffff; UIN 0 selects ordinary
	// chat and UIN 1 selects the paid bugle presentation.
	SectionChatBroadcastPlayerID uint16 = 0xffff
	SectionChatPublicUIN         uint32 = 0
	SectionChatSmallBugleUIN     uint32 = 1

	// FUN_1001f3d6 selects the bugle renderer when bit 14 of the notification
	// Identity field is present.  It is a per-message format flag and must not
	// be persisted into the player's account identity.
	SectionChatSmallBugleIdentity uint32 = 0x4000
	SmallBugleItemID              uint16 = 294

	// Room player IDs are allocated from the room's eight seats.  Reserve the
	// out-of-range all-ones value for a server-authored local system line.  A
	// narrow QQTSection patch consumes it before the native player lookup.
	RoomChatSystemSourcePlayerID uint16 = 0xffff
)

type RoomChatRequest struct {
	UIN                 uint32
	Time                uint32
	DestinationPlayerID uint16
	Content             string
}

type SectionChatRequest struct {
	UIN                  uint32
	Time                 uint32
	DestinationPlayerID  uint16
	DestinationPlayerUIN uint32
	Content              string
}

type AcrossSectionChatRequest struct {
	UIN                  uint32
	Time                 uint32
	ServiceID            uint32
	DestinationPlayerID  uint16
	DestinationPlayerUIN uint32
	Content              string
}

type RoomChatNotification struct {
	SourcePlayerID      uint16
	DestinationPlayerID uint16
	Nickname            string
	Content             string
}

type SectionChatNotification struct {
	SourcePlayerID      uint16
	DestinationPlayerID uint16
	Nickname            string
	Content             string
	UIN                 uint32
	Identity            uint32
	XEffectID           uint32
	Point               uint32
	KinFlagID           KinFlagID
}

type AcrossSectionChatNotification struct {
	SourceUIN uint32
	Content   string
	Nickname  string
}

func DecodeLocalRoomChatRequest(packet []byte) (RoomChatRequest, error) {
	decoded, payload, err := decodeChatPacket(packet, RoomChatCommand, RoomChatHeaderSize, RoomChatRequestSize)
	if err != nil {
		return RoomChatRequest{}, err
	}
	content, err := decodeChatContent(payload[RoomChatHeaderSize:], binary.BigEndian.Uint16(payload[10:12]))
	if err != nil {
		return RoomChatRequest{}, err
	}
	request := RoomChatRequest{
		UIN: binary.BigEndian.Uint32(payload[0:4]), Time: binary.BigEndian.Uint32(payload[4:8]),
		DestinationPlayerID: binary.BigEndian.Uint16(payload[8:10]), Content: content,
	}
	if err := validateChatUIN(request.UIN, decoded.EnvelopeUIN); err != nil {
		return RoomChatRequest{}, err
	}
	return request, nil
}

func DecodeLocalSectionChatRequest(packet []byte) (SectionChatRequest, error) {
	decoded, payload, err := decodeChatPacket(packet, SectionChatCommand, SectionChatHeaderSize, SectionChatRequestSize)
	if err != nil {
		return SectionChatRequest{}, err
	}
	content, err := decodeChatContent(payload[SectionChatHeaderSize:], binary.BigEndian.Uint16(payload[14:16]))
	if err != nil {
		return SectionChatRequest{}, err
	}
	request := SectionChatRequest{
		UIN: binary.BigEndian.Uint32(payload[0:4]), Time: binary.BigEndian.Uint32(payload[4:8]),
		DestinationPlayerID:  binary.BigEndian.Uint16(payload[8:10]),
		DestinationPlayerUIN: binary.BigEndian.Uint32(payload[10:14]), Content: content,
	}
	if err := validateChatUIN(request.UIN, decoded.EnvelopeUIN); err != nil {
		return SectionChatRequest{}, err
	}
	return request, nil
}

func DecodeLocalAcrossSectionChatRequest(packet []byte) (AcrossSectionChatRequest, error) {
	decoded, payload, err := decodeChatPacket(packet, ChatAcrossSectionCommand, AcrossSectionChatHeaderSize, AcrossSectionChatRequestSize)
	if err != nil {
		return AcrossSectionChatRequest{}, err
	}
	content, err := decodeChatContent(payload[AcrossSectionChatHeaderSize:], binary.BigEndian.Uint16(payload[18:20]))
	if err != nil {
		return AcrossSectionChatRequest{}, err
	}
	request := AcrossSectionChatRequest{
		UIN: binary.BigEndian.Uint32(payload[0:4]), Time: binary.BigEndian.Uint32(payload[4:8]),
		ServiceID:            binary.BigEndian.Uint32(payload[8:12]),
		DestinationPlayerID:  binary.BigEndian.Uint16(payload[12:14]),
		DestinationPlayerUIN: binary.BigEndian.Uint32(payload[14:18]), Content: content,
	}
	if err := validateChatUIN(request.UIN, decoded.EnvelopeUIN); err != nil {
		return AcrossSectionChatRequest{}, err
	}
	return request, nil
}

func BuildLocalRoomChatResponse(packet []byte, resultID uint16) ([]byte, error) {
	return buildLocalChatResponse(packet, RoomChatCommand, binary.BigEndian.AppendUint16(nil, resultID), nil)
}

func BuildLocalSectionChatResponse(packet []byte, resultID uint16) ([]byte, error) {
	return buildLocalChatResponse(packet, SectionChatCommand, binary.BigEndian.AppendUint16(nil, resultID), nil)
}

func BuildLocalAcrossSectionChatResponse(packet []byte, resultID uint16, destinationUIN uint32) ([]byte, error) {
	payload := binary.BigEndian.AppendUint16(nil, resultID)
	payload = binary.BigEndian.AppendUint32(payload, destinationUIN)
	return buildLocalChatResponse(packet, ChatAcrossSectionCommand, payload, nil)
}

// Chat notifications use dedicated SMC commands. Echoing the request command
// is accepted by the transport but never reaches the native chat widgets.
func BuildLocalRoomChatNotification(recipientPacket []byte, message RoomChatNotification) ([]byte, error) {
	payload, err := marshalRoomChatNotification(message)
	if err != nil {
		return nil, err
	}
	return buildChatNotification(recipientPacket, RoomChatNotifyCommand, payload)
}

func BuildLocalSectionChatNotification(recipientPacket []byte, message SectionChatNotification) ([]byte, error) {
	payload, err := marshalSectionChatNotification(message)
	if err != nil {
		return nil, err
	}
	return buildChatNotification(recipientPacket, SectionChatNotifyCommand, payload)
}

func BuildLocalAcrossSectionChatNotification(recipientPacket []byte, message AcrossSectionChatNotification) ([]byte, error) {
	payload, err := marshalAcrossSectionChatNotification(message)
	if err != nil {
		return nil, err
	}
	return buildChatNotification(recipientPacket, AcrossSectionChatNotifyCommand, payload)
}

func decodeChatPacket(packet []byte, command uint16, minimumPayloadSize, maximumPayloadSize int) (localPacket, []byte, error) {
	decoded, err := decodeLocalPacket(packet)
	if err != nil {
		return localPacket{}, nil, err
	}
	if decoded.Command != command {
		return localPacket{}, nil, fmt.Errorf("chat command 0x%04X, want 0x%04X", decoded.Command, command)
	}
	payload := decoded.Plaintext[localInnerHeaderSize:]
	if len(payload) < minimumPayloadSize || len(payload) > maximumPayloadSize {
		return localPacket{}, nil, fmt.Errorf("chat command 0x%04X payload length %d is outside %d..%d", command, len(payload), minimumPayloadSize, maximumPayloadSize)
	}
	return decoded, payload, nil
}

func decodeChatContent(slot []byte, length uint16) (string, error) {
	if length == 0 || int(length) > len(slot) || length > ChatContentMaximum {
		return "", fmt.Errorf("chat content length %d is outside 1..%d", length, ChatContentMaximum)
	}
	content, err := decodeLegacyGBKSlot(slot[:length])
	if err != nil {
		return "", err
	}
	if content == "" {
		return "", fmt.Errorf("chat content is empty")
	}
	// Captured 5.2 clients normally send exactly the counted GBK bytes, while
	// older fixtures may pad the schema's 500-byte maximum slot with zeroes.
	// Accept both forms without silently accepting a second trailing message.
	for _, trailing := range slot[length:] {
		if trailing != 0 {
			return "", fmt.Errorf("chat content has non-zero bytes after declared length %d", length)
		}
	}
	return content, nil
}

func marshalRoomChatNotification(message RoomChatNotification) ([]byte, error) {
	var (
		encoded []byte
		err     error
	)
	if message.SourcePlayerID == RoomChatSystemSourcePlayerID {
		// QQTSection's 0xFFFF hook copies Msg into a local buffer and renders
		// that buffer as-is. A name prefix would show up as part of the system line.
		encoded, err = encodeLegacyGBKText(message.Content, RoomChatNotifyContentMaximum)
		if err != nil {
			return nil, err
		}
		if len(encoded) == 0 {
			return nil, fmt.Errorf("chat content is empty")
		}
	} else {
		// QQTSection!FUN_1001fb9f always advances Msg by strlen(nickname)+3
		// when MsgLength is larger than that prefix. The +3 is the GBK marker
		// "说:". Sending only the user text makes the first nickname-length
		// bytes of the chat disappear; a two-character GBK name eats 7 bytes
		// ("1234567890" becomes "890", and leftover GBK fragments render as boxes).
		encoded, err = encodeNamedChatMessage(message.Nickname, message.Content, SectionChatNicknameMaximum)
		if err != nil {
			return nil, err
		}
	}
	payload := make([]byte, 6+len(encoded))
	binary.BigEndian.PutUint16(payload[0:2], message.SourcePlayerID)
	binary.BigEndian.PutUint16(payload[2:4], message.DestinationPlayerID)
	binary.BigEndian.PutUint16(payload[4:6], uint16(len(encoded)))
	copy(payload[6:], encoded)
	return payload, nil
}

func marshalSectionChatNotification(message SectionChatNotification) ([]byte, error) {
	// QQTSection!FUN_1002b1fa splits NOTIFY_SECTION_MSG.Msg at the
	// three-byte GBK marker "说:". Bytes before it are the displayed name;
	// bytes starting at the marker are the rendered chat text. Sending only
	// the user-entered text makes the client use the marker itself as the name.
	encoded, err := encodeNamedChatMessage(message.Nickname, message.Content, SectionChatNicknameMaximum)
	if err != nil {
		return nil, err
	}
	payload := make([]byte, 6+len(encoded), 6+len(encoded)+24)
	binary.BigEndian.PutUint16(payload[0:2], message.SourcePlayerID)
	binary.BigEndian.PutUint16(payload[2:4], message.DestinationPlayerID)
	binary.BigEndian.PutUint16(payload[4:6], uint16(len(encoded)))
	copy(payload[6:], encoded)
	payload = binary.BigEndian.AppendUint32(payload, message.UIN)
	payload = binary.BigEndian.AppendUint32(payload, message.Identity)
	payload = binary.BigEndian.AppendUint32(payload, message.XEffectID)
	payload = binary.BigEndian.AppendUint32(payload, message.Point)
	payload = append(payload, message.KinFlagID[:]...)
	return payload, nil
}


func encodeNamedChatMessage(nickname, content string, nicknameMaximum int) ([]byte, error) {
	nicknameBytes, err := encodeLegacyGBKText(nickname, nicknameMaximum)
	if err != nil {
		return nil, fmt.Errorf("chat nickname: %w", err)
	}
	if len(nicknameBytes) == 0 {
		return nil, fmt.Errorf("chat nickname is empty")
	}
	contentBytes, err := encodeLegacyGBKText(content, RoomChatNotifyContentMaximum)
	if err != nil {
		return nil, err
	}
	if len(contentBytes) == 0 {
		return nil, fmt.Errorf("chat content is empty")
	}
	const separator = "\xcb\xb5:"
	encoded := make([]byte, 0, len(nicknameBytes)+len(separator)+len(contentBytes))
	encoded = append(encoded, nicknameBytes...)
	encoded = append(encoded, separator...)
	encoded = append(encoded, contentBytes...)
	if len(encoded) > RoomChatNotifyContentMaximum {
		return nil, fmt.Errorf("encoded chat content length %d exceeds %d", len(encoded), RoomChatNotifyContentMaximum)
	}
	return encoded, nil
}

func marshalAcrossSectionChatNotification(message AcrossSectionChatNotification) ([]byte, error) {
	content, err := encodeLegacyGBKText(message.Content, ChatContentMaximum)
	if err != nil {
		return nil, err
	}
	if len(content) == 0 {
		return nil, fmt.Errorf("chat content is empty")
	}
	nickname, err := encodeLegacyGBKText(message.Nickname, ChatNicknameSlotSize-1)
	if err != nil {
		return nil, err
	}
	payload := make([]byte, 6+len(content)+ChatNicknameSlotSize)
	binary.BigEndian.PutUint32(payload[0:4], message.SourceUIN)
	binary.BigEndian.PutUint16(payload[4:6], uint16(len(content)))
	copy(payload[6:], content)
	copy(payload[6+len(content):], nickname)
	return payload, nil
}

func buildChatNotification(recipientPacket []byte, command uint16, payload []byte) ([]byte, error) {
	template, err := decodeLocalPacket(recipientPacket)
	if err != nil {
		return nil, err
	}
	return buildLocalNotificationFromRequest(recipientPacket, template, command, payload, nil)
}

func validateChatUIN(uin, envelopeUIN uint32) error {
	if uin == 0 || uin != envelopeUIN {
		return fmt.Errorf("chat UIN %d does not match envelope UIN %d", uin, envelopeUIN)
	}
	return nil
}

func buildLocalChatResponse(packet []byte, command uint16, payload []byte, entropy io.Reader) ([]byte, error) {
	decoded, err := decodeLocalPacket(packet)
	if err != nil {
		return nil, err
	}
	return buildLocalResponse(packet, decoded, command, payload, entropy)
}
