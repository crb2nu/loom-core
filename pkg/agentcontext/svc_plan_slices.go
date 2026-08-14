// svc_plan_slices.go -- PlanSlice CRUD + claim. Slices are their own records so
// parallel slice-implementers update status/decisions independently without
// racing on the parent plan. A fresh implementer resolves its slice by id
// (agent_plan_slice_get) instead of relying on the spawn prompt.
//
// Slices are non-semantic (zero/fallback vector); they are queried by plan_id /
// status / assigned_agent_id keyword indexes.
package agentcontext

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"gitlab.flexinfer.ai/libs/mcp-go"

	"github.com/crb2nu/loom/pkg/validate"
)

// ---- Service delegates -----------------------------------------------------

func (s *Service) HandlePlanSliceAdd(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	return s.plans.SliceAdd(ctx, args)
}
func (s *Service) HandlePlanSliceUpdate(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	return s.plans.SliceUpdate(ctx, args)
}
func (s *Service) HandlePlanSliceGet(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	return s.plans.SliceGet(ctx, args)
}
func (s *Service) HandlePlanSliceList(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	return s.plans.SliceList(ctx, args)
}
func (s *Service) HandlePlanSliceClaim(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	return s.plans.SliceClaim(ctx, args)
}

// ---- SliceAdd --------------------------------------------------------------

// SliceAdd appends a slice to an existing plan. The slice id is
// <plan_id>#<next-order>.
func (ps *PlanSvc) SliceAdd(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	v := validate.NewArgs(args)
	planID := v.Required("plan_id")
	name := v.Required("name")
	if err := v.Validate(); err != nil {
		return mcp.ErrorResult(err), nil
	}
	if plan, _ := ps.fetch(ctx, planID); plan == nil {
		return mcp.ErrorResult(fmt.Errorf("plan %q not found", planID)), nil
	}

	existing := ps.slicesForPlan(ctx, planID)
	order := len(existing) + 1
	now := time.Now().UTC()
	s := &PlanSlice{
		ID:                 fmt.Sprintf("%s#%d", planID, order),
		PlanID:             planID,
		Order:              order,
		Name:               name,
		Goal:               v.String("goal", ""),
		Files:              v.StringSlice("files"),
		AcceptanceCriteria: v.String("acceptance_criteria", ""),
		TestStrategy:       v.String("test_strategy", ""),
		InterfaceContracts: v.String("interface_contracts", ""),
		BranchName:         v.String("branch_name", ""),
		DependsOn:          v.StringSlice("depends_on"),
		Phase:              SlicePhasePending,
		CreatedAt:          now,
		UpdatedAt:          now,
	}
	if err := ps.persistSlice(ctx, s); err != nil {
		return mcp.ErrorResult(fmt.Errorf("persist slice: %w", err)), nil
	}
	return mcp.JSONResult(map[string]any{"ok": true, "slice_id": s.ID, "order": order})
}

// ---- SliceUpdate -----------------------------------------------------------

// SliceUpdate patches slice status/refs/decisions. Decisions append; refs append.
func (ps *PlanSvc) SliceUpdate(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	v := validate.NewArgs(args)
	sliceID := v.Required("slice_id")
	if err := v.Validate(); err != nil {
		return mcp.ErrorResult(err), nil
	}
	s, err := ps.fetchSlice(ctx, sliceID)
	if err != nil || s == nil {
		return mcp.ErrorResult(fmt.Errorf("slice %q not found", sliceID)), nil
	}
	if ph, ok := args["phase"]; ok {
		p := toString(ph)
		if !slicePhaseValid(p) {
			return mcp.ErrorResult(fmt.Errorf("invalid slice phase %q", p)), nil
		}
		s.Phase = p
	}
	if _, ok := args["mr_ref"]; ok {
		s.MRRef = toString(args["mr_ref"])
	}
	if _, ok := args["branch_name"]; ok {
		s.BranchName = toString(args["branch_name"])
	}
	if _, ok := args["add_commit_ref"]; ok {
		s.CommitRefs = appendUnique(s.CommitRefs, toString(args["add_commit_ref"]))
	}
	if _, ok := args["add_decision"]; ok {
		if d := toString(args["add_decision"]); d != "" {
			s.Decisions = append(s.Decisions, d)
		}
	}
	if _, ok := args["files"]; ok {
		s.Files = toStringSlice(args["files"])
	}
	s.UpdatedAt = time.Now().UTC()
	if err := ps.persistSlice(ctx, s); err != nil {
		return mcp.ErrorResult(fmt.Errorf("persist slice: %w", err)), nil
	}
	return mcp.JSONResult(map[string]any{"ok": true, "slice_id": s.ID, "phase": s.Phase})
}

// ---- SliceGet / SliceList --------------------------------------------------

// SliceGet returns a single slice by id. NOT agent-scoped — this is how a fresh
// slice-implementer looks up its own slice.
func (ps *PlanSvc) SliceGet(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	v := validate.NewArgs(args)
	sliceID := v.Required("slice_id")
	if err := v.Validate(); err != nil {
		return mcp.ErrorResult(err), nil
	}
	s, err := ps.fetchSlice(ctx, sliceID)
	if err != nil || s == nil {
		return mcp.ErrorResult(fmt.Errorf("slice %q not found", sliceID)), nil
	}
	return mcp.JSONResult(map[string]any{"ok": true, "slice": s})
}

// SliceList returns all slices for a plan, ordered.
func (ps *PlanSvc) SliceList(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	v := validate.NewArgs(args)
	planID := v.Required("plan_id")
	if err := v.Validate(); err != nil {
		return mcp.ErrorResult(err), nil
	}
	slices := ps.slicesForPlan(ctx, planID)
	return mcp.JSONResult(map[string]any{"ok": true, "count": len(slices), "slices": slices})
}

