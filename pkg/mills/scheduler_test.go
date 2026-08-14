package mills

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/crb2nu/loom/pkg/mills/store"
)

// TestNextInterval_IdleThrottle exercises the cadence-decision logic in
// isolation so we don't have to drive the real ticker. Wall-clock state
// is fed in via the Clock function on Scheduler.
func TestNextInterval_IdleThrottle(t *testing.T) {
	const (
		fast = 60 * time.Second
		slow = 5 * time.Minute
		idle = 5 * time.Minute
	)

	now := time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC)
	s := &Scheduler{Clock: func() time.Time { return now }}

	// First tick is a no-op. We're entering the streak — should still
	// use the fast cadence and start the streak clock.
	streakStart := now
	got, idleSince := s.nextInterval(TickResult{}, time.Time{}, fast, slow, idle)
	if got != fast {
		t.Fatalf("first idle tick: want fast=%s, got %s", fast, got)
	}
	if idleSince != streakStart {
		t.Fatalf("first idle tick: want idleSince=%s, got %s", streakStart, idleSince)
	}

	// Advance 4 minutes — still under IdleAfter, stay fast.
	now = streakStart.Add(4 * time.Minute)
	got, idleSince = s.nextInterval(TickResult{}, idleSince, fast, slow, idle)
	if got != fast {
		t.Fatalf("4min into streak: want fast, got %s", got)
	}
	if idleSince != streakStart {
		t.Fatalf("4min into streak: idleSince should not advance: want %s got %s", streakStart, idleSince)
	}

	// Advance to exactly IdleAfter — should switch to slow.
	now = streakStart.Add(idle)
	got, idleSince = s.nextInterval(TickResult{}, idleSince, fast, slow, idle)
	if got != slow {
		t.Fatalf("at IdleAfter: want slow=%s, got %s", slow, got)
	}
	if idleSince != streakStart {
		t.Fatalf("at IdleAfter: idleSince should still equal streakStart, got %s", idleSince)
	}

	// Stay slow if no work shows up.
	now = streakStart.Add(2 * idle)
	got, _ = s.nextInterval(TickResult{}, idleSince, fast, slow, idle)
	if got != slow {
		t.Fatalf("deep idle: want slow, got %s", got)
	}

	// Real work resets the streak and snaps back to fast.
	got, idleSince = s.nextInterval(TickResult{Inspected: 1, Started: 1}, idleSince, fast, slow, idle)
	if got != fast {
		t.Fatalf("work tick: want fast, got %s", got)
	}
	if !idleSince.IsZero() {
		t.Fatalf("work tick: idleSince should reset to zero, got %s", idleSince)
	}

	// Even Inspected > 0 with no started counts as "work" (e.g. an item
	// got deferred for budget). The reconciler is doing meaningful work.
	got, idleSince = s.nextInterval(TickResult{Inspected: 3, Deferred: 3}, time.Time{}, fast, slow, idle)
	if got != fast || !idleSince.IsZero() {
		t.Fatalf("deferred tick: want fast/zero, got %s/%s", got, idleSince)
	}
}

func TestNextInterval_ThrottleDisabled(t *testing.T) {
	const fast = 60 * time.Second
	now := time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC)
	s := &Scheduler{Clock: func() time.Time { return now }}

	// idleAfter=0 in the helper signature means "disabled" because the
	// Run() prelude maps a negative IdleAfter onto zero. Verify that
	// passing zero through nextInterval keeps cadence fast forever.
	idleSince := time.Time{}
	for i := 0; i < 10; i++ {
		now = now.Add(time.Hour)
		got, next := s.nextInterval(TickResult{}, idleSince, fast, 5*time.Minute, 0)
		if got != fast {
			t.Fatalf("iter %d: throttle disabled but got %s, want fast", i, got)
		}
		if !next.IsZero() {
			t.Fatalf("iter %d: throttle disabled, idleSince should stay zero, got %s", i, next)
		}
		idleSince = next
	}
}

