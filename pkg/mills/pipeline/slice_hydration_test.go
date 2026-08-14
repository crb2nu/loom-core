package pipeline

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/crb2nu/loom/pkg/mills/gates"
	"github.com/crb2nu/loom/pkg/mills/store"
)

// fakeSliceHydrator is a canned PlanSliceHydrator for hydration tests.
type fakeSliceHydrator struct {
	slices []store.Slice
	files  []string
	err    error
	calls  int
	planID string
}

func (f *fakeSliceHydrator) SliceScopeForPlan(_ context.Context, planID string) ([]store.Slice, []string, error) {
	f.calls++
	f.planID = planID
	return f.slices, f.files, f.err
}

// TestHydrateSliceScope_StampsAndPersists: a slice-less, plan-linked item
// (the gl-47-334 shape — GitLab-issue import, plan authored, Slices=[])
// gains the plan store's file-bearing slices in-memory AND in the persisted
// row, so this run's gates and any resumed Drive both see the envelope.
func TestHydrateSliceScope_StampsAndPersists(t *testing.T) {
	st, run, item := newRunnerEnv(t)
	item.PlanID = "plan-mills-gl-47-334"
	if err := st.Backlog.Put(context.Background(), item); err != nil {
		t.Fatalf("stamp plan id: %v", err)
	}
	hyd := &fakeSliceHydrator{
		slices: []store.Slice{{Name: "council-durability", Files: []string{"pkg/mills/runner/council.go"}}},
		files:  []string{"pkg/mills/runner/council.go"},
	}
	r := New(st, nil, &fakeDispatcher{}, nil)
	r.SliceHydrator = hyd

	r.hydrateSliceScope(context.Background(), run, item)

	if hyd.calls != 1 || hyd.planID != "plan-mills-gl-47-334" {
		t.Fatalf("hydrator calls=%d planID=%q, want 1 call for the item's plan", hyd.calls, hyd.planID)
	}
	if len(item.Slices) != 1 || item.Slices[0].Name != "council-durability" {
		t.Fatalf("in-memory item slices = %+v, want the hydrated slice", item.Slices)
	}
	persisted, err := st.Backlog.Get(context.Background(), item.ID)
	if err != nil {
		t.Fatalf("get persisted: %v", err)
	}
	if len(persisted.Slices) != 1 || len(persisted.Slices[0].Files) != 1 {
		t.Fatalf("persisted slices = %+v, want the hydrated slice with files", persisted.Slices)
	}
}

// TestHydrateSliceScope_NoFileBearingSlicesLeavesItemUntouched: a plan whose
// slices declare no files (the #332 shape — a council docs slice) hydrates
// nothing; the item stays slice-less and the scope gate skips downstream.
func TestHydrateSliceScope_NoFileBearingSlicesLeavesItemUntouched(t *testing.T) {
	st, run, item := newRunnerEnv(t)
	item.PlanID = "plan-council-docs-only"
	if err := st.Backlog.Put(context.Background(), item); err != nil {
		t.Fatalf("stamp plan id: %v", err)
	}
	hyd := &fakeSliceHydrator{} // returns no slices
	r := New(st, nil, &fakeDispatcher{}, nil)
	r.SliceHydrator = hyd

	r.hydrateSliceScope(context.Background(), run, item)

	if hyd.calls != 1 {
		t.Fatalf("hydrator calls = %d, want 1", hyd.calls)
	}
	if len(item.Slices) != 0 {
		t.Fatalf("item slices = %+v, want untouched empty", item.Slices)
	}
}

// TestHydrateSliceScope_SkipsUnlinkedAndAlreadyScopedItems: no PlanID means
// nothing to read; an item that already carries file-bearing scope must not
// be overwritten by a re-run (plan-store contents could have drifted).
func TestHydrateSliceScope_SkipsUnlinkedAndAlreadyScopedItems(t *testing.T) {
	st, run, item := newRunnerEnv(t)
	hyd := &fakeSliceHydrator{
		slices: []store.Slice{{Name: "drifted", Files: []string{"pkg/other/x.go"}}},
	}
	r := New(st, nil, &fakeDispatcher{}, nil)
	r.SliceHydrator = hyd

	// Unlinked: PlanID empty.
	r.hydrateSliceScope(context.Background(), run, item)
	if hyd.calls != 0 {
		t.Fatalf("hydrator ran for an unlinked item (calls=%d)", hyd.calls)
	}

	// Already scoped: intake materialized file-bearing slices.
	item.PlanID = "plan-x"
	item.Slices = []store.Slice{{Name: "intake", Files: []string{"pkg/a/a.go"}}}
	r.hydrateSliceScope(context.Background(), run, item)
	if hyd.calls != 0 {
		t.Fatalf("hydrator ran for an already-scoped item (calls=%d)", hyd.calls)
	}
	if item.Slices[0].Name != "intake" {
		t.Fatalf("existing scope overwritten: %+v", item.Slices)
	}
}

