package match

import (
	"testing"

	"qqtang/internal/clientdata/sceneelement"
)

func TestCompetitiveNativeSceneRewardsArePerPlayerAndDeduplicated(t *testing.T) {
	battle, err := NewCompetitiveBattleWithRuleConfig(1, 11, 1, []CompetitiveParticipant{
		{PlayerID: 1, RoleID: 1, TeamID: 1},
		{PlayerID: 2, RoleID: 2, TeamID: 2},
	}, CompetitiveRuleConfig{InitialSceneItems: map[uint32]uint32{
		uint32(sceneelement.SugarCoin100): 1,
		uint32(sceneelement.Experience20): 1,
	}})
	if err != nil {
		t.Fatal(err)
	}
	recorded, err := battle.RecordNativeSceneReward(1, 100, uint32(sceneelement.SugarCoin100), 20, 40)
	if err != nil || !recorded {
		t.Fatalf("record money = %t, %v", recorded, err)
	}
	recorded, err = battle.RecordNativeSceneReward(1, 100, uint32(sceneelement.SugarCoin100), 20, 40)
	if err != nil || recorded {
		t.Fatalf("duplicate money = %t, %v", recorded, err)
	}
	if recorded, err = battle.RecordNativeSceneReward(2, 101, uint32(sceneelement.Experience20), 30, 40); err != nil || !recorded {
		t.Fatalf("record experience = %t, %v", recorded, err)
	}
	rewards := battle.SceneRewards()
	if rewards[1] != (CompetitiveSceneRewards{Money: 100}) || rewards[2] != (CompetitiveSceneRewards{Experience: 20}) {
		t.Fatalf("scene rewards = %+v", rewards)
	}
	if _, err = battle.RecordNativeSceneReward(1, 102, uint32(sceneelement.RewardChest91), 20, 40); err == nil {
		t.Fatal("unresolved chest was accepted as a reward")
	}
	if _, err = battle.RecordNativeSceneReward(1, 103, uint32(sceneelement.TreasureGemOne), 20, 40); err == nil {
		t.Fatal("treasure objective was accepted as a settlement reward")
	}
}

