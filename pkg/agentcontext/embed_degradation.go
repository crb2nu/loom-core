package agentcontext

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/crb2nu/loom/pkg/env"
)

// ErrEmbedderDegraded is the sentinel for the fail-closed embedder gate:
// the write-path fallback (persisting deterministic vectors when the embedder
// is down) has exceeded its bounded thresholds and the service now refuses the
// write instead of silently degrading the vector store. Callers detect it with
// errors.Is and surface the degradation to the operator.
//
// This bounds — it does not replace — the best-effort decoupling from !804:
// a sporadic embed failure still never drops a write. Only a systemic outage
// (fallback ratio above EmbedDegradationConfig.MaxFallbackRatio, or embedding
// failing continuously past MaxContinuousFailure, e.g. the circuit breaker
// held open) trips the gate, because at that point every "successful" write is
// an unsearchable hash vector poisoning recall.
var ErrEmbedderDegraded = errors.New("embedder degraded: fallback vectors exceed fail-closed thresholds")

// minContinuousFailureSamples is the least number of consecutive failed embed
// attempts required before the continuous-failure gate may trip. It exists so
// a single isolated failure followed by a long idle gap can never read as a
// 30-minute outage.
const minContinuousFailureSamples = 2

// EmbedDegradationConfig tunes the fail-closed thresholds for the write-path
// embed fallback. A non-positive MaxFallbackRatio or MaxContinuousFailure
// disables that gate (used by tests; the env loader only produces positive
// values).
type EmbedDegradationConfig struct {
	// MaxFallbackRatio is the windowed fraction of write-path embed attempts
	// that may fall back to deterministic vectors before writes fail closed.
	MaxFallbackRatio float64
	// MinSamples is how many recorded attempts the window needs before the
	// ratio gate is meaningful; below it the gate never trips.
	MinSamples int
	// MaxContinuousFailure bounds how long embedding may fail without a single
	// success (the observable effect of the circuit breaker staying open)
	// before writes fail closed.
	MaxContinuousFailure time.Duration
	// WindowSize is the number of recent attempts the ratio is computed over.
	WindowSize int
}

// DefaultEmbedDegradationConfig returns the thresholds from the council plan:
// fail closed when more than 20% of the last 50 write-path embed attempts fell
// back (with at least 10 samples), or when embedding has failed continuously
// for over 30 minutes.
func DefaultEmbedDegradationConfig() EmbedDegradationConfig {
	return EmbedDegradationConfig{
		MaxFallbackRatio:     0.20,
		MinSamples:           10,
		MaxContinuousFailure: 30 * time.Minute,
		WindowSize:           50,
	}
}

// embedDegradationConfigFromEnv applies optional env overrides to the
// defaults:
//
//	AGENT_CONTEXT_EMBED_MAX_FALLBACK_RATIO      float in (0,1], ratio gate threshold (default 0.20)
//	AGENT_CONTEXT_EMBED_DEGRADED_MIN_SAMPLES    int, samples before the ratio gate arms (default 10)
//	AGENT_CONTEXT_EMBED_MAX_CONTINUOUS_FAILURE  Go duration, continuous-failure bound (default 30m)
//	AGENT_CONTEXT_EMBED_DEGRADED_WINDOW         int, ratio window size (default 50)
func embedDegradationConfigFromEnv() EmbedDegradationConfig {
	c := DefaultEmbedDegradationConfig()
	c.MaxFallbackRatio = env.Float("AGENT_CONTEXT_EMBED_MAX_FALLBACK_RATIO", c.MaxFallbackRatio)
	if v := env.IntWithZero("AGENT_CONTEXT_EMBED_DEGRADED_MIN_SAMPLES", 0); v > 0 {
		c.MinSamples = v
	}
	if v := env.Duration("AGENT_CONTEXT_EMBED_MAX_CONTINUOUS_FAILURE", 0); v > 0 {
		c.MaxContinuousFailure = v
	}
	if v := env.IntWithZero("AGENT_CONTEXT_EMBED_DEGRADED_WINDOW", 0); v > 0 {
		c.WindowSize = v
	}
	return c
}

// EmbedderDegradedError carries the tripped threshold's evidence. It unwraps
// to ErrEmbedderDegraded.
type EmbedderDegradedError struct {
	// Reason is "fallback_ratio" or "continuous_failure".
	Reason string
	// Ratio is the windowed fallback ratio at rejection time.
	Ratio float64
	// Samples is how many attempts the window held.
	Samples int
	// FailingFor is the continuous-failure duration (continuous_failure only).
	FailingFor time.Duration
}

func (e *EmbedderDegradedError) Error() string {
	switch e.Reason {
	case "continuous_failure":
		return fmt.Sprintf(
			"embedder degraded: embedding has failed continuously for %s (fallback on %.0f%% of the last %d writes)",
			e.FailingFor.Round(time.Second), e.Ratio*100, e.Samples)
	default:
		return fmt.Sprintf(
			"embedder degraded: fallback vectors on %.0f%% of the last %d writes exceed the fail-closed threshold",
			e.Ratio*100, e.Samples)
	}
}

