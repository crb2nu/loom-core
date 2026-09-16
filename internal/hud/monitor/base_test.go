package monitor

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

func TestBaseMonitor_SnapshotAndUpdate(t *testing.T) {
	var b BaseMonitor[int]
	b.InitBase(nil, nil, "test")

	// Zero value snapshot.
	if got := b.Snapshot(); got != 0 {
		t.Fatalf("expected 0, got %d", got)
	}

	b.Update(42)
	if got := b.Snapshot(); got != 42 {
		t.Fatalf("expected 42, got %d", got)
	}
}

func TestBaseMonitor_OnRefreshCallback(t *testing.T) {
	var b BaseMonitor[string]
	b.InitBase(nil, nil, "test")

	var received string
	b.OnRefresh(func(val string) { received = val })

	b.Update("hello")
	if received != "hello" {
		t.Fatalf("expected callback with 'hello', got %q", received)
	}
}

func TestBaseMonitor_StopIdempotent(t *testing.T) {
	var b BaseMonitor[int]
	b.InitBase(nil, nil, "test")

	b.Stop()
	b.Stop() // should not panic
}

func TestBaseMonitor_StartAndStop(t *testing.T) {
	var b BaseMonitor[int]
	b.InitBase(nil, nil, "test")

	var calls atomic.Int32
	refreshFn := func(_ context.Context) (int, error) {
		calls.Add(1)
		return int(calls.Load()), nil
	}

	b.Start(50*time.Millisecond, refreshFn)

	// Wait for at least the initial refresh + one poll cycle.
	time.Sleep(200 * time.Millisecond)
	b.Stop()

	// Allow goroutines to finish.
	time.Sleep(50 * time.Millisecond)

	if c := calls.Load(); c < 2 {
		t.Fatalf("expected at least 2 refresh calls, got %d", c)
	}
	if got := b.Snapshot(); got == 0 {
		t.Fatal("expected non-zero snapshot after refresh")
	}
}

func TestBaseMonitor_BackoffOnErrors(t *testing.T) {
	var b BaseMonitor[int]
	b.InitBase(nil, nil, "test")

	var calls atomic.Int32
	refreshFn := func(_ context.Context) (int, error) {
		n := calls.Add(1)
		if n <= 3 {
			return 0, errors.New("transient error")
		}
		return int(n), nil
	}

	b.Start(20*time.Millisecond, refreshFn)

	// With backoff, after 3 errors the monitor skips ticks. Eventually
	// it should recover. Give it enough time.
	time.Sleep(500 * time.Millisecond)
	b.Stop()

	time.Sleep(50 * time.Millisecond)

	if got := b.Snapshot(); got == 0 {
		t.Fatal("expected recovery after transient errors")
	}
}

// TestRefreshBackoff_LongOutageStaysVisibleWithoutFlooding pins the loop's
// logging contract over a sustained outage: three WARNs, then silence until
// one "still failing" summary per backoffSummaryEvery, ticks skipped up to
// the effective-interval cap, and a single recovery line with the outage
// duration. Driven with a fake clock so it is deterministic.
func TestRefreshBackoff_LongOutageStaysVisibleWithoutFlooding(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	origSummary, origMax := backoffSummaryEvery, backoffMaxEffectiveInterval
	backoffSummaryEvery, backoffMaxEffectiveInterval = 10*time.Minute, 5*time.Minute
	t.Cleanup(func() { backoffSummaryEvery, backoffMaxEffectiveInterval = origSummary, origMax })

	const interval = 15 * time.Second
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	var s refreshBackoff
	upstream := errors.New("list sessions: connection refused")

	var skips []int
	for i := 0; i < 60; i++ {
		skip := s.failed(logger, interval, upstream, now)
		skips = append(skips, skip)
		now = now.Add(time.Duration(skip+1) * interval)
	}
	if got := strings.Count(buf.String(), "refresh error"); got != backoffWarnCount {
		t.Fatalf("initial warnings = %d, want %d\n%s", got, backoffWarnCount, buf.String())
	}
	// 60 failures with a 5-minute effective cap span well over an hour of
	// fake time; summaries must appear, spaced by backoffSummaryEvery.
	elapsed := now.Sub(time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC))
	summaries := strings.Count(buf.String(), "refresh still failing")
	wantMax := int(elapsed/backoffSummaryEvery) + 1
	if summaries == 0 || summaries > wantMax {
		t.Fatalf("summaries = %d over %s, want between 1 and %d\n%s", summaries, elapsed, wantMax, buf.String())
	}
	// Skips grow with consecutive failures and cap at the effective-interval
	// ceiling: (5m / 15s) - 1 = 19 ticks skipped, never more.
	if skips[0] != 0 || skips[1] != 1 || skips[2] != 2 {
		t.Fatalf("early skips = %v, want 0,1,2,…", skips[:3])
	}
	maxSkip := int(backoffMaxEffectiveInterval/interval) - 1
	for i, skip := range skips {
		if skip > maxSkip {
			t.Fatalf("skip[%d] = %d exceeds cap %d", i, skip, maxSkip)
		}
	}
	if skips[len(skips)-1] != maxSkip {
		t.Fatalf("final skip = %d, want the cap %d", skips[len(skips)-1], maxSkip)
	}

	buf.Reset()
	s.recovered(logger, now)
	if !strings.Contains(buf.String(), "refresh recovered") || !strings.Contains(buf.String(), "degraded_for=") {
		t.Fatalf("recovery line missing duration: %s", buf.String())
	}
	if s.consecutive != 0 {
		t.Fatalf("recovered must reset state, consecutive = %d", s.consecutive)
	}
	// A recovery with no prior failure is silent.
	buf.Reset()
	s.recovered(logger, now)
	if buf.Len() != 0 {
		t.Fatalf("recovery without an outage must not log: %s", buf.String())
	}
}

