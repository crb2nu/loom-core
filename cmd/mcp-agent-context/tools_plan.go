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
				"objective": map[string]any{
					"type":        "string",
					"description": "The plan's synthesized end-state + the through-line connecting its slices (2-4 sentences). NOT an echo of the raw brief — the 'why' a reviewer reads before the slice list. Distinct from spec_doc (which holds the full spec/brief body).",
				},
				"priority": map[string]any{
					"type":        "string",
					"enum":        []string{"P0", "P1", "P2", "P3"},
					"description": "Warp-beam priority bucket (P0 highest). Propagates onto Mills backlog items emitted from this plan's slices; queued items dispatch in priority order.",
				},
				"spec_doc": map[string]any{
					"type":        "string",
					"description": "Plan/spec body (markdown). Canonical content; the .loom mirror is rendered from this.",
				},
				"slices": map[string]any{
					"type":        "array",
					"description": "Optional seed slices. Beyond name/goal/files each slice may carry connective tissue: depends_on (earlier slices this one needs — referenced by slice NAME; the store resolves names to slice_ids), interface_contracts (what this slice PROVIDES for later slices / CONSUMES from earlier ones), and acceptance_criteria.",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"name":                map[string]any{"type": "string"},
							"goal":                map[string]any{"type": "string"},
							"files":               map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
							"depends_on":          map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Names (or slice_ids) of earlier slices that must land first — the DAG edges. Names are resolved to slice_ids on create."},
							"interface_contracts": map[string]any{"type": "string", "description": "The contract this slice provides for later slices and/or consumes from earlier ones (e.g. slice 1 publishing the schema the rest code against)."},
							"acceptance_criteria": map[string]any{"type": "string"},
							"test_strategy":       map[string]any{"type": "string"},
							"branch_name":         map[string]any{"type": "string"},
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
				"respun_from": map[string]any{
					"type":        "string",
					"description": "Optional source plan_id this plan was respun from (an operator redoing an older/sparse plan in the Spinning Room). Provenance only; links the fresh draft back to the plan it supersedes.",
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
		Description: "List plans filtered by project, namespace, and/or phase. Cross-agent: NOT filtered by agent_id.",
		InputSchema: mcp.InputSchema{
			Type: "object",
			Properties: map[string]any{
				"project":   map[string]any{"type": "string", "description": "Filter by canonical project id."},
				"namespace": map[string]any{"type": "string", "description": "Filter by namespace."},
				"phase":     map[string]any{"type": "string", "description": "Filter by lifecycle phase."},
				"limit":     map[string]any{"type": "integer", "description": "Max plans to return (default 100)."},
			},
		},
	}, func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		return svc.HandlePlanList(ctx, args)
	})

	server.AddTool(mcp.Tool{
		Name:        "agent_plan_update",
		Description: "Patch mutable plan fields (objective, spec_doc, title, project, namespace, priority, success criteria, kill_test_status, mirror_path, mills_backlog_id) and append MR/pipeline/deploy refs. Use project/namespace to correct a mis-scoped plan in place (the HUD groups by the exact project string). Phase changes must use agent_plan_lifecycle_advance.",
		InputSchema: mcp.InputSchema{
			Type: "object",
			Properties: map[string]any{
				"plan_id":          map[string]any{"type": "string", "description": "Plan to update."},
				"title":            map[string]any{"type": "string"},
				"project":          map[string]any{"type": "string", "description": "Re-scope the plan's canonical project id (e.g. correct bare 'loom-core' to 'services/loom-core'). The HUD groups by this exact string."},
				"namespace":        map[string]any{"type": "string", "description": "Re-scope the plan's project/branch namespace."},
				"objective":        map[string]any{"type": "string", "description": "Set/replace the plan's synthesized end-state + through-line (2-4 sentences). Use to enrich a plan authored before it had an objective, or a sparse spun draft."},
				"spec_doc":         map[string]any{"type": "string"},
				"spec_anchor":      map[string]any{"type": "string"},
				"priority":         map[string]any{"type": "string", "enum": []string{"P0", "P1", "P2", "P3", ""}, "description": "Set the warp-beam priority bucket (P0 highest; empty string clears). Still-queued Mills items emitted from this plan resync on the emitter's next tick."},
				"mirror_path":      map[string]any{"type": "string", "description": "Path of the rendered .loom mirror."},
				"kill_test_status": map[string]any{"type": "string"},
				"mills_backlog_id": map[string]any{"type": "string"},
				"add_mr_ref":       map[string]any{"type": "string", "description": "Append an MR ref/URL."},
				"add_pipeline_ref": map[string]any{"type": "string", "description": "Append a pipeline ref/URL."},
				"add_deploy_ref":   map[string]any{"type": "string", "description": "Append a deploy ref/URL."},
				"success": map[string]any{
					"type":        "object",
					"description": "Replace success criteria.",
					"properties": map[string]any{
						"tests":        map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
						"metrics":      map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
						"manual_check": map[string]any{"type": "string"},
					},
				},
			},
			Required: []string{"plan_id"},
		},
	}, func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		return svc.HandlePlanUpdate(ctx, args)
	})

	server.AddTool(mcp.Tool{
		Name:        "agent_plan_search",
		Description: "Semantic search over plan title+spec, optionally scoped to a project. Falls back to a keyword list if no embedder is available. Cross-agent.",
		InputSchema: mcp.InputSchema{
			Type: "object",
			Properties: map[string]any{
				"query":   map[string]any{"type": "string", "description": "Natural-language query."},
				"project": map[string]any{"type": "string", "description": "Optional project scope."},
				"limit":   map[string]any{"type": "integer", "description": "Max results (default 20)."},
			},
			Required: []string{"query"},
		},
	}, func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		return svc.HandlePlanSearch(ctx, args)
	})

	server.AddTool(mcp.Tool{
		Name:        "agent_plan_lifecycle_advance",
		Description: "Advance a plan to a new lifecycle phase (draft→planned→in_progress→in_review→merging→merged→deployed→done; abandoned from any). Validates the transition and records it in phase_history.",
		InputSchema: mcp.InputSchema{
			Type: "object",
			Properties: map[string]any{
				"plan_id":  map[string]any{"type": "string"},
				"to_phase": map[string]any{"type": "string", "enum": []string{"draft", "planned", "in_progress", "in_review", "merging", "merged", "deployed", "done", "abandoned"}},
				"agent_id": map[string]any{"type": "string", "description": "Actor (attribution for the transition)."},
				"note":     map[string]any{"type": "string", "description": "Why the transition happened."},
			},
			Required: []string{"plan_id", "to_phase"},
		},
	}, func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		return svc.HandlePlanLifecycleAdvance(ctx, args)
	})

	server.AddTool(mcp.Tool{
		Name:        "agent_plan_render",
		Description: "Render a plan's human/MR-reviewable markdown mirror from the store (the store is canonical). With `path`, writes the file atomically and records it as the plan's mirror_path. Always returns the markdown.",
		InputSchema: mcp.InputSchema{
			Type: "object",
			Properties: map[string]any{
				"plan_id":         map[string]any{"type": "string"},
				"path":            map[string]any{"type": "string", "description": "Optional file path to write the mirror to (e.g. .loom/NNN-plan-<slug>-<date>.md). Written atomically."},
				"set_mirror_path": map[string]any{"type": "boolean", "description": "Record the written path on the plan (default true)."},
			},
			Required: []string{"plan_id"},
		},
	}, func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		return svc.HandlePlanRender(ctx, args)
	})

	registerPlanSliceTools(server, svc)
}

