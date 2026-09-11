package battleenv

import (
	"reflect"
	"testing"

	"qqtang/internal/game/battleengine"
)

func TestHeldActionFeaturesUseFixedNativeIdentity(t *testing.T) {
	self := battleengine.ActorObservation{
		PlayerID: 1,
		TeamID:   1,
		HeldActions: [battleengine.NativeBattleActionSlots]battleengine.HeldActionSlot{
			{ActionID: 64, Count: 7},
			{ActionID: 41, Count: 2},
		},
	}
	observation := battleengine.Observation{
		PlayerID: 1,
		Grid:     battleengine.Grid{Width: 1, Height: 1},
		Actors:   []battleengine.ActorObservation{self},
	}
	values := make([]float32, ScalarFeatures)
	setScalarFeatures(values, observation, self)
	if got, want := values[34], float32(2.0/9.0); got != want {
		t.Fatalf("ActionID 41 feature = %v, want %v", got, want)
	}
	if got, want := values[40], float32(7.0/9.0); got != want {
		t.Fatalf("ActionID 64 feature = %v, want %v", got, want)
	}
	if values[35] != 0 {
		t.Fatalf("pickup slot order leaked into ActionID 42 feature: %v", values[35])
	}
}

func TestVisiblePickupFactsExposeEffectiveUpgradeAndForkIdentity(t *testing.T) {
	grid := battleengine.Grid{Width: 4, Height: 1, Cells: []battleengine.Tile{
		{Kind: battleengine.CellOpen}, {Kind: battleengine.CellOpen},
		{Kind: battleengine.CellOpen}, {Kind: battleengine.CellOpen},
	}}
	self := battleengine.ActorObservation{
		PlayerID: 1, TeamID: 1, Cell: battleengine.Cell{},
		BombCapacity: 2, MaxBombCapacity: 8,
		BombPower: 8, MaxBombPower: 9,
		Capabilities: battleengine.ActorCapabilities{CanCollectItems: true},
	}
	observation := battleengine.Observation{
		PlayerID: 1, Grid: grid, Actors: []battleengine.ActorObservation{self},
		Pickups: []battleengine.Pickup{
			{SceneID: battleengine.SceneBombCapacityLarge, Cell: battleengine.Cell{Col: 1}, State: battleengine.PickupAvailable},
			{SceneID: battleengine.SceneBombPowerLarge, Cell: battleengine.Cell{Col: 2}, State: battleengine.PickupAvailable},
			{SceneID: 24, Cell: battleengine.Cell{Col: 3}, State: battleengine.PickupAvailable},
		},
	}
	tensors := newTensorBatch(1, 1, 1, 4)
	if err := encodeActor(&tensors, 0, 0, observation, battleengine.DangerTimeline{}, battleengine.TacticalConsequences{}, battleengine.ActionMask{}); err != nil {
		t.Fatal(err)
	}
	at := func(channel, col int) float32 { return tensors.Spatial[channel*4+col] }
	if at(20, 1) != 1 || at(pickupAttributeAmountChannel, 1) != 1 || at(pickupEffectiveGainChannel, 1) != 0.75 {
		t.Fatalf("large capacity pickup facts = category %v amount %v gain %v", at(20, 1), at(pickupAttributeAmountChannel, 1), at(pickupEffectiveGainChannel, 1))
	}
	if at(21, 2) != 1 || at(pickupAttributeAmountChannel, 2) != 1 || at(pickupEffectiveGainChannel, 2) != 0.125 {
		t.Fatalf("capped large power pickup facts = category %v amount %v gain %v", at(21, 2), at(pickupAttributeAmountChannel, 2), at(pickupEffectiveGainChannel, 2))
	}
	forkChannel := pickupActionChannelStart + 5 // native ActionID 63
	if at(24, 3) != 1 || at(forkChannel, 3) != 1 {
		t.Fatalf("fork pickup facts = category %v identity %v", at(24, 3), at(forkChannel, 3))
	}
}

