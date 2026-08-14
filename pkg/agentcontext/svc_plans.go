// svc_plans.go -- PlanSvc: plan-level CRUD, semantic search, and validated
// lifecycle transitions for the first-class Plan entity.
//
// SCOPING INVARIANT (the whole point of this entity): plan reads are scoped by
// plan_id / project / namespace and are NEVER filtered by agent_id. The default
// context recall path filters by agent_id (service_recall.go), which would hide
// a plan from exactly the parallel + Mills agents that need it. Plans are
// deliberately cross-agent: any agent may read a project's plans; writes are
// attributed (created_by) but not gated by identity.
//
// Slices live in their own collection (svc_plan_slices.go) so parallel
// slice-implementers update them independently; Get aggregates them onto the
// plan for callers.
package agentcontext

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"gitlab.flexinfer.ai/libs/mcp-go"

	"github.com/crb2nu/loom/pkg/codebase/embed"
	"github.com/crb2nu/loom/pkg/projectmeta"
	"github.com/crb2nu/loom/pkg/validate"
)

// PlanSvc manages Plan + PlanSlice records. Qdrant is the source of truth (so
// plans are visible across processes/worktrees/agents); the in-memory maps are
// a write-through cache consulted only as a fallback when Qdrant is unavailable.
type PlanSvc struct {
	mu     sync.RWMutex
	plans  map[string]*Plan
	slices map[string]*PlanSlice // slice_id -> slice

	plansQ  *QdrantClient // CollPlans
	slicesQ *QdrantClient // CollPlanSlices
	embedr  embed.Embedder
	// vectorSize is the shared discovered embedding dimension (same pointer the
	// task service uses) so collections stay consistent.
	vectorSize *int
	logger     *slog.Logger

	// claimFiles, when wired (service.go), hard-claims a slice's files for the
	// claiming agent and returns any files held by another active agent (empty =
	// all acquired). This makes a slice's file boundary enforced, not advisory.
	claimFiles func(ctx context.Context, agentID, sessionID, reason string, files []string) []string
}

// NewPlanSvc constructs a PlanSvc. embedr/vectorSize may be nil in tests (embed
// is then skipped and a deterministic fallback vector is used).
func NewPlanSvc(plansQ, slicesQ *QdrantClient, embedr embed.Embedder, vectorSize *int, logger *slog.Logger) *PlanSvc {
	if vectorSize == nil {
		vs := 0
		vectorSize = &vs
	}
	return &PlanSvc{
		plans:      make(map[string]*Plan),
		slices:     make(map[string]*PlanSlice),
		plansQ:     plansQ,
		slicesQ:    slicesQ,
		embedr:     embedr,
		vectorSize: vectorSize,
		logger:     logger,
	}
}

// ---- Service delegates -----------------------------------------------------

func (s *Service) HandlePlanCreate(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	return s.plans.Create(ctx, args)
}
func (s *Service) HandlePlanGet(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	return s.plans.Get(ctx, args)
}
func (s *Service) HandlePlanList(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	return s.plans.List(ctx, args)
}
func (s *Service) HandlePlanUpdate(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	return s.plans.Update(ctx, args)
}
func (s *Service) HandlePlanSearch(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	return s.plans.Search(ctx, args)
}
func (s *Service) HandlePlanLifecycleAdvance(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	return s.plans.LifecycleAdvance(ctx, args)
}

// ---- Create ----------------------------------------------------------------

