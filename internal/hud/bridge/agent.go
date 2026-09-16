// agent.go defines the AgentBridge struct, its constructor, internal helpers,
// and constants used across domain files.
//
// DTO types are in agent_dto.go.
//
// Domain files:
//   - agent_session.go    — Session lifecycle (Start/End/Get/List/Prune) + presence
//   - agent_task.go       — Task CRUD + dispatch
//   - agent_context.go    — Context inspect/stream/knowledge + budget helpers
//   - agent_graph.go      — Knowledge graph: entities, relations, annotations
//   - agent_ops.go        — Workflows, memory, handoffs, coordination
//   - agent_contracts.go  — Shared HTTP request/response contracts
//   - agent_dto.go        — Shared DTO types
package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
	"golang.org/x/sync/singleflight"
)

// AgentBridge wraps agent-context tool calls, routing them through the daemon's
// tools/call endpoint. Each method calls the appropriate agent_context__* tool
// and unmarshals the result into a clean Go struct.
type AgentBridge struct {
	client Caller       // Caller interface (DaemonClient or LocalCaller)
	cache  *Cache       // session lookup cache (internal, always in-memory)
	tracer trace.Tracer // OTel tracer for bridge operations

	// engramGraphFlight coalesces concurrent agent_engram_graph fetches so
	// GET /api/engrams/graph and GET /api/engrams/summary, which the HUD
	// requests together, share one upstream call. engramGraphTTL is how long
	// that result stays reusable afterwards; <= 0 keeps only the in-flight
	// coalescing. See EngramGraph.
	engramGraphFlight singleflight.Group
	engramGraphTTL    time.Duration

	// sessionListFlight coalesces the light agent_session_list projections
	// the HUD monitors poll in overlapping cadences; sessionListTTL is how
	// long a fetched projection is reused and sessionListBudget adapts the
	// recv budget to the store's current latency. See agent_session_light.go.
	sessionListFlight singleflight.Group
	sessionListTTL    time.Duration
	sessionListBudget sessionListBudget
}

const defaultSessionListLimit = 1000

// NewAgentBridge creates an AgentBridge backed by the given Caller.
// The tracer defaults to a no-op; use SetTracer to enable OTel instrumentation.
func NewAgentBridge(client Caller) *AgentBridge {
	return &AgentBridge{
		client:         client,
		cache:          NewCache(),
		tracer:         noop.NewTracerProvider().Tracer(""),
		engramGraphTTL: defaultEngramGraphTTL,
		sessionListTTL: sessionListCacheTTL,
	}
}

// SetTracer replaces the bridge's OTel tracer. Pass nil to revert to no-op.
func (a *AgentBridge) SetTracer(t trace.Tracer) {
	if t == nil {
		t = noop.NewTracerProvider().Tracer("")
	}
	a.tracer = t
}

const (
	contextInspectSystemPromptTokensDefault   = 768
	contextInspectResponseBudgetTokensDefault = 2048
)

// --- Internal helpers ---

func isUnknownToolErr(err error, toolName string) bool {
	if err == nil || strings.TrimSpace(toolName) == "" {
		return false
	}
	return strings.Contains(err.Error(), "unknown tool: "+toolName)
}

// toolCallFn abstracts the difference between CallTool and CallToolWithTimeout.
// The caller binds the tool name, arguments, and optional timeout into the
// closure so that callWithSpan only needs to invoke the function.
type toolCallFn func() (json.RawMessage, error)

// callWithSpan executes a tool call wrapped in an OTel span and handles the
// shared post-call logic: error recording, tool-error envelope checks (when
// target is nil), unmarshalling (when target is non-nil), and span status.
func (a *AgentBridge) callWithSpan(toolName string, call toolCallFn, target any) error {
	_, span := a.tracer.Start(context.Background(), "bridge."+toolName)
	defer span.End()

	raw, err := call()
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("agent tool %s: %w", toolName, err)
	}

	// A tool error envelope is the server's own failure ("list sessions: max
	// retries exceeded: … connection refused"), not a decode failure. Check it
	// before decoding so the log names the tool that failed instead of
	// labelling a dead upstream "unmarshal <tool> result" (the 2026-09-10
	// qdrant outage produced ~5,500 such lines in one day).
	if err := checkToolError(raw); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		if target == nil {
			return err
		}
		return fmt.Errorf("agent tool %s failed: %w", toolName, err)
	}
	if target == nil {
		span.SetStatus(codes.Ok, "")
		return nil
	}
	if err := UnmarshalToolResult(raw, target); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("unmarshal %s result: %w", toolName, err)
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

// callAgentTool invokes an agent_context tool and unmarshals the response
// into the provided target. It unwraps the MCP CallToolResult envelope and
// supports both JSON and TOON (Token-Optimized Object Notation) text payloads.
// Each call produces an OTel span named "bridge.<toolName>".
func (a *AgentBridge) callAgentTool(toolName string, args map[string]any, target any) error {
	return a.callWithSpan(toolName, func() (json.RawMessage, error) {
		return a.client.CallTool("agent_context__"+toolName, args)
	}, target)
}

// callAgentToolTimeout is like callAgentTool but uses a per-call timeout
// override on the underlying DaemonClient RPC.
// Each call produces an OTel span named "bridge.<toolName>".
func (a *AgentBridge) callAgentToolTimeout(toolName string, args map[string]any, target any, timeout time.Duration) error {
	return a.callWithSpan(toolName, func() (json.RawMessage, error) {
		return a.client.CallToolWithTimeout("agent_context__"+toolName, args, timeout)
	}, target)
}

// invalidateSessionCache removes the cached active-session entry for an agent.
func (a *AgentBridge) invalidateSessionCache(agentID string) {
	a.cache.Invalidate("active_session:" + agentID)
	// A session started or ended through this bridge must show on the next
	// monitor tick, not after the light projections' reuse window.
	a.InvalidateSessionLists()
}
