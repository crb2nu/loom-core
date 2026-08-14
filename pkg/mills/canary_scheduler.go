package mills

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// CanaryRunFn is the side-effect the autopilot fires on a schedule match. The
// operator wires this to a closure in main.go that builds the heartbeat
// backlog item, applies the 24h canary dedupe, persists it, and hands it to
// the reconciler's StartQueuedItem — the same enqueue+start the
// `loom mills pipelines canary` CLI performs over HTTP, but in-process.
//
// reason is a short trigger label ("autopilot") threaded into logs/audit.
type CanaryRunFn func(ctx context.Context, reason string) error

// CanaryScheduler is the wall-clock side of the heartbeat-canary autopilot
// (.loom/126 Wave 1 / A3-sustain). It wakes every minute, checks the policy's
// CanaryAutopilotCron against the current UTC minute, and fires the run fn on
// match. It is the automation that lets autonomous_merges_24h tick ≥1/day
// without a human running the canary CLI — the gap that dropped the loop to 0
// merges on 2026-06-26.
//
// Design deliberately mirrors CouncilScheduler so the two read identically:
//   - The schedule + enabled flag are read from policy on every tick, so a
//     ConfigMap hot-reload (enable, retime) takes effect on the next minute
//     without restarting the operator.
//   - The kill switch (policy.enabled=false) AND the autopilot's own
//     CanaryAutopilotEnabled gate are honored on every tick — default-OFF, so
//     an operator that hasn't opted in stays inert.
//   - The run fn can touch the store + reconciler; it runs in a fire-and-forget
//     goroutine so a slow enqueue can't wedge the per-minute loop.
//   - Per-minute de-dup (lastFiredMinute) prevents a clock skip from
//     double-firing inside one minute. The run fn additionally applies the
//     backlog-level 24h canary dedupe, so even a missed-minute replay cannot
//     enqueue two canaries in a window.
//   - cronMatches (council_scheduler.go) is reused — same narrow 5-field
//     "*", int, "*/N" grammar that covers the daily "0 9 * * *" default.
type CanaryScheduler struct {
	// RunFn is invoked when the schedule matches. Nil makes the scheduler a
	// benign no-op (e.g. when the operator lacks a reconciler).
	RunFn  CanaryRunFn
	Policy *PolicyManager
	Logger *slog.Logger
	// Enabled is an additional live admission fence (for example a crash
	// lease). Nil preserves the policy-only scheduler contract.
	Enabled func() bool

	// Now is injectable for tests so the wall-clock check is deterministic.
	// Defaults to time.Now.
	Now func() time.Time

	// Interval is the tick cadence. Zero falls back to one minute. Tests set a
	// much smaller value paired with a controlled Now.
	Interval time.Duration

	mu              sync.Mutex
	lastFiredMinute time.Time
	warnedBadCron   string
	active          atomic.Int64
}

// NewCanaryScheduler returns a scheduler wired to a run fn + policy. Either
// argument may be nil; the resulting scheduler is a no-op that waits for
// ctx.Done so the errgroup stays balanced.
func NewCanaryScheduler(fn CanaryRunFn, pm *PolicyManager) *CanaryScheduler {
	return &CanaryScheduler{RunFn: fn, Policy: pm}
}

// Run drives the per-minute check until ctx is cancelled. Returns nil on clean
// shutdown so it composes with errgroup.WithContext alongside the other mills
// schedulers in the operator.
func (s *CanaryScheduler) Run(ctx context.Context) error {
	if s == nil || s.RunFn == nil || s.Policy == nil {
		<-ctx.Done()
		return nil
	}
	if s.Logger != nil {
		s.Logger.Info("canary autopilot scheduler armed",
			"enabled", s.Policy.Current().CanaryAutopilotEnabled(),
			"schedule", s.scheduleExpr(),
		)
	}
	interval := s.Interval
	if interval <= 0 {
		interval = time.Minute
	}
	// Initial check so a pod that restarts on a scheduled minute still fires
	// that minute rather than skipping a whole window.
	s.maybeFire(ctx)

	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			s.maybeFire(ctx)
		}
	}
}

// maybeFire fires the run fn in a goroutine iff the autopilot is enabled, the
// kill switch is off, the current UTC minute matches the schedule, and the same
// minute hasn't already fired this process.
func (s *CanaryScheduler) maybeFire(ctx context.Context) {
	// Publish activity before the first policy read. This closes the stale-gate
	// window where admission flips off while a pre-gate call is descheduled.
	s.active.Add(1)
	handedOff := false
	defer func() {
		if !handedOff {
			s.active.Add(-1)
		}
	}()
	if s.Enabled != nil && !s.Enabled() {
		return
	}
	pol := s.Policy.Current()
	if pol == nil || !pol.IsEnabled() {
		return
	}
	if !pol.CanaryAutopilotEnabled() {
		return
	}
	expr := strings.TrimSpace(pol.CanaryAutopilotCron())
	if expr == "" {
		return
	}
	now := s.now().UTC().Truncate(time.Minute)
	matches, err := cronMatches(expr, now)
	if err != nil {
		s.warnBadCron(expr, err)
		return
	}
	if !matches {
		return
	}

	s.mu.Lock()
	if !s.lastFiredMinute.Before(now) {
		s.mu.Unlock()
		return
	}
	s.lastFiredMinute = now
	s.mu.Unlock()

	if s.Logger != nil {
		s.Logger.Info("canary autopilot firing", "trigger", "cron", "at", now.Format(time.RFC3339))
	}
	// The run fn enqueues + starts a pipeline (store writes + reconciler
	// dispatch). Detach so the scheduler loop stays on its per-minute cadence
	// and a single slow enqueue can't wedge the loop. The detached context is
	// rooted at the scheduler's ctx so shutdown still propagates.
	// Publish activity before launching so the admission barrier cannot observe
	// a zero in the scheduler-to-goroutine handoff window.
	handedOff = true
	go s.fire(ctx)
}

func (s *CanaryScheduler) fire(ctx context.Context) {
	defer s.active.Add(-1)
	if err := s.RunFn(ctx, "autopilot"); err != nil {
		if s.Logger != nil {
			s.Logger.Warn("scheduled canary autopilot run failed", "error", err)
		}
	}
}

// ActiveOperations reports heartbeat-canary admissions currently executing.
func (s *CanaryScheduler) ActiveOperations() int64 {
	if s == nil {
		return 0
	}
	return s.active.Load()
}

func (s *CanaryScheduler) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *CanaryScheduler) scheduleExpr() string {
	if pol := s.Policy.Current(); pol != nil {
		return pol.CanaryAutopilotCron()
	}
	return ""
}

func (s *CanaryScheduler) warnBadCron(expr string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.warnedBadCron == expr {
		return
	}
	s.warnedBadCron = expr
	if s.Logger != nil {
		s.Logger.Warn("canary autopilot schedule_cron unparseable; scheduler idle until fixed",
			"schedule", expr, "error", err)
	}
}
