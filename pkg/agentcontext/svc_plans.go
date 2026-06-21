// svc_plans.go -- PlanSvc: create/get/list for the first-class Plan entity.
//
// SCOPING INVARIANT (the whole point of this entity): plan reads are scoped by
// plan_id / project / namespace and are NEVER filtered by agent_id. The default
// context recall path filters by agent_id (service_recall.go), which would hide
// a plan from exactly the parallel + Mills agents that need it. Plans are
// deliberately cross-agent: any agent may read a project's plans; writes are
// attributed (created_by) but not gated by identity.
package agentcontext

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"gitlab.flexinfer.ai/libs/mcp-go"

	"github.com/crb2nu/loom/pkg/validate"
)

// PlanSvc manages Plan records. Qdrant is the source of truth (so plans are
// visible across processes/worktrees/agents); the in-memory map is a
// write-through cache and is only consulted as a fallback when Qdrant is
// unavailable.
type PlanSvc struct {
	mu     sync.RWMutex
	plans  map[string]*Plan
	qdrant *QdrantClient // CollPlans
	logger *slog.Logger
}

// HandlePlanCreate creates a new plan.
func (s *Service) HandlePlanCreate(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	return s.plans.Create(ctx, args)
}

// HandlePlanGet returns a plan by id (cross-agent, not agent-scoped).
func (s *Service) HandlePlanGet(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	return s.plans.Get(ctx, args)
}

// HandlePlanList lists plans by project/namespace (cross-agent, not agent-scoped).
func (s *Service) HandlePlanList(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	return s.plans.List(ctx, args)
}

// NewPlanSvc constructs a PlanSvc.
func NewPlanSvc(qdrant *QdrantClient, logger *slog.Logger) *PlanSvc {
	return &PlanSvc{
		plans:  make(map[string]*Plan),
		qdrant: qdrant,
		logger: logger,
	}
}

