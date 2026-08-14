package embed

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/httpclient"
	"github.com/crb2nu/loom/pkg/telemetry"
)

type capturedRequestMetric struct {
	provider string
	elapsed  time.Duration
	failed   bool
}

type requestMetricRecorder struct {
	mu      sync.Mutex
	metrics []capturedRequestMetric
}

func (r *requestMetricRecorder) RecordEmbedderRequest(_ context.Context, provider string, elapsed time.Duration, failed bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.metrics = append(r.metrics, capturedRequestMetric{provider: provider, elapsed: elapsed, failed: failed})
}

func (r *requestMetricRecorder) snapshot() []capturedRequestMetric {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]capturedRequestMetric(nil), r.metrics...)
}

func TestMorphClientRecordsSuccessLatency(t *testing.T) {
	recorder := captureRequestMetrics(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"data":[{"embedding":[0.1,0.2]}]}`)
	}))
	defer server.Close()

	client := NewMorphClient(httpclient.NewDefault(), server.URL, "key", "model")
	if _, err := client.EmbedQuery(context.Background(), "secret request content"); err != nil {
		t.Fatalf("EmbedQuery: %v", err)
	}

	assertSingleRequestMetric(t, recorder.snapshot(), telemetry.EmbedderProviderMorph, false)
}

func TestFlexInferClientRecordsFailureLatency(t *testing.T) {
	recorder := captureRequestMetrics(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "sensitive provider error", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	client := NewFlexInferClient(httpclient.NewDefault(), server.URL, "", "model")
	if _, err := client.EmbedDocuments(context.Background(), []string{"secret request content"}); err == nil {
		t.Fatal("EmbedDocuments returned nil error")
	}

	assertSingleRequestMetric(t, recorder.snapshot(), telemetry.EmbedderProviderFlexInfer, true)
}

func captureRequestMetrics(t *testing.T) *requestMetricRecorder {
	t.Helper()
	recorder := &requestMetricRecorder{}
	restore := telemetry.SetEmbedderRequestRecorderForTest(recorder)
	t.Cleanup(restore)
	return recorder
}

func assertSingleRequestMetric(t *testing.T, got []capturedRequestMetric, provider string, failed bool) {
	t.Helper()
	if len(got) != 1 {
		t.Fatalf("got %d metrics, want 1: %+v", len(got), got)
	}
	if got[0].provider != provider || got[0].failed != failed {
		t.Fatalf("metric = %+v, want provider=%q failed=%t", got[0], provider, failed)
	}
	if got[0].elapsed < 0 {
		t.Fatalf("elapsed = %v, want non-negative", got[0].elapsed)
	}
}
