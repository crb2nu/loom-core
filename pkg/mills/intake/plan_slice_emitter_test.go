package intake

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/clients"
	"github.com/crb2nu/loom/pkg/mills/store"
)

// fakePlanReader returns canned plan/slice lists. details supplies the per-slice
// `files` the LIST view omits (the live agent_plan_slice_list / _get split).
type fakePlanReader struct {
	plans     []clients.PlanSummary
	slices    map[string][]clients.PlanSliceSummary
	details   map[string]clients.PlanSliceSummary
	calls     int
	sliceGets int
	// plansByProject, when non-nil, returns plans keyed by the project arg so a
	// multi-repo test can hand the emitter distinct demand per repo. nil falls
	// back to plans (single-repo tests unchanged).
	plansByProject map[string][]clients.PlanSummary
	// plansByPhase overrides the usual phase filtering for tests that model a
	// malformed reader returning one plan from multiple lifecycle reads.
	plansByPhase map[string][]clients.PlanSummary
	// perProjectCalls counts ListPlans calls keyed by project, so a dedup test
	// can assert a repo in two demand sources is scanned once.
	perProjectCalls map[string]int
	planCalls       []planReadCall
	planErrors      map[planReadCall]error
	sliceCalls      map[string]int
}

type blockingPlanReader struct{}

