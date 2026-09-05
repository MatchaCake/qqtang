package battleengine

import "fmt"

// VerifiedExplodedBomb is one bomb entry from an authenticated native 0x0FA4.
// BombID is the local deterministic mirror ID resolved by the transport
// adapter; zero is permitted when the native event repairs a previously
// missing placement while its cell and exact blast bounds remain useful.
type VerifiedExplodedBomb struct {
	BombID      uint32
	OwnerID     uint16
	Cell        Cell
	BlastRowMin int16
	BlastRowMax int16
	BlastColMin int16
	BlastColMax int16
}

// VerifiedMapElementHit is one exact destroyed map-element entry carried by
// 0x0FA4. The original receiver resolves Row/Column and immediately invokes
// the object's destruction callback.
type VerifiedMapElementHit struct {
	MapElementID uint32
	Cell         Cell
}

// ApplyVerifiedMovementCheckpoint aligns one actor to an absolute position
// decoded from an original-client recording or authenticated live mirror.
// Policies themselves must still advance actors only through Step.
//
// A cell-changing correction invalidates the native one-cell pass state because
// that state describes continuous contact with a specific obstacle at the old
// location. Same-cell pixel corrections preserve it so a harmless packet-level
// quantization difference does not erase a genuine 500..600 ms charge window.
func (engine *Engine) ApplyVerifiedMovementCheckpoint(playerID uint16, position Position) error {
	if engine == nil {
		return fmt.Errorf("battle engine is nil")
	}
	if position.X < 0 || position.Y < 0 || position.X >= int32(engine.grid.Width)*CellSizePixels || position.Y >= int32(engine.grid.Height)*CellSizePixels {
		return fmt.Errorf("movement checkpoint for player %d is outside map at %d,%d", playerID, position.X, position.Y)
	}
	cell := position.Cell()
	if _, inside := engine.grid.Cell(cell); !inside {
		return fmt.Errorf("movement checkpoint for player %d is outside map at %d,%d", playerID, position.X, position.Y)
	}
	for index := range engine.actors {
		actor := &engine.actors[index]
		if actor.PlayerID != playerID {
			continue
		}
		if actor.State == ActorEliminated {
			return fmt.Errorf("movement checkpoint for eliminated player %d", playerID)
		}
		if actor.Position.Cell() != cell {
			actor.NativePassCollisionValid = false
			actor.NativePassCollisionCell = Cell{}
			actor.NativePassCollisionStartedAt = 0
			actor.NativePassCollisionLastAt = 0
			actor.NativePassActive = false
			actor.NativePassStartedAt = 0
			actor.NativePassDurationMS = 0
		}
		actor.nativePreviousPosition = actor.Position
		actor.Position = position
		actor.moveRemainder = 0
		return nil
	}
	return fmt.Errorf("movement checkpoint player %d is not a participant", playerID)
}

// ApplyVerifiedBombPlacement installs one placement already confirmed by an
// original-client event stream or authenticated live mirror. It deliberately bypasses local capacity and
// occupancy predicates: strict Run/Step covers those producer-side rules,
// while event-aligned replay needs the recorded bomb as a fixed input to test
// fuse, chain, flame, wall, pickup and actor outcomes independently.
func (engine *Engine) ApplyVerifiedBombPlacement(playerID uint16, cell Cell, power uint16, property byte) (Event, error) {
	return engine.ApplyVerifiedBombPlacementAt(playerID, cell, power, property, engine.elapsedMS)
}

