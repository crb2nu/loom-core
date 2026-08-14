package embed

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/crb2nu/loom/pkg/telemetry"
)

// ErrEmbedderUnavailable is returned when the circuit breaker is open: the
// underlying provider has failed repeatedly and is being given a cooldown
// before the next trial. Callers can use errors.Is to detect this (and any
// other embed error) and fall back to a non-vector path.
var ErrEmbedderUnavailable = errors.New("embedder unavailable (circuit breaker open)")

// ResilientConfig tunes the resilient embedder wrapper.
type ResilientConfig struct {
	// Timeout caps each embed call. Zero disables the per-call timeout.
	Timeout time.Duration
	// FailureThreshold is the number of consecutive failures that trips the
	// breaker open. Zero or negative disables the breaker entirely.
	FailureThreshold int
	// Cooldown is how long the breaker stays open before allowing a single
	// half-open trial request through.
	Cooldown time.Duration
}

// DefaultResilientConfig returns conservative defaults: fail fast (3s) so a
// stalled provider cannot head-of-line-block the single MCP stdio transport,
// and open the breaker after 3 consecutive failures for 30s. These were sized
// against an observed Morph embeddings outage where each call blocked ~15-20s
// before a Cloudflare 522, starving unrelated tools sharing the transport.
func DefaultResilientConfig() ResilientConfig {
	return ResilientConfig{
		Timeout:          3 * time.Second,
		FailureThreshold: 3,
		Cooldown:         30 * time.Second,
	}
}

// ResilientEmbedder wraps an Embedder with a per-call timeout and a
// consecutive-failure circuit breaker. When the breaker is open it returns
// ErrEmbedderUnavailable immediately without calling the upstream provider, so
// outages degrade fast instead of hanging.
type ResilientEmbedder struct {
	inner Embedder
	cfg   ResilientConfig
	now   func() time.Time // injectable for tests

	mu          sync.Mutex
	consecFails int
	openUntil   time.Time
}

// Ensure ResilientEmbedder implements Embedder.
var _ Embedder = (*ResilientEmbedder)(nil)

// NewResilientEmbedder wraps inner with the given resilience policy.
func NewResilientEmbedder(inner Embedder, cfg ResilientConfig) *ResilientEmbedder {
	if cfg.FailureThreshold < 0 {
		cfg.FailureThreshold = 0
	}
	return &ResilientEmbedder{inner: inner, cfg: cfg, now: time.Now}
}

// Name returns the underlying embedder name.
func (r *ResilientEmbedder) Name() string { return r.inner.Name() }

// Model returns the underlying model identifier.
func (r *ResilientEmbedder) Model() string { return r.inner.Model() }

// allow reports whether a call may proceed given the breaker state. When the
// cooldown has elapsed it transitions to half-open (clears openUntil but leaves
// consecFails high, so a single failed trial re-opens immediately).
func (r *ResilientEmbedder) allow() bool {
	if r.cfg.FailureThreshold <= 0 {
		return true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.openUntil.IsZero() {
		return true
	}
	if r.now().Before(r.openUntil) {
		return false
	}
	r.openUntil = time.Time{} // half-open trial
	return true
}

// record updates breaker counters after a call completes.
func (r *ResilientEmbedder) record(err error) {
	if r.cfg.FailureThreshold <= 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err == nil {
		r.consecFails = 0
		r.openUntil = time.Time{}
		return
	}
	r.consecFails++
	if r.consecFails >= r.cfg.FailureThreshold {
		// Trip (or stay) open. Leave consecFails at/above the threshold so a
		// failed half-open trial re-opens on the very next failure.
		r.openUntil = r.now().Add(r.cfg.Cooldown)
	}
}

func (r *ResilientEmbedder) snapshotThresholds() (int, int, time.Duration, time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cfg.FailureThreshold, r.consecFails, r.cfg.Cooldown, r.cfg.Timeout
}

// call applies the breaker gate and per-call timeout around fn.
func (r *ResilientEmbedder) call(ctx context.Context, path string, batchSize int, fn func(context.Context) error) error {
	if !r.allow() {
		threshold, consecFails, cooldown, timeout := r.snapshotThresholds()
		telemetry.RecordEmbeddingFallback(ctx, telemetry.EmbeddingFallbackEvent{
			Outcome:          telemetry.EmbeddingOutcomeShortCircuit,
			Reason:           telemetry.EmbeddingReasonCircuitOpen,
			Path:             path,
			PrimaryProvider:  r.Name(),
			PrimaryModel:     r.Model(),
			BatchSize:        batchSize,
			FailureThreshold: threshold,
			ConsecutiveFails: consecFails,
			Cooldown:         cooldown,
			Timeout:          timeout,
		})
		return ErrEmbedderUnavailable
	}
	if r.cfg.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.cfg.Timeout)
		defer cancel()
	}
	err := fn(ctx)
	r.record(err)
	if err != nil && r.cfg.FailureThreshold > 0 {
		threshold, consecFails, cooldown, timeout := r.snapshotThresholds()
		if consecFails >= threshold {
			telemetry.RecordEmbeddingFallback(ctx, telemetry.EmbeddingFallbackEvent{
				Outcome:          telemetry.EmbeddingOutcomeThresholdOpen,
				Reason:           telemetry.EmbeddingReasonFailureThresholdExceeded,
				Path:             path,
				PrimaryProvider:  r.Name(),
				PrimaryModel:     r.Model(),
				BatchSize:        batchSize,
				FailureThreshold: threshold,
				ConsecutiveFails: consecFails,
				Cooldown:         cooldown,
				Timeout:          timeout,
			})
		}
	}
	return err
}

// EmbedQuery implements Embedder.
func (r *ResilientEmbedder) EmbedQuery(ctx context.Context, query string) ([]float64, error) {
	var out []float64
	err := r.call(ctx, telemetry.EmbeddingPathQuery, 1, func(ctx context.Context) error {
		v, e := r.inner.EmbedQuery(ctx, query)
		out = v
		return e
	})
	return out, err
}

// EmbedDocuments implements Embedder.
func (r *ResilientEmbedder) EmbedDocuments(ctx context.Context, texts []string) ([][]float64, error) {
	var out [][]float64
	err := r.call(ctx, telemetry.EmbeddingPathDocuments, len(texts), func(ctx context.Context) error {
		v, e := r.inner.EmbedDocuments(ctx, texts)
		out = v
		return e
	})
	return out, err
}
