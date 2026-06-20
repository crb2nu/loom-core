package daemon

import (
	"encoding/json"
	"testing"
)

// TestWeaverQueryRequestFromParams_ParentSessionID is a regression test
// for a wiring defect where the loom/weaver/query RPC handler decoded
// params into a local struct that omitted parent_session_id and never
// set QueryRequest.ParentSessionID. That silently broke Mills' weaver
// delegator (which sets parent_session_id to the pipeline run ID): the
// agent-context recorder recorded an empty parent and the spawn bridge
// never forwarded LOOM_PARENT_SESSION_ID, so weaver queries could not be
// joined to the originating run in the HUD.
func TestWeaverQueryRequestFromParams_ParentSessionID(t *testing.T) {
	raw := json.RawMessage(`{
		"query": "what is the cluster status",
		"domains": ["cluster-ops"],
		"max_tokens": 512,
		"agent_id": "loom-mills-operator",
		"session_id": "run-123",
		"parent_session_id": "run-123"
	}`)

	req, err := weaverQueryRequestFromParams(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if req.ParentSessionID != "run-123" {
		t.Errorf("ParentSessionID = %q, want %q (parent_session_id dropped on the wire->request hop)", req.ParentSessionID, "run-123")
	}
	if req.Query != "what is the cluster status" {
		t.Errorf("Query = %q, want %q", req.Query, "what is the cluster status")
	}
	if req.MaxTokens != 512 {
		t.Errorf("MaxTokens = %d, want 512", req.MaxTokens)
	}
	if len(req.Domains) != 1 || req.Domains[0] != "cluster-ops" {
		t.Errorf("Domains = %v, want [cluster-ops]", req.Domains)
	}
	if req.Identity.AgentID != "loom-mills-operator" {
		t.Errorf("Identity.AgentID = %q, want %q", req.Identity.AgentID, "loom-mills-operator")
	}
	if req.Identity.SessionID != "run-123" {
		t.Errorf("Identity.SessionID = %q, want %q", req.Identity.SessionID, "run-123")
	}
}

func TestWeaverQueryRequestFromParams_Validation(t *testing.T) {
	if _, err := weaverQueryRequestFromParams(json.RawMessage(`{"query": ""}`)); err == nil {
		t.Error("expected error for empty query, got nil")
	}
	if _, err := weaverQueryRequestFromParams(json.RawMessage(`{not json`)); err == nil {
		t.Error("expected error for malformed JSON, got nil")
	}
}
