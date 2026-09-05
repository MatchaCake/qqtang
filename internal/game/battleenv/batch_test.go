package battleenv

import (
	"math"
	"reflect"
	"testing"

	"qqtang/internal/game/battleengine"
	"qqtang/internal/game/mapdata"
)

func requireRewardNear(t *testing.T, got, want float32) {
	t.Helper()
	if math.Abs(float64(got-want)) > 1e-6 {
		t.Fatalf("reward=%v, want %v", got, want)
	}
}

func TestPickupAttributeMetricsUseActualCappedPositiveChanges(t *testing.T) {
	batch, err := NewBatch(testBatchConfig(1))
	if err != nil {
		t.Fatal(err)
	}
	rewards := make([]float32, len(batch.episodes[0].playerIDs))
	batch.accumulateEvents(0, []battleengine.Event{
		{Kind: battleengine.EventPickupCollected, PlayerID: 1, Attribute: battleengine.AttributeBombCapacity, ValueBefore: 2, ValueAfter: 5},
		{Kind: battleengine.EventPickupCollected, PlayerID: 1, Attribute: battleengine.AttributeBombCapacity, ValueBefore: 5, ValueAfter: 5},
		{Kind: battleengine.EventPickupCollected, PlayerID: 1, Attribute: battleengine.AttributeBombPower, ValueBefore: 6, ValueAfter: 8},
		{Kind: battleengine.EventPickupCollected, PlayerID: 1, Attribute: battleengine.AttributeSpeedRate, ValueBefore: 4, ValueAfter: 5},
		{Kind: battleengine.EventPickupCollected, PlayerID: 1, Attribute: battleengine.AttributeBombPower, ValueBefore: 8, ValueAfter: 1},
		{Kind: battleengine.EventPickupCollected, PlayerID: 1, Effect: battleengine.PickupEffectBattleAction, ActionID: 63, ActionCount: 1},
		{Kind: battleengine.EventPickupCollected, PlayerID: 2, Attribute: battleengine.AttributeBombCapacity, ValueBefore: 1, ValueAfter: 2},
	}, rewards)
	got := batch.episodes[0].metrics.Agents[0]
	if got.Pickups != 6 || got.PickupCapacityGain != 3 || got.PickupPowerGain != 2 || got.PickupSpeedGain != 1 {
		t.Fatalf("incorrect pickup evidence: %+v", got)
	}
	if got := batch.episodes[0].metrics.Agents[1].PickupCapacityGain; got != 1 {
		t.Fatalf("other actor gain=%d, want 1", got)
	}
	before := got
	recordPickupAttributeGain(&got, battleengine.Event{Kind: battleengine.EventActorMoved, Attribute: battleengine.AttributeSpeedRate, ValueBefore: 1, ValueAfter: 5})
	if got != before {
		t.Fatal("non-pickup state change counted as a pickup gain")
	}
}

func TestParallelSearchTeacherMatchesSerialTransitions(t *testing.T) {
	config := testBatchConfig(4)
	config.WorkerCount = 1
	serial, err := NewBatch(config)
	if err != nil {
		t.Fatal(err)
	}
	config.WorkerCount = 4
	parallel, err := NewBatch(config)
	if err != nil {
		t.Fatal(err)
	}
	actionCount := int(battleengine.DiscreteActionCount)
	logits := make([]float32, config.EnvCount*config.ParticipantCount*actionCount)
	for base := 0; base < len(logits); base += actionCount {
		logits[base+int(battleengine.ActionPlaceBomb)] = 1
	}
	search := battleengine.SearchConfig{TopK: 4, HorizonMS: 800, AlwaysSearch: true, TacticalSafety: true}
	for step := 0; step < 8; step++ {
		left, err := serial.SearchTeacherScores(logits, search)
		if err != nil {
			t.Fatal(err)
		}
		right, err := parallel.SearchTeacherScores(logits, search)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(left, right) {
			t.Fatalf("parallel search differs at step %d", step)
		}
		leftStep, err := serial.Step(left.Actions)
		if err != nil {
			t.Fatal(err)
		}
		rightStep, err := parallel.Step(right.Actions)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(leftStep, rightStep) {
			t.Fatalf("parallel transition differs at step %d", step)
		}
	}
}

func TestBatchResetAndObservationAreDeterministic(t *testing.T) {
	config := testBatchConfig(3)
	first, err := NewBatch(config)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewBatch(config)
	if err != nil {
		t.Fatal(err)
	}
	firstObservation, err := first.Observe()
	if err != nil {
		t.Fatal(err)
	}
	secondObservation, err := second.Observe()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first.MapIDs(), second.MapIDs()) || !reflect.DeepEqual(firstObservation, secondObservation) {
		t.Fatal("same batch seed did not produce identical reset tensors")
	}
	if got, want := len(firstObservation.Spatial), 3*2*SpatialChannels*3*5; got != want {
		t.Fatalf("spatial length = %d, want %d", got, want)
	}
	if got, want := len(firstObservation.Scalars), 3*2*ScalarFeatures; got != want {
		t.Fatalf("scalar length = %d, want %d", got, want)
	}
	for envIndex := 0; envIndex < 3; envIndex++ {
		for actorIndex := 0; actorIndex < 2; actorIndex++ {
			base := (envIndex*2 + actorIndex) * int(battleengine.DiscreteActionCount)
			if firstObservation.Legal[base+int(battleengine.ActionWait)] != 1 || firstObservation.Active[envIndex*2+actorIndex] != 1 {
				t.Fatalf("environment %d actor %d lacks active wait action", envIndex, actorIndex)
			}
		}
	}
}

func TestLateDuelReplayRestoresIsolatedAuthoritativeState(t *testing.T) {
	config := testBatchConfig(1)
	config.LateDuelReplayPermille = 1_000
	batch, err := NewBatch(config)
	if err != nil {
		t.Fatal(err)
	}
	episode := &batch.episodes[0]
	for episode.engine.RoundElapsedMS() < lateDuelReplayMinimumOrdinaryElapsedMS {
		if _, err := episode.engine.Step([]battleengine.Action{{PlayerID: 1}, {PlayerID: 2}}); err != nil {
			t.Fatal(err)
		}
	}
	batch.captureLateDuelReplay(episode)
	if episode.lateDuelCandidate == nil || episode.lateDuelReplay != nil {
		t.Fatal("ordinary final duel did not produce an unvalidated replay candidate")
	}
	wantElapsed := episode.lateDuelCandidate.engine.ElapsedMS()
	wantActors := episode.lateDuelCandidate.engine.Actors()
	for index := range wantActors {
		wantActors[index].PublicBehavior = battleengine.PublicBehaviorMemory{}
	}
	for episode.engine.RoundElapsedMS() < lateDuelReplayMinimumOrdinaryElapsedMS+10_000 {
		if _, err := episode.engine.Step([]battleengine.Action{{PlayerID: 1}, {PlayerID: 2}}); err != nil {
			t.Fatal(err)
		}
	}
	batch.promoteLateDuelReplay(episode, battleengine.Outcome{Ended: true, WinnerTeamID: 1})
	if episode.lateDuelReplay == nil {
		t.Fatal("decisive source tail was not promoted for replay")
	}
	if err := batch.Reset([]int{0}); err != nil {
		t.Fatal(err)
	}
	observation, err := batch.Observe()
	if err != nil {
		t.Fatal(err)
	}
	if got := observation.CurriculumIDs[0]; got != CurriculumLateDuelReplay {
		t.Fatalf("replay curriculum ID=%d, want %d", got, CurriculumLateDuelReplay)
	}
	if episode.engine.ElapsedMS() != wantElapsed || !reflect.DeepEqual(episode.engine.Actors(), wantActors) {
		t.Fatal("replay reset did not preserve the captured engine state")
	}
	for _, teamID := range episode.teamIDs {
		if got := episode.teamProgress[teamID]; got != episode.engine.RoundElapsedMS() {
			t.Fatalf("replay team %d progress clock=%d, want fresh boundary %d", teamID, got, episode.engine.RoundElapsedMS())
		}
	}
	if _, err := episode.engine.Step([]battleengine.Action{{PlayerID: 1}, {PlayerID: 2}}); err != nil {
		t.Fatal(err)
	}
	if episode.engine.ElapsedMS() == wantElapsed {
		t.Fatal("test mutation did not advance restored engine")
	}
	if err := batch.Reset([]int{0}); err != nil {
		t.Fatal(err)
	}
	if episode.engine.ElapsedMS() != wantElapsed || !reflect.DeepEqual(episode.engine.Actors(), wantActors) {
		t.Fatal("mutating one replay changed the stored snapshot")
	}
}

func TestLateDuelReplayRejectsTimedOutSourceTail(t *testing.T) {
	config := testBatchConfig(1)
	config.LateDuelReplayPermille = 1_000
	batch, err := NewBatch(config)
	if err != nil {
		t.Fatal(err)
	}
	episode := &batch.episodes[0]
	captureAt := rewardRoundDurationMS - 45_000
	for episode.engine.RoundElapsedMS() < captureAt {
		if _, err := episode.engine.Step([]battleengine.Action{{PlayerID: 1}, {PlayerID: 2}}); err != nil {
			t.Fatal(err)
		}
	}
	batch.captureLateDuelReplay(episode)
	for episode.engine.RoundElapsedMS() < captureAt+40_000 {
		if _, err := episode.engine.Step([]battleengine.Action{{PlayerID: 1}, {PlayerID: 2}}); err != nil {
			t.Fatal(err)
		}
	}
	batch.promoteLateDuelReplay(episode, battleengine.Outcome{Ended: true, Draw: true, TimedOut: true})
	if episode.lateDuelReplay != nil {
		t.Fatal("timed-out source tail was admitted for replay")
	}
}

func TestLateDuelReplayRejectsNonTimeoutDraw(t *testing.T) {
	config := testBatchConfig(1)
	config.LateDuelReplayPermille = 1_000
	batch, err := NewBatch(config)
	if err != nil {
		t.Fatal(err)
	}
	episode := &batch.episodes[0]
	for episode.engine.RoundElapsedMS() < lateDuelReplayMinimumOrdinaryElapsedMS {
		if _, err := episode.engine.Step([]battleengine.Action{{PlayerID: 1}, {PlayerID: 2}}); err != nil {
			t.Fatal(err)
		}
	}
	batch.captureLateDuelReplay(episode)
	for episode.engine.RoundElapsedMS() < lateDuelReplayMinimumOrdinaryElapsedMS+10_000 {
		if _, err := episode.engine.Step([]battleengine.Action{{PlayerID: 1}, {PlayerID: 2}}); err != nil {
			t.Fatal(err)
		}
	}
	batch.promoteLateDuelReplay(episode, battleengine.Outcome{Ended: true, Draw: true})
	if episode.lateDuelReplay != nil {
		t.Fatal("non-timeout draw was admitted as a corrective replay")
	}
}

func TestLateDuelReplayKeepsCandidateUntilSourceEnds(t *testing.T) {
	config := testBatchConfig(1)
	config.LateDuelReplayPermille = 1_000
	batch, err := NewBatch(config)
	if err != nil {
		t.Fatal(err)
	}
	episode := &batch.episodes[0]
	for episode.engine.RoundElapsedMS() < lateDuelReplayMinimumOrdinaryElapsedMS {
		if _, err := episode.engine.Step([]battleengine.Action{{PlayerID: 1}, {PlayerID: 2}}); err != nil {
			t.Fatal(err)
		}
	}
	batch.captureLateDuelReplay(episode)
	want := episode.lateDuelCandidate
	batch.promoteLateDuelReplay(episode, battleengine.Outcome{})
	if episode.lateDuelCandidate != want || episode.lateDuelReplay != nil {
		t.Fatal("non-terminal decision consumed the replay candidate")
	}
}

func TestLateDuelReplayProbabilityRejectsOutOfRangeValue(t *testing.T) {
	config := testBatchConfig(1)
	config.LateDuelReplayPermille = 1_001
	if _, err := NewBatch(config); err == nil {
		t.Fatal("out-of-range late duel replay probability was accepted")
	}
}

func TestStepReturnsAuthoritativeMetricsOnlyWhenEpisodeCompletes(t *testing.T) {
	config := testBatchConfig(1)
	config.BombEscapeCurriculumPermille = 1_000
	config.BombEscapeCapacity = 1
	config.BombEscapeRequiredDetonations = 1
	batch, err := NewBatch(config)
	if err != nil {
		t.Fatal(err)
	}
	episode := &batch.episodes[0]
	trainingActor := -1
	for index, actor := range episode.engine.Actors() {
		if actor.TeamID == episode.metrics.BombEscapeTrainingTeamID {
			trainingActor = index
			break
		}
	}
	if trainingActor < 0 {
		t.Fatalf("training team %d has no actor", episode.metrics.BombEscapeTrainingTeamID)
	}
	actions := []battleengine.ActionID{battleengine.ActionWait, battleengine.ActionWait}
	actions[trainingActor] = battleengine.ActionMoveRight
	result, err := batch.Step([][]battleengine.ActionID{actions})
	if err != nil {
		t.Fatal(err)
	}
	if result.Dones[0] != 0 {
		t.Fatal("bomb-escape episode completed before its owned bomb detonated")
	}
	if got := result.CompletedAgentMetrics[0]; got != (AgentMetrics{}) {
		t.Fatalf("live episode leaked partial metrics: %+v", got)
	}
	if err := episode.engine.ApplyVerifiedMovementCheckpoint(
		episode.playerIDs[trainingActor],
		battleengine.PositionAtCellCenter(battleengine.Cell{Row: 2, Col: 2}),
	); err != nil {
		t.Fatal(err)
	}
	for decision := 1; decision < 120 && result.Dones[0] == 0; decision++ {
		result, err = batch.Step([][]battleengine.ActionID{{
			battleengine.ActionWait,
			battleengine.ActionWait,
		}})
		if err != nil {
			t.Fatal(err)
		}
	}
	if result.Dones[0] != 1 {
		t.Fatal("bomb-escape episode did not complete")
	}
	metrics := result.CompletedAgentMetrics[0]
	if metrics.BombsPlaced != 0 || metrics.MovedTicks == 0 {
		t.Fatalf("completed authoritative metrics = %+v", metrics)
	}
}

