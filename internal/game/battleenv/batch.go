package battleenv

import (
	"errors"
	"fmt"
	"runtime"
	"sort"
	"sync"

	"qqtang/internal/game/battleengine"
	"qqtang/internal/game/mapdata"
)

var defaultRoleIDs = [...]uint16{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 13, 14, 15, 16, 18, 19, 20, 21, 22}

var errCurriculumLayoutUnavailable = errors.New("curriculum layout unavailable")

type Config struct {
	Maps     []mapdata.CompetitiveMap
	EnvCount int
	// ParticipantCount is the fixed tensor capacity. ParticipantCounts may
	// select a smaller real actor count on each reset; unused tensor slots stay
	// inactive so one policy update can mix 2/4/8-player episodes without
	// changing model or checkpoint shapes.
	ParticipantCount  int
	ParticipantCounts []int
	TeamCount         int
	// TeamLayouts optionally supplies one or more explicit TeamID layouts. Each
	// layout has ParticipantCount entries and at least two teams. A deterministic
	// layout is selected on every reset. When empty, TeamCount preserves the
	// legacy equal-sized layout.
	TeamLayouts [][]uint8
	// TeamSpawnPermille mixes the client's free-room and equal two-team spawn
	// consumers when the chosen topology supports both. Uneven/multi-team
	// layouts always use the native free-room consumer.
	TeamSpawnPermille uint16
	// DuelCurriculumPermille replaces a sampled two-actor opening with a legal
	// late-game state: breakable terrain is already cleared, opponents start a
	// few cells apart and native attributes are partially developed. Combat and
	// outcome rules remain unchanged; this only makes rare endgame blockade
	// states frequent enough for policy-gradient training.
	DuelCurriculumPermille uint16
	// LateDuelReplayPermille restarts a sampled episode from an exact clone of
	// a late two-team confrontation previously reached by the same ordinary
	// environment slot. Unlike the synthetic duel courses, this preserves the
	// real map, attributes, bombs, pickups, eliminated actors and round clock.
	// The marker is training metadata only and never enters actor observations.
	LateDuelReplayPermille uint16
	// DuelCurriculumBombCapacity optionally fixes only the cleared combat
	// course capacity. Zero retains the role-derived developed value.
	DuelCurriculumBombCapacity byte
	// BlockadeCurriculumPermille samples a cleared legal duel whose two spawns
	// are both in nearby low-degree corridors. Outcomes remain ordinary combat
	// wins; the topology only makes rare escape-denial evidence frequent.
	BlockadeCurriculumPermille uint16
	// ChainFinisherCurriculumPermille samples a cleared 1v1 conversion state
	// with several staggered owned bubbles. The attacker and defender keep
	// ordinary engine actions and outcomes; the reset makes the expert rolling
	// move-to-blast-line -> late place-and-depart sequence common enough for
	// self-play to discover and counter.
	ChainFinisherCurriculumPermille uint16
	// ChainFinisherMinimumPresetRoots samples that many through all three
	// visible root bubbles. Lower values progressively require the attacker to
	// create more of the rolling setup itself. Zero preserves the original
	// fully assisted three-root course for existing callers.
	ChainFinisherMinimumPresetRoots byte
	// ChainFinisherMaximumPresetRoots caps sampled assistance. Zero preserves
	// the historical maximum of three; bounds 1..1 isolate the one-root,
	// three-trigger rolling-chain lesson.
	ChainFinisherMaximumPresetRoots byte
	// ChainFinisherTriggerOnly completes the finite capability lesson after the
	// required number of safe, flame-triggered post-reset bombs. The default
	// false additionally requires the final trigger to trap the defender.
	ChainFinisherTriggerOnly bool
	// ChainFinisherPressureOnly completes the lesson when the final verified
	// trigger's authoritative blast hits the defender or meaningfully closes
	// its open exits. It is an intermediate placement course, not a combat rule.
	ChainFinisherPressureOnly bool
	// BombEscapeCurriculumPermille samples a short, training-only capability
	// episode on native terrain with verified short escape routes. Reset places
	// one bubble under each actor through the ordinary Engine.Step path, and the
	// episode succeeds only after the configured number of owned bubbles explode
	// while their owner remains active. This concentrates learning on the exact
	// post-placement route without masking actions or changing any live rule.
	BombEscapeCurriculumPermille uint16
	// Successive capability stages may expose more native bubble capacity and
	// require multiple completed safe detonations. Zero keeps the conservative
	// single-capacity/single-cycle defaults for existing callers.
	BombEscapeCapacity            byte
	BombEscapeRequiredDetonations byte
	// DevelopmentCurriculumPermille samples a bounded opening lesson on the unmodified
	// native opening map. Each actor starts beside a real breakable wall that
	// contains a hidden native pickup and has a verified short bomb-escape
	// route. The first surviving team to complete the configured productive
	// sequence receives one small auxiliary bonus, then the same battle continues
	// to a real competitive outcome. No action is forced or rewarded by count.
	DevelopmentCurriculumPermille uint16
	// DevelopmentRequiredProductiveDetonations optionally requires distinct
	// engine ticks where an owned bomb actually explodes, its owner remains
	// active after blast resolution, and that owner destroys real terrain in
	// the same tick. Zero preserves the ordinary wall-then-pickup course.
	DevelopmentRequiredProductiveDetonations byte
	// StratifyCurriculumSlots interprets the configured curriculum shares as
	// persistent environment-slot occupancy instead of a fresh draw per reset.
	// Short capability episodes would otherwise contribute far fewer
	// transitions than their nominal reset probability beside full rounds.
	// Layouts and seeds still change on every reset; only the course class stays
	// assigned to the slot.
	StratifyCurriculumSlots bool
	// WorkerCount controls independent environment execution. Zero uses the
	// current Go scheduler parallelism; one preserves serial execution.
	WorkerCount     int
	TickMS          uint32
	DecisionMS      uint32
	TrapDurationMS  uint32
	DangerHorizonMS uint32
	BaseSeed        uint64
	// ReuseTensorBuffers lets a streaming caller reuse the large observation
	// backing arrays between requests. A returned TensorBatch then remains valid
	// only until the next Observe or Step call on this Batch. The binary training
	// worker writes each response before reading another request, so it can use
	// this mode without retaining stale views. Ordinary Go callers keep the
	// default independent-return semantics.
	ReuseTensorBuffers bool
	// HoldCompletedEpisodes leaves completed slots inactive until Reset. Their
	// terminal events/metrics are emitted once. Other slots keep advancing.
	// The default retains Step's requirement to reset terminal slots.
	HoldCompletedEpisodes bool
}

type AgentMetrics struct {
	MovedTicks     uint32
	BombsPlaced    uint32
	WallsDestroyed uint32
	Pickups        uint32
	// Traps and Eliminations count only results against another team.
	// Source-attributed self/friendly hits are not offensive success.
	Traps            uint32
	TrapAssists      uint32
	Eliminations     uint32
	SelfEliminations uint32
	Rescues          uint32
	ActionsUsed      uint32
	NativePasses     uint32
	BlockedEntries   uint32
	BlockedTicks     uint32
	LateStallTicks   uint32
	// OwnedBombDetonations counts real authoritative explosion events for this
	// actor. SurvivedOwnedBombDetonations is the subset after which the actor is
	// still active once every blast effect in that engine tick has resolved.
	// Keeping both counters avoids confusing the competitive bomb-escape course
	// result with a direct per-detonation survival measurement.
	OwnedBombDetonations         uint32
	SurvivedOwnedBombDetonations uint32
	// Positive changes caused by actual pickups, after native caps. These do
	// not count unchanged pickups, debuff expiry or starting attributes.
	PickupCapacityGain uint32
	PickupPowerGain    uint32
	PickupSpeedGain    uint32
}

type EpisodeMetrics struct {
	MapID                         uint32
	DuelCurriculum                bool
	BlockadeCurriculum            bool
	ChainFinisherCurriculum       bool
	ChainFinisherAttackerTeamID   byte
	ChainFinisherPresetRoots      byte
	ChainFinisherRequiredTriggers byte
	ChainFinisherTriggers         uint32
	ChainFinisherPressureCells    uint32
	ChainFinisherSucceeded        bool
	ChainFinisherFailed           bool
	ChainFinisherTriggerOnly      bool
	ChainFinisherPressureOnly     bool
	BombEscapeCurriculum          bool
	// BombEscapeTrainingTeamID binds the asymmetric survival lesson to the
	// policy-controlled team. The other team remains live pressure, but cannot
	// finish the learner's lesson merely by escaping its own bomb first.
	BombEscapeTrainingTeamID byte
	DevelopmentCurriculum    bool
	LateDuelReplayCurriculum bool
	Decisions                uint32
	Ticks                    uint32
	TimedOut                 bool
	Agents                   []AgentMetrics
}

const (
	// Placing a bomb is an action, not progress by itself. Rewarding every
	// placement lets a policy farm return by spamming harmless bombs; useful
	// placements already receive delayed credit through wall, debuff, trap,
	// elimination, pickup and terminal events.
	rewardBombPlaced    float32 = 0
	rewardWallDestroyed float32 = 0.02
	rewardPickup        float32 = 0.01
	// Opening development bonuses are finite because walls and pickups are
	// consumed.  They make a safe resource route competitive with speculative
	// early pursuit without creating a repeatable reward loop.
	rewardDevelopmentWallBonus   float32 = 0.01
	rewardDevelopmentPickupBonus float32 = 0.03
	// Completing the authoritative development sequence is useful auxiliary
	// evidence, but it is not a battle win. Pay one bounded bonus and continue
	// the ordinary match so the much larger terminal return still comes only
	// from the real competitive outcome.
	rewardDevelopmentCourseCompletion float32 = 0.25
	rewardMovementDebuff              float32 = 0.05
	// A terminal win can also happen because an opponent traps itself.  Give
	// substantially more causal credit to a bubble that actually traps an
	// enemy, then to the resulting elimination, so an attacking policy is more
	// valuable than walking safely until the other team makes a mistake.
	rewardTrap float32 = 0.6
	// A second actor whose bubble/flame closes an immediate escape cell shares
	// causal credit for a resulting trap.  This is one bounded pool across all
	// assistants, not a per-bomb bonus, so surrounding a victim with more bombs
	// cannot multiply reward without bound.
	rewardTrapAssistTotal float32 = 0.6
	rewardElimination     float32 = 1.0
	rewardRescue          float32 = 0.2
	// Repeated legitimate rescues decay geometrically against this per-target
	// episode budget: 0.20, 0.10, 0.05... and can never exceed 0.40 total.
	rewardRescueBudget float32 = rewardRescue * 2
	// Breaking a visible transformation consumes an opponent's temporary extra
	// life, but remains less valuable than trapping or eliminating the actor.
	rewardTransformationBroken float32 = 0.1
	// The opening is a development phase.  Do not pay the learner merely for
	// closing distance before it has had time to open terrain and collect the
	// visible resource upgrades that make a real midgame attack sustainable.
	// Opportunistic traps and eliminations still keep their ordinary causal
	// rewards; only the generic chase potential is delayed.
	rewardDevelopmentEndMS uint32 = 30_000
	// Potential shaping guides an actor toward the nearest currently observable
	// enemy without rewarding loops: approaching earns exactly what moving away
	// later gives back. Terrain destruction remains the complementary signal
	// when a direct route is blocked.
	rewardEnemyApproachPerCell float32 = 0.005
	// Decisive play is preferable to surviving until the clock. A fast win may
	// double its terminal reward, while an immediate throw is punished more
	// heavily than a late loss. Timeout draws are evaluated separately from the
	// public initial/live team material: an outnumbered survivor must never learn
	// that deliberate elimination is preferable to holding the round.
	rewardEarlyWinScale  float32 = 1.0
	rewardEarlyLossScale float32 = 1.0
	// Outcomes are normalized against the public initial team-size prior. A
	// strict one-player-versus-one-coalition room retains survival scoring for
	// draws because holding against several same-team opponents is meaningful.
	// Every other draw is a bounded negative outcome: worse than continuing to
	// contest the round, but still better than deliberately losing a 1v1.
	rewardDraw float32 = -0.5
	// From 90 seconds of controllable play onward, a team that has made no
	// material progress for 15 seconds pays a continuous cost. This is earlier
	// than the mathematical half of the 237-second native round because normal
	// rule-one matches are expected to resolve around the one-to-two minute
	// window; waiting until 118.5 seconds left a measurable timeout tail in the
	// current-engine promotion gate. Movement and bomb
	// spam do not reset it; destroying terrain, collecting an item, affecting an
	// enemy, rescuing a teammate, or eliminating an enemy does.  The terminal
	// outcome remains much larger than this shaping signal.
	rewardRoundDurationMS          uint32 = battleengine.StandardRoundTimeMS - battleengine.NativeRoundStartClockMS
	rewardLateStallStartMS         uint32 = 90_000
	rewardLateStallProgressGraceMS uint32 = 15_000
	// Bound the entire late-round shaping cost, not just the per-tick value.
	// The old 0.01/s ramp accumulated to 2.94 in equal-team games, larger
	// than the 1..2 terminal win/loss magnitude. Responsibility is at most 2,
	// so even its maximum costs <=0.4, below the 1v1 draw-to-loss gap of 0.5.
	rewardLateStallBudget        float32 = 0.2
	rewardLateStallMaxMultiplier float32 = 3.0
	// Native bubble traversal is not protected and remains unrestricted. Only
	// entering a static blocked cell (the wall-invulnerability technique) is
	// shaped: a few tactical entries are free, immediate re-entry and prolonged
	// wall occupancy are progressively unprofitable.
	rewardFreeBlockedEntries          uint32  = 2
	rewardRepeatedBlockedEntryPenalty float32 = 0.02
	rewardBlockedEntryPenaltyCap      float32 = 0.1
	rewardBlockedReentryWindowMS      uint32  = 5_000
	rewardRapidBlockedReentryPenalty  float32 = 0.05
	rewardBlockedCellGraceMS          uint32  = 1_200
	rewardBlockedCellPenaltyPerSecond float32 = 0.025
	// Friendly fire remains a useful negative signal, but the terminal result
	// is the objective.  Keep the attacker's shaping penalty small enough that
	// a tactically useful blast (or a later rescue) is not dominated by it.
	rewardFriendlyFireFactor float32 = 0.1
)

type teamMaterial struct {
	initial int
	live    int
}

func materialByTeam(actors []battleengine.Actor) map[byte]teamMaterial {
	result := make(map[byte]teamMaterial)
	for _, actor := range actors {
		material := result[actor.TeamID]
		material.initial++
		if actor.State != battleengine.ActorEliminated {
			material.live++
		}
		result[actor.TeamID] = material
	}
	return result
}

func timeoutDrawReward(teamID byte, material map[byte]teamMaterial) float32 {
	team := material[teamID]
	if team.initial <= 0 || len(material) < 2 {
		return rewardDraw
	}
	totalRetention := float32(0)
	for _, candidate := range material {
		if candidate.initial > 0 {
			totalRetention += float32(candidate.live) / float32(candidate.initial)
		}
	}
	if totalRetention <= 0 {
		return initialOutcomeReward(teamID, 1/float32(len(material)), material)
	}
	teamRetention := float32(team.live) / float32(team.initial)
	return initialOutcomeReward(teamID, teamRetention/totalRetention, material)
}

// oneVersusCoalition reports only the native room layout described by 1Vn:
// exactly two teams, one with one player and the other with several same-team
// players. FFA, equal teams and other uneven layouts do not receive the draw
// survival exception.
func oneVersusCoalition(material map[byte]teamMaterial) bool {
	if len(material) != 2 {
		return false
	}
	var solo, coalition bool
	for _, team := range material {
		switch {
		case team.initial == 1:
			solo = true
		case team.initial > 1:
			coalition = true
		default:
			return false
		}
	}
	return solo && coalition
}

func initialOutcomeReward(teamID byte, score float32, material map[byte]teamMaterial) float32 {
	team := material[teamID]
	// A coalition's combat strength is not linear in its seat count: members
	// add both individual material and pairwise opportunities to cooperate.
	// Squared team size is the cold-start prior until an exact-layout mirror
	// baseline is available to the promotion evaluator.  It distinguishes one
	// actor facing a seven-person coalition from eight mutually hostile solo
	// teams, even though both contain eight actors.
	totalInitialStrength := 0
	for _, candidate := range material {
		totalInitialStrength += candidate.initial * candidate.initial
	}
	teamCount := len(material)
	if team.initial <= 0 || totalInitialStrength <= 0 || teamCount < 2 {
		return 2*score - 1
	}
	expected := float32(team.initial*team.initial) / float32(totalInitialStrength)
	scale := float32(teamCount) / float32(teamCount-1)
	return scale * (score - expected)
}

func terminalOutcomeReward(teamID byte, outcome battleengine.Outcome, material map[byte]teamMaterial) float32 {
	if !outcome.Ended {
		return 0
	}
	if outcome.Draw {
		if oneVersusCoalition(material) {
			if outcome.TimedOut {
				return timeoutDrawReward(teamID, material)
			}
			return initialOutcomeReward(teamID, 1/float32(len(material)), material)
		}
		return rewardDraw
	}
	elapsed := outcome.EndedAtMS
	if elapsed > rewardRoundDurationMS {
		elapsed = rewardRoundDurationMS
	}
	remaining := float32(rewardRoundDurationMS-elapsed) / float32(rewardRoundDurationMS)
	if teamID == outcome.WinnerTeamID {
		base := initialOutcomeReward(teamID, 1, material)
		return base * (1 + rewardEarlyWinScale*remaining)
	}
	base := initialOutcomeReward(teamID, 0, material)
	return base * (1 + rewardEarlyLossScale*remaining)
}

type rewardDelta struct {
	playerID uint16
	amount   float32
}

// trapRewardCredit is deliberately reversible.  A trap is a near-terminal
// advantage only while it remains unresolved; a native teammate rescue must
// remove the attacker's/direct-assistant credit and the victim's penalty.
type trapRewardCredit struct {
	deltas      []rewardDelta
	enemyCaused bool
}