func TestCompetitiveBossSceneRewardUsesBoundedInfoInventoryWithoutDropOrdering(t *testing.T) {
	battle, err := NewCompetitiveBattleWithRuleConfig(2, 11, 1, []CompetitiveParticipant{
		{PlayerID: 1, RoleID: 1, TeamID: 1},
		{PlayerID: 2, RoleID: 2, TeamID: 1},
	}, CompetitiveRuleConfig{
		ConclusionPolicy: CompetitiveConclusionClientRule,
		Objective:        CompetitiveObjectiveBoss, TeamTopology: CompetitiveTeamsCooperative,
		BossEntityIDs: []uint16{30001}, BossID: "sailor",
		BossSceneItems: map[uint16]map[uint32]uint32{30001: {
			uint32(sceneelement.SugarCoin50): 2,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if recorded, recordErr := battle.RecordNativeSceneReward(1, 1, uint32(sceneelement.SugarCoin50), 10, 20); recordErr != nil || !recorded {
		t.Fatalf("record Boss reward before drop notification = %t, %v", recorded, recordErr)
	}
	if recorded, recordErr := battle.RecordNativeSceneReward(1, 1, uint32(sceneelement.SugarCoin50), 10, 20); recordErr != nil || recorded {
		t.Fatalf("duplicate Boss reward = %t, %v", recorded, recordErr)
	}
	if err = battle.RecordBossSceneDrops(30001, []uint32{uint32(sceneelement.SugarCoin50)}); err != nil {
		t.Fatal(err)
	}
	if recorded, recordErr := battle.RecordNativeSceneReward(1, 2, uint32(sceneelement.SugarCoin50), 11, 20); recordErr != nil || !recorded {
		t.Fatalf("record second Boss reward = %t, %v", recorded, recordErr)
	}
	if _, recordErr := battle.RecordNativeSceneReward(1, 3, uint32(sceneelement.SugarCoin50), 12, 20); recordErr == nil {
		t.Fatal("Boss reward beyond BOSS_INFO inventory was accepted")
	}
	if err = battle.RecordBossSceneDrops(30001, []uint32{
		uint32(sceneelement.SugarCoin50), uint32(sceneelement.SugarCoin50),
	}); err == nil {
		t.Fatal("Boss drop exceeding configured inventory was accepted")
	}
}

func TestCompetitiveBossPermanentPickupIsBoundedAndDeduplicated(t *testing.T) {
	battle, err := NewCompetitiveBattleWithRuleConfig(3, 11, 1, []CompetitiveParticipant{
		{PlayerID: 1, RoleID: 1, TeamID: 1},
		{PlayerID: 2, RoleID: 2, TeamID: 1},
	}, CompetitiveRuleConfig{
		ConclusionPolicy: CompetitiveConclusionClientRule,
		Objective:        CompetitiveObjectiveBoss, TeamTopology: CompetitiveTeamsCooperative,
		BossEntityIDs: []uint16{30001}, BossID: "sailor",
		BossDeathItems: map[uint16]map[uint32]uint32{30001: {451: 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = battle.RecordBossSceneDrops(30001, []uint32{451}); err == nil {
		t.Fatal("death-only permanent item was accepted as a normal hit drop")
	}
	if err = battle.RecordBossDeathDrops(30001, []uint32{451}); err != nil {
		t.Fatal(err)
	}
	if resolution, deathErr := battle.RecordBossDeath(30001); deathErr != nil || !resolution.NewlyConcluded {
		t.Fatalf("start Boss victory loot grace = %+v, %v", resolution, deathErr)
	}
	recorded, err := battle.RecordBossPermanentItemPickup(2, 100, 451, 20, 30)
	if err != nil || !recorded {
		t.Fatalf("record permanent Boss item = %t, %v", recorded, err)
	}
	recorded, err = battle.RecordBossPermanentItemPickup(2, 100, 451, 20, 30)
	if err != nil || recorded {
		t.Fatalf("duplicate permanent Boss item = %t, %v", recorded, err)
	}
	if items := battle.CollectedBossItems(); items[2][451] != 1 {
		t.Fatalf("collected Boss items = %+v", items)
	}
	if _, err = battle.RecordBossPermanentItemPickup(1, 101, 451, 21, 30); err == nil {
		t.Fatal("permanent Boss item beyond BOSS_INFO inventory was accepted")
	}
	if recorded, err = battle.RecordBossPermanentItemPickup(1, 102, 452, 22, 30); err != nil || recorded {
		t.Fatalf("unconfigured permanent scene item = %t, %v", recorded, err)
	}
	battle.CompleteBossVictoryGrace()
	if recorded, err = battle.RecordBossPermanentItemPickup(1, 103, 451, 23, 30); err == nil || recorded {
		t.Fatalf("post-grace permanent Boss item = %t, %v", recorded, err)
	}
}

func TestCompetitiveBossSettlementFallbackAwardsUnreportedSoloOutfit(t *testing.T) {
	battle, err := NewCompetitiveBattleWithRuleConfig(5, 11, 1, []CompetitiveParticipant{
		{PlayerID: 1, RoleID: 1, TeamID: 1},
	}, CompetitiveRuleConfig{
		ConclusionPolicy: CompetitiveConclusionClientRule,
		Objective:        CompetitiveObjectiveBoss, TeamTopology: CompetitiveTeamsCooperative,
		BossEntityIDs: []uint16{30001}, BossID: "sailor",
		BossDeathItems: map[uint16]map[uint32]uint32{30001: {451: 1, 452: 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = battle.RecordBossDeathDrops(30001, []uint32{451, 452}); err != nil {
		t.Fatal(err)
	}
	if got := battle.CollectedBossItemsForSettlement(); len(got) != 0 {
		t.Fatalf("pre-victory fallback awarded items = %+v", got)
	}
	if resolution, recordErr := battle.RecordBossDeath(30001); recordErr != nil || !resolution.NewlyConcluded {
		t.Fatalf("record Boss victory = %+v, %v", resolution, recordErr)
	}
	items := battle.CollectedBossItemsForSettlement()
	if items[1][451] != 1 || items[1][452] != 1 {
		t.Fatalf("solo Boss settlement items = %+v", items)
	}
}

func TestCompetitiveBossSettlementFallbackDoesNotDuplicateReportedPickup(t *testing.T) {
	battle, err := NewCompetitiveBattleWithRuleConfig(6, 11, 1, []CompetitiveParticipant{
		{PlayerID: 1, RoleID: 1, TeamID: 1},
	}, CompetitiveRuleConfig{
		ConclusionPolicy: CompetitiveConclusionClientRule,
		Objective:        CompetitiveObjectiveBoss, TeamTopology: CompetitiveTeamsCooperative,
		BossEntityIDs: []uint16{30001}, BossID: "sailor",
		BossDeathItems: map[uint16]map[uint32]uint32{30001: {451: 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = battle.RecordBossDeathDrops(30001, []uint32{451}); err != nil {
		t.Fatal(err)
	}
	if resolution, recordErr := battle.RecordBossDeath(30001); recordErr != nil || !resolution.NewlyConcluded {
		t.Fatalf("record Boss victory = %+v, %v", resolution, recordErr)
	}
	if recorded, pickupErr := battle.RecordBossPermanentItemPickup(1, 100, 451, 20, 30); pickupErr != nil || !recorded {
		t.Fatalf("record reported pickup = %t, %v", recorded, pickupErr)
	}
	if got := battle.CollectedBossItemsForSettlement()[1][451]; got != 1 {
		t.Fatalf("reported solo Boss settlement item count = %d, want 1", got)
	}
}

func TestCompetitiveBossSettlementFallbackRemainsDisabledForMultiplayer(t *testing.T) {
	battle, err := NewCompetitiveBattleWithRuleConfig(7, 11, 1, []CompetitiveParticipant{
		{PlayerID: 1, RoleID: 1, TeamID: 1},
		{PlayerID: 2, RoleID: 2, TeamID: 1},
	}, CompetitiveRuleConfig{
		ConclusionPolicy: CompetitiveConclusionClientRule,
		Objective:        CompetitiveObjectiveBoss, TeamTopology: CompetitiveTeamsCooperative,
		BossEntityIDs: []uint16{30001}, BossID: "sailor",
		BossDeathItems: map[uint16]map[uint32]uint32{30001: {451: 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = battle.RecordBossDeathDrops(30001, []uint32{451}); err != nil {
		t.Fatal(err)
	}
	if resolution, recordErr := battle.RecordBossDeath(30001); recordErr != nil || !resolution.NewlyConcluded {
		t.Fatalf("record Boss victory = %+v, %v", resolution, recordErr)
	}
	if got := battle.CollectedBossItemsForSettlement(); len(got) != 0 {
		t.Fatalf("multiplayer fallback awarded items = %+v", got)
	}
}

func TestCompetitiveBossDeathAllowsPartialInventoryButRejectsOverflow(t *testing.T) {
	newBattle := func() *CompetitiveBattle {
		battle, err := NewCompetitiveBattleWithRuleConfig(4, 11, 1, []CompetitiveParticipant{
			{PlayerID: 1, RoleID: 1, TeamID: 1},
			{PlayerID: 2, RoleID: 2, TeamID: 1},
		}, CompetitiveRuleConfig{
			ConclusionPolicy: CompetitiveConclusionClientRule,
			Objective:        CompetitiveObjectiveBoss, TeamTopology: CompetitiveTeamsCooperative,
			BossEntityIDs: []uint16{30001}, BossID: "sailor",
			BossSceneItems: map[uint16]map[uint32]uint32{30001: {
				uint32(sceneelement.SugarCoin50): 2,
			}},
			BossDeathItems: map[uint16]map[uint32]uint32{30001: {451: 1, 452: 1}},
		})
		if err != nil {
			t.Fatal(err)
		}
		return battle
	}
	if err := newBattle().RecordBossDeathDrops(30001, []uint32{451}); err != nil {
		t.Fatalf("partial Boss death inventory was rejected: %v", err)
	}
	if err := newBattle().RecordBossDeathDrops(30001, []uint32{451, 452, 452}); err == nil {
		t.Fatal("Boss death exceeded OutfitItems inventory")
	}
	if err := newBattle().RecordBossDeathDrops(30001, []uint32{
		1, 2, 3, uint32(sceneelement.SugarCoin50), uint32(sceneelement.SugarCoin50), 451, 452,
	}); err != nil {
		t.Fatalf("Boss death with native base items and complete configured inventories: %v", err)
	}
}
