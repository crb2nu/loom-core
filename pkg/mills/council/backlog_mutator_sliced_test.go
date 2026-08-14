package council

import (
	"context"
	"errors"
	"testing"

	"github.com/crb2nu/loom/pkg/mills/store"
)

// stubSlicedAuthor implements both PlanAuthor and the optional SlicedPlanAuthor
// so the mutator's type-assert routing can be exercised.
type stubSlicedAuthor struct {
	planID      string
	slicedCalls int
	flatCalls   int
	gotInput    SlicedPlanInput
	slicedErr   error
}

func (s *stubSlicedAuthor) AuthorPlan(_ context.Context, _ *store.BacklogItem, _ string) (string, error) {
	s.flatCalls++
	return "plan-flat", nil
}

func (s *stubSlicedAuthor) AuthorSlicedPlan(_ context.Context, in SlicedPlanInput) (string, error) {
	s.slicedCalls++
	s.gotInput = in
	if s.slicedErr != nil {
		return "", s.slicedErr
	}
	return s.planID, nil
}

func slicedProposalOutput() *EditorOutput {
	return &EditorOutput{
		BacklogProposals: []BacklogProposal{{
			Title:    "Add the thing",
			Priority: store.P2,
			PlanSlices: []PlanSliceSpec{
				{Name: "slice one", Goal: "do one", Files: []string{"a.go"}},
				{Name: "slice two", Goal: "do two", Files: []string{"b.go"}},
			},
		}},
	}
}