func TestPublicWallItemProfileUsesSchemaThreeScalarTail(t *testing.T) {
	self := battleengine.ActorObservation{PlayerID: 1, TeamID: 1}
	observation := battleengine.Observation{
		PlayerID: 1,
		Grid:     battleengine.Grid{Width: 1, Height: 1},
		Actors:   []battleengine.ActorObservation{self},
	}
	observation.PublicWallItems.Categories[battleengine.WallItemBombCapacity] = battleengine.PublicWallItemCategoryProfile{
		PresenceProbability: 0.75,
		ExpectedDensity:     0.125,
	}
	observation.PublicWallItems.Categories[battleengine.WallItemTransformation] = battleengine.PublicWallItemCategoryProfile{
		PresenceProbability: 0.5,
		ExpectedDensity:     0.25,
	}
	values := make([]float32, ScalarFeatures)
	setScalarFeatures(values, observation, self)
	if values[48] != 0.75 || values[49] != 0.125 {
		t.Fatalf("capacity profile = %v/%v, want 0.75/0.125", values[48], values[49])
	}
	base := 48 + int(battleengine.WallItemTransformation)*2
	if values[base] != 0.5 || values[base+1] != 0.25 {
		t.Fatalf("transformation profile = %v/%v, want 0.5/0.25", values[base], values[base+1])
	}
}

func TestMapElementOccupancyAndPushProgressMatchNativeGridSemantics(t *testing.T) {
	grid := battleengine.Grid{Width: 2, Height: 1, Cells: []battleengine.Tile{
		{Kind: battleengine.CellOpen},
		{
			Kind: battleengine.CellOpen, MapElementOccupied: true,
			NormalPushable: true, PushCounter: 6,
		},
	}}
	observation := battleengine.Observation{
		PlayerID: 1, Grid: grid,
		Actors: []battleengine.ActorObservation{{
			PlayerID: 1, TeamID: 1, Cell: battleengine.Cell{},
		}},
	}
	tensors := newTensorBatch(1, 1, 1, 2)
	if err := encodeActor(&tensors, 0, 0, observation, battleengine.DangerTimeline{}, battleengine.TacticalConsequences{}, battleengine.ActionMask{}); err != nil {
		t.Fatal(err)
	}
	at := func(channel, col int) float32 { return tensors.Spatial[channel*2+col] }
	if at(1, 0) != 1 || at(1, 1) != 0 {
		t.Fatalf("walkable-open channel = %v/%v, occupied native element must not be open", at(1, 0), at(1, 1))
	}
	if at(5, 1) != 1 || at(63, 1) != 0.5 {
		t.Fatalf("pushable/progress channels = %v/%v, want 1/0.5", at(5, 1), at(63, 1))
	}
}

func TestPublicBehaviorMemoryUsesActorCellChannels(t *testing.T) {
	grid := battleengine.Grid{Width: 2, Height: 1, Cells: []battleengine.Tile{
		{Kind: battleengine.CellOpen}, {Kind: battleengine.CellOpen},
	}}
	memory := battleengine.PublicBehaviorMemory{
		Recent:  [battleengine.PublicBehaviorKindCount]uint16{32768, 16384, 1311, 0},
		Totals:  [battleengine.PublicBehaviorKindCount]uint32{50, 25, 2, 1},
		Samples: 100,
	}
	observation := battleengine.Observation{
		PlayerID: 1, Grid: grid,
		Actors: []battleengine.ActorObservation{
			{PlayerID: 1, TeamID: 1, Cell: battleengine.Cell{}},
			{PlayerID: 2, TeamID: 2, Cell: battleengine.Cell{Col: 1}, PublicBehavior: memory},
		},
	}
	tensors := newTensorBatch(1, 1, 1, 2)
	if err := encodeActor(&tensors, 0, 0, observation, battleengine.DangerTimeline{}, battleengine.TacticalConsequences{}, battleengine.ActionMask{}); err != nil {
		t.Fatal(err)
	}
	at := func(channel, col int) float32 { return tensors.Spatial[channel*2+col] }
	if got := at(64, 1); got < 0.49 || got > 0.51 {
		t.Fatalf("recent move memory = %v, want about 0.5", got)
	}
	if got := at(66, 1); got < 0.99 {
		t.Fatalf("recent bomb memory = %v, want events/second scaling", got)
	}
	if got := at(68, 1); got != 0.5 {
		t.Fatalf("whole-round move memory = %v, want 0.5", got)
	}
	if got := at(70, 1); got != 1 {
		t.Fatalf("whole-round bomb memory = %v, want 1 event/second", got)
	}
}

