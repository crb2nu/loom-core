package takeup

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/crb2nu/loom/pkg/mills/clients"
	"github.com/crb2nu/loom/pkg/mills/council"
	"github.com/crb2nu/loom/pkg/mills/intake"
	"github.com/crb2nu/loom/pkg/mills/store"
)

// --- fakes -------------------------------------------------------------

type fakePlanStore struct {
	plans            []clients.PlanSummary
	plansByPhase     map[string][]clients.PlanSummary
	listErrByPhase   map[string]error
	slices           map[string][]clients.PlanSliceSummary // plan_id -> slices
	listCalls        int
	listPhases       []string
	listSlicePlanIDs []string

	phaseUpdates  map[string]string // slice_id -> phase
	decisions     map[string][]string
	planAdvances  map[string][]string // plan_id -> to_phase hops
	advanceErrOn  string              // to_phase that errors
	detailErr     bool
	sliceDetails  map[string]clients.PlanSliceSummary
	detailFetches int
}

func newFakePlanStore() *fakePlanStore {
	return &fakePlanStore{
		plansByPhase:   map[string][]clients.PlanSummary{},
		listErrByPhase: map[string]error{},
		slices:         map[string][]clients.PlanSliceSummary{},
		phaseUpdates:   map[string]string{},
		decisions:      map[string][]string{},
		planAdvances:   map[string][]string{},
		sliceDetails:   map[string]clients.PlanSliceSummary{},
	}
}

func (f *fakePlanStore) ListPlans(_ context.Context, _, _, phase string) ([]clients.PlanSummary, error) {
	f.listCalls++
	f.listPhases = append(f.listPhases, phase)
	if err := f.listErrByPhase[phase]; err != nil {
		return nil, err
	}
	if plans, ok := f.plansByPhase[phase]; ok {
		return plans, nil
	}
	return f.plans, nil
}
func (f *fakePlanStore) ListSlices(_ context.Context, planID string) ([]clients.PlanSliceSummary, error) {
	f.listSlicePlanIDs = append(f.listSlicePlanIDs, planID)
	return f.slices[planID], nil
}
func (f *fakePlanStore) GetSlice(_ context.Context, sliceID string) (clients.PlanSliceSummary, error) {
	f.detailFetches++
	if f.detailErr {
		return clients.PlanSliceSummary{}, fmt.Errorf("detail unavailable")
	}
	if d, ok := f.sliceDetails[sliceID]; ok {
		return d, nil
	}
	return clients.PlanSliceSummary{ID: sliceID}, nil
}
func (f *fakePlanStore) UpdateSlicePhase(_ context.Context, sliceID, phase string) error {
	f.phaseUpdates[sliceID] = phase
	return nil
}
func (f *fakePlanStore) AppendSliceDecision(_ context.Context, sliceID, note string) error {
	f.decisions[sliceID] = append(f.decisions[sliceID], note)
	return nil
}
func (f *fakePlanStore) AdvancePlan(_ context.Context, planID, toPhase, _ string) error {
	if toPhase == f.advanceErrOn {
		return fmt.Errorf("illegal transition")
	}
	f.planAdvances[planID] = append(f.planAdvances[planID], toPhase)
	return nil
}

type fakeMRs struct {
	states map[int64]string // iid -> state
	calls  int
}

func (f *fakeMRs) MRState(_ context.Context, iid int64) (string, error) {
	f.calls++
	if s, ok := f.states[iid]; ok {
		return s, nil
	}
	return "", fmt.Errorf("MR %d not found", iid)
}

type fakeBacklog struct {
	items map[string]*store.BacklogItem
	puts  int
}

func newFakeBacklog() *fakeBacklog {
	return &fakeBacklog{items: map[string]*store.BacklogItem{}}
}

func TestReconciler_GlobalAdmissionBarrier(t *testing.T) {
	plans := newFakePlanStore()
	r := New(plans, &fakeMRs{}, newFakeBacklog(), Config{Project: "services/loom-core", Namespace: "mills"}, nil)
	r.Enabled = func() bool { return false }
	stats := r.runTick(context.Background(), "test")
	if plans.listCalls != 0 || r.ActiveOperations() != 0 || stats != (TickStats{}) {
		t.Fatalf("disabled tick calls=%d active=%d stats=%+v", plans.listCalls, r.ActiveOperations(), stats)
	}
}
func (s *fakeBacklog) Get(_ context.Context, id string) (*store.BacklogItem, error) {
	if it, ok := s.items[id]; ok {
		return it, nil
	}
	return nil, store.ErrNotFound
}
func (s *fakeBacklog) Put(_ context.Context, item *store.BacklogItem) error {
	s.items[item.ID] = item
	s.puts++
	return nil
}

