package hud

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crb2nu/loom/internal/hud/bridge"
)

type engramAPICaller struct {
	callTool func(string, map[string]any) (json.RawMessage, error)
}

func (c *engramAPICaller) Call(string, any) (json.RawMessage, error) { return nil, nil }
func (c *engramAPICaller) CallWithTimeout(string, any, time.Duration) (json.RawMessage, error) {
	return nil, nil
}
func (c *engramAPICaller) CallTool(name string, args map[string]any) (json.RawMessage, error) {
	return c.callTool(name, args)
}
func (c *engramAPICaller) CallToolWithTimeout(name string, args map[string]any, _ time.Duration) (json.RawMessage, error) {
	return c.CallTool(name, args)
}
func (c *engramAPICaller) CircuitOpen() bool { return false }
func (c *engramAPICaller) Close() error      { return nil }

func apiMCPResult(payload string) json.RawMessage {
	encoded, _ := json.Marshal(payload)
	return json.RawMessage(fmt.Sprintf(`{"content":[{"type":"text","text":%s}]}`, encoded))
}

func apiTestApp(caller *engramAPICaller) *App {
	return &App{
		config: Config{Dev: true},
		agent:  bridge.NewAgentBridge(caller),
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// TestHandleEngramSummary_NilBridgeReturnsEmptySummary covers the catalog
// view's "no daemon yet" path: the endpoint must serve an empty but
// well-formed summary instead of 500ing when a.agent is nil.
func TestHandleEngramSummary_NilBridgeReturnsEmptySummary(t *testing.T) {
	app := &App{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		// agent is intentionally left nil — exercises the early return.
	}

	req := httptest.NewRequest(http.MethodGet, "/api/engrams/summary", nil)
	rec := httptest.NewRecorder()
	app.handleEngramSummary(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var got struct {
		Total    int            `json:"total"`
		ByStatus map[string]int `json:"by_status"`
		ByTier   map[string]int `json:"by_tier"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v (body=%s)", err, rec.Body.String())
	}
	if got.Total != 0 {
		t.Errorf("total: got %d want 0", got.Total)
	}
	for _, key := range []string{"unverified", "verified", "stale", "failing"} {
		if _, ok := got.ByStatus[key]; !ok {
			t.Errorf("by_status missing key %q (frontend indexes without nil checks)", key)
		}
	}
	if got.ByTier == nil {
		t.Error("by_tier should be a non-nil empty map")
	}
}

func TestEngramHandlersNilAgentDegradeWithArrays(t *testing.T) {
	a := &App{}
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
		arrays  []string
	}{
		{"list", a.handleEngramList, []string{"engrams"}},
		{"graph", a.handleEngramGraph, []string{"nodes", "edges"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			tc.handler(rr, httptest.NewRequest(http.MethodGet, "/", nil))
			if rr.Code != http.StatusOK {
				t.Fatalf("status=%d", rr.Code)
			}
			var body map[string]any
			if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body["degraded"] != true {
				t.Fatalf("body=%v", body)
			}
			for _, key := range tc.arrays {
				if v, ok := body[key].([]any); !ok || v == nil {
					t.Fatalf("%s missing/non-array: %v", key, body[key])
				}
			}
		})
	}
}

func TestEngramRoutesHappyPathAndCORS(t *testing.T) {
	caller := &engramAPICaller{callTool: func(name string, _ map[string]any) (json.RawMessage, error) {
		switch name {
		case "agent_context__agent_engram_list":
			return apiMCPResult(`{"items":[{"id":"e1","name":"HTTP","tier":2}]}`), nil
		case "agent_context__agent_engram_graph":
			return apiMCPResult(`{"nodes":[{"id":"e1","name":"HTTP","tier":2}],"edges":[{"from":"e1","to":"e0"}]}`), nil
		default:
			return nil, fmt.Errorf("unexpected tool %s", name)
		}
	}}
	a := apiTestApp(caller)
	mux := http.NewServeMux()
	a.registerRoutes(mux)
	for _, tc := range []struct {
		path string
		keys []string
	}{
		{"/api/engrams", []string{"engrams"}},
		{"/api/engrams/graph", []string{"nodes", "edges"}},
	} {
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if rr.Code != http.StatusOK || rr.Header().Get("Access-Control-Allow-Origin") != "*" {
			t.Fatalf("%s: status=%d cors=%q body=%s", tc.path, rr.Code, rr.Header().Get("Access-Control-Allow-Origin"), rr.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body["degraded"] != false {
			t.Fatalf("%s: body=%v", tc.path, body)
		}
		for _, key := range tc.keys {
			if values, ok := body[key].([]any); !ok || len(values) != 1 {
				t.Fatalf("%s: %s=%v", tc.path, key, body[key])
			}
		}
	}

	// The summary rides the graph fetch just served and gets the same CORS
	// treatment; its one node is the whole rollup.
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/engrams/summary", nil))
	if rr.Code != http.StatusOK || rr.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("summary: status=%d cors=%q body=%s", rr.Code, rr.Header().Get("Access-Control-Allow-Origin"), rr.Body.String())
	}
	var summary map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &summary); err != nil {
		t.Fatal(err)
	}
	if summary["total"] != float64(1) || summary["degraded"] != false {
		t.Fatalf("summary: body=%v", summary)
	}
}

// TestEngramSummaryAndGraphShareOneUpstreamFetch pins the request budget the
// engrams store relies on: it fires GET /api/engrams/graph and
// GET /api/engrams/summary together on every poll, and the summary used to
// cost a second upstream call (agent_engram_list with the catalog list's exact
// arguments). One agent_engram_graph fetch must now serve both routes, in
// either order, with both response shapes unchanged.
func TestEngramSummaryAndGraphShareOneUpstreamFetch(t *testing.T) {
	const graphPayload = `{"nodes":[
		{"id":"engram://a/x","uri":"engram://a/x","title":"A","tier":1,"proof_status":"verified","prerequisites":[],"content":"","proof":"a.go:1"},
		{"id":"engram://b/x","uri":"engram://b/x","title":"B","tier":2,"proof_status":"stale","prerequisites":["engram://a/x","engram://gone/x"],"content":"","proof":""},
		{"id":"engram://gone/x","uri":"engram://gone/x","title":"","tier":1,"proof_status":"unverified","prerequisites":[],"stub":true}
	],"edges":[{"from":"engram://b/x","to":"engram://a/x"},{"from":"engram://b/x","to":"engram://gone/x"}],"truncated":false}`

	for _, order := range [][]string{
		{"/api/engrams/graph", "/api/engrams/summary"},
		{"/api/engrams/summary", "/api/engrams/graph"},
	} {
		t.Run(strings.Join(order, " then "), func(t *testing.T) {
			var graphCalls, listCalls atomic.Int32
			caller := &engramAPICaller{callTool: func(name string, _ map[string]any) (json.RawMessage, error) {
				switch name {
				case "agent_context__agent_engram_graph":
					graphCalls.Add(1)
					return apiMCPResult(graphPayload), nil
				case "agent_context__agent_engram_list":
					listCalls.Add(1)
					return nil, errors.New("the summary must not re-list the catalog")
				default:
					return nil, fmt.Errorf("unexpected tool %s", name)
				}
			}}
			a := apiTestApp(caller)
			mux := http.NewServeMux()
			a.registerRoutes(mux)

			bodies := map[string]map[string]any{}
			for _, path := range order {
				rr := httptest.NewRecorder()
				mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
				if rr.Code != http.StatusOK {
					t.Fatalf("%s: status=%d body=%s", path, rr.Code, rr.Body.String())
				}
				var body map[string]any
				if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
					t.Fatalf("%s: %v", path, err)
				}
				bodies[path] = body
			}
			if g, l := graphCalls.Load(), listCalls.Load(); g != 1 || l != 0 {
				t.Fatalf("upstream calls: agent_engram_graph=%d agent_engram_list=%d; want exactly one graph fetch serving both routes", g, l)
			}

			// Graph shape unchanged: the stub stays a node (the tree renders the
			// gap) and the marker never reaches the wire.
			graph := bodies["/api/engrams/graph"]
			nodes, _ := graph["nodes"].([]any)
			edges, _ := graph["edges"].([]any)
			if len(nodes) != 3 || len(edges) != 2 || graph["degraded"] != false {
				t.Fatalf("graph shape changed: %v", graph)
			}
			for _, n := range nodes {
				if _, leaked := n.(map[string]any)["stub"]; leaked {
					t.Fatalf("stub marker leaked onto the graph wire: %v", n)
				}
			}

			// Summary shape identical to the list-era contract; the stub is a
			// gap, not an engram, so it is not counted.
			summary := bodies["/api/engrams/summary"]
			if summary["total"] != float64(2) || summary["degraded"] != false {
				t.Fatalf("summary = %v; want total 2 and degraded=false", summary)
			}
			byStatus, _ := summary["by_status"].(map[string]any)
			for key, want := range map[string]float64{"verified": 1, "stale": 1, "unverified": 0, "failing": 0} {
				if got, ok := byStatus[key]; !ok || got != want {
					t.Fatalf("by_status[%s] = %v (present=%v), want %v: %v", key, got, ok, want, byStatus)
				}
			}
			byTier, _ := summary["by_tier"].(map[string]any)
			if byTier["tier:1"] != float64(1) || byTier["tier:2"] != float64(1) || len(byTier) != 2 {
				t.Fatalf("by_tier = %v", byTier)
			}
		})
	}
}

func TestEngramHandlersBridgeErrorReturnsBadGateway(t *testing.T) {
	caller := &engramAPICaller{callTool: func(string, map[string]any) (json.RawMessage, error) { return nil, errors.New("bridge offline") }}
	a := apiTestApp(caller)
	for _, handler := range []http.HandlerFunc{a.handleEngramList, a.handleEngramGraph, a.handleEngramSummary} {
		rr := httptest.NewRecorder()
		handler(rr, httptest.NewRequest(http.MethodGet, "/", nil))
		if rr.Code != http.StatusBadGateway {
			t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
		}
	}
}

// TestHandleEngramSummary_UpstreamErrorIsNotSticky: a failed fetch must not
// poison the shared graph result — the next poll retries and succeeds.
func TestHandleEngramSummary_UpstreamErrorIsNotSticky(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	caller := &engramAPICaller{callTool: func(string, map[string]any) (json.RawMessage, error) {
		if fail.Load() {
			return nil, errors.New("bridge offline")
		}
		return apiMCPResult(`{"nodes":[{"id":"e1","uri":"e1","title":"HTTP","tier":2,"proof_status":"verified","prerequisites":[]}],"edges":[]}`), nil
	}}
	a := apiTestApp(caller)

	rr := httptest.NewRecorder()
	a.handleEngramSummary(rr, httptest.NewRequest(http.MethodGet, "/api/engrams/summary", nil))
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s, want 502 while the bridge is offline", rr.Code, rr.Body.String())
	}

	fail.Store(false)
	rr = httptest.NewRecorder()
	a.handleEngramSummary(rr, httptest.NewRequest(http.MethodGet, "/api/engrams/summary", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200 once the bridge is back", rr.Code, rr.Body.String())
	}
	var got struct {
		Total int `json:"total"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Total != 1 {
		t.Fatalf("total = %d, want 1 from the fresh fetch", got.Total)
	}
}