type StepResult struct {
	Observation      TensorBatch
	Rewards          []float32
	Dones            []uint8
	SelfEliminations []uint8
	// SelfFatalBombPlacementLookbacks identifies the exact learner placement
	// that later caused its own elimination. Zero means no attributable bomb;
	// otherwise 1 is the current decision, 2 the preceding decision, and so on.
	// The value comes from stable engine BombID identity, not a proximity guess.
	SelfFatalBombPlacementLookbacks []uint16
	// EnemyFatalBombPlacementLookbacks identifies the exact placement whose
	// stable BombID later eliminated an opposing-team actor. The lookback is
	// stored on the bomb owner, not the victim. Zero means this decision did not
	// resolve an attributable enemy elimination.
	EnemyFatalBombPlacementLookbacks []uint16
	// EnemyFatalBombCausalPlacementLookbacks contains every policy placement
	// on the authoritative trigger chain that ended in an opposing-team
	// elimination. Rows are environment-major then participant-major and are
	// variable length because a native chain is not a fixed three-bomb lesson.
	EnemyFatalBombCausalPlacementLookbacks [][][]uint16
	// EnemyTraps counts authoritative EventActorTrapped events attributed to
	// each actor against another team during this decision interval. It is an
	// observed combat result, not a predicted route or future blast overlap.
	EnemyTraps []uint8
	// DevelopmentProgress counts authoritative wall-destruction and pickup
	// events attributed to each actor during this decision interval.  It lets
	// training rehearse only development actions that actually changed the
	// world, without rewarding arbitrary bomb placement or inferred progress.
	DevelopmentProgress []uint8
	// DevelopmentResults is zero outside the one decision that resolves a
	// development lesson, otherwise it is the authoritative completing team or
	// developmentFailure. Resolving this auxiliary lesson never terminates the
	// real match, and its result is emitted only once, so capability metrics
	// cannot borrow an unrelated combat winner or count the same lesson twice.
	DevelopmentResults []uint8
	// ChainFinisherResults is zero outside a completed chain lesson, one for a
	// verified attacking conversion and two for a failed attacking attempt.
	ChainFinisherResults []uint8
	// BombEscapeResults is zero while no bomb-escape lesson has completed,
	// otherwise it is the team that completed the lesson from authoritative
	// post-explosion state. bombEscapeFailure marks a terminal lesson in which
	// no team completed the required safe detonations.
	BombEscapeResults []uint8
	// CompletedAgentMetrics is a fixed environment-major snapshot of the
	// authoritative per-actor episode counters. Rows are populated only on the
	// decision that ends an episode, before the streaming worker resets it.
	// Evaluation can therefore measure movement, development and combat
	// activity without reconstructing events or observing hidden wall items.
	CompletedAgentMetrics []AgentMetrics
	Outcomes              []battleengine.Outcome
	Events                [][]battleengine.Event
}

type episode struct {
	engine       *battleengine.Engine
	playerIDs    []uint16
	teamIDs      []uint8
	searchSafety []*battleengine.TacticalSafetyPolicy
	episodeIndex uint64
	metrics      EpisodeMetrics
	training     []agentTrainingState
	teamProgress map[uint8]uint32
	trapCredits  map[uint16]trapRewardCredit
	// Rescue shaping spends a bounded per-target episode budget. Repeated
	// trap/rescue cycles therefore decay toward zero instead of minting an
	// unbounded positive return.
	rescueRewardPaid     map[uint16]float32
	chainFinisher        chainFinisherTrainingState
	development          developmentTrainingState
	lateDuelReplay       *lateDuelReplaySnapshot
	lateDuelCandidate    *lateDuelReplaySnapshot
	lateDuelCapturedAtMS uint32
	// bombPlacementDecisions retains the authoritative decision index for each
	// policy-created BombID until the episode ends. A trap may not turn into an
	// elimination until several seconds after the bomb itself has disappeared.
	bombPlacementDecisions map[uint32]bombPlacementDecision
	// bombTriggerParents records only authoritative fuse-shortening links from
	// EventBombExploded. Natural explosions that happen in the same tick remain
	// independent roots.
	bombTriggerParents map[uint32]uint32
}

type bombPlacementDecision struct {
	decision uint32
	ownerID  uint16
}

type lateDuelReplaySnapshot struct {
	engine    *battleengine.Engine
	playerIDs []uint16
	teamIDs   []uint8
	mapID     uint32
}

type developmentTrainingState struct {
	deadlineMS                    uint32
	requiredProductiveDetonations byte
	wallsDestroyed                map[byte]bool
	pickups                       map[byte]bool
	productiveDetonations         map[byte]byte
	settled                       bool
	resultReported                bool
}

type chainFinisherTrainingState struct {
	attackerID       uint16
	attackerTeamID   byte
	defenderTeamID   byte
	requiredTriggers byte
	deadlineMS       uint32
	bombFuseMS       uint32
	placedBombs      map[uint32]uint32
	triggerOnly      bool
	pressureOnly     bool
}

type agentTrainingState struct {
	blocked           bool
	hasBlockedExit    bool
	lastBlockedExitMS uint32
	blockedTicks      uint32
	hasEnemyDistance  bool
	enemyDistance     int
	safeDetonations   byte
}

type Batch struct {
	config    Config
	maps      []mapdata.CompetitiveMap
	episodes  []episode
	maxWidth  int
	maxHeight int
	workers   int
	tensors   TensorBatch
}

type fixedSearchCandidates []battleengine.ScoredAction

func (candidates fixedSearchCandidates) CandidateActions(_ battleengine.Observation, _ []battleengine.Action, limit int) ([]battleengine.ScoredAction, error) {
	if limit <= 0 || limit > len(candidates) {
		limit = len(candidates)
	}
	return append([]battleengine.ScoredAction(nil), candidates[:limit]...), nil
}

type fixedSearchAction struct {
	action battleengine.Action
}

func (policy fixedSearchAction) ChooseAction(_ battleengine.Observation, _ []battleengine.Action) (battleengine.Action, error) {
	return policy.action, nil
}

