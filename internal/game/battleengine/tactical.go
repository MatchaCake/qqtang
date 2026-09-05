package battleengine

import "fmt"

// NoDangerImpact marks a cell with no known impact inside a forecast horizon.
const NoDangerImpact uint32 = ^uint32(0)

// DangerTimeline is a deterministic projection of already-existing bombs and
// action projectiles. Values are absolute engine times, not wall-clock times.
// It deliberately does not predict actions that another participant has not
// taken yet.
type DangerTimeline struct {
	GeneratedAtMS    uint32
	HorizonMS        uint32
	Width            uint16
	Height           uint16
	EarliestImpactMS []uint32
	LatestImpactMS   []uint32
	SafeAfterMS      []uint32
	ImpactWaves      []uint8
}

// ImpactAt returns the earliest known flame impact for a map cell.
func (timeline DangerTimeline) ImpactAt(cell Cell) (uint32, bool) {
	return timeline.valueAt(timeline.EarliestImpactMS, cell)
}

// LastImpactAt returns the final currently predictable flame impact for a
// cell. Multiple bombs may cover one cell at different times even when the
// first impact is escaped successfully.
func (timeline DangerTimeline) LastImpactAt(cell Cell) (uint32, bool) {
	return timeline.valueAt(timeline.LatestImpactMS, cell)
}

// ClearAt returns the first millisecond strictly after the last known damage
// impact on the cell. Visible flame lifetime is not a damage interval. The
// earliest/latest summary remains conservative between separated future waves.
func (timeline DangerTimeline) ClearAt(cell Cell) (uint32, bool) {
	return timeline.valueAt(timeline.SafeAfterMS, cell)
}

// WaveCountAt returns the number of distinct predicted impact instants. Two
// overlapping arms in the same engine tick are one tactical wave.
func (timeline DangerTimeline) WaveCountAt(cell Cell) uint8 {
	if cell.Row < 0 || cell.Col < 0 || int(cell.Row) >= int(timeline.Height) || int(cell.Col) >= int(timeline.Width) {
		return 0
	}
	index := int(cell.Row)*int(timeline.Width) + int(cell.Col)
	if index < 0 || index >= len(timeline.ImpactWaves) {
		return 0
	}
	return timeline.ImpactWaves[index]
}

func (timeline DangerTimeline) valueAt(values []uint32, cell Cell) (uint32, bool) {
	if cell.Row < 0 || cell.Col < 0 || int(cell.Row) >= int(timeline.Height) || int(cell.Col) >= int(timeline.Width) {
		return 0, false
	}
	index := int(cell.Row)*int(timeline.Width) + int(cell.Col)
	if index < 0 || index >= len(values) || values[index] == NoDangerImpact {
		return 0, false
	}
	return values[index], true
}

// DangerTimeline advances an actor-free clone through the same bomb, chain,
// wall-damage and projectile-resolution functions used by Step. Skipping the
// actor and outcome phases prevents a terminal player state from truncating a
// purely environmental forecast while preserving production explosion rules.
func (engine *Engine) DangerTimeline(horizonMS uint32) (DangerTimeline, error) {
	return engine.dangerTimelineWithAdditionalBomb(horizonMS, nil)
}

