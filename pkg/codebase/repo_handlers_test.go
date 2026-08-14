package codebase

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	mcp "gitlab.flexinfer.ai/libs/mcp-go"

	"github.com/crb2nu/loom/pkg/codebase/embed"
)

// countMockQdrant stands up a fake Qdrant that only answers the points/count
// endpoint. It records every filter body it receives and returns a count based
// on which language/chunk_type the filter selects, so tests can assert both the
// aggregation math and the exact filters that were sent.
type countMockQdrant struct {
	srv *httptest.Server

	mu     sync.Mutex
	bodies []string
}

func newCountMockQdrant(t *testing.T) *countMockQdrant {
	t.Helper()
	m := &countMockQdrant{}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/points/count") {
			http.NotFound(w, r)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		body := string(raw)

		m.mu.Lock()
		m.bodies = append(m.bodies, body)
		m.mu.Unlock()

		count := 0
		switch {
		case strings.Contains(body, `"language"`) && strings.Contains(body, `"go"`):
			count = 60
		case strings.Contains(body, `"language"`) && strings.Contains(body, `"typescript"`):
			count = 40
		case strings.Contains(body, `"language"`):
			count = 0 // javascript, python, rust — unindexed in this fixture
		case strings.Contains(body, `"chunk_type"`) && strings.Contains(body, `"function"`):
			count = 55
		case strings.Contains(body, `"chunk_type"`):
			count = 0
		default:
			count = 100 // total (match-all, or repo-only filter)
		}
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"result":{"count":` + strconv.Itoa(count) + `}}`))
	}))
	t.Cleanup(m.srv.Close)
	return m
}

func (m *countMockQdrant) recordedBodies() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.bodies))
	copy(out, m.bodies)
	return out
}

func newStatsService(t *testing.T, qdrantURL string) *Service {
	t.Helper()
	svc, err := NewServiceWithEmbedder(Config{
		QdrantURL:        qdrantURL,
		QdrantCollection: "codebase_memory_v1",
		QdrantDistance:   "Cosine",
		MaxFileBytes:     2 << 20,
	}, embed.NewDummyEmbedder(1))
	if err != nil {
		t.Fatalf("NewServiceWithEmbedder: %v", err)
	}
	return svc
}

// parseStatsResult extracts the JSON object emitted by HandleStats from the
// MCP CallToolResult's text content.
func parseStatsResult(t *testing.T, res *mcp.CallToolResult) map[string]any {
	t.Helper()
	if res == nil || len(res.Content) == 0 {
		t.Fatal("result has no content")
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(res.Content[0].Text), &m); err != nil {
		t.Fatalf("unmarshal stats result %q: %v", res.Content[0].Text, err)
	}
	return m
}

// TestHandleStats_AggregateWhenNoRepoID is the regression guard for the HUD
// codebase poller: it polls codebase_stats with no repo_id and no
// CODEBASE_REPO_ID configured. Previously that returned
// "repo_id is required (or set CODEBASE_REPO_ID)" on every poll forever.
// Now it must succeed and summarize the whole collection across all repos.
func TestHandleStats_AggregateWhenNoRepoID(t *testing.T) {
	t.Setenv("LOOM_MCP_OUTPUT_FORMAT", "json") // deterministic JSON (default is TOON)

	mock := newCountMockQdrant(t)
	svc := newStatsService(t, mock.srv.URL)

	res, err := svc.HandleStats(context.Background(), map[string]any{})
	if err != nil {
		t.Fatalf("HandleStats (no repo_id) must not error, got: %v", err)
	}

	got := parseStatsResult(t, res)

	if agg, _ := got["aggregate"].(bool); !agg {
		t.Fatalf("expected aggregate=true, got %v", got["aggregate"])
	}
	if total, _ := got["total_chunks"].(float64); total != 100 {
		t.Fatalf("expected total_chunks=100, got %v", got["total_chunks"])
	}
	byLang, _ := got["by_language"].(map[string]any)
	if v, _ := byLang["go"].(float64); v != 60 {
		t.Fatalf("expected by_language.go=60, got %v", byLang["go"])
	}
	if v, _ := byLang["typescript"].(float64); v != 40 {
		t.Fatalf("expected by_language.typescript=40, got %v", byLang["typescript"])
	}
	// Zero-valued buckets must be omitted from the fleet summary.
	if _, present := byLang["python"]; present {
		t.Fatalf("expected zero-valued python bucket to be omitted, got %v", byLang["python"])
	}
	byType, _ := got["by_chunk_type"].(map[string]any)
	if v, _ := byType["function"].(float64); v != 55 {
		t.Fatalf("expected by_chunk_type.function=55, got %v", byType["function"])
	}
	if _, present := byType["method"]; present {
		t.Fatalf("expected zero-valued method bucket to be omitted, got %v", byType["method"])
	}

	// The load-bearing invariant: aggregate mode must never filter by repo_id,
	// or Qdrant would count only the empty-string repo and return nothing.
	bodies := mock.recordedBodies()
	if len(bodies) == 0 {
		t.Fatal("expected at least one count call to Qdrant")
	}
	for _, b := range bodies {
		if strings.Contains(b, "repo_id") {
			t.Fatalf("aggregate count filter must not contain repo_id, got body: %s", b)
		}
	}
}

// TestHandleStats_PerRepoStillScopes guards the existing single-repo path: when
// a repo_id is supplied the counts must be scoped to that repo.
func TestHandleStats_PerRepoStillScopes(t *testing.T) {
	t.Setenv("LOOM_MCP_OUTPUT_FORMAT", "json")

	mock := newCountMockQdrant(t)
	svc := newStatsService(t, mock.srv.URL)

	res, err := svc.HandleStats(context.Background(), map[string]any{"repo_id": "loom-core"})
	if err != nil {
		t.Fatalf("HandleStats (repo_id=loom-core): %v", err)
	}

	got := parseStatsResult(t, res)

	if _, present := got["aggregate"]; present {
		t.Fatalf("per-repo result must not set aggregate, got %v", got["aggregate"])
	}
	if got["repo_id"] != "loom-core" {
		t.Fatalf("expected repo_id 'loom-core', got %v", got["repo_id"])
	}

	// Every count in per-repo mode must be scoped by repo_id.
	bodies := mock.recordedBodies()
	if len(bodies) == 0 {
		t.Fatal("expected count calls")
	}
	for _, b := range bodies {
		if !strings.Contains(b, `"repo_id"`) || !strings.Contains(b, `"loom-core"`) {
			t.Fatalf("per-repo count filter must scope by repo_id, got body: %s", b)
		}
	}
}

// TestHandleStats_DefaultRepoIDStillUsed ensures CODEBASE_REPO_ID (the
// RepoIDDefault) continues to select single-repo mode when set.
func TestHandleStats_DefaultRepoIDStillUsed(t *testing.T) {
	t.Setenv("LOOM_MCP_OUTPUT_FORMAT", "json")

	mock := newCountMockQdrant(t)
	svc := newStatsService(t, mock.srv.URL)
	svc.cfg.RepoIDDefault = "configured-repo"

	res, err := svc.HandleStats(context.Background(), map[string]any{})
	if err != nil {
		t.Fatalf("HandleStats with RepoIDDefault: %v", err)
	}
	got := parseStatsResult(t, res)
	if got["repo_id"] != "configured-repo" {
		t.Fatalf("expected repo_id 'configured-repo', got %v", got["repo_id"])
	}
	if _, present := got["aggregate"]; present {
		t.Fatalf("configured-default result must not be aggregate")
	}
}
