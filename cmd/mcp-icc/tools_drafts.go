package main

import (
	"context"
	"encoding/json"
	"fmt"

	"gitlab.flexinfer.ai/libs/mcp-go"

	"github.com/crb2nu/loom/internal/iccclient"
)

// Status-draft composer tool. Wraps POST /api/drafts/status — the
// SD-1/SD-2/SD-EA composer that backs `iccctl drafts` and the SPA's
// Status Draft modal. Semantically a read (composes a draft from
// existing entity rows; no mutations), but uses POST because the
// backend handler takes a JSON body. No write gate.

func draftStatusComposeSchema() mcp.InputSchema {
	return mcp.InputSchema{
		Type: "object",
		Properties: map[string]any{
			"project_id": map[string]any{
				"type":        "string",
				"description": "Filter the draft to one project (optional)",
			},
			"vendor_id": map[string]any{
				"type":        "string",
				"description": "Filter the draft to one vendor (optional)",
			},
			"since_ms": map[string]any{
				"type":        "number",
				"description": "Lower reviewed_at bound in epoch milliseconds (optional)",
			},
			"until_ms": map[string]any{
				"type":        "number",
				"description": "Upper reviewed_at bound in epoch milliseconds (optional)",
			},
		},
	}
}

func makeDraftStatusComposeHandler(icc *iccclient.Client) iccToolHandler {
	return func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		body := map[string]any{}
		if v, ok := args["project_id"].(string); ok && v != "" {
			body["project_id"] = v
		}
		if v, ok := args["vendor_id"].(string); ok && v != "" {
			body["vendor_id"] = v
		}
		if v, ok := args["since_ms"].(float64); ok {
			body["since_ms"] = int64(v)
		}
		if v, ok := args["until_ms"].(float64); ok {
			body["until_ms"] = int64(v)
		}
		ctx = iccclient.WithTool(ctx, "icc_draft_status_compose")
		_, result, err := postJSON[json.RawMessage](ctx, icc, "/api/drafts/status", body)
		if err != nil {
			return mcp.ErrorResult(fmt.Errorf("draft_status_compose: %w", err)), nil
		}
		return jsonResult(result)
	}
}
