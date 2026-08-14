package clients

// plan_takeup.go -- the take-up reconciler's write surface on the Plan Store.
// The take-up motion (Live Beam slice 2) trues plan/slice lifecycle state to
// GitLab MR reality: slices whose MR merged advance to "merged", plans whose
// slices are all merged advance toward "merged", and orphaned slices (MR
// closed without merging) get a decision note. These wrap the same
// agent_plan_* MCP tools agents use, so every write lands in phase_history /
// decisions with "mills:take-up" attribution — auditable, not silent.

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// takeupActor is the attribution recorded on take-up writes.
const takeupActor = "mills:take-up"

// okEnvelope decodes the {"ok": bool} shape all agent_plan_* writes return.
type okEnvelope struct {
	OK bool `json:"ok"`
}

// UpdateSlicePhase sets a slice's lifecycle phase via agent_plan_slice_update.
// The store validates phase names but deliberately not transitions, so the
// reconciler can true an "implementing" slice straight to "merged" when its
// MR merged externally.
func (c *PlanClient) UpdateSlicePhase(ctx context.Context, sliceID, phase string) error {
	if c == nil || c.Hub == nil {
		return errors.New("plan: client not configured")
	}
	if strings.TrimSpace(sliceID) == "" {
		return errors.New("plan: slice_id required")
	}
	return c.planWrite(ctx, "agent_plan_slice_update", map[string]any{
		"slice_id": sliceID,
		"phase":    phase,
	})
}

// UpdateSliceMRRef records a slice's merge request reference via
// agent_plan_slice_update. mr_ref is a REPLACE column on the store side (not
// append-only like decisions/commit refs), so a retried MR overwrites a stale
// ref rather than accumulating — which is what the take-up reconciler wants:
// it trues the slice against whichever MR is live now.
//
// This is the write the mr stage owes the plan: before it existed, a
// plan-linked item's slice kept mr_ref empty unless the spawned agent
// remembered to call agent_plan_slice_update itself, so take-up had nothing
// to poll and the plan never walked to merged (observed 2026-08-01 on
// plan-stamp-loom-runbook-loom-runbook slice #1, MR !1380).
func (c *PlanClient) UpdateSliceMRRef(ctx context.Context, sliceID, mrRef string) error {
	if c == nil || c.Hub == nil {
		return errors.New("plan: client not configured")
	}
	if strings.TrimSpace(sliceID) == "" {
		return errors.New("plan: slice_id required")
	}
	if strings.TrimSpace(mrRef) == "" {
		return errors.New("plan: mr_ref required")
	}
	return c.planWrite(ctx, "agent_plan_slice_update", map[string]any{
		"slice_id": sliceID,
		"mr_ref":   mrRef,
	})
}

// AppendSliceDecision appends a decision/blocker note to a slice via
// agent_plan_slice_update (add_decision is append-only on the store side).
func (c *PlanClient) AppendSliceDecision(ctx context.Context, sliceID, note string) error {
	if c == nil || c.Hub == nil {
		return errors.New("plan: client not configured")
	}
	if strings.TrimSpace(sliceID) == "" {
		return errors.New("plan: slice_id required")
	}
	return c.planWrite(ctx, "agent_plan_slice_update", map[string]any{
		"slice_id":     sliceID,
		"add_decision": note,
	})
}

// AdvancePlan advances a plan's lifecycle phase via
// agent_plan_lifecycle_advance. The store enforces the phase DAG and records
// the hop (with actor + note) in phase_history.
func (c *PlanClient) AdvancePlan(ctx context.Context, planID, toPhase, note string) error {
	if c == nil || c.Hub == nil {
		return errors.New("plan: client not configured")
	}
	if strings.TrimSpace(planID) == "" {
		return errors.New("plan: plan_id required")
	}
	return c.planWrite(ctx, "agent_plan_lifecycle_advance", map[string]any{
		"plan_id":  planID,
		"to_phase": toPhase,
		"agent_id": takeupActor,
		"note":     note,
	})
}

// planWrite calls tool with args and fails unless the response decodes to
// {"ok": true}. Tool-error results (illegal transition, unknown id) surface
// as errors so the reconciler logs them instead of silently proceeding.
func (c *PlanClient) planWrite(ctx context.Context, tool string, args map[string]any) error {
	body, err := c.Hub.CallTool(ctx, c.serverName(), tool, args)
	if err != nil && body == "" {
		return fmt.Errorf("plan: %s: %w", tool, err)
	}
	var env okEnvelope
	if derr := decodeListBody(body, &env); derr != nil {
		if err != nil {
			return fmt.Errorf("plan: %s: %w", tool, err)
		}
		return fmt.Errorf("plan: %s decode: %w; raw=%q", tool, derr, truncateBody(body, 240))
	}
	if !env.OK {
		return fmt.Errorf("plan: %s rejected: %s", tool, truncateBody(body, 240))
	}
	return nil
}
