package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func openSpinTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(context.Background(), Options{Path: filepath.Join(t.TempDir(), "mills.db")})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestSpinDAO_RoundTripCountAndOrphanSweep(t *testing.T) {
	st := openSpinTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 7, 4, 13, 0, 0, 0, time.UTC)

	// A running spin and a succeeded spin.
	running := &SpinRun{
		ID: "spin-running", Brief: "roving A", Frames: []string{"jacquard"},
		Priority: "P1", Project: "services/loom-core", Namespace: "mills/spun",
		Status: SpinRunning, PlanIDs: []string{}, StartedAt: base,
	}
	if err := st.Spin.Put(ctx, running); err != nil {
		t.Fatalf("put running: %v", err)
	}
	done := base.Add(90 * time.Second)
	succeeded := &SpinRun{
		ID: "spin-done", Brief: "roving B", Frames: []string{"ring", "mule"},
		Status: SpinSucceeded, PlanIDs: []string{"plan-x", "plan-y"}, Competitive: true,
		StartedAt: base.Add(2 * time.Second), EndedAt: &done,
	}
	if err := st.Spin.Put(ctx, succeeded); err != nil {
		t.Fatalf("put succeeded: %v", err)
	}

	// Get round-trips fields, including the JSON slices and scope columns.
	got, err := st.Spin.Get(ctx, "spin-running")
	if err != nil {
		t.Fatalf("get running: %v", err)
	}
	if got.Status != SpinRunning || got.Priority != "P1" || got.Project != "services/loom-core" {
		t.Errorf("running round-trip = %+v", got)
	}
	if len(got.Frames) != 1 || got.Frames[0] != "jacquard" {
		t.Errorf("frames = %v", got.Frames)
	}
	gotDone, err := st.Spin.Get(ctx, "spin-done")
	if err != nil {
		t.Fatalf("get done: %v", err)
	}
	if !gotDone.Competitive || len(gotDone.PlanIDs) != 2 || gotDone.PlanIDs[0] != "plan-x" {
		t.Errorf("done round-trip = %+v", gotDone)
	}
	if gotDone.EndedAt == nil {
		t.Errorf("done ended_at should be set")
	}

	// CountActive counts only pending/running (the succeeded row is excluded).
	if n, err := st.Spin.CountActive(ctx); err != nil || n != 1 {
		t.Fatalf("CountActive = %d, err %v; want 1", n, err)
	}

	// List is newest-first by started_at.
	runs, err := st.Spin.List(ctx, 10)
	if err != nil || len(runs) != 2 {
		t.Fatalf("List len = %d, err %v; want 2", len(runs), err)
	}
	if runs[0].ID != "spin-done" || runs[1].ID != "spin-running" {
		t.Errorf("List order = %s,%s; want spin-done,spin-running", runs[0].ID, runs[1].ID)
	}

	// Orphan-sweep fails the running row and leaves the terminal one alone.
	swept, err := st.Spin.MarkOrphaned(ctx)
	if err != nil || swept != 1 {
		t.Fatalf("MarkOrphaned = %d, err %v; want 1", swept, err)
	}
	after, err := st.Spin.Get(ctx, "spin-running")
	if err != nil {
		t.Fatalf("get after sweep: %v", err)
	}
	if after.Status != SpinFailed || after.EndedAt == nil {
		t.Errorf("swept row = %+v; want failed + ended_at", after)
	}
	if after.Error == "" {
		t.Errorf("swept row should carry an orphaned reason")
	}
	// A second sweep is a no-op (idempotent).
	if swept, err := st.Spin.MarkOrphaned(ctx); err != nil || swept != 0 {
		t.Fatalf("second MarkOrphaned = %d, err %v; want 0", swept, err)
	}

	// Unknown id → ErrNotFound.
	if _, err := st.Spin.Get(ctx, "spin-nope"); err != ErrNotFound {
		t.Errorf("Get(unknown) err = %v; want ErrNotFound", err)
	}
}
