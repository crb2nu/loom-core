package clients

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/crb2nu/loom/pkg/mills/council"
)

// AuthorSlicedPlan authors a sliced Plan directly in a watched namespace (no
// backlog item) so the plan-slice emitter (S2) ships each slice as its own MR.
// This is the S2-lane half of the council decomposition path (.loom/163 S3):
// the mutator routes a decomposed proposal here instead of persisting a flat
// fan-out item. Idempotent via a deterministic id derived from the title, so a
// re-running council upserts the same Plan rather than duplicating it.
//
// *PlanClient satisfies council.SlicedPlanAuthor (the optional extension of
// council.PlanAuthor); the mutator type-asserts for it.
func (c *PlanClient) AuthorSlicedPlan(ctx context.Context, in council.SlicedPlanInput) (string, error) {
	if c == nil || c.Hub == nil {
		return "", errors.New("plan: client not configured")
	}
	if strings.TrimSpace(in.Title) == "" {
		return "", errors.New("plan: sliced plan title required")
	}
	if len(in.Slices) == 0 {
		return "", errors.New("plan: sliced plan needs >=1 slice")
	}
	slices := make([]map[string]any, 0, len(in.Slices))
	for _, s := range in.Slices {
		if strings.TrimSpace(s.Name) == "" {
			continue
		}
		sl := map[string]any{"name": s.Name}
		if g := strings.TrimSpace(s.Goal); g != "" {
			sl["goal"] = g
		}
		if len(s.Files) > 0 {
			sl["files"] = s.Files
		}
		slices = append(slices, sl)
	}
	if len(slices) == 0 {
		return "", errors.New("plan: sliced plan has no named slices")
	}
	args := map[string]any{
		"id":       planIDForSlicedTitle(in.Title),
		"title":    in.Title,
		"phase":    "planned",
		"slices":   slices,
		"agent_id": c.AgentID,
	}
	if p := strings.TrimSpace(in.Project); p != "" {
		args["project"] = p
	}
	if ns := strings.TrimSpace(in.Namespace); ns != "" {
		args["namespace"] = ns
	}
	body, err := c.Hub.CallTool(ctx, c.serverName(), "agent_plan_create", args)
	if err != nil && body == "" {
		return "", fmt.Errorf("plan: author sliced: %w", err)
	}
	parsed, perr := decodePlanCreateResponse(body)
	if perr != nil {
		return "", fmt.Errorf("plan: author sliced decode: %w; raw=%q", perr, truncateBody(body, 240))
	}
	if !parsed.OK && parsed.PlanID == "" {
		return "", fmt.Errorf("plan: author sliced reported failure: %q", truncateBody(body, 240))
	}
	return parsed.PlanID, nil
}

// planIDForSlicedTitle derives a deterministic, store-safe plan id from a
// proposal title so a re-running council upserts the same Plan.
func planIDForSlicedTitle(title string) string {
	slug := strings.ToLower(strings.TrimSpace(title))
	slug = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			return r
		default:
			return '-'
		}
	}, slug)
	for strings.Contains(slug, "--") {
		slug = strings.ReplaceAll(slug, "--", "-")
	}
	slug = strings.Trim(slug, "-")
	if slug == "" {
		slug = "untitled"
	}
	if len(slug) > 60 {
		slug = strings.Trim(slug[:60], "-")
	}
	return "plan-council-" + slug
}

// compile-time assertion that *PlanClient satisfies the optional sliced-plan
// author + plan-lister the council mutator type-asserts for.
var _ council.SlicedPlanAuthor = (*PlanClient)(nil)
var _ council.PlanLister = (*PlanClient)(nil)
