package pipeline

import (
	"net/url"
	"testing"

	"github.com/crb2nu/loom/pkg/mills/store"
)

func TestBranchContractFor_StraightThrough(t *testing.T) {
	run := &store.PipelineRun{ID: "PIPE-1", BacklogID: "BL X"}
	item := &store.BacklogItem{ID: "BL X"}
	got := BranchContractFor(run, item, Stage{ID: "implement"}, "")
	if got.SourceBranch != "feat/BL-X" {
		t.Errorf("source branch = %q", got.SourceBranch)
	}
	if got.IntegrationBranch != "integrate/BL-X" {
		t.Errorf("integration branch = %q", got.IntegrationBranch)
	}
}

func TestBranchContractFor_RetryIsStableAcrossStagesAndEscalations(t *testing.T) {
	item := &store.BacklogItem{ID: "BL branch/name", Labels: []string{"type/fix"}, Priority: store.P0}
	want := "fix/BL-branch-name/api-changes"
	for _, tc := range []struct {
		name  string
		stage string
		run   *store.PipelineRun
	}{
		{"initial implement", "implement", &store.PipelineRun{ID: "run-1"}},
		{"retry mr", "mr", &store.PipelineRun{ID: "run-1-retry"}},
		{"post-escalation ci", "ci_watch", &store.PipelineRun{ID: "run-1-escalated"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := BranchContractFor(tc.run, item, Stage{ID: tc.stage}, "api changes").SourceBranch
			if got != want {
				t.Fatalf("source branch = %q, want retry-stable %q", got, want)
			}
			escaped := url.PathEscape(got)
			roundTrip, err := url.PathUnescape(escaped)
			if err != nil || roundTrip != got {
				t.Fatalf("PathEscape round trip = %q, %v; want %q", roundTrip, err, got)
			}
		})
	}
}

func TestBranchContractFor_PrefixPrecedenceAndSanitization(t *testing.T) {
	for _, tc := range []struct {
		name     string
		labels   []string
		priority store.Priority
		want     string
	}{
		{"explicit hotfix wins", []string{"type/fix", "branch/hotfix"}, store.P2, "hotfix/BL-item/slice-name"},
		{"fix wins over feature", []string{"feature", "kind/bug"}, store.P2, "fix/BL-item/slice-name"},
		{"feature label", []string{"type/feature"}, store.P2, "feat/BL-item/slice-name"},
		{"p0 fallback", nil, store.P0, "hotfix/BL-item/slice-name"},
		{"feature fallback", nil, store.P2, "feat/BL-item/slice-name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			item := &store.BacklogItem{ID: " BL / item ", Labels: tc.labels, Priority: tc.priority}
			got := BranchContractFor(nil, item, Stage{ID: "mr"}, "slice / name").SourceBranch
			if got != tc.want {
				t.Errorf("source branch = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBranchContractFor_FanOutParentUsesIntegrationBranch(t *testing.T) {
	run := &store.PipelineRun{ID: "PIPE-1", BacklogID: "BL-PAR"}
	item := &store.BacklogItem{
		ID: "BL-PAR",
		Slices: []store.Slice{
			{Name: "api changes", ParallelWith: []string{"ui"}},
			{Name: "ui", ParallelWith: []string{"api changes"}},
		},
	}
	got := BranchContractFor(run, item, Stage{ID: "mr"}, "")
	if got.SourceBranch != "integrate/BL-PAR" {
		t.Errorf("source branch = %q", got.SourceBranch)
	}
}

func TestBranchContractFor_FanOutSliceUsesSliceBranch(t *testing.T) {
	parentID := "PIPE-1"
	run := &store.PipelineRun{ID: "PIPE-1-api", BacklogID: "BL-PAR", ParentSessionID: parentID}
	item := &store.BacklogItem{
		ID:     "BL-PAR",
		Slices: []store.Slice{{Name: "api changes"}},
	}
	got := BranchContractFor(run, item, Stage{ID: "implement"}, "")
	if got.SourceBranch != "feat/BL-PAR/api-changes" {
		t.Errorf("source branch = %q", got.SourceBranch)
	}
	if got.SliceBranch != "feat/BL-PAR/api-changes" {
		t.Errorf("slice branch = %q", got.SliceBranch)
	}
}
