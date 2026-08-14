package clients

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	mcp "gitlab.flexinfer.ai/libs/mcp-go"

	"github.com/crb2nu/loom/pkg/mills/pipeline"
)

// DevboxServerName is the registered name of mcp-devbox in the MCP hub.
// Matches the registry.yaml entry the operator's hub profile references.
const DevboxServerName = "devbox"

// DevboxClient implements pipeline.DevboxClient against the
// devbox_quality_gate MCP tool, called via MCPHubClient.
//
// Production deployments wire one shared MCPHubClient and pass it to
// every wrapper client constructor — the hub manages connection
// lifetime, the wrappers translate domain types.
type DevboxClient struct {
	Hub        *MCPHubClient
	ServerName string // overridable for tests / non-default hub registries
}

// NewDevboxClient returns a DevboxClient bound to hub. ServerName falls
// back to DevboxServerName.
func NewDevboxClient(hub *MCPHubClient) *DevboxClient {
	return &DevboxClient{Hub: hub, ServerName: DevboxServerName}
}

// devboxQualityGateResult mirrors mcp-devbox's qualityGateResult.
type devboxQualityGateResult struct {
	Language        string                  `json:"language"`
	Passed          bool                    `json:"passed"`
	Checks          []devboxQualityCheckRow `json:"checks"`
	TotalDurationMs int64                   `json:"total_duration_ms"`
}

type devboxQualityCheckRow struct {
	Name       string `json:"name"`
	Passed     bool   `json:"passed"`
	ExitCode   int    `json:"exit_code,omitempty"`
	DurationMs int64  `json:"duration_ms"`
	OutputTail string `json:"output_tail,omitempty"`
	StderrTail string `json:"stderr_tail,omitempty"`
}

// QualityGate implements pipeline.DevboxClient.
func (c *DevboxClient) QualityGate(ctx context.Context, req pipeline.DevboxRequest) (pipeline.DevboxResponse, error) {
	if c == nil || c.Hub == nil {
		return pipeline.DevboxResponse{}, errors.New("devbox: client not configured")
	}
	if req.Project == "" {
		return pipeline.DevboxResponse{}, errors.New("devbox: Project required")
	}
	args := map[string]any{"project": req.Project}
	if req.AgentID != "" {
		args["agent_id"] = req.AgentID
	}
	// Forward the explicit Checks selector when the caller scoped the
	// gate (canary path uses Checks=["fmt"] so `go vet` / `go test` aren't
	// run for a backlog item that only touched a non-Go fixture).
	if len(req.Checks) > 0 {
		checks := make([]any, 0, len(req.Checks))
		for _, c := range req.Checks {
			checks = append(checks, c)
		}
		args["checks"] = checks
	}
	if len(req.TestCommands) > 0 {
		commands := make([]any, 0, len(req.TestCommands))
		for _, command := range req.TestCommands {
			commands = append(commands, command)
		}
		args["extra_test_commands"] = commands
	}
	if len(req.Env) > 0 {
		env := make(map[string]any, len(req.Env))
		for key, value := range req.Env {
			env[key] = value
		}
		args["env"] = env
	}
	server := c.ServerName
	if server == "" {
		server = DevboxServerName
	}
	body, err := c.Hub.CallTool(ctx, server, "devbox_quality_gate", args)
	// devbox_quality_gate returns IsError=true with a structured body
	// when checks fail; we treat that as a real (non-passing) result
	// rather than a transport error so the runner can surface the
	// failure path normally.
	if err != nil && body == "" {
		return pipeline.DevboxResponse{}, fmt.Errorf("devbox quality_gate: %w", err)
	}
	parsed, perr := parseDevboxQualityGateResult(body)
	if perr != nil {
		if err != nil {
			return pipeline.DevboxResponse{}, fmt.Errorf("devbox quality_gate: %w; raw=%q", err, body)
		}
		return pipeline.DevboxResponse{}, fmt.Errorf("devbox: decode body: %w; raw=%q", perr, body)
	}
	checks := make([]pipeline.DevboxCheck, 0, len(parsed.Checks))
	for _, row := range parsed.Checks {
		// Fall back to stderr when the canonical stdout tail is empty
		// so the runner's stage_results.artifacts_json carries an
		// actionable failure message (the canary failure mode of
		// "make fmt" with stderr-only output otherwise vanishes here).
		output := row.OutputTail
		if output == "" {
			output = row.StderrTail
		}
		checks = append(checks, pipeline.DevboxCheck{
			Name:     row.Name,
			Passed:   row.Passed,
			ExitCode: row.ExitCode,
			Duration: float64(row.DurationMs) / 1000.0,
			Output:   output,
		})
	}
	return pipeline.DevboxResponse{
		Passed:   parsed.Passed,
		CostUSD:  0, // devbox_quality_gate runs locally; no LLM cost.
		LogTail:  buildDevboxLogTail(parsed),
		Checks:   checks,
		Language: parsed.Language,
	}, nil
}

// parseDevboxQualityGateResult decodes body as a devbox_quality_gate
// result, accepting either raw JSON or the hub's TOON re-encoding.
//
// The decode is STRICT: the document must carry a boolean "passed" field,
// which every genuine qualityGateResult serialization includes. Without
// the probe, a plain-text tool-error body (e.g. "ensure sandbox: sandbox
// image still building after 8m0s") slipped through the TOON fallback —
// any single line containing a colon decodes as a {key: value} object —
// and unmarshaled into a ZERO-VALUE result (passed=false, checks=[]).
// QualityGate then dropped the CallTool error and fabricated a not-passed
// verdict with zero executed checks, which the tests stage burned as a
// code-class failure (escalation #322: "0/0 checks marked failed; gate
// reported not passed" ×3 at ~8m/attempt, 2026-07-16 — the real cause, a
// cold sandbox image build exceeding the gate's build wait, was invisible).
func parseDevboxQualityGateResult(body string) (devboxQualityGateResult, error) {
	raw := []byte(body)
	if !json.Valid(raw) {
		jsonBody, err := mcp.DecodeTOONToJSON(body)
		if err != nil {
			return devboxQualityGateResult{}, err
		}
		raw = jsonBody
	}
	var probe struct {
		Passed *bool `json:"passed"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return devboxQualityGateResult{}, err
	}
	if probe.Passed == nil {
		return devboxQualityGateResult{}, errors.New(`not a quality-gate result (no "passed" field)`)
	}
	var parsed devboxQualityGateResult
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return devboxQualityGateResult{}, err
	}
	return parsed, nil
}

// buildDevboxLogTail collapses the per-check output into a single
// human-readable string for stage_results.log_tail. Useful when a gate
// fails and the operator needs to surface what broke without exposing
// the full schema to the HUD.
func buildDevboxLogTail(result devboxQualityGateResult) string {
	if len(result.Checks) == 0 {
		return ""
	}
	var b []byte
	for _, c := range result.Checks {
		status := "PASS"
		if !c.Passed {
			status = "FAIL"
		}
		b = append(b, []byte(fmt.Sprintf("%s %s (%dms)\n", status, c.Name, c.DurationMs))...)
		if !c.Passed && c.OutputTail != "" {
			b = append(b, []byte(c.OutputTail)...)
			if len(c.OutputTail) > 0 && c.OutputTail[len(c.OutputTail)-1] != '\n' {
				b = append(b, '\n')
			}
		}
	}
	return string(b)
}

// Compile-time assertion that DevboxClient satisfies the pipeline interface.
var _ pipeline.DevboxClient = (*DevboxClient)(nil)
