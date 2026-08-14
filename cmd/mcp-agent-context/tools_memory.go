package main

import (
	"context"

	"gitlab.flexinfer.ai/libs/mcp-go"

	"github.com/crb2nu/loom/pkg/agentcontext"

	"go.opentelemetry.io/otel/trace"
)

func registerMemoryTools(server *mcp.Server, svc *agentcontext.Service, tracer trace.Tracer) {
	// Memory Hierarchy Tools

	server.AddTool(mcp.Tool{
		Name:        "agent_memory_add",
		Description: "Add items to the tiered memory hierarchy. Items start in working memory and can be promoted to short-term or long-term memory.",
		InputSchema: mcp.InputSchema{
			Type: "object",
			Properties: map[string]any{
				"session_id": map[string]any{
					"type":        "string",
					"description": "Session ID for the memories.",
				},
				"agent_id": map[string]any{
					"type":        "string",
					"description": "Agent ID.",
				},
				"namespace": map[string]any{
					"type":        "string",
					"description": "Namespace for organization.",
				},
				"items": map[string]any{
					"type":        "array",
					"description": "Memory items to add.",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"title": map[string]any{
								"type":        "string",
								"description": "Short title for the memory.",
							},
							"content": map[string]any{
								"type":        "string",
								"description": "Full content of the memory.",
							},
							"tier": map[string]any{
								"type":        "string",
								"enum":        []string{"working", "short_term", "long_term"},
								"description": "Memory tier (default: working).",
							},
							"importance": map[string]any{
								"type":        "string",
								"enum":        []string{"low", "medium", "high", "critical"},
								"description": "Importance level (default: medium).",
							},
							"category": map[string]any{
								"type":        "string",
								"description": "Category for grouping (e.g., 'decision', 'insight', 'task').",
							},
							"tags": map[string]any{
								"type":        "array",
								"items":       map[string]any{"type": "string"},
								"description": "Tags for filtering.",
							},
							"metadata": map[string]any{
								"type":        "object",
								"description": "Additional metadata.",
							},
							"related_ids": map[string]any{
								"type":        "array",
								"items":       map[string]any{"type": "string"},
								"description": "Related memory item IDs.",
							},
						},
						"required": []string{"title", "content"},
					},
				},
			},
			Required: []string{"items"},
		},
	}, traced(tracer, "agent_memory_add", func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		return svc.HandleMemoryAdd(ctx, args)
	}))

	server.AddTool(mcp.Tool{
		Name:        "agent_memory_get",
		Description: "Retrieve memory items by ID.",
		InputSchema: mcp.InputSchema{
			Type: "object",
			Properties: map[string]any{
				"item_ids": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"description": "Memory item IDs to retrieve.",
				},
			},
			Required: []string{"item_ids"},
		},
	}, func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		return svc.HandleMemoryGet(ctx, args)
	})

	server.AddTool(mcp.Tool{
		Name:        "agent_memory_delete",
		Description: "Delete memory items.",
		InputSchema: mcp.InputSchema{
			Type: "object",
			Properties: map[string]any{
				"item_ids": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"description": "Memory item IDs to delete.",
				},
				"confirm": map[string]any{
					"type":        "boolean",
					"description": "Must be true to confirm deletion.",
				},
			},
			Required: []string{"item_ids", "confirm"},
		},
	}, func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		return svc.HandleMemoryDelete(ctx, args)
	})

	// NOTE: agent_memory_compress and agent_memory_merge removed in SIMP-2.
	// Tier management is automatic via background compaction. Underlying
	// handlers remain for internal/CLI use. agent_memory_promote and
	// agent_memory_demote were re-registered: the HUD memory panel
	// (POST /api/memory/{id}/promote|demote) calls them over MCP.

	server.AddTool(mcp.Tool{
		Name:        "agent_memory_promote",
		Description: "Promote memory items to a higher tier (working -> short_term -> long_term).",
		InputSchema: mcp.InputSchema{
			Type: "object",
			Properties: map[string]any{
				"item_ids": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"description": "Memory item IDs to promote.",
				},
			},
			Required: []string{"item_ids"},
		},
	}, func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		return svc.HandleMemoryPromote(ctx, args)
	})

	server.AddTool(mcp.Tool{
		Name:        "agent_memory_demote",
		Description: "Demote memory items to a lower tier (long_term -> short_term -> working).",
		InputSchema: mcp.InputSchema{
			Type: "object",
			Properties: map[string]any{
				"item_ids": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"description": "Memory item IDs to demote.",
				},
			},
			Required: []string{"item_ids"},
		},
	}, func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		return svc.HandleMemoryDemote(ctx, args)
	})

	server.AddTool(mcp.Tool{
		Name:        "agent_memory_stats",
		Description: "Get statistics about the memory hierarchy.",
		InputSchema: mcp.InputSchema{
			Type:       "object",
			Properties: map[string]any{},
		},
	}, func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		return svc.HandleMemoryStats(ctx, args)
	})

	server.AddTool(mcp.Tool{
		Name:        "agent_memory_policy_get",
		Description: "Get the retention policy for a memory tier.",
		InputSchema: mcp.InputSchema{
			Type: "object",
			Properties: map[string]any{
				"tier": map[string]any{
					"type":        "string",
					"enum":        []string{"working", "short_term", "long_term"},
					"description": "Memory tier.",
				},
			},
			Required: []string{"tier"},
		},
	}, func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		return svc.HandleMemoryPolicyGet(ctx, args)
	})

	// NOTE: agent_memory_policy_set removed in SIMP-2. Policy configuration is
	// config-only (AGENT_CONTEXT_* env); there is no `loom agent` equivalent.
	// agent_memory_policy_get retained for read-only introspection.

	// NOTE: agent_memory_export and agent_memory_import removed in SIMP-3.
	// The capability was removed, not relocated: no CLI equivalent exists.
	// Memory persistence is handled by Qdrant storage.

	// NOTE: agent_memory_recall removed. It was a deprecated alias; call
	// agent_recall with scope="memory".
}
