package daemon

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/crb2nu/loom/internal/router"
	"gitlab.flexinfer.ai/libs/mcp-go"
)

// These tests cover the HUD agent_id-correlation keystone: an interactive
// agent's MCP calls that route over the hub to a central daemon must carry the
// agent's identity so the receiving daemon stamps a non-empty agent_id +
// session_id (see spec-hud-agent-id-correlation-2026-06-01.md). Two halves:
//
//	egress  — buildForwardRequest injects identity into the forwarded params
//	          when, and only when, the call targets the hub.
//	ingress — the receiving daemon parses those top-level fields back into
//	          callParams (the previously-UNVERIFIED sub-assumption).

// --- egress: constructed {name, arguments} branch ---------------------------

func TestBuildForwardRequest_HubEgressInjectsIdentity(t *testing.T) {
	d := newCallPipelineTestDaemon()
	p := newCallPipeline(d, context.Background(), &mcp.Message{
		JSONRPC: mcp.JSONRPCVersion,
		ID:      "hub-inject",
	})
	p.toolName = "status"
	p.method = "tools/call"
	p.target = router.TargetHub
	p.params = callParams{
		AgentID:   "claude-code-552019522",
		SessionID: "lease-abc123",
		AgentType: "claude-code",
		Arguments: json.RawMessage(`{"repo":"loom-core"}`),
	}

	req, errResp := p.buildForwardRequest()
	if errResp != nil {
		t.Fatalf("unexpected error response: %+v", errResp)
	}

	var params map[string]any
	if err := json.Unmarshal(req.Params, &params); err != nil {
		t.Fatalf("unmarshal params: %v", err)
	}
	if params["agent_id"] != "claude-code-552019522" {
		t.Errorf("forwarded agent_id = %v, want claude-code-552019522", params["agent_id"])
	}
	if params["session_id"] != "lease-abc123" {
		t.Errorf("forwarded session_id = %v, want lease-abc123", params["session_id"])
	}
	if params["agent_type"] != "claude-code" {
		t.Errorf("forwarded agent_type = %v, want claude-code", params["agent_type"])
	}
	// The actual call payload must survive intact.
	if params["name"] != "status" {
		t.Errorf("forwarded name = %v, want status", params["name"])
	}
}

// --- egress: verbatim raw-Params branch -------------------------------------

func TestBuildForwardRequest_HubEgressInjectsIdentity_RawParams(t *testing.T) {
	d := newCallPipelineTestDaemon()
	p := newCallPipeline(d, context.Background(), &mcp.Message{
		JSONRPC: mcp.JSONRPCVersion,
		ID:      "hub-inject-raw",
	})
	p.toolName = "search"
	p.method = "tools/call"
	p.target = router.TargetHub
	p.params = callParams{
		AgentID:   "codex-552019522",
		SessionID: "lease-raw",
		Params:    json.RawMessage(`{"name":"search","arguments":{"query":"hello"}}`),
	}

	req, errResp := p.buildForwardRequest()
	if errResp != nil {
		t.Fatalf("unexpected error response: %+v", errResp)
	}

	var params map[string]any
	if err := json.Unmarshal(req.Params, &params); err != nil {
		t.Fatalf("unmarshal params: %v", err)
	}
	if params["agent_id"] != "codex-552019522" {
		t.Errorf("forwarded agent_id = %v, want codex-552019522", params["agent_id"])
	}
	if params["session_id"] != "lease-raw" {
		t.Errorf("forwarded session_id = %v, want lease-raw", params["session_id"])
	}
	if params["name"] != "search" {
		t.Errorf("forwarded name = %v, want search (verbatim payload corrupted)", params["name"])
	}
}

// --- egress: local target must NOT be touched (no regression) ---------------

