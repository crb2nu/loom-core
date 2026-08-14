package council

import (
	"context"
	"errors"
	"testing"

	"github.com/crb2nu/loom/pkg/mills/store"
)

// stubPlanListerAuthor implements PlanAuthor, SlicedPlanAuthor AND the optional
// PlanLister so the mutator's plan-store dedup path can be exercised. existing
// seeds the namespace snapshot; listErr forces the fail-open branch.
type stubPlanListerAuthor struct {
	existing    []ExistingPlan
	listErr     error
	listCalls   int
	slicedCalls int
	flatCalls   int
	authored    []string // titles passed to AuthorSlicedPlan, in order
}

func (s *stubPlanListerAuthor) AuthorPlan(_ context.Context, _ *store.BacklogItem, _ string) (string, error) {
	s.flatCalls++
	return "plan-flat", nil
}

func (s *stubPlanListerAuthor) AuthorSlicedPlan(_ context.Context, in SlicedPlanInput) (string, error) {
	s.slicedCalls++
	s.authored = append(s.authored, in.Title)
	return planIDForTitle(in.Title), nil
}

func (s *stubPlanListerAuthor) ListExistingPlans(_ context.Context, _, _ string) ([]ExistingPlan, error) {
	s.listCalls++
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.existing, nil
}

// planIDForTitle mirrors the clients-side deterministic id just enough for
// test assertions; the mutator only needs a non-empty id back.
func planIDForTitle(title string) string {
	return "plan-council-" + title
}

func slicedProposal(title string) BacklogProposal {
	return BacklogProposal{
		Title:    title,
		Priority: store.P2,
		PlanSlices: []PlanSliceSpec{
			{Name: "slice one", Goal: "do one", Files: []string{"a.go"}},
		},
	}
}