func TestPublicCoalitionContextDistinguishesEnemyTeamPartitions(t *testing.T) {
	grid := battleengine.Grid{Width: 4, Height: 1, Cells: []battleengine.Tile{
		{Kind: battleengine.CellOpen}, {Kind: battleengine.CellOpen},
		{Kind: battleengine.CellOpen}, {Kind: battleengine.CellOpen},
	}}
	observation := battleengine.Observation{
		PlayerID: 1, Grid: grid,
		Actors: []battleengine.ActorObservation{
			{PlayerID: 1, TeamID: 1, Cell: battleengine.Cell{Col: 0}},
			{PlayerID: 2, TeamID: 1, State: battleengine.ActorEliminated},
			{PlayerID: 3, TeamID: 2, Cell: battleengine.Cell{Col: 1}},
			{PlayerID: 4, TeamID: 2, State: battleengine.ActorTrapped, Cell: battleengine.Cell{Col: 2}},
			{PlayerID: 5, TeamID: 3, Cell: battleengine.Cell{Col: 3}},
			{PlayerID: 6, TeamID: 4, State: battleengine.ActorEliminated},
		},
	}
	tensors := newTensorBatch(1, 1, 1, 4)
	if err := encodeActor(&tensors, 0, 0, observation, battleengine.DangerTimeline{}, battleengine.TacticalConsequences{}, battleengine.ActionMask{}); err != nil {
		t.Fatal(err)
	}
	at := func(channel, col int) float32 { return tensors.Spatial[channel*4+col] }
	if at(72, 1) != 2.0/8 || at(72, 3) != 3.0/8 {
		t.Fatalf("public enemy TeamID channels = %v/%v, want 2/8 and 3/8", at(72, 1), at(72, 3))
	}
	if at(73, 1) != 2.0/8 || at(73, 3) != 1.0/8 || at(74, 1) != 2.0/8 {
		t.Fatalf("enemy initial/alive team sizes = %v/%v/%v", at(73, 1), at(73, 3), at(74, 1))
	}
	for col := 0; col < 4; col++ {
		if at(75, col) != 2.0/8 || at(76, col) != 1.0/8 || at(77, col) != 2.0/8 || at(78, col) != 2.0/8 || at(79, col) != 3.0/8 {
			t.Fatalf("coalition context at col %d = %v/%v/%v/%v/%v", col, at(75, col), at(76, col), at(77, col), at(78, col), at(79, col))
		}
		if at(80, col) != 1 || at(81, col) != 0 {
			t.Fatalf("first within-team role at col %d = %v/%v, want 1/0", col, at(80, col), at(81, col))
		}
	}

	observation.PlayerID = 4
	secondRole := newTensorBatch(1, 1, 1, 4)
	if err := encodeActor(&secondRole, 0, 0, observation, battleengine.DangerTimeline{}, battleengine.TacticalConsequences{}, battleengine.ActionMask{}); err != nil {
		t.Fatal(err)
	}
	roleAt := func(channel, col int) float32 { return secondRole.Spatial[channel*4+col] }
	for col := 0; col < 4; col++ {
		if roleAt(80, col) != 0 || roleAt(81, col) != 1 {
			t.Fatalf("second within-team role at col %d = %v/%v, want 0/1", col, roleAt(80, col), roleAt(81, col))
		}
	}
}

func TestNativePassScalarCarriesChargeThenTraversalProgress(t *testing.T) {
	values := make([]float32, ScalarFeatures)
	self := battleengine.ActorObservation{
		PlayerID: 1, TeamID: 1,
		NativePassCollisionValid: true, NativePassCollisionStartedAt: 1_000,
	}
	observation := battleengine.Observation{
		PlayerID: 1, ClockMS: 1_300,
		Grid:   battleengine.Grid{Width: 1, Height: 1},
		Actors: []battleengine.ActorObservation{self},
	}
	setScalarFeatures(values, observation, self)
	if values[41] != 1 || values[42] != 0 || values[43] != 0.5 {
		t.Fatalf("pass charge features = %v/%v/%v, want 1/0/0.5", values[41], values[42], values[43])
	}

	self.NativePassActive = true
	self.NativePassStartedAt = 1_200
	self.NativePassDurationMS = 400
	observation.ClockMS = 1_300
	observation.Actors[0] = self
	clear(values)
	setScalarFeatures(values, observation, self)
	if values[41] != 1 || values[42] != 1 || values[43] != 0.25 {
		t.Fatalf("pass traversal features = %v/%v/%v, want 1/1/0.25", values[41], values[42], values[43])
	}
}

