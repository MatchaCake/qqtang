package battleengine

import (
	"reflect"
	"strings"
	"testing"

	"qqtang/internal/clientdata/sceneelement"
	"qqtang/internal/game/mapdata"
)

func TestNativeMapRNGMatchesClientSequence(t *testing.T) {
	rng := nativeMapRNG{state: 0x12345678}
	want := []uint32{924655133, 424129417, 154869316, 1003447247}
	got := make([]uint32, len(want))
	for index := range got {
		got[index] = rng.next()
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("native item RNG = %v, want %v", got, want)
	}
}

func TestNativeSpeedScalarTableIsFixedPointAndBounded(t *testing.T) {
	want := []uint16{0, 1000, 1800, 3500, 4000, 4500, 5100, 5800, 6700, 8000, 13000}
	wantPixelsPerSecond := []uint16{0, 40, 72, 140, 160, 180, 204, 232, 268, 320, 520}
	for rate, expected := range want {
		value, ok := NativeSpeedScalarMilli(byte(rate))
		if !ok || value != expected {
			t.Fatalf("native speed scalar %d = %d/%v, want %d/true", rate, value, ok, expected)
		}
		pixelsPerSecond, ok := NativeSpeedPixelsPerSecond(byte(rate))
		if !ok || pixelsPerSecond != wantPixelsPerSecond[rate] {
			t.Fatalf("native speed rate %d = %d/%v pixels/second, want %d/true", rate, pixelsPerSecond, ok, wantPixelsPerSecond[rate])
		}
	}
	if _, ok := NativeSpeedScalarMilli(MaxNativeSpeedRate + 1); ok {
		t.Fatal("out-of-range native speed rate was accepted")
	}
	if _, ok := NativeSpeedPixelsPerSecond(MaxNativeSpeedRate + 1); ok {
		t.Fatal("out-of-range native pixels/second rate was accepted")
	}
}

func TestPlaceHiddenPickupsUsesNativeShuffleAndWireOrder(t *testing.T) {
	candidates := []Cell{{Col: 0}, {Col: 1}, {Col: 2}, {Col: 3}, {Col: 4}, {Col: 5}}
	items := []mapdata.CompetitiveWallItem{
		{SceneID: SceneBombCapacitySmall, Quantity: 2},
		{SceneID: SceneBombPowerSmall, Quantity: 3},
		{SceneID: SceneSpeedSmall, Quantity: 4},
	}
	pickups, err := PlaceHiddenPickups(0x12345678, candidates, items)
	if err != nil {
		t.Fatal(err)
	}
	want := []Pickup{
		{SceneID: SceneBombCapacitySmall, Cell: Cell{Col: 0}, State: PickupHidden},
		{SceneID: SceneBombCapacitySmall, Cell: Cell{Col: 5}, State: PickupHidden},
		{SceneID: SceneBombPowerSmall, Cell: Cell{Col: 2}, State: PickupHidden},
		{SceneID: SceneBombPowerSmall, Cell: Cell{Col: 1}, State: PickupHidden},
		{SceneID: SceneBombPowerSmall, Cell: Cell{Col: 3}, State: PickupHidden},
		{SceneID: SceneSpeedSmall, Cell: Cell{Col: 4}, State: PickupHidden},
	}
	if !reflect.DeepEqual(pickups, want) {
		t.Fatalf("hidden pickups = %+v, want %+v", pickups, want)
	}
	if _, err := PlaceHiddenPickups(1, []Cell{{}, {}}, items); err == nil {
		t.Fatal("duplicate hidden-item candidates were accepted")
	}
}

func TestHiddenPickupRevealsOnlyWhenWallIsDestroyed(t *testing.T) {
	config := testConfig()
	wall := Cell{Row: 1, Col: 2}
	config.Grid.Cells[int(wall.Row)*int(config.Grid.Width)+int(wall.Col)] = Tile{Kind: CellBreakable, Durability: 2}
	config.Pickups = []Pickup{{SceneID: SceneBombPowerSmall, Cell: wall, State: PickupHidden}}
	engine := mustEngine(t, config)
	// Install the fixture at the pre-movement explosion clock.
	engine.elapsedMS = 100
	engine.bombs = []Bomb{{ID: 1, OwnerID: 1, Cell: Cell{Row: 1, Col: 1}, Power: 2, ExplodeAtMS: 100}}
	engine.nextBombID = 2
	events, err := engine.Step(nil)
	if err != nil {
		t.Fatal(err)
	}
	if engine.pickups[0].State != PickupHidden || hasEvent(events, EventPickupRevealed, 0) {
		t.Fatalf("durable wall revealed pickup on first hit: pickup=%+v events=%+v", engine.pickups[0], events)
	}
	engine.bombs = []Bomb{{ID: 2, OwnerID: 1, Cell: Cell{Row: 1, Col: 1}, Power: 2, ExplodeAtMS: 200}}
	engine.nextBombID = 3
	events, err = engine.Step(nil)
	if err != nil {
		t.Fatal(err)
	}
	if engine.pickups[0].State != PickupAvailable || !hasEvent(events, EventPickupRevealed, 0) {
		t.Fatalf("destroyed wall did not reveal pickup: pickup=%+v events=%+v", engine.pickups[0], events)
	}
}

