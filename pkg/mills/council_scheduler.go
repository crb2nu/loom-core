package mills

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/crb2nu/loom/pkg/mills/store"
)

// CouncilRunFn is the side-effect the scheduler fires on schedule match.
// The operator wires this to runner.Runner.Run via a closure in main.go,
// keeping the scheduler independent of the runner package and breaking
// what would otherwise be a pkg/mills ↔ pkg/mills/runner import cycle.
type CouncilRunFn func(ctx context.Context, trigger store.CouncilTrigger, reason string) error

// CouncilScheduler is the wall-clock side of the council. It wakes every
// minute, checks the policy's Council.ScheduleCron against the current
// UTC time, and fires runner.Run on match. De-duplication is keyed on the
// minute so two ticks inside the same minute (after a clock skip, say)
// cannot double-fire.
//
// Design notes mirror the reconciler scheduler:
//   - The schedule is read from policy on every tick so a hot-reload to
//     policy.council.schedule_cron takes effect on the next minute
//     without restarting the operator.
//   - Kill switch (policy.enabled=false) is honored on every tick. A
//     paused operator stays paused regardless of schedule.
//   - runner.Run can block for several minutes. It runs in a fire-and-
//     forget goroutine so the scheduler loop stays on a 60s cadence.
//   - The cron parser is a minimal 5-field implementation supporting
//     "*", fixed integers, and "*/N" steps. That covers every default
//     this codebase ships and the two patterns documented in the
//     Mills runbook ("0 H * * *" and "0 */N * * *"). Anything more
//     exotic (ranges, enums, named days) would require pulling in a
//     real cron library; punt until we need it.
type CouncilScheduler struct {
	// RunFn is invoked when the schedule matches. The operator's main.go
	// wires it to runner.Runner.Run via a closure. Nil makes the
	// scheduler a benign no-op.
	RunFn  CouncilRunFn
	Policy *PolicyManager
	Logger *slog.Logger
	// Enabled is an additional live admission fence (for example a crash
	// lease). Nil preserves the policy-only scheduler contract.
	Enabled func() bool

	// Now is injectable for tests so the scheduler's wall-clock check
	// can be driven deterministically. Defaults to time.Now.
	Now func() time.Time

	// Interval is the tick cadence. Zero falls back to one minute. Tests
	// set this to a much smaller value paired with a controlled Now.
	Interval time.Duration

	// CatchUpGrace bounds how far back Run looks for a cron slot the process
	// missed while it was not running (rollouts land at HH:59–HH:01 often
	// enough that "0 */6 * * *" was skipped outright on 2026-09-02: the pod
	// stopped 17:59:32 and the scheduler re-armed 18:01:04, 64s past the
	// slot, and the initial check only covers the CURRENT minute). Zero
	// uses DefaultCouncilCatchUpGrace; negative disables catch-up.
	CatchUpGrace time.Duration
	// RanSince reports whether a council run already started at or after
	// the given time — the previous process may have fired the slot before
	// it died. Nil means "unknown", which fails open to firing (a duplicate
	// ~$0.70 pass beats a silently skipped six-hour window).
	RanSince func(ctx context.Context, since time.Time) (bool, error)

	mu              sync.Mutex
	lastFiredMinute time.Time
	warnedBadCron   string
	active          atomic.Int64
}

// DefaultCouncilCatchUpGrace is how far back a starting scheduler looks for
// a missed cron slot. Fifteen minutes covers an image roll plus boot.
const DefaultCouncilCatchUpGrace = 15 * time.Minute

// NewCouncilScheduler returns a scheduler wired to a run function + policy.
// Either argument may be nil; the resulting scheduler is a no-op that
// waits for ctx.Done so the errgroup stays balanced.
func NewCouncilScheduler(fn CouncilRunFn, pm *PolicyManager) *CouncilScheduler {
	return &CouncilScheduler{RunFn: fn, Policy: pm}
}

// Run drives the per-minute check until ctx is cancelled. Returns nil on
// clean shutdown so it composes with errgroup.WithContext alongside the
// reconciler scheduler in the operator.
func (s *CouncilScheduler) Run(ctx context.Context) error {
	if s == nil || s.RunFn == nil || s.Policy == nil {
		<-ctx.Done()
		return nil
	}
	if s.Logger != nil {
		s.Logger.Info("council scheduler armed",
			"schedule", s.scheduleExpr(),
		)
	}
	interval := s.Interval
	if interval <= 0 {
		interval = time.Minute
	}
	// Initial check so a pod that restarts on a scheduled minute still
	// fires that minute rather than skipping a whole window — and a
	// catch-up for a slot that fell inside the restart gap.
	s.maybeFire(ctx)
	s.catchUp(ctx)

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

// maybeFire fires runner.Run in a goroutine iff the current UTC minute
// matches the policy's schedule, the kill switch is off, and the same
// minute hasn't already fired this process.
func (s *CouncilScheduler) maybeFire(ctx context.Context) {
	// Cover the whole policy/schedule decision, not just the detached RunFn.
	// A policy reload may otherwise close admission after the first read while
	// this goroutine is descheduled, letting quiescence observe a false zero.
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
	expr := strings.TrimSpace(pol.Council.ScheduleCron)
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
		s.Logger.Info("council scheduler firing", "trigger", "cron", "at", now.Format(time.RFC3339))
	}
	// runner.Run blocks for the duration of the council pass (FlexInfer
	// calls, judge, mutation). Detach so the scheduler loop stays on its
	// per-minute cadence and so a single hung run can't wedge the loop.
	// The detached context is rooted at the scheduler's ctx so shutdown
	// still propagates.
	// Publish activity before launching. If the goroutine were responsible for
	// its own increment, quiescence could sample zero after launch but before
	// the runtime scheduled it.
	handedOff = true
	go s.fire(ctx)
}

