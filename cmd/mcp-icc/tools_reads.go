package main

import (
	"context"
	"encoding/json"
	"fmt"

	"gitlab.flexinfer.ai/libs/mcp-go"

	"github.com/crb2nu/loom/internal/iccclient"
	"github.com/crb2nu/loom/pkg/validate"
)

// All ICC GET endpoints return a bare JSON payload (no
// {"ok":..., "result":...} envelope) — see app.py do_GET dispatchers
// for the precedent. So every read handler uses getRaw with
// json.RawMessage as the result type. That lets the MCP server pass
// the backend's payload straight through to callers without dropping
// fields, which is the right default for a broad workbench tool: the
// SPA-shaped JSON is already the canonical surface.
//
// Schemas keep optional filter args as strings rather than enums.
// The backend validates and returns a structured 400 if a filter
// is bogus; mirroring that allowlist client-side would add drift
// risk for low value.

// --- common ----------------------------------------------------------------

// queryFromArgs builds a map[string]string from the listed args keys.
// Empty values are skipped (iccclient.Get drops them anyway, but
// keeping the map small is cheaper than building over and over).
// Bool values become "true"/"false" so backend `?include_done=false`
// style filters work.
func queryFromArgs(args map[string]any, keys ...string) map[string]string {
	q := map[string]string{}
	for _, k := range keys {
		switch v := args[k].(type) {
		case string:
			if v != "" {
				q[k] = v
			}
		case bool:
			if v {
				q[k] = "true"
			} else {
				q[k] = "false"
			}
		case float64:
			q[k] = fmt.Sprintf("%v", v)
		}
	}
	return q
}

// --- /api/projects/overview → icc_project_list ----------------------------

func projectListSchema() mcp.InputSchema {
	return mcp.InputSchema{
		Type:       "object",
		Properties: map[string]any{},
	}
}

func makeProjectListHandler(icc *iccclient.Client) iccToolHandler {
	return func(ctx context.Context, _ map[string]any) (*mcp.CallToolResult, error) {
		ctx = iccclient.WithTool(ctx, "icc_project_list")
		_, raw, err := getRaw[json.RawMessage](ctx, icc, "/api/projects/overview", nil)
		if err != nil {
			return mcp.ErrorResult(fmt.Errorf("project_list: %w", err)), nil
		}
		return jsonResult(raw)
	}
}

// --- /api/project-brief?project_id=... → icc_project_brief --------------

func projectBriefSchema() mcp.InputSchema {
	return mcp.InputSchema{
		Type:     "object",
		Required: []string{"project_id"},
		Properties: map[string]any{
			"project_id": map[string]any{"type": "string"},
		},
	}
}

func makeProjectBriefHandler(icc *iccclient.Client) iccToolHandler {
	return func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		v := validate.NewArgs(args)
		projectID := v.Required("project_id")
		if err := v.Validate(); err != nil {
			return mcp.ErrorResult(err), nil
		}
		ctx = iccclient.WithTool(ctx, "icc_project_brief")
		_, raw, err := getRaw[json.RawMessage](ctx, icc,
			"/api/project-brief", map[string]string{"project_id": projectID})
		if err != nil {
			return mcp.ErrorResult(fmt.Errorf("project_brief: %w", err)), nil
		}
		return jsonResult(raw)
	}
}

// --- /api/projects/<id>/kanban etc → per-project read tools -------------

// projectScopedSchema is the shared schema for tools that take a
// required project_id plus optional pass-through query args.
func projectScopedSchema(extra map[string]any) mcp.InputSchema {
	props := map[string]any{
		"project_id": map[string]any{"type": "string"},
	}
	for k, v := range extra {
		props[k] = v
	}
	return mcp.InputSchema{
		Type:       "object",
		Required:   []string{"project_id"},
		Properties: props,
	}
}

// makeProjectScopedReader returns a handler for /api/projects/<id>/<suffix>
// style routes (kanban, calendar, gantt, status, changes, blocked).
// extraQueryKeys lists additional query params that should be
// forwarded if the caller supplies them.
func makeProjectScopedReader(icc *iccclient.Client, label, suffix string, extraQueryKeys ...string) iccToolHandler {
	return func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		v := validate.NewArgs(args)
		projectID := v.Required("project_id")
		if err := v.Validate(); err != nil {
			return mcp.ErrorResult(err), nil
		}
		ctx = iccclient.WithTool(ctx, "icc_"+label)
		path := fmt.Sprintf("/api/projects/%s/%s", projectID, suffix)
		_, raw, err := getRaw[json.RawMessage](ctx, icc, path,
			queryFromArgs(args, extraQueryKeys...))
		if err != nil {
			return mcp.ErrorResult(fmt.Errorf("%s: %w", label, err)), nil
		}
		return jsonResult(raw)
	}
}

// --- list endpoints -------------------------------------------------------

