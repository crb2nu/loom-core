package mills

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// canaryPolicyMgr writes the given policy YAML to a temp file and returns a
// watch-less PolicyManager over it. Separate from writePolicyYAMLForTest
// (which always writes fixtureV1) because the canary tests need the
// intake.canary_autopilot block fixtureV1 doesn't carry.
func canaryPolicyMgr(t *testing.T, yaml string) *PolicyManager {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatalf("write policy: %v", err)
	}
	pm, err := NewPolicyManager(context.Background(), path, PolicyManagerOptions{SkipWatch: true})
	if err != nil {
		t.Fatalf("policy mgr: %v", err)
	}
	t.Cleanup(func() { _ = pm.Close() })
	return pm
}

const canaryPolicyEnabled = `
version: 2
budgets:
  council:  { max_usd_per_run: 1, max_usd_per_day: 1 }
  pipeline: { max_usd_per_run: 1, max_usd_per_day: 1 }
pipeline:
  retry: { max_attempts: 1, cooldown_seconds: 0 }
intake:
  canary_autopilot:
    enabled: true
    schedule_cron: "* * * * *"
`

const canaryPolicyDisabled = `
version: 2
budgets:
  council:  { max_usd_per_run: 1, max_usd_per_day: 1 }
  pipeline: { max_usd_per_run: 1, max_usd_per_day: 1 }
pipeline:
  retry: { max_attempts: 1, cooldown_seconds: 0 }
`

func TestCanaryScheduler_NoOpWhenNilDeps(t *testing.T) {
	// Nil run fn or nil policy: maybeFire early-returns without panicking.
	s := &CanaryScheduler{}
	s.maybeFire(nil) // nolint:staticcheck // intentional nil ctx; only exercising the early return
}

func TestCanaryScheduler_FiresWhenEnabled(t *testing.T) {
	pm := canaryPolicyMgr(t, canaryPolicyEnabled)

	var (
		fires  atomic.Int32
		gotRsn atomic.Value
		wg     sync.WaitGroup
	)
	runFn := CanaryRunFn(func(ctx context.Context, reason string) error {
		defer wg.Done()
		fires.Add(1)
		gotRsn.Store(reason)
		return nil
	})

	// "* * * * *" matches every minute. Pin the clock; two calls in the same
	// minute → one fire; advancing one minute → a second fire.
	at := time.Date(2026, 6, 27, 9, 0, 0, 0, time.UTC)
	s := &CanaryScheduler{RunFn: runFn, Policy: pm, Now: func() time.Time { return at }}

	wg.Add(1)
	s.maybeFire(context.Background())
	s.maybeFire(context.Background()) // same minute → deduped
	wg.Wait()
	if got := fires.Load(); got != 1 {
		t.Fatalf("after duplicate calls in same minute, fires = %d; want 1", got)
	}
	if r, _ := gotRsn.Load().(string); r != "autopilot" {
		t.Errorf("run fn reason = %q; want autopilot", r)
	}

	at = at.Add(time.Minute)
	wg.Add(1)
	s.maybeFire(context.Background())
	wg.Wait()
	if got := fires.Load(); got != 2 {
		t.Fatalf("after next matching minute, fires = %d; want 2", got)
	}
}

func TestCanaryScheduler_DisabledByDefault(t *testing.T) {
	// No intake.canary_autopilot block → autopilot is inert even though the
	// kill switch is on and the cron would otherwise match every minute.
	pm := canaryPolicyMgr(t, canaryPolicyDisabled)
	if pm.Current().CanaryAutopilotEnabled() {
		t.Fatal("autopilot should be disabled when the block is omitted")
	}

	var fires atomic.Int32
	s := &CanaryScheduler{
		RunFn:  CanaryRunFn(func(context.Context, string) error { fires.Add(1); return nil }),
		Policy: pm,
		Now:    func() time.Time { return time.Date(2026, 6, 27, 9, 0, 0, 0, time.UTC) },
	}
	s.maybeFire(context.Background())
	time.Sleep(10 * time.Millisecond) // grace for any stray goroutine
	if got := fires.Load(); got != 0 {
		t.Fatalf("disabled autopilot fired %d time(s)", got)
	}
}

func TestCanaryScheduler_KillSwitchHonored(t *testing.T) {
	// policy.enabled=false overrides an enabled autopilot — a paused operator
	// stays paused regardless of the canary schedule.
	pm := canaryPolicyMgr(t, "enabled: false\n"+canaryPolicyEnabled)

	var fires atomic.Int32
	s := &CanaryScheduler{
		RunFn:  CanaryRunFn(func(context.Context, string) error { fires.Add(1); return nil }),
		Policy: pm,
		Now:    func() time.Time { return time.Date(2026, 6, 27, 9, 0, 0, 0, time.UTC) },
	}
	s.maybeFire(context.Background())
	time.Sleep(10 * time.Millisecond)
	if got := fires.Load(); got != 0 {
		t.Fatalf("kill switch off but autopilot fired %d time(s)", got)
	}
}

func TestCanaryScheduler_PublishesActivityBeforeDetachedRun(t *testing.T) {
	pm := canaryPolicyMgr(t, canaryPolicyEnabled)
	started := make(chan struct{})
	release := make(chan struct{})
	s := &CanaryScheduler{
		Policy: pm,
		Now:    func() time.Time { return time.Date(2026, 6, 27, 9, 0, 0, 0, time.UTC) },
		RunFn: func(context.Context, string) error {
			close(started)
			<-release
			return nil
		},
	}
	s.maybeFire(context.Background())
	if got := s.ActiveOperations(); got != 1 {
		t.Fatalf("activity immediately after launch = %d, want 1", got)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("detached canary run did not start")
	}
	close(release)
	deadline := time.Now().Add(time.Second)
	for s.ActiveOperations() != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := s.ActiveOperations(); got != 0 {
		t.Fatalf("activity after completion = %d, want 0", got)
	}
}

func TestCanaryAutopilotAccessors(t *testing.T) {
	// Nil-safe defaults.
	var nilP *Policy
	if nilP.CanaryAutopilotEnabled() {
		t.Error("nil policy must report autopilot disabled")
	}
	if got := nilP.CanaryAutopilotCron(); got != "0 9 * * *" {
		t.Errorf("nil policy cron default = %q; want 0 9 * * *", got)
	}

	// Enabled block with everything unset → effective defaults.
	pm := canaryPolicyMgr(t, "version: 2\nbudgets:\n  council:  { max_usd_per_run: 1, max_usd_per_day: 1 }\n  pipeline: { max_usd_per_run: 1, max_usd_per_day: 1 }\npipeline:\n  retry: { max_attempts: 1, cooldown_seconds: 0 }\nintake:\n  canary_autopilot:\n    enabled: true\n")
	p := pm.Current()
	if !p.CanaryAutopilotEnabled() {
		t.Fatal("expected autopilot enabled")
	}
	if got := p.CanaryAutopilotCron(); got != "0 9 * * *" {
		t.Errorf("cron default = %q; want 0 9 * * *", got)
	}
	if got := p.CanaryAutopilotPriority(); got != "P3" {
		t.Errorf("priority default = %q; want P3", got)
	}
	if got := p.CanaryAutopilotFixturePath(); got != "testdata/mills-canary/heartbeat.md" {
		t.Errorf("path default = %q; want testdata/mills-canary/heartbeat.md", got)
	}
}
