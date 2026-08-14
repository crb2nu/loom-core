package agentcontext

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/httpclient"
)

func newTestTracker(cfg EmbedDegradationConfig) *EmbedDegradationTracker {
	return NewEmbedDegradationTracker(cfg)
}

func TestEmbedDegradationTracker_RatioGate(t *testing.T) {
	tr := newTestTracker(EmbedDegradationConfig{
		MaxFallbackRatio: 0.20,
		MinSamples:       10,
		WindowSize:       50,
		// Duration gate disabled so only the ratio gate is under test.
	})

	// 100% fallback but below MinSamples: gate must not arm.
	for i := 0; i < 9; i++ {
		tr.RecordOutcome(true)
		if derr := tr.Degraded(); derr != nil {
			t.Fatalf("degraded after %d samples (below MinSamples=10): %v", i+1, derr)
		}
	}

	// 10th sample a success: 9/10 = 90% > 20% → degraded via ratio.
	tr.RecordOutcome(false)
	derr := tr.Degraded()
	if derr == nil {
		t.Fatal("not degraded at 90% fallback over 10 samples")
	}
	if derr.Reason != "fallback_ratio" {
		t.Errorf("reason = %q, want fallback_ratio", derr.Reason)
	}
	if !errors.Is(derr, ErrEmbedderDegraded) {
		t.Error("EmbedderDegradedError does not unwrap to ErrEmbedderDegraded")
	}
}

func TestEmbedDegradationTracker_RatioAtThresholdDoesNotTrip(t *testing.T) {
	tr := newTestTracker(EmbedDegradationConfig{
		MaxFallbackRatio: 0.20,
		MinSamples:       10,
		WindowSize:       50,
	})
	// Exactly 2 fallbacks in 10 = 20%: threshold is strictly "exceeds".
	for i := 0; i < 10; i++ {
		tr.RecordOutcome(i < 2)
	}
	if derr := tr.Degraded(); derr != nil {
		t.Fatalf("degraded at exactly the 20%% threshold: %v", derr)
	}
	// One more fallback: 3/11 ≈ 27% → trips.
	tr.RecordOutcome(true)
	if tr.Degraded() == nil {
		t.Fatal("not degraded at 3/11 fallback")
	}
}

func TestEmbedDegradationTracker_WindowEvictionRecovers(t *testing.T) {
	tr := newTestTracker(EmbedDegradationConfig{
		MaxFallbackRatio: 0.20,
		MinSamples:       4,
		WindowSize:       4,
	})
	tr.RecordOutcome(true)
	tr.RecordOutcome(true)
	tr.RecordOutcome(false)
	tr.RecordOutcome(false)
	if tr.Degraded() == nil {
		t.Fatal("not degraded at 2/4 fallback")
	}
	// Two more successes evict the two fallbacks from the ring: 0/4.
	tr.RecordOutcome(false)
	tr.RecordOutcome(false)
	if derr := tr.Degraded(); derr != nil {
		t.Fatalf("still degraded after fallbacks rolled out of the window: %v", derr)
	}
	if ratio, samples := tr.Ratio(); ratio != 0 || samples != 4 {
		t.Errorf("ratio = %v over %d samples, want 0 over 4", ratio, samples)
	}
}

func TestEmbedDegradationTracker_ContinuousFailureGate(t *testing.T) {
	base := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	clock := base
	tr := newTestTracker(EmbedDegradationConfig{
		// Ratio gate disabled so only the duration gate is under test.
		MaxContinuousFailure: 30 * time.Minute,
		WindowSize:           50,
	})
	tr.now = func() time.Time { return clock }

	// A single failure spanning any duration never trips the gate.
	tr.RecordOutcome(true)
	clock = base.Add(2 * time.Hour)
	if derr := tr.Degraded(); derr != nil {
		t.Fatalf("degraded on a single isolated failure: %v", derr)
	}

	// A second failure while the first is >30m old: continuous failure trips.
	tr.RecordOutcome(true)
	derr := tr.Degraded()
	if derr == nil {
		t.Fatal("not degraded after 2h of continuous failure")
	}
	if derr.Reason != "continuous_failure" {
		t.Errorf("reason = %q, want continuous_failure", derr.Reason)
	}
	if derr.FailingFor < 2*time.Hour {
		t.Errorf("FailingFor = %v, want >= 2h", derr.FailingFor)
	}

	// One success resets the streak entirely.
	tr.RecordOutcome(false)
	tr.RecordOutcome(true)
	clock = clock.Add(29 * time.Minute)
	tr.RecordOutcome(true)
	if derr := tr.Degraded(); derr != nil {
		t.Fatalf("degraded 29m into a fresh streak: %v", derr)
	}
	clock = clock.Add(2 * time.Minute)
	if tr.Degraded() == nil {
		t.Fatal("not degraded 31m into the fresh streak")
	}
}

func TestEmbedDegradationTracker_DisabledGatesNeverTrip(t *testing.T) {
	tr := newTestTracker(EmbedDegradationConfig{WindowSize: 8})
	for i := 0; i < 32; i++ {
		tr.RecordOutcome(true)
	}
	if derr := tr.Degraded(); derr != nil {
		t.Fatalf("degraded with both gates disabled: %v", derr)
	}
}

