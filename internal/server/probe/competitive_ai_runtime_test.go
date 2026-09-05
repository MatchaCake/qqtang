package probe

import (
	"io"
	"net"
	"os"
	"reflect"
	"sort"
	"testing"
	"time"

	"qqtang/internal/game/battleai"
	"qqtang/internal/game/battleengine"
	"qqtang/internal/game/match"
	"qqtang/internal/protocol/game"
)

type competitiveAIActorPolicyFactoryProbe struct {
	created int
}

func (*competitiveAIActorPolicyFactoryProbe) ChooseAction(
	observation battleengine.Observation,
	_ []battleengine.Action,
) (battleengine.Action, error) {
	return battleengine.Action{PlayerID: observation.PlayerID}, nil
}

func (factory *competitiveAIActorPolicyFactoryProbe) NewActorPolicy() battleengine.Policy {
	factory.created++
	return battleengine.PolicyFunc(func(
		observation battleengine.Observation,
		_ []battleengine.Action,
	) (battleengine.Action, error) {
		return battleengine.Action{PlayerID: observation.PlayerID}, nil
	})
}

func TestCompetitiveAIRuntimeForksPolicyStatePerVirtualActor(t *testing.T) {
	participants := []match.CompetitiveParticipant{
		{PlayerID: 1, RoleID: 1, TeamID: 1, Source: match.CompetitiveParticipantHuman},
		{PlayerID: 2, RoleID: 4, TeamID: 1, Source: match.CompetitiveParticipantHuman},
		{PlayerID: 20001, RoleID: 2, TeamID: 2, Source: match.CompetitiveParticipantVirtualAI},
		{PlayerID: 20002, RoleID: 3, TeamID: 2, Source: match.CompetitiveParticipantVirtualAI},
	}
	factory := &competitiveAIActorPolicyFactoryProbe{}
	if _, err := newLiveCompetitiveAIRuntime(
		7,
		testCompetitiveAIMap(4),
		game.GameBeginData{GameID: 99, SpawnSeed: 11, ItemSeed: 22},
		participants,
		false,
		factory,
		100,
	); err != nil {
		t.Fatal(err)
	}
	if factory.created != 2 {
		t.Fatalf("actor-local policies created = %d, want 2", factory.created)
	}
}

func BenchmarkCompetitiveAILiveWorldStep(b *testing.B) {
	participants := []match.CompetitiveParticipant{
		{PlayerID: 1, RoleID: 1, TeamID: 1, Source: match.CompetitiveParticipantHuman},
	}
	for index := uint16(0); index < 7; index++ {
		teamID := byte(2)
		if index < 3 {
			teamID = 1
		}
		participants = append(participants, match.CompetitiveParticipant{
			PlayerID: 20001 + index, RoleID: 2, TeamID: teamID, Source: match.CompetitiveParticipantVirtualAI,
		})
	}
	modelPath := os.Getenv("QQTANG_AI_CANDIDATE_MODEL")
	for _, benchmark := range []struct {
		name           string
		searchEnabled  bool
		intraOpThreads int
		freeForAll     bool
	}{
		{"greedy", false, 0, false},
		{"greedy_single_thread", false, 1, false},
		{"greedy_ffa_single_thread", false, 1, true},
		{"top_k_search_single_thread", true, 1, false},
	} {
		b.Run(benchmark.name, func(b *testing.B) {
			participants := append([]match.CompetitiveParticipant(nil), participants...)
			if benchmark.freeForAll {
				for index := range participants {
					participants[index].TeamID = byte(index + 1)
				}
			}
			policyConfig := battleai.NativePolicyConfig{
				DangerHorizonMS: 3_500,
				DecisionMS:      100,
				EnableSearch:    benchmark.searchEnabled,
				Search: battleengine.SearchConfig{
					TopK: 4, HorizonMS: battleengine.NativeBombFuseMS + 200, DangerHorizonMS: 3_500,
					PriorWeight: 0.35, EliminationValue: 100, TrapValue: 12,
				},
			}
			deployment := battleai.DeploymentPolicyConfig{Backend: battleai.DeploymentBackendNative, ModelPath: modelPath, NativePolicyConfig: policyConfig}
			if onnx := os.Getenv("QQTANG_TEST_ONNX_MODEL"); onnx != "" {
				deployment.Backend = battleai.DeploymentBackendONNXRuntime
				deployment.ModelPath = onnx
				deployment.MetadataPath = os.Getenv("QQTANG_TEST_ONNX_METADATA")
				deployment.SharedLibraryPath = os.Getenv("QQTANG_TEST_ONNX_RUNTIME")
				deployment.IntraOpThreads = benchmark.intraOpThreads
				deployment.InterOpThreads = 1
			}
			if deployment.ModelPath == "" {
				b.Skip("set QQTANG_TEST_ONNX_MODEL or QQTANG_AI_CANDIDATE_MODEL for inference benchmarking")
			}
			loaded, err := battleai.LoadDeploymentPolicy(deployment)
			if err != nil {
				b.Fatal(err)
			}
			if loaded.Closer != nil {
				b.Cleanup(func() { _ = loaded.Closer.Close() })
			}
			runtime, err := newLiveCompetitiveAIRuntime(
				7, testCompetitiveAIMap(8), game.GameBeginData{GameID: 99, SpawnSeed: 11, ItemSeed: 22},
				participants, benchmark.freeForAll, loaded.Policy, 100,
			)
			if err != nil {
				b.Fatal(err)
			}
			battle, err := match.NewCompetitiveBattle(99, 2, 1, participants)
			if err != nil {
				b.Fatal(err)
			}
			server := &Server{
				competitiveAIRuntime: map[uint32]*liveCompetitiveAIRuntime{99: runtime},
				competitiveBattles:   map[uint32]*match.CompetitiveBattle{99: battle},
				logWriter:            io.Discard,
			}
			stepDurations := make([]time.Duration, 0, b.N)
			var maxStepDuration time.Duration
			maxStepIndex := 0
			b.ResetTimer()
			for index := 0; index < b.N; index++ {
				started := time.Now()
				target := runtime.runtime.EngineSnapshot().ElapsedMS() + competitiveAIWorldTickMS
				runtime.mu.Lock()
				err = runtime.advanceToLocked(server, target, 1)
				runtime.mu.Unlock()
				if err != nil {
					b.Fatal(err)
				}
				duration := time.Since(started)
				stepDurations = append(stepDurations, duration)
				if duration > maxStepDuration {
					maxStepDuration, maxStepIndex = duration, index
				}
			}
			b.StopTimer()
			sort.Slice(stepDurations, func(i, j int) bool { return stepDurations[i] < stepDurations[j] })
			if len(stepDurations) > 0 {
				b.ReportMetric(float64(stepDurations[(len(stepDurations)-1)*95/100].Nanoseconds())/1e6, "p95-ms/step")
				b.ReportMetric(float64(stepDurations[len(stepDurations)-1].Nanoseconds())/1e6, "max-ms/step")
				overBudget := len(stepDurations) - sort.Search(len(stepDurations), func(index int) bool {
					return stepDurations[index] > time.Duration(competitiveAIWorldTickMS)*time.Millisecond
				})
				b.ReportMetric(float64(overBudget), "over-20ms-steps")
				b.Logf("slowest world step: %d", maxStepIndex)
			}
		})
	}
}

func TestCompetitiveAIInboundFanoutDeduplicatesBeforeWorldLock(t *testing.T) {
	runtime := &liveCompetitiveAIRuntime{}
	packages := []game.GameplayDataPackage{{
		PlayerID: 1, Time: 4321, GameID: 99,
		Messages: []game.BattleMessageData{{
			Time: 4321, DataID: uint32(game.PlayerMoveSchema), Sequence: 17, GameTime: 4321, Data: []byte{1, 2, 3},
		}},
	}}
	const copies = 32
	start := make(chan struct{})
	results := make(chan bool, copies)
	for index := 0; index < copies; index++ {
		go func() {
			<-start
			results <- len(runtime.filterNativeInbound(packages)) != 0
		}()
	}
	close(start)
	unique := 0
	for index := 0; index < copies; index++ {
		if <-results {
			unique++
		}
	}
	if unique != 1 || runtime.nativeInboundCount() != 1 {
		t.Fatalf("concurrent fanout consumed %d calls/%d facts, want 1/1", unique, runtime.nativeInboundCount())
	}
}

func TestCompetitiveAIProjectsNativeDeathDropsAndMapElementDirection(t *testing.T) {
	events := []battleengine.Event{
		{Kind: battleengine.EventPickupDropped, TimeMS: 900, PlayerID: 20001, SceneID: 21, Cell: battleengine.Cell{Row: 2, Col: 3}},
		{Kind: battleengine.EventPickupDropped, TimeMS: 900, PlayerID: 20001, SceneID: 25, Cell: battleengine.Cell{Row: 4, Col: 5}},
		{Kind: battleengine.EventPickupDropped, TimeMS: 901, PlayerID: 20001, SceneID: 23, Cell: battleengine.Cell{Row: 6, Col: 7}},
	}
	items := droppedGameItems(events, 20001, 900)
	want := []game.GameItem{{ItemID: 21, Row: 2, Col: 3}, {ItemID: 25, Row: 4, Col: 5}}
	if len(items) != len(want) || items[0] != want[0] || items[1] != want[1] {
		t.Fatalf("projected death items = %+v, want %+v", items, want)
	}

	directions := []struct {
		to   battleengine.Cell
		want byte
	}{{battleengine.Cell{Row: 5, Col: 7}, 0}, {battleengine.Cell{Row: 4, Col: 6}, 1}, {battleengine.Cell{Row: 5, Col: 5}, 2}, {battleengine.Cell{Row: 6, Col: 6}, 3}}
	from := battleengine.Cell{Row: 5, Col: 6}
	for _, test := range directions {
		got, ok := nativeMapElementDirection(from, test.to)
		if !ok || got != test.want {
			t.Fatalf("direction %v -> %v = %d/%v, want %d/true", from, test.to, got, ok, test.want)
		}
	}
}

func TestCompetitiveAIWireTimeMatchesNativeTimeline(t *testing.T) {
	if got := competitiveAIWireTime(3100); got != 3100 {
		t.Fatalf("wire time = %d, want 3100", got)
	}
	if got := competitiveAIWireTime(^uint32(0)); got != ^uint32(0) {
		t.Fatalf("maximum wire time = %d, want identity", got)
	}
	if got := competitiveAIEngineTime(3100); got != 3100 {
		t.Fatalf("engine time = %d, want 3100", got)
	}
	if got := competitiveAIEngineTime(2500); got != battleengine.NativeRoundStartClockMS {
		t.Fatalf("pre-round engine time = %d, want %d", got, battleengine.NativeRoundStartClockMS)
	}
}

