package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"gitlab.flexinfer.ai/libs/mcp-go"

	"github.com/crb2nu/loom/pkg/openairesponses"
	"github.com/crb2nu/loom/pkg/weaver"
)

// weaverQueryRequestFromParams decodes the JSON params of a
// loom/weaver/query request into a weaver.QueryRequest. It is the single
// source of truth for the wire->request mapping so every field forwarded
// by callers (notably parent_session_id, which the Mills weaver
// delegator sets to the pipeline run ID for session stitching) survives
// the hop. Returns an error for malformed JSON or an empty query.
func weaverQueryRequestFromParams(raw json.RawMessage) (weaver.QueryRequest, error) {
	var params struct {
		Query           string   `json:"query"`
		Domains         []string `json:"domains,omitempty"`
		MaxTokens       int      `json:"max_tokens,omitempty"`
		AgentID         string   `json:"agent_id,omitempty"`
		SessionID       string   `json:"session_id,omitempty"`
		ParentSessionID string   `json:"parent_session_id,omitempty"`
	}
	if err := json.Unmarshal(raw, &params); err != nil {
		return weaver.QueryRequest{}, fmt.Errorf("invalid params: %v", err)
	}
	if params.Query == "" {
		return weaver.QueryRequest{}, fmt.Errorf("query is required")
	}
	return weaver.QueryRequest{
		Query:           params.Query,
		Domains:         params.Domains,
		MaxTokens:       params.MaxTokens,
		ParentSessionID: params.ParentSessionID,
		Identity: openairesponses.ExecutionIdentity{
			AgentID:   params.AgentID,
			SessionID: params.SessionID,
		},
	}, nil
}

// handleWeaverQuery handles loom/weaver/query requests.
func (d *Daemon) handleWeaverQuery(ctx context.Context, msg *mcp.Message) (*mcp.Message, error) {
	if d.weaver == nil {
		return newErrorResponse(msg.ID, mcp.InternalError,
			"weaver is not enabled", nil), nil
	}

	req, err := weaverQueryRequestFromParams(msg.Params)
	if err != nil {
		return newErrorResponse(msg.ID, mcp.InvalidParams, err.Error(), nil), nil
	}

	result, err := d.weaver.Query(ctx, req)
	if err != nil {
		return newErrorResponse(msg.ID, mcp.InternalError,
			fmt.Sprintf("weaver query failed: %v", err), nil), nil
	}

	return mcp.NewResponse(msg.ID, result)
}

// handleWeaverGather handles loom/weaver/gather requests.
func (d *Daemon) handleWeaverGather(ctx context.Context, msg *mcp.Message) (*mcp.Message, error) {
	if d.weaver == nil {
		return newErrorResponse(msg.ID, mcp.InternalError,
			"weaver is not enabled", nil), nil
	}

	var params struct {
		Query     string   `json:"query"`
		Domains   []string `json:"domains"`
		AgentID   string   `json:"agent_id,omitempty"`
		SessionID string   `json:"session_id,omitempty"`
	}
	if err := json.Unmarshal(msg.Params, &params); err != nil {
		return newErrorResponse(msg.ID, mcp.InvalidParams,
			fmt.Sprintf("invalid params: %v", err), nil), nil
	}
	if params.Query == "" {
		return newErrorResponse(msg.ID, mcp.InvalidParams,
			"query is required", nil), nil
	}
	if len(params.Domains) == 0 {
		return newErrorResponse(msg.ID, mcp.InvalidParams,
			"domains is required for gather", nil), nil
	}

	identity := openairesponses.ExecutionIdentity{
		AgentID:   params.AgentID,
		SessionID: params.SessionID,
	}

	result, err := d.weaver.Gather(ctx, params.Domains, params.Query, identity)
	if err != nil {
		return newErrorResponse(msg.ID, mcp.InternalError,
			fmt.Sprintf("weaver gather failed: %v", err), nil), nil
	}

	return mcp.NewResponse(msg.ID, result)
}

// handleWeaverStatus handles loom/weaver/status requests. Merges the
// router's static config snapshot with the daemon's most recent
// preflight (model catalog availability). HUD/iOS/extension surface
// the merged degraded/missing_models/ready_models fields as a yellow
// banner so operators see broken model bindings before the first
// query 404s.
func (d *Daemon) handleWeaverStatus(_ context.Context, msg *mcp.Message) (*mcp.Message, error) {
	if d.weaver == nil {
		return mcp.NewResponse(msg.ID, map[string]any{
			"enabled": false,
		})
	}
	status := d.weaver.Status()
	if pre, ok := d.weaverPreflight.Get(); ok {
		status["degraded"] = pre.Degraded
		status["missing_models"] = pre.MissingModels
		status["ready_models"] = pre.ReadyModels
		status["catalog_size"] = pre.CatalogSize
		if pre.CatalogError != "" {
			status["catalog_error"] = pre.CatalogError
		}
		status["preflight_at"] = pre.CheckedAt.Format(time.RFC3339)
	}
	return mcp.NewResponse(msg.ID, status)
}

// handleWeaverHistory handles loom/weaver/history requests.
func (d *Daemon) handleWeaverHistory(_ context.Context, msg *mcp.Message) (*mcp.Message, error) {
	if d.weaver == nil {
		return mcp.NewResponse(msg.ID, map[string]any{
			"entries": []any{},
		})
	}
	return mcp.NewResponse(msg.ID, map[string]any{
		"entries": d.weaver.History(),
	})
}

// handleWeaverMetrics handles loom/weaver/metrics requests.
func (d *Daemon) handleWeaverMetrics(_ context.Context, msg *mcp.Message) (*mcp.Message, error) {
	if d.weaver == nil {
		return mcp.NewResponse(msg.ID, map[string]any{
			"total_queries": 0, "avg_latency_ms": 0,
			"error_rate": 0, "total_tokens": 0, "error_count": 0,
		})
	}
	summary := d.weaver.MetricsSummary()
	if summary == nil {
		return mcp.NewResponse(msg.ID, map[string]any{
			"total_queries": 0, "avg_latency_ms": 0,
			"error_rate": 0, "total_tokens": 0, "error_count": 0,
		})
	}
	return mcp.NewResponse(msg.ID, summary)
}
