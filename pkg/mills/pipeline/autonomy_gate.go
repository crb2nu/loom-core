package pipeline

import (
	"context"
	"fmt"
	"strings"

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
	if decision.Allowed {
		return true, nil
	}
	code := strings.TrimSpace(decision.Code)
	if code == "" {
		code = council.AutonomyReasonBlocked
	}
	r.event(ctx, "pipeline.autonomy_blocked", "error", map[string]any{
		"run": run.ID, "stage": stage.ID, "reason_code": code, "blockers": decision.Blockers,
	})
	// An autonomy block is an operator POLICY decision, not a defect in the
	// diff: mark it config (terminal, human signal, never auto-requeued) so it
	// stops persisting as an unclassified escalation. Without the marker the
	// unmarked-escalation fallback would read this prose as ClassCode.
	reason := fmt.Sprintf("autonomy circuit breaker blocked before stage %s [class=%s] [reason_code=%s]",
		stage.ID, ClassConfig, code)
	if len(decision.Blockers) > 0 {
		reason += ": " + strings.Join(decision.Blockers, "; ")
	}
	if err := r.escalateWithItem(ctx, run, item, ClassConfig, reason); err != nil {
		return false, err
	}
	return false, nil
}
