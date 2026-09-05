package battleengine

import (
	"fmt"
	"sort"
)

// Engine owns only deterministic rule state. It contains no sockets, wall
// clocks, goroutines or callbacks, which makes the same transition core safe
// for the live server, replay verification and later vectorized RL workers.
type Engine struct {
	seed                    uint64
	rngState                uint64
	rules                   Rules
	grid                    Grid
	actors                  []Actor
	bombs                   []Bomb
	flames                  []Flame
	fieldObjects            []FieldObject
	projectiles             []ActionProjectile
	pickups                 []Pickup
	pendingPickupDispatches []pendingPickupDispatch
	recycledPickupSceneIDs  []uint32
	lastPickupDispatchMS    uint32
	publicWallItems         PublicWallItemProfile
	elapsedMS               uint32
	nextBombID              uint32
	nextFieldObjectID       uint32
	nextProjectileID        uint32
	outcome                 Outcome
	nativeContactRequests   map[nativeContactRequestKey]struct{}
}

type nativeContactRequestKey struct {
	sourceID uint16
	targetID uint16
	rescue   bool
}

func New(config Config) (*Engine, error) {
	engine := &Engine{}
	if err := engine.Reset(config); err != nil {
		return nil, err
	}
	return engine, nil
}

// Reset replaces all state with a deep copy of config.
func (engine *Engine) Reset(config Config) error {
	if engine == nil {
		return fmt.Errorf("battle engine is nil")
	}
	if err := config.Grid.validate(); err != nil {
		return err
	}
	if err := config.Rules.validate(); err != nil {
		return err
	}
	if len(config.Participants) < 2 || len(config.Participants) > MaxParticipants {
		return fmt.Errorf("battle participant count %d is outside 2..%d", len(config.Participants), MaxParticipants)
	}
	participants := append([]Participant(nil), config.Participants...)
	sort.Slice(participants, func(i, j int) bool { return participants[i].PlayerID < participants[j].PlayerID })
	teamSeen := make(map[byte]struct{}, len(participants))
	actors := make([]Actor, 0, len(participants))
	for index, participant := range participants {
		if participant.PlayerID == 0 {
			return fmt.Errorf("battle participant %d has zero player ID", index)
		}
		if index > 0 && participants[index-1].PlayerID == participant.PlayerID {
			return fmt.Errorf("battle participant repeats player ID %d", participant.PlayerID)
		}
		if participant.TeamID == 0 {
			return fmt.Errorf("battle participant %d has zero team ID", participant.PlayerID)
		}
		if participant.Source > ParticipantVirtualAI {
			return fmt.Errorf("battle participant %d has invalid source %d", participant.PlayerID, participant.Source)
		}
		if participant.BombCapacity == 0 || participant.BombPower == 0 {
			return fmt.Errorf("battle participant %d has zero bomb capacity or bomb power", participant.PlayerID)
		}
		if participant.MaxBombCapacity == 0 {
			participant.MaxBombCapacity = participant.BombCapacity
		}
		if participant.MaxBombPower == 0 {
			participant.MaxBombPower = participant.BombPower
		}
		if participant.BombCapacity > participant.MaxBombCapacity || participant.BombPower > participant.MaxBombPower {
			return fmt.Errorf("battle participant %d current bomb attributes exceed their maxima", participant.PlayerID)
		}
		if participant.SpeedRate == 0 {
			if participant.MaxSpeedRate != 0 {
				return fmt.Errorf("battle participant %d has a maximum speed rate without a current rate", participant.PlayerID)
			}
			if participant.SpeedPixelsPerSecond == 0 {
				return fmt.Errorf("battle participant %d has zero movement speed", participant.PlayerID)
			}
		} else {
			if participant.MaxSpeedRate == 0 {
				participant.MaxSpeedRate = participant.SpeedRate
			}
			if participant.SpeedRate > participant.MaxSpeedRate || participant.MaxSpeedRate > MaxNativeSpeedRate {
				return fmt.Errorf("battle participant %d has invalid native speed rate %d/%d", participant.PlayerID, participant.SpeedRate, participant.MaxSpeedRate)
			}
			for rate := participant.SpeedRate; rate <= participant.MaxSpeedRate; rate++ {
				if config.Rules.SpeedPixelsPerSecondByRate[rate] == 0 {
					return fmt.Errorf("battle participant %d has no pixels/second projection for native speed rate %d", participant.PlayerID, rate)
				}
			}
			projected := config.Rules.SpeedPixelsPerSecondByRate[participant.SpeedRate]
			if participant.SpeedPixelsPerSecond != 0 && participant.SpeedPixelsPerSecond != projected {
				return fmt.Errorf("battle participant %d speed %d disagrees with native rate %d projection %d", participant.PlayerID, participant.SpeedPixelsPerSecond, participant.SpeedRate, projected)
			}
			participant.SpeedPixelsPerSecond = projected
		}
		tile, ok := config.Grid.Cell(participant.Spawn)
		if !ok || tile.Kind != CellOpen {
			return fmt.Errorf("battle participant %d spawn %d,%d is not open", participant.PlayerID, participant.Spawn.Row, participant.Spawn.Col)
		}
		position := PositionAtCellCenter(participant.Spawn)
		actors = append(actors, Actor{
			Participant: participant, Position: position, State: ActorActive, Facing: DirectionDown,
			OxygenValue: NativeOxygenValueInitial,
		})
		teamSeen[participant.TeamID] = struct{}{}
		participants[index] = participant
	}
	if len(teamSeen) < 2 {
		return fmt.Errorf("standard battle requires at least two teams")
	}
	pickups := append([]Pickup(nil), config.Pickups...)
	if err := validateInitialPickups(config.Grid, config.Rules, participants, pickups); err != nil {
		return err
	}
	sort.Slice(pickups, func(i, j int) bool {
		if pickups[i].Cell.Row != pickups[j].Cell.Row {
			return pickups[i].Cell.Row < pickups[j].Cell.Row
		}
		if pickups[i].Cell.Col != pickups[j].Cell.Col {
			return pickups[i].Cell.Col < pickups[j].Cell.Col
		}
		return pickups[i].SceneID < pickups[j].SceneID
	})
	seed := config.Seed
	if seed == 0 {
		seed = 0x9e3779b97f4a7c15
	}
	engine.seed = config.Seed
	engine.rngState = seed
	engine.rules = config.Rules
	engine.grid = config.Grid.Clone()
	engine.actors = actors
	engine.bombs = nil
	engine.flames = nil
	engine.fieldObjects = nil
	engine.projectiles = nil
	engine.pickups = pickups
	engine.pendingPickupDispatches = nil
	engine.recycledPickupSceneIDs = nil
	engine.lastPickupDispatchMS = 0
	engine.publicWallItems = config.PublicWallItemProfile
	engine.elapsedMS = config.Rules.StartClockMS
	engine.nextBombID = 1
	engine.nextFieldObjectID = 1
	engine.nextProjectileID = 1
	engine.outcome = Outcome{}
	engine.nativeContactRequests = nil
	return nil
}

