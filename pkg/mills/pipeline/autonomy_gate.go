package pipeline

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/council"
	"github.com/crb2nu/loom/pkg/mills/store"
)

// AutonomyGateFunc is the pipeline-facing circuit breaker. It is checked before
// each autonomous stage so an already-running pipeline cannot continue toward
// MR creation, CI watch, or merge after the operator becomes blocked.
type AutonomyGateFunc func(ctx context.Context, run *store.PipelineRun, item *store.BacklogItem, stage Stage) council.AutonomyGateDecision

// AutonomyGateFromCouncil adapts a council AutonomyGate into the richer
// pipeline callback shape.
func AutonomyGateFromCouncil(g council.AutonomyGate) AutonomyGateFunc {
	if g == nil {
		return nil
	}
	return func(ctx context.Context, _ *store.PipelineRun, _ *store.BacklogItem, _ Stage) council.AutonomyGateDecision {
		return council.NormalizeAutonomyDecision(g.CheckAutonomy(ctx))
	}
}

func (r *Runner) enforceAutonomy(ctx context.Context, run *store.PipelineRun, item *store.BacklogItem, stage Stage) (bool, error) {
	if r.AutonomyGate == nil {
		return true, nil
	}
	decision := council.NormalizeAutonomyDecision(r.AutonomyGate(ctx, run, item, stage))
	if decision.Allowed || (recoveringResume(ctx) && decision.Code == "capability_red" && onlyHubUnavailable(decision.Blockers)) {
		return true, nil
	}
	// A capability_red whose every blocker is a transport failure (hub DNS
	// blip, connection refused, EOF) is a substrate wobble, not an operator
	// policy decision: HOLD the run inside a bounded window and re-check the
	// gate with backoff instead of escalating it as terminal config. Completed
	// stages, the run state, and the attempt count are untouched while it
	// waits. When the window expires the run escalates as retryable
	// infrastructure so auto-requeue can pick it up; when the blockers turn
	// into configuration failures the hold ends and the config escalation
	// below applies unchanged.
	cls := ClassConfig
	if capabilities := decision.TransientCapabilities(); len(capabilities) > 0 {
		mills.AutonomyBreakerHeldRuns.Inc()
		defer mills.AutonomyBreakerHeldRuns.Dec()
		for _, capability := range capabilities {
			mills.AutonomyBreakerHoldsTotal.WithLabelValues(capability).Inc()
		}
		r.event(ctx, "pipeline.stage.held", "warn", map[string]any{
			"run": run.ID, "stage": stage.ID, "reason_code": decision.Code,
			"capabilities": capabilities, "blockers": decision.Blockers, "class": ClassSubstrate,
			"reason": strings.Join(decision.Blockers, "; "),
		})
		deadline := r.now().Add(r.policy().Pipeline.AutonomyHoldDuration())
		wait := r.AutonomyWait
		if wait == nil {
			wait = r.waitRetry
		}
		for delay := time.Minute; ; delay *= 2 {
			if delay > 5*time.Minute {
				delay = 5 * time.Minute
			}
			if err := ctx.Err(); err != nil {
				return false, err
			}
			remaining := deadline.Sub(r.now())
			if remaining <= 0 {
				cls = ClassInfra
				break
			}
			sleep := delay
			if remaining < sleep {
				sleep = remaining
			}
			if err := wait(ctx, sleep); err != nil {
				return false, err
			}
			if err := ctx.Err(); err != nil {
				return false, err
			}
			// Every poll is observed runner activity: the silent-run watchdog
			// (SweepSilentRuns) must not expire a run that is legitimately
			// waiting inside its hold window.
			r.touchActivity(run.ID)
			// A manual stop during a hold must remain durable after recovery.
			if terminal, err := r.runTerminatedExternally(ctx, run); err != nil {
				return false, err
			} else if terminal {
				return false, nil
			}
			decision = council.NormalizeAutonomyDecision(r.AutonomyGate(ctx, run, item, stage))
			if decision.Allowed || len(decision.TransientCapabilities()) == 0 {
				break
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if decision.Allowed {
		return true, nil
	}
	code := strings.TrimSpace(decision.Code)
	if code == "" {
		code = council.AutonomyReasonBlocked
	}
	r.event(ctx, "pipeline.autonomy_blocked", "error", map[string]any{
		"run": run.ID, "stage": stage.ID, "reason_code": code, "blockers": decision.Blockers, "class": cls,
	})
	// An autonomy block is an operator POLICY decision, not a defect in the
	// diff: mark it config (terminal, human signal, never auto-requeued) so it
	// stops persisting as an unclassified escalation. Without the marker the
	// unmarked-escalation fallback would read this prose as ClassCode. The one
	// exception is an expired transient hold, which is retryable infra.
	reason := fmt.Sprintf("autonomy circuit breaker blocked before stage %s [class=%s] [reason_code=%s]",
		stage.ID, cls, code)
	if cls == ClassInfra {
		reason += " (transient capability hold window expired)"
	}
	if len(decision.Blockers) > 0 {
		reason += ": " + strings.Join(decision.Blockers, "; ")
	}
	if err := r.escalateWithItem(ctx, run, item, cls, reason); err != nil {
		return false, err
	}
	return false, nil
}
