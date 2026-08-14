package pipeline

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

type recordedMRRef struct {
	PlanID    string
	BacklogID string
	MRRef     string
	Files     []string
}

type recordingPlanMRRecorder struct {
	calls     []recordedMRRef
	sliceID   string
	returnErr error
}

func (r *recordingPlanMRRecorder) RecordMRRef(_ context.Context, planID, backlogID, mrRef string, files []string) (string, error) {
	r.calls = append(r.calls, recordedMRRef{PlanID: planID, BacklogID: backlogID, MRRef: mrRef, Files: files})
	return r.sliceID, r.returnErr
}

// The gap this closes: the mr stage created the MR and never told the plan,
// so a plan-linked item's slice kept mr_ref empty, take-up had nothing to
// poll, and the plan never walked to merged (observed 2026-08-01 on
// plan-stamp-loom-runbook-loom-runbook slice #1 after MR !1380 merged).
func TestGitLabWorker_CreateMR_RecordsMRRefOnPlanSlice(t *testing.T) {
	gl := &fakeGitLab{createResp: CreateMRResponse{MRIID: 1380}}
	rec := &recordingPlanMRRecorder{sliceID: "plan-stamp-x#1"}
	w := &GitLabWorker{Client: gl, PlanMRRecorder: rec}
	jc := sampleJobContext("mr", func(jc *JobContext) {
		jc.Item.PlanID = "plan-stamp-x"
		jc.Prior["implement"] = StageOutput{FilesChanged: []string{"docs/runbook.md"}}
	})

	if _, err := w.Run(context.Background(), jc); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(rec.calls) != 1 {
		t.Fatalf("recorder called %d times, want 1", len(rec.calls))
	}
	got := rec.calls[0]
	want := recordedMRRef{
		PlanID:    "plan-stamp-x",
		BacklogID: "BL-X",
		MRRef:     "!1380",
		Files:     []string{"docs/runbook.md"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("recorded %+v, want %+v", got, want)
	}
}

// An adopted MR is still the run's MR: the plan needs the ref either way.
func TestGitLabWorker_CreateMR_RecordsMRRefForAdoptedMR(t *testing.T) {
	gl := &fakeGitLab{createResp: CreateMRResponse{MRIID: 77, Adopted: true}}
	rec := &recordingPlanMRRecorder{sliceID: "plan-x#1"}
	w := &GitLabWorker{Client: gl, PlanMRRecorder: rec}
	jc := sampleJobContext("mr", func(jc *JobContext) { jc.Item.PlanID = "plan-x" })

	if _, err := w.Run(context.Background(), jc); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(rec.calls) != 1 || rec.calls[0].MRRef != "!77" {
		t.Fatalf("calls = %+v, want one !77 record", rec.calls)
	}
}

// Best-effort contract: the MR exists, so a plan-store failure must never turn
// a created MR into a failed stage.
func TestGitLabWorker_CreateMR_PlanRecordFailureDoesNotFailStage(t *testing.T) {
	gl := &fakeGitLab{createResp: CreateMRResponse{MRIID: 1380, URL: "u"}}
	rec := &recordingPlanMRRecorder{returnErr: errors.New("hub unreachable")}
	w := &GitLabWorker{Client: gl, PlanMRRecorder: rec}
	jc := sampleJobContext("mr", func(jc *JobContext) { jc.Item.PlanID = "plan-x" })

	out, err := w.Run(context.Background(), jc)
	if err != nil {
		t.Fatalf("run: %v, want the mr stage to succeed despite the plan write", err)
	}
	if out.MRIID != 1380 {
		t.Fatalf("MRIID = %d, want 1380", out.MRIID)
	}
}

// Items with no plan link have nothing to record; an unwired recorder must
// keep the legacy path intact.
func TestGitLabWorker_CreateMR_SkipsPlanRecordWhenUnlinked(t *testing.T) {
	gl := &fakeGitLab{createResp: CreateMRResponse{MRIID: 1380}}
	rec := &recordingPlanMRRecorder{}
	w := &GitLabWorker{Client: gl, PlanMRRecorder: rec}

	if _, err := w.Run(context.Background(), sampleJobContext("mr")); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(rec.calls) != 0 {
		t.Fatalf("recorder called for an item with no PlanID: %+v", rec.calls)
	}

	plain := &GitLabWorker{Client: &fakeGitLab{createResp: CreateMRResponse{MRIID: 5}}}
	jc := sampleJobContext("mr", func(jc *JobContext) { jc.Item.PlanID = "plan-x" })
	if _, err := plain.Run(context.Background(), jc); err != nil {
		t.Fatalf("run with no recorder wired: %v", err)
	}
}

func TestPriorFilesChanged(t *testing.T) {
	tests := []struct {
		name  string
		prior map[string]StageOutput
		want  []string
	}{
		{
			name: "implement wins",
			prior: map[string]StageOutput{
				"implement": {FilesChanged: []string{"pkg/a.go"}},
				"tests":     {FilesChanged: []string{"pkg/b.go"}},
			},
			want: []string{"pkg/a.go"},
		},
		{
			name: "falls back to a deterministic deduped union",
			prior: map[string]StageOutput{
				"tests":          {FilesChanged: []string{"pkg/b.go", "pkg/a.go"}},
				"implement":      {},
				"pr_self_review": {FilesChanged: []string{"pkg/a.go", "pkg/c.go"}},
			},
			want: []string{"pkg/a.go", "pkg/c.go", "pkg/b.go"},
		},
		{name: "no captures", prior: map[string]StageOutput{}, want: nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := priorFilesChanged(tc.prior); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("priorFilesChanged = %v, want %v", got, tc.want)
			}
		})
	}
}