// Create persists a new Plan and returns its id.
func (ps *PlanSvc) Create(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	v := validate.NewArgs(args)
	title := v.Required("title")
	if err := v.Validate(); err != nil {
		return mcp.ErrorResult(err), nil
	}

	now := time.Now().UTC()
	phase := v.String("phase", PlanPhaseDraft)
	plan := &Plan{
		ID:            v.String("id", ""),
		Title:         title,
		Project:       v.String("project", ""),
		Namespace:     v.String("namespace", ""),
		Phase:         phase,
		SpecDoc:       v.String("spec_doc", ""),
		CreatedBy:     v.String("agent_id", ""),
		SourceSession: v.String("session_id", ""),
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if plan.ID == "" {
		plan.ID = GeneratePlanID(title, plan.Namespace, now)
	}
	plan.Slug = planSlug(title)
	plan.Slices = parsePlanSlicesArg(args["slices"], plan.ID)

	if err := ps.persist(ctx, plan); err != nil {
		return mcp.ErrorResult(fmt.Errorf("persist plan: %w", err)), nil
	}

	ps.mu.Lock()
	ps.plans[plan.ID] = plan
	ps.mu.Unlock()

	return mcp.JSONResult(map[string]any{
		"ok":          true,
		"plan_id":     plan.ID,
		"slug":        plan.Slug,
		"phase":       plan.Phase,
		"slice_count": len(plan.Slices),
	})
}

// Get returns a Plan by id. Reads Qdrant first (cross-process source of truth),
// falling back to the in-memory cache only if Qdrant errors. NOT agent-scoped.
func (ps *PlanSvc) Get(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	v := validate.NewArgs(args)
	planID := v.Required("plan_id")
	if err := v.Validate(); err != nil {
		return mcp.ErrorResult(err), nil
	}

	plan, err := ps.fetch(ctx, planID)
	if err != nil {
		return mcp.ErrorResult(fmt.Errorf("get plan: %w", err)), nil
	}
	if plan == nil {
		return mcp.ErrorResult(fmt.Errorf("plan %q not found", planID)), nil
	}
	return mcp.JSONResult(map[string]any{"ok": true, "plan": plan})
}

// fetch resolves a plan by id, Qdrant-first.
func (ps *PlanSvc) fetch(ctx context.Context, planID string) (*Plan, error) {
	if ps.qdrant != nil {
		raw, err := ps.qdrant.GetPoint(ctx, planID, false)
		switch {
		case err == nil:
			if p := payloadToPlan(raw.Payload); p != nil {
				ps.mu.Lock()
				ps.plans[p.ID] = p
				ps.mu.Unlock()
				return p, nil
			}
		case errors.Is(err, ErrCollectionNotFound):
			// collection not yet created → no plans persisted
		default:
			// Qdrant transport/HTTP error (incl. 404 for a missing point).
			// Fall through to the cache below rather than hard-failing.
			ps.logger.Debug("plan fetch from qdrant failed; trying cache", "plan_id", planID, "error", err)
		}
	}

	ps.mu.RLock()
	cached := ps.plans[planID]
	ps.mu.RUnlock()
	return cached, nil
}

// List returns plans filtered by project and/or namespace. NOT agent-scoped.
func (ps *PlanSvc) List(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	v := validate.NewArgs(args)
	project := v.String("project", "")
	namespace := v.String("namespace", "")
	limit := v.Int("limit", 100)

	plans, err := ps.list(ctx, project, namespace, limit)
	if err != nil {
		return mcp.ErrorResult(fmt.Errorf("list plans: %w", err)), nil
	}
	return mcp.JSONResult(map[string]any{
		"ok":    true,
		"count": len(plans),
		"plans": plans,
	})
}

func (ps *PlanSvc) list(ctx context.Context, project, namespace string, limit int) ([]*Plan, error) {
	if ps.qdrant == nil {
		return ps.listFromCache(project, namespace), nil
	}
	var conds []any
	if project != "" {
		conds = append(conds, Match("project", project))
	}
	if namespace != "" {
		conds = append(conds, Match("namespace", namespace))
	}
	var filter map[string]any
	if len(conds) > 0 {
		filter = FilterMust(conds...)
	}
	points, err := ps.qdrant.ScrollPoints(ctx, filter, limit, false)
	if err != nil {
		return ps.listFromCache(project, namespace), nil
	}
	out := make([]*Plan, 0, len(points))
	for _, p := range points {
		if plan := payloadToPlan(p.Payload); plan != nil {
			out = append(out, plan)
		}
	}
	return out, nil
}

func (ps *PlanSvc) listFromCache(project, namespace string) []*Plan {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	out := make([]*Plan, 0, len(ps.plans))
	for _, p := range ps.plans {
		if project != "" && p.Project != project {
			continue
		}
		if namespace != "" && p.Namespace != namespace {
			continue
		}
		out = append(out, p)
	}
	return out
}

// persist writes a plan to Qdrant (zero vector for the non-semantic MVP).
func (ps *PlanSvc) persist(ctx context.Context, p *Plan) error {
	if ps.qdrant == nil {
		return nil
	}
	if err := ps.qdrant.EnsureCollection(ctx, sessionsVectorSize); err != nil {
		return err
	}
	point := Point{
		ID:      p.ID,
		Vector:  make([]float64, sessionsVectorSize),
		Payload: planToPayload(p),
	}
	return ps.qdrant.Upsert(ctx, []Point{point}, true)
}

// parsePlanSlicesArg normalizes the optional "slices" argument into PlanSlices,
// stamping each with a stable id (<plan_id>#<n>) and a default phase.
func parsePlanSlicesArg(raw any, planID string) []PlanSlice {
	arr, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := make([]PlanSlice, 0, len(arr))
	for i, item := range arr {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		s := PlanSlice{
			ID:    fmt.Sprintf("%s#%d", planID, i+1),
			Order: i + 1,
			Name:  toString(m["name"]),
			Goal:  toString(m["goal"]),
			Files: toStringSlice(m["files"]),
			Phase: SlicePhasePending,
		}
		out = append(out, s)
	}
	return out
}