func TestCompetitiveAINativeMovementIntentUsesHeldInputNotPositionDelta(t *testing.T) {
	actor := battleengine.Actor{State: battleengine.ActorActive, Facing: battleengine.DirectionRight}
	moving, direction := competitiveAINativeMovementIntent(
		battleengine.Action{PlayerID: 20001, Move: battleengine.DirectionRight}, actor,
	)
	if !moving || direction != battleengine.DirectionRight {
		t.Fatalf("blocked held input = %v/%d, want moving/right", moving, direction)
	}
	moving, _ = competitiveAINativeMovementIntent(battleengine.Action{PlayerID: 20001}, actor)
	if moving {
		t.Fatal("released input remained moving")
	}
	actor.State = battleengine.ActorTrapped
	moving, _ = competitiveAINativeMovementIntent(
		battleengine.Action{PlayerID: 20001, Move: battleengine.DirectionRight}, actor,
	)
	if moving {
		t.Fatal("trapped actor retained movement input")
	}
}

func TestCompetitiveAIMovementHeartbeatKeepsHeldSequence(t *testing.T) {
	participants := []match.CompetitiveParticipant{
		{PlayerID: 1, RoleID: 1, TeamID: 1, Source: match.CompetitiveParticipantHuman},
		{PlayerID: 20001, RoleID: 2, TeamID: 2, Source: match.CompetitiveParticipantVirtualAI},
	}
	runtime, err := newLiveCompetitiveAIRuntime(
		7, testCompetitiveAIMap(2), game.GameBeginData{GameID: 102, SpawnSeed: 11, ItemSeed: 22}, participants, false,
		battleengine.PolicyFunc(func(observation battleengine.Observation, legal []battleengine.Action) (battleengine.Action, error) {
			return battleengine.Action{PlayerID: observation.PlayerID, Move: battleengine.DirectionLeft}, nil
		}), 100,
	)
	if err != nil {
		t.Fatal(err)
	}
	engine := runtime.runtime.EngineSnapshot()
	projection := liveCompetitiveAIMovementProjection{}
	var first game.PlayerMoveSequence
	for step := 0; step < 9; step++ {
		action := battleengine.Action{PlayerID: 20001, Move: battleengine.DirectionLeft}
		_, stepErr := engine.Step([]battleengine.Action{action})
		if stepErr != nil {
			t.Fatal(stepErr)
		}
		var move game.PlayerMoveSequence
		var emitted bool
		projection, move, emitted, stepErr = competitiveAIMovementSample(engine, 20001, action, projection, false)
		if stepErr != nil {
			t.Fatal(stepErr)
		}
		switch step {
		case 0:
			if !emitted || move.Sequence != 1 || move.WalkAndDirection&0x10 == 0 {
				t.Fatalf("initial held segment = emitted %v move %+v", emitted, move)
			}
			speed, validSpeed := battleengine.NativeSpeedPixelsPerSecond(move.Speed)
			toEndpoint := int(move.EndPosX) - int(move.CurrentPosX)
			if toEndpoint < 0 {
				toEndpoint = -toEndpoint
			}
			oneHeartbeat := int(speed) * int(competitiveAIMovementHeartbeatMS) / 1000
			if !validSpeed || toEndpoint <= oneHeartbeat {
				t.Fatalf("initial endpoint distance = %d at speed %d, want beyond one %dms heartbeat (%d)", toEndpoint, speed, competitiveAIMovementHeartbeatMS, oneHeartbeat)
			}
			first = move
		case 1, 2, 3, 4, 5, 6, 7:
			if emitted {
				t.Fatalf("held segment emitted before heartbeat at step %d: %+v", step, move)
			}
		case 8:
			delta := move.TimeStamp - first.TimeStamp
			if !emitted || move.Sequence != first.Sequence || delta < competitiveAIMovementHeartbeatMS || delta >= competitiveAIMovementHeartbeatMS+competitiveAIWorldTickMS {
				t.Fatalf("heartbeat = emitted %v move %+v first %+v", emitted, move, first)
			}
			if move.EndPosX != first.EndPosX || move.EndPosY != first.EndPosY {
				t.Fatalf("held corridor endpoint changed from (%d,%d) to (%d,%d)", first.EndPosX, first.EndPosY, move.EndPosX, move.EndPosY)
			}
		}
	}
}

func TestCompetitiveAIMovementPathChangeFlushesWithoutChangingHeldSequence(t *testing.T) {
	runtime, err := newLiveCompetitiveAIRuntime(
		7, testCompetitiveAIMap(2), game.GameBeginData{GameID: 102, SpawnSeed: 11, ItemSeed: 22},
		[]match.CompetitiveParticipant{
			{PlayerID: 1, RoleID: 1, TeamID: 1, Source: match.CompetitiveParticipantHuman},
			{PlayerID: 20001, RoleID: 2, TeamID: 2, Source: match.CompetitiveParticipantVirtualAI},
		}, false,
		battleengine.PolicyFunc(func(o battleengine.Observation, _ []battleengine.Action) (battleengine.Action, error) {
			return battleengine.Action{PlayerID: o.PlayerID}, nil
		}), 100,
	)
	if err != nil {
		t.Fatal(err)
	}
	engine := runtime.runtime.EngineSnapshot()
	action := battleengine.Action{PlayerID: 20001, Move: battleengine.DirectionLeft}
	if _, err = engine.Step([]battleengine.Action{action}); err != nil {
		t.Fatal(err)
	}
	projection, first, emitted, err := competitiveAIMovementSample(engine, 20001, action, liveCompetitiveAIMovementProjection{}, false)
	if err != nil || !emitted {
		t.Fatalf("initial sample: %+v/%v/%v", first, emitted, err)
	}
	if _, err = engine.ApplyVerifiedBombPlacementAt(1, battleengine.Cell{Row: 1, Col: 2}, 1, 0, engine.ElapsedMS()); err != nil {
		t.Fatal(err)
	}
	if _, err = engine.Step([]battleengine.Action{action}); err != nil {
		t.Fatal(err)
	}
	_, changed, emitted, err := competitiveAIMovementSample(engine, 20001, action, projection, false)
	if err != nil || !emitted || changed.Sequence != first.Sequence || changed.WalkAndDirection&0x20 != 0 {
		t.Fatalf("changed path: %+v/%v/%v, want ordinary same-sequence flush", changed, emitted, err)
	}
	if changed.TimeStamp-first.TimeStamp != competitiveAIWorldTickMS || changed.EndPosX == first.EndPosX {
		t.Fatalf("collision endpoint was not refreshed on the next world frame: first=%+v changed=%+v", first, changed)
	}

	// The native producer retains a previously advertised corner once the
	// actor passes it, until the endpoint or held segment changes.
	projection, _, _, err = competitiveAIMovementSample(engine, 20001, action, projection, false)
	if err != nil {
		t.Fatal(err)
	}
	projection.path.Corner = battleengine.Position{X: 139, Y: 60}
	projection.path.HasCorner = true
	_, continued, emitted, err := competitiveAIMovementSample(engine, 20001, action, projection, true)
	if err != nil || !emitted || continued.CornerPosX != 139 || continued.CornerPosY != 60 || continued.Sequence != first.Sequence {
		t.Fatalf("passed corner was discarded: %+v/%v/%v", continued, emitted, err)
	}
}

func TestCompetitiveAIMovementInputBoundaryStartsNewSequence(t *testing.T) {
	participants := []match.CompetitiveParticipant{
		{PlayerID: 1, RoleID: 1, TeamID: 1, Source: match.CompetitiveParticipantHuman},
		{PlayerID: 20001, RoleID: 2, TeamID: 2, Source: match.CompetitiveParticipantVirtualAI},
	}
	runtime, err := newLiveCompetitiveAIRuntime(
		7, testCompetitiveAIMap(2), game.GameBeginData{GameID: 103, SpawnSeed: 11, ItemSeed: 22}, participants, false,
		battleengine.PolicyFunc(func(observation battleengine.Observation, legal []battleengine.Action) (battleengine.Action, error) {
			return battleengine.Action{PlayerID: observation.PlayerID}, nil
		}), 100,
	)
	if err != nil {
		t.Fatal(err)
	}
	engine := runtime.runtime.EngineSnapshot()
	projection := liveCompetitiveAIMovementProjection{}
	right := battleengine.Action{PlayerID: 20001, Move: battleengine.DirectionRight}
	if _, err = engine.Step([]battleengine.Action{right}); err != nil {
		t.Fatal(err)
	}
	projection, first, emitted, err := competitiveAIMovementSample(engine, 20001, right, projection, false)
	if err != nil || !emitted {
		t.Fatalf("initial movement = %+v/%v/%v", first, emitted, err)
	}
	left := battleengine.Action{PlayerID: 20001, Move: battleengine.DirectionLeft}
	if _, err = engine.Step([]battleengine.Action{left}); err != nil {
		t.Fatal(err)
	}
	_, second, emitted, err := competitiveAIMovementSample(engine, 20001, left, projection, false)
	if err != nil || !emitted || second.Sequence != first.Sequence+1 || second.WalkAndDirection&0x0f != nativeWalkDirection(battleengine.DirectionLeft) {
		t.Fatalf("direction boundary = %+v/%v/%v, first %+v", second, emitted, err, first)
	}
}

func TestCompetitiveAIMovementForcedFlushPreservesHeldSequence(t *testing.T) {
	participants := []match.CompetitiveParticipant{
		{PlayerID: 1, RoleID: 1, TeamID: 1, Source: match.CompetitiveParticipantHuman},
		{PlayerID: 20001, RoleID: 2, TeamID: 2, Source: match.CompetitiveParticipantVirtualAI},
	}
	runtime, err := newLiveCompetitiveAIRuntime(
		7, testCompetitiveAIMap(2), game.GameBeginData{GameID: 104, SpawnSeed: 11, ItemSeed: 22}, participants, false,
		battleengine.PolicyFunc(func(observation battleengine.Observation, legal []battleengine.Action) (battleengine.Action, error) {
			return battleengine.Action{PlayerID: observation.PlayerID}, nil
		}), 100,
	)
	if err != nil {
		t.Fatal(err)
	}
	engine := runtime.runtime.EngineSnapshot()
	right := battleengine.Action{PlayerID: 20001, Move: battleengine.DirectionRight}
	if _, err = engine.Step([]battleengine.Action{right}); err != nil {
		t.Fatal(err)
	}
	projection, first, emitted, err := competitiveAIMovementSample(engine, 20001, right, liveCompetitiveAIMovementProjection{}, false)
	if err != nil || !emitted {
		t.Fatalf("initial movement = %+v/%v/%v", first, emitted, err)
	}
	_, forced, emitted, err := competitiveAIMovementSample(engine, 20001, right, projection, true)
	if err != nil || !emitted || forced.WalkAndDirection&0x20 == 0 {
		t.Fatalf("forced movement flush = %+v/%v/%v, want bit 5", forced, emitted, err)
	}
	if forced.Sequence != first.Sequence || forced.WalkAndDirection&0x10 == 0 {
		t.Fatalf("forced held segment = %+v, first %+v", forced, first)
	}
}