func (blockingPlanReader) ListPlans(ctx context.Context, _, _, _ string) ([]clients.PlanSummary, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (blockingPlanReader) ListSlices(context.Context, string) ([]clients.PlanSliceSummary, error) {
	return nil, errors.New("unexpected ListSlices call")
}

func (blockingPlanReader) GetSlice(context.Context, string) (clients.PlanSliceSummary, error) {
	return clients.PlanSliceSummary{}, errors.New("unexpected GetSlice call")
}

type planReadCall struct {
	project string
	phase   string
}

func (f *fakePlanReader) ListPlans(_ context.Context, project, _, phase string) ([]clients.PlanSummary, error) {
	f.calls++
	call := planReadCall{project: project, phase: phase}
	f.planCalls = append(f.planCalls, call)
	if err := f.planErrors[call]; err != nil {
		return nil, err
	}
	if f.perProjectCalls == nil {
		f.perProjectCalls = map[string]int{}
	}
	f.perProjectCalls[project]++
	if f.plansByPhase != nil {
		return f.plansByPhase[phase], nil
	}
	if f.plansByProject != nil {
		return plansInPhase(f.plansByProject[project], phase), nil
	}
	return plansInPhase(f.plans, phase), nil
}

func plansInPhase(plans []clients.PlanSummary, phase string) []clients.PlanSummary {
	var filtered []clients.PlanSummary
	for _, plan := range plans {
		if plan.Phase == phase {
			filtered = append(filtered, plan)
		}
	}
	return filtered
}

// plansByProjectCalls reports how many times ListPlans was called for project.
func (f *fakePlanReader) plansByProjectCalls(project string) int {
	return f.perProjectCalls[project]
}

func (f *fakePlanReader) ListSlices(_ context.Context, planID string) ([]clients.PlanSliceSummary, error) {
	if f.sliceCalls == nil {
		f.sliceCalls = map[string]int{}
	}
	f.sliceCalls[planID]++
	return f.slices[planID], nil
}

func (f *fakePlanReader) GetSlice(_ context.Context, sliceID string) (clients.PlanSliceSummary, error) {
	f.sliceGets++
	if f.details != nil {
		if d, ok := f.details[sliceID]; ok {
			return d, nil
		}
	}
	return clients.PlanSliceSummary{}, nil
}

// fakeBacklogStore is an in-memory BacklogStore (Get + Put).
type fakeBacklogStore struct {
	items map[string]*store.BacklogItem
	puts  int
}

func newFakeStore() *fakeBacklogStore {
	return &fakeBacklogStore{items: map[string]*store.BacklogItem{}}
}

func (s *fakeBacklogStore) Get(_ context.Context, id string) (*store.BacklogItem, error) {
	if it, ok := s.items[id]; ok {
		return it, nil
	}
	return nil, store.ErrNotFound
}

func (s *fakeBacklogStore) Put(_ context.Context, item *store.BacklogItem) error {
	s.items[item.ID] = item
	s.puts++
	return nil
}

func emitterFixture() *fakePlanReader {
	return &fakePlanReader{
		plans: []clients.PlanSummary{
			{ID: "plan-ready", Project: "services/loom-core", Phase: "in_progress", Title: "Ready plan"},
			{ID: "plan-draft", Project: "services/loom-core", Phase: "draft", Title: "Draft plan"},
		},
		slices: map[string][]clients.PlanSliceSummary{
			"plan-ready": {
				{ID: "plan-ready#1", PlanID: "plan-ready", Name: "do thing", Phase: "pending", Goal: "make it", AcceptanceCriteria: "tests pass", Files: []string{"pkg/foo/bar.go"}},
				{ID: "plan-ready#2", PlanID: "plan-ready", Name: "done thing", Phase: "merged", Goal: "x", AcceptanceCriteria: "y"},
			},
			// A draft plan's pending slice must NOT emit (plan phase gate).
			"plan-draft": {
				{ID: "plan-draft#1", PlanID: "plan-draft", Name: "scaffold", Phase: "pending"},
			},
		},
	}
}

func TestPlanSliceEmitter_GlobalAdmissionBarrier(t *testing.T) {
	plans := emitterFixture()
	emitter := NewPlanSliceEmitter(plans, newFakeStore(), PlanSliceEmitterConfig{Project: "services/loom-core", Namespace: "mills"}, nil)
	emitter.Enabled = func() bool { return false }
	if n, err := emitter.Tick(context.Background()); err != nil || n != 0 || plans.calls != 0 || emitter.ActiveOperations() != 0 {
		t.Fatalf("disabled Tick() n=%d calls=%d active=%d err=%v", n, plans.calls, emitter.ActiveOperations(), err)
	}
}

func TestPlanSliceEmitter_RunTickBoundsStalledPlanRead(t *testing.T) {
	emitter := NewPlanSliceEmitter(blockingPlanReader{}, newFakeStore(), PlanSliceEmitterConfig{
		Project:      "services/loom-core",
		Namespace:    "mills",
		TickTimeout:  20 * time.Millisecond,
		PollInterval: time.Hour,
	}, nil)
	started := time.Now()
	if _, err := emitter.runTick(context.Background(), "test"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("runTick error = %v, want context deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("bounded tick took %v, want <500ms", elapsed)
	}
	if emitter.ActiveOperations() != 0 {
		t.Fatalf("active operations = %d after timeout, want 0", emitter.ActiveOperations())
	}
}

func testEmitter(pr PlanReader, st BacklogStore) *PlanSliceEmitter {
	return NewPlanSliceEmitter(pr, st, PlanSliceEmitterConfig{
		Project:   "services/loom-core",
		Namespace: "mills/eligible",
	}, nil)
}

func TestPlanSliceEmitter_EmitsReadyPendingSlice(t *testing.T) {
	pr, st := emitterFixture(), newFakeStore()
	n, err := testEmitter(pr, st).Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	// Only plan-ready#1 qualifies: plan-ready#2 is merged, plan-draft#1's plan is draft.
	if n != 1 {
		t.Fatalf("emitted=%d, want 1", n)
	}
	item, err := st.Get(context.Background(), planSliceBacklogID("plan-ready#1"))
	if err != nil || item == nil {
		t.Fatalf("expected emitted item, got err=%v", err)
	}
	if item.PlanID != "plan-ready" {
		t.Errorf("PlanID=%q, want plan-ready", item.PlanID)
	}
	if item.State != store.BacklogQueued {
		t.Errorf("State=%q, want queued", item.State)
	}
	if item.Priority != store.P2 {
		t.Errorf("Priority=%q, want P2 (default)", item.Priority)
	}
	if item.Title != "Ready plan — do thing" {
		t.Errorf("Title=%q", item.Title)
	}
	if len(item.Labels) != 1 || item.Labels[0] != planSliceDefaultLabel {
		t.Errorf("Labels=%v, want [%s]", item.Labels, planSliceDefaultLabel)
	}
	if item.CreatedBy != planSliceCreatedBy {
		t.Errorf("CreatedBy=%q", item.CreatedBy)
	}
	if want := "make it\n\n## Acceptance criteria\ntests pass"; item.SpecDoc != want {
		t.Errorf("SpecDoc=%q, want %q", item.SpecDoc, want)
	}
}

func TestPlanSliceEmitter_ListsOnlyEligiblePhases(t *testing.T) {
	pr := &fakePlanReader{
		plans: []clients.PlanSummary{
			{ID: "plan-planned", Phase: "planned", Title: "Planned"},
			{ID: "plan-progress", Phase: "in_progress", Title: "In progress"},
			{ID: "plan-done", Phase: "done", Title: "Terminal"},
		},
		slices: map[string][]clients.PlanSliceSummary{
			"plan-planned":  {{ID: "plan-planned#1", PlanID: "plan-planned", Name: "planned", Phase: "pending", Files: []string{"planned.go"}}},
			"plan-progress": {{ID: "plan-progress#1", PlanID: "plan-progress", Name: "progress", Phase: "pending", Files: []string{"progress.go"}}},
			"plan-done":     {{ID: "plan-done#1", PlanID: "plan-done", Name: "terminal", Phase: "pending", Files: []string{"done.go"}}},
		},
	}
	n, err := testEmitter(pr, newFakeStore()).Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if n != 2 {
		t.Fatalf("emitted=%d, want 2 eligible plans", n)
	}
	wantCalls := []planReadCall{
		{project: "services/loom-core", phase: "planned"},
		{project: "services/loom-core", phase: "in_progress"},
	}
	if len(pr.planCalls) != len(wantCalls) {
		t.Fatalf("ListPlans calls=%v, want %v", pr.planCalls, wantCalls)
	}
	for i, want := range wantCalls {
		if got := pr.planCalls[i]; got != want {
			t.Errorf("ListPlans call %d=%v, want %v", i, got, want)
		}
	}
	if got := pr.sliceCalls["plan-done"]; got != 0 {
		t.Errorf("terminal plan ListSlices calls=%d, want 0", got)
	}
}

func TestPlanSliceEmitter_DedupesPlanAcrossPhaseResponses(t *testing.T) {
	pr := &fakePlanReader{
		plansByPhase: map[string][]clients.PlanSummary{
			"planned":     {{ID: "plan-duplicate", Phase: "planned", Title: "First"}},
			"in_progress": {{ID: "plan-duplicate", Phase: "in_progress", Title: "Second"}},
		},
		slices: map[string][]clients.PlanSliceSummary{
			"plan-duplicate": {{ID: "plan-duplicate#1", PlanID: "plan-duplicate", Name: "slice", Phase: "pending", Files: []string{"pkg/a.go"}}},
		},
	}
	st := newFakeStore()
	n, err := testEmitter(pr, st).Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if n != 1 || st.puts != 1 {
		t.Fatalf("emitted=%d puts=%d, want 1/1", n, st.puts)
	}
	if got := pr.sliceCalls["plan-duplicate"]; got != 1 {
		t.Errorf("duplicate plan ListSlices calls=%d, want 1", got)
	}
}

func TestPlanSliceEmitter_HomePhaseErrorStopsExternalDemand(t *testing.T) {
	homeErr := errors.New("home plan read failed")
	pr := &fakePlanReader{
		plansByPhase: map[string][]clients.PlanSummary{
			"planned": {{ID: "plan-ready", Phase: "planned", Title: "Ready"}},
		},
		planErrors: map[planReadCall]error{
			{project: "services/loom-core", phase: "in_progress"}: homeErr,
		},
		slices: map[string][]clients.PlanSliceSummary{
			"plan-ready": {{ID: "plan-ready#1", PlanID: "plan-ready", Name: "ready", Phase: "pending", Files: []string{"ready.go"}}},
		},
	}
	st := newFakeStore()
	e := NewPlanSliceEmitter(pr, st, PlanSliceEmitterConfig{
		Project:        "services/loom-core",
		Namespace:      "mills/eligible",
		DemandProjects: []string{"services/flexdeck"},
	}, nil)
	if n, err := e.Tick(context.Background()); !errors.Is(err, homeErr) || n != 0 {
		t.Fatalf("tick n=%d err=%v, want hard home error", n, err)
	}
	want := []planReadCall{
		{project: "services/loom-core", phase: "planned"},
		{project: "services/loom-core", phase: "in_progress"},
	}
	if got := pr.planCalls; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("ListPlans calls=%v, want %v (no external scan)", got, want)
	}
	if st.puts != 0 {
		t.Errorf("backlog puts=%d, want 0 when a later home phase read fails", st.puts)
	}
}

// TestPlanSliceEmitter_MultiRepoDemandStampsTargetProject pins S6: with a
// DemandProjects allowlist the emitter ALSO sources each foreign repo's ready
// slices and stamps the emitted item's TargetProject with the foreign path,
// while home items stay target-less (single-repo). The foreign item routes
// cross-repo; the reconciler's CrossRepoPolicy.Enabled gate still guards
// execution.
func TestPlanSliceEmitter_MultiRepoDemandStampsTargetProject(t *testing.T) {
	pr := &fakePlanReader{
		plansByProject: map[string][]clients.PlanSummary{
			"services/loom-core": {
				{ID: "plan-home", Project: "services/loom-core", Phase: "in_progress", Title: "Home"},
			},
			"services/flexdeck": {
				{ID: "plan-fd", Project: "services/flexdeck", Phase: "in_progress", Title: "Flexdeck"},
			},
		},
		slices: map[string][]clients.PlanSliceSummary{
			"plan-home": {{ID: "plan-home#1", PlanID: "plan-home", Name: "home slice", Phase: "pending", Goal: "g", AcceptanceCriteria: "a", Files: []string{"pkg/a/a.go"}}},
			"plan-fd":   {{ID: "plan-fd#1", PlanID: "plan-fd", Name: "fd slice", Phase: "pending", Goal: "g", AcceptanceCriteria: "a", Files: []string{"README.md"}}},
		},
	}
	st := newFakeStore()
	e := NewPlanSliceEmitter(pr, st, PlanSliceEmitterConfig{
		Project:        "services/loom-core",
		Namespace:      "mills/eligible",
		DemandProjects: []string{"services/flexdeck"},
	}, nil)
	n, err := e.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if n != 2 {
		t.Fatalf("emitted=%d, want 2 (home + flexdeck)", n)
	}
	home, err := st.Get(context.Background(), planSliceBacklogID("plan-home#1"))
	if err != nil || home == nil {
		t.Fatalf("expected home item: %v", err)
	}
	if home.TargetProject != "" {
		t.Errorf("home TargetProject=%q, want empty (home repo)", home.TargetProject)
	}
	fd, err := st.Get(context.Background(), planSliceBacklogID("plan-fd#1"))
	if err != nil || fd == nil {
		t.Fatalf("expected flexdeck item: %v", err)
	}
	if fd.TargetProject != "services/flexdeck" {
		t.Errorf("flexdeck TargetProject=%q, want services/flexdeck", fd.TargetProject)
	}
}

// TestPlanSliceEmitter_DynamicDemandUnionsAndDedups pins the bootstrap demand
// path: a runtime-minted repo supplied via DynamicDemandProjects is sourced
// alongside the static allowlist, and a project appearing in BOTH is scanned
// once (no double-emit). It is resolved every tick, so a repo minted after the
// emitter started still dispatches.
func TestPlanSliceEmitter_DynamicDemandUnionsAndDedups(t *testing.T) {
	pr := &fakePlanReader{
		plansByProject: map[string][]clients.PlanSummary{
			"services/loom-core": {
				{ID: "plan-home", Project: "services/loom-core", Phase: "in_progress", Title: "Home"},
			},
			"services/flexdeck": {
				{ID: "plan-fd", Project: "services/flexdeck", Phase: "in_progress", Title: "Flexdeck"},
			},
			"services/procmodel": {
				{ID: "plan-pm", Project: "services/procmodel", Phase: "in_progress", Title: "Procmodel"},
			},
		},
		slices: map[string][]clients.PlanSliceSummary{
			"plan-home": {{ID: "plan-home#1", PlanID: "plan-home", Name: "home slice", Phase: "pending", Goal: "g", AcceptanceCriteria: "a", Files: []string{"pkg/a/a.go"}}},
			"plan-fd":   {{ID: "plan-fd#1", PlanID: "plan-fd", Name: "fd slice", Phase: "pending", Goal: "g", AcceptanceCriteria: "a", Files: []string{"README.md"}}},
			"plan-pm":   {{ID: "plan-pm#1", PlanID: "plan-pm", Name: "pm slice", Phase: "pending", Goal: "g", AcceptanceCriteria: "a", Files: []string{"main.go"}}},
		},
	}
	st := newFakeStore()
	e := NewPlanSliceEmitter(pr, st, PlanSliceEmitterConfig{
		Project:   "services/loom-core",
		Namespace: "mills/eligible",
		// flexdeck is in BOTH the static allowlist and the dynamic source;
		// procmodel is bootstrapped-only.
		DemandProjects: []string{"services/flexdeck"},
		DynamicDemandProjects: func(context.Context) []string {
			return []string{"services/flexdeck", "services/procmodel"}
		},
	}, nil)
	n, err := e.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if n != 3 {
		t.Fatalf("emitted=%d, want 3 (home + flexdeck + procmodel, flexdeck deduped)", n)
	}
	pm, err := st.Get(context.Background(), planSliceBacklogID("plan-pm#1"))
	if err != nil || pm == nil {
		t.Fatalf("expected procmodel item: %v", err)
	}
	if pm.TargetProject != "services/procmodel" {
		t.Errorf("procmodel TargetProject=%q, want services/procmodel", pm.TargetProject)
	}
	// flexdeck was listed twice across the two demand sources but scanned once:
	// each scan makes one bounded read for each eligible lifecycle phase.
	if got := pr.plansByProjectCalls("services/flexdeck"); got != len(emitterReadyPlanPhases) {
		t.Errorf("flexdeck ListPlans calls=%d, want %d (deduped union)", got, len(emitterReadyPlanPhases))
	}
}

// TestPlanSliceEmitter_NilDynamicDemandIsHomeAndStaticOnly pins that a nil
// dynamic provider is a no-op: only the static allowlist + home are sourced.
func TestPlanSliceEmitter_NilDynamicDemandIsHomeAndStaticOnly(t *testing.T) {
	pr := &fakePlanReader{
		plansByProject: map[string][]clients.PlanSummary{
			"services/loom-core": {{ID: "plan-home", Project: "services/loom-core", Phase: "in_progress", Title: "Home"}},
		},
		slices: map[string][]clients.PlanSliceSummary{
			"plan-home": {{ID: "plan-home#1", PlanID: "plan-home", Name: "home slice", Phase: "pending", Goal: "g", AcceptanceCriteria: "a", Files: []string{"pkg/a/a.go"}}},
		},
	}
	st := newFakeStore()
	e := NewPlanSliceEmitter(pr, st, PlanSliceEmitterConfig{
		Project:   "services/loom-core",
		Namespace: "mills/eligible",
		// nil DynamicDemandProjects.
	}, nil)
	n, err := e.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if n != 1 {
		t.Fatalf("emitted=%d, want 1 (home only)", n)
	}
}

// TestPlanSliceEmitter_EmptyDemandProjectsHomeOnly pins the pre-S6 default:
// without an allowlist the emitter never scans a foreign repo, even one holding
// a ready slice.
func TestPlanSliceEmitter_EmptyDemandProjectsHomeOnly(t *testing.T) {
	pr := &fakePlanReader{
		plansByProject: map[string][]clients.PlanSummary{
			"services/loom-core": {
				{ID: "plan-home", Project: "services/loom-core", Phase: "in_progress", Title: "Home"},
			},
			"services/flexdeck": {
				{ID: "plan-fd", Project: "services/flexdeck", Phase: "in_progress", Title: "Flexdeck"},
			},
		},
		slices: map[string][]clients.PlanSliceSummary{
			"plan-home": {{ID: "plan-home#1", PlanID: "plan-home", Name: "home slice", Phase: "pending", Goal: "g", AcceptanceCriteria: "a", Files: []string{"pkg/a/a.go"}}},
			"plan-fd":   {{ID: "plan-fd#1", PlanID: "plan-fd", Name: "fd slice", Phase: "pending", Goal: "g", AcceptanceCriteria: "a", Files: []string{"README.md"}}},
		},
	}
	st := newFakeStore()
	e := NewPlanSliceEmitter(pr, st, PlanSliceEmitterConfig{
		Project:   "services/loom-core",
		Namespace: "mills/eligible",
		// DemandProjects empty → home-only.
	}, nil)
	n, err := e.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if n != 1 {
		t.Fatalf("emitted=%d, want 1 (home only)", n)
	}
	if _, err := st.Get(context.Background(), planSliceBacklogID("plan-fd#1")); err == nil {
		t.Error("flexdeck slice emitted despite empty DemandProjects allowlist")
	}
}

// TestPlanSliceEmitter_EmittedItemCarriesSliceScope pins that an emitted item
// carries the slice's files as a single-slice scope. Without it the item
// lands slice-less and trips the pipeline scope gate on every implement
// attempt — the same escalation cascade sliceless council items hit, which is
// exactly the regression that would defeat enabling the S2 lane.
func TestPlanSliceEmitter_EmittedItemCarriesSliceScope(t *testing.T) {
	pr, st := emitterFixture(), newFakeStore()
	if _, err := testEmitter(pr, st).Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	item, err := st.Get(context.Background(), planSliceBacklogID("plan-ready#1"))
	if err != nil || item == nil {
		t.Fatalf("expected emitted item, got err=%v", err)
	}
	if len(item.Slices) != 1 {
		t.Fatalf("emitted item Slices=%d, want 1 (single-slice scope)", len(item.Slices))
	}
	if item.Slices[0].Name != "do thing" {
		t.Errorf("slice name=%q, want 'do thing'", item.Slices[0].Name)
	}
	if len(item.Slices[0].Files) != 1 || item.Slices[0].Files[0] != "pkg/foo/bar.go" {
		t.Errorf("slice files=%v, want [pkg/foo/bar.go]", item.Slices[0].Files)
	}
}

// TestPlanSliceEmitter_RecoversFilesFromSliceDetail mirrors the live bug: the
// agent_plan_slice_list TOON tabular view omits the `files` array, so every
// ready slice arrives with empty Files and the emitted item lands slice-less,
// fail-closing at the scope gate. The emitter must recover files from the
// detail (agent_plan_slice_get) view so the item carries a real scope.
func TestPlanSliceEmitter_RecoversFilesFromSliceDetail(t *testing.T) {
	pr := &fakePlanReader{
		plans: []clients.PlanSummary{
			{ID: "plan-ready", Project: "services/loom-core", Phase: "in_progress", Title: "Ready plan"},
		},
		slices: map[string][]clients.PlanSliceSummary{
			// LIST omits files — exactly what the live tabular view returns.
			"plan-ready": {{ID: "plan-ready#1", PlanID: "plan-ready", Name: "do thing", Phase: "pending", Goal: "g", AcceptanceCriteria: "a"}},
		},
		details: map[string]clients.PlanSliceSummary{
			// GET (detail) carries the declared files.
			"plan-ready#1": {ID: "plan-ready#1", PlanID: "plan-ready", Name: "do thing", Phase: "pending", Files: []string{"pkg/mills/policy.go", "pkg/mills/crossrepo/registry.go"}},
		},
	}
	st := newFakeStore()
	if _, err := testEmitter(pr, st).Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if pr.sliceGets == 0 {
		t.Error("expected the emitter to fetch slice detail when the list omits files")
	}
	item, err := st.Get(context.Background(), planSliceBacklogID("plan-ready#1"))
	if err != nil || item == nil {
		t.Fatalf("expected emitted item, got err=%v", err)
	}
	if len(item.Slices) != 1 {
		t.Fatalf("emitted item Slices=%d, want 1 (scope recovered from detail)", len(item.Slices))
	}
	if got := item.Slices[0].Files; len(got) != 2 || got[0] != "pkg/mills/policy.go" {
		t.Errorf("slice files=%v, want the two detail files", got)
	}
}

// TestPlanSliceEmitter_DetailAlsoFilelessStaysScopeless keeps the fail-closed
// guarantee: if neither the list nor the detail declares files, the item lands
// slice-less so the scope gate still flags the under-specified slice.
func TestPlanSliceEmitter_DetailAlsoFilelessStaysScopeless(t *testing.T) {
	pr := &fakePlanReader{
		plans: []clients.PlanSummary{{ID: "plan-ready", Project: "services/loom-core", Phase: "in_progress", Title: "P"}},
		slices: map[string][]clients.PlanSliceSummary{
			"plan-ready": {{ID: "plan-ready#1", PlanID: "plan-ready", Name: "no files", Phase: "pending"}},
		},
		details: map[string]clients.PlanSliceSummary{
			"plan-ready#1": {ID: "plan-ready#1", PlanID: "plan-ready", Name: "no files", Phase: "pending"}, // still no files
		},
	}
	st := newFakeStore()
	if _, err := testEmitter(pr, st).Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	item, _ := st.Get(context.Background(), planSliceBacklogID("plan-ready#1"))
	if item == nil || len(item.Slices) != 0 {
		t.Fatalf("expected slice-less item (fail-closed), got %+v", item)
	}
}

// TestSliceToBacklog_NoFilesLeavesScopeless documents the deliberate
// fail-closed behavior: a slice that declared no files yields no scope, so the
// gate still flags it rather than auto-merging an unscoped item.
func TestSliceToBacklog_NoFilesLeavesScopeless(t *testing.T) {
	item := sliceToBacklog(
		clients.PlanSummary{ID: "plan-x", Title: "P"},
		clients.PlanSliceSummary{ID: "plan-x#1", PlanID: "plan-x", Name: "no files", Phase: "pending"},
		PlanSliceEmitterConfig{Label: planSliceDefaultLabel, Priority: store.P2},
		nil,
		nil,
	)
	if len(item.Slices) != 0 {
		t.Fatalf("Slices=%d, want 0 (no files declared)", len(item.Slices))
	}
}

// TestSliceToBacklog_PreDeclaresProtectedTouches verifies the emitter records a
// slice's protected-path touches on the item so the path_policy gate treats a
// plan-declared touch as intended (the "auto-proceed if planned" policy).
func TestSliceToBacklog_PreDeclaresProtectedTouches(t *testing.T) {
	item := sliceToBacklog(
		clients.PlanSummary{ID: "plan-a", Title: "Auth"},
		clients.PlanSliceSummary{ID: "plan-a#1", PlanID: "plan-a", Name: "touch auth", Phase: "pending",
			Files: []string{"pkg/x/auth.go", "pkg/x/helper.go"}},
		PlanSliceEmitterConfig{Label: planSliceDefaultLabel, Priority: store.P2},
		[]string{"pkg/x/auth.go"},
		nil,
	)
	if got := item.Policy.ProtectedPathsTouched; len(got) != 1 || got[0] != "pkg/x/auth.go" {
		t.Fatalf("ProtectedPathsTouched=%v, want [pkg/x/auth.go]", got)
	}
	// No protected touches → field stays empty (prior behavior, no spurious opt-in).
	item = sliceToBacklog(
		clients.PlanSummary{ID: "plan-a", Title: "Plain"},
		clients.PlanSliceSummary{ID: "plan-a#2", PlanID: "plan-a", Name: "plain", Phase: "pending",
			Files: []string{"pkg/x/helper.go"}},
		PlanSliceEmitterConfig{Label: planSliceDefaultLabel, Priority: store.P2},
		nil,
		nil,
	)
	if len(item.Policy.ProtectedPathsTouched) != 0 {
		t.Errorf("ProtectedPathsTouched=%v, want empty", item.Policy.ProtectedPathsTouched)
	}
}

func TestPlanSliceEmitter_DedupesOnReRun(t *testing.T) {
	pr, st := emitterFixture(), newFakeStore()
	e := testEmitter(pr, st)
	if _, err := e.Tick(context.Background()); err != nil {
		t.Fatalf("tick1: %v", err)
	}
	n, err := e.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick2: %v", err)
	}
	if n != 0 {
		t.Fatalf("second tick emitted=%d, want 0 (dedup)", n)
	}
	if st.puts != 1 {
		t.Fatalf("total puts=%d, want 1", st.puts)
	}
}

