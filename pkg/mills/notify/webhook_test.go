package notify

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/crb2nu/loom/pkg/mills/store"
)

type stubStore struct {
	stages []*store.StageResult
	err    error
}

func (s *stubStore) ListStages(_ context.Context, _ string) ([]*store.StageResult, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.stages, nil
}

func mrIID(v int64) *int64 { return &v }

func TestWebhookHook_DisabledURL_IsNoOp(t *testing.T) {
	srvCalls := int64(0)
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&srvCalls, 1)
	}))
	defer srv.Close()
	// Construct with empty URL despite the server being live. Should
	// not call the server.
	h := New(Config{URL: ""}, nil, srv.Client(), nil)
	if h.Enabled() {
		t.Fatal("hook.Enabled() = true; want false for empty URL")
	}
	if err := h.OnMerged(context.Background(),
		&store.PipelineRun{ID: "PIPE-A-1"},
		&store.BacklogItem{ID: "BL-A", Title: "A"}); err != nil {
		t.Errorf("OnMerged returned %v, want nil", err)
	}
	if atomic.LoadInt64(&srvCalls) != 0 {
		t.Errorf("server called %d times; want 0 when URL empty",
			atomic.LoadInt64(&srvCalls))
	}
}

func TestWebhookHook_EnabledURL_PostsExpectedPayload(t *testing.T) {
	var mu sync.Mutex
	var receivedPayload map[string]any
	var receivedContentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		receivedContentType = r.Header.Get("Content-Type")
		dec := json.NewDecoder(r.Body)
		_ = dec.Decode(&receivedPayload)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	st := &stubStore{stages: []*store.StageResult{
		{Stage: "implement", Attempt: 1, LogTail: "websocket: close 1006 (abnormal closure)"},
		{Stage: "implement", Attempt: 2, LogTail: "ok"},
		{Stage: "tests", Attempt: 1, LogTail: "pod not found during reconciliation"},
		{Stage: "tests", Attempt: 2, LogTail: "ok"},
	}}
	h := New(Config{URL: srv.URL, MRBaseURL: "https://gitlab.example/group/repo"},
		st, srv.Client(), nil)
	if !h.Enabled() {
		t.Fatal("hook.Enabled() = false; want true")
	}

	err := h.OnMerged(context.Background(),
		&store.PipelineRun{ID: "PIPE-X-1", MRIID: mrIID(42), CostUSD: 0.0123, Attempts: 1},
		&store.BacklogItem{ID: "BL-X", Title: "Add new feature"})
	if err != nil {
		t.Fatalf("OnMerged: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if receivedContentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", receivedContentType)
	}
	text, _ := receivedPayload["text"].(string)
	for _, sub := range []string{
		"Mills merged BL-X",
		"Add new feature",
		"merge_requests/42",
		"stage retries: 2",
		"k8s-pod-gc", "mcp-transport",
	} {
		if !strings.Contains(text, sub) {
			t.Errorf("payload text missing %q\nfull text: %s", sub, text)
		}
	}
}

func TestWebhookHook_FailureSwallowed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"slack down"}`))
	}))
	defer srv.Close()

	h := New(Config{URL: srv.URL}, nil, srv.Client(), nil)
	// 500 → post() returns an error internally, but OnMerged must
	// always return nil so the merge chain isn't blocked.
	if err := h.OnMerged(context.Background(),
		&store.PipelineRun{ID: "PIPE-A-1"},
		&store.BacklogItem{ID: "BL-A", Title: "A"}); err != nil {
		t.Errorf("OnMerged returned %v, want nil even on webhook 500", err)
	}
}

func TestWebhookHook_NilRunOrItem_Skipped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("server should not be called for nil run/item")
	}))
	defer srv.Close()
	h := New(Config{URL: srv.URL}, nil, srv.Client(), nil)
	_ = h.OnMerged(context.Background(), nil, &store.BacklogItem{ID: "x"})
	_ = h.OnMerged(context.Background(), &store.PipelineRun{ID: "x"}, nil)
}

func TestWebhookHook_StoreFailureDoesNotBlockNotification(t *testing.T) {
	var posted bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		posted = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	st := &stubStore{err: errors.New("db read failed")}
	h := New(Config{URL: srv.URL}, st, srv.Client(), nil)
	_ = h.OnMerged(context.Background(),
		&store.PipelineRun{ID: "PIPE-A-1"},
		&store.BacklogItem{ID: "BL-A", Title: "A"})
	if !posted {
		t.Errorf("webhook not called; store failure should not block notify")
	}
}

func TestSummarizeStages_CountsRetriesAndExtractsHints(t *testing.T) {
	stages := []*store.StageResult{
		{Stage: "implement", Attempt: 1, LogTail: "websocket: close 1006"},
		{Stage: "implement", Attempt: 2, LogTail: ""},
		{Stage: "tests", Attempt: 1, LogTail: "buildah build failed: exit_code=243"},
		{Stage: "tests", Attempt: 2, LogTail: "ok"},
		{Stage: "tests", Attempt: 3, LogTail: "broken pipe write tcp"},
	}
	retries, hints := summarizeStages(stages)
	if retries != 3 {
		t.Errorf("retries = %d, want 3 (3 attempts > 1)", retries)
	}
	// Convert hints slice to set for order-independent comparison.
	got := map[string]bool{}
	for _, h := range hints {
		got[h] = true
	}
	for _, want := range []string{"mcp-transport", "buildah-build-fail", "mcp-broken-pipe"} {
		if !got[want] {
			t.Errorf("hints missing %q; got %v", want, hints)
		}
	}
}