// ApplyVerifiedBombPlacementAt retains the native placement timestamp. Live
// QQTang ClientTime uses the round clock, so fuse expiry must be based on that
// value rather than on whichever server tick happened to receive the packet.
func (engine *Engine) ApplyVerifiedBombPlacementAt(playerID uint16, cell Cell, power uint16, property byte, placedAtMS uint32) (Event, error) {
	if engine == nil {
		return Event{}, fmt.Errorf("battle engine is nil")
	}
	if power == 0 || power > 0xff {
		return Event{}, fmt.Errorf("verified bomb for player %d has invalid power %d", playerID, power)
	}
	if _, inside := engine.grid.Cell(cell); !inside {
		return Event{}, fmt.Errorf("verified bomb for player %d is outside map at %d,%d", playerID, cell.Row, cell.Col)
	}
	actorIndex := engine.actorIndex(playerID)
	if actorIndex < 0 {
		return Event{}, fmt.Errorf("verified bomb player %d is not a participant", playerID)
	}
	if engine.actors[actorIndex].State != ActorActive {
		return Event{}, fmt.Errorf("verified bomb player %d is not active", playerID)
	}
	bomb := Bomb{
		ID: engine.nextBombID, OwnerID: playerID, Cell: cell, Power: byte(power),
		ExplodeAtMS: saturatingAdd(saturatingAdd(placedAtMS, engine.rules.BombFuseMS), 1), SceneFourEffect: property != 0,
	}
	engine.nextBombID++
	engine.bombs = append(engine.bombs, bomb)
	return Event{Kind: EventBombPlaced, TimeMS: engine.elapsedMS, PlayerID: playerID, BombID: bomb.ID, Cell: cell}, nil
}

// ApplyVerifiedBombExplosion reconciles a native-client-authored 0x0FA4 into a
// NativeOutcomeAuthority mirror. Explosion/map events are state-only because
// the native event has already reached every real client. It may also return
// explosion/map events only. An AI has no owning Client.exe capable of
// producing its self-hit request, so the live transport adapter derives that
// request from these verified blast bounds without committing the hit. The
// later authority-audited reliable 0x0FA5 remains the sole actor-state
// transition.
func (engine *Engine) ApplyVerifiedBombExplosion(bombs []VerifiedExplodedBomb, mapHits []VerifiedMapElementHit, destroyedItems []Pickup) ([]Event, error) {
	if engine == nil {
		return nil, fmt.Errorf("battle engine is nil")
	}
	if len(bombs) == 0 {
		return nil, fmt.Errorf("verified bomb explosion has no bombs")
	}
	for _, bomb := range bombs {
		if bomb.OwnerID == 0 {
			return nil, fmt.Errorf("verified bomb explosion has zero owner")
		}
		if _, inside := engine.grid.Cell(bomb.Cell); !inside {
			return nil, fmt.Errorf("verified exploded bomb is outside map at %d,%d", bomb.Cell.Row, bomb.Cell.Col)
		}
		if bomb.BlastRowMin > bomb.Cell.Row || bomb.BlastRowMax < bomb.Cell.Row ||
			bomb.BlastColMin > bomb.Cell.Col || bomb.BlastColMax < bomb.Cell.Col ||
			bomb.BlastRowMin < 0 || bomb.BlastColMin < 0 ||
			int(bomb.BlastRowMax) >= int(engine.grid.Height) || int(bomb.BlastColMax) >= int(engine.grid.Width) {
			return nil, fmt.Errorf("verified exploded bomb %d has invalid bounds rows %d..%d cols %d..%d", bomb.BombID, bomb.BlastRowMin, bomb.BlastRowMax, bomb.BlastColMin, bomb.BlastColMax)
		}
	}
	for _, hit := range mapHits {
		if hit.MapElementID == 0 {
			return nil, fmt.Errorf("verified map-element hit has zero ID")
		}
		if _, inside := engine.grid.Cell(hit.Cell); !inside {
			return nil, fmt.Errorf("verified map-element %d is outside map at %d,%d", hit.MapElementID, hit.Cell.Row, hit.Cell.Col)
		}
	}
	for _, pickup := range destroyedItems {
		if pickup.SceneID == 0 {
			return nil, fmt.Errorf("verified destroyed pickup has zero scene ID")
		}
		if _, inside := engine.grid.Cell(pickup.Cell); !inside {
			return nil, fmt.Errorf("verified destroyed pickup %d is outside map at %d,%d", pickup.SceneID, pickup.Cell.Row, pickup.Cell.Col)
		}
	}

	events := make([]Event, 0, len(bombs)+len(mapHits))
	explodedIDs := make(map[uint32]struct{}, len(bombs))
	for _, bomb := range bombs {
		if bomb.BombID != 0 {
			explodedIDs[bomb.BombID] = struct{}{}
		}
		events = append(events, Event{
			Kind: EventBombExploded, TimeMS: engine.elapsedMS, PlayerID: bomb.OwnerID,
			BombID: bomb.BombID, Cell: bomb.Cell,
			BlastRowMin: bomb.BlastRowMin, BlastRowMax: bomb.BlastRowMax,
			BlastColMin: bomb.BlastColMin, BlastColMax: bomb.BlastColMax,
		})
		for col := bomb.BlastColMin; col <= bomb.BlastColMax; col++ {
			engine.addFlame(Cell{Row: bomb.Cell.Row, Col: col}, bomb.OwnerID)
		}
		for row := bomb.BlastRowMin; row <= bomb.BlastRowMax; row++ {
			if row != bomb.Cell.Row {
				engine.addFlame(Cell{Row: row, Col: bomb.Cell.Col}, bomb.OwnerID)
			}
		}
	}
	if len(explodedIDs) != 0 {
		kept := engine.bombs[:0]
		for _, bomb := range engine.bombs {
			if _, exploded := explodedIDs[bomb.ID]; !exploded {
				kept = append(kept, bomb)
			}
		}
		engine.bombs = kept
	}

	// FUN_006075d0 destroys every listed map object. Hidden wall contents are
	// not serialized again in 0x0FA4: every peer already constructed the same
	// hidden ItemSeed/NewItems layout at GAME_BEGIN. Revealing that existing
	// hidden object here keeps the AI mirror on the same information boundary as
	// the real arbitrator without exposing it one tick early to the policy.
	for _, hit := range mapHits {
		event, cells, changed, err := engine.applyVerifiedMapElementHit(hit)
		if err != nil {
			return events, err
		}
		if changed {
			events = append(events, event)
		}
		for _, cell := range cells {
			engine.retireFieldObjectsAtCell(cell)
			if revealed, ok := engine.revealPickupAt(cell, 0, 0); ok {
				events = append(events, revealed)
			}
		}
	}

	// FUN_00607760 consumes the third 0x0FA4 vector after map destruction. It
	// finds each already-visible scene object by ItemID/row/column and removes
	// it (FUN_005d9a97/FUN_005cf798). Installing these entries as new pickups was
	// the root cause of virtual actors chasing objects the arbitrator had just
	// destroyed and receiving 0x0FAD PlayerID=0xffff.
	for _, pickup := range destroyedItems {
		engine.retirePendingPickupDispatchesAtCell(pickup.Cell)
		engine.retirePickupsAtCell(pickup.Cell)
		engine.retireFieldObjectsAtCell(pickup.Cell)
	}
	return events, nil
}

