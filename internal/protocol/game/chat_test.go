package game

import (
	"encoding/binary"
	"testing"
)

func TestRoomAndSectionChatCodecs(t *testing.T) {
	roomPayload := make([]byte, RoomChatHeaderSize+5)
	binary.BigEndian.PutUint32(roomPayload[0:4], 1_000_001)
	binary.BigEndian.PutUint32(roomPayload[4:8], 123)
	binary.BigEndian.PutUint16(roomPayload[10:12], 5)
	copy(roomPayload[12:], "hello")
	roomPacket := makeLocalPacketForTest(t, append(buildInnerHeaderForTest(RoomChatCommand), roomPayload...))
	room, err := DecodeLocalRoomChatRequest(roomPacket)
	if err != nil || room.Content != "hello" {
		t.Fatalf("room chat = %+v, %v", room, err)
	}
	response, err := BuildLocalRoomChatResponse(roomPacket, 0)
	if err != nil {
		t.Fatal(err)
	}
	inspection, err := InspectLocalPacket(response)
	if err != nil || inspection.Command != RoomChatCommand || len(inspection.Payload) != 2 {
		t.Fatalf("room response = %+v, %v", inspection, err)
	}
	roomNotification, err := BuildLocalRoomChatNotification(roomPacket, RoomChatNotification{
		SourcePlayerID: 1, DestinationPlayerID: 2, Nickname: "糖一", Content: "hello",
	})
	if err != nil {
		t.Fatal(err)
	}
	roomNotificationInspection, err := InspectLocalPacket(roomNotification)
	if err != nil || roomNotificationInspection.Command != RoomChatNotifyCommand || roomNotificationInspection.RouteSequence != 0 || roomNotificationInspection.InnerSequence != 0 {
		t.Fatalf("room notification = %+v, %v", roomNotificationInspection, err)
	}
	roomWire := []byte{0xCC, 0xC7, 0xD2, 0xBB, 0xCB, 0xB5, 0x3A, 'h', 'e', 'l', 'l', 'o'}
	if got := roomNotificationInspection.Payload; len(got) != 6+len(roomWire) || binary.BigEndian.Uint16(got[0:2]) != 1 || binary.BigEndian.Uint16(got[2:4]) != 2 || binary.BigEndian.Uint16(got[4:6]) != uint16(len(roomWire)) || string(got[6:]) != string(roomWire) {
		t.Fatalf("room notification payload = %x", got)
	}

	sectionPayload := make([]byte, SectionChatRequestSize)
	binary.BigEndian.PutUint32(sectionPayload[0:4], 1_000_001)
	binary.BigEndian.PutUint16(sectionPayload[14:16], 4)
	copy(sectionPayload[16:], []byte{0xCC, 0xC7, 0xD2, 0xBB}) // 糖一, GBK
	sectionPacket := makeLocalPacketForTest(t, append(buildInnerHeaderForTest(SectionChatCommand), sectionPayload...))
	section, err := DecodeLocalSectionChatRequest(sectionPacket)
	if err != nil || section.Content != "糖一" {
		t.Fatalf("section chat = %+v, %v", section, err)
	}
	notification, err := BuildLocalSectionChatNotification(roomPacket, SectionChatNotification{
		SourcePlayerID: 1, Nickname: "糖一", Content: section.Content, UIN: 1_000_001, Identity: uint32(IdentityPurpleDiamond), Point: 99,
	})
	if err != nil {
		t.Fatal(err)
	}
	notificationInspection, err := InspectLocalPacket(notification)
	if err != nil || notificationInspection.Command != SectionChatNotifyCommand || notificationInspection.RouteSequence != 0 || notificationInspection.InnerSequence != 0 {
		t.Fatalf("section notification = %+v, %v", notificationInspection, err)
	}
	wireContent := []byte{0xCC, 0xC7, 0xD2, 0xBB, 0xCB, 0xB5, 0x3A, 0xCC, 0xC7, 0xD2, 0xBB}
	if got := binary.BigEndian.Uint16(notificationInspection.Payload[4:6]); got != uint16(len(wireContent)) {
		t.Fatalf("section notification content length = %d", got)
	}
	if got := notificationInspection.Payload[6 : 6+len(wireContent)]; string(got) != string(wireContent) {
		t.Fatalf("section notification encoded name/content = %x, want %x", got, wireContent)
	}
	if got := len(notificationInspection.Payload); got != 6+len(wireContent)+24 {
		t.Fatalf("section notification payload length = %d, want %d", got, 6+len(wireContent)+24)
	}
	if got := binary.BigEndian.Uint32(notificationInspection.Payload[6+len(wireContent) : 10+len(wireContent)]); got != 1_000_001 {
		t.Fatalf("section notification UIN = %d", got)
	}

	acrossNotification, err := BuildLocalAcrossSectionChatNotification(roomPacket, AcrossSectionChatNotification{
		SourceUIN: 1_000_001, Content: "hello", Nickname: "糖一",
	})
	if err != nil {
		t.Fatal(err)
	}
	acrossInspection, err := InspectLocalPacket(acrossNotification)
	if err != nil || acrossInspection.Command != AcrossSectionChatNotifyCommand || acrossInspection.RouteSequence != 0 || acrossInspection.InnerSequence != 0 {
		t.Fatalf("across-section notification = %+v, %v", acrossInspection, err)
	}
	if got := acrossInspection.Payload; len(got) != 6+5+ChatNicknameSlotSize || binary.BigEndian.Uint32(got[0:4]) != 1_000_001 || binary.BigEndian.Uint16(got[4:6]) != 5 || string(got[6:11]) != "hello" {
		t.Fatalf("across-section notification payload = %x", got)
	}
}

