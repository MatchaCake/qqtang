package battleengine

const (
	nativePickupDispatchIntervalMS uint32 = 30_000
	nativePickupBirdStartX         int32  = 800
	nativePickupBirdSpeedXPerMS    int32  = 1 // represented as 1/5 protocol-clock pixel per ms
	nativePickupBirdSpeedDivisor   int32  = 5
	// The native movement parameter controls only the client's presentation and
	// is not a peer-to-peer synchronization delay. Keep the AI observation guard
	// explicit and small; authority-result consumption is reconciled separately.
	nativePickupVisibilityDelayMS uint32 = 300
	nativePickupDispatchColumns          = 15
)

// pendingPickupDispatch is an authenticated 0x0FAE target that does not have
// a pickable scene object yet. Client FUN_005e24ef starts the bird at x=800;
// FUN_005e259d creates the object only when the bird enters its target column,
// The created object remains in native state 3 while its presentation runs.
// Neither live AI nor training observes the authenticated target until the
// explicit visibility delay has elapsed.
type pendingPickupDispatch struct {
	Pickup       Pickup
	ActivateAtMS uint32
}

func nativePickupDispatchDelayMS(col int16) uint32 {
	// The updater notices a target column c after crossing x=(c+1)*40 from
	// x=800 at 0.2 px per scene-clock millisecond. FUN_005e259d first multiplies
	// its millisecond delta by 5, then multiplies by Client.exe float32 0.04 at
	// VA 0x007a1e08. All terms are integral multiples of 200 ms for 40-pixel
	// columns.
	distance := nativePickupBirdStartX - int32(col+1)*CellSizePixels
	if distance < 0 {
		distance = 0
	}
	flightMS := uint32(distance * nativePickupBirdSpeedDivisor / nativePickupBirdSpeedXPerMS)
	return saturatingAdd(flightMS, nativePickupVisibilityDelayMS)
}

func (engine *Engine) schedulePickupDispatch(dispatchTime uint32, pickup Pickup) {
	engine.retirePendingPickupDispatchesAtCell(pickup.Cell)
	// A correctly mirrored target cell was already cleared by the preceding
	// destruction. Retiring any stale local object now prevents the AI from
	// contacting a scene lifetime which the authority no longer has while the
	// replacement is still flying.
	engine.retirePickupsAtCell(pickup.Cell)
	engine.retireFieldObjectsAtCell(pickup.Cell)
	pickup.State = PickupAvailable
	engine.pendingPickupDispatches = append(engine.pendingPickupDispatches, pendingPickupDispatch{
		Pickup: pickup, ActivateAtMS: saturatingAdd(dispatchTime, nativePickupDispatchDelayMS(pickup.Cell.Col)),
	})
}

func (engine *Engine) activatePendingPickupDispatches() {
	if engine == nil || len(engine.pendingPickupDispatches) == 0 {
		return
	}
	kept := engine.pendingPickupDispatches[:0]
	for _, pending := range engine.pendingPickupDispatches {
		if pending.ActivateAtMS > engine.elapsedMS {
			kept = append(kept, pending)
			continue
		}
		if nativeFieldSceneID(pending.Pickup.SceneID) {
			engine.installAuthoritativeFieldObject(uint8(pending.Pickup.SceneID), pending.Pickup.Cell)
			continue
		}
		engine.retireFieldObjectsAtCell(pending.Pickup.Cell)
		engine.installAuthoritativePickup(pending.Pickup)
	}
	engine.pendingPickupDispatches = kept
}

func (engine *Engine) retirePendingPickupDispatchesAtCell(cell Cell) {
	if engine == nil || len(engine.pendingPickupDispatches) == 0 {
		return
	}
	kept := engine.pendingPickupDispatches[:0]
	for _, pending := range engine.pendingPickupDispatches {
		if pending.Pickup.Cell != cell {
			kept = append(kept, pending)
		}
	}
	engine.pendingPickupDispatches = kept
}