func TestTrainingMapSelectionUsesSinglePoolNativePlacementInStandardTeams(t *testing.T) {
	singlePool := testTrainingMap()
	singlePool.SpawnGroupA = append(singlePool.SpawnGroupA, singlePool.SpawnGroupB...)
	singlePool.SpawnGroupB = nil
	batch := &Batch{maps: []mapdata.CompetitiveMap{singlePool}}
	entry, mode, err := batch.selectEpisodeMap(1, true, []uint8{1, 2})
	if err != nil {
		t.Fatal(err)
	}
	if entry.ID != singlePool.ID || mode != battleengine.NativeSpawnFree {
		t.Fatalf("single-pool standard training selection = map %d mode %d", entry.ID, mode)
	}

	twoPool := testTrainingMap()
	batch.maps = []mapdata.CompetitiveMap{twoPool}
	_, mode, err = batch.selectEpisodeMap(1, true, []uint8{1, 2})
	if err != nil {
		t.Fatal(err)
	}
	if mode != battleengine.NativeSpawnTeams {
		t.Fatalf("two-pool standard training mode = %d, want teams", mode)
	}
}

func TestDuelCurriculumBuildsConnectedDevelopedEndgameState(t *testing.T) {
	grid := battleengine.Grid{Width: 5, Height: 3, Cells: make([]battleengine.Tile, 15)}
	for index := range grid.Cells {
		grid.Cells[index] = battleengine.Tile{Kind: battleengine.CellOpen, FlamePassable: true}
	}
	grid.Cells[7] = battleengine.Tile{Kind: battleengine.CellBreakable, Durability: 1}
	config := battleengine.Config{
		Grid: grid,
		Participants: []battleengine.Participant{
			{PlayerID: 1, TeamID: 1, BombCapacity: 1, MaxBombCapacity: 4, BombPower: 1, MaxBombPower: 4, SpeedRate: 2, MaxSpeedRate: 6},
			{PlayerID: 2, TeamID: 2, BombCapacity: 1, MaxBombCapacity: 4, BombPower: 1, MaxBombPower: 4, SpeedRate: 2, MaxSpeedRate: 6},
		},
		Pickups: []battleengine.Pickup{{SceneID: 1}},
	}

	if err := applyDuelCurriculum(&config, 123); err != nil {
		t.Fatal(err)
	}
	if tile, _ := config.Grid.Cell(battleengine.Cell{Row: 1, Col: 2}); tile.Kind != battleengine.CellOpen || tile.MapElementOccupied {
		t.Fatalf("breakable terrain survived duel curriculum: %+v", tile)
	}
	if len(config.Pickups) != 0 {
		t.Fatalf("developed duel retained opening pickups: %+v", config.Pickups)
	}
	first, second := config.Participants[0], config.Participants[1]
	distance := absInt(int(first.Spawn.Row-second.Spawn.Row)) + absInt(int(first.Spawn.Col-second.Spawn.Col))
	if distance < 3 || distance > 6 {
		t.Fatalf("duel spawn distance=%d, actors=%+v/%+v", distance, first, second)
	}
	for _, participant := range config.Participants {
		if participant.BombCapacity < 2 || participant.BombPower < 2 || participant.SpeedRate <= 2 {
			t.Fatalf("duel attributes were not developed: %+v", participant)
		}
		if participant.BombCapacity > participant.MaxBombCapacity || participant.BombPower > participant.MaxBombPower || participant.SpeedRate > participant.MaxSpeedRate {
			t.Fatalf("duel attributes exceeded native maxima: %+v", participant)
		}
	}
}

func TestBombEscapeCurriculumSpawnsHaveShortSafeRoutes(t *testing.T) {
	grid := battleengine.Grid{Width: 7, Height: 5, Cells: make([]battleengine.Tile, 35)}
	for index := range grid.Cells {
		grid.Cells[index] = battleengine.Tile{Kind: battleengine.CellOpen, FlamePassable: true}
	}
	config := battleengine.Config{
		Grid: grid,
		Participants: []battleengine.Participant{
			{PlayerID: 1, TeamID: 1, BombCapacity: 1, MaxBombCapacity: 4, BombPower: 1, MaxBombPower: 4, SpeedRate: 2, MaxSpeedRate: 6},
			{PlayerID: 2, TeamID: 2, BombCapacity: 1, MaxBombCapacity: 4, BombPower: 1, MaxBombPower: 4, SpeedRate: 2, MaxSpeedRate: 6},
		},
	}
	for seed := uint64(1); seed <= 64; seed++ {
		candidate := config
		candidate.Participants = append([]battleengine.Participant(nil), config.Participants...)
		if err := applyBombEscapeCurriculum(&candidate, seed, 1); err != nil {
			t.Fatal(err)
		}
		for _, participant := range candidate.Participants {
			if participant.BombCapacity != 1 {
				t.Fatalf("seed %d retained multi-bomb capacity: %+v", seed, participant)
			}
			if !bombEscapeRouteExists(candidate.Grid, participant.Spawn, participant.BombPower, bombEscapeMaximumPathCells) {
				t.Fatalf("seed %d produced impossible escape spawn: %+v", seed, participant)
			}
		}
	}
}

func TestBombEscapeCurriculumRetainsNativeTerrainAndPickups(t *testing.T) {
	grid := battleengine.Grid{Width: 9, Height: 5, Cells: make([]battleengine.Tile, 45)}
	for index := range grid.Cells {
		grid.Cells[index] = battleengine.Tile{Kind: battleengine.CellOpen, FlamePassable: true}
	}
	wall := battleengine.Cell{Row: 2, Col: 4}
	grid.Cells[int(wall.Row)*int(grid.Width)+int(wall.Col)] = battleengine.Tile{
		Kind: battleengine.CellBreakable, Durability: 1, MapElementID: 77,
	}
	config := battleengine.Config{
		Grid: grid,
		Participants: []battleengine.Participant{
			{PlayerID: 1, TeamID: 1, BombCapacity: 3, MaxBombCapacity: 4, BombPower: 2, MaxBombPower: 4, SpeedRate: 2, MaxSpeedRate: 6},
			{PlayerID: 2, TeamID: 2, BombCapacity: 3, MaxBombCapacity: 4, BombPower: 2, MaxBombPower: 4, SpeedRate: 2, MaxSpeedRate: 6},
		},
		Pickups: []battleengine.Pickup{{
			SceneID: battleengine.SceneBombCapacitySmall, Cell: wall, State: battleengine.PickupHidden,
		}},
	}
	if err := applyBombEscapeCurriculum(&config, 123, 1); err != nil {
		t.Fatal(err)
	}
	if tile, _ := config.Grid.Cell(wall); tile.Kind != battleengine.CellBreakable || tile.MapElementID != 77 {
		t.Fatalf("bomb escape course changed native terrain: %+v", tile)
	}
	if len(config.Pickups) != 1 || config.Pickups[0].Cell != wall || config.Pickups[0].State != battleengine.PickupHidden {
		t.Fatalf("bomb escape course changed native pickups: %+v", config.Pickups)
	}
	for _, participant := range config.Participants {
		if participant.BombCapacity != 1 {
			t.Fatalf("bomb escape course retained multi-bomb capacity: %+v", participant)
		}
		if !bombEscapeRouteExists(config.Grid, participant.Spawn, participant.BombPower, bombEscapeMaximumPathCells) {
			t.Fatalf("bomb escape course produced impossible native spawn: %+v", participant)
		}
	}
}

func TestDevelopmentCurriculumUsesRealHiddenWallsAndEscapableSpawns(t *testing.T) {
	grid := battleengine.Grid{Width: 11, Height: 5, Cells: make([]battleengine.Tile, 55)}
	for index := range grid.Cells {
		grid.Cells[index] = battleengine.Tile{Kind: battleengine.CellOpen, FlamePassable: true}
	}
	walls := []battleengine.Cell{{Row: 2, Col: 2}, {Row: 2, Col: 8}}
	for index, wall := range walls {
		grid.Cells[int(wall.Row)*int(grid.Width)+int(wall.Col)] = battleengine.Tile{
			Kind: battleengine.CellBreakable, Durability: 1, MapElementID: uint32(index + 1),
		}
	}
	config := battleengine.Config{
		Grid: grid,
		Participants: []battleengine.Participant{
			{PlayerID: 1, TeamID: 1, BombCapacity: 1, MaxBombCapacity: 4, BombPower: 2, MaxBombPower: 4, SpeedRate: 2, MaxSpeedRate: 6},
			{PlayerID: 2, TeamID: 2, BombCapacity: 1, MaxBombCapacity: 4, BombPower: 2, MaxBombPower: 4, SpeedRate: 2, MaxSpeedRate: 6},
		},
		Pickups: []battleengine.Pickup{
			{SceneID: battleengine.SceneBombCapacitySmall, Cell: walls[0], State: battleengine.PickupHidden},
			{SceneID: battleengine.SceneBombPowerSmall, Cell: walls[1], State: battleengine.PickupHidden},
		},
	}
	if err := applyDevelopmentCurriculum(&config, 123); err != nil {
		t.Fatal(err)
	}
	usedWalls := make(map[battleengine.Cell]bool)
	for index, participant := range config.Participants {
		adjacentWall := battleengine.Cell{}
		found := false
		for _, wall := range walls {
			distance := absInt(int(participant.Spawn.Row-wall.Row)) + absInt(int(participant.Spawn.Col-wall.Col))
			if distance == 1 {
				adjacentWall = wall
				found = true
				break
			}
		}
		if !found || usedWalls[adjacentWall] {
			t.Fatalf("participant %d spawn %v did not use a distinct hidden-item wall", index, participant.Spawn)
		}
		usedWalls[adjacentWall] = true
		if !bombEscapeRouteExists(config.Grid, participant.Spawn, participant.BombPower, bombEscapeMaximumPathCells) {
			t.Fatalf("participant %d spawn %v has no native short bomb escape", index, participant.Spawn)
		}
	}
}

func TestDevelopmentCurriculumRequiresWallThenPickupAndSurvival(t *testing.T) {
	batch, err := NewBatch(testBatchConfig(1))
	if err != nil {
		t.Fatal(err)
	}
	episode := &batch.episodes[0]
	episode.metrics.DevelopmentCurriculum = true
	episode.development = developmentTrainingState{
		deadlineMS: 10_000, wallsDestroyed: make(map[byte]bool), pickups: make(map[byte]bool),
	}
	if completed, _, _ := updateDevelopmentCurriculum(episode, []battleengine.Event{{
		Kind: battleengine.EventPickupCollected, PlayerID: 1,
	}}); completed {
		t.Fatal("pickup before attributed terrain destruction completed the course")
	}
	if completed, _, _ := updateDevelopmentCurriculum(episode, []battleengine.Event{{
		Kind: battleengine.EventCellDestroyed, PlayerID: 1,
	}}); completed {
		t.Fatal("terrain destruction without collection completed the course")
	}
	completed, winner, timedOut := updateDevelopmentCurriculum(episode, []battleengine.Event{{
		Kind: battleengine.EventPickupCollected, PlayerID: 1,
	}})
	if !completed || winner != 1 || timedOut {
		t.Fatalf("development completion = %t/%d/%t, want true/1/false", completed, winner, timedOut)
	}
}

func TestDevelopmentCurriculumRequiresRepeatedSafeProductiveDetonations(t *testing.T) {
	batch, err := NewBatch(testBatchConfig(1))
	if err != nil {
		t.Fatal(err)
	}
	episode := &batch.episodes[0]
	episode.metrics.DevelopmentCurriculum = true
	episode.development = developmentTrainingState{
		deadlineMS: 30_000, requiredProductiveDetonations: 2,
		wallsDestroyed: make(map[byte]bool), pickups: make(map[byte]bool),
		productiveDetonations: make(map[byte]byte),
	}
	productive := []battleengine.Event{
		{Kind: battleengine.EventBombExploded, PlayerID: 1},
		{Kind: battleengine.EventCellDestroyed, PlayerID: 1},
	}
	if completed, _, _ := updateDevelopmentCurriculum(episode, productive); completed {
		t.Fatal("one safe productive detonation completed a two-cycle course")
	}
	if completed, _, _ := updateDevelopmentCurriculum(episode, []battleengine.Event{{
		Kind: battleengine.EventPickupCollected, PlayerID: 1,
	}}); completed {
		t.Fatal("pickup completed the course before the repeated detonation quota")
	}
	if completed, _, _ := updateDevelopmentCurriculum(episode, []battleengine.Event{
		{Kind: battleengine.EventBombExploded, PlayerID: 1},
	}); completed {
		t.Fatal("a safe explosion without real terrain progress counted as productive")
	}
	completed, winner, timedOut := updateDevelopmentCurriculum(episode, productive)
	if !completed || winner != 1 || timedOut {
		t.Fatalf("repeated development completion = %t/%d/%t, want true/1/false", completed, winner, timedOut)
	}
	if completed, _, _ := updateDevelopmentCurriculum(episode, productive); completed {
		t.Fatal("settled development course reported completion more than once")
	}
}

func TestBombEscapeRouteRejectsClosedStraightCorridor(t *testing.T) {
	grid := battleengine.Grid{Width: 5, Height: 1, Cells: make([]battleengine.Tile, 5)}
	for index := range grid.Cells {
		grid.Cells[index] = battleengine.Tile{Kind: battleengine.CellOpen, FlamePassable: true}
	}
	if bombEscapeRouteExists(grid, battleengine.Cell{Col: 2}, 4, bombEscapeMaximumPathCells) {
		t.Fatal("straight corridor entirely inside the blast was considered escapable")
	}
}

func TestBlockadeCurriculumUsesConnectedLimitedExitSpawns(t *testing.T) {
	grid := battleengine.Grid{Width: 5, Height: 5, Cells: make([]battleengine.Tile, 25)}
	for index := range grid.Cells {
		grid.Cells[index] = battleengine.Tile{Kind: battleengine.CellOpen, FlamePassable: true}
	}
	config := battleengine.Config{
		Grid: grid,
		Participants: []battleengine.Participant{
			{PlayerID: 1, TeamID: 1, BombCapacity: 1, MaxBombCapacity: 4, BombPower: 1, MaxBombPower: 4, SpeedRate: 2, MaxSpeedRate: 6},
			{PlayerID: 2, TeamID: 2, BombCapacity: 1, MaxBombCapacity: 4, BombPower: 1, MaxBombPower: 4, SpeedRate: 2, MaxSpeedRate: 6},
		},
	}
	for seed := uint64(1); seed <= 64; seed++ {
		candidate := config
		candidate.Participants = append([]battleengine.Participant(nil), config.Participants...)
		if err := applyBlockadeCurriculum(&candidate, seed); err != nil {
			t.Fatal(err)
		}
		first, second := candidate.Participants[0], candidate.Participants[1]
		if openNeighborCount(candidate.Grid, first.Spawn) > 2 || openNeighborCount(candidate.Grid, second.Spawn) > 2 {
			t.Fatalf("seed %d produced open-arena spawns: %+v/%+v", seed, first.Spawn, second.Spawn)
		}
		distance, connected := openPathDistance(candidate.Grid, first.Spawn, second.Spawn, 6)
		if !connected || distance < 2 {
			t.Fatalf("seed %d produced disconnected/distant spawns: distance=%d connected=%v", seed, distance, connected)
		}
	}
}

