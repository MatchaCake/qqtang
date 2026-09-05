package battleengine

import (
	"reflect"
	"testing"
)

func TestExplosionStopsAtSolidAndBreakableCellsAndChainsBombs(t *testing.T) {
	config := testConfig()
	config.Grid = testOpenGrid(7, 5)
	config.Grid.Cells[1*7+2] = Tile{Kind: CellSolid}
	config.Grid.Cells[2*7+4] = Tile{Kind: CellBreakable, Durability: 1}
	config.Participants[0].Spawn = Cell{Row: 2, Col: 2}
	config.Participants[1].Spawn = Cell{Row: 4, Col: 6}
	engine := mustEngine(t, config)
	// Install the fixture at the pre-movement explosion clock.
	engine.elapsedMS = 100
	engine.bombs = []Bomb{
		{ID: 1, OwnerID: 1, Cell: Cell{Row: 2, Col: 2}, Power: 3, ExplodeAtMS: 100},
		{ID: 2, OwnerID: 2, Cell: Cell{Row: 2, Col: 3}, Power: 1, ExplodeAtMS: 9_000},
	}
	engine.nextBombID = 3
	events, err := engine.Step(nil)
	if err != nil {
		t.Fatal(err)
	}
	var exploded []uint32
	triggeredBy := make(map[uint32]uint32)
	for _, event := range events {
		if event.Kind == EventBombExploded {
			exploded = append(exploded, event.BombID)
			triggeredBy[event.BombID] = event.TriggeredByBombID
		}
	}
	if !reflect.DeepEqual(exploded, []uint32{1, 2}) {
		t.Fatalf("exploded bombs = %v, want [1 2]", exploded)
	}
	if len(engine.bombs) != 0 {
		t.Fatalf("chain left bombs behind: %+v", engine.bombs)
	}
	if triggeredBy[1] != 0 || triggeredBy[2] != 1 {
		t.Fatalf("chain provenance = %v, want root 1 and 2 triggered by 1", triggeredBy)
	}
	if tile, _ := engine.grid.Cell(Cell{Row: 2, Col: 4}); tile.Kind != CellOpen {
		t.Fatalf("breakable cell survived as %d", tile.Kind)
	}
	if hasFlame(engine.flames, Cell{Row: 1, Col: 2}) {
		t.Fatal("flame crossed a solid cell")
	}
	if hasFlame(engine.flames, Cell{Row: 2, Col: 5}) {
		t.Fatal("flame crossed a breakable cell")
	}
}

func TestTrapRescueOpponentFinishAndTerminal(t *testing.T) {
	config := testConfig()
	config.Participants = []Participant{
		testParticipant(1, 1, ParticipantHuman, Cell{Row: 1, Col: 1}),
		testParticipant(2, 1, ParticipantHuman, Cell{Row: 1, Col: 2}),
		testParticipant(3, 2, ParticipantHuman, Cell{Row: 1, Col: 3}),
	}
	engine := mustEngine(t, config)
	engine.actors[0].State = ActorTrapped
	engine.actors[1].Position = engine.actors[0].Position
	events, err := engine.Step(nil)
	if err != nil {
		t.Fatal(err)
	}
	if engine.actors[0].State != ActorActive || !hasEvent(events, EventActorRescued, 1) {
		t.Fatalf("teammate did not rescue trapped actor: state=%d events=%+v", engine.actors[0].State, events)
	}

	engine.actors[0].State = ActorTrapped
	engine.actors[1].Position = PositionAtCellCenter(Cell{Row: 0, Col: 4})
	engine.actors[2].Position = engine.actors[0].Position
	events, err = engine.Step(nil)
	if err != nil {
		t.Fatal(err)
	}
	if engine.actors[0].State != ActorEliminated || !hasEvent(events, EventActorEliminated, 1) {
		t.Fatalf("opponent did not finish trapped actor: state=%d events=%+v", engine.actors[0].State, events)
	}
	if engine.Terminal().Ended {
		t.Fatal("team with a living teammate ended prematurely")
	}

	engine.actors[1].State = ActorEliminated
	events, err = engine.Step(nil)
	if err != nil {
		t.Fatal(err)
	}
	outcome := engine.Terminal()
	if !outcome.Ended || outcome.Draw || outcome.WinnerTeamID != 2 || !hasEvent(events, EventMatchEnded, 0) {
		t.Fatalf("terminal outcome = %+v events=%+v", outcome, events)
	}
}

func TestTrapTimeoutEliminatesActor(t *testing.T) {
	engine := mustEngine(t, testConfig())
	engine.actors[1].State = ActorTrapped
	engine.actors[1].TrappedBy = 1
	engine.actors[1].TrapExpiresAt = 100
	events, err := engine.Step(nil)
	if err != nil {
		t.Fatal(err)
	}
	if engine.actors[1].State != ActorEliminated || !engine.Terminal().Ended || engine.Terminal().WinnerTeamID != 1 {
		t.Fatalf("trap timeout state=%d outcome=%+v events=%+v", engine.actors[1].State, engine.Terminal(), events)
	}
}

func TestBombIdentitySurvivesTrapUntilElimination(t *testing.T) {
	engine := mustEngine(t, testConfig())
	actor := &engine.actors[0]
	engine.elapsedMS = 100
	engine.flames = []Flame{{
		Cell: actor.Position.Cell(), OwnerID: actor.PlayerID, BombID: 77,
		ImpactAtMS: engine.elapsedMS, ExpiresAtMS: engine.elapsedMS + 500,
	}}

	trapEvents := engine.applyFlameHazards()
	if actor.State != ActorTrapped || actor.TrappedByBombID != 77 {
		t.Fatalf("bomb trap state=%d bomb=%d", actor.State, actor.TrappedByBombID)
	}
	if len(trapEvents) != 1 || trapEvents[0].Kind != EventActorTrapped || trapEvents[0].BombID != 77 {
		t.Fatalf("bomb trap events = %+v", trapEvents)
	}

	actor.TrapExpiresAt = engine.elapsedMS
	deathEvents := engine.expireTraps()
	if len(deathEvents) == 0 || deathEvents[0].Kind != EventActorEliminated || deathEvents[0].BombID != 77 {
		t.Fatalf("bomb elimination events = %+v", deathEvents)
	}
	if actor.TrappedByBombID != 0 {
		t.Fatalf("eliminated actor retained bomb ID %d", actor.TrappedByBombID)
	}
}

func TestNativeTrappedDeathConfirmationUsesSharedTerminalPath(t *testing.T) {
	config := testConfig()
	config.Rules.TrapDurationMS = 0
	engine := mustEngine(t, config)
	engine.actors[1].State = ActorTrapped
	engine.actors[1].TrappedBy = 1

	if _, err := engine.Step(nil); err != nil {
		t.Fatal(err)
	}
	if engine.actors[1].State != ActorTrapped {
		t.Fatalf("zero-timeout trap changed state to %d before native confirmation", engine.actors[1].State)
	}
	events, err := engine.ConfirmTrappedDeath(2)
	if err != nil {
		t.Fatal(err)
	}
	if engine.actors[1].State != ActorEliminated || !hasEvent(events, EventActorEliminated, 2) {
		t.Fatalf("native confirmation state=%d events=%+v", engine.actors[1].State, events)
	}
	if outcome := engine.Terminal(); !outcome.Ended || outcome.Draw || outcome.WinnerTeamID != 1 {
		t.Fatalf("native confirmation outcome = %+v", outcome)
	}
	if len(events) != 2 || events[1].Kind != EventMatchEnded {
		t.Fatalf("native confirmation did not share terminal path: %+v", events)
	}
}

