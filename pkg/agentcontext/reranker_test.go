package agentcontext

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

func TestNoopReranker_ReturnsEntriesUnchanged(t *testing.T) {
	t.Parallel()

	entries := []ContextEntry{
		{ID: "a", Title: "alpha"},
		{ID: "b", Title: "beta"},
		{ID: "c", Title: "gamma"},
	}
	r := NoopReranker{}
	out, err := r.Rerank(context.Background(), "query", entries)
	if err != nil {
		t.Fatalf("Rerank: %v", err)
	}
	if len(out) != len(entries) {
		t.Fatalf("len = %d, want %d", len(out), len(entries))
	}
	for i := range out {
		if out[i].ID != entries[i].ID {
			t.Errorf("entry[%d].ID = %q, want %q (order must not change)", i, out[i].ID, entries[i].ID)
		}
	}
	if r.Backend() != "off" {
		t.Errorf("Backend() = %q, want %q", r.Backend(), "off")
	}
}

func TestNewReranker_UnknownKindFallsBackToNoop(t *testing.T) {
	t.Parallel()

	r := NewReranker(RerankerKind("not-a-real-backend"), RerankerConfig{}, slog.Default())
	if _, ok := r.(NoopReranker); !ok {
		t.Fatalf("NewReranker(unknown) = %T, want NoopReranker", r)
	}
}

func TestNewReranker_EmptyKindFallsBackToNoop(t *testing.T) {
	t.Parallel()

	r := NewReranker("", RerankerConfig{}, nil)
	if _, ok := r.(NoopReranker); !ok {
		t.Fatalf("NewReranker(empty) = %T, want NoopReranker", r)
	}
}

func TestNewReranker_FlexInferKindReturnsFlexInferBackend(t *testing.T) {
	t.Parallel()

	r := NewReranker(RerankerKindFlexInfer, RerankerConfig{BaseURL: "http://example.invalid"}, slog.Default())
	if r.Backend() != "flexinfer" {
		t.Errorf("Backend() = %q, want %q", r.Backend(), "flexinfer")
	}
}

func TestNewReranker_BGEKindReturnsBGEBackend(t *testing.T) {
	t.Parallel()

	r := NewReranker(RerankerKindBGE, RerankerConfig{BaseURL: "http://example.invalid"}, slog.Default())
	if r.Backend() != "bge" {
		t.Errorf("Backend() = %q, want %q", r.Backend(), "bge")
	}
}