func TestVisibleTransformationsAndRecoveryProtectionUseSpatialChannels(t *testing.T) {
	grid := battleengine.Grid{Width: 3, Height: 3, Cells: make([]battleengine.Tile, 9)}
	observation := battleengine.Observation{
		PlayerID:  1,
		ElapsedMS: 1_000,
		Grid:      grid,
		Actors: []battleengine.ActorObservation{
			{PlayerID: 1, TeamID: 1, Cell: battleengine.Cell{Row: 0, Col: 0}},
			{PlayerID: 2, TeamID: 1, Cell: battleengine.Cell{Row: 1, Col: 1}, TransformationSceneID: 104, TransformationExpiresAt: 16_000},
			{PlayerID: 3, TeamID: 2, Cell: battleengine.Cell{Row: 2, Col: 2}, TransformationSceneID: 115, TransformationExpiresAt: 31_000, HarmProtectionExpiresAt: 4_000},
		},
	}
	tensors := newTensorBatch(1, 3, 3, 3)
	if err := encodeActor(&tensors, 0, 0, observation, battleengine.DangerTimeline{}, battleengine.TacticalConsequences{}, battleengine.ActionMask{}); err != nil {
		t.Fatal(err)
	}
	at := func(channel, row, col int) float32 {
		return tensors.Spatial[(channel*3+row)*3+col]
	}
	if got := at(27, 1, 1); got != 0.625 {
		t.Fatalf("duck transformation identity/countdown = %v, want 0.625", got)
	}
	if got := at(33, 2, 2); got != 1 {
		t.Fatalf("axe transformation remaining = %v, want 1", got)
	}
	if got := at(34, 2, 2); got != 1 {
		t.Fatalf("visible harm protection remaining = %v, want 1", got)
	}
	if got := at(34, 1, 1); got != 0 {
		t.Fatalf("unprotected transformed ally = %v, want 0", got)
	}
}