func TestNativeTrappedDeathConfirmationRejectsInvalidState(t *testing.T) {
	engine := mustEngine(t, testConfig())
	if _, err := engine.ConfirmTrappedDeath(99); err == nil {
		t.Fatal("unknown native trapped-death player was accepted")
	}
	if _, err := engine.ConfirmTrappedDeath(1); err == nil {
		t.Fatal("active native trapped-death player was accepted")
	}
}

func TestVirtualTrapTimeoutDoesNotInventHumanDeath(t *testing.T) {
	config := testConfig()
	config.Rules.TrapDurationMS = 0
	config.Rules.VirtualTrapDurationMS = 100
	config.Participants[1].Source = ParticipantVirtualAI
	engine := mustEngine(t, config)
	if _, changed, err := engine.ApplyVerifiedActorHit(1, Position{}, false); err != nil || !changed {
		t.Fatalf("trap human: changed=%v err=%v", changed, err)
	}
	if _, changed, err := engine.ApplyVerifiedActorHit(2, Position{}, false); err != nil || !changed {
		t.Fatalf("trap virtual actor: changed=%v err=%v", changed, err)
	}
	if _, err := engine.Step(nil); err != nil {
		t.Fatal(err)
	}
	actors := engine.Actors()
	if actors[0].State != ActorTrapped || actors[0].TrapExpiresAt != 0 {
		t.Fatalf("human mirror received a guessed death timer: %+v", actors[0])
	}
	if actors[1].State != ActorEliminated {
		t.Fatalf("virtual actor did not expire without an owning native client: %+v", actors[1])
	}
}