func TestPlanSliceEmitter_FailClosedWithoutNamespace(t *testing.T) {
	pr, st := emitterFixture(), newFakeStore()
	// No namespace → inert even though plans/slices would otherwise qualify.
	e := NewPlanSliceEmitter(pr, st, PlanSliceEmitterConfig{Project: "services/loom-core"}, nil)
	n, err := e.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if n != 0 || st.puts != 0 {
		t.Fatalf("emitted=%d puts=%d, want 0/0 (fail-closed)", n, st.puts)
	}
	if pr.calls != 0 {
		t.Fatalf("ListPlans called %d times, want 0 (short-circuit before any read)", pr.calls)
	}
}

func TestPlanSliceBacklogID_Deterministic(t *testing.T) {
	got := planSliceBacklogID("plan-mills-demand#3")
	if got != "psl-plan-mills-demand-3" {
		t.Fatalf("id=%q, want psl-plan-mills-demand-3", got)
	}
	// Distinct slice ids must not collide.
	if planSliceBacklogID("plan-a#1") == planSliceBacklogID("plan-b#1") {
		t.Fatal("distinct slice ids collided")
	}
}

// TestPlanSliceEmitter_PropagatesPlanPriority is the warp-beam junction test:
// a plan's declared priority bucket must land on the emitted backlog item
// (the dispatcher orders queued items priority ASC, so this is what makes
// plan-level reordering steer dispatch). Unknown/unset plan priorities fall
// back to the emitter default.
func TestPlanSliceEmitter_PropagatesPlanPriority(t *testing.T) {
	pr := &fakePlanReader{
		plans: []clients.PlanSummary{
			{ID: "plan-hot", Project: "services/loom-core", Phase: "in_progress", Title: "Hot", Priority: "P0"},
			{ID: "plan-mild", Project: "services/loom-core", Phase: "in_progress", Title: "Mild", Priority: "p3"}, // lowercase must normalize
			{ID: "plan-none", Project: "services/loom-core", Phase: "in_progress", Title: "None"},
			{ID: "plan-junk", Project: "services/loom-core", Phase: "in_progress", Title: "Junk", Priority: "urgent"},
		},
		slices: map[string][]clients.PlanSliceSummary{
			"plan-hot":  {{ID: "plan-hot#1", PlanID: "plan-hot", Name: "h", Phase: "pending", Files: []string{"a.go"}}},
			"plan-mild": {{ID: "plan-mild#1", PlanID: "plan-mild", Name: "m", Phase: "pending", Files: []string{"b.go"}}},
			"plan-none": {{ID: "plan-none#1", PlanID: "plan-none", Name: "n", Phase: "pending", Files: []string{"c.go"}}},
			"plan-junk": {{ID: "plan-junk#1", PlanID: "plan-junk", Name: "j", Phase: "pending", Files: []string{"d.go"}}},
		},
	}
	st := newFakeStore()
	if _, err := testEmitter(pr, st).Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	want := map[string]store.Priority{
		"plan-hot#1":  store.P0,
		"plan-mild#1": store.P3,
		"plan-none#1": store.P2, // emitter default
		"plan-junk#1": store.P2, // unknown bucket falls back
	}
	for sliceID, wantP := range want {
		item, err := st.Get(context.Background(), planSliceBacklogID(sliceID))
		if err != nil || item == nil {
			t.Fatalf("missing emitted item for %s: %v", sliceID, err)
		}
		if item.Priority != wantP {
			t.Errorf("%s Priority=%q, want %q", sliceID, item.Priority, wantP)
		}
	}
}