// listToolSchema builds an object schema with the listed string
// query args (all optional). Used for the GET /api/<thing> tools
// where project_id / vendor_id / status / owner / etc. are filters.
func listToolSchema(filters ...string) mcp.InputSchema {
	props := map[string]any{}
	for _, f := range filters {
		props[f] = map[string]any{"type": "string"}
	}
	return mcp.InputSchema{
		Type:       "object",
		Properties: props,
	}
}

// makeListReader returns a handler for GET /api/<thing>?<filters>
// where the response is a bare {key: [...]} payload.
func makeListReader(icc *iccclient.Client, label, path string, queryKeys ...string) iccToolHandler {
	return func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		ctx = iccclient.WithTool(ctx, "icc_"+label)
		_, raw, err := getRaw[json.RawMessage](ctx, icc, path,
			queryFromArgs(args, queryKeys...))
		if err != nil {
			return mcp.ErrorResult(fmt.Errorf("%s: %w", label, err)), nil
		}
		return jsonResult(raw)
	}
}

// --- single-entity GET by id ---------------------------------------------

func makeGetByIDReader(icc *iccclient.Client, label, basePath, idArg string) iccToolHandler {
	return func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		v := validate.NewArgs(args)
		id := v.Required(idArg)
		if err := v.Validate(); err != nil {
			return mcp.ErrorResult(err), nil
		}
		ctx = iccclient.WithTool(ctx, "icc_"+label)
		path := basePath + "/" + id
		_, raw, err := getRaw[json.RawMessage](ctx, icc, path, nil)
		if err != nil {
			return mcp.ErrorResult(fmt.Errorf("%s: %w", label, err)), nil
		}
		return jsonResult(raw)
	}
}

// --- /api/search ----------------------------------------------------------

func searchSchema() mcp.InputSchema {
	return mcp.InputSchema{
		Type:     "object",
		Required: []string{"q"},
		Properties: map[string]any{
			"q":              map[string]any{"type": "string", "description": "Search query string"},
			"kind":           map[string]any{"type": "string", "description": "Currently only 'artifact'"},
			"mode":           map[string]any{"type": "string", "description": "lexical | hybrid"},
			"project_id":     map[string]any{"type": "string"},
			"vendor_id":      map[string]any{"type": "string"},
			"classification": map[string]any{"type": "string"},
			"limit":          map[string]any{"type": "string", "description": "Max results (string-encoded int)"},
			"include_phi":    map[string]any{"type": "string", "description": "Pass '1' with a reason to include PHI rows"},
			"reason":         map[string]any{"type": "string", "description": "Audit reason for PHI override"},
		},
	}
}

func makeSearchHandler(icc *iccclient.Client) iccToolHandler {
	return func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		v := validate.NewArgs(args)
		_ = v.Required("q")
		if err := v.Validate(); err != nil {
			return mcp.ErrorResult(err), nil
		}
		ctx = iccclient.WithTool(ctx, "icc_search")
		_, raw, err := getRaw[json.RawMessage](ctx, icc, "/api/search",
			queryFromArgs(args, "q", "kind", "mode", "project_id", "vendor_id",
				"classification", "limit", "include_phi", "reason"))
		if err != nil {
			return mcp.ErrorResult(fmt.Errorf("search: %w", err)), nil
		}
		return jsonResult(raw)
	}
}

// --- /api/needs-attention → icc_needs_attention (bonus) -----------------

func makeNeedsAttentionHandler(icc *iccclient.Client) iccToolHandler {
	return func(ctx context.Context, _ map[string]any) (*mcp.CallToolResult, error) {
		ctx = iccclient.WithTool(ctx, "icc_needs_attention")
		_, raw, err := getRaw[json.RawMessage](ctx, icc, "/api/needs-attention", nil)
		if err != nil {
			return mcp.ErrorResult(fmt.Errorf("needs_attention: %w", err)), nil
		}
		return jsonResult(raw)
	}
}

// --- /api/artifacts/<id>/links → icc_artifact_links_list ---------------

func artifactLinksSchema() mcp.InputSchema {
	return mcp.InputSchema{
		Type:     "object",
		Required: []string{"artifact_id"},
		Properties: map[string]any{
			"artifact_id": map[string]any{"type": "string"},
		},
	}
}

func makeArtifactLinksHandler(icc *iccclient.Client) iccToolHandler {
	return func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		v := validate.NewArgs(args)
		artifactID := v.Required("artifact_id")
		if err := v.Validate(); err != nil {
			return mcp.ErrorResult(err), nil
		}
		ctx = iccclient.WithTool(ctx, "icc_artifact_links_list")
		path := fmt.Sprintf("/api/artifacts/%s/links", artifactID)
		_, raw, err := getRaw[json.RawMessage](ctx, icc, path, nil)
		if err != nil {
			return mcp.ErrorResult(fmt.Errorf("artifact_links: %w", err)), nil
		}
		return jsonResult(raw)
	}
}
