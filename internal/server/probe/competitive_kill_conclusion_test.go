package probe

import (
	"fmt"
	"io"
	"testing"

	"qqtang/internal/game/match"
	roomstate "qqtang/internal/game/room"
	"qqtang/internal/protocol/game"
)

func TestCompetitiveDoubleKillWaitsForNativeConclusion(t *testing.T) {
	for _, schema := range []uint16{game.RequestKillPlayer, game.NotifyPlayerKilled} {
		t.Run(fmt.Sprintf("0x%04X", schema), func(t *testing.T) {
			server := &Server{logWriter: io.Discard}
			owner := &connectionSession{UIN: 1_000_001, Profile: game.DefaultPlayerProfile()}
			if err := server.createSessionRoom(owner, 1, byte(roomstate.GameTypeCompetitiveNoItem)); err != nil {
				t.Fatal(err)
			}
			participants := []match.CompetitiveParticipant{{PlayerID: owner.Profile.PlayerID, RoleID: 1, TeamID: 1}}
			for i := uint16(1); i <= 7; i++ {
				participant := match.CompetitiveParticipant{PlayerID: 20_000 + i, RoleID: 2, TeamID: byte(i + 1), Source: match.CompetitiveParticipantVirtualAI}
				participants = append(participants, participant)
				if _, err := server.worldState().AttachMember(owner.RoomID, roomstate.Member{PlayerID: participant.PlayerID, RoleID: participant.RoleID, TeamID: participant.TeamID, Ready: true}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := server.worldState().StartMatchWithID(owner.UIN, 5); err != nil {
				t.Fatal(err)
			}
			owner.CurrentGameID, owner.CurrentMapID = 5, 905
			battle, err := match.NewCompetitiveBattle(5, 905, owner.Profile.PlayerID, participants)
			if err != nil {
				t.Fatal(err)
			}
			server.competitiveBattles = map[uint32]*match.CompetitiveBattle{5: battle}
			// The human is spectating. AI5 touches both remaining opponents in
			// one native frame, just as in the reported eight-player match.
			for _, participant := range participants {
				if participant.PlayerID != 20_001 && participant.PlayerID != 20_002 && participant.PlayerID != 20_005 {
					if _, err := battle.RecordDeath(participant.PlayerID); err != nil {
						t.Fatal(err)
					}
				}
			}
			var last gameEventMessageResult
			for index, target := range []uint16{20_001, 20_002, 20_002} {
				body := game.PlayerInteractionEvent{PlayerID: 20_005, DestinationPlayerID: target, ClientTime: 200_620, PosX: 350, PosY: 343}
				var encoded []byte
				if schema == game.NotifyPlayerKilled {
					encoded, err = (game.PlayerKilledEvent{PlayerInteractionEvent: body}).MarshalNetworkBinary()
				} else {
					encoded, err = body.MarshalNetworkBinary()
				}
				if err != nil {
					t.Fatal(err)
				}
				result := sendReliableBossRuleEvent(t, server, owner, uint32(index+1), schema, encoded)
				if !result.handled || len(result.response) == 0 || len(result.followUp) != 0 {
					t.Fatalf("kill %d must ACK without duplicating the native scene: %+v", index, result)
				}
				if index != 1 {
					if result.completeCompetitiveAfterSend || len(result.startFollowUp) != 0 {
						t.Fatalf("first/duplicate kill generated settlement: %+v", result)
					}
					continue
				}
				last = result
				if !result.completeCompetitiveAfterSend || result.competitiveRoomSettlement == nil || len(result.startFollowUp) == 0 {
					t.Fatalf("last opponent did not produce settlement: %+v", result)
				}
				if result.startFollowUpDelay != competitiveNativeConclusionDelay {
					t.Errorf("last kill conclusion delay = %s, want native scene grace %s", result.startFollowUpDelay, competitiveNativeConclusionDelay)
				}
			}
			state, err := server.sessionRoom(owner)
			if err != nil {
				t.Fatal(err)
			}
			if state.Snapshot().Phase != roomstate.PhaseInMatch || owner.CurrentGameID != 5 {
				t.Fatal("room was cleared before GAME_OVER delivery")
			}
			results := last.competitiveRoomSettlement.GameOver.Results
			if len(results) != 8 {
				t.Fatalf("results = %d, want all eight participants", len(results))
			}
			for _, result := range results {
				if (result.Result == game.GameResultWin) != (result.PlayerID == 20_005) {
					t.Fatalf("incorrect double-kill result: %+v", result)
				}
			}
		})
	}
}