// catchUp fires once for the most recent cron slot inside CatchUpGrace that
// this process did not fire and no run already covered. Runs only at Run
// start: a slot missed by a rollout gap, not by policy (the kill switch and
// enabled gates still apply through the same checks maybeFire makes).
func (s *CouncilScheduler) catchUp(ctx context.Context) {
	grace := s.CatchUpGrace
	if grace == 0 {
		grace = DefaultCouncilCatchUpGrace
	}
	if grace < 0 {
		return
	}
	if s.Enabled != nil && !s.Enabled() {
		return
	}
	pol := s.Policy.Current()
	if pol == nil || !pol.IsEnabled() {
		return
	}
	expr := strings.TrimSpace(pol.Council.ScheduleCron)
	if expr == "" {
		return
	}
	now := s.now().UTC().Truncate(time.Minute)
	var slot time.Time
	for back := time.Minute; back <= grace; back += time.Minute {
		candidate := now.Add(-back)
		matches, err := cronMatches(expr, candidate)
		if err != nil {
			s.warnBadCron(expr, err)
			return
		}
		if matches {
			slot = candidate
			break
		}
	}
	if slot.IsZero() {
		return
	}
	s.mu.Lock()
	alreadyFired := !s.lastFiredMinute.Before(slot)
	s.mu.Unlock()
	if alreadyFired {
		return
	}
	if s.RanSince != nil {
		ran, err := s.RanSince(ctx, slot)
		if err != nil && s.Logger != nil {
			s.Logger.Warn("council catch-up: could not check for a prior run; firing", "slot", slot.Format(time.RFC3339), "error", err)
		}
		if err == nil && ran {
			return
		}
	}
	s.mu.Lock()
	s.lastFiredMinute = now
	s.mu.Unlock()
	if s.Logger != nil {
		s.Logger.Info("council scheduler firing missed slot", "trigger", "cron", "slot", slot.Format(time.RFC3339), "at", now.Format(time.RFC3339))
	}
	s.active.Add(1)
	go func() {
		defer s.active.Add(-1)
		if err := s.RunFn(ctx, store.CouncilTriggerCron, "scheduler catch-up: missed slot "+slot.Format(time.RFC3339)); err != nil && s.Logger != nil {
			s.Logger.Warn("catch-up council run failed", "error", err)
		}
	}()
}

func (s *CouncilScheduler) fire(ctx context.Context) {
	defer s.active.Add(-1)
	if err := s.RunFn(ctx, store.CouncilTriggerCron, "scheduler"); err != nil {
		if s.Logger != nil {
			s.Logger.Warn("scheduled council run failed", "error", err)
		}
	}
}

// ActiveOperations reports scheduled council runs currently executing.
func (s *CouncilScheduler) ActiveOperations() int64 {
	if s == nil {
		return 0
	}
	return s.active.Load()
}

func (s *CouncilScheduler) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *CouncilScheduler) scheduleExpr() string {
	if pol := s.Policy.Current(); pol != nil {
		return pol.Council.ScheduleCron
	}
	return ""
}

func (s *CouncilScheduler) warnBadCron(expr string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.warnedBadCron == expr {
		return
	}
	s.warnedBadCron = expr
	if s.Logger != nil {
		s.Logger.Warn("council schedule_cron unparseable; scheduler idle until fixed",
			"schedule", expr, "error", err)
	}
}

// cronMatches reports whether the 5-field cron expression matches the
// given time at minute resolution. The supported syntax is intentionally
// narrow — see the type doc for rationale.
func cronMatches(expr string, t time.Time) (bool, error) {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return false, &cronParseError{expr: expr, reason: "expected 5 space-separated fields"}
	}
	type fieldCheck struct {
		raw         string
		val, lo, hi int
	}
	checks := []fieldCheck{
		{fields[0], t.Minute(), 0, 59},
		{fields[1], t.Hour(), 0, 23},
		{fields[2], t.Day(), 1, 31},
		{fields[3], int(t.Month()), 1, 12},
		{fields[4], int(t.Weekday()), 0, 6},
	}
	for _, c := range checks {
		ok, err := cronFieldMatches(c.raw, c.val, c.lo, c.hi)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
	}
	return true, nil
}

func cronFieldMatches(field string, val, lo, hi int) (bool, error) {
	if field == "*" {
		return true, nil
	}
	if strings.HasPrefix(field, "*/") {
		step, err := strconv.Atoi(field[2:])
		if err != nil || step <= 0 {
			return false, &cronParseError{expr: field, reason: "invalid step"}
		}
		if val < lo || val > hi {
			return false, nil
		}
		return (val-lo)%step == 0, nil
	}
	n, err := strconv.Atoi(field)
	if err != nil {
		return false, &cronParseError{expr: field, reason: "expected integer, '*', or '*/N'"}
	}
	if n < lo || n > hi {
		return false, &cronParseError{expr: field, reason: "value out of range"}
	}
	return n == val, nil
}

type cronParseError struct {
	expr   string
	reason string
}

func (e *cronParseError) Error() string {
	return "cron parse: " + e.reason + " (" + e.expr + ")"
}
