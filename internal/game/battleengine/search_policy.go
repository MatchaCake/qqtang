package battleengine

import (
	"fmt"
	"math"
	"sort"
)

// ScoredAction is one legal policy proposal and its unnormalized model score.
// Learned inference adapters expose only the top few actions through this
// type; the tactical search never needs the full neural-network output.
type ScoredAction struct {
	Action Action
	Score  float32
}

// CandidatePolicy supplies legal actions in descending preference order.
// Implementations may be neural, scripted, or test doubles.
type CandidatePolicy interface {
	CandidateActions(observation Observation, legal []Action, limit int) ([]ScoredAction, error)
}

// SnapshotCandidatePolicy may use rule-derived public features such as the
// danger timeline. The snapshot is still a clone; candidate implementations
// must encode the supplied actor observation rather than hidden wall items.
type SnapshotCandidatePolicy interface {
	CandidateActionsWithSnapshot(snapshot *Engine, observation Observation, legal []Action, limit int) ([]ScoredAction, error)
}

// SearchConfig bounds inference-time work. A shallow search is intentionally
// used only to reject immediately unsafe or tactically wasteful policy choices;
// the learned policy remains responsible for long-horizon strategy.
type SearchConfig struct {
	TopK             int
	HorizonMS        uint32
	PriorWeight      float64
	DangerHorizonMS  uint32
	EliminationValue float64
	TrapValue        float64
	AlwaysSearch     bool
	// TacticalSafety is an offline teacher continuation. Production-aligned
	// evaluation leaves it false because the live TopKSearchPolicy does not
	// apply this stateful escape controller.
	TacticalSafety bool
}

// GreedyCandidatePolicy runs the learned actor once and accepts its strongest
// legal action. It preserves the same public danger-timeline encoder used by
// search-capable candidates, but never advances speculative engine clones.
// This is the normal low-cost live-server policy after training converges.
type GreedyCandidatePolicy struct {
	Candidate CandidatePolicy
}

func (policy GreedyCandidatePolicy) ChooseAction(observation Observation, legal []Action) (Action, error) {
	if policy.Candidate == nil {
		return Action{}, fmt.Errorf("greedy policy has no candidate policy")
	}
	candidates, err := policy.Candidate.CandidateActions(observation, legal, 1)
	if err != nil {
		return Action{}, err
	}
	if len(candidates) == 0 {
		return Action{}, fmt.Errorf("candidate policy returned no legal action")
	}
	return candidates[0].Action, nil
}

func (policy GreedyCandidatePolicy) ChooseActionWithSnapshot(snapshot *Engine, observation Observation, legal []Action) (Action, error) {
	if snapshot == nil {
		return Action{}, fmt.Errorf("greedy policy received nil engine snapshot")
	}
	if policy.Candidate == nil {
		return Action{}, fmt.Errorf("greedy policy has no candidate policy")
	}
	var candidates []ScoredAction
	var err error
	if snapshotCandidate, ok := policy.Candidate.(SnapshotCandidatePolicy); ok {
		// Runtime already supplied an isolated snapshot. The candidate only reads
		// it to derive public danger/legal features, so no second clone is needed.
		candidates, err = snapshotCandidate.CandidateActionsWithSnapshot(snapshot, observation, legal, 1)
	} else {
		candidates, err = policy.Candidate.CandidateActions(observation, legal, 1)
	}
	if err != nil {
		return Action{}, err
	}
	if len(candidates) == 0 {
		return Action{}, fmt.Errorf("candidate policy returned no legal action")
	}
	return candidates[0].Action, nil
}

func (config SearchConfig) normalized(tickMS uint32) SearchConfig {
	if config.TopK <= 0 {
		config.TopK = 4
	}
	if config.HorizonMS == 0 {
		config.HorizonMS = 600
	}
	if config.HorizonMS < tickMS {
		config.HorizonMS = tickMS
	}
	if config.PriorWeight == 0 {
		config.PriorWeight = 0.35
	}
	if config.DangerHorizonMS == 0 {
		config.DangerHorizonMS = 3_200
	}
	if config.EliminationValue == 0 {
		config.EliminationValue = 100
	}
	if config.TrapValue == 0 {
		config.TrapValue = 12
	}
	return config
}