// LoadEligibleMaps returns only selectable no-item ordinary maps whose entire
// native wall-item table is supported by the restricted engine.
func LoadEligibleMaps(clientRoot string, participantCount int) ([]mapdata.CompetitiveMap, error) {
	catalog, err := mapdata.LoadCatalog(clientRoot)
	if err != nil {
		return nil, err
	}
	var result []mapdata.CompetitiveMap
	for _, mapID := range catalog.CompetitiveIDs() {
		entry, ok := catalog.CompetitiveMap(mapID)
		if !ok || entry.RequiredItemField != 0 || entry.NativeRule != 1 || !entry.UsesOrdinaryElimination() || int(entry.PlayerLimit) < participantCount {
			continue
		}
		if validateErr := battleengine.ValidateNativeRuntimeMap(entry); validateErr != nil {
			continue
		}
		freeTeams := make([]byte, participantCount)
		for index := range freeTeams {
			freeTeams[index] = byte(index + 1)
		}
		if validateErr := battleengine.ValidateNativeSpawnTopology(entry, battleengine.NativeSpawnFree, freeTeams); validateErr != nil {
			continue
		}
		result = append(result, entry)
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("no eligible no-item ordinary maps for %d participants", participantCount)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}

func NewBatch(config Config) (*Batch, error) {
	if config.EnvCount <= 0 {
		return nil, fmt.Errorf("environment count must be positive")
	}
	if config.ParticipantCount < 2 || config.ParticipantCount > battleengine.MaxParticipants {
		return nil, fmt.Errorf("participant count %d is outside 2..%d", config.ParticipantCount, battleengine.MaxParticipants)
	}
	if len(config.ParticipantCounts) == 0 {
		config.ParticipantCounts = []int{config.ParticipantCount}
	}
	allowedParticipantCounts := make(map[int]struct{}, len(config.ParticipantCounts))
	for _, participantCount := range config.ParticipantCounts {
		if participantCount < 2 || participantCount > config.ParticipantCount {
			return nil, fmt.Errorf("sampled participant count %d is outside 2..%d", participantCount, config.ParticipantCount)
		}
		allowedParticipantCounts[participantCount] = struct{}{}
	}
	if len(config.TeamLayouts) == 0 {
		if config.TeamCount == 0 {
			config.TeamCount = 2
		}
		for _, participantCount := range config.ParticipantCounts {
			if config.TeamCount < 2 || config.TeamCount > participantCount || participantCount%config.TeamCount != 0 {
				return nil, fmt.Errorf("team count %d must divide %d participants and stay within 2..%d", config.TeamCount, participantCount, participantCount)
			}
			teamSize := participantCount / config.TeamCount
			layout := make([]uint8, participantCount)
			for index := range layout {
				layout[index] = uint8(index/teamSize + 1)
			}
			config.TeamLayouts = append(config.TeamLayouts, layout)
		}
	}
	if config.TeamSpawnPermille > 1000 {
		return nil, fmt.Errorf("team spawn probability %d is outside 0..1000", config.TeamSpawnPermille)
	}
	if config.DuelCurriculumPermille > 1000 {
		return nil, fmt.Errorf("duel curriculum probability %d is outside 0..1000", config.DuelCurriculumPermille)
	}
	if config.LateDuelReplayPermille > 1000 {
		return nil, fmt.Errorf("late duel replay probability %d is outside 0..1000", config.LateDuelReplayPermille)
	}
	if config.BombEscapeCurriculumPermille > 1000 {
		return nil, fmt.Errorf("bomb escape curriculum probability %d is outside 0..1000", config.BombEscapeCurriculumPermille)
	}
	if config.BlockadeCurriculumPermille > 1000 {
		return nil, fmt.Errorf("blockade curriculum probability %d is outside 0..1000", config.BlockadeCurriculumPermille)
	}
	if config.ChainFinisherCurriculumPermille > 1000 {
		return nil, fmt.Errorf("chain finisher curriculum probability %d is outside 0..1000", config.ChainFinisherCurriculumPermille)
	}
	if config.DevelopmentCurriculumPermille > 1000 {
		return nil, fmt.Errorf("development curriculum probability %d is outside 0..1000", config.DevelopmentCurriculumPermille)
	}
	if config.ChainFinisherMinimumPresetRoots > 3 {
		return nil, fmt.Errorf("minimum chain finisher preset roots %d is outside 0..3", config.ChainFinisherMinimumPresetRoots)
	}
	if config.ChainFinisherMaximumPresetRoots > 3 {
		return nil, fmt.Errorf("maximum chain finisher preset roots %d is outside 0..3", config.ChainFinisherMaximumPresetRoots)
	}
	minimumChainRoots := config.ChainFinisherMinimumPresetRoots
	if minimumChainRoots == 0 {
		minimumChainRoots = 3
	}
	maximumChainRoots := config.ChainFinisherMaximumPresetRoots
	if maximumChainRoots == 0 {
		maximumChainRoots = 3
	}
	if minimumChainRoots > maximumChainRoots {
		return nil, fmt.Errorf("minimum chain finisher preset roots %d exceeds maximum %d", minimumChainRoots, maximumChainRoots)
	}
	if config.ChainFinisherTriggerOnly && config.ChainFinisherPressureOnly {
		return nil, fmt.Errorf("chain finisher trigger-only and pressure-only courses are mutually exclusive")
	}
	curriculumPermille := uint32(config.DuelCurriculumPermille) +
		uint32(config.BlockadeCurriculumPermille) + uint32(config.ChainFinisherCurriculumPermille) +
		uint32(config.BombEscapeCurriculumPermille) + uint32(config.DevelopmentCurriculumPermille)
	if curriculumPermille > 1000 {
		return nil, fmt.Errorf(
			"combined duel curriculum probability %d is outside 0..1000", curriculumPermille,
		)
	}
	if config.BombEscapeCapacity == 0 {
		config.BombEscapeCapacity = 1
	}
	if config.BombEscapeRequiredDetonations == 0 {
		config.BombEscapeRequiredDetonations = 1
	}
	if config.WorkerCount < 0 {
		return nil, fmt.Errorf("worker count %d cannot be negative", config.WorkerCount)
	}
	layoutCounts := make(map[int]int, len(allowedParticipantCounts))
	for index, layout := range config.TeamLayouts {
		if _, allowed := allowedParticipantCounts[len(layout)]; !allowed {
			return nil, fmt.Errorf("team layout %d has unsupported participant count %d", index, len(layout))
		}
		if err := validateTeamLayout(layout, len(layout)); err != nil {
			return nil, fmt.Errorf("team layout %d: %w", index, err)
		}
		config.TeamLayouts[index] = append([]uint8(nil), layout...)
		layoutCounts[len(layout)]++
	}
	for participantCount := range allowedParticipantCounts {
		if layoutCounts[participantCount] == 0 {
			return nil, fmt.Errorf("sampled participant count %d has no team layout", participantCount)
		}
	}
	if config.TickMS == 0 || config.DecisionMS == 0 || config.DecisionMS%config.TickMS != 0 {
		return nil, fmt.Errorf("decision interval %d must be a positive multiple of tick %d", config.DecisionMS, config.TickMS)
	}
	if config.TrapDurationMS == 0 {
		return nil, fmt.Errorf("offline training requires an explicit trap duration")
	}
	if config.DangerHorizonMS == 0 {
		config.DangerHorizonMS = battleengine.NativeBombFuseMS
	}
	if len(config.Maps) == 0 {
		return nil, fmt.Errorf("training map pool is empty")
	}
	workers := config.WorkerCount
	if workers == 0 {
		workers = runtime.GOMAXPROCS(0)
	}
	if workers > config.EnvCount {
		workers = config.EnvCount
	}
	batch := &Batch{config: config, maps: append([]mapdata.CompetitiveMap(nil), config.Maps...), episodes: make([]episode, config.EnvCount), workers: workers}
	for _, entry := range batch.maps {
		if entry.RequiredItemField != 0 || entry.NativeRule != 1 || !entry.UsesOrdinaryElimination() || int(entry.PlayerLimit) < config.ParticipantCount {
			return nil, fmt.Errorf("map %d is outside restricted no-item rule-1 scope", entry.ID)
		}
		if err := battleengine.ValidateNativeRuntimeMap(entry); err != nil {
			return nil, err
		}
		if int(entry.Battlefield.Width) > batch.maxWidth {
			batch.maxWidth = int(entry.Battlefield.Width)
		}
		if int(entry.Battlefield.Height) > batch.maxHeight {
			batch.maxHeight = int(entry.Battlefield.Height)
		}
	}
	if err := batch.ResetAll(); err != nil {
		return nil, err
	}
	return batch, nil
}

func (batch *Batch) ResetAll() error {
	indices := make([]int, len(batch.episodes))
	for index := range indices {
		indices[index] = index
	}
	return batch.Reset(indices)
}

func (batch *Batch) Reset(indices []int) error {
	if batch == nil {
		return fmt.Errorf("training batch is nil")
	}
	seen := make(map[int]struct{}, len(indices))
	for _, envIndex := range indices {
		if envIndex < 0 || envIndex >= len(batch.episodes) {
			return fmt.Errorf("environment index %d is outside batch", envIndex)
		}
		if _, duplicate := seen[envIndex]; duplicate {
			return fmt.Errorf("environment index %d is repeated", envIndex)
		}
		seen[envIndex] = struct{}{}
	}
	return batch.parallelIndices(indices, func(envIndex int) error {
		if err := batch.resetEpisode(envIndex); err != nil {
			return fmt.Errorf("reset environment %d: %w", envIndex, err)
		}
		return nil
	})
}

func (batch *Batch) resetEpisode(envIndex int) error {
	episode := &batch.episodes[envIndex]
	seed := splitMix64(batch.config.BaseSeed + uint64(envIndex)*0x9e3779b97f4a7c15 + episode.episodeIndex*0xbf58476d1ce4e5b9)
	if episode.lateDuelReplay != nil &&
		splitMix64(seed^0x71b54a32d192ed03)%1000 < uint64(batch.config.LateDuelReplayPermille) {
		batch.restoreLateDuelReplay(episode)
		return nil
	}
	spawnSeed := uint32(splitMix64(seed))
	itemSeed := uint32(splitMix64(seed ^ 0x94d049bb133111eb))
	layoutSeed := splitMix64(seed ^ 0x2f6e2b1d5a8c39e7)
	participantCount := batch.config.ParticipantCounts[int(layoutSeed%uint64(len(batch.config.ParticipantCounts)))]
	matchingLayouts := make([][]uint8, 0, len(batch.config.TeamLayouts))
	for _, layout := range batch.config.TeamLayouts {
		if len(layout) == participantCount {
			matchingLayouts = append(matchingLayouts, layout)
		}
	}
	teamIDs := append([]uint8(nil), matchingLayouts[int(splitMix64(layoutSeed)%uint64(len(matchingLayouts)))]...)
	participants := make([]battleengine.Participant, participantCount)
	playerIDs := make([]uint16, participantCount)
	for index := range participants {
		playerID := uint16(index + 1)
		teamID := teamIDs[index]
		roleID := defaultRoleIDs[int(splitMix64(seed+uint64(index)+1)%uint64(len(defaultRoleIDs)))]
		participant, err := battleengine.ParticipantFromNativeRole(playerID, roleID, teamID, battleengine.ParticipantVirtualAI)
		if err != nil {
			return err
		}
		participants[index] = participant
		playerIDs[index] = playerID
	}
	preferTeamSpawns := false
	spawnRoll := splitMix64(seed^0x8f4d2b71935ac6e1) % 1000
	if balancedNativeTeamLayout(teamIDs) && spawnRoll < uint64(batch.config.TeamSpawnPermille) {
		preferTeamSpawns = true
	}
	entry, spawnMode, err := batch.selectEpisodeMap(seed, preferTeamSpawns, teamIDs)
	if err != nil {
		return err
	}
	ordinaryConfig := func() (battleengine.Config, error) {
		return battleengine.ConfigFromCompetitiveMap(entry, battleengine.CompetitiveMapConfigOptions{
			SimulationSeed: seed, SpawnSeed: spawnSeed, ItemSeed: itemSeed, SpawnMode: spawnMode,
			Rules:        battleengine.Rules{TickMS: batch.config.TickMS, TrapDurationMS: batch.config.TrapDurationMS},
			Participants: participants,
		})
	}
	config, err := ordinaryConfig()
	if err != nil {
		return err
	}
	curriculumRoll := curriculumSelectionRoll(batch.config, envIndex, seed)
	developmentCurriculum := participantCount == 2 &&
		curriculumRoll < uint64(batch.config.DevelopmentCurriculumPermille)
	bombEscapeThreshold := uint64(batch.config.DevelopmentCurriculumPermille) + uint64(batch.config.BombEscapeCurriculumPermille)
	bombEscapeCurriculum := participantCount == 2 && !developmentCurriculum && curriculumRoll < bombEscapeThreshold
	blockadeThreshold := bombEscapeThreshold + uint64(batch.config.BlockadeCurriculumPermille)
	blockadeCurriculum := participantCount == 2 && !developmentCurriculum && !bombEscapeCurriculum && curriculumRoll < blockadeThreshold
	chainFinisherThreshold := blockadeThreshold + uint64(batch.config.ChainFinisherCurriculumPermille)
	chainFinisherCurriculum := participantCount == 2 && !developmentCurriculum && !bombEscapeCurriculum && !blockadeCurriculum &&
		curriculumRoll < chainFinisherThreshold
	duelCurriculum := participantCount == 2 && !developmentCurriculum && !bombEscapeCurriculum && !blockadeCurriculum && !chainFinisherCurriculum &&
		curriculumRoll < chainFinisherThreshold+uint64(batch.config.DuelCurriculumPermille)
	var chainSetup chainFinisherSetup
	if developmentCurriculum {
		if err := applyDevelopmentCurriculum(&config, seed); err != nil {
			if !errors.Is(err, errCurriculumLayoutUnavailable) {
				return fmt.Errorf("apply development curriculum: %w", err)
			}
			config, err = ordinaryConfig()
			if err != nil {
				return fmt.Errorf("restore ordinary map after unavailable development course: %w", err)
			}
			developmentCurriculum = false
		}
	} else if bombEscapeCurriculum {
		if err := applyBombEscapeCurriculum(&config, seed, batch.config.BombEscapeCapacity); err != nil {
			if !errors.Is(err, errCurriculumLayoutUnavailable) {
				return fmt.Errorf("apply bomb escape curriculum: %w", err)
			}
			config, err = ordinaryConfig()
			if err != nil {
				return fmt.Errorf("restore ordinary map after unavailable bomb escape course: %w", err)
			}
			bombEscapeCurriculum = false
		}
	} else if blockadeCurriculum {
		if err := applyBlockadeCurriculum(&config, seed); err != nil {
			if !errors.Is(err, errCurriculumLayoutUnavailable) {
				return fmt.Errorf("apply blockade curriculum: %w", err)
			}
			config, err = ordinaryConfig()
			if err != nil {
				return fmt.Errorf("restore ordinary map after unavailable blockade course: %w", err)
			}
			blockadeCurriculum = false
		} else if batch.config.DuelCurriculumBombCapacity != 0 {
			if err := setCurriculumBombCapacity(&config, batch.config.DuelCurriculumBombCapacity); err != nil {
				return fmt.Errorf("apply blockade curriculum capacity: %w", err)
			}
		}
	} else if chainFinisherCurriculum {
		chainSetup, err = applyChainFinisherCurriculum(
			&config, seed,
			batch.config.ChainFinisherMinimumPresetRoots,
			batch.config.ChainFinisherMaximumPresetRoots,
		)
		if err != nil {
			if !errors.Is(err, errCurriculumLayoutUnavailable) {
				return fmt.Errorf("apply chain finisher curriculum: %w", err)
			}
			// Some native maps legitimately do not contain the selected chain
			// lesson's geometry. Keep those samples as unmodified ordinary
			// matches instead of crashing the actor or narrowing the map pool.
			config, err = ordinaryConfig()
			if err != nil {
				return fmt.Errorf("restore ordinary map after unavailable chain course: %w", err)
			}
			chainFinisherCurriculum = false
		}
	} else if duelCurriculum {
		if err := applyDuelCurriculum(&config, seed); err != nil {
			if !errors.Is(err, errCurriculumLayoutUnavailable) {
				return fmt.Errorf("apply duel curriculum: %w", err)
			}
			config, err = ordinaryConfig()
			if err != nil {
				return fmt.Errorf("restore ordinary map after unavailable duel course: %w", err)
			}
			duelCurriculum = false
		} else if batch.config.DuelCurriculumBombCapacity != 0 {
			if err := setCurriculumBombCapacity(&config, batch.config.DuelCurriculumBombCapacity); err != nil {
				return fmt.Errorf("apply duel curriculum capacity: %w", err)
			}
		}
	}
	engine, err := battleengine.New(config)
	if err != nil {
		return err
	}
	var bombEscapeTrainingTeamID byte
	if bombEscapeCurriculum {
		bombEscapeTrainingTeamID = selectCurriculumTrainingTeamID(teamIDs, seed^0x6a09e667f3bcc909)
		if bombEscapeTrainingTeamID == 0 {
			return fmt.Errorf("bomb escape curriculum has no training team")
		}
		if err := seedBombEscapeCurriculum(engine, playerIDs); err != nil {
			return fmt.Errorf("seed bomb escape curriculum: %w", err)
		}
	}
	if chainFinisherCurriculum {
		for _, bubble := range chainSetup.bubbles {
			ageMS := config.Rules.BombFuseMS - bubble.remainingMS
			if ageMS > engine.ElapsedMS() {
				return fmt.Errorf("seed chain finisher root bubble age %d exceeds clock %d", ageMS, engine.ElapsedMS())
			}
			placedAtMS := engine.ElapsedMS() - ageMS
			if _, err := engine.ApplyVerifiedBombPlacementAt(
				bubble.ownerID, bubble.root, uint16(bubble.power), 0, placedAtMS,
			); err != nil {
				return fmt.Errorf("seed chain finisher root bubble: %w", err)
			}
		}
	}
	episode.engine = engine
	episode.playerIDs = playerIDs
	episode.teamIDs = teamIDs
	episode.searchSafety = make([]*battleengine.TacticalSafetyPolicy, len(playerIDs))
	for index := range episode.searchSafety {
		episode.searchSafety[index] = &battleengine.TacticalSafetyPolicy{}
	}
	episode.episodeIndex++
	episode.metrics = EpisodeMetrics{
		MapID: entry.ID, DuelCurriculum: duelCurriculum,
		BlockadeCurriculum:       blockadeCurriculum,
		ChainFinisherCurriculum:  chainFinisherCurriculum,
		BombEscapeCurriculum:     bombEscapeCurriculum,
		BombEscapeTrainingTeamID: bombEscapeTrainingTeamID,
		DevelopmentCurriculum:    developmentCurriculum,
		Agents:                   make([]AgentMetrics, len(playerIDs)),
	}
	episode.lateDuelCandidate = nil
	episode.lateDuelCapturedAtMS = 0
	if chainFinisherCurriculum {
		episode.metrics.ChainFinisherAttackerTeamID = chainSetup.attackerTeamID
		episode.metrics.ChainFinisherPresetRoots = byte(len(chainSetup.bubbles))
		episode.metrics.ChainFinisherRequiredTriggers = chainSetup.requiredTriggers
		episode.metrics.ChainFinisherTriggerOnly = batch.config.ChainFinisherTriggerOnly
		episode.metrics.ChainFinisherPressureOnly = batch.config.ChainFinisherPressureOnly
		episode.chainFinisher = chainFinisherTrainingState{
			attackerID: chainSetup.ownerID, attackerTeamID: chainSetup.attackerTeamID,
			defenderTeamID: chainSetup.defenderTeamID, requiredTriggers: chainSetup.requiredTriggers,
			deadlineMS: chainSetup.deadlineMS, bombFuseMS: config.Rules.BombFuseMS,
			placedBombs: make(map[uint32]uint32), triggerOnly: batch.config.ChainFinisherTriggerOnly,
			pressureOnly: batch.config.ChainFinisherPressureOnly,
		}
	} else {
		episode.chainFinisher = chainFinisherTrainingState{}
	}
	if developmentCurriculum {
		deadlineMS := developmentCurriculumDeadlineMS
		if batch.config.DevelopmentRequiredProductiveDetonations > 0 {
			deadlineMS = productiveDevelopmentCurriculumDeadlineMS
		}
		episode.development = developmentTrainingState{
			deadlineMS:                    engine.ElapsedMS() + deadlineMS,
			requiredProductiveDetonations: batch.config.DevelopmentRequiredProductiveDetonations,
			wallsDestroyed:                make(map[byte]bool, len(teamIDs)),
			pickups:                       make(map[byte]bool, len(teamIDs)),
			productiveDetonations:         make(map[byte]byte, len(teamIDs)),
		}
	} else {
		episode.development = developmentTrainingState{}
	}
	episode.training = make([]agentTrainingState, len(playerIDs))
	episode.teamProgress = make(map[uint8]uint32, len(teamIDs))
	for _, teamID := range teamIDs {
		episode.teamProgress[teamID] = 0
	}
	episode.trapCredits = make(map[uint16]trapRewardCredit, len(playerIDs))
	episode.rescueRewardPaid = make(map[uint16]float32, len(playerIDs))
	episode.bombPlacementDecisions = make(map[uint32]bombPlacementDecision)
	episode.bombTriggerParents = make(map[uint32]uint32)
	return nil
}

func (batch *Batch) restoreLateDuelReplay(episode *episode) {
	snapshot := episode.lateDuelReplay
	episode.engine = snapshot.engine.Clone()
	episode.engine.ClearPublicBehaviorMemories()
	episode.playerIDs = append([]uint16(nil), snapshot.playerIDs...)
	episode.teamIDs = append([]uint8(nil), snapshot.teamIDs...)
	episode.searchSafety = make([]*battleengine.TacticalSafetyPolicy, len(snapshot.playerIDs))
	for index := range episode.searchSafety {
		episode.searchSafety[index] = &battleengine.TacticalSafetyPolicy{}
	}
	episode.episodeIndex++
	episode.metrics = EpisodeMetrics{
		MapID: snapshot.mapID, LateDuelReplayCurriculum: true,
		Agents: make([]AgentMetrics, len(snapshot.playerIDs)),
	}
	episode.training = make([]agentTrainingState, len(snapshot.playerIDs))
	episode.teamProgress = make(map[uint8]uint32, len(snapshot.teamIDs))
	for _, teamID := range snapshot.teamIDs {
		episode.teamProgress[teamID] = episode.engine.RoundElapsedMS()
	}
	episode.trapCredits = make(map[uint16]trapRewardCredit, len(snapshot.playerIDs))
	episode.rescueRewardPaid = make(map[uint16]float32, len(snapshot.playerIDs))
	episode.bombPlacementDecisions = make(map[uint32]bombPlacementDecision)
	episode.bombTriggerParents = make(map[uint32]uint32)
	episode.chainFinisher = chainFinisherTrainingState{}
	episode.development = developmentTrainingState{}
	episode.lateDuelCandidate = nil
	episode.lateDuelCapturedAtMS = 0
}

const (
	lateDuelReplayMinimumOrdinaryElapsedMS uint32 = 30_000
	lateDuelReplayMinimumRemainingMS       uint32 = 30_000
	lateDuelReplayCaptureIntervalMS        uint32 = 15_000
	lateDuelReplayMinimumOutcomeGapMS      uint32 = 5_000
	lateDuelReplayMaximumOutcomeGapMS      uint32 = 20_000
)

func (batch *Batch) captureLateDuelReplay(episode *episode) {
	if batch.config.LateDuelReplayPermille == 0 || episode.metrics.LateDuelReplayCurriculum ||
		episode.metrics.DuelCurriculum || episode.metrics.BlockadeCurriculum ||
		episode.metrics.ChainFinisherCurriculum || episode.metrics.BombEscapeCurriculum ||
		episode.metrics.DevelopmentCurriculum || episode.engine.Terminal().Ended ||
		len(episode.trapCredits) != 0 {
		return
	}
	elapsed := episode.engine.RoundElapsedMS()
	// A native 1v1 is a "final two" from the opening frame. Wait until it has
	// accumulated real play before capturing; multi-player matches may capture
	// immediately when eliminations produce their genuine final confrontation.
	if len(episode.playerIDs) == 2 && elapsed < lateDuelReplayMinimumOrdinaryElapsedMS {
		return
	}
	if elapsed+lateDuelReplayMinimumRemainingMS > rewardRoundDurationMS {
		return
	}
	if episode.lateDuelCapturedAtMS != 0 && elapsed-episode.lateDuelCapturedAtMS < lateDuelReplayCaptureIntervalMS {
		return
	}
	actors := episode.engine.Actors()
	active := make([]battleengine.Actor, 0, 2)
	for _, actor := range actors {
		if actor.State == battleengine.ActorTrapped {
			return
		}
		if actor.State == battleengine.ActorActive {
			active = append(active, actor)
		}
	}
	if len(active) != 2 || active[0].TeamID == active[1].TeamID {
		return
	}
	episode.lateDuelCandidate = &lateDuelReplaySnapshot{
		engine: episode.engine.Clone(), playerIDs: append([]uint16(nil), episode.playerIDs...),
		teamIDs: append([]uint8(nil), episode.teamIDs...), mapID: episode.metrics.MapID,
	}
	episode.lateDuelCapturedAtMS = elapsed
}

// promoteLateDuelReplay admits a real tail only after its source reaches a
// decisive result within the short outcome window. Drawn and timed-out tails
// are deliberately excluded: their negative credit is too diffuse to identify
// a useful attacking action and empirically encourages passive play.
func (batch *Batch) promoteLateDuelReplay(episode *episode, outcome battleengine.Outcome) {
	if episode.metrics.LateDuelReplayCurriculum || !outcome.Ended {
		return
	}
	candidate := episode.lateDuelCandidate
	episode.lateDuelCandidate = nil
	if candidate == nil {
		return
	}
	if outcome.Draw || outcome.TimedOut || outcome.WinnerTeamID == 0 {
		return
	}
	elapsed := episode.engine.RoundElapsedMS()
	if elapsed < episode.lateDuelCapturedAtMS {
		return
	}
	gap := elapsed - episode.lateDuelCapturedAtMS
	if gap < lateDuelReplayMinimumOutcomeGapMS || gap > lateDuelReplayMaximumOutcomeGapMS {
		return
	}
	episode.lateDuelReplay = candidate
}

// seedBombEscapeCurriculum starts from the exact authoritative state that an
// ordinary actor sees immediately after placing a bubble under itself.  The
// earlier version spent most course samples deciding whether to place at all,
// even though the missing transferable capability is the multi-decision route
// from a successful placement to post-blast survival.  Stepping both actors
// through the production placement path also preserves native pass activation,
// placement timing and the first post-placement collision state.  No route is
// selected and no movement is forced; the course result still comes only from
// the real later explosion and actor state.
func seedBombEscapeCurriculum(engine *battleengine.Engine, playerIDs []uint16) error {
	if engine == nil {
		return fmt.Errorf("engine is nil")
	}
	actions := make([]battleengine.Action, len(playerIDs))
	for index, playerID := range playerIDs {
		actions[index] = battleengine.Action{PlayerID: playerID, PlaceBomb: true}
	}
	events, err := engine.Step(actions)
	if err != nil {
		return err
	}
	placed := make(map[uint16]bool, len(playerIDs))
	for _, event := range events {
		if event.Kind == battleengine.EventBombPlaced {
			placed[event.PlayerID] = true
		}
	}
	for _, playerID := range playerIDs {
		if !placed[playerID] {
			return fmt.Errorf("player %d did not place the seeded bubble", playerID)
		}
	}
	return nil
}

func curriculumSelectionRoll(config Config, envIndex int, seed uint64) uint64 {
	if !config.StratifyCurriculumSlots {
		return splitMix64(seed^0x6a09e667f3bcc909) % 1000
	}
	// Midpoints divide [0,1000) into EnvCount equal strata. This keeps the
	// configured occupancy unbiased at small actor-local batch sizes and avoids
	// always assigning a tiny non-zero course to slot zero.
	count := uint64(config.EnvCount)
	return (uint64(2*envIndex+1) * 1000) / (2 * count)
}

func selectCurriculumTrainingTeamID(teamIDs []uint8, seed uint64) byte {
	teams := make([]byte, 0, len(teamIDs))
	seen := make(map[byte]struct{}, len(teamIDs))
	for _, teamID := range teamIDs {
		if teamID == 0 {
			continue
		}
		if _, duplicate := seen[teamID]; duplicate {
			continue
		}
		seen[teamID] = struct{}{}
		teams = append(teams, teamID)
	}
	if len(teams) == 0 {
		return 0
	}
	return teams[int(splitMix64(seed)%uint64(len(teams)))]
}

type duelSpawnPair struct {
	first  battleengine.Cell
	second battleengine.Cell
}

func applyDuelCurriculum(config *battleengine.Config, seed uint64) error {
	if config == nil || len(config.Participants) != 2 {
		return fmt.Errorf("duel curriculum requires exactly two participants")
	}
	config.Grid = config.Grid.Clone()
	for index, tile := range config.Grid.Cells {
		if tile.Kind == battleengine.CellBreakable {
			config.Grid.Cells[index] = battleengine.Tile{Kind: battleengine.CellOpen, FlamePassable: true}
		}
	}
	config.Pickups = nil
	config.PublicWallItemProfile = battleengine.PublicWallItemProfile{}

	component := make([]int, len(config.Grid.Cells))
	for index := range component {
		component[index] = -1
	}
	var open []battleengine.Cell
	componentID := 0
	cardinal := [...]battleengine.Cell{{Row: -1}, {Col: 1}, {Row: 1}, {Col: -1}}
	for index, tile := range config.Grid.Cells {
		if tile.Kind != battleengine.CellOpen || tile.MapElementOccupied || component[index] >= 0 {
			continue
		}
		start := battleengine.Cell{Row: int16(index / int(config.Grid.Width)), Col: int16(index % int(config.Grid.Width))}
		queue := []battleengine.Cell{start}
		component[index] = componentID
		for len(queue) != 0 {
			cell := queue[0]
			queue = queue[1:]
			open = append(open, cell)
			for _, delta := range cardinal {
				next := battleengine.Cell{Row: cell.Row + delta.Row, Col: cell.Col + delta.Col}
				tile, inside := config.Grid.Cell(next)
				if !inside || tile.Kind != battleengine.CellOpen || tile.MapElementOccupied {
					continue
				}
				nextIndex := int(next.Row)*int(config.Grid.Width) + int(next.Col)
				if component[nextIndex] >= 0 {
					continue
				}
				component[nextIndex] = componentID
				queue = append(queue, next)
			}
		}
		componentID++
	}
	var preferred, fallback []duelSpawnPair
	for left := 0; left < len(open); left++ {
		leftIndex := int(open[left].Row)*int(config.Grid.Width) + int(open[left].Col)
		for right := left + 1; right < len(open); right++ {
			rightIndex := int(open[right].Row)*int(config.Grid.Width) + int(open[right].Col)
			if component[leftIndex] != component[rightIndex] {
				continue
			}
			distance := absInt(int(open[left].Row-open[right].Row)) + absInt(int(open[left].Col-open[right].Col))
			if distance < 2 {
				continue
			}
			pair := duelSpawnPair{first: open[left], second: open[right]}
			fallback = append(fallback, pair)
			if distance >= 3 && distance <= 6 {
				preferred = append(preferred, pair)
			}
		}
	}
	pairs := preferred
	if len(pairs) == 0 {
		pairs = fallback
	}
	if len(pairs) == 0 {
		return fmt.Errorf("%w: cleared map has no connected duel spawn pair", errCurriculumLayoutUnavailable)
	}
	pair := pairs[int(splitMix64(seed^0xbb67ae8584caa73b)%uint64(len(pairs)))]
	if splitMix64(seed^0x3c6ef372fe94f82b)&1 != 0 {
		pair.first, pair.second = pair.second, pair.first
	}
	config.Participants[0].Spawn = pair.first
	config.Participants[1].Spawn = pair.second
	for index := range config.Participants {
		participant := &config.Participants[index]
		attributeSeed := seed + uint64(index+1)*0x9e3779b97f4a7c15
		participant.BombCapacity = developedDuelAttribute(participant.BombCapacity, participant.MaxBombCapacity, attributeSeed, 2)
		participant.BombPower = developedDuelAttribute(participant.BombPower, participant.MaxBombPower, attributeSeed^0xa54ff53a5f1d36f1, 2)
		participant.SpeedRate = developedDuelAttribute(participant.SpeedRate, participant.MaxSpeedRate, attributeSeed^0x510e527fade682d1, participant.SpeedRate)
	}
	return nil
}

func setCurriculumBombCapacity(config *battleengine.Config, capacity byte) error {
	if config == nil || capacity == 0 {
		return fmt.Errorf("curriculum bomb capacity must be positive")
	}
	for index := range config.Participants {
		participant := &config.Participants[index]
		if capacity > participant.MaxBombCapacity {
			return fmt.Errorf(
				"capacity %d exceeds player %d native maximum %d",
				capacity, participant.PlayerID, participant.MaxBombCapacity,
			)
		}
		participant.BombCapacity = capacity
	}
	return nil
}

func openNeighborCount(grid battleengine.Grid, cell battleengine.Cell) int {
	directions := [...]battleengine.Cell{{Row: -1}, {Col: 1}, {Row: 1}, {Col: -1}}
	count := 0
	for _, direction := range directions {
		next := battleengine.Cell{Row: cell.Row + direction.Row, Col: cell.Col + direction.Col}
		tile, inside := grid.Cell(next)
		if inside && tile.Kind == battleengine.CellOpen && !tile.MapElementOccupied {
			count++
		}
	}
	return count
}

func openPathDistance(grid battleengine.Grid, start, target battleengine.Cell, maximum int) (int, bool) {
	if start == target {
		return 0, true
	}
	type pathNode struct {
		cell     battleengine.Cell
		distance int
	}
	directions := [...]battleengine.Cell{{Row: -1}, {Col: 1}, {Row: 1}, {Col: -1}}
	queue := []pathNode{{cell: start}}
	visited := map[battleengine.Cell]struct{}{start: {}}
	for len(queue) != 0 {
		node := queue[0]
		queue = queue[1:]
		if node.distance >= maximum {
			continue
		}
		for _, direction := range directions {
			next := battleengine.Cell{Row: node.cell.Row + direction.Row, Col: node.cell.Col + direction.Col}
			if _, seen := visited[next]; seen {
				continue
			}
			tile, inside := grid.Cell(next)
			if !inside || tile.Kind != battleengine.CellOpen || tile.MapElementOccupied {
				continue
			}
			distance := node.distance + 1
			if next == target {
				return distance, true
			}
			visited[next] = struct{}{}
			queue = append(queue, pathNode{cell: next, distance: distance})
		}
	}
	return 0, false
}

func applyBlockadeCurriculum(config *battleengine.Config, seed uint64) error {
	if err := applyDuelCurriculum(config, seed); err != nil {
		return err
	}
	type blockadeCell struct {
		cell  battleengine.Cell
		exits int
	}
	candidatesByPlayer := [2][]blockadeCell{}
	for player := range config.Participants {
		power := config.Participants[player].BombPower
		for index, tile := range config.Grid.Cells {
			if tile.Kind != battleengine.CellOpen || tile.MapElementOccupied {
				continue
			}
			cell := battleengine.Cell{Row: int16(index / int(config.Grid.Width)), Col: int16(index % int(config.Grid.Width))}
			exits := openNeighborCount(config.Grid, cell)
			if exits == 0 || exits > 2 || !bombEscapeRouteExists(config.Grid, cell, power, bombEscapeMaximumPathCells) {
				continue
			}
			candidatesByPlayer[player] = append(candidatesByPlayer[player], blockadeCell{cell: cell, exits: exits})
		}
	}
	var preferred, fallback []duelSpawnPair
	for _, first := range candidatesByPlayer[0] {
		for _, second := range candidatesByPlayer[1] {
			distance, connected := openPathDistance(config.Grid, first.cell, second.cell, 6)
			if !connected || distance < 2 {
				continue
			}
			pair := duelSpawnPair{first: first.cell, second: second.cell}
			fallback = append(fallback, pair)
			if distance <= 4 && (first.exits == 1 || second.exits == 1) {
				preferred = append(preferred, pair)
			}
		}
	}
	pairs := preferred
	if len(pairs) == 0 {
		pairs = fallback
	}
	if len(pairs) == 0 {
		return fmt.Errorf(
			"%w: cleared map has no connected escapable low-degree blockade spawn pair",
			errCurriculumLayoutUnavailable,
		)
	}
	pair := pairs[int(splitMix64(seed^0x5be0cd19137e2179)%uint64(len(pairs)))]
	if splitMix64(seed^0xcbbb9d5dc1059ed8)&1 != 0 {
		pair.first, pair.second = pair.second, pair.first
	}
	config.Participants[0].Spawn = pair.first
	config.Participants[1].Spawn = pair.second
	return nil
}

type chainFinisherSetup struct {
	ownerID          uint16
	attackerTeamID   byte
	defenderTeamID   byte
	bubbles          []chainFinisherBubble
	requiredTriggers byte
	deadlineMS       uint32
}

type chainFinisherBubble struct {
	ownerID     uint16
	root        battleengine.Cell
	power       byte
	remainingMS uint32
}

type chainFinisherLayout struct {
	roots    [3]battleengine.Cell
	triggers [3]battleengine.Cell
	attacker battleengine.Cell
	defender battleengine.Cell
}

// applyChainFinisherCurriculum exposes a rolling expert conversion without
// changing any combat rule. Three already-placed roots have distinct fuse
// deadlines and cannot chain one another. Their useful trigger arms form a
// short route around the defender, so the attacker must repeatedly arrive
// late, place to hit or close an exit, escape, and move to the next deadline.
// Normal self-play still has to learn how to create this setup from scratch.
func applyChainFinisherCurriculum(config *battleengine.Config, seed uint64, minimumPresetRoots, maximumPresetRoots byte) (chainFinisherSetup, error) {
	if err := applyDuelCurriculum(config, seed); err != nil {
		return chainFinisherSetup{}, err
	}
	const (
		rootCount          = 3
		curriculumPower    = byte(3)
		curriculumCapacity = byte(rootCount + 2)
		curriculumSpeed    = byte(6)
	)
	if minimumPresetRoots == 0 {
		minimumPresetRoots = rootCount
	}
	if maximumPresetRoots == 0 {
		maximumPresetRoots = rootCount
	}
	if minimumPresetRoots > maximumPresetRoots || maximumPresetRoots > rootCount {
		return chainFinisherSetup{}, fmt.Errorf(
			"preset root range %d..%d is outside 1..%d", minimumPresetRoots, maximumPresetRoots, rootCount,
		)
	}
	if config.Rules.BombFuseMS < 2_900 || config.Rules.StartClockMS < config.Rules.BombFuseMS-900 {
		return chainFinisherSetup{}, fmt.Errorf(
			"native clock/fuse %d/%d cannot stage staggered visible bubbles",
			config.Rules.StartClockMS, config.Rules.BombFuseMS,
		)
	}
	// Exactly two preset roots form the foundational three-bubble lesson:
	// root A ignites root B, which ignites the learner's newly placed third
	// bubble. Generate this from the real cleared collision graph instead of
	// forcing the sparse fixed rolling template below. The three-root form is
	// retained as the harder staggered-deadline conversion course.
	if minimumPresetRoots == 2 && maximumPresetRoots == 2 {
		return applyCompactThreeBubbleChainCurriculum(
			config, seed, curriculumPower, curriculumCapacity, curriculumSpeed,
		)
	}
	layouts := chainFinisherLayouts(config.Grid, curriculumPower)
	if len(layouts) == 0 {
		return chainFinisherSetup{}, fmt.Errorf(
			"%w: cleared map has no rolling three-bubble conversion layout",
			errCurriculumLayoutUnavailable,
		)
	}
	layout := layouts[int(splitMix64(seed^0x243f6a8885a308d3)%uint64(len(layouts)))]
	eligibleAttackers := make([]int, 0, len(config.Participants))
	for index, participant := range config.Participants {
		if participant.MaxBombCapacity >= curriculumCapacity &&
			participant.MaxBombPower >= curriculumPower && participant.MaxSpeedRate >= curriculumSpeed {
			eligibleAttackers = append(eligibleAttackers, index)
		}
	}
	if len(eligibleAttackers) == 0 {
		return chainFinisherSetup{}, fmt.Errorf(
			"rolling chain curriculum requires native capacity/power/speed at least %d/%d/%d",
			curriculumCapacity, curriculumPower, curriculumSpeed,
		)
	}
	attackerIndex := eligibleAttackers[int(splitMix64(seed^0x13198a2e03707344)%uint64(len(eligibleAttackers)))]
	defenderIndex := 1 - attackerIndex
	config.Participants[attackerIndex].Spawn = layout.attacker
	config.Participants[defenderIndex].Spawn = layout.defender
	attacker := &config.Participants[attackerIndex]
	attacker.BombCapacity = curriculumCapacity
	attacker.BombPower = curriculumPower
	attacker.SpeedRate = curriculumSpeed
	baseRemainingMS := uint32(900 + splitMix64(seed^0xa4093822299f31d0)%101)
	presetRoots := int(minimumPresetRoots) + int(splitMix64(seed^0x082efa98ec4e6c89)%uint64(int(maximumPresetRoots-minimumPresetRoots)+1))
	bubbles := make([]chainFinisherBubble, presetRoots)
	for index, root := range layout.roots[:presetRoots] {
		remainingMS := baseRemainingMS + uint32(index)*900
		if remainingMS >= config.Rules.BombFuseMS {
			return chainFinisherSetup{}, fmt.Errorf(
				"rolling root %d remaining %d exceeds fuse %d", index, remainingMS, config.Rules.BombFuseMS,
			)
		}
		ownerID := attacker.PlayerID
		// A rolling placement can be ignited by any bubble already on the
		// field. Mix friendly and hostile root ownership so the policy learns
		// fuse geometry rather than memorizing an owner-specific shortcut.
		if splitMix64(seed^uint64(index+1)*0x9e3779b97f4a7c15)&1 != 0 {
			ownerID = config.Participants[defenderIndex].PlayerID
		}
		bubbles[index] = chainFinisherBubble{ownerID: ownerID, root: root, power: curriculumPower, remainingMS: remainingMS}
	}
	return chainFinisherSetup{
		ownerID: attacker.PlayerID, attackerTeamID: attacker.TeamID,
		defenderTeamID:   config.Participants[defenderIndex].TeamID,
		bubbles:          bubbles,
		requiredTriggers: byte(2 + (rootCount-presetRoots)/2),
		deadlineMS:       uint32(5_000 + (rootCount-presetRoots)*2_000),
	}, nil
}

type compactThreeBubbleChainLayout struct {
	firstRoot  battleengine.Cell
	secondRoot battleengine.Cell
	trigger    battleengine.Cell
	attacker   battleengine.Cell
	defender   battleengine.Cell
}

func applyCompactThreeBubbleChainCurriculum(
	config *battleengine.Config,
	seed uint64,
	power, capacity, speed byte,
) (chainFinisherSetup, error) {
	layout, ok := compactThreeBubbleChainLayoutFor(config.Grid, power, seed)
	if !ok {
		return chainFinisherSetup{}, fmt.Errorf(
			"%w: cleared map has no compact three-bubble chain layout",
			errCurriculumLayoutUnavailable,
		)
	}
	eligibleAttackers := make([]int, 0, len(config.Participants))
	for index, participant := range config.Participants {
		if participant.MaxBombCapacity >= capacity &&
			participant.MaxBombPower >= power && participant.MaxSpeedRate >= speed {
			eligibleAttackers = append(eligibleAttackers, index)
		}
	}
	if len(eligibleAttackers) == 0 {
		return chainFinisherSetup{}, fmt.Errorf(
			"compact chain curriculum requires native capacity/power/speed at least %d/%d/%d",
			capacity, power, speed,
		)
	}
	attackerIndex := eligibleAttackers[int(splitMix64(seed^0x6d2b79f5aa4c31e7)%uint64(len(eligibleAttackers)))]
	defenderIndex := 1 - attackerIndex
	config.Participants[attackerIndex].Spawn = layout.attacker
	config.Participants[defenderIndex].Spawn = layout.defender
	attacker := &config.Participants[attackerIndex]
	attacker.BombCapacity = capacity
	attacker.BombPower = power
	attacker.SpeedRate = speed
	firstRemainingMS := uint32(1_900 + splitMix64(seed^0x9e3779b97f4a7c15)%201)
	secondRemainingMS := config.Rules.BombFuseMS - 100
	if secondRemainingMS <= firstRemainingMS {
		return chainFinisherSetup{}, fmt.Errorf(
			"compact chain fuse %d cannot stage %d/%dms roots",
			config.Rules.BombFuseMS, firstRemainingMS, secondRemainingMS,
		)
	}
	owners := [2]uint16{attacker.PlayerID, attacker.PlayerID}
	for index := range owners {
		if splitMix64(seed^uint64(index+1)*0x94d049bb133111eb)&1 != 0 {
			owners[index] = config.Participants[defenderIndex].PlayerID
		}
	}
	return chainFinisherSetup{
		ownerID: attacker.PlayerID, attackerTeamID: attacker.TeamID,
		defenderTeamID: config.Participants[defenderIndex].TeamID,
		bubbles: []chainFinisherBubble{
			{ownerID: owners[0], root: layout.firstRoot, power: power, remainingMS: firstRemainingMS},
			{ownerID: owners[1], root: layout.secondRoot, power: power, remainingMS: secondRemainingMS},
		},
		requiredTriggers: 1,
		deadlineMS:       firstRemainingMS + 1_000,
	}, nil
}

func compactThreeBubbleChainLayoutFor(
	grid battleengine.Grid,
	power byte,
	seed uint64,
) (compactThreeBubbleChainLayout, bool) {
	open := make([]battleengine.Cell, 0, len(grid.Cells))
	for index, tile := range grid.Cells {
		if tile.Kind != battleengine.CellOpen || tile.MapElementOccupied {
			continue
		}
		open = append(open, battleengine.Cell{
			Row: int16(index / int(grid.Width)), Col: int16(index % int(grid.Width)),
		})
	}
	if len(open) < 5 {
		return compactThreeBubbleChainLayout{}, false
	}
	ordered := func(cells []battleengine.Cell, salt uint64) []battleengine.Cell {
		if len(cells) < 2 {
			return cells
		}
		offset := int(splitMix64(seed^salt) % uint64(len(cells)))
		result := make([]battleengine.Cell, 0, len(cells))
		result = append(result, cells[offset:]...)
		result = append(result, cells[:offset]...)
		return result
	}
	sources := func(target battleengine.Cell, salt uint64) []battleengine.Cell {
		candidates := make([]battleengine.Cell, 0, int(power)*4)
		for _, origin := range open {
			if origin != target && chainBlastContains(grid, origin, power, target) {
				candidates = append(candidates, origin)
			}
		}
		return ordered(candidates, salt)
	}
	defenders := ordered(open, 0x243f6a8885a308d3)
	cardinal := [...]battleengine.Cell{{Row: -1}, {Col: 1}, {Row: 1}, {Col: -1}}
	for defenderIndex, defender := range defenders {
		for triggerIndex, trigger := range sources(defender, uint64(defenderIndex)^0x13198a2e03707344) {
			for secondIndex, secondRoot := range sources(trigger, uint64(triggerIndex)^0xa4093822299f31d0) {
				if secondRoot == defender || chainBlastContains(grid, secondRoot, power, defender) {
					continue
				}
				for _, firstRoot := range sources(secondRoot, uint64(secondIndex)^0x082efa98ec4e6c89) {
					if firstRoot == defender || firstRoot == trigger ||
						chainBlastContains(grid, firstRoot, power, trigger) ||
						chainBlastContains(grid, firstRoot, power, defender) {
						continue
					}
					for _, delta := range cardinal {
						attacker := battleengine.Cell{Row: trigger.Row + delta.Row, Col: trigger.Col + delta.Col}
						if attacker == defender || attacker == firstRoot || attacker == secondRoot {
							continue
						}
						tile, inside := grid.Cell(attacker)
						if !inside || tile.Kind != battleengine.CellOpen || tile.MapElementOccupied ||
							chainBlastContains(grid, firstRoot, power, attacker) {
							continue
						}
						layout := compactThreeBubbleChainLayout{
							firstRoot: firstRoot, secondRoot: secondRoot, trigger: trigger,
							attacker: attacker, defender: defender,
						}
						if compactThreeBubbleEscapeExists(grid, layout, power) {
							return layout, true
						}
					}
				}
			}
		}
	}
	return compactThreeBubbleChainLayout{}, false
}

func compactThreeBubbleEscapeExists(
	grid battleengine.Grid,
	layout compactThreeBubbleChainLayout,
	power byte,
) bool {
	danger := make(map[battleengine.Cell]struct{})
	for _, origin := range [...]battleengine.Cell{layout.firstRoot, layout.secondRoot, layout.trigger} {
		for _, cell := range chainBlastCells(grid, origin, power) {
			danger[cell] = struct{}{}
		}
	}
	blocked := map[battleengine.Cell]struct{}{
		layout.firstRoot: {}, layout.secondRoot: {}, layout.defender: {},
	}
	type node struct {
		cell     battleengine.Cell
		distance int
	}
	queue := []node{{cell: layout.trigger}}
	visited := map[battleengine.Cell]struct{}{layout.trigger: {}}
	cardinal := [...]battleengine.Cell{{Row: -1}, {Col: 1}, {Row: 1}, {Col: -1}}
	for len(queue) != 0 {
		current := queue[0]
		queue = queue[1:]
		if current.distance != 0 {
			if _, unsafe := danger[current.cell]; !unsafe {
				return true
			}
		}
		if current.distance >= 8 {
			continue
		}
		for _, delta := range cardinal {
			next := battleengine.Cell{Row: current.cell.Row + delta.Row, Col: current.cell.Col + delta.Col}
			if _, seen := visited[next]; seen {
				continue
			}
			tile, inside := grid.Cell(next)
			_, occupied := blocked[next]
			if !inside || tile.Kind != battleengine.CellOpen || tile.MapElementOccupied || occupied {
				continue
			}
			visited[next] = struct{}{}
			queue = append(queue, node{cell: next, distance: current.distance + 1})
		}
	}
	return false
}

// chainFinisherLayouts applies every rotation/reflection of one sparse 3x5
// rolling pattern. Only the eight cells that participate in the lesson must
// be open; ordinary map walls remain authoritative everywhere else.
func chainFinisherLayouts(grid battleengine.Grid, power byte) []chainFinisherLayout {
	base := chainFinisherLayout{
		roots:    [3]battleengine.Cell{{Row: 0, Col: 0}, {Row: 0, Col: 4}, {Row: 2, Col: 1}},
		triggers: [3]battleengine.Cell{{Row: 1, Col: 0}, {Row: 1, Col: 4}, {Row: 2, Col: 2}},
		attacker: battleengine.Cell{Row: 1, Col: 1},
		defender: battleengine.Cell{Row: 0, Col: 1},
	}
	var layouts []chainFinisherLayout
	seen := make(map[chainFinisherLayout]struct{})
	for variant := 0; variant < 8; variant++ {
		transformed := transformChainFinisherLayout(base, variant)
		maximumRow, maximumCol := int16(0), int16(0)
		cells := append(append(transformed.roots[:], transformed.triggers[:]...), transformed.attacker, transformed.defender)
		for _, cell := range cells {
			if cell.Row > maximumRow {
				maximumRow = cell.Row
			}
			if cell.Col > maximumCol {
				maximumCol = cell.Col
			}
		}
		for row := int16(0); row+maximumRow < int16(grid.Height); row++ {
			for col := int16(0); col+maximumCol < int16(grid.Width); col++ {
				layout := offsetChainFinisherLayout(transformed, battleengine.Cell{Row: row, Col: col})
				if !validChainFinisherLayout(grid, layout, power) {
					continue
				}
				if _, duplicate := seen[layout]; duplicate {
					continue
				}
				seen[layout] = struct{}{}
				layouts = append(layouts, layout)
			}
		}
	}
	return layouts
}

func transformChainFinisherLayout(layout chainFinisherLayout, variant int) chainFinisherLayout {
	transform := func(cell battleengine.Cell) battleengine.Cell {
		row, col := cell.Row, cell.Col
		if variant >= 4 {
			col = -col
		}
		for rotation := 0; rotation < variant%4; rotation++ {
			row, col = col, -row
		}
		return battleengine.Cell{Row: row, Col: col}
	}
	for index := range layout.roots {
		layout.roots[index] = transform(layout.roots[index])
	}
	for index := range layout.triggers {
		layout.triggers[index] = transform(layout.triggers[index])
	}
	layout.attacker, layout.defender = transform(layout.attacker), transform(layout.defender)
	minimumRow, minimumCol := layout.attacker.Row, layout.attacker.Col
	for _, cell := range append(append(layout.roots[:], layout.triggers[:]...), layout.defender) {
		if cell.Row < minimumRow {
			minimumRow = cell.Row
		}
		if cell.Col < minimumCol {
			minimumCol = cell.Col
		}
	}
	return offsetChainFinisherLayout(layout, battleengine.Cell{Row: -minimumRow, Col: -minimumCol})
}

func offsetChainFinisherLayout(layout chainFinisherLayout, offset battleengine.Cell) chainFinisherLayout {
	add := func(cell battleengine.Cell) battleengine.Cell {
		return battleengine.Cell{Row: cell.Row + offset.Row, Col: cell.Col + offset.Col}
	}
	for index := range layout.roots {
		layout.roots[index] = add(layout.roots[index])
	}
	for index := range layout.triggers {
		layout.triggers[index] = add(layout.triggers[index])
	}
	layout.attacker, layout.defender = add(layout.attacker), add(layout.defender)
	return layout
}

func validChainFinisherLayout(grid battleengine.Grid, layout chainFinisherLayout, power byte) bool {
	open := func(cell battleengine.Cell) bool {
		tile, inside := grid.Cell(cell)
		return inside && tile.Kind == battleengine.CellOpen && !tile.MapElementOccupied
	}
	for _, cell := range append(append(layout.roots[:], layout.triggers[:]...), layout.attacker, layout.defender) {
		if !open(cell) {
			return false
		}
	}
	for left := range layout.roots {
		for right := range layout.roots {
			if left != right && chainBlastContains(grid, layout.roots[left], power, layout.roots[right]) {
				return false
			}
			if left != right && chainBlastContains(grid, layout.triggers[left], power, layout.roots[right]) {
				return false
			}
		}
	}
	if chainBlastContains(grid, layout.roots[0], power, layout.attacker) {
		return false
	}
	for index, trigger := range layout.triggers {
		useful := chainBlastContains(grid, trigger, power, layout.defender)
		for _, delta := range [...]battleengine.Cell{{Row: -1}, {Col: 1}, {Row: 1}, {Col: -1}} {
			escape := battleengine.Cell{Row: layout.defender.Row + delta.Row, Col: layout.defender.Col + delta.Col}
			if open(escape) && chainBlastContains(grid, trigger, power, escape) {
				useful = true
			}
		}
		if !useful || !chainCombinedEscapeExists(grid, layout, index, power) {
			return false
		}
	}
	return true
}

func chainBlastContains(grid battleengine.Grid, origin battleengine.Cell, power byte, target battleengine.Cell) bool {
	for _, cell := range chainBlastCells(grid, origin, power) {
		if cell == target {
			return true
		}
	}
	return false
}

func chainBlastCells(grid battleengine.Grid, origin battleengine.Cell, power byte) []battleengine.Cell {
	result := []battleengine.Cell{origin}
	for _, delta := range [...]battleengine.Cell{{Row: -1}, {Col: 1}, {Row: 1}, {Col: -1}} {
		for distance := byte(1); distance <= power; distance++ {
			cell := battleengine.Cell{Row: origin.Row + delta.Row*int16(distance), Col: origin.Col + delta.Col*int16(distance)}
			tile, inside := grid.Cell(cell)
			if !inside || tile.Kind == battleengine.CellSolid || tile.MapElementOccupied {
				break
			}
			result = append(result, cell)
			if tile.Kind == battleengine.CellBreakable {
				break
			}
		}
	}
	return result
}

func chainCombinedEscapeExists(grid battleengine.Grid, layout chainFinisherLayout, index int, power byte) bool {
	danger := make(map[battleengine.Cell]struct{})
	for _, cell := range append(chainBlastCells(grid, layout.roots[index], power), chainBlastCells(grid, layout.triggers[index], power)...) {
		danger[cell] = struct{}{}
	}
	blocked := make(map[battleengine.Cell]struct{}, len(layout.roots)+1)
	for _, root := range layout.roots {
		blocked[root] = struct{}{}
	}
	blocked[layout.defender] = struct{}{}
	type node struct {
		cell     battleengine.Cell
		distance int
	}
	queue := []node{{cell: layout.triggers[index]}}
	visited := map[battleengine.Cell]struct{}{layout.triggers[index]: {}}
	for len(queue) != 0 {
		current := queue[0]
		queue = queue[1:]
		if current.distance != 0 {
			if _, unsafe := danger[current.cell]; !unsafe {
				return true
			}
		}
		if current.distance >= 4 {
			continue
		}
		for _, delta := range [...]battleengine.Cell{{Row: -1}, {Col: 1}, {Row: 1}, {Col: -1}} {
			next := battleengine.Cell{Row: current.cell.Row + delta.Row, Col: current.cell.Col + delta.Col}
			if _, seen := visited[next]; seen {
				continue
			}
			tile, inside := grid.Cell(next)
			_, occupied := blocked[next]
			if !inside || tile.Kind != battleengine.CellOpen || tile.MapElementOccupied || occupied {
				continue
			}
			visited[next] = struct{}{}
			queue = append(queue, node{cell: next, distance: current.distance + 1})
		}
	}
	return false
}

const (
	bombEscapeMaximumPathCells                       = 4
	developmentCurriculumDeadlineMS           uint32 = 15_000
	productiveDevelopmentCurriculumDeadlineMS uint32 = 30_000
)

type developmentTarget struct {
	wall  battleengine.Cell
	spawn battleengine.Cell
}

type developmentTargetPair struct {
	first  developmentTarget
	second developmentTarget
}

// applyDevelopmentCurriculum keeps the native grid and hidden-item shuffle
// intact. It only selects two real hidden-item walls and relocates the actors
// to adjacent legal cells from which their native bomb power has a short escape
// route. This makes useful opening development frequent without fabricating a
// pickup, changing collision, exposing hidden data to the actor, or forcing an
// action sequence.
func applyDevelopmentCurriculum(config *battleengine.Config, seed uint64) error {
	if config == nil || len(config.Participants) != 2 {
		return fmt.Errorf("%w: development course requires exactly two participants", errCurriculumLayoutUnavailable)
	}
	firstTargets := developmentTargets(config, config.Participants[0].BombPower)
	secondTargets := developmentTargets(config, config.Participants[1].BombPower)
	var preferred, fallback []developmentTargetPair
	for _, first := range firstTargets {
		for _, second := range secondTargets {
			if first.wall == second.wall || first.spawn == second.spawn {
				continue
			}
			firstPower := config.Participants[0].BombPower
			secondPower := config.Participants[1].BombPower
			if chainBlastContains(config.Grid, first.spawn, firstPower, second.spawn) ||
				chainBlastContains(config.Grid, second.spawn, secondPower, first.spawn) {
				continue
			}
			distance := absInt(int(first.spawn.Row-second.spawn.Row)) + absInt(int(first.spawn.Col-second.spawn.Col))
			if distance < 4 {
				continue
			}
			pair := developmentTargetPair{first: first, second: second}
			fallback = append(fallback, pair)
			if distance >= 8 {
				preferred = append(preferred, pair)
			}
		}
	}
	pairs := preferred
	if len(pairs) == 0 {
		pairs = fallback
	}
	if len(pairs) == 0 {
		return fmt.Errorf("%w: native map has no separated escapable hidden-item walls", errCurriculumLayoutUnavailable)
	}
	pair := pairs[int(splitMix64(seed^0x3c6ef372fe94f82b)%uint64(len(pairs)))]
	config.Participants[0].Spawn = pair.first.spawn
	config.Participants[1].Spawn = pair.second.spawn
	return nil
}

func developmentTargets(config *battleengine.Config, power byte) []developmentTarget {
	if config == nil || power == 0 {
		return nil
	}
	directions := [...]battleengine.Cell{{Row: -1}, {Col: 1}, {Row: 1}, {Col: -1}}
	seen := make(map[developmentTarget]struct{})
	result := make([]developmentTarget, 0, len(config.Pickups)*2)
	for _, pickup := range config.Pickups {
		if pickup.State != battleengine.PickupHidden {
			continue
		}
		wall, inside := config.Grid.Cell(pickup.Cell)
		if !inside || wall.Kind != battleengine.CellBreakable {
			continue
		}
		for _, direction := range directions {
			spawn := battleengine.Cell{Row: pickup.Cell.Row + direction.Row, Col: pickup.Cell.Col + direction.Col}
			tile, inside := config.Grid.Cell(spawn)
			if !inside || tile.Kind != battleengine.CellOpen || tile.MapElementOccupied ||
				!bombEscapeRouteExists(config.Grid, spawn, power, bombEscapeMaximumPathCells) {
				continue
			}
			target := developmentTarget{wall: pickup.Cell, spawn: spawn}
			if _, exists := seen[target]; exists {
				continue
			}
			seen[target] = struct{}{}
			result = append(result, target)
		}
	}
	return result
}

// applyBombEscapeCurriculum retains the native terrain and restricts both
// spawn positions to cells with a real short path outside that actor's own
// native blast. resetEpisode subsequently places the initial bubbles through
// Engine.Step, so observations begin at the exact post-placement state. The
// earlier cleared-duel lesson saturated without transferring to ordinary walls
// and corridors: it proved that the actor could leave an open blast cross, not
// that it could execute the same cycle on a real map.
// Without the route filter a one-cell corridor can still create an impossible
// lesson where every action loses, imposing an artificial success-rate ceiling
// unrelated to policy quality.
func applyBombEscapeCurriculum(config *battleengine.Config, seed uint64, capacity byte) error {
	// Stage one isolates exactly one placement and one escape. Developed duel
	// capacity can start above one and let an untrained policy surround itself with
	// several bubbles before the first fuse expires, accidentally turning the
	// foundation lesson into the harder multi-bomb blockade task. Power remains
	// native so the learned route must respect the real blast geometry.
	if err := setCurriculumBombCapacity(config, capacity); err != nil {
		return fmt.Errorf("bomb escape %w", err)
	}
	firstPower := config.Participants[0].BombPower
	secondPower := config.Participants[1].BombPower
	var firstCells, secondCells []battleengine.Cell
	for index, tile := range config.Grid.Cells {
		if tile.Kind != battleengine.CellOpen || tile.MapElementOccupied {
			continue
		}
		cell := battleengine.Cell{
			Row: int16(index / int(config.Grid.Width)),
			Col: int16(index % int(config.Grid.Width)),
		}
		if bombEscapeRouteExists(config.Grid, cell, firstPower, bombEscapeMaximumPathCells) {
			firstCells = append(firstCells, cell)
		}
		if bombEscapeRouteExists(config.Grid, cell, secondPower, bombEscapeMaximumPathCells) {
			secondCells = append(secondCells, cell)
		}
	}
	var preferred, fallback []duelSpawnPair
	for _, first := range firstCells {
		for _, second := range secondCells {
			if first == second {
				continue
			}
			distance := absInt(int(first.Row-second.Row)) + absInt(int(first.Col-second.Col))
			if distance < 3 {
				continue
			}
			pair := duelSpawnPair{first: first, second: second}
			fallback = append(fallback, pair)
			if distance <= 6 {
				preferred = append(preferred, pair)
			}
		}
	}
	pairs := preferred
	if len(pairs) == 0 {
		pairs = fallback
	}
	if len(pairs) == 0 {
		return fmt.Errorf("%w: cleared map has no two escapable bomb-course spawns", errCurriculumLayoutUnavailable)
	}
	pair := pairs[int(splitMix64(seed^0x1f83d9abfb41bd6b)%uint64(len(pairs)))]
	config.Participants[0].Spawn = pair.first
	config.Participants[1].Spawn = pair.second
	return nil
}

func bombEscapeRouteExists(grid battleengine.Grid, start battleengine.Cell, power byte, maximumSteps int) bool {
	if maximumSteps <= 0 || power == 0 {
		return false
	}
	blast := map[battleengine.Cell]struct{}{start: {}}
	directions := [...]battleengine.Cell{{Row: -1}, {Col: 1}, {Row: 1}, {Col: -1}}
	for _, direction := range directions {
		for distance := int16(1); distance <= int16(power); distance++ {
			cell := battleengine.Cell{
				Row: start.Row + direction.Row*distance,
				Col: start.Col + direction.Col*distance,
			}
			tile, inside := grid.Cell(cell)
			if !inside {
				break
			}
			if tile.Kind == battleengine.CellBreakable {
				blast[cell] = struct{}{}
				break
			}
			if !tile.FlamePassable {
				break
			}
			blast[cell] = struct{}{}
		}
	}
	type pathNode struct {
		cell  battleengine.Cell
		steps int
	}
	queue := []pathNode{{cell: start}}
	visited := map[battleengine.Cell]struct{}{start: {}}
	for len(queue) != 0 {
		node := queue[0]
		queue = queue[1:]
		if node.steps != 0 {
			if _, threatened := blast[node.cell]; !threatened {
				return true
			}
		}
		if node.steps >= maximumSteps {
			continue
		}
		for _, direction := range directions {
			next := battleengine.Cell{Row: node.cell.Row + direction.Row, Col: node.cell.Col + direction.Col}
			if _, seen := visited[next]; seen {
				continue
			}
			tile, inside := grid.Cell(next)
			if !inside || tile.Kind != battleengine.CellOpen || tile.MapElementOccupied {
				continue
			}
			visited[next] = struct{}{}
			queue = append(queue, pathNode{cell: next, steps: node.steps + 1})
		}
	}
	return false
}

func developedDuelAttribute(base, maximum byte, seed uint64, minimum byte) byte {
	if maximum <= base {
		return base
	}
	value := base + byte(1+splitMix64(seed)%uint64(maximum-base))
	if value < minimum && maximum >= minimum {
		value = minimum
	}
	return value
}

func (batch *Batch) selectEpisodeMap(seed uint64, preferTeamSpawns bool, teamIDs []uint8) (mapdata.CompetitiveMap, battleengine.NativeSpawnMode, error) {
	start := int(seed % uint64(len(batch.maps)))
	for offset := range batch.maps {
		entry := batch.maps[(start+offset)%len(batch.maps)]
		spawnMode := battleengine.NativeSpawnFree
		if preferTeamSpawns {
			spawnMode = battleengine.NativeSpawnModeForMap(entry, false)
		}
		if err := battleengine.ValidateNativeSpawnTopology(entry, spawnMode, teamIDs); err == nil {
			return entry, spawnMode, nil
		}
	}
	return mapdata.CompetitiveMap{}, battleengine.NativeSpawnFree, fmt.Errorf("no training map supports native spawn preference team=%t for teams %v", preferTeamSpawns, teamIDs)
}

func (batch *Batch) Observe() (TensorBatch, error) {
	if batch == nil {
		return TensorBatch{}, fmt.Errorf("training batch is nil")
	}
	var tensors TensorBatch
	if batch.config.ReuseTensorBuffers {
		if batch.tensors.EnvCount == 0 {
			batch.tensors = newTensorBatch(len(batch.episodes), batch.config.ParticipantCount, batch.maxHeight, batch.maxWidth)
		} else {
			clearTensorBatch(&batch.tensors)
		}
		tensors = batch.tensors
	} else {
		tensors = newTensorBatch(len(batch.episodes), batch.config.ParticipantCount, batch.maxHeight, batch.maxWidth)
	}
	indices := make([]int, len(batch.episodes))
	for index := range indices {
		indices[index] = index
	}
	if err := batch.parallelIndices(indices, func(envIndex int) error {
		episode := &batch.episodes[envIndex]
		copy(tensors.TeamIDs[envIndex*batch.config.ParticipantCount:], episode.teamIDs)
		switch {
		case episode.metrics.ChainFinisherCurriculum:
			tensors.TrainingTeamIDs[envIndex] = episode.metrics.ChainFinisherAttackerTeamID
		case episode.metrics.BombEscapeCurriculum:
			tensors.TrainingTeamIDs[envIndex] = episode.metrics.BombEscapeTrainingTeamID
		}
		switch {
		case episode.metrics.DevelopmentCurriculum:
			tensors.CurriculumIDs[envIndex] = CurriculumDevelopment
		case episode.metrics.LateDuelReplayCurriculum:
			tensors.CurriculumIDs[envIndex] = CurriculumLateDuelReplay
		case episode.metrics.ChainFinisherCurriculum:
			tensors.CurriculumIDs[envIndex] = CurriculumChainFinisher
		case episode.metrics.BombEscapeCurriculum:
			tensors.CurriculumIDs[envIndex] = CurriculumBombEscape
		case episode.metrics.BlockadeCurriculum:
			tensors.CurriculumIDs[envIndex] = CurriculumBlockade
		case episode.metrics.DuelCurriculum:
			tensors.CurriculumIDs[envIndex] = CurriculumDuel
		default:
			tensors.CurriculumIDs[envIndex] = CurriculumOrdinary
		}
		if batch.config.HoldCompletedEpisodes && episode.engine.Terminal().Ended {
			// A completed slot has no policy actor. Preserve its layout metadata,
			// but avoid unconsumed forecasts and expose only inactive WAIT rows.
			for actorIndex := 0; actorIndex < batch.config.ParticipantCount; actorIndex++ {
				base := (envIndex*batch.config.ParticipantCount + actorIndex) * int(battleengine.DiscreteActionCount)
				tensors.Legal[base+int(battleengine.ActionWait)] = 1
			}
			return nil
		}
		danger, err := episode.engine.DangerTimeline(batch.config.DangerHorizonMS)
		if err != nil {
			return fmt.Errorf("environment %d danger timeline: %w", envIndex, err)
		}
		observations, err := episode.engine.Observations(episode.playerIDs)
		if err != nil {
			return fmt.Errorf("environment %d observations: %w", envIndex, err)
		}
		consequences, err := episode.engine.TacticalConsequencesForPlayersAtDecision(
			episode.playerIDs, danger, batch.config.DecisionMS,
		)
		if err != nil {
			return fmt.Errorf("environment %d tactical consequences: %w", envIndex, err)
		}
		for actorIndex, playerID := range episode.playerIDs {
			legal, legalErr := episode.engine.LegalActionMask(playerID)
			if legalErr != nil {
				return fmt.Errorf("environment %d legal player %d: %w", envIndex, playerID, legalErr)
			}
			if err := encodeActor(&tensors, envIndex, actorIndex, observations[actorIndex], danger, consequences[playerID], legal); err != nil {
				return fmt.Errorf("environment %d encode player %d: %w", envIndex, playerID, err)
			}
		}
		return nil
	}); err != nil {
		return TensorBatch{}, err
	}
	return tensors, nil
}

func balancedNativeTeamLayout(teamIDs []uint8) bool {
	if len(teamIDs) == 0 || len(teamIDs)%2 != 0 {
		return false
	}
	counts := make(map[uint8]int, 2)
	for _, teamID := range teamIDs {
		counts[teamID]++
	}
	if len(counts) != 2 {
		return false
	}
	expected := len(teamIDs) / 2
	for _, count := range counts {
		if count != expected {
			return false
		}
	}
	return true
}

func (batch *Batch) parallelIndices(indices []int, operation func(int) error) error {
	if len(indices) == 0 {
		return nil
	}
	workers := batch.workers
	if workers <= 1 || len(indices) == 1 {
		for _, envIndex := range indices {
			if err := operation(envIndex); err != nil {
				return err
			}
		}
		return nil
	}
	if workers > len(indices) {
		workers = len(indices)
	}
	errorsByIndex := make([]error, len(indices))
	var wait sync.WaitGroup
	wait.Add(workers)
	for worker := 0; worker < workers; worker++ {
		go func(worker int) {
			defer wait.Done()
			for index := worker; index < len(indices); index += workers {
				errorsByIndex[index] = operation(indices[index])
			}
		}(worker)
	}
	wait.Wait()
	for _, err := range errorsByIndex {
		if err != nil {
			return err
		}
	}
	return nil
}

func validateTeamLayout(layout []uint8, participantCount int) error {
	if len(layout) != participantCount {
		return fmt.Errorf("has %d participants, want %d", len(layout), participantCount)
	}
	seen := make(map[uint8]struct{}, participantCount)
	for _, teamID := range layout {
		if teamID == 0 || int(teamID) > participantCount {
			return fmt.Errorf("team ID %d is outside 1..%d", teamID, participantCount)
		}
		seen[teamID] = struct{}{}
	}
	if len(seen) < 2 {
		return fmt.Errorf("must contain at least two teams")
	}
	for teamID := uint8(1); teamID <= uint8(len(seen)); teamID++ {
		if _, ok := seen[teamID]; !ok {
			return fmt.Errorf("team IDs must be contiguous from 1")
		}
	}
	return nil
}

func (batch *Batch) Step(actionIDs [][]battleengine.ActionID) (StepResult, error) {
	if batch == nil {
		return StepResult{}, fmt.Errorf("training batch is nil")
	}
	if len(actionIDs) != len(batch.episodes) {
		return StepResult{}, fmt.Errorf("action environment count %d, want %d", len(actionIDs), len(batch.episodes))
	}
	result := StepResult{
		Rewards: make([]float32, len(batch.episodes)*batch.config.ParticipantCount),
		Dones:   make([]uint8, len(batch.episodes)), Outcomes: make([]battleengine.Outcome, len(batch.episodes)),
		SelfEliminations:                       make([]uint8, len(batch.episodes)*batch.config.ParticipantCount),
		SelfFatalBombPlacementLookbacks:        make([]uint16, len(batch.episodes)*batch.config.ParticipantCount),
		EnemyFatalBombPlacementLookbacks:       make([]uint16, len(batch.episodes)*batch.config.ParticipantCount),
		EnemyFatalBombCausalPlacementLookbacks: make([][][]uint16, len(batch.episodes)),
		EnemyTraps:                             make([]uint8, len(batch.episodes)*batch.config.ParticipantCount),
		DevelopmentProgress:                    make([]uint8, len(batch.episodes)*batch.config.ParticipantCount),
		DevelopmentResults:                     make([]uint8, len(batch.episodes)),
		ChainFinisherResults:                   make([]uint8, len(batch.episodes)),
		BombEscapeResults:                      make([]uint8, len(batch.episodes)),
		CompletedAgentMetrics:                  make([]AgentMetrics, len(batch.episodes)*batch.config.ParticipantCount),
		Events:                                 make([][]battleengine.Event, len(batch.episodes)),
	}
	for envIndex := range result.EnemyFatalBombCausalPlacementLookbacks {
		result.EnemyFatalBombCausalPlacementLookbacks[envIndex] = make([][]uint16, batch.config.ParticipantCount)
	}
	initialByEnvironment := make([][]battleengine.Action, len(batch.episodes))
	indices := make([]int, 0, len(batch.episodes))
	for envIndex := range batch.episodes {
		episode := &batch.episodes[envIndex]
		if len(actionIDs[envIndex]) != batch.config.ParticipantCount {
			return StepResult{}, fmt.Errorf("environment %d action count %d, want tensor capacity %d", envIndex, len(actionIDs[envIndex]), batch.config.ParticipantCount)
		}
		if episode.engine.Terminal().Ended {
			if !batch.config.HoldCompletedEpisodes {
				return StepResult{}, fmt.Errorf("environment %d is terminal and must be reset", envIndex)
			}
			// Repeated in-range actions on a held slot are idempotent no-ops.
			// Malformed IDs still fail the protocol boundary check.
			for actorIndex, playerID := range episode.playerIDs {
				if _, ok := battleengine.ActionFromID(playerID, actionIDs[envIndex][actorIndex]); !ok {
					return StepResult{}, fmt.Errorf("environment %d player %d has invalid action ID %d", envIndex, playerID, actionIDs[envIndex][actorIndex])
				}
			}
			continue
		}
		initial := make([]battleengine.Action, len(episode.playerIDs))
		for actorIndex, playerID := range episode.playerIDs {
			action, ok := battleengine.ActionFromID(playerID, actionIDs[envIndex][actorIndex])
			if !ok {
				return StepResult{}, fmt.Errorf("environment %d player %d has invalid action ID %d", envIndex, playerID, actionIDs[envIndex][actorIndex])
			}
			mask, maskErr := episode.engine.LegalActionMask(playerID)
			if maskErr != nil {
				return StepResult{}, maskErr
			}
			if !mask[actionIDs[envIndex][actorIndex]] {
				return StepResult{}, fmt.Errorf("environment %d player %d selected illegal action %d", envIndex, playerID, actionIDs[envIndex][actorIndex])
			}
			initial[actorIndex] = action
		}
		initialByEnvironment[envIndex] = initial
		indices = append(indices, envIndex)
	}
	if err := batch.parallelIndices(indices, func(envIndex int) error {
		episode := &batch.episodes[envIndex]
		initial := initialByEnvironment[envIndex]
		ticks := int(batch.config.DecisionMS / batch.config.TickMS)
		var developmentWinnerTeam byte
		developmentCompleted := false
		var bombEscapeWinnerTeam byte
		bombEscapeSucceeded := false
		var chainFinisherWinnerTeam byte
		for tick := 0; tick < ticks && !episode.engine.Terminal().Ended; tick++ {
			actions := make([]battleengine.Action, len(initial))
			for actorIndex, action := range initial {
				actions[actorIndex] = action
				if tick != 0 {
					actions[actorIndex].PlaceBomb = false
					actions[actorIndex].UseActionID = 0
				}
			}
			events, err := episode.engine.Step(actions)
			if err != nil {
				return fmt.Errorf("environment %d tick %d: %w", envIndex, tick, err)
			}
			episode.metrics.Ticks++
			result.Events[envIndex] = append(result.Events[envIndex], events...)
			batch.accumulateEvents(envIndex, events, result.Rewards)
			recordBombPlacementDecisions(episode, events)
			recordSelfEliminations(
				episode.playerIDs,
				events,
				result.SelfEliminations[envIndex*batch.config.ParticipantCount:(envIndex+1)*batch.config.ParticipantCount],
			)
			recordSelfFatalBombPlacementLookbacks(
				episode,
				events,
				result.SelfFatalBombPlacementLookbacks[envIndex*batch.config.ParticipantCount:(envIndex+1)*batch.config.ParticipantCount],
			)
			recordEnemyFatalBombPlacementLookbacks(
				episode,
				events,
				result.EnemyFatalBombPlacementLookbacks[envIndex*batch.config.ParticipantCount:(envIndex+1)*batch.config.ParticipantCount],
			)
			recordEnemyFatalBombCausalPlacementLookbacks(
				episode,
				events,
				result.EnemyFatalBombCausalPlacementLookbacks[envIndex],
			)
			recordEnemyTraps(
				episode.engine.Actors(),
				events,
				result.EnemyTraps[envIndex*batch.config.ParticipantCount:(envIndex+1)*batch.config.ParticipantCount],
			)
			recordDevelopmentProgress(
				episode.engine.Actors(),
				events,
				result.DevelopmentProgress[envIndex*batch.config.ParticipantCount:(envIndex+1)*batch.config.ParticipantCount],
			)
			batch.accumulateEnemyApproach(envIndex, result.Rewards)
			batch.accumulateLateStallBehavior(envIndex, result.Rewards)
			batch.accumulateBlockedCellBehavior(envIndex, result.Rewards)
			batch.captureLateDuelReplay(episode)
			if episode.metrics.DevelopmentCurriculum {
				completed, winnerTeam, _ := updateDevelopmentCurriculum(episode, events)
				if completed {
					developmentCompleted = true
					developmentWinnerTeam = winnerTeam
					if winnerTeam != 0 {
						for actorIndex, actor := range episode.engine.Actors() {
							if actor.TeamID == winnerTeam {
								result.Rewards[envIndex*batch.config.ParticipantCount+actorIndex] += rewardDevelopmentCourseCompletion
							}
						}
					}
				}
			}
			if episode.metrics.ChainFinisherCurriculum {
				chainResult, winnerTeam := updateChainFinisherCurriculum(episode, events)
				if chainResult != 0 {
					result.ChainFinisherResults[envIndex] = chainResult
					chainFinisherWinnerTeam = winnerTeam
					break
				}
			}
			if episode.metrics.BombEscapeCurriculum {
				if completed, trainingTeam := updateBombEscapeCurriculum(
					episode, events, batch.config.BombEscapeRequiredDetonations,
				); completed {
					bombEscapeWinnerTeam = trainingTeam
					bombEscapeSucceeded = true
					break
				}
			}
		}
		episode.metrics.Decisions++
		outcome := episode.engine.Terminal()
		if episode.metrics.ChainFinisherCurriculum && outcome.Ended && result.ChainFinisherResults[envIndex] == 0 {
			// A preset root may end the ordinary duel without the learner ever
			// executing the rolling trigger sequence. That is not completion of
			// this capability lesson and must not provide a free attacking win.
			// Keep a real attacker defeat negative, but treat a preset-only enemy
			// hit as a neutral miss instead of fabricating a defender victory.
			episode.metrics.ChainFinisherFailed = true
			result.ChainFinisherResults[envIndex] = chainFinisherFailure
			for _, actor := range episode.engine.Actors() {
				if actor.PlayerID == episode.chainFinisher.attackerID && actor.State != battleengine.ActorActive {
					chainFinisherWinnerTeam = episode.chainFinisher.defenderTeamID
					break
				}
			}
		}
		if !outcome.Ended && bombEscapeSucceeded {
			outcome = battleengine.Outcome{
				Ended: true, WinnerTeamID: bombEscapeWinnerTeam,
				EndedAtMS: episode.engine.ElapsedMS(),
			}
		}
		if result.ChainFinisherResults[envIndex] != 0 {
			outcome = battleengine.Outcome{
				Ended: true, Draw: chainFinisherWinnerTeam == 0, WinnerTeamID: chainFinisherWinnerTeam,
				EndedAtMS: episode.engine.ElapsedMS(),
			}
		}
		result.BombEscapeResults[envIndex] = bombEscapeCurriculumResult(
			episode.metrics.BombEscapeCurriculum,
			outcome,
			bombEscapeSucceeded,
			bombEscapeWinnerTeam,
		)
		result.DevelopmentResults[envIndex] = developmentCurriculumResult(
			episode.metrics.DevelopmentCurriculum,
			outcome,
			developmentCompleted,
			developmentWinnerTeam,
			episode.development.resultReported,
		)
		if result.DevelopmentResults[envIndex] != 0 {
			episode.development.resultReported = true
		}
		batch.promoteLateDuelReplay(episode, outcome)
		result.Outcomes[envIndex] = outcome
		if outcome.Ended {
			result.Dones[envIndex] = 1
			episode.metrics.TimedOut = outcome.TimedOut
			metricBase := envIndex * batch.config.ParticipantCount
			copy(
				result.CompletedAgentMetrics[metricBase:metricBase+len(episode.metrics.Agents)],
				episode.metrics.Agents,
			)
			actors := episode.engine.Actors()
			material := materialByTeam(actors)
			for actorIndex, actor := range actors {
				rewardIndex := envIndex*batch.config.ParticipantCount + actorIndex
				result.Rewards[rewardIndex] += terminalOutcomeReward(actor.TeamID, outcome, material)
			}
		}
		return nil
	}); err != nil {
		return StepResult{}, err
	}
	observation, err := batch.Observe()
	if err != nil {
		return StepResult{}, err
	}
	result.Observation = observation
	return result, nil
}

const (
	chainFinisherSuccess uint8 = 1
	chainFinisherFailure uint8 = 2
	bombEscapeFailure    uint8 = 0xff
	developmentFailure   uint8 = 0xff
)

// developmentCurriculumResult reports only completion observed by the
// authoritative curriculum state machine. An ordinary elimination that ends
// the engine before the repeated development sequence is complete is a course
// miss, even when that combat outcome has a winner.
func developmentCurriculumResult(
	curriculum bool,
	outcome battleengine.Outcome,
	resolved bool,
	winnerTeam byte,
	alreadyReported bool,
) uint8 {
	if !curriculum || alreadyReported {
		return 0
	}
	if resolved {
		if winnerTeam != 0 {
			return winnerTeam
		}
		return developmentFailure
	}
	if outcome.Ended {
		return developmentFailure
	}
	return 0
}

// bombEscapeCurriculumResult deliberately reports only an observed course
// result. Tactical reachability projections are inputs to the policy, never
// evidence that the actor actually survived its bomb in the formal engine.
func bombEscapeCurriculumResult(
	curriculum bool,
	outcome battleengine.Outcome,
	succeeded bool,
	winnerTeam byte,
) uint8 {
	if !curriculum {
		return 0
	}
	if succeeded && winnerTeam != 0 {
		return winnerTeam
	}
	if outcome.Ended {
		return bombEscapeFailure
	}
	return 0
}

// updateChainFinisherCurriculum recognizes only an authoritative rolling
// conversion. A post-reset attacker bomb must explode early and trap the enemy
// in that same engine tick while the attacker remains active. Trigger- and
// pressure-only foundation lessons additionally require their configured
// repetition quota; a real hit is never rejected merely because it arrived
// before that artificial practice quota. Waiting, unrelated bombs and trades
// cannot complete the lesson.
func updateChainFinisherCurriculum(episode *episode, events []battleengine.Event) (uint8, byte) {
	state := &episode.chainFinisher
	if state.attackerID == 0 {
		return 0, 0
	}
	for _, event := range events {
		if event.Kind == battleengine.EventBombPlaced && event.PlayerID == state.attackerID {
			state.placedBombs[event.BombID] = event.TimeMS
		}
	}
	newTriggers := uint32(0)
	triggeredExplosions := make([]battleengine.Event, 0, state.requiredTriggers)
	for _, event := range events {
		if event.Kind != battleengine.EventBombExploded || event.PlayerID != state.attackerID {
			continue
		}
		placedAtMS, placed := state.placedBombs[event.BombID]
		if !placed {
			continue
		}
		delete(state.placedBombs, event.BombID)
		if event.TimeMS >= placedAtMS && event.TimeMS-placedAtMS < state.bombFuseMS {
			newTriggers++
			triggeredExplosions = append(triggeredExplosions, event)
		}
	}
	actors := episode.engine.Actors()
	attackerActive := false
	teamsByPlayer := make(map[uint16]byte, len(actors))
	for _, actor := range actors {
		teamsByPlayer[actor.PlayerID] = actor.TeamID
		if actor.PlayerID == state.attackerID {
			attackerActive = actor.State == battleengine.ActorActive
		}
	}
	var pressure chainFinisherPressure
	if attackerActive {
		episode.metrics.ChainFinisherTriggers += newTriggers
		pressure = chainFinisherExplosionPressure(episode, triggeredExplosions, actors)
		episode.metrics.ChainFinisherPressureCells += uint32(pressure.coveredCells)
	}
	if newTriggers != 0 && attackerActive {
		for _, event := range events {
			if event.Kind == battleengine.EventActorTrapped && event.PlayerID == state.attackerID &&
				teamsByPlayer[event.TargetID] == state.defenderTeamID {
				episode.metrics.ChainFinisherSucceeded = true
				return chainFinisherSuccess, state.attackerTeamID
			}
		}
		if episode.metrics.ChainFinisherTriggers >= uint32(state.requiredTriggers) {
			if state.triggerOnly {
				episode.metrics.ChainFinisherSucceeded = true
				return chainFinisherSuccess, state.attackerTeamID
			}
			if state.pressureOnly && pressure.isMeaningful() {
				episode.metrics.ChainFinisherSucceeded = true
				return chainFinisherSuccess, state.attackerTeamID
			}
		}
	}
	if !attackerActive {
		episode.metrics.ChainFinisherFailed = true
		return chainFinisherFailure, state.defenderTeamID
	}
	if episode.engine.RoundElapsedMS() >= state.deadlineMS {
		// Surviving without a conversion is still a failed lesson, but a short
		// curriculum deadline is not evidence that the opponent defeated the
		// learner. Neutral terminal credit lets placement progress compete with
		// the sparse authoritative trap while self-elimination remains negative.
		episode.metrics.ChainFinisherFailed = true
		return chainFinisherFailure, 0
	}
	return 0, 0
}

type chainFinisherPressure struct {
	coveredCells   int
	currentHit     bool
	openEscapes    int
	coveredEscapes int
}

// isMeaningful rejects the old one-cell proxy that V57 showed did not transfer
// to authoritative traps. A blast must hit the defender, close the only exit,
// or cover at least two currently legal exits. This still permits wall-assisted
// blockades while requiring real geometric pressure in an open area.
func (pressure chainFinisherPressure) isMeaningful() bool {
	if pressure.currentHit {
		return true
	}
	if pressure.openEscapes == 0 {
		return false
	}
	required := pressure.openEscapes
	if required > 2 {
		required = 2
	}
	return pressure.coveredEscapes >= required
}

func chainFinisherExplosionPressure(episode *episode, explosions []battleengine.Event, actors []battleengine.Actor) chainFinisherPressure {
	var defender battleengine.Actor
	found := false
	for _, actor := range actors {
		if actor.TeamID == episode.chainFinisher.defenderTeamID {
			defender, found = actor, true
			break
		}
	}
	if !found {
		return chainFinisherPressure{}
	}
	candidates := [...]battleengine.Cell{
		defender.Position.Cell(),
		{Row: defender.Position.Cell().Row - 1, Col: defender.Position.Cell().Col},
		{Row: defender.Position.Cell().Row + 1, Col: defender.Position.Cell().Col},
		{Row: defender.Position.Cell().Row, Col: defender.Position.Cell().Col - 1},
		{Row: defender.Position.Cell().Row, Col: defender.Position.Cell().Col + 1},
	}
	pressure := chainFinisherPressure{}
	for index, cell := range candidates {
		if index != 0 {
			tile, inside := episode.engine.TileAt(cell)
			if !inside || tile.Kind != battleengine.CellOpen || tile.MapElementOccupied {
				continue
			}
			pressure.openEscapes++
		}
		for _, explosion := range explosions {
			if explosionBlastContains(explosion, cell) {
				pressure.coveredCells++
				if index == 0 {
					pressure.currentHit = true
				} else {
					pressure.coveredEscapes++
				}
				break
			}
		}
	}
	return pressure
}

func explosionBlastContains(explosion battleengine.Event, cell battleengine.Cell) bool {
	return (cell.Row == explosion.Cell.Row && cell.Col >= explosion.BlastColMin && cell.Col <= explosion.BlastColMax) ||
		(cell.Col == explosion.Cell.Col && cell.Row >= explosion.BlastRowMin && cell.Row <= explosion.BlastRowMax)
}

func actorIndexByPlayerID(actors []battleengine.Actor, playerID uint16) int {
	for index, actor := range actors {
		if actor.PlayerID == playerID {
			return index
		}
	}
	return -1
}

// safeBombEscapeOwners recognizes authoritative completed escapes, not
// predicted safe actions. A bomb must actually explode and its owner must
// still be active after the engine has applied every blast effect in the tick.
func safeBombEscapeOwners(events []battleengine.Event, actors []battleengine.Actor) []uint16 {
	activePlayers := make(map[uint16]struct{}, len(actors))
	for _, actor := range actors {
		if actor.State == battleengine.ActorActive {
			activePlayers[actor.PlayerID] = struct{}{}
		}
	}
	seen := make(map[uint16]struct{})
	owners := make([]uint16, 0, len(events))
	for _, event := range events {
		if event.Kind != battleengine.EventBombExploded {
			continue
		}
		_, active := activePlayers[event.PlayerID]
		if !active {
			continue
		}
		if _, duplicate := seen[event.PlayerID]; duplicate {
			continue
		}
		seen[event.PlayerID] = struct{}{}
		owners = append(owners, event.PlayerID)
	}
	return owners
}

// updateBombEscapeCurriculum records only authoritative explosions owned by
// the designated learner team. The opposing policy remains in the world and
// can create realistic interference, but its own escape speed is not evidence
// that the learner survived. This keeps the lesson outcome aligned with the
// trajectory that PPO is actually optimizing.
func updateBombEscapeCurriculum(episode *episode, events []battleengine.Event, required byte) (bool, byte) {
	if episode == nil || episode.engine == nil || !episode.metrics.BombEscapeCurriculum || required == 0 {
		return false, 0
	}
	trainingTeamID := episode.metrics.BombEscapeTrainingTeamID
	if trainingTeamID == 0 {
		return false, 0
	}
	actors := episode.engine.Actors()
	for _, ownerID := range safeBombEscapeOwners(events, actors) {
		owner := actorIndexByPlayerID(actors, ownerID)
		if owner < 0 || actors[owner].TeamID != trainingTeamID {
			continue
		}
		training := &episode.training[owner]
		if training.safeDetonations < ^byte(0) {
			training.safeDetonations++
		}
		if training.safeDetonations >= required {
			return true, trainingTeamID
		}
	}
	return false, 0
}

func recordSelfEliminations(playerIDs []uint16, events []battleengine.Event, result []uint8) {
	for _, event := range events {
		if event.Kind != battleengine.EventActorEliminated || event.PlayerID != event.TargetID {
			continue
		}
		for actorIndex, playerID := range playerIDs {
			if playerID == event.TargetID && actorIndex < len(result) {
				if result[actorIndex] < ^uint8(0) {
					result[actorIndex]++
				}
				break
			}
		}
	}
}

func recordBombPlacementDecisions(episode *episode, events []battleengine.Event) {
	if episode == nil {
		return
	}
	if episode.bombPlacementDecisions == nil {
		episode.bombPlacementDecisions = make(map[uint32]bombPlacementDecision)
	}
	if episode.bombTriggerParents == nil {
		episode.bombTriggerParents = make(map[uint32]uint32)
	}
	for _, event := range events {
		if event.Kind == battleengine.EventBombPlaced && event.BombID != 0 {
			episode.bombPlacementDecisions[event.BombID] = bombPlacementDecision{
				decision: episode.metrics.Decisions,
				ownerID:  event.PlayerID,
			}
		}
		if event.Kind == battleengine.EventBombExploded && event.BombID != 0 && event.TriggeredByBombID != 0 {
			episode.bombTriggerParents[event.BombID] = event.TriggeredByBombID
		}
	}
}

func recordSelfFatalBombPlacementLookbacks(episode *episode, events []battleengine.Event, result []uint16) {
	if episode == nil {
		return
	}
	for _, event := range events {
		if event.Kind != battleengine.EventActorEliminated || event.PlayerID != event.TargetID || event.BombID == 0 {
			continue
		}
		placement, attributed := episode.bombPlacementDecisions[event.BombID]
		if !attributed || placement.decision > episode.metrics.Decisions {
			continue
		}
		actorIndex := -1
		for index, playerID := range episode.playerIDs {
			if playerID == event.TargetID {
				actorIndex = index
				break
			}
		}
		if actorIndex < 0 || actorIndex >= len(result) {
			continue
		}
		lookback := episode.metrics.Decisions - placement.decision + 1
		if lookback > uint32(^uint16(0)) {
			lookback = uint32(^uint16(0))
		}
		result[actorIndex] = uint16(lookback)
	}
}

func recordEnemyFatalBombPlacementLookbacks(episode *episode, events []battleengine.Event, result []uint16) {
	if episode == nil {
		return
	}
	for _, event := range events {
		if event.Kind != battleengine.EventActorEliminated || event.PlayerID == event.TargetID || event.BombID == 0 {
			continue
		}
		placement, attributed := episode.bombPlacementDecisions[event.BombID]
		if !attributed || placement.decision > episode.metrics.Decisions {
			continue
		}
		owner := -1
		target := -1
		for index, playerID := range episode.playerIDs {
			if playerID == event.PlayerID {
				owner = index
			}
			if playerID == event.TargetID {
				target = index
			}
		}
		if owner < 0 || target < 0 || owner >= len(result) || owner >= len(episode.teamIDs) || target >= len(episode.teamIDs) || episode.teamIDs[owner] == episode.teamIDs[target] {
			continue
		}
		lookback := episode.metrics.Decisions - placement.decision + 1
		if lookback > uint32(^uint16(0)) {
			lookback = uint32(^uint16(0))
		}
		// One actor may eliminate several opponents in the same decision. Keep
		// the most recent attributable placement; every entry remains exact and
		// the current 1v1 curriculum can produce only one.
		if result[owner] == 0 || lookback < uint32(result[owner]) {
			result[owner] = uint16(lookback)
		}
	}
}

func recordEnemyFatalBombCausalPlacementLookbacks(episode *episode, events []battleengine.Event, result [][]uint16) {
	if episode == nil || len(result) == 0 {
		return
	}
	playerIndex := make(map[uint16]int, len(episode.playerIDs))
	for index, playerID := range episode.playerIDs {
		playerIndex[playerID] = index
	}
	for _, event := range events {
		if event.Kind != battleengine.EventActorEliminated || event.PlayerID == event.TargetID || event.BombID == 0 {
			continue
		}
		target, knownTarget := playerIndex[event.TargetID]
		if !knownTarget || target >= len(episode.teamIDs) {
			continue
		}
		seenBombs := make(map[uint32]struct{})
		for bombID := event.BombID; bombID != 0; bombID = episode.bombTriggerParents[bombID] {
			if _, duplicate := seenBombs[bombID]; duplicate {
				break
			}
			seenBombs[bombID] = struct{}{}
			placement, attributed := episode.bombPlacementDecisions[bombID]
			if !attributed || placement.decision > episode.metrics.Decisions {
				continue
			}
			owner, knownOwner := playerIndex[placement.ownerID]
			if !knownOwner || owner >= len(result) || owner >= len(episode.teamIDs) || episode.teamIDs[owner] == episode.teamIDs[target] {
				continue
			}
			lookback := episode.metrics.Decisions - placement.decision + 1
			if lookback > uint32(^uint16(0)) {
				lookback = uint32(^uint16(0))
			}
			encoded := uint16(lookback)
			duplicate := false
			for _, existing := range result[owner] {
				if existing == encoded {
					duplicate = true
					break
				}
			}
			if !duplicate {
				result[owner] = append(result[owner], encoded)
			}
		}
	}
}

func recordOwnedBombDetonation(
	metrics []AgentMetrics,
	playerIDs []uint16,
	actors []battleengine.Actor,
	event battleengine.Event,
) {
	if event.Kind != battleengine.EventBombExploded {
		return
	}
	owner := -1
	for index, playerID := range playerIDs {
		if playerID == event.PlayerID {
			owner = index
			break
		}
	}
	if owner < 0 || owner >= len(metrics) || owner >= len(actors) {
		return
	}
	metrics[owner].OwnedBombDetonations++
	// actors is the post-Step authoritative state, so survival is recorded only
	// after all simultaneous blast effects in this engine tick were applied.
	if actors[owner].State == battleengine.ActorActive {
		metrics[owner].SurvivedOwnedBombDetonations++
	}
}

func recordEnemyTraps(actors []battleengine.Actor, events []battleengine.Event, result []uint8) {
	for _, event := range events {
		if event.Kind != battleengine.EventActorTrapped || event.PlayerID == event.TargetID {
			continue
		}
		owner := actorIndexByPlayerID(actors, event.PlayerID)
		target := actorIndexByPlayerID(actors, event.TargetID)
		if owner < 0 || target < 0 || owner >= len(result) || actors[owner].TeamID == actors[target].TeamID {
			continue
		}
		if result[owner] < ^uint8(0) {
			result[owner]++
		}
	}
}

func recordDevelopmentProgress(actors []battleengine.Actor, events []battleengine.Event, result []uint8) {
	for _, event := range events {
		if event.Kind != battleengine.EventCellDestroyed && event.Kind != battleengine.EventPickupCollected {
			continue
		}
		owner := actorIndexByPlayerID(actors, event.PlayerID)
		if owner < 0 || owner >= len(result) {
			continue
		}
		if result[owner] < ^uint8(0) {
			result[owner]++
		}
	}
}

func recordPickupAttributeGain(metrics *AgentMetrics, event battleengine.Event) {
	if event.Kind != battleengine.EventPickupCollected || event.ValueAfter <= event.ValueBefore {
		return
	}
	gain := uint32(event.ValueAfter - event.ValueBefore)
	switch event.Attribute {
	case battleengine.AttributeBombCapacity:
		metrics.PickupCapacityGain += gain
	case battleengine.AttributeBombPower:
		metrics.PickupPowerGain += gain
	case battleengine.AttributeSpeedRate:
		metrics.PickupSpeedGain += gain
	}
}

// updateDevelopmentCurriculum recognizes only world changes authored by the
// engine. A team must first destroy at least one wall, then collect a pickup,
// and still have an active actor. An optional repeated-development stage also
// counts only distinct ticks where a real owned explosion both leaves its
// owner active and destroys terrain. Processing blast and wall events before
// pickup events makes simultaneous reveal/contact ordering irrelevant without
// crediting a pickup collected before any terrain was opened.
func updateDevelopmentCurriculum(episode *episode, events []battleengine.Event) (completed bool, winnerTeam byte, timedOut bool) {
	if episode == nil || !episode.metrics.DevelopmentCurriculum || episode.engine == nil {
		return false, 0, false
	}
	actors := episode.engine.Actors()
	state := &episode.development
	if state.settled {
		return false, 0, false
	}
	destroyedByOwner := make(map[uint16]struct{})
	for _, event := range events {
		if event.Kind != battleengine.EventCellDestroyed {
			continue
		}
		actorIndex := actorIndexByPlayerID(actors, event.PlayerID)
		if actorIndex >= 0 {
			state.wallsDestroyed[actors[actorIndex].TeamID] = true
			destroyedByOwner[event.PlayerID] = struct{}{}
		}
	}
	for _, ownerID := range safeBombEscapeOwners(events, actors) {
		if _, productive := destroyedByOwner[ownerID]; !productive {
			continue
		}
		actorIndex := actorIndexByPlayerID(actors, ownerID)
		if actorIndex < 0 {
			continue
		}
		teamID := actors[actorIndex].TeamID
		if state.productiveDetonations == nil {
			state.productiveDetonations = make(map[byte]byte)
		}
		if state.productiveDetonations[teamID] < ^byte(0) {
			state.productiveDetonations[teamID]++
		}
	}
	for _, event := range events {
		if event.Kind != battleengine.EventPickupCollected {
			continue
		}
		actorIndex := actorIndexByPlayerID(actors, event.PlayerID)
		if actorIndex < 0 {
			continue
		}
		teamID := actors[actorIndex].TeamID
		if state.wallsDestroyed[teamID] {
			state.pickups[teamID] = true
		}
	}
	winners := make([]byte, 0, 2)
	seen := make(map[byte]struct{}, 2)
	for _, actor := range actors {
		teamID := actor.TeamID
		if _, checked := seen[teamID]; checked {
			continue
		}
		seen[teamID] = struct{}{}
		if !state.wallsDestroyed[teamID] || !state.pickups[teamID] ||
			state.productiveDetonations[teamID] < state.requiredProductiveDetonations ||
			!teamHasActiveActor(actors, teamID) {
			continue
		}
		winners = append(winners, teamID)
	}
	if len(winners) == 1 {
		state.settled = true
		return true, winners[0], false
	}
	if len(winners) > 1 {
		state.settled = true
		return true, 0, false
	}
	if state.deadlineMS != 0 && episode.engine.ElapsedMS() >= state.deadlineMS {
		state.settled = true
		return true, 0, true
	}
	return false, 0, false
}

func teamHasActiveActor(actors []battleengine.Actor, teamID byte) bool {
	for _, actor := range actors {
		if actor.TeamID == teamID && actor.State == battleengine.ActorActive {
			return true
		}
	}
	return false
}

func (batch *Batch) accumulateEnemyApproach(envIndex int, rewards []float32) {
	episode := &batch.episodes[envIndex]
	actors := episode.engine.Actors()
	for actorIndex, actor := range actors {
		training := &episode.training[actorIndex]
		if actor.State == battleengine.ActorEliminated {
			training.hasEnemyDistance = false
			continue
		}
		nearest := -1
		cell := actor.Position.Cell()
		for _, candidate := range actors {
			if candidate.State == battleengine.ActorEliminated || candidate.TeamID == actor.TeamID {
				continue
			}
			other := candidate.Position.Cell()
			distance := absInt(int(cell.Row-other.Row)) + absInt(int(cell.Col-other.Col))
			if nearest < 0 || distance < nearest {
				nearest = distance
			}
		}
		if nearest < 0 {
			training.hasEnemyDistance = false
			continue
		}
		// Keep the baseline current while trapped, but do not pay movement
		// shaping. Clearing it here allowed approach -> trap -> rescue cycles to
		// earn the same approach potential repeatedly without moving away.
		if actor.State == battleengine.ActorActive && training.hasEnemyDistance {
			rewards[envIndex*batch.config.ParticipantCount+actorIndex] += enemyApproachReward(
				episode.engine.RoundElapsedMS(), training.enemyDistance, nearest,
			)
		}
		training.enemyDistance = nearest
		training.hasEnemyDistance = true
	}
}

func enemyApproachReward(elapsedMS uint32, previousDistance, currentDistance int) float32 {
	if elapsedMS < rewardDevelopmentEndMS {
		return 0
	}
	return float32(previousDistance-currentDistance) * rewardEnemyApproachPerCell
}

func developmentProgressReward(elapsedMS uint32, base, openingBonus float32) float32 {
	if elapsedMS < rewardDevelopmentEndMS {
		return base + openingBonus
	}
	return base
}

// pickupProgressReward values only consequences proven by the authoritative
// pickup event. Attribute items scale with the amount that actually changed
// the actor, so a large upgrade is worth its remaining distance to the native
// cap and an already-capped pickup earns no artificial development credit.
// Held-action pickups scale with the exact inventory count granted. A fork is
// additionally one finite self-rescue and uses the same bounded value as a
// genuine rescue; the later rescue remains responsible for reversing combat
// credit. Every source item is finite, so none of these signals can be farmed.
func pickupProgressReward(event battleengine.Event, elapsedMS uint32) float32 {
	if event.Attribute != battleengine.AttributeNone {
		if event.ValueAfter <= event.ValueBefore {
			return 0
		}
		units := float32(event.ValueAfter - event.ValueBefore)
		// A real attribute point has the same development value when picked
		// up later. Do not cut an equally useful upgrade to one quarter of
		// its value merely because the clock crossed thirty seconds.
		return (rewardPickup + rewardDevelopmentPickupBonus) * units
	}

	units := float32(1)
	if event.Effect == battleengine.PickupEffectBattleAction && event.ActionCount > 0 {
		units = float32(event.ActionCount)
	}
	reward := developmentProgressReward(
		elapsedMS,
		rewardPickup*units,
		rewardDevelopmentPickupBonus*units,
	)
	if event.Effect == battleengine.PickupEffectBattleAction && event.ActionID == 63 {
		reward += rewardRescue * units
	}
	return reward
}

func absInt(value int) int {
	if value < 0 {
		return -value
	}
	return value
}

// blockadeAssistOwners attributes only immediate, observable escape denial.
// An assistant must own either a bubble occupying one of the victim's four
// neighboring cells or a distinct flame that impacted such a cell in the same
// engine tick. Distant future blast rays are intentionally excluded: they may
// look threatening but did not physically close the route that produced this
// trap. The returned owner list is unique and stable.
func blockadeAssistOwners(engine *battleengine.Engine, actors []battleengine.Actor, targetIndex int, directOwnerID uint16) []uint16 {
	if engine == nil || targetIndex < 0 || targetIndex >= len(actors) {
		return nil
	}
	target := actors[targetIndex]
	neighbors := make(map[battleengine.Cell]struct{}, 4)
	for _, direction := range [...]battleengine.Cell{{Row: -1}, {Col: 1}, {Row: 1}, {Col: -1}} {
		cell := battleengine.Cell{Row: target.Position.Cell().Row + direction.Row, Col: target.Position.Cell().Col + direction.Col}
		if _, inside := engine.TileAt(cell); inside {
			neighbors[cell] = struct{}{}
		}
	}
	ownerTeam := func(playerID uint16) (uint8, bool) {
		for _, actor := range actors {
			if actor.PlayerID == playerID {
				return actor.TeamID, true
			}
		}
		return 0, false
	}
	owners := make(map[uint16]struct{})
	accept := func(playerID uint16) {
		if playerID == 0 || playerID == directOwnerID {
			return
		}
		teamID, ok := ownerTeam(playerID)
		if !ok || teamID == target.TeamID {
			return
		}
		owners[playerID] = struct{}{}
	}
	for _, bomb := range engine.Bombs() {
		if _, adjacent := neighbors[bomb.Cell]; adjacent {
			accept(bomb.OwnerID)
		}
	}
	for _, flame := range engine.Flames() {
		if flame.ImpactAtMS != engine.ElapsedMS() {
			continue
		}
		if _, adjacent := neighbors[flame.Cell]; adjacent {
			accept(flame.OwnerID)
		}
	}
	result := make([]uint16, 0, len(owners))
	for playerID := range owners {
		result = append(result, playerID)
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result
}

// SearchTeacher reranks the actor's visible-state logits with bounded tactical
// simulation. It is an offline demonstration/evaluation path: the live server
// defaults to one greedy actor inference and never calls this method.
func (batch *Batch) SearchTeacher(logits []float32, config battleengine.SearchConfig) ([][]battleengine.ActionID, error) {
	result, err := batch.SearchTeacherScores(logits, config)
	if err != nil {
		return nil, err
	}
	return result.Actions, nil
}

// SearchTeacherResult preserves the chosen action and the scored Top-K set.
// Status is zero for unevaluated actions, one for evaluated unsafe actions and
// two for evaluated actions that survive the bounded tactical rollout.
type SearchTeacherResult struct {
	Actions  [][]battleengine.ActionID
	Values   []float32
	Statuses []uint8
}

// SearchTeacherScores exposes the multi-action Search value distribution for
// offline distillation without changing the live one-action policy contract.
func (batch *Batch) SearchTeacherScores(logits []float32, config battleengine.SearchConfig) (*SearchTeacherResult, error) {
	return batch.SearchTeacherScoresSelected(logits, config, nil)
}

// SearchTeacherScoresSelected queries only the selected actors. Unselected
// actors retain their teacher continuation state and have zero output/status.
// A nil selection retains the full-batch behavior used by existing callers.
func (batch *Batch) SearchTeacherScoresSelected(logits []float32, config battleengine.SearchConfig, selected []bool) (*SearchTeacherResult, error) {
	return batch.searchTeacherScoresSelected(logits, config, selected, true)
}

// SearchTeacherLabelScoresSelected is a hypothetical label query for on-policy
// learning. Its action is not executed, so even the teacher's escape latch and
// Base pointer must remain unchanged. Native observations and physics are the
// same as an ordinary query; only controller-state ownership differs.
func (batch *Batch) SearchTeacherLabelScoresSelected(logits []float32, config battleengine.SearchConfig, selected []bool) (*SearchTeacherResult, error) {
	return batch.searchTeacherScoresSelected(logits, config, selected, false)
}

func (batch *Batch) searchTeacherScoresSelected(logits []float32, config battleengine.SearchConfig, selected []bool, advanceTeacher bool) (*SearchTeacherResult, error) {
	if batch == nil {
		return nil, fmt.Errorf("training batch is nil")
	}
	actionCount := int(battleengine.DiscreteActionCount)
	expected := len(batch.episodes) * batch.config.ParticipantCount * actionCount
	if len(logits) != expected {
		return nil, fmt.Errorf("search-teacher logit count %d, want %d", len(logits), expected)
	}
	if selected != nil && len(selected) != len(batch.episodes)*batch.config.ParticipantCount {
		return nil, fmt.Errorf("search-teacher selection count %d, want %d", len(selected), len(batch.episodes)*batch.config.ParticipantCount)
	}
	result := &SearchTeacherResult{
		Actions:  make([][]battleengine.ActionID, len(batch.episodes)),
		Values:   make([]float32, expected),
		Statuses: make([]uint8, expected),
	}
	indices := make([]int, len(batch.episodes))
	for index := range indices {
		indices[index] = index
		result.Actions[index] = make([]battleengine.ActionID, batch.config.ParticipantCount)
	}
	err := batch.parallelIndices(indices, func(envIndex int) error {
		episode := &batch.episodes[envIndex]
		for actorIndex, playerID := range episode.playerIDs {
			if selected != nil && !selected[envIndex*batch.config.ParticipantCount+actorIndex] {
				continue
			}
			observation, err := episode.engine.Observation(playerID)
			if err != nil {
				return fmt.Errorf("environment %d observe player %d: %w", envIndex, playerID, err)
			}
			legal, err := episode.engine.LegalActions(playerID)
			if err != nil {
				return fmt.Errorf("environment %d legal player %d: %w", envIndex, playerID, err)
			}
			candidates := make(fixedSearchCandidates, 0, len(legal))
			base := (envIndex*batch.config.ParticipantCount + actorIndex) * actionCount
			for _, action := range legal {
				actionID, ok := action.ID()
				if !ok {
					return fmt.Errorf("environment %d player %d legal action has no stable ID", envIndex, playerID)
				}
				score := logits[base+int(actionID)]
				// The Python safety layer uses a large negative sentinel for
				// actions it intentionally excludes. Do not refill Top-K with
				// those actions merely because an actor has few safe choices.
				if score <= -1e8 {
					continue
				}
				candidates = append(candidates, battleengine.ScoredAction{Action: action, Score: score})
			}
			if len(candidates) == 0 {
				return fmt.Errorf("environment %d player %d has no search candidates after safety mask", envIndex, playerID)
			}
			sort.SliceStable(candidates, func(i, j int) bool {
				if candidates[i].Score == candidates[j].Score {
					left, _ := candidates[i].Action.ID()
					right, _ := candidates[j].Action.ID()
					return left < right
				}
				return candidates[i].Score > candidates[j].Score
			})
			snapshot, err := episode.engine.PolicySnapshot(playerID)
			if err != nil {
				return fmt.Errorf("environment %d policy snapshot player %d: %w", envIndex, playerID, err)
			}
			policy := battleengine.TopKSearchPolicy{Candidate: candidates, Config: config}
			ranked, err := policy.RankActionsWithSnapshot(snapshot, observation, legal)
			if err != nil {
				return fmt.Errorf("environment %d search player %d: %w", envIndex, playerID, err)
			}
			if len(ranked) == 0 {
				return fmt.Errorf("environment %d search player %d returned no ranked action", envIndex, playerID)
			}
			for _, scored := range ranked {
				actionID, ok := scored.Action.ID()
				if !ok {
					return fmt.Errorf("environment %d search player %d returned unstable scored action", envIndex, playerID)
				}
				offset := base + int(actionID)
				result.Values[offset] = float32(scored.Value)
				result.Statuses[offset] = 1
				if scored.Survives {
					result.Statuses[offset] = 2
				}
			}
			chosen := ranked[0].Action
			if config.TacticalSafety {
				// Search values assume tacticalEscapeRollout supplies the continuation
				// after the first action. Preserve that same continuation across actual
				// teacher decisions; otherwise the next actor step can reverse into the
				// very blast that made the speculative placement look safe.
				safety := episode.searchSafety[actorIndex]
				if !advanceTeacher {
					copy := *safety
					safety = &copy
				}
				safety.Base = fixedSearchAction{action: chosen}
				chosen, err = safety.ChooseActionWithSnapshot(snapshot, observation, legal)
				if err != nil {
					return fmt.Errorf("environment %d guard search player %d: %w", envIndex, playerID, err)
				}
			}
			actionID, ok := chosen.ID()
			if !ok {
				return fmt.Errorf("environment %d search player %d returned unstable action", envIndex, playerID)
			}
			result.Actions[envIndex][actorIndex] = actionID
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (batch *Batch) accumulateEvents(envIndex int, events []battleengine.Event, rewards []float32) {
	episode := &batch.episodes[envIndex]
	actors := episode.engine.Actors()
	actorIndex := func(playerID uint16) int {
		for index, candidate := range episode.playerIDs {
			if candidate == playerID {
				return index
			}
		}
		return -1
	}
	add := func(playerID uint16, amount float32) bool {
		if index := actorIndex(playerID); index >= 0 {
			rewards[envIndex*batch.config.ParticipantCount+index] += amount
			return true
		}
		return false
	}
	applyDelta := func(deltas *[]rewardDelta, playerID uint16, amount float32) {
		if amount != 0 && add(playerID, amount) && deltas != nil {
			*deltas = append(*deltas, rewardDelta{playerID: playerID, amount: amount})
		}
	}
	addCombatResult := func(owner, target int, amount float32, deltas *[]rewardDelta) {
		if owner >= 0 && target >= 0 && owner != target {
			if actors[owner].TeamID == actors[target].TeamID {
				applyDelta(deltas, actors[owner].PlayerID, -amount*rewardFriendlyFireFactor)
			} else {
				applyDelta(deltas, actors[owner].PlayerID, amount)
			}
		}
		if target >= 0 {
			applyDelta(deltas, actors[target].PlayerID, -amount)
		}
	}
	markProgress := func(owner int, combat bool) {
		if owner >= 0 {
			elapsed := episode.engine.RoundElapsedMS()
			// Terrain and pickups are useful early development, but after the
			// midpoint only an actual interaction with another team postpones
			// the escalating anti-stall cost.
			if combat || elapsed < rewardLateStallStartMS {
				episode.teamProgress[actors[owner].TeamID] = elapsed
			}
		}
	}
	isEnemyResult := func(owner, target int) bool {
		return owner >= 0 && target >= 0 && owner != target && actors[owner].TeamID != actors[target].TeamID
	}
	teamImpactScale := func(target int) float32 {
		if target < 0 || target >= len(actors) {
			return 1
		}
		members := 0
		for _, actor := range actors {
			if actor.TeamID == actors[target].TeamID {
				members++
			}
		}
		if members <= 1 {
			return 1
		}
		return 1 / float32(members)
	}
	rollbackTrap := func(targetID uint16) (trapRewardCredit, bool) {
		credit, ok := episode.trapCredits[targetID]
		if !ok {
			return trapRewardCredit{}, false
		}
		for _, delta := range credit.deltas {
			add(delta.playerID, -delta.amount)
		}
		delete(episode.trapCredits, targetID)
		return credit, true
	}
	for _, event := range events {
		owner := actorIndex(event.PlayerID)
		target := actorIndex(event.TargetID)
		switch event.Kind {
		case battleengine.EventActorMoved:
			if owner >= 0 {
				episode.metrics.Agents[owner].MovedTicks++
			}
		case battleengine.EventBombPlaced:
			if owner >= 0 {
				episode.metrics.Agents[owner].BombsPlaced++
			}
			add(event.PlayerID, rewardBombPlaced)
		case battleengine.EventBombExploded:
			recordOwnedBombDetonation(episode.metrics.Agents, episode.playerIDs, actors, event)
		case battleengine.EventCellDestroyed:
			if owner >= 0 {
				episode.metrics.Agents[owner].WallsDestroyed++
			}
			add(event.PlayerID, developmentProgressReward(
				episode.engine.RoundElapsedMS(), rewardWallDestroyed, rewardDevelopmentWallBonus,
			))
			markProgress(owner, false)
		case battleengine.EventPickupCollected:
			if owner >= 0 {
				episode.metrics.Agents[owner].Pickups++
				recordPickupAttributeGain(&episode.metrics.Agents[owner], event)
			}
			add(event.PlayerID, pickupProgressReward(
				event, episode.engine.RoundElapsedMS(),
			))
			markProgress(owner, false)
		case battleengine.EventFieldObjectTriggered:
			if event.ActionID == 42 || event.ActionID == 43 {
				addCombatResult(owner, target, rewardMovementDebuff*teamImpactScale(target), nil)
				if isEnemyResult(owner, target) {
					markProgress(owner, true)
				}
			}
		case battleengine.EventActorTrapped:
			if isEnemyResult(owner, target) {
				episode.metrics.Agents[owner].Traps++
			}
			// A duplicated/reconciled hit must replace, not stack with, an
			// unresolved credit for the same target.
			rollbackTrap(event.TargetID)
			credit := trapRewardCredit{enemyCaused: isEnemyResult(owner, target)}
			impactScale := teamImpactScale(target)
			addCombatResult(owner, target, rewardTrap*impactScale, &credit.deltas)
			if credit.enemyCaused {
				markProgress(owner, true)
				assistants := blockadeAssistOwners(episode.engine, actors, target, event.PlayerID)
				if len(assistants) != 0 {
					share := rewardTrapAssistTotal * impactScale / float32(len(assistants))
					for _, assistantID := range assistants {
						applyDelta(&credit.deltas, assistantID, share)
						assistant := actorIndex(assistantID)
						if assistant >= 0 {
							episode.metrics.Agents[assistant].TrapAssists++
							markProgress(assistant, true)
						}
					}
				}
			}
			if target >= 0 {
				episode.trapCredits[event.TargetID] = credit
			}
		case battleengine.EventActorEliminated:
			// Elimination confirms the pending trap advantage; keep the
			// deltas but remove the rollback handle before awarding the kill.
			delete(episode.trapCredits, event.TargetID)
			if isEnemyResult(owner, target) {
				episode.metrics.Agents[owner].Eliminations++
			}
			if owner >= 0 && owner == target {
				episode.metrics.Agents[owner].SelfEliminations++
			}
			addCombatResult(owner, target, rewardElimination*teamImpactScale(target), nil)
			if isEnemyResult(owner, target) {
				markProgress(owner, true)
			}
		case battleengine.EventActorRescued:
			credit, hadTrap := rollbackTrap(event.TargetID)
			validTeamRescue := owner >= 0 && target >= 0 && actors[owner].TeamID == actors[target].TeamID
			forkSelfRescue := event.ActionID == 63 && event.PlayerID == event.TargetID
			// Genuine enemy-caused rescue (or a finite fork self-rescue) spends
			// half of its remaining bounded budget. Friendly trap/rescue
			// cooperation earns nothing, while repeated useful rescues remain
			// learnable with 0.20, 0.10, 0.05... diminishing credit.
			if forkSelfRescue || (hadTrap && credit.enemyCaused && validTeamRescue) {
				paid := episode.rescueRewardPaid[event.TargetID]
				budget := rewardRescueBudget * teamImpactScale(target)
				bonus := (budget - paid) / 2
				if bonus > 0 {
					add(event.PlayerID, bonus)
					episode.rescueRewardPaid[event.TargetID] = paid + bonus
				}
				if owner >= 0 {
					episode.metrics.Agents[owner].Rescues++
				}
				markProgress(owner, true)
			}
		case battleengine.EventActorTransformationEnded:
			if event.TransformationEnd == battleengine.TransformationEndHit {
				// Transformation events name the victim in PlayerID and the
				// attacking source in TargetID, opposite to trap/elimination.
				addCombatResult(target, owner, rewardTransformationBroken*teamImpactScale(owner), nil)
				if isEnemyResult(target, owner) {
					markProgress(target, true)
				}
			}
		case battleengine.EventBattleActionUsed:
			if owner >= 0 {
				episode.metrics.Agents[owner].ActionsUsed++
			}
		case battleengine.EventNativePassStarted:
			if owner >= 0 {
				episode.metrics.Agents[owner].NativePasses++
			}
		}
	}
}

func lateStallPenalty(elapsedMS, lastProgressMS, tickMS uint32) float32 {
	if elapsedMS < rewardLateStallStartMS || elapsedMS >= rewardRoundDurationMS || elapsedMS-lastProgressMS <= rewardLateStallProgressGraceMS {
		return 0
	}
	if tickMS > rewardRoundDurationMS-elapsedMS {
		tickMS = rewardRoundDurationMS - elapsedMS
	}
	window := float64(rewardRoundDurationMS - rewardLateStallStartMS)
	start := float64(elapsedMS-rewardLateStallStartMS) / window
	end := float64(elapsedMS-rewardLateStallStartMS+tickMS) / window
	// Integral of the increasing density over this tick. Its full-round
	// integral is one; progress/grace can only remove charge, never reset
	// a budget or make the accumulated penalty larger.
	ramp := float64(rewardLateStallMaxMultiplier - 1)
	mass := ((end - start) + ramp*(end*end-start*start)/2) / (1 + ramp/2)
	return -rewardLateStallBudget * float32(mass)
}

func lateStallResponsibility(teamID byte, actors []battleengine.Actor) float32 {
	teamLive := 0
	opponentLive := 0
	for _, actor := range actors {
		if actor.State == battleengine.ActorEliminated {
			continue
		}
		if actor.TeamID == teamID {
			teamLive++
		} else {
			opponentLive++
		}
	}
	if teamLive < opponentLive {
		return 0
	}
	totalLive := teamLive + opponentLive
	if totalLive <= 0 {
		return 0
	}
	advantage := float32(teamLive-opponentLive) / float32(totalLive)
	return 1 + advantage
}

func (batch *Batch) accumulateLateStallBehavior(envIndex int, rewards []float32) {
	episode := &batch.episodes[envIndex]
	elapsedMS := episode.engine.RoundElapsedMS()
	actors := episode.engine.Actors()
	for actorIndex, actor := range actors {
		if actor.State != battleengine.ActorActive {
			continue
		}
		responsibility := lateStallResponsibility(actor.TeamID, actors)
		penalty := responsibility * lateStallPenalty(elapsedMS, episode.teamProgress[actor.TeamID], batch.config.TickMS)
		if penalty == 0 {
			continue
		}
		episode.metrics.Agents[actorIndex].LateStallTicks++
		rewards[envIndex*batch.config.ParticipantCount+actorIndex] += penalty
	}
}

func (batch *Batch) accumulateBlockedCellBehavior(envIndex int, rewards []float32) {
	episode := &batch.episodes[envIndex]
	actors := episode.engine.Actors()
	graceTicks := (rewardBlockedCellGraceMS + batch.config.TickMS - 1) / batch.config.TickMS
	penaltyPerTick := rewardBlockedCellPenaltyPerSecond * float32(batch.config.TickMS) / 1_000
	for actorIndex, actor := range actors {
		training := &episode.training[actorIndex]
		if actor.State != battleengine.ActorActive {
			training.blocked = false
			training.blockedTicks = 0
			continue
		}
		tile, inside := episode.engine.TileAt(actor.Position.Cell())
		if !inside || tile.Kind == battleengine.CellOpen {
			if training.blocked {
				training.hasBlockedExit = true
				training.lastBlockedExitMS = episode.engine.ElapsedMS()
			}
			training.blocked = false
			training.blockedTicks = 0
			continue
		}
		if !training.blocked {
			training.blocked = true
			metrics := &episode.metrics.Agents[actorIndex]
			metrics.BlockedEntries++
			rewardIndex := envIndex*batch.config.ParticipantCount + actorIndex
			if metrics.BlockedEntries > rewardFreeBlockedEntries {
				penalty := float32(metrics.BlockedEntries-rewardFreeBlockedEntries) * rewardRepeatedBlockedEntryPenalty
				if penalty > rewardBlockedEntryPenaltyCap {
					penalty = rewardBlockedEntryPenaltyCap
				}
				rewards[rewardIndex] -= penalty
			}
			if training.hasBlockedExit && episode.engine.ElapsedMS()-training.lastBlockedExitMS < rewardBlockedReentryWindowMS {
				rewards[rewardIndex] -= rewardRapidBlockedReentryPenalty
			}
		}
		training.blockedTicks++
		episode.metrics.Agents[actorIndex].BlockedTicks++
		if training.blockedTicks <= graceTicks {
			continue
		}
		rewardIndex := envIndex*batch.config.ParticipantCount + actorIndex
		rewards[rewardIndex] -= penaltyPerTick
	}
}

func (batch *Batch) Metrics() []EpisodeMetrics {
	if batch == nil {
		return nil
	}
	result := make([]EpisodeMetrics, len(batch.episodes))
	for index := range batch.episodes {
		result[index] = batch.episodes[index].metrics
		result[index].Agents = append([]AgentMetrics(nil), batch.episodes[index].metrics.Agents...)
	}
	return result
}

func (batch *Batch) MapIDs() []uint32 {
	if batch == nil {
		return nil
	}
	result := make([]uint32, len(batch.episodes))
	for index := range batch.episodes {
		result[index] = batch.episodes[index].metrics.MapID
	}
	return result
}

func splitMix64(value uint64) uint64 {
	value += 0x9e3779b97f4a7c15
	value = (value ^ (value >> 30)) * 0xbf58476d1ce4e5b9
	value = (value ^ (value >> 27)) * 0x94d049bb133111eb
	return value ^ (value >> 31)
}