func TestIsNoOp(t *testing.T) {
	cases := []struct {
		name string
		res  TickResult
		want bool
	}{
		{"empty", TickResult{}, true},
		{"policy disabled", TickResult{SkipReason: "policy disabled"}, true},
		{"deferred", TickResult{Inspected: 2, Deferred: 2}, false},
		{"started", TickResult{Inspected: 1, Started: 1}, false},
		{"errored only", TickResult{Errored: 1}, true},
		// Errored alone (without Inspected) is treated as a no-op. Real
		// errors come with Inspected > 0 (we tried to look at items and
		// the call failed); a pure read failure surfaces via the err
		// return, not the result.
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.res.IsNoOp(); got != tc.want {
				t.Fatalf("IsNoOp(%+v) = %v, want %v", tc.res, got, tc.want)
			}
		})
	}
}

func TestScheduler_TickRecordsKPISnapshot(t *testing.T) {
	env := newRecEnv(t, nil)
	writer := NewKPIWriter(env.store, env.policy)
	writer.Clock = func() time.Time { return env.now }
	writer.Windows = []time.Duration{kpiWindow1d}

	sch := NewScheduler(env.rec)
	sch.KPIRecorder = writer

	if _, err := sch.tickOnce(context.Background()); err != nil {
		t.Fatalf("tickOnce: %v", err)
	}
	if _, err := env.store.KPI.Latest(context.Background(), int(kpiWindow1d.Seconds())); err != nil {
		t.Fatalf("expected scheduler to record KPI snapshot: %v", err)
	}
}

func TestScheduler_TickDeadlineAndErrorSkipKPI(t *testing.T) {
	t.Run("deadline", func(t *testing.T) {
		env := newRecEnv(t, nil)
		// Let Tick reach a clean early-return path exactly as its parent budget
		// expires. The scheduler must inspect the tick context even though Tick
		// itself returned nil.
		env.rec.AutonomyGate = func(ctx context.Context) (bool, []string) {
			<-ctx.Done()
			return false, []string{"test deadline"}
		}
		counter := &kpiTickCounter{}
		sch := NewScheduler(env.rec)
		sch.KPIRecorder = counter
		sch.Logger = nil

		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		if _, err := sch.tickOnce(ctx); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("tickOnce error = %v, want deadline exceeded", err)
		}
		if got := counter.count(); got != 0 {
			t.Fatalf("KPI calls after tick deadline = %d, want 0", got)
		}
	})

	t.Run("error", func(t *testing.T) {
		counter := &kpiTickCounter{}
		sch := NewScheduler(&Reconciler{})
		sch.KPIRecorder = counter
		sch.Logger = nil

		if _, err := sch.tickOnce(context.Background()); err == nil {
			t.Fatal("tickOnce error = nil, want reconciler configuration error")
		}
		if got := counter.count(); got != 0 {
			t.Fatalf("KPI calls after tick error = %d, want 0", got)
		}
	})
}

type kpiContextRecorder struct {
	mu        sync.Mutex
	calls     int
	remaining time.Duration
	ctxErr    error
}

func (r *kpiContextRecorder) Record(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.ctxErr = ctx.Err()
	if deadline, ok := ctx.Deadline(); ok {
		r.remaining = time.Until(deadline)
	}
	return nil
}

func (r *kpiContextRecorder) snapshot() (int, time.Duration, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls, r.remaining, r.ctxErr
}

func TestScheduler_SuccessfulTickGivesKPIFreshBudget(t *testing.T) {
	env := newRecEnv(t, nil)
	recorder := &kpiContextRecorder{}
	sch := NewScheduler(env.rec)
	sch.KPIRecorder = recorder
	sch.Logger = nil

	if _, err := sch.tickOnce(context.Background()); err != nil {
		t.Fatalf("tickOnce: %v", err)
	}
	calls, remaining, ctxErr := recorder.snapshot()
	if calls != 1 {
		t.Fatalf("KPI calls = %d, want 1", calls)
	}
	if ctxErr != nil {
		t.Fatalf("KPI context already canceled: %v", ctxErr)
	}
	if remaining < schedulerKPIRecordTimeout-time.Second || remaining > schedulerKPIRecordTimeout+time.Second {
		t.Fatalf("KPI budget = %s, want a fresh budget near %s", remaining, schedulerKPIRecordTimeout)
	}
}

