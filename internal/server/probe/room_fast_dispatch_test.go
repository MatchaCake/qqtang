package probe

import (
	"encoding/binary"
	"encoding/hex"
	"net"
	"sync"
	"testing"
	"time"

	"qqtang/internal/clientdata/sceneelement"
	"qqtang/internal/game/match"
	"qqtang/internal/protocol/game"
)

func TestRoomFastPacketConvertsTCPUploadToType2Mode1Datagram(t *testing.T) {
	packet, err := hex.DecodeString("00000057000000000352ffff000f42420105006501000109e8fb533c3f1872ede2c5d0ac197a425e456e1475bc4176a9bebcaadc1d0f9210a04d2654627143f968dc52d05c1b45da7d1b563e0add8a18c804326dac6492")
	if err != nil {
		t.Fatal(err)
	}
	server, recipient, sender := newRoomPeerUDPTestServer(t)
	serverSocket := listenRoomPeerUDPTest(t)
	senderMode1 := listenRoomPeerUDPTest(t)
	recipientMode1 := listenRoomPeerUDPTest(t)
	defer serverSocket.Close()
	defer senderMode1.Close()
	defer recipientMode1.Close()

	senderPresence := game.LegacyUDPControlPacket{
		Header: game.LegacyUDPControlHeader{
			PacketNumber: 100,
			PlayerID:     sender.Profile.PlayerID,
			UIN:          sender.UIN,
			Type:         game.LegacyUDPPresenceType,
		},
		Presence: &game.LegacyUDPEndpoint{
			IPv4: [4]byte{127, 0, 0, 1},
			Port: uint16(senderMode1.LocalAddr().(*net.UDPAddr).Port),
		},
	}
	senderPresenceData, err := senderPresence.Encode()
	if err != nil {
		t.Fatal(err)
	}
	server.handleLegacyUDPControl(
		serverSocket, "game-udp", "sender-presence", serverSocket.LocalAddr().String(),
		senderMode1.LocalAddr().(*net.UDPAddr), senderPresenceData,
	)
	expectLegacyUDPPresenceObserved(t, senderMode1, serverSocket, senderPresence)

	presence := game.LegacyUDPControlPacket{
		Header: game.LegacyUDPControlHeader{
			PlayerID: recipient.Profile.PlayerID,
			UIN:      recipient.UIN,
			Type:     game.LegacyUDPPresenceType,
		},
		Presence: &game.LegacyUDPEndpoint{
			IPv4: [4]byte{127, 0, 0, 1},
			Port: uint16(recipientMode1.LocalAddr().(*net.UDPAddr).Port),
		},
	}
	presenceData, err := presence.Encode()
	if err != nil {
		t.Fatal(err)
	}
	server.handleLegacyUDPControl(
		serverSocket, "game-udp", "recipient-presence", serverSocket.LocalAddr().String(),
		recipientMode1.LocalAddr().(*net.UDPAddr), presenceData,
	)
	expectLegacyUDPPresenceObserved(t, recipientMode1, serverSocket, presence)

	if !server.handleRoomFastPacket(sender, "sender", packet) {
		t.Fatal("room fast handler closed actor connection")
	}
	if err = recipientMode1.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, game.LegacyUDPMaxDatagramSize)
	n, from, err := recipientMode1.ReadFromUDP(got)
	if err != nil {
		t.Fatal(err)
	}
	if from.Port != serverSocket.LocalAddr().(*net.UDPAddr).Port {
		t.Fatalf("Type-2 source = %s, want server port %d", from, serverSocket.LocalAddr().(*net.UDPAddr).Port)
	}
	routed, err := game.DecodeLegacyUDPControlPacket(got[:n])
	if err != nil {
		t.Fatal(err)
	}
	if routed.Header.Type != game.LegacyUDPMulticastType || routed.Header.PlayerID != sender.Profile.PlayerID || routed.Header.UIN != sender.UIN {
		t.Fatalf("Type-2 header = %+v, want sender player %d UIN %d", routed.Header, sender.Profile.PlayerID, sender.UIN)
	}
	if routed.Header.PacketNumber != 101 {
		t.Fatalf("Type-2 packet number = %d, want sender presence sequence + 1", routed.Header.PacketNumber)
	}
	if routed.Multicast == nil || len(routed.Multicast.Targets) != 1 || routed.Multicast.Targets[0] != (game.LegacyUDPMulticastTarget{PlayerID: recipient.Profile.PlayerID, UIN: recipient.UIN}) {
		t.Fatalf("Type-2 route = %+v", routed.Multicast)
	}
	event, err := game.ParseRoomMessageData(routed.Multicast.Data)
	if err != nil {
		t.Fatal(err)
	}
	move, err := game.DecodeRoomPlayerMoveInfo(event.Data)
	if err != nil {
		t.Fatal(err)
	}
	if event.DataID != game.RoomPlayerMoveInfoEvent || move.SeatIndex != 1 {
		t.Fatalf("Type-2 event = %+v move = %+v", event, move)
	}
}

