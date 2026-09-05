package battleengine

import (
	"math/rand"
	"testing"
)

func TestTacticalConsequencesExposeSafeEnemyPressure(t *testing.T) {
	config := testConfig()
	config.Rules.BombFuseMS = 1_000
	config.Rules.FlameDurationMS = 200
	config.Participants[0].SpeedPixelsPerSecond = 400
	config.Participants[0].BombPower = 2
	engine := mustEngine(t, config)
	current, err := engine.DangerTimeline(3_500)
	if err != nil {
		t.Fatal(err)
	}
	facts, err := engine.TacticalConsequencesForPlayer(1, current)
	if err != nil {
		t.Fatal(err)
	}
	if !facts.BombLegal || !facts.BombKnownDangerProjectionValid || facts.BombEarliestWholeCellRefugeEstimate == 0 {
		t.Fatalf("open-map placement should be legal and escapable: %+v", facts)
	}
	if facts.NewEnemyThreatRatio != 1 {
		t.Fatalf("enemy on the new blast line was not exposed: %+v", facts)
	}
	if facts.BombKnownDangerReachable[4] == 0 {
		t.Fatalf("open-map counterfactual found no safe refuge: %+v", facts)
	}
}

func TestTacticalConsequencesRejectSealedBombEscape(t *testing.T) {
	config := testConfig()
	config.Rules.BombFuseMS = 1_000
	config.Rules.FlameDurationMS = 200
	for index := range config.Grid.Cells {
		config.Grid.Cells[index] = Tile{Kind: CellSolid}
	}
	config.Grid.Cells[1*int(config.Grid.Width)+1] = Tile{Kind: CellOpen, FlamePassable: true}
	config.Grid.Cells[4] = Tile{Kind: CellOpen, FlamePassable: true}
	config.Participants[1].Spawn = Cell{Row: 0, Col: 4}
	engine := mustEngine(t, config)
	current, err := engine.DangerTimeline(3_500)
	if err != nil {
		t.Fatal(err)
	}
	facts, err := engine.TacticalConsequencesForPlayer(1, current)
	if err != nil {
		t.Fatal(err)
	}
	if !facts.BombLegal || !facts.BombKnownDangerProjectionValid || facts.BombEarliestWholeCellRefugeEstimate != 0 || facts.BombKnownDangerReachable[4] != 0 {
		t.Fatalf("sealed placement incorrectly reported a full-horizon refuge: %+v", facts)
	}
}

func TestTacticalConsequencesExposeWallAndChainEffects(t *testing.T) {
	t.Run("breakable wall", func(t *testing.T) {
		config := testConfig()
		config.Rules.BombFuseMS = 1_000
		config.Grid.Cells[1*int(config.Grid.Width)+2] = Tile{
			Kind: CellBreakable, Durability: 1,
		}
		engine := mustEngine(t, config)
		current, err := engine.DangerTimeline(3_500)
		if err != nil {
			t.Fatal(err)
		}
		facts, err := engine.TacticalConsequencesForPlayer(1, current)
		if err != nil {
			t.Fatal(err)
		}
		if facts.NewBreakableThreatRatio != 1 {
			t.Fatalf("newly threatened breakable wall was not exposed: %+v", facts)
		}
	})

	t.Run("accelerated chain", func(t *testing.T) {
		config := testConfig()
		config.Rules.BombFuseMS = 1_000
		config.Participants[0].BombPower = 2
		engine := mustEngine(t, config)
		engine.bombs = append(engine.bombs, Bomb{
			ID: 77, OwnerID: 2, Cell: Cell{Row: 1, Col: 3}, Power: 1,
			ExplodeAtMS: 3_000,
		})
		engine.nextBombID = 78
		current, err := engine.DangerTimeline(3_500)
		if err != nil {
			t.Fatal(err)
		}
		facts, err := engine.TacticalConsequencesForPlayer(1, current)
		if err != nil {
			t.Fatal(err)
		}
		if facts.AcceleratedChainRatio != 1 {
			t.Fatalf("new placement did not expose accelerated chain: %+v", facts)
		}
	})
}

func TestTacticalConsequencesExposeAllyRisk(t *testing.T) {
	config := testConfig()
	config.Rules.BombFuseMS = 1_000
	config.Participants[0].BombPower = 2
	config.Participants = append(config.Participants,
		testParticipant(3, 1, ParticipantHuman, Cell{Row: 1, Col: 3}),
	)
	config.Participants[1].Spawn = Cell{Row: 0, Col: 4}
	engine := mustEngine(t, config)
	current, err := engine.DangerTimeline(3_500)
	if err != nil {
		t.Fatal(err)
	}
	facts, err := engine.TacticalConsequencesForPlayer(1, current)
	if err != nil {
		t.Fatal(err)
	}
	if facts.NewAllyThreatRatio != 1 {
		t.Fatalf("ally on the new blast line was not exposed: %+v", facts)
	}
}

