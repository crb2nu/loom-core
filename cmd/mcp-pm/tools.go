package main

import (
	"context"

	"gitlab.flexinfer.ai/libs/mcp-go"

	"github.com/crb2nu/loom/pkg/pm"
	"github.com/crb2nu/loom/pkg/validate"
)

func registerTools(server *mcp.Server, svc *pm.Service) {
	// =========================================================================
	// pm_risk_create
	// =========================================================================
	server.AddTool(mcp.Tool{
		Name:        "pm_risk_create",
		Description: "Create a project risk. project is the canonical GitLab path_with_namespace (e.g. \"services/flexdeck\"). Returns the new risk id.",
		InputSchema: mcp.InputSchema{
			Type: "object",
			Properties: map[string]any{
				"project":    map[string]any{"type": "string", "description": "Canonical project key (GitLab path_with_namespace)."},
				"title":      map[string]any{"type": "string", "description": "Short risk title."},
				"likelihood": map[string]any{"type": "string", "enum": []string{"low", "medium", "high"}, "description": "Likelihood (default: medium)."},
				"impact":     map[string]any{"type": "string", "enum": []string{"low", "medium", "high"}, "description": "Impact (default: medium)."},
				"mitigation": map[string]any{"type": "string", "description": "Mitigation plan."},
				"owner":      map[string]any{"type": "string", "description": "Risk owner."},
				"status":     map[string]any{"type": "string", "enum": []string{"identified", "mitigating", "accepted", "closed"}, "description": "Status (default: identified)."},
			},
			Required: []string{"project", "title"},
		},
	}, func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		v := validate.NewArgs(args)
		in := pm.CreateRiskInput{
			Project:    v.Required("project"),
			Title:      v.Required("title"),
			Likelihood: v.String("likelihood", ""),
			Impact:     v.String("impact", ""),
			Mitigation: v.String("mitigation", ""),
			Owner:      v.String("owner", ""),
			Status:     v.String("status", ""),
		}
		if err := v.Validate(); err != nil {
			return mcp.ErrorResult(err), nil
		}
		id, err := svc.CreateRisk(ctx, in)
		if err != nil {
			return mcp.ErrorResult(err), nil
		}
		return mcp.JSONResult(map[string]any{"id": id})
	})

	// =========================================================================
	// pm_risk_list
	// =========================================================================
	server.AddTool(mcp.Tool{
		Name:        "pm_risk_list",
		Description: "List risks, optionally filtered by project and/or status.",
		InputSchema: mcp.InputSchema{
			Type: "object",
			Properties: map[string]any{
				"project": map[string]any{"type": "string", "description": "Filter by canonical project key."},
				"status":  map[string]any{"type": "string", "enum": []string{"identified", "mitigating", "accepted", "closed"}, "description": "Filter by status."},
			},
		},
	}, func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		v := validate.NewArgs(args)
		project := v.String("project", "")
		status := v.String("status", "")
		risks, err := svc.ListRisks(ctx, project, status)
		if err != nil {
			return mcp.ErrorResult(err), nil
		}
		if risks == nil {
			risks = []pm.Risk{}
		}
		return mcp.JSONResult(map[string]any{"risks": risks})
	})

	// =========================================================================
	// pm_risk_update
	// =========================================================================
	server.AddTool(mcp.Tool{
		Name:        "pm_risk_update",
		Description: "Update mutable fields of an existing risk by id. Only provided fields change.",
		InputSchema: mcp.InputSchema{
			Type: "object",
			Properties: map[string]any{
				"id":         map[string]any{"type": "string", "description": "Risk id to update."},
				"title":      map[string]any{"type": "string", "description": "New title."},
				"likelihood": map[string]any{"type": "string", "enum": []string{"low", "medium", "high"}, "description": "New likelihood."},
				"impact":     map[string]any{"type": "string", "enum": []string{"low", "medium", "high"}, "description": "New impact."},
				"mitigation": map[string]any{"type": "string", "description": "New mitigation."},
				"owner":      map[string]any{"type": "string", "description": "New owner."},
				"status":     map[string]any{"type": "string", "enum": []string{"identified", "mitigating", "accepted", "closed"}, "description": "New status."},
			},
			Required: []string{"id"},
		},
	}, func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		v := validate.NewArgs(args)
		id := v.Required("id")
		if err := v.Validate(); err != nil {
			return mcp.ErrorResult(err), nil
		}
		in := pm.UpdateRiskInput{
			Title:      optString(args, "title"),
			Likelihood: optString(args, "likelihood"),
			Impact:     optString(args, "impact"),
			Mitigation: optString(args, "mitigation"),
			Owner:      optString(args, "owner"),
			Status:     optString(args, "status"),
		}
		if err := svc.UpdateRisk(ctx, id, in); err != nil {
			return mcp.ErrorResult(err), nil
		}
		return mcp.JSONResult(map[string]any{"ok": true})
	})

	// =========================================================================
	// pm_risk_link
	// =========================================================================
	server.AddTool(mcp.Tool{
		Name:        "pm_risk_link",
		Description: "Append a reference (gitlab issue path or task id) to a risk's links.",
		InputSchema: mcp.InputSchema{
			Type: "object",
			Properties: map[string]any{
				"id":  map[string]any{"type": "string", "description": "Risk id."},
				"ref": map[string]any{"type": "string", "description": "Reference to append (gitlab issue path or task id)."},
			},
			Required: []string{"id", "ref"},
		},
	}, func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		v := validate.NewArgs(args)
		id := v.Required("id")
		ref := v.Required("ref")
		if err := v.Validate(); err != nil {
			return mcp.ErrorResult(err), nil
		}
		if err := svc.LinkRisk(ctx, id, ref); err != nil {
			return mcp.ErrorResult(err), nil
		}
		return mcp.JSONResult(map[string]any{"ok": true})
	})
}

// optString returns a pointer to the string value of args[key] when present
// (so callers can distinguish "absent" from "empty"), else nil.
func optString(args map[string]any, key string) *string {
	v, ok := args[key]
	if !ok {
		return nil
	}
	s, ok := v.(string)
	if !ok {
		return nil
	}
	return &s
}
