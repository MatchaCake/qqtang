package probe

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"testing"
	"time"

	"qqtang/internal/game/battleai"
	"qqtang/internal/game/battleengine"
	"qqtang/internal/game/mapdata"
)

type searchMatchResult struct {
	Format          string               `json:"format"`
	Seed            uint32               `json:"seed"`
	Swap            bool                 `json:"swap"`
	Winner          string               `json:"winner"`
	Outcome         battleengine.Outcome `json:"outcome"`
	SearchKills     int                  `json:"search_kills"`
	GreedyKills     int                  `json:"greedy_kills"`
	SearchSelfKills int                  `json:"search_self_kills"`
	GreedySelfKills int                  `json:"greedy_self_kills"`
	WallSeconds     float64              `json:"wall_seconds"`
	Error           string               `json:"error,omitempty"`
}

// Opt-in complete 240-second native-rule matches, with swapped policy sides.
// Both sides use the actual ONNX and the same live cadence/placement guards.
// This is an engine A/B, not a claim about human-client network behavior.
func TestCompetitiveAISearchPairedMatches(t *testing.T) {
	output := os.Getenv("QQTANG_TEST_SEARCH_MATCH_OUTPUT")
	if output == "" {
		t.Skip("set QQTANG_TEST_SEARCH_MATCH_OUTPUT and ONNX/client paths")
	}
	catalog, err := mapdata.LoadCatalog(os.Getenv("QQTANG_TEST_CLIENT_ROOT"))
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := catalog.CompetitiveMapMetadata(905)
	if !ok {
		t.Fatal("native map 905 unavailable")
	}
	var templates [2]interface{ NewActorPolicy() battleengine.Policy }
	for index := range templates {
		loaded, loadErr := battleai.LoadDeploymentPolicy(battleai.DeploymentPolicyConfig{
			Backend:   battleai.DeploymentBackendONNXRuntime,
			ModelPath: os.Getenv("QQTANG_TEST_ONNX_MODEL"), MetadataPath: os.Getenv("QQTANG_TEST_ONNX_METADATA"),
			SharedLibraryPath: os.Getenv("QQTANG_TEST_ONNX_RUNTIME"), IntraOpThreads: 1, InterOpThreads: 1,
			NativePolicyConfig: battleai.NativePolicyConfig{
				DangerHorizonMS: 3500, DecisionMS: 100, EnableSearch: index == 1,
				Search: battleengine.SearchConfig{TopK: 4, HorizonMS: 3200, DangerHorizonMS: 3500, PriorWeight: 0.35, EliminationValue: 100, TrapValue: 12},
			},
		})
		if loadErr != nil {
			t.Fatal(loadErr)
		}
		if loaded.Closer != nil {
			t.Cleanup(func() { _ = loaded.Closer.Close() })
		}
		templates[index] = loaded.Policy.(interface{ NewActorPolicy() battleengine.Policy })
	}
	jobs := make(chan searchMatchResult, 32)
	results := make(chan searchMatchResult, 32)
	total := 0
	for _, setup := range []struct {
		format string
		seeds  int
	}{{"duel", 8}, {"team", 4}, {"ffa", 4}} {
		for seed := 0; seed < setup.seeds; seed++ {
			for _, swap := range []bool{false, true} {
				jobs <- searchMatchResult{Format: setup.format, Seed: 295001 + uint32(seed)*7919, Swap: swap}
				total++
			}
		}
	}
	close(jobs)
	for worker := 0; worker < 2; worker++ {
		go func() {
			for job := range jobs {
				results <- runSearchMatch(entry, templates, job)
			}
		}()
	}
	all := make([]searchMatchResult, 0, total)
	for range total {
		result := <-results
		all = append(all, result)
		t.Logf("%s seed=%d swap=%v winner=%s game_ms=%d wall=%.2fs error=%s", result.Format, result.Seed, result.Swap, result.Winner, result.Outcome.EndedAtMS, result.WallSeconds, result.Error)
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].Format != all[j].Format {
			return all[i].Format < all[j].Format
		}
		if all[i].Seed != all[j].Seed {
			return all[i].Seed < all[j].Seed
		}
		return !all[i].Swap && all[j].Swap
	})
	data, err := json.MarshalIndent(all, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(output, data, 0600); err != nil {
		t.Fatal(err)
	}
	for _, result := range all {
		if result.Error != "" {
			t.Error(result.Error)
		}
	}
}

func runSearchMatch(entry mapdata.CompetitiveMap, templates [2]interface{ NewActorPolicy() battleengine.Policy }, result searchMatchResult) searchMatchResult {
	started := time.Now()
	count := 2
	if result.Format == "team" {
		count = 4
	} else if result.Format == "ffa" {
		count = 8
	}
	participants := make([]battleengine.Participant, 0, count)
	policies := make(map[uint16]battleengine.Policy, count)
	isSearch := make(map[uint16]bool, count)
	searchTeams := make(map[byte]bool)
	for i := 0; i < count; i++ {
		playerID := uint16(i + 1)
		team := byte(i%2 + 1)
		if result.Format == "ffa" {
			team = byte(i + 1)
		}
		participant, err := battleengine.ParticipantFromNativeRole(playerID, 9, team, battleengine.ParticipantVirtualAI)
		if err != nil {
			result.Error = err.Error()
			return result
		}
		participants = append(participants, participant)
		search := i%2 == 0
		if result.Swap {
			search = !search
		}
		isSearch[playerID] = search
		searchTeams[team] = search
		variant := 0
		if search {
			variant = 1
		}
		phase := competitiveAIDecisionPhase(1, result.Seed, result.Seed^0x9E3779B9, playerID, 5)
		policies[playerID] = competitiveAILivePolicy(templates[variant].NewActorPolicy(), 5, phase)
	}
	config, err := battleengine.ConfigFromCompetitiveMap(entry, battleengine.CompetitiveMapConfigOptions{
		SpawnSeed: result.Seed, ItemSeed: result.Seed ^ 0x9E3779B9,
		SpawnMode: battleengine.NativeSpawnModeForMap(entry, result.Format == "ffa"),
		Rules:     battleengine.Rules{TickMS: 20, TrapDurationMS: 6000}, Participants: participants,
	})
	if err != nil {
		result.Error = err.Error()
		return result
	}
	engine, err := battleengine.New(config)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	runtime, err := battleengine.NewRuntime(engine, policies)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	for !engine.Terminal().Ended {
		events, stepErr := runtime.Step(nil)
		if stepErr != nil {
			result.Error = fmt.Sprintf("%s %d: %v", result.Format, engine.ElapsedMS(), stepErr)
			break
		}
		for _, event := range events {
			if event.Kind != battleengine.EventActorEliminated {
				continue
			}
			if event.PlayerID == event.TargetID {
				if isSearch[event.PlayerID] {
					result.SearchSelfKills++
				} else {
					result.GreedySelfKills++
				}
			} else if event.PlayerID != 0 {
				if isSearch[event.PlayerID] {
					result.SearchKills++
				} else {
					result.GreedyKills++
				}
			}
		}
	}
	result.Outcome = engine.Terminal()
	result.Winner = "draw"
	if result.Outcome.Ended && !result.Outcome.Draw {
		result.Winner = "greedy"
		if searchTeams[result.Outcome.WinnerTeamID] {
			result.Winner = "search"
		}
	}
	result.WallSeconds = time.Since(started).Seconds()
	return result
}