// ApplyVerifiedPickupDispatch schedules the combined QQT_GAME_ITEM and
// ITEM_FROM_SERVER targets of an authenticated 0x0FAE. Native clients do not
// install these entries immediately: their bird crosses columns right-to-left
// and each created object remains non-pickable in state 3 while it moves.
// dispatchTime is the 0x0FAE scene clock and therefore remains correct even if
// the mirror consumes a delayed packet after its normal causal frame.
func (engine *Engine) ApplyVerifiedPickupDispatch(dispatchTime uint32, dispatched []Pickup) error {
	if engine == nil {
		return fmt.Errorf("battle engine is nil")
	}
	seen := make(map[Cell]struct{}, len(dispatched))
	for index, pickup := range dispatched {
		if pickup.SceneID == 0 {
			return fmt.Errorf("verified dispatched pickup %d has zero scene ID", index)
		}
		if _, _, supported := supportedPickupEffect(pickup.SceneID); !supported && !nativeFieldSceneID(pickup.SceneID) {
			return fmt.Errorf("verified dispatched pickup %d uses unsupported scene ID %d", index, pickup.SceneID)
		}
		tile, inside := engine.grid.Cell(pickup.Cell)
		if !inside || tile.Kind != CellOpen {
			return fmt.Errorf("verified dispatched pickup %d cell %d,%d is not open", index, pickup.Cell.Row, pickup.Cell.Col)
		}
		if _, duplicate := seen[pickup.Cell]; duplicate {
			return fmt.Errorf("verified dispatched pickups repeat cell %d,%d", pickup.Cell.Row, pickup.Cell.Col)
		}
		if pickup.Cell.Col < 0 || pickup.Cell.Col >= nativePickupDispatchColumns {
			return fmt.Errorf("verified dispatched pickup %d column %d is outside native bird range 0..%d", index, pickup.Cell.Col, nativePickupDispatchColumns-1)
		}
		seen[pickup.Cell] = struct{}{}
	}
	for _, pickup := range dispatched {
		engine.schedulePickupDispatch(dispatchTime, pickup)
	}
	engine.activatePendingPickupDispatches()
	return nil
}

