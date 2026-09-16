package mills

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// BootPhase is a one-shot latch that orders the operator's boot-time first
// passes. Every loop in the operator used to fire its first pass in the same
// few milliseconds after boot; on a cold page cache the single SQLite store
// then served the report-rollup warm-up (~30s of window scans), the
// reconciler's boot tick, the escalation sweep and the intake ticks all at
// once, and the loops with the shortest budgets lost — the 2026-09-07/08
// boot bursts (auto-requeue budget expired with candidates unjudged, boot
// tick dead at its 30s deadline, ledger rows lost to the same dead contexts).
//
// A phase is released by the loop that owns it once its first pass returns;
// later loops Gate their own first pass behind it. Every wait is budgeted:
// ordering is a boot optimisation, never a liveness dependency, so a wedged
// or missing phase logs a warning and the waiter proceeds.
type BootPhase struct {
	name    string
	logger  *slog.Logger
	created time.Time
	once    sync.Once
	done    chan struct{}
}

// NewBootPhase returns an unreleased phase. A nil logger is allowed.
func NewBootPhase(name string, logger *slog.Logger) *BootPhase {
	return &BootPhase{name: name, logger: logger, created: time.Now(), done: make(chan struct{})}
}

// Release marks the phase complete. Idempotent and nil-safe.
func (p *BootPhase) Release() {
	if p == nil {
		return
	}
	p.once.Do(func() {
		close(p.done)
		if p.logger != nil {
			p.logger.Info("boot phase released", "phase", p.name, "elapsed", time.Since(p.created).String())
		}
	})
}

// Done returns a channel that is closed once the phase is released. A nil
// phase is treated as already released.
func (p *BootPhase) Done() <-chan struct{} {
	if p == nil {
		closed := make(chan struct{})
		close(closed)
		return closed
	}
	return p.done
}

// Wait blocks until the phase is released, budget elapses, or ctx is done,
// and reports whether the phase was actually released. A budget expiry logs
// a warning; a non-positive budget waits without one.
func (p *BootPhase) Wait(ctx context.Context, budget time.Duration) bool {
	if p == nil {
		return true
	}
	var expiry <-chan time.Time
	if budget > 0 {
		timer := time.NewTimer(budget)
		defer timer.Stop()
		expiry = timer.C
	}
	select {
	case <-p.done:
		return true
	case <-ctx.Done():
		return false
	case <-expiry:
		if p.logger != nil {
			p.logger.Warn("boot phase not released within budget; proceeding without it",
				"phase", p.name, "budget", budget.String())
		}
		return false
	}
}

// Gate runs loop once the phase is released or budget elapses. A ctx that
// ends while waiting returns nil without running loop, matching the
// clean-shutdown contract of the loops it wraps.
func (p *BootPhase) Gate(ctx context.Context, budget time.Duration, loop func(context.Context) error) error {
	p.Wait(ctx, budget)
	if err := ctx.Err(); err != nil {
		return nil
	}
	return loop(ctx)
}