func testReconciler(ps *fakePlanStore, mrs *fakeMRs, bs *fakeBacklog) *Reconciler {
	return New(ps, mrs, bs, Config{
		Project:       "services/loom-core",
		GitLabBaseURL: "https://gitlab.flexinfer.ai/api/v4",
		Namespace:     "mills/eligible",
	}, nil)
}

// --- tests -------------------------------------------------------------

func TestTakeup_ListsOnlyActivePhasesInOrder(t *testing.T) {
	ps := newFakePlanStore()
	_, err := testReconciler(ps, &fakeMRs{}, newFakeBacklog()).Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(ps.listPhases) != len(activePlanPhases) {
		t.Fatalf("ListPlans phases = %v, want %v", ps.listPhases, activePlanPhases)
	}
	for i, want := range activePlanPhases {
		if got := ps.listPhases[i]; got != want {
			t.Fatalf("ListPlans phase %d = %q, want %q; all calls: %v", i, got, want, ps.listPhases)
		}
	}
}

func TestTakeup_TerminalPlansAreNotFetchedOrScanned(t *testing.T) {
	ps := newFakePlanStore()
	// The unfiltered fake response models the production terminal-heavy list.
	// Per-phase responses deliberately contain only the active plan.
	ps.plans = []clients.PlanSummary{{ID: "terminal", Phase: "merged"}}
	ps.plansByPhase["planned"] = []clients.PlanSummary{{ID: "active", Phase: "planned"}}
	ps.plansByPhase["in_progress"] = []clients.PlanSummary{}
	ps.plansByPhase["in_review"] = []clients.PlanSummary{}
	ps.plansByPhase["merging"] = []clients.PlanSummary{}
	ps.slices["active"] = []clients.PlanSliceSummary{}

	stats, err := testReconciler(ps, &fakeMRs{}, newFakeBacklog()).Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if stats.PlansScanned != 1 {
		t.Fatalf("PlansScanned = %d, want 1 active plan", stats.PlansScanned)
	}
	if len(ps.listSlicePlanIDs) != 1 || ps.listSlicePlanIDs[0] != "active" {
		t.Fatalf("ListSlices plans = %v, want only active", ps.listSlicePlanIDs)
	}
}

func TestTakeup_DeduplicatesPlansAcrossPhaseResponses(t *testing.T) {
	ps := newFakePlanStore()
	ps.plansByPhase["planned"] = []clients.PlanSummary{{ID: "moving", Phase: "planned"}}
	ps.plansByPhase["in_progress"] = []clients.PlanSummary{{ID: "moving", Phase: "in_progress"}}
	ps.plansByPhase["in_review"] = []clients.PlanSummary{}
	ps.plansByPhase["merging"] = []clients.PlanSummary{}
	ps.slices["moving"] = []clients.PlanSliceSummary{}

	stats, err := testReconciler(ps, &fakeMRs{}, newFakeBacklog()).Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if stats.PlansScanned != 1 {
		t.Fatalf("PlansScanned = %d, want 1 deduplicated plan", stats.PlansScanned)
	}
	if len(ps.listSlicePlanIDs) != 1 || ps.listSlicePlanIDs[0] != "moving" {
		t.Fatalf("ListSlices plans = %v, want moving once", ps.listSlicePlanIDs)
	}
}

func TestTakeup_ActivePhaseListErrorsFailTick(t *testing.T) {
	for _, phase := range activePlanPhases {
		t.Run(phase, func(t *testing.T) {
			ps := newFakePlanStore()
			ps.listErrByPhase[phase] = fmt.Errorf("hub unavailable")

			stats, err := testReconciler(ps, &fakeMRs{}, newFakeBacklog()).Tick(context.Background())
			if err == nil {
				t.Fatal("Tick returned nil error after active phase list failure")
			}
			want := fmt.Sprintf("list active plans for phase %q: hub unavailable", phase)
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("Tick error = %q, want containing %q", err, want)
			}
			if stats != (TickStats{}) {
				t.Fatalf("stats = %+v, want no complete tick stats", stats)
			}
		})
	}
}