// Create persists a new Plan (and any seed slices) and returns its id.
func (ps *PlanSvc) Create(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	v := validate.NewArgs(args)
	title := v.Required("title")
	if err := v.Validate(); err != nil {
		return mcp.ErrorResult(err), nil
	}

	now := time.Now().UTC()
	phase := v.String("phase", PlanPhaseDraft)
	if !planPhaseValid(phase) {
		return mcp.ErrorResult(fmt.Errorf("invalid phase %q", phase)), nil
	}
	priority := normalizePlanPriority(v.String("priority", ""))
	if priority != "" && !planPriorityValid(priority) {
		return mcp.ErrorResult(fmt.Errorf("invalid priority %q (want P0..P3 or empty)", priority)), nil
	}
	plan := &Plan{
		ID:                 v.String("id", ""),
		Title:              title,
		Project:            v.String("project", ""),
		Namespace:          v.String("namespace", ""),
		Phase:              phase,
		Priority:           priority,
		Objective:          v.String("objective", ""),
		SpecDoc:            v.String("spec_doc", ""),
		SpecAnchor:         v.String("spec_anchor", ""),
		CreatedBy:          v.String("agent_id", ""),
		SourceSession:      v.String("session_id", ""),
		RespunFrom:         v.String("respun_from", ""),
		Budget:             v.String("budget", ""),
		RiskiestAssumption: v.String("riskiest_assumption", ""),
		KillTest:           v.String("kill_test", ""),
		KillTestStatus:     v.String("kill_test_status", ""),
		Dependencies:       v.StringSlice("dependencies"),
		MillsBacklogID:     v.String("mills_backlog_id", ""),
		GitLabIssueIID:     v.Int("gitlab_issue_iid", 0),
		CreatedAt:          now,
		UpdatedAt:          now,
	}
	if plan.ID == "" {
		plan.ID = GeneratePlanID(title, plan.Namespace, now)
	}
	plan.Slug = planSlug(title)
	plan.Success = parseSuccessArg(args["success"])
	ps.warnBareProject(plan.ID, plan.Project)

	if err := ps.persist(ctx, plan); err != nil {
		return mcp.ErrorResult(fmt.Errorf("persist plan: %w", err)), nil
	}

	// Seed slices (own collection).
	seeded := parsePlanSlicesArg(args["slices"], plan.ID, now)
	for _, sl := range seeded {
		s := sl
		if err := ps.persistSlice(ctx, &s); err != nil {
			ps.logger.Warn("persist seed slice failed", "plan_id", plan.ID, "slice", s.ID, "error", err)
		}
	}

	ps.mu.Lock()
	ps.plans[plan.ID] = plan
	ps.mu.Unlock()

	return mcp.JSONResult(map[string]any{
		"ok":          true,
		"plan_id":     plan.ID,
		"slug":        plan.Slug,
		"phase":       plan.Phase,
		"slice_count": len(seeded),
	})
}

// ---- Get -------------------------------------------------------------------

// Get returns a Plan (with aggregated slices) by id. Qdrant-first, cache
// fallback. NOT agent-scoped.
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
	plan.Slices = ps.slicesForPlan(ctx, planID)
	return mcp.JSONResult(map[string]any{"ok": true, "plan": plan})
}

// fetch resolves a plan by id, Qdrant-first.
func (ps *PlanSvc) fetch(ctx context.Context, planID string) (*Plan, error) {
	if ps.plansQ != nil {
		raw, err := ps.plansQ.GetPoint(ctx, planID, false)
		switch {
		case err == nil:
			if p := payloadToPlan(raw.Payload); p != nil {
				ps.mu.Lock()
				ps.plans[p.ID] = p
				ps.mu.Unlock()
				return p, nil
			}
		case errors.Is(err, ErrCollectionNotFound):
		default:
			ps.logger.Debug("plan fetch from qdrant failed; trying cache", "plan_id", planID, "error", err)
		}
	}
	ps.mu.RLock()
	cached := ps.plans[planID]
	ps.mu.RUnlock()
	return cached, nil
}

// ---- Update ----------------------------------------------------------------