func TestTacticalConsequencesMarkForcedSlideProjectionUnavailable(t *testing.T) {
	config := testConfig()
	engine := mustEngine(t, config)
	engine.actors[0].MovementStatus = MovementStatusForcedSlide
	engine.actors[0].Facing = DirectionRight
	current, err := engine.DangerTimeline(3_500)
	if err != nil {
		t.Fatal(err)
	}
	facts, err := engine.TacticalConsequencesForPlayer(1, current)
	if err != nil {
		t.Fatal(err)
	}
	if facts.KnownDangerProjectionValid {
		t.Fatalf("forced-slide input was incorrectly represented as controllable reachability: %+v", facts)
	}
	if !facts.BombLegal {
		t.Fatalf("forced slide incorrectly changed the independent placement rule: %+v", facts)
	}
	if facts.BombKnownDangerProjectionValid {
		t.Fatalf("forced-slide bomb reachability was incorrectly marked valid: %+v", facts)
	}
	if facts.CurrentKnownDangerReachable != ([5]float32{}) || facts.BombKnownDangerReachable != ([5]float32{}) {
		t.Fatalf("unavailable projection exposed ambiguous non-zero reachability: %+v", facts)
	}
}

func TestTacticalConsequencesUseActualSubCellEscapeDistance(t *testing.T) {
	config := testConfig()
	config.Grid = testOpenGrid(5, 1)
	config.Rules.TickMS = 20
	config.Rules.BombFuseMS = 220
	config.Rules.FlameDurationMS = 200
	config.Participants[0].Spawn = Cell{Col: 1}
	config.Participants[0].SpeedPixelsPerSecond = 400
	config.Participants[0].BombPower = 1
	config.Participants[1].Spawn = Cell{Col: 4}

	centered := mustEngine(t, config)
	centerFacts := tacticalFactsForTest(t, centered, 1)
	if centerFacts.BombEarliestWholeCellRefugeEstimate == 0 {
		t.Fatalf("centered actor should reach column 3 before the 220ms blast: %+v", centerFacts)
	}

	offset := mustEngine(t, config)
	offset.actors[0].Position = Position{X: 41, Y: CellSizePixels / 2}
	offsetFacts := tacticalFactsForTest(t, offset, 1)
	if offsetFacts.BombEarliestWholeCellRefugeEstimate != 0 {
		t.Fatalf("left-edge actor cannot cover the longer real distance before the blast: %+v", offsetFacts)
	}
}

func TestTacticalConsequencesDistinguishOneTickEscapeDirections(t *testing.T) {
	config := testConfig()
	config.Grid = testOpenGrid(5, 1)
	config.Rules.TickMS = 20
	config.Rules.BombFuseMS = 219
	config.Rules.FlameDurationMS = 200
	config.Participants[0].Spawn = Cell{Col: 1}
	config.Participants[0].SpeedPixelsPerSecond = 400
	config.Participants[0].BombPower = 1
	config.Participants[1].Spawn = Cell{Col: 4}
	engine := mustEngine(t, config)
	facts := tacticalFactsForTest(t, engine, 1)

	if facts.BombDirectionKnownDangerReachable[DirectionRight][4] == 0 {
		t.Fatalf("one rightward production tick should retain a full-horizon refuge: %+v", facts)
	}
	if facts.BombDirectionKnownDangerReachable[DirectionNone][4] != 0 {
		t.Fatalf("waiting one production tick should miss the exact escape window: %+v", facts)
	}
	if facts.BombDirectionKnownDangerReachable[DirectionLeft][4] != 0 {
		t.Fatalf("leftward movement cannot escape the right-side refuge route: %+v", facts)
	}
	if !facts.BombDirectionWholeCellRefugeFound[DirectionRight] ||
		facts.BombDirectionEarliestRefuge[DirectionRight] == 0 {
		t.Fatalf("rightward candidate omitted its earliest full-horizon refuge: %+v", facts)
	}
	if facts.BombDirectionWholeCellRefugeFound[DirectionNone] ||
		facts.BombDirectionEarliestRefuge[DirectionNone] != 0 {
		t.Fatalf("waiting candidate incorrectly reported a full-horizon refuge: %+v", facts)
	}
}

