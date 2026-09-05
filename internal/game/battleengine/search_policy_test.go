package battleengine

import "testing"

func TestDevelopmentSearchHorizonMustReachNativeWallDestruction(t *testing.T) {
	config := testConfig()
	config.Grid = testOpenGrid(5, 5)
	wall := Cell{Row: 1, Col: 2}
	config.Grid.Cells[int(wall.Row)*5+int(wall.Col)] = Tile{Kind: CellBreakable, Durability: 1}
	config.Rules.TickMS = 20
	config.Rules.BombFuseMS = NativeBombFuseMS
	config.Rules.FlameDurationMS = NativeFlameDurationMS
	config.Rules.RoundDurationMS = 10_000
	config.Rules.ActorHalfSizePixels = NativeActorHalfSizePixels
	config.Participants = []Participant{
		{PlayerID: 1, TeamID: 1, Source: ParticipantVirtualAI, Spawn: Cell{Row: 1, Col: 1}, SpeedPixelsPerSecond: 160, BombCapacity: 1, BombPower: 1},
		{PlayerID: 2, TeamID: 2, Source: ParticipantHuman, Spawn: Cell{Row: 4, Col: 4}, SpeedPixelsPerSecond: 160, BombCapacity: 1, BombPower: 1},
	}
	engine := mustEngine(t, config)
	observation, err := engine.Observation(1)
	if err != nil {
		t.Fatal(err)
	}
	action := Action{PlayerID: 1, Move: DirectionDown, PlaceBomb: true}
	values := make([]float64, 2)
	for index, horizon := range []uint32{800, NativeBombFuseMS + NativeFlameDurationMS + 100} {
		value, survives, err := searchActionValue(engine, observation, action, SearchConfig{
			HorizonMS: horizon, DangerHorizonMS: NativeBombFuseMS + NativeFlameDurationMS,
		}.normalized(config.Rules.TickMS))
		if err != nil || !survives {
			t.Fatalf("horizon %d: survives=%v err=%v", horizon, survives, err)
		}
		values[index] = value
	}
	if values[0] != 0 || values[1] < 0.75 {
		t.Fatalf("pre-fuse search must not see future wall credit: short=%v full=%v", values[0], values[1])
	}
	unchanged, _ := engine.grid.Cell(wall)
	if unchanged.Kind != CellBreakable || len(engine.bombs) != 0 || engine.elapsedMS != 0 {
		t.Fatal("offline horizon comparison mutated the source world")
	}
}

type fixedCandidates []ScoredAction

func (candidates fixedCandidates) CandidateActions(_ Observation, _ []Action, limit int) ([]ScoredAction, error) {
	if limit > len(candidates) {
		limit = len(candidates)
	}
	return append([]ScoredAction(nil), candidates[:limit]...), nil
}

func TestTopKSearchRejectsImmediateSelfTrap(t *testing.T) {
	engine := mustEngine(t, testConfig())
	playerID := uint16(1)
	observation, err := engine.Observation(playerID)
	if err != nil {
		t.Fatal(err)
	}
	legal, err := engine.LegalActions(playerID)
	if err != nil {
		t.Fatal(err)
	}
	place, ok := ActionFromID(playerID, ActionPlaceBomb)
	if !ok {
		t.Fatal("decode place action")
	}
	move, ok := ActionFromID(playerID, ActionMoveRight)
	if !ok {
		t.Fatal("decode move action")
	}
	policy := TopKSearchPolicy{
		Candidate: fixedCandidates{{Action: place, Score: 2}, {Action: move, Score: 1}},
		Config:    SearchConfig{TopK: 2, HorizonMS: NativeBombFuseMS + 200, PriorWeight: 0.1},
	}
	chosen, err := policy.ChooseActionWithSnapshot(engine.Clone(), observation, legal)
	if err != nil {
		t.Fatal(err)
	}
	if chosen != move {
		t.Fatalf("search chose %+v, want safe move %+v", chosen, move)
	}
}