// TestTakeup_MergedMRAdvancesSliceAndClosesItem is the core take-up contract:
// an externally-merged MR advances the slice to merged and marks the emitted
// backlog item merged so the dispatcher never re-runs finished work.
func TestTakeup_MergedMRAdvancesSliceAndClosesItem(t *testing.T) {
	ps := newFakePlanStore()
	ps.plans = []clients.PlanSummary{{ID: "plan-a", Phase: "in_progress", Title: "A"}}
	ps.slices["plan-a"] = []clients.PlanSliceSummary{
		{ID: "plan-a#1", PlanID: "plan-a", Phase: "in_review", MRRef: "!912"},
	}
	mrs := &fakeMRs{states: map[int64]string{912: "merged"}}
	bs := newFakeBacklog()
	itemID := intake.BacklogIDForSlice("plan-a#1")
	bs.items[itemID] = &store.BacklogItem{ID: itemID, State: store.BacklogQueued}

	stats, err := testReconciler(ps, mrs, bs).Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if ps.phaseUpdates["plan-a#1"] != "merged" {
		t.Errorf("slice phase = %q, want merged", ps.phaseUpdates["plan-a#1"])
	}
	if bs.items[itemID].State != store.BacklogMerged {
		t.Errorf("backlog state = %q, want merged", bs.items[itemID].State)
	}
	if stats.SlicesMerged != 1 || stats.ItemsClosed != 1 {
		t.Errorf("stats = %+v, want 1 slice merged + 1 item closed", stats)
	}
	// The single-slice plan is now fully merged → rolled forward to merged.
	wantHops := []string{"in_review", "merging", "merged"}
	if got := ps.planAdvances["plan-a"]; len(got) != len(wantHops) {
		t.Fatalf("plan hops = %v, want %v", got, wantHops)
	}
	if stats.PlansMerged != 1 {
		t.Errorf("PlansMerged = %d, want 1", stats.PlansMerged)
	}
}

func TestTakeup_ProjectBoundMRURLMatchesConfiguredProject(t *testing.T) {
	ps := newFakePlanStore()
	ps.plans = []clients.PlanSummary{{ID: "plan-url", Phase: "in_review"}}
	ps.slices["plan-url"] = []clients.PlanSliceSummary{{
		ID:     "plan-url#1",
		PlanID: "plan-url",
		Phase:  "in_review",
		MRRef:  "https://gitlab.flexinfer.ai/services/loom-core/-/merge_requests/920/diffs",
	}}
	mrs := &fakeMRs{states: map[int64]string{920: "merged"}}

	stats, err := testReconciler(ps, mrs, newFakeBacklog()).Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if mrs.calls != 1 || ps.phaseUpdates["plan-url#1"] != "merged" {
		t.Fatalf("matching project URL calls=%d phase=%q, want one call and merged", mrs.calls, ps.phaseUpdates["plan-url#1"])
	}
	if stats.Errors != 0 {
		t.Fatalf("Errors = %d, want 0", stats.Errors)
	}
}

func TestTakeup_ForeignProjectMRURLFailsClosed(t *testing.T) {
	ps := newFakePlanStore()
	ps.plans = []clients.PlanSummary{{ID: "plan-foreign", Phase: "in_review"}}
	ps.slices["plan-foreign"] = []clients.PlanSliceSummary{{
		ID:     "plan-foreign#1",
		PlanID: "plan-foreign",
		Phase:  "in_review",
		MRRef:  "https://gitlab.flexinfer.ai/services/other/-/merge_requests/920",
	}}
	mrs := &fakeMRs{states: map[int64]string{920: "merged"}}

	stats, err := testReconciler(ps, mrs, newFakeBacklog()).Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if mrs.calls != 0 {
		t.Fatalf("foreign project URL made %d MR calls, want 0", mrs.calls)
	}
	if phase := ps.phaseUpdates["plan-foreign#1"]; phase != "" {
		t.Fatalf("foreign project URL advanced slice to %q", phase)
	}
	if len(ps.planAdvances["plan-foreign"]) != 0 {
		t.Fatalf("foreign project URL advanced plan: %v", ps.planAdvances["plan-foreign"])
	}
	if stats.Errors != 1 {
		t.Fatalf("Errors = %d, want 1", stats.Errors)
	}
}

