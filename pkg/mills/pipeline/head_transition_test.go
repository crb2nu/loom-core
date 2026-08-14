package pipeline

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/store"
)

// ----- the ci_transition_seq re-authorization fence (#374 slice 2) -----

func TestRunCI_StampsHeadTransitionFence(t *testing.T) {
	fake := &fakeGitLab{pollResp: testCIPollResponse("success", "tested-head")}
	w := &GitLabWorker{Client: fake}
	jc := sampleJobContext("ci_watch")
	jc.HeadTransitionSeq = 3
	addMRProvenance(&jc, 42, testCIProject, testCISource, testCITarget)

	out, err := w.Run(context.Background(), jc)
	if err != nil {
		t.Fatalf("ci_watch: %v", err)
	}
	got, ok := out.Artifacts[ciTransitionSeqArtifact]
	if !ok {
		t.Fatalf("ci_watch must stamp %s alongside ci_sha; artifacts = %v", ciTransitionSeqArtifact, out.Artifacts)
	}
	if got != int64(3) {
		t.Errorf("%s = %v (%T), want int64(3)", ciTransitionSeqArtifact, got, got)
	}
}

// A run whose head never moved stamps 0, which is exactly what a legacy row
// (no stamp at all) reads as — that equivalence is what makes the fence
// backfill-free.
func TestRunCI_StampsZeroWhenLedgerEmpty(t *testing.T) {
	fake := &fakeGitLab{pollResp: testCIPollResponse("success", "tested-head")}
	w := &GitLabWorker{Client: fake}
	jc := sampleJobContext("ci_watch")
	addMRProvenance(&jc, 42, testCIProject, testCISource, testCITarget)

	out, err := w.Run(context.Background(), jc)
	if err != nil {
		t.Fatalf("ci_watch: %v", err)
	}
	if out.Artifacts[ciTransitionSeqArtifact] != int64(0) {
		t.Errorf("%s = %v, want int64(0)", ciTransitionSeqArtifact, out.Artifacts[ciTransitionSeqArtifact])
	}
}

func mergeJobContext(t *testing.T, ciArtifacts map[string]any, fenceSeq int64) JobContext {
	t.Helper()
	jc := sampleJobContext("merge")
	jc.HeadTransitionSeq = fenceSeq
	addMRProvenance(&jc, 42, testCIProject, testCISource, testCITarget)
	jc.Prior["ci_watch"] = StageOutput{Artifacts: ciArtifacts}
	return jc
}

// The core slice-2 guarantee: a settled head movement advances the run's fence
// past whatever ci_watch stamped, and merge refuses rather than merging on the
// strength of a verdict issued for a SHA GitLab no longer holds.
func TestCIMergeRequestFrom_StaleFenceFailsClosed(t *testing.T) {
	artifacts := testCIArtifacts("tested-head")
	artifacts[ciTransitionSeqArtifact] = int64(1)
	jc := mergeJobContext(t, artifacts, 2)

	_, err := ciMergeRequestFrom(jc, 42)
	if !errors.Is(err, ErrMergeAuthorizationStale) {
		t.Fatalf("err = %v, want ErrMergeAuthorizationStale", err)
	}
	for _, needle := range []string{"head transition 1", "settled 2"} {
		if !strings.Contains(err.Error(), needle) {
			t.Errorf("error must name both fence values (%q missing): %v", needle, err)
		}
	}
}

// A worker that never sees a ledger row must behave exactly as before this
// feature existed. This is the no-backfill guarantee.
func TestCIMergeRequestFrom_LegacyRowsWithoutFenceStillMerge(t *testing.T) {
	jc := mergeJobContext(t, testCIArtifacts("tested-head"), 0)

	req, err := ciMergeRequestFrom(jc, 42)
	if err != nil {
		t.Fatalf("a legacy ci_watch row without %s must still authorize a merge: %v", ciTransitionSeqArtifact, err)
	}
	if req.ExpectedSHA != "tested-head" {
		t.Errorf("expected sha = %q", req.ExpectedSHA)
	}
}

// artifacts_json round-trips an int64 back as a float64 after an operator
// restart. If the comparison were type-strict, every resumed run would
// fail-close its merge.
func TestCIMergeRequestFrom_FenceSurvivesJSONRoundTrip(t *testing.T) {
	for name, stamped := range map[string]any{
		"int64":   int64(2),
		"int":     2,
		"float64": float64(2),
		"string":  "2",
	} {
		t.Run(name, func(t *testing.T) {
			artifacts := testCIArtifacts("tested-head")
			artifacts[ciTransitionSeqArtifact] = stamped
			if _, err := ciMergeRequestFrom(mergeJobContext(t, artifacts, 2), 42); err != nil {
				t.Fatalf("fence stamped as %T must compare equal to int64(2): %v", stamped, err)
			}
		})
	}
}