func TestDangerChainAndVisibleActorDetailsUseDerivedChannels(t *testing.T) {
	grid := battleengine.Grid{Width: 3, Height: 3, Cells: make([]battleengine.Tile, 9)}
	observation := battleengine.Observation{
		PlayerID:  1,
		ElapsedMS: 1_000,
		Grid:      grid,
		Actors: []battleengine.ActorObservation{
			{PlayerID: 1, RoleID: 1, TeamID: 1, Cell: battleengine.Cell{Row: 1, Col: 1}, Position: battleengine.Position{X: 50, Y: 70}, Facing: battleengine.DirectionRight},
			{PlayerID: 2, RoleID: 2, TeamID: 2, Cell: battleengine.Cell{Row: 2, Col: 2}, Position: battleengine.Position{X: 100, Y: 100}, BombCapacity: 7, MaxBombCapacity: 8, BombPower: 8, MaxBombPower: 10, SpeedRate: 9, MaxSpeedRate: 10, MovementStatus: battleengine.MovementStatusSlow, HeldActions: [battleengine.NativeBattleActionSlots]battleengine.HeldActionSlot{{ActionID: 41, Count: 2}, {ActionID: 64, Count: 1}}},
		},
		Bombs: []battleengine.Bomb{{ID: 1, OwnerID: 2, Cell: battleengine.Cell{Row: 0, Col: 2}, ExplodeAtMS: 2_000}},
	}
	danger := battleengine.DangerTimeline{
		GeneratedAtMS: 1_000, HorizonMS: 4_000, Width: 3, Height: 3,
		EarliestImpactMS: []uint32{battleengine.NoDangerImpact, battleengine.NoDangerImpact, 2_000, battleengine.NoDangerImpact, battleengine.NoDangerImpact, battleengine.NoDangerImpact, battleengine.NoDangerImpact, battleengine.NoDangerImpact, battleengine.NoDangerImpact},
		LatestImpactMS:   []uint32{battleengine.NoDangerImpact, battleengine.NoDangerImpact, 4_000, battleengine.NoDangerImpact, battleengine.NoDangerImpact, battleengine.NoDangerImpact, battleengine.NoDangerImpact, battleengine.NoDangerImpact, battleengine.NoDangerImpact},
		SafeAfterMS:      []uint32{battleengine.NoDangerImpact, battleengine.NoDangerImpact, 4_500, battleengine.NoDangerImpact, battleengine.NoDangerImpact, battleengine.NoDangerImpact, battleengine.NoDangerImpact, battleengine.NoDangerImpact, battleengine.NoDangerImpact},
		ImpactWaves:      []uint8{0, 0, 2, 0, 0, 0, 0, 0, 0},
	}
	tensors := newTensorBatch(1, 2, 3, 3)
	if err := encodeActor(&tensors, 0, 0, observation, danger, battleengine.TacticalConsequences{}, battleengine.ActionMask{}); err != nil {
		t.Fatal(err)
	}
	at := func(channel, row, col int) float32 { return tensors.Spatial[(channel*3+row)*3+col] }
	if got := at(35, 0, 2); got != 0.25 {
		t.Fatalf("last-impact urgency = %v, want 0.25", got)
	}
	if got := at(36, 0, 2); got != float32(3_500.0/4_500.0) {
		t.Fatalf("unsafe remaining = %v, want %v", got, float32(3_500.0/4_500.0))
	}
	if got := at(37, 0, 2); got != 0.5 || at(40, 0, 2) != 1 {
		t.Fatalf("wave/enemy-bomb channels = %v/%v, want 0.5/1", got, at(40, 0, 2))
	}
	if at(41, 1, 1) != 0.25 || at(42, 1, 1) != 0.75 || at(44, 1, 1) != 1 {
		t.Fatalf("self subcell/facing channels = %v/%v/%v", at(41, 1, 1), at(42, 1, 1), at(44, 1, 1))
	}
	if at(47, 2, 2) != 1 || at(50, 2, 2) != 0.875 || at(52, 2, 2) != 0.8 || at(54, 2, 2) != 0.9 {
		t.Fatalf("enemy status/current attributes = %v/%v/%v/%v", at(47, 2, 2), at(50, 2, 2), at(52, 2, 2), at(54, 2, 2))
	}
	if got, want := at(56, 2, 2), float32(2.0/9.0); got != want || at(62, 2, 2) != float32(1.0/9.0) {
		t.Fatalf("enemy inferred shortcut ledger = %v/%v, want %v/%v", got, at(62, 2, 2), want, float32(1.0/9.0))
	}
}

func TestDeadlineFeaturesUseAbsoluteNativeClockAndThrownLanding(t *testing.T) {
	grid := battleengine.Grid{Width: 1, Height: 1, Cells: make([]battleengine.Tile, 1)}
	observation := battleengine.Observation{
		PlayerID: 1, ClockMS: 3_000, ElapsedMS: 0, RemainingMS: 237_000, Grid: grid,
		Actors: []battleengine.ActorObservation{{PlayerID: 1, TeamID: 1, Cell: battleengine.Cell{}}},
		Bombs: []battleengine.Bomb{{
			ID: 1, OwnerID: 1, Cell: battleengine.Cell{},
			ExplodeAtMS: 3_100, FlightUntilMS: 3_400,
		}},
	}
	tensors := newTensorBatch(1, 1, 1, 1)
	if err := encodeActor(&tensors, 0, 0, observation, battleengine.DangerTimeline{}, battleengine.TacticalConsequences{}, battleengine.ActionMask{}); err != nil {
		t.Fatal(err)
	}
	got := tensors.Spatial[14]
	want := float32(400.0 / float64(battleengine.NativeBombFuseMS))
	if got != want {
		t.Fatalf("absolute thrown-bomb countdown = %v, want %v", got, want)
	}
}

