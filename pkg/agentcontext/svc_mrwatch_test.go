package agentcontext

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// mrResultPayload decodes the JSON text content of an mcp result into a map.
// Callers must set LOOM_MCP_OUTPUT_FORMAT=json (the mcp-go default is TOON).
func mrResultPayload(t *testing.T, res any) map[string]any {
	t.Helper()
	// The mcp.CallToolResult marshals to JSON with a Content[].Text field; the
	// simplest robust assertion is to re-marshal the whole result and pull the
	// text out. But JSONResult stores the payload as a single text block of
	// JSON, so decode that.
	b, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	var wrapper struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(b, &wrapper); err != nil {
		t.Fatalf("unmarshal result wrapper: %v (%s)", err, string(b))
	}
	if len(wrapper.Content) == 0 {
		t.Fatalf("result had no content: %s", string(b))
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(wrapper.Content[0].Text), &payload); err != nil {
		t.Fatalf("unmarshal payload text: %v (%s)", err, wrapper.Content[0].Text)
	}
	return payload
}

func TestHandleMRStatus_Available(t *testing.T) {
	t.Setenv("LOOM_MCP_OUTPUT_FORMAT", "json")
	var gotBranch, gotRepo string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/agent/mr-status" {
			http.NotFound(w, r)
			return
		}
		gotBranch = r.URL.Query().Get("branch")
		gotRepo = r.URL.Query().Get("repo")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"branch":"feat/x","count":1,"merge_requests":[{"iid":42,"state":"conflict","reason":"needs rebase"}]}`))
	}))
	defer ts.Close()

	svc := &MRStatusSvc{Service: &Service{cfg: Config{HUDBaseURL: ts.URL}}}
	res, err := svc.HandleMRStatus(context.Background(), map[string]any{
		"branch": "feat/x",
		"repo":   "services/loom-core",
	})
	if err != nil {
		t.Fatalf("HandleMRStatus error: %v", err)
	}
	if gotBranch != "feat/x" {
		t.Fatalf("HUD saw branch %q, want feat/x", gotBranch)
	}
	if gotRepo != "services/loom-core" {
		t.Fatalf("HUD saw repo %q, want services/loom-core", gotRepo)
	}

	payload := mrResultPayload(t, res)
	if avail, _ := payload["available"].(bool); !avail {
		t.Fatalf("expected available=true, got %v", payload["available"])
	}
	if cnt, _ := payload["count"].(float64); cnt != 1 {
		t.Fatalf("expected count 1, got %v", payload["count"])
	}
	mrs, _ := payload["merge_requests"].([]any)
	if len(mrs) != 1 {
		t.Fatalf("expected 1 MR, got %v", payload["merge_requests"])
	}
}

func TestHandleMRStatus_MissingBranchIsError(t *testing.T) {
	t.Setenv("LOOM_MCP_OUTPUT_FORMAT", "json")
	svc := &MRStatusSvc{Service: &Service{cfg: Config{HUDBaseURL: "http://127.0.0.1:1"}}}
	res, err := svc.HandleMRStatus(context.Background(), map[string]any{})
	if err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	// Validation failure surfaces as an mcp error result (IsError), not a Go
	// error — assert the result marshals with isError true.
	b, _ := json.Marshal(res)
	var wrapper struct {
		IsError bool `json:"isError"`
	}
	_ = json.Unmarshal(b, &wrapper)
	if !wrapper.IsError {
		t.Fatalf("expected an error result for missing branch, got %s", string(b))
	}
}

func TestHandleMRStatus_UnreachableHUDReturnsUnavailable(t *testing.T) {
	t.Setenv("LOOM_MCP_OUTPUT_FORMAT", "json")
	svc := &MRStatusSvc{Service: &Service{cfg: Config{HUDBaseURL: "http://127.0.0.1:1"}}}
	res, err := svc.HandleMRStatus(context.Background(), map[string]any{"branch": "feat/x"})
	if err != nil {
		t.Fatalf("HandleMRStatus should not return a Go error on unreachable HUD: %v", err)
	}
	payload := mrResultPayload(t, res)
	if avail, _ := payload["available"].(bool); avail {
		t.Fatalf("expected available=false on unreachable HUD, got %v", payload)
	}
	if payload["branch"] != "feat/x" {
		t.Fatalf("expected branch echoed in unavailable result, got %v", payload["branch"])
	}
}

func TestHandleMRStatus_NoHUDConfigured(t *testing.T) {
	t.Setenv("LOOM_MCP_OUTPUT_FORMAT", "json")
	svc := &MRStatusSvc{Service: &Service{cfg: Config{HUDBaseURL: ""}}}
	res, err := svc.HandleMRStatus(context.Background(), map[string]any{"branch": "feat/x"})
	if err != nil {
		t.Fatalf("HandleMRStatus error: %v", err)
	}
	payload := mrResultPayload(t, res)
	if avail, _ := payload["available"].(bool); avail {
		t.Fatalf("expected available=false when HUD unconfigured, got %v", payload)
	}
}