func TestTakeup_ForeignGitLabHostMRURLFailsClosed(t *testing.T) {
	ps := newFakePlanStore()
	ps.plans = []clients.PlanSummary{{ID: "plan-foreign-host", Phase: "in_review"}}
	ps.slices["plan-foreign-host"] = []clients.PlanSliceSummary{{
		ID:     "plan-foreign-host#1",
		PlanID: "plan-foreign-host",
		Phase:  "in_review",
		MRRef:  "https://other-gitlab.example/services/loom-core/-/merge_requests/920",
	}}
	mrs := &fakeMRs{states: map[int64]string{920: "merged"}}

	stats, err := testReconciler(ps, mrs, newFakeBacklog()).Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if mrs.calls != 0 {
		t.Fatalf("foreign GitLab URL made %d MR calls, want 0", mrs.calls)
	}
	if phase := ps.phaseUpdates["plan-foreign-host#1"]; phase != "" {
		t.Fatalf("foreign GitLab URL advanced slice to %q", phase)
	}
	if stats.Errors != 1 {
		t.Fatalf("Errors = %d, want 1", stats.Errors)
	}
}

func TestTakeup_DoubleEncodedProjectMRURLFailsClosed(t *testing.T) {
	ps := newFakePlanStore()
	ps.plans = []clients.PlanSummary{{ID: "plan-encoded", Phase: "in_review"}}
	ps.slices["plan-encoded"] = []clients.PlanSliceSummary{{
		ID:     "plan-encoded#1",
		PlanID: "plan-encoded",
		Phase:  "in_review",
		MRRef:  "https://gitlab.flexinfer.ai/services%252Floom-core/-/merge_requests/920",
	}}
	mrs := &fakeMRs{states: map[int64]string{920: "merged"}}

	stats, err := testReconciler(ps, mrs, newFakeBacklog()).Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if mrs.calls != 0 {
		t.Fatalf("double-encoded project URL made %d MR calls, want 0", mrs.calls)
	}
	if phase := ps.phaseUpdates["plan-encoded#1"]; phase != "" {
		t.Fatalf("double-encoded project URL advanced slice to %q", phase)
	}
	if stats.Errors != 1 {
		t.Fatalf("Errors = %d, want 1", stats.Errors)
	}
}

func TestTakeup_ProjectIdentityAliasesFailClosed(t *testing.T) {
	for _, project := range []string{"Services/loom-core", "services/loom-core.git"} {
		t.Run(project, func(t *testing.T) {
			ps := newFakePlanStore()
			ps.plans = []clients.PlanSummary{{ID: "plan-alias", Phase: "in_review"}}
			ps.slices["plan-alias"] = []clients.PlanSliceSummary{{
				ID:     "plan-alias#1",
				PlanID: "plan-alias",
				Phase:  "in_review",
				MRRef:  "https://gitlab.flexinfer.ai/" + project + "/-/merge_requests/920",
			}}
			mrs := &fakeMRs{states: map[int64]string{920: "merged"}}

			stats, err := testReconciler(ps, mrs, newFakeBacklog()).Tick(context.Background())
			if err != nil {
				t.Fatalf("tick: %v", err)
			}
			if mrs.calls != 0 || ps.phaseUpdates["plan-alias#1"] != "" || stats.Errors != 1 {
				t.Fatalf("project alias authorized: calls=%d phase=%q stats=%+v", mrs.calls, ps.phaseUpdates["plan-alias#1"], stats)
			}
		})
	}
}

