package agentcontext

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"gitlab.flexinfer.ai/libs/mcp-go"

	"github.com/crb2nu/loom/pkg/validate"
)

// MRStatusSvc backs the agent_mr_status MCP tool. It queries the HUD mrwatch
// registry (slice M1) over its REST endpoint and returns the classified
// branch→MR status. When the HUD is unreachable it returns a structured
// "unavailable" result rather than a tool error, so a probing agent gets a
// usable answer instead of a broken tool call.
type MRStatusSvc struct {
	*Service
	// client is swappable in tests; nil → a short-timeout default.
	client *http.Client
}

const mrStatusHTTPTimeout = 3 * time.Second

// HandleMRStatus resolves the classified status of open merge requests for a
// branch by calling GET /api/agent/mr-status on the HUD. Params:
//
//	branch (required) — the source branch to query.
//	repo   (optional) — narrow to a single GitLab project path.
//
// HUD base URL resolution reuses the same AGENT_CONTEXT_HUD_URL → LOOM_HUD_URL
// → http://127.0.0.1:3333 chain the service already uses for live nudges
// (cfg.HUDBaseURL).
func (s *MRStatusSvc) HandleMRStatus(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	v := validate.NewArgs(args)
	branch := v.Required("branch")
	repo := v.String("repo", "")
	if err := v.Validate(); err != nil {
		return mcp.ErrorResult(err), nil
	}

	base := s.cfg.HUDBaseURL
	if base == "" {
		return mcp.JSONResult(mrStatusUnavailable(branch, "HUD base URL is not configured"))
	}

	endpoint := base + "/api/agent/mr-status?branch=" + url.QueryEscape(branch)
	if repo != "" {
		endpoint += "&repo=" + url.QueryEscape(repo)
	}

	nctx, cancel := context.WithTimeout(ctx, mrStatusHTTPTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(nctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return mcp.JSONResult(mrStatusUnavailable(branch, err.Error()))
	}

	client := s.client
	if client == nil {
		client = &http.Client{Timeout: mrStatusHTTPTimeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		return mcp.JSONResult(mrStatusUnavailable(branch, err.Error()))
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return mcp.JSONResult(mrStatusUnavailable(branch, fmt.Sprintf("HUD returned status %d", resp.StatusCode)))
	}

	var status map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return mcp.JSONResult(mrStatusUnavailable(branch, "malformed HUD response: "+err.Error()))
	}

	// Pass the HUD status through verbatim, flagged available so a caller can
	// distinguish it from the unavailable shape without inspecting fields.
	status["available"] = true
	return mcp.JSONResult(status)
}

// mrStatusUnavailable is the structured result returned when the HUD registry
// cannot be reached. It is intentionally NOT an error: the tool call succeeds
// and the agent learns the status is simply unknown right now.
func mrStatusUnavailable(branch, reason string) map[string]any {
	return map[string]any{
		"available":      false,
		"branch":         branch,
		"reason":         reason,
		"merge_requests": []any{},
		"count":          0,
	}
}
