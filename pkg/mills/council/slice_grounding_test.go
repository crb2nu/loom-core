package council

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/store"
)

// dirSet builds a directory-existence predicate over a fixed allowlist of
// repo-relative directory paths (the parent dirs of grounded files).
func dirSet(dirs ...string) func(string) bool {
	set := make(map[string]struct{}, len(dirs))
	for _, d := range dirs {
		set[d] = struct{}{}
	}
	return func(d string) bool {
		_, ok := set[d]
		return ok
	}
}

// realDirs is the predicate used across the pure-function tests: the only
// directory that "exists" is pkg/mills/clients (where loom-core council code
// actually lives), so pkg/planning and pkg/pipeline read as fictional — the
// exact paths the editor invented in the live failure.
var realDirs = dirSet("pkg/mills/clients", "pkg/mills/council")

func TestSanitizeProposalSlices_RealSliceKept(t *testing.T) {
	in := []BacklogProposal{{
		Title: "Real work",
		PlanSlices: []PlanSliceSpec{{
			Name:  "guard",
			Files: []string{"pkg/mills/clients/council.go", "pkg/mills/council/backlog_mutator.go"},
		}},
	}}
	got, out := SanitizeProposalSlices(in, realDirs)
	if len(got) != 1 || len(got[0].PlanSlices) != 1 {
		t.Fatalf("proposal/slice dropped unexpectedly: %+v", got)
	}
	if out.SlicesDropped != 0 || out.SlicesFlagged != 0 || out.ProposalsDropped != 0 {
		t.Fatalf("outcome = %+v, want all zero", out)
	}
	if len(out.DroppedPaths) != 0 {
		t.Fatalf("DroppedPaths = %v, want none", out.DroppedPaths)
	}
}

func TestSanitizeProposalSlices_AllNewDirSliceKeptAsSpeculative(t *testing.T) {
	in := []BacklogProposal{{
		Title: "New package work",
		PlanSlices: []PlanSliceSpec{{
			Name:  "new-pkg",
			Files: []string{"pkg/planning/orchestrator.go", "pkg/pipeline/runner.go"},
		}},
	}}
	got, out := SanitizeProposalSlices(in, realDirs)
	// Flag-never-drop: a wholly-new-directory slice is KEPT (not dropped) — it
	// is indistinguishable by path from a legitimate new package.
	if len(got) != 1 || len(got[0].PlanSlices) != 1 {
		t.Fatalf("got %+v, want the proposal kept with its slice", got)
	}
	if out.SlicesSpeculative != 1 {
		t.Errorf("SlicesSpeculative = %d, want 1", out.SlicesSpeculative)
	}
	if out.SlicesDropped != 0 || out.ProposalsDropped != 0 {
		t.Errorf("SlicesDropped=%d ProposalsDropped=%d, want 0/0 (nothing drops)", out.SlicesDropped, out.ProposalsDropped)
	}
	want := []string{"pkg/pipeline/runner.go", "pkg/planning/orchestrator.go"}
	if !reflect.DeepEqual(out.DroppedPaths, want) {
		t.Errorf("DroppedPaths = %v, want %v (kept but recorded)", out.DroppedPaths, want)
	}
}

func TestSanitizeProposalSlices_MixedSliceFlagged(t *testing.T) {
	in := []BacklogProposal{{
		Title: "Mostly real",
		PlanSlices: []PlanSliceSpec{{
			Name:  "mixed",
			Files: []string{"pkg/mills/clients/council.go", "pkg/planning/x.go"},
		}},
	}}
	got, out := SanitizeProposalSlices(in, realDirs)
	// A mixed slice is KEPT (flagged), so the proposal survives with it.
	if len(got) != 1 || len(got[0].PlanSlices) != 1 {
		t.Fatalf("mixed slice should be kept: %+v", got)
	}
	if out.SlicesFlagged != 1 {
		t.Errorf("SlicesFlagged = %d, want 1", out.SlicesFlagged)
	}
	if out.SlicesDropped != 0 || out.ProposalsDropped != 0 {
		t.Errorf("SlicesDropped=%d ProposalsDropped=%d, want 0/0", out.SlicesDropped, out.ProposalsDropped)
	}
	if want := []string{"pkg/planning/x.go"}; !reflect.DeepEqual(out.DroppedPaths, want) {
		t.Errorf("DroppedPaths = %v, want %v", out.DroppedPaths, want)
	}
}

func TestSanitizeProposalSlices_KeepsBothRealAndNewDirSlices(t *testing.T) {
	in := []BacklogProposal{{
		Title: "Two slices",
		PlanSlices: []PlanSliceSpec{
			{Name: "real", Files: []string{"pkg/mills/clients/new.go"}},
			{Name: "new-pkg", Files: []string{"pkg/planning/x.go"}},
		},
	}}
	got, out := SanitizeProposalSlices(in, realDirs)
	if len(got) != 1 {
		t.Fatalf("got %d proposals, want 1", len(got))
	}
	// Both slices kept now — the new-directory slice is speculative, not dropped.
	if len(got[0].PlanSlices) != 2 {
		t.Fatalf("surviving slices = %+v, want both kept", got[0].PlanSlices)
	}
	if out.SlicesSpeculative != 1 || out.SlicesDropped != 0 || out.ProposalsDropped != 0 {
		t.Errorf("Speculative=%d Dropped=%d ProposalsDropped=%d, want 1/0/0",
			out.SlicesSpeculative, out.SlicesDropped, out.ProposalsDropped)
	}
}

