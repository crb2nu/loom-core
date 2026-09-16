package pipeline

import (
	"context"
	"fmt"
	"strings"

	"github.com/crb2nu/loom/pkg/mills/council"
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
	if recoveringResume(ctx) && ((err != nil && hubUnavailable(err.Error())) || (!decision.Allowed && onlyHubUnavailable(decision.Reasons))) {
		// The stage records and budgets this outage as an explicit attempt.
		return false, nil
	}
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

// Hub readiness is deliberately narrow: unrelated policy/configuration blocks
// must not be converted into rollout retries.
func hubUnavailable(s string) bool {
	s = strings.ToLower(s)
	hub := strings.Contains(s, "mcp_hub_session") || strings.Contains(s, "mcphub") || strings.Contains(s, "mcp hub") || strings.Contains(s, "mcp-hub") || strings.Contains(s, "hub session")
	return hub && (strings.Contains(s, "unavailable") || strings.Contains(s, "connection refused") || strings.Contains(s, "connection reset") || strings.Contains(s, "transport closed") || strings.Contains(s, "broken pipe"))
}

func onlyHubUnavailable(reasons []string) bool {
	if len(reasons) == 0 {
		return false
	}
	for _, reason := range reasons {
		if !hubUnavailable(reason) {
			return false
		}
	}
	return true
}

type resumeRecoveryKey struct{}

func recoveringResume(ctx context.Context) bool {
	v, _ := ctx.Value(resumeRecoveryKey{}).(bool)
	return v
}

func (r *Runner) resumeHubReadiness(ctx context.Context, run *store.PipelineRun, item *store.BacklogItem, stage Stage) error {
	if !recoveringResume(ctx) {
		return nil
	}
	if r.HealthGates != nil {
		decision, err := r.HealthGates.DecideHealthGates(ctx)
		if err != nil && hubUnavailable(err.Error()) {
			return err
		}
		if !decision.Allowed && onlyHubUnavailable(decision.Reasons) {
			return fmt.Errorf("MCP hub unavailable after operator rollout: %s", strings.Join(decision.Reasons, "; "))
		}
		if err != nil || !decision.Allowed {
			cls := ClassInfra
			if err != nil || decision.FailClosed {
				cls = ClassConfig
			}
			if e := r.escalateWithItemPolicy(ctx, run, item, cls, fmt.Sprintf("resume health policy blocked: %v %s", err, strings.Join(decision.Reasons, "; ")), false); e != nil {
				return e
			}
			return errRunTerminated
		}
	}
	if r.AutonomyGate != nil {
		decision := council.NormalizeAutonomyDecision(r.AutonomyGate(ctx, run, item, stage))
		if !decision.Allowed && decision.Code == "capability_red" && onlyHubUnavailable(decision.Blockers) {
			return fmt.Errorf("MCP hub unavailable after operator rollout: %s", strings.Join(decision.Blockers, "; "))
		}
		if !decision.Allowed {
			if e := r.escalateWithItemPolicy(ctx, run, item, ClassConfig, "resume autonomy policy blocked: "+strings.Join(decision.Blockers, "; "), false); e != nil {
				return e
			}
			return errRunTerminated
		}
	}
	return nil
}