// The fence composes with, rather than replaces, the existing merge-recovery
// authorization checks: a complete tuple at a matching fence still merges, and
// an incomplete tuple still fails closed regardless of the fence.
func TestCIMergeRequestFrom_FenceComposesWithIdentityChecks(t *testing.T) {
	artifacts := testCIArtifacts("tested-head")
	artifacts[ciTransitionSeqArtifact] = int64(4)
	req, err := ciMergeRequestFrom(mergeJobContext(t, artifacts, 4), 42)
	if err != nil {
		t.Fatalf("matching fence must authorize: %v", err)
	}
	if req.ExpectedSHA != "tested-head" || req.Project != testCIProject {
		t.Errorf("authorization tuple = %+v", req)
	}

	delete(artifacts, "ci_sha")
	if _, err := ciMergeRequestFrom(mergeJobContext(t, artifacts, 4), 42); !errors.Is(err, ErrMergeAuthorizationStale) {
		t.Fatalf("a missing ci_sha must still fail closed at a matching fence: %v", err)
	}
}

// ----- external head-movement ledger + rewind (runner) -----

const (
	htReviewedSHA  = "1111111111111111111111111111111111111111"
	htSuccessorSHA = "2222222222222222222222222222222222222222"
)

func headMovedError() *MergeSourceSHAMismatchError {
	return &MergeSourceSHAMismatchError{
		MRIID:        77,
		Project:      testCIProject,
		SourceBranch: testCISource,
		TargetBranch: testCITarget,
		ReviewedSHA:  htReviewedSHA,
		ObservedSHA:  htSuccessorSHA,
		Message:      "mr 77 source sha \"" + htSuccessorSHA + "\" no longer matches CI-authorized source sha \"" + htReviewedSHA + "\": pipeline: merge authorization is stale",
	}
}

func headMovementRunner(t *testing.T, st *store.Store) (*Runner, *fakeDispatcher) {
	t.Helper()
	disp := &fakeDispatcher{
		canned: map[string]StageOutput{
			"implement": {
				FilesChanged:   []string{"foo.go"},
				DiffPatch:      []byte("diff --git a/foo.go b/foo.go\n+x\n"),
				CommitMessages: []string{"feat: x"},
			},
			"mr": {MRIID: 77},
		},
		errFor: map[string]error{"merge": headMovedError()},
	}
	r := New(st, newPassingGates(t), disp, nil)
	r.Clock = func() time.Time { return time.Date(2026, 7, 25, 16, 0, 0, 0, time.UTC) }
	return r, disp
}

// Slice 1's minting contract + slice 2's rewind, end to end through Drive:
// a merge-stage source-sha mismatch becomes exactly one durable ledger row and
// rewinds the run to the first source-sensitive stage instead of escalating.
func TestRunner_ExternalHeadMovementMintsLedgerRowAndRewinds(t *testing.T) {
	st, run, item := newRunnerEnv(t)
	r, _ := headMovementRunner(t, st)
	ctx := context.Background()

	if err := r.Drive(ctx, run, item); err != nil {
		t.Fatalf("drive: %v", err)
	}

	rows, err := st.MRHeadTransitions.ListByRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("list transitions: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("ledger rows = %d, want exactly 1", len(rows))
	}
	row := rows[0]
	if row.Trigger != store.MRHeadTriggerExternal {
		t.Errorf("trigger = %q, want external", row.Trigger)
	}
	// An unrequested movement carries no evidence tying it to anything Mills
	// did, so it can never be attributed.
	if row.State != store.MRHeadTransitionAmbiguous {
		t.Errorf("state = %q, want ambiguous", row.State)
	}
	if row.ReviewedSHA != htReviewedSHA || row.SuccessorSHA != htSuccessorSHA {
		t.Errorf("row must name both SHAs: %+v", row)
	}
	if row.SettledAt == nil {
		t.Error("an observed movement is terminal on sight; settled_at must be stamped")
	}
	if row.MRIID != 77 || row.Project != testCIProject {
		t.Errorf("row identity = %+v", row)
	}

	got, err := st.Pipeline.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if got.CurrentStage != headTransitionRewindStage {
		t.Errorf("current_stage = %q, want %q", got.CurrentStage, headTransitionRewindStage)
	}
	if got.State != store.PipelineImplementing {
		t.Errorf("state = %q, want implementing (the run must stay drivable)", got.State)
	}
	if got.State == store.PipelineEscalated {
		t.Error("the first head movement must re-gate, not escalate")
	}

	// The fence now reads 1, so the ci_watch authorization stamped at 0 can
	// no longer authorize a merge even if the rewind were skipped.
	seq, err := st.MRHeadTransitions.MaxSettledSeq(ctx, run.ID)
	if err != nil {
		t.Fatalf("max settled seq: %v", err)
	}
	if seq != 1 {
		t.Errorf("fence = %d, want 1", seq)
	}
}

