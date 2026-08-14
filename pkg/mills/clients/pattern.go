package clients

import (
	"context"
	"fmt"
	"strings"

	"github.com/crb2nu/loom/pkg/mills/council"
)

// PatternClient reads the approved-pattern catalog from the agent-context
// store via agent_pattern_list (Pattern Loom A1), so the council editor can
// constrain its proposals to a vetted archetype. Read-only and cross-agent
// (the store never scopes pattern reads by agent_id). Reuses the shared MCP
// hub client the plan/handoff/worktree clients already share.
type PatternClient struct {
	Hub        *MCPHubClient
	ServerName string
}

// NewPatternClient returns a PatternClient bound to hub.
func NewPatternClient(hub *MCPHubClient) *PatternClient {
	return &PatternClient{
		Hub:        hub,
		ServerName: AgentContextServerName,
	}
}

// patternSummary is the projection of one Pattern returned by
// agent_pattern_list. The list view carries the full entity; only the
// fields the editor prompt cites — plus the slug the take-up harvester
// matches stamped plan ids against — are decoded here.
type patternSummary struct {
	ID    string `json:"id"`
	Makes string `json:"makes"`
	Name  string `json:"name"`
	Slug  string `json:"slug"`
}

type patternListEnvelope struct {
	OK       bool             `json:"ok"`
	Patterns []patternSummary `json:"patterns"`
}

// ListApprovedPatterns returns the approved-pattern catalog via
// agent_pattern_list with status="approved". It satisfies
// council.PatternLister and honors that interface's best-effort contract:
// on ANY error (client unconfigured, transport, decode) it returns an empty
// slice + nil so the council editor never blocks decomposition on a pattern
// fetch — a missing catalog simply drops the prompt's catalog section.
func (c *PatternClient) ListApprovedPatterns(ctx context.Context) ([]council.PatternRef, error) {
	if c == nil || c.Hub == nil {
		return nil, nil
	}
	body, err := c.Hub.CallTool(ctx, c.serverName(), "agent_pattern_list", map[string]any{
		"status": "approved",
	})
	if err != nil && body == "" {
		return nil, fmt.Errorf("pattern: list: %w", err)
	}
	var env patternListEnvelope
	if derr := decodeListBody(body, &env); derr != nil {
		if err != nil {
			return nil, fmt.Errorf("pattern: list: %w; raw=%q", err, truncateBody(body, 240))
		}
		return nil, fmt.Errorf("pattern: list decode: %w; raw=%q", derr, truncateBody(body, 240))
	}
	if !env.OK {
		return nil, fmt.Errorf("pattern: list rejected: %s", truncateBody(body, 240))
	}
	out := make([]council.PatternRef, 0, len(env.Patterns))
	for _, p := range env.Patterns {
		id := strings.TrimSpace(p.ID)
		if id == "" {
			continue
		}
		out = append(out, council.PatternRef{
			ID:    id,
			Makes: strings.TrimSpace(p.Makes),
			Name:  strings.TrimSpace(p.Name),
			Slug:  strings.TrimSpace(p.Slug),
		})
	}
	return out, nil
}

// PatternHarvest is the taste-gate outcome of recording one green-shipped
// instance (agent_pattern_record_instance): the pattern's updated green count,
// status, and whether this instance auto-promoted it candidate→approved.
// Engram verification results are deliberately not projected — without a
// checkout reachable by the agent-context pod they report unverified (safe),
// and the harvester only logs the taste-gate outcome.
type PatternHarvest struct {
	InstancesShippedGreen int    `json:"instances_shipped_green"`
	Status                string `json:"status"`
	Promoted              bool   `json:"promoted"`
}

type patternRecordEnvelope struct {
	OK bool `json:"ok"`
	PatternHarvest
}

// RecordInstance records a merged pattern-stamped instance against the taste
// gate (Pattern Loom B2/J2 auto-harvest): increments instances_shipped_green,
// which auto-promotes a candidate at the policy threshold. mrRef lands in the
// pattern's provenance notes; repo feeds `unlocked_in` bookkeeping. No
// repo_root is passed, so composed-engram file_ref proofs stay unverified
// until the agent-context pod has a merged-instance checkout (A2 tail).
func (c *PatternClient) RecordInstance(ctx context.Context, patternID, mrRef, repo string) (PatternHarvest, error) {
	return c.RecordGradedInstance(ctx, patternID, mrRef, repo, "")
}

// RecordGradedInstance records an instance and includes its optional Mills
// taste grade in the cross-store request.
func (c *PatternClient) RecordGradedInstance(ctx context.Context, patternID, mrRef, repo, grade string) (PatternHarvest, error) {
	if c == nil || c.Hub == nil {
		return PatternHarvest{}, fmt.Errorf("pattern: record instance: client not configured")
	}
	args := map[string]any{"pattern_id": patternID}
	if strings.TrimSpace(mrRef) != "" {
		args["mr_ref"] = mrRef
	}
	if strings.TrimSpace(repo) != "" {
		args["repo"] = repo
	}
	if strings.TrimSpace(grade) != "" {
		args["grade"] = strings.TrimSpace(grade)
	}
	body, err := c.Hub.CallTool(ctx, c.serverName(), "agent_pattern_record_instance", args)
	if err != nil && body == "" {
		return PatternHarvest{}, fmt.Errorf("pattern: record instance: %w", err)
	}
	var env patternRecordEnvelope
	if derr := decodeListBody(body, &env); derr != nil {
		if err != nil {
			return PatternHarvest{}, fmt.Errorf("pattern: record instance: %w; raw=%q", err, truncateBody(body, 240))
		}
		return PatternHarvest{}, fmt.Errorf("pattern: record instance decode: %w; raw=%q", derr, truncateBody(body, 240))
	}
	if !env.OK {
		return PatternHarvest{}, fmt.Errorf("pattern: record instance rejected: %s", truncateBody(body, 240))
	}
	return env.PatternHarvest, nil
}

func (c *PatternClient) serverName() string {
	if c.ServerName != "" {
		return c.ServerName
	}
	return AgentContextServerName
}

var _ council.PatternLister = (*PatternClient)(nil)