// Update patches mutable plan fields (spec, title, refs, links). Phase changes
// must go through LifecycleAdvance.
func (ps *PlanSvc) Update(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	v := validate.NewArgs(args)
	planID := v.Required("plan_id")
	if err := v.Validate(); err != nil {
		return mcp.ErrorResult(err), nil
	}
	plan, err := ps.fetch(ctx, planID)
	if err != nil || plan == nil {
		return mcp.ErrorResult(fmt.Errorf("plan %q not found", planID)), nil
	}

	if title, ok := args["title"]; ok {
		plan.Title = toString(title)
		plan.Slug = planSlug(plan.Title)
	}
	// project/namespace are mutable so a plan minted with a mis-scoped project
	// (e.g. bare "loom-core" instead of canonical "services/loom-core") can be
	// corrected in place — without a destructive re-Create, which upserts by id.
	// The HUD groups by the exact project string, so this is the only non-lossy
	// way to merge a stray card back into its project.
	if _, ok := args["project"]; ok {
		plan.Project = toString(args["project"])
		ps.warnBareProject(plan.ID, plan.Project)
	}
	if _, ok := args["namespace"]; ok {
		plan.Namespace = toString(args["namespace"])
	}
	if _, ok := args["objective"]; ok {
		// Objective is patchable so an operator/agent can enrich a plan authored
		// before this field existed (or a sparse spun draft) in place.
		plan.Objective = toString(args["objective"])
	}
	if _, ok := args["spec_doc"]; ok {
		plan.SpecDoc = toString(args["spec_doc"])
	}
	if _, ok := args["spec_anchor"]; ok {
		plan.SpecAnchor = toString(args["spec_anchor"])
	}
	if _, ok := args["mirror_path"]; ok {
		plan.MirrorPath = toString(args["mirror_path"])
	}
	if _, ok := args["kill_test_status"]; ok {
		plan.KillTestStatus = toString(args["kill_test_status"])
	}
	// priority is the operator's beam-steering knob: settable and clearable
	// (empty string) any time. The plan-slice emitter resyncs still-queued
	// Mills backlog items to it on its next tick, so a reorder here changes
	// dispatch order without re-emitting.
	if _, ok := args["priority"]; ok {
		pr := normalizePlanPriority(toString(args["priority"]))
		if pr != "" && !planPriorityValid(pr) {
			return mcp.ErrorResult(fmt.Errorf("invalid priority %q (want P0..P3 or empty)", pr)), nil
		}
		plan.Priority = pr
	}
	if _, ok := args["mills_backlog_id"]; ok {
		plan.MillsBacklogID = toString(args["mills_backlog_id"])
	}
	if _, ok := args["add_mr_ref"]; ok {
		plan.MRRefs = appendUnique(plan.MRRefs, toString(args["add_mr_ref"]))
	}
	if _, ok := args["add_pipeline_ref"]; ok {
		plan.PipelineRefs = appendUnique(plan.PipelineRefs, toString(args["add_pipeline_ref"]))
	}
	if _, ok := args["add_deploy_ref"]; ok {
		plan.DeployRefs = appendUnique(plan.DeployRefs, toString(args["add_deploy_ref"]))
	}
	if s, ok := args["success"]; ok {
		plan.Success = parseSuccessArg(s)
	}
	plan.UpdatedAt = time.Now().UTC()

	if err := ps.persist(ctx, plan); err != nil {
		return mcp.ErrorResult(fmt.Errorf("persist plan: %w", err)), nil
	}
	ps.mu.Lock()
	ps.plans[plan.ID] = plan
	ps.mu.Unlock()
	return mcp.JSONResult(map[string]any{"ok": true, "plan_id": plan.ID, "phase": plan.Phase})
}

// ---- Lifecycle -------------------------------------------------------------

// LifecycleAdvance transitions a plan to a new phase if the DAG allows it, and
// records the hop in phase_history for HUD/audit rendering.
func (ps *PlanSvc) LifecycleAdvance(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	v := validate.NewArgs(args)
	planID := v.Required("plan_id")
	to := v.Required("to_phase")
	if err := v.Validate(); err != nil {
		return mcp.ErrorResult(err), nil
	}
	if !planPhaseValid(to) {
		return mcp.ErrorResult(fmt.Errorf("invalid to_phase %q", to)), nil
	}
	plan, err := ps.fetch(ctx, planID)
	if err != nil || plan == nil {
		return mcp.ErrorResult(fmt.Errorf("plan %q not found", planID)), nil
	}
	from := plan.Phase
	if err := ps.advancePhase(ctx, plan, to, v.String("agent_id", ""), v.String("note", "")); err != nil {
		return mcp.ErrorResult(err), nil
	}
	return mcp.JSONResult(map[string]any{
		"ok":         true,
		"plan_id":    plan.ID,
		"from_phase": from,
		"to_phase":   plan.Phase,
	})
}