func (e *EmbedderDegradedError) Unwrap() error { return ErrEmbedderDegraded }

// EmbedDegradationTracker observes write-path embed outcomes (context and task
// document embeds) and decides when the fallback path must fail closed. One
// tracker is shared across all write paths so the ratio reflects the service,
// not one collection. All methods are nil-receiver safe: a nil tracker records
// nothing and never reports degradation, preserving pre-gate behavior for
// directly-constructed sub-services in tests.
type EmbedDegradationTracker struct {
	cfg EmbedDegradationConfig
	now func() time.Time // injectable for tests

	mu        sync.Mutex
	window    []bool // ring buffer: true = attempt fell back
	next      int
	filled    int
	fallbacks int
	// failingSince is the time of the first failure in the current
	// consecutive-failure streak; zeroed by any success.
	failingSince time.Time
	consecFails  int
}

// NewEmbedDegradationTracker builds a tracker, normalizing degenerate config.
func NewEmbedDegradationTracker(cfg EmbedDegradationConfig) *EmbedDegradationTracker {
	if cfg.WindowSize < 1 {
		cfg.WindowSize = DefaultEmbedDegradationConfig().WindowSize
	}
	if cfg.MinSamples < 1 {
		cfg.MinSamples = 1
	}
	if cfg.MinSamples > cfg.WindowSize {
		cfg.MinSamples = cfg.WindowSize
	}
	return &EmbedDegradationTracker{
		cfg:    cfg,
		now:    time.Now,
		window: make([]bool, cfg.WindowSize),
	}
}

// RecordOutcome records one write-path embed attempt. fallback is true when
// the attempt could not produce usable vectors and the caller would persist
// deterministic fallback vectors instead. Outcomes are per embed call, not per
// entry, so batch size cannot skew the ratio.
func (t *EmbedDegradationTracker) RecordOutcome(fallback bool) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.filled == len(t.window) {
		if t.window[t.next] {
			t.fallbacks--
		}
	} else {
		t.filled++
	}
	t.window[t.next] = fallback
	if fallback {
		t.fallbacks++
		t.consecFails++
		if t.failingSince.IsZero() {
			t.failingSince = t.now()
		}
	} else {
		t.consecFails = 0
		t.failingSince = time.Time{}
	}
	t.next = (t.next + 1) % len(t.window)
}

// Ratio returns the windowed fallback ratio and the number of samples backing
// it. A nil or empty tracker reports 0, 0.
func (t *EmbedDegradationTracker) Ratio() (float64, int) {
	if t == nil {
		return 0, 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.ratioLocked()
}

func (t *EmbedDegradationTracker) ratioLocked() (float64, int) {
	if t.filled == 0 {
		return 0, 0
	}
	return float64(t.fallbacks) / float64(t.filled), t.filled
}

// Degraded reports whether the fail-closed thresholds are currently exceeded.
// Nil means the fallback path may proceed.
func (t *EmbedDegradationTracker) Degraded() *EmbedderDegradedError {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	ratio, samples := t.ratioLocked()
	if t.cfg.MaxFallbackRatio > 0 && samples >= t.cfg.MinSamples && ratio > t.cfg.MaxFallbackRatio {
		return &EmbedderDegradedError{Reason: "fallback_ratio", Ratio: ratio, Samples: samples}
	}
	if t.cfg.MaxContinuousFailure > 0 && !t.failingSince.IsZero() && t.consecFails >= minContinuousFailureSamples {
		if d := t.now().Sub(t.failingSince); d > t.cfg.MaxContinuousFailure {
			return &EmbedderDegradedError{Reason: "continuous_failure", Ratio: ratio, Samples: samples, FailingFor: d}
		}
	}
	return nil
}

// recordEmbedWriteOutcome is the single hook the write paths call after an
// embed attempt: it records the outcome on the shared tracker, publishes the
// cumulative attempt/fallback counters to m, and — only when this attempt fell
// back — returns the typed degraded error if the fail-closed thresholds are
// exceeded. A successful attempt never returns an error, so a recovered
// embedder immediately unblocks writes regardless of tracker state. Both t and
// m may be nil.
func recordEmbedWriteOutcome(t *EmbedDegradationTracker, m *Metrics, fallback bool) error {
	if m != nil {
		m.EmbedWriteAttempts.Add(1)
		if fallback {
			m.EmbedFallbackWrites.Add(1)
		}
	}
	if t == nil {
		return nil
	}
	t.RecordOutcome(fallback)
	if !fallback {
		return nil
	}
	if derr := t.Degraded(); derr != nil {
		if m != nil {
			m.EmbedDegradedRejections.Add(1)
		}
		return derr
	}
	return nil
}