// ---- SliceClaim ------------------------------------------------------------

// SliceClaim assigns a pending/unassigned slice to an agent and marks it
// claimed. Returns a conflict (without overwriting) if another live agent
// already holds it, unless force=true.
func (ps *PlanSvc) SliceClaim(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	v := validate.NewArgs(args)
	sliceID := v.Required("slice_id")
	agentID := v.Required("agent_id")
	if err := v.Validate(); err != nil {
		return mcp.ErrorResult(err), nil
	}
	s, err := ps.fetchSlice(ctx, sliceID)
	if err != nil || s == nil {
		return mcp.ErrorResult(fmt.Errorf("slice %q not found", sliceID)), nil
	}
	force := v.Bool("force", false)
	if s.AssignedAgentID != "" && s.AssignedAgentID != agentID && !force {
		return mcp.JSONResult(map[string]any{
			"ok":         false,
			"conflict":   true,
			"slice_id":   s.ID,
			"held_by":    s.AssignedAgentID,
			"held_phase": s.Phase,
			"hint":       "another agent holds this slice; pass force=true to steal",
		})
	}

	sessionID := v.String("session_id", s.SessionID)

	// Enforce the slice's file boundary: hard-claim its files for this agent.
	// If any file is held by another active agent, refuse the slice claim
	// (all-or-nothing) so two parallel implementers never collide on a file.
	if ps.claimFiles != nil && len(s.Files) > 0 {
		if conflicting := ps.claimFiles(ctx, agentID, sessionID, "slice "+s.ID, s.Files); len(conflicting) > 0 {
			return mcp.JSONResult(map[string]any{
				"ok":                false,
				"conflict":          true,
				"slice_id":          s.ID,
				"conflicting_files": conflicting,
				"hint":              "one or more of this slice's files are claimed by another agent",
			})
		}
	}

	s.AssignedAgentID = agentID
	s.SessionID = sessionID
	s.WorktreeID = v.String("worktree_id", s.WorktreeID)
	if v.String("branch_name", "") != "" {
		s.BranchName = v.String("branch_name", "")
	}
	if s.Phase == SlicePhasePending {
		s.Phase = SlicePhaseClaimed
	}
	s.UpdatedAt = time.Now().UTC()
	if err := ps.persistSlice(ctx, s); err != nil {
		return mcp.ErrorResult(fmt.Errorf("persist slice: %w", err)), nil
	}
	return mcp.JSONResult(map[string]any{
		"ok":         true,
		"slice_id":   s.ID,
		"claimed_by": agentID,
		"phase":      s.Phase,
	})
}

// ---- internals -------------------------------------------------------------

// slicesForPlan returns a plan's slices ordered by Order. Qdrant-first, cache
// fallback.
func (ps *PlanSvc) slicesForPlan(ctx context.Context, planID string) []PlanSlice {
	var out []PlanSlice
	if ps.slicesQ != nil {
		points, err := ps.slicesQ.ScrollPoints(ctx, FilterMust(Match("plan_id", planID)), 500, false)
		if err == nil {
			for _, p := range points {
				if s := payloadToSlice(p.Payload); s != nil {
					out = append(out, *s)
					ps.mu.Lock()
					ps.slices[s.ID] = s
					ps.mu.Unlock()
				}
			}
			sort.Slice(out, func(i, j int) bool { return out[i].Order < out[j].Order })
			return out
		}
		ps.logger.Debug("slice scroll failed; using cache", "plan_id", planID, "error", err)
	}
	ps.mu.RLock()
	for _, s := range ps.slices {
		if s.PlanID == planID {
			out = append(out, *s)
		}
	}
	ps.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Order < out[j].Order })
	return out
}

// fetchSlice resolves a slice by id, Qdrant-first.
func (ps *PlanSvc) fetchSlice(ctx context.Context, sliceID string) (*PlanSlice, error) {
	if ps.slicesQ != nil {
		raw, err := ps.slicesQ.GetPoint(ctx, sliceID, false)
		switch {
		case err == nil:
			if s := payloadToSlice(raw.Payload); s != nil {
				ps.mu.Lock()
				ps.slices[s.ID] = s
				ps.mu.Unlock()
				return s, nil
			}
		case errors.Is(err, ErrCollectionNotFound):
		default:
			ps.logger.Debug("slice fetch failed; trying cache", "slice_id", sliceID, "error", err)
		}
	}
	ps.mu.RLock()
	cached := ps.slices[sliceID]
	ps.mu.RUnlock()
	return cached, nil
}

// persistSlice writes a slice (non-semantic fallback vector) and updates cache.
func (ps *PlanSvc) persistSlice(ctx context.Context, s *PlanSlice) error {
	ps.mu.Lock()
	ps.slices[s.ID] = s
	ps.mu.Unlock()
	if ps.slicesQ == nil {
		return nil
	}
	if err := ps.slicesQ.EnsureCollection(ctx, sessionsVectorSize); err != nil {
		return err
	}
	point := Point{
		ID:      s.ID,
		Vector:  fallbackEmbedVector(sessionsVectorSize),
		Payload: sliceToPayload(s),
	}
	return ps.slicesQ.Upsert(ctx, []Point{point}, true)
}

// slicePhaseValid reports whether p is a known slice phase.
func slicePhaseValid(p string) bool {
	switch p {
	case SlicePhasePending, SlicePhaseClaimed, SlicePhaseImplementing,
		SlicePhaseImplemented, SlicePhaseInReview, SlicePhaseIntegrated, SlicePhaseMerged:
		return true
	default:
		return false
	}
}