// TestPlanSliceEmitter_ResyncsQueuedPriority pins the live-steering half of
// the beam: when a plan's priority changes AFTER its slice was emitted, the
// next tick must propagate the new bucket onto the still-queued item (the
// read-then-insert dedup would otherwise freeze the priority at emit time,
// making HUD reordering cosmetic).
func TestPlanSliceEmitter_ResyncsQueuedPriority(t *testing.T) {
	pr := &fakePlanReader{
		plans: []clients.PlanSummary{
			{ID: "plan-r", Project: "services/loom-core", Phase: "in_progress", Title: "R", Priority: "P2"},
		},
		slices: map[string][]clients.PlanSliceSummary{
			"plan-r": {{ID: "plan-r#1", PlanID: "plan-r", Name: "r", Phase: "pending", Files: []string{"r.go"}}},
		},
	}
	st := newFakeStore()
	e := testEmitter(pr, st)
	if _, err := e.Tick(context.Background()); err != nil {
		t.Fatalf("tick1: %v", err)
	}
	id := planSliceBacklogID("plan-r#1")
	if item, _ := st.Get(context.Background(), id); item.Priority != store.P2 {
		t.Fatalf("initial Priority=%q, want P2", item.Priority)
	}

	// Operator promotes the plan to P0; the still-queued item must follow.
	pr.plans[0].Priority = "P0"
	n, err := e.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick2: %v", err)
	}
	if n != 0 {
		t.Fatalf("resync tick emitted=%d, want 0 (no new item)", n)
	}
	item, _ := st.Get(context.Background(), id)
	if item.Priority != store.P0 {
		t.Fatalf("resynced Priority=%q, want P0", item.Priority)
	}
	if item.State != store.BacklogQueued {
		t.Fatalf("resync must not disturb state; got %q", item.State)
	}
}