func TestVisibleTacticalRoutesExposeStableReachableCandidates(t *testing.T) {
	grid := battleengine.Grid{Width: 5, Height: 4, Cells: make([]battleengine.Tile, 20)}
	for index := range grid.Cells {
		grid.Cells[index] = battleengine.Tile{Kind: battleengine.CellOpen, FlamePassable: true}
	}
	// The direct enemy route is blocked, so stable U/R/D/L BFS goes up first.
	grid.Cells[1*5+1] = battleengine.Tile{Kind: battleengine.CellSolid}
	// A separate breakable wall provides a reachable demolition candidate.
	grid.Cells[2*5+3] = battleengine.Tile{Kind: battleengine.CellBreakable, Durability: 1}
	self := battleengine.ActorObservation{
		PlayerID: 1, TeamID: 1, Cell: battleengine.Cell{Row: 1, Col: 0},
		Capabilities: battleengine.ActorCapabilities{CanCollectItems: true},
	}
	observation := battleengine.Observation{
		PlayerID: 1, Grid: grid,
		Actors: []battleengine.ActorObservation{
			self,
			{PlayerID: 2, TeamID: 1, State: battleengine.ActorTrapped, Cell: battleengine.Cell{Row: 0, Col: 0}},
			{PlayerID: 3, TeamID: 2, Cell: battleengine.Cell{Row: 1, Col: 4}},
		},
		Pickups: []battleengine.Pickup{{SceneID: 1, Cell: battleengine.Cell{Row: 2, Col: 0}, State: battleengine.PickupAvailable}},
	}
	routes := visibleTacticalRoutes(observation, self)
	checks := []struct {
		kind     int
		first    battleengine.Direction
		distance int
	}{
		{tacticalRouteEnemy, battleengine.DirectionUp, 6},
		{tacticalRouteTrappedAlly, battleengine.DirectionUp, 1},
		{tacticalRoutePickup, battleengine.DirectionDown, 1},
		{tacticalRouteDemolition, battleengine.DirectionDown, 3},
	}
	for _, check := range checks {
		got := routes[check.kind]
		if !got.found || got.first != check.first || got.distance != check.distance {
			t.Fatalf("route %d = %+v, want first=%d distance=%d", check.kind, got, check.first, check.distance)
		}
	}

	tensors := newTensorBatch(1, 1, int(grid.Height), int(grid.Width))
	if err := encodeActor(&tensors, 0, 0, observation, battleengine.DangerTimeline{}, battleengine.TacticalConsequences{}, battleengine.ActionMask{}); err != nil {
		t.Fatal(err)
	}
	if tensors.Scalars[64] != 1 || tensors.Scalars[69] != 1 || tensors.Scalars[76] != 1 || tensors.Scalars[81] != 1 {
		t.Fatalf("route first-step one-hots = enemy %v ally %v pickup %v demolition %v", tensors.Scalars[64:68], tensors.Scalars[69:73], tensors.Scalars[74:78], tensors.Scalars[79:83])
	}
	if tensors.Scalars[68] == 0 || tensors.Scalars[73] == 0 || tensors.Scalars[78] == 0 || tensors.Scalars[83] == 0 {
		t.Fatalf("route presence/distances missing: %v", tensors.Scalars[64:84])
	}
}

func TestTacticalConsequencesUseSchemaElevenScalarTail(t *testing.T) {
	facts := battleengine.TacticalConsequences{
		CurrentKnownDangerReachable:         [5]float32{0.1, 0.2, 0.3, 0.4, 0.5},
		BombKnownDangerReachable:            [5]float32{0.6, 0.7, 0.8, 0.9, 1},
		KnownDangerProjectionValid:          true,
		BombLegal:                           true,
		BombKnownDangerProjectionValid:      true,
		BombEarliestWholeCellRefugeEstimate: 0.25,
		EnemyCurrentKnownReachable:          [3]float32{0.7, 0.6, 0.5},
		EnemyBombKnownReachable:             [3]float32{0.4, 0.3, 0.2},
		EnemyKnownReachReduction:            [3]float32{0.3, 0.3, 0.3},
		AllyKnownReachReduction:             0.1,
		NewEnemyThreatRatio:                 1,
		NewAllyThreatRatio:                  0.5,
		NewBreakableThreatRatio:             0.75,
		AcceleratedChainRatio:               0.25,
	}
	values := make([]float32, ScalarFeatures)
	setTacticalConsequenceFeatures(values, facts)
	want := []float32{
		0.1, 0.2, 0.3, 0.4, 0.5,
		0.6, 0.7, 0.8, 0.9, 1,
		1, 1, 0.25,
		0.7, 0.6, 0.5,
		0.4, 0.3, 0.2,
		0.3, 0.3, 0.3,
		0.1, 1, 0.5, 0.75, 0.25, 1,
	}
	if got := values[84:112]; !reflect.DeepEqual(got, want) {
		t.Fatalf("schema-11 tactical tail = %v, want %v", got, want)
	}
}