func TestBaseMonitor_NoopTracerWhenNil(t *testing.T) {
	var b BaseMonitor[int]
	b.InitBase(nil, nil, "test")

	// The tracer should be a noop tracer (not nil).
	if b.Tracer == nil {
		t.Fatal("expected non-nil tracer")
	}

	// Verify it produces noop spans by starting one.
	_, span := b.Tracer.Start(context.Background(), "test-span")
	if span.SpanContext().IsValid() {
		t.Fatal("expected noop span to have invalid SpanContext")
	}
	span.End()
}

// recordingTracer is a test tracer that records started span names. Start is
// called from the monitor's concurrent refresh goroutines (initial refresh +
// poll loop), so span recording must be synchronized — a plain slice append
// tripped the race detector under `go test -race`.
type recordingTracer struct {
	noop.Tracer
	mu    sync.Mutex
	spans []string
}

func (rt *recordingTracer) Start(ctx context.Context, name string, _ ...trace.SpanStartOption) (context.Context, trace.Span) {
	rt.mu.Lock()
	rt.spans = append(rt.spans, name)
	rt.mu.Unlock()
	return rt.Tracer.Start(ctx, name)
}

// snapshot returns a copy of the recorded span names for race-free assertions.
func (rt *recordingTracer) snapshot() []string {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	out := make([]string, len(rt.spans))
	copy(out, rt.spans)
	return out
}

func TestBaseMonitor_OTelSpanPerRefresh(t *testing.T) {
	var b BaseMonitor[int]
	tracer := &recordingTracer{}
	b.InitBase(nil, tracer, "my-monitor")

	refreshFn := func(_ context.Context) (int, error) {
		return 1, nil
	}

	b.Start(50*time.Millisecond, refreshFn)
	time.Sleep(200 * time.Millisecond)
	b.Stop()
	time.Sleep(50 * time.Millisecond)

	// Should have at least the initial refresh span + poll spans.
	spans := tracer.snapshot()
	found := false
	for _, name := range spans {
		if name == "my-monitor.refresh" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected span named 'my-monitor.refresh', got spans: %v", spans)
	}
}

func TestBaseMonitor_OTelSpanOnError(t *testing.T) {
	var b BaseMonitor[int]
	tracer := &recordingTracer{}
	b.InitBase(nil, tracer, "err-monitor")

	refreshFn := func(_ context.Context) (int, error) {
		return 0, errors.New("boom")
	}

	// Call doRefresh directly to verify span is recorded on error.
	_, err := b.doRefresh(refreshFn)
	if err == nil {
		t.Fatal("expected error")
	}

	spans := tracer.snapshot()
	found := false
	for _, name := range spans {
		if name == "err-monitor.refresh" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected span on error path, got spans: %v", spans)
	}
}

func TestBaseMonitor_StopDuringPoll(t *testing.T) {
	var b BaseMonitor[int]
	b.InitBase(nil, nil, "test")

	refreshFn := func(_ context.Context) (int, error) {
		return 1, nil
	}

	b.Start(10*time.Millisecond, refreshFn)
	// Stop almost immediately.
	time.Sleep(5 * time.Millisecond)
	b.Stop()

	// Ensure goroutines have exited cleanly.
	time.Sleep(50 * time.Millisecond)
}

func TestBaseMonitor_LockUnlock(t *testing.T) {
	var b BaseMonitor[string]
	b.InitBase(nil, nil, "test")

	b.Lock()
	b.SetSnapshot("direct")
	b.Unlock()

	b.RLock()
	got := b.GetSnapshot()
	b.RUnlock()

	if got != "direct" {
		t.Fatalf("expected 'direct', got %q", got)
	}
}

func TestBaseMonitor_StartLoopAndStop(t *testing.T) {
	var b BaseMonitor[int]
	b.InitBase(nil, nil, "test")

	var calls atomic.Int32
	refreshFn := func() error {
		n := calls.Add(1)
		b.Update(int(n))
		return nil
	}

	b.StartLoop(50*time.Millisecond, refreshFn)

	// Wait for initial refresh + at least one poll cycle.
	time.Sleep(200 * time.Millisecond)
	b.Stop()
	time.Sleep(50 * time.Millisecond)

	if c := calls.Load(); c < 2 {
		t.Fatalf("expected at least 2 refresh calls, got %d", c)
	}
	if got := b.Snapshot(); got == 0 {
		t.Fatal("expected non-zero snapshot after refresh")
	}
}

func TestBaseMonitor_StartLoopBackoffOnErrors(t *testing.T) {
	var b BaseMonitor[int]
	b.InitBase(nil, nil, "test")

	var calls atomic.Int32
	refreshFn := func() error {
		n := calls.Add(1)
		if n <= 3 {
			return errors.New("transient error")
		}
		b.Update(int(n))
		return nil
	}

	b.StartLoop(20*time.Millisecond, refreshFn)

	// With backoff, after 3 errors the monitor skips ticks. Eventually
	// it should recover. Give it enough time.
	time.Sleep(500 * time.Millisecond)
	b.Stop()
	time.Sleep(50 * time.Millisecond)

	if got := b.Snapshot(); got == 0 {
		t.Fatal("expected recovery after transient errors")
	}
}
