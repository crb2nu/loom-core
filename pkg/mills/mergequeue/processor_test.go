package mergequeue

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/pipeline"
	"github.com/crb2nu/loom/pkg/mills/store"
)

// ----- fixtures -----

func newQueueStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), store.Options{
		Path: filepath.Join(t.TempDir(), "mills.db"),
	})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func seedRun(t *testing.T, st *store.Store, id string) string {
	t.Helper()
	ctx := context.Background()
	item := &store.BacklogItem{ID: "BL-" + id, Title: "fixture", State: store.BacklogRunning, Priority: store.P2}
	if err := st.Backlog.Put(ctx, item); err != nil {
		t.Fatalf("seed backlog: %v", err)
	}
	run := &store.PipelineRun{
		ID: "PIPE-" + id, BacklogID: item.ID, Template: "mills-default-pipeline",
		State: store.PipelineMerging, Attempts: 1,
		StartedAt: time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC),
	}
	if err := st.Pipeline.PutRun(ctx, run); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	return run.ID
}

func enqueue(t *testing.T, st *store.Store, runID, sha string) *store.MergeQueueEntry {
	t.Helper()
	e, _, err := st.MergeQueue.Enqueue(context.Background(), &store.MergeQueueEntry{
		PipelineRunID: runID, BacklogID: "BL-x", Project: "services/loom-core",
		MRIID: 42, SourceBranch: "feat/x", TargetBranch: "main", EnqueuedSHA: sha,
	}, 10)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	return e
}

// fakeForge scripts the GitLab surface. Zero-value fields mean "test never
// reaches that call".
type fakeForge struct {
	snapshot     MRSnapshot
	snapshotErr  error
	tip          string
	rebaseCalls  int
	rebaseErr    error
	observation  pipeline.HeadObservation
	pipelineStat PipelineStatus
	created      *PipelineStatus
	mergeCalls   int
	mergeArgs    pipeline.MergeRequestArgs
	mergeResp    pipeline.MergeResponse
	mergeErr     error
	calls        []string
}

func (f *fakeForge) MRSnapshot(context.Context, int64) (MRSnapshot, error) {
	f.calls = append(f.calls, "snapshot")
	return f.snapshot, f.snapshotErr
}
func (f *fakeForge) BranchTip(context.Context, string) (string, error) {
	f.calls = append(f.calls, "tip")
	return f.tip, nil
}
func (f *fakeForge) RequestRebase(context.Context, int64) error {
	f.calls = append(f.calls, "rebase")
	f.rebaseCalls++
	return f.rebaseErr
}
func (f *fakeForge) ReadHeadCursors(context.Context, pipeline.HeadCursorRequest) (pipeline.HeadCursors, error) {
	f.calls = append(f.calls, "cursors")
	return pipeline.HeadCursors{VersionsCursor: 7, EventsCursor: 9}, nil
}
func (f *fakeForge) ObserveHead(context.Context, pipeline.HeadObservationRequest) (pipeline.HeadObservation, error) {
	f.calls = append(f.calls, "observe")
	return f.observation, nil
}
func (f *fakeForge) BranchPipelineStatus(context.Context, string, string) (PipelineStatus, error) {
	f.calls = append(f.calls, "pipeline")
	return f.pipelineStat, nil
}
func (f *fakeForge) CreateQueuePipeline(context.Context, string) (PipelineStatus, error) {
	f.calls = append(f.calls, "create")
	if f.created != nil {
		return *f.created, nil
	}
	return PipelineStatus{}, fmt.Errorf("unexpected create")
}
func (f *fakeForge) Merge(_ context.Context, req pipeline.MergeRequestArgs) (pipeline.MergeResponse, error) {
	f.calls = append(f.calls, "merge")
	f.mergeCalls++
	f.mergeArgs = req
	return f.mergeResp, f.mergeErr
}

func newProcessor(st *store.Store, f *fakeForge) *Processor {
	return &Processor{
		Store:      st,
		ForProject: func(string) Forge { return f },
		Enabled:    func() bool { return true },
	}
}

