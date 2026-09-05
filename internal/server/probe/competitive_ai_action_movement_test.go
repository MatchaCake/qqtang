package probe

import (
	"net"
	"testing"
	"time"

	"qqtang/internal/game/battleengine"
	"qqtang/internal/protocol/game"
)

// Native FUN_005b08e0 sends FA3 without setting actor+0x478. The user's
// 2026-08-22 QBVs contain 588 placements and no same-clock forced move after
// placement. In contrast, every one of their 29 FA5 hits has that checkpoint.
func TestCompetitiveAIBombTurnUsesOrdinaryNativeMovement(t *testing.T) {
	server, actor, peer := newRoomPeerUDPTestServer(t)
	serverSocket := listenRoomPeerUDPTest(t)
	t.Cleanup(func() { _ = serverSocket.Close() })
	var receiver *net.UDPConn
	for _, session := range []*connectionSession{actor, peer} {
		socket := listenRoomPeerUDPTest(t)
		t.Cleanup(func() { _ = socket.Close() })
		receiver = socket
		presence := game.LegacyUDPControlPacket{
			Header:   game.LegacyUDPControlHeader{PlayerID: session.Profile.PlayerID, UIN: session.UIN, Type: game.LegacyUDPPresenceType},
			Presence: &game.LegacyUDPEndpoint{IPv4: [4]byte{127, 0, 0, 1}, Port: uint16(socket.LocalAddr().(*net.UDPAddr).Port)},
		}
		data, err := presence.Encode()
		if err != nil {
			t.Fatal(err)
		}
		server.handleLegacyUDPControl(serverSocket, "game-udp", "presence", serverSocket.LocalAddr().String(), socket.LocalAddr().(*net.UDPAddr), data)
		expectLegacyUDPPresenceObserved(t, socket, serverSocket, presence)
	}
	engine, projection := turnTestEngine(t, battleengine.DirectionRight)
	before := engine.Clone()
	origin, _ := actorByID(before.Actors(), 20001)
	action := battleengine.Action{PlayerID: 20001, Move: battleengine.DirectionUp, PlaceBomb: true}
	events, err := engine.Step([]battleengine.Action{action})
	if err != nil {
		t.Fatal(err)
	}
	world, err := battleengine.NewRuntime(engine, map[uint16]battleengine.Policy{20001: battleengine.PolicyFunc(func(o battleengine.Observation, _ []battleengine.Action) (battleengine.Action, error) {
		return battleengine.Action{PlayerID: o.PlayerID}, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	runtime := &liveCompetitiveAIRuntime{
		roomID: 7, gameID: 91, runtime: world,
		virtualIDs:      map[uint16]struct{}{20001: {}},
		roomProjections: []competitiveAIRoomProjection{{PlayerID: 20001, UIN: 1_020_001}},
		messageSeq:      make(map[uint16]uint32), bombs: make(map[uint32]liveCompetitiveAIBomb),
		movement: map[uint16]liveCompetitiveAIMovementProjection{20001: projection},
	}
	runtime.projectEvents(server, before, engine, events)
	runtime.projectMovement(server, before, engine, []battleengine.Action{action})
	if err := receiver.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	var moves []game.PlayerMoveSequence
	bombs := 0
	for i := uint32(0); i < runtime.messageSeq[20001]; i++ {
		data := make([]byte, game.LegacyUDPMaxDatagramSize)
		n, _, err := receiver.ReadFromUDP(data)
		if err != nil {
			t.Fatal(err)
		}
		packet, err := game.DecodeLegacyUDPControlPacket(data[:n])
		if err != nil || packet.Multicast == nil {
			t.Fatalf("packet=%+v err=%v", packet, err)
		}
		batch, err := game.DecodeQQTPPPGameplayBatch(packet.Multicast.Data)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range batch.Entries {
			for _, message := range entry.Package.Messages {
				switch message.DataID {
				case uint32(game.PlayerUseBomb):
					bombs++
				case uint32(game.PlayerMoveSchema):
					movement, err := game.ParsePlayerMove(message.Data)
					if err != nil {
						t.Fatal(err)
					}
					for _, sample := range movement.Entries {
						moves = append(moves, sample.Move)
					}
				}
			}
		}
	}
	if bombs != 1 || len(moves) == 0 {
		t.Fatalf("bombs=%d moves=%+v", bombs, moves)
	}
	for _, move := range moves {
		if move.WalkAndDirection&0x20 != 0 {
			t.Fatalf("ordinary placement invented a forced coordinate correction: %+v", move)
		}
	}
	first := moves[0]
	if first.TimeStamp != before.ElapsedMS() || first.CurrentPosX != uint16(origin.Position.X) || first.CurrentPosY != uint16(origin.Position.Y) {
		t.Fatalf("bomb+turn lost actual turn origin at %d/%+v: %+v", before.ElapsedMS(), origin.Position, first)
	}
}