func TestCompetitiveAIMovementPublishesForcedSlideRate(t *testing.T) {
	participants := []match.CompetitiveParticipant{
		{PlayerID: 1, RoleID: 1, TeamID: 1, Source: match.CompetitiveParticipantHuman},
		{PlayerID: 20001, RoleID: 2, TeamID: 2, Source: match.CompetitiveParticipantVirtualAI},
	}
	runtime, err := newLiveCompetitiveAIRuntime(
		7, testCompetitiveAIMap(2), game.GameBeginData{GameID: 105, SpawnSeed: 11, ItemSeed: 22}, participants, false,
		battleengine.PolicyFunc(func(observation battleengine.Observation, legal []battleengine.Action) (battleengine.Action, error) {
			return battleengine.Action{PlayerID: observation.PlayerID}, nil
		}), 100,
	)
	if err != nil {
		t.Fatal(err)
	}
	engine := runtime.runtime.EngineSnapshot()
	right := battleengine.Action{PlayerID: 20001, Move: battleengine.DirectionRight}
	if _, err = engine.Step([]battleengine.Action{right}); err != nil {
		t.Fatal(err)
	}
	projection, base, emitted, err := competitiveAIMovementSample(engine, 20001, right, liveCompetitiveAIMovementProjection{}, false)
	if err != nil || !emitted {
		t.Fatalf("base movement = %+v/%v/%v", base, emitted, err)
	}
	actor, present := actorByID(engine.Actors(), 20001)
	if !present {
		t.Fatal("virtual actor is absent")
	}
	if err = engine.ApplyVerifiedPickupDispatch(engine.ElapsedMS(), []battleengine.Pickup{{SceneID: 42, Cell: actor.Position.Cell(), State: battleengine.PickupAvailable}}); err != nil {
		t.Fatal(err)
	}
	for len(engine.FieldObjects()) == 0 {
		if _, err = engine.Step(nil); err != nil {
			t.Fatal(err)
		}
	}
	actor, _ = actorByID(engine.Actors(), 20001)
	if _, err = engine.ApplyVerifiedFieldObjectContact(20001, 42, actor.Position); err != nil {
		t.Fatal(err)
	}
	_, slide, emitted, err := competitiveAIMovementSample(engine, 20001, right, projection, false)
	if err != nil || !emitted {
		t.Fatalf("forced-slide movement = %+v/%v/%v", slide, emitted, err)
	}
	if slide.Speed != 10 || slide.Sequence != base.Sequence+1 || slide.WalkAndDirection&0x10 == 0 {
		t.Fatalf("forced-slide movement = %+v, base %+v; want rate 10 on a new held segment", slide, base)
	}
}

func TestCompetitiveAIHitMovementUsesEventCoordinateAndStopsFacingDown(t *testing.T) {
	grid := battleengine.Grid{Width: 10, Height: 8, Cells: make([]battleengine.Tile, 80)}
	event := battleengine.Event{
		Kind: battleengine.EventActorHitRequested, TimeMS: 4321, PlayerID: 20001,
		Position: battleengine.Position{X: 123, Y: 77},
	}
	frame := liveCompetitiveAIActorFrame{position: event.Position, facing: battleengine.DirectionRight, speed: 4}
	held := liveCompetitiveAIMovementProjection{
		initialized: true, moving: true, direction: battleengine.DirectionRight,
		speed: 4, sequence: 9, lastSentAt: 4300,
	}
	stopped, move := competitiveAIHitMovementSample(event, frame, held, grid)
	if stopped.moving || stopped.sequence != 10 || stopped.lastSentAt != event.TimeMS || stopped.lastSentPosition != event.Position {
		t.Fatalf("hit projection = %+v, want event-time stopped segment", stopped)
	}
	if move.TimeStamp != event.TimeMS || move.CurrentPosX != 123 || move.CurrentPosY != 77 || move.EndPosX != 123 || move.EndPosY != 77 || move.WalkAndDirection != 0x23 {
		t.Fatalf("hit movement = %+v, want stopped downward bit-5 event checkpoint", move)
	}
}

func TestCompetitiveAIProjectileWireIncludesEmptyRayEndpoint(t *testing.T) {
	runtime := &liveCompetitiveAIRuntime{bombs: map[uint32]liveCompetitiveAIBomb{
		9: {ownerID: 20002, placedAt: 3500, power: 4, position: battleengine.Cell{Row: 6, Col: 10}},
	}}
	for _, actionID := range []uint8{44, 46} {
		for _, bombID := range []uint32{0, 9} {
			event := battleengine.Event{PlayerID: 20001, TimeMS: 4000, ActionID: actionID,
				Position: battleengine.Position{X: 300, Y: 260}, BombID: bombID,
				ProjectileDirection: battleengine.DirectionRight, ProjectileTargetCell: battleengine.Cell{Row: 6, Col: 10}}
			use := runtime.battleActionSample(event)
			body, err := use.MarshalNetworkBinary()
			if err != nil {
				t.Fatal(err)
			}
			wire, err := game.ParseWorldUseItemEvent(game.GameEvent{Schema: game.NotifyPlayerUseItem, Body: body})
			if err != nil {
				t.Fatal(err)
			}
			if wire.Flag1&0xffff != 0x060a || wire.PosX != 300 || wire.PosY != 260 {
				t.Fatalf("projectile endpoint corrupted: %+v", wire)
			}
			if bombID == 0 && (wire.Flag2 != ^uint32(0) || wire.Flag3 != 0 || wire.Flag4 != 0) {
				t.Fatalf("miss invented a target bomb: %+v", wire)
			}
			if bombID != 0 && (wire.Flag1>>16&0xff != 4 || wire.Flag2 != 20002 || wire.Flag3 != 3500 || wire.Flag4 != 1) {
				t.Fatalf("hit lost target identity: %+v", wire)
			}
		}
	}
}

func TestCompetitiveAITargetElapsedTracksNativeWallClock(t *testing.T) {
	started := time.Unix(100, 0)
	if got := competitiveAITargetElapsedMS(started, started.Add(2*time.Second), 100); got != battleengine.NativeRoundStartClockMS {
		t.Fatalf("pre-control target = %d, want %d", got, battleengine.NativeRoundStartClockMS)
	}
	if got := competitiveAITargetElapsedMS(started, started.Add(54_484*time.Millisecond), 100); got != 54_400 {
		t.Fatalf("live target = %d, want completed 100 ms boundary 54400", got)
	}
	if got := competitiveAITargetElapsedMS(started, started.Add(3_101*time.Millisecond), 100); got != 3_100 {
		t.Fatalf("near-boundary target = %d, want 3100 rather than future tick 3200", got)
	}
	if got := competitiveAITargetElapsedMS(started, started.Add(3_099*time.Millisecond), 100); got != 3_000 {
		t.Fatalf("incomplete first tick target = %d, want 3000", got)
	}
}

func TestCompetitiveAITargetElapsedFollowsAuthoritySceneClock(t *testing.T) {
	runtime, err := newLiveCompetitiveAIRuntime(
		7, testCompetitiveAIMap(2), game.GameBeginData{GameID: 105, SpawnSeed: 11, ItemSeed: 22},
		[]match.CompetitiveParticipant{
			{PlayerID: 1, RoleID: 1, TeamID: 1, Source: match.CompetitiveParticipantHuman},
			{PlayerID: 20001, RoleID: 2, TeamID: 2, Source: match.CompetitiveParticipantVirtualAI},
		}, false,
		battleengine.PolicyFunc(func(observation battleengine.Observation, legal []battleengine.Action) (battleengine.Action, error) {
			return battleengine.Action{PlayerID: observation.PlayerID}, nil
		}), 100,
	)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Unix(100, 0)
	observed := started.Add(12 * time.Second)
	runtime.nativeGameStartedAt = started
	runtime.observeAuthorityClockLocked(10_000, observed)
	if got := runtime.targetElapsedMSLocked(observed); got != 10_000 {
		t.Fatalf("anchored target = %d, want authority 10000 instead of wall 12000", got)
	}
	runtime.observeAuthorityClockLocked(10_000, observed.Add(400*time.Millisecond))
	if got := runtime.targetElapsedMSLocked(observed.Add(500 * time.Millisecond)); got != 10_500 {
		t.Fatalf("duplicate clock reset anchor: target = %d, want 10500", got)
	}
	runtime.observeAuthorityClockLocked(10_240, observed.Add(500*time.Millisecond))
	if got := runtime.targetElapsedMSLocked(observed.Add(500 * time.Millisecond)); got != 10_240 {
		t.Fatalf("new authority sample target = %d, want 10240", got)
	}
}

func TestCompetitiveAIAuthorityNotificationCatchesUpBeforeConsumption(t *testing.T) {
	participants := []match.CompetitiveParticipant{
		{PlayerID: 1, RoleID: 1, TeamID: 1, Source: match.CompetitiveParticipantHuman},
		{PlayerID: 20001, RoleID: 2, TeamID: 2, Source: match.CompetitiveParticipantVirtualAI},
	}
	runtime, err := newLiveCompetitiveAIRuntime(
		7, testCompetitiveAIMap(2), game.GameBeginData{GameID: 106, SpawnSeed: 11, ItemSeed: 22}, participants, false,
		battleengine.PolicyFunc(func(observation battleengine.Observation, legal []battleengine.Action) (battleengine.Action, error) {
			return battleengine.Action{PlayerID: observation.PlayerID}, nil
		}), 100,
	)
	if err != nil {
		t.Fatal(err)
	}
	battle, err := match.NewCompetitiveBattle(106, 2, 1, participants)
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{
		competitiveAIRuntime: map[uint32]*liveCompetitiveAIRuntime{106: runtime},
		competitiveBattles:   map[uint32]*match.CompetitiveBattle{106: battle},
		logWriter:            io.Discard,
	}
	server.recordCompetitiveAIHumanPackages(106, []game.GameplayDataPackage{{
		PlayerID: 1, Time: 3617, GameID: 106,
		Messages: []game.BattleMessageData{{
			Time: 3617, DataID: uint32(game.NotifyDispatchItem), Sequence: 1, Data: []byte{0},
		}},
	}})
	if elapsed := runtime.runtime.EngineSnapshot().ElapsedMS(); elapsed != 3600 {
		t.Fatalf("authority catch-up elapsed = %d, want completed event frame 3600", elapsed)
	}
	if _, ok := runtime.actorFrameAtLocked(20001, 3617); !ok {
		t.Fatal("authority event was consumed without its completed position frame")
	}
}

