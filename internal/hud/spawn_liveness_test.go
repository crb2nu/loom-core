package hud

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/crb2nu/loom/internal/hud/bridge"
)

// testOrchestrator returns a minimal orchestrator whose only used field is the
// logger, sufficient for exercising runLivenessWatcher in isolation.
func testOrchestrator() *SpawnOrchestrator {
	return &SpawnOrchestrator{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

// TestRunLivenessWatcher_TripsOnStall is the core regression guard for the
// zombie-pod wedge: when a streaming spawn produces no output for longer than
// the stall timeout, the watcher must cancel the exec context with
// errSpawnStalled as the cause and record a "stalled" error on the telemetry.
func TestRunLivenessWatcher_TripsOnStall(t *testing.T) {
	o := testOrchestrator()
	acc := bridge.NewSpawnTelemetryAccumulator() // never Touch()ed → goes stale

	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	done := make(chan struct{})
	defer close(done)

	go o.runLivenessWatcher(ctx, "spawn-stall", acc, 80*time.Millisecond, cancel, done)

	select {
	case <-ctx.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("liveness watcher did not trip within 3s on a stalled spawn")
	}

	if cause := context.Cause(ctx); !errors.Is(cause, errSpawnStalled) {
		t.Fatalf("cancellation cause = %v, want errSpawnStalled", cause)
	}

	snap := acc.Snapshot()
	var foundStalled bool
	for _, e := range snap.Errors {
		if e.Type == "stalled" {
			foundStalled = true
			break
		}
	}
	if !foundStalled {
		t.Fatalf("expected a 'stalled' telemetry error, got %+v", snap.Errors)
	}
}

// TestRunLivenessWatcher_StaysAliveWhileActive verifies the watcher does NOT
// trip while the agent keeps emitting output (each line Touch()es the
// accumulator) — the false-positive guard that protects healthy long-running
// implement spawns.
func TestRunLivenessWatcher_StaysAliveWhileActive(t *testing.T) {
	o := testOrchestrator()
	acc := bridge.NewSpawnTelemetryAccumulator()

	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	done := make(chan struct{})

	// stallTimeout comfortably larger than the touch cadence below.
	go o.runLivenessWatcher(ctx, "spawn-active", acc, 1*time.Second, cancel, done)

	// Keep the spawn "alive" for several poll cycles.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		acc.Touch()
		time.Sleep(50 * time.Millisecond)
	}
	close(done)

	if cause := context.Cause(ctx); cause != nil {
		t.Fatalf("watcher tripped on an active spawn: cause=%v", cause)
	}
}

// TestRunLivenessWatcher_ExitsOnDone verifies the watcher returns promptly when
// the caller closes done (exec finished) without ever cancelling exec.
func TestRunLivenessWatcher_ExitsOnDone(t *testing.T) {
	o := testOrchestrator()
	acc := bridge.NewSpawnTelemetryAccumulator()

	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	done := make(chan struct{})
	exited := make(chan struct{})

	go func() {
		o.runLivenessWatcher(ctx, "spawn-done", acc, 1*time.Hour, cancel, done)
		close(exited)
	}()

	close(done)
	select {
	case <-exited:
	case <-time.After(2 * time.Second):
		t.Fatal("watcher did not exit after done was closed")
	}
	if cause := context.Cause(ctx); cause != nil {
		t.Fatalf("watcher should not cancel exec on done: cause=%v", cause)
	}
}