// TopKSearchPolicy re-ranks a learned policy's strongest actions by advancing
// clones of the authoritative Go engine. Other actors are held stationary in
// this deliberately small search; it is a tactical safety/value correction,
// not an alternative combat engine or a hidden-information planner.
type TopKSearchPolicy struct {
	Candidate CandidatePolicy
	Config    SearchConfig
}

// TacticalSafetyPolicy turns the escape continuation used by speculative
// Top-K search into an action sequence that is actually executed. Without this
// stateful boundary, every model decision was evaluated under the assumption
// that later frames would follow tacticalEscapeRollout, while the next live
// decision was free to reverse course into the same blast again.
//
// The wrapper engages only after the actor occupies a cell covered by an
// already-known explosion wave and that wave enters EngageWithinMS. Before
// that deadline, standing on one's own bubble is a legitimate native tactic:
// it reserves four exits and supports multi-bubble pressure. Once engaged, the
// wrapper follows the same time-feasible route used by search, then rejects
// only movements that would re-enter a cell covered by that still-live wave.
// Safe movement, placement and item use remain under the learned policy.
// Pulses are suppressed only when emergency escape replaces model movement.
// One instance must be allocated per virtual actor because the escape latch is
// deliberately actor-local mutable state.
type TacticalSafetyPolicy struct {
	Base           Policy
	HorizonMS      uint32
	EngageWithinMS uint32

	engagedUntilMS uint32
}

func (policy *TacticalSafetyPolicy) ChooseAction(observation Observation, legal []Action) (Action, error) {
	if policy == nil || policy.Base == nil {
		return Action{}, fmt.Errorf("tactical safety policy has no base policy")
	}
	return policy.Base.ChooseAction(observation, legal)
}

func (policy *TacticalSafetyPolicy) ChooseActionWithSnapshot(snapshot *Engine, observation Observation, legal []Action) (Action, error) {
	if policy == nil || policy.Base == nil {
		return Action{}, fmt.Errorf("tactical safety policy has no base policy")
	}
	if snapshot == nil {
		return Action{}, fmt.Errorf("tactical safety policy received nil engine snapshot")
	}
	var (
		chosen Action
		err    error
	)
	if snapshotPolicy, ok := policy.Base.(SnapshotPolicy); ok {
		chosen, err = snapshotPolicy.ChooseActionWithSnapshot(snapshot, observation, legal)
	} else {
		chosen, err = policy.Base.ChooseAction(observation, legal)
	}
	if err != nil {
		return Action{}, err
	}
	if !containsAction(legal, chosen) {
		return Action{}, fmt.Errorf("tactical safety base returned illegal action %+v", chosen)
	}

	horizonMS := policy.HorizonMS
	if horizonMS == 0 {
		horizonMS = saturatingAdd(snapshot.rules.BombFuseMS, snapshot.rules.FlameDurationMS)
	}
	actorIndex := snapshot.actorIndex(observation.PlayerID)
	if actorIndex < 0 || snapshot.actors[actorIndex].State != ActorActive {
		policy.engagedUntilMS = 0
		return chosen, nil
	}
	timeline, timelineErr := snapshot.DangerTimeline(horizonMS)
	if timelineErr != nil {
		return Action{}, timelineErr
	}
	actorCell := snapshot.actors[actorIndex].Position.Cell()
	impactAt, cellThreatened := timeline.ImpactAt(actorCell)
	direction, threatened, found, escapeAt := tacticalEscapePlan(snapshot, observation.PlayerID, horizonMS)
	engageWithinMS := policy.EngageWithinMS
	if found && escapeAt > snapshot.elapsedMS {
		// The fixed deadline is only the minimum reaction margin. A slow actor or
		// a long blast arm may need several cells before it is genuinely outside
		// the wave; waiting until the final 400 ms made the newly restored
		// stand-on-bubble tactic periodically commit suicide. Start exactly early
		// enough for the proven route plus two physics ticks of projection slack.
		routeLeadMS := escapeAt - snapshot.elapsedMS
		routeLeadMS = saturatingAdd(routeLeadMS, snapshot.rules.TickMS*2)
		if routeLeadMS > engageWithinMS {
			engageWithinMS = routeLeadMS
		}
	}
	engageAt := saturatingAdd(snapshot.elapsedMS, engageWithinMS)
	if policy.engagedUntilMS <= snapshot.elapsedMS && (!cellThreatened || impactAt > engageAt) {
		// Future danger is policy context, not a command to flee immediately.
		// Do not destroy the native stand-on-bubble tactic during the seconds
		// before the final escape deadline.
		return chosen, nil
	}
	if threatened {
		policy.engagedUntilMS = latestDangerClearMS(timeline, snapshot.elapsedMS)
		if found {
			return tacticalSafetyAction(legal, observation.PlayerID, direction, chosen), nil
		}
		// There is no proven route from this exact state. Do not compound the
		// hazard with another placement/use pulse, but preserve the model's
		// legal movement because it may still be the only native escape chance.
		chosen.PlaceBomb = false
		chosen.UseActionID = 0
		return legalEquivalentOrFallback(legal, chosen), nil
	}
	if policy.engagedUntilMS > snapshot.elapsedMS {
		// The route has reached a currently safe cell. Preserve every safe model
		// movement, but do not let the next decision immediately reverse into a
		// cell that an already-visible bomb will cover. Preserve safe placement
		// and item pulses; suppressing them until the first wave cleared created
		// the observed deterministic one-bubble-at-a-time behaviour.
		guarded, guardErr := tacticalGuardKnownDanger(snapshot, timeline, observation.PlayerID, legal, chosen)
		if guardErr != nil {
			return Action{}, guardErr
		}
		return guarded, nil
	}
	policy.engagedUntilMS = 0
	return chosen, nil
}