func TestVirtualContactRequestsNativeKillWithoutInventingHumanDeath(t *testing.T) {
	config := testConfig()
	config.Rules.NativeOutcomeAuthority = true
	config.Rules.TrapDurationMS = 0
	config.Participants[0].Source = ParticipantHuman
	config.Participants[1].Source = ParticipantVirtualAI
	engine := mustEngine(t, config)
	engine.actors[0].State = ActorTrapped
	engine.actors[1].Position = engine.actors[0].Position

	events, err := engine.Step(nil)
	if err != nil {
		t.Fatal(err)
	}
	if engine.actors[0].State != ActorTrapped || !hasEvent(events, EventActorEliminationRequested, 1) {
		t.Fatalf("virtual contact did not request native kill: state=%d events=%+v", engine.actors[0].State, events)
	}
	second, err := engine.Step(nil)
	if err != nil {
		t.Fatal(err)
	}
	if hasEvent(second, EventActorEliminationRequested, 1) {
		t.Fatalf("continuous native contact repeated request: %+v", second)
	}

	engine.actors[1].Position = PositionAtCellCenter(Cell{Row: 0, Col: 4})
	if _, err = engine.Step(nil); err != nil {
		t.Fatal(err)
	}
	engine.actors[1].Position = engine.actors[0].Position
	third, err := engine.Step(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !hasEvent(third, EventActorEliminationRequested, 1) {
		t.Fatalf("new native contact did not request kill: %+v", third)
	}
}

func TestRoundTimeoutIsDraw(t *testing.T) {
	config := testConfig()
	config.Rules.RoundDurationMS = config.Rules.TickMS
	engine := mustEngine(t, config)
	if _, err := engine.Step(nil); err != nil {
		t.Fatal(err)
	}
	outcome := engine.Terminal()
	if !outcome.Ended || !outcome.Draw || !outcome.TimedOut || outcome.EndedAtMS != config.Rules.TickMS {
		t.Fatalf("timeout outcome = %+v", outcome)
	}
}

func TestFlameTraversalIsIndependentFromActorCollision(t *testing.T) {
	config := testConfig()
	config.Grid = testOpenGrid(7, 3)
	// This scenery blocks actors but explicitly lets flame traverse.
	config.Grid.Cells[1*7+2] = Tile{Kind: CellSolid, FlamePassable: true}
	// This open cell permits actors but blocks flame traversal.
	config.Grid.Cells[1*7+4] = Tile{Kind: CellOpen}
	config.Participants[0].Spawn = Cell{Row: 1, Col: 1}
	config.Participants[1].Spawn = Cell{Row: 2, Col: 6}
	engine := mustEngine(t, config)
	// Install the fixture at the pre-movement explosion clock.
	engine.elapsedMS = 100
	engine.bombs = []Bomb{{ID: 1, OwnerID: 1, Cell: Cell{Row: 1, Col: 1}, Power: 5, ExplodeAtMS: 100}}
	engine.nextBombID = 2
	if _, err := engine.Step(nil); err != nil {
		t.Fatal(err)
	}
	if !hasFlame(engine.flames, Cell{Row: 1, Col: 2}) || !hasFlame(engine.flames, Cell{Row: 1, Col: 3}) {
		t.Fatalf("flame did not cross flame-passable solid scenery: %+v", engine.flames)
	}
	if hasFlame(engine.flames, Cell{Row: 1, Col: 4}) || hasFlame(engine.flames, Cell{Row: 1, Col: 5}) {
		t.Fatalf("flame crossed actor-passable flame barrier: %+v", engine.flames)
	}
}

func TestLaterExplosionRearmsAnExistingFlameCell(t *testing.T) {
	config := testConfig()
	config.Rules.FlameDurationMS = 500
	config.Participants[0].Spawn = Cell{Row: 1, Col: 1}
	config.Participants[1].Spawn = Cell{Row: 1, Col: 3}
	engine := mustEngine(t, config)
	engine.actors[1].Position = PositionAtCellCenter(Cell{Row: 1, Col: 2})
	engine.addFlame(Cell{Row: 1, Col: 2}, 1)
	firstImpact := engine.flames[0].ImpactAtMS
	engine.elapsedMS += 100
	engine.addFlame(Cell{Row: 1, Col: 2}, 1)
	if engine.flames[0].ImpactAtMS == firstImpact {
		t.Fatalf("later explosion did not re-arm flame: %+v", engine.flames[0])
	}
	events := engine.applyFlameHazards()
	if engine.actors[1].State != ActorTrapped || !hasEvent(events, EventActorTrapped, 2) {
		t.Fatalf("re-armed explosion did not hit actor: actor=%+v events=%+v", engine.actors[1], events)
	}
}

func TestNativeExplosionHitsCenterCellOnlyAtImpactInstant(t *testing.T) {
	config := testConfig()
	// Keep the non-subject participant outside the impact cell so this test
	// observes only the half-body boundary of player 2.
	config.Participants[0].Spawn = Cell{Row: 2, Col: 3}
	engine := mustEngine(t, config)
	actor := &engine.actors[1]
	flameCell := Cell{Row: 1, Col: 1}
	engine.elapsedMS = 1_000
	engine.flames = []Flame{{Cell: flameCell, OwnerID: 1, ImpactAtMS: engine.elapsedMS, ExpiresAtMS: 1_500}}

	// X=39 leaves the actor centre in col 0 while the native +/-19 movement
	// footprint visibly overlaps flame col 1. This is the source-backed 半身
	// boundary and must remain safe.
	actor.Position = Position{X: 39, Y: 60}
	if events := engine.applyFlameHazards(); actor.State != ActorActive || len(events) != 0 {
		t.Fatalf("half-body adjacent centre was hit: actor=%+v events=%+v", *actor, events)
	}

	// Visible flame lifetime does not repeatedly hurt. Entering after its one
	// native impact notification is safe even though the visual still exists.
	engine.elapsedMS = 1_100
	actor.Position = Position{X: 40, Y: 60}
	if events := engine.applyFlameHazards(); actor.State != ActorActive || len(events) != 0 {
		t.Fatalf("visible post-impact flame hit repeatedly: actor=%+v events=%+v", *actor, events)
	}

	engine.flames[0].ImpactAtMS = engine.elapsedMS
	if events := engine.applyFlameHazards(); actor.State != ActorTrapped || !hasEvent(events, EventActorTrapped, actor.PlayerID) {
		t.Fatalf("centre-cell impact missed actor: actor=%+v events=%+v", *actor, events)
	}
}

func TestNativeOutcomeAuthorityWaitsForVerifiedHitsForEveryParticipant(t *testing.T) {
	config := testConfig()
	config.Rules.NativeOutcomeAuthority = true
	config.Participants[1].Source = ParticipantVirtualAI
	engine := mustEngine(t, config)
	impactCell := Cell{Row: 1, Col: 1}
	engine.elapsedMS = 1_000
	engine.actors[0].Position = PositionAtCellCenter(impactCell)
	engine.actors[1].Position = PositionAtCellCenter(impactCell)
	engine.flames = []Flame{{Cell: impactCell, OwnerID: 9, ImpactAtMS: engine.elapsedMS, ExpiresAtMS: 1_500}}

	events := engine.applyFlameHazards()
	if engine.actors[0].State != ActorActive {
		t.Fatalf("externally authoritative human state = %d, want active", engine.actors[0].State)
	}
	if engine.actors[1].State != ActorActive || len(events) != 0 {
		t.Fatalf("predicted live flame changed virtual state: state=%d events=%+v", engine.actors[1].State, events)
	}
	engine.fieldObjects = []FieldObject{{ID: 1, ActionID: 41, OwnerID: engine.actors[0].PlayerID, Cell: impactCell}}
	if events = engine.resolveFieldObjectContacts(); !hasEvent(events, EventActorHitRequested, engine.actors[0].PlayerID) ||
		engine.actors[1].State != ActorActive || len(engine.fieldObjects) != 0 {
		t.Fatalf("engine omitted virtual field contact: events=%+v objects=%+v", events, engine.fieldObjects)
	}

	event, changed, err := engine.ApplyVerifiedActorHit(engine.actors[0].PlayerID, engine.actors[0].Position, false)
	if err != nil || !changed || event.Kind != EventActorTrapped || engine.actors[0].State != ActorTrapped {
		t.Fatalf("verified native human hit = event %+v changed %v err %v actor %+v", event, changed, err, engine.actors[0])
	}
}

func TestNativeOutcomeAuthorityWaitsForVerifiedHumanPickup(t *testing.T) {
	config := testConfig()
	config.Rules.NativeOutcomeAuthority = true
	config.Pickups = []Pickup{{SceneID: SceneBombCapacitySmall, Cell: Cell{Row: 1, Col: 1}, State: PickupAvailable}}
	engine, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	actor := &engine.actors[0]
	actor.Position = PositionAtCellCenter(config.Pickups[0].Cell)
	before := actor.BombCapacity
	actor.MaxBombCapacity = before + 2
	if events := engine.resolvePickupContacts(); len(events) != 0 || actor.BombCapacity != before || engine.pickups[0].State != PickupAvailable {
		t.Fatalf("predicted human pickup mutated native-authority state: events=%v capacity=%d state=%d", events, actor.BombCapacity, engine.pickups[0].State)
	}
	if _, err = engine.ApplyVerifiedPickupAt(actor.PlayerID, SceneBombCapacitySmall, actor.Position); err != nil {
		t.Fatal(err)
	}
	if actor.BombCapacity != before+1 || engine.pickups[0].State != PickupCollected {
		t.Fatalf("verified pickup capacity/state = %d/%d, want %d/%d", actor.BombCapacity, engine.pickups[0].State, before+1, PickupCollected)
	}
}

func TestNativeOutcomeAuthorityVirtualPickupIsIntentUntilVerified(t *testing.T) {
	config := testConfig()
	config.Rules.NativeOutcomeAuthority = true
	config.Participants[1].Source = ParticipantVirtualAI
	config.Pickups = []Pickup{{SceneID: SceneBombCapacitySmall, Cell: Cell{Row: 1, Col: 1}, State: PickupAvailable}}
	engine, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	actor := &engine.actors[1]
	actor.Position = PositionAtCellCenter(config.Pickups[0].Cell)
	before := actor.BombCapacity
	actor.MaxBombCapacity = before + 2
	events := engine.resolvePickupContacts()
	if len(events) != 1 || events[0].Kind != EventPickupCollectRequested || events[0].PlayerID != actor.PlayerID {
		t.Fatalf("virtual pickup intent events = %+v", events)
	}
	if actor.BombCapacity != before || engine.pickups[0].State != PickupAvailable {
		t.Fatalf("virtual intent mutated pickup before native confirmation: capacity=%d pickup=%+v", actor.BombCapacity, engine.pickups[0])
	}
	if _, err = engine.ApplyVerifiedPickupAt(actor.PlayerID, SceneBombCapacitySmall, actor.Position); err != nil {
		t.Fatal(err)
	}
	if actor.BombCapacity != before+1 || engine.pickups[0].State != PickupCollected {
		t.Fatalf("verified virtual pickup capacity/state = %d/%d, want %d/%d", actor.BombCapacity, engine.pickups[0].State, before+1, PickupCollected)
	}
}

func TestLiveVirtualPickupRequiresOneExistingSceneObject(t *testing.T) {
	config := testConfig()
	config.Rules.NativeOutcomeAuthority = true
	config.Participants[1].Source = ParticipantVirtualAI
	cell := Cell{Row: 1, Col: 1}
	config.Pickups = []Pickup{{SceneID: SceneBombCapacitySmall, Cell: cell, State: PickupAvailable}}
	engine, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	actor := &engine.actors[1]
	actor.Position = PositionAtCellCenter(cell)
	actor.MaxBombCapacity = actor.BombCapacity + 2

	if _, err = engine.ApplyExistingPickupAt(actor.PlayerID, SceneBombCapacitySmall, actor.Position); err != nil {
		t.Fatal(err)
	}
	afterFirst := actor.BombCapacity
	if _, err = engine.ApplyExistingPickupAt(actor.PlayerID, SceneBombCapacitySmall, actor.Position); err == nil {
		t.Fatal("duplicate live virtual pickup unexpectedly synthesized another scene object")
	}
	if actor.BombCapacity != afterFirst || engine.pickups[0].State != PickupCollected {
		t.Fatalf("duplicate pickup changed state: capacity=%d pickup=%+v", actor.BombCapacity, engine.pickups[0])
	}
}

func TestNativeOutcomeAuthorityWaitsForVerifiedExplosionAndReconcilesWorld(t *testing.T) {
	config := testConfig()
	config.Rules.NativeOutcomeAuthority = true
	wall := Cell{Row: 1, Col: 2}
	config.Grid.Cells[1*int(config.Grid.Width)+2] = Tile{
		Kind: CellBreakable, FlamePassable: true, Durability: 1, MapElementID: 10,
		PandaPushable: true, ElementWidth: 1, ElementHeight: 1, ElementAnchor: wall,
	}
	config.Pickups = []Pickup{{SceneID: SceneBombPowerSmall, Cell: wall, State: PickupHidden}}
	engine := mustEngine(t, config)
	placed, err := engine.ApplyVerifiedBombPlacementAt(1, Cell{Row: 1, Col: 1}, 2, 0, engine.elapsedMS)
	if err != nil {
		t.Fatal(err)
	}
	for step := uint32(0); step < config.Rules.BombFuseMS/config.Rules.TickMS+2; step++ {
		if _, err = engine.Step(nil); err != nil {
			t.Fatal(err)
		}
	}
	if len(engine.bombs) != 1 || engine.grid.Cells[1*int(config.Grid.Width)+2].Kind != CellBreakable || engine.pickups[0].State != PickupHidden {
		t.Fatalf("live mirror predicted native explosion: bombs=%+v wall=%+v pickup=%+v", engine.bombs, engine.grid.Cells[1*int(config.Grid.Width)+2], engine.pickups[0])
	}
	events, err := engine.ApplyVerifiedBombExplosion(
		[]VerifiedExplodedBomb{{BombID: placed.BombID, OwnerID: 1, Cell: Cell{Row: 1, Col: 1}, BlastRowMin: 1, BlastRowMax: 1, BlastColMin: 0, BlastColMax: 2}},
		[]VerifiedMapElementHit{{MapElementID: 10, Cell: wall}},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(engine.bombs) != 0 || engine.grid.Cells[1*int(config.Grid.Width)+2].Kind != CellOpen || engine.pickups[0].State != PickupAvailable {
		t.Fatalf("verified explosion did not reconcile: bombs=%+v wall=%+v pickup=%+v events=%+v", engine.bombs, engine.grid.Cells[1*int(config.Grid.Width)+2], engine.pickups[0], events)
	}
	if !hasFlame(engine.flames, Cell{Row: 1, Col: 0}) || !hasFlame(engine.flames, wall) {
		t.Fatalf("verified blast bounds did not create policy flames: %+v", engine.flames)
	}
}

func TestVerifiedExplosionDestroysVisibleItemAndRevealsSeededWallPickup(t *testing.T) {
	config := testConfig()
	config.Rules.NativeOutcomeAuthority = true
	wall := Cell{Row: 1, Col: 2}
	visibleCell := Cell{Row: 0, Col: 4}
	config.Grid.Cells[1*int(config.Grid.Width)+2] = Tile{
		Kind: CellBreakable, MapElementOccupied: true, Durability: 1, MapElementID: 10, PandaPushable: true,
		ElementWidth: 1, ElementHeight: 1, ElementAnchor: wall,
	}
	config.Pickups = []Pickup{
		{SceneID: SceneBombCapacitySmall, Cell: wall, State: PickupHidden},
		{SceneID: SceneBombPowerSmall, Cell: visibleCell, State: PickupAvailable},
	}
	engine := mustEngine(t, config)
	before, err := engine.Observation(1)
	if err != nil {
		t.Fatal(err)
	}
	for _, pickup := range before.Pickups {
		if pickup.Cell == wall {
			t.Fatalf("policy observed hidden wall pickup before native destruction: %+v", before.Pickups)
		}
	}
	events, err := engine.ApplyVerifiedBombExplosion(
		[]VerifiedExplodedBomb{{OwnerID: 1, Cell: Cell{Row: 1, Col: 1}, BlastRowMin: 1, BlastRowMax: 1, BlastColMin: 1, BlastColMax: 2}},
		[]VerifiedMapElementHit{{MapElementID: 10, Cell: wall}},
		[]Pickup{{SceneID: SceneBombPowerSmall, Cell: visibleCell, State: PickupAvailable}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if tile, _ := engine.grid.Cell(wall); tile.Kind != CellOpen {
		t.Fatalf("native destroyed wall remained %+v", tile)
	}
	wallRevealed, visibleDestroyed := false, false
	for _, pickup := range engine.pickups {
		wallRevealed = wallRevealed || (pickup.Cell == wall && pickup.SceneID == SceneBombCapacitySmall && pickup.State == PickupAvailable)
		visibleDestroyed = visibleDestroyed || (pickup.Cell == visibleCell && pickup.SceneID == SceneBombPowerSmall && pickup.State == PickupCollected)
	}
	if !wallRevealed {
		t.Fatalf("wall seed was not revealed: %+v", engine.pickups)
	}
	if !visibleDestroyed {
		t.Fatalf("0x0FA4 destroyed item survived: %+v", engine.pickups)
	}
	if !hasEvent(events, EventPickupRevealed, 0) {
		t.Fatalf("wall reveal event missing: %+v", events)
	}
	after, err := engine.Observation(1)
	if err != nil {
		t.Fatal(err)
	}
	policySeesWallPickup, policySeesDestroyedItem := false, false
	for _, pickup := range after.Pickups {
		policySeesWallPickup = policySeesWallPickup || (pickup.Cell == wall && pickup.SceneID == SceneBombCapacitySmall)
		policySeesDestroyedItem = policySeesDestroyedItem || pickup.Cell == visibleCell
	}
	if !policySeesWallPickup || policySeesDestroyedItem {
		t.Fatalf("policy pickup boundary after native destruction = %+v", after.Pickups)
	}
}

func TestVerifiedExplosionDoesNotRevealWhenMirrorGeometryIdentityMismatches(t *testing.T) {
	config := testConfig()
	config.Rules.NativeOutcomeAuthority = true
	cell := Cell{Row: 1, Col: 2}
	config.Grid.Cells[1*int(config.Grid.Width)+2] = Tile{
		Kind: CellBreakable, MapElementOccupied: true, Durability: 1, MapElementID: 11, PandaPushable: true,
		ElementWidth: 1, ElementHeight: 1, ElementAnchor: cell,
	}
	config.Pickups = []Pickup{{SceneID: SceneBombCapacitySmall, Cell: cell, State: PickupHidden}}
	engine := mustEngine(t, config)
	_, err := engine.ApplyVerifiedBombExplosion(
		[]VerifiedExplodedBomb{{OwnerID: 1, Cell: Cell{Row: 1, Col: 1}, BlastRowMin: 1, BlastRowMax: 1, BlastColMin: 1, BlastColMax: 2}},
		[]VerifiedMapElementHit{{MapElementID: 10, Cell: cell}}, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if engine.pickups[0].State != PickupHidden {
		t.Fatalf("mismatched map identity exposed hidden seed: %+v", engine.pickups[0])
	}
}

func TestNativeOutcomeAuthorityWaitsForArbitratorForEveryDueBomb(t *testing.T) {
	config := testConfig()
	config.Grid = testOpenGrid(7, 3)
	config.Rules.NativeOutcomeAuthority = true
	config.Participants[0].Spawn = Cell{Row: 1, Col: 1}
	config.Participants[1].Spawn = Cell{Row: 1, Col: 5}
	config.Participants[1].Source = ParticipantVirtualAI
	engine := mustEngine(t, config)
	engine.bombs = []Bomb{
		{ID: 1, OwnerID: 1, Cell: Cell{Row: 1, Col: 1}, Power: 1, ExplodeAtMS: 100},
		{ID: 2, OwnerID: 2, Cell: Cell{Row: 1, Col: 5}, Power: 1, ExplodeAtMS: 100},
	}
	engine.nextBombID = 3

	events, err := engine.Step(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(engine.bombs) != 2 {
		t.Fatalf("native-authority bombs after due tick = %+v, want both pending", engine.bombs)
	}
	var exploded []uint32
	for _, event := range events {
		if event.Kind == EventBombExploded {
			exploded = append(exploded, event.BombID)
		}
	}
	if len(exploded) != 0 {
		t.Fatalf("locally authored exploded bombs = %v, want none", exploded)
	}
	if engine.actors[0].State != ActorActive {
		t.Fatalf("unverified human state changed to %d", engine.actors[0].State)
	}
	if engine.actors[1].State != ActorActive || hasEvent(events, EventActorTrapped, 2) {
		t.Fatalf("virtual owner changed before arbitrator confirmation: actor=%+v events=%+v", engine.actors[1], events)
	}
}

func TestNativeOutcomeAuthorityWaitsForArbitratorToDescribeChain(t *testing.T) {
	config := testConfig()
	config.Grid = testOpenGrid(7, 3)
	config.Rules.NativeOutcomeAuthority = true
	config.Participants[0].Spawn = Cell{Row: 2, Col: 1}
	config.Participants[1].Spawn = Cell{Row: 2, Col: 5}
	config.Participants[1].Source = ParticipantVirtualAI
	engine := mustEngine(t, config)
	engine.bombs = []Bomb{
		{ID: 1, OwnerID: 2, Cell: Cell{Row: 1, Col: 2}, Power: 2, ExplodeAtMS: 100},
		{ID: 2, OwnerID: 1, Cell: Cell{Row: 1, Col: 3}, Power: 1, ExplodeAtMS: 9_000},
	}
	engine.nextBombID = 3

	events, err := engine.Step(nil)
	if err != nil {
		t.Fatal(err)
	}
	var exploded []uint32
	for _, event := range events {
		if event.Kind == EventBombExploded {
			exploded = append(exploded, event.BombID)
		}
	}
	if len(exploded) != 0 || len(engine.bombs) != 2 {
		t.Fatalf("unverified native chain = %v bombs=%+v, want no events and both bombs pending", exploded, engine.bombs)
	}
}

func TestVerifiedNativeExplosionDoesNotCommitActorHits(t *testing.T) {
	config := testConfig()
	config.Rules.NativeOutcomeAuthority = true
	config.Participants[0].Spawn = Cell{Row: 1, Col: 1}
	config.Participants[1].Spawn = Cell{Row: 1, Col: 2}
	config.Participants[1].Source = ParticipantVirtualAI
	engine := mustEngine(t, config)
	placed, err := engine.ApplyVerifiedBombPlacementAt(1, Cell{Row: 1, Col: 0}, 2, 0, engine.elapsedMS)
	if err != nil {
		t.Fatal(err)
	}
	events, err := engine.ApplyVerifiedBombExplosion([]VerifiedExplodedBomb{{
		BombID: placed.BombID, OwnerID: 1, Cell: Cell{Row: 1, Col: 0},
		BlastRowMin: 1, BlastRowMax: 1, BlastColMin: 0, BlastColMax: 2,
	}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if engine.actors[0].State != ActorActive {
		t.Fatalf("verified explosion invented a human hit: %+v", engine.actors[0])
	}
	if engine.actors[1].State != ActorActive || hasEvent(events, EventActorTrapped, 2) || hasEvent(events, EventActorHitRequested, 2) {
		t.Fatalf("verified explosion committed actor state: actor=%+v events=%+v", engine.actors[1], events)
	}
}

func TestVirtualBlastHitRequestWaitsForArbitratorAndPreservesOwner(t *testing.T) {
	config := testConfig()
	config.Rules.NativeOutcomeAuthority = true
	config.Participants[1].Source = ParticipantVirtualAI
	engine := mustEngine(t, config)
	past := PositionAtCellCenter(Cell{Row: 1, Col: 1})
	engine.actors[1].Position = PositionAtCellCenter(Cell{Row: 1, Col: 3})

	event, changed, err := engine.BuildVirtualBlastHitRequest(2, 1, past)
	if err != nil || !changed {
		t.Fatalf("virtual hit request = %+v/%v/%v", event, changed, err)
	}
	actor := engine.actors[1]
	if actor.State != ActorActive || actor.Position == past {
		t.Fatalf("request mutated virtual actor = %+v", actor)
	}
	if event.Kind != EventActorHitRequested || event.PlayerID != 2 || event.TargetID != 1 || event.Position != past {
		t.Fatalf("virtual hit request event = %+v", event)
	}
	if _, changed, err = engine.ApplyVerifiedActorHit(2, past, false); err != nil || !changed || engine.actors[1].State != ActorTrapped {
		t.Fatalf("arbitrator hit did not commit: actor=%+v changed=%v err=%v", engine.actors[1], changed, err)
	}
}

func TestVerifiedNativeExplosionBreaksVirtualTransformation(t *testing.T) {
	config := testConfig()
	config.Rules.NativeOutcomeAuthority = true
	config.Participants[1].Spawn = Cell{Row: 1, Col: 2}
	config.Participants[1].Source = ParticipantVirtualAI
	engine := mustEngine(t, config)
	engine.actors[1].TransformationSceneID = 101
	engine.actors[1].AvatarRoleID = 43
	placed, err := engine.ApplyVerifiedBombPlacementAt(1, Cell{Row: 1, Col: 1}, 1, 0, engine.elapsedMS)
	if err != nil {
		t.Fatal(err)
	}
	events, err := engine.ApplyVerifiedBombExplosion([]VerifiedExplodedBomb{{
		BombID: placed.BombID, OwnerID: 1, Cell: Cell{Row: 1, Col: 1},
		BlastRowMin: 1, BlastRowMax: 1, BlastColMin: 1, BlastColMax: 2,
	}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if engine.actors[1].State != ActorActive || engine.actors[1].TransformationSceneID != 101 || len(events) != 1 {
		t.Fatalf("explosion committed virtual transformation hit: actor=%+v events=%+v", engine.actors[1], events)
	}
	request, requested, err := engine.BuildVirtualBlastHitRequest(2, 1, engine.actors[1].Position)
	if err != nil || !requested || request.Kind != EventActorHitRequested || request.SceneID != 101 {
		t.Fatalf("virtual avatar hit request = %+v/%v/%v", request, requested, err)
	}
	event, changed, err := engine.ApplyVerifiedActorHit(2, engine.actors[1].Position, true)
	if err != nil || !changed || event.Kind != EventActorTransformationEnded || event.TransformationEnd != TransformationEndHit || engine.actors[1].TransformationSceneID != 0 {
		t.Fatalf("confirmed virtual transformation hit = %+v/%v/%v actor=%+v", event, changed, err, engine.actors[1])
	}
}

func TestBreakableDurabilityConsumesOneHitAtATime(t *testing.T) {
	config := testConfig()
	config.Grid = testOpenGrid(5, 3)
	config.Grid.Cells[1*5+2] = Tile{Kind: CellBreakable, Durability: 2}
	config.Participants[0].Spawn = Cell{Row: 1, Col: 1}
	config.Participants[1].Spawn = Cell{Row: 2, Col: 4}
	engine := mustEngine(t, config)
	// Install the fixture at the pre-movement explosion clock.
	engine.elapsedMS = 100
	engine.bombs = []Bomb{{ID: 1, OwnerID: 1, Cell: Cell{Row: 1, Col: 1}, Power: 2, ExplodeAtMS: 100}}
	engine.nextBombID = 2
	events, err := engine.Step(nil)
	if err != nil {
		t.Fatal(err)
	}
	tile, _ := engine.grid.Cell(Cell{Row: 1, Col: 2})
	if tile.Kind != CellBreakable || tile.Durability != 1 || !hasEvent(events, EventCellDamaged, 0) || hasEvent(events, EventCellDestroyed, 0) {
		t.Fatalf("first wall hit tile=%+v events=%+v", tile, events)
	}
	engine.bombs = []Bomb{{ID: 2, OwnerID: 1, Cell: Cell{Row: 1, Col: 1}, Power: 2, ExplodeAtMS: 200}}
	engine.nextBombID = 3
	events, err = engine.Step(nil)
	if err != nil {
		t.Fatal(err)
	}
	tile, _ = engine.grid.Cell(Cell{Row: 1, Col: 2})
	if tile.Kind != CellOpen || tile.Durability != 0 || !hasEvent(events, EventCellDestroyed, 0) {
		t.Fatalf("second wall hit tile=%+v events=%+v", tile, events)
	}
}

func TestNativeBubbleBoundaryLetsActorsLeaveButBlocksReentry(t *testing.T) {
	config := testConfig()
	config.Rules.ActorHalfSizePixels = NativeActorHalfSizePixels
	config.Rules.BombFuseMS = 10_000
	config.Participants[0].Spawn = Cell{Row: 1, Col: 1}
	engine := mustEngine(t, config)
	engine.bombs = []Bomb{{ID: 1, OwnerID: 1, Cell: Cell{Row: 1, Col: 1}, Power: 1, ExplodeAtMS: 10_000}}

	actor := &engine.actors[0]
	for _, direction := range []Direction{DirectionUp, DirectionRight, DirectionDown, DirectionLeft} {
		dx, dy, _ := direction.delta()
		candidate := Position{X: actor.Position.X + dx, Y: actor.Position.Y + dy}
		if !engine.positionWalkable(actor, candidate, direction) {
			t.Fatalf("new bubble blocked actor from leaving toward %d", direction)
		}
	}

	checks := []struct {
		position  Position
		direction Direction
	}{
		{Position{X: 20, Y: 60}, DirectionRight},
		{Position{X: 99, Y: 60}, DirectionLeft},
		{Position{X: 60, Y: 20}, DirectionDown},
		{Position{X: 60, Y: 99}, DirectionUp},
	}
	for _, check := range checks {
		dx, dy, _ := check.direction.delta()
		actor.Position = check.position
		candidate := Position{X: actor.Position.X + dx, Y: actor.Position.Y + dy}
		if engine.positionWalkable(actor, candidate, check.direction) {
			t.Fatalf("bubble allowed reentry from direction %d at %+v", check.direction, candidate)
		}
	}
}

func TestNativeDynamicBoundaryIsExactlyThreePixels(t *testing.T) {
	tests := []struct {
		point     Position
		direction Direction
		blocked   bool
	}{
		{Position{X: 40}, DirectionRight, true},
		{Position{X: 42}, DirectionRight, true},
		{Position{X: 43}, DirectionRight, false},
		{Position{X: 79}, DirectionLeft, true},
		{Position{X: 77}, DirectionLeft, true},
		{Position{X: 76}, DirectionLeft, false},
		{Position{Y: 40}, DirectionDown, true},
		{Position{Y: 43}, DirectionDown, false},
		{Position{Y: 79}, DirectionUp, true},
		{Position{Y: 76}, DirectionUp, false},
	}
	for _, test := range tests {
		if got := nativeDynamicBoundaryBlocks(test.point, test.direction); got != test.blocked {
			t.Fatalf("boundary point=%+v direction=%d blocked=%t, want %t", test.point, test.direction, got, test.blocked)
		}
	}
}

func TestNativeSimultaneousPlacementCanStackButLaterPlacementCannot(t *testing.T) {
	config := testConfig()
	config.Rules.BombFuseMS = 10_000
	config.Participants[0].Spawn = Cell{Row: 1, Col: 1}
	config.Participants[1].Spawn = Cell{Row: 1, Col: 1}
	engine := mustEngine(t, config)
	events, err := engine.Step([]Action{{PlayerID: 1, PlaceBomb: true}, {PlayerID: 2, PlaceBomb: true}})
	if err != nil {
		t.Fatal(err)
	}
	if len(engine.bombs) != 2 || engine.bombs[0].Cell != engine.bombs[1].Cell {
		t.Fatalf("simultaneous native placement did not stack: bombs=%+v events=%+v", engine.bombs, events)
	}
	if _, err := engine.Step([]Action{{PlayerID: 1, PlaceBomb: true}}); err != nil {
		t.Fatal(err)
	}
	if len(engine.bombs) != 2 {
		t.Fatalf("later placement entered an occupied native cell: %+v", engine.bombs)
	}
}

func TestNativeSimultaneousPlacementSupportsAllEightParticipants(t *testing.T) {
	config := testConfig()
	config.Grid = testOpenGrid(5, 5)
	config.Participants = make([]Participant, MaxParticipants)
	actions := make([]Action, MaxParticipants)
	for index := 0; index < MaxParticipants; index++ {
		playerID := uint16(index + 1)
		config.Participants[index] = testParticipant(playerID, byte(index%2+1), ParticipantHuman, Cell{Row: 2, Col: 2})
		config.Participants[index].BombCapacity = 1
		actions[index] = Action{PlayerID: playerID, PlaceBomb: true}
	}
	engine := mustEngine(t, config)
	events, err := engine.Step(actions)
	if err != nil {
		t.Fatal(err)
	}
	if len(engine.bombs) != MaxParticipants {
		t.Fatalf("simultaneous eight-player stack has %d bombs, want %d: events=%+v", len(engine.bombs), MaxParticipants, events)
	}
	for _, bomb := range engine.bombs {
		if bomb.Cell != (Cell{Row: 2, Col: 2}) {
			t.Fatalf("stacked bomb left shared cell: %+v", bomb)
		}
	}
}

func TestNativePassChargeUsesStrictSourceWindowAndOneCellDuration(t *testing.T) {
	engine := mustEngine(t, testConfig())
	actor := &engine.actors[0]
	actor.Facing = DirectionRight
	engine.elapsedMS = 100
	engine.recordNativePassCollision(actor, Cell{Row: 1, Col: 2})
	engine.elapsedMS = 600
	engine.recordNativePassCollision(actor, Cell{Row: 1, Col: 2})
	engine.activateNativePassState(actor)
	if actor.NativePassActive {
		t.Fatal("native pass activated at the excluded 500 ms boundary")
	}
	engine.elapsedMS = 601
	engine.activateNativePassState(actor)
	if !actor.NativePassActive {
		t.Fatal("native pass did not activate inside the strict 500..600 ms window")
	}
	if actor.NativePassStartedAt != engine.elapsedMS {
		t.Fatalf("native pass started at %d, want immediate native clock %d", actor.NativePassStartedAt, engine.elapsedMS)
	}
	wantDuration := uint32(CellSizePixels) * 1000 / uint32(engine.effectiveSpeedPixelsPerSecond(actor, DirectionRight))
	if actor.NativePassDurationMS != wantDuration {
		t.Fatalf("native pass duration=%d, want one-cell duration %d", actor.NativePassDurationMS, wantDuration)
	}
	engine.elapsedMS = actor.NativePassStartedAt + actor.NativePassDurationMS - 1
	engine.expireNativePassState(actor)
	if !actor.NativePassActive {
		t.Fatal("native pass expired before one cell duration")
	}
	engine.elapsedMS++
	engine.expireNativePassState(actor)
	if actor.NativePassActive || !actor.NativePassCollisionValid || actor.NativePassCollisionStartedAt != 0 ||
		actor.NativePassCollisionCell != (Cell{Row: 1, Col: 2}) || actor.NativePassCollisionLastAt != 600 ||
		actor.NativePassStartedAt != 601 || actor.NativePassDurationMS != wantDuration {
		t.Fatalf("native pass exact partial expiry mismatch: %+v", *actor)
	}
	// Touching the same remembered object updates freshness but cannot restart
	// its cleared charge window. Moving to another object starts a new window.
	engine.elapsedMS++
	engine.recordNativePassCollision(actor, Cell{Row: 1, Col: 2})
	if actor.NativePassCollisionStartedAt != 0 || actor.NativePassCollisionLastAt != engine.elapsedMS {
		t.Fatalf("same-cell post-expiry touch restarted charge: %+v", *actor)
	}
	engine.recordNativePassCollision(actor, Cell{Row: 1, Col: 3})
	if actor.NativePassCollisionStartedAt != engine.elapsedMS || actor.NativePassCollisionCell != (Cell{Row: 1, Col: 3}) {
		t.Fatalf("new-cell post-expiry touch did not restart charge: %+v", *actor)
	}

	actor = &engine.actors[0]
	*actor = Actor{Participant: actor.Participant, State: ActorActive, Facing: DirectionRight}
	engine.elapsedMS = 100
	engine.recordNativePassCollision(actor, Cell{Row: 1, Col: 2})
	engine.elapsedMS = 700
	engine.recordNativePassCollision(actor, Cell{Row: 1, Col: 2})
	engine.activateNativePassState(actor)
	if actor.NativePassActive {
		t.Fatal("native pass activated at the excluded 600 ms boundary")
	}

	*actor = Actor{Participant: actor.Participant, State: ActorActive, Facing: DirectionRight}
	actor.NativePassCollisionValid = true
	actor.NativePassCollisionCell = Cell{Row: 1, Col: 2}
	actor.NativePassCollisionStartedAt = 100
	actor.NativePassCollisionLastAt = 501
	engine.elapsedMS = 601
	engine.activateNativePassState(actor)
	if actor.NativePassActive {
		t.Fatal("native pass accepted a collision that was exactly 100 ms stale")
	}
}

func TestNativePassAlignmentResetsWhenLeadingCornersHitDifferentCells(t *testing.T) {
	engine := mustEngine(t, testConfig())
	actor := &engine.actors[0]
	for now := uint32(100); now <= 700; now += 10 {
		engine.elapsedMS = now
		engine.recordNativePassCollision(actor, Cell{Row: 0, Col: 2})
		engine.recordNativePassCollision(actor, Cell{Row: 1, Col: 2})
		engine.activateNativePassState(actor)
	}
	if actor.NativePassActive {
		t.Fatal("unaligned alternating collision cells charged native traversal")
	}
	if actor.NativePassCollisionStartedAt != 700 {
		t.Fatalf("collision start=%d, want latest alternating reset 700", actor.NativePassCollisionStartedAt)
	}
}

func TestNativePassEdgeContactRecordsBothLeadingCellsAndCannotCharge(t *testing.T) {
	config := testConfig()
	config.Rules.TickMS = 10
	config.Rules.RoundDurationMS = 10_000
	config.Rules.ActorHalfSizePixels = NativeActorHalfSizePixels
	config.Grid = testOpenGrid(5, 4)
	config.Participants[0].Spawn = Cell{Row: 1, Col: 0}
	config.Participants[1].Spawn = Cell{Row: 3, Col: 4}
	engine := mustEngine(t, config)
	actor := &engine.actors[0]
	// At Y=40 the right leading corners straddle rows 0 and 1. Only the
	// lower corner meets the bubble, but FUN_005b7999 still touches both
	// leading cells in native order on every rejected update.
	actor.Position = Position{X: 20, Y: 40}
	engine.bombs = []Bomb{{ID: 1, Cell: Cell{Row: 1, Col: 1}, ExplodeAtMS: 10_000}}
	for step := 0; step < 80; step++ {
		engine.elapsedMS = uint32(step) * config.Rules.TickMS
		if engine.positionWalkable(actor, Position{X: 21, Y: 40}, DirectionRight) {
			t.Fatal("one-corner bubble contact unexpectedly produced movement")
		}
		engine.activateNativePassState(actor)
		if actor.NativePassActive {
			t.Fatalf("unaligned one-corner bubble contact charged native pass at step %d: %+v", step, *actor)
		}
	}
	if actor.NativePassCollisionCell != (Cell{Row: 0, Col: 1}) ||
		actor.NativePassCollisionStartedAt != engine.elapsedMS {
		t.Fatalf("two-cell native touch order/state mismatch: %+v", *actor)
	}
}

func TestFastMovementCannotSkipDynamicBubbleEntryBoundary(t *testing.T) {
	config := testConfig()
	config.Rules.TickMS = 20
	config.Rules.RoundDurationMS = 10_000
	config.Rules.ActorHalfSizePixels = NativeActorHalfSizePixels
	config.Rules.SpeedPixelsPerSecondByRate = nativeSpeedPixelsPerSecondByRate
	config.Grid = testOpenGrid(5, 3)
	config.Participants[0].Spawn = Cell{Row: 1, Col: 0}
	config.Participants[0].SpeedRate = 10
	config.Participants[0].SpeedPixelsPerSecond = nativeSpeedPixelsPerSecondByRate[10]
	config.Participants[1].Spawn = Cell{Row: 2, Col: 4}
	engine := mustEngine(t, config)
	actor := &engine.actors[0]
	engine.bombs = []Bomb{{ID: 1, Cell: Cell{Row: 1, Col: 1}, ExplodeAtMS: 10_000}}

	// At rate 10 one 20 ms update requests ten pixels. The actor's leading
	// edge starts at X=39 and the final point is already nine pixels inside the
	// bubble cell, beyond the native three-pixel entry boundary. The swept
	// query must still observe X=40 and reject the entire native update.
	start := actor.Position
	if _, err := engine.Step([]Action{{PlayerID: actor.PlayerID, Move: DirectionRight}}); err != nil {
		t.Fatal(err)
	}
	if actor.Position != start || actor.NativePassActive || !actor.NativePassCollisionValid ||
		actor.NativePassCollisionCell != (Cell{Row: 1, Col: 1}) {
		t.Fatalf("fast actor skipped bubble entry boundary: start=%+v actor=%+v", start, *actor)
	}

	projection, err := engine.ProjectNativeMovement(actor.PlayerID, DirectionRight)
	if err != nil {
		t.Fatal(err)
	}
	if projection.End != start {
		t.Fatalf("native movement projection skipped bubble boundary: end=%+v want=%+v", projection.End, start)
	}
}

func TestNativePassCrossesOnlyOneTypeOneCellWithOpenExit(t *testing.T) {
	config := testConfig()
	config.Rules.ActorHalfSizePixels = NativeActorHalfSizePixels
	config.Grid = testOpenGrid(5, 3)
	config.Participants[0].Spawn = Cell{Row: 1, Col: 0}
	config.Participants[1].Spawn = Cell{Row: 2, Col: 4}
	engine := mustEngine(t, config)
	actor := &engine.actors[0]
	actor.NativePassActive = true
	actor.NativePassDurationMS = 1_000
	engine.bombs = []Bomb{{ID: 1, Cell: Cell{Row: 1, Col: 1}, ExplodeAtMS: 10_000}}
	if !engine.positionWalkable(actor, Position{X: 21, Y: 60}, DirectionRight) {
		t.Fatal("active native pass did not enter one dynamic type-1 cell with an open exit")
	}
	config.Grid.Cells[1*5+1] = Tile{Kind: CellSolid}
	config.Grid.Cells[1*5+2] = Tile{Kind: CellSolid}
	blocked := mustEngine(t, config)
	blockedActor := &blocked.actors[0]
	blockedActor.NativePassActive = true
	blockedActor.NativePassDurationMS = 1_000
	if blocked.positionWalkable(blockedActor, Position{X: 21, Y: 60}, DirectionRight) {
		t.Fatal("active native pass crossed a two-cell solid barrier")
	}

	bombConfig := testConfig()
	bombConfig.Rules.ActorHalfSizePixels = NativeActorHalfSizePixels
	bombBlocked := mustEngine(t, bombConfig)
	bombActor := &bombBlocked.actors[0]
	bombActor.NativePassActive = true
	bombActor.NativePassDurationMS = 1_000
	bombBlocked.bombs = []Bomb{
		{ID: 1, Cell: Cell{Row: 1, Col: 1}, ExplodeAtMS: 10_000},
		{ID: 2, Cell: Cell{Row: 1, Col: 2}, ExplodeAtMS: 10_000},
	}
	bombActor.Position = Position{X: 20, Y: 60}
	if bombBlocked.positionWalkable(bombActor, Position{X: 21, Y: 60}, DirectionRight) {
		t.Fatal("active native pass crossed two adjacent dynamic bubbles")
	}
}

func TestNativePassWindowUsesImmediateSourceDuration(t *testing.T) {
	config := testConfig()
	config.Rules.TickMS = 10
	config.Rules.RoundDurationMS = 10_000
	config.Rules.ActorHalfSizePixels = NativeActorHalfSizePixels
	config.Grid = testOpenGrid(5, 3)
	config.Participants[0].Spawn = Cell{Row: 1, Col: 0}
	config.Participants[1].Spawn = Cell{Row: 2, Col: 4}
	engine := mustEngine(t, config)
	actor := &engine.actors[0]
	engine.bombs = []Bomb{{ID: 1, Cell: Cell{Row: 1, Col: 1}, ExplodeAtMS: 10_000}}
	if !containsAction(mustLegalActions(t, engine, 1), Action{PlayerID: 1, Move: DirectionRight}) {
		t.Fatal("native pass charge direction is absent from legal actions")
	}
	// Native uses zero as the uncharged sentinel; enter the collision after the
	// scene clock has begun, as a real match always does.
	if _, err := engine.Step(nil); err != nil {
		t.Fatal(err)
	}

	// Continuous collision only charges the native state. It must not activate
	// until the actor successfully places its own bubble during the strict
	// source window (FUN_005b08e0 -> actor slot +0x1c -> FUN_005d33ab).
	for step := 0; step < 52; step++ {
		if _, err := engine.Step([]Action{{PlayerID: 1, Move: DirectionRight}}); err != nil {
			t.Fatal(err)
		}
		if actor.NativePassActive {
			t.Fatalf("continuous collision activated native pass without placing a bubble at step %d", step)
		}
	}
	events, err := engine.Step([]Action{{PlayerID: 1, Move: DirectionRight, PlaceBomb: true}})
	if err != nil {
		t.Fatal(err)
	}
	if !actor.NativePassActive {
		t.Fatalf("charged native pass did not activate after successful local bubble placement: actor=%+v bombs=%+v events=%+v", *actor, engine.bombs, events)
	}
	if len(engine.bombs) != 2 || engine.bombs[1].OwnerID != actor.PlayerID ||
		engine.bombs[1].Cell != (Cell{Row: 1, Col: 0}) {
		t.Fatalf("activation bubble mismatch: %+v", engine.bombs)
	}
	started := false
	for _, event := range events {
		if event.Kind == EventNativePassStarted && event.PlayerID == actor.PlayerID {
			started = true
		}
	}
	if !started {
		t.Fatalf("successful activation omitted native pass event: %+v", events)
	}
	activatedAtX := actor.Position.X
	for step := 0; step < 80 && actor.NativePassActive; step++ {
		if _, err := engine.Step([]Action{{PlayerID: 1, Move: DirectionRight}}); err != nil {
			t.Fatal(err)
		}
	}
	if actor.NativePassActive {
		t.Fatal("native pass remained active beyond its source duration")
	}
	if actor.Position.X <= activatedAtX {
		t.Fatalf("actor did not advance during the active native pass window: %+v", *actor)
	}
	// Expiry resets DurationMS, so use the source one-cell bound directly.
	maxDistance := int32(CellSizePixels)
	if actor.Position.X-activatedAtX > maxDistance {
		t.Fatalf("actor advanced %d pixels during a %d-pixel native pass", actor.Position.X-activatedAtX, maxDistance)
	}
}

func TestNativePassActivationRequiresSuccessfulLocalPlacement(t *testing.T) {
	config := testConfig()
	config.Rules.RoundDurationMS = 10_000
	engine := mustEngine(t, config)
	actor := &engine.actors[0]
	actor.Facing = DirectionRight
	actor.NativePassCollisionValid = true
	actor.NativePassCollisionCell = Cell{Row: 1, Col: 2}
	actor.NativePassCollisionStartedAt = 100
	actor.NativePassCollisionLastAt = 600
	engine.elapsedMS = 601
	cell := actor.Position.Cell()
	tile := engine.grid.Cells[engine.gridIndex(cell)]
	tile.MapElementOccupied = true
	engine.grid.Cells[engine.gridIndex(cell)] = tile

	events, err := engine.Step([]Action{{PlayerID: actor.PlayerID, PlaceBomb: true}})
	if err != nil {
		t.Fatal(err)
	}
	if actor.NativePassActive || len(engine.bombs) != 0 {
		t.Fatalf("rejected local placement activated pass: actor=%+v bombs=%+v", *actor, engine.bombs)
	}
	for _, event := range events {
		if event.Kind == EventNativePassStarted {
			t.Fatalf("rejected local placement emitted pass event: %+v", events)
		}
	}
}

func TestSameTickTrapExpiryUsesLiveSequentialSettlement(t *testing.T) {
	engine := mustEngine(t, testConfig())
	for index := range engine.actors {
		engine.actors[index].State = ActorTrapped
		engine.actors[index].TrapExpiresAt = engine.rules.TickMS
	}
	if _, err := engine.Step(nil); err != nil {
		t.Fatal(err)
	}
	if outcome := engine.Terminal(); !outcome.Ended || outcome.Draw || outcome.WinnerTeamID != 2 {
		t.Fatalf("same-tick sequential outcome = %+v", outcome)
	}
	if engine.actors[0].State != ActorEliminated || engine.actors[1].State == ActorEliminated {
		t.Fatalf("settlement did not stop after the first decisive confirmation: %+v", engine.actors)
	}
}

func hasFlame(flames []Flame, cell Cell) bool {
	for _, flame := range flames {
		if flame.Cell == cell {
			return true
		}
	}
	return false
}

func hasEvent(events []Event, kind EventKind, targetID uint16) bool {
	for _, event := range events {
		if event.Kind == kind && (targetID == 0 || event.TargetID == targetID) {
			return true
		}
	}
	return false
}