func TestBuildForwardRequest_LocalTargetNoInjection(t *testing.T) {
	d := newCallPipelineTestDaemon()
	p := newCallPipeline(d, context.Background(), &mcp.Message{
		JSONRPC: mcp.JSONRPCVersion,
		ID:      "local-no-inject",
	})
	p.toolName = "status"
	p.method = "tools/call"
	p.target = router.TargetLocal
	p.params = callParams{
		AgentID:   "claude-code-552019522",
		SessionID: "lease-abc123",
		Arguments: json.RawMessage(`{"repo":"loom-core"}`),
	}

	req, errResp := p.buildForwardRequest()
	if errResp != nil {
		t.Fatalf("unexpected error response: %+v", errResp)
	}

	var params map[string]any
	if err := json.Unmarshal(req.Params, &params); err != nil {
		t.Fatalf("unmarshal params: %v", err)
	}
	if _, ok := params["agent_id"]; ok {
		t.Errorf("local-target params should not carry agent_id, got %v", params["agent_id"])
	}
	if _, ok := params["session_id"]; ok {
		t.Errorf("local-target params should not carry session_id, got %v", params["session_id"])
	}
}

// --- egress: agent_id resolved from the session lease when params omit it ----

func TestBuildForwardRequest_HubEgressResolvesAgentIDFromSession(t *testing.T) {
	d := newCallPipelineTestDaemon()
	d.sessions = NewSessionManager(10, time.Minute, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	sess := d.sessions.Open(SessionClientInfo{PresenceAgentID: "claude-code-552019522"}, "")

	p := newCallPipeline(d, context.Background(), &mcp.Message{
		JSONRPC: mcp.JSONRPCVersion,
		ID:      "hub-inject-from-lease",
	})
	p.toolName = "status"
	p.method = "tools/call"
	p.target = router.TargetHub
	// AgentID intentionally empty: the proxy session lease knows the identity.
	p.params = callParams{SessionID: sess.ID}

	req, errResp := p.buildForwardRequest()
	if errResp != nil {
		t.Fatalf("unexpected error response: %+v", errResp)
	}

	var params map[string]any
	if err := json.Unmarshal(req.Params, &params); err != nil {
		t.Fatalf("unmarshal params: %v", err)
	}
	if params["agent_id"] != "claude-code-552019522" {
		t.Errorf("forwarded agent_id = %v, want claude-code-552019522 (resolved from lease)", params["agent_id"])
	}
	if params["session_id"] != sess.ID {
		t.Errorf("forwarded session_id = %v, want %q", params["session_id"], sess.ID)
	}
}

// --- ingress kill-test: the receiving daemon parses the forwarded identity ---
//
// This is the previously-UNVERIFIED sub-assumption from the relocated keystone:
// that a daemon receiving a forwarded tools/call whose params blob carries
// top-level agent_id/session_id parses them back into its own callParams. It
// proves the egress injection above is not inert on the receiving side. Paired
// with TestEmitAuditPublishesSessionScopedToolCall (which stamps from
// callParams), the full chain — egress inject -> hub -> ingress parse -> audit
// stamp + tool.call event — is covered in-binary.
func TestParseAndResolve_ParsesForwardedIdentity(t *testing.T) {
	d := newCallPipelineTestDaemon()
	msg := newCallMessage(t, map[string]any{
		"server":     "git",
		"tool":       "status",
		"agent_id":   "claude-code-552019522",
		"session_id": "lease-abc123",
		"agent_type": "claude-code",
		"arguments":  map[string]any{"repo": "loom-core"},
	})

	p := newCallPipeline(d, context.Background(), msg)
	if resp := p.parseAndResolve(); resp != nil {
		t.Fatalf("unexpected error response: %+v", resp.Error)
	}

	if p.params.AgentID != "claude-code-552019522" {
		t.Errorf("parsed agent_id = %q, want claude-code-552019522", p.params.AgentID)
	}
	if p.params.SessionID != "lease-abc123" {
		t.Errorf("parsed session_id = %q, want lease-abc123", p.params.SessionID)
	}
	if p.params.AgentType != "claude-code" {
		t.Errorf("parsed agent_type = %q, want claude-code", p.params.AgentType)
	}
}