func TestDirectionalTacticalConsequencesUseSchemaTwelveScalarTail(t *testing.T) {
	facts := battleengine.TacticalConsequences{}
	facts.CurrentDirectionKnownDangerReachable[battleengine.DirectionRight] = [5]float32{
		0.1, 0.2, 0.3, 0.4, 0.5,
	}
	facts.BombDirectionKnownDangerReachable[battleengine.DirectionLeft] = [5]float32{
		0.6, 0.7, 0.8, 0.9, 1,
	}
	values := make([]float32, ScalarFeatures)
	setTacticalConsequenceFeatures(values, facts)

	currentStart := 112 + int(battleengine.DirectionRight)*5
	bombStart := 112 + battleengine.TacticalMovementCandidateCount*5 +
		int(battleengine.DirectionLeft)*5
	if got, want := values[currentStart:currentStart+5], []float32{0.1, 0.2, 0.3, 0.4, 0.5}; !reflect.DeepEqual(got, want) {
		t.Fatalf("schema-12 rightward current consequences = %v, want %v", got, want)
	}
	if got, want := values[bombStart:bombStart+5], []float32{0.6, 0.7, 0.8, 0.9, 1}; !reflect.DeepEqual(got, want) {
		t.Fatalf("schema-12 leftward bomb consequences = %v, want %v", got, want)
	}
}

func TestDirectionalRefugesUseSchemaThirteenScalarTail(t *testing.T) {
	facts := battleengine.TacticalConsequences{}
	facts.CurrentDirectionWholeCellRefugeFound[battleengine.DirectionRight] = true
	facts.CurrentDirectionEarliestRefuge[battleengine.DirectionRight] = 0.2
	facts.BombDirectionWholeCellRefugeFound[battleengine.DirectionLeft] = true
	facts.BombDirectionEarliestRefuge[battleengine.DirectionLeft] = 0.75
	values := make([]float32, ScalarFeatures)
	setTacticalConsequenceFeatures(values, facts)

	if values[162+int(battleengine.DirectionRight)] != 1 ||
		values[167+int(battleengine.DirectionRight)] != 0.2 {
		t.Fatalf("schema-13 current refuge tail = %v", values[162:172])
	}
	if values[172+int(battleengine.DirectionLeft)] != 1 ||
		values[177+int(battleengine.DirectionLeft)] != 0.75 {
		t.Fatalf("schema-13 bomb refuge tail = %v", values[172:182])
	}
}

func TestVisibleTacticalRoutesDoNotLeakHiddenOrUnreachableTargets(t *testing.T) {
	grid := battleengine.Grid{Width: 3, Height: 3, Cells: make([]battleengine.Tile, 9)}
	for index := range grid.Cells {
		grid.Cells[index] = battleengine.Tile{Kind: battleengine.CellSolid}
	}
	grid.Cells[0] = battleengine.Tile{Kind: battleengine.CellOpen}
	grid.Cells[8] = battleengine.Tile{Kind: battleengine.CellOpen}
	self := battleengine.ActorObservation{
		PlayerID: 1, TeamID: 1, Cell: battleengine.Cell{},
		Capabilities: battleengine.ActorCapabilities{CanCollectItems: true},
	}
	observation := battleengine.Observation{
		PlayerID: 1, Grid: grid,
		Actors: []battleengine.ActorObservation{
			self,
			{PlayerID: 2, TeamID: 2, Cell: battleengine.Cell{Row: 2, Col: 2}},
		},
	}
	// Public probabilities describe map rules, not actual hidden allocations.
	observation.PublicWallItems.Categories[battleengine.WallItemBombCapacity] = battleengine.PublicWallItemCategoryProfile{
		PresenceProbability: 1, ExpectedDensity: 1,
	}
	routes := visibleTacticalRoutes(observation, self)
	if routes[tacticalRouteEnemy].found || routes[tacticalRoutePickup].found || routes[tacticalRouteDemolition].found {
		t.Fatalf("unreachable or hidden targets leaked into routes: %+v", routes)
	}
}