// destroyBlastObjectsAtCell mirrors the ordinary-rule portion of the native
// 0x0FBF producer. An already visible collectible is removed and queued for
// the client's bird/dispatcher. A pickup revealed from the wall by this same
// blast is intentionally processed later and therefore survives this flame.
// Placed field objects are destructible but their consumed action is not
// returned to the collectible queue.
func (engine *Engine) destroyBlastObjectsAtCell(cell Cell, ownerID uint16, bombID uint32) []Event {
	if engine == nil {
		return nil
	}
	events := []Event{}
	for index := range engine.pickups {
		pickup := &engine.pickups[index]
		if pickup.Cell != cell || pickup.State != PickupAvailable {
			continue
		}
		pickup.State = PickupCollected
		if !engine.rules.NativeOutcomeAuthority {
			engine.recycledPickupSceneIDs = append(engine.recycledPickupSceneIDs, pickup.SceneID)
		}
		events = append(events, Event{
			Kind: EventPickupDestroyed, TimeMS: engine.elapsedMS, PlayerID: ownerID,
			BombID: bombID, Cell: cell, SceneID: pickup.SceneID,
		})
	}
	engine.retireFieldObjectsAtCell(cell)
	return events
}

// dispatchRecycledPickups mirrors Client+0x1E280C/0x20E288 for the ordinary
// rule: the immediate queue becomes eligible every 30 seconds, open cells are
// selected without replacement, at most 64 ordinary entries are serialized,
// and the attempted batch is cleared. Live native-authority mirrors never run
// this producer; they install the authenticated 0x0FAE instead.
func (engine *Engine) dispatchRecycledPickups() []Event {
	if engine == nil || engine.rules.NativeOutcomeAuthority || len(engine.recycledPickupSceneIDs) == 0 {
		return nil
	}
	if engine.elapsedMS-engine.lastPickupDispatchMS < nativePickupDispatchIntervalMS {
		return nil
	}
	engine.lastPickupDispatchMS = engine.elapsedMS
	count := len(engine.recycledPickupSceneIDs)
	if count > nativeDeathDropMaximum {
		count = nativeDeathDropMaximum
	}
	cells := engine.nativeWholeMapDropCells(count)
	if len(cells) < count {
		count = len(cells)
	}
	events := make([]Event, 0, count)
	for index := 0; index < count; index++ {
		pickup := Pickup{SceneID: engine.recycledPickupSceneIDs[index], Cell: cells[index], State: PickupAvailable}
		engine.schedulePickupDispatch(engine.elapsedMS, pickup)
		events = append(events, Event{
			Kind: EventPickupDispatched, TimeMS: engine.elapsedMS,
			Cell: pickup.Cell, Position: PositionAtCellCenter(pickup.Cell), SceneID: pickup.SceneID,
		})
	}
	// FUN_005e2d0a clears the whole immediate vector after one dispatch attempt,
	// including entries beyond the protocol/cell capacity.
	engine.recycledPickupSceneIDs = nil
	return events
}

func (engine *Engine) nativeWholeMapDropCells(count int) []Cell {
	if count <= 0 {
		return nil
	}
	candidates := make([]Cell, 0, int(engine.grid.Width)*int(engine.grid.Height))
	for row := int16(0); row < int16(engine.grid.Height); row++ {
		for col := int16(0); col < int16(engine.grid.Width); col++ {
			cell := Cell{Row: row, Col: col}
			tile, _ := engine.grid.Cell(cell)
			if tile.Kind != CellOpen || tile.MapElementOccupied || engine.bombAt(cell) >= 0 || engine.deathDropCellOccupied(cell) {
				continue
			}
			candidates = append(candidates, cell)
		}
	}
	for len(candidates) > count {
		remove := int(engine.nextSimulationRandom() % uint64(len(candidates)))
		copy(candidates[remove:], candidates[remove+1:])
		candidates = candidates[:len(candidates)-1]
	}
	return candidates
}
