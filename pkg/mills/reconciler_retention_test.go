package mills

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/store"
)

func openRetentionTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), store.Options{Path: filepath.Join(t.TempDir(), "r.db")})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func countEventsByKind(t *testing.T, st *store.Store, kind string) int {
	t.Helper()
	n, err := st.Events.CountByKindSince(context.Background(), kind, time.Unix(0, 0))
	if err != nil {
		t.Fatalf("count %s: %v", kind, err)
	}
	return n
}

// TestReconciler_NoiseGate_SuppressesRepeatedBookkeepingRows pins the
// cooldown: the same deferral for the same item is one row per cooldown
// window, a changed reason lands immediately, audit kinds are untouched, and
// the deferral METRIC still counts every occurrence.
func TestReconciler_NoiseGate_SuppressesRepeatedBookkeepingRows(t *testing.T) {
	st := openRetentionTestStore(t)
	now := time.Date(2026, 9, 2, 18, 0, 0, 0, time.UTC)
	r := &Reconciler{Store: st, Clock: func() time.Time { return now }, RetentionDisabled: true}
	ctx := context.Background()

	// Five ticks, same item, same reason: one row.
	for i := 0; i < 5; i++ {
		r.append(ctx, "reconciler.deferred", "scope_overlap", map[string]any{"item": "bl-x", "blocked_by": "bl-y"})
		now = now.Add(time.Minute)
	}
	if got := countEventsByKind(t, st, "reconciler.deferred"); got != 1 {
		t.Fatalf("deferred rows = %d, want 1 (identical restatements inside the cooldown)", got)
	}
	// Reason changes: a new fact, a new row.
	r.append(ctx, "reconciler.deferred", "scope_overlap", map[string]any{"item": "bl-x", "blocked_by": "bl-z"})
	if got := countEventsByKind(t, st, "reconciler.deferred"); got != 2 {
		t.Fatalf("deferred rows = %d, want 2 after the blocker changed", got)
	}
	// Different item: its own row.
	r.append(ctx, "reconciler.deferred", "scope_overlap", map[string]any{"item": "bl-w", "blocked_by": "bl-y"})
	if got := countEventsByKind(t, st, "reconciler.deferred"); got != 3 {
		t.Fatalf("deferred rows = %d, want 3 for a second item", got)
	}
	// Cooldown elapsed: the original restatement lands again.
	now = now.Add(eventNoiseCooldown)
	r.append(ctx, "reconciler.deferred", "scope_overlap", map[string]any{"item": "bl-x", "blocked_by": "bl-y"})
	if got := countEventsByKind(t, st, "reconciler.deferred"); got != 4 {
		t.Fatalf("deferred rows = %d, want 4 once the cooldown elapsed", got)
	}
	// The heartbeat and audit kinds are never gated.
	for i := 0; i < 3; i++ {
		r.append(ctx, "reconciler.tick", "ok", map[string]any{"n": i})
		r.append(ctx, "reconciler.escalated", "code", map[string]any{"item": "bl-x"})
	}
	if got := countEventsByKind(t, st, "reconciler.tick"); got != 3 {
		t.Fatalf("tick rows = %d, want 3 (heartbeat is never cooled down)", got)
	}
	if got := countEventsByKind(t, st, "reconciler.escalated"); got != 3 {
		t.Fatalf("escalated rows = %d, want 3 (audit kinds are never gated)", got)
	}
}

