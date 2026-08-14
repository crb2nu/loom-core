package daemon

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"gitlab.flexinfer.ai/libs/mcp-go"
)

// TestToolRefreshDebounce_Coalesces verifies that many rapid schedule() calls
// within the debounce window coalesce into a single onFire invocation, fired
// only after the interval has elapsed since the last call.
//
// On a loaded runner a single sleep can overshoot the debounce window, which
// makes a mid-batch fire legitimate. The test measures the actual gap between
// consecutive schedule() calls and only enforces the strict assertions when
// scheduler jitter did not span the window; jittered runs still pin an
// overshoot-derived upper bound on total fires.
func TestToolRefreshDebounce_Coalesces(t *testing.T) {
	t.Parallel()

	var fires atomic.Int32
	// Use a short interval so the test runs quickly; the real daemon interval
	// is 3s but the debounce logic is agnostic.
	interval := 100 * time.Millisecond
	d := newToolRefreshDebounce(interval, func(context.Context) {
		fires.Add(1)
	})

	// Fire 20 rapid schedule() calls, tracking per-iteration gaps. A gap that
	// reaches interval/2 counts as an overshoot: conservative slack, since the
	// timestamps bracket (rather than exactly measure) the schedule-to-schedule
	// gap the timer actually sees.
	var maxGap time.Duration
	overshoots := 0
	prev := time.Now()
	for i := 0; i < 20; i++ {
		d.schedule()
		time.Sleep(interval / 20) // ~5ms; well under the window
		now := time.Now()
		if gap := now.Sub(prev); gap > maxGap {
			maxGap = gap
		}
		if now.Sub(prev) >= interval/2 {
			overshoots++
		}
		prev = now
	}
	midBatch := fires.Load()

	// Each schedule() resets the timer, so nothing may fire mid-batch unless a
	// gap overshot toward the window.
	if midBatch != 0 && overshoots == 0 {
		t.Fatalf("expected 0 fires during rapid-batch (max gap %v < %v), got %d", maxGap, interval/2, midBatch)
	}

	// After the batch goes quiet the pending timer must fire. Poll rather than
	// sleep a fixed amount so a slow runner cannot miss it.
	deadline := time.Now().Add(10 * interval)
	for fires.Load() == midBatch && time.Now().Before(deadline) {
		time.Sleep(interval / 10)
	}
	// Settle long enough to catch any spurious extra fires.
	time.Sleep(interval * 2)

	total := fires.Load()
	if total <= midBatch {
		t.Fatalf("expected a fire after quiet period, still at %d", total)
	}
	if overshoots == 0 && total != 1 {
		t.Fatalf("expected exactly 1 fire after quiet period, got %d", total)
	}
	// Even with jitter, 20 schedule() calls may produce at most one fire per
	// overshot gap plus the final quiet-period fire.
	if total > int32(overshoots)+1 {
		t.Fatalf("coalescing broken: %d fires for 20 schedules with %d overshot gaps (max gap %v)", total, overshoots, maxGap)
	}
}

// TestToolRefreshDebounce_StopCancels verifies stop() prevents a pending fire.
func TestToolRefreshDebounce_StopCancels(t *testing.T) {
	t.Parallel()

	var fires atomic.Int32
	d := newToolRefreshDebounce(30*time.Millisecond, func(context.Context) {
		fires.Add(1)
	})

	d.schedule()
	d.stop()

	time.Sleep(80 * time.Millisecond)

	if got := fires.Load(); got != 0 {
		t.Fatalf("expected 0 fires after stop(), got %d", got)
	}
}

// TestToolRefreshDebounce_StopCancelsRunningAndWaits verifies that shutdown
// cancels an active refresh and joins it before returning.
func TestToolRefreshDebounce_StopCancelsRunningAndWaits(t *testing.T) {
	started := make(chan struct{})
	cancelObserved := make(chan struct{})
	release := make(chan struct{})
	exited := make(chan struct{})
	d := newToolRefreshDebounce(time.Millisecond, func(ctx context.Context) {
		close(started)
		<-ctx.Done()
		close(cancelObserved)
		<-release
		close(exited)
	})

	d.schedule()
	awaitSignal(t, started, "refresh callback to start")

	stopDone := make(chan struct{})
	go func() {
		d.stop()
		close(stopDone)
	}()

	awaitSignal(t, cancelObserved, "refresh callback to observe cancellation")
	d.schedule()
	d.mu.Lock()
	timerWhileStopping := d.timer
	d.mu.Unlock()
	if timerWhileStopping != nil {
		t.Fatal("schedule created a timer after stop began")
	}
	select {
	case <-stopDone:
		t.Fatal("stop returned before the running refresh callback exited")
	default:
	}

	close(release)
	awaitSignal(t, exited, "refresh callback to exit")
	awaitSignal(t, stopDone, "stop to join the refresh callback")

	// A stopped debounce must ignore all later schedule attempts.
	d.schedule()
	d.mu.Lock()
	timerAfterStop := d.timer
	d.mu.Unlock()
	if timerAfterStop != nil {
		t.Fatal("schedule created a timer after stop returned")
	}
}

func TestToolRefreshDebounce_StopCancelsSingleflightFollower(t *testing.T) {
	d := &Daemon{}
	leaderStarted := make(chan struct{})
	releaseLeader := make(chan struct{})
	leaderDone := make(chan struct{})
	go func() {
		defer close(leaderDone)
		key := toolRefreshSingleflightKey(d.cacheSnapshot().revision)
		_, _, _ = d.refreshGroup.Do(key, func() (any, error) {
			close(leaderStarted)
			<-releaseLeader
			return []mcp.Tool{}, nil
		})
	}()
	awaitSignal(t, leaderStarted, "singleflight leader to start")

	followerStarted := make(chan struct{})
	followerErr := make(chan error, 1)
	debounce := newToolRefreshDebounce(time.Millisecond, func(ctx context.Context) {
		close(followerStarted)
		_, err := d.refreshToolCacheDeduplicated(ctx)
		followerErr <- err
	})
	debounce.schedule()
	awaitSignal(t, followerStarted, "singleflight follower to start")

	stopDone := make(chan struct{})
	go func() {
		debounce.stop()
		close(stopDone)
	}()
	awaitSignal(t, stopDone, "debounce follower stop")
	if err := <-followerErr; !errors.Is(err, context.Canceled) {
		t.Fatalf("singleflight follower error = %v, want context.Canceled", err)
	}

	close(releaseLeader)
	awaitSignal(t, leaderDone, "singleflight leader to exit")
}

func awaitSignal(t *testing.T, signal <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s", description)
	}
}

// TestToolRefreshDebounce_NilSafe exercises the nil-receiver guards.
func TestToolRefreshDebounce_NilSafe(t *testing.T) {
	t.Parallel()
	var d *toolRefreshDebounce
	d.schedule()
	d.stop()
}