// advancePhase applies one validated phase hop to an already-fetched plan:
// records the transition in phase_history, persists, and refreshes the cache
// (persist alone does not, so a Qdrant outage would otherwise serve the stale
// pre-hop phase). Non-MCP callers — the plan truth sweep — use this directly.
func (ps *PlanSvc) advancePhase(ctx context.Context, plan *Plan, to, actor, note string) error {
	from := plan.Phase
	if !planPhaseCanTransition(from, to) {
		return fmt.Errorf("illegal transition %s -> %s (allowed: %v)", from, to, planPhaseTransitions[from])
	}
	now := time.Now().UTC()
	if from != to {
		plan.PhaseHistory = append(plan.PhaseHistory, PhaseTransition{
			From:  from,
			To:    to,
			At:    now,
			Actor: actor,
			Note:  note,
		})
		plan.Phase = to
		plan.UpdatedAt = now
	}
	if err := ps.persist(ctx, plan); err != nil {
		return fmt.Errorf("persist plan: %w", err)
	}
	ps.mu.Lock()
	ps.plans[plan.ID] = plan
	ps.mu.Unlock()
	return nil
}

// ---- List / Search ---------------------------------------------------------

// List returns plans filtered by project and/or namespace. NOT agent-scoped.
func (ps *PlanSvc) List(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	v := validate.NewArgs(args)
	project := v.String("project", "")
	namespace := v.String("namespace", "")
	phase := v.String("phase", "")
	limit := v.Int("limit", 100)

	plans, err := ps.list(ctx, project, namespace, phase, limit)
	if err != nil {
		return mcp.ErrorResult(fmt.Errorf("list plans: %w", err)), nil
	}
	sortPlansBeamOrder(plans)
	ps.attachSliceSummaries(ctx, plans)
	return mcp.JSONResult(map[string]any{"ok": true, "count": len(plans), "plans": plans})
}

// sortPlansBeamOrder orders plans the way the warp beam dispatches: explicit
// priority buckets first (P0 highest), unset priorities last, newest-updated
// first within a bucket. List returns this order deterministically so the HUD
// and the emitter see the same beam without client-side sorting.
func sortPlansBeamOrder(plans []*Plan) {
	sort.SliceStable(plans, func(i, j int) bool {
		ri, rj := planPriorityRank(plans[i].Priority), planPriorityRank(plans[j].Priority)
		if ri != rj {
			return ri < rj
		}
		return plans[i].UpdatedAt.After(plans[j].UpdatedAt)
	})
}

// attachSliceSummaries fills each plan's SliceSummary (phase -> count) with ONE
// batched scroll of the slice collection (grouped in memory by plan_id), so the
// HUD board can render per-plan slice progress without a detail fetch per card.
// Best-effort: on any error or empty store it leaves summaries unset and the
// cards simply omit the progress bar.
func (ps *PlanSvc) attachSliceSummaries(ctx context.Context, plans []*Plan) {
	if len(plans) == 0 {
		return
	}
	byPlan := make(map[string]map[string]int)
	add := func(planID, phase string) {
		if planID == "" {
			return
		}
		m := byPlan[planID]
		if m == nil {
			m = make(map[string]int)
			byPlan[planID] = m
		}
		m[phase]++
	}

	if ps.slicesQ != nil {
		if points, err := ps.slicesQ.ScrollPoints(ctx, nil, 2000, false); err == nil {
			for _, pt := range points {
				if s := payloadToSlice(pt.Payload); s != nil {
					add(s.PlanID, s.Phase)
				}
			}
		} else {
			ps.logger.Debug("slice-summary scroll failed; using cache", "error", err)
		}
	}
	// Fall back to the in-memory cache when Qdrant is absent (tests) or empty.
	if len(byPlan) == 0 {
		ps.mu.RLock()
		for _, s := range ps.slices {
			if s != nil {
				add(s.PlanID, s.Phase)
			}
		}
		ps.mu.RUnlock()
	}

	for _, p := range plans {
		if m, ok := byPlan[p.ID]; ok && len(m) > 0 {
			p.SliceSummary = m
		}
	}
}

func (ps *PlanSvc) list(ctx context.Context, project, namespace, phase string, limit int) ([]*Plan, error) {
	if ps.plansQ == nil {
		return ps.listFromCache(project, namespace, phase), nil
	}
	var conds []any
	if project != "" {
		conds = append(conds, Match("project", project))
	}
	if namespace != "" {
		conds = append(conds, Match("namespace", namespace))
	}
	if phase != "" {
		conds = append(conds, Match("status", phase))
	}
	var filter map[string]any
	if len(conds) > 0 {
		filter = FilterMust(conds...)
	}
	points, err := ps.plansQ.ScrollPoints(ctx, filter, limit, false)
	if err != nil {
		return ps.listFromCache(project, namespace, phase), nil
	}
	out := make([]*Plan, 0, len(points))
	for _, p := range points {
		if plan := payloadToPlan(p.Payload); plan != nil {
			out = append(out, plan)
		}
	}
	return out, nil
}