func tacticalGuardKnownDanger(snapshot *Engine, timeline DangerTimeline, playerID uint16, legal []Action, chosen Action) (Action, error) {
	chosen = legalEquivalentOrFallback(legal, chosen)
	safe, err := tacticalActionDestinationSafe(snapshot, timeline, playerID, chosen)
	if err != nil {
		return Action{}, err
	}
	if safe {
		return chosen, nil
	}
	// Prefer standing on the already-proven safe cell, then another legal
	// movement whose next authoritative engine position stays outside every
	// currently known blast wave. Item/bomb pulses are deliberately excluded.
	candidates := []Action{{PlayerID: playerID, Move: DirectionNone}}
	for _, direction := range []Direction{DirectionRight, DirectionUp, DirectionLeft, DirectionDown} {
		candidates = append(candidates, Action{PlayerID: playerID, Move: direction})
	}
	for _, candidate := range candidates {
		if !containsAction(legal, candidate) {
			continue
		}
		candidateSafe, candidateErr := tacticalActionDestinationSafe(snapshot, timeline, playerID, candidate)
		if candidateErr != nil {
			return Action{}, candidateErr
		}
		if candidateSafe {
			return candidate, nil
		}
	}
	return tacticalSafetyAction(legal, playerID, DirectionNone, chosen), nil
}

func tacticalActionDestinationSafe(snapshot *Engine, timeline DangerTimeline, playerID uint16, action Action) (bool, error) {
	clone := snapshot.Clone()
	if _, err := clone.Step([]Action{action}); err != nil {
		return false, fmt.Errorf("simulate tactical guard action: %w", err)
	}
	actorIndex := clone.actorIndex(playerID)
	if actorIndex < 0 || clone.actors[actorIndex].State != ActorActive {
		return false, nil
	}
	_, threatened := timeline.ImpactAt(clone.actors[actorIndex].Position.Cell())
	return !threatened, nil
}

func latestDangerClearMS(timeline DangerTimeline, fallback uint32) uint32 {
	latest := fallback
	for _, clearAt := range timeline.SafeAfterMS {
		if clearAt != NoDangerImpact && clearAt > latest {
			latest = clearAt
		}
	}
	return latest
}