func TestUnavailableBlockadeCourseFallsBackToOrdinaryMap(t *testing.T) {
	const width, height = 3, 3
	field := mapdata.CompetitiveBattlefield{
		Width: width, Height: height,
		Cells: make([]mapdata.CompetitiveBattleCell, width*height),
	}
	for index := range field.Cells {
		field.Cells[index] = mapdata.CompetitiveBattleCell{Collision: mapdata.CompetitiveCellSolid}
	}
	field.Cells[0] = mapdata.CompetitiveBattleCell{Collision: mapdata.CompetitiveCellOpen, FlamePassable: true}
	field.Cells[width*height-1] = mapdata.CompetitiveBattleCell{Collision: mapdata.CompetitiveCellOpen, FlamePassable: true}
	config := testBatchConfig(1)
	config.Maps = []mapdata.CompetitiveMap{{
		ID: 3, Name: "disconnected ordinary", Selectable: true, PlayerLimit: 2,
		NativeRule: 1, Rule: mapdata.CompetitiveRuleOrdinary,
		SpawnGroupA: []mapdata.CompetitiveCell{{Row: 0, Col: 0}},
		SpawnGroupB: []mapdata.CompetitiveCell{{Row: 2, Col: 2}},
		Battlefield: field,
	}}
	config.BlockadeCurriculumPermille = 1_000
	batch, err := NewBatch(config)
	if err != nil {
		t.Fatal(err)
	}
	metrics := batch.Metrics()[0]
	if metrics.BlockadeCurriculum {
		t.Fatalf("unavailable blockade course remained marked active: %+v", metrics)
	}
	tensors, err := batch.Observe()
	if err != nil {
		t.Fatal(err)
	}
	if got := tensors.CurriculumIDs[0]; got != CurriculumOrdinary {
		t.Fatalf("fallback curriculum ID=%d, want ordinary %d", got, CurriculumOrdinary)
	}
}

func TestBatchMarksSampledDuelCurriculum(t *testing.T) {
	config := testBatchConfig(1)
	config.DuelCurriculumPermille = 1000
	batch, err := NewBatch(config)
	if err != nil {
		t.Fatal(err)
	}
	if !batch.Metrics()[0].DuelCurriculum {
		t.Fatal("forced duel curriculum reset was not reported")
	}
}

func TestTensorBatchReportsAuthoritativeCurriculumIDs(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*Config)
		want      uint8
	}{
		{name: "ordinary", configure: func(*Config) {}, want: CurriculumOrdinary},
		{name: "duel", configure: func(config *Config) { config.DuelCurriculumPermille = 1000 }, want: CurriculumDuel},
		{name: "blockade", configure: func(config *Config) { config.BlockadeCurriculumPermille = 1000 }, want: CurriculumBlockade},
		{name: "chain", configure: func(config *Config) { config.ChainFinisherCurriculumPermille = 1000 }, want: CurriculumChainFinisher},
		{name: "escape", configure: func(config *Config) { config.BombEscapeCurriculumPermille = 1000 }, want: CurriculumBombEscape},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := testBatchConfig(1)
			test.configure(&config)
			batch, err := NewBatch(config)
			if err != nil {
				t.Fatal(err)
			}
			tensors, err := batch.Observe()
			if err != nil {
				t.Fatal(err)
			}
			if got := tensors.CurriculumIDs[0]; got != test.want {
				t.Fatalf("curriculum ID=%d, want %d", got, test.want)
			}
		})
	}
}

func TestDuelCurriculumCanFixNativeBombCapacity(t *testing.T) {
	config := testBatchConfig(1)
	config.DuelCurriculumPermille = 1000
	config.DuelCurriculumBombCapacity = 2
	batch, err := NewBatch(config)
	if err != nil {
		t.Fatal(err)
	}
	for _, actor := range batch.episodes[0].engine.Actors() {
		if actor.BombCapacity != 2 {
			t.Fatalf("duel course capacity=%d, want 2", actor.BombCapacity)
		}
	}
}

func TestBatchRejectsInvalidDuelCurriculumProbability(t *testing.T) {
	config := testBatchConfig(1)
	config.DuelCurriculumPermille = 1001
	if _, err := NewBatch(config); err == nil {
		t.Fatal("invalid duel curriculum probability was accepted")
	}
}

func TestCurriculumSlotStratificationPreservesConfiguredOccupancy(t *testing.T) {
	config := Config{EnvCount: 10, StratifyCurriculumSlots: true}
	want := []uint64{50, 150, 250, 350, 450, 550, 650, 750, 850, 950}
	for envIndex, expected := range want {
		first := curriculumSelectionRoll(config, envIndex, 1)
		second := curriculumSelectionRoll(config, envIndex, 999)
		if first != expected || second != expected {
			t.Fatalf("environment %d rolls=%d/%d, want stable stratum %d", envIndex, first, second, expected)
		}
	}
}

func TestBatchMarksSampledBombEscapeCurriculum(t *testing.T) {
	config := testBatchConfig(1)
	config.BombEscapeCurriculumPermille = 1000
	batch, err := NewBatch(config)
	if err != nil {
		t.Fatal(err)
	}
	metrics := batch.Metrics()[0]
	if !metrics.BombEscapeCurriculum || metrics.DuelCurriculum {
		t.Fatalf("forced bomb escape curriculum reset was not isolated: %+v", metrics)
	}
	tensors, err := batch.Observe()
	if err != nil {
		t.Fatal(err)
	}
	if metrics.BombEscapeTrainingTeamID == 0 || tensors.TrainingTeamIDs[0] != metrics.BombEscapeTrainingTeamID {
		t.Fatalf("bomb escape training team metadata=%d/%d", metrics.BombEscapeTrainingTeamID, tensors.TrainingTeamIDs[0])
	}
	engine := batch.episodes[0].engine
	if got, want := len(engine.Bombs()), 2; got != want {
		t.Fatalf("seeded bomb escape bubbles=%d, want %d", got, want)
	}
}

func TestBatchSeedsSampledChainFinisherCurriculum(t *testing.T) {
	config := testBatchConfig(1)
	config.ChainFinisherCurriculumPermille = 1000
	batch, err := NewBatch(config)
	if err != nil {
		t.Fatal(err)
	}
	metrics := batch.Metrics()[0]
	if !metrics.ChainFinisherCurriculum || metrics.DuelCurriculum || metrics.BlockadeCurriculum || metrics.BombEscapeCurriculum {
		t.Fatalf("forced chain finisher reset was not isolated: %+v", metrics)
	}
	engine := batch.episodes[0].engine
	bombs := engine.Bombs()
	if len(bombs) != 3 {
		t.Fatalf("chain finisher root bubbles=%d, want 3: %+v", len(bombs), bombs)
	}
	seenCells := make(map[battleengine.Cell]struct{}, len(bombs))
	previousRemaining := uint32(0)
	for index, bomb := range bombs {
		remaining := bomb.EffectiveExplodeAtMS() - engine.ElapsedMS()
		if index == 0 && (remaining < 900 || remaining > 1_000) {
			t.Fatalf("first chain root remaining=%dms, want 900..1000", remaining)
		}
		if index != 0 && remaining-previousRemaining != 900 {
			t.Fatalf("chain root %d spacing=%dms, want 900", index, remaining-previousRemaining)
		}
		if _, duplicate := seenCells[bomb.Cell]; duplicate {
			t.Fatalf("duplicate chain root cell %+v", bomb.Cell)
		}
		seenCells[bomb.Cell] = struct{}{}
		previousRemaining = remaining
	}
	attacker := -1
	attackerRoots := 0
	for index, actor := range engine.Actors() {
		if actor.TeamID == metrics.ChainFinisherAttackerTeamID {
			attacker = index
			for _, bomb := range bombs {
				if bomb.OwnerID == actor.PlayerID {
					attackerRoots++
				}
			}
		}
	}
	if attacker < 0 {
		t.Fatalf("chain finisher attacker team %d is absent", metrics.ChainFinisherAttackerTeamID)
	}
	actors := engine.Actors()
	if reserve := int(actors[attacker].BombCapacity) - attackerRoots; reserve < 2 {
		t.Fatalf("rolling chain bubble reserve=%d, want at least 2: actor=%+v", reserve, actors[attacker])
	}
	timeline, err := engine.DangerTimeline(3_000)
	if err != nil {
		t.Fatal(err)
	}
	for _, bomb := range bombs {
		impactAt, ok := timeline.ImpactAt(bomb.Cell)
		if !ok || impactAt < bomb.EffectiveExplodeAtMS() || impactAt > bomb.EffectiveExplodeAtMS()+config.TickMS {
			t.Fatalf("root %+v chains before its own fuse: impact=%d found=%t", bomb, impactAt, ok)
		}
	}
}

func TestCompactThreeBubbleChainCourseSeedsTwoDynamicRoots(t *testing.T) {
	config := testBatchConfig(8)
	config.ChainFinisherCurriculumPermille = 1_000
	config.ChainFinisherMinimumPresetRoots = 2
	config.ChainFinisherMaximumPresetRoots = 2
	batch, err := NewBatch(config)
	if err != nil {
		t.Fatal(err)
	}
	for envIndex, metrics := range batch.Metrics() {
		if !metrics.ChainFinisherCurriculum {
			t.Fatalf("environment %d compact chain course fell back to ordinary", envIndex)
		}
		if metrics.ChainFinisherPresetRoots != 2 || metrics.ChainFinisherRequiredTriggers != 1 {
			t.Fatalf(
				"environment %d compact roots/triggers=%d/%d, want 2/1",
				envIndex, metrics.ChainFinisherPresetRoots, metrics.ChainFinisherRequiredTriggers,
			)
		}
		if bombs := batch.episodes[envIndex].engine.Bombs(); len(bombs) != 2 {
			t.Fatalf("environment %d seeded bombs=%d, want two: %+v", envIndex, len(bombs), bombs)
		}
	}
}

func TestUnavailableRollingChainCourseFallsBackToOrdinaryMap(t *testing.T) {
	const width, height = 4, 4
	field := mapdata.CompetitiveBattlefield{
		Width: width, Height: height,
		Cells: make([]mapdata.CompetitiveBattleCell, width*height),
	}
	for index := range field.Cells {
		field.Cells[index] = mapdata.CompetitiveBattleCell{
			Collision: mapdata.CompetitiveCellOpen, FlamePassable: true,
		}
	}
	config := testBatchConfig(1)
	config.Maps = []mapdata.CompetitiveMap{{
		ID: 2, Name: "compact ordinary", Selectable: true, PlayerLimit: 2,
		NativeRule: 1, Rule: mapdata.CompetitiveRuleOrdinary,
		SpawnGroupA: []mapdata.CompetitiveCell{{Row: 0, Col: 0}},
		SpawnGroupB: []mapdata.CompetitiveCell{{Row: 3, Col: 3}},
		Battlefield: field,
	}}
	config.ChainFinisherCurriculumPermille = 1_000
	batch, err := NewBatch(config)
	if err != nil {
		t.Fatal(err)
	}
	metrics := batch.Metrics()[0]
	if metrics.ChainFinisherCurriculum {
		t.Fatalf("unavailable rolling course remained marked active: %+v", metrics)
	}
	tensors, err := batch.Observe()
	if err != nil {
		t.Fatal(err)
	}
	if got := tensors.CurriculumIDs[0]; got != CurriculumOrdinary {
		t.Fatalf("fallback curriculum ID=%d, want ordinary %d", got, CurriculumOrdinary)
	}
	if bombs := batch.episodes[0].engine.Bombs(); len(bombs) != 0 {
		t.Fatalf("ordinary fallback retained staged root bubbles: %+v", bombs)
	}
}

func TestChainFinisherCurriculumBindsAttackerAndSamplesAssistance(t *testing.T) {
	config := testBatchConfig(12)
	config.ChainFinisherCurriculumPermille = 1000
	config.ChainFinisherMinimumPresetRoots = 1
	batch, err := NewBatch(config)
	if err != nil {
		t.Fatal(err)
	}
	tensors, err := batch.Observe()
	if err != nil {
		t.Fatal(err)
	}
	seenFriendlyRoot, seenHostileRoot := false, false
	for envIndex, metrics := range batch.Metrics() {
		if metrics.ChainFinisherPresetRoots < 1 || metrics.ChainFinisherPresetRoots > 3 {
			t.Fatalf("environment %d preset roots=%d, want 1..3", envIndex, metrics.ChainFinisherPresetRoots)
		}
		if metrics.ChainFinisherRequiredTriggers < 2 {
			t.Fatalf("environment %d required triggers=%d, want at least 2", envIndex, metrics.ChainFinisherRequiredTriggers)
		}
		if tensors.TrainingTeamIDs[envIndex] == 0 || tensors.TrainingTeamIDs[envIndex] != metrics.ChainFinisherAttackerTeamID {
			t.Fatalf("environment %d training team=%d attacker=%d", envIndex, tensors.TrainingTeamIDs[envIndex], metrics.ChainFinisherAttackerTeamID)
		}
		teams := make(map[uint16]byte)
		for _, actor := range batch.episodes[envIndex].engine.Actors() {
			teams[actor.PlayerID] = actor.TeamID
		}
		for _, bomb := range batch.episodes[envIndex].engine.Bombs() {
			if teams[bomb.OwnerID] == metrics.ChainFinisherAttackerTeamID {
				seenFriendlyRoot = true
			} else {
				seenHostileRoot = true
			}
		}
	}
	if !seenFriendlyRoot || !seenHostileRoot {
		t.Fatalf("sampled roots did not cover both owners: friendly=%t hostile=%t", seenFriendlyRoot, seenHostileRoot)
	}
}

func TestChainFinisherCurriculumCanFixOneRootThreeTriggerLesson(t *testing.T) {
	config := testBatchConfig(8)
	config.ChainFinisherCurriculumPermille = 1000
	config.ChainFinisherMinimumPresetRoots = 1
	config.ChainFinisherMaximumPresetRoots = 1
	batch, err := NewBatch(config)
	if err != nil {
		t.Fatal(err)
	}
	for envIndex, metrics := range batch.Metrics() {
		if metrics.ChainFinisherPresetRoots != 1 || metrics.ChainFinisherRequiredTriggers != 3 {
			t.Fatalf("environment %d lesson roots/triggers=%d/%d, want 1/3", envIndex, metrics.ChainFinisherPresetRoots, metrics.ChainFinisherRequiredTriggers)
		}
		if bombs := batch.episodes[envIndex].engine.Bombs(); len(bombs) != 1 {
			t.Fatalf("environment %d seeded bombs=%d, want one", envIndex, len(bombs))
		}
	}
}

