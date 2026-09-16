package main

import (
	"context"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/lifecycle"
)

func TestAsyncRegistryCleanup_UsesCompletedTimestamp(t *testing.T) {
	t.Parallel()

	registry := newAsyncRegistry()
	now := time.Now()
	oldStart := now.Add(-30 * time.Minute)
	recentCompletion := now.Add(-2 * time.Minute)

	registry.add(&asyncExec{
		ID:          "exec-completed-recently",
		Status:      "completed",
		StartedAt:   oldStart,
		CompletedAt: &recentCompletion,
	})

	registry.cleanup(10 * time.Minute)

	if got := registry.get("exec-completed-recently"); got == nil {
		t.Fatalf("expected completed async exec to remain when completion is within max age")
	}
}

func TestAsyncRegistryCleanup_RemovesOldCompletedTimestamp(t *testing.T) {
	t.Parallel()

	registry := newAsyncRegistry()
	now := time.Now()
	oldCompletion := now.Add(-20 * time.Minute)

	registry.add(&asyncExec{
		ID:          "exec-completed-old",
		Status:      "failed",
		StartedAt:   now.Add(-30 * time.Minute),
		CompletedAt: &oldCompletion,
	})

	registry.cleanup(10 * time.Minute)

	if got := registry.get("exec-completed-old"); got != nil {
		t.Fatalf("expected old completed async exec to be removed")
	}
}

func TestAsyncRegistryCleanup_BackCompatWithoutCompletedTimestamp(t *testing.T) {
	t.Parallel()

	registry := newAsyncRegistry()
	oldStart := time.Now().Add(-20 * time.Minute)
	registry.add(&asyncExec{
		ID:        "legacy-entry",
		Status:    "completed",
		StartedAt: oldStart,
	})

	registry.cleanup(10 * time.Minute)

	if got := registry.get("legacy-entry"); got != nil {
		t.Fatalf("expected legacy completed async exec to be removed based on started time fallback")
	}
}

func TestAsyncDrainWaitsForTerminalPoll(t *testing.T) {
	d := &lifecycle.Drain{}
	d.Admit()  // launching call
	d.Retain() // detached job
	r := newAsyncRegistry()
	ae := &asyncExec{ID: "job", Status: "running", StartedAt: time.Now(), drainDone: d.Release}
	r.add(ae)
	m := &manager{asyncExecs: r}
	_, done := d.Begin()
	d.Release()
	if _, err := m.handleExecPoll(context.Background(), map[string]any{"exec_id": "job"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
		t.Fatal("running job released")
	default:
	}
	ae.Status = "completed"
	if _, err := m.handleExecPoll(context.Background(), map[string]any{"exec_id": "job"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	default:
		t.Fatal("terminal poll did not release job")
	}
	// Repeated polls must not double-release the hold.
	_, _ = m.handleExecPoll(context.Background(), map[string]any{"exec_id": "job"})
}
