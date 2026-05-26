package main

import (
	"context"
	"encoding/json"
	"fmt"

	"gitlab.flexinfer.ai/libs/mcp-go"

	"github.com/crb2nu/loom/internal/iccclient"
	"github.com/crb2nu/loom/pkg/validate"
)

// icc_quick_capture is the M53.5 convenience tool that pairs with
// Slice 5's web form fields (code_path + session_id on the artifact
// create flow). It posts to /api/artifacts with the form-shape body
// the backend already accepts, sparing MCP callers from a two-step
// (create_artifact + add_link) dance for the common code-snapshot
// workflow.

func quickCaptureSchema() mcp.InputSchema {
	return mcp.InputSchema{
		Type:     "object",
		Required: []string{"project_id", "title"},
		Properties: map[string]any{
			"project_id": map[string]any{
				"type":        "string",
				"description": "ICC project_id (prj_...)",
			},
			"title": map[string]any{
				"type":        "string",
				"description": "Artifact title (one-line summary)",
			},
			"summary": map[string]any{
				"type":        "string",
				"description": "Optional long-form summary",
			},
			"code_path": map[string]any{
				"type":        "string",
				"description": "Workspace-relative path that originated this capture (Slice 5 field)",
			},
			"session_id": map[string]any{
				"type":        "string",
				"description": "Agent session id that produced the capture (Slice 5 field)",
			},
			"classification": map[string]any{
				"type":        "string",
				"description": "Classification floor (default: possible_phi)",
			},
			"kind": map[string]any{
				"type":        "string",
				"description": "Artifact kind tag (default: note)",
			},
			"payload": map[string]any{
				"type":        "object",
				"description": "Optional extra fields to merge into the create-artifact payload",
			},
		},
	}
}

func makeQuickCaptureHandler(icc *iccclient.Client) iccToolHandler {
	return func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		v := validate.NewArgs(args)
		projectID := v.Required("project_id")
		title := v.Required("title")
		summary := v.String("summary", "")
		codePath := v.String("code_path", "")
		sessionID := v.String("session_id", "")
		classification := v.String("classification", "possible_phi")
		kind := v.String("kind", "note")
		if err := v.Validate(); err != nil {
			return mcp.ErrorResult(err), nil
		}

		// Start from any caller-supplied payload so they can override
		// defaults (kind, classification) without having to thread
		// every backend field through this tool's schema.
		body, err := payloadFromArgs(args)
		if err != nil {
			return mcp.ErrorResult(err), nil
		}

		// Apply explicit-arg wins-over-payload semantics for the
		// fields the schema enumerates: it's clearer to a caller that
		// `title: "foo"` lands as title=foo than to silently let
		// payload.title win.
		body["project_id"] = projectID
		body["title"] = title
		if summary != "" {
			body["summary"] = summary
		}
		if classification != "" {
			body["classification"] = classification
		}
		if kind != "" {
			body["kind"] = kind
		}
		if codePath != "" {
			body["code_path"] = codePath
		}
		if sessionID != "" {
			body["session_id"] = sessionID
		}

		ctx = iccclient.WithTool(ctx, "icc_quick_capture")
		_, result, err := postJSON[json.RawMessage](ctx, icc, "/api/artifacts", body)
		if err != nil {
			return mcp.ErrorResult(fmt.Errorf("quick_capture: %w", err)), nil
		}
		return jsonResult(result)
	}
}
