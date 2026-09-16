package pipeline

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// fakeMergeQueue scripts the queue's contract. statuses are consumed one per
// Status call; the last one repeats.
type fakeMergeQueue struct {
	enqueued []MergeQueueCandidate
	statuses []mergeQueueStatusStep
	reads    int
}

type mergeQueueStatusStep struct {
	st  MergeQueueStatus
	err error
}

func (f *fakeMergeQueue) Enqueue(_ context.Context, c MergeQueueCandidate) error {
	f.enqueued = append(f.enqueued, c)
	return nil
}

func (f *fakeMergeQueue) Status(context.Context, string) (MergeQueueStatus, error) {
	i := f.reads
	if i >= len(f.statuses) {
		i = len(f.statuses) - 1
	}
	f.reads++
	step := f.statuses[i]
	return step.st, step.err
}

func queueWorker(fake *fakeMergeQueue, gitlab *fakeGitLab, enabled bool) *GitLabWorker {
	return &GitLabWorker{
		Client:                 gitlab,
		MergeQueue:             fake,
		MergeQueueEnabled:      func() bool { return enabled },
		MergeQueuePollInterval: time.Millisecond,
	}
}

// Queue mode: the stage validates the ci_watch authorization once, enqueues
// the exact tuple, waits, and reports the queue's merged SHA.
func TestRunMerge_QueueModeEnqueuesAndReportsMerge(t *testing.T) {
	fq := &fakeMergeQueue{statuses: []mergeQueueStatusStep{
		{err: ErrMergeQueueUnknownRun}, // pre-enqueue probe
		{st: MergeQueueStatus{State: "queued", Position: 2}},
		{st: MergeQueueStatus{State: "merged", Terminal: true, Merged: true, MergedSHA: "queue-merged"}},
	}}
	gl := &fakeGitLab{}
	w := queueWorker(fq, gl, true)
	jc := mergeJobContext(t, testCIArtifacts("tested-head"), 0)

	out, err := w.Run(context.Background(), jc)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if out.MergedSHA != "queue-merged" {
		t.Fatalf("MergedSHA = %q, want queue-merged", out.MergedSHA)
	}
	if out.Artifacts["merged_via"] != "merge_queue" {
		t.Fatalf("merged_via artifact missing: %+v", out.Artifacts)
	}
	if len(gl.mergeCalls) != 0 {
		t.Fatalf("queue mode must not merge directly")
	}
	if len(fq.enqueued) != 1 {
		t.Fatalf("expected exactly one enqueue, got %d", len(fq.enqueued))
	}
	c := fq.enqueued[0]
	if c.SHA != "tested-head" || c.Project != testCIProject || c.SourceBranch != testCISource || c.TargetBranch != testCITarget || c.MRIID != 42 {
		t.Fatalf("candidate must carry the exact ci_watch authorization: %+v", c)
	}
	if c.RunID == "" {
		t.Fatalf("candidate must carry the run id")
	}
}

// An eviction surfaces as a stage error naming the distinct reason — the
// run's normal escalation path takes it from there.
func TestRunMerge_QueueEvictionEscalates(t *testing.T) {
	fq := &fakeMergeQueue{statuses: []mergeQueueStatusStep{
		{err: ErrMergeQueueUnknownRun},
		{st: MergeQueueStatus{State: "evicted", Terminal: true, EvictionReason: "ci_red", Detail: "pipeline 9 failed"}},
	}}
	w := queueWorker(fq, &fakeGitLab{}, true)
	jc := mergeJobContext(t, testCIArtifacts("tested-head"), 0)

	_, err := w.Run(context.Background(), jc)
	if err == nil {
		t.Fatalf("eviction must fail the stage")
	}
	for _, needle := range []string{"ci_red", "pipeline 9 failed"} {
		if !strings.Contains(err.Error(), needle) {
			t.Errorf("eviction error must carry %q: %v", needle, err)
		}
	}
}

// A full lane refuses at enqueue and the stage escalates immediately.
func TestRunMerge_QueueFullEscalatesImmediately(t *testing.T) {
	fq := &fakeMergeQueue{statuses: []mergeQueueStatusStep{{err: ErrMergeQueueUnknownRun}}}
	w := queueWorker(fq, &fakeGitLab{}, true)
	w.MergeQueue = &fullMergeQueue{inner: fq}
	jc := mergeJobContext(t, testCIArtifacts("tested-head"), 0)

	_, err := w.Run(context.Background(), jc)
	if !errors.Is(err, ErrMergeQueueFull) || !strings.Contains(err.Error(), "queue_full") {
		t.Fatalf("expected queue_full escalation, got %v", err)
	}
}

type fullMergeQueue struct{ inner *fakeMergeQueue }

func (f *fullMergeQueue) Enqueue(context.Context, MergeQueueCandidate) error {
	return ErrMergeQueueFull
}
func (f *fullMergeQueue) Status(ctx context.Context, runID string) (MergeQueueStatus, error) {
	return f.inner.Status(ctx, runID)
}

