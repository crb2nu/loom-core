package mills

import (
	"context"
	"time"

	"github.com/crb2nu/loom/pkg/mills/store"
)

// storeBudgetReader adapts a *store.Store to the BudgetReader interface.
// Defined here (not in pkg/mills/store) so the store package stays free of
// mills-side abstractions and so the BudgetReader interface lives next to
// its only consumer.
type storeBudgetReader struct {
	st *store.Store
}

// NewStoreBudgetReader returns a BudgetReader backed by the given canonical
// store. Used by the operator's wiring code to plug the real DAOs into the
// budget enforcer.
func NewStoreBudgetReader(st *store.Store) BudgetReader {
	return &storeBudgetReader{st: st}
}

func (r *storeBudgetReader) CouncilCostSince(ctx context.Context, since time.Time) (float64, error) {
	return r.st.Council.SumCostSince(ctx, since)
}

func (r *storeBudgetReader) PipelineCostSince(ctx context.Context, since time.Time) (float64, error) {
	return r.st.Pipeline.SumCostSince(ctx, since)
}

func (r *storeBudgetReader) CouncilRunsSince(ctx context.Context, since time.Time) (int, error) {
	return r.st.Council.CountSince(ctx, since)
}

func (r *storeBudgetReader) PipelineRunsSince(ctx context.Context, since time.Time) (int, error) {
	// CountBudgetedSince, not CountSince: escalations that cannot recur
	// without human action — no-op capacity escalations ($0 transient_quota,
	// the 2026-07-02 spawn-pool wedge) and terminal config escalations (the
	// 2026-07-07 scope-gate storm) — must not consume the daily MaxRunsPerDay
	// budget. A burst of either otherwise DoSes autonomy for a full day.
	// Real (code/infra/transient) failures still count.
	return r.st.Pipeline.CountBudgetedSince(ctx, since)
}

func (r *storeBudgetReader) PipelineActiveRuns(ctx context.Context) (int, error) {
	return r.st.Pipeline.CountActive(ctx)
}

func (r *storeBudgetReader) DebateCostSince(ctx context.Context, since time.Time) (float64, error) {
	return r.st.Debate.SumCostSince(ctx, since)
}
