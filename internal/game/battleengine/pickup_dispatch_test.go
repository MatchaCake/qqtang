package battleengine

import "testing"

func TestDestroyedVisiblePickupReturnsThroughNativeBirdQueue(t *testing.T) {
	config := testConfig()
	config.Grid = testOpenGrid(7, 3)
	config.Rules.RoundDurationMS = 60_000
	config.Participants[0].Spawn = Cell{Row: 1, Col: 0}
	config.Participants[1].Spawn = Cell{Row: 1, Col: 6}
	itemCell := Cell{Row: 1, Col: 4}
	config.Pickups = []Pickup{{SceneID: SceneBombPowerSmall, Cell: itemCell, State: PickupAvailable}}
	engine := mustEngine(t, config)
	// Install the fixture at the pre-movement explosion clock.
	engine.elapsedMS = 100
	engine.bombs = []Bomb{{ID: 1, OwnerID: 1, Cell: Cell{Row: 1, Col: 3}, Power: 1, ExplodeAtMS: config.Rules.TickMS}}
	engine.nextBombID = 2

	events, err := engine.Step(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !hasEvent(events, EventPickupDestroyed, 0) || engine.pickups[0].State != PickupCollected {
		t.Fatalf("visible pickup was not destroyed and queued: events=%+v pickups=%+v", events, engine.pickups)
	}
	if len(engine.recycledPickupSceneIDs) != 1 || engine.recycledPickupSceneIDs[0] != SceneBombPowerSmall {
		t.Fatalf("bird queue = %+v, want scene %d", engine.recycledPickupSceneIDs, SceneBombPowerSmall)
	}
	clone := engine.Clone()
	clone.recycledPickupSceneIDs[0] = SceneSpeedSmall
	if engine.recycledPickupSceneIDs[0] != SceneBombPowerSmall {
		t.Fatal("Clone leaked bird queue storage")
	}

	for engine.ElapsedMS() <= nativePickupDispatchIntervalMS {
		events, err = engine.Step(nil)
		if err != nil {
			t.Fatal(err)
		}
	}
	if !hasEvent(events, EventPickupDispatched, 0) {
		t.Fatalf("30-second bird dispatch missing: elapsed=%d events=%+v", engine.ElapsedMS(), events)
	}
	if len(engine.pendingPickupDispatches) != 1 {
		t.Fatalf("bird dispatch pending queue = %+v, want one target", engine.pendingPickupDispatches)
	}
	live := 0
	for _, pickup := range engine.pickups {
		if pickup.SceneID == SceneBombPowerSmall && pickup.State == PickupAvailable {
			live++
			tile, _ := engine.grid.Cell(pickup.Cell)
			if tile.Kind != CellOpen || engine.deathDropCellOccupiedExcludingPickup(pickup.Cell, pickup) {
				t.Fatalf("bird selected an occupied cell: %+v tile=%+v", pickup, tile)
			}
		}
	}
	if live != 0 || len(engine.recycledPickupSceneIDs) != 0 {
		t.Fatalf("pre-landing live count/queue = %d/%+v, pickups=%+v", live, engine.recycledPickupSceneIDs, engine.pickups)
	}
	observation, err := engine.Observation(config.Participants[0].PlayerID)
	if err != nil {
		t.Fatal(err)
	}
	visible := 0
	for _, pickup := range observation.Pickups {
		if pickup.SceneID == SceneBombPowerSmall && pickup.State == PickupAvailable {
			visible++
		}
	}
	if visible != 0 {
		t.Fatalf("training observation leaked %d flying pickups: %+v", visible, observation.Pickups)
	}
	activateAt := engine.pendingPickupDispatches[0].ActivateAtMS
	for engine.ElapsedMS() < activateAt {
		if _, err = engine.Step(nil); err != nil {
			t.Fatal(err)
		}
	}
	if len(engine.pendingPickupDispatches) != 0 {
		t.Fatalf("landed pickup remained pending: %+v", engine.pendingPickupDispatches)
	}
	for _, pickup := range engine.pickups {
		if pickup.SceneID == SceneBombPowerSmall && pickup.State == PickupAvailable {
			live++
		}
	}
	if live != 1 {
		t.Fatalf("landed live pickup count = %d, want 1: %+v", live, engine.pickups)
	}
	observation, err = engine.Observation(config.Participants[0].PlayerID)
	if err != nil {
		t.Fatal(err)
	}
	visible = 0
	for _, pickup := range observation.Pickups {
		if pickup.SceneID == SceneBombPowerSmall && pickup.State == PickupAvailable {
			visible++
		}
	}
	if visible != 1 {
		t.Fatalf("training observation sees %d landed pickups, want 1: %+v", visible, observation.Pickups)
	}
}

func TestVerifiedPickupDispatchUsesNativeBirdClockAndContactGuard(t *testing.T) {
	config := testConfig()
	config.Grid = testOpenGrid(15, 3)
	config.Rules.RoundDurationMS = 60_000
	config.Participants[0].Spawn = Cell{Row: 1, Col: 0}
	config.Participants[1].Spawn = Cell{Row: 1, Col: 14}
	engine := mustEngine(t, config)
	dispatchTime := uint32(30_000)
	pickup := Pickup{SceneID: SceneSpeedSmall, Cell: Cell{Row: 1, Col: 7}, State: PickupAvailable}
	engine.elapsedMS = dispatchTime

	if err := engine.ApplyVerifiedPickupDispatch(dispatchTime, []Pickup{pickup}); err != nil {
		t.Fatal(err)
	}
	wantActivateAt := dispatchTime + 2_700 // 2400 ms bird flight + 300 ms AI visibility delay
	if len(engine.pendingPickupDispatches) != 1 || engine.pendingPickupDispatches[0].ActivateAtMS != wantActivateAt {
		t.Fatalf("pending dispatch = %+v, want activation %d", engine.pendingPickupDispatches, wantActivateAt)
	}
	if snapshot, err := engine.Observation(config.Participants[0].PlayerID); err != nil || len(snapshot.Pickups) != 0 {
		t.Fatalf("flying pickup observation = %+v, err=%v", snapshot.Pickups, err)
	}
	policy, err := engine.PolicySnapshot(config.Participants[0].PlayerID)
	if err != nil {
		t.Fatal(err)
	}
	if len(policy.pendingPickupDispatches) != 0 {
		t.Fatalf("policy snapshot leaked dispatch target: %+v", policy.pendingPickupDispatches)
	}
	clone := engine.Clone()
	clone.pendingPickupDispatches[0].Pickup.SceneID = SceneBombPowerSmall
	if engine.pendingPickupDispatches[0].Pickup.SceneID != SceneSpeedSmall {
		t.Fatal("Clone leaked pending dispatch storage")
	}

	engine.elapsedMS = wantActivateAt - 1
	engine.activatePendingPickupDispatches()
	if len(engine.Pickups()) != 0 {
		t.Fatalf("pickup activated one millisecond early: %+v", engine.Pickups())
	}
	engine.elapsedMS = wantActivateAt
	engine.activatePendingPickupDispatches()
	if len(engine.pendingPickupDispatches) != 0 || !snapshotHasPickup(engine, pickup) {
		t.Fatalf("pickup did not activate after the native flight/contact guard: pending=%+v pickups=%+v", engine.pendingPickupDispatches, engine.Pickups())
	}
}

func TestVerifiedPickupDispatchColumnTwoMatchesNativeFlightCoefficient(t *testing.T) {
	config := testConfig()
	config.Grid = testOpenGrid(15, 3)
	config.Rules.RoundDurationMS = 90_000
	config.Participants[0].Spawn = Cell{Row: 1, Col: 0}
	config.Participants[1].Spawn = Cell{Row: 1, Col: 14}
	engine := mustEngine(t, config)
	dispatchTime := uint32(60_000)
	pickup := Pickup{SceneID: SceneSpeedLarge, Cell: Cell{Row: 1, Col: 2}, State: PickupAvailable}
	engine.elapsedMS = dispatchTime

	if err := engine.ApplyVerifiedPickupDispatch(dispatchTime, []Pickup{pickup}); err != nil {
		t.Fatal(err)
	}
	wantActivateAt := uint32(63_700) // 3400 ms bird flight + 300 ms AI visibility delay
	if got := engine.pendingPickupDispatches[0].ActivateAtMS; got != wantActivateAt {
		t.Fatalf("column-2 activation = %d, want %d", got, wantActivateAt)
	}
	engine.elapsedMS = wantActivateAt - 1
	engine.activatePendingPickupDispatches()
	if snapshotHasPickup(engine, pickup) {
		t.Fatal("column-2 pickup became available before native bird/contact boundary")
	}
	engine.elapsedMS = wantActivateAt
	engine.activatePendingPickupDispatches()
	if !snapshotHasPickup(engine, pickup) {
		t.Fatal("column-2 pickup did not become available at native bird/contact boundary")
	}
}

func snapshotHasPickup(engine *Engine, want Pickup) bool {
	for _, pickup := range engine.Pickups() {
		if pickup.SceneID == want.SceneID && pickup.Cell == want.Cell && pickup.State == PickupAvailable {
			return true
		}
	}
	return false
}

func TestNativeAuthorityNeverAuthorsBirdRedispatch(t *testing.T) {
	config := testConfig()
	config.Rules.NativeOutcomeAuthority = true
	config.Rules.RoundDurationMS = 60_000
	cell := Cell{Row: 0, Col: 2}
	config.Pickups = []Pickup{{SceneID: SceneBombCapacitySmall, Cell: cell, State: PickupAvailable}}
	engine := mustEngine(t, config)
	events := engine.destroyBlastObjectsAtCell(cell, 1, 1)
	if !hasEvent(events, EventPickupDestroyed, 0) || len(engine.recycledPickupSceneIDs) != 0 {
		t.Fatalf("native authority created a Go dispatch queue: events=%+v queue=%+v", events, engine.recycledPickupSceneIDs)
	}
	engine.elapsedMS = nativePickupDispatchIntervalMS
	if dispatched := engine.dispatchRecycledPickups(); len(dispatched) != 0 {
		t.Fatalf("native authority authored dispatch events: %+v", dispatched)
	}
}

func (engine *Engine) deathDropCellOccupiedExcludingPickup(cell Cell, except Pickup) bool {
	for _, actor := range engine.actors {
		if actor.State != ActorEliminated && engine.positionOverlapsCell(actor.Position, cell) {
			return true
		}
	}
	for _, pickup := range engine.pickups {
		if pickup == except {
			continue
		}
		if pickup.State != PickupCollected && pickup.Cell == cell {
			return true
		}
	}
	for _, object := range engine.fieldObjects {
		if object.Cell == cell {
			return true
		}
	}
	return engine.bombAt(cell) >= 0
}
