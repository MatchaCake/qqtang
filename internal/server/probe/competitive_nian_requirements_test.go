package probe

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"testing"

	"qqtang/internal/game/mapdata"
	"qqtang/internal/game/match"
	"qqtang/internal/protocol/game"
	"qqtang/internal/server/persistence"
)

func TestGreatNianRequiresOwnedOutfitsButOnlyPlansOfferingConsumption(t *testing.T) {
	candidate, ok := mapdata.LookupCompetitiveBossCandidate("great_nian")
	if !ok {
		t.Fatal("great_nian candidate is missing")
	}
	participants := []match.CompetitiveParticipant{{PlayerID: 1}, {PlayerID: 2}}
	for _, missing := range []uint16{0, 431, 433, 437} {
		t.Run(fmt.Sprintf("missing_%d", missing), func(t *testing.T) {
			inventories := make(map[uint16][]game.ItemInfo)
			for _, participant := range participants {
				for _, id := range []uint16{431, 433, 437} {
					if participant.PlayerID != 2 || id != missing {
						inventories[participant.PlayerID] = append(inventories[participant.PlayerID], game.NewPermanentItemInfo(id, 1))
					}
				}
			}
			plan, satisfied, err := competitiveBossCandidateConsumptionPlan(candidate, participants, inventories)
			if err != nil || satisfied != (missing == 0) {
				t.Fatalf("satisfied=%t err=%v", satisfied, err)
			}
			if missing != 0 {
				return
			}
			for _, participant := range participants {
				want := []mapdata.CompetitiveBossItemRequirement{{ItemID: 437, Count: 1}}
				if !slices.Equal(plan[participant.PlayerID], want) {
					t.Fatalf("player %d debit plan=%+v, want only the offering", participant.PlayerID, plan[participant.PlayerID])
				}
				for _, id := range []uint16{431, 433, 437} {
					if inventoryQuantity(inventories[participant.PlayerID], id) != 1 {
						t.Fatalf("qualification mutated player %d item %d", participant.PlayerID, id)
					}
				}
			}
		})
	}
}

func TestGreatNianRepeatChallengesPreserveOutfitsAndConsumeOfferings(t *testing.T) {
	server, owner, peer := newRoomPeerUDPTestServer(t)
	store, err := persistence.OpenPlayerStore(filepath.Join(t.TempDir(), "players.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	server.playerStore = store
	for _, session := range []*connectionSession{owner, peer} {
		session.Profile.Inventory = []game.ItemInfo{
			game.NewPermanentItemInfo(431, 1), game.NewPermanentItemInfo(433, 1), game.NewPermanentItemInfo(437, 2),
		}
		if _, err = store.LoadOrCreate(context.Background(), session.UIN, session.Profile); err != nil {
			t.Fatal(err)
		}
	}
	selected := mapdata.CompetitiveMap{ID: 513, BossCandidates: mapdata.LookupCompetitiveBossCandidates(513)}
	participants := []match.CompetitiveParticipant{{PlayerID: owner.Profile.PlayerID}, {PlayerID: peer.Profile.PlayerID}}
	for attempt := 1; attempt <= 2; attempt++ {
		activation, err := server.resolveCompetitiveMatchActivation(owner, selected, participants)
		if err != nil || !activation.Active || activation.Candidate.ID != "great_nian" {
			t.Fatalf("attempt %d activation=%+v err=%v", attempt, activation, err)
		}
		profiles, err := server.consumeCompetitiveBossSummonItems(owner, activation)
		if err != nil {
			t.Fatal(err)
		}
		for _, session := range []*connectionSession{owner, peer} {
			durable, err := store.Load(context.Background(), session.UIN)
			if err != nil {
				t.Fatal(err)
			}
			for _, inventory := range [][]game.ItemInfo{session.Profile.Inventory, durable.Inventory} {
				for id, want := range map[uint16]uint32{431: 1, 433: 1, 437: uint32(2 - attempt)} {
					if got := inventoryQuantity(inventory, id); got != want {
						t.Fatalf("attempt %d UIN %d item %d quantity=%d, want %d", attempt, session.UIN, id, got, want)
					}
				}
			}
		}
		refreshes := competitiveBossInventoryRefreshes(activation, profiles)
		if len(refreshes) != 2 {
			t.Fatalf("attempt %d refreshes=%+v", attempt, refreshes)
		}
		for _, refresh := range refreshes {
			if refresh.Item.ItemID != 437 || refresh.Item.NumOfItem != uint32(2-attempt) {
				t.Fatalf("attempt %d unexpected inventory refresh=%+v", attempt, refresh)
			}
		}
	}
	activation, err := server.resolveCompetitiveMatchActivation(owner, selected, participants)
	if err != nil || activation.Active {
		t.Fatalf("no offerings left: activation=%+v err=%v", activation, err)
	}
}