func (ps *PlanSvc) listFromCache(project, namespace, phase string) []*Plan {
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
		if phase != "" && p.Phase != phase {
			continue
		}
		out = append(out, p)
	}
	return out
}

// Search runs a semantic search over plan title+spec, optionally scoped to a
// project. Falls back to a keyword list when no embedder/Qdrant is available.
func (ps *PlanSvc) Search(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	v := validate.NewArgs(args)
	query := v.Required("query")
	if err := v.Validate(); err != nil {
		return mcp.ErrorResult(err), nil
	}
	project := v.String("project", "")
	limit := v.Int("limit", 20)

	if ps.plansQ == nil || ps.embedr == nil {
		plans, _ := ps.list(ctx, project, "", "", limit)
		return mcp.JSONResult(map[string]any{"ok": true, "count": len(plans), "plans": plans, "mode": "fallback-list"})
	}
	vec, err := ps.embedr.EmbedQuery(ctx, query)
	if err != nil || len(vec) == 0 {
		ps.logger.Warn("plan search embed failed; falling back to list", "error", err)
		plans, _ := ps.list(ctx, project, "", "", limit)
		return mcp.JSONResult(map[string]any{"ok": true, "count": len(plans), "plans": plans, "mode": "fallback-list"})
	}
	var filter map[string]any
	if project != "" {
		filter = FilterMust(Match("project", project))
	}
	points, err := ps.plansQ.SearchRaw(ctx, vec, filter, limit)
	if err != nil {
		return mcp.ErrorResult(fmt.Errorf("plan search: %w", err)), nil
	}
	out := make([]*Plan, 0, len(points))
	for _, p := range points {
		if plan := payloadToPlan(p.Payload); plan != nil {
			out = append(out, plan)
		}
	}
	return mcp.JSONResult(map[string]any{"ok": true, "count": len(out), "plans": out, "mode": "semantic"})
}

// ---- persistence -----------------------------------------------------------

// persist writes a plan to Qdrant, embedding title+spec best-effort (a failed
// embedder must NEVER block the write — embedding is enrichment, not a
// correctness gate; a deterministic fallback vector keeps the point valid under
// cosine distance).
func (ps *PlanSvc) persist(ctx context.Context, p *Plan) error {
	if ps.plansQ == nil {
		return nil
	}
	vec := ps.embedText(ctx, p.Title+" "+p.SpecDoc)
	size := ps.resolveVectorSize(ctx, vec, ps.plansQ)
	if err := ps.plansQ.EnsureCollection(ctx, size); err != nil {
		return err
	}
	if len(vec) != size {
		vec = fallbackEmbedVector(size)
	}
	return ps.plansQ.Upsert(ctx, []Point{{ID: p.ID, Vector: vec, Payload: planToPayload(p)}}, true)
}

// embedText returns an embedding for text, or nil on any failure / no embedder.
func (ps *PlanSvc) embedText(ctx context.Context, text string) []float64 {
	if ps.embedr == nil {
		return nil
	}
	vecs, err := ps.embedr.EmbedDocuments(ctx, []string{text})
	if err != nil || len(vecs) != 1 || len(vecs[0]) == 0 {
		ps.logger.Warn("plan embed failed; using fallback vector", "error", err)
		return nil
	}
	return vecs[0]
}

// resolveVectorSize picks the embedding dimension: a real vector wins, then the
// shared discovered size, then the live collection, then the embedder default.
func (ps *PlanSvc) resolveVectorSize(ctx context.Context, vec []float64, q *QdrantClient) int {
	if len(vec) > 0 {
		*ps.vectorSize = len(vec)
	}
	if *ps.vectorSize <= 0 {
		if exists, size, err := q.GetCollectionVectorSize(ctx); err == nil && exists && size > 0 {
			*ps.vectorSize = size
		}
	}
	if *ps.vectorSize <= 0 {
		*ps.vectorSize = defaultEmbedVectorSize
	}
	return *ps.vectorSize
}

// ---- arg parsing -----------------------------------------------------------

