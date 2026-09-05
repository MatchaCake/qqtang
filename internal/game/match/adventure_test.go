package match

import "testing"

func TestAdventureBattleConcludesOnlyAfterAllPlayersDie(t *testing.T) {
	battle, err := NewAdventureBattle(7, 1601, 1, []AdventureParticipant{
		{PlayerID: 2, TeamID: 1},
		{PlayerID: 1, TeamID: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := battle.RecordDeath(1)
	if err != nil {
		t.Fatal(err)
	}
	if first.Concluded || first.NewlyConcluded {
		t.Fatal("one multiplayer death unexpectedly concluded the battle")
	}
	second, err := battle.RecordDeath(2)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Concluded || !second.NewlyConcluded {
		t.Fatal("all players dead did not conclude the battle")
	}
	if len(second.Participants) != 2 || second.Participants[0].PlayerID != 1 || second.Participants[1].PlayerID != 2 {
		t.Fatalf("participants = %+v", second.Participants)
	}
	repeated, err := battle.RecordDeath(2)
	if err != nil {
		t.Fatal(err)
	}
	if !repeated.Concluded || repeated.NewlyConcluded {
		t.Fatal("repeated death generated a second conclusion")
	}
}

func TestSinglePlayerAdventureDeathConcludesBattle(t *testing.T) {
	battle, err := NewAdventureBattle(8, 1607, 1, []AdventureParticipant{{PlayerID: 1, TeamID: 1}})
	if err != nil {
		t.Fatal(err)
	}
	resolution, err := battle.RecordDeath(1)
	if err != nil {
		t.Fatal(err)
	}
	if !resolution.Concluded || !resolution.NewlyConcluded {
		t.Fatal("single-player death did not conclude the battle")
	}
}

func TestAdventureStatisticsFollowSoloPlayerAcrossStages(t *testing.T) {
	battle, err := NewAdventureBattle(9, 1601, 1, []AdventureParticipant{{PlayerID: 1, TeamID: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if progress, err := battle.RecordNPCDeath(10); err != nil || !progress.Credited || progress.CreditedPlayerID != 1 {
		t.Fatalf("record solo NPC kill = %+v/%v", progress, err)
	}
	if err := battle.RecordPlayerRescue(1, 20); err != nil {
		t.Fatal(err)
	}
	transition, err := battle.PrepareNextStage(1)
	if err != nil {
		t.Fatal(err)
	}
	if err := battle.AdvanceStage(1602, transition.ArbitratorPlayerID, transition.Survivors); err != nil {
		t.Fatal(err)
	}
	statistics, err := battle.PlayerStatistics(1)
	if err != nil {
		t.Fatal(err)
	}
	if statistics.KillCount != 1 || statistics.KillScore != 10 || statistics.RescueCount != 1 || statistics.RescueScore != 20 {
		t.Fatalf("next-stage statistics = %+v", statistics)
	}
}

func TestAdventureStageCountsRepeatedNPCTypeInstances(t *testing.T) {
	battle, err := NewAdventureBattleWithStage(15, 1649, 7, 2, 1, []AdventureParticipant{{PlayerID: 1, TeamID: 1}})
	if err != nil {
		t.Fatal(err)
	}
	first, err := battle.RecordNPCDeath(10)
	if err != nil || first.StageCompleted || first.DeathOrdinal != 1 {
		t.Fatalf("first repeated-type instance = %+v/%v", first, err)
	}
	second, err := battle.RecordNPCDeath(10)
	if err != nil || !second.NewlyCompleted || second.DeathOrdinal != 2 || !battle.CanAdvanceStage() {
		t.Fatalf("second repeated-type instance = %+v/%v", second, err)
	}
}

func TestAdventureNPCDeathAwardsSharedCourageIncludingStageEliminatedPlayer(t *testing.T) {
	battle, err := NewAdventureBattleWithStage(25, 1601, 7, 1, 1, []AdventureParticipant{
		{PlayerID: 1, TeamID: 1},
		{PlayerID: 2, TeamID: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = battle.RecordDeath(2); err != nil {
		t.Fatal(err)
	}
	progress, err := battle.RecordNPCDeath(37)
	if err != nil || !progress.NewlyCompleted || progress.Credited {
		t.Fatalf("shared NPC death = %+v err:%v", progress, err)
	}
	for _, playerID := range []uint16{1, 2} {
		statistics, statisticsErr := battle.PlayerStatistics(playerID)
		if statisticsErr != nil || statistics.KillCount != 1 || statistics.KillScore != 37 {
			t.Fatalf("player %d statistics = %+v err:%v", playerID, statistics, statisticsErr)
		}
	}
	transition, err := battle.PrepareNextStage(1)
	if err != nil || len(transition.SettledOut) != 1 || transition.SettledOut[0].Participant.PlayerID != 2 {
		t.Fatalf("next-stage transition = %+v err:%v", transition, err)
	}
}

func TestAdventureDoorwayRequiresKnownStageNPCCompletion(t *testing.T) {
	battle, err := NewAdventureBattleWithStage(16, 1649, 7, 3, 1, []AdventureParticipant{{PlayerID: 1, TeamID: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = battle.RecordNPCDeath(10); err != nil {
		t.Fatal(err)
	}
	if battle.CanAdvanceStage() {
		t.Fatal("incomplete NPC death count unexpectedly opened the stage")
	}
	if _, err := battle.PrepareNextStage(1); err == nil || battle.CanAdvanceStage() {
		t.Fatalf("doorway request bypassed missing NPC deaths: %v", err)
	}
	for range 2 {
		if _, err := battle.RecordNPCDeath(10); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := battle.PrepareNextStage(1); err != nil || !battle.CanAdvanceStage() {
		t.Fatalf("cleared stage rejected doorway: %v", err)
	}
}

func TestAdventureNextStageKeepsOnlySurvivors(t *testing.T) {
	battle, err := NewAdventureBattle(10, 1601, 1, []AdventureParticipant{
		{PlayerID: 1, TeamID: 1},
		{PlayerID: 2, TeamID: 1},
		{PlayerID: 3, TeamID: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := battle.RecordDeath(2); err != nil {
		t.Fatal(err)
	}
	transition, err := battle.PrepareNextStage(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(transition.Survivors) != 2 || transition.Survivors[0].PlayerID != 1 || transition.Survivors[1].PlayerID != 3 {
		t.Fatalf("survivors = %+v", transition.Survivors)
	}
	if transition.ArbitratorPlayerID != 1 {
		t.Fatalf("next-stage arbitrator = %d, want surviving current arbitrator 1", transition.ArbitratorPlayerID)
	}
	if len(transition.SettledOut) != 1 || transition.SettledOut[0].Participant.PlayerID != 2 || transition.SettledOut[0].Reason != AdventureSettlementDiedBeforeNextStage {
		t.Fatalf("settled out = %+v", transition.SettledOut)
	}
	if _, err := battle.PrepareNextStage(2); err == nil {
		t.Fatal("eliminated player advanced to the next stage")
	}
}

func TestAdventureSavedNotificationRestoresEliminatedTeammate(t *testing.T) {
	battle, err := NewAdventureBattle(20, 1601, 1, []AdventureParticipant{
		{PlayerID: 1, TeamID: 1}, {PlayerID: 2, TeamID: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = battle.RecordDeath(2); err != nil {
		t.Fatal(err)
	}
	revived, err := battle.RecordPlayerSaved(1, 2, 20)
	if err != nil || !revived {
		t.Fatalf("saved transition revived=%t err=%v", revived, err)
	}
	transition, err := battle.PrepareNextStage(2)
	if err != nil {
		t.Fatal(err)
	}
	if len(transition.Survivors) != 2 || len(transition.SettledOut) != 0 {
		t.Fatalf("transition after revival = %+v", transition)
	}
	statistics, err := battle.PlayerStatistics(1)
	if err != nil || statistics.RescueCount != 1 || statistics.RescueScore != 20 {
		t.Fatalf("revival statistics = %+v/%v", statistics, err)
	}
}

func TestAdventureReliveNotificationRestoresEliminatedTeammateWithoutInventingRescuer(t *testing.T) {
	battle, err := NewAdventureBattle(21, 1601, 1, []AdventureParticipant{
		{PlayerID: 1, TeamID: 1}, {PlayerID: 2, TeamID: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = battle.RecordDeath(2); err != nil {
		t.Fatal(err)
	}
	revived, err := battle.RecordPlayerRelive(2)
	if err != nil || !revived {
		t.Fatalf("relive transition revived=%t err=%v", revived, err)
	}
	transition, err := battle.PrepareNextStage(2)
	if err != nil {
		t.Fatal(err)
	}
	if len(transition.Survivors) != 2 || len(transition.SettledOut) != 0 {
		t.Fatalf("transition after relive = %+v", transition)
	}
	statistics, err := battle.PlayerStatistics(1)
	if err != nil || statistics.RescueCount != 0 || statistics.RescueScore != 0 {
		t.Fatalf("relive invented rescuer statistics = %+v/%v", statistics, err)
	}
}

func TestAdventureNextStageReplacesDeadArbitrator(t *testing.T) {
	battle, err := NewAdventureBattle(11, 1601, 2, []AdventureParticipant{
		{PlayerID: 3, TeamID: 1},
		{PlayerID: 1, TeamID: 1},
		{PlayerID: 2, TeamID: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := battle.RecordDeath(2); err != nil {
		t.Fatal(err)
	}
	transition, err := battle.PrepareNextStage(3)
	if err != nil {
		t.Fatal(err)
	}
	if transition.ArbitratorPlayerID != 1 {
		t.Fatalf("replacement arbitrator = %d, want stable surviving player 1", transition.ArbitratorPlayerID)
	}
}

func TestAdventureDeathKeepsArbitratorUntilStageTransition(t *testing.T) {
	battle, err := NewAdventureBattle(12, 1601, 2, []AdventureParticipant{
		{PlayerID: 1, TeamID: 1},
		{PlayerID: 2, TeamID: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = battle.RecordDeath(2); err != nil {
		t.Fatal(err)
	}
	if !battle.IsArbitrator(2) {
		t.Fatal("dead participant lost arbitration before leaving the current stage")
	}
	transition, err := battle.PrepareNextStage(1)
	if err != nil {
		t.Fatal(err)
	}
	if transition.ArbitratorPlayerID != 1 {
		t.Fatalf("next-stage arbitrator = %d, want surviving player 1", transition.ArbitratorPlayerID)
	}
}

func TestNewAdventureBattleRejectsArbitratorOutsideRoom(t *testing.T) {
	if _, err := NewAdventureBattle(12, 1601, 7, []AdventureParticipant{{PlayerID: 1, TeamID: 1}}); err == nil {
		t.Fatal("accepted adventure arbitrator outside participant set")
	}
}

func TestAdventureDisconnectTransfersArbitratorImmediately(t *testing.T) {
	battle, err := NewAdventureBattle(13, 1601, 2, []AdventureParticipant{
		{PlayerID: 3, TeamID: 1},
		{PlayerID: 1, TeamID: 1},
		{PlayerID: 2, TeamID: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	departure, err := battle.RemoveParticipant(2, AdventureDepartureDisconnected)
	if err != nil {
		t.Fatal(err)
	}
	if departure.Concluded || departure.ArbitratorPlayerID != 1 {
		t.Fatalf("departure = %+v, want continued battle with arbitrator 1", departure)
	}
}

func TestAdventureDepartureCanTransferArbitrationToDeadConnectedClient(t *testing.T) {
	battle, err := NewAdventureBattle(14, 1601, 3, []AdventureParticipant{
		{PlayerID: 1, TeamID: 1},
		{PlayerID: 2, TeamID: 1},
		{PlayerID: 3, TeamID: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = battle.RecordDeath(1); err != nil {
		t.Fatal(err)
	}
	departure, err := battle.RemoveParticipant(3, AdventureDepartureDisconnected)
	if err != nil {
		t.Fatal(err)
	}
	if departure.Concluded || departure.ArbitratorPlayerID != 1 {
		t.Fatalf("departure = %+v, want connected player 1 to arbitrate while player 2 remains alive", departure)
	}
}

func TestAdventureLastLivingPlayerLeavingMatchConcludes(t *testing.T) {
	battle, err := NewAdventureBattle(14, 1601, 1, []AdventureParticipant{
		{PlayerID: 1, TeamID: 1},
		{PlayerID: 2, TeamID: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := battle.RecordDeath(2); err != nil {
		t.Fatal(err)
	}
	departure, err := battle.RemoveParticipant(1, AdventureDepartureLeftMatch)
	if err != nil {
		t.Fatal(err)
	}
	if !departure.Concluded || !departure.NewlyConcluded || departure.ArbitratorPlayerID != 0 {
		t.Fatalf("departure = %+v, want newly concluded battle without arbitrator", departure)
	}
}

func TestAdventureBattlePlayerItemPickupDeduplicatesRequestAndNotify(t *testing.T) {
	battle, err := NewAdventureBattleWithStage(15, 1649, 7, 1, 1, []AdventureParticipant{{PlayerID: 1, TeamID: 1}})
	if err != nil {
		t.Fatal(err)
	}
	pickup := AdventureItemPickup{PlayerID: 1, ClientTime: 1234, ItemID: 30044, PosX: 10, PosY: 20}
	recorded, err := battle.RecordPlayerItemPickup(pickup, 10)
	if err != nil || !recorded {
		t.Fatalf("first pickup recorded=%t err=%v", recorded, err)
	}
	recorded, err = battle.RecordPlayerItemPickup(pickup, 10)
	if err != nil || recorded {
		t.Fatalf("duplicate pickup recorded=%t err=%v", recorded, err)
	}
	items, err := battle.CollectedItems(1)
	if err != nil || items[30044] != 1 {
		t.Fatalf("collected items=%v err=%v", items, err)
	}
	statistics, err := battle.PlayerStatistics(1)
	if err != nil || statistics.RewardCount != 1 || statistics.RewardScore != 10 {
		t.Fatalf("statistics=%+v err=%v", statistics, err)
	}
}

func TestAdventureFinalStageClearMakesEveryRemainingTeammateWinner(t *testing.T) {
	battle, err := NewAdventureBattleWithStage(16, 1650, 9, 1, 1, []AdventureParticipant{
		{PlayerID: 1, TeamID: 1},
		{PlayerID: 2, TeamID: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := battle.RecordDeath(2); err != nil {
		t.Fatal(err)
	}
	progress, err := battle.RecordNPCDeath(10)
	if err != nil || !progress.NewlyCompleted {
		t.Fatalf("final NPC progress=%+v err=%v", progress, err)
	}
	pending, err := battle.BeginFinalStageVictory()
	if err != nil || !pending.NewlyConcluded || len(pending.Players) != 2 {
		t.Fatalf("pending final victory=%+v err=%v", pending, err)
	}
	if !pending.Players[0].Won || !pending.Players[1].Won {
		t.Fatalf("final player outcomes=%+v, want both final-stage teammates to win", pending.Players)
	}
	afterDeath, err := battle.RecordDeath(1)
	if err != nil || !afterDeath.FinalVictoryPending || !afterDeath.AllPlayersDead || afterDeath.Concluded {
		t.Fatalf("pending-win death resolution=%+v err=%v", afterDeath, err)
	}
	resolution, err := battle.FinalizePendingVictory()
	if err != nil || !resolution.NewlyConcluded || len(resolution.Players) != 2 {
		t.Fatalf("final resolution=%+v err=%v", resolution, err)
	}
	if !resolution.Players[0].Won || !resolution.Players[1].Won {
		t.Fatalf("finalized player outcomes=%+v, want both final-stage teammates to win", resolution.Players)
	}
}