// dangerTimelineWithAdditionalBomb projects one already-validated placement
// without first cloning the complete engine merely to append it. The forecast
// itself still advances an isolated clone through the production explosion
// functions, so ordinary and counterfactual timelines share identical rules.
func (engine *Engine) dangerTimelineWithAdditionalBomb(
	horizonMS uint32,
	additional *Bomb,
) (DangerTimeline, error) {
	if engine == nil {
		return DangerTimeline{}, fmt.Errorf("battle engine is nil")
	}
	if horizonMS == 0 {
		return DangerTimeline{}, fmt.Errorf("danger horizon must be non-zero")
	}
	timeline := DangerTimeline{
		GeneratedAtMS:    engine.elapsedMS,
		HorizonMS:        horizonMS,
		Width:            engine.grid.Width,
		Height:           engine.grid.Height,
		EarliestImpactMS: make([]uint32, len(engine.grid.Cells)),
		LatestImpactMS:   make([]uint32, len(engine.grid.Cells)),
		SafeAfterMS:      make([]uint32, len(engine.grid.Cells)),
		ImpactWaves:      make([]uint8, len(engine.grid.Cells)),
	}
	for index := range timeline.EarliestImpactMS {
		timeline.EarliestImpactMS[index] = NoDangerImpact
		timeline.LatestImpactMS[index] = NoDangerImpact
		timeline.SafeAfterMS[index] = NoDangerImpact
	}
	recordImpact := func(cell Cell, impactAt uint32) {
		index := int(cell.Row)*int(timeline.Width) + int(cell.Col)
		if index < 0 || index >= len(timeline.EarliestImpactMS) {
			return
		}
		if timeline.EarliestImpactMS[index] == NoDangerImpact || impactAt < timeline.EarliestImpactMS[index] {
			timeline.EarliestImpactMS[index] = impactAt
		}
		if timeline.LatestImpactMS[index] == NoDangerImpact || impactAt > timeline.LatestImpactMS[index] {
			timeline.LatestImpactMS[index] = impactAt
			if timeline.ImpactWaves[index] < ^uint8(0) {
				timeline.ImpactWaves[index]++
			}
		}
		safeAfter := saturatingAdd(impactAt, 1)
		if timeline.SafeAfterMS[index] == NoDangerImpact || safeAfter > timeline.SafeAfterMS[index] {
			timeline.SafeAfterMS[index] = safeAfter
		}
	}
	// Existing flames are visual residue, not a new hit at GeneratedAtMS.
	// Current-clock bombs can still be due in the next pre-movement phase.
	// Re-arming the old flame objects here
	// contradicts applyFlameHazards and prevents source-valid post-blast entry.
	// A later explosion on the same cell is still recorded by the clone below.
	clone := engine.Clone()
	if additional != nil {
		clone.bombs = append(clone.bombs, *additional)
	}
	deadline := saturatingAdd(engine.elapsedMS, horizonMS)
	for clone.elapsedMS <= deadline && (len(clone.bombs) != 0 || len(clone.projectiles) != 0) {
		nextDue := NoDangerImpact
		for _, bomb := range clone.bombs {
			if due := bomb.EffectiveExplodeAtMS(); due < nextDue {
				nextDue = due
			}
		}
		for _, projectile := range clone.projectiles {
			if projectile.ResolveAtMS < nextDue {
				nextDue = projectile.ResolveAtMS
			}
		}
		if nextDue == NoDangerImpact {
			break
		}
		// Jump to the pre-movement boundary, including the current clock when
		// a bomb is already due. Projectile timers were updated at the end of
		// the preceding movement frame and are visible to this phase.
		delta := uint32(0)
		if nextDue > clone.elapsedMS {
			delta = nextDue - clone.elapsedMS
		}
		steps := delta / clone.rules.TickMS
		if delta%clone.rules.TickMS != 0 {
			steps++
		}
		next := saturatingAdd(clone.elapsedMS, steps*clone.rules.TickMS)
		if next > deadline || next < clone.elapsedMS {
			break
		}
		clone.elapsedMS = next
		clone.expireFlames()
		clone.resolveActionProjectiles()
		clone.explodeDueBombs()
		for _, flame := range clone.flames {
			if flame.ImpactAtMS != clone.elapsedMS {
				continue
			}
			recordImpact(flame.Cell, clone.elapsedMS)
		}
	}
	return timeline, nil
}

// SafetyMask filters the current legal-action mask by exact survival during a
// short held-input window. Placement and item use are pulses on the first
// tick; movement remains held on subsequent ticks, matching the native input
// model. This is a tactical guardrail, not a proof of survival until every
// bomb explodes: longer planning consumes DangerTimeline or a search policy.
func (engine *Engine) SafetyMask(playerID uint16, holdMS uint32) (ActionMask, error) {
	legal, err := engine.LegalActionMask(playerID)
	if err != nil {
		return ActionMask{}, err
	}
	if holdMS == 0 {
		holdMS = engine.rules.TickMS
	}
	var safe ActionMask
	for id, allowed := range legal {
		if !allowed {
			continue
		}
		action, ok := ActionFromID(playerID, ActionID(id))
		if !ok {
			return ActionMask{}, fmt.Errorf("legal action ID %d cannot be decoded", id)
		}
		clone := engine.Clone()
		clone.rules.NativeOutcomeAuthority = false
		remaining := holdMS
		first := true
		for remaining > 0 && !clone.outcome.Ended {
			pulse := action
			if !first {
				pulse.PlaceBomb = false
				pulse.UseActionID = 0
			}
			if _, stepErr := clone.Step([]Action{pulse}); stepErr != nil {
				return ActionMask{}, fmt.Errorf("simulate safety action %d: %w", id, stepErr)
			}
			actorIndex := clone.actorIndex(playerID)
			if actorIndex < 0 || clone.actors[actorIndex].State != ActorActive {
				break
			}
			if remaining <= clone.rules.TickMS {
				remaining = 0
			} else {
				remaining -= clone.rules.TickMS
			}
			first = false
		}
		// Include the impact at the hold endpoint: the next input cannot move
		// the actor before that frame's hit callback. Keep the safety window
		// closed at its deadline without inventing another movement step.
		clone.explodeDueBombs()
		clone.expireTransformations()
		clone.applyFlameHazards()
		actorIndex := clone.actorIndex(playerID)
		if actorIndex >= 0 && clone.actors[actorIndex].State == ActorActive {
			safe[id] = true
		}
	}
	return safe, nil
}