// ApplyVerifiedItemDestruction reconciles one authenticated 0x0FBF. The
// native row/column is the scene-object identity boundary: if the restricted
// mirror retained a different item ID at that exact cell, it is stale too and
// must be retired. Field objects share the same native scene cell lifecycle,
// so an exploded banana/trap/slow-glue object is removed here as well.
func (engine *Engine) ApplyVerifiedItemDestruction(destroyed []Pickup) error {
	if engine == nil {
		return fmt.Errorf("battle engine is nil")
	}
	seen := make(map[Cell]struct{}, len(destroyed))
	for index, item := range destroyed {
		if item.SceneID == 0 {
			return fmt.Errorf("verified destroyed item %d has zero scene ID", index)
		}
		if _, inside := engine.grid.Cell(item.Cell); !inside {
			return fmt.Errorf("verified destroyed item %d cell %d,%d is outside map", index, item.Cell.Row, item.Cell.Col)
		}
		if _, duplicate := seen[item.Cell]; duplicate {
			return fmt.Errorf("verified destroyed items repeat cell %d,%d", item.Cell.Row, item.Cell.Col)
		}
		seen[item.Cell] = struct{}{}
	}
	for _, item := range destroyed {
		engine.retirePendingPickupDispatchesAtCell(item.Cell)
		engine.retirePickupsAtCell(item.Cell)
		engine.retireFieldObjectsAtCell(item.Cell)
	}
	return nil
}

func (engine *Engine) applyVerifiedMapElementHit(hit VerifiedMapElementHit) (Event, []Cell, bool, error) {
	tile, _ := engine.grid.Cell(hit.Cell)
	if tile.Kind == CellOpen {
		return Event{}, nil, false, nil
	}
	if tile.Kind != CellBreakable || tile.MapElementID != hit.MapElementID {
		// The native event remains authoritative even if an incomplete mirror
		// lacks this map element. Do not fabricate geometry from an asset ID, but
		// also do not reject the bomb and visible-item destruction facts that can
		// be reconciled exactly. The hidden seed stays hidden because this mirror
		// could not prove which local map object owned it.
		return Event{}, nil, false, nil
	}
	footprint := []Cell{hit.Cell}
	if tile.ElementWidth != 0 && tile.ElementHeight != 0 {
		footprint = footprint[:0]
		for row := int16(0); row < int16(tile.ElementHeight); row++ {
			for col := int16(0); col < int16(tile.ElementWidth); col++ {
				cell := Cell{Row: tile.ElementAnchor.Row + row, Col: tile.ElementAnchor.Col + col}
				member, inside := engine.grid.Cell(cell)
				if !inside || member.MapElementID != tile.MapElementID || member.ElementAnchor != tile.ElementAnchor {
					return Event{}, nil, false, fmt.Errorf("verified map-element %d has inconsistent footprint", hit.MapElementID)
				}
				footprint = append(footprint, cell)
			}
		}
	}
	// FUN_006075d0 is the original 0x0FA4 receiver. It looks up every listed
	// object by Row/Column and calls its destroy callback immediately; the list
	// does not describe a merely damaged durable wall.
	for _, cell := range footprint {
		index := engine.gridIndex(cell)
		engine.grid.Cells[index] = Tile{Kind: CellOpen, FlamePassable: true}
	}
	return Event{Kind: EventCellDestroyed, TimeMS: engine.elapsedMS, Cell: hit.Cell, ObjectID: hit.MapElementID}, footprint, true, nil
}