// registerPlanSliceTools registers the slice-level tools. Slices are their own
// records so a fresh slice-implementer resolves its work by slice_id (cross-
// agent, NOT prompt-bound) and records status/decisions back to the shared record.
func registerPlanSliceTools(server *mcp.Server, svc *agentcontext.Service) {
	server.AddTool(mcp.Tool{
		Name:        "agent_plan_slice_add",
		Description: "Append a slice to a plan. Returns slice_id <plan_id>#<order>.",
		InputSchema: mcp.InputSchema{
			Type: "object",
			Properties: map[string]any{
				"plan_id":             map[string]any{"type": "string"},
				"name":                map[string]any{"type": "string"},
				"goal":                map[string]any{"type": "string"},
				"files":               map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Disjoint file set this slice owns (basis for claim enforcement)."},
				"acceptance_criteria": map[string]any{"type": "string"},
				"test_strategy":       map[string]any{"type": "string"},
				"interface_contracts": map[string]any{"type": "string"},
				"branch_name":         map[string]any{"type": "string"},
				"depends_on":          map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "slice_ids this slice depends on."},
			},
			Required: []string{"plan_id", "name"},
		},
	}, func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		return svc.HandlePlanSliceAdd(ctx, args)
	})

	server.AddTool(mcp.Tool{
		Name:        "agent_plan_slice_get",
		Description: "Fetch one slice by slice_id. This is how a fresh slice-implementer looks up its own scope. Cross-agent: NOT filtered by agent_id.",
		InputSchema: mcp.InputSchema{
			Type:       "object",
			Properties: map[string]any{"slice_id": map[string]any{"type": "string"}},
			Required:   []string{"slice_id"},
		},
	}, func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		return svc.HandlePlanSliceGet(ctx, args)
	})

	server.AddTool(mcp.Tool{
		Name:        "agent_plan_slice_list",
		Description: "List all slices for a plan, ordered.",
		InputSchema: mcp.InputSchema{
			Type:       "object",
			Properties: map[string]any{"plan_id": map[string]any{"type": "string"}},
			Required:   []string{"plan_id"},
		},
	}, func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		return svc.HandlePlanSliceList(ctx, args)
	})

	server.AddTool(mcp.Tool{
		Name:        "agent_plan_slice_update",
		Description: "Update a slice's phase/refs and APPEND decisions or commit refs. This is where a slice-implementer records status and blockers back to the shared record (instead of losing them to its context window).",
		InputSchema: mcp.InputSchema{
			Type: "object",
			Properties: map[string]any{
				"slice_id":       map[string]any{"type": "string"},
				"phase":          map[string]any{"type": "string", "enum": []string{"pending", "claimed", "implementing", "implemented", "in_review", "integrated", "merged"}},
				"mr_ref":         map[string]any{"type": "string"},
				"branch_name":    map[string]any{"type": "string"},
				"add_commit_ref": map[string]any{"type": "string"},
				"add_decision":   map[string]any{"type": "string", "description": "Append a decision/blocker note for the orchestrator."},
				"files":          map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			},
			Required: []string{"slice_id"},
		},
	}, func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		return svc.HandlePlanSliceUpdate(ctx, args)
	})

	server.AddTool(mcp.Tool{
		Name:        "agent_plan_slice_claim",
		Description: "Claim a slice for an agent (sets assignee + worktree, marks 'claimed'). Returns conflict if another agent holds it unless force=true.",
		InputSchema: mcp.InputSchema{
			Type: "object",
			Properties: map[string]any{
				"slice_id":    map[string]any{"type": "string"},
				"agent_id":    map[string]any{"type": "string"},
				"session_id":  map[string]any{"type": "string"},
				"worktree_id": map[string]any{"type": "string"},
				"branch_name": map[string]any{"type": "string"},
				"force":       map[string]any{"type": "boolean", "description": "Steal an already-held slice."},
			},
			Required: []string{"slice_id", "agent_id"},
		},
	}, func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		return svc.HandlePlanSliceClaim(ctx, args)
	})
}