func TestCompetitiveAILiveRuntimeSeparatesPhysicsAndProjectionCadence(t *testing.T) {
	participants := []match.CompetitiveParticipant{
		{PlayerID: 1, RoleID: 1, TeamID: 1, Source: match.CompetitiveParticipantHuman},
		{PlayerID: 20001, RoleID: 2, TeamID: 2, Source: match.CompetitiveParticipantVirtualAI},
	}
	runtime, err := newLiveCompetitiveAIRuntime(
		7, testCompetitiveAIMap(2), game.GameBeginData{GameID: 97, SpawnSeed: 11, ItemSeed: 22}, participants, false,
		battleengine.PolicyFunc(func(observation battleengine.Observation, legal []battleengine.Action) (battleengine.Action, error) {
			return battleengine.Action{PlayerID: observation.PlayerID}, nil
		}), 100,
	)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.decisionTick != 100*time.Millisecond {
		t.Fatalf("decision cadence = %s, want 100ms", runtime.decisionTick)
	}
	if runtime.worldTick != 20*time.Millisecond {
		t.Fatalf("world/projection cadence = %s, want 20ms", runtime.worldTick)
	}
	before := runtime.runtime.EngineSnapshot().ElapsedMS()
	if _, err = runtime.runtime.Step(nil); err != nil {
		t.Fatal(err)
	}
	after := runtime.runtime.EngineSnapshot().ElapsedMS()
	if after-before != competitiveAIWorldTickMS {
		t.Fatalf("physics step = %dms, want %dms", after-before, competitiveAIWorldTickMS)
	}
}

func TestCompetitiveAIBombIntentUsesNativePeerGameplayChannel(t *testing.T) {
	body, err := (game.PlayerUseBombEvent{
		PlayerID: 20001, ClientTime: 3400, BombID: competitiveAIDefaultBombAppearance,
		Row: 6, Column: 3, Power: 3,
	}).MarshalNetworkBinary()
	if err != nil {
		t.Fatal(err)
	}
	payload, err := buildCompetitiveAIPeerGameplayPayload(9, 20001, 3400, 7, game.PlayerUseBomb, 0xffff0065, body)
	if err != nil {
		t.Fatal(err)
	}
	batch, err := game.DecodeQQTPPPGameplayBatch(payload)
	if err != nil {
		t.Fatal(err)
	}
	if batch.GameID != 9 || len(batch.Entries) != 1 {
		t.Fatalf("decoded batch = %+v", batch)
	}
	entry := batch.Entries[0]
	if entry.Index != 7 || entry.Package.PlayerID != 20001 || entry.Package.Time != 3400 || entry.Package.GameID != 9 || len(entry.Package.Messages) != 1 {
		t.Fatalf("decoded peer entry = %+v", entry)
	}
	message := entry.Package.Messages[0]
	if message.DataID != uint32(game.PlayerUseBomb) || message.Sequence != 7 || message.GameTime != 0xffff0065 {
		t.Fatalf("decoded bomb message = %+v", message)
	}
	// Request and peer-notify variants share the recovered object body.
	decoded, err := game.ParsePlayerUseBombEvent(game.GameEvent{Schema: game.PlayerUseBomb, Body: message.Data})
	if err != nil {
		t.Fatal(err)
	}
	if decoded.PlayerID != 20001 || decoded.Row != 6 || decoded.Column != 3 || decoded.BombID != competitiveAIDefaultBombAppearance {
		t.Fatalf("decoded bomb = %+v", decoded)
	}
}

func TestCompetitiveAIActorIntentsEncodeOnNativePeerChannel(t *testing.T) {
	tests := []struct {
		name     string
		schema   uint16
		gameTime uint32
		body     []byte
	}{
		{
			name: "trapped", schema: game.PlayerBeExploded, gameTime: 6300,
			body: mustMarshalPlayerExploded(t, game.PlayerExplodedEvent{PlayerID: 20001, ClientTime: 6300, PosX: 100, PosY: 140}),
		},
		{
			name: "pickup", schema: game.RequestGetItem, gameTime: 6400,
			body: mustMarshalPlayerItem(t, game.PlayerItemEvent{PlayerID: 20001, ClientTime: 6400, ItemID: 21, PosX: 100, PosY: 140}),
		},
		{
			name: "rescue", schema: game.RequestSavePlayer, gameTime: 6500,
			body: mustMarshalPlayerInteraction(t, game.PlayerInteractionEvent{PlayerID: 20002, ClientTime: 6500, DestinationPlayerID: 20001, PosX: 100, PosY: 140}),
		},
		{
			name: "kill", schema: game.RequestKillPlayer, gameTime: 6600,
			body: mustMarshalPlayerInteraction(t, game.PlayerInteractionEvent{PlayerID: 20001, ClientTime: 6600, DestinationPlayerID: 20002, PosX: 100, PosY: 140}),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			payload, err := buildCompetitiveAIPeerGameplayPayload(9, 20001, test.gameTime, 7, test.schema, test.gameTime, test.body)
			if err != nil {
				t.Fatal(err)
			}
			batch, err := game.DecodeQQTPPPGameplayBatch(payload)
			if err != nil {
				t.Fatal(err)
			}
			message := batch.Entries[0].Package.Messages[0]
			if message.DataID != uint32(test.schema) || message.GameTime != test.gameTime || string(message.Data) != string(test.body) {
				t.Fatalf("native peer intent = %+v", message)
			}
		})
	}
}

func TestCompetitiveAIEventRoutesSeparateRequestsFromFacts(t *testing.T) {
	requests := []uint16{
		game.RequestKillPlayer, game.RequestSavePlayer,
		game.RequestGetItem, game.RequestMoveMapElement, game.RequestMoveBomb,
	}
	for _, schema := range requests {
		if !competitiveAIArbitratorRequest(schema) {
			t.Fatalf("schema 0x%04X must route only to the arbitrator", schema)
		}
	}
	for _, schema := range []uint16{game.PlayerMoveSchema, game.PlayerUseBomb, game.PlayerBeExploded, game.NotifyPlayerDieEvent, game.RequestUseItem} {
		if competitiveAIArbitratorRequest(schema) {
			t.Fatalf("fact schema 0x%04X must broadcast to peers", schema)
		}
	}
	for _, schema := range []uint16{
		game.NotifyBombExplode, game.NotifyPlayerExploded, game.NotifyPlayerKilled,
		game.NotifyPlayerSaved, game.NotifyPlayerGetItem,
		game.NotifyDispatchItem, game.NotifyItemExploded, game.NotifyMapElementMoved,
		game.NotifyPlayerMoveBomb, game.NotifyRecoverAvatar,
	} {
		if !competitiveAIArbitratorNotification(schema) {
			t.Fatalf("schema 0x%04X must require the arbitrator as producer", schema)
		}
	}
	if competitiveAIArbitratorNotification(game.NotifyPlayerUseItem) {
		t.Fatal("server-owned NOTIFY_PLAYER_USE_ITEM must not trust a peer producer")
	}
}

func TestCompetitiveAIPlayerExplodedBroadcastsOriginalSceneFact(t *testing.T) {
	const (
		roomID          uint16 = 7
		gameID          uint32 = 91
		virtualPlayerID uint16 = 20001
		virtualUIN      uint32 = 1_020_001
	)
	server, actor, peer := newRoomPeerUDPTestServer(t)
	actor.CurrentGameID, peer.CurrentGameID = gameID, gameID
	serverSocket := listenRoomPeerUDPTest(t)
	actorMode1 := listenRoomPeerUDPTest(t)
	peerMode1 := listenRoomPeerUDPTest(t)
	defer serverSocket.Close()
	defer actorMode1.Close()
	defer peerMode1.Close()

	for _, setup := range []struct {
		session *connectionSession
		socket  *net.UDPConn
	}{{actor, actorMode1}, {peer, peerMode1}} {
		presence := game.LegacyUDPControlPacket{
			Header:   game.LegacyUDPControlHeader{PlayerID: setup.session.Profile.PlayerID, UIN: setup.session.UIN, Type: game.LegacyUDPPresenceType},
			Presence: &game.LegacyUDPEndpoint{IPv4: [4]byte{127, 0, 0, 1}, Port: uint16(setup.socket.LocalAddr().(*net.UDPAddr).Port)},
		}
		data, encodeErr := presence.Encode()
		if encodeErr != nil {
			t.Fatal(encodeErr)
		}
		server.handleLegacyUDPControl(serverSocket, "game-udp", "presence", serverSocket.LocalAddr().String(), setup.socket.LocalAddr().(*net.UDPAddr), data)
		expectLegacyUDPPresenceObserved(t, setup.socket, serverSocket, presence)
	}

	runtime := &liveCompetitiveAIRuntime{
		roomID: roomID, gameID: gameID,
		roomProjections: []competitiveAIRoomProjection{{PlayerID: virtualPlayerID, UIN: virtualUIN}},
		messageSeq:      make(map[uint16]uint32),
	}
	want := game.PlayerExplodedEvent{PlayerID: virtualPlayerID, ClientTime: 6300, PosX: 120, PosY: 240}
	body := mustMarshalPlayerExploded(t, want)
	if err := runtime.sendPeerGameplayEvent(server, virtualPlayerID, want.ClientTime, game.PlayerBeExploded, want.ClientTime, body); err != nil {
		t.Fatal(err)
	}
	for name, socket := range map[string]*net.UDPConn{"actor": actorMode1, "peer": peerMode1} {
		if err := socket.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		data := make([]byte, game.LegacyUDPMaxDatagramSize)
		n, _, readErr := socket.ReadFromUDP(data)
		if readErr != nil {
			t.Fatalf("%s did not receive scene fact: %v", name, readErr)
		}
		routed, decodeErr := game.DecodeLegacyUDPControlPacket(data[:n])
		if decodeErr != nil || routed.Header.Type != game.LegacyUDPMulticastType || routed.Header.PlayerID != virtualPlayerID || routed.Header.UIN != virtualUIN || routed.Multicast == nil {
			t.Fatalf("%s routed packet = %+v, %v", name, routed, decodeErr)
		}
		batch, decodeErr := game.DecodeQQTPPPGameplayBatch(routed.Multicast.Data)
		if decodeErr != nil || len(batch.Entries) != 1 || len(batch.Entries[0].Package.Messages) != 1 {
			t.Fatalf("%s gameplay batch = %+v, %v", name, batch, decodeErr)
		}
		entry := batch.Entries[0]
		message := entry.Package.Messages[0]
		if entry.Package.PlayerID != virtualPlayerID || message.DataID != uint32(game.PlayerBeExploded) {
			t.Fatalf("%s scene fact = package %+v message %+v", name, entry.Package, message)
		}
		got, parseErr := game.ParsePlayerExplodedEvent(game.GameEvent{Schema: game.PlayerBeExploded, Body: message.Data})
		if parseErr != nil || got != want {
			t.Fatalf("%s player exploded = %+v, %v; want %+v", name, got, parseErr, want)
		}
	}
}