func tacticalSafetyAction(legal []Action, playerID uint16, direction Direction, fallback Action) Action {
	wanted := Action{PlayerID: playerID, Move: direction}
	return legalEquivalentOrFallback(legal, wanted, fallback)
}

func legalEquivalentOrFallback(legal []Action, candidates ...Action) Action {
	for _, candidate := range candidates {
		if containsAction(legal, candidate) {
			return candidate
		}
	}
	if len(legal) != 0 {
		return legal[0]
	}
	return Action{}
}

func (policy TopKSearchPolicy) ChooseAction(observation Observation, legal []Action) (Action, error) {
	if policy.Candidate == nil {
		return Action{}, fmt.Errorf("top-k search has no candidate policy")
	}
	candidates, err := policy.Candidate.CandidateActions(observation, legal, 1)
	if err != nil {
		return Action{}, err
	}
	if len(candidates) == 0 {
		return Action{}, fmt.Errorf("candidate policy returned no legal action")
	}
	return candidates[0].Action, nil
}

func requiresTacticalSearch(snapshot *Engine, playerID uint16, action Action, config SearchConfig) bool {
	if action.PlaceBomb || action.UseActionID != 0 || action.Move == DirectionNone {
		return true
	}
	// Once any bomb exists, a direction that is harmless now can still end in
	// its future blast. Re-rank movement until the field is clear; otherwise a
	// policy can place a safe bomb, reverse course on the next tick, and trap
	// itself long before the short "immediate threat" window opens.
	if len(snapshot.bombs) != 0 {
		return true
	}
	actorIndex := snapshot.actorIndex(playerID)
	if actorIndex < 0 || snapshot.actors[actorIndex].State != ActorActive {
		return true
	}
	danger, err := snapshot.DangerTimeline(config.DangerHorizonMS)
	if err != nil {
		return true
	}
	impact, threatened := danger.ImpactAt(snapshot.actors[actorIndex].Position.Cell())
	return threatened && impact <= saturatingAdd(snapshot.elapsedMS, config.HorizonMS)
}

// RankedSearchAction exposes the authoritative tactical value behind one
// offline Search candidate. Survives remains separate because Search treats
// survival lexicographically rather than as an arbitrary scalar bonus.
type RankedSearchAction struct {
	Action   Action
	Value    float64
	Survives bool
}

func (policy TopKSearchPolicy) ChooseActionWithSnapshot(snapshot *Engine, observation Observation, legal []Action) (Action, error) {
	ranked, err := policy.RankActionsWithSnapshot(snapshot, observation, legal)
	if err != nil {
		return Action{}, err
	}
	if len(ranked) == 0 {
		return Action{}, fmt.Errorf("top-k search returned no ranked action")
	}
	return ranked[0].Action, nil
}

