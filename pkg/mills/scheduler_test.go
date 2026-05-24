package mills

import (
	"context"
	"sync"
	"testing"
	"time"
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

	// Wait for the initial boot tick so the kick lands on the
	// steady-state select loop, not the boot-tick path.
	deadline := time.Now().Add(1 * time.Second)
	for counter.count() < 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if counter.count() < 1 {
		sch.Stop()
		<-runErrCh
		t.Fatal("initial tick never fired within 1s")
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
