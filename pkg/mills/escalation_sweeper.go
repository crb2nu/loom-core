package mills

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

const (
	defaultEscalationSweepInterval = 5 * time.Minute
	defaultEscalationSweepBudget   = 60 * time.Second
)

// EscalationSweeper owns the slow GitLab escalation reconciliation formerly
// embedded in Reconciler.Tick. Passes are serialized, so the reconciler's
// sweep caches need no synchronization.
type EscalationSweeper struct {
	Reconciler *Reconciler
	Policy     *PolicyManager
	Enabled    func() bool
	Logger     *slog.Logger
	Interval   time.Duration
	Budget     time.Duration
}

func NewEscalationSweeper(r *Reconciler, pm *PolicyManager) *EscalationSweeper {
	return &EscalationSweeper{Reconciler: r, Policy: pm}
}

func (s *EscalationSweeper) Run(ctx context.Context) error {
	if s == nil || s.Reconciler == nil || s.Policy == nil {
		<-ctx.Done()
		return nil
	}
	interval := s.Interval
	if interval <= 0 {
		interval = defaultEscalationSweepInterval
	}
	s.runPass(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			s.runPass(ctx)
		}
	}
}

func (s *EscalationSweeper) runPass(parent context.Context) {
	if s.Enabled != nil && !s.Enabled() {
		return
	}
	policy := s.Policy.Current()
	if policy == nil || !policy.IsEnabled() {
		return
	}
	budget := s.Budget
	if budget <= 0 {
		budget = defaultEscalationSweepBudget
	}
	passCtx, cancel := context.WithTimeout(parent, budget)
	defer cancel()
	started := time.Now()
	// Reserve the final third of the pass for auto-requeue. SweepGhostSparks
	// further divides its allocation between IID and branch lookups, so neither
	// escalation phase can starve a later phase by exhausting the shared pass.
	ghostCtx, ghostCancel := context.WithTimeout(passCtx, budget*2/3)
	ghost, ghostErr := s.Reconciler.SweepGhostSparks(ghostCtx)
	ghostCancel()

	// Deliberately unconditional and independently budgeted: ordering is ghost
	// then auto-requeue, but a ghost failure or sub-deadline must not starve
	// retry admission. The outer pass deadline still caps total wall time.
	autoCtx, autoCancel := context.WithTimeout(passCtx, budget/3)
	auto, autoErr := s.Reconciler.SweepAutoRequeue(autoCtx)
	autoCancel()
	duration := time.Since(started)
	passErr := passCtx.Err()
	EscalationSweepDurationSeconds.Observe(duration.Seconds())
	// Inspected already includes both IID and branch calls; BranchInspected is
	// the diagnostic subset and must not be added a second time.
	EscalationSweepLookups.Observe(float64(ghost.Inspected))
	timedOut := errors.Is(ghostErr, context.DeadlineExceeded) || errors.Is(autoErr, context.DeadlineExceeded) || errors.Is(passErr, context.DeadlineExceeded)
	if timedOut {
		EscalationSweepTimeoutsTotal.Inc()
	}
	outcome := "ok"
	if timedOut {
		outcome = "timeout"
	} else if ghostErr != nil || autoErr != nil {
		outcome = "error"
	}
	payload := map[string]any{
		"duration_seconds": duration.Seconds(), "ghost_inspected": ghost.Inspected,
		"ghost_branch_inspected": ghost.BranchInspected, "ghost_merged": ghost.Merged,
		"ghost_mr_closed": ghost.MRClosed, "ghost_errored": ghost.Errored,
		"auto_requeue_inspected": auto.Inspected, "auto_requeued": auto.Requeued,
		"auto_requeue_skipped": auto.Skipped, "auto_requeue_errored": auto.Errored,
	}
	if ghostErr != nil {
		payload["ghost_error"] = ghostErr.Error()
	}
	if autoErr != nil {
		payload["auto_requeue_error"] = autoErr.Error()
	}
	s.Reconciler.append(parent, "reconciler.escalation_sweep", outcome, payload)
}