func TestTopKSearchResolvesHazardsInsideNativeAuthoritySnapshot(t *testing.T) {
	config := testConfig()
	config.Rules.NativeOutcomeAuthority = true
	engine := mustEngine(t, config)
	playerID := uint16(1)
	observation, err := engine.Observation(playerID)
	if err != nil {
		t.Fatal(err)
	}
	legal, err := engine.LegalActions(playerID)
	if err != nil {
		t.Fatal(err)
	}
	place, _ := ActionFromID(playerID, ActionPlaceBomb)
	move, _ := ActionFromID(playerID, ActionMoveRight)
	policy := TopKSearchPolicy{
		Candidate: fixedCandidates{{Action: place, Score: 2}, {Action: move, Score: 1}},
		Config:    SearchConfig{TopK: 2, HorizonMS: NativeBombFuseMS + 200, PriorWeight: 0.1},
	}
	chosen, err := policy.ChooseActionWithSnapshot(engine.Clone(), observation, legal)
	if err != nil {
		t.Fatal(err)
	}
	if chosen != move {
		t.Fatalf("native-authority search chose %+v, want safe move %+v", chosen, move)
	}
}

func TestTopKSearchAcceptsTwoTurnEscapeFromThreeCellSpawnPocket(t *testing.T) {
	config := testConfig()
	config.Grid = Grid{Width: 4, Height: 4, Cells: make([]Tile, 16)}
	for index := range config.Grid.Cells {
		config.Grid.Cells[index] = Tile{Kind: CellSolid}
	}
	open := func(cell Cell) {
		config.Grid.Cells[int(cell.Row)*int(config.Grid.Width)+int(cell.Col)] = Tile{Kind: CellOpen, FlamePassable: true}
	}
	upper := Cell{Row: 1, Col: 1}
	corner := Cell{Row: 2, Col: 1}
	right := Cell{Row: 2, Col: 2}
	opponent := Cell{Row: 0, Col: 3}
	open(upper)
	open(corner)
	open(right)
	open(opponent)
	// Native movement is integrated at the 20 ms physics cadence used by the
	// vectorized environment and live AI; the learned decision remains held for
	// five such frames by the runtime adapter.
	config.Rules.TickMS = 20
	config.Rules.BombFuseMS = NativeBombFuseMS
	config.Rules.FlameDurationMS = NativeFlameDurationMS
	config.Rules.RoundDurationMS = 10_000
	config.Rules.ActorHalfSizePixels = NativeActorHalfSizePixels
	config.Participants = []Participant{
		{PlayerID: 1, TeamID: 1, Source: ParticipantVirtualAI, Spawn: upper, SpeedPixelsPerSecond: 160, BombCapacity: 1, BombPower: 1},
		{PlayerID: 2, TeamID: 2, Source: ParticipantHuman, Spawn: opponent, SpeedPixelsPerSecond: 160, BombCapacity: 1, BombPower: 1},
	}
	engine := mustEngine(t, config)
	observation, err := engine.Observation(1)
	if err != nil {
		t.Fatal(err)
	}
	action := Action{PlayerID: 1, Move: DirectionDown, PlaceBomb: true}
	value, survives, err := searchActionValue(engine, observation, action, SearchConfig{
		HorizonMS: NativeBombFuseMS + 400, DangerHorizonMS: NativeBombFuseMS + NativeFlameDurationMS,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !survives {
		t.Fatalf("two-turn L-pocket escape was classified as fatal (value %.2f)", value)
	}
}

func TestTacticalSearchRemainsActiveWhileBombExists(t *testing.T) {
	engine := mustEngine(t, testConfig())
	if _, err := engine.Step([]Action{{PlayerID: 1, PlaceBomb: true}}); err != nil {
		t.Fatal(err)
	}
	if !requiresTacticalSearch(engine, 1, Action{PlayerID: 1, Move: DirectionRight}, SearchConfig{HorizonMS: NativeBombFuseMS + 200}) {
		t.Fatal("movement bypassed tactical search while a live bomb existed")
	}
}

func TestTacticalSafetyExecutesEscapeAndPreventsReturnIntoOwnBlast(t *testing.T) {
	config := testConfig()
	config.Grid = testOpenGrid(5, 3)
	config.Rules.TickMS = 20
	config.Rules.BombFuseMS = 1_000
	config.Rules.FlameDurationMS = 200
	config.Rules.ActorHalfSizePixels = NativeActorHalfSizePixels
	config.Participants = []Participant{
		{PlayerID: 1, TeamID: 1, Source: ParticipantVirtualAI, Spawn: Cell{Row: 1, Col: 1}, SpeedPixelsPerSecond: 160, BombCapacity: 1, BombPower: 2},
		{PlayerID: 2, TeamID: 2, Source: ParticipantHuman, Spawn: Cell{Row: 1, Col: 4}, SpeedPixelsPerSecond: 160, BombCapacity: 1, BombPower: 1},
	}
	engine := mustEngine(t, config)
	if _, err := engine.Step([]Action{{PlayerID: 1, PlaceBomb: true}}); err != nil {
		t.Fatal(err)
	}

	// The learned actor persistently proposes walking back down into the bomb's
	// row. Search can imagine a later rescue for each isolated proposal; the
	// live safety latch must instead execute one route and hold the reached safe
	// cell until the actor's own wave has cleared.
	policy := &TacticalSafetyPolicy{
		Base: PolicyFunc(func(observation Observation, _ []Action) (Action, error) {
			return Action{PlayerID: observation.PlayerID, Move: DirectionDown}, nil
		}),
		HorizonMS: 1_400, EngageWithinMS: 1_400,
	}
	for engine.elapsedMS < 1_300 && !engine.outcome.Ended {
		observation, err := engine.Observation(1)
		if err != nil {
			t.Fatal(err)
		}
		legal, err := engine.LegalActions(1)
		if err != nil {
			t.Fatal(err)
		}
		chosen, err := policy.ChooseActionWithSnapshot(engine.Clone(), observation, legal)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = engine.Step([]Action{chosen}); err != nil {
			t.Fatal(err)
		}
	}
	if actor := engine.actors[engine.actorIndex(1)]; actor.State != ActorActive {
		t.Fatalf("actor returned into its own blast: %+v", actor)
	}
}

func TestTacticalSafetyAllowsSafeMovementAfterEscape(t *testing.T) {
	config := testConfig()
	config.Grid = testOpenGrid(5, 4)
	config.Rules.TickMS = 20
	config.Rules.BombFuseMS = 1_000
	config.Rules.FlameDurationMS = 200
	config.Rules.ActorHalfSizePixels = NativeActorHalfSizePixels
	config.Participants = []Participant{
		{PlayerID: 1, TeamID: 1, Source: ParticipantVirtualAI, Spawn: Cell{Row: 1, Col: 1}, SpeedPixelsPerSecond: 160, BombCapacity: 2, BombPower: 2},
		{PlayerID: 2, TeamID: 2, Source: ParticipantHuman, Spawn: Cell{Row: 3, Col: 4}, SpeedPixelsPerSecond: 160, BombCapacity: 1, BombPower: 1},
	}
	engine := mustEngine(t, config)
	if _, err := engine.Step([]Action{{PlayerID: 1, PlaceBomb: true}}); err != nil {
		t.Fatal(err)
	}
	policyCalls := 0
	policy := &TacticalSafetyPolicy{
		Base: PolicyFunc(func(observation Observation, _ []Action) (Action, error) {
			policyCalls++
			return Action{PlayerID: observation.PlayerID, Move: DirectionRight, PlaceBomb: policyCalls > 1}, nil
		}),
		HorizonMS: 1_400, EngageWithinMS: 1_400,
	}
	// First engage the latch while the actor is standing in its own future
	// blast, then place it on a safe row as if the escape route just completed.
	observation, err := engine.Observation(1)
	if err != nil {
		t.Fatal(err)
	}
	legal, err := engine.LegalActions(1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = policy.ChooseActionWithSnapshot(engine.Clone(), observation, legal); err != nil {
		t.Fatal(err)
	}
	engine.actors[engine.actorIndex(1)].Position = PositionAtCellCenter(Cell{Row: 2, Col: 2})
	observation, err = engine.Observation(1)
	if err != nil {
		t.Fatal(err)
	}
	legal, err = engine.LegalActions(1)
	if err != nil {
		t.Fatal(err)
	}
	chosen, err := policy.ChooseActionWithSnapshot(engine.Clone(), observation, legal)
	if err != nil {
		t.Fatal(err)
	}
	if chosen.Move != DirectionRight || !chosen.PlaceBomb {
		t.Fatalf("safe learned movement/pulse was frozen: got %+v, want right+bomb", chosen)
	}
}

func TestTacticalSafetyPreservesStandOnBubbleBeforeEscapeDeadline(t *testing.T) {
	config := testConfig()
	config.Grid = testOpenGrid(5, 3)
	config.Rules.TickMS = 20
	config.Rules.BombFuseMS = 1_000
	config.Rules.FlameDurationMS = 200
	config.Participants = []Participant{
		{PlayerID: 1, TeamID: 1, Source: ParticipantVirtualAI, Spawn: Cell{Row: 1, Col: 1}, SpeedPixelsPerSecond: 160, BombCapacity: 2, BombPower: 2},
		{PlayerID: 2, TeamID: 2, Source: ParticipantHuman, Spawn: Cell{Row: 1, Col: 4}, SpeedPixelsPerSecond: 160, BombCapacity: 1, BombPower: 1},
	}
	engine := mustEngine(t, config)
	if _, err := engine.Step([]Action{{PlayerID: 1, PlaceBomb: true}}); err != nil {
		t.Fatal(err)
	}
	policy := &TacticalSafetyPolicy{
		Base: PolicyFunc(func(observation Observation, _ []Action) (Action, error) {
			return Action{PlayerID: observation.PlayerID, Move: DirectionNone}, nil
		}),
		HorizonMS: 1_400, EngageWithinMS: 200,
	}
	observation, err := engine.Observation(1)
	if err != nil {
		t.Fatal(err)
	}
	legal, err := engine.LegalActions(1)
	if err != nil {
		t.Fatal(err)
	}
	chosen, err := policy.ChooseActionWithSnapshot(engine.Clone(), observation, legal)
	if err != nil {
		t.Fatal(err)
	}
	if chosen.Move != DirectionNone || policy.engagedUntilMS != 0 {
		t.Fatalf("future bubble forced an early escape: action=%+v engagedUntil=%d", chosen, policy.engagedUntilMS)
	}
}

func TestTacticalSafetyUsesRouteAwareEscapeDeadline(t *testing.T) {
	config := testConfig()
	config.Grid = testOpenGrid(5, 3)
	config.Rules.TickMS = 20
	config.Rules.BombFuseMS = 999
	config.Rules.FlameDurationMS = 200
	config.Participants = []Participant{
		{PlayerID: 1, TeamID: 1, Source: ParticipantVirtualAI, Spawn: Cell{Row: 1, Col: 1}, SpeedPixelsPerSecond: 85, BombCapacity: 2, BombPower: 2},
		{PlayerID: 2, TeamID: 2, Source: ParticipantHuman, Spawn: Cell{Row: 2, Col: 4}, SpeedPixelsPerSecond: 160, BombCapacity: 1, BombPower: 1},
	}
	engine := mustEngine(t, config)
	if _, err := engine.Step([]Action{{PlayerID: 1, PlaceBomb: true}}); err != nil {
		t.Fatal(err)
	}
	policy := &TacticalSafetyPolicy{
		Base: PolicyFunc(func(observation Observation, _ []Action) (Action, error) {
			return Action{PlayerID: observation.PlayerID, Move: DirectionNone}, nil
		}),
		HorizonMS: 1_400, EngageWithinMS: 200,
	}
	observation, err := engine.Observation(1)
	if err != nil {
		t.Fatal(err)
	}
	legal, err := engine.LegalActions(1)
	if err != nil {
		t.Fatal(err)
	}
	chosen, err := policy.ChooseActionWithSnapshot(engine.Clone(), observation, legal)
	if err != nil {
		t.Fatal(err)
	}
	// The nearest cell outside a power-2 cross requires two 471 ms cell
	// traversals at this speed. Although the fixed margin is only 200 ms, the
	// policy must begin that route now instead of waiting until escape is
	// physically impossible.
	if chosen.Move == DirectionNone || policy.engagedUntilMS == 0 {
		t.Fatalf("slow actor retained wait past route-aware deadline: action=%+v engagedUntil=%d", chosen, policy.engagedUntilMS)
	}
}

func TestRuntimeGivesSearchPolicyOnlySnapshot(t *testing.T) {
	config := testConfig()
	config.Participants[1].Source = ParticipantVirtualAI
	engine := mustEngine(t, config)
	virtualID := uint16(2)
	policy := TopKSearchPolicy{Candidate: fixedCandidates{{Action: Action{PlayerID: virtualID, Move: DirectionNone}, Score: 1}}}
	runtime, err := NewRuntime(engine, map[uint16]Policy{virtualID: policy})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Step(nil); err != nil {
		t.Fatal(err)
	}
	if engine.ElapsedMS() == 0 {
		t.Fatal("runtime did not advance authoritative engine")
	}
}

func TestGreedyCandidatePolicyDoesNotRerankActorChoice(t *testing.T) {
	engine := mustEngine(t, testConfig())
	playerID := uint16(1)
	observation, err := engine.Observation(playerID)
	if err != nil {
		t.Fatal(err)
	}
	legal, err := engine.LegalActions(playerID)
	if err != nil {
		t.Fatal(err)
	}
	place, _ := ActionFromID(playerID, ActionPlaceBomb)
	move, _ := ActionFromID(playerID, ActionMoveRight)
	policy := GreedyCandidatePolicy{Candidate: fixedCandidates{
		{Action: place, Score: 2},
		{Action: move, Score: 1},
	}}
	chosen, err := policy.ChooseActionWithSnapshot(engine.Clone(), observation, legal)
	if err != nil {
		t.Fatal(err)
	}
	if chosen != place {
		t.Fatalf("greedy policy chose %+v, want actor top-1 %+v", chosen, place)
	}
}

func TestSearchPickupProgressValueUsesEffectiveAttributeGain(t *testing.T) {
	before := Actor{Participant: Participant{
		BombCapacity: 2,
		BombPower:    4,
		SpeedRate:    3,
	}}
	after := before
	after.BombCapacity = 8
	after.BombPower = 6
	after.SpeedRate = 4

	const want = 9 * searchDevelopmentUnitValue
	if got := searchPickupProgressValue(before, after, SearchConfig{TrapValue: 12}); got != want {
		t.Fatalf("attribute pickup progress value = %v, want %v", got, want)
	}
	if got := searchPickupProgressValue(after, after, SearchConfig{TrapValue: 12}); got != 0 {
		t.Fatalf("capped/no-op attribute pickup progress value = %v, want 0", got)
	}
}

func TestSearchPickupProgressValueTreatsForkAsInventoryAndRecoverableHit(t *testing.T) {
	before := Actor{}
	after := before
	after.HeldActions[0] = HeldActionSlot{ActionID: 63, Count: 1}

	const trapValue = 12
	want := searchDevelopmentUnitValue + trapValue
	if got := searchPickupProgressValue(before, after, SearchConfig{TrapValue: trapValue}); got != want {
		t.Fatalf("fork pickup progress value = %v, want %v", got, want)
	}
}