// TestPlanSliceEmitter_NoResyncOnceInFlight keeps the resync surgical: an item
// that has left queued (running/terminal) is never touched, even if the plan's
// priority changes — priority is inert after dispatch and clobbering an active
// item risks racing the reconciler's state transitions.
func TestPlanSliceEmitter_NoResyncOnceInFlight(t *testing.T) {
	pr := &fakePlanReader{
		plans: []clients.PlanSummary{
			{ID: "plan-f", Project: "services/loom-core", Phase: "in_progress", Title: "F", Priority: "P2"},
		},
		slices: map[string][]clients.PlanSliceSummary{
			"plan-f": {{ID: "plan-f#1", PlanID: "plan-f", Name: "f", Phase: "pending", Files: []string{"f.go"}}},
		},
	}
	st := newFakeStore()
	e := testEmitter(pr, st)
	if _, err := e.Tick(context.Background()); err != nil {
		t.Fatalf("tick1: %v", err)
	}
	id := planSliceBacklogID("plan-f#1")
	item, _ := st.Get(context.Background(), id)
	item.State = store.BacklogRunning
	putsBefore := st.puts

	pr.plans[0].Priority = "P0"
	if _, err := e.Tick(context.Background()); err != nil {
		t.Fatalf("tick2: %v", err)
	}
	after, _ := st.Get(context.Background(), id)
	if after.Priority != store.P2 {
		t.Fatalf("in-flight item priority changed to %q; must stay P2", after.Priority)
	}
	if st.puts != putsBefore {
		t.Fatalf("in-flight item was re-Put (%d -> %d puts); must be untouched", putsBefore, st.puts)
	}
}