func (engine *Engine) Clone() *Engine {
	if engine == nil {
		return nil
	}
	clone := *engine
	clone.grid = engine.grid.Clone()
	clone.actors = append([]Actor(nil), engine.actors...)
	clone.bombs = append([]Bomb(nil), engine.bombs...)
	clone.flames = append([]Flame(nil), engine.flames...)
	clone.fieldObjects = append([]FieldObject(nil), engine.fieldObjects...)
	for index := range clone.fieldObjects {
		clone.fieldObjects[index].PassableBy = append([]uint16(nil), engine.fieldObjects[index].PassableBy...)
	}
	clone.pickups = append([]Pickup(nil), engine.pickups...)
	clone.pendingPickupDispatches = append([]pendingPickupDispatch(nil), engine.pendingPickupDispatches...)
	clone.recycledPickupSceneIDs = append([]uint32(nil), engine.recycledPickupSceneIDs...)
	clone.projectiles = append([]ActionProjectile(nil), engine.projectiles...)
	if len(engine.nativeContactRequests) != 0 {
		clone.nativeContactRequests = make(map[nativeContactRequestKey]struct{}, len(engine.nativeContactRequests))
		for key := range engine.nativeContactRequests {
			clone.nativeContactRequests[key] = struct{}{}
		}
	}
	return &clone
}

// PolicySnapshot returns an isolated simulation state with the observer's
// information boundary applied. Hidden wall pickups outside an active native
// detector's 3x3 view are removed, so tactical search cannot learn their
// rolled coordinates by speculatively destroying a wall. Public terrain,
// existing bombs and already revealed pickups remain exact.
func (engine *Engine) PolicySnapshot(playerID uint16) (*Engine, error) {
	observation, err := engine.Observation(playerID)
	if err != nil {
		return nil, err
	}
	visibleHidden := make(map[Pickup]struct{})
	for _, pickup := range observation.Pickups {
		if pickup.State == PickupHidden {
			visibleHidden[pickup] = struct{}{}
		}
	}
	clone := engine.Clone()
	filtered := clone.pickups[:0]
	for _, pickup := range clone.pickups {
		if pickup.State == PickupHidden {
			if _, visible := visibleHidden[pickup]; !visible {
				continue
			}
		}
		filtered = append(filtered, pickup)
	}
	clone.pickups = filtered
	// FUN_005e259d does not create a scene object until the bird crosses the
	// target column. The object then remains in native state 3 while moving to
	// its cell; the dispatch scheduler's contact guard prevents a policy snapshot
	// from revealing authenticated coordinates before it is normally observable.
	clone.pendingPickupDispatches = nil
	return clone, nil
}