// RankActionsWithSnapshot returns the complete bounded Search ranking used by
// ChooseActionWithSnapshot. It exists for offline value-distribution
// distillation; normal live inference still consumes only the first action.
func (policy TopKSearchPolicy) RankActionsWithSnapshot(snapshot *Engine, observation Observation, legal []Action) ([]RankedSearchAction, error) {
	if snapshot == nil {
		return nil, fmt.Errorf("top-k search received nil engine snapshot")
	}
	if policy.Candidate == nil {
		return nil, fmt.Errorf("top-k search has no candidate policy")
	}
	config := policy.Config.normalized(snapshot.rules.TickMS)
	var candidates []ScoredAction
	var err error
	if snapshotCandidate, ok := policy.Candidate.(SnapshotCandidatePolicy); ok {
		candidates, err = snapshotCandidate.CandidateActionsWithSnapshot(snapshot.Clone(), observation, legal, config.TopK)
	} else {
		candidates, err = policy.Candidate.CandidateActions(observation, legal, config.TopK)
	}
	if err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("candidate policy returned no legal action")
	}
	if !config.AlwaysSearch && !requiresTacticalSearch(snapshot, observation.PlayerID, candidates[0].Action, config) {
		return []RankedSearchAction{{Action: candidates[0].Action, Value: float64(candidates[0].Score), Survives: true}}, nil
	}
	legalSet := make(map[Action]struct{}, len(legal))
	for _, action := range legal {
		legalSet[action] = struct{}{}
	}
	type ranked struct {
		action   Action
		value    float64
		prior    int
		survives bool
	}
	rankedActions := make([]ranked, 0, len(candidates))
	seenActions := make(map[Action]struct{}, len(candidates))
	for prior, candidate := range candidates {
		if _, ok := legalSet[candidate.Action]; !ok {
			return nil, fmt.Errorf("candidate policy returned illegal action %+v", candidate.Action)
		}
		value, survives, simulateErr := searchActionValue(snapshot, observation, candidate.Action, config)
		if simulateErr != nil {
			return nil, simulateErr
		}
		// Clamp arbitrary model logits before mixing them with rule values.
		modelPrior := math.Max(-20, math.Min(20, float64(candidate.Score)))
		rankedActions = append(rankedActions, ranked{candidate.Action, value + config.PriorWeight*modelPrior, prior, survives})
		seenActions[candidate.Action] = struct{}{}
	}
	// A learned top-k can contain only placement variants or only routes that
	// enter a known blast. The live safety layer must still be able to select a
	// lower-ranked legal escape; otherwise TopK silently turns into "choose the
	// least bad suicide". Expand to the remaining legal actions only when every
	// learned candidate is unsafe, keeping the normal inference path bounded.
	anyCandidateSurvives := false
	for _, candidate := range rankedActions {
		if candidate.survives {
			anyCandidateSurvives = true
			break
		}
	}
	if !anyCandidateSurvives {
		for _, action := range legal {
			if _, seen := seenActions[action]; seen {
				continue
			}
			value, survives, simulateErr := searchActionValue(snapshot, observation, action, config)
			if simulateErr != nil {
				return nil, simulateErr
			}
			rankedActions = append(rankedActions, ranked{
				action: action, value: value - 20*config.PriorWeight,
				prior: len(candidates) + len(rankedActions), survives: survives,
			})
		}
	}
	sort.SliceStable(rankedActions, func(i, j int) bool {
		if rankedActions[i].survives != rankedActions[j].survives {
			return rankedActions[i].survives
		}
		if rankedActions[i].value == rankedActions[j].value {
			return rankedActions[i].prior < rankedActions[j].prior
		}
		return rankedActions[i].value > rankedActions[j].value
	})
	result := make([]RankedSearchAction, len(rankedActions))
	for index, candidate := range rankedActions {
		result[index] = RankedSearchAction{
			Action: candidate.action, Value: candidate.value, Survives: candidate.survives,
		}
	}
	return result, nil
}

func searchActionValue(snapshot *Engine, observation Observation, action Action, config SearchConfig) (float64, bool, error) {
	clone := snapshot.Clone()
	// NativeOutcomeAuthority is a mutation boundary for the authoritative live
	// engine, not a rule saying explosions are harmless. A speculative search
	// clone has no native client callback to wait for, so it must resolve the
	// same flame hazards locally. Leaving this flag enabled made live search
	// blind to self-traps while offline tests (where the flag is false) passed.
	clone.rules.NativeOutcomeAuthority = false
	beforeActors := clone.Actors()
	beforeGrid := clone.Grid()
	remaining := config.HorizonMS
	first := true
	escape := tacticalEscapeRollout{}
	for remaining > 0 && !clone.Terminal().Ended {
		pulse := action
		if !first {
			pulse.PlaceBomb = false
			pulse.UseActionID = 0
			pulse.Move = escape.movement(clone, observation.PlayerID, config.DangerHorizonMS, pulse.Move)
		}
		if _, err := clone.Step([]Action{pulse}); err != nil {
			return 0, false, fmt.Errorf("simulate candidate action %+v: %w", action, err)
		}
		if remaining <= clone.rules.TickMS {
			break
		}
		remaining -= clone.rules.TickMS
		first = false
	}
	survives := false
	if selfIndex := clone.actorIndex(observation.PlayerID); selfIndex >= 0 {
		self := clone.actors[selfIndex]
		survives = self.State == ActorActive
		if outcome := clone.Terminal(); outcome.Ended && !outcome.Draw && outcome.WinnerTeamID == self.TeamID {
			survives = true
		}
	}
	return evaluateSearchState(clone, beforeActors, beforeGrid, observation.PlayerID, config), survives, nil
}

