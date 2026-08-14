package pipeline

import (
	"context"
	"fmt"
	"strings"

	"github.com/crb2nu/loom/pkg/mills/gates"
	"github.com/crb2nu/loom/pkg/mills/store"
)

// HealthGatePreflight supplies the latest infrastructure gate verdict before a
// pipeline starts or resumes. Implementations should call gates.EvaluateHealthSnapshot
// over fresh evidence from the daemon, MCP hub, GitLab, vector DB, and devbox layer.
type HealthGatePreflight interface {
	DecideHealthGates(ctx context.Context) (gates.HealthDecision, error)
}

// StaticHealthPreflight is a small adapter for tests and fixed snapshots.
type StaticHealthPreflight struct {
	Decision gates.HealthDecision
	Err      error
}

func (p StaticHealthPreflight) DecideHealthGates(context.Context) (gates.HealthDecision, error) {
	return p.Decision, p.Err
}

func (r *Runner) runPreflight(ctx context.Context, run *store.PipelineRun, item *store.BacklogItem) (bool, error) {
	if r.HealthGates == nil {
		return false, nil
	}
	decision, err := r.HealthGates.DecideHealthGates(ctx)
	if err != nil {
		decision = gates.HealthDecision{
			Allowed:    false,
			FailClosed: true,
			Status:     "block",
			Reasons:    []string{fmt.Sprintf("health gates unavailable: %v", err)},
		}
	}
	if decision.Allowed {
		r.event(ctx, "pipeline.preflight.health_gates", "ok", map[string]any{
			"run": run.ID, "backlog_id": item.ID, "status": decision.Status,
		})
		return false, nil
	}
	// Classify deliberately rather than letting the unmarked-escalation
	// fallback guess from prose (it would read "infrastructure health gates
	// blocked pipeline" as ClassCode — a code defect, which this never is).
	//
	// The two block shapes are genuinely different faults:
	//   - fail-closed: the gate could NOT be evaluated, so health is unknown.
	//     A human should find out why the evaluator is unreachable —
	//     ClassConfig, terminal, never auto-requeued.
	//   - measured-unhealthy: infrastructure is down and expected to recover,
	//     which is exactly what ClassInfra's bounded auto-requeue (cooldown +
	//     per-item + per-day caps) exists for.
	blockClass := ClassInfra
	if decision.FailClosed {
		blockClass = ClassConfig
	}
	reason := fmt.Sprintf("infrastructure health gates blocked pipeline [class=%s]", blockClass)
	if len(decision.Reasons) > 0 {
		reason += ": " + strings.Join(decision.Reasons, "; ")
	}
	if decision.FailClosed {
		reason += " (fail-closed)"
	}
	r.event(ctx, "pipeline.preflight.health_gates", "fail", map[string]any{
		"run": run.ID, "backlog_id": item.ID, "status": decision.Status,
		"fail_closed": decision.FailClosed, "reasons": decision.Reasons,
	})
	return true, r.escalateWithItem(ctx, run, item, blockClass, reason)
}