// TestTakeup_RunningItemUntouched: the pipeline owns a running item — take-up
// must not race its state transitions even when the MR merged.
func TestTakeup_RunningItemUntouched(t *testing.T) {
	ps := newFakePlanStore()
	ps.plans = []clients.PlanSummary{{ID: "plan-r", Phase: "in_progress"}}
	ps.slices["plan-r"] = []clients.PlanSliceSummary{
		{ID: "plan-r#1", PlanID: "plan-r", Phase: "implementing", MRRef: "913"},
	}
	mrs := &fakeMRs{states: map[int64]string{913: "merged"}}
	bs := newFakeBacklog()
	itemID := intake.BacklogIDForSlice("plan-r#1")
	bs.items[itemID] = &store.BacklogItem{ID: itemID, State: store.BacklogRunning}

	stats, _ := testReconciler(ps, mrs, bs).Tick(context.Background())
	if bs.items[itemID].State != store.BacklogRunning {
		t.Errorf("running item state changed to %q", bs.items[itemID].State)
	}
	if bs.puts != 0 {
		t.Errorf("running item was Put %d times, want 0", bs.puts)
	}
	// The slice itself still trues to merged (MR reality).
	if ps.phaseUpdates["plan-r#1"] != "merged" {
		t.Errorf("slice phase = %q, want merged", ps.phaseUpdates["plan-r#1"])
	}
	if stats.ItemsClosed != 0 {
		t.Errorf("ItemsClosed = %d, want 0", stats.ItemsClosed)
	}
}

// TestTakeup_EscalatedItemResolvedByExternalMerge: an escalated item whose MR
// merged externally is resolved by reality (unblocks dedupe guards).
func TestTakeup_EscalatedItemResolvedByExternalMerge(t *testing.T) {
	ps := newFakePlanStore()
	ps.plans = []clients.PlanSummary{{ID: "plan-e", Phase: "in_review"}}
	ps.slices["plan-e"] = []clients.PlanSliceSummary{
		{ID: "plan-e#1", PlanID: "plan-e", Phase: "in_review", MRRef: "!914"},
	}
	mrs := &fakeMRs{states: map[int64]string{914: "merged"}}
	bs := newFakeBacklog()
	itemID := intake.BacklogIDForSlice("plan-e#1")
	bs.items[itemID] = &store.BacklogItem{ID: itemID, State: store.BacklogEscalated}

	testReconciler(ps, mrs, bs).Tick(context.Background()) //nolint:errcheck
	if bs.items[itemID].State != store.BacklogMerged {
		t.Errorf("escalated item state = %q, want merged", bs.items[itemID].State)
	}
}

// TestTakeup_ClosedMRFlagsOrphanOnce: a closed-without-merge MR appends ONE
// decision note — re-ticks must not spam it (dedupe via slice detail).
func TestTakeup_ClosedMRFlagsOrphanOnce(t *testing.T) {
	ps := newFakePlanStore()
	ps.plans = []clients.PlanSummary{{ID: "plan-o", Phase: "in_progress"}}
	ps.slices["plan-o"] = []clients.PlanSliceSummary{
		{ID: "plan-o#1", PlanID: "plan-o", Phase: "in_review", MRRef: "!915"},
	}
	mrs := &fakeMRs{states: map[int64]string{915: "closed"}}
	bs := newFakeBacklog()
	r := testReconciler(ps, mrs, bs)

	stats, _ := r.Tick(context.Background())
	if stats.OrphansFlagged != 1 || len(ps.decisions["plan-o#1"]) != 1 {
		t.Fatalf("first tick: flagged=%d decisions=%v", stats.OrphansFlagged, ps.decisions["plan-o#1"])
	}
	// Simulate the appended note now visible in the detail view.
	ps.sliceDetails["plan-o#1"] = clients.PlanSliceSummary{
		ID: "plan-o#1", Decisions: ps.decisions["plan-o#1"],
	}
	stats2, _ := r.Tick(context.Background())
	if stats2.OrphansFlagged != 0 || len(ps.decisions["plan-o#1"]) != 1 {
		t.Fatalf("second tick re-flagged: flagged=%d decisions=%v", stats2.OrphansFlagged, ps.decisions["plan-o#1"])
	}
	// Plan must NOT roll forward — the closed slice is not merged.
	if len(ps.planAdvances["plan-o"]) != 0 {
		t.Errorf("plan advanced despite orphaned slice: %v", ps.planAdvances["plan-o"])
	}
}

