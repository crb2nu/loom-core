package telemetry

import (
	"context"
	"sync"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	metricdata "go.opentelemetry.io/otel/sdk/metric/metricdata"
)

type capturedEmbedderRequest struct {
	provider string
	elapsed  time.Duration
	failed   bool
}

func TestOTelEmbedderRequestRecorderRecordsSymmetricLatencyAndFailuresOnly(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	recorder := newOTelEmbedderRequestRecorderWithMeter(provider.Meter("test"))

	recorder.RecordEmbedderRequest(context.Background(), EmbedderProviderMorph, time.Second, false)
	recorder.RecordEmbedderRequest(context.Background(), EmbedderProviderMorph, 2*time.Second, true)

	var collected metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &collected); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	var latencyCount, failureCount uint64
	for _, scope := range collected.ScopeMetrics {
		for _, observed := range scope.Metrics {
			switch observed.Name {
			case EmbedderLatencyMetric:
				histogram, ok := observed.Data.(metricdata.Histogram[float64])
				if !ok {
					t.Fatalf("latency data type = %T", observed.Data)
				}
				for _, point := range histogram.DataPoints {
					latencyCount += point.Count
				}
			case EmbedderRequestFailuresMetric:
				sum, ok := observed.Data.(metricdata.Sum[int64])
				if !ok {
					t.Fatalf("failure data type = %T", observed.Data)
				}
				for _, point := range sum.DataPoints {
					failureCount += uint64(point.Value)
				}
			}
		}
	}
	if latencyCount != 2 {
		t.Fatalf("latency observations = %d, want 2", latencyCount)
	}
	if failureCount != 1 {
		t.Fatalf("failure count = %d, want 1", failureCount)
	}
}

type captureEmbedderRequestRecorder struct {
	mu       sync.Mutex
	requests []capturedEmbedderRequest
}

func (r *captureEmbedderRequestRecorder) RecordEmbedderRequest(_ context.Context, provider string, elapsed time.Duration, failed bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests = append(r.requests, capturedEmbedderRequest{provider: provider, elapsed: elapsed, failed: failed})
}

func TestRecordEmbedderRequestBoundsProviderAndPreservesOutcome(t *testing.T) {
	recorder := &captureEmbedderRequestRecorder{}
	restore := SetEmbedderRequestRecorderForTest(recorder)
	defer restore()

	RecordEmbedderRequest(context.Background(), "https://provider.invalid/model?payload=secret", time.Second, true)

	if len(recorder.requests) != 1 {
		t.Fatalf("got %d requests, want 1", len(recorder.requests))
	}
	got := recorder.requests[0]
	if got.provider != EmbedderProviderUnknown {
		t.Fatalf("provider = %q, want %q", got.provider, EmbedderProviderUnknown)
	}
	if !got.failed || got.elapsed != time.Second {
		t.Fatalf("unexpected request: %+v", got)
	}
}

func TestRecordEmbedderRequestConcurrent(t *testing.T) {
	recorder := &captureEmbedderRequestRecorder{}
	restore := SetEmbedderRequestRecorderForTest(recorder)
	defer restore()

	const requests = 100
	var wg sync.WaitGroup
	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			RecordEmbedderRequest(context.Background(), EmbedderProviderMorph, time.Millisecond, false)
		}()
	}
	wg.Wait()

	if len(recorder.requests) != requests {
		t.Fatalf("got %d requests, want %d", len(recorder.requests), requests)
	}
}