func TestPickupCollectionAppliesNativeAmountAndCap(t *testing.T) {
	config := testConfig()
	config.Participants[0].BombCapacity = 2
	config.Participants[0].MaxBombCapacity = 5
	config.Pickups = []Pickup{{SceneID: SceneBombCapacityLarge, Cell: config.Participants[0].Spawn, State: PickupAvailable}}
	engine := mustEngine(t, config)
	events, err := engine.Step(nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := engine.actors[0].BombCapacity; got != 5 {
		t.Fatalf("large capacity pickup produced %d, want capped 5", got)
	}
	if engine.pickups[0].State != PickupCollected {
		t.Fatalf("pickup state = %d, want collected", engine.pickups[0].State)
	}
	var collected Event
	for _, event := range events {
		if event.Kind == EventPickupCollected {
			collected = event
		}
	}
	if collected.SceneID != SceneBombCapacityLarge || collected.Attribute != AttributeBombCapacity || collected.ValueBefore != 2 || collected.ValueAfter != 5 {
		t.Fatalf("pickup event = %+v", collected)
	}
}

func TestVerifiedPickupUsesExactNativeCellInsteadOfAdjacentOverlap(t *testing.T) {
	config := testConfig()
	config.Rules.NativeOutcomeAuthority = true
	adjacent := Cell{Row: 1, Col: 1}
	config.Pickups = []Pickup{{SceneID: SceneBombCapacitySmall, Cell: adjacent, State: PickupAvailable}}
	engine := mustEngine(t, config)
	actor := &engine.actors[0]
	actor.Position = Position{X: 39, Y: 60} // native cell 1,0; footprint overlaps 1,1
	actor.MaxBombCapacity = actor.BombCapacity + 2

	if _, err := engine.ApplyVerifiedPickupAt(actor.PlayerID, SceneBombCapacitySmall, actor.Position); err != nil {
		t.Fatal(err)
	}
	if engine.pickups[0].State != PickupAvailable {
		t.Fatalf("adjacent native object was consumed: %+v", engine.pickups[0])
	}
}

func TestOfflinePickupUsesNativeCenterCellInsteadOfAdjacentFootprint(t *testing.T) {
	config := testConfig()
	adjacent := Cell{Row: 1, Col: 1}
	config.Pickups = []Pickup{{SceneID: SceneBombCapacitySmall, Cell: adjacent, State: PickupAvailable}}
	engine := mustEngine(t, config)
	actor := &engine.actors[0]
	actor.Position = Position{X: 39, Y: 60} // centre col 0; +/-19 footprint overlaps col 1
	actor.MaxBombCapacity = actor.BombCapacity + 2
	before := actor.BombCapacity

	if events := engine.resolvePickupContacts(); len(events) != 0 || actor.BombCapacity != before || engine.pickups[0].State != PickupAvailable {
		t.Fatalf("adjacent footprint collected early: events=%+v actor=%+v pickup=%+v", events, *actor, engine.pickups[0])
	}
	actor.Position.X = 40
	events := engine.resolvePickupContacts()
	if len(events) != 1 || events[0].Kind != EventPickupCollected || actor.BombCapacity != before+1 || engine.pickups[0].State != PickupCollected {
		t.Fatalf("centre-cell pickup = events=%+v actor=%+v pickup=%+v", events, *actor, engine.pickups[0])
	}
}

func TestVerifiedPickupRetiresConflictingObjectAtAuthenticatedCell(t *testing.T) {
	config := testConfig()
	config.Rules.NativeOutcomeAuthority = true
	cell := Cell{Row: 1, Col: 1}
	config.Pickups = []Pickup{{SceneID: SceneBombPowerSmall, Cell: cell, State: PickupAvailable}}
	engine := mustEngine(t, config)
	actor := &engine.actors[0]
	actor.Position = PositionAtCellCenter(cell)
	actor.MaxBombCapacity = actor.BombCapacity + 2

	if _, err := engine.ApplyVerifiedPickupAt(actor.PlayerID, SceneBombCapacitySmall, actor.Position); err != nil {
		t.Fatal(err)
	}
	if engine.pickups[0].State != PickupCollected {
		t.Fatalf("conflicting mirror pickup survived native success: %+v", engine.pickups[0])
	}
}

func TestSpeedPickupUsesExplicitNativeRateProjection(t *testing.T) {
	config := testConfig()
	config.Rules.SpeedPixelsPerSecondByRate[2] = 180
	config.Rules.SpeedPixelsPerSecondByRate[3] = 350
	config.Rules.SpeedPixelsPerSecondByRate[4] = 400
	for index := range config.Participants {
		config.Participants[index].SpeedRate = 2
		config.Participants[index].MaxSpeedRate = 4
		config.Participants[index].SpeedPixelsPerSecond = 180
	}
	config.Pickups = []Pickup{{SceneID: SceneSpeedSmall, Cell: config.Participants[0].Spawn, State: PickupAvailable}}
	engine := mustEngine(t, config)
	if _, err := engine.Step(nil); err != nil {
		t.Fatal(err)
	}
	actor := engine.actors[0]
	if actor.SpeedRate != 3 || actor.SpeedPixelsPerSecond != 350 {
		t.Fatalf("speed pickup actor rate/speed = %d/%d, want 3/350", actor.SpeedRate, actor.SpeedPixelsPerSecond)
	}
}

func TestSceneSixtyOneRevealsButDoesNotCollectAdjacentHiddenPickup(t *testing.T) {
	config := testConfig()
	config.Rules.RoundDurationMS = 60_000
	wall := Cell{Row: 0, Col: 1}
	config.Grid.Cells[int(wall.Row)*int(config.Grid.Width)+int(wall.Col)] = Tile{Kind: CellBreakable, Durability: 2}
	config.Pickups = []Pickup{
		{SceneID: SceneHiddenPickupReach, Cell: config.Participants[0].Spawn, State: PickupAvailable},
		{SceneID: SceneBombPowerSmall, Cell: wall, State: PickupHidden},
	}
	engine := mustEngine(t, config)
	events, err := engine.Step(nil)
	if err != nil {
		t.Fatal(err)
	}
	var hidden Pickup
	for _, pickup := range engine.pickups {
		if pickup.SceneID == SceneBombPowerSmall {
			hidden = pickup
		}
	}
	if hidden.State != PickupHidden {
		t.Fatalf("adjacent hidden pickup state = %d, want hidden", hidden.State)
	}
	if tile, _ := engine.grid.Cell(wall); tile.Kind != CellBreakable || tile.Durability != 2 {
		t.Fatalf("SceneID 61 changed the containing wall: %+v", tile)
	}
	if engine.actors[0].HiddenPickupReachExpiresAt != NativeHiddenPickupReachMS {
		t.Fatalf("hidden reach expires at %d, want %d", engine.actors[0].HiddenPickupReachExpiresAt, NativeHiddenPickupReachMS)
	}
	var effectEvent Event
	for _, event := range events {
		if event.SceneID == SceneHiddenPickupReach {
			effectEvent = event
		}
	}
	if effectEvent.Effect != PickupEffectHiddenReach || effectEvent.EffectExpiresAt != NativeHiddenPickupReachMS {
		t.Fatalf("SceneID 61 event = %+v", effectEvent)
	}
	observation, err := engine.Observation(engine.actors[0].PlayerID)
	if err != nil {
		t.Fatal(err)
	}
	visible := false
	for _, pickup := range observation.Pickups {
		visible = visible || (pickup.SceneID == SceneBombPowerSmall && pickup.Cell == wall && pickup.State == PickupHidden)
	}
	if !visible {
		t.Fatalf("detector did not reveal hidden pickup in observation: %+v", observation.Pickups)
	}
}

func TestExpiredSceneSixtyOneDoesNotCollectHiddenPickup(t *testing.T) {
	config := testConfig()
	config.Rules.RoundDurationMS = 60_000
	wall := Cell{Row: 0, Col: 1}
	config.Grid.Cells[int(wall.Row)*int(config.Grid.Width)+int(wall.Col)] = Tile{Kind: CellBreakable, Durability: 1}
	config.Pickups = []Pickup{{SceneID: SceneBombPowerSmall, Cell: wall, State: PickupHidden}}
	engine := mustEngine(t, config)
	engine.elapsedMS = NativeHiddenPickupReachMS + 1
	engine.actors[0].HiddenPickupReachExpiresAt = NativeHiddenPickupReachMS
	if _, err := engine.Step(nil); err != nil {
		t.Fatal(err)
	}
	if engine.pickups[0].State != PickupHidden {
		t.Fatalf("expired hidden reach collected pickup: %+v", engine.pickups[0])
	}
}

func TestSceneFourMarksBombsWithoutChangingCombatAndSceneSixtyTwoPreservesNativeValue(t *testing.T) {
	config := testConfig()
	config.Rules.RoundDurationMS = 60_000
	config.Pickups = []Pickup{
		{SceneID: SceneFourBubbleEffect, Cell: config.Participants[0].Spawn, State: PickupAvailable},
		{SceneID: SceneOxygenValueAdd, Cell: config.Participants[1].Spawn, State: PickupAvailable},
	}
	engine := mustEngine(t, config)
	if _, err := engine.Step(nil); err != nil {
		t.Fatal(err)
	}
	if engine.actors[0].SceneFourEffectExpiresAt != NativeSceneFourEffectMS {
		t.Fatalf("SceneID 4 expiry = %d, want %d", engine.actors[0].SceneFourEffectExpiresAt, NativeSceneFourEffectMS)
	}
	if engine.actors[1].OxygenValue != 7_000 {
		t.Fatalf("SceneID 62 oxygen value = %d, want 7000", engine.actors[1].OxygenValue)
	}
	if _, placed := engine.placeBomb(0); !placed || len(engine.bombs) != 1 || !engine.bombs[0].SceneFourEffect {
		t.Fatalf("SceneID 4 bubble marker was not projected: %+v", engine.bombs)
	}
	if engine.bombs[0].ExplodeAtMS != engine.elapsedMS+engine.rules.BombFuseMS+1 || engine.bombs[0].Power != engine.actors[0].BombPower {
		t.Fatalf("SceneID 4 changed combat fields: %+v", engine.bombs[0])
	}
	engine.elapsedMS = NativeSceneFourEffectMS + 1
	engine.bombs = nil
	if _, placed := engine.placeBomb(0); !placed || engine.bombs[0].SceneFourEffect {
		t.Fatalf("expired SceneID 4 still marked bubble: %+v", engine.bombs)
	}
}

func TestSuperShoeAndSlowGlueReplaceTheSameNativeMovementStatus(t *testing.T) {
	config := testConfig()
	config.Rules.SpeedPixelsPerSecondByRate = nativeSpeedPixelsPerSecondByRate
	engine := mustEngine(t, config)
	actor := &engine.actors[0]
	engine.installMovementStatus(actor, MovementStatusSlow)

	pickup := Pickup{SceneID: SceneFastMovement, Cell: actor.Position.Cell(), State: PickupAvailable}
	engine.collectPickup(actor, &pickup)
	if actor.MovementStatus != MovementStatusFast || actor.MovementStatusExpiresAt != NativeMovementStatusMS {
		t.Fatalf("super shoe did not replace slow status: %+v", *actor)
	}
	if got := engine.effectiveSpeedPixelsPerSecond(actor, DirectionRight); got != nativeSpeedPixelsPerSecondByRate[8] {
		t.Fatalf("super shoe effective speed = %d, want native rate 8 (%d)", got, nativeSpeedPixelsPerSecondByRate[8])
	}

	engine.elapsedMS = 1_000
	engine.installMovementStatus(actor, MovementStatusSlow)
	if actor.MovementStatus != MovementStatusSlow || actor.MovementStatusExpiresAt != 1_000+NativeMovementStatusMS {
		t.Fatalf("later slow glue did not replace super shoe: %+v", *actor)
	}
}

func TestSceneFiveUsesNativeCellSeededAttributeAndSign(t *testing.T) {
	tests := []struct {
		cell      Cell
		attribute AttributeKind
		delta     int8
	}{
		{cell: Cell{Row: 0, Col: 0}, attribute: AttributeSpeedRate, delta: 1},
		{cell: Cell{Row: 0, Col: 3}, attribute: AttributeBombCapacity, delta: -1},
		{cell: Cell{Row: 1, Col: 1}, attribute: AttributeBombPower, delta: -1},
	}
	for _, test := range tests {
		attribute, delta := nativeRandomAttributeEffect(test.cell)
		if attribute != test.attribute || delta != test.delta {
			t.Fatalf("SceneID 5 at %+v = %d/%d, want %d/%d", test.cell, attribute, delta, test.attribute, test.delta)
		}

	}

	config := testConfig()
	for index := range config.Participants {
		config.Participants[index].SpeedRate = 2
		config.Participants[index].MaxSpeedRate = 4
		config.Participants[index].SpeedPixelsPerSecond = 72
	}
	config.Rules.SpeedPixelsPerSecondByRate = nativeSpeedPixelsPerSecondByRate
	config.Pickups = []Pickup{{SceneID: SceneRandomAttribute, Cell: config.Participants[0].Spawn, State: PickupAvailable}}
	engine := mustEngine(t, config)
	events, err := engine.Step(nil)
	if err != nil {
		t.Fatal(err)
	}
	if engine.actors[0].BombPower != 1 {
		t.Fatalf("SceneID 5 power = %d, want 1", engine.actors[0].BombPower)
	}
	var collected Event
	for _, event := range events {
		if event.Kind == EventPickupCollected {
			collected = event
		}
	}
	if collected.Attribute != AttributeBombPower || collected.ValueBefore != 2 || collected.ValueAfter != 1 {
		t.Fatalf("SceneID 5 event = %+v", collected)
	}
	opponentView, err := engine.Observation(2)
	if err != nil {
		t.Fatal(err)
	}
	if got := opponentView.Actors[0]; got.BombPower != 1 || got.MaxBombPower != engine.actors[0].MaxBombPower {
		t.Fatalf("public random-pickup attribute result = %+v", got)
	}
}

func TestPickupValidationRejectsUnsupportedOrUnprojectedEffects(t *testing.T) {
	config := testConfig()
	config.Pickups = []Pickup{{SceneID: 9, Cell: config.Participants[0].Spawn, State: PickupAvailable}}
	if _, err := New(config); err == nil || !strings.Contains(err.Error(), "unsupported scene ID 9") {
		t.Fatalf("unsupported pickup error = %v", err)
	}
	config.Pickups[0].SceneID = SceneSpeedSmall
	if _, err := New(config); err == nil || !strings.Contains(err.Error(), "needs a native speed rate") {
		t.Fatalf("unprojected speed pickup error = %v", err)
	}
}

func TestPickupCloneAndObservationAreIndependent(t *testing.T) {
	config := testConfig()
	config.Pickups = []Pickup{{SceneID: SceneBombPowerSmall, Cell: Cell{Row: 0, Col: 4}, State: PickupAvailable}}
	engine := mustEngine(t, config)
	clone := engine.Clone()
	clone.pickups[0].State = PickupCollected
	observation, err := engine.Observation(1)
	if err != nil {
		t.Fatal(err)
	}
	observation.Pickups[0].SceneID = 999
	if engine.pickups[0].State != PickupAvailable || engine.pickups[0].SceneID != SceneBombPowerSmall {
		t.Fatalf("pickup state leaked through clone/observation: %+v", engine.pickups[0])
	}
}

func TestObservationDoesNotExposeHiddenWallItemsWithoutDetector(t *testing.T) {
	config := testConfig()
	hiddenCell := Cell{Row: 1, Col: 2}
	config.Grid.Cells[int(hiddenCell.Row)*int(config.Grid.Width)+int(hiddenCell.Col)] = Tile{Kind: CellBreakable, Durability: 1}
	config.Pickups = []Pickup{
		{SceneID: SceneBombCapacitySmall, Cell: hiddenCell, State: PickupHidden},
		{SceneID: SceneBombPowerSmall, Cell: Cell{Row: 0, Col: 4}, State: PickupAvailable},
	}
	engine := mustEngine(t, config)
	observation, err := engine.Observation(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(observation.Pickups) != 1 || observation.Pickups[0].SceneID != SceneBombPowerSmall {
		t.Fatalf("hidden pickup leaked into policy observation: %+v", observation.Pickups)
	}
	engine.actors[0].HiddenPickupReachExpiresAt = engine.elapsedMS + 1_000
	observation, err = engine.Observation(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(observation.Pickups) != 2 {
		t.Fatalf("detector did not reveal nearby hidden pickup: %+v", observation.Pickups)
	}
}

func TestPolicySnapshotCannotSpeculateUnseenWallPickup(t *testing.T) {
	config := testConfig()
	hiddenCell := Cell{Row: 1, Col: 2}
	config.Grid.Cells[int(hiddenCell.Row)*int(config.Grid.Width)+int(hiddenCell.Col)] = Tile{Kind: CellBreakable, Durability: 1}
	config.Pickups = []Pickup{
		{SceneID: SceneBombCapacitySmall, Cell: hiddenCell, State: PickupHidden},
		{SceneID: SceneBombPowerSmall, Cell: Cell{Row: 0, Col: 4}, State: PickupAvailable},
	}
	engine := mustEngine(t, config)
	snapshot, err := engine.PolicySnapshot(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.pickups) != 1 || snapshot.pickups[0].State != PickupAvailable {
		t.Fatalf("policy snapshot leaked hidden pickup: %+v", snapshot.pickups)
	}
	engine.actors[0].HiddenPickupReachExpiresAt = engine.elapsedMS + 1_000
	snapshot, err = engine.PolicySnapshot(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.pickups) != 2 {
		t.Fatalf("policy snapshot omitted detector-visible pickup: %+v", snapshot.pickups)
	}
}

func TestTransformationPickupReplacesExpiresAndProtectsFromOneHit(t *testing.T) {
	config := testConfig()
	config.Rules.RoundDurationMS = 60_000
	config.Pickups = []Pickup{{SceneID: 101, Cell: config.Participants[0].Spawn, State: PickupAvailable}}
	engine := mustEngine(t, config)
	events, err := engine.Step(nil)
	if err != nil {
		t.Fatal(err)
	}
	actor := &engine.actors[0]
	if actor.TransformationSceneID != 101 || actor.AvatarRoleID != 43 || actor.TransformationExpiresAt != sceneelement.NativeTransformationDurationMS {
		t.Fatalf("first transformation = scene %d avatar %d expiry %d", actor.TransformationSceneID, actor.AvatarRoleID, actor.TransformationExpiresAt)
	}
	if len(events) == 0 || events[0].Effect != PickupEffectTransformation || events[0].AvatarRoleID != 43 {
		t.Fatalf("transformation pickup events = %+v", events)
	}
	opponentView, err := engine.Observation(2)
	if err != nil {
		t.Fatal(err)
	}
	if got := opponentView.Actors[0]; got.TransformationSceneID != 101 || got.TransformationExpiresAt != actor.TransformationExpiresAt {
		t.Fatalf("opponent transformation view = %+v, actor = %+v", got, *actor)
	}

	engine.elapsedMS = 1_000
	replacement := Pickup{SceneID: 115, Cell: actor.Position.Cell(), State: PickupAvailable}
	replaced := engine.collectPickup(actor, &replacement)
	if actor.TransformationSceneID != 115 || actor.AvatarRoleID != 55 || actor.TransformationExpiresAt != 31_000 || replaced.AvatarRoleID != 55 {
		t.Fatalf("replacement transformation actor/event = %+v/%+v", *actor, replaced)
	}
	engine.flames = []Flame{{Cell: actor.Position.Cell(), OwnerID: 2, ImpactAtMS: engine.elapsedMS, ExpiresAtMS: 2_000}}
	events = engine.applyFlameHazards()
	if actor.State != ActorActive || actor.TransformationSceneID != 0 || actor.AvatarRoleID != 0 || actor.TransformationExpiresAt != 0 {
		t.Fatalf("transformed hit actor = %+v", *actor)
	}
	if len(events) != 1 || events[0].Kind != EventActorTransformationEnded || events[0].TransformationEnd != TransformationEndHit || events[0].TargetID != 2 {
		t.Fatalf("transformed hit events = %+v", events)
	}
	if actor.HarmProtectionExpiresAt != engine.elapsedMS+NativePostTransformationProtectionMS || events[0].EffectExpiresAt != actor.HarmProtectionExpiresAt {
		t.Fatalf("post-transformation protection actor/event = %+v/%+v", *actor, events[0])
	}
	opponentView, err = engine.Observation(2)
	if err != nil {
		t.Fatal(err)
	}
	if got := opponentView.Actors[0]; got.TransformationSceneID != 0 || got.HarmProtectionExpiresAt != actor.HarmProtectionExpiresAt {
		t.Fatalf("opponent post-transformation protection view = %+v, actor = %+v", got, *actor)
	}
	engine.elapsedMS++
	events = engine.applyFlameHazards()
	if actor.State != ActorActive || len(events) != 0 {
		t.Fatalf("one native impact repeated during visual flame lifetime: actor=%+v events=%+v", *actor, events)
	}

	engine.flames = nil
	replacement.State = PickupAvailable
	engine.collectPickup(actor, &replacement)
	engine.elapsedMS = actor.TransformationExpiresAt + 1
	events = engine.expireTransformations()
	if actor.TransformationSceneID != 0 || len(events) != 1 || events[0].TransformationEnd != TransformationEndExpired {
		t.Fatalf("transformation expiry actor/events = %+v/%+v", *actor, events)
	}
	if actor.HarmProtectionExpiresAt != engine.elapsedMS+NativePostTransformationProtectionMS {
		t.Fatalf("natural recovery protection = %d", actor.HarmProtectionExpiresAt)
	}
}

func TestVerifiedPickupConsumesMatchingNativeMapEntry(t *testing.T) {
	config := testConfig()
	cell := config.Participants[0].Spawn
	config.Pickups = []Pickup{
		{SceneID: SceneBombCapacitySmall, Cell: cell, State: PickupAvailable},
		{SceneID: SceneBombCapacitySmall, Cell: Cell{Row: cell.Row, Col: cell.Col + 1}, State: PickupAvailable},
	}
	engine := mustEngine(t, config)
	position := PositionAtCellCenter(cell)
	event, err := engine.ApplyVerifiedPickupAt(config.Participants[0].PlayerID, SceneBombCapacitySmall, position)
	if err != nil {
		t.Fatal(err)
	}
	if event.Cell != cell || engine.pickups[0].State != PickupCollected || engine.pickups[1].State != PickupAvailable {
		t.Fatalf("verified pickup consumed wrong map entry: event=%+v pickups=%+v", event, engine.pickups)
	}
}

func TestVerifiedAvatarHitAndRecoveryAreIdempotent(t *testing.T) {
	config := testConfig()
	config.Pickups = []Pickup{{SceneID: 101, Cell: config.Participants[0].Spawn, State: PickupAvailable}}
	engine := mustEngine(t, config)
	if _, err := engine.Step(nil); err != nil {
		t.Fatal(err)
	}
	playerID := config.Participants[0].PlayerID
	position := engine.actors[0].Position
	event, changed, err := engine.ApplyVerifiedActorHit(playerID, position, true)
	if err != nil || !changed || event.Kind != EventActorTransformationEnded || event.TransformationEnd != TransformationEndHit {
		t.Fatalf("verified avatar hit = %+v/%v/%v", event, changed, err)
	}
	if _, changed, err = engine.ApplyVerifiedActorHit(playerID, position, true); err != nil || changed {
		t.Fatalf("duplicate verified avatar hit changed state: changed=%v err=%v", changed, err)
	}
	if _, changed, err = engine.ApplyVerifiedTransformationRecovery(playerID, position); err != nil || changed {
		t.Fatalf("late recovery changed already restored avatar: changed=%v err=%v", changed, err)
	}
}

func TestPostTransformationProtectionRejectsLaterHarmUntilExactExpiry(t *testing.T) {
	config := testConfig()
	config.Pickups = []Pickup{{SceneID: 101, Cell: config.Participants[0].Spawn, State: PickupAvailable}}
	engine := mustEngine(t, config)
	if _, err := engine.Step(nil); err != nil {
		t.Fatal(err)
	}
	actor := &engine.actors[0]
	engine.flames = []Flame{{Cell: actor.Position.Cell(), OwnerID: 2, ImpactAtMS: engine.elapsedMS, ExpiresAtMS: engine.elapsedMS + 500}}
	engine.applyFlameHazards()
	protectionExpiresAt := actor.HarmProtectionExpiresAt

	engine.elapsedMS = protectionExpiresAt - 1
	engine.flames[0].ImpactAtMS = engine.elapsedMS
	if events := engine.applyFlameHazards(); actor.State != ActorActive || len(events) != 0 {
		t.Fatalf("protected actor was harmed: actor=%+v events=%+v", *actor, events)
	}

	engine.elapsedMS = protectionExpiresAt
	engine.flames[0].ImpactAtMS = engine.elapsedMS
	events := engine.applyFlameHazards()
	if actor.State != ActorTrapped || len(events) != 1 || events[0].Kind != EventActorTrapped {
		t.Fatalf("expired protection still blocked harm: actor=%+v events=%+v", *actor, events)
	}
}

func TestDuckTransformationCrossesStaticWallsButNotBombsAndCannotCollect(t *testing.T) {
	config := testConfig()
	config.Rules.TickMS = 1
	config.Rules.RoundDurationMS = 120_000
	config.Participants[0].Spawn = Cell{Row: 1, Col: 1}
	config.Participants[0].SpeedPixelsPerSecond = 400
	config.Grid.Cells[1*int(config.Grid.Width)+2] = Tile{Kind: CellSolid}
	config.Pickups = []Pickup{{SceneID: 104, Cell: config.Participants[0].Spawn, State: PickupAvailable}}
	engine := mustEngine(t, config)
	if _, err := engine.Step(nil); err != nil {
		t.Fatal(err)
	}
	actor := &engine.actors[0]
	if !engine.actorCapabilities(actor, actor.Facing).TraverseStaticTerrain {
		t.Fatalf("duck transformation is not airborne: %+v", *actor)
	}

	for attempts := 0; actor.Position.Cell() != (Cell{Row: 1, Col: 2}) && attempts < 200; attempts++ {
		if _, err := engine.Step([]Action{{PlayerID: actor.PlayerID, Move: DirectionRight}}); err != nil {
			t.Fatal(err)
		}
	}
	if actor.Position.Cell() != (Cell{Row: 1, Col: 2}) {
		t.Fatalf("duck position = %+v, want static-wall cell 1,2", actor.Position)
	}

	engine.pickups = append(engine.pickups, Pickup{SceneID: SceneBombPowerSmall, Cell: actor.Position.Cell(), State: PickupAvailable})
	beforePower := actor.BombPower
	if _, err := engine.Step(nil); err != nil {
		t.Fatal(err)
	}
	if engine.pickups[len(engine.pickups)-1].State != PickupAvailable || actor.BombPower != beforePower {
		t.Fatalf("airborne duck collected map pickup: actor=%+v pickup=%+v", *actor, engine.pickups[len(engine.pickups)-1])
	}

	engine.bombs = append(engine.bombs, Bomb{ID: 99, OwnerID: 2, Cell: Cell{Row: 1, Col: 3}, Power: 1, ExplodeAtMS: 50_000})
	for attempts := 0; attempts < 200; attempts++ {
		before := actor.Position
		if _, err := engine.Step([]Action{{PlayerID: actor.PlayerID, Move: DirectionRight}}); err != nil {
			t.Fatal(err)
		}
		if actor.Position == before && actor.moveRemainder == 0 {
			break
		}
	}
	if actor.Position.Cell() != (Cell{Row: 1, Col: 2}) {
		t.Fatalf("duck crossed dynamic bomb: after=%+v", actor.Position)
	}

	engine.elapsedMS = actor.TransformationExpiresAt
	events := engine.expireTransformations()
	if len(events) != 0 || actor.TransformationSceneID != 104 {
		t.Fatalf("duck recovered over static wall: actor=%+v events=%+v", *actor, events)
	}
	recovered := false
	for attempts := 0; attempts < 200 && !recovered; attempts++ {
		stepEvents, err := engine.Step([]Action{{PlayerID: actor.PlayerID, Move: DirectionLeft}})
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range stepEvents {
			if event.Kind != EventActorTransformationEnded {
				continue
			}
			if actor.Position.Cell() != (Cell{Row: 1, Col: 1}) || event.TransformationEnd != TransformationEndExpired {
				t.Fatalf("duck recovered before landing on an open cell: actor=%+v event=%+v", *actor, event)
			}
			recovered = true
		}
		if !recovered && actor.TransformationSceneID == 0 {
			t.Fatalf("duck transformation disappeared without an expiry event: actor=%+v events=%+v", *actor, stepEvents)
		}
	}
	if !recovered || actor.TransformationSceneID != 0 {
		t.Fatalf("duck did not recover on the movement step that reached open terrain: actor=%+v", *actor)
	}
}

func TestMatchSugarPickupUsesNativeAccumulatorAndObservation(t *testing.T) {
	config := testConfig()
	config.Pickups = []Pickup{{SceneID: uint32(sceneelement.SugarChest1000), Cell: config.Participants[0].Spawn, State: PickupAvailable}}
	engine := mustEngine(t, config)
	events, err := engine.Step(nil)
	if err != nil {
		t.Fatal(err)
	}
	if engine.actors[0].MatchSugar != 1_000 {
		t.Fatalf("match sugar = %d, want 1000", engine.actors[0].MatchSugar)
	}
	if len(events) == 0 || events[0].Effect != PickupEffectMatchSugar || events[0].RewardBefore != 0 || events[0].RewardAfter != 1_000 {
		t.Fatalf("match-sugar pickup events = %+v", events)
	}
	observation, err := engine.Observation(1)
	if err != nil {
		t.Fatal(err)
	}
	if observation.Actors[0].MatchSugar != 1_000 {
		t.Fatalf("observed match sugar = %d, want 1000", observation.Actors[0].MatchSugar)
	}
	clone := engine.Clone()
	clone.actors[0].MatchSugar = 0
	if engine.actors[0].MatchSugar != 1_000 {
		t.Fatal("match sugar leaked through clone")
	}
}

func TestVerifiedPickupDispatchReplacesStaleCellObject(t *testing.T) {
	config := testConfig()
	cell := Cell{Row: 1, Col: 2}
	config.Pickups = []Pickup{{SceneID: SceneBombPowerSmall, Cell: cell, State: PickupAvailable}}
	engine := mustEngine(t, config)

	dispatchTime := engine.elapsedMS
	if err := engine.ApplyVerifiedPickupDispatch(dispatchTime, []Pickup{{SceneID: SceneSpeedSmall, Cell: cell, State: PickupAvailable}}); err != nil {
		t.Fatal(err)
	}
	if len(engine.pendingPickupDispatches) != 1 {
		t.Fatalf("scheduled dispatches = %+v, want one", engine.pendingPickupDispatches)
	}
	engine.elapsedMS = engine.pendingPickupDispatches[0].ActivateAtMS
	engine.activatePendingPickupDispatches()
	live := 0
	for _, pickup := range engine.pickups {
		if pickup.Cell != cell || pickup.State != PickupAvailable {
			continue
		}
		live++
		if pickup.SceneID != SceneSpeedSmall {
			t.Fatalf("live dispatched pickup = %+v, want SceneID %d", pickup, SceneSpeedSmall)
		}
	}
	if live != 1 {
		t.Fatalf("live dispatched pickups at %v = %d, want 1: %+v", cell, live, engine.pickups)
	}
}

func TestVerifiedPickupDispatchAcceptsMixedFieldAndOrdinarySceneObjects(t *testing.T) {
	config := testConfig()
	fieldCell := Cell{Row: 1, Col: 1}
	pickupCell := Cell{Row: 1, Col: 2}
	engine := mustEngine(t, config)

	dispatchTime := engine.elapsedMS
	if err := engine.ApplyVerifiedPickupDispatch(dispatchTime, []Pickup{
		{SceneID: 42, Cell: fieldCell, State: PickupAvailable},
		{SceneID: SceneSpeedSmall, Cell: pickupCell, State: PickupAvailable},
	}); err != nil {
		t.Fatal(err)
	}
	if len(engine.FieldObjects()) != 0 || len(engine.Pickups()) != 0 {
		t.Fatalf("dispatch targets became observable before landing: fields=%+v pickups=%+v", engine.FieldObjects(), engine.Pickups())
	}
	latest := uint32(0)
	for _, pending := range engine.pendingPickupDispatches {
		if pending.ActivateAtMS > latest {
			latest = pending.ActivateAtMS
		}
	}
	engine.elapsedMS = latest
	engine.activatePendingPickupDispatches()
	fields := engine.FieldObjects()
	if len(fields) != 1 || fields[0].ActionID != 42 || fields[0].Cell != fieldCell || fields[0].OwnerID != 0 {
		t.Fatalf("mixed dispatch field object = %+v", fields)
	}
	livePickup := false
	for _, pickup := range engine.Pickups() {
		if pickup.SceneID == SceneSpeedSmall && pickup.Cell == pickupCell && pickup.State == PickupAvailable {
			livePickup = true
		}
	}
	if !livePickup {
		t.Fatalf("mixed dispatch lost ordinary pickup: %+v", engine.Pickups())
	}
}

func TestVerifiedPickupDispatchRejectsOutOfBoundsAtomically(t *testing.T) {
	engine := mustEngine(t, testConfig())
	before := engine.Clone()
	err := engine.ApplyVerifiedPickupDispatch(engine.elapsedMS, []Pickup{
		{SceneID: SceneSpeedSmall, Cell: Cell{Row: 1, Col: 2}, State: PickupAvailable},
		{SceneID: SceneBombPowerSmall, Cell: Cell{Row: -1, Col: 2}, State: PickupAvailable},
	})
	if err == nil {
		t.Fatal("out-of-bounds authority dispatch was accepted")
	}
	if !reflect.DeepEqual(engine, before) {
		t.Fatalf("rejected authority dispatch mutated engine\nbefore=%+v\nafter=%+v", before, engine)
	}
}

func TestVerifiedItemDestructionRetiresPickupAndFieldObjectAtNativeCell(t *testing.T) {
	config := testConfig()
	cell := Cell{Row: 1, Col: 2}
	config.Pickups = []Pickup{{SceneID: SceneBombPowerSmall, Cell: cell, State: PickupAvailable}}
	engine := mustEngine(t, config)
	engine.fieldObjects = []FieldObject{{ID: 9, ActionID: 43, OwnerID: 1, Cell: cell}}

	// The ID deliberately differs from the stale mirror pickup. The native
	// coordinate is authoritative and must clear both scene representations.
	if err := engine.ApplyVerifiedItemDestruction([]Pickup{{SceneID: 43, Cell: cell, State: PickupAvailable}}); err != nil {
		t.Fatal(err)
	}
	for _, pickup := range engine.pickups {
		if pickup.Cell == cell && pickup.State == PickupAvailable {
			t.Fatalf("destroyed native cell retained pickup %+v", pickup)
		}
	}
	if len(engine.fieldObjects) != 0 {
		t.Fatalf("destroyed native cell retained field objects %+v", engine.fieldObjects)
	}
}
