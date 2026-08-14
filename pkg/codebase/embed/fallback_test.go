package embed

import (
	"context"
	"errors"
	"testing"

	"github.com/crb2nu/loom/pkg/telemetry"
)

func TestFallbackEmbedder_EmbedDocuments_PrimarySucceeds(t *testing.T) {
	primary := &fakeEmbedder{}
	secondary := &fakeEmbedder{}
	f := NewFallbackEmbedder(primary, secondary)

	if _, err := f.EmbedDocuments(context.Background(), []string{"a", "b"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if secondary.callCount() != 0 {
		t.Fatalf("secondary must not be called when primary succeeds, got %d", secondary.callCount())
	}
}

func TestFallbackEmbedder_EmbedDocuments_FallsBackOnPrimaryError(t *testing.T) {
	primary := &fakeEmbedder{err: errors.New("primary 522")}
	secondary := &fakeEmbedder{}
	f := NewFallbackEmbedder(primary, secondary)
	sink, restore := captureEmbeddingFallbackTelemetry(t)
	defer restore()

	vecs, err := f.EmbedDocuments(context.Background(), []string{"a", "b"})
	if err != nil {
		t.Fatalf("expected fallback to succeed, got %v", err)
	}
	if len(vecs) != 2 {
		t.Fatalf("expected 2 vectors from secondary, got %d", len(vecs))
	}
	if secondary.callCount() != 1 {
		t.Fatalf("expected secondary to be called once, got %d", secondary.callCount())
	}
	events := sink.events()
	if len(events) != 1 {
		t.Fatalf("expected 1 telemetry event, got %d", len(events))
	}
	assertEmbeddingFallbackEvent(t, events[0], telemetry.EmbeddingFallbackEvent{
		Path:              telemetry.EmbeddingPathDocuments,
		Outcome:           telemetry.EmbeddingOutcomeFallbackSuccess,
		Reason:            telemetry.EmbeddingReasonPrimaryError,
		PrimaryProvider:   "fake",
		PrimaryModel:      "fake-model",
		SecondaryProvider: "fake",
		SecondaryModel:    "fake-model",
		BatchSize:         2,
	})
}

func TestFallbackEmbedder_EmbedDocuments_ClassifiesPrimaryOverload(t *testing.T) {
	primary := &fakeEmbedder{err: &HTTPStatusError{Provider: "morph", StatusCode: 503, Body: "model temporarily unavailable"}}
	secondary := &fakeEmbedder{}
	f := NewFallbackEmbedder(primary, secondary)
	sink, restore := captureEmbeddingFallbackTelemetry(t)
	defer restore()

	if _, err := f.EmbedDocuments(context.Background(), []string{"a", "b"}); err != nil {
		t.Fatalf("expected fallback to succeed, got %v", err)
	}
	events := sink.events()
	if len(events) != 1 {
		t.Fatalf("expected 1 telemetry event, got %d", len(events))
	}
	assertEmbeddingFallbackEvent(t, events[0], telemetry.EmbeddingFallbackEvent{
		Path:              telemetry.EmbeddingPathDocuments,
		Outcome:           telemetry.EmbeddingOutcomeFallbackSuccess,
		Reason:            telemetry.EmbeddingReasonProviderOverload,
		PrimaryProvider:   "fake",
		PrimaryModel:      "fake-model",
		SecondaryProvider: "fake",
		SecondaryModel:    "fake-model",
		BatchSize:         2,
	})
}

func TestFallbackEmbedder_EmbedDocuments_ClassifiesCircuitOpenFallback(t *testing.T) {
	primary := &fakeEmbedder{err: ErrEmbedderUnavailable}
	secondary := &fakeEmbedder{}
	f := NewFallbackEmbedder(primary, secondary)
	sink, restore := captureEmbeddingFallbackTelemetry(t)
	defer restore()

	if _, err := f.EmbedDocuments(context.Background(), []string{"a"}); err != nil {
		t.Fatalf("expected fallback to succeed, got %v", err)
	}
	events := sink.events()
	if len(events) != 1 {
		t.Fatalf("expected 1 telemetry event, got %d", len(events))
	}
	assertEmbeddingFallbackEvent(t, events[0], telemetry.EmbeddingFallbackEvent{
		Path:              telemetry.EmbeddingPathDocuments,
		Outcome:           telemetry.EmbeddingOutcomeFallbackSuccess,
		Reason:            telemetry.EmbeddingReasonCircuitOpen,
		PrimaryProvider:   "fake",
		PrimaryModel:      "fake-model",
		SecondaryProvider: "fake",
		SecondaryModel:    "fake-model",
		BatchSize:         1,
	})
}

func TestFallbackEmbedder_EmbedQuery_NeverFallsBack(t *testing.T) {
	primary := &fakeEmbedder{err: errors.New("primary down")}
	secondary := &fakeEmbedder{}
	f := NewFallbackEmbedder(primary, secondary)
	sink, restore := captureEmbeddingFallbackTelemetry(t)
	defer restore()

	// Query path must surface the primary error and never touch the secondary,
	// so the caller degrades to keyword search instead of cross-space vectors.
	if _, err := f.EmbedQuery(context.Background(), "q"); err == nil {
		t.Fatal("expected primary error to propagate on EmbedQuery")
	}
	if secondary.callCount() != 0 {
		t.Fatalf("secondary must never serve queries, got %d calls", secondary.callCount())
	}
	events := sink.events()
	if len(events) != 1 {
		t.Fatalf("expected 1 telemetry event, got %d", len(events))
	}
	assertEmbeddingFallbackEvent(t, events[0], telemetry.EmbeddingFallbackEvent{
		Path:              telemetry.EmbeddingPathQuery,
		Outcome:           telemetry.EmbeddingOutcomeDegraded,
		Reason:            telemetry.EmbeddingReasonQueryPrimaryError,
		PrimaryProvider:   "fake",
		PrimaryModel:      "fake-model",
		SecondaryProvider: "fake",
		SecondaryModel:    "fake-model",
		BatchSize:         1,
	})
}

func TestFallbackEmbedder_EmbedQuery_ClassifiesCircuitOpen(t *testing.T) {
	primary := &fakeEmbedder{err: ErrEmbedderUnavailable}
	secondary := &fakeEmbedder{}
	f := NewFallbackEmbedder(primary, secondary)
	sink, restore := captureEmbeddingFallbackTelemetry(t)
	defer restore()

	if _, err := f.EmbedQuery(context.Background(), "q"); !errors.Is(err, ErrEmbedderUnavailable) {
		t.Fatalf("expected breaker-open error to propagate, got %v", err)
	}
	if secondary.callCount() != 0 {
		t.Fatalf("secondary must never serve queries, got %d calls", secondary.callCount())
	}
	events := sink.events()
	if len(events) != 1 {
		t.Fatalf("expected 1 telemetry event, got %d", len(events))
	}
	assertEmbeddingFallbackEvent(t, events[0], telemetry.EmbeddingFallbackEvent{
		Path:              telemetry.EmbeddingPathQuery,
		Outcome:           telemetry.EmbeddingOutcomeDegraded,
		Reason:            telemetry.EmbeddingReasonCircuitOpen,
		PrimaryProvider:   "fake",
		PrimaryModel:      "fake-model",
		SecondaryProvider: "fake",
		SecondaryModel:    "fake-model",
		BatchSize:         1,
	})
}

func TestFallbackEmbedder_NilSecondary_PropagatesPrimaryError(t *testing.T) {
	primary := &fakeEmbedder{err: errors.New("down")}
	f := NewFallbackEmbedder(primary, nil)
	sink, restore := captureEmbeddingFallbackTelemetry(t)
	defer restore()

	if _, err := f.EmbedDocuments(context.Background(), []string{"a"}); err == nil {
		t.Fatal("expected primary error with nil secondary")
	}
	events := sink.events()
	if len(events) != 1 {
		t.Fatalf("expected 1 telemetry event, got %d", len(events))
	}
	assertEmbeddingFallbackEvent(t, events[0], telemetry.EmbeddingFallbackEvent{
		Path:              telemetry.EmbeddingPathDocuments,
		Outcome:           telemetry.EmbeddingOutcomeDegraded,
		Reason:            telemetry.EmbeddingReasonNoSecondary,
		PrimaryProvider:   "fake",
		PrimaryModel:      "fake-model",
		SecondaryProvider: "none",
		SecondaryModel:    "none",
		BatchSize:         1,
	})
}

func TestFallbackEmbedder_NilSecondary_ReportsNoSecondaryEvenWhenPrimaryCircuitOpen(t *testing.T) {
	primary := &fakeEmbedder{err: ErrEmbedderUnavailable}
	f := NewFallbackEmbedder(primary, nil)
	sink, restore := captureEmbeddingFallbackTelemetry(t)
	defer restore()

	if _, err := f.EmbedDocuments(context.Background(), []string{"a"}); !errors.Is(err, ErrEmbedderUnavailable) {
		t.Fatalf("expected breaker-open error with nil secondary, got %v", err)
	}
	events := sink.events()
	if len(events) != 1 {
		t.Fatalf("expected 1 telemetry event, got %d", len(events))
	}
	assertEmbeddingFallbackEvent(t, events[0], telemetry.EmbeddingFallbackEvent{
		Path:              telemetry.EmbeddingPathDocuments,
		Outcome:           telemetry.EmbeddingOutcomeDegraded,
		Reason:            telemetry.EmbeddingReasonNoSecondary,
		PrimaryProvider:   "fake",
		PrimaryModel:      "fake-model",
		SecondaryProvider: "none",
		SecondaryModel:    "none",
		BatchSize:         1,
	})
}

func TestFallbackEmbedder_NameModelFromPrimary(t *testing.T) {
	primary := &fakeEmbedder{}
	secondary := &fakeEmbedder{}
	f := NewFallbackEmbedder(primary, secondary)
	if f.Name() != "fake" || f.Model() != "fake-model" {
		t.Fatalf("expected primary Name/Model, got %s/%s", f.Name(), f.Model())
	}
}

func TestFallbackEmbedder_EmbedDocuments_RecordsSecondaryFailure(t *testing.T) {
	primary := &fakeEmbedder{err: errors.New("primary 522")}
	secondary := &fakeEmbedder{err: errors.New("secondary 500")}
	f := NewFallbackEmbedder(primary, secondary)
	sink, restore := captureEmbeddingFallbackTelemetry(t)
	defer restore()

	if _, err := f.EmbedDocuments(context.Background(), []string{"a", "b", "c"}); err == nil {
		t.Fatal("expected secondary error")
	}
	events := sink.events()
	if len(events) != 1 {
		t.Fatalf("expected 1 telemetry event, got %d", len(events))
	}
	assertEmbeddingFallbackEvent(t, events[0], telemetry.EmbeddingFallbackEvent{
		Path:              telemetry.EmbeddingPathDocuments,
		Outcome:           telemetry.EmbeddingOutcomeFallbackError,
		Reason:            telemetry.EmbeddingReasonSecondaryError,
		PrimaryProvider:   "fake",
		PrimaryModel:      "fake-model",
		SecondaryProvider: "fake",
		SecondaryModel:    "fake-model",
		BatchSize:         3,
	})
}

func TestFallbackEmbedder_EmbedDocuments_ClassifiesSecondaryOverload(t *testing.T) {
	primary := &fakeEmbedder{err: errors.New("primary 522")}
	secondary := &fakeEmbedder{err: &HTTPStatusError{Provider: "flexinfer", StatusCode: 429, Body: "too many requests"}}
	f := NewFallbackEmbedder(primary, secondary)
	sink, restore := captureEmbeddingFallbackTelemetry(t)
	defer restore()

	if _, err := f.EmbedDocuments(context.Background(), []string{"a"}); err == nil {
		t.Fatal("expected secondary error")
	}
	events := sink.events()
	if len(events) != 1 {
		t.Fatalf("expected 1 telemetry event, got %d", len(events))
	}
	assertEmbeddingFallbackEvent(t, events[0], telemetry.EmbeddingFallbackEvent{
		Path:              telemetry.EmbeddingPathDocuments,
		Outcome:           telemetry.EmbeddingOutcomeFallbackError,
		Reason:            telemetry.EmbeddingReasonProviderOverload,
		PrimaryProvider:   "fake",
		PrimaryModel:      "fake-model",
		SecondaryProvider: "fake",
		SecondaryModel:    "fake-model",
		BatchSize:         1,
	})
}

func assertEmbeddingFallbackEvent(t *testing.T, got, want telemetry.EmbeddingFallbackEvent) {
	t.Helper()
	if got.Path != want.Path ||
		got.Outcome != want.Outcome ||
		got.Reason != want.Reason ||
		got.PrimaryProvider != want.PrimaryProvider ||
		got.PrimaryModel != want.PrimaryModel ||
		got.SecondaryProvider != want.SecondaryProvider ||
		got.SecondaryModel != want.SecondaryModel ||
		got.BatchSize != want.BatchSize {
		t.Fatalf("event mismatch\ngot:  %+v\nwant: %+v", got, want)
	}
}
