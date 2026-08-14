package telemetry

import (
	"context"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

const (
	EmbeddingPathQuery     = "query"
	EmbeddingPathDocuments = "documents"

	EmbeddingOutcomeFallbackSuccess = "fallback_success"
	EmbeddingOutcomeFallbackError   = "fallback_error"
	EmbeddingOutcomeDegraded        = "degraded"
	EmbeddingOutcomeShortCircuit    = "short_circuit"
	EmbeddingOutcomeThresholdOpen   = "threshold_open"

	EmbeddingReasonPrimaryError             = "primary_error"
	EmbeddingReasonPrimaryUnavailable       = "primary_unavailable"
	EmbeddingReasonNoSecondary              = "no_secondary"
	EmbeddingReasonSecondaryError           = "secondary_error"
	EmbeddingReasonSecondaryUnavailable     = "secondary_unavailable"
	EmbeddingReasonQueryPrimaryError        = "query_primary_error"
	EmbeddingReasonProviderOverload         = "provider_overload"
	EmbeddingReasonCircuitOpen              = "circuit_open"
	EmbeddingReasonFailureThresholdExceeded = "failure_threshold_exceeded"
)

// EmbeddingFallbackEvent is emitted when embedding behavior degrades from the
// normal primary-provider path: fallback provider use, keyword degradation, or a
// breaker threshold/open short-circuit.
type EmbeddingFallbackEvent struct {
	Path              string
	Outcome           string
	Reason            string
	PrimaryProvider   string
	PrimaryModel      string
	SecondaryProvider string
	SecondaryModel    string
	BatchSize         int
	FailureThreshold  int
	ConsecutiveFails  int
	Cooldown          time.Duration
	Timeout           time.Duration
}

// EmbeddingFallbackSink receives degraded embedding telemetry. The default sink
// emits OTel metrics; tests can install a capture sink with
// SetEmbeddingFallbackSinkForTest.
type EmbeddingFallbackSink interface {
	RecordEmbeddingFallback(ctx context.Context, event EmbeddingFallbackEvent)
}

var embeddingFallbackSink = struct {
	sync.RWMutex
	sink EmbeddingFallbackSink
}{sink: newOTelEmbeddingFallbackSink()}

// RecordEmbeddingFallback records reason-coded degraded embedding telemetry.
func RecordEmbeddingFallback(ctx context.Context, event EmbeddingFallbackEvent) {
	embeddingFallbackSink.RLock()
	sink := embeddingFallbackSink.sink
	embeddingFallbackSink.RUnlock()
	if sink == nil {
		return
	}
	sink.RecordEmbeddingFallback(ctx, normalizeEmbeddingFallbackEvent(event))
}

// SetEmbeddingFallbackSinkForTest installs a process-local sink and returns a
// restore function. It is intended for tests only.
func SetEmbeddingFallbackSinkForTest(sink EmbeddingFallbackSink) func() {
	embeddingFallbackSink.Lock()
	prev := embeddingFallbackSink.sink
	embeddingFallbackSink.sink = sink
	embeddingFallbackSink.Unlock()
	return func() {
		embeddingFallbackSink.Lock()
		embeddingFallbackSink.sink = prev
		embeddingFallbackSink.Unlock()
	}
}

func normalizeEmbeddingFallbackEvent(event EmbeddingFallbackEvent) EmbeddingFallbackEvent {
	if event.Path == "" {
		event.Path = "unknown"
	}
	if event.Outcome == "" {
		event.Outcome = "unknown"
	}
	if event.Reason == "" {
		event.Reason = "unknown"
	}
	if event.PrimaryProvider == "" {
		event.PrimaryProvider = "unknown"
	}
	if event.PrimaryModel == "" {
		event.PrimaryModel = "unknown"
	}
	if event.SecondaryProvider == "" {
		event.SecondaryProvider = "none"
	}
	if event.SecondaryModel == "" {
		event.SecondaryModel = "none"
	}
	return event
}

type otelEmbeddingFallbackSink struct {
	events     metric.Int64Counter
	batches    metric.Int64Histogram
	thresholds metric.Int64Counter
}

func newOTelEmbeddingFallbackSink() *otelEmbeddingFallbackSink {
	meter := otel.GetMeterProvider().Meter("github.com/crb2nu/loom/pkg/telemetry")
	events, _ := meter.Int64Counter(
		"loom_embedding_fallback_events_total",
		metric.WithDescription("Total degraded embedding fallback events by reason and outcome."),
	)
	batches, _ := meter.Int64Histogram(
		"loom_embedding_fallback_batch_size",
		metric.WithDescription("Batch size observed when embedding fallback or degradation occurs."),
	)
	thresholds, _ := meter.Int64Counter(
		"loom_embedding_fallback_threshold_events_total",
		metric.WithDescription("Total embedding breaker threshold/open events."),
	)
	return &otelEmbeddingFallbackSink{events: events, batches: batches, thresholds: thresholds}
}

func (s *otelEmbeddingFallbackSink) RecordEmbeddingFallback(ctx context.Context, event EmbeddingFallbackEvent) {
	attrs := []attribute.KeyValue{
		attribute.String("embedding.path", event.Path),
		attribute.String("embedding.outcome", event.Outcome),
		attribute.String("embedding.reason", event.Reason),
		attribute.String("embedding.primary_provider", event.PrimaryProvider),
		attribute.String("embedding.primary_model", event.PrimaryModel),
		attribute.String("embedding.secondary_provider", event.SecondaryProvider),
		attribute.String("embedding.secondary_model", event.SecondaryModel),
	}
	if event.FailureThreshold > 0 {
		attrs = append(attrs, attribute.Int("embedding.failure_threshold", event.FailureThreshold))
	}
	if event.Cooldown > 0 {
		attrs = append(attrs, attribute.Int64("embedding.cooldown_ms", event.Cooldown.Milliseconds()))
	}
	if event.Timeout > 0 {
		attrs = append(attrs, attribute.Int64("embedding.timeout_ms", event.Timeout.Milliseconds()))
	}
	opts := metric.WithAttributes(attrs...)
	s.events.Add(ctx, 1, opts)
	if event.BatchSize > 0 {
		s.batches.Record(ctx, int64(event.BatchSize), opts)
	}
	if event.FailureThreshold > 0 || event.ConsecutiveFails > 0 {
		s.thresholds.Add(ctx, 1, opts)
	}
}
