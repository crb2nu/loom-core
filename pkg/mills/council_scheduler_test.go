package mills

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/store"
)

func TestCronMatches_DailyAtHour(t *testing.T) {
	// "0 5 * * *" — minute 0 of hour 5, every day.
	cases := []struct {
		when  time.Time
		match bool
	}{
		{time.Date(2026, 5, 19, 5, 0, 0, 0, time.UTC), true},
		{time.Date(2026, 5, 19, 5, 1, 0, 0, time.UTC), false}, // wrong minute
		{time.Date(2026, 5, 19, 6, 0, 0, 0, time.UTC), false}, // wrong hour
		{time.Date(2026, 5, 19, 4, 0, 0, 0, time.UTC), false}, // wrong hour
	}
	for _, tc := range cases {
		got, err := cronMatches("0 5 * * *", tc.when)
		if err != nil {
			t.Fatalf("unexpected parse error for %s: %v", tc.when, err)
		}
		if got != tc.match {
			t.Errorf("cronMatches(%q, %s) = %v; want %v", "0 5 * * *", tc.when, got, tc.match)
		}
	}
}

func TestCronMatches_EveryNHours(t *testing.T) {
	// "0 */6 * * *" — minute 0 of hours 0, 6, 12, 18.
	expr := "0 */6 * * *"
	hits := []int{0, 6, 12, 18}
	miss := []int{1, 5, 7, 11, 13, 17, 19, 23}
	for _, h := range hits {
		when := time.Date(2026, 5, 19, h, 0, 0, 0, time.UTC)
		got, err := cronMatches(expr, when)
		if err != nil {
			t.Fatalf("unexpected parse error: %v", err)
		}
		if !got {
			t.Errorf("cronMatches(%q, hour=%d) = false; want true", expr, h)
		}
	}
	for _, h := range miss {
		when := time.Date(2026, 5, 19, h, 0, 0, 0, time.UTC)
		got, _ := cronMatches(expr, when)
		if got {
			t.Errorf("cronMatches(%q, hour=%d) = true; want false", expr, h)
		}
	}
	// Wrong minute on a matching hour: still no match.
	wrongMin := time.Date(2026, 5, 19, 6, 30, 0, 0, time.UTC)
	if got, _ := cronMatches(expr, wrongMin); got {
		t.Errorf("cronMatches(%q, 06:30) = true; want false (minute mismatch)", expr)
	}
}

func TestCronMatches_Wildcards(t *testing.T) {
	// "* * * * *" matches every minute.
	when := time.Date(2026, 5, 19, 14, 37, 0, 0, time.UTC)
	got, err := cronMatches("* * * * *", when)
	if err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}
	if !got {
		t.Error("\"* * * * *\" should match every minute")
	}
}

func TestCronMatches_ParseErrors(t *testing.T) {
	bad := []string{
		"",
		"0 5 * *",      // 4 fields
		"0 5 * * * *",  // 6 fields
		"abc 5 * * *",  // non-integer
		"0 */0 * * *",  // step=0
		"0 */-3 * * *", // negative step
		"99 * * * *",   // out-of-range minute
		"0 24 * * *",   // out-of-range hour
	}
	when := time.Date(2026, 5, 19, 5, 0, 0, 0, time.UTC)
	for _, expr := range bad {
		if _, err := cronMatches(expr, when); err == nil {
			t.Errorf("cronMatches(%q) expected parse error, got nil", expr)
		}
	}
}

func TestCouncilScheduler_NoOpWhenNilDeps(t *testing.T) {
	// Nil runner or nil policy: Run blocks on ctx.Done and returns nil.
	// We don't drive the loop here — the contract is just "doesn't panic
	// and unblocks errgroup."
	s := &CouncilScheduler{}
	// Direct call to maybeFire with nil policy is a defensive guard.
	s.maybeFire(nil) // nolint:staticcheck // intentional nil ctx; we only exercise the early-return path
}

func TestCouncilScheduler_FireDedup(t *testing.T) {
	// Drive the scheduler with a counting RunFn and a fixed Now.
	// Two maybeFire calls on the same minute → exactly one fire.
	// A later matching minute → another fire (total: 2).
	dir := t.TempDir()
	p := Default()
	on := true
	p.Enabled = &on
	policyPath := filepath.Join(dir, "policy.yaml")
	writePolicyYAMLForTest(t, policyPath, p)
	pm, err := NewPolicyManager(context.Background(), policyPath, PolicyManagerOptions{SkipWatch: true})
	if err != nil {
		t.Fatalf("policy mgr: %v", err)
	}
	t.Cleanup(func() { _ = pm.Close() })

	var (
		fires atomic.Int32
		wg    sync.WaitGroup
	)
	runFn := CouncilRunFn(func(ctx context.Context, trigger store.CouncilTrigger, reason string) error {
		defer wg.Done()
		fires.Add(1)
		if trigger != store.CouncilTriggerCron {
			t.Errorf("trigger = %q; want cron", trigger)
		}
		return nil
	})

	// fixtureV1 used by writePolicyYAMLForTest pins schedule_cron to
	// "0 5 * * *" (daily 5 UTC). Pin the wall clock to a minute that
	// matches and a "later minute" 24h out so we cross another match.
	at := time.Date(2026, 5, 19, 5, 0, 0, 0, time.UTC)
	s := &CouncilScheduler{
		RunFn:  runFn,
		Policy: pm,
		Now:    func() time.Time { return at },
	}

	// First fire: one goroutine spawn.
	wg.Add(1)
	s.maybeFire(context.Background())
	// Second call on the same minute: dedup, no goroutine.
	s.maybeFire(context.Background())
	wg.Wait()
	if got := fires.Load(); got != 1 {
		t.Fatalf("after duplicate calls in same minute, fires = %d; want 1", got)
	}

	// Advance to tomorrow at 5 UTC — another match.
	at = at.Add(24 * time.Hour)
	wg.Add(1)
	s.maybeFire(context.Background())
	wg.Wait()
	if got := fires.Load(); got != 2 {
		t.Fatalf("after second matching minute, fires = %d; want 2", got)
	}
}

func TestCouncilScheduler_KillSwitchHonored(t *testing.T) {
	// policy.enabled=false → scheduler is silent even on a matching minute.
	dir := t.TempDir()
	p := Default()
	off := false
	p.Enabled = &off
	policyPath := filepath.Join(dir, "policy.yaml")
	writePolicyYAMLForTest(t, policyPath, p)
	pm, err := NewPolicyManager(context.Background(), policyPath, PolicyManagerOptions{SkipWatch: true})
	if err != nil {
		t.Fatalf("policy mgr: %v", err)
	}
	t.Cleanup(func() { _ = pm.Close() })

	var fires atomic.Int32
	runFn := CouncilRunFn(func(ctx context.Context, trigger store.CouncilTrigger, reason string) error {
		fires.Add(1)
		return nil
	})
	s := &CouncilScheduler{
		RunFn:  runFn,
		Policy: pm,
		// 5 UTC matches fixtureV1's "0 5 * * *" — the kill switch is what
		// suppresses the fire here, not the schedule.
		Now: func() time.Time { return time.Date(2026, 5, 19, 5, 0, 0, 0, time.UTC) },
	}
	s.maybeFire(context.Background())
	// No goroutines spawned at all → fires stays at 0.
	time.Sleep(10 * time.Millisecond) // small grace for any stray goroutine
	if got := fires.Load(); got != 0 {
		t.Fatalf("kill switch off but scheduler fired %d time(s)", got)
	}
}