// TestSanitizeProposalSlices_LegitNewPackageKept is the motivating idx0 case: a
// slice creating a real new sub-package (pkg/mills/clients/newsub/, whose parent
// pkg/mills/clients exists) is structurally identical to a fabricated path, so
// flag-never-drop keeps it rather than escalating real new-package work.
func TestSanitizeProposalSlices_LegitNewPackageKept(t *testing.T) {
	in := []BacklogProposal{{
		Title: "Add a new sub-package",
		PlanSlices: []PlanSliceSpec{{
			Name:  "new-subpkg",
			Files: []string{"pkg/mills/clients/newsub/service.go", "pkg/mills/clients/newsub/service_test.go"},
		}},
	}}
	got, out := SanitizeProposalSlices(in, realDirs)
	if len(got) != 1 || len(got[0].PlanSlices) != 1 {
		t.Fatalf("legit new-package slice should be kept: %+v", got)
	}
	if out.SlicesSpeculative != 1 || out.SlicesDropped != 0 {
		t.Errorf("Speculative=%d Dropped=%d, want 1/0", out.SlicesSpeculative, out.SlicesDropped)
	}
}

func TestSanitizeProposalSlices_NoPlanSlices_Untouched(t *testing.T) {
	in := []BacklogProposal{{
		Title:  "Single unit",
		Slices: []store.Slice{{Name: "s", Files: []string{"pkg/planning/x.go"}}},
	}}
	got, out := SanitizeProposalSlices(in, realDirs)
	if len(got) != 1 {
		t.Fatalf("proposal without PlanSlices must pass through: %+v", got)
	}
	if out.SlicesDropped != 0 || out.SlicesFlagged != 0 || out.ProposalsDropped != 0 {
		t.Fatalf("outcome = %+v, want all zero (PlanSlices is the only validated carrier)", out)
	}
}

func TestSanitizeProposalSlices_NilPredicate_NoOp(t *testing.T) {
	in := []BacklogProposal{{
		Title:      "x",
		PlanSlices: []PlanSliceSpec{{Name: "s", Files: []string{"pkg/planning/x.go"}}},
	}}
	got, out := SanitizeProposalSlices(in, nil)
	if len(got) != 1 || len(got[0].PlanSlices) != 1 {
		t.Fatalf("nil predicate must be a no-op: %+v", got)
	}
	if out.SlicesDropped != 0 || out.ProposalsDropped != 0 {
		t.Fatalf("outcome = %+v, want all zero", out)
	}
}

func TestSanitizeProposalSlices_EmptyInput(t *testing.T) {
	got, out := SanitizeProposalSlices(nil, realDirs)
	if len(got) != 0 {
		t.Fatalf("got %v, want empty", got)
	}
	if out.guarded() {
		t.Fatalf("outcome = %+v, want all zero", out)
	}
}

func TestRepoDirChecker(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "pkg", "mills", "clients"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "pkg", "mills", "clients", "x.go"), []byte("package clients\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	check := repoDirChecker(root)
	cases := []struct {
		rel  string
		want bool
	}{
		{"pkg/mills/clients", true},
		{"./pkg/mills/clients", true},
		{"pkg/mills", true},
		{".", true},                       // root itself (top-level file parent)
		{"pkg/planning", false},           // the invented dir
		{"pkg/mills/clients/x.go", false}, // a file, not a directory
		{"../outside", false},             // escape attempt
	}
	for _, c := range cases {
		if got := check(c.rel); got != c.want {
			t.Errorf("check(%q) = %v, want %v", c.rel, got, c.want)
		}
	}

	// An empty root yields a predicate that reports nothing exists, so the
	// caller must gate the guard on repoRootIsDir.
	if repoDirChecker("")("pkg/mills/clients") {
		t.Error("empty-root checker should report nothing exists")
	}
}

func TestRepoRootIsDir(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "go.mod")
	if err := os.WriteFile(file, []byte("module x\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if !repoRootIsDir(root) {
		t.Error("repoRootIsDir(temp dir) = false, want true")
	}
	if repoRootIsDir("") {
		t.Error("repoRootIsDir(\"\") = true, want false")
	}
	if repoRootIsDir(filepath.Join(root, "missing")) {
		t.Error("repoRootIsDir(missing) = true, want false")
	}
	if repoRootIsDir(file) {
		t.Error("repoRootIsDir(file) = true, want false (not a directory)")
	}
}