func TestScheduler_RunBootTickSkipsKPIUntilPeriodicTick(t *testing.T) {
	env := newRecEnv(t, nil)
	reconciles := make(chan struct{}, 2)
	env.rec.AutonomyGate = func(context.Context) (bool, []string) {
		reconciles <- struct{}{}
		return false, []string{"test no-op"}
	}

	counter := &kpiTickCounter{}
	sch := NewScheduler(env.rec)
	sch.Interval = 250 * time.Millisecond
	sch.IdleAfter = -1
	sch.KPIRecorder = counter
	sch.Logger = nil

	ctx, cancel := context.WithCancel(context.Background())
	runErrCh := make(chan error, 1)
	go func() { runErrCh <- sch.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-runErrCh:
			if err != nil {
				t.Errorf("scheduler Run: %v", err)
			}
		case <-time.After(time.Second):
			t.Error("scheduler did not stop")
		}
	})

	waitForReconcile := func(phase string) {
		t.Helper()
		select {
		case <-reconciles:
		case <-time.After(time.Second):
			t.Fatalf("%s reconcile did not start", phase)
		}
		deadline := time.Now().Add(time.Second)
		for sch.ActiveOperations() != 0 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		if got := sch.ActiveOperations(); got != 0 {
			t.Fatalf("%s reconcile still active: %d", phase, got)
		}
	}

	waitForReconcile("boot")
	if got := counter.count(); got != 0 {
		t.Fatalf("KPI calls after boot reconcile = %d, want 0", got)
	}

	waitForReconcile("first periodic")
	if got := counter.count(); got != 1 {
		t.Fatalf("KPI calls after first periodic reconcile = %d, want 1", got)
	}
	cancel()
}

func TestScheduler_ParentCancellationDuringGhostSweepStopsTailAndKPI(t *testing.T) {
	env := newRecEnv(t, nil)
	for i := 0; i < 3; i++ {
		seedEscalatedGhostSpark(
			t, env, fmt.Sprintf("MILLS-CANCEL-%d", i), int64(8100+i),
			env.now.Add(-time.Duration(i+1)*time.Hour),
		)
	}
	seedEscalatedForRequeue(t, env, seedRequeueSpec{
		id: "MILLS-REQUEUE-PARENT-CANCEL", class: autoRequeueClassInfra,
		endedAgo: time.Hour,
	})
	ctx, cancel := context.WithCancel(context.Background())
	mrs := &cancelOnFirstMRStateClient{cancel: cancel}
	env.rec.GhostSparkMRState = mrs
	counter := &kpiTickCounter{}
	sch := NewScheduler(env.rec)
	sch.KPIRecorder = counter
	sch.Logger = nil
	env.rec.Logger = nil
	cancel()

	if _, err := sch.tickOnce(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("tickOnce error = %v, want context canceled", err)
	}
	if got := mrs.callCount(); got != 0 {
		t.Fatalf("MRState calls from reconciler tick = %d, want 0", got)
	}
	if got := counter.count(); got != 0 {
		t.Fatalf("KPI calls after parent cancellation = %d, want 0", got)
	}
	if got := backlogState(t, env, "MILLS-REQUEUE-PARENT-CANCEL"); got != store.BacklogEscalated {
		t.Fatalf("auto-requeue state after parent cancellation = %s, want escalated", got)
	}
	if got := autoRequeueEventCount(t, env, "MILLS-REQUEUE-PARENT-CANCEL"); got != 0 {
		t.Fatalf("auto-requeue events after parent cancellation = %d, want 0", got)
	}

	events, err := env.store.Events.ListSince(context.Background(), time.Unix(0, 0), 100)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	for _, event := range events {
		if event.Kind == "reconciler.ghost_spark_sweep_failed" || event.Kind == "reconciler.tick" || strings.HasPrefix(event.Kind, "reconciler.auto_requeue") {
			t.Fatalf("tail event %q emitted after parent cancellation", event.Kind)
		}
	}
}

