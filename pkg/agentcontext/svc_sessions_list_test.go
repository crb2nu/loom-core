package agentcontext

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/httpclient"
	"gitlab.flexinfer.ai/libs/mcp-go"
)

// orderedSessionsQdrantStub mimics a Qdrant scroll endpoint that returns
// matching points in deterministic insertion order, truncated to the
// requested limit *before* any payload-level sorting. This is the
// behavior the production list endpoint suffered from: scroll truncates,
// the caller sorts, the most-recent rows fall off the bottom.
type orderedSessionsQdrantStub struct {
	mu       sync.Mutex
	order    []string                  // insertion order of session IDs
	payloads map[string]map[string]any // id -> payload
}

func newOrderedSessionsQdrantStub(t *testing.T, seeded ...Session) (*QdrantClient, *orderedSessionsQdrantStub) {
	t.Helper()

	stub := &orderedSessionsQdrantStub{
		order:    make([]string, 0, len(seeded)),
		payloads: make(map[string]map[string]any, len(seeded)),
	}
	for _, sess := range seeded {
		stub.order = append(stub.order, sess.ID)
		stub.payloads[sess.ID] = clonePayload(SessionToPayload(sess))
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/collections/"+CollSessions, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeJSON(t, w, map[string]any{
				"result": map[string]any{
					"config": map[string]any{
						"params": map[string]any{
							"vectors": map[string]any{
								"size":     sessionsVectorSize,
								"distance": "Cosine",
							},
						},
					},
				},
			})
		case http.MethodPut:
			writeJSON(t, w, map[string]any{"status": "ok"})
		default:
			http.NotFound(w, r)
		}
	})
	mux.HandleFunc("/collections/"+CollSessions+"/points/scroll", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode scroll body: %v", err)
		}
		filter, _ := body["filter"].(map[string]any)
		// Qdrant defaults to no limit; the wrapper passes one explicitly.
		limit := -1
		if raw, ok := body["limit"].(float64); ok {
			limit = int(raw)
		}
		// Offset is a session_id passed back via next_page_offset on the
		// previous response. We use it to resume insertion-order paging.
		offsetID, _ := body["offset"].(string)

		stub.mu.Lock()
		points := make([]map[string]any, 0, len(stub.order))
		started := offsetID == ""
		for _, id := range stub.order {
			if !started {
				if id == offsetID {
					started = true
				}
				continue
			}
			payload, ok := stub.payloads[id]
			if !ok {
				continue
			}
			if !matchesPayloadFilter(filter, payload) {
				continue
			}
			points = append(points, map[string]any{
				"id":      toPointID(id),
				"payload": clonePayload(payload),
			})
			if limit > 0 && len(points) >= limit {
				break
			}
		}
		stub.mu.Unlock()

		var nextOffset any
		if limit > 0 && len(points) == limit {
			// Hand back the next id in insertion order so paged callers can
			// keep walking. Mirrors Qdrant's next_page_offset semantics.
			stub.mu.Lock()
			for i, id := range stub.order {
				if toPointID(id) == points[len(points)-1]["id"] {
					if i+1 < len(stub.order) {
						nextOffset = stub.order[i+1]
					}
					break
				}
			}
			stub.mu.Unlock()
		}
		writeJSON(t, w, map[string]any{
			"result": map[string]any{
				"points":           points,
				"next_page_offset": nextOffset,
			},
		})
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	client := NewQdrantClient(httpclient.NewDefault(), server.URL, "", CollSessions, "Cosine")
	return client, stub
}

