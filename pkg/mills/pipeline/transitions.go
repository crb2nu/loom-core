package pipeline

import (
	"context"
	"fmt"

	"github.com/crb2nu/loom/pkg/mills/spin"
	"github.com/crb2nu/loom/pkg/mills/store"
)

const preflightFailedStage = "preflight_failed"

type implementPreflighter interface {
	PreflightImplement(context.Context, *store.PipelineRun, *store.BacklogItem, Stage, map[string]StageOutput) spin.PreflightResult
}

func (r *Runner) runImplementPreflight(ctx context.Context, run *store.PipelineRun, item *store.BacklogItem, stage Stage, prior map[string]StageOutput) (bool, error) {
	if stage.ID != "implement" {
		return false, nil
	}
	p, ok := r.Dispatcher.(implementPreflighter)
	if !ok {
		return false, nil
	}
	result := p.PreflightImplement(ctx, run, item, stage, prior)
	if result.OK {
		r.event(ctx, "pipeline.implement.preflight", "ok", map[string]any{"run": run.ID, "reason_code": "ok"})
		return false, nil
	}
	reason := fmt.Sprintf("implement invocation preflight failed [class=%s] [reason=%s]: %s", ClassConfig, result.Reason, result.Detail)
	run.CurrentStage = preflightFailedStage
	if err := r.escalateWithItem(ctx, run, item, ClassConfig, reason); err != nil {
		return true, err
	}
	run.State = store.PipelinePreflightFailed
	if err := r.Store.Pipeline.PutRun(ctx, run); err != nil {
		return true, fmt.Errorf("persist preflight_failed state: %w", err)
	}
	r.event(ctx, "pipeline.implement.preflight", "preflight_failed", map[string]any{
		"run": run.ID, "reason_code": string(result.Reason), "detail": result.Detail,
		"missing_credentials": result.Missing,
	})
	return true, nil
}