func TestVisibleTacticalRoutesCrossPlayerPassableMapElements(t *testing.T) {
	grid := battleengine.Grid{Width: 3, Height: 1, Cells: []battleengine.Tile{
		{Kind: battleengine.CellOpen},
		{Kind: battleengine.CellOpen, MapElementOccupied: true},
		{Kind: battleengine.CellOpen},
	}}
	self := battleengine.ActorObservation{
		PlayerID: 1, TeamID: 1, Cell: battleengine.Cell{},
		Capabilities: battleengine.ActorCapabilities{CanCollectItems: true},
	}
	observation := battleengine.Observation{
		PlayerID: 1, Grid: grid,
		Actors: []battleengine.ActorObservation{self, {PlayerID: 2, TeamID: 2, Cell: battleengine.Cell{Col: 2}}},
		Pickups: []battleengine.Pickup{{SceneID: 1, Cell: battleengine.Cell{Col: 2}, State: battleengine.PickupAvailable}},
	}
	routes := visibleTacticalRoutes(observation, self)
	for _, kind := range []int{tacticalRouteEnemy, tacticalRoutePickup} {
		if got := routes[kind]; !got.found || got.first != battleengine.DirectionRight || got.distance != 2 {
			t.Fatalf("route through player-passable map element = %+v, want right/2", got)
		}
	}
	observation.Grid.Cells[1].Kind = battleengine.CellSolid
	if got := visibleTacticalRoutes(observation, self); got[tacticalRouteEnemy].found || got[tacticalRoutePickup].found {
		t.Fatal("static blocking element became a traversable route")
	}
	observation.Grid.Cells[1].Kind = battleengine.CellOpen
	observation.Bombs = []battleengine.Bomb{{Cell: battleengine.Cell{Col: 1}}}
	if got := visibleTacticalRoutes(observation, self); got[tacticalRouteEnemy].found || got[tacticalRoutePickup].found {
		t.Fatal("dynamic bubble was ignored on a player-passable map element")
	}
}

func TestDuckTacticalRoutesTraverseStaticTerrainButIgnorePickups(t *testing.T) {
	grid := battleengine.Grid{Width: 3, Height: 1, Cells: []battleengine.Tile{
		{Kind: battleengine.CellOpen}, {Kind: battleengine.CellSolid}, {Kind: battleengine.CellOpen},
	}}
	self := battleengine.ActorObservation{
		PlayerID: 1, TeamID: 1, Cell: battleengine.Cell{}, TransformationSceneID: 104,
		Capabilities: battleengine.ActorCapabilities{TraverseStaticTerrain: true, CanCollectItems: false},
	}
	observation := battleengine.Observation{
		PlayerID: 1, Grid: grid,
		Actors:  []battleengine.ActorObservation{self, {PlayerID: 2, TeamID: 2, Cell: battleengine.Cell{Col: 2}}},
		Pickups: []battleengine.Pickup{{SceneID: 1, Cell: battleengine.Cell{Col: 2}, State: battleengine.PickupAvailable}},
	}
	routes := visibleTacticalRoutes(observation, self)
	if got := routes[tacticalRouteEnemy]; !got.found || got.first != battleengine.DirectionRight || got.distance != 2 {
		t.Fatalf("duck enemy route = %+v, want right/2", got)
	}
	if routes[tacticalRoutePickup].found || routes[tacticalRouteDemolition].found {
		t.Fatalf("duck received impossible collection/demolition routes: %+v", routes)
	}
}
