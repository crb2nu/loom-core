package main

import (
	"context"

	"gitlab.flexinfer.ai/libs/mcp-go"
	"go.opentelemetry.io/otel/trace"

	"github.com/crb2nu/loom/pkg/agentcontext"
	"github.com/crb2nu/loom/pkg/validate"
)

func registerMaintenanceTools(server *mcp.Server, svc *agentcontext.Service, tracer trace.Tracer) {
	server.AddTool(mcp.Tool{
		Name:        "agent_embedding_backfill",
		Description: "Repair one bounded page of context or pattern fallback embeddings; pass cursor from the prior response to resume.",
		InputSchema: mcp.InputSchema{Type: "object", Properties: map[string]any{
			"collection": map[string]any{"type": "string", "enum": []string{"context", "patterns"}},
			"limit":      map[string]any{"type": "integer", "minimum": 1, "maximum": 256},
			"cursor":     map[string]any{"description": "Opaque Qdrant scroll cursor from the previous response."},
		}, Required: []string{"collection"}},
	}, traced(tracer, "agent_embedding_backfill", func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		v := validate.NewArgs(args)
		collection := v.Required("collection")
		limit := v.Int("limit", 100)
		cursor := v.Any("cursor")
		if err := v.Validate(); err != nil {
			return mcp.ErrorResult(err), nil
		}
		result, err := svc.HandleBackfillFallbackVectors(ctx, collection, limit, cursor)
		if err != nil {
			return mcp.ErrorResult(err), nil
		}
		return mcp.JSONResult(result)
	}))
}