func drive(t *testing.T, p *Processor, st *store.Store, runID string) *store.MergeQueueEntry {
	t.Helper()
	ctx := context.Background()
	e, err := st.MergeQueue.Get(ctx, runID)
	if err != nil {
		t.Fatalf("get entry: %v", err)
	}
	if err := p.driveHead(ctx, e); err != nil {
		t.Fatalf("drive from %s: %v", e.State, err)
	}
	e, err = st.MergeQueue.Get(ctx, runID)
	if err != nil {
		t.Fatalf("re-get entry: %v", err)
	}
	return e
}

// ----- tests -----

// An up-to-date head (base == target tip) merges with NO rebase and no fresh
// pipeline — the run's own ci_watch verdict is the proof.
func TestProcessor_UpToDateHeadMergesDirectly(t *testing.T) {
	st := newQueueStore(t)
	runID := seedRun(t, st, "fast")
	enqueue(t, st, runID, "sha-head")

	f := &fakeForge{
		snapshot:  MRSnapshot{SHA: "sha-head", State: "opened", BaseSHA: "tip-1"},
		tip:       "tip-1",
		mergeResp: pipeline.MergeResponse{MergedSHA: "merged-1"},
	}
	p := newProcessor(st, f)

	e := drive(t, p, st, runID) // queued → merging
	if e.State != store.MergeQueueMerging {
		t.Fatalf("expected merging, got %s", e.State)
	}
	e = drive(t, p, st, runID) // merging → merged
	if e.State != store.MergeQueueMerged || e.MergedSHA != "merged-1" {
		t.Fatalf("expected merged, got %+v", e)
	}
	if f.rebaseCalls != 0 {
		t.Fatalf("rebase must not be invoked for an up-to-date head")
	}
	if f.mergeArgs.ExpectedSHA != "sha-head" {
		t.Fatalf("merge must be SHA-preconditioned on the authorized head, got %q", f.mergeArgs.ExpectedSHA)
	}
}

// A behind head is rebased (with a durable #374 ledger row), its rebased head
// re-proven by a fresh pipeline, then merged on the NEW sha; the next lane
// head is promoted after settle.
func TestProcessor_BehindHeadRebasesProvesThenMerges(t *testing.T) {
	st := newQueueStore(t)
	runID := seedRun(t, st, "behind")
	enqueue(t, st, runID, "sha-old")
	nextRun := seedRun(t, st, "behind-next")
	st2 := enqueue(t, st, nextRun, "sha-next")
	_ = st2

	f := &fakeForge{
		snapshot:    MRSnapshot{SHA: "sha-old", State: "opened", BaseSHA: "base-stale"},
		tip:         "tip-2",
		observation: pipeline.HeadObservation{Verdict: pipeline.HeadVerdictAttributed, SuccessorSHA: "sha-rebased"},
		pipelineStat: PipelineStatus{
			ID: 77, SHA: "sha-rebased", Status: "success", Found: true,
		},
		mergeResp: pipeline.MergeResponse{MergedSHA: "merged-2"},
	}
	p := newProcessor(st, f)
	ctx := context.Background()

	e := drive(t, p, st, runID) // queued → rebasing
	if e.State != store.MergeQueueRebasing || f.rebaseCalls != 1 {
		t.Fatalf("expected rebasing after 1 rebase call, got %s calls=%d", e.State, f.rebaseCalls)
	}
	e = drive(t, p, st, runID) // rebasing → awaiting_pipeline
	if e.State != store.MergeQueueAwaitingPipeline || e.CurrentSHA != "sha-rebased" {
		t.Fatalf("expected awaiting on rebased head, got %+v", e)
	}
	e = drive(t, p, st, runID) // awaiting → merging
	if e.State != store.MergeQueueMerging {
		t.Fatalf("expected merging, got %s", e.State)
	}
	e = drive(t, p, st, runID) // merging → merged
	if e.State != store.MergeQueueMerged {
		t.Fatalf("expected merged, got %s", e.State)
	}
	if f.mergeArgs.ExpectedSHA != "sha-rebased" {
		t.Fatalf("merge must be preconditioned on the REBASED head, got %q", f.mergeArgs.ExpectedSHA)
	}

	// The rebase is a settled attributed row in the #374 ledger.
	rows, err := st.MRHeadTransitions.ListByRun(ctx, runID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("ledger rows: %+v err=%v", rows, err)
	}
	if rows[0].State != store.MRHeadTransitionAttributed || rows[0].SuccessorSHA != "sha-rebased" {
		t.Fatalf("unexpected ledger row: %+v", rows[0])
	}

	// Lane head is promoted to the next run.
	heads, err := st.MergeQueue.Heads(ctx)
	if err != nil || len(heads) != 1 || heads[0].PipelineRunID != nextRun {
		t.Fatalf("expected promoted head %s, got %+v err=%v", nextRun, heads, err)
	}
}