func TestBatchRejectsReversedChainFinisherRootRange(t *testing.T) {
	config := testBatchConfig(1)
	config.ChainFinisherMinimumPresetRoots = 2
	config.ChainFinisherMaximumPresetRoots = 1
	if _, err := NewBatch(config); err == nil {
		t.Fatal("reversed chain finisher root range was accepted")
	}
}

func TestChainFinisherStrictSuccessAcceptsAuthoritativeHitBeforePracticeQuota(t *testing.T) {
	config := testBatchConfig(1)
	config.ChainFinisherCurriculumPermille = 1000
	batch, err := NewBatch(config)
	if err != nil {
		t.Fatal(err)
	}
	episode := &batch.episodes[0]
	state := &episode.chainFinisher
	state.requiredTriggers = 3
	actors := episode.engine.Actors()
	var defenderID uint16
	for _, actor := range actors {
		if actor.TeamID == state.defenderTeamID {
			defenderID = actor.PlayerID
			break
		}
	}
	if defenderID == 0 {
		t.Fatal("chain defender is absent")
	}
	events := []battleengine.Event{
		{Kind: battleengine.EventBombPlaced, PlayerID: state.attackerID, BombID: 1001},
		{Kind: battleengine.EventBombExploded, PlayerID: state.attackerID, BombID: 1001},
		{Kind: battleengine.EventActorTrapped, PlayerID: state.attackerID, TargetID: defenderID},
	}
	result, winner := updateChainFinisherCurriculum(episode, events)
	if result != chainFinisherSuccess || winner != state.attackerTeamID || !episode.metrics.ChainFinisherSucceeded {
		t.Fatalf("early authoritative hit result=%d winner=%d metrics=%+v", result, winner, episode.metrics)
	}
}

func TestChainFinisherTriggerFoundationCompletesWithoutDefenderTrap(t *testing.T) {
	config := testBatchConfig(1)
	config.ChainFinisherCurriculumPermille = 1000
	config.ChainFinisherTriggerOnly = true
	batch, err := NewBatch(config)
	if err != nil {
		t.Fatal(err)
	}
	episode := &batch.episodes[0]
	state := &episode.chainFinisher
	state.requiredTriggers = 2
	events := []battleengine.Event{
		{Kind: battleengine.EventBombPlaced, TimeMS: 100, PlayerID: state.attackerID, BombID: 2001},
		{Kind: battleengine.EventBombExploded, TimeMS: 200, PlayerID: state.attackerID, BombID: 2001},
	}
	if result, winner := updateChainFinisherCurriculum(episode, events); result != 0 || winner != 0 {
		t.Fatalf("one foundation trigger result=%d winner=%d, want lesson to continue", result, winner)
	}
	events = []battleengine.Event{
		{Kind: battleengine.EventBombPlaced, TimeMS: 300, PlayerID: state.attackerID, BombID: 2002},
		{Kind: battleengine.EventBombExploded, TimeMS: 400, PlayerID: state.attackerID, BombID: 2002},
	}
	result, winner := updateChainFinisherCurriculum(episode, events)
	if result != chainFinisherSuccess || winner != state.attackerTeamID {
		t.Fatalf("trigger foundation result=%d winner=%d, want success for team %d", result, winner, state.attackerTeamID)
	}
}

func TestChainFinisherPressureFoundationRequiresMeaningfulExitCoverage(t *testing.T) {
	config := testBatchConfig(1)
	config.ChainFinisherCurriculumPermille = 1000
	config.ChainFinisherPressureOnly = true
	batch, err := NewBatch(config)
	if err != nil {
		t.Fatal(err)
	}
	episode := &batch.episodes[0]
	state := &episode.chainFinisher
	state.requiredTriggers = 1
	escapes := make([]battleengine.Cell, 0, 4)
	for _, actor := range episode.engine.Actors() {
		if actor.TeamID != state.defenderTeamID {
			continue
		}
		cell := actor.Position.Cell()
		for _, candidate := range []battleengine.Cell{
			{Row: cell.Row - 1, Col: cell.Col}, {Row: cell.Row + 1, Col: cell.Col},
			{Row: cell.Row, Col: cell.Col - 1}, {Row: cell.Row, Col: cell.Col + 1},
		} {
			tile, inside := episode.engine.TileAt(candidate)
			if inside && tile.Kind == battleengine.CellOpen && !tile.MapElementOccupied {
				escapes = append(escapes, candidate)
			}
		}
	}
	if len(escapes) < 2 {
		t.Fatalf("chain defender has %d open escape cells, want at least two", len(escapes))
	}
	events := []battleengine.Event{
		{Kind: battleengine.EventBombPlaced, TimeMS: 100, PlayerID: state.attackerID, BombID: 3001},
		{Kind: battleengine.EventBombExploded, TimeMS: 200, PlayerID: state.attackerID, BombID: 3001,
			Cell: escapes[0], BlastRowMin: escapes[0].Row, BlastRowMax: escapes[0].Row,
			BlastColMin: escapes[0].Col, BlastColMax: escapes[0].Col},
	}
	result, winner := updateChainFinisherCurriculum(episode, events)
	if result != 0 || winner != 0 {
		t.Fatalf("one weakly covered exit result=%d winner=%d, want lesson to continue", result, winner)
	}
	if episode.metrics.ChainFinisherPressureCells != 1 {
		t.Fatalf("pressure cells=%d, want one covered escape cell", episode.metrics.ChainFinisherPressureCells)
	}
	events = []battleengine.Event{
		{Kind: battleengine.EventBombPlaced, TimeMS: 300, PlayerID: state.attackerID, BombID: 3002},
		{Kind: battleengine.EventBombPlaced, TimeMS: 300, PlayerID: state.attackerID, BombID: 3003},
		{Kind: battleengine.EventBombExploded, TimeMS: 400, PlayerID: state.attackerID, BombID: 3002,
			Cell: escapes[0], BlastRowMin: escapes[0].Row, BlastRowMax: escapes[0].Row,
			BlastColMin: escapes[0].Col, BlastColMax: escapes[0].Col},
		{Kind: battleengine.EventBombExploded, TimeMS: 400, PlayerID: state.attackerID, BombID: 3003,
			Cell: escapes[1], BlastRowMin: escapes[1].Row, BlastRowMax: escapes[1].Row,
			BlastColMin: escapes[1].Col, BlastColMax: escapes[1].Col},
	}
	result, winner = updateChainFinisherCurriculum(episode, events)
	if result != chainFinisherSuccess || winner != state.attackerTeamID {
		t.Fatalf("two covered exits result=%d winner=%d, want success for team %d", result, winner, state.attackerTeamID)
	}
}

func TestBatchRejectsConflictingChainFinisherLessons(t *testing.T) {
	config := testBatchConfig(1)
	config.ChainFinisherTriggerOnly = true
	config.ChainFinisherPressureOnly = true
	if _, err := NewBatch(config); err == nil {
		t.Fatal("conflicting chain finisher lessons were accepted")
	}
}

func TestChainFinisherSurvivingDeadlineIsNeutralFailure(t *testing.T) {
	config := testBatchConfig(1)
	config.ChainFinisherCurriculumPermille = 1000
	batch, err := NewBatch(config)
	if err != nil {
		t.Fatal(err)
	}
	episode := &batch.episodes[0]
	episode.chainFinisher.deadlineMS = 0
	result, winner := updateChainFinisherCurriculum(episode, nil)
	if result != chainFinisherFailure || winner != 0 || !episode.metrics.ChainFinisherFailed {
		t.Fatalf("surviving deadline result=%d winner=%d metrics=%+v, want neutral failure", result, winner, episode.metrics)
	}
}

func TestBatchRejectsOverlappingCurriculumProbability(t *testing.T) {
	config := testBatchConfig(1)
	config.DuelCurriculumPermille = 600
	config.BombEscapeCurriculumPermille = 500
	if _, err := NewBatch(config); err == nil {
		t.Fatal("combined curriculum probability above 100% was accepted")
	}
}

func TestSafeBombEscapeOwnersRequirePostBlastActiveActor(t *testing.T) {
	actors := []battleengine.Actor{
		{Participant: battleengine.Participant{PlayerID: 1, TeamID: 1}, State: battleengine.ActorActive},
		{Participant: battleengine.Participant{PlayerID: 2, TeamID: 2}, State: battleengine.ActorTrapped},
	}
	events := []battleengine.Event{{Kind: battleengine.EventBombExploded, PlayerID: 1}}
	if owners := safeBombEscapeOwners(events, actors); !reflect.DeepEqual(owners, []uint16{1}) {
		t.Fatalf("safe owner was not credited: %v", owners)
	}
	events = []battleengine.Event{{Kind: battleengine.EventBombExploded, PlayerID: 2}}
	if owners := safeBombEscapeOwners(events, actors); len(owners) != 0 {
		t.Fatalf("trapped owner received escape credit: %v", owners)
	}
	actors[1].State = battleengine.ActorActive
	events = []battleengine.Event{
		{Kind: battleengine.EventBombExploded, PlayerID: 1},
		{Kind: battleengine.EventBombExploded, PlayerID: 2},
		{Kind: battleengine.EventBombExploded, PlayerID: 1},
	}
	if owners := safeBombEscapeOwners(events, actors); !reflect.DeepEqual(owners, []uint16{1, 2}) {
		t.Fatalf("simultaneous safe owners were not returned once each: %v", owners)
	}
}

func TestBombEscapeCurriculumOnlyDesignatedTeamCanComplete(t *testing.T) {
	config := testBatchConfig(1)
	config.BombEscapeCurriculumPermille = 1_000
	config.BombEscapeRequiredDetonations = 2
	batch, err := NewBatch(config)
	if err != nil {
		t.Fatal(err)
	}
	episode := &batch.episodes[0]
	var learnerID, opponentID uint16
	for _, actor := range episode.engine.Actors() {
		if actor.TeamID == episode.metrics.BombEscapeTrainingTeamID {
			learnerID = actor.PlayerID
		} else {
			opponentID = actor.PlayerID
		}
	}
	if learnerID == 0 || opponentID == 0 {
		t.Fatalf("invalid learner/opponent binding: learner=%d opponent=%d metrics=%+v", learnerID, opponentID, episode.metrics)
	}
	if completed, winner := updateBombEscapeCurriculum(
		episode,
		[]battleengine.Event{{Kind: battleengine.EventBombExploded, PlayerID: opponentID}},
		2,
	); completed || winner != 0 {
		t.Fatalf("opponent escape completed learner course: completed=%t winner=%d", completed, winner)
	}
	learnerIndex := actorIndexByPlayerID(episode.engine.Actors(), learnerID)
	opponentIndex := actorIndexByPlayerID(episode.engine.Actors(), opponentID)
	if episode.training[opponentIndex].safeDetonations != 0 {
		t.Fatalf("opponent progress was recorded: %+v", episode.training[opponentIndex])
	}
	event := []battleengine.Event{{Kind: battleengine.EventBombExploded, PlayerID: learnerID}}
	if completed, winner := updateBombEscapeCurriculum(episode, event, 2); completed || winner != 0 {
		t.Fatalf("course completed after one learner escape: completed=%t winner=%d", completed, winner)
	}
	completed, winner := updateBombEscapeCurriculum(episode, event, 2)
	if !completed || winner != episode.metrics.BombEscapeTrainingTeamID || episode.training[learnerIndex].safeDetonations != 2 {
		t.Fatalf("learner course completion=%t winner=%d progress=%+v", completed, winner, episode.training[learnerIndex])
	}
}

func TestAgentMetricsCountAuthoritativeOwnedBombSurvival(t *testing.T) {
	playerIDs := []uint16{1, 2}
	actors := []battleengine.Actor{
		{Participant: battleengine.Participant{PlayerID: 1, TeamID: 1}, State: battleengine.ActorActive},
		{Participant: battleengine.Participant{PlayerID: 2, TeamID: 2}, State: battleengine.ActorEliminated},
	}
	metrics := make([]AgentMetrics, 2)
	for _, event := range []battleengine.Event{
		{Kind: battleengine.EventBombExploded, PlayerID: 1, BombID: 101},
		{Kind: battleengine.EventBombExploded, PlayerID: 1, BombID: 102},
		{Kind: battleengine.EventBombExploded, PlayerID: 2, BombID: 201},
	} {
		recordOwnedBombDetonation(metrics, playerIDs, actors, event)
	}
	first := metrics[0]
	second := metrics[1]
	if first.OwnedBombDetonations != 2 || first.SurvivedOwnedBombDetonations != 2 {
		t.Fatalf("surviving owner metrics = %+v", first)
	}
	if second.OwnedBombDetonations != 1 || second.SurvivedOwnedBombDetonations != 0 {
		t.Fatalf("eliminated owner metrics = %+v", second)
	}
}

func TestBombEscapeCurriculumResultRequiresObservedCompletion(t *testing.T) {
	tests := []struct {
		name       string
		curriculum bool
		outcome    battleengine.Outcome
		succeeded  bool
		winnerTeam byte
		want       uint8
	}{
		{name: "ordinary battle", outcome: battleengine.Outcome{Ended: true}, succeeded: true, winnerTeam: 1},
		{name: "projection is not completion", curriculum: true, succeeded: false},
		{name: "authoritative team one completion", curriculum: true, succeeded: true, winnerTeam: 1, want: 1},
		{name: "authoritative team two completion", curriculum: true, succeeded: true, winnerTeam: 2, want: 2},
		{name: "terminal course miss", curriculum: true, outcome: battleengine.Outcome{Ended: true}, want: bombEscapeFailure},
		{name: "zero winner is not completion", curriculum: true, outcome: battleengine.Outcome{Ended: true}, succeeded: true, want: bombEscapeFailure},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := bombEscapeCurriculumResult(
				test.curriculum,
				test.outcome,
				test.succeeded,
				test.winnerTeam,
			)
			if got != test.want {
				t.Fatalf("result=%d, want %d", got, test.want)
			}
		})
	}
}

