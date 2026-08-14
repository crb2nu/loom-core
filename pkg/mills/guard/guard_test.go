package guard

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/store"
)

// countingAgent ticks a counter and returns a scripted result.
type countingAgent struct {
	ticks  atomic.Int64
	result TickResult
}

func (a *countingAgent) Name() string { return "counting" }
func (a *countingAgent) Tick(context.Context) (TickResult, error) {
	a.ticks.Add(1)
	return a.result, nil
}

func TestHarnessRunGatesOnEnabled(t *testing.T) {
	agent := &countingAgent{}
	enabled := atomic.Bool{}
	h := &Harness{
		Agent:    agent,
		Enabled:  func() bool { return enabled.Load() },
		Interval: func() time.Duration { return 5 * time.Millisecond },
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.Run(ctx) }()

	// Disabled: the loop idles.
	time.Sleep(30 * time.Millisecond)
	if n := agent.ticks.Load(); n != 0 {
		t.Fatalf("disabled harness ticked %d times", n)
	}
	// Enabled: ticks flow.
	enabled.Store(true)
	deadline := time.Now().Add(2 * time.Second)
	for agent.ticks.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if agent.ticks.Load() == 0 {
		t.Fatal("enabled harness never ticked")
	}
	// Paused: ticks stop again.
	h.SetPaused(true)
	base := agent.ticks.Load()
	time.Sleep(30 * time.Millisecond)
	if n := agent.ticks.Load(); n > base+1 { // one in-flight tick may land
		t.Fatalf("paused harness kept ticking: %d -> %d", base, n)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("run returned %v", err)
	}
}

// TestHarnessBootTickFiresBeforeInterval: with BootTick set, the first tick
// arrives on the boot delay, not the (here: absurdly long) interval — the
// churn-resilience contract: Recreate rollouts must not starve long-interval
// agents of their first tick.
func TestHarnessBootTickFiresBeforeInterval(t *testing.T) {
	agent := &countingAgent{}
	h := &Harness{
		Agent:    agent,
		Interval: func() time.Duration { return time.Hour },
		BootTick: 10 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.Run(ctx) }()

	deadline := time.Now().Add(2 * time.Second)
	for agent.ticks.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if agent.ticks.Load() == 0 {
		t.Fatal("boot tick never fired ahead of the 1h interval")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("run returned %v", err)
	}
}

func TestHarnessStatusSnapshot(t *testing.T) {
	agent := &countingAgent{result: TickResult{Inspected: 3, Acted: 1}}
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	h := &Harness{Agent: agent, Clock: func() time.Time { return now }}
	if _, err := h.TickOnce(context.Background()); err != nil {
		t.Fatalf("tick once: %v", err)
	}
	s := h.Status()
	if s.Name != "counting" || s.LastResult.Inspected != 3 || s.LastResult.Acted != 1 {
		t.Fatalf("status = %+v", s)
	}
	if s.LastTickAt == nil || !s.LastTickAt.Equal(now) {
		t.Fatalf("last tick at = %v", s.LastTickAt)
	}
}

func TestActionRecorderKindsAndCaps(t *testing.T) {
	st, err := store.Open(context.Background(), store.Options{Path: filepath.Join(t.TempDir(), "r.db")})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	now := time.Now().UTC()

	dry := true
	rec := &ActionRecorder{Events: st.Events, Actor: "overseer.test", DryRun: func() bool { return dry }}

	// Dry-run records under the .dryrun kind and never consumes the day cap.
	if err := rec.Record(ctx, "act", "backlog_item", "X", nil); err != nil {
		t.Fatalf("dry record: %v", err)
	}
	used, err := rec.DayUsed(ctx, now, "act")
	if err != nil {
		t.Fatalf("day used: %v", err)
	}
	if used != 0 {
		t.Fatalf("dry-run consumed day cap: %d", used)
	}

	// Committed records count.
	dry = false
	if err := rec.Record(ctx, "act", "backlog_item", "X", nil); err != nil {
		t.Fatalf("record: %v", err)
	}
	used, err = rec.DayUsed(ctx, now, "act")
	if err != nil {
		t.Fatalf("day used: %v", err)
	}
	if used != 1 {
		t.Fatalf("day used = %d, want 1", used)
	}
	n, err := rec.SubjectCount(ctx, "act", "backlog_item", "X")
	if err != nil || n != 1 {
		t.Fatalf("subject count = %d err=%v, want 1", n, err)
	}

	// FlagOnce ignores dry-run and is idempotent per subject.
	dry = true
	for i := 0; i < 2; i++ {
		if _, err := rec.FlagOnce(ctx, "flag", "backlog_item", "Y", nil); err != nil {
			t.Fatalf("flag once: %v", err)
		}
	}
	fn, err := st.Events.CountByKindSince(ctx, "overseer.test.flag", now.Add(-time.Hour))
	if err != nil || fn != 1 {
		t.Fatalf("flag count = %d err=%v, want 1", fn, err)
	}
}

func TestListByActorSince(t *testing.T) {
	st, err := store.Open(context.Background(), store.Options{Path: filepath.Join(t.TempDir(), "a.db")})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	for i, actor := range []string{"overseer.groomer", "overseer.groomer", "reconciler"} {
		if err := st.Events.Append(ctx, &store.Event{Actor: actor, Kind: "k", SubjectKind: "s", SubjectID: string(rune('a' + i))}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	events, err := st.Events.ListByActorSince(ctx, "overseer.groomer", time.Now().Add(-time.Hour), 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("len = %d, want 2", len(events))
	}
	for _, e := range events {
		if e.Actor != "overseer.groomer" {
			t.Fatalf("foreign actor leaked: %s", e.Actor)
		}
	}
}