// ----- Emit-time slice grounding (fabricated-slice fail-closed) -----

// groundingCall records one invocation of the emitter's grounding hook.
type groundingCall struct {
	project string
	files   []string
}

// installGrounder wires a canned grounding hook onto e and returns the call
// log. missingByFile maps a declared path to true when the fake repo tree
// lacks it; ok=false models an ungroundable project (foreign repo, no clone).
func installGrounder(e *PlanSliceEmitter, revision string, ok bool, missing map[string]bool) *[]groundingCall {
	calls := &[]groundingCall{}
	e.SetSliceGrounder(func(_ context.Context, project string, files []string) ([]string, string, bool) {
		*calls = append(*calls, groundingCall{project: project, files: append([]string(nil), files...)})
		if !ok {
			return nil, "", false
		}
		var out []string
		for _, f := range files {
			if missing[f] {
				out = append(out, f)
			}
		}
		return out, revision, true
	})
	return calls
}

func TestPlanSliceEmitter_GroundsAndFlagsFabricatedSlice(t *testing.T) {
	pr, st := emitterFixture(), newFakeStore()
	e := testEmitter(pr, st)
	// The fixture's only ready slice declares pkg/foo/bar.go; the fake tree
	// lacks it, so the slice carries the fabricated-slice signature.
	installGrounder(e, "abc1234", true, map[string]bool{"pkg/foo/bar.go": true})
	if _, err := e.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	item, err := st.Get(context.Background(), planSliceBacklogID("plan-ready#1"))
	if err != nil || item == nil {
		t.Fatalf("expected emitted item, got err=%v", err)
	}
	if len(item.Slices) != 1 {
		t.Fatalf("Slices=%d, want 1", len(item.Slices))
	}
	s := item.Slices[0]
	if !s.Fabricated {
		t.Error("slice not flagged Fabricated despite every declared file missing")
	}
	if len(s.MissingFiles) != 1 || s.MissingFiles[0] != "pkg/foo/bar.go" {
		t.Errorf("MissingFiles=%v, want [pkg/foo/bar.go]", s.MissingFiles)
	}
	if s.GroundingRevision != "abc1234" {
		t.Errorf("GroundingRevision=%q, want abc1234", s.GroundingRevision)
	}
	var hasLabel bool
	for _, l := range item.Labels {
		if l == planSliceFabricatedLabel {
			hasLabel = true
		}
	}
	if !hasLabel {
		t.Errorf("Labels=%v, want %q present", item.Labels, planSliceFabricatedLabel)
	}
}

