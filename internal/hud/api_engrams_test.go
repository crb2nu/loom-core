package hud

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
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
}

func TestEngramHandlersBridgeErrorReturnsBadGateway(t *testing.T) {
	caller := &engramAPICaller{callTool: func(string, map[string]any) (json.RawMessage, error) { return nil, errors.New("bridge offline") }}
	a := apiTestApp(caller)
	for _, handler := range []http.HandlerFunc{a.handleEngramList, a.handleEngramGraph} {
		rr := httptest.NewRecorder()
		handler(rr, httptest.NewRequest(http.MethodGet, "/", nil))
		if rr.Code != http.StatusBadGateway {
			t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
		}
	}
}
