package hud

import (
	"context"

	"github.com/crb2nu/loom/internal/hud/autofix"
	"github.com/crb2nu/loom/internal/hud/coordinator"
	"github.com/crb2nu/loom/pkg/flexinfer"
)

// autofixSpawnerAdapter adapts *SpawnOrchestrator to autofix.SpawnerOps.
// autofix declares its own minimal SpawnRequest so the package doesn't
// import the spawn machinery; this maps it onto the orchestrator's request
// type (same pattern as mobileSpawnerAdapter).
type autofixSpawnerAdapter struct {
	s *SpawnOrchestrator
}

func (sa *autofixSpawnerAdapter) Spawn(ctx context.Context, req autofix.SpawnRequest) (string, error) {
	return sa.s.Spawn(ctx, SpawnRequest{
		AgentType:       req.AgentType,
		Project:         req.Project,
		TaskDescription: req.TaskDescription,
		Branch:          req.Branch,
		BaseBranch:      req.BaseBranch,
		Namespace:       req.Namespace,
	})
}

// initAutofixEngine wires the auto-fix engine behind config.AutofixEnabled
// (default off until soak, mirroring the overseers pattern). While the
// engine is nil the alerting domain's /api/autofix/* routes keep serving
// honest empty lists and 503s on mutations.
//
// Call after initSpawnOrchestrator and initCoordinator: the engine shares
// the coordinator's FlexInfer client (breaker + usage accounting) when the
// coordinator is up, and falls back to a dedicated client built from the
// same FlexInfer config when it isn't. No LLM backend at all means no
// engine — diagnosis is impossible without one.
func (a *App) initAutofixEngine() {
	if !a.config.AutofixEnabled {
		return
	}

	llm := a.coordinator.Client()
	llmSource := "coordinator"
	if llm == nil {
		if a.config.FlexInferURL == "" {
			a.logger.Warn("autofix: enabled but no LLM backend configured (FLEXINFER_URL unset); engine disabled")
			return
		}
		coordCfg := coordinator.ConfigFromEnv()
		breaker := flexinfer.NewCircuitBreaker(coordCfg.CircuitBreakerThreshold, coordCfg.CircuitBreakerReset)
		llm = flexinfer.NewClient(a.config.FlexInferURL, a.config.FlexInferKey, coordCfg.DefaultTimeout, breaker, a.logger)
		llmSource = "dedicated"
	}

	// Spawner is optional: without it the agent_fix strategy reports
	// "no spawn orchestrator available" instead of pretending to act.
	var spawner autofix.SpawnerOps
	if a.spawner != nil {
		spawner = &autofixSpawnerAdapter{s: a.spawner}
	}

	// a.agent satisfies PipelineBridge, JobTraceBridge, and PipelineRetrier
	// (the retry strategy discovers the latter by type assertion). Model ""
	// resolves through aimodels.RoleAutofix inside the constructor.
	a.autofixEngine = autofix.NewAutoFixEngine(llm, a.agent, a.agent, spawner, "", a.logger)
	a.logger.Info("autofix engine enabled",
		"llm", llmSource,
		"spawner", spawner != nil,
	)
}