// A red pipeline on the rebased head evicts ci_red; a failed rebase evicts
// rebase_conflict. Distinct reasons, both terminal, both escalation-visible.
func TestProcessor_EvictionsCarryDistinctReasons(t *testing.T) {
	st := newQueueStore(t)

	// ci_red
	redRun := seedRun(t, st, "red")
	redEntry := enqueue(t, st, redRun, "sha-a")
	if _, err := st.MergeQueue.Transition(context.Background(), store.MergeQueueTransition{
		ID: redEntry.ID, From: store.MergeQueueQueued, To: store.MergeQueueAwaitingPipeline,
	}); err != nil {
		t.Fatalf("prep: %v", err)
	}
	f := &fakeForge{pipelineStat: PipelineStatus{ID: 9, Status: "failed", Found: true}}
	e := drive(t, newProcessor(st, f), st, redRun)
	if e.State != store.MergeQueueEvicted || e.EvictionReason != store.MergeQueueEvictCIRed {
		t.Fatalf("expected ci_red eviction, got %+v", e)
	}

	// rebase_conflict
	confRun := seedRun(t, st, "conflict")
	confEntry := enqueue(t, st, confRun, "sha-b")
	confEntry2, err := st.MergeQueue.Transition(context.Background(), store.MergeQueueTransition{
		ID: confEntry.ID, From: store.MergeQueueQueued, To: store.MergeQueueRebasing,
	})
	if err != nil {
		t.Fatalf("prep: %v", err)
	}
	_ = confEntry2
	f2 := &fakeForge{observation: pipeline.HeadObservation{
		Verdict: pipeline.HeadVerdictFailed, Reason: "merge_error after settle", MergeError: "conflict",
	}}
	e = drive(t, newProcessor(st, f2), st, confRun)
	if e.State != store.MergeQueueEvicted || e.EvictionReason != store.MergeQueueEvictRebaseConflict {
		t.Fatalf("expected rebase_conflict eviction, got %+v", e)
	}
}

// The policy fence halts the tick before any forge call, and entries survive
// untouched for the next enabled tick.
func TestProcessor_DisabledFenceHaltsWithoutLosingState(t *testing.T) {
	st := newQueueStore(t)
	runID := seedRun(t, st, "fence")
	enqueue(t, st, runID, "sha-f")

	f := &fakeForge{}
	p := newProcessor(st, f)
	p.Enabled = func() bool { return false }
	p.tick(context.Background())

	if len(f.calls) != 0 {
		t.Fatalf("disabled tick must not touch the forge, saw %v", f.calls)
	}
	e, err := st.MergeQueue.Get(context.Background(), runID)
	if err != nil || e.State != store.MergeQueueQueued {
		t.Fatalf("entry must survive the fence untouched: %+v err=%v", e, err)
	}
}

