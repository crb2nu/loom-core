package main

import (
	"context"

	"gitlab.flexinfer.ai/libs/mcp-go"

	"github.com/crb2nu/loom/pkg/agentcontext"

	"go.opentelemetry.io/otel/trace"
)

// NOTE: agent_compaction_trigger and agent_reconcile_trigger removed in
// SIMP-6. Background schedulers continue running automatically; manual
// triggering is available via CLI: loom agent compaction / reconcile.
// agent_compaction_status was re-registered: the HUD memory panel compaction
// tile calls it over MCP.
func registerCompactionTools(server *mcp.Server, svc *agentcontext.Service, _ trace.Tracer) {
	server.AddTool(mcp.Tool{
		Name:        "agent_compaction_status",
		Description: "Get compaction scheduler status and last run statistics.",
		InputSchema: mcp.InputSchema{
			Type:       "object",
			Properties: map[string]any{},
		},
	}, func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		return svc.HandleCompactionStatus(ctx, args)
	})
}