// TestHydrateSliceScope_HydratorErrorIsNonFatal: a plan-store failure logs
// and leaves the item as-is — hydration is best-effort, never run-fatal.
func TestHydrateSliceScope_HydratorErrorIsNonFatal(t *testing.T) {
	st, run, item := newRunnerEnv(t)
	item.PlanID = "plan-x"
	hyd := &fakeSliceHydrator{err: errors.New("hub down")}
	r := New(st, nil, &fakeDispatcher{}, nil)
	r.SliceHydrator = hyd

	r.hydrateSliceScope(context.Background(), run, item)
	if len(item.Slices) != 0 {
		t.Fatalf("item slices = %+v, want untouched on hydrator error", item.Slices)
	}
}

// TestRunner_DriveHydratesScopeAfterPlanSlice is the end-to-end regression
// for escalations #332/#338: a slice-less, plan-linked item runs plan_slice,
// the hydrated envelope reaches post_implement_gate, and the REAL scope gate
// evaluates it as a PASS (an enforced envelope) — not the advisory slice-less
// skip, and certainly not the old terminal fail that dead-ended both runs.
func TestRunner_DriveHydratesScopeAfterPlanSlice(t *testing.T) {
	st, run, item := newRunnerEnv(t)
	item.PlanID = "plan-mills-gl-47-334"
	if err := st.Backlog.Put(context.Background(), item); err != nil {
		t.Fatalf("stamp plan id: %v", err)
	}
	disp := &fakeDispatcher{canned: map[string]StageOutput{
		"implement": {
			CostUSD:        0.10,
			FilesChanged:   []string{"pkg/feature/feature.go"},
			LinesAdded:     5,
			DiffPatch:      []byte("diff --git a/pkg/feature/feature.go b/pkg/feature/feature.go\n+x\n"),
			CommitMessages: []string{"feat: x"},
		},
		"mr":    {MRIID: 7},
		"merge": {MergedSHA: "abc"},
	}}
	gr := gates.NewRegistry()
	gr.Register(&gates.Scope{})
	r := New(st, gr, disp, nil)
	r.SliceHydrator = &fakeSliceHydrator{
		slices: []store.Slice{{Name: "feature", Files: []string{"pkg/feature/feature.go"}}},
		files:  []string{"pkg/feature/feature.go"},
	}
	if err := r.Drive(context.Background(), run, item); err != nil {
		t.Fatalf("drive: %v", err)
	}
	got, err := st.Pipeline.GetRun(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("getrun: %v", err)
	}
	if got.State != store.PipelineDone {
		t.Fatalf("state = %s, want done", got.State)
	}
	rows, err := st.Pipeline.ListGates(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("list gates: %v", err)
	}
	sawScope := false
	for _, g := range rows {
		if g.GateName != "scope" {
			continue
		}
		sawScope = true
		if g.Outcome != store.GateOutcomePass {
			t.Errorf("scope outcome = %s (reasons %v), want pass via the hydrated envelope", g.Outcome, g.Reasons)
		}
	}
	if !sawScope {
		t.Errorf("no persisted scope gate row: %+v", rows)
	}
	persisted, err := st.Backlog.Get(context.Background(), item.ID)
	if err != nil {
		t.Fatalf("get persisted: %v", err)
	}
	if len(persisted.Slices) != 1 {
		t.Errorf("persisted slices = %+v, want the hydrated slice", persisted.Slices)
	}
}

// TestRunner_HydratedScopeIsEnforced: the hydrated envelope is a real
// constraint, not decoration — an implement diff outside it fails the scope
// gate and the run escalates after the retry budget instead of merging.
func TestRunner_HydratedScopeIsEnforced(t *testing.T) {
	st, run, item := newRunnerEnv(t)
	item.PlanID = "plan-mills-gl-47-334"
	if err := st.Backlog.Put(context.Background(), item); err != nil {
		t.Fatalf("stamp plan id: %v", err)
	}
	disp := &fakeDispatcher{canned: map[string]StageOutput{
		"implement": {
			FilesChanged:   []string{"internal/unrelated/detour.go"},
			LinesAdded:     5,
			DiffPatch:      []byte("diff --git a/internal/unrelated/detour.go b/internal/unrelated/detour.go\n+x\n"),
			CommitMessages: []string{"feat: detour"},
		},
	}}
	gr := gates.NewRegistry()
	gr.Register(&gates.Scope{})
	esc := &reasonCapturingEscalator{}
	r := New(st, gr, disp, nil)
	r.Escalator = esc
	r.SliceHydrator = &fakeSliceHydrator{
		slices: []store.Slice{{Name: "feature", Files: []string{"pkg/feature/feature.go"}}},
		files:  []string{"pkg/feature/feature.go"},
	}
	if err := r.Drive(context.Background(), run, item); err != nil {
		t.Fatalf("drive: %v", err)
	}
	got, err := st.Pipeline.GetRun(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("getrun: %v", err)
	}
	if got.State != store.PipelineEscalated {
		t.Fatalf("state = %s, want escalated (out-of-envelope diff)", got.State)
	}
	if len(esc.reasons) == 0 || !strings.Contains(esc.reasons[0], "outside slice scope") {
		t.Errorf("escalation reasons = %v, want an outside-slice-scope gate failure", esc.reasons)
	}
}