// resumeIndex must land on the rewind stage, so the stages that actually read
// branch content re-run and plan_slice/research/implement do not.
func TestRunner_HeadTransitionRewindResumesAtSourceSensitiveStage(t *testing.T) {
	st, run, item := newRunnerEnv(t)
	r, disp := headMovementRunner(t, st)
	ctx := context.Background()

	if err := r.Drive(ctx, run, item); err != nil {
		t.Fatalf("drive: %v", err)
	}
	first := disp.callsList()
	idx, err := r.resumeIndex(run)
	if err != nil {
		t.Fatalf("resume index: %v", err)
	}
	if r.Stages[idx].ID != headTransitionRewindStage {
		t.Fatalf("resume stage = %q, want %q", r.Stages[idx].ID, headTransitionRewindStage)
	}

	// A second drive re-runs the source-sensitive tail only.
	disp.calls = nil
	_ = r.Drive(ctx, run, item)
	for _, replayed := range disp.callsList() {
		switch replayed {
		case "plan_slice", "research", "implement":
			t.Errorf("stage %q must not re-run on a head-transition rewind; the code is the same work on a new base", replayed)
		}
	}
	for _, want := range []string{"tests", "mr", "ci_watch", "merge"} {
		if !containsString(disp.callsList(), want) {
			t.Errorf("stage %q must re-run for the successor sha (first drive: %v)", want, first)
		}
	}
}

// One rebase per run, then a human looks. Without the budget a rebase↔push
// ping-pong would re-gate forever.
func TestRunner_HeadTransitionBudgetExhaustionEscalates(t *testing.T) {
	st, run, item := newRunnerEnv(t)
	r, _ := headMovementRunner(t, st)
	ctx := context.Background()

	if err := r.Drive(ctx, run, item); err != nil {
		t.Fatalf("first drive: %v", err)
	}
	if err := r.Drive(ctx, run, item); err != nil {
		t.Fatalf("second drive: %v", err)
	}

	rows, err := st.MRHeadTransitions.ListByRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("list transitions: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("ledger rows = %d, want 2 (one row per movement)", len(rows))
	}
	got, err := st.Pipeline.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if got.State != store.PipelineEscalated {
		t.Fatalf("state = %q, want escalated once the transition budget is spent", got.State)
	}
}

func TestRunner_HeadTransitionBudgetIsConfigurable(t *testing.T) {
	t.Setenv(headTransitionBudgetEnv, "2")
	st, run, item := newRunnerEnv(t)
	r, _ := headMovementRunner(t, st)
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		if err := r.Drive(ctx, run, item); err != nil {
			t.Fatalf("drive %d: %v", i+1, err)
		}
	}
	got, err := st.Pipeline.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if got.State == store.PipelineEscalated {
		t.Fatalf("budget 2 must absorb two movements; state = %q", got.State)
	}
	if err := r.Drive(ctx, run, item); err != nil {
		t.Fatalf("third drive: %v", err)
	}
	got, err = st.Pipeline.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if got.State != store.PipelineEscalated {
		t.Errorf("state = %q, want escalated on the third movement", got.State)
	}
}

func TestMaxHeadTransitions_DefaultsAndOverrides(t *testing.T) {
	if got := maxHeadTransitions(nil); got != headTransitionDefaultBudget {
		t.Errorf("default = %d, want %d", got, headTransitionDefaultBudget)
	}
	if got := maxHeadTransitions(map[string]string{headTransitionBudgetEnv: "5"}); got != 5 {
		t.Errorf("stage env override = %d, want 5", got)
	}
	// A nonsense or non-positive value must not silently unbound the loop.
	for _, raw := range []string{"", "banana", "0", "-3"} {
		if got := maxHeadTransitions(map[string]string{headTransitionBudgetEnv: raw}); got != headTransitionDefaultBudget {
			t.Errorf("budget for %q = %d, want the default %d", raw, got, headTransitionDefaultBudget)
		}
	}
	t.Setenv(headTransitionBudgetEnv, "4")
	if got := maxHeadTransitions(nil); got != 4 {
		t.Errorf("process env override = %d, want 4", got)
	}
}