// TestSessionList_LimitDoesNotDropMostRecentSessions is the regression
// test for the HUD "0 active sessions" bug. When the underlying
// collection holds more sessions than the requested limit, an
// insertion-order scroll truncates before the caller can sort by
// StartedAt. The most-recently-started sessions live at the tail of
// insertion order, so they vanish from the response — exactly the
// pattern observed in production where the Fleet view called
// agent_session_list(limit=1000) and got back 1000 *ended* sessions
// with zero active rows, despite 12 active sessions existing.
//
// The fix is to scroll all matching points, sort by StartedAt DESC,
// then truncate to the requested limit.
func TestSessionList_LimitDoesNotDropMostRecentSessions(t *testing.T) {
	t.Setenv("LOOM_MCP_OUTPUT_FORMAT", "json")
	now := time.Date(2026, 5, 28, 9, 0, 0, 0, time.UTC)
	base := time.Hour

	// 30 older sessions (status=ended), inserted first. Their StartedAt
	// values are older than the active ones below.
	seeded := make([]Session, 0, 35)
	for i := 0; i < 30; i++ {
		id := stableHexID(i)
		seeded = append(seeded, Session{
			ID:        id,
			AgentID:   "agent-old",
			Status:    string(SessionStatusEnded),
			StartedAt: now.Add(-time.Duration(30-i) * base),
		})
	}
	// 5 fresh active sessions, inserted after the ended ones. Their
	// StartedAt values are newer than every ended row above.
	activeIDs := make(map[string]bool, 5)
	for i := 0; i < 5; i++ {
		id := stableHexID(100 + i)
		activeIDs[id] = true
		seeded = append(seeded, Session{
			ID:        id,
			AgentID:   "agent-new",
			Status:    string(SessionStatusActive),
			StartedAt: now.Add(time.Duration(i) * base),
		})
	}

	client, _ := newOrderedSessionsQdrantStub(t, seeded...)
	svc := newTestService()
	svc.sess = NewSessionSvc(client, svc.cfg, svc.logger, svc.metrics)

	// Ask for fewer rows than total. Limit < 35 means *some* sessions
	// will be dropped; the contract is that the dropped ones are the
	// *oldest*, not the newest.
	result, err := svc.HandleSessionList(context.Background(), map[string]any{
		"limit": 10,
	})
	if err != nil {
		t.Fatalf("HandleSessionList: %v", err)
	}
	if result.IsError {
		t.Fatalf("HandleSessionList returned error result: %+v", result)
	}

	sessions := decodeListResult(t, result)
	if len(sessions) == 0 {
		t.Fatal("list returned 0 sessions; expected at least the 5 active rows")
	}

	// Every active session must appear in the limit=10 response. With
	// the buggy scroll-then-sort-then-truncate, the 5 fresh actives
	// (inserted last) get cut off by the scroll's limit=10 — only the
	// first 10 inserted (all ended) come back, and the post-scroll
	// sort cannot recover the dropped rows.
	foundActives := 0
	for _, s := range sessions {
		if activeIDs[s.ID] {
			foundActives++
		}
	}
	if foundActives != 5 {
		t.Errorf("expected all 5 active sessions in limit=10 response, got %d active out of %d returned",
			foundActives, len(sessions))
		for i, s := range sessions {
			t.Logf("  [%d] id=%s status=%s started_at=%s", i, s.ID, s.Status, s.StartedAt.Format(time.RFC3339))
		}
	}

	// Result must also be sorted by StartedAt DESC.
	for i := 1; i < len(sessions); i++ {
		if sessions[i-1].StartedAt.Before(sessions[i].StartedAt) {
			t.Errorf("result not sorted by StartedAt DESC at index %d: %s before %s",
				i, sessions[i-1].StartedAt.Format(time.RFC3339), sessions[i].StartedAt.Format(time.RFC3339))
		}
	}
}

// TestSessionList_FilterByStatusActiveStillWorks ensures the
// scroll-all path doesn't regress filtered queries: when the caller
// asks for status="active", only active rows come back, in StartedAt
// DESC order, capped to the requested limit.
func TestSessionList_FilterByStatusActiveStillWorks(t *testing.T) {
	t.Setenv("LOOM_MCP_OUTPUT_FORMAT", "json")
	now := time.Date(2026, 5, 28, 9, 0, 0, 0, time.UTC)
	seeded := []Session{
		{ID: stableHexID(1), AgentID: "a", Status: string(SessionStatusEnded), StartedAt: now.Add(-3 * time.Hour)},
		{ID: stableHexID(2), AgentID: "b", Status: string(SessionStatusActive), StartedAt: now.Add(-2 * time.Hour)},
		{ID: stableHexID(3), AgentID: "c", Status: string(SessionStatusActive), StartedAt: now.Add(-1 * time.Hour)},
		{ID: stableHexID(4), AgentID: "d", Status: string(SessionStatusEnded), StartedAt: now.Add(-30 * time.Minute)},
	}
	client, _ := newOrderedSessionsQdrantStub(t, seeded...)
	svc := newTestService()
	svc.sess = NewSessionSvc(client, svc.cfg, svc.logger, svc.metrics)

	result, err := svc.HandleSessionList(context.Background(), map[string]any{
		"status": "active",
		"limit":  20,
	})
	if err != nil {
		t.Fatalf("HandleSessionList: %v", err)
	}
	if result.IsError {
		t.Fatalf("HandleSessionList returned error result: %+v", result)
	}

	sessions := decodeListResult(t, result)
	if len(sessions) != 2 {
		t.Fatalf("expected 2 active sessions, got %d", len(sessions))
	}
	for _, s := range sessions {
		if s.Status != string(SessionStatusActive) {
			t.Errorf("non-active row leaked through status filter: %+v", s)
		}
	}
	if sessions[0].StartedAt.Before(sessions[1].StartedAt) {
		t.Errorf("filtered result not StartedAt DESC: %v before %v",
			sessions[0].StartedAt, sessions[1].StartedAt)
	}
}

// stableHexID returns a deterministic 16-char hex string for tests.
// Mirrors the shape of GenerateID output without depending on time.
func stableHexID(seed int) string {
	const hex = "0123456789abcdef"
	out := make([]byte, 16)
	for i := 0; i < 16; i++ {
		out[i] = hex[(seed+i*7)%16]
	}
	// Vary the leading byte by seed so different seeds produce
	// distinct ids even after the modulo above repeats.
	out[0] = hex[seed%16]
	out[1] = hex[(seed/16)%16]
	return string(out)
}

func decodeListResult(t *testing.T, result *mcp.CallToolResult) []Session {
	t.Helper()
	if result == nil {
		t.Fatal("nil result")
	}
	if len(result.Content) == 0 {
		t.Fatal("result has no content")
	}
	var payload struct {
		Sessions []Session `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(result.Content[0].Text), &payload); err != nil {
		t.Fatalf("unmarshal sessions payload: %v\nraw: %s", err, result.Content[0].Text)
	}
	return payload.Sessions
}
