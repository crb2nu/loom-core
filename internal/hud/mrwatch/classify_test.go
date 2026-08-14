package mrwatch

import (
	"testing"
	"time"
)

func openMR() MRInfo {
	return MRInfo{
		Repo:                      "services/loom-core",
		IID:                       1,
		Title:                     "feat: thing",
		SourceBranch:              "feat/thing",
		TargetBranch:              "main",
		State:                     "opened",
		MergeWhenPipelineSucceeds: true,
		UpdatedAt:                 time.Now(),
	}
}

func TestClassify_Taxonomy(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	recent := now.Add(-1 * time.Hour)
	old := now.Add(-30 * 24 * time.Hour)

	cases := []struct {
		name       string
		mutate     func(*MRInfo)
		wantState  State
		wantReason string // "" = don't assert reason
	}{
		{
			name: "ok_green_armed",
			mutate: func(m *MRInfo) {
				m.UpdatedAt = recent
				m.Pipeline = &PipelineInfo{Status: "success"}
			},
			wantState:  StateOK,
			wantReason: "",
		},
		{
			name: "awaiting_pipeline_nil_head",
			mutate: func(m *MRInfo) {
				m.UpdatedAt = recent
				m.Pipeline = nil
			},
			wantState:  StateAwaitingPipeline,
			wantReason: "no_head_pipeline",
		},
		{
			name: "ci_running",
			mutate: func(m *MRInfo) {
				m.UpdatedAt = recent
				m.Pipeline = &PipelineInfo{Status: "running"}
			},
			wantState:  StateCIRunning,
			wantReason: "ci_running",
		},
		{
			name: "ci_failed_deterministic_unclassified",
			mutate: func(m *MRInfo) {
				m.UpdatedAt = recent
				m.Pipeline = &PipelineInfo{Status: "failed"}
			},
			wantState:  StateCIFailedDeterministic,
			wantReason: "unclassified",
		},
		{
			name: "ci_failed_deterministic_code_signature",
			mutate: func(m *MRInfo) {
				m.UpdatedAt = recent
				m.Pipeline = &PipelineInfo{
					Status:        "failed",
					FailureReason: "build failed: undefined: someSymbol",
				}
			},
			wantState: StateCIFailedDeterministic,
		},
		{
			name: "ci_failed_flaky_signature",
			mutate: func(m *MRInfo) {
				m.UpdatedAt = recent
				m.Pipeline = &PipelineInfo{
					Status:        "failed",
					FailureReason: flakySignature(),
				}
			},
			wantState: StateCIFailedFlaky,
		},
		{
			name: "conflict_boolean",
			mutate: func(m *MRInfo) {
				m.UpdatedAt = recent
				m.HasConflicts = true
				m.Pipeline = &PipelineInfo{Status: "success"}
			},
			wantState:  StateConflict,
			wantReason: "merge_conflict",
		},
		{
			name: "conflict_detailed_status",
			mutate: func(m *MRInfo) {
				m.UpdatedAt = recent
				m.DetailedMergeStatus = "conflict"
			},
			wantState:  StateConflict,
			wantReason: "merge_conflict",
		},
		{
			name: "automerge_unarmed_green",
			mutate: func(m *MRInfo) {
				m.UpdatedAt = recent
				m.MergeWhenPipelineSucceeds = false
				m.Pipeline = &PipelineInfo{Status: "success"}
			},
			wantState:  StateAutomergeUnarmed,
			wantReason: "mwps_unarmed",
		},
		{
			name: "automerge_unarmed_ci_running",
			mutate: func(m *MRInfo) {
				m.UpdatedAt = recent
				m.MergeWhenPipelineSucceeds = false
				m.Pipeline = &PipelineInfo{Status: "running"}
			},
			wantState:  StateAutomergeUnarmed,
			wantReason: "mwps_unarmed_ci_running",
		},
		{
			name: "pipeline_skipped",
			mutate: func(m *MRInfo) {
				m.UpdatedAt = recent
				m.Pipeline = &PipelineInfo{Status: "skipped"}
			},
			wantState:  StatePipelineSkipped,
			wantReason: "pipeline_skipped",
		},
		{
			name: "stale_branch_green_armed_old",
			mutate: func(m *MRInfo) {
				m.UpdatedAt = old
				m.Pipeline = &PipelineInfo{Status: "success"}
			},
			wantState:  StateStaleBranch,
			wantReason: "branch_stale",
		},
		{
			name: "draft_idle",
			mutate: func(m *MRInfo) {
				m.UpdatedAt = recent
				m.Draft = true
				m.Pipeline = &PipelineInfo{Status: "running"}
			},
			wantState:  StateDraftIdle,
			wantReason: "draft",
		},
		{
			name: "canceled_deterministic",
			mutate: func(m *MRInfo) {
				m.UpdatedAt = recent
				m.Pipeline = &PipelineInfo{Status: "canceled"}
			},
			wantState:  StateCIFailedDeterministic,
			wantReason: "pipeline_canceled",
		},
		{
			// Terminal lifecycles win over everything: a merged MR classifies
			// merged even though its last-known CI/conflict fields would
			// otherwise dominate.
			name: "merged_beats_open_signals",
			mutate: func(m *MRInfo) {
				m.State = "merged"
				m.UpdatedAt = recent
				m.HasConflicts = true
				m.Pipeline = &PipelineInfo{Status: "failed"}
			},
			wantState:  StateMerged,
			wantReason: "merged",
		},
		{
			// Closed-unmerged must be its own class — never merged.
			name: "closed_unmerged_is_closed",
			mutate: func(m *MRInfo) {
				m.State = "closed"
				m.UpdatedAt = recent
				m.Pipeline = &PipelineInfo{Status: "success"}
			},
			wantState:  StateClosed,
			wantReason: "closed",
		},
	}

	seen := make(map[State]bool)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mr := openMR()
			tc.mutate(&mr)
			gotState, gotReason := Classify(mr, now, DefaultStaleAfter)
			if gotState != tc.wantState {
				t.Fatalf("state = %q, want %q (reason %q)", gotState, tc.wantState, gotReason)
			}
			if tc.wantReason != "" && gotReason != tc.wantReason {
				t.Fatalf("reason = %q, want %q", gotReason, tc.wantReason)
			}
			seen[gotState] = true
		})
	}

	// Every taxonomy class must be reachable via the table above.
	for _, s := range AllStates() {
		if !seen[s] {
			t.Errorf("taxonomy class %q not exercised by classification table", s)
		}
	}
}