// An operator that died between minting the row and settling it must settle
// THAT row on the next drive — one movement is one ledger row, forever.
func TestRunner_HeadTransitionRewindIsRestartSafe(t *testing.T) {
	st, run, item := newRunnerEnv(t)
	r, _ := headMovementRunner(t, st)
	ctx := context.Background()

	// Simulate the crash window: a row exists, unsettled.
	opened, err := st.MRHeadTransitions.Open(ctx, &store.MRHeadTransition{
		PipelineRunID: run.ID,
		Project:       testCIProject,
		MRIID:         77,
		SourceBranch:  testCISource,
		TargetBranch:  testCITarget,
		ReviewedSHA:   htReviewedSHA,
		Trigger:       store.MRHeadTriggerRebaseRequest,
		State:         store.MRHeadTransitionInProgress,
	})
	if err != nil {
		t.Fatalf("seed open row: %v", err)
	}

	if err := r.Drive(ctx, run, item); err != nil {
		t.Fatalf("drive: %v", err)
	}

	rows, err := st.MRHeadTransitions.ListByRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("list transitions: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("ledger rows = %d, want 1 — a resumed drive must settle the open row, not mint a second", len(rows))
	}
	if rows[0].Seq != opened.Seq {
		t.Errorf("settled seq = %d, want the pre-existing %d", rows[0].Seq, opened.Seq)
	}
	if rows[0].State != store.MRHeadTransitionAmbiguous || rows[0].SettledAt == nil {
		t.Errorf("open row must be settled: %+v", rows[0])
	}
	if rows[0].SuccessorSHA != htSuccessorSHA {
		t.Errorf("successor = %q, want %q", rows[0].SuccessorSHA, htSuccessorSHA)
	}

	got, err := st.Pipeline.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if got.CurrentStage != headTransitionRewindStage {
		t.Errorf("current_stage = %q, want %q", got.CurrentStage, headTransitionRewindStage)
	}
}

// A run that has already settled a movement stamps the advanced fence on its
// NEXT ci_watch, so the fresh authorization and the ledger agree and the merge
// is allowed through.
func TestRunner_CIWatchStampsAdvancedFenceAfterMovement(t *testing.T) {
	st, run, item := newRunnerEnv(t)
	ctx := context.Background()
	if _, err := st.MRHeadTransitions.Open(ctx, &store.MRHeadTransition{
		PipelineRunID: run.ID,
		Project:       testCIProject,
		MRIID:         77,
		SourceBranch:  testCISource,
		TargetBranch:  testCITarget,
		ReviewedSHA:   htReviewedSHA,
		SuccessorSHA:  htSuccessorSHA,
		Trigger:       store.MRHeadTriggerExternal,
		State:         store.MRHeadTransitionAmbiguous,
	}); err != nil {
		t.Fatalf("seed settled movement: %v", err)
	}

	disp := &fakeDispatcher{canned: map[string]StageOutput{
		"implement": {
			FilesChanged:   []string{"foo.go"},
			DiffPatch:      []byte("diff --git a/foo.go b/foo.go\n+x\n"),
			CommitMessages: []string{"feat: x"},
		},
		"mr":       {MRIID: 77},
		"ci_watch": {Artifacts: map[string]any{"ci_sha": htSuccessorSHA}},
		"merge":    {MergedSHA: "merged"},
	}}
	r := New(st, newPassingGates(t), disp, nil)
	if err := r.Drive(ctx, run, item); err != nil {
		t.Fatalf("drive: %v", err)
	}

	// The runner threads the fence onto JobContext; assert the value the
	// dispatcher observed at each of the two fenced stages.
	if disp.seenFence["ci_watch"] != 1 {
		t.Errorf("ci_watch fence = %d, want 1", disp.seenFence["ci_watch"])
	}
	if disp.seenFence["merge"] != 1 {
		t.Errorf("merge fence = %d, want 1", disp.seenFence["merge"])
	}
	// Stages that neither issue nor consume a CI authorization are not fenced.
	if disp.seenFence["implement"] != 0 {
		t.Errorf("implement fence = %d, want 0", disp.seenFence["implement"])
	}
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