func TestDevelopmentCurriculumResultRequiresObservedCompletion(t *testing.T) {
	tests := []struct {
		name       string
		curriculum bool
		outcome    battleengine.Outcome
		completed  bool
		winnerTeam byte
		reported   bool
		want       uint8
	}{
		{name: "ordinary battle", outcome: battleengine.Outcome{Ended: true}, completed: true, winnerTeam: 1},
		{name: "unfinished lesson", curriculum: true},
		{name: "authoritative team one completion", curriculum: true, completed: true, winnerTeam: 1, want: 1},
		{name: "authoritative team two completion", curriculum: true, completed: true, winnerTeam: 2, want: 2},
		{name: "resolved course miss before battle end", curriculum: true, completed: true, want: developmentFailure},
		{name: "ordinary elimination is a course miss", curriculum: true, outcome: battleengine.Outcome{Ended: true, WinnerTeamID: 1}, want: developmentFailure},
		{name: "zero winner is not completion", curriculum: true, outcome: battleengine.Outcome{Ended: true}, completed: true, want: developmentFailure},
		{name: "reported completion is not duplicated", curriculum: true, completed: true, winnerTeam: 1, reported: true},
		{name: "reported miss is not duplicated at terminal", curriculum: true, outcome: battleengine.Outcome{Ended: true}, reported: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := developmentCurriculumResult(
				test.curriculum,
				test.outcome,
				test.completed,
				test.winnerTeam,
				test.reported,
			)
			if got != test.want {
				t.Fatalf("result=%d, want %d", got, test.want)
			}
		})
	}
}

func TestDevelopmentCourseResolutionDoesNotEndMatch(t *testing.T) {
	batch, err := NewBatch(testBatchConfig(1))
	if err != nil {
		t.Fatal(err)
	}
	episode := &batch.episodes[0]
	episode.metrics.DevelopmentCurriculum = true
	episode.development = developmentTrainingState{
		deadlineMS: 1, wallsDestroyed: make(map[byte]bool), pickups: make(map[byte]bool),
		productiveDetonations: make(map[byte]byte),
	}
	actions := [][]battleengine.ActionID{{battleengine.ActionWait, battleengine.ActionWait}}
	result, err := batch.Step(actions)
	if err != nil {
		t.Fatal(err)
	}
	if got := result.DevelopmentResults[0]; got != developmentFailure {
		t.Fatalf("development result=%d, want failure=%d", got, developmentFailure)
	}
	if result.Dones[0] != 0 || result.Outcomes[0].Ended || episode.engine.Terminal().Ended {
		t.Fatalf("course miss ended the real match: done=%d outcome=%+v", result.Dones[0], result.Outcomes[0])
	}
	result, err = batch.Step(actions)
	if err != nil {
		t.Fatal(err)
	}
	if result.DevelopmentResults[0] != 0 || result.Dones[0] != 0 {
		t.Fatalf("settled course result repeated: result=%d done=%d", result.DevelopmentResults[0], result.Dones[0])
	}
}

func TestBatchDecisionHoldsMovementButPulsesPlacementOnce(t *testing.T) {
	batch, err := NewBatch(testBatchConfig(1))
	if err != nil {
		t.Fatal(err)
	}
	before := batch.episodes[0].engine.Actors()[0]
	result, err := batch.Step([][]battleengine.ActionID{{battleengine.ActionMoveRightAndPlaceBomb, battleengine.ActionWait}})
	if err != nil {
		t.Fatal(err)
	}
	after := batch.episodes[0].engine.Actors()[0]
	if after.Position.X <= before.Position.X {
		t.Fatalf("held decision did not move actor: before=%+v after=%+v", before.Position, after.Position)
	}
	if got := len(batch.episodes[0].engine.Bombs()); got != 1 {
		t.Fatalf("placement pulse repeated across decision ticks: bombs=%d", got)
	}
	if result.Dones[0] != 0 || result.Observation.EnvCount != 1 || batch.episodes[0].metrics.Ticks != 5 {
		t.Fatalf("unexpected decision result: done=%d envs=%d metrics=%+v", result.Dones[0], result.Observation.EnvCount, batch.episodes[0].metrics)
	}
}

func TestBatchRejectsItemFieldAndSpecialRuleMaps(t *testing.T) {
	config := testBatchConfig(1)
	config.Maps[0].RequiredItemField = 1
	if _, err := NewBatch(config); err == nil {
		t.Fatal("item-field map entered restricted training batch")
	}
	config = testBatchConfig(1)
	config.Maps[0].NativeRule = 2
	config.Maps[0].Rule = mapdata.CompetitiveRuleKickBomb
	if _, err := NewBatch(config); err == nil {
		t.Fatal("special-rule map entered restricted training batch")
	}
}

func TestBatchAssignsEqualSizedMultiTeams(t *testing.T) {
	config := testBatchConfig(1)
	config.ParticipantCount = 4
	config.TeamCount = 4
	config.Maps[0].PlayerLimit = 4
	config.Maps[0].SpawnGroupA = []mapdata.CompetitiveCell{{Row: 0, Col: 0}, {Row: 0, Col: 4}}
	config.Maps[0].SpawnGroupB = []mapdata.CompetitiveCell{{Row: 2, Col: 0}, {Row: 2, Col: 4}}
	batch, err := NewBatch(config)
	if err != nil {
		t.Fatal(err)
	}
	actors := batch.episodes[0].engine.Actors()
	for index, actor := range actors {
		if want := byte(index + 1); actor.TeamID != want {
			t.Fatalf("actor %d team = %d, want %d", index, actor.TeamID, want)
		}
	}
}

func TestBatchAcceptsExplicitUnevenTeams(t *testing.T) {
	config := testBatchConfig(1)
	config.ParticipantCount = 4
	config.TeamLayouts = [][]uint8{{1, 2, 2, 2}}
	config.Maps[0].PlayerLimit = 4
	config.Maps[0].SpawnGroupA = []mapdata.CompetitiveCell{{Row: 0, Col: 0}, {Row: 0, Col: 4}}
	config.Maps[0].SpawnGroupB = []mapdata.CompetitiveCell{{Row: 2, Col: 0}, {Row: 2, Col: 4}}
	batch, err := NewBatch(config)
	if err != nil {
		t.Fatal(err)
	}
	actors := batch.episodes[0].engine.Actors()
	for index, want := range []byte{1, 2, 2, 2} {
		if actors[index].TeamID != want {
			t.Fatalf("actor %d team=%d, want %d", index, actors[index].TeamID, want)
		}
	}
	observation, err := batch.Observe()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(observation.TeamIDs, []uint8{1, 2, 2, 2}) {
		t.Fatalf("tensor team IDs=%v", observation.TeamIDs)
	}
}

func TestBatchMixesParticipantCountsWithInactivePadding(t *testing.T) {
	config := testBatchConfig(16)
	config.ParticipantCount = 4
	config.ParticipantCounts = []int{2, 4}
	config.TeamLayouts = [][]uint8{{1, 2}, {1, 1, 2, 2}, {1, 2, 3, 4}}
	config.Maps[0].PlayerLimit = 4
	config.Maps[0].SpawnGroupA = []mapdata.CompetitiveCell{{Row: 0, Col: 0}, {Row: 0, Col: 4}}
	config.Maps[0].SpawnGroupB = []mapdata.CompetitiveCell{{Row: 2, Col: 0}, {Row: 2, Col: 4}}
	batch, err := NewBatch(config)
	if err != nil {
		t.Fatal(err)
	}
	observation, err := batch.Observe()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[int]bool{}
	actions := make([][]battleengine.ActionID, len(batch.episodes))
	for envIndex, episode := range batch.episodes {
		count := len(episode.playerIDs)
		seen[count] = true
		actions[envIndex] = make([]battleengine.ActionID, config.ParticipantCount)
		for actorIndex := 0; actorIndex < config.ParticipantCount; actorIndex++ {
			active := observation.Active[envIndex*config.ParticipantCount+actorIndex]
			teamID := observation.TeamIDs[envIndex*config.ParticipantCount+actorIndex]
			if actorIndex < count && (active != 1 || teamID == 0) {
				t.Fatalf("environment %d real actor %d active/team=%d/%d", envIndex, actorIndex, active, teamID)
			}
			if actorIndex >= count && (active != 0 || teamID != 0) {
				t.Fatalf("environment %d padded actor %d active/team=%d/%d", envIndex, actorIndex, active, teamID)
			}
		}
	}
	if !seen[2] || !seen[4] {
		t.Fatalf("deterministic mixed reset counts=%v, want both 2 and 4", seen)
	}
	if _, err := batch.Step(actions); err != nil {
		t.Fatalf("padded action tensor was rejected: %v", err)
	}
}

func TestBatchUsesNativeTeamSpawnsOnlyForBalancedTwoTeamLayouts(t *testing.T) {
	config := testBatchConfig(1)
	config.ParticipantCount = 4
	config.TeamLayouts = [][]uint8{{1, 1, 2, 2}}
	config.TeamSpawnPermille = 1000
	config.Maps[0].PlayerLimit = 4
	config.Maps[0].SpawnGroupA = []mapdata.CompetitiveCell{{Row: 0, Col: 0}, {Row: 0, Col: 4}}
	config.Maps[0].SpawnGroupB = []mapdata.CompetitiveCell{{Row: 2, Col: 0}, {Row: 2, Col: 4}}
	batch, err := NewBatch(config)
	if err != nil {
		t.Fatal(err)
	}
	actors := batch.episodes[0].engine.Actors()
	for _, actor := range actors {
		if actor.TeamID == 1 && actor.Position.Cell().Row != 0 {
			t.Fatalf("team 1 actor spawned outside native group A: %+v", actor)
		}
		if actor.TeamID == 2 && actor.Position.Cell().Row != 2 {
			t.Fatalf("team 2 actor spawned outside native group B: %+v", actor)
		}
	}

	config.TeamLayouts = [][]uint8{{1, 2, 2, 2}}
	if _, err = NewBatch(config); err != nil {
		t.Fatalf("uneven layout did not fall back to native free spawn: %v", err)
	}
}

func TestBatchRejectsMalformedExplicitTeamLayout(t *testing.T) {
	config := testBatchConfig(1)
	config.TeamLayouts = [][]uint8{{1, 3}}
	if _, err := NewBatch(config); err == nil {
		t.Fatal("non-contiguous explicit TeamIDs were accepted")
	}
}

func TestEventRewardsDoNotCreditSelfDamage(t *testing.T) {
	batch, err := NewBatch(testBatchConfig(1))
	if err != nil {
		t.Fatal(err)
	}
	rewards := make([]float32, 2)
	batch.accumulateEvents(0, []battleengine.Event{
		{Kind: battleengine.EventBombPlaced, PlayerID: 1},
		{Kind: battleengine.EventActorTrapped, PlayerID: 1, TargetID: 1},
	}, rewards)
	if got, want := rewards[0], rewardBombPlaced-rewardTrap; got != want {
		t.Fatalf("self-trap reward = %v, want %v", got, want)
	}
	rewards = make([]float32, 2)
	batch.accumulateEvents(0, []battleengine.Event{{Kind: battleengine.EventActorTrapped, PlayerID: 1, TargetID: 2}}, rewards)
	if rewards[0] != rewardTrap || rewards[1] != -rewardTrap {
		t.Fatalf("enemy trap rewards = %v, want [%v %v]", rewards, rewardTrap, -rewardTrap)
	}
}

func TestEventRewardsPenalizeFriendlyFire(t *testing.T) {
	config := testBatchConfig(1)
	config.ParticipantCount = 4
	config.TeamCount = 2
	config.Maps[0].PlayerLimit = 4
	config.Maps[0].SpawnGroupA = []mapdata.CompetitiveCell{{Row: 0, Col: 0}, {Row: 0, Col: 4}}
	config.Maps[0].SpawnGroupB = []mapdata.CompetitiveCell{{Row: 2, Col: 0}, {Row: 2, Col: 4}}
	batch, err := NewBatch(config)
	if err != nil {
		t.Fatal(err)
	}
	rewards := make([]float32, 4)
	batch.accumulateEvents(0, []battleengine.Event{
		{Kind: battleengine.EventActorTrapped, PlayerID: 1, TargetID: 2},
	}, rewards)
	teamScale := float32(0.5)
	if rewards[0] != -rewardTrap*teamScale*rewardFriendlyFireFactor || rewards[1] != -rewardTrap*teamScale {
		t.Fatalf("friendly trap rewards = %v, want mild attacker and full target penalties", rewards)
	}
	for _, value := range rewards[2:] {
		if value != 0 {
			t.Fatalf("unrelated opponent received friendly-fire reward: %v", rewards)
		}
	}
}

func TestOffensiveMetricsExcludeSelfFriendlyAndUnknownTargets(t *testing.T) {
	config := testBatchConfig(1)
	config.ParticipantCount = 4
	config.TeamCount = 2
	config.Maps[0].PlayerLimit = 4
	config.Maps[0].SpawnGroupA = []mapdata.CompetitiveCell{{Row: 0, Col: 0}, {Row: 0, Col: 4}}
	config.Maps[0].SpawnGroupB = []mapdata.CompetitiveCell{{Row: 2, Col: 0}, {Row: 2, Col: 4}}
	for _, test := range []struct {
		name                  string
		target                uint16
		wantOffense, wantSelf uint32
	}{
		{"self", 1, 0, 1},
		{"friendly", 2, 0, 0},
		{"enemy", 3, 1, 0},
		{"unknown", 999, 0, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			batch, err := NewBatch(config)
			if err != nil {
				t.Fatal(err)
			}
			batch.accumulateEvents(0, []battleengine.Event{
				{Kind: battleengine.EventActorTrapped, PlayerID: 1, TargetID: test.target},
				{Kind: battleengine.EventActorEliminated, PlayerID: 1, TargetID: test.target},
			}, make([]float32, 4))
			metrics := batch.episodes[0].metrics.Agents[0]
			if metrics.Traps != test.wantOffense || metrics.Eliminations != test.wantOffense || metrics.SelfEliminations != test.wantSelf {
				t.Fatalf("offense/self metrics = %d/%d/%d, want %d/%d/%d", metrics.Traps, metrics.Eliminations, metrics.SelfEliminations, test.wantOffense, test.wantOffense, test.wantSelf)
			}
		})
	}
}

