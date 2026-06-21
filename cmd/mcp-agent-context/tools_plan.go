package main

import (
	"context"

	"gitlab.flexinfer.ai/libs/mcp-go"

	"github.com/crb2nu/loom/pkg/agentcontext"

	"go.opentelemetry.io/otel/trace"
)

// registerPlanTools registers the first-class Plan store tools. Plans live in
// the shared global Qdrant and are addressed by a stable plan_id, so a fresh
// agent in ANY worktree/repo (Claude, Codex, or a Mills pod) retrieves the live
// plan by id rather than from a frozen `.loom/` checkout. Reads are scoped by
// plan_id/project/namespace and are deliberately NOT filtered by agent_id.
func registerPlanTools(server *mcp.Server, svc *agentcontext.Service, _ trace.Tracer) {
	server.AddTool(mcp.Tool{
		Name:        "agent_plan_create",
		Description: "Create a first-class, worktree-resilient plan in the shared store. Returns a stable plan_id any agent (Claude/Codex/Mills) can resolve from any worktree. Optionally seed slices.",
		InputSchema: mcp.InputSchema{
			Type: "object",
			Properties: map[string]any{
				"title": map[string]any{
					"type":        "string",
					"description": "Plan title.",
				},
				"id": map[string]any{
					"type":        "string",
					"description": "Optional explicit plan_id. Auto-generated as plan-<slug>-<short> when omitted.",
				},
				"project": map[string]any{
					"type":        "string",
					"description": "Canonical project id (e.g. GitLab path_with_namespace). Scopes list queries.",
				},
				"namespace": map[string]any{
					"type":        "string",
					"description": "Conventional project/branch namespace.",
				},
				"phase": map[string]any{
					"type":        "string",
					"enum":        []string{"draft", "planned", "in_progress", "in_review", "merging", "merged", "deployed", "done", "abandoned"},
					"description": "Lifecycle phase (default: 'draft').",
				},
				"spec_doc": map[string]any{
					"type":        "string",
					"description": "Plan/spec body (markdown). Canonical content; the .loom mirror is rendered from this.",
				},
				"slices": map[string]any{
					"type":        "array",
					"description": "Optional seed slices.",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"name":  map[string]any{"type": "string"},
							"goal":  map[string]any{"type": "string"},
							"files": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
						},
					},
				},
				"agent_id": map[string]any{
					"type":        "string",
					"description": "Creating agent (attribution only; does not scope reads).",
				},
				"session_id": map[string]any{
					"type":        "string",
					"description": "Source session id (provenance).",
				},
			},
			Required: []string{"title"},
		},
	}, func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		return svc.HandlePlanCreate(ctx, args)
	})

	server.AddTool(mcp.Tool{
		Name:        "agent_plan_get",
		Description: "Fetch a plan (and its slices) by plan_id from the shared store. Cross-agent and cross-worktree: NOT filtered by agent_id.",
		InputSchema: mcp.InputSchema{
			Type: "object",
			Properties: map[string]any{
				"plan_id": map[string]any{
					"type":        "string",
					"description": "The plan_id returned by agent_plan_create.",
				},
			},
			Required: []string{"plan_id"},
		},
	}, func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		return svc.HandlePlanGet(ctx, args)
	})

	server.AddTool(mcp.Tool{
		Name:        "agent_plan_list",
		Description: "List plans filtered by project and/or namespace. Cross-agent: NOT filtered by agent_id.",
		InputSchema: mcp.InputSchema{
			Type: "object",
			Properties: map[string]any{
				"project":   map[string]any{"type": "string", "description": "Filter by canonical project id."},
				"namespace": map[string]any{"type": "string", "description": "Filter by namespace."},
				"limit":     map[string]any{"type": "integer", "description": "Max plans to return (default 100)."},
			},
		},
	}, func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		return svc.HandlePlanList(ctx, args)
	})
}