func TestPlanSliceEmitter_PartialMissingIsNotFabricated(t *testing.T) {
	pr := &fakePlanReader{
		plans: []clients.PlanSummary{{ID: "plan-ready", Project: "services/loom-core", Phase: "in_progress", Title: "P"}},
		slices: map[string][]clients.PlanSliceSummary{
			"plan-ready": {{ID: "plan-ready#1", PlanID: "plan-ready", Name: "mixed", Phase: "pending",
				Files: []string{"pkg/exists.go", "pkg/new_file.go"}}},
		},
	}
	st := newFakeStore()
	e := testEmitter(pr, st)
	installGrounder(e, "rev9", true, map[string]bool{"pkg/new_file.go": true})
	if _, err := e.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	item, _ := st.Get(context.Background(), planSliceBacklogID("plan-ready#1"))
	if item == nil || len(item.Slices) != 1 {
		t.Fatalf("expected single-slice item, got %+v", item)
	}
	s := item.Slices[0]
	if s.Fabricated {
		t.Error("partial-missing slice flagged Fabricated; a planned new file next to a real one is legitimate")
	}
	if len(s.MissingFiles) != 1 || s.MissingFiles[0] != "pkg/new_file.go" {
		t.Errorf("MissingFiles=%v, want [pkg/new_file.go]", s.MissingFiles)
	}
	for _, l := range item.Labels {
		if l == planSliceFabricatedLabel {
			t.Errorf("Labels=%v carry %q; partial missing must not flag", item.Labels, planSliceFabricatedLabel)
		}
	}
}