func TestEventRewardsNormalizeUnevenFreeRoomTeams(t *testing.T) {
	config := testBatchConfig(1)
	config.ParticipantCount = 4
	config.TeamCount = 3
	config.TeamLayouts = [][]uint8{{1, 2, 2, 3}}
	config.Maps[0].PlayerLimit = 4
	config.Maps[0].SpawnGroupA = []mapdata.CompetitiveCell{{Row: 0, Col: 0}, {Row: 0, Col: 4}}
	config.Maps[0].SpawnGroupB = []mapdata.CompetitiveCell{{Row: 2, Col: 0}, {Row: 2, Col: 4}}
	batch, err := NewBatch(config)
	if err != nil {
		t.Fatal(err)
	}
	rewards := make([]float32, 4)
	batch.accumulateEvents(0, []battleengine.Event{
		{Kind: battleengine.EventActorTrapped, PlayerID: 1, TargetID: 2},
		{Kind: battleengine.EventActorTrapped, PlayerID: 1, TargetID: 3},
	}, rewards)
	if got, want := rewards[0], rewardTrap; got != want {
		t.Fatalf("two members of one free-room team paid %v, want one team-sized %v", got, want)
	}
	if rewards[1] != -rewardTrap/2 || rewards[2] != -rewardTrap/2 || rewards[3] != 0 {
		t.Fatalf("uneven free-room target scaling=%v", rewards)
	}

	rewards = make([]float32, 4)
	batch.accumulateEvents(0, []battleengine.Event{{Kind: battleengine.EventActorTrapped, PlayerID: 1, TargetID: 4}}, rewards)
	if rewards[0] != rewardTrap || rewards[3] != -rewardTrap {
		t.Fatalf("singleton free-room team should retain 1v1 impact: %v", rewards)
	}
}

func TestEventRewardsDoNotPayFriendlyTrapRescueLoops(t *testing.T) {
	config := testBatchConfig(1)
	config.ParticipantCount = 4
	config.TeamCount = 2
	config.Maps[0].PlayerLimit = 4
	config.Maps[0].SpawnGroupA = []mapdata.CompetitiveCell{{Row: 0, Col: 0}, {Row: 0, Col: 4}}
	config.Maps[0].SpawnGroupB = []mapdata.CompetitiveCell{{Row: 2, Col: 0}, {Row: 2, Col: 4}}
	batch, err := NewBatch(config)
	if err != nil {
		t.Fatal(err)
	}
	rewards := make([]float32, 4)
	batch.accumulateEvents(0, []battleengine.Event{
		{Kind: battleengine.EventActorTrapped, PlayerID: 1, TargetID: 2},
		{Kind: battleengine.EventActorRescued, PlayerID: 1, TargetID: 2},
	}, rewards)
	for _, value := range rewards {
		if value != 0 {
			t.Fatalf("friendly trap/rescue loop retained reward: %v", rewards)
		}
	}
}

func TestEventRewardsRollbackEnemyTrapAndBoundRescueCredit(t *testing.T) {
	config := testBatchConfig(1)
	config.ParticipantCount = 4
	config.TeamCount = 2
	config.Maps[0].PlayerLimit = 4
	config.Maps[0].SpawnGroupA = []mapdata.CompetitiveCell{{Row: 0, Col: 0}, {Row: 0, Col: 4}}
	config.Maps[0].SpawnGroupB = []mapdata.CompetitiveCell{{Row: 2, Col: 0}, {Row: 2, Col: 4}}
	batch, err := NewBatch(config)
	if err != nil {
		t.Fatal(err)
	}
	// Player 2's adjacent bubble closes one immediate route while player 1
	// supplies the trapping flame. The assist pool is credited once and then
	// rolled back with the direct trap when player 4 rescues player 3.
	targetCell := battleengine.Cell{Row: 1, Col: 2}
	if err := batch.episodes[0].engine.ApplyVerifiedMovementCheckpoint(3, battleengine.PositionAtCellCenter(targetCell)); err != nil {
		t.Fatal(err)
	}
	if _, err := batch.episodes[0].engine.ApplyVerifiedBombPlacement(2, battleengine.Cell{Row: 1, Col: 1}, 2, 0); err != nil {
		t.Fatal(err)
	}
	rewards := make([]float32, 4)
	batch.accumulateEvents(0, []battleengine.Event{{Kind: battleengine.EventActorTrapped, PlayerID: 1, TargetID: 3}}, rewards)
	teamScale := float32(0.5)
	if rewards[0] != rewardTrap*teamScale || rewards[1] != rewardTrapAssistTotal*teamScale || rewards[2] != -rewardTrap*teamScale || rewards[3] != 0 {
		t.Fatalf("direct/assist trap rewards=%v", rewards)
	}
	batch.accumulateEvents(0, []battleengine.Event{{Kind: battleengine.EventActorRescued, PlayerID: 4, TargetID: 3}}, rewards)
	if rewards[0] != 0 || rewards[1] != 0 || rewards[2] != 0 || rewards[3] != rewardRescue*teamScale {
		t.Fatalf("rescued enemy trap did not roll back causal credit: %v", rewards)
	}
	// A later rescue of the same target still rolls combat credit back and
	// receives only half of the previous rescue credit.
	batch.accumulateEvents(0, []battleengine.Event{
		{Kind: battleengine.EventActorTrapped, PlayerID: 1, TargetID: 3},
		{Kind: battleengine.EventActorRescued, PlayerID: 4, TargetID: 3},
	}, rewards)
	if rewards[0] != 0 || rewards[1] != 0 || rewards[2] != 0 || rewards[3] != (rewardRescue+rewardRescue/2)*teamScale {
		t.Fatalf("repeat rescue did not decay geometrically: %v", rewards)
	}
}

func TestEventRewardsCreditUsefulActiveItems(t *testing.T) {
	batch, err := NewBatch(testBatchConfig(1))
	if err != nil {
		t.Fatal(err)
	}
	rewards := make([]float32, 2)
	batch.accumulateEvents(0, []battleengine.Event{
		{Kind: battleengine.EventFieldObjectTriggered, PlayerID: 1, TargetID: 2, ActionID: 43},
		{Kind: battleengine.EventActorRescued, PlayerID: 1, TargetID: 1, ActionID: 63},
	}, rewards)
	if got, want := rewards[0], rewardMovementDebuff+rewardRescue; got != want {
		t.Fatalf("active-item owner reward = %v, want %v", got, want)
	}
	if got, want := rewards[1], -rewardMovementDebuff; got != want {
		t.Fatalf("debuff target reward = %v, want %v", got, want)
	}
}

func TestEventRewardsTreatHitTransformationAsExtraLife(t *testing.T) {
	batch, err := NewBatch(testBatchConfig(1))
	if err != nil {
		t.Fatal(err)
	}
	rewards := make([]float32, 2)
	batch.accumulateEvents(0, []battleengine.Event{{
		Kind: battleengine.EventActorTransformationEnded, PlayerID: 2, TargetID: 1,
		TransformationEnd: battleengine.TransformationEndHit,
	}}, rewards)
	if rewards[0] != rewardTransformationBroken || rewards[1] != -rewardTransformationBroken {
		t.Fatalf("enemy transformation break rewards = %v", rewards)
	}
	rewards = make([]float32, 2)
	batch.accumulateEvents(0, []battleengine.Event{{
		Kind: battleengine.EventActorTransformationEnded, PlayerID: 2,
		TransformationEnd: battleengine.TransformationEndExpired,
	}}, rewards)
	if rewards[0] != 0 || rewards[1] != 0 {
		t.Fatalf("natural transformation expiry changed rewards: %v", rewards)
	}
}

func TestTerminalRewardPrefersFastWinsAndPenalizesFastLosses(t *testing.T) {
	duel := materialByTeam([]battleengine.Actor{
		{Participant: battleengine.Participant{TeamID: 1}},
		{Participant: battleengine.Participant{TeamID: 2}},
	})
	fast := battleengine.Outcome{Ended: true, WinnerTeamID: 1, EndedAtMS: 60_000}
	late := battleengine.Outcome{Ended: true, WinnerTeamID: 1, EndedAtMS: 220_000}
	if terminalOutcomeReward(1, fast, duel) <= terminalOutcomeReward(1, late, duel) {
		t.Fatalf("fast win reward=%v, late=%v", terminalOutcomeReward(1, fast, duel), terminalOutcomeReward(1, late, duel))
	}
	if terminalOutcomeReward(2, fast, duel) >= terminalOutcomeReward(2, late, duel) {
		t.Fatalf("fast loss reward=%v, late=%v", terminalOutcomeReward(2, fast, duel), terminalOutcomeReward(2, late, duel))
	}
	draw := battleengine.Outcome{Ended: true, Draw: true, TimedOut: true, EndedAtMS: battleengine.StandardRoundTimeMS}
	if got := terminalOutcomeReward(1, draw, duel); got != rewardDraw {
		t.Fatalf("draw reward=%v, want %v", got, rewardDraw)
	}
	earlyDraw := battleengine.Outcome{Ended: true, Draw: true, EndedAtMS: 60_000}
	if got := terminalOutcomeReward(1, earlyDraw, duel); got != rewardDraw {
		t.Fatalf("non-timeout draw reward=%v, want %v", got, rewardDraw)
	}
	lateLoss := battleengine.Outcome{Ended: true, WinnerTeamID: 2, EndedAtMS: battleengine.StandardRoundTimeMS}
	immediateLoss := battleengine.Outcome{Ended: true, WinnerTeamID: 2, EndedAtMS: 0}
	if terminalOutcomeReward(1, draw, duel) <= terminalOutcomeReward(1, lateLoss, duel) {
		t.Fatalf("timeout draw reward=%v must exceed late loss=%v", terminalOutcomeReward(1, draw, duel), terminalOutcomeReward(1, lateLoss, duel))
	}
	if terminalOutcomeReward(1, draw, duel) <= terminalOutcomeReward(1, immediateLoss, duel) {
		t.Fatalf("timeout draw reward=%v must exceed immediate loss=%v", terminalOutcomeReward(1, draw, duel), terminalOutcomeReward(1, immediateLoss, duel))
	}
}

func TestTimeoutDrawRewardUsesInitialAndLiveTeamMaterial(t *testing.T) {
	actors := make([]battleengine.Actor, 0, 8)
	actors = append(actors, battleengine.Actor{Participant: battleengine.Participant{TeamID: 1}})
	for range 7 {
		actors = append(actors, battleengine.Actor{Participant: battleengine.Participant{TeamID: 2}})
	}
	material := materialByTeam(actors)
	requireRewardNear(t, timeoutDrawReward(1, material), 0.96)
	requireRewardNear(t, timeoutDrawReward(2, material), -0.96)
	for index := 1; index < 7; index++ {
		actors[index].State = battleengine.ActorEliminated
	}
	material = materialByTeam(actors)
	requireRewardNear(t, timeoutDrawReward(1, material), 1.71)
	requireRewardNear(t, timeoutDrawReward(2, material), -1.71)
}

func TestTerminalOutcomeRewardNormalizesUnevenTeamDifficulty(t *testing.T) {
	actors := make([]battleengine.Actor, 0, 8)
	actors = append(actors, battleengine.Actor{Participant: battleengine.Participant{TeamID: 1}})
	for range 7 {
		actors = append(actors, battleengine.Actor{Participant: battleengine.Participant{TeamID: 2}})
	}
	material := materialByTeam(actors)
	hardWin := battleengine.Outcome{
		Ended: true, WinnerTeamID: 1, EndedAtMS: battleengine.StandardRoundTimeMS,
	}
	easyWin := battleengine.Outcome{
		Ended: true, WinnerTeamID: 2, EndedAtMS: battleengine.StandardRoundTimeMS,
	}
	requireRewardNear(t, terminalOutcomeReward(1, hardWin, material), 1.96)
	requireRewardNear(t, terminalOutcomeReward(2, hardWin, material), -1.96)
	requireRewardNear(t, terminalOutcomeReward(2, easyWin, material), 0.04)
	requireRewardNear(t, terminalOutcomeReward(1, easyWin, material), -0.04)
	nonTimeoutDraw := battleengine.Outcome{Ended: true, Draw: true}
	requireRewardNear(t, terminalOutcomeReward(1, nonTimeoutDraw, material), 0.96)
	requireRewardNear(t, terminalOutcomeReward(2, nonTimeoutDraw, material), -0.96)
}

func TestTerminalOutcomeRewardPenalizesDrawsOutsideOneVersusCoalition(t *testing.T) {
	draws := []battleengine.Outcome{
		{Ended: true, Draw: true},
		{Ended: true, Draw: true, TimedOut: true, EndedAtMS: battleengine.StandardRoundTimeMS},
	}
	layouts := map[string][]battleengine.Actor{
		"two-versus-two": {
			{Participant: battleengine.Participant{TeamID: 1}},
			{Participant: battleengine.Participant{TeamID: 1}},
			{Participant: battleengine.Participant{TeamID: 2}},
			{Participant: battleengine.Participant{TeamID: 2}},
		},
		"two-versus-three": {
			{Participant: battleengine.Participant{TeamID: 1}},
			{Participant: battleengine.Participant{TeamID: 1}},
			{Participant: battleengine.Participant{TeamID: 2}},
			{Participant: battleengine.Participant{TeamID: 2}},
			{Participant: battleengine.Participant{TeamID: 2}},
		},
		"free-for-all": {
			{Participant: battleengine.Participant{TeamID: 1}},
			{Participant: battleengine.Participant{TeamID: 2}},
			{Participant: battleengine.Participant{TeamID: 3}},
		},
	}
	for name, actors := range layouts {
		t.Run(name, func(t *testing.T) {
			material := materialByTeam(actors)
			if oneVersusCoalition(material) {
				t.Fatal("layout was incorrectly classified as one-versus-coalition")
			}
			for _, draw := range draws {
				for teamID := range material {
					requireRewardNear(t, terminalOutcomeReward(teamID, draw, material), rewardDraw)
				}
			}
		})
	}
}

func TestOneVersusCoalitionRequiresSeveralSameTeamOpponents(t *testing.T) {
	actors := []battleengine.Actor{
		{Participant: battleengine.Participant{TeamID: 1}},
		{Participant: battleengine.Participant{TeamID: 2}},
		{Participant: battleengine.Participant{TeamID: 2}},
		{Participant: battleengine.Participant{TeamID: 2}},
	}
	material := materialByTeam(actors)
	if !oneVersusCoalition(material) {
		t.Fatal("strict one-versus-coalition layout was not recognized")
	}
	draw := battleengine.Outcome{Ended: true, Draw: true, TimedOut: true}
	if terminalOutcomeReward(1, draw, material) <= 0 {
		t.Fatalf("solo draw reward=%v, want positive survival score", terminalOutcomeReward(1, draw, material))
	}
	if terminalOutcomeReward(2, draw, material) >= 0 {
		t.Fatalf("coalition draw reward=%v, want corresponding negative score", terminalOutcomeReward(2, draw, material))
	}
}