type tacticalEscapeRollout struct {
	initialized bool
	active      bool
	override    bool
	cell        Cell
	direction   Direction
}

// movement replans only after crossing a cell boundary. A 20 ms physics step
// may need several frames merely to centre the actor at a corner; rebuilding a
// complete DangerTimeline and route on every one of those frames is redundant
// and made Top-K cost scale with fuse milliseconds instead of route length.
func (rollout *tacticalEscapeRollout) movement(engine *Engine, playerID uint16, horizonMS uint32, fallback Direction) Direction {
	actorIndex := engine.actorIndex(playerID)
	if actorIndex < 0 {
		return fallback
	}
	cell := engine.actors[actorIndex].Position.Cell()
	if rollout.initialized && rollout.cell == cell {
		if rollout.override {
			return rollout.direction
		}
		return fallback
	}
	rollout.initialized = true
	rollout.cell = cell
	direction, threatened, found := tacticalEscapeDirection(engine, playerID, horizonMS)
	switch {
	case threatened && found:
		rollout.active = true
		rollout.override = true
		rollout.direction = direction
	case !threatened && rollout.active:
		// The bounded rollout reached a cell outside every known explosion
		// wave. Stop there instead of resuming the candidate's original held
		// direction and walking back into its own blast.
		rollout.override = true
		rollout.direction = DirectionNone
	default:
		rollout.override = false
		rollout.direction = DirectionNone
	}
	if rollout.override {
		return rollout.direction
	}
	return fallback
}

// tacticalEscapeDirection returns the first input direction of a public,
// time-feasible route from the actor's currently threatened cell to a cell
// outside all already-known explosion waves. It is used only inside disposable
// tactical rollouts: learned policy still chooses the real action every
// decision, while this continuation prevents the safety layer from declaring
// an ordinary two-turn escape impossible merely because it held one key for a
// full three-second fuse.
func tacticalEscapeDirection(engine *Engine, playerID uint16, horizonMS uint32) (direction Direction, threatened, found bool) {
	direction, threatened, found, _ = tacticalEscapePlan(engine, playerID, horizonMS)
	return direction, threatened, found
}

