package battleai

import (
	"fmt"
	"sync"

	"qqtang/internal/game/battleengine"
)

// NativePolicyConfig describes the complete dependency-free server policy.
// Greedy actor inference is the deployment default so formal evaluation and
// live execution use the same learned decisions. Bounded search remains an
// explicit diagnostic/teacher option rather than an implicit safety layer.
type NativePolicyConfig struct {
	DangerHorizonMS uint32
	DecisionMS      uint32
	EnableSearch    bool
	Search          battleengine.SearchConfig
}

// LoadNativePolicy constructs a server-ready policy from one QTAI artifact.
// The returned policy owns immutable weights and is safe to share among every
// virtual participant in a match; each participant still receives a separate
// visibility-bounded observation from battleengine.Runtime.
func LoadNativePolicy(modelPath string, config NativePolicyConfig) (battleengine.Policy, error) {
	runner, err := LoadNativeRunner(modelPath)
	if err != nil {
		return nil, err
	}
	return buildActorPolicy(runner.Contract(), runner, config)
}

func buildActorPolicy(
	contract Contract,
	runner LogitRunner,
	config NativePolicyConfig,
) (battleengine.Policy, error) {
	if err := contract.Validate(); err != nil {
		return nil, fmt.Errorf("validate AI contract: %w", err)
	}
	template := &actorPolicyTemplate{
		contract: contract, runner: runner, config: config,
	}
	template.defaultPolicy = template.NewActorPolicy()
	return template, nil
}

// actorPolicyTemplate owns immutable model/session resources and creates the
// actor-local wrapper that owns recurrent memory. The default delegate keeps
// direct single-actor callers working; live matches always fork one delegate
// per virtual participant.
type actorPolicyTemplate struct {
	contract      Contract
	runner        LogitRunner
	config        NativePolicyConfig
	defaultMu     sync.Mutex
	defaultPolicy battleengine.Policy
}

func (template *actorPolicyTemplate) NewActorPolicy() battleengine.Policy {
	candidates := &NeuralCandidates{
		Contract:        template.contract,
		Runner:          template.runner,
		DangerHorizonMS: template.config.DangerHorizonMS,
		DecisionMS:      template.config.DecisionMS,
		resetMemory:     template.contract.RecurrentHiddenSize > 0,
	}
	if !template.config.EnableSearch {
		return battleengine.GreedyCandidatePolicy{Candidate: candidates}
	}
	return battleengine.TopKSearchPolicy{
		Candidate: candidates, Config: template.config.Search,
	}
}

func (template *actorPolicyTemplate) ChooseAction(
	observation battleengine.Observation,
	legal []battleengine.Action,
) (battleengine.Action, error) {
	template.defaultMu.Lock()
	defer template.defaultMu.Unlock()
	return template.defaultPolicy.ChooseAction(observation, legal)
}

func (template *actorPolicyTemplate) ChooseActionWithSnapshot(
	snapshot *battleengine.Engine,
	observation battleengine.Observation,
	legal []battleengine.Action,
) (battleengine.Action, error) {
	template.defaultMu.Lock()
	defer template.defaultMu.Unlock()
	if policy, ok := template.defaultPolicy.(battleengine.SnapshotPolicy); ok {
		return policy.ChooseActionWithSnapshot(snapshot, observation, legal)
	}
	return template.defaultPolicy.ChooseAction(observation, legal)
}