func TestTacticalConsequencesRespectActorFootprintAtWallBoundary(t *testing.T) {
	config := testConfig()
	config.Grid = testOpenGrid(3, 2)
	config.Rules.TickMS = 20
	config.Rules.BombFuseMS = 500
	config.Participants[0].Spawn = Cell{Row: 1, Col: 0}
	config.Participants[0].SpeedPixelsPerSecond = 400
	config.Participants[0].BombPower = 1
	config.Participants[1].Spawn = Cell{Row: 1, Col: 2}
	// The actor centre is in the lower row, but its upper footprint corner is
	// still inside row zero. This wall blocks the exact rightward segment even
	// though the destination centre cell itself is open.
	config.Grid.Cells[0*int(config.Grid.Width)+1] = Tile{Kind: CellSolid}
	engine := mustEngine(t, config)
	engine.actors[0].Position.Y = CellSizePixels + 5
	facts := tacticalFactsForTest(t, engine, 1)
	if facts.BombEarliestWholeCellRefugeEstimate != 0 {
		t.Fatalf("whole-cell route crossed a wall touched by the actor footprint: %+v", facts)
	}
}

func TestTacticalMinimumSpeedIncludesStatusExpiry(t *testing.T) {
	config := testConfig()
	config.Rules.SpeedPixelsPerSecondByRate = nativeSpeedPixelsPerSecondByRate
	engine := mustEngine(t, config)
	actor := &engine.actors[0]
	actor.MovementStatus = MovementStatusFast
	actor.MovementStatusExpiresAt = engine.elapsedMS + 100
	if got := engine.tacticalMinimumSpeed(actor, DirectionRight, engine.elapsedMS+50); got != uint32(nativeSpeedPixelsPerSecondByRate[8]) {
		t.Fatalf("speed before fast expiry = %d, want %d", got, nativeSpeedPixelsPerSecondByRate[8])
	}
	if got := engine.tacticalMinimumSpeed(actor, DirectionRight, engine.elapsedMS+3_500); got != uint32(actor.SpeedPixelsPerSecond) {
		t.Fatalf("long-horizon speed ignored status expiry: got %d want base %d", got, actor.SpeedPixelsPerSecond)
	}
}

func TestTacticalStraightSegmentMatchesProductionPixelSweep(t *testing.T) {
	random := rand.New(rand.NewSource(0x51afe))
	config := testConfig()
	config.Grid = testOpenGrid(7, 5)
	engine := mustEngine(t, config)
	actor := engine.actors[0]
	directions := [...]Direction{DirectionUp, DirectionRight, DirectionDown, DirectionLeft}

	for sample := 0; sample < 10_000; sample++ {
		for index := range engine.grid.Cells {
			engine.grid.Cells[index] = Tile{Kind: CellOpen, FlamePassable: true}
			if random.Intn(5) == 0 {
				engine.grid.Cells[index] = Tile{Kind: CellSolid}
			}
		}
		engine.bombs = engine.bombs[:0]
		for bombIndex := 0; bombIndex < random.Intn(6); bombIndex++ {
			engine.bombs = append(engine.bombs, Bomb{
				ID: uint32(bombIndex + 1), OwnerID: 2,
				Cell: Cell{Row: int16(random.Intn(5)), Col: int16(random.Intn(7))},
			})
		}
		actor.Position = Position{
			X: int32(10 + random.Intn(7*int(CellSizePixels)-20)),
			Y: int32(10 + random.Intn(5*int(CellSizePixels)-20)),
		}
		actor.NativePassActive = random.Intn(5) == 0
		actor.TransformationSceneID = 0
		actor.AvatarRoleID = 0
		if random.Intn(8) == 0 {
			actor.TransformationSceneID = 104
			actor.AvatarRoleID = 41
		}
		direction := directions[random.Intn(len(directions))]
		distance := 1 + random.Intn(2*int(CellSizePixels)-1)
		exactProbe := actor
		fastProbe := actor
		exact := engine.movementSegmentWalkable(&exactProbe, actor.Position, direction, distance)
		fast := engine.tacticalStraightSegmentWalkable(&fastProbe, actor.Position, direction, distance)
		if exact != fast {
			t.Fatalf("sample %d direction=%d distance=%d actor=%+v bombs=%+v: boundary=%v pixel=%v", sample, direction, distance, actor, engine.bombs, fast, exact)
		}
	}
}

func tacticalFactsForTest(t *testing.T, engine *Engine, playerID uint16) TacticalConsequences {
	t.Helper()
	current, err := engine.DangerTimeline(3_500)
	if err != nil {
		t.Fatal(err)
	}
	facts, err := engine.TacticalConsequencesForPlayer(playerID, current)
	if err != nil {
		t.Fatal(err)
	}
	return facts
}