func TestVirtualRoomFastSenderPreservesSequenceWriteOrderAcrossGoroutines(t *testing.T) {
	serverSocket := listenRoomPeerUDPTest(t)
	recipientSocket := listenRoomPeerUDPTest(t)
	defer serverSocket.Close()
	defer recipientSocket.Close()

	server, _, _ := newRoomPeerUDPTestServer(t)
	recipients := []roomFastUDPRecipient{{
		PlayerID: 2,
		UIN:      1_000_002,
		Endpoint: roomPeerUDPEndpoint{
			Connection: serverSocket,
			Address:    recipientSocket.LocalAddr().(*net.UDPAddr),
			Local:      serverSocket.LocalAddr().String(),
		},
	}}
	const packetCount = 128
	start := make(chan struct{})
	errors := make(chan error, packetCount)
	var wait sync.WaitGroup
	for index := 0; index < packetCount; index++ {
		wait.Add(1)
		go func(value byte) {
			defer wait.Done()
			<-start
			_, err := server.sendVirtualRoomFastUDPPayloadsFromIdentity(
				20_005, 2_000_005, 7, [][]byte{{value}}, recipients,
			)
			errors <- err
		}(byte(index))
	}
	close(start)
	wait.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}

	if err := recipientSocket.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, game.LegacyUDPMaxDatagramSize)
	for want := uint32(1); want <= packetCount; want++ {
		length, _, err := recipientSocket.ReadFromUDP(buffer)
		if err != nil {
			t.Fatalf("read virtual packet %d: %v", want, err)
		}
		packet, err := game.DecodeLegacyUDPControlPacket(buffer[:length])
		if err != nil {
			t.Fatalf("decode virtual packet %d: %v", want, err)
		}
		if packet.Header.PacketNumber != want {
			t.Fatalf("virtual packet arrival sequence = %d, want %d", packet.Header.PacketNumber, want)
		}
	}
}

func TestValidateRoomFastEventsRejectsSeatSpoofing(t *testing.T) {
	events := []game.RoomFastEvent{{DataID: game.RoomPlayerPutBombEvent, Data: []byte{1, 0, 0, 0, 0, 0, 0, 0, 0}}}
	if err := validateRoomFastEvents(events, 1); err == nil {
		t.Fatal("zero-based seat 1 should not be accepted for one-based room seat 1")
	}
}

func TestValidateGameplayFastPackagesUsesPerStageGameID(t *testing.T) {
	packages := []game.GameplayDataPackage{{PlayerID: 1, GameID: 8}}
	if err := validateGameplayFastPackages(packages, 1, 8); err != nil {
		t.Fatal(err)
	}
	if err := validateGameplayFastPackages(packages, 1, 7); err == nil {
		t.Fatal("previous-stage identity was accepted")
	}
}

