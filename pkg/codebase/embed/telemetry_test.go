package embed

import (
	"context"
	"sync"
	"testing"

	"github.com/crb2nu/loom/pkg/telemetry"
)

type captureEmbeddingFallbackSink struct {
	mu       sync.Mutex
	recorded []telemetry.EmbeddingFallbackEvent
}

func captureEmbeddingFallbackTelemetry(t *testing.T) (*captureEmbeddingFallbackSink, func()) {
	t.Helper()
	sink := &captureEmbeddingFallbackSink{}
	restore := telemetry.SetEmbeddingFallbackSinkForTest(sink)
	return sink, restore
}

func (s *captureEmbeddingFallbackSink) RecordEmbeddingFallback(_ context.Context, event telemetry.EmbeddingFallbackEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recorded = append(s.recorded, event)
}

func (s *captureEmbeddingFallbackSink) events() []telemetry.EmbeddingFallbackEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]telemetry.EmbeddingFallbackEvent, len(s.recorded))
	copy(out, s.recorded)
	return out
}