// tacticalEscapePlan also returns the conservative whole-cell arrival time at
// the first cell outside every known wave. Live safety uses that duration to
// choose a speed- and route-aware escape deadline; speculative search only
// needs the first direction.
func tacticalEscapePlan(engine *Engine, playerID uint16, horizonMS uint32) (direction Direction, threatened, found bool, arrivalMS uint32) {
	if engine == nil || horizonMS == 0 {
		return DirectionNone, false, false, 0
	}
	actorIndex := engine.actorIndex(playerID)
	if actorIndex < 0 || engine.actors[actorIndex].State != ActorActive || engine.actors[actorIndex].MovementStatus == MovementStatusForcedSlide {
		return DirectionNone, false, false, 0
	}
	actor := &engine.actors[actorIndex]
	timeline, err := engine.DangerTimeline(horizonMS)
	if err != nil {
		return DirectionNone, false, false, 0
	}
	start := actor.Position.Cell()
	if tacticalCellSafeAfter(timeline, start, engine.elapsedMS) {
		return DirectionNone, false, false, 0
	}

	deadline := saturatingAdd(engine.elapsedMS, horizonMS)
	arrival := map[Cell]uint32{start: engine.elapsedMS}
	firstDirection := map[Cell]Direction{start: DirectionNone}
	visited := make(map[Cell]bool)
	cardinal := [...]Direction{DirectionUp, DirectionRight, DirectionDown, DirectionLeft}
	for len(visited) < len(engine.grid.Cells) {
		current := Cell{}
		currentAt := NoDangerImpact
		hasCurrent := false
		for cell, at := range arrival {
			if visited[cell] {
				continue
			}
			if !hasCurrent || at < currentAt || (at == currentAt && (cell.Row < current.Row || (cell.Row == current.Row && cell.Col < current.Col))) {
				current, currentAt, hasCurrent = cell, at, true
			}
		}
		if !hasCurrent || currentAt > deadline {
			break
		}
		visited[current] = true
		if current != start && tacticalCellSafeAfter(timeline, current, currentAt) {
			// transformInput is an involution for the reverse-control avatar:
			// route planning stays in world coordinates while the returned action
			// remains the key direction consumed by Step.
			return engine.transformInput(actor, firstDirection[current]), true, true, currentAt
		}
		for _, worldDirection := range cardinal {
			dx, dy, _ := worldDirection.delta()
			next := Cell{Row: current.Row + int16(dy), Col: current.Col + int16(dx)}
			if visited[next] || !tacticalCellTraversable(engine, actor, start, next, worldDirection) {
				continue
			}
			speed := uint32(engine.effectiveSpeedPixelsPerSecond(actor, worldDirection))
			if speed == 0 {
				continue
			}
			travelMS := (uint32(CellSizePixels)*1000 + speed - 1) / speed
			if travelMS < engine.rules.TickMS {
				travelMS = engine.rules.TickMS
			}
			nextAt := saturatingAdd(currentAt, travelMS)
			departAt := saturatingAdd(nextAt, travelMS)
			if nextAt > deadline || !tacticalCellSafeDuring(timeline, next, nextAt, departAt) {
				continue
			}
			knownAt, known := arrival[next]
			if known && knownAt <= nextAt {
				continue
			}
			arrival[next] = nextAt
			if current == start {
				firstDirection[next] = worldDirection
			} else {
				firstDirection[next] = firstDirection[current]
			}
		}
	}
	return DirectionNone, true, false, 0
}

func tacticalCellTraversable(engine *Engine, actor *Actor, start, cell Cell, direction Direction) bool {
	tile, inside := engine.grid.Cell(cell)
	if !inside {
		return false
	}
	capabilities := engine.actorCapabilities(actor, direction)
	if tile.Kind != CellOpen && !capabilities.TraverseStaticTerrain {
		return false
	}
	if cell != start && engine.bombAt(cell) >= 0 {
		return false
	}
	for _, object := range engine.fieldObjects {
		if object.Cell == cell {
			return false
		}
	}
	return true
}

func tacticalCellSafeAfter(timeline DangerTimeline, cell Cell, arrivalMS uint32) bool {
	if _, impacted := timeline.ImpactAt(cell); !impacted {
		return true
	}
	clearAt, known := timeline.ClearAt(cell)
	return known && clearAt <= arrivalMS
}

func tacticalCellSafeDuring(timeline DangerTimeline, cell Cell, arrivalMS, departMS uint32) bool {
	impactAt, impacted := timeline.ImpactAt(cell)
	if !impacted {
		return true
	}
	if clearAt, known := timeline.ClearAt(cell); known && clearAt <= arrivalMS {
		return true
	}
	return impactAt > departMS
}