func TestOutcomePriorDistinguishesCoalitionFromEightSoloTeams(t *testing.T) {
	coalitionActors := []battleengine.Actor{{Participant: battleengine.Participant{TeamID: 1}}}
	for range 7 {
		coalitionActors = append(coalitionActors, battleengine.Actor{Participant: battleengine.Participant{TeamID: 2}})
	}
	soloActors := make([]battleengine.Actor, 0, 8)
	for teamID := byte(1); teamID <= 8; teamID++ {
		soloActors = append(soloActors, battleengine.Actor{Participant: battleengine.Participant{TeamID: teamID}})
	}
	coalitionWin := initialOutcomeReward(1, 1, materialByTeam(coalitionActors))
	soloWin := initialOutcomeReward(1, 1, materialByTeam(soloActors))
	requireRewardNear(t, coalitionWin, 1.96)
	requireRewardNear(t, soloWin, 1)
	if coalitionWin <= soloWin {
		t.Fatalf("coalition upset reward=%v must exceed eight-way win=%v", coalitionWin, soloWin)
	}
}

func TestTimeoutDrawRewardMeasuresEqualTeamMaterialSwing(t *testing.T) {
	actors := []battleengine.Actor{
		{Participant: battleengine.Participant{TeamID: 1}},
		{Participant: battleengine.Participant{TeamID: 1}, State: battleengine.ActorEliminated},
		{Participant: battleengine.Participant{TeamID: 2}},
		{Participant: battleengine.Participant{TeamID: 2}},
	}
	material := materialByTeam(actors)
	requireRewardNear(t, timeoutDrawReward(1, material), -1.0/3.0)
	requireRewardNear(t, timeoutDrawReward(2, material), 1.0/3.0)
}

func TestThreeTeamOutcomeNormalizationIsZeroSum(t *testing.T) {
	actors := []battleengine.Actor{
		{Participant: battleengine.Participant{TeamID: 1}},
		{Participant: battleengine.Participant{TeamID: 2}},
		{Participant: battleengine.Participant{TeamID: 3}},
	}
	material := materialByTeam(actors)
	outcome := battleengine.Outcome{
		Ended: true, WinnerTeamID: 1, EndedAtMS: battleengine.StandardRoundTimeMS,
	}
	rewards := []float32{
		terminalOutcomeReward(1, outcome, material),
		terminalOutcomeReward(2, outcome, material),
		terminalOutcomeReward(3, outcome, material),
	}
	requireRewardNear(t, rewards[0], 1)
	requireRewardNear(t, rewards[1], -0.5)
	requireRewardNear(t, rewards[2], -0.5)
	if math.Abs(float64(rewards[0]+rewards[1]+rewards[2])) > 1e-6 {
		t.Fatalf("three-team rewards are not zero sum: %v", rewards)
	}
}

func TestLateStallResponsibilityExemptsOutnumberedTeam(t *testing.T) {
	actors := []battleengine.Actor{
		{Participant: battleengine.Participant{TeamID: 1}},
		{Participant: battleengine.Participant{TeamID: 2}},
		{Participant: battleengine.Participant{TeamID: 2}},
	}
	if got := lateStallResponsibility(1, actors); got != 0 {
		t.Fatalf("outnumbered responsibility=%v, want 0", got)
	}
	if got := lateStallResponsibility(2, actors); got != 4.0/3.0 {
		t.Fatalf("advantaged responsibility=%v, want %v", got, float32(4.0/3.0))
	}
}

func TestEnemyApproachPotentialDoesNotRewardLoops(t *testing.T) {
	if got := enemyApproachReward(rewardDevelopmentEndMS-1, 5, 4); got != 0 {
		t.Fatalf("development-phase approach reward = %v, want 0", got)
	}
	if got := enemyApproachReward(rewardDevelopmentEndMS, 5, 4); got <= 0 {
		t.Fatalf("approach reward = %v", got)
	}
	forward := enemyApproachReward(rewardDevelopmentEndMS, 5, 4)
	backward := enemyApproachReward(rewardDevelopmentEndMS, 4, 5)
	if forward+backward != 0 {
		t.Fatalf("round-trip potential reward = %v", forward+backward)
	}
}

func TestDevelopmentProgressBonusEndsAtThirtySeconds(t *testing.T) {
	if got, want := developmentProgressReward(rewardDevelopmentEndMS-1, rewardWallDestroyed, rewardDevelopmentWallBonus), float32(0.03); got != want {
		t.Fatalf("opening wall reward = %v, want %v", got, want)
	}
	if got, want := developmentProgressReward(rewardDevelopmentEndMS-1, rewardPickup, rewardDevelopmentPickupBonus), float32(0.04); got != want {
		t.Fatalf("opening pickup reward = %v, want %v", got, want)
	}
	if got := developmentProgressReward(rewardDevelopmentEndMS, rewardPickup, rewardDevelopmentPickupBonus); got != rewardPickup {
		t.Fatalf("midgame pickup reward = %v, want %v", got, rewardPickup)
	}
}

func TestPickupProgressRewardUsesEffectiveGainAndFiniteInventory(t *testing.T) {
	largeUpgrade := battleengine.Event{
		Kind:        battleengine.EventPickupCollected,
		Attribute:   battleengine.AttributeBombPower,
		ValueBefore: 2,
		ValueAfter:  9,
	}
	requireRewardNear(t, pickupProgressReward(largeUpgrade, rewardDevelopmentEndMS-1), 7*0.04)
	for _, elapsed := range []uint32{rewardDevelopmentEndMS, 60_000, 180_000} {
		requireRewardNear(t, pickupProgressReward(largeUpgrade, elapsed), 7*0.04)
	}

	cappedUpgrade := largeUpgrade
	cappedUpgrade.ValueBefore = 9
	cappedUpgrade.ValueAfter = 9
	if got := pickupProgressReward(cappedUpgrade, rewardDevelopmentEndMS-1); got != 0 {
		t.Fatalf("capped attribute pickup reward = %v, want 0", got)
	}

	threeItems := battleengine.Event{
		Kind:        battleengine.EventPickupCollected,
		Effect:      battleengine.PickupEffectBattleAction,
		ActionID:    43,
		ActionCount: 3,
	}
	requireRewardNear(t, pickupProgressReward(threeItems, rewardDevelopmentEndMS), 3*rewardPickup)

	fork := battleengine.Event{
		Kind:        battleengine.EventPickupCollected,
		Effect:      battleengine.PickupEffectBattleAction,
		ActionID:    63,
		ActionCount: 1,
	}
	requireRewardNear(
		t,
		pickupProgressReward(fork, rewardDevelopmentEndMS),
		rewardPickup+rewardRescue,
	)
}

func TestLateStallPenaltyStartsOnlyAfterNinetySecondsAndProgressGrace(t *testing.T) {
	if got := lateStallPenalty(rewardLateStallStartMS-1, 0, 20); got != 0 {
		t.Fatalf("first-half stall penalty=%v, want 0", got)
	}
	lastProgress := rewardLateStallStartMS - 1_000
	if got := lateStallPenalty(rewardLateStallStartMS+rewardLateStallProgressGraceMS-1_000, lastProgress, 20); got != 0 {
		t.Fatalf("progress grace penalty=%v, want 0", got)
	}
	first := lateStallPenalty(rewardLateStallStartMS+rewardLateStallProgressGraceMS, 0, 20)
	if first >= 0 {
		t.Fatalf("late stall penalty=%v, want negative", first)
	}
	later := lateStallPenalty(rewardRoundDurationMS-20, 0, 20)
	if later >= first {
		t.Fatalf("late stall penalty did not escalate: first=%v later=%v", first, later)
	}
}

func TestWholeRoundLateStallCostStaysBelowOutcomeGap(t *testing.T) {
	for _, tick := range []uint32{20, 100, 137} {
		total := float32(0)
		for elapsed := rewardLateStallStartMS; elapsed < rewardRoundDurationMS; elapsed += tick {
			total += lateStallPenalty(elapsed, 0, tick)
		}
		if math.Abs(float64(total+rewardLateStallBudget)) > 0.00001 {
			t.Fatalf("tick %d: whole-round cost=%v, want %v", tick, total, -rewardLateStallBudget)
		}
		// The largest responsibility multiplier is 2. Shaping alone must
		// not make even a whole-round draw worse than a late 1v1 loss.
		if rewardDraw+2*total <= -1 {
			t.Fatalf("tick %d: shaping overwhelms draw-to-loss ordering", tick)
		}
	}
	if lateStallPenalty(rewardRoundDurationMS, 0, 20) != 0 {
		t.Fatal("stall penalty continued after the native round ended")
	}
}

func TestNativeBubblePassesAreMeasuredButNotPenalized(t *testing.T) {
	batch, err := NewBatch(testBatchConfig(1))
	if err != nil {
		t.Fatal(err)
	}
	rewards := make([]float32, 2)
	events := make([]battleengine.Event, 4)
	for index := range events {
		events[index] = battleengine.Event{Kind: battleengine.EventNativePassStarted, PlayerID: 1, TimeMS: uint32(index * 1_000)}
	}
	batch.accumulateEvents(0, events, rewards)
	if rewards[0] != 0 {
		t.Fatalf("native bubble passes reward=%v, want 0", rewards[0])
	}
	if got := batch.episodes[0].metrics.Agents[0].NativePasses; got != 4 {
		t.Fatalf("native pass metric=%d, want 4", got)
	}
}

func TestSelectedSearchDoesNotPrimeUncontrolledActors(t *testing.T) {
	batch, err := NewBatch(testBatchConfig(2))
	if err != nil {
		t.Fatal(err)
	}
	before, err := batch.Observe()
	if err != nil {
		t.Fatal(err)
	}
	count := batch.config.ParticipantCount
	logits := make([]float32, 2*count*int(battleengine.DiscreteActionCount))
	selected := make([]bool, 2*count)
	selected[0] = true
	config := battleengine.SearchConfig{TopK: 4, HorizonMS: 800, TacticalSafety: true}
	result, err := batch.SearchTeacherScoresSelected(logits, config, selected)
	if err != nil {
		t.Fatal(err)
	}
	for envIndex := range batch.episodes {
		for actorIndex, controller := range batch.episodes[envIndex].searchSafety {
			if (controller.Base != nil) != selected[envIndex*count+actorIndex] {
				t.Fatalf("unexpected continuation state update: env=%d actor=%d", envIndex, actorIndex)
			}
			if !selected[envIndex*count+actorIndex] && result.Actions[envIndex][actorIndex] != 0 {
				t.Fatal("unselected action is not zero")
			}
		}
	}
	controller := *batch.episodes[0].searchSafety[0]
	selected[0] = false
	if _, err := batch.SearchTeacherScoresSelected(logits, config, selected); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(controller, *batch.episodes[0].searchSafety[0]) {
		t.Fatal("skipped Search modified an existing continuation")
	}
	after, err := batch.Observe()
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("Search changed native observation: %v", err)
	}
}

func TestSearchLabelsDoNotAdvanceTeacherContinuation(t *testing.T) {
	batch, err := NewBatch(testBatchConfig(1))
	if err != nil {
		t.Fatal(err)
	}
	config := battleengine.SearchConfig{TopK: 10, HorizonMS: 3400, TacticalSafety: true}
	logits := make([]float32, batch.config.ParticipantCount*int(battleengine.DiscreteActionCount))
	selected := make([]bool, batch.config.ParticipantCount)
	selected[0] = true
	before, _ := batch.Observe()
	controller := *batch.episodes[0].searchSafety[0]
	labels, err := batch.SearchTeacherLabelScoresSelected(logits, config, selected)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(controller, *batch.episodes[0].searchSafety[0]) {
		t.Fatal("unexecuted labels changed teacher state")
	}
	executed, err := batch.SearchTeacherScoresSelected(logits, config, selected)
	if err != nil || !reflect.DeepEqual(labels, executed) {
		t.Fatalf("labels differ from the same first executable query: %v", err)
	}
	controller = *batch.episodes[0].searchSafety[0]
	logits[0] = 10
	if _, err := batch.SearchTeacherLabelScoresSelected(logits, config, selected); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(controller, *batch.episodes[0].searchSafety[0]) {
		t.Fatal("labels replaced existing teacher continuation")
	}
	after, err := batch.Observe()
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("label queries changed the native world: %v", err)
	}
}

func TestSearchTeacherSafetyStateIsActorLocalAndResetWithEpisode(t *testing.T) {
	batch, err := NewBatch(testBatchConfig(1))
	if err != nil {
		t.Fatal(err)
	}
	episode := &batch.episodes[0]
	if got, want := len(episode.searchSafety), len(episode.playerIDs); got != want {
		t.Fatalf("search safety states=%d, want %d", got, want)
	}
	if episode.searchSafety[0] == episode.searchSafety[1] {
		t.Fatal("search safety state is shared between actors")
	}
	beforeReset := episode.searchSafety[0]
	logits := make([]float32, batch.config.EnvCount*batch.config.ParticipantCount*int(battleengine.DiscreteActionCount))
	if _, err := batch.SearchTeacherScores(logits, battleengine.SearchConfig{TopK: 4, HorizonMS: 800, TacticalSafety: true}); err != nil {
		t.Fatal(err)
	}
	if beforeReset.Base == nil {
		t.Fatal("search teacher did not route its chosen action through tactical safety")
	}
	if err := batch.Reset([]int{0}); err != nil {
		t.Fatal(err)
	}
	afterReset := batch.episodes[0].searchSafety[0]
	if afterReset == beforeReset {
		t.Fatal("episode reset retained tactical safety latch state")
	}
	if afterReset.Base != nil {
		t.Fatal("fresh tactical safety state retained a previous search action")
	}
}