// TestTakeup_OrphanDetailFetchFailureSkipsFlag: on a detail failure the flag
// is skipped (fail-safe against append-spam), not appended blindly.
func TestTakeup_OrphanDetailFetchFailureSkipsFlag(t *testing.T) {
	ps := newFakePlanStore()
	ps.detailErr = true
	ps.plans = []clients.PlanSummary{{ID: "plan-o2", Phase: "in_progress"}}
	ps.slices["plan-o2"] = []clients.PlanSliceSummary{
		{ID: "plan-o2#1", PlanID: "plan-o2", Phase: "in_review", MRRef: "!916"},
	}
	mrs := &fakeMRs{states: map[int64]string{916: "closed"}}
	stats, _ := testReconciler(ps, mrs, newFakeBacklog()).Tick(context.Background())
	if len(ps.decisions["plan-o2#1"]) != 0 || stats.OrphansFlagged != 0 {
		t.Errorf("flag appended despite detail failure: %v", ps.decisions["plan-o2#1"])
	}
	if stats.Errors == 0 {
		t.Error("detail failure must count as an error")
	}
}

// TestTakeup_OpenMRAndMissingRefHoldThePlan: open MRs and ref-less slices
// keep the plan where it is; already-merged slices are skipped without an MR
// query.
func TestTakeup_OpenMRAndMissingRefHoldThePlan(t *testing.T) {
	ps := newFakePlanStore()
	ps.plans = []clients.PlanSummary{{ID: "plan-m", Phase: "in_progress"}}
	ps.slices["plan-m"] = []clients.PlanSliceSummary{
		{ID: "plan-m#1", PlanID: "plan-m", Phase: "merged", MRRef: "!801"}, // already merged: no query
		{ID: "plan-m#2", PlanID: "plan-m", Phase: "in_review", MRRef: "!802"},
		{ID: "plan-m#3", PlanID: "plan-m", Phase: "pending"}, // no MR yet
	}
	mrs := &fakeMRs{states: map[int64]string{802: "opened"}}
	stats, _ := testReconciler(ps, mrs, newFakeBacklog()).Tick(context.Background())
	if mrs.calls != 1 {
		t.Errorf("MR queries = %d, want 1 (merged + ref-less slices skip)", mrs.calls)
	}
	if len(ps.planAdvances["plan-m"]) != 0 {
		t.Errorf("plan advanced with in-flight slices: %v", ps.planAdvances["plan-m"])
	}
	if stats.SlicesMerged != 0 {
		t.Errorf("SlicesMerged = %d, want 0", stats.SlicesMerged)
	}
}

// TestTakeup_InactiveAndSlicelessPlansSkipped: draft/done plans and plans
// with zero slices are never touched.
func TestTakeup_InactiveAndSlicelessPlansSkipped(t *testing.T) {
	ps := newFakePlanStore()
	ps.plans = []clients.PlanSummary{
		{ID: "plan-d", Phase: "draft"},
		{ID: "plan-x", Phase: "done"},
		{ID: "plan-s", Phase: "in_progress"}, // no slices
	}
	mrs := &fakeMRs{states: map[int64]string{}}
	stats, _ := testReconciler(ps, mrs, newFakeBacklog()).Tick(context.Background())
	if stats.PlansScanned != 1 {
		t.Errorf("PlansScanned = %d, want 1 (only the active plan)", stats.PlansScanned)
	}
	if mrs.calls != 0 || len(ps.planAdvances) != 0 {
		t.Errorf("inactive/sliceless plans caused work: calls=%d advances=%v", mrs.calls, ps.planAdvances)
	}
}

// TestTakeup_FailClosedWithoutNamespace mirrors the emitter's namespace gate.
func TestTakeup_FailClosedWithoutNamespace(t *testing.T) {
	ps := newFakePlanStore()
	ps.plans = []clients.PlanSummary{{ID: "plan-a", Phase: "in_progress"}}
	r := New(ps, &fakeMRs{}, newFakeBacklog(), Config{Project: "p"}, nil)
	stats, err := r.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if stats.PlansScanned != 0 {
		t.Errorf("scanned %d plans without a namespace gate, want 0", stats.PlansScanned)
	}
}