func evaluateSearchState(engine *Engine, beforeActors []Actor, beforeGrid Grid, playerID uint16, config SearchConfig) float64 {
	selfIndex := engine.actorIndex(playerID)
	if selfIndex < 0 {
		return -config.EliminationValue
	}
	self := engine.actors[selfIndex]
	value := 0.0
	if outcome := engine.Terminal(); outcome.Ended {
		switch {
		case outcome.Draw:
			value -= 5
		case outcome.WinnerTeamID == self.TeamID:
			value += config.EliminationValue
		default:
			value -= config.EliminationValue
		}
	}
	switch self.State {
	case ActorEliminated:
		value -= config.EliminationValue
	case ActorTrapped:
		value -= config.TrapValue
	}
	beforeByID := make(map[uint16]Actor, len(beforeActors))
	for _, actor := range beforeActors {
		beforeByID[actor.PlayerID] = actor
	}
	for _, actor := range engine.actors {
		before, ok := beforeByID[actor.PlayerID]
		if !ok || actor.PlayerID == playerID {
			continue
		}
		enemy := actor.TeamID != self.TeamID
		if before.State == ActorActive && actor.State == ActorTrapped {
			if enemy {
				value += config.TrapValue
			} else {
				value -= config.TrapValue
			}
		}
		if before.State != ActorEliminated && actor.State == ActorEliminated {
			if enemy {
				value += config.EliminationValue
			} else {
				value -= config.EliminationValue
			}
		}
	}
	if beforeSelf, ok := beforeByID[playerID]; ok {
		value += searchPickupProgressValue(beforeSelf, self, config)
	}
	// Reward opening destructible terrain without inspecting the hidden item
	// carried by a wall. This preserves the actor's information boundary.
	for index, before := range beforeGrid.Cells {
		if index >= len(engine.grid.Cells) {
			break
		}
		after := engine.grid.Cells[index]
		if before.Kind == CellBreakable && after.Kind == CellOpen {
			value += 0.75
		}
	}
	if self.State == ActorActive {
		if danger, err := engine.DangerTimeline(config.DangerHorizonMS); err == nil {
			if impact, ok := danger.ImpactAt(self.Position.Cell()); ok {
				remaining := float64(0)
				if impact > engine.elapsedMS {
					remaining = float64(impact-engine.elapsedMS) / float64(config.DangerHorizonMS)
				}
				value -= 18 * (1 - remaining)
			}
		}
	}
	return value
}

const searchDevelopmentUnitValue = 0.75

// searchPickupProgressValue scores only state changes that happened inside
// the disposable authoritative rollout. It does not inspect hidden wall
// contents and it does not assign a permanent preference to any map item.
// Native attribute points and granted inventory are exact finite gains; a
// newly held fork or transformation is additionally one recoverable hit.
func searchPickupProgressValue(before, after Actor, config SearchConfig) float64 {
	positiveByteDelta := func(old, current byte) float64 {
		if current <= old {
			return 0
		}
		return float64(current - old)
	}
	value := searchDevelopmentUnitValue * (positiveByteDelta(before.BombCapacity, after.BombCapacity) +
		positiveByteDelta(before.BombPower, after.BombPower) +
		positiveByteDelta(before.SpeedRate, after.SpeedRate))

	for index := 0; index < NativeBattleActionSlots; index++ {
		actionID, ok := NativeUseActionIDAt(index)
		if !ok {
			continue
		}
		oldCount := heldActionCount(before.HeldActions, actionID)
		newCount := heldActionCount(after.HeldActions, actionID)
		if newCount <= oldCount {
			continue
		}
		gain := float64(newCount - oldCount)
		value += searchDevelopmentUnitValue * gain
		if actionID == 63 {
			value += config.TrapValue * gain
		}
	}
	if after.TransformationSceneID != 0 && after.TransformationSceneID != before.TransformationSceneID {
		value += config.TrapValue
	}
	if after.MovementStatus == MovementStatusFast && before.MovementStatus != MovementStatusFast {
		value += searchDevelopmentUnitValue
	}
	if after.HiddenPickupReachExpiresAt > before.HiddenPickupReachExpiresAt {
		value += searchDevelopmentUnitValue
	}
	if after.SceneFourEffectExpiresAt > before.SceneFourEffectExpiresAt {
		value += searchDevelopmentUnitValue
	}
	if after.OxygenValue > before.OxygenValue {
		value += searchDevelopmentUnitValue * float64(after.OxygenValue-before.OxygenValue) / float64(NativeOxygenValueIncrement)
	}
	return value
}

func heldActionCount(slots [NativeBattleActionSlots]HeldActionSlot, actionID uint8) byte {
	for _, slot := range slots {
		if slot.ActionID == actionID {
			return slot.Count
		}
	}
	return 0
}