func TestScheduler_TickDoesNotRunEscalationSweeps(t *testing.T) {
	env := newRecEnv(t, nil)
	seedEscalatedGhostSpark(t, env, "MILLS-GHOST-TIMEOUT", 8200, env.now.Add(-time.Hour))
	seedEscalatedForRequeue(t, env, seedRequeueSpec{
		id: "MILLS-REQUEUE-AFTER-GHOST-TIMEOUT", class: autoRequeueClassInfra,
		endedAgo: time.Hour,
	})
	mrs := &deadlineExceededMRStateClient{}
	env.rec.GhostSparkMRState = mrs
	counter := &kpiTickCounter{}
	sch := NewScheduler(env.rec)
	sch.KPIRecorder = counter
	sch.Logger = nil

	beforeTicks := testutil.ToFloat64(ReconcileTicksTotal.WithLabelValues("no_op"))
	if _, err := sch.tickOnce(context.Background()); err != nil {
		t.Fatalf("tickOnce after child timeout: %v", err)
	}
	if got := counter.count(); got != 1 {
		t.Fatalf("KPI calls after child timeout = %d, want 1", got)
	}
	if got := mrs.callCount(); got != 0 {
		t.Fatalf("MRState calls from reconciler tick = %d, want zero", got)
	}
	if got := backlogState(t, env, "MILLS-GHOST-TIMEOUT"); got != store.BacklogEscalated {
		t.Fatalf("ghost state after child timeout = %s, want escalated", got)
	}
	if got := backlogState(t, env, "MILLS-REQUEUE-AFTER-GHOST-TIMEOUT"); got != store.BacklogEscalated {
		t.Fatalf("auto-requeue state after tick = %s, want escalated", got)
	}
	if got := autoRequeueEventCount(t, env, "MILLS-REQUEUE-AFTER-GHOST-TIMEOUT"); got != 0 {
		t.Fatalf("auto-requeue events from tick = %d, want 0", got)
	}
	if delta := testutil.ToFloat64(ReconcileTicksTotal.WithLabelValues("no_op")) - beforeTicks; delta != 1 {
		t.Fatalf("tail tick counter delta after child timeout = %v, want 1", delta)
	}

	events, err := env.store.Events.ListSince(context.Background(), time.Unix(0, 0), 100)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	var failures, autoSweeps, tails int
	for _, event := range events {
		switch event.Kind {
		case "reconciler.ghost_spark_sweep_failed":
			failures++
			if event.Payload["outcome"] != "timeout" || !strings.Contains(fmt.Sprint(event.Payload["error"]), context.DeadlineExceeded.Error()) {
				t.Fatalf("ghost timeout payload = %+v", event.Payload)
			}
		case "reconciler.auto_requeue_sweep":
			autoSweeps++
			if event.Payload["requeued"] != float64(1) {
				t.Fatalf("auto-requeue sweep payload = %+v", event.Payload)
			}
		case "reconciler.tick":
			tails++
			if event.Payload["outcome"] != "ok" {
				t.Fatalf("tick payload after child timeout = %+v", event.Payload)
			}
			if _, ok := event.Payload["auto_requeued"]; ok {
				t.Fatalf("tick retained escalation sweep fields: %+v", event.Payload)
			}
		}
	}
	if failures != 0 || autoSweeps != 0 || tails != 1 {
		t.Fatalf("events after tick: failure=%d auto_sweep=%d tick=%d, want 0,0,1", failures, autoSweeps, tails)
	}
}

