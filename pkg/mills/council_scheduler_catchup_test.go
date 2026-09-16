package mills

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/store"
)

// newCatchUpScheduler builds a scheduler against the fixture policy
// (schedule_cron "0 5 * * *") with a pinned clock and a counting RunFn.
func newCatchUpScheduler(t *testing.T, at *time.Time, ranSince func(context.Context, time.Time) (bool, error)) (*CouncilScheduler, *atomic.Int32, *sync.WaitGroup, *[]string) {
	t.Helper()
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
		fires   atomic.Int32
		wg      sync.WaitGroup
		reasons []string
		mu      sync.Mutex
	)
	s := &CouncilScheduler{
		Policy:   pm,
		Now:      func() time.Time { return *at },
		RanSince: ranSince,
		RunFn: func(_ context.Context, trigger store.CouncilTrigger, reason string) error {
			defer wg.Done()
			fires.Add(1)
			mu.Lock()
			reasons = append(reasons, reason)
			mu.Unlock()
			if trigger != store.CouncilTriggerCron {
				t.Errorf("trigger = %q; want cron", trigger)
			}
			return nil
		},
	}
	return s, &fires, &wg, &reasons
}

// TestCouncilScheduler_CatchUp_FiresMissedSlotInsideGrace pins the 2026-09-02
// 18:00Z gap: the process starts 3 minutes past a slot no run covered → one
// cron-triggered run whose reason names the slot; the per-minute check on
// the same boot must not fire it again.
func TestCouncilScheduler_CatchUp_FiresMissedSlotInsideGrace(t *testing.T) {
	at := time.Date(2026, 5, 19, 5, 3, 4, 0, time.UTC)
	var asked []time.Time
	s, fires, wg, reasons := newCatchUpScheduler(t, &at, func(_ context.Context, since time.Time) (bool, error) {
		asked = append(asked, since)
		return false, nil
	})

	wg.Add(1)
	s.maybeFire(context.Background()) // current minute 05:03 does not match
	s.catchUp(context.Background())
	wg.Wait()
	if got := fires.Load(); got != 1 {
		t.Fatalf("fires = %d; want 1 catch-up run", got)
	}
	if len(asked) != 1 || !asked[0].Equal(time.Date(2026, 5, 19, 5, 0, 0, 0, time.UTC)) {
		t.Fatalf("RanSince asked %v; want the 05:00 slot", asked)
	}
	if len(*reasons) != 1 || !strings.Contains((*reasons)[0], "catch-up") || !strings.Contains((*reasons)[0], "05:00:00Z") {
		t.Fatalf("reason = %v; want a catch-up reason naming the slot", *reasons)
	}
	// A second catch-up on the same boot is a no-op.
	s.catchUp(context.Background())
	if got := fires.Load(); got != 1 {
		t.Fatalf("fires = %d after a repeated catch-up; want 1", got)
	}
}

func TestCouncilScheduler_CatchUp_SkipsWhenAlreadyRanOrOutsideGrace(t *testing.T) {
	t.Run("previous process already fired the slot", func(t *testing.T) {
		at := time.Date(2026, 5, 19, 5, 3, 0, 0, time.UTC)
		s, fires, _, _ := newCatchUpScheduler(t, &at, func(context.Context, time.Time) (bool, error) { return true, nil })
		s.catchUp(context.Background())
		if got := fires.Load(); got != 0 {
			t.Fatalf("fires = %d; want 0 when a run already covered the slot", got)
		}
	})
	t.Run("slot older than the grace window", func(t *testing.T) {
		at := time.Date(2026, 5, 19, 5, 20, 0, 0, time.UTC)
		s, fires, _, _ := newCatchUpScheduler(t, &at, func(context.Context, time.Time) (bool, error) { return false, nil })
		s.catchUp(context.Background())
		if got := fires.Load(); got != 0 {
			t.Fatalf("fires = %d; want 0 for a slot 20m back with a 15m grace", got)
		}
	})
	t.Run("catch-up disabled", func(t *testing.T) {
		at := time.Date(2026, 5, 19, 5, 3, 0, 0, time.UTC)
		s, fires, _, _ := newCatchUpScheduler(t, &at, func(context.Context, time.Time) (bool, error) { return false, nil })
		s.CatchUpGrace = -1
		s.catchUp(context.Background())
		if got := fires.Load(); got != 0 {
			t.Fatalf("fires = %d; want 0 with catch-up disabled", got)
		}
	})
	t.Run("nil RanSince fails open to firing", func(t *testing.T) {
		at := time.Date(2026, 5, 19, 5, 3, 0, 0, time.UTC)
		s, fires, wg, _ := newCatchUpScheduler(t, &at, nil)
		wg.Add(1)
		s.catchUp(context.Background())
		wg.Wait()
		if got := fires.Load(); got != 1 {
			t.Fatalf("fires = %d; want 1 when the store is unknown", got)
		}
	})
}
