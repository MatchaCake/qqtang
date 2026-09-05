package probe

import (
	"encoding/binary"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"qqtang/internal/game/mapdata"
	"qqtang/internal/game/match"
	"qqtang/internal/protocol/game"
)

func TestAdventureDoorwayRequiresClearAndRetainsCatalogGapCompatibility(t *testing.T) {
	catalog, err := mapdata.LoadCatalog(filepath.Join("..", "..", "..", "runtime", "client-patched"))
	if err != nil {
		t.Skipf("verified runtime client is unavailable: %v", err)
	}
	for _, cleared := range []bool{false, true} {
		t.Run(map[bool]string{false: "uncleared", true: "cleared"}[cleared], func(t *testing.T) {
			server := &Server{mapCatalog: catalog, logWriter: io.Discard}
			profile := game.DefaultPlayerProfile()
			profile.PlayerID = 1
			profile.GameInfo.RoleID = 1
			session := &connectionSession{UIN: 1000001, Profile: profile, RoomID: 1, CurrentGameID: 7, CurrentStageGameID: 7, CurrentMapID: 1646}
			battle, err := match.NewAdventureBattleWithStage(7, 1646, 1, 8, 1, []match.AdventureParticipant{{PlayerID: 1, RoleID: 1, TeamID: 1}})
			if err != nil {
				t.Fatal(err)
			}
			server.battles = map[uint32]*match.AdventureBattle{7: battle}
			if cleared {
				for range 8 {
					if _, err := battle.RecordNPCDeath(10); err != nil {
						t.Fatal(err)
					}
				}
			}
			body := make([]byte, 14)
			binary.BigEndian.PutUint16(body[0:2], 1)
			binary.BigEndian.PutUint32(body[2:6], 12000)
			binary.BigEndian.PutUint32(body[6:10], game.NoRemoteContinueFileIDWire)
			binary.BigEndian.PutUint32(body[10:14], 1647)
			payload, err := game.MarshalGameEventPayload(session.UIN, 1, game.RequestGameNextMap, body)
			if err != nil {
				t.Fatal(err)
			}
			packet := testLocalRoutedPacketWithPayload(t, game.GameEventRequestCommand, 3, 0xffff, 1, session.UIN, payload)
			result, err := server.handleAdventureNextMap(session, packet)
			if !cleared {
				if err == nil || !strings.Contains(err.Error(), "not cleared") || len(result.followUp) != 0 || session.CurrentMapID != 1646 || battle.CanAdvanceStage() {
					t.Fatalf("uncleared doorway mutated stage: map=%d advance=%t followup=%d err=%v", session.CurrentMapID, battle.CanAdvanceStage(), len(result.followUp), err)
				}
			} else if err != nil || result.followUpResult != "qqt_adventure_next_map_1648" || len(result.followUp) == 0 {
				t.Fatalf("cleared native 1647 placeholder did not produce catalog 1648: %+v, %v", result, err)
			}
		})
	}
}

func TestAdventureNextMapTargetAllowsDoorwayUnspecifiedValue(t *testing.T) {
	if !adventureNextMapTargetMatches(0, 1649, 1650, true) {
		t.Fatal("doorway target zero was rejected even though the active route has one next stage")
	}
	if !adventureNextMapTargetMatches(1650, 1649, 1650, true) {
		t.Fatal("explicit next map ID was rejected")
	}
	if adventureNextMapTargetMatches(1602, 1649, 1650, true) {
		t.Fatal("unrelated explicit next map ID was accepted")
	}
	if !adventureNextMapTargetMatches(0, 1650, 0, false) {
		t.Fatal("final doorway target zero was rejected")
	}
}

func TestAdventureNextMapTargetAllowsNativeSequentialPlaceholderAcrossCatalogGap(t *testing.T) {
	if !adventureNextMapTargetMatches(1647, 1646, 1648, true) {
		t.Fatal("native sequential placeholder 1647 was rejected for installed route 1646 -> 1648")
	}
	if adventureNextMapTargetMatches(1649, 1646, 1648, true) {
		t.Fatal("unrelated target was accepted across catalog gap")
	}
}

func TestAdventureFinalStageParticipantsIncludeDeadTeammates(t *testing.T) {
	transition := match.AdventureStageTransition{
		Survivors: []match.AdventureParticipant{{PlayerID: 2, TeamID: 1}},
		SettledOut: []match.AdventureSettlement{{
			Participant: match.AdventureParticipant{PlayerID: 1, TeamID: 1},
			Reason:      match.AdventureSettlementDiedBeforeNextStage,
		}},
	}
	participants := adventureFinalStageParticipants(transition)
	if len(participants) != 2 || participants[0].PlayerID != 1 || participants[1].PlayerID != 2 {
		t.Fatalf("final-stage participants = %+v, want both teammates ordered by player ID", participants)
	}
}