// An externally-merged MR settles the entry merged (success for the waiting
// run); an externally-closed MR evicts mr_closed.
func TestProcessor_ExternalTerminalStates(t *testing.T) {
	st := newQueueStore(t)

	mergedRun := seedRun(t, st, "ext-merged")
	enqueue(t, st, mergedRun, "sha-m")
	f := &fakeForge{snapshot: MRSnapshot{SHA: "sha-m", State: "merged", MergedSHA: "ext-sha"}}
	e := drive(t, newProcessor(st, f), st, mergedRun)
	if e.State != store.MergeQueueMerged || e.MergedSHA != "ext-sha" {
		t.Fatalf("expected external merge settle, got %+v", e)
	}

	closedRun := seedRun(t, st, "ext-closed")
	enqueue(t, st, closedRun, "sha-c")
	f2 := &fakeForge{snapshot: MRSnapshot{SHA: "sha-c", State: "closed"}}
	e = drive(t, newProcessor(st, f2), st, closedRun)
	if e.State != store.MergeQueueEvicted || e.EvictionReason != store.MergeQueueEvictMRClosed {
		t.Fatalf("expected mr_closed eviction, got %+v", e)
	}
}

// An EXTERNAL candidate (fleet producer) that is already based on the target
// tip must NOT merge until a terminal successful pipeline exists for its head
// — fleet producers routinely enqueue while CI is still running, and the
// merge PUT's short not-ready settle window would evict a healthy MR.
func TestProcessor_ExternalUpToDateAwaitsPipelineBeforeMerging(t *testing.T) {
	st := newQueueStore(t)
	runID := seedRun(t, st, "ext-await")
	e, _, err := st.MergeQueue.Enqueue(context.Background(), &store.MergeQueueEntry{
		PipelineRunID: runID, BacklogID: "BL-x", Project: "services/loom-core",
		MRIID: 43, SourceBranch: "feat/ext", TargetBranch: "main", EnqueuedSHA: "sha-ext",
		Detail: map[string]any{"producer": "mcp_gitlab", "idempotency_key": "k"},
	}, 10)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	_ = e

	f := &fakeForge{
		snapshot: MRSnapshot{SHA: "sha-ext", State: "opened", BaseSHA: "tip-9"},
		tip:      "tip-9",
		pipelineStat: PipelineStatus{
			ID: 88, SHA: "sha-ext", Status: "success", Found: true,
		},
		mergeResp: pipeline.MergeResponse{MergedSHA: "merged-ext"},
	}
	p := newProcessor(st, f)

	got := drive(t, p, st, runID) // queued → awaiting_pipeline (NOT merging)
	if got.State != store.MergeQueueAwaitingPipeline {
		t.Fatalf("external up-to-date candidate must await its pipeline, got %s", got.State)
	}
	if f.mergeCalls != 0 {
		t.Fatalf("merge must not fire before the pipeline is proven")
	}
	got = drive(t, p, st, runID) // awaiting → merging (pipeline success)
	if got.State != store.MergeQueueMerging {
		t.Fatalf("expected merging after green pipeline, got %s", got.State)
	}
	got = drive(t, p, st, runID) // merging → merged
	if got.State != store.MergeQueueMerged || f.mergeArgs.ExpectedSHA != "sha-ext" {
		t.Fatalf("expected SHA-preconditioned merge, got %+v args=%+v", got, f.mergeArgs)
	}
}

// A PIPELINE candidate keeps the fast path: base == tip merges directly on
// the run's own ci_watch proof (regression guard for the external fix).
func TestProcessor_PipelineUpToDateStillMergesDirectly(t *testing.T) {
	st := newQueueStore(t)
	runID := seedRun(t, st, "pipe-fast")
	enqueue(t, st, runID, "sha-pipe")

	f := &fakeForge{
		snapshot:  MRSnapshot{SHA: "sha-pipe", State: "opened", BaseSHA: "tip-x"},
		tip:       "tip-x",
		mergeResp: pipeline.MergeResponse{MergedSHA: "merged-pipe"},
	}
	p := newProcessor(st, f)
	got := drive(t, p, st, runID)
	if got.State != store.MergeQueueMerging {
		t.Fatalf("pipeline candidate must keep the direct fast path, got %s", got.State)
	}
}