// With the policy fence off (or no queue wired) the stage keeps the exact
// pre-queue direct-merge behaviour.
func TestRunMerge_QueueDisabledUsesDirectPath(t *testing.T) {
	fq := &fakeMergeQueue{statuses: []mergeQueueStatusStep{{err: ErrMergeQueueUnknownRun}}}
	gl := &fakeGitLab{mergeResp: MergeResponse{MergedSHA: "direct-merged"}}
	w := queueWorker(fq, gl, false)
	jc := mergeJobContext(t, testCIArtifacts("tested-head"), 0)

	out, err := w.Run(context.Background(), jc)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if out.MergedSHA != "direct-merged" || len(gl.mergeCalls) != 1 {
		t.Fatalf("expected direct merge, got %+v calls=%d", out, len(gl.mergeCalls))
	}
	if len(fq.enqueued) != 0 {
		t.Fatalf("disabled queue must never see an enqueue")
	}
}

// Resume ordering: a run with an EXISTING queue entry must not re-validate
// the ci_watch authorization — the queue's own rebase legitimately advances
// the #374 fence, and re-validation would fail-close a healthy queued run.
func TestRunMerge_QueueResumeSkipsStaleFenceRevalidation(t *testing.T) {
	fq := &fakeMergeQueue{statuses: []mergeQueueStatusStep{
		// Entry already exists (restart mid-queue): first probe returns it.
		{st: MergeQueueStatus{State: "merging", Position: 1}},
		{st: MergeQueueStatus{State: "merged", Terminal: true, Merged: true, MergedSHA: "resumed-merge"}},
	}}
	w := queueWorker(fq, &fakeGitLab{}, true)
	// Stale fence: ci_watch stamped seq 0, but the queue's rebase settled seq 1.
	jc := mergeJobContext(t, testCIArtifacts("tested-head"), 1)

	out, err := w.Run(context.Background(), jc)
	if err != nil {
		t.Fatalf("resumed queue merge must succeed despite the advanced fence: %v", err)
	}
	if out.MergedSHA != "resumed-merge" {
		t.Fatalf("MergedSHA = %q", out.MergedSHA)
	}
	if len(fq.enqueued) != 0 {
		t.Fatalf("resume must not re-enqueue")
	}
}

// A1: evictions surface as typed errors — head_moved reconstructs the
// runner-rewindable *MergeSourceSHAMismatchError, everything else the
// classifiable *MergeQueueEvictedError.
func TestRunMerge_QueueEvictionTypedErrors(t *testing.T) {
	fq := &fakeMergeQueue{statuses: []mergeQueueStatusStep{
		{err: ErrMergeQueueUnknownRun},
		{st: MergeQueueStatus{
			State: "evicted", Terminal: true, EvictionReason: "head_moved",
			Detail:       "head moved externally while queued: authorized aaa, observed bbb",
			Project:      "services/loom-core",
			SourceBranch: "feat/x", TargetBranch: "main",
			AuthorizedSHA: "aaa-full", ObservedSHA: "bbb-full",
		}},
	}}
	w := queueWorker(fq, &fakeGitLab{}, true)
	jc := mergeJobContext(t, testCIArtifacts("tested-head"), 0)

	_, err := w.Run(context.Background(), jc)
	var moved *MergeSourceSHAMismatchError
	if !errors.As(err, &moved) {
		t.Fatalf("head_moved eviction must be a MergeSourceSHAMismatchError, got %T: %v", err, err)
	}
	if moved.ReviewedSHA != "aaa-full" || moved.ObservedSHA != "bbb-full" || moved.Project != "services/loom-core" {
		t.Fatalf("mismatch fields = %+v", moved)
	}
	if !strings.Contains(moved.Error(), "head moved externally") {
		t.Fatalf("historical text lost: %v", moved.Error())
	}

	// head_moved WITHOUT a recorded successor (pre-031 rows) stays a typed
	// eviction — classified, never a phantom rewind on empty SHAs.
	fq2 := &fakeMergeQueue{statuses: []mergeQueueStatusStep{
		{err: ErrMergeQueueUnknownRun},
		{st: MergeQueueStatus{State: "evicted", Terminal: true, EvictionReason: "head_moved", Detail: "legacy row"}},
	}}
	w2 := queueWorker(fq2, &fakeGitLab{}, true)
	_, err2 := w2.Run(context.Background(), mergeJobContext(t, testCIArtifacts("tested-head"), 0))
	var evicted *MergeQueueEvictedError
	if !errors.As(err2, &evicted) || evicted.Reason != "head_moved" {
		t.Fatalf("legacy head_moved must be MergeQueueEvictedError, got %T: %v", err2, err2)
	}
}

// A1: per-reason classification stops evictions burning code-class budgets
// on no-op retries.
func TestClassify_MergeQueueEvictions(t *testing.T) {
	cases := []struct {
		reason string
		want   ErrorClass
	}{
		{"ci_timeout", ClassInfra},
		{"queue_full", ClassInfra},
		{"mr_closed", ClassConfig},
		{"ci_red", ClassCode},
		{"rebase_conflict", ClassCode},
		{"rebase_ambiguous", ClassCode},
	}
	for _, tc := range cases {
		err := fmt.Errorf("merge stage: %w", &MergeQueueEvictedError{MRIID: 7, Reason: tc.reason, Detail: "x"})
		if got := Classify(err); got != tc.want {
			t.Errorf("Classify(%s) = %s, want %s", tc.reason, got, tc.want)
		}
	}
	// merge_failed defers to the needle classifiers via Detail text.
	err405 := &MergeQueueEvictedError{MRIID: 7, Reason: "merge_failed", Detail: "merge mr 7: status 405"}
	if got := Classify(err405); got != ClassConfig {
		t.Errorf("Classify(merge_failed/405) = %s, want %s", got, ClassConfig)
	}
}