func TestRecordEmbedWriteOutcome_NilSafe(t *testing.T) {
	if err := recordEmbedWriteOutcome(nil, nil, true); err != nil {
		t.Fatalf("nil tracker/metrics returned error: %v", err)
	}
	var tr *EmbedDegradationTracker
	tr.RecordOutcome(true) // must not panic
	if tr.Degraded() != nil {
		t.Fatal("nil tracker reports degradation")
	}
}

func TestRecordEmbedWriteOutcome_PublishesMetrics(t *testing.T) {
	m := NewMetrics()
	tr := newTestTracker(EmbedDegradationConfig{
		MaxFallbackRatio: 0.20,
		MinSamples:       3, // not armed at 2 samples — this test covers publishing, not gating
		WindowSize:       4,
	})

	if err := recordEmbedWriteOutcome(tr, m, false); err != nil {
		t.Fatalf("success outcome returned error: %v", err)
	}
	if err := recordEmbedWriteOutcome(tr, m, true); err != nil {
		t.Fatalf("fallback below MinSamples returned error: %v", err)
	}

	snap := m.Snapshot()
	if snap.EmbedWriteAttempts != 2 {
		t.Errorf("EmbedWriteAttempts = %d, want 2", snap.EmbedWriteAttempts)
	}
	if snap.EmbedFallbackWrites != 1 {
		t.Errorf("EmbedFallbackWrites = %d, want 1", snap.EmbedFallbackWrites)
	}
	if snap.EmbedFallbackRatio != 0.5 {
		t.Errorf("EmbedFallbackRatio = %v, want 0.5", snap.EmbedFallbackRatio)
	}
	if snap.EmbedDegradedRejections != 0 {
		t.Errorf("EmbedDegradedRejections = %d, want 0", snap.EmbedDegradedRejections)
	}

	// Third sample arms the gate: 2/3 ≈ 67% > 20% → rejection counted.
	if err := recordEmbedWriteOutcome(tr, m, true); err == nil {
		t.Fatal("fallback past armed threshold returned nil")
	}
	if got := m.EmbedDegradedRejections.Load(); got != 1 {
		t.Errorf("EmbedDegradedRejections = %d, want 1", got)
	}
}

func TestStoreContextEntries_FailClosedWhenDegraded(t *testing.T) {
	var captured []capturedUpsert
	server := newEmbedOutageQdrant(t, &captured)
	t.Cleanup(server.Close)

	cfg := Config{
		QdrantURL:         server.URL,
		QdrantDistance:    "Cosine",
		ContextCollection: "context",
	}
	vectorSize := 0
	metrics := NewMetrics()
	cs := &ContextSvc{
		qdrant:     NewQdrantRegistry(httpclient.NewDefault(), cfg),
		embed:      failingEmbedder{},
		vectorSize: &vectorSize,
		cfg:        cfg,
		metrics:    metrics,
		logger:     slog.Default(),
		embedDegradation: NewEmbedDegradationTracker(EmbedDegradationConfig{
			MaxFallbackRatio: 0.20,
			MinSamples:       1,
			WindowSize:       4,
		}),
	}

	entries := []ContextEntry{{
		ID:      "ctx-degraded-1",
		AgentID: "claude-code-test",
		Title:   "should be rejected",
		Content: "systemic embedder degradation must fail closed",
	}}

	_, err := cs.storeContextEntries(context.Background(), entries, []string{"should be rejected"})
	if err == nil {
		t.Fatal("storeContextEntries persisted a fallback-vector write past the fail-closed threshold")
	}
	if !errors.Is(err, ErrEmbedderDegraded) {
		t.Fatalf("error = %v, want errors.Is ErrEmbedderDegraded", err)
	}
	if len(captured) != 0 {
		t.Fatalf("captured %d upserts, want 0 (degraded write must not persist)", len(captured))
	}
	if got := metrics.EmbedDegradedRejections.Load(); got != 1 {
		t.Errorf("EmbedDegradedRejections = %d, want 1", got)
	}
}

func TestTaskAdd_FailClosedWhenDegraded(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/collections/"+CollTasks, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		writeJSON(t, w, map[string]any{
			"result": map[string]any{
				"config": map[string]any{
					"params": map[string]any{
						"vectors": map[string]any{"size": 1536, "distance": "Cosine"},
					},
				},
			},
		})
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	qdrant := NewQdrantClient(httpclient.NewDefault(), server.URL, "", CollTasks, "Cosine")
	vectorSize := 1536
	ts := NewTaskSvc(qdrant, failingEmbedder{}, Config{}, slog.Default(), &vectorSize)
	ts.embedDegradation = NewEmbedDegradationTracker(EmbedDegradationConfig{
		MaxFallbackRatio: 0.20,
		MinSamples:       1,
		WindowSize:       4,
	})
	ts.getSession = func(_ context.Context, sessionID string) (*Session, error) {
		return &Session{ID: sessionID, AgentID: "claude-code-test"}, nil
	}
	var captured []Point
	ts.upsertBatched = func(_ context.Context, _ *QdrantClient, points []Point) error {
		captured = append(captured, points...)
		return nil
	}

	res, err := ts.Add(context.Background(), map[string]any{
		"session_id": "s1",
		"tasks": []any{
			map[string]any{"title": "should be rejected"},
		},
	})
	if err != nil {
		t.Fatalf("Add returned transport error: %v", err)
	}
	if res == nil || !res.IsError {
		t.Fatal("Add succeeded past the fail-closed threshold; want a degraded tool error")
	}
	if len(captured) != 0 {
		t.Fatalf("captured %d points, want 0 (degraded write must not persist)", len(captured))
	}
}