// TestFlexInferReranker_ReordersByScore asserts the happy path: the backend
// inverts the seed order based on relevance_score returned by the proxy.
func TestFlexInferReranker_ReordersByScore(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/rerank" {
			http.NotFound(w, r)
			return
		}
		// Return scores in inverted order: index 2 first, index 0 last.
		resp := map[string]any{
			"results": []map[string]any{
				{"index": 2, "relevance_score": 0.99},
				{"index": 1, "relevance_score": 0.50},
				{"index": 0, "relevance_score": 0.10},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	r := newFlexInferReranker(RerankerConfig{
		BaseURL: srv.URL,
		Model:   "bge-reranker-v2-m3",
		Timeout: 2 * time.Second,
	}, slog.Default())

	entries := []ContextEntry{
		{ID: "a", Title: "alpha", Content: "first"},
		{ID: "b", Title: "beta", Content: "second"},
		{ID: "c", Title: "gamma", Content: "third"},
	}
	out, err := r.Rerank(context.Background(), "query", entries)
	if err != nil {
		t.Fatalf("Rerank: %v", err)
	}
	if len(out) != 3 {
		t.Fatalf("len = %d, want 3", len(out))
	}
	want := []string{"c", "b", "a"}
	for i, id := range want {
		if out[i].ID != id {
			t.Errorf("out[%d].ID = %q, want %q", i, out[i].ID, id)
		}
	}
}

// TestFlexInferReranker_TimeoutAnnotatesStatus asserts a soft-failure path:
// a timeout returns entries unchanged with rerank_status set on each.
func TestFlexInferReranker_TimeoutAnnotatesStatus(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	defer srv.Close()

	r := newFlexInferReranker(RerankerConfig{
		BaseURL: srv.URL,
		Timeout: 20 * time.Millisecond,
	}, slog.Default())

	entries := []ContextEntry{
		{ID: "a", Title: "alpha"},
		{ID: "b", Title: "beta"},
	}
	out, err := r.Rerank(context.Background(), "query", entries)
	if err != nil {
		t.Fatalf("Rerank returned error (expected soft-fail): %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("len = %d, want 2", len(out))
	}
	// Order preserved.
	if out[0].ID != "a" || out[1].ID != "b" {
		t.Errorf("order changed on timeout: %v, want [a b]", []string{out[0].ID, out[1].ID})
	}
	// Status annotated.
	for i, e := range out {
		status, _ := e.Metadata["rerank_status"].(string)
		if status != "timeout" {
			t.Errorf("out[%d].Metadata[rerank_status] = %q, want %q", i, status, "timeout")
		}
	}
}

// TestFlexInferReranker_404AnnotatesUnavailable asserts the proxy-missing path.
func TestFlexInferReranker_404AnnotatesUnavailable(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	r := newFlexInferReranker(RerankerConfig{BaseURL: srv.URL, Timeout: time.Second}, slog.Default())
	entries := []ContextEntry{{ID: "a"}, {ID: "b"}}
	out, err := r.Rerank(context.Background(), "query", entries)
	if err != nil {
		t.Fatalf("Rerank returned error (expected soft-fail): %v", err)
	}
	if len(out) != 2 || out[0].ID != "a" || out[1].ID != "b" {
		t.Fatalf("order changed on 404: %v", out)
	}
	status, _ := out[0].Metadata["rerank_status"].(string)
	if status != "unavailable" {
		t.Errorf("rerank_status = %q, want %q", status, "unavailable")
	}
}

// TestApplyRerankScores_PreservesOmittedEntries asserts entries the backend
// omits are preserved at the end of the slice, never dropped.
func TestApplyRerankScores_PreservesOmittedEntries(t *testing.T) {
	t.Parallel()

	entries := []ContextEntry{
		{ID: "a"}, {ID: "b"}, {ID: "c"}, {ID: "d"},
	}
	scores := []flexInferRerankResult{
		{Index: 2, RelevanceScore: 0.9},
		{Index: 0, RelevanceScore: 0.5},
		// Backend forgot indices 1 and 3.
	}
	out := applyRerankScores(entries, scores)
	if len(out) != 4 {
		t.Fatalf("len = %d, want 4 (omitted entries must be preserved)", len(out))
	}
	if out[0].ID != "c" || out[1].ID != "a" {
		t.Errorf("scored prefix = [%s %s], want [c a]", out[0].ID, out[1].ID)
	}
	// Omitted entries tail preserves original order.
	tail := []string{out[2].ID, out[3].ID}
	if tail[0] != "b" || tail[1] != "d" {
		t.Errorf("omitted tail = %v, want [b d]", tail)
	}
}

// TestApplyReranker_NilRerankerIsNoOp asserts Service.ApplyReranker short-
// circuits when no reranker is configured.
func TestApplyReranker_NilRerankerIsNoOp(t *testing.T) {
	t.Parallel()

	svc := &Service{}
	entries := []ContextEntry{{ID: "a"}, {ID: "b"}}
	out, err := svc.ApplyReranker(context.Background(), nil, "q", entries)
	if err != nil {
		t.Fatalf("ApplyReranker: %v", err)
	}
	if len(out) != 2 || out[0].ID != "a" {
		t.Fatalf("nil reranker changed order: %v", out)
	}
}

// TestLoadRerankerConfigFromEnv_DefaultsToOff asserts the default kind is
// "off" when WEAVER_RERANKER is unset.
func TestLoadRerankerConfigFromEnv_DefaultsToOff(t *testing.T) {
	t.Setenv("WEAVER_RERANKER", "")
	cfg := LoadRerankerConfigFromEnv()
	if cfg.Kind != RerankerKindOff {
		t.Errorf("Kind = %q, want %q", cfg.Kind, RerankerKindOff)
	}
}

func TestLoadRerankerConfigFromEnv_ParsesFlexInfer(t *testing.T) {
	t.Setenv("WEAVER_RERANKER", "flexinfer")
	t.Setenv("WEAVER_RERANKER_MODEL", "custom-model")
	t.Setenv("WEAVER_RERANKER_TIMEOUT", "7s")
	cfg := LoadRerankerConfigFromEnv()
	if cfg.Kind != RerankerKindFlexInfer {
		t.Errorf("Kind = %q, want %q", cfg.Kind, RerankerKindFlexInfer)
	}
	if cfg.Model != "custom-model" {
		t.Errorf("Model = %q, want %q", cfg.Model, "custom-model")
	}
	if cfg.Timeout != 7*time.Second {
		t.Errorf("Timeout = %v, want %v", cfg.Timeout, 7*time.Second)
	}
}

// fakeReranker is a deterministic test backend. It reverses the entry order
// and reports a fixed backend tag so the recall-wiring tests can assert that
// HandleUnifiedRecall's second stage actually runs.
type fakeReranker struct {
	backend string
	calls   int
}

func (f *fakeReranker) Backend() string { return f.backend }

func (f *fakeReranker) Rerank(_ context.Context, _ string, entries []ContextEntry) ([]ContextEntry, error) {
	f.calls++
	out := make([]ContextEntry, len(entries))
	for i := range entries {
		out[i] = entries[len(entries)-1-i]
	}
	return out, nil
}

// TestRerankRecallEntries_NilRerankerIsNoOp asserts the recall-stage wrapper
// short-circuits (no reorder, empty backend) when no reranker is wired.
func TestRerankRecallEntries_NilRerankerIsNoOp(t *testing.T) {
	t.Parallel()

	svc := &Service{}
	entries := []ContextEntry{{ID: "a"}, {ID: "b"}, {ID: "c"}}
	out, backend := svc.rerankRecallEntries(context.Background(), "q", entries)
	if backend != "" {
		t.Errorf("backend = %q, want empty", backend)
	}
	if len(out) != 3 || out[0].ID != "a" || out[2].ID != "c" {
		t.Errorf("order changed for nil reranker: %v", idsOf(out))
	}
}

// TestRerankRecallEntries_OffBackendIsNoOp asserts a wired-but-"off" reranker
// (NoopReranker) is treated as disabled: no reorder, empty backend tag, and
// the response path stays byte-stable.
func TestRerankRecallEntries_OffBackendIsNoOp(t *testing.T) {
	t.Parallel()

	svc := &Service{reranker: NoopReranker{}}
	entries := []ContextEntry{{ID: "a"}, {ID: "b"}, {ID: "c"}}
	out, backend := svc.rerankRecallEntries(context.Background(), "q", entries)
	if backend != "" {
		t.Errorf("backend = %q, want empty for off backend", backend)
	}
	if len(out) != 3 || out[0].ID != "a" {
		t.Errorf("off backend reordered entries: %v", idsOf(out))
	}
}

// TestRerankRecallEntries_ActiveBackendReorders asserts that a wired, active
// reranker reorders the candidate set and reports its backend tag for
// recall_meta.
func TestRerankRecallEntries_ActiveBackendReorders(t *testing.T) {
	t.Parallel()

	fake := &fakeReranker{backend: "flexinfer"}
	svc := &Service{reranker: fake}
	entries := []ContextEntry{{ID: "a"}, {ID: "b"}, {ID: "c"}}
	out, backend := svc.rerankRecallEntries(context.Background(), "q", entries)
	if backend != "flexinfer" {
		t.Errorf("backend = %q, want flexinfer", backend)
	}
	if fake.calls != 1 {
		t.Errorf("reranker called %d times, want 1", fake.calls)
	}
	want := []string{"c", "b", "a"}
	if got := idsOf(out); !reflect.DeepEqual(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
}

func idsOf(entries []ContextEntry) []string {
	ids := make([]string, len(entries))
	for i, e := range entries {
		ids[i] = e.ID
	}
	return ids
}