// TestReconciler_SweepRetention prunes only bookkeeping kinds past their
// retention and KPI snapshots past theirs; audit rows and young rows stay.
func TestReconciler_SweepRetention(t *testing.T) {
	st := openRetentionTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 2, 18, 0, 0, 0, time.UTC)
	r := &Reconciler{Store: st, Clock: func() time.Time { return now }}

	old := now.Add(-eventNoiseRetention - time.Hour)
	young := now.Add(-time.Hour)
	seed := func(kind string, at time.Time) {
		t.Helper()
		if err := st.Events.Append(ctx, &store.Event{Actor: "reconciler", Kind: kind, OccurredAt: at, Payload: map[string]any{}}); err != nil {
			t.Fatalf("seed %s: %v", kind, err)
		}
	}
	for i := 0; i < 7; i++ {
		seed("reconciler.deferred", old)
		seed("reconciler.ghost_spark_skipped", old)
	}
	seed("reconciler.deferred", young)
	seed("reconciler.escalated", old)       // audit kind: kept regardless of age
	seed("pipeline.stage.done", old)        // audit kind: kept regardless of age
	seed("council.mutator.dedup_skip", old) // audit kind: kept regardless of age

	for _, at := range []time.Time{now.Add(-kpiSnapshotRetention - time.Hour), now.Add(-kpiSnapshotRetention - 2*time.Hour), now.Add(-time.Hour)} {
		if err := st.KPI.RecordSnapshot(ctx, &store.KPISnapshot{SnapshotAt: at, WindowSeconds: 86400, Metrics: map[string]any{"x": 1}}); err != nil {
			t.Fatalf("seed kpi: %v", err)
		}
	}

	res, err := r.SweepRetention(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.EventsPruned != 14 {
		t.Fatalf("events pruned = %d, want 14", res.EventsPruned)
	}
	if res.KPISnapshotsPruned != 2 {
		t.Fatalf("kpi pruned = %d, want 2", res.KPISnapshotsPruned)
	}
	for kind, want := range map[string]int{
		"reconciler.deferred":            1,
		"reconciler.ghost_spark_skipped": 0,
		"reconciler.escalated":           1,
		"pipeline.stage.done":            1,
		"council.mutator.dedup_skip":     1,
	} {
		if got := countEventsByKind(t, st, kind); got != want {
			t.Fatalf("%s rows after sweep = %d, want %d", kind, got, want)
		}
	}
	latest, err := st.KPI.Latest(ctx, 86400)
	if err != nil || latest == nil {
		t.Fatalf("latest kpi after sweep: %v %v", latest, err)
	}

	// A second pass is a no-op; retentionDue honours the interval.
	res, err = r.SweepRetention(ctx)
	if err != nil || res.EventsPruned != 0 || res.KPISnapshotsPruned != 0 {
		t.Fatalf("second sweep = %+v err=%v, want nothing", res, err)
	}
	if !r.retentionDue(now) {
		t.Fatal("first tick must be due")
	}
	r.nextRetention = now.Add(r.retentionInterval())
	if r.retentionDue(now.Add(time.Hour)) {
		t.Fatal("must not be due again inside the interval")
	}
	if r.retentionDue(now.Add(DefaultRetentionInterval)) != true {
		t.Fatal("must be due once the interval elapsed")
	}
}

// TestReconciler_SweepRetentionDue_CatchUpAfterTimeout pins the retry
// cadence: a pass that finishes backs off a full interval, a pass whose budget
// is already spent (the cold-cache timeout) comes back on the catch-up
// interval instead of waiting a day, and a spent PARENT context is the only
// outcome reported as an error.
func TestReconciler_SweepRetentionDue_CatchUpAfterTimeout(t *testing.T) {
	st := openRetentionTestStore(t)
	now := time.Date(2026, 9, 8, 18, 0, 0, 0, time.UTC)
	r := &Reconciler{Store: st, Clock: func() time.Time { return now }}

	// Clean pass: due immediately, then not until the daily interval.
	if err := r.sweepRetentionDue(context.Background(), now); err != nil {
		t.Fatalf("clean pass: %v", err)
	}
	if want := now.Add(DefaultRetentionInterval); !r.nextRetention.Equal(want) {
		t.Fatalf("after clean pass next = %v, want %v", r.nextRetention, want)
	}
	if r.retentionDue(now.Add(retentionCatchUpInterval)) {
		t.Fatal("a clean pass must not be re-run on the catch-up cadence")
	}

	// Spent parent (the tick budget expired underneath the sweep): reported as
	// an error, and the pessimistic catch-up slot armed before the sweep ran
	// is what remains.
	r.nextRetention = time.Time{}
	deadCtx, cancelDead := context.WithTimeout(context.Background(), 0)
	defer cancelDead()
	if err := r.sweepRetentionDue(deadCtx, now); err == nil {
		t.Fatal("a spent parent context must be reported")
	}
	if want := now.Add(retentionCatchUpInterval); !r.nextRetention.Equal(want) {
		t.Fatalf("after parent-expired pass next = %v, want catch-up %v", r.nextRetention, want)
	}

	// Sweep that does not finish under a LIVE parent (the daily pass hitting
	// its own budget): not a tick error, and retried on the catch-up cadence
	// rather than backed off a day. A closed store makes the prune fail the
	// same way a deadline does from the scheduler's point of view.
	r.nextRetention = time.Time{}
	if err := st.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	if err := r.sweepRetentionDue(context.Background(), now); err != nil {
		t.Fatalf("an unfinished sweep under a live parent must not error the tick: %v", err)
	}
	if want := now.Add(retentionCatchUpInterval); !r.nextRetention.Equal(want) {
		t.Fatalf("after unfinished pass next = %v, want catch-up %v", r.nextRetention, want)
	}
	if !r.retentionDue(now.Add(retentionCatchUpInterval)) {
		t.Fatal("an unfinished pass must be due again on the catch-up cadence")
	}
	if r.retentionDue(now.Add(retentionCatchUpInterval - time.Minute)) {
		t.Fatal("an unfinished pass must not be re-run on every tick")
	}
}
