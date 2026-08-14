package clients

// plan_project.go -- the plan→repo bootstrap flow's Plan Store surface.
// Bootstrap needs two things the other plan files don't expose: a plan's
// full detail (agent_plan_get — project/phase guard the mint; title/spec_doc
// seed the new repo's README) and a project/namespace re-scope
// (agent_plan_update) so the plan-slice emitter can source the plan once the
// repo exists.

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// PlanDetail is the projection of agent_plan_get the bootstrap flow needs.
type PlanDetail struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Project   string `json:"project"`
	Namespace string `json:"namespace"`
	Phase     string `json:"phase"`
	SpecDoc   string `json:"spec_doc"`
}

type planGetEnvelope struct {
	OK   bool       `json:"ok"`
	Plan PlanDetail `json:"plan"`
}

// GetPlan fetches one plan's detail via agent_plan_get. Read-only and
// cross-agent, like the list reads.
func (c *PlanClient) GetPlan(ctx context.Context, planID string) (PlanDetail, error) {
	if c == nil || c.Hub == nil {
		return PlanDetail{}, errors.New("plan: client not configured")
	}
	if strings.TrimSpace(planID) == "" {
		return PlanDetail{}, errors.New("plan: plan_id required")
	}
	body, err := c.Hub.CallTool(ctx, c.serverName(), "agent_plan_get", map[string]any{"plan_id": planID})
	if err != nil && body == "" {
		return PlanDetail{}, fmt.Errorf("plan: get: %w", err)
	}
	var env planGetEnvelope
	if derr := decodeListBody(body, &env); derr != nil {
		if err != nil {
			return PlanDetail{}, fmt.Errorf("plan: get: %w; raw=%q", err, truncateBody(body, 240))
		}
		return PlanDetail{}, fmt.Errorf("plan: get decode: %w; raw=%q", derr, truncateBody(body, 240))
	}
	if !env.OK || env.Plan.ID == "" {
		return PlanDetail{}, fmt.Errorf("plan: get %s: not found: %s", planID, truncateBody(body, 240))
	}
	return env.Plan, nil
}

// RescopePlan sets a plan's canonical project id and namespace via
// agent_plan_update. This is the junction that makes a bootstrapped plan
// visible to the plan-slice emitter: project must equal the demand project
// path exactly, and namespace must match the emitter's namespace gate
// (which applies to foreign projects too).
func (c *PlanClient) RescopePlan(ctx context.Context, planID, project, namespace string) error {
	if c == nil || c.Hub == nil {
		return errors.New("plan: client not configured")
	}
	if strings.TrimSpace(planID) == "" {
		return errors.New("plan: plan_id required")
	}
	if strings.TrimSpace(project) == "" {
		return errors.New("plan: project required")
	}
	args := map[string]any{
		"plan_id": planID,
		"project": project,
	}
	if s := strings.TrimSpace(namespace); s != "" {
		args["namespace"] = s
	}
	return c.planWrite(ctx, "agent_plan_update", args)
}