// TestApply_PlanLane_SkipsNamespaceDuplicate is the core demand-sourcing fix:
// a proposal whose title is a near-duplicate of a Plan already in the namespace
// must NOT be authored again (that's the 36-plan flood). The blocking plan can
// be in any lifecycle phase; ListExistingPlans returns all phases.
func TestApply_PlanLane_SkipsNamespaceDuplicate(t *testing.T) {
	m, st, _ := newMutatorEnv(t)
	author := &stubPlanListerAuthor{existing: []ExistingPlan{
		{ID: "plan-council-ci-failure-classification-runbook", Title: "CI failure classification runbook"},
	}}
	m.PlanAuthor = author
	m.Project = "services/loom-core"
	m.PlanSliceNamespace = "mills/demand-sourcing"

	// Near-dup: same theme with an added qualifier (how the council re-asks).
	// tokens {ci,failure,classification,runbook,automation} vs the existing
	// {ci,failure,classification,runbook} → Jaccard 4/5 = 0.8 ≥ 0.7.
	out := &EditorOutput{BacklogProposals: []BacklogProposal{
		slicedProposal("CI failure classification runbook automation"),
	}}
	res, err := m.Apply(context.Background(), "COUNCIL-P", out, MutationOptions{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if author.listCalls != 1 {
		t.Fatalf("ListExistingPlans calls=%d, want 1 (one snapshot per Apply)", author.listCalls)
	}
	if author.slicedCalls != 0 {
		t.Fatalf("AuthorSlicedPlan calls=%d, want 0 (deduped)", author.slicedCalls)
	}
	if len(res.PlanDuplicatesSkipped) != 1 {
		t.Fatalf("PlanDuplicatesSkipped=%d, want 1", len(res.PlanDuplicatesSkipped))
	}
	if got := res.PlanDuplicatesSkipped[0]; got.SimilarPlanID != "plan-council-ci-failure-classification-runbook" {
		t.Errorf("SimilarPlanID=%q", got.SimilarPlanID)
	}
	if len(res.RoutedPlanLane) != 0 {
		t.Fatalf("RoutedPlanLane=%v, want empty", res.RoutedPlanLane)
	}
	// Deduped plan-lane proposal must NOT fall through to a flat backlog item.
	if len(res.CreatedItems) != 0 {
		t.Fatalf("CreatedItems=%d, want 0", len(res.CreatedItems))
	}
	items, _ := st.Backlog.List(context.Background())
	if len(items) != 0 {
		t.Fatalf("backlog items=%d, want 0", len(items))
	}
}

// TestApply_PlanLane_WithinBatchDedup: two near-duplicate proposals in the same
// Apply, with an empty namespace snapshot, must collapse to a single authored
// Plan — the second dedups against the first authored in-batch.
func TestApply_PlanLane_WithinBatchDedup(t *testing.T) {
	m, _, _ := newMutatorEnv(t)
	author := &stubPlanListerAuthor{} // no existing plans
	m.PlanAuthor = author
	m.Project = "services/loom-core"
	m.PlanSliceNamespace = "mills/demand-sourcing"

	out := &EditorOutput{BacklogProposals: []BacklogProposal{
		slicedProposal("Add autonomy circuit breaker gate"),
		slicedProposal("Add autonomy circuit-breaker gate"), // near-dup
	}}
	res, err := m.Apply(context.Background(), "COUNCIL-D", out, MutationOptions{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if author.slicedCalls != 1 {
		t.Fatalf("AuthorSlicedPlan calls=%d, want 1 (within-batch dedup)", author.slicedCalls)
	}
	if len(res.RoutedPlanLane) != 1 {
		t.Fatalf("RoutedPlanLane=%d, want 1", len(res.RoutedPlanLane))
	}
	if len(res.PlanDuplicatesSkipped) != 1 {
		t.Fatalf("PlanDuplicatesSkipped=%d, want 1", len(res.PlanDuplicatesSkipped))
	}
}

// TestApply_PlanLane_DistinctThemesBothAuthored: dedup must not suppress
// genuinely different themes — both distinct proposals author their own Plan.
func TestApply_PlanLane_DistinctThemesBothAuthored(t *testing.T) {
	m, _, _ := newMutatorEnv(t)
	author := &stubPlanListerAuthor{existing: []ExistingPlan{
		{ID: "plan-x", Title: "CI failure classification runbook"},
	}}
	m.PlanAuthor = author
	m.Project = "services/loom-core"
	m.PlanSliceNamespace = "mills/demand-sourcing"

	out := &EditorOutput{BacklogProposals: []BacklogProposal{
		slicedProposal("Wire Grafana dashboard for spawn pool latency"),
		slicedProposal("Add mobile widget escalate confirmation flow"),
	}}
	res, err := m.Apply(context.Background(), "COUNCIL-N", out, MutationOptions{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if author.slicedCalls != 2 {
		t.Fatalf("AuthorSlicedPlan calls=%d, want 2 (distinct themes)", author.slicedCalls)
	}
	if len(res.PlanDuplicatesSkipped) != 0 {
		t.Fatalf("PlanDuplicatesSkipped=%d, want 0", len(res.PlanDuplicatesSkipped))
	}
	if len(res.RoutedPlanLane) != 2 {
		t.Fatalf("RoutedPlanLane=%d, want 2", len(res.RoutedPlanLane))
	}
}

// TestApply_PlanLane_ListerErrorFailsOpen: a Plan Store read blip must never
// block authoring. On snapshot error the proposal is authored (no dedup) rather
// than dropped.
func TestApply_PlanLane_ListerErrorFailsOpen(t *testing.T) {
	m, _, _ := newMutatorEnv(t)
	author := &stubPlanListerAuthor{listErr: errors.New("hub down")}
	m.PlanAuthor = author
	m.Project = "services/loom-core"
	m.PlanSliceNamespace = "mills/demand-sourcing"

	out := &EditorOutput{BacklogProposals: []BacklogProposal{
		slicedProposal("Add autonomy circuit breaker gate"),
	}}
	res, err := m.Apply(context.Background(), "COUNCIL-E", out, MutationOptions{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if author.slicedCalls != 1 {
		t.Fatalf("AuthorSlicedPlan calls=%d, want 1 (fail-open authors)", author.slicedCalls)
	}
	if len(res.PlanDuplicatesSkipped) != 0 {
		t.Fatalf("PlanDuplicatesSkipped=%d, want 0 on fail-open", len(res.PlanDuplicatesSkipped))
	}
	if len(res.RoutedPlanLane) != 1 {
		t.Fatalf("RoutedPlanLane=%d, want 1", len(res.RoutedPlanLane))
	}
}

// TestApply_PlanLane_NoLister_AuthorsWithoutDedup: a SlicedPlanAuthor that does
// NOT implement PlanLister keeps its prior behavior — authoring proceeds and no
// plan-store dedup runs (nil snapshot).
func TestApply_PlanLane_NoLister_AuthorsWithoutDedup(t *testing.T) {
	m, _, _ := newMutatorEnv(t)
	author := &stubSlicedAuthor{planID: "plan-council-x"} // no ListExistingPlans
	m.PlanAuthor = author
	m.Project = "services/loom-core"
	m.PlanSliceNamespace = "mills/demand-sourcing"

	out := &EditorOutput{BacklogProposals: []BacklogProposal{
		slicedProposal("Add autonomy circuit breaker gate"),
	}}
	res, err := m.Apply(context.Background(), "COUNCIL-A", out, MutationOptions{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if author.slicedCalls != 1 {
		t.Fatalf("AuthorSlicedPlan calls=%d, want 1", author.slicedCalls)
	}
	if len(res.PlanDuplicatesSkipped) != 0 {
		t.Fatalf("PlanDuplicatesSkipped=%d, want 0 (no lister)", len(res.PlanDuplicatesSkipped))
	}
}

// TestFindDuplicatePlan covers the Plan-Store dedup predicate directly: a
// near-duplicate crosses the default 0.7 cutoff, a distinct theme does not, and
// an out-of-range threshold disables the check.
func TestFindDuplicatePlan(t *testing.T) {
	candidates := []ExistingPlan{
		{ID: "p1", Title: "CI failure classification runbook"},
		{ID: "p2", Title: "Spawn pool latency dashboard"},
	}
	// {ci,failure,classification,runbook,automation} vs p1's four tokens →
	// Jaccard 4/5 = 0.8 ≥ 0.7.
	if hit := findDuplicatePlan("CI failure classification runbook automation", candidates, defaultDedupThreshold); hit == nil || hit.plan.ID != "p1" {
		t.Fatalf("near-dup should match p1, got %+v", hit)
	}
	if hit := findDuplicatePlan("Add mobile widget escalate flow", candidates, defaultDedupThreshold); hit != nil {
		t.Fatalf("distinct theme should not match, got %+v", hit)
	}
	if hit := findDuplicatePlan("CI failure classification runbook", candidates, 0); hit != nil {
		t.Fatalf("zero threshold should disable, got %+v", hit)
	}
	if hit := findDuplicatePlan("", candidates, defaultDedupThreshold); hit != nil {
		t.Fatalf("empty title should never match, got %+v", hit)
	}
}
