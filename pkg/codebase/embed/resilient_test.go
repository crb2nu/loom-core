package embed

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeEmbedder is a controllable Embedder for tests.
type fakeEmbedder struct {
	mu    sync.Mutex
	err   error
	delay time.Duration
	calls int
}

func (f *fakeEmbedder) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *fakeEmbedder) EmbedQuery(ctx context.Context, query string) ([]float64, error) {
	f.mu.Lock()
	f.calls++
	delay, err := f.delay, f.err
	f.mu.Unlock()
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if err != nil {
		return nil, err
	}
	return []float64{1, 2, 3}, nil
}

func (f *fakeEmbedder) EmbedDocuments(ctx context.Context, texts []string) ([][]float64, error) {
	v, err := f.EmbedQuery(ctx, "")
	if err != nil {
		return nil, err
	}
	out := make([][]float64, len(texts))
	for i := range texts {
		out[i] = v
	}
	return out, nil
}

func (f *fakeEmbedder) Name() string  { return "fake" }
func (f *fakeEmbedder) Model() string { return "fake-model" }

func (f *fakeEmbedder) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func TestResilientEmbedder_PassthroughAndNameModel(t *testing.T) {
	inner := &fakeEmbedder{}
	r := NewResilientEmbedder(inner, DefaultResilientConfig())

	if r.Name() != "fake" || r.Model() != "fake-model" {
		t.Fatalf("Name/Model not passed through: %s/%s", r.Name(), r.Model())
	}
	v, err := r.EmbedQuery(context.Background(), "hi")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(v) != 3 {
		t.Fatalf("expected 3-dim vector, got %d", len(v))
	}
}

func TestResilientEmbedder_TimeoutFailsFast(t *testing.T) {
	inner := &fakeEmbedder{delay: 5 * time.Second}
	r := NewResilientEmbedder(inner, ResilientConfig{
		Timeout:          50 * time.Millisecond,
		FailureThreshold: 0, // breaker disabled; isolate the timeout
	})

	start := time.Now()
	_, err := r.EmbedQuery(context.Background(), "slow")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if elapsed > time.Second {
		t.Fatalf("expected fast failure (<1s), took %v", elapsed)
	}
}

func TestResilientEmbedder_BreakerOpensAfterThreshold(t *testing.T) {
	inner := &fakeEmbedder{err: errors.New("upstream 522")}
	r := NewResilientEmbedder(inner, ResilientConfig{
		Timeout:          0,
		FailureThreshold: 3,
		Cooldown:         time.Minute,
	})

	// 3 real failures trip the breaker.
	for i := 0; i < 3; i++ {
		if _, err := r.EmbedQuery(context.Background(), "x"); err == nil {
			t.Fatalf("call %d: expected error", i)
		}
	}
	if got := inner.callCount(); got != 3 {
		t.Fatalf("expected 3 upstream calls before open, got %d", got)
	}

	// Breaker open: next call must short-circuit without hitting upstream.
	_, err := r.EmbedQuery(context.Background(), "x")
	if !errors.Is(err, ErrEmbedderUnavailable) {
		t.Fatalf("expected ErrEmbedderUnavailable, got %v", err)
	}
	if got := inner.callCount(); got != 3 {
		t.Fatalf("breaker should not call upstream, got %d calls", got)
	}
}

func TestResilientEmbedder_HalfOpenRecoversOnSuccess(t *testing.T) {
	inner := &fakeEmbedder{err: errors.New("down")}
	fakeNow := time.Unix(1_700_000_000, 0)
	r := NewResilientEmbedder(inner, ResilientConfig{
		FailureThreshold: 2,
		Cooldown:         30 * time.Second,
	})
	r.now = func() time.Time { return fakeNow }

	// Trip open.
	for i := 0; i < 2; i++ {
		_, _ = r.EmbedQuery(context.Background(), "x")
	}
	if _, err := r.EmbedQuery(context.Background(), "x"); !errors.Is(err, ErrEmbedderUnavailable) {
		t.Fatalf("expected open breaker, got %v", err)
	}

	// Advance past cooldown and let upstream recover.
	fakeNow = fakeNow.Add(31 * time.Second)
	inner.setErr(nil)

	v, err := r.EmbedQuery(context.Background(), "x")
	if err != nil {
		t.Fatalf("half-open trial should succeed, got %v", err)
	}
	if len(v) != 3 {
		t.Fatalf("expected vector after recovery, got %d", len(v))
	}

	// Fully closed now: still-healthy upstream keeps serving.
	if _, err := r.EmbedQuery(context.Background(), "x"); err != nil {
		t.Fatalf("expected closed breaker, got %v", err)
	}
}

func TestResilientEmbedder_HalfOpenReopensOnFailure(t *testing.T) {
	inner := &fakeEmbedder{err: errors.New("still down")}
	fakeNow := time.Unix(1_700_000_000, 0)
	r := NewResilientEmbedder(inner, ResilientConfig{
		FailureThreshold: 2,
		Cooldown:         30 * time.Second,
	})
	r.now = func() time.Time { return fakeNow }

	for i := 0; i < 2; i++ {
		_, _ = r.EmbedQuery(context.Background(), "x")
	}

	// Past cooldown: a single failed half-open trial must reopen immediately.
	fakeNow = fakeNow.Add(31 * time.Second)
	callsBeforeTrial := inner.callCount()
	if _, err := r.EmbedQuery(context.Background(), "x"); err == nil {
		t.Fatal("expected half-open trial to fail")
	}
	if inner.callCount() != callsBeforeTrial+1 {
		t.Fatalf("half-open should make exactly one upstream call")
	}
	// Reopened: next call short-circuits.
	if _, err := r.EmbedQuery(context.Background(), "x"); !errors.Is(err, ErrEmbedderUnavailable) {
		t.Fatalf("expected breaker to reopen after failed trial, got %v", err)
	}
}

func TestResilientEmbedder_DisabledBreakerAlwaysCallsUpstream(t *testing.T) {
	inner := &fakeEmbedder{err: errors.New("down")}
	r := NewResilientEmbedder(inner, ResilientConfig{FailureThreshold: 0})

	for i := 0; i < 5; i++ {
		if _, err := r.EmbedQuery(context.Background(), "x"); errors.Is(err, ErrEmbedderUnavailable) {
			t.Fatal("breaker should be disabled")
		}
	}
	if inner.callCount() != 5 {
		t.Fatalf("expected 5 upstream calls, got %d", inner.callCount())
	}
}
