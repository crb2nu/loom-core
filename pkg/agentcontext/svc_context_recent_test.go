package agentcontext

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/httpclient"
)

// recentFixtureEntries returns three entries deliberately NOT in time order
// so a fake Qdrant that ignores order_by hands them back unsorted.
func recentFixtureEntries(now time.Time) []ContextEntry {
	return []ContextEntry{
		{ID: "ctx-old", AgentID: "codex", EntryType: EntryType("summary"), Title: "months ago", Content: "old", Timestamp: now.Add(-60 * 24 * time.Hour)},
		{ID: "ctx-new", AgentID: "claude-code", EntryType: EntryType("decision"), Title: "just now", Content: "new", Timestamp: now.Add(-1 * time.Minute)},
		{ID: "ctx-mid", AgentID: "claude-code", EntryType: EntryType("finding"), Title: "an hour ago", Content: "mid", Timestamp: now.Add(-1 * time.Hour)},
	}
}

type scrollRequest struct {
	Limit   int            `json:"limit"`
	OrderBy map[string]any `json:"order_by"`
	Filter  map[string]any `json:"filter"`
}

// newRecentQdrant fakes the scroll endpoint. When orderBySupported is false
// every request carrying order_by is rejected with 400 (an older collection
// without the timestamp index / an older Qdrant), which must drive the
// in-process fallback. Plain scrolls answer the fixture in insertion order.
func newRecentQdrant(t *testing.T, now time.Time, orderBySupported bool, seen *[]scrollRequest) *httptest.Server {
	t.Helper()
	entries := recentFixtureEntries(now)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/collections/context":
			_, _ = w.Write([]byte(`{"status":"ok","result":{"config":{"params":{"vectors":{"size":4}}}}}`))
		case r.Method == http.MethodPut:
			_, _ = w.Write([]byte(`{"status":"ok","result":true}`))
		case r.Method == http.MethodPost && r.URL.Path == "/collections/context/points/scroll":
			body, _ := io.ReadAll(r.Body)
			var req scrollRequest
			if err := json.Unmarshal(body, &req); err != nil {
				t.Fatalf("decode scroll body: %v", err)
			}
			*seen = append(*seen, req)
			if req.OrderBy != nil && !orderBySupported {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"status":{"error":"Index required but not found for \"timestamp\""}}`))
				return
			}
			points := make([]map[string]any, 0, len(entries))
			for _, e := range entries {
				points = append(points, map[string]any{"id": e.ID, "payload": EntryToPayload(e, "test")})
			}
			if req.OrderBy != nil {
				// A real Qdrant honours order_by; mimic newest-first.
				points = []map[string]any{points[1], points[2], points[0]}
			}
			resp := map[string]any{"status": "ok", "result": map[string]any{"points": points, "next_page_offset": nil}}
			_ = json.NewEncoder(w).Encode(resp)
		default:
			t.Fatalf("unexpected qdrant request: %s %s", r.Method, r.URL.Path)
		}
	}))
}

func resultIDs(results []SearchResult) []string {
	ids := make([]string, 0, len(results))
	for _, r := range results {
		ids = append(ids, r.Entry.ID)
	}
	return ids
}

func newRecentContextSvc(t *testing.T, server *httptest.Server) *ContextSvc {
	t.Helper()
	cfg := Config{QdrantURL: server.URL, QdrantDistance: "Cosine", ContextCollection: "context"}
	vectorSize := 4
	return &ContextSvc{
		qdrant:     NewQdrantRegistry(httpclient.NewDefault(), cfg),
		embed:      failingEmbedder{},
		vectorSize: &vectorSize,
		cfg:        cfg,
		metrics:    NewMetrics(),
		logger:     slog.Default(),
	}
}

// TestContextSearch_SortRecent_OrderedScroll pins the live-stream contract:
// sort=recent needs no query, asks Qdrant for a timestamp-ordered scroll with
// the since bound as a datetime range filter, and returns newest-first.
func TestContextSearch_SortRecent_OrderedScroll(t *testing.T) {
	now := time.Date(2026, 9, 2, 14, 0, 0, 0, time.UTC)
	var seen []scrollRequest
	server := newRecentQdrant(t, now, true, &seen)
	t.Cleanup(server.Close)
	cs := newRecentContextSvc(t, server)

	since := now.Add(-2 * time.Hour)
	results, degraded, err := cs.recentEntries(context.Background(), nil, since, 10, true)
	if err != nil {
		t.Fatalf("recentEntries: %v", err)
	}
	if got, want := strings.Join(resultIDs(results), ","), "ctx-new,ctx-mid,ctx-old"; got != want {
		t.Fatalf("ids = %s, want %s (newest first)", got, want)
	}
	if degraded != "" {
		t.Fatalf("ordered scroll path must not report degraded: %q", degraded)
	}
	// The tool surface accepts sort=recent without a query.
	res, err := cs.Search(context.Background(), map[string]any{"sort": "recent", "since": since.Format(time.RFC3339), "limit": 10})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if res.IsError {
		t.Fatalf("Search(sort=recent) returned a tool error: %+v", res)
	}
	seen = seen[:1]
	if len(seen) != 1 {
		t.Fatalf("scroll requests = %d, want 1", len(seen))
	}
	if seen[0].OrderBy == nil || seen[0].OrderBy["key"] != "timestamp" || seen[0].OrderBy["direction"] != "desc" {
		t.Fatalf("order_by = %v, want timestamp desc", seen[0].OrderBy)
	}
	filterJSON, _ := json.Marshal(seen[0].Filter)
	if !strings.Contains(string(filterJSON), `"range"`) || !strings.Contains(string(filterJSON), since.UTC().Format(time.RFC3339Nano)) {
		t.Fatalf("filter lacks the since range: %s", filterJSON)
	}
}

// TestContextSearch_SortRecent_FallsBackWithoutIndex: an older collection
// rejects order_by; the listing must still come back newest-first and honour
// since, flagged degraded.
func TestContextSearch_SortRecent_FallsBackWithoutIndex(t *testing.T) {
	now := time.Date(2026, 9, 2, 14, 0, 0, 0, time.UTC)
	var seen []scrollRequest
	server := newRecentQdrant(t, now, false, &seen)
	t.Cleanup(server.Close)
	cs := newRecentContextSvc(t, server)

	results, degraded, err := cs.recentEntries(context.Background(), nil, now.Add(-2*time.Hour), 10, true)
	if err != nil {
		t.Fatalf("recentEntries: %v", err)
	}
	if got, want := strings.Join(resultIDs(results), ","), "ctx-new,ctx-mid"; got != want {
		t.Fatalf("ids = %s, want %s (since filter applied in-process, newest first)", got, want)
	}
	if degraded == "" {
		t.Fatal("fallback path must report degraded")
	}
	if len(seen) != 2 || seen[0].OrderBy == nil || seen[1].OrderBy != nil {
		t.Fatalf("expected an ordered attempt then a plain pool scroll, got %+v", seen)
	}
}

// TestContextSearch_RelevanceStillRequiresQuery guards the default contract.
func TestContextSearch_RelevanceStillRequiresQuery(t *testing.T) {
	now := time.Date(2026, 9, 2, 14, 0, 0, 0, time.UTC)
	var seen []scrollRequest
	server := newRecentQdrant(t, now, true, &seen)
	t.Cleanup(server.Close)
	cs := newRecentContextSvc(t, server)

	res, err := cs.Search(context.Background(), map[string]any{"limit": 3})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if !res.IsError {
		t.Fatalf("relevance search without a query must be a tool error, got %+v", res)
	}
}