func (engine *Engine) ElapsedMS() uint32 {
	if engine == nil {
		return 0
	}
	return engine.elapsedMS
}

// RoundElapsedMS excludes the frozen native scene countdown represented by
// Rules.StartClockMS. Engine event timestamps remain on the original client's
// game clock while timeout/reward logic observes only controllable play time.
func (engine *Engine) RoundElapsedMS() uint32 {
	if engine == nil || engine.elapsedMS <= engine.rules.StartClockMS {
		return 0
	}
	return engine.elapsedMS - engine.rules.StartClockMS
}

func (engine *Engine) Terminal() Outcome {
	if engine == nil {
		return Outcome{Ended: true, Draw: true}
	}
	return engine.outcome
}

func (engine *Engine) Grid() Grid {
	if engine == nil {
		return Grid{}
	}
	return engine.grid.Clone()
}

// TileAt exposes one current public terrain cell without cloning the entire
// grid. Training uses it only for behavior shaping (for example detecting an
// actor that remains embedded in a wall after a native pass); live combat
// rules never depend on that shaping state.
func (engine *Engine) TileAt(cell Cell) (Tile, bool) {
	if engine == nil {
		return Tile{Kind: CellSolid}, false
	}
	return engine.grid.Cell(cell)
}

func (engine *Engine) Actors() []Actor {
	if engine == nil {
		return nil
	}
	return append([]Actor(nil), engine.actors...)
}

// ClearPublicBehaviorMemories starts a new controller-history boundary without
// changing physical battle state. Offline state replay may attach different
// policies to the cloned actors, so the previous controllers' public action
// frequencies must not be presented as evidence about the new controllers.
func (engine *Engine) ClearPublicBehaviorMemories() {
	if engine == nil {
		return
	}
	for index := range engine.actors {
		engine.actors[index].PublicBehavior = PublicBehaviorMemory{}
	}
}

func (engine *Engine) Bombs() []Bomb {
	if engine == nil {
		return nil
	}
	return append([]Bomb(nil), engine.bombs...)
}

func (engine *Engine) Flames() []Flame {
	if engine == nil {
		return nil
	}
	return append([]Flame(nil), engine.flames...)
}

func (engine *Engine) FieldObjects() []FieldObject {
	if engine == nil {
		return nil
	}
	result := append([]FieldObject(nil), engine.fieldObjects...)
	for index := range result {
		result[index].PassableBy = append([]uint16(nil), result[index].PassableBy...)
	}
	return result
}

func (engine *Engine) ActionProjectiles() []ActionProjectile {
	if engine == nil {
		return nil
	}
	return append([]ActionProjectile(nil), engine.projectiles...)
}

func (engine *Engine) Pickups() []Pickup {
	if engine == nil {
		return nil
	}
	return append([]Pickup(nil), engine.pickups...)
}

func (engine *Engine) EligibleArbitratorIDs() []uint16 {
	if engine == nil {
		return nil
	}
	result := make([]uint16, 0, len(engine.actors))
	for _, actor := range engine.actors {
		if actor.Source == ParticipantHuman && actor.State != ActorEliminated {
			result = append(result, actor.PlayerID)
		}
	}
	return result
}

// SelectArbitrator makes the virtual-AI exclusion explicit at the engine
// boundary. Live room migration can use this helper without knowing policy
// implementation details.
func (engine *Engine) SelectArbitrator(preferredID uint16) (uint16, error) {
	eligible := engine.EligibleArbitratorIDs()
	if len(eligible) == 0 {
		return 0, fmt.Errorf("battle has no eligible human arbitrator")
	}
	for _, playerID := range eligible {
		if playerID == preferredID {
			return playerID, nil
		}
	}
	return eligible[0], nil
}

func (engine *Engine) actorIndex(playerID uint16) int {
	index := sort.Search(len(engine.actors), func(index int) bool { return engine.actors[index].PlayerID >= playerID })
	if index == len(engine.actors) || engine.actors[index].PlayerID != playerID {
		return -1
	}
	return index
}

func (engine *Engine) activeBombCount(playerID uint16) int {
	count := 0
	for _, bomb := range engine.bombs {
		if bomb.OwnerID == playerID {
			count++
		}
	}
	return count
}

func (engine *Engine) bombAt(cell Cell) int {
	for index := range engine.bombs {
		if engine.bombs[index].Cell == cell {
			return index
		}
	}
	return -1
}

func containsSortedPlayerID(ids []uint16, playerID uint16) bool {
	index := sort.Search(len(ids), func(index int) bool { return ids[index] >= playerID })
	return index < len(ids) && ids[index] == playerID
}

func removeSortedPlayerID(ids []uint16, playerID uint16) []uint16 {
	index := sort.Search(len(ids), func(index int) bool { return ids[index] >= playerID })
	if index == len(ids) || ids[index] != playerID {
		return ids
	}
	return append(ids[:index], ids[index+1:]...)
}