func TestSearchTeacherCanMatchProductionSearchTriggerWithoutSafetyController(t *testing.T) {
	batch, err := NewBatch(testBatchConfig(1))
	if err != nil {
		t.Fatal(err)
	}
	actionCount := int(battleengine.DiscreteActionCount)
	logits := make([]float32, batch.config.EnvCount*batch.config.ParticipantCount*actionCount)
	for index := range logits {
		logits[index] = -100
	}
	episode := &batch.episodes[0]
	for actorIndex, playerID := range episode.playerIDs {
		legal, legalErr := episode.engine.LegalActions(playerID)
		if legalErr != nil {
			t.Fatal(legalErr)
		}
		selected := battleengine.DiscreteActionCount
		for _, action := range legal {
			if action.Move == battleengine.DirectionNone || action.PlaceBomb || action.UseActionID != 0 {
				continue
			}
			actionID, ok := action.ID()
			if ok {
				selected = actionID
				break
			}
		}
		if selected == battleengine.DiscreteActionCount {
			t.Fatalf("player %d has no legal pure movement action", playerID)
		}
		base := actorIndex * actionCount
		logits[base+int(selected)] = 100
	}
	result, err := batch.SearchTeacherScores(logits, battleengine.SearchConfig{
		TopK: 4, HorizonMS: 800, AlwaysSearch: false, TacticalSafety: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	for actorIndex := range episode.playerIDs {
		base := actorIndex * actionCount
		evaluated := 0
		for _, status := range result.Statuses[base : base+actionCount] {
			if status != 0 {
				evaluated++
			}
		}
		if evaluated != 1 {
			t.Fatalf("actor %d evaluated actions=%d, want greedy production shortcut", actorIndex, evaluated)
		}
		if episode.searchSafety[actorIndex].Base != nil {
			t.Fatalf("actor %d unexpectedly activated offline safety controller", actorIndex)
		}
	}
}

func TestBlockedCellCampingHasGraceThenCostsTime(t *testing.T) {
	config := testBatchConfig(1)
	config.Maps[0].Battlefield.Cells[1*5+1] = mapdata.CompetitiveBattleCell{Collision: mapdata.CompetitiveCellSolid}
	batch, err := NewBatch(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := batch.episodes[0].engine.ApplyVerifiedMovementCheckpoint(1, battleengine.PositionAtCellCenter(battleengine.Cell{Row: 1, Col: 1})); err != nil {
		t.Fatal(err)
	}
	rewards := make([]float32, 2)
	graceTicks := (rewardBlockedCellGraceMS + config.TickMS - 1) / config.TickMS
	for range graceTicks {
		batch.accumulateBlockedCellBehavior(0, rewards)
	}
	if rewards[0] != 0 {
		t.Fatalf("blocked-cell grace reward=%v, want 0", rewards[0])
	}
	batch.accumulateBlockedCellBehavior(0, rewards)
	want := -rewardBlockedCellPenaltyPerSecond * float32(config.TickMS) / 1_000
	if rewards[0] != want {
		t.Fatalf("blocked-cell post-grace reward=%v, want %v", rewards[0], want)
	}
	if got := batch.episodes[0].metrics.Agents[0].BlockedTicks; got != graceTicks+1 {
		t.Fatalf("blocked-cell ticks=%d, want %d", got, graceTicks+1)
	}
	if err := batch.episodes[0].engine.ApplyVerifiedMovementCheckpoint(1, battleengine.PositionAtCellCenter(battleengine.Cell{Row: 1, Col: 0})); err != nil {
		t.Fatal(err)
	}
	batch.accumulateBlockedCellBehavior(0, rewards)
	if err := batch.episodes[0].engine.ApplyVerifiedMovementCheckpoint(1, battleengine.PositionAtCellCenter(battleengine.Cell{Row: 1, Col: 1})); err != nil {
		t.Fatal(err)
	}
	beforeReentry := rewards[0]
	batch.accumulateBlockedCellBehavior(0, rewards)
	if got := rewards[0] - beforeReentry; math.Abs(float64(got+rewardRapidBlockedReentryPenalty)) > 1e-6 {
		t.Fatalf("rapid wall re-entry reward delta=%v, want %v", got, -rewardRapidBlockedReentryPenalty)
	}
}

func BenchmarkBatchObserveEightParticipants(b *testing.B) {
	config := testBatchConfig(8)
	config.ReuseTensorBuffers = true
	config.ParticipantCount = 8
	config.ParticipantCounts = []int{8}
	config.TeamCount = 4
	config.TeamLayouts = [][]uint8{{1, 1, 2, 2, 3, 3, 4, 4}}
	config.Maps[0] = benchmarkTrainingMap()
	batch, err := NewBatch(config)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := batch.Observe(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkBatchObserveTwoParticipants(b *testing.B) {
	config := testBatchConfig(8)
	config.ReuseTensorBuffers = true
	config.Maps[0] = benchmarkTrainingMap()
	batch, err := NewBatch(config)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := batch.Observe(); err != nil {
			b.Fatal(err)
		}
	}
}

func TestReusableTensorObservationClearsPreviousFrame(t *testing.T) {
	config := testBatchConfig(1)
	config.ReuseTensorBuffers = true
	batch, err := NewBatch(config)
	if err != nil {
		t.Fatal(err)
	}
	first, err := batch.Observe()
	if err != nil {
		t.Fatal(err)
	}
	for index := range first.Spatial {
		first.Spatial[index] = 1
	}
	second, err := batch.Observe()
	if err != nil {
		t.Fatal(err)
	}
	for actorIndex := 0; actorIndex < second.ParticipantCount; actorIndex++ {
		base := actorIndex * SpatialChannels * second.Height * second.Width
		for cellIndex := 0; cellIndex < second.Height*second.Width; cellIndex++ {
			if second.Spatial[base+cellIndex] != 1 {
				t.Fatalf("frame base channel actor=%d cell=%d was not re-encoded", actorIndex, cellIndex)
			}
		}
	}
	// Channel 63 has no push-progress cells in this fixture. Any retained one
	// proves the reusable storage was not cleared before encoding.
	channelSize := second.Height * second.Width
	for actorIndex := 0; actorIndex < second.ParticipantCount; actorIndex++ {
		base := (actorIndex*SpatialChannels + 63) * channelSize
		for cellIndex := 0; cellIndex < channelSize; cellIndex++ {
			if second.Spatial[base+cellIndex] != 0 {
				t.Fatalf("stale spatial value actor=%d cell=%d", actorIndex, cellIndex)
			}
		}
	}
}

func testBatchConfig(envs int) Config {
	return Config{
		Maps: []mapdata.CompetitiveMap{testTrainingMap()}, EnvCount: envs, ParticipantCount: 2,
		TickMS: 20, DecisionMS: 100, TrapDurationMS: 6_000, DangerHorizonMS: 3_500, BaseSeed: 123,
	}
}

func testTrainingMap() mapdata.CompetitiveMap {
	field := mapdata.CompetitiveBattlefield{Width: 5, Height: 3, Cells: make([]mapdata.CompetitiveBattleCell, 15)}
	for index := range field.Cells {
		field.Cells[index] = mapdata.CompetitiveBattleCell{Collision: mapdata.CompetitiveCellOpen, FlamePassable: true}
	}
	return mapdata.CompetitiveMap{
		ID: 1, Name: "training", Selectable: true, PlayerLimit: 2,
		NativeRule: 1, Rule: mapdata.CompetitiveRuleOrdinary, RequiredItemField: 0,
		SpawnGroupA: []mapdata.CompetitiveCell{{Row: 1, Col: 0}},
		SpawnGroupB: []mapdata.CompetitiveCell{{Row: 1, Col: 4}},
		Battlefield: field,
	}
}

func benchmarkTrainingMap() mapdata.CompetitiveMap {
	const width, height = 15, 13
	field := mapdata.CompetitiveBattlefield{Width: width, Height: height, Cells: make([]mapdata.CompetitiveBattleCell, width*height)}
	for index := range field.Cells {
		field.Cells[index] = mapdata.CompetitiveBattleCell{Collision: mapdata.CompetitiveCellOpen, FlamePassable: true}
	}
	spawnsA := []mapdata.CompetitiveCell{{Row: 1, Col: 1}, {Row: 1, Col: 13}, {Row: 11, Col: 1}, {Row: 11, Col: 13}}
	spawnsB := []mapdata.CompetitiveCell{{Row: 1, Col: 7}, {Row: 6, Col: 1}, {Row: 6, Col: 13}, {Row: 11, Col: 7}}
	return mapdata.CompetitiveMap{
		ID: 2, Name: "benchmark", Selectable: true, PlayerLimit: 8,
		NativeRule: 1, Rule: mapdata.CompetitiveRuleOrdinary, RequiredItemField: 0,
		SpawnGroupA: spawnsA, SpawnGroupB: spawnsB, Battlefield: field,
	}
}

func TestRecordSelfEliminationsCountsOnlyOwnerTargetMatch(t *testing.T) {
	result := make([]uint8, 3)
	recordSelfEliminations(
		[]uint16{100, 200},
		[]battleengine.Event{
			{Kind: battleengine.EventActorEliminated, PlayerID: 100, TargetID: 100},
			{Kind: battleengine.EventActorEliminated, PlayerID: 100, TargetID: 200},
			{Kind: battleengine.EventActorTrapped, PlayerID: 200, TargetID: 200},
		},
		result,
	)
	if result[0] != 1 || result[1] != 0 || result[2] != 0 {
		t.Fatalf("self-elimination counts = %v, want [1 0 0]", result)
	}
}

func TestRecordSelfFatalBombPlacementLookbackUsesExactBombIdentity(t *testing.T) {
	episode := &episode{
		playerIDs: []uint16{100, 200},
		metrics:   EpisodeMetrics{Decisions: 9},
		bombPlacementDecisions: map[uint32]bombPlacementDecision{
			41: {decision: 4, ownerID: 100},
			42: {decision: 8, ownerID: 200},
		},
	}
	result := make([]uint16, 2)
	recordSelfFatalBombPlacementLookbacks(
		episode,
		[]battleengine.Event{
			{Kind: battleengine.EventActorEliminated, PlayerID: 100, TargetID: 100, BombID: 41},
			{Kind: battleengine.EventActorEliminated, PlayerID: 200, TargetID: 100, BombID: 42},
		},
		result,
	)
	if result[0] != 6 || result[1] != 0 {
		t.Fatalf("self-fatal bomb lookbacks = %v, want [6 0]", result)
	}
}

func TestRecordEnemyFatalBombPlacementLookbackUsesExactBombIdentity(t *testing.T) {
	episode := &episode{
		playerIDs: []uint16{100, 200, 300},
		teamIDs:   []uint8{1, 2, 1},
		metrics:   EpisodeMetrics{Decisions: 12},
		bombPlacementDecisions: map[uint32]bombPlacementDecision{
			51: {decision: 3, ownerID: 100},
			52: {decision: 10, ownerID: 100},
			53: {decision: 11, ownerID: 300},
		},
	}
	result := make([]uint16, 3)
	recordEnemyFatalBombPlacementLookbacks(
		episode,
		[]battleengine.Event{
			{Kind: battleengine.EventActorEliminated, PlayerID: 100, TargetID: 200, BombID: 51},
			{Kind: battleengine.EventActorEliminated, PlayerID: 100, TargetID: 200, BombID: 52},
			{Kind: battleengine.EventActorEliminated, PlayerID: 300, TargetID: 100, BombID: 53},
			{Kind: battleengine.EventActorEliminated, PlayerID: 200, TargetID: 200, BombID: 52},
		},
		result,
	)
	if result[0] != 3 || result[1] != 0 || result[2] != 0 {
		t.Fatalf("enemy-fatal bomb lookbacks = %v, want [3 0 0]", result)
	}
}

func TestRecordBombPlacementDecisionKeepsStableBombID(t *testing.T) {
	episode := &episode{metrics: EpisodeMetrics{Decisions: 12}}
	recordBombPlacementDecisions(
		episode,
		[]battleengine.Event{
			{Kind: battleengine.EventBombPlaced, BombID: 71},
			{Kind: battleengine.EventBombExploded, BombID: 72},
		},
	)
	if len(episode.bombPlacementDecisions) != 1 || episode.bombPlacementDecisions[71] != (bombPlacementDecision{decision: 12}) {
		t.Fatalf("bomb placement decisions = %v", episode.bombPlacementDecisions)
	}
}

func TestRecordEnemyFatalBombCausalPlacementLookbacksFollowsExactChain(t *testing.T) {
	episode := &episode{
		playerIDs: []uint16{100, 200, 300},
		teamIDs:   []uint8{1, 2, 1},
		metrics:   EpisodeMetrics{Decisions: 20},
		bombPlacementDecisions: map[uint32]bombPlacementDecision{
			71: {decision: 8, ownerID: 100},
			72: {decision: 12, ownerID: 300},
			73: {decision: 17, ownerID: 100},
			74: {decision: 18, ownerID: 100}, // unrelated placement
		},
		bombTriggerParents: map[uint32]uint32{72: 71, 73: 72},
	}
	result := make([][]uint16, 3)
	recordEnemyFatalBombCausalPlacementLookbacks(
		episode,
		[]battleengine.Event{{
			Kind: battleengine.EventActorEliminated, PlayerID: 100,
			TargetID: 200, BombID: 73,
		}},
		result,
	)
	if !reflect.DeepEqual(result[0], []uint16{4, 13}) || !reflect.DeepEqual(result[2], []uint16{9}) || len(result[1]) != 0 {
		t.Fatalf("causal fatal placement lookbacks = %v, want [[4 13] [] [9]]", result)
	}
}

func TestRecordEnemyTrapsCountsOnlyAttributedOpponentHits(t *testing.T) {
	actors := []battleengine.Actor{
		{Participant: battleengine.Participant{PlayerID: 100, TeamID: 1}},
		{Participant: battleengine.Participant{PlayerID: 200, TeamID: 2}},
		{Participant: battleengine.Participant{PlayerID: 300, TeamID: 1}},
	}
	result := make([]uint8, len(actors))
	recordEnemyTraps(
		actors,
		[]battleengine.Event{
			{Kind: battleengine.EventActorTrapped, PlayerID: 100, TargetID: 200},
			{Kind: battleengine.EventActorTrapped, PlayerID: 100, TargetID: 300},
			{Kind: battleengine.EventActorTrapped, PlayerID: 200, TargetID: 200},
			{Kind: battleengine.EventActorEliminated, PlayerID: 200, TargetID: 100},
			{Kind: battleengine.EventActorTrapped, PlayerID: 999, TargetID: 100},
		},
		result,
	)
	if result[0] != 1 || result[1] != 0 || result[2] != 0 {
		t.Fatalf("enemy trap counts = %v, want [1 0 0]", result)
	}
}

func TestRecordDevelopmentProgressCountsOnlyAttributedWorldChanges(t *testing.T) {
	actors := []battleengine.Actor{
		{Participant: battleengine.Participant{PlayerID: 100, TeamID: 1}},
		{Participant: battleengine.Participant{PlayerID: 200, TeamID: 2}},
	}
	result := make([]uint8, len(actors))
	recordDevelopmentProgress(
		actors,
		[]battleengine.Event{
			{Kind: battleengine.EventCellDestroyed, PlayerID: 100},
			{Kind: battleengine.EventPickupCollected, PlayerID: 100},
			{Kind: battleengine.EventPickupCollected, PlayerID: 200},
			{Kind: battleengine.EventBombPlaced, PlayerID: 100},
			{Kind: battleengine.EventCellDestroyed, PlayerID: 999},
		},
		result,
	)
	if result[0] != 2 || result[1] != 1 {
		t.Fatalf("development progress counts = %v, want [2 1]", result)
	}
}