// TestTakeup_AdvanceStopsOnRejectedHop: a store-rejected hop (concurrent
// transition) stops the walk without counting the plan as merged.
func TestTakeup_AdvanceStopsOnRejectedHop(t *testing.T) {
	ps := newFakePlanStore()
	ps.advanceErrOn = "merging"
	ps.plans = []clients.PlanSummary{{ID: "plan-c", Phase: "in_review"}}
	ps.slices["plan-c"] = []clients.PlanSliceSummary{
		{ID: "plan-c#1", PlanID: "plan-c", Phase: "merged", MRRef: "!917"},
	}
	stats, _ := testReconciler(ps, &fakeMRs{}, newFakeBacklog()).Tick(context.Background())
	if stats.PlansMerged != 0 {
		t.Errorf("PlansMerged = %d, want 0 (hop rejected)", stats.PlansMerged)
	}
	if stats.Errors == 0 {
		t.Error("rejected hop must count as an error")
	}
}

func TestParseMRIID(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		ok   bool
	}{
		{"!912", 912, true},
		{"912", 912, true},
		{"https://gitlab.flexinfer.ai/services/loom-core/-/merge_requests/912", 912, true},
		{"https://gitlab.flexinfer.ai/services/loom-core/-/merge_requests/912/diffs", 912, true},
		{"https://gitlab.flexinfer.ai/services/loom-core/-/merge_requests/912?tab=1", 912, true},
		{"912 (rebased)", 912, true},
		{"", 0, false},
		{"main", 0, false},
		{"!0", 0, false},
		{"https://gitlab.flexinfer.ai/services/loom-core/-/issues/44", 0, false},
	}
	for _, c := range cases {
		got, ok := ParseMRIID(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("ParseMRIID(%q) = (%d,%v), want (%d,%v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

// --- J2 pattern auto-harvest -------------------------------------------

type recordedInstance struct {
	patternID, mrRef, repo string
}

type fakeHarvester struct {
	patterns []council.PatternRef
	listErr  error
	recErr   error
	recorded []recordedInstance
}

func (f *fakeHarvester) ListApprovedPatterns(_ context.Context) ([]council.PatternRef, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.patterns, nil
}

func (f *fakeHarvester) RecordInstance(_ context.Context, patternID, mrRef, repo string) (clients.PatternHarvest, error) {
	if f.recErr != nil {
		return clients.PatternHarvest{}, f.recErr
	}
	f.recorded = append(f.recorded, recordedInstance{patternID, mrRef, repo})
	return clients.PatternHarvest{InstancesShippedGreen: 2, Status: "approved", Promoted: true}, nil
}

// mergedStampFixture returns a store whose single stamped plan fully merges on
// the next tick (the harvest's one-shot observation point).
func mergedStampFixture(planID string) (*fakePlanStore, *fakeMRs, *fakeBacklog) {
	ps := newFakePlanStore()
	ps.plans = []clients.PlanSummary{{ID: planID, Phase: "merging", Title: "stamped"}}
	ps.slices[planID] = []clients.PlanSliceSummary{
		{ID: planID + "#1", PlanID: planID, Phase: "in_review", MRRef: "!874"},
	}
	return ps, &fakeMRs{states: map[int64]string{874: "merged"}}, newFakeBacklog()
}

func TestTakeup_MergedStampHarvestsPattern_LongestSlugWins(t *testing.T) {
	ps, mrs, bs := mergedStampFixture("plan-stamp-go-rest-service-sprocket-1782949652")
	h := &fakeHarvester{patterns: []council.PatternRef{
		{ID: "pattern-go-rest", Slug: "go-rest"},
		{ID: "pattern-go-rest-service", Slug: "go-rest-service"},
	}}
	r := testReconciler(ps, mrs, bs)
	r.Patterns = h

	stats, err := r.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(h.recorded) != 1 {
		t.Fatalf("recorded = %v, want exactly one harvest", h.recorded)
	}
	got := h.recorded[0]
	if got.patternID != "pattern-go-rest-service" {
		t.Errorf("pattern = %q, want longest-slug match pattern-go-rest-service", got.patternID)
	}
	if got.mrRef != "!874" {
		t.Errorf("mr_ref = %q, want !874", got.mrRef)
	}
	if got.repo != "loom-core" {
		t.Errorf("repo = %q, want loom-core (RepoBase of the configured project)", got.repo)
	}
	if stats.PatternsHarvested != 1 || stats.Errors != 0 {
		t.Errorf("stats = %+v, want 1 harvest and no errors", stats)
	}
}

func TestTakeup_NonStampPlanNeverTouchesTheCatalog(t *testing.T) {
	ps, mrs, bs := mergedStampFixture("plan-live-beam-355257")
	h := &fakeHarvester{patterns: []council.PatternRef{{ID: "pattern-go-rest-service", Slug: "go-rest-service"}}}
	r := testReconciler(ps, mrs, bs)
	r.Patterns = h

	stats, err := r.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(h.recorded) != 0 {
		t.Errorf("non-stamp plan harvested: %v", h.recorded)
	}
	if stats.PlansMerged != 1 || stats.PatternsHarvested != 0 {
		t.Errorf("stats = %+v, want merged plan without harvest", stats)
	}
}

func TestTakeup_UnmatchedStampSlugAttributesToNothing(t *testing.T) {
	ps, mrs, bs := mergedStampFixture("plan-stamp-python-fastapi-service-widget-1")
	h := &fakeHarvester{patterns: []council.PatternRef{{ID: "pattern-go-rest-service", Slug: "go-rest-service"}}}
	r := testReconciler(ps, mrs, bs)
	r.Patterns = h

	stats, err := r.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(h.recorded) != 0 {
		t.Errorf("unmatched slug harvested: %v", h.recorded)
	}
	// Deliberately NOT an error: deprecation/rename between stamp and merge.
	if stats.Errors != 0 || stats.PatternsHarvested != 0 {
		t.Errorf("stats = %+v, want no errors and no harvest", stats)
	}
}

func TestTakeup_HarvestFailuresNeverDisturbTheMergedPlan(t *testing.T) {
	for name, h := range map[string]*fakeHarvester{
		"catalog fetch fails":   {listErr: fmt.Errorf("hub down")},
		"record instance fails": {patterns: []council.PatternRef{{ID: "pattern-go-rest-service", Slug: "go-rest-service"}}, recErr: fmt.Errorf("store rejected")},
	} {
		ps, mrs, bs := mergedStampFixture("plan-stamp-go-rest-service-widget-1")
		r := testReconciler(ps, mrs, bs)
		r.Patterns = h
		stats, err := r.Tick(context.Background())
		if err != nil {
			t.Fatalf("%s: tick: %v", name, err)
		}
		if stats.PlansMerged != 1 {
			t.Errorf("%s: plan advance disturbed: %+v", name, stats)
		}
		if stats.PatternsHarvested != 0 || stats.Errors != 1 {
			t.Errorf("%s: stats = %+v, want 0 harvests and 1 error", name, stats)
		}
	}
}

func TestTakeup_NilHarvesterIsPreJ2Behavior(t *testing.T) {
	ps, mrs, bs := mergedStampFixture("plan-stamp-go-rest-service-widget-1")
	stats, err := testReconciler(ps, mrs, bs).Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if stats.PlansMerged != 1 || stats.PatternsHarvested != 0 || stats.Errors != 0 {
		t.Errorf("stats = %+v, want plain merged plan", stats)
	}
}

func TestMatchStampedPattern(t *testing.T) {
	patterns := []council.PatternRef{
		{ID: "pattern-go-rest", Slug: "go-rest"},
		{ID: "pattern-go-rest-service", Slug: "go-rest-service"},
		{ID: "pattern-slugless"},
	}
	cases := map[string]string{
		"plan-stamp-go-rest-service-widget-1": "pattern-go-rest-service",
		"plan-stamp-go-rest-service":          "pattern-go-rest-service",
		"plan-stamp-go-rest-widget":           "pattern-go-rest",
		"plan-stamp-go-restless-widget":       "", // prefix must break on '-'
		"plan-stamp-unknown-thing":            "",
	}
	for planID, want := range cases {
		if got := matchStampedPattern(planID, patterns); got != want {
			t.Errorf("matchStampedPattern(%q) = %q, want %q", planID, got, want)
		}
	}
}

func TestMergedMRRefs_JoinsAndCaps(t *testing.T) {
	slices := []clients.PlanSliceSummary{
		{MRRef: "!1"}, {MRRef: ""}, {MRRef: "!2"}, {MRRef: "!3"}, {MRRef: "!4"},
	}
	if got := mergedMRRefs(slices); got != "!1, !2, !3" {
		t.Errorf("mergedMRRefs = %q, want capped join", got)
	}
	if got := mergedMRRefs(nil); got != "" {
		t.Errorf("mergedMRRefs(nil) = %q, want empty", got)
	}
}