func TestScheduler_StopDuringTickSkipsKPI(t *testing.T) {
	env := newRecEnv(t, nil)
	gateEntered := make(chan struct{})
	releaseGate := make(chan struct{})
	env.rec.AutonomyGate = func(context.Context) (bool, []string) {
		close(gateEntered)
		<-releaseGate
		return false, []string{"test stop"}
	}
	counter := &kpiTickCounter{}
	sch := NewScheduler(env.rec)
	sch.KPIRecorder = counter
	sch.Logger = nil

	done := make(chan error, 1)
	go func() { done <- sch.Run(context.Background()) }()
	select {
	case <-gateEntered:
	case <-time.After(time.Second):
		t.Fatal("tick did not reach autonomy gate")
	}
	sch.Stop()
	close(releaseGate)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("scheduler Run: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("scheduler did not stop")
	}
	if got := counter.count(); got != 0 {
		t.Fatalf("KPI calls after Stop during tick = %d, want 0", got)
	}
}

// ----- Slice 3b: tick-on-merge via KickNow -----

// TestScheduler_KickNow_TriggersImmediateTick pins the tick-on-merge
// behaviour: after the merge hook calls KickNow, the scheduler runs a
// Tick within ~1s instead of waiting up to 60s for the next scheduled
// tick. Without this, even with auto-merge wired, the operator sits
// idle for up to a minute between merges.
// kpiTickCounter is a KPIRecorder stub used as the observable for
// "did a Tick actually fire?" — the scheduler calls Record exactly
// once per successful tickOnce.
type kpiTickCounter struct {
	mu sync.Mutex
	n  int
}

func (c *kpiTickCounter) Record(_ context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
	return nil
}

func (c *kpiTickCounter) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

func TestScheduler_KickNow_TriggersImmediateTick(t *testing.T) {
	env := newRecEnv(t, nil)
	reconciles := make(chan struct{}, 2)
	env.rec.AutonomyGate = func(context.Context) (bool, []string) {
		reconciles <- struct{}{}
		return false, []string{"test no-op"}
	}
	counter := &kpiTickCounter{}
	// Long interval so the test only passes if KickNow actually fires.
	sch := NewScheduler(env.rec)
	sch.Interval = 10 * time.Minute
	sch.IdleAfter = -1 // disable idle throttle so cadence stays fast
	sch.Logger = nil
	sch.KPIRecorder = counter

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	runErrCh := make(chan error, 1)
	go func() { runErrCh <- sch.Run(ctx) }()

	// Wait for the initial boot reconcile to finish so the kick lands on
	// the steady-state select loop. Boot intentionally skips KPI recording.
	select {
	case <-reconciles:
	case <-time.After(time.Second):
		sch.Stop()
		<-runErrCh
		t.Fatal("initial reconcile never fired within 1s")
	}
	deadline := time.Now().Add(time.Second)
	for sch.ActiveOperations() != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := sch.ActiveOperations(); got != 0 {
		sch.Stop()
		<-runErrCh
		t.Fatalf("initial reconcile still active: %d", got)
	}
	if got := counter.count(); got != 0 {
		sch.Stop()
		<-runErrCh
		t.Fatalf("KPI calls after initial reconcile = %d, want 0", got)
	}
	initial := counter.count()

	// Kick → expect counter to grow within ~1s without waiting for
	// the 10-minute scheduled tick.
	sch.KickNow()
	kickDeadline := time.Now().Add(1 * time.Second)
	for counter.count() <= initial && time.Now().Before(kickDeadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if counter.count() <= initial {
		sch.Stop()
		<-runErrCh
		t.Fatalf("tick count did not grow after KickNow: %d (initial %d)",
			counter.count(), initial)
	}

	sch.Stop()
	<-runErrCh
}

// TestScheduler_KickNow_BeforeRunIsSafe pins that KickNow doesn't
// panic / leak goroutines when called before Run sets up kickCh.
func TestScheduler_KickNow_BeforeRunIsSafe(t *testing.T) {
	sch := NewScheduler(nil)
	sch.KickNow()
	sch.KickNow() // double for good measure
}

// TestScheduler_KickNow_NilReceiverNoPanic guards the wiring path
// where a hook closure captures a nil *Scheduler. Should silently
// no-op.
func TestScheduler_KickNow_NilReceiverNoPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("KickNow panic on nil receiver: %v", r)
		}
	}()
	var sch *Scheduler
	sch.KickNow()
}