func mustMarshalPlayerExploded(t *testing.T, event game.PlayerExplodedEvent) []byte {
	t.Helper()
	body, err := event.MarshalNetworkBinary()
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func mustMarshalPlayerItem(t *testing.T, event game.PlayerItemEvent) []byte {
	t.Helper()
	body, err := event.MarshalNetworkBinary()
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func mustMarshalPlayerInteraction(t *testing.T, event game.PlayerInteractionEvent) []byte {
	t.Helper()
	body, err := event.MarshalNetworkBinary()
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestCompetitiveAIHeldDirectionPolicyFiltersTickAlternation(t *testing.T) {
	choices := []battleengine.Direction{battleengine.DirectionRight, battleengine.DirectionLeft, battleengine.DirectionRight, battleengine.DirectionDown, battleengine.DirectionDown}
	index := 0
	policy := &competitiveAIHeldDirectionPolicy{base: battleengine.PolicyFunc(func(observation battleengine.Observation, legal []battleengine.Action) (battleengine.Action, error) {
		choice := choices[index]
		index++
		return battleengine.Action{PlayerID: observation.PlayerID, Move: choice}, nil
	})}
	legal := []battleengine.Action{
		{PlayerID: 20001, Move: battleengine.DirectionRight},
		{PlayerID: 20001, Move: battleengine.DirectionLeft},
		{PlayerID: 20001, Move: battleengine.DirectionDown},
	}
	want := []battleengine.Direction{battleengine.DirectionRight, battleengine.DirectionRight, battleengine.DirectionRight, battleengine.DirectionRight, battleengine.DirectionDown}
	clocks := []uint32{3000, 3020, 3040, 3060, 3180}
	for step, direction := range want {
		got, err := policy.ChooseAction(battleengine.Observation{PlayerID: 20001, ClockMS: clocks[step]}, legal)
		if err != nil || got.Move != direction {
			t.Fatalf("stable direction step %d = %+v/%v, want %d", step, got, err, direction)
		}
	}
}

func TestCompetitiveAIHeldDirectionPolicyStopsAndRestartsImmediately(t *testing.T) {
	choices := []battleengine.Direction{
		battleengine.DirectionRight,
		battleengine.DirectionNone,
		battleengine.DirectionDown,
	}
	index := 0
	policy := &competitiveAIHeldDirectionPolicy{base: battleengine.PolicyFunc(func(observation battleengine.Observation, legal []battleengine.Action) (battleengine.Action, error) {
		choice := choices[index]
		index++
		return battleengine.Action{PlayerID: observation.PlayerID, Move: choice}, nil
	})}
	legal := []battleengine.Action{
		{PlayerID: 20001, Move: battleengine.DirectionNone},
		{PlayerID: 20001, Move: battleengine.DirectionRight},
		{PlayerID: 20001, Move: battleengine.DirectionDown},
	}
	for step, want := range choices {
		got, err := policy.ChooseAction(battleengine.Observation{PlayerID: 20001, ClockMS: 3000 + uint32(step)*20}, legal)
		if err != nil || got.Move != want {
			t.Fatalf("key transition step %d = %+v/%v, want %d", step, got, err, want)
		}
	}
}

func TestCompetitiveAILivePolicyPreservesModelDirections(t *testing.T) {
	choices := []battleengine.Direction{battleengine.DirectionRight, battleengine.DirectionLeft, battleengine.DirectionRight, battleengine.DirectionDown, battleengine.DirectionDown}
	index := 0
	policy := competitiveAILivePolicy(battleengine.PolicyFunc(func(observation battleengine.Observation, legal []battleengine.Action) (battleengine.Action, error) {
		choice := choices[index]
		index++
		return battleengine.Action{PlayerID: observation.PlayerID, Move: choice}, nil
	}), 1, 0)
	legal := []battleengine.Action{
		{PlayerID: 20001, Move: battleengine.DirectionRight},
		{PlayerID: 20001, Move: battleengine.DirectionLeft},
		{PlayerID: 20001, Move: battleengine.DirectionDown},
	}
	want := choices
	clocks := []uint32{3000, 3020, 3040, 3060, 3180}
	for step, direction := range want {
		got, err := policy.ChooseAction(battleengine.Observation{PlayerID: 20001, ClockMS: clocks[step]}, legal)
		if err != nil || got.Move != direction {
			t.Fatalf("live policy direction step %d = %+v/%v, want %d", step, got, err, direction)
		}
	}

	_, ok := policy.(*competitiveAIDecisionCadencePolicy)
	if !ok {
		t.Fatalf("live policy wrapper = %T, want decision-cadence policy", policy)
	}
}

func TestCompetitiveAIHeldDirectionPolicyPreservesBombPulseAndAcceptsBlockedTurn(t *testing.T) {
	policy := &competitiveAIHeldDirectionPolicy{base: battleengine.PolicyFunc(func(observation battleengine.Observation, legal []battleengine.Action) (battleengine.Action, error) {
		return battleengine.Action{PlayerID: observation.PlayerID, Move: battleengine.DirectionDown, PlaceBomb: true}, nil
	})}
	policy.initialized = true
	policy.held = battleengine.DirectionRight
	legal := []battleengine.Action{{PlayerID: 20001, Move: battleengine.DirectionDown, PlaceBomb: true}}
	got, err := policy.ChooseAction(battleengine.Observation{PlayerID: 20001}, legal)
	if err != nil || got.Move != battleengine.DirectionDown || !got.PlaceBomb {
		t.Fatalf("blocked held direction result = %+v/%v", got, err)
	}
}

func TestCompetitiveAIDecisionCadenceHoldsMovementWithoutRepeatingPulses(t *testing.T) {
	calls := 0
	policy := &competitiveAIDecisionCadencePolicy{
		steps: 2,
		base: battleengine.PolicyFunc(func(observation battleengine.Observation, legal []battleengine.Action) (battleengine.Action, error) {
			calls++
			return battleengine.Action{PlayerID: observation.PlayerID, Move: battleengine.DirectionRight, PlaceBomb: true, UseActionID: 41}, nil
		}),
	}
	legal := []battleengine.Action{
		{PlayerID: 20001, Move: battleengine.DirectionRight},
		{PlayerID: 20001, Move: battleengine.DirectionRight, PlaceBomb: true, UseActionID: 41},
	}
	first, err := policy.ChooseAction(battleengine.Observation{PlayerID: 20001}, legal)
	if err != nil || !first.PlaceBomb || first.UseActionID != 41 {
		t.Fatalf("first decision = %+v/%v", first, err)
	}
	second, err := policy.ChooseAction(battleengine.Observation{PlayerID: 20001}, legal)
	if err != nil || second.Move != battleengine.DirectionRight || second.PlaceBomb || second.UseActionID != 0 {
		t.Fatalf("held world tick = %+v/%v", second, err)
	}
	third, err := policy.ChooseAction(battleengine.Observation{PlayerID: 20001}, legal)
	if err != nil || !third.PlaceBomb || third.UseActionID != 41 || calls != 2 {
		t.Fatalf("second decision = %+v/%v calls=%d", third, err, calls)
	}
}

func TestCompetitiveAIDecisionCadenceRerunsModelOnEveryImminentDangerTick(t *testing.T) {
	participants := []match.CompetitiveParticipant{
		{PlayerID: 1, RoleID: 1, TeamID: 1, Source: match.CompetitiveParticipantHuman},
		{PlayerID: 20001, RoleID: 2, TeamID: 2, Source: match.CompetitiveParticipantVirtualAI},
	}
	live, err := newLiveCompetitiveAIRuntime(
		7, testCompetitiveAIMap(2), game.GameBeginData{GameID: 109, SpawnSeed: 11, ItemSeed: 22}, participants, false,
		battleengine.PolicyFunc(func(observation battleengine.Observation, _ []battleengine.Action) (battleengine.Action, error) {
			return battleengine.Action{PlayerID: observation.PlayerID}, nil
		}), 100,
	)
	if err != nil {
		t.Fatal(err)
	}
	engine := live.runtime.EngineSnapshot()
	calls := 0
	policy := &competitiveAIDecisionCadencePolicy{
		steps: 5,
		base: battleengine.PolicyFunc(func(observation battleengine.Observation, _ []battleengine.Action) (battleengine.Action, error) {
			calls++
			return battleengine.Action{
				PlayerID:  observation.PlayerID,
				PlaceBomb: calls == 1,
			}, nil
		}),
	}
	const ticks = 140
	sawImminentDanger := false
	for tick := 0; tick < ticks; tick++ {
		observation, observeErr := engine.Observation(20001)
		if observeErr != nil {
			t.Fatal(observeErr)
		}
		legal, legalErr := engine.LegalActions(20001)
		if legalErr != nil {
			t.Fatal(legalErr)
		}
		imminentDanger := competitiveAIImminentDanger(engine, observation)
		callsBefore := calls
		action, chooseErr := policy.ChooseActionWithSnapshot(engine, observation, legal)
		if chooseErr != nil {
			t.Fatal(chooseErr)
		}
		if imminentDanger {
			sawImminentDanger = true
			if calls != callsBefore+1 {
				t.Fatalf("danger tick %d model calls advanced %d, want 1", tick, calls-callsBefore)
			}
		}
		if _, stepErr := engine.Step([]battleengine.Action{action}); stepErr != nil {
			t.Fatal(stepErr)
		}
	}
	if !sawImminentDanger {
		t.Fatal("known bubble never entered the 400ms imminent-danger window")
	}
	if normalCadenceCalls := (ticks + int(policy.steps) - 1) / int(policy.steps); calls <= normalCadenceCalls {
		t.Fatalf("model calls inside known blast = %d, want more than normal 100ms cadence %d", calls, normalCadenceCalls)
	}
}

func TestCompetitiveAIDecisionCadenceUsesIndependentSeededPhase(t *testing.T) {
	const steps = uint32(5)
	phases := make(map[uint32]bool)
	for playerID := uint16(20001); playerID <= 20007; playerID++ {
		phases[competitiveAIDecisionPhase(91, 11, 22, playerID, steps)] = true
	}
	if len(phases) < 2 {
		t.Fatalf("all virtual actors received one decision phase: %+v", phases)
	}

	calls := 0
	policy := &competitiveAIDecisionCadencePolicy{
		steps: 5, initialWait: 2,
		base: battleengine.PolicyFunc(func(observation battleengine.Observation, _ []battleengine.Action) (battleengine.Action, error) {
			calls++
			return battleengine.Action{PlayerID: observation.PlayerID, Move: battleengine.DirectionRight}, nil
		}),
	}
	legal := []battleengine.Action{
		{PlayerID: 20001, Move: battleengine.DirectionNone},
		{PlayerID: 20001, Move: battleengine.DirectionRight},
	}
	for step, want := range []battleengine.Direction{battleengine.DirectionNone, battleengine.DirectionNone, battleengine.DirectionRight} {
		got, err := policy.ChooseAction(battleengine.Observation{PlayerID: 20001}, legal)
		if err != nil || got.Move != want {
			t.Fatalf("phase step %d = %+v/%v, want %d", step, got, err, want)
		}
	}
	if calls != 1 {
		t.Fatalf("seeded phase performed %d model calls, want 1", calls)
	}
}

func TestCompetitiveAIHumanDepartureRemovesPolicyTarget(t *testing.T) {
	participants := []match.CompetitiveParticipant{
		{PlayerID: 1, RoleID: 1, TeamID: 1, Source: match.CompetitiveParticipantHuman},
		{PlayerID: 20001, RoleID: 2, TeamID: 2, Source: match.CompetitiveParticipantVirtualAI},
	}
	runtime, err := newLiveCompetitiveAIRuntime(
		7, testCompetitiveAIMap(2), game.GameBeginData{GameID: 91, SpawnSeed: 11, ItemSeed: 22}, participants, false,
		battleengine.PolicyFunc(func(observation battleengine.Observation, legal []battleengine.Action) (battleengine.Action, error) {
			return battleengine.Action{PlayerID: observation.PlayerID}, nil
		}), 100,
	)
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{competitiveAIRuntime: map[uint32]*liveCompetitiveAIRuntime{91: runtime}}
	server.recordCompetitiveAIHumanDeparture(91, 1)

	runtime.mu.Lock()
	_, stillHuman := runtime.humanIDs[1]
	snapshot := runtime.runtime.EngineSnapshot()
	runtime.mu.Unlock()
	if stillHuman {
		t.Fatal("departed human remained a live runtime source")
	}
	for _, actor := range snapshot.Actors() {
		if actor.PlayerID == 1 && actor.State != battleengine.ActorEliminated {
			t.Fatalf("departed actor state = %d, want eliminated", actor.State)
		}
	}
}

func TestCompetitiveAIReliableKillEliminatesVirtualActorExactlyOnce(t *testing.T) {
	participants := []match.CompetitiveParticipant{
		{PlayerID: 1, RoleID: 1, TeamID: 1, Source: match.CompetitiveParticipantHuman},
		{PlayerID: 2, RoleID: 2, TeamID: 1, Source: match.CompetitiveParticipantHuman},
		{PlayerID: 20001, RoleID: 3, TeamID: 2, Source: match.CompetitiveParticipantVirtualAI},
		{PlayerID: 20002, RoleID: 4, TeamID: 2, Source: match.CompetitiveParticipantVirtualAI},
	}
	runtime, err := newLiveCompetitiveAIRuntime(
		7, testCompetitiveAIMap(4), game.GameBeginData{GameID: 97, SpawnSeed: 11, ItemSeed: 22}, participants, false,
		battleengine.PolicyFunc(func(observation battleengine.Observation, legal []battleengine.Action) (battleengine.Action, error) {
			return battleengine.Action{PlayerID: observation.PlayerID}, nil
		}), 100,
	)
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{competitiveAIRuntime: map[uint32]*liveCompetitiveAIRuntime{97: runtime}}
	server.recordCompetitiveAINativeElimination(97, 1, 20001, nil)
	server.recordCompetitiveAINativeElimination(97, 1, 20001, nil)

	eliminated, ok := actorByID(runtime.runtime.EngineSnapshot().Actors(), 20001)
	if !ok || eliminated.State != battleengine.ActorEliminated {
		t.Fatalf("reliable kill target = %+v/%v, want eliminated", eliminated, ok)
	}
	active, ok := actorByID(runtime.runtime.EngineSnapshot().Actors(), 20002)
	if !ok || active.State != battleengine.ActorActive {
		t.Fatalf("unrelated virtual actor = %+v/%v, want active", active, ok)
	}
}

func TestCompetitiveAIArbitratorDeathIsLifecycleBarrier(t *testing.T) {
	participants := []match.CompetitiveParticipant{
		{PlayerID: 1, RoleID: 1, TeamID: 1, Source: match.CompetitiveParticipantHuman},
		{PlayerID: 2, RoleID: 2, TeamID: 1, Source: match.CompetitiveParticipantHuman},
		{PlayerID: 20001, RoleID: 3, TeamID: 2, Source: match.CompetitiveParticipantVirtualAI},
		{PlayerID: 20002, RoleID: 4, TeamID: 2, Source: match.CompetitiveParticipantVirtualAI},
	}
	runtime, err := newLiveCompetitiveAIRuntime(
		7, testCompetitiveAIMap(4), game.GameBeginData{GameID: 99, SpawnSeed: 11, ItemSeed: 22}, participants, false,
		battleengine.PolicyFunc(func(observation battleengine.Observation, legal []battleengine.Action) (battleengine.Action, error) {
			return battleengine.Action{PlayerID: observation.PlayerID}, nil
		}), 100,
	)
	if err != nil {
		t.Fatal(err)
	}
	virtual, ok := actorByID(runtime.runtime.EngineSnapshot().Actors(), 20001)
	if !ok {
		t.Fatal("virtual actor is absent")
	}
	runtime.movement[20001] = liveCompetitiveAIMovementProjection{initialized: true, moving: true}
	runtime.hitRequests[competitiveAIHitKey(game.PlayerExplodedEvent{
		PlayerID: 20001, ClientTime: 3000, PosX: uint16(virtual.Position.X), PosY: uint16(virtual.Position.Y),
	})] = liveCompetitiveAIHitRequest{sourceID: 1, state: competitiveAIHitPending}
	runtime.pickupRequests[liveCompetitiveAIPickupRequestKey{playerID: 20001, sceneID: 25}] = liveCompetitiveAIPickupRequest{timeMS: 3000, position: virtual.Position}
	runtime.sceneRequests[liveCompetitiveAISceneRequestKey{kind: competitiveAIRequestAction, playerID: 20001, actionID: 41}] = 3000

	battle, err := match.NewCompetitiveBattle(99, 2, 1, participants)
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{
		competitiveAIRuntime: map[uint32]*liveCompetitiveAIRuntime{99: runtime},
		competitiveBattles:   map[uint32]*match.CompetitiveBattle{99: battle},
		logWriter:            io.Discard,
	}
	body, err := (game.PlayerDeathEvent{
		PlayerID: 20001, ClientTime: 4000,
		PosX: uint16(virtual.Position.X), PosY: uint16(virtual.Position.Y),
	}).MarshalNetworkBinary()
	if err != nil {
		t.Fatal(err)
	}
	// The original arbitrator authors the package while the body names the
	// virtual participant. This is the form seen in real Type-2 captures.
	packages := []game.GameplayDataPackage{{
		PlayerID: 1, GameID: 99,
		Messages: []game.BattleMessageData{{DataID: game.NotifyPlayerDieEvent, Data: body}},
	}}
	server.recordCompetitiveAIHumanPackages(99, packages)
	server.recordCompetitiveAIHumanPackages(99, packages)
	if runtime.nativeInboundCount() != 1 {
		t.Fatalf("UDP/reliable duplicate identities = %d, want one consumed native fact", runtime.nativeInboundCount())
	}

	eliminated, ok := actorByID(runtime.runtime.EngineSnapshot().Actors(), 20001)
	if !ok || eliminated.State != battleengine.ActorEliminated {
		t.Fatalf("arbitrator death target = %+v/%v, want eliminated", eliminated, ok)
	}
	if _, ok = runtime.movement[20001]; ok {
		t.Fatal("movement projection survived death")
	}
	if len(runtime.hitRequests) != 0 || len(runtime.pickupRequests) != 0 || len(runtime.sceneRequests) != 0 {
		t.Fatalf("pending requests survived death: hit=%d pickup=%d scene=%d", len(runtime.hitRequests), len(runtime.pickupRequests), len(runtime.sceneRequests))
	}
	active, ok := actorByID(runtime.runtime.EngineSnapshot().Actors(), 20002)
	if !ok || active.State != battleengine.ActorActive {
		t.Fatalf("unrelated virtual actor = %+v/%v, want active", active, ok)
	}
}

func TestCompetitiveAIRejectedPickupStaysBlockedUntilSceneBoundary(t *testing.T) {
	participants := []match.CompetitiveParticipant{
		{PlayerID: 1, RoleID: 1, TeamID: 1, Source: match.CompetitiveParticipantHuman},
		{PlayerID: 20001, RoleID: 2, TeamID: 2, Source: match.CompetitiveParticipantVirtualAI},
	}
	runtime, err := newLiveCompetitiveAIRuntime(
		7, testCompetitiveAIMap(2), game.GameBeginData{GameID: 101, SpawnSeed: 11, ItemSeed: 22}, participants, false,
		battleengine.PolicyFunc(func(observation battleengine.Observation, legal []battleengine.Action) (battleengine.Action, error) {
			return battleengine.Action{PlayerID: observation.PlayerID}, nil
		}), 100,
	)
	if err != nil {
		t.Fatal(err)
	}
	virtual, ok := actorByID(runtime.runtime.EngineSnapshot().Actors(), 20001)
	if !ok {
		t.Fatal("virtual actor is absent")
	}
	pickup := battleengine.Pickup{SceneID: battleengine.SceneBombCapacitySmall, Cell: virtual.Position.Cell(), State: battleengine.PickupAvailable}
	if err = runtime.runtime.AcceptNativePickupDispatch(runtime.runtime.EngineSnapshot().ElapsedMS(), []battleengine.Pickup{pickup}); err != nil {
		t.Fatal(err)
	}
	for !snapshotHasAvailablePickup(runtime.runtime.EngineSnapshot(), pickup) {
		if _, err = runtime.runtime.Step(nil); err != nil {
			t.Fatal(err)
		}
	}
	key := liveCompetitiveAIPickupRequestKey{playerID: 20001, sceneID: pickup.SceneID, row: pickup.Cell.Row, col: pickup.Cell.Col}
	runtime.pickupRequests[key] = liveCompetitiveAIPickupRequest{
		timeMS: 3200, position: virtual.Position,
		state: competitiveAIPickupPending, attempts: 1,
	}
	if rejected, matched := runtime.takeRejectedPickupRequest(pickup.SceneID, 3200, virtual.Position); !matched || rejected != key {
		t.Fatalf("negative pickup ACK matched %+v/%v, want %+v/true", rejected, matched, key)
	}
	runtime.refreshPickupRequests(runtime.runtime.EngineSnapshot())
	if request, exists := runtime.pickupRequests[key]; !exists || request.state != competitiveAIPickupRejectedUntilExit {
		t.Fatalf("same-cell rejected request = %+v/%v, want sticky rejection", request, exists)
	}

	// A still-pending request survives complete footprint exit because its ACK
	// may already be queued by the native arbitrator. Once that exact request is
	// rejected, exiting the footprint creates the next legitimate contact edge.
	snapshot := runtime.runtime.EngineSnapshot()
	grid := snapshot.Grid()
	farCell := battleengine.Cell{}
	foundFarCell := false
	for row := int16(0); row < int16(grid.Height) && !foundFarCell; row++ {
		for col := int16(0); col < int16(grid.Width); col++ {
			candidate := battleengine.Cell{Row: row, Col: col}
			tile, inside := grid.Cell(candidate)
			if inside && tile.Kind == battleengine.CellOpen && !snapshot.PositionOverlapsCell(virtual.Position, candidate) {
				farCell = candidate
				foundFarCell = true
				break
			}
		}
	}
	if !foundFarCell {
		t.Fatalf("no open cell outside virtual footprint at %+v", virtual.Position)
	}
	farPickup := battleengine.Pickup{SceneID: battleengine.SceneBombPowerSmall, Cell: farCell, State: battleengine.PickupAvailable}
	if err = runtime.runtime.AcceptNativePickupDispatch(runtime.runtime.EngineSnapshot().ElapsedMS(), []battleengine.Pickup{farPickup}); err != nil {
		t.Fatal(err)
	}
	for !snapshotHasAvailablePickup(runtime.runtime.EngineSnapshot(), farPickup) {
		if _, err = runtime.runtime.Step(nil); err != nil {
			t.Fatal(err)
		}
	}
	farKey := liveCompetitiveAIPickupRequestKey{playerID: 20001, sceneID: farPickup.SceneID, row: farCell.Row, col: farCell.Col}
	runtime.pickupRequests[farKey] = liveCompetitiveAIPickupRequest{
		timeMS: 3300, lastSentAt: 3300, position: battleengine.PositionAtCellCenter(farCell),
		state: competitiveAIPickupPending, attempts: 1,
	}
	runtime.refreshPickupRequests(runtime.runtime.EngineSnapshot())
	if _, exists := runtime.pickupRequests[farKey]; !exists {
		t.Fatal("unacknowledged pickup transaction was released on footprint exit")
	}
	sequenceBefore := runtime.messageSeq[20001]
	runtime.projectEvents(nil, runtime.runtime.EngineSnapshot(), runtime.runtime.EngineSnapshot(), []battleengine.Event{{
		Kind: battleengine.EventPickupCollectRequested, TimeMS: 3300,
		PlayerID: 20001, SceneID: farPickup.SceneID, Cell: farCell,
		Position: battleengine.PositionAtCellCenter(farCell),
	}})
	if runtime.messageSeq[20001] != sequenceBefore {
		t.Fatal("suppressed duplicate pickup still produced a packet")
	}
	request := runtime.pickupRequests[farKey]
	request.attempts = 3
	runtime.pickupRequests[farKey] = request
	runtime.refreshPickupRequests(runtime.runtime.EngineSnapshot())
	if _, exists := runtime.pickupRequests[farKey]; exists {
		t.Fatal("exhausted pickup transaction survived complete footprint exit")
	}
	runtime.pickupRequests[farKey] = liveCompetitiveAIPickupRequest{
		timeMS: 3300, lastSentAt: 3300, position: battleengine.PositionAtCellCenter(farCell),
		state: competitiveAIPickupRejectedUntilExit, attempts: 1,
	}
	request = runtime.pickupRequests[farKey]
	request.state = competitiveAIPickupRejectedUntilExit
	runtime.pickupRequests[farKey] = request
	runtime.refreshPickupRequests(runtime.runtime.EngineSnapshot())
	if _, exists := runtime.pickupRequests[farKey]; exists {
		t.Fatal("rejected pickup transaction survived complete footprint exit")
	}

	if err = runtime.runtime.AcceptNativeItemDestruction([]battleengine.Pickup{pickup}); err != nil {
		t.Fatal(err)
	}
	runtime.refreshPickupRequests(runtime.runtime.EngineSnapshot())
	if _, exists := runtime.pickupRequests[key]; exists {
		t.Fatal("scene-object destruction did not release pickup transaction")
	}
}

func TestBattlePickupsFromDispatchIncludesMaturedDelayedItems(t *testing.T) {
	dispatch := game.DispatchItemData{
		Time:  90_000,
		Items: []game.GameItem{{ItemID: battleengine.SceneBombCapacitySmall, Row: 1, Col: 2}},
		DelayedItems: []game.DelayedGameItem{
			{ItemID: battleengine.SceneSpeedSmall, DispatchTime: 60_000, Row: 3, Col: 4},
			{ItemID: battleengine.SceneBombPowerSmall, DispatchTime: 90_000, Row: 5, Col: 6},
		},
	}
	got := battlePickupsFromDispatch(dispatch)
	want := []battleengine.Pickup{
		{SceneID: battleengine.SceneBombCapacitySmall, Cell: battleengine.Cell{Row: 1, Col: 2}, State: battleengine.PickupAvailable},
		{SceneID: battleengine.SceneSpeedSmall, Cell: battleengine.Cell{Row: 3, Col: 4}, State: battleengine.PickupAvailable},
		{SceneID: battleengine.SceneBombPowerSmall, Cell: battleengine.Cell{Row: 5, Col: 6}, State: battleengine.PickupAvailable},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("dispatch pickups = %+v, want %+v", got, want)
	}
}

func snapshotHasAvailablePickup(snapshot *battleengine.Engine, want battleengine.Pickup) bool {
	if snapshot == nil {
		return false
	}
	for _, pickup := range snapshot.Pickups() {
		if pickup.SceneID == want.SceneID && pickup.Cell == want.Cell && pickup.State == battleengine.PickupAvailable {
			return true
		}
	}
	return false
}

func TestCompetitiveAINativeFieldContactCommitsAuthorityNotification(t *testing.T) {
	participants := []match.CompetitiveParticipant{
		{PlayerID: 1, RoleID: 1, TeamID: 1, Source: match.CompetitiveParticipantHuman},
		{PlayerID: 20001, RoleID: 2, TeamID: 2, Source: match.CompetitiveParticipantVirtualAI},
	}
	runtime, err := newLiveCompetitiveAIRuntime(
		7, testCompetitiveAIMap(2), game.GameBeginData{GameID: 103, SpawnSeed: 11, ItemSeed: 22}, participants, false,
		battleengine.PolicyFunc(func(observation battleengine.Observation, legal []battleengine.Action) (battleengine.Action, error) {
			return battleengine.Action{PlayerID: observation.PlayerID}, nil
		}), 100,
	)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := runtime.runtime.EngineSnapshot()
	human, humanOK := actorByID(snapshot.Actors(), 1)
	virtual, virtualOK := actorByID(snapshot.Actors(), 20001)
	if !humanOK || !virtualOK {
		t.Fatalf("field-contact actors missing: human=%+v/%v virtual=%+v/%v", human, humanOK, virtual, virtualOK)
	}
	// Scene 25 grants action 43. The verified use creates the placed field that
	// the arbitrator's later ItemID=43 notification must consume.
	if _, err = runtime.runtime.AcceptNativePickup(human.PlayerID, 25, human.Position); err != nil {
		t.Fatal(err)
	}
	if _, err = runtime.runtime.AcceptNativeBattleAction(human.PlayerID, 43, virtual.Position, 0); err != nil {
		t.Fatal(err)
	}
	if fields := runtime.runtime.EngineSnapshot().FieldObjects(); len(fields) != 1 || fields[0].ActionID != 43 {
		t.Fatalf("slow field setup = %+v", fields)
	}
	requestKey := liveCompetitiveAIPickupRequestKey{
		playerID: virtual.PlayerID, sceneID: 43,
		row: virtual.Position.Cell().Row, col: virtual.Position.Cell().Col,
	}
	runtime.pickupRequests[requestKey] = liveCompetitiveAIPickupRequest{
		timeMS: 3_200, lastSentAt: 3_200, position: virtual.Position,
		state: competitiveAIPickupPending, attempts: 1,
	}
	sequenceBefore := runtime.messageSeq[virtual.PlayerID]
	runtime.projectEvents(nil, snapshot, runtime.runtime.EngineSnapshot(), []battleengine.Event{{
		Kind: battleengine.EventFieldObjectTriggered, TimeMS: 3_200,
		PlayerID: human.PlayerID, TargetID: virtual.PlayerID,
		Cell: virtual.Position.Cell(), Position: virtual.Position, ActionID: 43,
	}})
	if runtime.messageSeq[virtual.PlayerID] != sequenceBefore {
		t.Fatal("suppressed duplicate field contact produced a packet")
	}
	if len(runtime.pickupRequests) != 1 {
		t.Fatalf("field contact did not reuse target transaction: %+v", runtime.pickupRequests)
	}
	battle, err := match.NewCompetitiveBattle(103, 2, 1, participants)
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{
		competitiveAIRuntime: map[uint32]*liveCompetitiveAIRuntime{103: runtime},
		competitiveBattles:   map[uint32]*match.CompetitiveBattle{103: battle},
		logWriter:            io.Discard,
	}
	body, err := (game.PlayerItemEvent{
		PlayerID: virtual.PlayerID, ClientTime: 3_200, ItemID: 43,
		PosX: uint16(virtual.Position.X), PosY: uint16(virtual.Position.Y),
	}).MarshalNetworkBinary()
	if err != nil {
		t.Fatal(err)
	}
	server.recordCompetitiveAIHumanPackages(103, []game.GameplayDataPackage{{
		PlayerID: human.PlayerID, Time: 3_200, GameID: 103,
		Messages: []game.BattleMessageData{{Time: 3_200, DataID: game.NotifyPlayerGetItem, Data: body}},
	}})

	after := runtime.runtime.EngineSnapshot()
	gotVirtual, ok := actorByID(after.Actors(), virtual.PlayerID)
	if !ok || gotVirtual.MovementStatus != battleengine.MovementStatusSlow {
		t.Fatalf("authority field contact target = %+v/%v", gotVirtual, ok)
	}
	if fields := after.FieldObjects(); len(fields) != 0 {
		t.Fatalf("authority field contact retained field %+v", fields)
	}
	if _, pending := runtime.pickupRequests[requestKey]; pending {
		t.Fatal("accepted field contact retained virtual 0x0FAC request")
	}
}

func TestCompetitiveAIReliableHitAuditCommitsExactPendingVirtualRequest(t *testing.T) {
	server, authority, nonAuthority, _ := newReliableBossRuleTest(t, 11, []uint16{1})
	participants := []match.CompetitiveParticipant{
		{PlayerID: authority.Profile.PlayerID, RoleID: 1, TeamID: 1, Source: match.CompetitiveParticipantHuman},
		{PlayerID: 20001, RoleID: 2, TeamID: 2, Source: match.CompetitiveParticipantVirtualAI},
	}
	runtime, err := newLiveCompetitiveAIRuntime(
		7, testCompetitiveAIMap(2), game.GameBeginData{GameID: 1, SpawnSeed: 11, ItemSeed: 22}, participants, false,
		battleengine.PolicyFunc(func(observation battleengine.Observation, legal []battleengine.Action) (battleengine.Action, error) {
			return battleengine.Action{PlayerID: observation.PlayerID}, nil
		}), 100,
	)
	if err != nil {
		t.Fatal(err)
	}
	before := runtime.runtime.EngineSnapshot()
	virtual, ok := actorByID(before.Actors(), 20001)
	if !ok {
		t.Fatal("virtual actor is absent")
	}
	hit := game.PlayerExplodedEvent{
		PlayerID: 20001, ClientTime: 3500,
		PosX: uint16(virtual.Position.X), PosY: uint16(virtual.Position.Y),
	}
	runtime.reserveHitRequest(hit, authority.Profile.PlayerID, time.Now())
	battle, err := match.NewCompetitiveBattle(1, 2, authority.Profile.PlayerID, participants)
	if err != nil {
		t.Fatal(err)
	}
	server.competitiveAIRuntime = map[uint32]*liveCompetitiveAIRuntime{1: runtime}
	server.competitiveBattles = map[uint32]*match.CompetitiveBattle{1: battle}
	body := mustMarshalPlayerExploded(t, hit)

	result := sendReliableBossRuleEvent(t, server, authority, 1, game.PlayerBeExploded, body)
	if !result.handled || len(result.response) == 0 || result.afterResponse == nil {
		t.Fatalf("exact authority audit was not ACKed transactionally: %+v", result)
	}
	virtual, _ = actorByID(runtime.runtime.EngineSnapshot().Actors(), 20001)
	if virtual.State != battleengine.ActorActive {
		t.Fatalf("hit committed before reliable ACK boundary: %+v", virtual)
	}
	result.afterResponse()
	virtual, _ = actorByID(runtime.runtime.EngineSnapshot().Actors(), 20001)
	if virtual.State != battleengine.ActorTrapped || virtual.Position != (battleengine.Position{X: int32(hit.PosX), Y: int32(hit.PosY)}) {
		t.Fatalf("reliable hit audit committed %+v, want trapped at exact request position", virtual)
	}

	duplicate := sendReliableBossRuleEvent(t, server, authority, 2, game.PlayerBeExploded, body)
	if len(duplicate.response) == 0 || duplicate.afterResponse == nil {
		t.Fatalf("reliable retry was not ACKed idempotently: %+v", duplicate)
	}
	duplicate.afterResponse()
	virtual, _ = actorByID(runtime.runtime.EngineSnapshot().Actors(), 20001)
	if virtual.State != battleengine.ActorTrapped {
		t.Fatalf("reliable retry changed committed actor: %+v", virtual)
	}

	mismatch := hit
	mismatch.PosX++
	mismatchResult := sendReliableBossRuleEvent(t, server, authority, 3, game.PlayerBeExploded, mustMarshalPlayerExploded(t, mismatch))
	if len(mismatchResult.response) != 0 || mismatchResult.afterResponse != nil {
		t.Fatalf("mismatched authority audit was accepted: %+v", mismatchResult)
	}
	nonAuthorityResult := sendReliableBossRuleEvent(t, server, nonAuthority, 4, game.PlayerBeExploded, body)
	if len(nonAuthorityResult.response) != 0 || nonAuthorityResult.afterResponse != nil {
		t.Fatalf("non-authority reliable audit was accepted: %+v", nonAuthorityResult)
	}

	if !runtime.hasHitRequest(hit, time.Now().Add(24*time.Hour)) || len(runtime.hitRequests) != 1 {
		t.Fatalf("committed lifecycle marker was lost before actor death: %+v", runtime.hitRequests)
	}
	runtime.mu.Lock()
	err = runtime.acceptNativeEliminationLocked(20001, authority.Profile.PlayerID, nil)
	runtime.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if len(runtime.hitRequests) != 0 {
		t.Fatalf("hit lifecycle marker survived actor death: %+v", runtime.hitRequests)
	}
}

func TestCompetitiveAIReconcilesNativeBlastAgainstExactEventFrame(t *testing.T) {
	newRuntime := func(gameID uint32) *liveCompetitiveAIRuntime {
		selectedMap := testCompetitiveAIMap(2)
		selectedMap.Battlefield.Height = 3
		selectedMap.Battlefield.Cells = append(selectedMap.Battlefield.Cells, selectedMap.Battlefield.Cells[:4]...)
		runtime, err := newLiveCompetitiveAIRuntime(
			7, selectedMap, game.GameBeginData{GameID: gameID, SpawnSeed: 11, ItemSeed: 22},
			[]match.CompetitiveParticipant{
				{PlayerID: 1, RoleID: 1, TeamID: 1, Source: match.CompetitiveParticipantHuman},
				{PlayerID: 20001, RoleID: 2, TeamID: 2, Source: match.CompetitiveParticipantVirtualAI},
			}, false,
			battleengine.PolicyFunc(func(observation battleengine.Observation, legal []battleengine.Action) (battleengine.Action, error) {
				return battleengine.Action{PlayerID: observation.PlayerID}, nil
			}), 100,
		)
		if err != nil {
			t.Fatal(err)
		}
		return runtime
	}

	runtime := newRuntime(93)
	actor, ok := actorByID(runtime.runtime.EngineSnapshot().Actors(), 20001)
	if !ok {
		t.Fatal("virtual actor is absent")
	}
	currentCell := actor.Position.Cell()
	futureCell := battleengine.Cell{}
	foundFutureCell := false
	grid := runtime.runtime.EngineSnapshot().Grid()
	for row := int16(0); row < int16(grid.Height) && !foundFutureCell; row++ {
		for col := int16(0); col < int16(grid.Width); col++ {
			candidate := battleengine.Cell{Row: row, Col: col}
			tile, inside := grid.Cell(candidate)
			if inside && tile.Kind == battleengine.CellOpen && candidate != currentCell {
				futureCell = candidate
				foundFutureCell = true
				break
			}
		}
	}
	if !foundFutureCell {
		t.Fatal("test map has no second open cell")
	}
	initialFrame, ok := runtime.actorFrameAtLocked(20001, battleengine.NativeRoundStartClockMS)
	if !ok {
		t.Fatal("initial virtual position frame is absent")
	}
	futureFrame := initialFrame
	futureFrame.position = battleengine.PositionAtCellCenter(futureCell)
	runtime.positionHistory = append(runtime.positionHistory, liveCompetitiveAIPositionFrame{
		timeMS: 4000, actors: map[uint16]liveCompetitiveAIActorFrame{20001: futureFrame},
	})
	bombs := []battleengine.VerifiedExplodedBomb{{
		OwnerID: 1, Cell: currentCell,
		BlastRowMin: currentCell.Row, BlastRowMax: currentCell.Row,
		BlastColMin: currentCell.Col, BlastColMax: currentCell.Col,
	}}
	events := runtime.reconcileVirtualExplosionHits(bombs, battleengine.NativeRoundStartClockMS)
	if actor.State != battleengine.ActorActive || len(events) != 1 ||
		events[0].Kind != battleengine.EventActorHitRequested || events[0].PlayerID != 20001 ||
		events[0].TargetID != 1 || events[0].Position != actor.Position || events[0].TimeMS != battleengine.NativeRoundStartClockMS {
		t.Fatalf("event-frame reconciliation = actor %+v events %+v", actor, events)
	}

	missCell := futureCell
	missBombs := []battleengine.VerifiedExplodedBomb{{
		OwnerID: 1, Cell: missCell,
		BlastRowMin: missCell.Row, BlastRowMax: missCell.Row,
		BlastColMin: missCell.Col, BlastColMax: missCell.Col,
	}}
	if events = runtime.reconcileVirtualExplosionHits(missBombs, battleengine.NativeRoundStartClockMS); len(events) != 0 {
		t.Fatalf("future receive-time cell produced historical false positive: actor %+v events %+v", actor, events)
	}
	if events = runtime.reconcileVirtualExplosionHits(missBombs, 4000); len(events) != 1 || events[0].Position != futureFrame.position {
		t.Fatalf("later event frame missed exact blast: frame %+v events %+v", futureFrame, events)
	}
	if _, ok = runtime.actorFrameAtLocked(20001, 5000); ok {
		t.Fatal("future authority event reused the latest stale position frame")
	}

	// Native blast registration uses the centre's discrete cell, not the
	// movement footprint. Check all four one-pixel boundaries through the live
	// timestamp/history adapter, including a later frame that has escaped again.
	for _, boundary := range []struct {
		name          string
		outside, edge battleengine.Position
	}{
		{"left", battleengine.Position{X: 39, Y: 60}, battleengine.Position{X: 40, Y: 60}},
		{"right", battleengine.Position{X: 80, Y: 60}, battleengine.Position{X: 79, Y: 60}},
		{"top", battleengine.Position{X: 60, Y: 39}, battleengine.Position{X: 60, Y: 40}},
		{"bottom", battleengine.Position{X: 60, Y: 80}, battleengine.Position{X: 60, Y: 79}},
	} {
		t.Run(boundary.name, func(t *testing.T) {
			live := newRuntime(94)
			outside := initialFrame
			outside.position = boundary.outside
			inside := outside
			inside.position = boundary.edge
			live.positionHistory = []liveCompetitiveAIPositionFrame{
				{timeMS: 4000, actors: map[uint16]liveCompetitiveAIActorFrame{20001: outside}},
				{timeMS: 4020, actors: map[uint16]liveCompetitiveAIActorFrame{20001: inside}},
				{timeMS: 4040, actors: map[uint16]liveCompetitiveAIActorFrame{20001: outside}},
			}
			blast := []battleengine.VerifiedExplodedBomb{{
				OwnerID: 1, Cell: battleengine.Cell{Row: 1, Col: 1},
				BlastRowMin: 1, BlastRowMax: 1, BlastColMin: 1, BlastColMax: 1,
			}}
			for _, at := range []uint32{4000, 4019, 4020, 4039, 4040} {
				requests := live.reconcileVirtualExplosionHits(blast, at)
				wantHit := at >= 4020 && at < 4040
				if (len(requests) == 1) != wantHit || len(requests) > 1 {
					t.Fatalf("at %d: hit=%t requests=%+v", at, wantHit, requests)
				}
				if wantHit && (requests[0].Position != boundary.edge || requests[0].TimeMS != at) {
					t.Fatalf("at %d: request lost impact position/time: %+v", at, requests)
				}
			}
		})
	}
}

func TestCompetitiveAINativePeerProgressUsesAuthenticatedPair(t *testing.T) {
	runtime := &liveCompetitiveAIRuntime{
		humanIDs:          map[uint16]struct{}{1: {}, 2: {}},
		virtualIDs:        map[uint16]struct{}{20001: {}, 20002: {}},
		nativePeers:       make(map[liveCompetitiveAIPeerKey]bool),
		nativePeerChanged: make(chan struct{}, 1),
	}
	if ready, expected := runtime.nativePeerProgress(); ready != 0 || expected != 4 {
		t.Fatalf("initial peer progress = %d/%d, want 0/4", ready, expected)
	}
	runtime.markNativePeerReady(1, 20001)
	runtime.markNativePeerReady(1, 20001)
	runtime.markNativePeerReady(99, 20001)
	runtime.markNativePeerReady(1, 29999)
	if ready, expected := runtime.nativePeerProgress(); ready != 1 || expected != 4 {
		t.Fatalf("authenticated peer progress = %d/%d, want 1/4", ready, expected)
	}
}
