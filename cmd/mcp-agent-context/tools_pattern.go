package main

import (
	"context"

	"gitlab.flexinfer.ai/libs/mcp-go"

	"github.com/crb2nu/loom/pkg/agentcontext"

	"go.opentelemetry.io/otel/trace"
)

// registerPatternTools registers the Pattern catalog tools (the "pattern
// library"). A Pattern is a vetted product archetype that, given Materials, is
// stamped into a Plan that Mills executes to a deployed instance. Like Plans,
// patterns live in the shared global Qdrant and are read cross-agent (NOT
// filtered by agent_id), so any agent/Mills pod resolves the live catalog by id.
func registerPatternTools(server *mcp.Server, svc *agentcontext.Service, _ trace.Tracer) {
	strArray := map[string]any{"type": "array", "items": map[string]any{"type": "string"}}

	server.AddTool(mcp.Tool{
		Name:        "agent_pattern_add",
		Description: "Register (or upsert) a Pattern in the shared catalog. A Pattern pins the load-bearing architecture so only Materials vary; it declares materials_schema, tools_manifest, pins, a gauge kill-test, a slice_template, and a deploy_contract. Returns a stable pattern_id (pattern-<slug>).",
		InputSchema: mcp.InputSchema{
			Type: "object",
			Properties: map[string]any{
				"name":            map[string]any{"type": "string", "description": "Pattern name (maps to a stable pattern-<slug> id)."},
				"makes":           map[string]any{"type": "string", "description": "The type of thing this produces, e.g. 'Go REST microservice'."},
				"id":              map[string]any{"type": "string", "description": "Optional explicit pattern_id. Auto-generated as pattern-<slug> when omitted."},
				"description":     map[string]any{"type": "string"},
				"version":         map[string]any{"type": "string", "description": "Pattern version (default '0.1')."},
				"status":          map[string]any{"type": "string", "enum": []string{"candidate", "approved", "deprecated"}, "description": "Approval status (default 'candidate')."},
				"deploy_contract": map[string]any{"type": "string", "description": "What 'deployed working version' means for this type."},
				"engrams":         mergeMap(strArray, "description", "Composed engram URIs."),
				"tags":            strArray,
				"materials_schema": map[string]any{
					"type":        "array",
					"description": "Typed inputs the user supplies (the 'fabric').",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"name":        map[string]any{"type": "string"},
							"type":        map[string]any{"type": "string", "description": "string|int|bool|enum|list|object"},
							"required":    map[string]any{"type": "boolean"},
							"description": map[string]any{"type": "string"},
							"enum":        strArray,
							"default":     map[string]any{"type": "string"},
							"example":     map[string]any{"type": "string"},
						},
					},
				},
				"tools_manifest": map[string]any{
					"type":        "array",
					"description": "Required environment capabilities (the 'basic tools'). Stamp aborts if a required tool is absent.",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"name":     map[string]any{"type": "string"},
							"kind":     map[string]any{"type": "string", "description": "toolchain|mcp_server|deploy_target|secret"},
							"required": map[string]any{"type": "boolean"},
							"check":    map[string]any{"type": "string", "description": "command or mcp tool name to verify presence"},
						},
					},
				},
				"pins": map[string]any{
					"type":        "array",
					"description": "Closed architecture decisions (axis -> pinned value) that make the pattern stampable.",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"axis":  map[string]any{"type": "string"},
							"value": map[string]any{"type": "string"},
						},
					},
				},
				"gauge": map[string]any{
					"type":        "object",
					"description": "The swatch: smallest end-to-end check proving a stamp is correct in this environment.",
					"properties": map[string]any{
						"description": map[string]any{"type": "string"},
						"commands":    strArray,
						"assertions":  strArray,
					},
				},
				"slice_template": map[string]any{
					"type":        "array",
					"description": "Slice blueprints; expanded with Materials into PlanSlices on Stamp.",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"name":                map[string]any{"type": "string"},
							"goal":                map[string]any{"type": "string"},
							"files":               strArray,
							"acceptance_criteria": map[string]any{"type": "string"},
							"engrams":             strArray,
						},
					},
				},
				"provenance": map[string]any{
					"type":        "object",
					"description": "Taste gate: author/approver + green-instance count.",
					"properties": map[string]any{
						"author":                  map[string]any{"type": "string"},
						"approved_by":             map[string]any{"type": "string"},
						"instances_shipped_green": map[string]any{"type": "integer"},
						"notes":                   map[string]any{"type": "string"},
					},
				},
				"agent_id": map[string]any{"type": "string", "description": "Creating agent (attribution only)."},
			},
			Required: []string{"name", "makes"},
		},
	}, func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		return svc.HandlePatternAdd(ctx, args)
	})

	server.AddTool(mcp.Tool{
		Name:        "agent_pattern_get",
		Description: "Fetch a Pattern by pattern_id from the shared catalog. Cross-agent and cross-worktree: NOT filtered by agent_id.",
		InputSchema: mcp.InputSchema{
			Type:       "object",
			Properties: map[string]any{"pattern_id": map[string]any{"type": "string", "description": "The pattern_id (e.g. pattern-go-rest-service)."}},
			Required:   []string{"pattern_id"},
		},
	}, func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		return svc.HandlePatternGet(ctx, args)
	})

	server.AddTool(mcp.Tool{
		Name:        "agent_pattern_list",
		Description: "List patterns filtered by makes (type of thing) and/or status. Cross-agent.",
		InputSchema: mcp.InputSchema{
			Type: "object",
			Properties: map[string]any{
				"makes":  map[string]any{"type": "string", "description": "Filter by the type of thing produced."},
				"status": map[string]any{"type": "string", "enum": []string{"candidate", "approved", "deprecated"}, "description": "Filter by approval status."},
				"limit":  map[string]any{"type": "integer", "description": "Max patterns to return (default 100)."},
			},
		},
	}, func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		return svc.HandlePatternList(ctx, args)
	})

	server.AddTool(mcp.Tool{
		Name:        "agent_pattern_search",
		Description: "Semantic search over pattern name+makes+description. Falls back to a keyword list if no embedder is available. Cross-agent.",
		InputSchema: mcp.InputSchema{
			Type: "object",
			Properties: map[string]any{
				"query": map[string]any{"type": "string", "description": "Natural-language query (e.g. 'http json crud service')."},
				"limit": map[string]any{"type": "integer", "description": "Max results (default 20)."},
			},
			Required: []string{"query"},
		},
	}, func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		return svc.HandlePatternSearch(ctx, args)
	})

	server.AddTool(mcp.Tool{
		Name:        "agent_pattern_stamp",
		Description: "Stamp a Pattern with Materials: validate the materials against the pattern's schema, then expand its slice_template into a concrete Plan in the shared store. Returns the new plan_id (executable by Mills). Pass available_tools (the probed environment) to ENFORCE the pattern's tools_manifest — the stamp aborts if a required tool is missing. To run it through Mills, the HUD's POST /api/patterns/stamp with enqueue=true projects the Plan into a queued BacklogItem.",
		InputSchema: mcp.InputSchema{
			Type: "object",
			Properties: map[string]any{
				"pattern_id":      map[string]any{"type": "string", "description": "Pattern to stamp (e.g. pattern-go-rest-service)."},
				"materials":       map[string]any{"type": "object", "description": "Typed inputs filling the pattern's materials_schema (the 'fabric')."},
				"project":         map[string]any{"type": "string", "description": "Canonical project id for the stamped plan."},
				"namespace":       map[string]any{"type": "string", "description": "Namespace for the stamped plan."},
				"agent_id":        map[string]any{"type": "string", "description": "Stamping agent (attribution)."},
				"available_tools": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Optional: the capabilities present in the target environment (capability names like 'devbox','gitlab' or raw MCP tool names). When supplied, the stamp enforces the pattern's tools_manifest and aborts loudly if a required tool is absent. Omit to surface tools_required without gating."},
			},
			Required: []string{"pattern_id", "materials"},
		},
	}, func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		return svc.HandlePatternStamp(ctx, args)
	})

	server.AddTool(mcp.Tool{
		Name:        "agent_pattern_promote",
		Description: "Change a Pattern's approval status (the taste gate). With no to_status, promotes candidate→approved iff it has enough green instances. With to_status, sets it explicitly; approving below threshold or any other transition requires force=true (human curation). Mills rails + the front door offer approved patterns by default.",
		InputSchema: mcp.InputSchema{
			Type: "object",
			Properties: map[string]any{
				"pattern_id": map[string]any{"type": "string"},
				"to_status":  map[string]any{"type": "string", "enum": []string{"candidate", "approved", "deprecated"}, "description": "Target status. Omit to auto-promote candidate→approved on threshold."},
				"force":      map[string]any{"type": "boolean", "description": "Human-curation override: approve below the green-instance threshold, or set any status."},
			},
			Required: []string{"pattern_id"},
		},
	}, func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		return svc.HandlePatternPromote(ctx, args)
	})

	server.AddTool(mcp.Tool{
		Name:        "agent_pattern_record_instance",
		Description: "Record that a stamped instance of a Pattern shipped green (increments instances_shipped_green). The stamp/merge path calls this on a green merge; a candidate auto-promotes to approved once the threshold is reached.",
		InputSchema: mcp.InputSchema{
			Type: "object",
			Properties: map[string]any{
				"pattern_id": map[string]any{"type": "string"},
				"mr_ref":     map[string]any{"type": "string", "description": "Optional MR/commit ref of the green instance (recorded in provenance notes)."},
			},
			Required: []string{"pattern_id"},
		},
	}, func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		return svc.HandlePatternRecordInstance(ctx, args)
	})
}

// mergeMap returns a shallow copy of base with an extra key/value added (used to
// annotate a shared schema fragment without mutating it).
func mergeMap(base map[string]any, k string, v any) map[string]any {
	out := make(map[string]any, len(base)+1)
	for kk, vv := range base {
		out[kk] = vv
	}
	out[k] = v
	return out
}