func TestChatCodecsAcceptZeroPaddedLegacySlots(t *testing.T) {
	payload := make([]byte, RoomChatRequestSize)
	binary.BigEndian.PutUint32(payload[0:4], 1_000_001)
	binary.BigEndian.PutUint16(payload[10:12], 2)
	copy(payload[12:], "ok")
	packet := makeLocalPacketForTest(t, append(buildInnerHeaderForTest(RoomChatCommand), payload...))
	request, err := DecodeLocalRoomChatRequest(packet)
	if err != nil || request.Content != "ok" {
		t.Fatalf("padded room chat = %+v, %v", request, err)
	}
}

func TestChatCodecsRejectUndeclaredTrailingContent(t *testing.T) {
	payload := make([]byte, RoomChatHeaderSize+3)
	binary.BigEndian.PutUint32(payload[0:4], 1_000_001)
	binary.BigEndian.PutUint16(payload[10:12], 2)
	copy(payload[12:], "bad")
	packet := makeLocalPacketForTest(t, append(buildInnerHeaderForTest(RoomChatCommand), payload...))
	if _, err := DecodeLocalRoomChatRequest(packet); err == nil {
		t.Fatal("room chat accepted bytes after its declared content length")
	}
}

func TestRoomChatNotificationKeepsSystemTextUnprefixed(t *testing.T) {
	packet := makeLocalPacketForTest(t, append(buildInnerHeaderForTest(RoomChatCommand), make([]byte, RoomChatHeaderSize+5)...))
	notification, err := BuildLocalRoomChatNotification(packet, RoomChatNotification{
		SourcePlayerID: RoomChatSystemSourcePlayerID, DestinationPlayerID: 1, Content: "cannot start",
	})
	if err != nil {
		t.Fatal(err)
	}
	inspection, err := InspectLocalPacket(notification)
	if err != nil {
		t.Fatal(err)
	}
	if got := inspection.Payload; binary.BigEndian.Uint16(got[0:2]) != RoomChatSystemSourcePlayerID || string(got[6:]) != "cannot start" {
		t.Fatalf("system room notification payload = %x", got)
	}
}

func TestRoomChatNotificationRejectsPlayerMessageWithoutNickname(t *testing.T) {
	packet := makeLocalPacketForTest(t, append(buildInnerHeaderForTest(RoomChatCommand), make([]byte, RoomChatHeaderSize+5)...))
	if _, err := BuildLocalRoomChatNotification(packet, RoomChatNotification{
		SourcePlayerID: 1, DestinationPlayerID: 2, Content: "hello",
	}); err == nil {
		t.Fatal("player room chat accepted an empty nickname")
	}
}

func TestRoomAndSectionChatNotificationNicknameLimits(t *testing.T) {
	packet := makeLocalPacketForTest(t, append(buildInnerHeaderForTest(RoomChatCommand), make([]byte, RoomChatHeaderSize+5)...))
	if _, err := BuildLocalRoomChatNotification(packet, RoomChatNotification{
		SourcePlayerID: 1, DestinationPlayerID: 2, Nickname: "12345678901234567890", Content: "hello",
	}); err != nil {
		t.Fatalf("room chat rejected a 20-byte nickname: %v", err)
	}
	if _, err := BuildLocalSectionChatNotification(packet, SectionChatNotification{
		SourcePlayerID: 1, Nickname: "12345678901234567890", Content: "hello",
	}); err == nil {
		t.Fatal("section chat accepted a 20-byte nickname")
	}
}