func parseSuccessArg(raw any) *SuccessCriteria {
	m, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	sc := &SuccessCriteria{
		Tests:       toStringSlice(m["tests"]),
		Metrics:     toStringSlice(m["metrics"]),
		ManualCheck: toString(m["manual_check"]),
	}
	if len(sc.Tests) == 0 && len(sc.Metrics) == 0 && sc.ManualCheck == "" {
		return nil
	}
	return sc
}

// parsePlanSlicesArg normalizes the optional "slices" arg into PlanSlices,
// stamping each with a stable id (<plan_id>#<n>) and a default phase. It then
// resolves each slice's depends_on entries: an entry that names a sibling slice
// (case-insensitive) is rewritten to that sibling's slice_id, so the DAG edges
// the producer emits by name (the only stable key it has — it cannot know the
// plan_id, and the spin flattens/reindexes slices) become resolvable slice_ids.
// Entries already in <plan_id>#N form (or that match no sibling name) pass
// through untouched, so callers that supply real slice_ids keep working.
func parsePlanSlicesArg(raw any, planID string, now time.Time) []PlanSlice {
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
		out = append(out, PlanSlice{
			ID:                 fmt.Sprintf("%s#%d", planID, i+1),
			PlanID:             planID,
			Order:              i + 1,
			Name:               toString(m["name"]),
			Goal:               toString(m["goal"]),
			Files:              toStringSlice(m["files"]),
			AcceptanceCriteria: toString(m["acceptance_criteria"]),
			TestStrategy:       toString(m["test_strategy"]),
			InterfaceContracts: toString(m["interface_contracts"]),
			BranchName:         toString(m["branch_name"]),
			DependsOn:          toStringSlice(m["depends_on"]),
			Phase:              SlicePhasePending,
			CreatedAt:          now,
			UpdatedAt:          now,
		})
	}
	resolveSliceDependsByName(out)
	return out
}

// resolveSliceDependsByName rewrites depends_on entries that name a sibling
// slice into that sibling's slice_id. Self-references and unresolved entries
// that are neither a sibling name nor already an id are dropped, so the stored
// DAG only ever holds real slice_ids the HUD can map back to order numbers.
func resolveSliceDependsByName(slices []PlanSlice) {
	if len(slices) == 0 {
		return
	}
	byName := make(map[string]string, len(slices)) // lower(name) -> slice_id
	ids := make(map[string]struct{}, len(slices))  // known slice_ids
	for _, s := range slices {
		if n := strings.ToLower(strings.TrimSpace(s.Name)); n != "" {
			byName[n] = s.ID
		}
		ids[s.ID] = struct{}{}
	}
	for i := range slices {
		if len(slices[i].DependsOn) == 0 {
			continue
		}
		resolved := make([]string, 0, len(slices[i].DependsOn))
		seen := make(map[string]struct{}, len(slices[i].DependsOn))
		for _, dep := range slices[i].DependsOn {
			d := strings.TrimSpace(dep)
			if d == "" {
				continue
			}
			id := d
			if _, isID := ids[d]; !isID {
				if mapped, ok := byName[strings.ToLower(d)]; ok {
					id = mapped
				} else {
					continue // neither a known id nor a sibling name → drop
				}
			}
			if id == slices[i].ID { // no self-edges
				continue
			}
			if _, dup := seen[id]; dup {
				continue
			}
			seen[id] = struct{}{}
			resolved = append(resolved, id)
		}
		slices[i].DependsOn = resolved
	}
}

// warnBareProject logs when a project id lacks a workspace/group path segment
// (e.g. "loom-core" rather than "services/loom-core"). Because the HUD groups by
// the exact project string, a bare id renders as its own ungrouped project card.
// This is a non-fatal nudge — the write still proceeds — so existing callers are
// never broken; canonicalization stays the caller's choice.
func (ps *PlanSvc) warnBareProject(planID, project string) {
	if projectmeta.LooksLikeBareRepo(project) {
		ps.logger.Warn("plan project looks like a bare repo name; HUD groups by the exact project string — prefer a workspace-bucketed path (services/<repo>, libs/<repo>, ...) or a GitLab path_with_namespace",
			"plan_id", planID, "project", project)
	}
}

func appendUnique(list []string, v string) []string {
	if v == "" {
		return list
	}
	for _, e := range list {
		if e == v {
			return list
		}
	}
	return append(list, v)
}