// mkRepoPkgDirs creates real package directories under repo so grounded
// slice files validate against them.
func mkRepoPkgDirs(t *testing.T, repo string, rels ...string) {
	t.Helper()
	for _, rel := range rels {
		if err := os.MkdirAll(filepath.Join(repo, filepath.FromSlash(rel)), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
	}
}

func TestApply_SliceGuard_KeepsNewDirProposalAsSpeculative(t *testing.T) {
	m, st, repo := newMutatorEnv(t)
	mkRepoPkgDirs(t, repo, "pkg/mills/clients")

	specCtr := mills.CouncilSlicesGuardTotal.WithLabelValues("speculative")
	beforeSpec := testutil.ToFloat64(specCtr)
	beforePaths := testutil.ToFloat64(mills.CouncilSlicePathsDroppedTotal)

	out := &EditorOutput{BacklogProposals: []BacklogProposal{{
		Title:    "New package",
		Priority: store.P2,
		PlanSlices: []PlanSliceSpec{{
			Name:  "new-pkg",
			Goal:  "do the thing",
			Files: []string{"pkg/planning/orchestrator.go"},
		}},
	}}}

	res, err := m.Apply(context.Background(), "COUNCIL-X", out, MutationOptions{RepoRoot: repo})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	// Flag-never-drop: the wholly-new-directory proposal is KEPT and creates an item.
	if len(res.CreatedItems) != 1 {
		t.Fatalf("created %d items, want 1 (new-dir proposal kept)", len(res.CreatedItems))
	}
	if res.SlicesSpeculative != 1 || res.SlicesDropped != 0 || res.FictionalProposalsDropped != 0 {
		t.Errorf("Speculative=%d Dropped=%d FictionalProposalsDropped=%d, want 1/0/0",
			res.SlicesSpeculative, res.SlicesDropped, res.FictionalProposalsDropped)
	}
	items, _ := st.Backlog.List(context.Background())
	if len(items) != 1 {
		t.Fatalf("backlog has %d items, want 1", len(items))
	}
	if got := testutil.ToFloat64(specCtr) - beforeSpec; got != 1 {
		t.Errorf("speculative counter delta = %v, want 1", got)
	}
	if got := testutil.ToFloat64(mills.CouncilSlicePathsDroppedTotal) - beforePaths; got != 1 {
		t.Errorf("paths counter delta = %v, want 1", got)
	}
}

func TestApply_SliceGuard_KeepsRealAndNewDirSlices(t *testing.T) {
	m, st, repo := newMutatorEnv(t)
	mkRepoPkgDirs(t, repo, "pkg/mills/clients")

	out := &EditorOutput{BacklogProposals: []BacklogProposal{{
		Title:    "Partly grounded",
		Priority: store.P2,
		PlanSlices: []PlanSliceSpec{
			{Name: "real", Goal: "g", Files: []string{"pkg/mills/clients/new.go"}},
			{Name: "new-pkg", Goal: "g", Files: []string{"pkg/pipeline/x.go"}},
		},
	}}}

	res, err := m.Apply(context.Background(), "COUNCIL-Y", out, MutationOptions{RepoRoot: repo})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(res.CreatedItems) != 1 {
		t.Fatalf("created %d items, want 1", len(res.CreatedItems))
	}
	item := res.CreatedItems[0]
	// Both slices kept now — the new-directory slice is speculative, not dropped.
	if len(item.Slices) != 2 {
		t.Fatalf("persisted slices = %+v, want both kept", item.Slices)
	}
	if res.SlicesSpeculative != 1 || res.SlicesDropped != 0 || res.FictionalProposalsDropped != 0 {
		t.Errorf("Speculative=%d Dropped=%d FictionalProposalsDropped=%d, want 1/0/0",
			res.SlicesSpeculative, res.SlicesDropped, res.FictionalProposalsDropped)
	}
	stored, err := st.Backlog.Get(context.Background(), item.ID)
	if err != nil {
		t.Fatalf("get persisted item: %v", err)
	}
	if len(stored.Slices) != 2 {
		t.Fatalf("persisted item Slices=%d, want 2", len(stored.Slices))
	}
}

func TestApply_SliceGuard_EmptyRepoRoot_NoOp(t *testing.T) {
	m, st, _ := newMutatorEnv(t)

	out := &EditorOutput{BacklogProposals: []BacklogProposal{{
		Title:    "Ungrounded but allowed",
		Priority: store.P2,
		PlanSlices: []PlanSliceSpec{{
			Name:  "invented",
			Goal:  "g",
			Files: []string{"pkg/planning/orchestrator.go"},
		}},
	}}}

	// RepoRoot intentionally empty → guard inert → the fictional slice persists.
	res, err := m.Apply(context.Background(), "COUNCIL-T", out, MutationOptions{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(res.CreatedItems) != 1 {
		t.Fatalf("created %d items, want 1 (guard inert with empty RepoRoot)", len(res.CreatedItems))
	}
	if res.SlicesDropped != 0 || res.SlicesFlagged != 0 || res.FictionalProposalsDropped != 0 {
		t.Errorf("guard counts = %d/%d/%d, want all 0 with empty RepoRoot",
			res.SlicesDropped, res.SlicesFlagged, res.FictionalProposalsDropped)
	}
	items, _ := st.Backlog.List(context.Background())
	if len(items) != 1 {
		t.Fatalf("backlog has %d items, want 1", len(items))
	}
}