// TestClassify_UnnameableLifecycleReturnsEmpty: only lifecycles the taxonomy can
// name are classified. locked/unknown/empty stay empty so the registry never
// reports a state it did not derive — merged and closed have their own classes
// and are covered by the taxonomy table.
func TestClassify_UnnameableLifecycleReturnsEmpty(t *testing.T) {
	now := time.Now()
	for _, state := range []string{"locked", "reopened_unknown", ""} {
		mr := openMR()
		mr.State = state
		gotState, gotReason := Classify(mr, now, DefaultStaleAfter)
		if gotState != "" || gotReason != "" {
			t.Errorf("state %q: got (%q,%q), want empty", state, gotState, gotReason)
		}
	}
}

// TestClassify_MergedNeverConflatedWithClosed pins the taxonomy's load-bearing
// distinction: only an MR GitLab reports as merged may yield StateMerged, which
// is the marker the plan-store truth sweep advances on.
func TestClassify_MergedNeverConflatedWithClosed(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		lifecycle string
		want      State
	}{
		{"merged", StateMerged},
		{"MERGED", StateMerged},
		{"closed", StateClosed},
		{" closed ", StateClosed},
		{"locked", ""},
	} {
		mr := openMR()
		mr.State = tc.lifecycle
		got, _ := Classify(mr, now, DefaultStaleAfter)
		if got != tc.want {
			t.Errorf("lifecycle %q classified %q, want %q", tc.lifecycle, got, tc.want)
		}
	}
}

func TestClassify_ConflictBeatsDraftAndCI(t *testing.T) {
	now := time.Now()
	mr := openMR()
	mr.Draft = true
	mr.HasConflicts = true
	mr.Pipeline = &PipelineInfo{Status: "failed"}
	got, _ := Classify(mr, now, DefaultStaleAfter)
	if got != StateConflict {
		t.Fatalf("conflict must win precedence, got %q", got)
	}
}

func TestClassify_ZeroUpdatedAtNotStale(t *testing.T) {
	now := time.Now()
	mr := openMR()
	mr.UpdatedAt = time.Time{} // zero
	mr.Pipeline = &PipelineInfo{Status: "success"}
	got, _ := Classify(mr, now, DefaultStaleAfter)
	if got != StateOK {
		t.Fatalf("zero UpdatedAt must not classify stale; got %q", got)
	}
}

// flakySignature returns a CI failure message the audit classifier flags as
// retryable/transient, so the flaky branch is exercised. It is derived from the
// audit classifier's own recognized signatures; if the audit taxonomy changes,
// this test surfaces it.
func flakySignature() string {
	return "ERROR: Job failed (system failure): runner system failure, will retry"
}
