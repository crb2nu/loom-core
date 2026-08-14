package pipeline

import (
	"context"
	"fmt"
	"strings"

	"github.com/crb2nu/loom/pkg/mills/council"
	"github.com/crb2nu/loom/pkg/mills/store"
)

// DegradedPolicyFunc is the pipeline-facing degraded-mode evaluator. It is
// checked before each stage so embedder/provider degradation can either proceed
// under an explicit degraded verdict or stop before autonomous writes continue.
type DegradedPolicyFunc func(ctx context.Context, run *store.PipelineRun, item *store.BacklogItem, stage Stage) council.DegradedModeDecision

// DegradedPolicyFromSignals adapts a signal source into the pipeline callback.
func DegradedPolicyFromSignals(policy council.DegradedModePolicy, source func(context.Context) []council.DegradedModeSignal) DegradedPolicyFunc {
	if source == nil {
		return nil
	}
	return func(ctx context.Context, _ *store.PipelineRun, _ *store.BacklogItem, _ Stage) council.DegradedModeDecision {
		return council.EvaluateDegradedModePolicy(council.DegradedModePolicyInput{
			Policy:  policy,
			Signals: source(ctx),
		})
	}
}

func (r *Runner) enforceDegradedPolicy(ctx context.Context, run *store.PipelineRun, item *store.BacklogItem, stage Stage) (bool, error) {
	if r.DegradedPolicy == nil {
		return true, nil
	}
	decision := council.NormalizeDegradedModeDecision(r.DegradedPolicy(ctx, run, item, stage))
	if decision.Allowed {
		if decision.Mode == council.DegradedPolicyModeDegraded {
			r.event(ctx, "pipeline.degraded_policy", "degraded", map[string]any{
				"run": run.ID, "stage": stage.ID, "reason_code": decision.Code,
				"reasons": decision.Reasons, "fallback_used": decision.FallbackUsed,
			})
		}
		return true, nil
	}
	code := strings.TrimSpace(decision.Code)
	if code == "" {
		code = council.DegradedPolicyCodeEmbedderUnavailable
	}
	r.event(ctx, "pipeline.degraded_policy", "fail", map[string]any{
		"run": run.ID, "stage": stage.ID, "reason_code": code,
		"reasons": decision.Reasons, "blockers": decision.Blockers,
		"fallback_used": decision.FallbackUsed,
	})
	// A degraded-mode block is an operator POLICY decision (a dependency the
	// policy declared degraded), not a defect in the diff: mark it config so it
	// stops persisting as an unclassified escalation. The dependency-recovery
	// path — not auto-requeue — is what clears these.
	reason := fmt.Sprintf("degraded-mode policy blocked before stage %s [class=%s] [reason_code=%s]",
		stage.ID, ClassConfig, code)
	if len(decision.Blockers) > 0 {
		reason += ": " + strings.Join(decision.Blockers, "; ")
	} else if len(decision.Reasons) > 0 {
		reason += ": " + strings.Join(decision.Reasons, "; ")
	}
	if err := r.escalateWithItem(ctx, run, item, ClassConfig, reason); err != nil {
		return false, err
	}
	return false, nil
}