func TestApply_RoutesSlicedProposalToPlanLane(t *testing.T) {
	m, st, _ := newMutatorEnv(t)
	author := &stubSlicedAuthor{planID: "plan-council-add-the-thing"}
	m.PlanAuthor = author
	m.Project = "services/loom-core"
	m.PlanSliceNamespace = "mills/eligible"

	res, err := m.Apply(context.Background(), "COUNCIL-X", slicedProposalOutput(), MutationOptions{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if author.slicedCalls != 1 {
		t.Fatalf("AuthorSlicedPlan calls=%d, want 1", author.slicedCalls)
	}
	if author.flatCalls != 0 {
		t.Fatalf("flat AuthorPlan calls=%d, want 0 (routed to plan lane)", author.flatCalls)
	}
	if len(res.CreatedItems) != 0 {
		t.Fatalf("flat items created=%d, want 0", len(res.CreatedItems))
	}
	if len(res.RoutedPlanLane) != 1 || res.RoutedPlanLane[0] != "plan-council-add-the-thing" {
		t.Fatalf("RoutedPlanLane=%v", res.RoutedPlanLane)
	}
	if author.gotInput.Namespace != "mills/eligible" || author.gotInput.Project != "services/loom-core" {
		t.Errorf("sliced input ns/project = %q/%q", author.gotInput.Namespace, author.gotInput.Project)
	}
	if len(author.gotInput.Slices) != 2 {
		t.Errorf("sliced input slices=%d, want 2", len(author.gotInput.Slices))
	}
	items, _ := st.Backlog.List(context.Background())
	if len(items) != 0 {
		t.Fatalf("backlog has %d items, want 0 (no flat item)", len(items))
	}
}

func TestApply_NoNamespace_SlicedProposalFallsToFlatItem(t *testing.T) {
	m, st, _ := newMutatorEnv(t)
	author := &stubSlicedAuthor{planID: "x"}
	m.PlanAuthor = author
	// PlanSliceNamespace intentionally empty → routing off.

	res, err := m.Apply(context.Background(), "COUNCIL-Y", slicedProposalOutput(), MutationOptions{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if author.slicedCalls != 0 {
		t.Fatalf("AuthorSlicedPlan calls=%d, want 0 (no namespace)", author.slicedCalls)
	}
	if len(res.CreatedItems) != 1 {
		t.Fatalf("created items=%d, want 1 (flat fallback)", len(res.CreatedItems))
	}
	items, _ := st.Backlog.List(context.Background())
	if len(items) != 1 {
		t.Fatalf("backlog has %d items, want 1", len(items))
	}
}

// TestApply_NoNamespace_FlatItemCarriesPlanSlices is the regression test for
// the 2026-06-28/29 escalation cascade: with the S2 plan lane off (prod had
// plan_slice_namespace empty), a council-decomposed proposal fell through to
// the flat-item path, which dropped PlanSlices entirely and produced a
// slice-less item. The slice-less item then tripped the scope gate on every
// implement attempt and escalated with an empty diff. The flat item MUST now
// carry the editor's parsed slices so the scope gate has something to enforce.
func TestApply_NoNamespace_FlatItemCarriesPlanSlices(t *testing.T) {
	m, st, _ := newMutatorEnv(t)
	// PlanSliceNamespace intentionally empty → routing off → flat path.

	res, err := m.Apply(context.Background(), "COUNCIL-C", slicedProposalOutput(), MutationOptions{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(res.CreatedItems) != 1 {
		t.Fatalf("created items=%d, want 1 (flat fallback)", len(res.CreatedItems))
	}
	item := res.CreatedItems[0]
	if len(item.Slices) != 2 {
		t.Fatalf("flat item Slices=%d, want 2 (carried from PlanSlices)", len(item.Slices))
	}
	if item.Slices[0].Name != "slice one" || item.Slices[1].Name != "slice two" {
		t.Errorf("slice names = %q,%q", item.Slices[0].Name, item.Slices[1].Name)
	}
	if len(item.Slices[0].Files) != 1 || item.Slices[0].Files[0] != "a.go" {
		t.Errorf("slice one files = %v, want [a.go]", item.Slices[0].Files)
	}
	// Persisted item (not just the in-memory return) carries the slices too.
	stored, err := st.Backlog.Get(context.Background(), item.ID)
	if err != nil {
		t.Fatalf("get persisted item: %v", err)
	}
	if len(stored.Slices) != 2 {
		t.Fatalf("persisted item Slices=%d, want 2", len(stored.Slices))
	}
}

// TestProposalItemSlices covers the slice-resolution helper directly: explicit
// Slices win, PlanSlices are the fallback, empty-name slices are dropped, and
// a proposal with neither yields nil.
func TestProposalItemSlices(t *testing.T) {
	t.Run("prefers explicit Slices", func(t *testing.T) {
		p := BacklogProposal{
			Slices:     []store.Slice{{Name: "explicit", Files: []string{"x.go"}}},
			PlanSlices: []PlanSliceSpec{{Name: "ignored", Files: []string{"y.go"}}},
		}
		got := proposalItemSlices(p)
		if len(got) != 1 || got[0].Name != "explicit" {
			t.Fatalf("got %+v, want explicit slice", got)
		}
	})
	t.Run("falls back to PlanSlices", func(t *testing.T) {
		p := BacklogProposal{PlanSlices: []PlanSliceSpec{
			{Name: "a", Goal: "dropped", Files: []string{"a.go", "b.go"}},
			{Name: "  ", Files: []string{"skip.go"}}, // blank name dropped
		}}
		got := proposalItemSlices(p)
		if len(got) != 1 || got[0].Name != "a" || len(got[0].Files) != 2 {
			t.Fatalf("got %+v, want one slice 'a' with 2 files", got)
		}
	})
	t.Run("nil when neither set", func(t *testing.T) {
		if got := proposalItemSlices(BacklogProposal{}); got != nil {
			t.Fatalf("got %+v, want nil", got)
		}
	})
}

func TestApply_SlicedAuthorError_FallsBackToFlatItem(t *testing.T) {
	m, st, _ := newMutatorEnv(t)
	author := &stubSlicedAuthor{slicedErr: errors.New("hub down")}
	m.PlanAuthor = author
	m.PlanSliceNamespace = "mills/eligible"

	res, err := m.Apply(context.Background(), "COUNCIL-T", slicedProposalOutput(), MutationOptions{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if author.slicedCalls != 1 {
		t.Fatalf("sliced calls=%d, want 1 (attempted)", author.slicedCalls)
	}
	if len(res.RoutedPlanLane) != 0 {
		t.Fatalf("RoutedPlanLane=%v, want empty on error", res.RoutedPlanLane)
	}
	if len(res.CreatedItems) != 1 {
		t.Fatalf("created items=%d, want 1 (fell back to flat item on sliced error)", len(res.CreatedItems))
	}
	items, _ := st.Backlog.List(context.Background())
	if len(items) != 1 {
		t.Fatalf("backlog has %d items, want 1", len(items))
	}
}