func TestPlanSliceEmitter_UngroundableEmitsUngrounded(t *testing.T) {
	pr, st := emitterFixture(), newFakeStore()
	e := testEmitter(pr, st)
	installGrounder(e, "", false, nil)
	if _, err := e.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	item, _ := st.Get(context.Background(), planSliceBacklogID("plan-ready#1"))
	if item == nil || len(item.Slices) != 1 {
		t.Fatalf("expected single-slice item, got %+v", item)
	}
	s := item.Slices[0]
	if s.Fabricated || len(s.MissingFiles) != 0 || s.GroundingRevision != "" {
		t.Errorf("ungroundable slice must emit unstamped, got %+v", s)
	}
}

func TestPlanSliceEmitter_GrounderReceivesOnlyConcretePaths(t *testing.T) {
	pr := &fakePlanReader{
		plans: []clients.PlanSummary{{ID: "plan-ready", Project: "services/loom-core", Phase: "in_progress", Title: "P"}},
		slices: map[string][]clients.PlanSliceSummary{
			"plan-ready": {{ID: "plan-ready#1", PlanID: "plan-ready", Name: "globby", Phase: "pending",
				Files: []string{"pkg/**/*auth*.go", " ./pkg/real.go ", ""}}},
		},
	}
	st := newFakeStore()
	e := testEmitter(pr, st)
	calls := installGrounder(e, "rev1", true, nil)
	if _, err := e.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(*calls) != 1 {
		t.Fatalf("grounder calls=%d, want 1", len(*calls))
	}
	got := (*calls)[0]
	if got.project != "services/loom-core" {
		t.Errorf("project=%q, want services/loom-core", got.project)
	}
	if len(got.files) != 1 || got.files[0] != "pkg/real.go" {
		t.Errorf("files=%v, want the single normalized concrete path [pkg/real.go]", got.files)
	}
}

// TestPlanSliceEmitter_GrounderSkippedForExistingItems pins the dedup-first
// ordering: re-emitting an already-tracked slice must not re-run grounding
// (it would double-count the verdict metric every tick).
func TestPlanSliceEmitter_GrounderSkippedForExistingItems(t *testing.T) {
	pr, st := emitterFixture(), newFakeStore()
	e := testEmitter(pr, st)
	calls := installGrounder(e, "rev1", true, nil)
	if _, err := e.Tick(context.Background()); err != nil {
		t.Fatalf("tick1: %v", err)
	}
	if _, err := e.Tick(context.Background()); err != nil {
		t.Fatalf("tick2: %v", err)
	}
	if len(*calls) != 1 {
		t.Fatalf("grounder calls=%d after two ticks, want 1 (existing item skips grounding)", len(*calls))
	}
}