func TestValidateGameplayFastPackagesAcceptsAdventureArbitratorProxies(t *testing.T) {
	battle, err := match.NewAdventureBattle(8, 1601, 1, []match.AdventureParticipant{
		{PlayerID: 1, TeamID: 1},
		{PlayerID: 2, TeamID: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, actorID := range []uint16{2, 30300} {
		packages := []game.GameplayDataPackage{{PlayerID: actorID, GameID: 8}}
		if err = validateGameplayFastPackagesForMatch(packages, 1, 8, nil, battle); err != nil {
			t.Fatalf("adventure arbitrator proxy %d rejected: %v", actorID, err)
		}
	}
	for _, actorID := range []uint16{1, 30300} {
		packages := []game.GameplayDataPackage{{PlayerID: actorID, GameID: 8}}
		if err = validateGameplayFastPackagesForMatch(packages, 2, 8, nil, battle); err == nil {
			t.Fatalf("non-arbitrator forwarded actor %d", actorID)
		}
	}
}

func TestValidateGameplayFastPackagesRestrictsBossProxyToArbitratorAndAllowlist(t *testing.T) {
	battle := newRoomFastBossBattle(t)
	itemBody := make([]byte, 14)
	binary.BigEndian.PutUint16(itemBody[0:2], 30001)
	binary.BigEndian.PutUint32(itemBody[2:6], 10)
	binary.BigEndian.PutUint32(itemBody[6:10], 99)
	binary.BigEndian.PutUint16(itemBody[10:12], 2)
	binary.BigEndian.PutUint16(itemBody[12:14], 3)
	var err error
	packages := []game.GameplayDataPackage{{
		PlayerID: 30001, GameID: 8,
		Messages: []game.BattleMessageData{{DataID: game.NotifyPlayerGetItem, Data: itemBody}},
	}}
	if err = validateGameplayFastPackagesWithBattle(packages, 1, 8, battle); err != nil {
		t.Fatalf("arbitrator Boss proxy rejected: %v", err)
	}
	if err = validateGameplayFastPackagesWithBattle(packages, 2, 8, battle); err == nil {
		t.Fatal("non-arbitrator was allowed to proxy the Boss")
	}
	skillBody, marshalErr := (game.NPCUseSkillEvent{
		// The package author is Boss 30001, while targeted native skills put
		// the target participant in the inner QQTMsgData PlayerID field.
		ObjectID: 2, Time: 1000, PosX: 10, PosY: 20, SkillID: 4,
		Items: []game.NPCSkillItem{{SceneID: 41, Row: 3, Col: 4}},
	}).MarshalNetworkBinary()
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	packages[0].Messages[0] = game.BattleMessageData{DataID: game.NotifyNPCUseSkill, Data: skillBody}
	if err = validateGameplayFastPackagesWithBattle(packages, 1, 8, battle); err != nil {
		t.Fatalf("arbitrator Boss skill rejected: %v", err)
	}
	forgedSkill := append([]byte(nil), skillBody...)
	binary.BigEndian.PutUint16(forgedSkill[10:12], 5)
	packages[0].Messages[0] = game.BattleMessageData{DataID: game.NotifyNPCUseSkill, Data: forgedSkill}
	if err = validateGameplayFastPackagesWithBattle(packages, 1, 8, battle); err == nil {
		t.Fatal("Boss skill absent from BOSS_INFO was accepted")
	}
	moveBody, marshalErr := (game.PlayerMove{
		PlayerID: 30001,
		SeqReply: 7,
		Entries: []game.PlayerMoveEntry{{
			PlayerID: 30001,
			Move: game.PlayerMoveSequence{
				Sequence: 8, TimeStamp: 1000,
				CurrentPosX: 10, CurrentPosY: 20,
				EndPosX: 12, EndPosY: 20,
			},
		}},
	}).MarshalNetworkBinary()
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	packages[0].Messages[0] = game.BattleMessageData{DataID: uint32(game.PlayerMoveSchema), Data: moveBody}
	if err = validateGameplayFastPackagesWithBattle(packages, 1, 8, battle); err != nil {
		t.Fatalf("arbitrator Boss movement rejected: %v", err)
	}
	spoofedMove := append([]byte(nil), moveBody...)
	binary.BigEndian.PutUint16(spoofedMove[7:9], 2)
	packages[0].Messages[0] = game.BattleMessageData{DataID: uint32(game.PlayerMoveSchema), Data: spoofedMove}
	if err = validateGameplayFastPackagesWithBattle(packages, 1, 8, battle); err == nil {
		t.Fatal("Boss movement containing another player's sequence was accepted")
	}
	talkBody, marshalErr := (game.NPCTalkEvent{ObjectID: 30001, Words: []byte("boss")}).MarshalNetworkBinary()
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	packages[0].Messages[0] = game.BattleMessageData{DataID: game.NotifyNPCTalk, Data: talkBody}
	if err = validateGameplayFastPackagesWithBattle(packages, 1, 8, battle); err != nil {
		t.Fatalf("arbitrator Boss talk rejected: %v", err)
	}
	bombBody := make([]byte, game.PlayerUseBombBodySize)
	binary.BigEndian.PutUint16(bombBody[0:2], 30001)
	binary.BigEndian.PutUint32(bombBody[2:6], 1001)
	binary.BigEndian.PutUint32(bombBody[6:10], 77)
	bombBody[10], bombBody[11] = 4, 5
	binary.BigEndian.PutUint16(bombBody[12:14], 3)
	packages[0].Messages[0] = game.BattleMessageData{DataID: game.PlayerUseBomb, Data: bombBody}
	if err = validateGameplayFastPackagesWithBattle(packages, 1, 8, battle); err != nil {
		t.Fatalf("arbitrator Boss bubble rejected: %v", err)
	}
	binary.BigEndian.PutUint16(bombBody[0:2], 30002)
	if err = validateGameplayFastPackagesWithBattle(packages, 1, 8, battle); err == nil {
		t.Fatal("Boss bubble with mismatched source was accepted")
	}
	explosionBody, marshalErr := (game.BombExplodeEvent{
		PlayerID:   30001,
		ClientTime: 1002,
		Bombs: []game.ExplodedBomb{{
			PlayerID: 30001, ClientTime: 1001, BombID: 1,
			Row: 4, Column: 5, RowMin: 4, RowMax: 4, ColumnMin: 5, ColumnMax: 5,
		}},
	}).MarshalNetworkBinary()
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	packages[0].Messages[0] = game.BattleMessageData{DataID: game.NotifyBombExplode, Data: explosionBody}
	if err = validateGameplayFastPackagesWithBattle(packages, 1, 8, battle); err != nil {
		t.Fatalf("arbitrator Boss explosion rejected: %v", err)
	}
	throwBody := make([]byte, 13)
	binary.BigEndian.PutUint32(throwBody[5:9], 1003)
	binary.BigEndian.PutUint16(throwBody[9:11], 30001)
	binary.BigEndian.PutUint16(throwBody[11:13], 3)
	packages[0].Messages[0] = game.BattleMessageData{DataID: game.PlayerThrowBomb, Data: throwBody}
	if err = validateGameplayFastPackagesWithBattle(packages, 1, 8, battle); err != nil {
		t.Fatalf("arbitrator Boss thrown bubble rejected: %v", err)
	}
	moveBombBody := make([]byte, 20)
	binary.BigEndian.PutUint16(moveBombBody[0:2], 30001)
	packages[0].Messages[0] = game.BattleMessageData{DataID: game.RequestMoveBomb, Data: moveBombBody}
	if err = validateGameplayFastPackagesWithBattle(packages, 1, 8, battle); err != nil {
		t.Fatalf("arbitrator Boss move-bomb request rejected: %v", err)
	}
	transformedHit, marshalErr := (game.PlayerExplodedEvent{
		PlayerID: 30001, ClientTime: 1004, PosX: 10, PosY: 20, IsAvatar: true,
	}).MarshalNetworkBinary()
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	packages[0].Messages[0] = game.BattleMessageData{DataID: game.PlayerBeExploded, Data: transformedHit}
	if err = validateGameplayFastPackagesWithBattle(packages, 1, 8, battle); err != nil {
		t.Fatalf("arbitrator transformed Boss hit rejected: %v", err)
	}
	normalHit := append([]byte(nil), transformedHit...)
	normalHit[len(normalHit)-1] = 0
	packages[0].Messages[0] = game.BattleMessageData{DataID: game.PlayerBeExploded, Data: normalHit}
	if err = validateGameplayFastPackagesWithBattle(packages, 1, 8, battle); err == nil {
		t.Fatal("normal-form Boss was allowed to use the transformed-avatar hit route")
	}
	recoveryBody, marshalErr := (game.AvatarRecoveryEvent{
		PlayerID: 30001, Time: 31_000, PosX: 10, PosY: 20,
	}).MarshalNetworkBinary()
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	packages[0].Messages[0] = game.BattleMessageData{DataID: game.NotifyRecoverAvatar, Data: recoveryBody}
	if err = validateGameplayFastPackagesWithBattle(packages, 1, 8, battle); err != nil {
		t.Fatalf("arbitrator Boss avatar recovery rejected: %v", err)
	}
	participantRecovery, marshalErr := (game.AvatarRecoveryEvent{
		PlayerID: 2, Time: 31_100, PosX: 12, PosY: 22,
	}).MarshalNetworkBinary()
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	packages[0].PlayerID = 1
	packages[0].Messages[0] = game.BattleMessageData{DataID: game.NotifyRecoverAvatar, Data: participantRecovery}
	if err = validateGameplayFastPackagesWithBattle(packages, 1, 8, battle); err != nil {
		t.Fatalf("arbitrator participant avatar recovery rejected: %v", err)
	}
	packages[0].PlayerID = 2
	if err = validateGameplayFastPackagesWithBattle(packages, 2, 8, battle); err == nil {
		t.Fatal("non-arbitrator participant avatar recovery was accepted")
	}
	packages[0].Messages[0] = game.BattleMessageData{DataID: game.NotifyGameOverEvent, Data: []byte{0, 0}}
	packages[0].PlayerID = 30001
	if err = validateGameplayFastPackagesWithBattle(packages, 1, 8, battle); err == nil {
		t.Fatal("Boss proxy was allowed to smuggle GAME_OVER")
	}
	packages[0].PlayerID = 30002
	if err = validateGameplayFastPackagesWithBattle(packages, 1, 8, battle); err == nil {
		t.Fatal("unregistered Boss entity was accepted")
	}
}

func TestConsumeCompetitiveFastPackagesConcludesBossLossAfterAllPlayersKilled(t *testing.T) {
	battle := newRoomFastBossBattle(t)
	for _, playerID := range []uint16{2, 1} {
		explodedBody, err := (game.PlayerExplodedEvent{
			PlayerID: playerID, ClientTime: uint32(100 + playerID), PosX: 3, PosY: 4, IsAvatar: false,
		}).MarshalNetworkBinary()
		if err != nil {
			t.Fatal(err)
		}
		reporterID := playerID
		if playerID == 2 {
			reporterID = 1 // native arbitrator reports another player's timeout
		}
		settlement, err := consumeCompetitiveFastPackages([]game.GameplayDataPackage{{
			PlayerID: reporterID, GameID: 8,
			Messages: []game.BattleMessageData{{DataID: game.PlayerBeExploded, Data: explodedBody}},
		}}, battle)
		if err != nil || settlement != nil {
			t.Fatalf("record trapped player %d settlement=%+v err=%v", playerID, settlement, err)
		}
		killedBody, err := (game.PlayerKilledEvent{PlayerInteractionEvent: game.PlayerInteractionEvent{
			PlayerID: 30001, ClientTime: uint32(200 + playerID), DestinationPlayerID: playerID, PosX: 3, PosY: 4,
		}}).MarshalNetworkBinary()
		if err != nil {
			t.Fatal(err)
		}
		bossPackage := []game.GameplayDataPackage{{
			PlayerID: 30001, GameID: 8,
			Messages: []game.BattleMessageData{{DataID: game.NotifyPlayerKilled, Data: killedBody}},
		}}
		if err = validateGameplayFastPackagesWithBattle(bossPackage, 1, 8, battle); err != nil {
			t.Fatal(err)
		}
		settlement, err = consumeCompetitiveFastPackages(bossPackage, battle)
		if err != nil {
			t.Fatal(err)
		}
		if playerID == 2 && settlement != nil {
			t.Fatal("Boss round concluded before the final participant died")
		}
		if playerID == 1 {
			if settlement == nil || len(settlement.GameOver.Results) != 2 {
				t.Fatalf("final Boss loss settlement = %+v", settlement)
			}
			for _, result := range settlement.GameOver.Results {
				if result.Result != game.GameResultLoss {
					t.Fatalf("player %d result = %d, want loss", result.PlayerID, result.Result)
				}
			}
		}
	}
}

func TestConsumeCompetitiveFastPackagesDoesNotTrapTransformedAvatar(t *testing.T) {
	battle := newRoomFastBossBattle(t)
	explodedBody, err := (game.PlayerExplodedEvent{
		PlayerID: 2, ClientTime: 102, PosX: 3, PosY: 4, IsAvatar: true,
	}).MarshalNetworkBinary()
	if err != nil {
		t.Fatal(err)
	}
	settlement, err := consumeCompetitiveFastPackages([]game.GameplayDataPackage{{
		PlayerID: 1, GameID: 8,
		Messages: []game.BattleMessageData{{DataID: game.PlayerBeExploded, Data: explodedBody}},
	}}, battle)
	if err != nil || settlement != nil {
		t.Fatalf("transformed-avatar hit settlement=%+v err=%v", settlement, err)
	}
	if _, err = battle.RecordBossKill(30001, 2); err == nil {
		t.Fatal("transformed-avatar hit incorrectly marked the player trapped")
	}
}

func TestNormalizeCompetitiveDurabilityFastPackagesFiltersEarlyDeaths(t *testing.T) {
	battle, err := match.NewCompetitiveBattleWithRuleConfig(18, 1301, 1, []match.CompetitiveParticipant{
		{PlayerID: 1, RoleID: 1, TeamID: 1},
		{PlayerID: 2, RoleID: 2, TeamID: 2},
	}, match.CompetitiveRuleConfig{
		ConclusionPolicy: match.CompetitiveConclusionClientRule,
		PlayerLifecycle:  match.CompetitivePlayerNativeDurability,
		NativeHitLimit:   4,
	})
	if err != nil {
		t.Fatal(err)
	}
	for hit := uint32(1); hit <= 4; hit++ {
		harmed := make([]byte, 13)
		binary.BigEndian.PutUint16(harmed[0:2], 2)
		binary.BigEndian.PutUint32(harmed[2:6], 100+hit)
		binary.BigEndian.PutUint16(harmed[6:8], 3)
		binary.BigEndian.PutUint16(harmed[8:10], 4)
		// The final client uses zero as the rule-7/8 one-layer durability marker.
		binary.BigEndian.PutUint16(harmed[11:13], 0)
		death := make([]byte, 11)
		binary.BigEndian.PutUint16(death[0:2], 2)
		binary.BigEndian.PutUint32(death[2:6], 100+hit)
		binary.BigEndian.PutUint16(death[6:8], 3)
		binary.BigEndian.PutUint16(death[8:10], 4)
		filtered, settlement, err := normalizeCompetitiveDurabilityFastPackages([]game.GameplayDataPackage{{
			// Rule 7/8 can package the victim's own harm/death observation.
			PlayerID: 2, GameID: 18,
			MessageIndexes: []uint32{uint32(hit * 2), uint32(hit*2 + 1)},
			Messages: []game.BattleMessageData{
				{DataID: game.PlayerBeHarmed, Data: harmed},
				{DataID: game.NotifyPlayerDieEvent, Data: death},
			},
		}}, battle)
		if err != nil {
			t.Fatalf("hit %d: %v", hit, err)
		}
		if len(filtered) != 1 || len(filtered[0].Messages) != 1+boolInt(hit == 4) {
			t.Fatalf("hit %d filtered packages = %+v", hit, filtered)
		}
		if filtered[0].Messages[0].DataID != game.PlayerBeHarmed {
			t.Fatalf("hit %d lost harm relay", hit)
		}
		if hit < 4 && settlement != nil {
			t.Fatalf("hit %d prematurely settled: %+v", hit, settlement)
		}
		if hit == 4 {
			if filtered[0].Messages[1].DataID != game.NotifyPlayerDieEvent {
				t.Fatalf("final death was not retained: %+v", filtered[0].Messages)
			}
			if settlement == nil || len(settlement.GameOver.Results) != 2 {
				t.Fatalf("final settlement = %+v", settlement)
			}
		}
		if _, err := filtered[0].MarshalNetworkBinary(); err != nil {
			t.Fatalf("hit %d filtered package is not wire-valid: %v", hit, err)
		}
	}
}

func TestNormalizeCompetitiveSculptureFastPackagesFiltersRetriesAndSettlesFourth(t *testing.T) {
	battle, err := match.NewCompetitiveBattleWithRuleConfig(19, 1201, 1, []match.CompetitiveParticipant{
		{PlayerID: 1, RoleID: 1, TeamID: 1}, {PlayerID: 2, RoleID: 2, TeamID: 2},
	}, match.CompetitiveRuleConfig{
		ConclusionPolicy: match.CompetitiveConclusionClientRule,
		PlayerLifecycle:  match.CompetitivePlayerTimedRespawn,
		Objective:        match.CompetitiveObjectiveSculpture,
	})
	if err != nil {
		t.Fatal(err)
	}
	normalize := func(index uint32, schema uint16, action game.BunActionEvent) ([]game.GameplayDataPackage, *competitiveRoomSettlementCommit, error) {
		t.Helper()
		body, marshalErr := action.MarshalNetworkBinary()
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		prepared, prepareErr := normalizeCompetitiveNativeRequestFastPackages([]game.GameplayDataPackage{{
			PlayerID: 1, GameID: 19, MessageIndexes: []uint32{index},
			Messages: []game.BattleMessageData{{DataID: uint32(schema), Data: body}},
		}}, battle)
		if prepareErr != nil {
			return nil, nil, prepareErr
		}
		return normalizeCompetitiveObjectiveFastPackages(prepared, battle)
	}
	arbitratorRequest := game.BunActionEvent{PlayerID: 1, ClientTime: 399, PosX: 1, PosY: 2, BunID: 0, BunTeamID: 12}
	filteredRequest, requestSettlement, requestErr := normalize(98, game.RequestPutBun, arbitratorRequest)
	if requestErr != nil || len(filteredRequest) != 0 || requestSettlement != nil {
		t.Fatalf("arbitrator objective request relayed=%+v settlement=%+v err=%v", filteredRequest, requestSettlement, requestErr)
	}
	for piece, material := range []byte{12, 13, 14, 15} {
		pickup := game.BunActionEvent{PlayerID: 1, ClientTime: 400 + uint32(piece*2), PosX: 1, PosY: 2, BunID: 2, BunTeamID: material}
		filtered, settlement, normalizeErr := normalize(uint32(piece*2), game.NotifyPlayerGetBun, pickup)
		if normalizeErr != nil || len(filtered) != 1 || settlement != nil {
			t.Fatalf("pickup %d filtered=%+v settlement=%+v err=%v", piece+1, filtered, settlement, normalizeErr)
		}
		deposit := pickup
		deposit.ClientTime++
		deposit.BunID = 0
		filtered, settlement, normalizeErr = normalize(uint32(piece*2+1), game.NotifyPlayerPutBun, deposit)
		if normalizeErr != nil || len(filtered) != 1 {
			t.Fatalf("deposit %d filtered=%+v err=%v", piece+1, filtered, normalizeErr)
		}
		if _, marshalErr := filtered[0].MarshalNetworkBinary(); marshalErr != nil {
			t.Fatalf("deposit %d filtered package is not wire-valid: %v", piece+1, marshalErr)
		}
		if piece < 3 && settlement != nil {
			t.Fatalf("deposit %d settled early: %+v", piece+1, settlement)
		}
		if piece == 3 && (settlement == nil || len(settlement.GameOver.Results) != 2) {
			t.Fatalf("fourth deposit settlement = %+v", settlement)
		}
		if piece == 0 {
			duplicate, duplicateSettlement, duplicateErr := normalize(99, game.NotifyPlayerPutBun, deposit)
			if duplicateErr != nil || len(duplicate) != 0 || duplicateSettlement != nil {
				t.Fatalf("duplicate deposit relayed=%+v settlement=%+v err=%v", duplicate, duplicateSettlement, duplicateErr)
			}
		}
	}
}

func TestNormalizeCompetitiveTreasureFastPackagesCountsPickupAndDropsDuplicateDeathTransport(t *testing.T) {
	battle, err := match.NewCompetitiveBattleWithRuleConfig(20, 1101, 1, []match.CompetitiveParticipant{
		{PlayerID: 1, RoleID: 1, TeamID: 1}, {PlayerID: 2, RoleID: 2, TeamID: 2},
	}, match.CompetitiveRuleConfig{
		ConclusionPolicy: match.CompetitiveConclusionClientRule,
		PlayerLifecycle:  match.CompetitivePlayerTimedRespawn,
		Objective:        match.CompetitiveObjectiveTreasure,
	})
	if err != nil {
		t.Fatal(err)
	}
	pickup := make([]byte, 14)
	binary.BigEndian.PutUint16(pickup[0:2], 2)
	binary.BigEndian.PutUint32(pickup[2:6], 100)
	binary.BigEndian.PutUint32(pickup[6:10], match.TreasureGemValueOne)
	binary.BigEndian.PutUint16(pickup[10:12], 40)
	binary.BigEndian.PutUint16(pickup[12:14], 80)
	death, marshalErr := (game.PlayerKilledEvent{PlayerInteractionEvent: game.PlayerInteractionEvent{
		PlayerID: 1, ClientTime: 101, DestinationPlayerID: 2, PosX: 40, PosY: 80,
	}, Items: []game.GameItem{{ItemID: match.TreasureGemValueOne}}}).MarshalNetworkBinary()
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	input := []game.GameplayDataPackage{{
		PlayerID: 1, GameID: 20, MessageIndexes: []uint32{1, 2},
		Messages: []game.BattleMessageData{
			{DataID: game.NotifyPlayerGetItem, Data: pickup},
			{DataID: game.NotifyPlayerKilled, Data: death},
		},
	}}
	filtered, settlement, err := normalizeCompetitiveTreasureFastPackages(input, battle)
	if err == nil {
		filtered, err = normalizeCompetitiveNativeRelayFastPackages(filtered, battle)
	}
	if err != nil || settlement != nil || len(filtered) != 1 || len(filtered[0].Messages) != 2 || filtered[0].Messages[0].DataID != game.NotifyPlayerGetItem || filtered[0].Messages[1].DataID != game.NotifyPlayerKilled {
		t.Fatalf("first treasure fast normalization filtered=%+v settlement=%+v err=%v", filtered, settlement, err)
	}
	filtered, settlement, err = normalizeCompetitiveTreasureFastPackages(input, battle)
	if err == nil {
		filtered, err = normalizeCompetitiveNativeRelayFastPackages(filtered, battle)
	}
	if err != nil || settlement != nil || len(filtered) != 0 {
		t.Fatalf("duplicate treasure fast normalization filtered=%+v settlement=%+v err=%v", filtered, settlement, err)
	}
}

func TestNormalizeCompetitiveNativeRelayFastPackagesAccountsBossRewards(t *testing.T) {
	battle := newRoomFastBossRewardBattle(t)
	if err := battle.RecordBossDeathDrops(30001, []uint32{
		451,
		uint32(sceneelement.SugarCoin50),
		uint32(sceneelement.Experience20),
	}); err != nil {
		t.Fatal(err)
	}
	if resolution, err := battle.RecordBossDeath(30001); err != nil || !resolution.NewlyConcluded {
		t.Fatalf("start Boss victory loot grace = %+v, %v", resolution, err)
	}

	items := make([]game.BattleMessageData, 0, 3)
	for index, itemID := range []uint32{451, uint32(sceneelement.SugarCoin50), uint32(sceneelement.Experience20)} {
		body, err := (game.PlayerItemEvent{
			PlayerID: 2, ClientTime: uint32(100 + index), ItemID: itemID,
			PosX: uint16(20 + index), PosY: 30,
		}).MarshalNetworkBinary()
		if err != nil {
			t.Fatal(err)
		}
		items = append(items, game.BattleMessageData{DataID: game.NotifyPlayerGetItem, Data: body})
	}
	input := []game.GameplayDataPackage{{
		PlayerID: 1, GameID: 8, MessageIndexes: []uint32{1, 2, 3}, Messages: items,
	}}
	filtered, err := normalizeCompetitiveNativeRelayFastPackages(input, battle)
	if err != nil || len(filtered) != 1 || len(filtered[0].Messages) != len(items) {
		t.Fatalf("first Boss reward fast normalization filtered=%+v err=%v", filtered, err)
	}
	filtered, err = normalizeCompetitiveNativeRelayFastPackages(input, battle)
	if err != nil || len(filtered) != 0 {
		t.Fatalf("duplicate Boss reward fast normalization filtered=%+v err=%v", filtered, err)
	}

	if got := battle.CollectedBossItems()[2][451]; got != 1 {
		t.Fatalf("fast Boss equipment count = %d, want 1", got)
	}
	if got := battle.SceneRewards()[2]; got != (match.CompetitiveSceneRewards{Money: 50, Experience: 20}) {
		t.Fatalf("fast Boss scene rewards = %+v", got)
	}
}

func TestNormalizeCompetitiveNativeRelayFastPackagesSkipsNativeActorPickup(t *testing.T) {
	battle := newRoomFastBossRewardBattle(t)
	body, err := (game.PlayerItemEvent{
		PlayerID: 30001, ClientTime: 100, ItemID: 451, PosX: 20, PosY: 30,
	}).MarshalNetworkBinary()
	if err != nil {
		t.Fatal(err)
	}
	input := []game.GameplayDataPackage{{
		PlayerID: 30001, GameID: 8, MessageIndexes: []uint32{1},
		Messages: []game.BattleMessageData{{DataID: game.NotifyPlayerGetItem, Data: body}},
	}}
	filtered, err := normalizeCompetitiveNativeRelayFastPackages(input, battle)
	if err != nil || len(filtered) != 1 || len(battle.CollectedBossItems()) != 0 {
		t.Fatalf("native actor pickup filtered=%+v collected=%+v err=%v", filtered, battle.CollectedBossItems(), err)
	}
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func newRoomFastBossBattle(t *testing.T) *match.CompetitiveBattle {
	t.Helper()
	battle, err := match.NewCompetitiveBattleWithRuleConfig(8, 11, 1, []match.CompetitiveParticipant{
		{PlayerID: 1, RoleID: 1, TeamID: 1},
		{PlayerID: 2, RoleID: 2, TeamID: 1},
	}, match.CompetitiveRuleConfig{
		ConclusionPolicy: match.CompetitiveConclusionClientRule,
		PlayerLifecycle:  match.CompetitivePlayerPermanentElimination,
		Objective:        match.CompetitiveObjectiveBoss,
		TeamTopology:     match.CompetitiveTeamsCooperative,
		BossEntityIDs:    []uint16{30001},
		BossID:           "test-boss",
		BossSkills:       map[uint16][]uint16{30001: {1, 2, 3, 4}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return battle
}

func newRoomFastBossRewardBattle(t *testing.T) *match.CompetitiveBattle {
	t.Helper()
	battle, err := match.NewCompetitiveBattleWithRuleConfig(8, 11, 1, []match.CompetitiveParticipant{
		{PlayerID: 1, RoleID: 1, TeamID: 1},
		{PlayerID: 2, RoleID: 2, TeamID: 1},
	}, match.CompetitiveRuleConfig{
		ConclusionPolicy: match.CompetitiveConclusionClientRule,
		PlayerLifecycle:  match.CompetitivePlayerPermanentElimination,
		Objective:        match.CompetitiveObjectiveBoss,
		TeamTopology:     match.CompetitiveTeamsCooperative,
		BossEntityIDs:    []uint16{30001},
		BossID:           "test-boss",
		BossDeathItems: map[uint16]map[uint32]uint32{30001: {
			451:                               1,
			uint32(sceneelement.SugarCoin50):  1,
			uint32(sceneelement.Experience20): 1,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return battle
}

func TestMatchReliableFastPacketAccountsWithoutDuplicatingNativeType2(t *testing.T) {
	packet, err := hex.DecodeString("0000006f00000000002fffff000f424201050065010001a344f986201c5e6430cc1cc532ea796592ae7dce8b9c22989699ca97a1aa22d4a1d334ed2e4e315e7159500a9d85454c592365b10a1f3fc8c847bc33c63b603c8e8a47550a5a83ee737d8e5493d7e9bf42f467805e61f4c2")
	if err != nil {
		t.Fatal(err)
	}
	server, recipient, sender := newRoomPeerUDPTestServer(t)
	if _, err = server.worldState().SetReady(sender.UIN, true); err != nil {
		t.Fatal(err)
	}
	if _, err = server.worldState().StartMatchWithID(recipient.UIN, 1); err != nil {
		t.Fatal(err)
	}
	sender.CurrentGameID, recipient.CurrentGameID = 1, 1
	runtime := &liveCompetitiveAIRuntime{
		roomID: 7, gameID: 1,
		nativeInbound: make(map[liveCompetitiveAIInboundKey]struct{}),
		stop:          make(chan struct{}),
	}
	server.competitiveAIRuntime = map[uint32]*liveCompetitiveAIRuntime{1: runtime}
	serverSocket := listenRoomPeerUDPTest(t)
	senderMode1 := listenRoomPeerUDPTest(t)
	recipientMode1 := listenRoomPeerUDPTest(t)
	defer serverSocket.Close()
	defer senderMode1.Close()
	defer recipientMode1.Close()

	for _, setup := range []struct {
		session *connectionSession
		socket  *net.UDPConn
	}{
		{sender, senderMode1}, {recipient, recipientMode1},
	} {
		presence := game.LegacyUDPControlPacket{
			Header:   game.LegacyUDPControlHeader{PlayerID: setup.session.Profile.PlayerID, UIN: setup.session.UIN, Type: game.LegacyUDPPresenceType},
			Presence: &game.LegacyUDPEndpoint{IPv4: [4]byte{127, 0, 0, 1}, Port: uint16(setup.socket.LocalAddr().(*net.UDPAddr).Port)},
		}
		data, encodeErr := presence.Encode()
		if encodeErr != nil {
			t.Fatal(encodeErr)
		}
		server.handleLegacyUDPControl(serverSocket, "game-udp", "presence", serverSocket.LocalAddr().String(), setup.socket.LocalAddr().(*net.UDPAddr), data)
		expectLegacyUDPPresenceObserved(t, setup.socket, serverSocket, presence)
	}
	if !server.handleRoomFastPacket(sender, "sender", packet) {
		t.Fatal("gameplay fast handler closed actor connection")
	}
	_ = recipientMode1.SetReadDeadline(time.Now().Add(75 * time.Millisecond))
	got := make([]byte, game.LegacyUDPMaxDatagramSize)
	n, _, err := recipientMode1.ReadFromUDP(got)
	if err == nil {
		t.Fatalf("reliable 0x0065 mirror produced a duplicate Type-2 datagram (%d bytes)", n)
	}
	if timeout, ok := err.(net.Error); !ok || !timeout.Timeout() {
		t.Fatalf("wait for duplicate Type-2 datagram: %v", err)
	}
	if got := runtime.nativeInboundCount(); got != 0 {
		t.Fatalf("reliable 0x0065 mirror entered the AI scene stream (%d facts)", got)
	}
}

func TestSimultaneousRoomFastRecipientResolutionDoesNotCrossLockSessions(t *testing.T) {
	server, one, two := newRoomPeerUDPTestServer(t)
	start := make(chan struct{})
	var ready sync.WaitGroup
	ready.Add(2)
	done := make(chan struct{}, 2)
	resolve := func(actor, target *connectionSession) {
		actor.mu.Lock()
		defer actor.mu.Unlock()
		ready.Done()
		<-start
		_, _ = server.resolveRoomFastUDPRecipients(
			actor.RoomID, actor.UIN,
			[]uint16{target.Profile.PlayerID},
			map[uint16]struct{}{target.Profile.PlayerID: {}},
		)
		done <- struct{}{}
	}
	go resolve(one, two)
	go resolve(two, one)
	ready.Wait()
	close(start)
	for range 2 {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("simultaneous fast packets cross-locked the two session mutexes")
		}
	}
}
