package clients

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/crb2nu/loom/pkg/mills/store"
)

// HubCaller is the narrow slice of *MCPHubClient the plan clients depend on:
// one tools/call round trip returning the tool's raw text body. Declaring it
// as an interface (rather than the concrete *MCPHubClient) lets cross-package
// tests inject a fake hub that returns production-shaped TOON without a live
// gateway — the seam the take-up reconciler's end-to-end decode test uses.
// *MCPHubClient satisfies it.
type HubCaller interface {
	CallTool(ctx context.Context, serverName, toolName string, args map[string]any) (string, error)
}

// PlanClient authors first-class Plans in the agent-context store from
// Mills BacklogItems, via agent_plan_create on mcp-agent-context (the
// same server backing HandoffClient + WorktreeAllocator). It is the
// write half of plan-store ↔ Mills convergence (plan store S7b): S7
// added the BacklogItem.PlanID link + read-through prompt; this
// populates it so a spawned agent resolves a live plan instead of a
// stale .loom SpecDoc.
type PlanClient struct {
	Hub        HubCaller
	ServerName string
	// AgentID is recorded as the plan's creator (attribution only; the
	// store never scopes reads by agent_id).
	AgentID string
}

// NewPlanClient returns a PlanClient bound to hub.
func NewPlanClient(hub *MCPHubClient, agentID string) *PlanClient {
	if strings.TrimSpace(agentID) == "" {
		agentID = "mills:plan-authoring"
	}
	pc := &PlanClient{
		ServerName: AgentContextServerName,
		AgentID:    agentID,
	}
	// Only assign a live hub: storing a typed-nil *MCPHubClient into the
	// interface field would read as non-nil and defeat the `c.Hub == nil`
	// guards in the read/write paths.
	if hub != nil {
		pc.Hub = hub
	}
	return pc
}

// planCreateResponse mirrors the payload agent_plan_create emits. We
// accept both JSON and YAML serialisations: the YAML form ships from
// MCP servers running in "concise text output" mode (same as the
// handoff decoder).
type planCreateResponse struct {
	OK     bool   `json:"ok" yaml:"ok"`
	PlanID string `json:"plan_id" yaml:"plan_id"`
}

// AuthorPlan creates (idempotently upserts) a Plan for item and returns
// its plan_id. The deterministic id (plan-mills-<backlog-id>) makes a
// re-run an upsert against the same store point rather than a duplicate.
// project scopes list queries; pass the canonical GitLab path when known.
func (c *PlanClient) AuthorPlan(ctx context.Context, item *store.BacklogItem, project string) (string, error) {
	if c == nil || c.Hub == nil {
		return "", errors.New("plan: client not configured")
	}
	if item == nil {
		return "", errors.New("plan: nil item")
	}
	if strings.TrimSpace(item.Title) == "" {
		return "", errors.New("plan: item title required")
	}
	server := c.ServerName
	if server == "" {
		server = AgentContextServerName
	}
	args := backlogItemToPlanArgs(item, project, c.AgentID)
	body, err := c.Hub.CallTool(ctx, server, "agent_plan_create", args)
	if err != nil && body == "" {
		return "", fmt.Errorf("plan: %w", err)
	}
	parsed, perr := decodePlanCreateResponse(body)
	if perr != nil {
		return "", fmt.Errorf("plan: decode: %w; raw=%q", perr, body)
	}
	if !parsed.OK && parsed.PlanID == "" {
		return "", fmt.Errorf("plan: service reported failure: %q", body)
	}
	return parsed.PlanID, nil
}

// backlogItemToPlanArgs maps a BacklogItem into agent_plan_create args.
// Pure (no I/O) so it is directly unit-tested. Only fields the tool
// honors are emitted; the round-trip linkage (mills_backlog_id,
// gitlab_issue_iid) lets the Plan and the backlog converge.
func backlogItemToPlanArgs(item *store.BacklogItem, project, agentID string) map[string]any {
	args := map[string]any{
		"title":            item.Title,
		"id":               PlanIDForBacklog(item.ID),
		"phase":            planPhaseForBacklogState(item.State),
		"spec_doc":         item.SpecDoc,
		"mills_backlog_id": item.ID,
		"agent_id":         agentID,
	}
	if strings.TrimSpace(project) != "" {
		args["project"] = project
	}
	if item.GitLabIssueIID != nil {
		args["gitlab_issue_iid"] = *item.GitLabIssueIID
	}
	if len(item.Dependencies) > 0 {
		args["dependencies"] = item.Dependencies
	}
	if len(item.Slices) > 0 {
		slices := make([]map[string]any, 0, len(item.Slices))
		for _, s := range item.Slices {
			slices = append(slices, map[string]any{
				"name":  s.Name,
				"files": s.Files,
			})
		}
		args["slices"] = slices
	}
	return args
}

// PlanIDForBacklog derives a deterministic, store-safe plan id from a
// backlog item id so backfill re-runs upsert the same Plan rather than
// minting duplicates.
func PlanIDForBacklog(backlogID string) string {
	slug := strings.ToLower(strings.TrimSpace(backlogID))
	slug = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			return r
		default:
			return '-'
		}
	}, slug)
	// Collapse runs of '-' and trim the ends so the id stays tidy.
	for strings.Contains(slug, "--") {
		slug = strings.ReplaceAll(slug, "--", "-")
	}
	slug = strings.Trim(slug, "-")
	if slug == "" {
		slug = "item"
	}
	return "plan-mills-" + slug
}

// planPhaseForBacklogState maps a backlog lifecycle state onto a valid
// Plan phase (draft/planned/in_progress/in_review/merging/merged/
// deployed/done/abandoned).
func planPhaseForBacklogState(s store.BacklogState) string {
	switch s {
	case store.BacklogRunning, store.BacklogEscalated:
		return "in_progress"
	case store.BacklogMerged:
		return "merged"
	default: // queued, paused, unknown
		return "planned"
	}
}

// decodePlanCreateResponse parses the body returned by agent_plan_create.
// MCP servers may emit JSON or YAML (concise-text-output mode), so we try
// JSON first and fall back to YAML, matching decodeHandoffCreateResponse.
func decodePlanCreateResponse(body string) (planCreateResponse, error) {
	trimmed := strings.TrimSpace(body)
	var parsed planCreateResponse
	if trimmed == "" {
		return parsed, errors.New("empty body")
	}
	if c := trimmed[0]; c == '{' || c == '[' {
		if err := json.Unmarshal([]byte(body), &parsed); err == nil {
			return parsed, nil
		}
	}
	if err := yaml.Unmarshal([]byte(body), &parsed); err != nil {
		return planCreateResponse{}, err
	}
	return parsed, nil
}
