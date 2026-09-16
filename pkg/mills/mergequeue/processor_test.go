package mergequeue

import (
	"context"
	"errors"
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
	mrPipeline   PipelineStatus
	proof        PipelineProof
	proofErr     error
	spec         SpeculativeHead
	specErr      error
	specCalls    int
	cancelled    []int64
	deleted      []string
	created      *PipelineStatus
	createMRIID  int64
	timing       PipelineTiming
	timingID     int64
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
func (f *fakeForge) MRPipelineStatus(context.Context, int64, string) (PipelineStatus, error) {
	f.calls = append(f.calls, "mr_pipeline")
	return f.mrPipeline, nil
}
func (f *fakeForge) FindActivePipeline(context.Context, string, string) (PipelineStatus, error) {
	f.calls = append(f.calls, "find_active_pipeline")
	return PipelineStatus{}, nil
}
func (f *fakeForge) PipelineProof(_ context.Context, sha string) (PipelineProof, error) {
	f.calls = append(f.calls, "proof")
	if f.proof.Found || f.proofErr != nil {
		return f.proof, f.proofErr
	}
	if f.pipelineStat.Found && f.pipelineStat.Status == "success" {
		ps := f.pipelineStat
		if ps.SHA == "" {
			ps.SHA = sha
		}
		return PipelineProof{PipelineStatus: ps, Tree: "tree-" + sha, Source: "sha"}, nil
	}
	if f.mrPipeline.Found && f.mrPipeline.Status == "success" {
		ps := f.mrPipeline
		if ps.SHA == "" {
			ps.SHA = sha
		}
		return PipelineProof{PipelineStatus: ps, Tree: "tree-" + sha, Source: "sha"}, nil
	}
	return PipelineProof{}, nil
}
func (f *fakeForge) PrepareSpeculativeHead(_ context.Context, _ int64, _, _ string) (SpeculativeHead, error) {
	f.specCalls++
	if f.specErr != nil {
		return SpeculativeHead{}, f.specErr
	}
	return f.spec, nil
}
func (f *fakeForge) CancelQueuePipeline(_ context.Context, id int64) error {
	f.cancelled = append(f.cancelled, id)
	return nil
}
func (f *fakeForge) DeleteQueueRef(_ context.Context, ref string) error {
	f.deleted = append(f.deleted, ref)
	return nil
}
func (f *fakeForge) CreateQueuePipeline(_ context.Context, _ string, mrIID int64) (PipelineStatus, error) {
	f.calls = append(f.calls, "create")
	f.createMRIID = mrIID
	if f.created != nil {
		return *f.created, nil
	}
	return PipelineStatus{}, fmt.Errorf("unexpected create")
}
func (f *fakeForge) PipelineTiming(_ context.Context, id int64) (PipelineTiming, error) {
	f.timingID = id
	return f.timing, nil
}
func (f *fakeForge) Merge(_ context.Context, req pipeline.MergeRequestArgs) (pipeline.MergeResponse, error) {
	f.calls = append(f.calls, "merge")
	f.mergeCalls++
	f.mergeArgs = req
	return f.mergeResp, f.mergeErr
}

func TestProcessor_PromotesEqualTreeProofAndPersistsTuple(t *testing.T) {
	st := newQueueStore(t)
	runID := seedRun(t, st, "tree")
	e := enqueue(t, st, runID, "head")
	if _, err := st.MergeQueue.Transition(context.Background(), store.MergeQueueTransition{ID: e.ID, From: store.MergeQueueQueued, To: store.MergeQueueAwaitingPipeline, CurrentSHA: "head", Detail: map[string]any{detailPipelineWaitSince: time.Now().UTC().Format(time.RFC3339)}}); err != nil {
		t.Fatal(err)
	}
	f := &fakeForge{proof: PipelineProof{PipelineStatus: PipelineStatus{ID: 77, SHA: "spec", Status: "success", WebURL: "https://gl/p/77", Found: true}, Tree: "tree-equal", Source: "speculative"}}
	p := newProcessor(st, f)
	got := drive(t, p, st, runID)
	if got.State != store.MergeQueueMerging || got.Detail[detailProofSource] != "speculative" {
		t.Fatalf("proof not persisted: %+v", got)
	}
	proof, ok := got.Detail[detailProof].(map[string]any)
	if !ok || detailInt64(proof, "pipeline_id") != 77 || proof["sha"] != "spec" || proof["tree"] != "tree-equal" {
		t.Fatalf("bad proof tuple: %#v", got.Detail[detailProof])
	}
}

func TestProcessor_PreparesAndCancelsSuccessorSpeculation(t *testing.T) {
	st := newQueueStore(t)
	head := enqueue(t, st, seedRun(t, st, "head"), "head")
	next := &store.MergeQueueEntry{PipelineRunID: seedRun(t, st, "next"), BacklogID: "BL-x", Project: "services/loom-core", MRIID: 43, SourceBranch: "feat/y", TargetBranch: "main", EnqueuedSHA: "next"}
	if _, _, err := st.MergeQueue.Enqueue(context.Background(), next, 10); err != nil {
		t.Fatal(err)
	}
	if _, err := st.MergeQueue.Transition(context.Background(), store.MergeQueueTransition{ID: head.ID, From: store.MergeQueueQueued, To: store.MergeQueueAwaitingPipeline, CurrentSHA: "rebased", Detail: map[string]any{detailPipelineWaitSince: time.Now().UTC().Format(time.RFC3339)}}); err != nil {
		t.Fatal(err)
	}
	f := &fakeForge{spec: SpeculativeHead{Ref: "mills-mq/spec-1-rebased", SHA: "spec-sha", Pipeline: PipelineStatus{ID: 88, SHA: "spec-sha", Status: "running", Found: true}}}
	p := newProcessor(st, f)
	p.SpeculationDepth = func() int { return 1 }
	p.prepareSpeculation(context.Background())
	got, _ := st.MergeQueue.Get(context.Background(), next.PipelineRunID)
	if f.specCalls != 1 || got.Detail[detailSpecSHA] != "spec-sha" || detailInt64(got.Detail, detailSpecPipelineID) != 88 {
		t.Fatalf("speculation not durable: calls=%d detail=%#v", f.specCalls, got.Detail)
	}
	if got.Detail[detailPipelineLane] != "queue" {
		t.Fatalf("minted speculation lane = %v, want queue", got.Detail[detailPipelineLane])
	}
	p.cleanupSpeculation(context.Background(), head, true)
	if len(f.cancelled) != 1 || f.cancelled[0] != 88 || len(f.deleted) != 1 {
		t.Fatalf("cleanup missing: cancel=%v delete=%v", f.cancelled, f.deleted)
	}
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

	wall, queued := 1280.0, 1212.0
	f := &fakeForge{
		snapshot:    MRSnapshot{SHA: "sha-old", State: "opened", BaseSHA: "base-stale"},
		tip:         "tip-2",
		observation: pipeline.HeadObservation{Verdict: pipeline.HeadVerdictAttributed, SuccessorSHA: "sha-rebased"},
		pipelineStat: PipelineStatus{
			ID: 77, SHA: "sha-rebased", Status: "success", Found: true,
		},
		mergeResp: pipeline.MergeResponse{MergedSHA: "merged-2"},
		timing:    PipelineTiming{Duration: &wall, QueuedDuration: &queued},
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
	if f.timingID != 77 {
		t.Fatalf("terminal pipeline timing fetched for %d, want 77", f.timingID)
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

// enqueueExternal inserts a fleet-producer candidate (mcp_gitlab shape) so
// tests exercise the external proof contract.
func enqueueExternal(t *testing.T, st *store.Store, runID, sha, key string) *store.MergeQueueEntry {
	t.Helper()
	e, _, err := st.MergeQueue.Enqueue(context.Background(), &store.MergeQueueEntry{
		PipelineRunID: runID, BacklogID: "BL-x", Project: "services/flexinfer",
		MRIID: 1004, SourceBranch: "feat/fx", TargetBranch: "main", EnqueuedSHA: sha,
		Detail: map[string]any{"producer": "mcp_gitlab", "idempotency_key": key},
	}, 10)
	if err != nil {
		t.Fatalf("enqueue external: %v", err)
	}
	return e
}

func hasCall(f *fakeForge, name string) bool {
	for _, c := range f.calls {
		if c == name {
			return true
		}
	}
	return false
}

// The flexinfer wedge (MR !1004, queue entry 207): repos that gate MRs on
// detached merge-request pipelines park their BRANCH pipelines on blocking
// manual jobs, which never turn terminal-green. An external candidate whose
// MR pipeline is green for the head must merge on that proof instead of
// waiting the branch pipeline out into a ci_timeout eviction.
func TestProcessor_ExternalManualBranchAcceptsGreenMRPipeline(t *testing.T) {
	st := newQueueStore(t)
	runID := seedRun(t, st, "ext-mrpipe")
	enqueueExternal(t, st, runID, "sha-fx", "k-mrpipe")

	f := &fakeForge{
		snapshot:     MRSnapshot{SHA: "sha-fx", State: "opened", BaseSHA: "tip-fx"},
		tip:          "tip-fx",
		pipelineStat: PipelineStatus{ID: 24616, SHA: "sha-fx", Status: "manual", Found: true},
		mrPipeline:   PipelineStatus{ID: 24615, SHA: "sha-fx", Status: "success", WebURL: "https://gl/p/24615", Found: true},
		mergeResp:    pipeline.MergeResponse{MergedSHA: "merged-fx"},
	}
	p := newProcessor(st, f)

	got := drive(t, p, st, runID) // queued → awaiting_pipeline
	if got.State != store.MergeQueueAwaitingPipeline {
		t.Fatalf("external candidate must await proof first, got %s", got.State)
	}
	got = drive(t, p, st, runID) // awaiting → merging on the MR pipeline proof
	if got.State != store.MergeQueueMerging {
		t.Fatalf("green MR pipeline must promote to merging, got %+v", got)
	}
	if url, _ := got.Detail[detailPipelineURL].(string); url != "https://gl/p/24615" {
		t.Fatalf("promotion must record the MR pipeline as proof, got %q", url)
	}
	got = drive(t, p, st, runID) // merging → merged
	if got.State != store.MergeQueueMerged || f.mergeArgs.ExpectedSHA != "sha-fx" {
		t.Fatalf("expected SHA-preconditioned merge, got %+v args=%+v", got, f.mergeArgs)
	}
}

// A live MR pipeline holds the wait open past the appear-grace WITHOUT
// minting a recovery branch pipeline underneath it — in MR-pipeline repos
// that recovery would just park on its own manual jobs (flexinfer #24616).
func TestProcessor_ExternalLiveMRPipelineSuppressesRecoveryCreate(t *testing.T) {
	st := newQueueStore(t)
	runID := seedRun(t, st, "ext-live")
	e := enqueueExternal(t, st, runID, "sha-live", "k-live")
	waitSince := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)
	if _, err := st.MergeQueue.Transition(context.Background(), store.MergeQueueTransition{
		ID: e.ID, From: store.MergeQueueQueued, To: store.MergeQueueAwaitingPipeline,
		Detail: map[string]any{"producer": "mcp_gitlab", "idempotency_key": "k-live", detailPipelineWaitSince: waitSince},
	}); err != nil {
		t.Fatalf("prep: %v", err)
	}

	f := &fakeForge{
		pipelineStat: PipelineStatus{ID: 1, Status: "manual", Found: true},
		mrPipeline:   PipelineStatus{ID: 2, Status: "running", Found: true},
		// created == nil: CreateQueuePipeline fails the drive if reached.
	}
	got := drive(t, newProcessor(st, f), st, runID)
	if got.State != store.MergeQueueAwaitingPipeline {
		t.Fatalf("live MR pipeline must keep the candidate awaiting, got %+v", got)
	}
	if hasCall(f, "create") {
		t.Fatalf("recovery pipeline must not be minted under a live MR pipeline")
	}
}

// A red MR pipeline is NOT eviction evidence: GitLab pins a spurious 0-job
// FAILED merge_request_event placeholder on MR heads even in repos whose
// workflow rules suppress MR pipelines (see GitLabClient.Merge), so red here
// proves nothing. The candidate keeps its normal path — fresh window stays
// awaiting; an exhausted window evicts ci_timeout, never ci_red.
func TestProcessor_ExternalRedMRPipelineNeverEvictsCIRed(t *testing.T) {
	st := newQueueStore(t)
	ctx := context.Background()
	runID := seedRun(t, st, "ext-red")
	e := enqueueExternal(t, st, runID, "sha-red", "k-red")
	if _, err := st.MergeQueue.Transition(ctx, store.MergeQueueTransition{
		ID: e.ID, From: store.MergeQueueQueued, To: store.MergeQueueAwaitingPipeline,
		Detail: map[string]any{"producer": "mcp_gitlab", "idempotency_key": "k-red"},
	}); err != nil {
		t.Fatalf("prep: %v", err)
	}

	f := &fakeForge{mrPipeline: PipelineStatus{ID: 3, Status: "failed", Found: true}} // branch: none
	p := newProcessor(st, f)
	got := drive(t, p, st, runID)
	if got.State != store.MergeQueueAwaitingPipeline {
		t.Fatalf("placeholder-red MR pipeline must not evict, got %+v", got)
	}

	stale := time.Now().Add(-46 * time.Minute).UTC().Format(time.RFC3339)
	if _, err := st.MergeQueue.Transition(ctx, store.MergeQueueTransition{
		ID: e.ID, From: store.MergeQueueAwaitingPipeline, To: store.MergeQueueAwaitingPipeline,
		Detail: map[string]any{"producer": "mcp_gitlab", "idempotency_key": "k-red", detailPipelineWaitSince: stale},
	}); err != nil {
		t.Fatalf("age window: %v", err)
	}
	got = drive(t, p, st, runID)
	if got.State != store.MergeQueueEvicted || got.EvictionReason != store.MergeQueueEvictCITimeout {
		t.Fatalf("expected ci_timeout (never ci_red) on a red MR pipeline, got %+v", got)
	}
}

// A manual-parked branch pipeline never turns green on its own, so a PIPELINE
// candidate no longer waits the full bound out on it: the create/timeout path
// engages as if no pipeline existed. MR pipelines are never consulted for
// pipeline candidates — their proof contract is the branch pipeline.
func TestProcessor_ManualBranchPipelineEngagesRecoveryPath(t *testing.T) {
	st := newQueueStore(t)
	runID := seedRun(t, st, "manual")
	e := enqueue(t, st, runID, "sha-man")
	waitSince := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)
	if _, err := st.MergeQueue.Transition(context.Background(), store.MergeQueueTransition{
		ID: e.ID, From: store.MergeQueueQueued, To: store.MergeQueueAwaitingPipeline,
		Detail: map[string]any{detailPipelineWaitSince: waitSince},
	}); err != nil {
		t.Fatalf("prep: %v", err)
	}

	f := &fakeForge{
		pipelineStat: PipelineStatus{ID: 5, SHA: "sha-man", Status: "manual", Found: true},
		created:      &PipelineStatus{ID: 6, SHA: "sha-man", Status: "created", Found: true},
	}
	got := drive(t, newProcessor(st, f), st, runID)
	if got.State != store.MergeQueueAwaitingPipeline || !detailBool(got.Detail, detailPipelineCreated) {
		t.Fatalf("manual pipeline must engage the recovery path, got %+v", got)
	}
	if f.createMRIID != 42 || got.Detail[detailPipelineLane] != "queue" {
		t.Fatalf("recovery mint iid/lane = %d/%v, want 42/queue", f.createMRIID, got.Detail[detailPipelineLane])
	}
	if hasCall(f, "mr_pipeline") {
		t.Fatalf("pipeline candidates must not consult MR pipelines")
	}
}

func TestProcessor_RecoveryPipelineIsSharedByExactHead(t *testing.T) {
	created := PipelineStatus{ID: 77, SHA: "sha-shared", Status: "created", WebURL: "https://gl/77", Found: true}
	f := &fakeForge{created: &created}
	p := &Processor{}
	first := &store.MergeQueueEntry{Project: "services/loom-core", SourceBranch: "feat/shared", CurrentSHA: "sha-shared", MRIID: 42}
	second := &store.MergeQueueEntry{Project: first.Project, SourceBranch: first.SourceBranch, CurrentSHA: first.CurrentSHA}

	gotFirst, adoptedFirst, err := p.adoptOrCreateRecoveryPipeline(context.Background(), f, first)
	if err != nil || adoptedFirst || gotFirst.ID != 77 {
		t.Fatalf("first recovery = %+v adopted=%v err=%v", gotFirst, adoptedFirst, err)
	}
	gotSecond, adoptedSecond, err := p.adoptOrCreateRecoveryPipeline(context.Background(), f, second)
	if err != nil || !adoptedSecond || gotSecond.ID != 77 || gotSecond.WebURL != "https://gl/77" {
		t.Fatalf("second recovery = %+v adopted=%v err=%v", gotSecond, adoptedSecond, err)
	}
	creates := 0
	for _, call := range f.calls {
		if call == "create" {
			creates++
		}
	}
	if creates != 1 {
		t.Fatalf("create calls = %d, want exactly 1", creates)
	}
}

func TestProcessor_RecoveryPipelineCachePreservesMintedLane(t *testing.T) {
	created := PipelineStatus{ID: 77, SHA: "sha-shared", Status: "created", Found: true}
	f := &fakeForge{created: &created}
	p := &Processor{}
	e := &store.MergeQueueEntry{Project: "services/loom-core", SourceBranch: "feat/shared", CurrentSHA: "sha-shared", MRIID: 42}

	if _, adopted, err := p.adoptOrCreateRecoveryPipeline(context.Background(), f, e); err != nil || adopted {
		t.Fatalf("first recovery = adopted %v, err %v; want minted", adopted, err)
	}
	if _, adopted, err := p.adoptOrCreateRecoveryPipeline(context.Background(), f, e); err != nil || adopted {
		t.Fatalf("cached recovery = adopted %v, err %v; want minted provenance preserved", adopted, err)
	}
}

func TestPipelineLaneUsesPersistedSpeculativeProvenance(t *testing.T) {
	proof := PipelineProof{Source: "speculative"}
	if got := pipelineLane(&store.MergeQueueEntry{Detail: map[string]any{detailPipelineLane: "queue"}}, proof); got != "queue" {
		t.Fatalf("minted speculative lane = %q", got)
	}
	if got := pipelineLane(&store.MergeQueueEntry{Detail: map[string]any{detailPipelineLane: "adopted"}}, proof); got != "adopted" {
		t.Fatalf("adopted speculative lane = %q", got)
	}
}

// seedEscalatedRunWithMR inserts an escalated run owning mrIID with durable
// mr-stage project provenance, mimicking a run whose merge stage escalated
// before the queue (or an external candidate) landed the MR.
func seedEscalatedRunWithMR(t *testing.T, st *store.Store, id string, mrIID int64, project string) string {
	t.Helper()
	ctx := context.Background()
	if err := st.Backlog.Put(ctx, &store.BacklogItem{ID: "BL-" + id, Title: "escalated fixture", State: store.BacklogEscalated, Priority: store.P2}); err != nil {
		t.Fatalf("seed backlog: %v", err)
	}
	iid := mrIID
	run := &store.PipelineRun{
		ID: "PIPE-" + id, BacklogID: "BL-" + id, Template: "mills-default-pipeline",
		State: store.PipelineEscalated, Attempts: 1, MRIID: &iid,
		StartedAt: time.Date(2026, 8, 9, 11, 0, 0, 0, time.UTC),
	}
	if err := st.Pipeline.PutRun(ctx, run); err != nil {
		t.Fatalf("seed escalated run: %v", err)
	}
	success := store.StageOutcomeSuccess
	ended := run.StartedAt.Add(time.Minute)
	if err := st.Pipeline.PutStage(ctx, &store.StageResult{
		PipelineRunID: run.ID, Stage: "mr", Attempt: 1,
		StartedAt: run.StartedAt, EndedAt: &ended, Outcome: &success,
		Artifacts: map[string]any{"mr_iid": mrIID, "mr_project": project},
	}); err != nil {
		t.Fatalf("seed provenance: %v", err)
	}
	return run.ID
}

// A3: a merged settle supersedes the verdict of every ESCALATED run owning
// the MR in the entry's project — first-writer, terminal row untouched,
// foreign-project iid twins skipped fail-closed.
func TestProcessor_SettleMergedCorrectsEscalatedRunVerdicts(t *testing.T) {
	st := newQueueStore(t)
	ctx := context.Background()

	escalated := seedEscalatedRunWithMR(t, st, "esc", 42, "services/loom-core")
	foreign := seedEscalatedRunWithMR(t, st, "foreign", 42, "services/other")

	runID := seedRun(t, st, "settle")
	entry := enqueue(t, st, runID, "sha-a")
	p := newProcessor(st, &fakeForge{})

	if err := p.settleMerged(ctx, entry, store.MergeQueueQueued, "sha-final"); err != nil {
		t.Fatalf("settle: %v", err)
	}

	ev, err := st.Events.FirstBySubjectKind(ctx, "pipeline_run", escalated, RunVerdictKindMergeQueueSettled)
	if err != nil {
		t.Fatalf("expected verdict correction on escalated run: %v", err)
	}
	if ev.Payload["class"] != "merged_after_escalation" || ev.Payload["merged_sha"] != "sha-final" {
		t.Fatalf("correction payload = %#v", ev.Payload)
	}
	if _, err := st.Events.FirstBySubjectKind(ctx, "pipeline_run", foreign, RunVerdictKindMergeQueueSettled); err == nil {
		t.Fatalf("foreign-project run must not be corrected")
	}
	// Terminal row untouched — verdict is the supersede-chain HEAD.
	run, _ := st.Pipeline.GetRun(ctx, escalated)
	if run.State != store.PipelineEscalated {
		t.Fatalf("escalated run row mutated to %s", run.State)
	}
	// Idempotent under a replayed settle: first-writer, one event.
	if err := p.settleMerged(ctx, entry, store.MergeQueueQueued, "sha-final"); err != nil {
		t.Fatalf("replay settle: %v", err)
	}
	events, err := st.Events.ListBySubject(ctx, "pipeline_run", escalated, 50)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	count := 0
	for _, e := range events {
		if e != nil && e.Kind == RunVerdictKindMergeQueueSettled {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("verdict corrections = %d, want 1", count)
	}
}

func TestProcessor_ExternalAdoptionSettlesOwningBacklog(t *testing.T) {
	st := newQueueStore(t)
	ctx := context.Background()
	runID := seedEscalatedRunWithMR(t, st, "adopt", 1004, "services/flexinfer")
	entry := enqueueExternal(t, st, runID, "sha-adopt", "adopt-owner")

	if err := newProcessor(st, &fakeForge{}).settleMerged(ctx, entry, store.MergeQueueQueued, "sha-merged"); err != nil {
		t.Fatalf("settle external adoption: %v", err)
	}
	item, err := st.Backlog.Get(ctx, "BL-adopt")
	if err != nil || item.State != store.BacklogMerged {
		t.Fatalf("owning backlog = %+v err=%v, want merged", item, err)
	}
	event, err := st.Events.FirstBySubjectKind(ctx, "backlog", item.ID, "backlog.settled")
	if err != nil {
		t.Fatalf("backlog settlement event: %v", err)
	}
	if event.Payload["reason"] != "external_mq_adoption" || event.Payload["mr_iid"] != float64(1004) {
		t.Fatalf("backlog settlement payload = %#v", event.Payload)
	}
}

func TestProcessor_ExternalAdoptionWithoutOwningRunTouchesNothing(t *testing.T) {
	st := newQueueStore(t)
	ctx := context.Background()
	runID := seedRun(t, st, "unrelated-external")
	entry := enqueueExternal(t, st, runID, "sha-unrelated", "unrelated")

	if err := newProcessor(st, &fakeForge{}).settleMerged(ctx, entry, store.MergeQueueQueued, "sha-merged"); err != nil {
		t.Fatalf("settle unrelated external merge: %v", err)
	}
	item, err := st.Backlog.Get(ctx, "BL-unrelated-external")
	if err != nil || item.State != store.BacklogRunning {
		t.Fatalf("unrelated backlog = %+v err=%v, want running", item, err)
	}
	if _, err := st.Events.FirstBySubjectKind(ctx, "backlog", item.ID, "backlog.settled"); err == nil {
		t.Fatal("unrelated external merge emitted backlog settlement")
	}
}

func TestProcessor_ExternalAdoptionAlreadyMergedIsNoOp(t *testing.T) {
	st := newQueueStore(t)
	ctx := context.Background()
	runID := seedEscalatedRunWithMR(t, st, "adopt-done", 1004, "services/flexinfer")
	item, err := st.Backlog.Get(ctx, "BL-adopt-done")
	if err != nil {
		t.Fatalf("get backlog: %v", err)
	}
	if _, err := st.Backlog.TransitionState(ctx, item.ID, item.ClaimVersion, item.State, store.BacklogMerged); err != nil {
		t.Fatalf("pre-merge backlog: %v", err)
	}
	entry := enqueueExternal(t, st, runID, "sha-done", "already-done")

	p := newProcessor(st, &fakeForge{})
	if err := p.settleMerged(ctx, entry, store.MergeQueueQueued, "sha-merged"); err != nil {
		t.Fatalf("settle already merged adoption: %v", err)
	}
	p.settleRecentExternalAdoptions(ctx)
	if _, err := st.Events.FirstBySubjectKind(ctx, "backlog", item.ID, "backlog.settled"); err == nil {
		t.Fatal("already-merged backlog emitted adoption settlement")
	}
}

func TestProcessor_ExternalAdoptionReplaySettlesAfterRestart(t *testing.T) {
	st := newQueueStore(t)
	ctx := context.Background()
	runID := seedEscalatedRunWithMR(t, st, "adopt-replay", 1004, "services/flexinfer")
	entry := enqueueExternal(t, st, runID, "sha-replay", "replay")
	if _, err := st.MergeQueue.MarkMerged(ctx, entry.ID, store.MergeQueueQueued, "sha-merged"); err != nil {
		t.Fatalf("seed committed queue merge: %v", err)
	}

	p := newProcessor(st, &fakeForge{})
	p.settleRecentExternalAdoptions(ctx)
	item, err := st.Backlog.Get(ctx, "BL-adopt-replay")
	if err != nil || item.State != store.BacklogMerged {
		t.Fatalf("replayed backlog = %+v err=%v, want merged", item, err)
	}
	if _, err := st.Events.FirstBySubjectKind(ctx, "backlog", item.ID, "backlog.settled"); err != nil {
		t.Fatalf("replayed settlement event: %v", err)
	}
}

// A2: a head_moved eviction re-enqueues ONCE as an external candidate under
// the observed successor; the hop's own candidate never re-hops; non-green
// reasons and a disabled flag do nothing.
func TestProcessor_EvictionRequeueHop(t *testing.T) {
	st := newQueueStore(t)
	ctx := context.Background()
	enabled := true
	p := newProcessor(st, &fakeForge{})
	p.External = &ExternalEnqueuer{Store: st}
	p.RequeueEvictions = func() bool { return enabled }

	runID := seedRun(t, st, "hop")
	entry := enqueue(t, st, runID, "sha-old")
	if err := p.evict(ctx, entry, store.MergeQueueEvictHeadMoved,
		"head moved externally", map[string]any{detailObservedSHA: "sha-successor"}); err != nil {
		t.Fatalf("evict: %v", err)
	}
	// The hop minted an external candidate under the successor SHA.
	heads, err := st.MergeQueue.Heads(ctx)
	if err != nil || len(heads) != 1 {
		t.Fatalf("heads = %v err=%v; want the hop candidate", heads, err)
	}
	hop := heads[0]
	if hop.EnqueuedSHA != "sha-successor" || hop.Detail["producer"] != evictionRequeueProducer {
		t.Fatalf("hop candidate = %+v", hop)
	}
	if _, err := st.Events.FirstBySubjectKind(ctx, "pipeline_run", runID, "mergequeue.eviction_requeued"); err != nil {
		t.Fatalf("requeue event: %v", err)
	}

	// The hop's own candidate evicting again is FINAL: no second hop.
	if err := p.evict(ctx, hop, store.MergeQueueEvictHeadMoved,
		"moved again", map[string]any{detailObservedSHA: "sha-third"}); err != nil {
		t.Fatalf("evict hop: %v", err)
	}
	heads, _ = st.MergeQueue.Heads(ctx)
	if len(heads) != 0 {
		t.Fatalf("one hop max; got %v", heads)
	}

	// A rebase_conflict eviction never hops.
	runID2 := seedRun(t, st, "hop2")
	entry2 := enqueue(t, st, runID2, "sha-b")
	if err := p.evict(ctx, entry2, store.MergeQueueEvictRebaseConflict, "conflict", nil); err != nil {
		t.Fatalf("evict conflict: %v", err)
	}
	heads, _ = st.MergeQueue.Heads(ctx)
	if len(heads) != 0 {
		t.Fatalf("conflict must not hop; got %v", heads)
	}

	// Flag off: nothing happens even for green reasons.
	enabled = false
	runID3 := seedRun(t, st, "hop3")
	entry3 := enqueue(t, st, runID3, "sha-c")
	if err := p.evict(ctx, entry3, store.MergeQueueEvictCITimeout, "timeout", nil); err != nil {
		t.Fatalf("evict timeout: %v", err)
	}
	heads, _ = st.MergeQueue.Heads(ctx)
	if len(heads) != 0 {
		t.Fatalf("disabled flag must not hop; got %v", heads)
	}
}

// A second active entry for the same MR (e.g. the mrwatch shepherd's external
// candidate next to the MR's own run) must never become a speculative
// successor of itself: 2026-09-12 that cherry-picked !1918 onto its own head
// on every tick for three hours.
func TestProcessor_SpeculationSkipsSameMRSuccessor(t *testing.T) {
	st := newQueueStore(t)
	head := enqueue(t, st, seedRun(t, st, "head"), "head")
	dup := enqueue(t, st, seedRun(t, st, "dup"), "head") // same MRIID 42 as head
	if _, err := st.MergeQueue.Transition(context.Background(), store.MergeQueueTransition{ID: head.ID, From: store.MergeQueueQueued, To: store.MergeQueueAwaitingPipeline, CurrentSHA: "rebased", Detail: map[string]any{detailPipelineWaitSince: time.Now().UTC().Format(time.RFC3339)}}); err != nil {
		t.Fatal(err)
	}
	f := &fakeForge{spec: SpeculativeHead{Ref: "mills-mq/spec-42-rebased", SHA: "spec-sha", Pipeline: PipelineStatus{ID: 88, Found: true}}}
	p := newProcessor(st, f)
	p.SpeculationDepth = func() int { return 2 }
	p.prepareSpeculation(context.Background())
	got, _ := st.MergeQueue.Get(context.Background(), dup.PipelineRunID)
	if f.specCalls != 0 || got.Detail[detailSpecSHA] != nil {
		t.Fatalf("same-MR successor was speculated: calls=%d detail=%#v", f.specCalls, got.Detail)
	}
}

// A failed speculative head is remembered per base so the tick loop does not
// repeat the identical attempt every 15s; a new base is tried again.
func TestProcessor_SpeculationBacksOffAfterFailure(t *testing.T) {
	st := newQueueStore(t)
	head := enqueue(t, st, seedRun(t, st, "head"), "head")
	next := &store.MergeQueueEntry{PipelineRunID: seedRun(t, st, "next"), BacklogID: "BL-x", Project: "services/loom-core", MRIID: 43, SourceBranch: "feat/y", TargetBranch: "main", EnqueuedSHA: "next"}
	if _, _, err := st.MergeQueue.Enqueue(context.Background(), next, 10); err != nil {
		t.Fatal(err)
	}
	if _, err := st.MergeQueue.Transition(context.Background(), store.MergeQueueTransition{ID: head.ID, From: store.MergeQueueQueued, To: store.MergeQueueAwaitingPipeline, CurrentSHA: "rebased", Detail: map[string]any{detailPipelineWaitSince: time.Now().UTC().Format(time.RFC3339)}}); err != nil {
		t.Fatal(err)
	}
	f := &fakeForge{specErr: errors.New("cherry-pick conflict")}
	p := newProcessor(st, f)
	p.SpeculationDepth = func() int { return 1 }
	p.prepareSpeculation(context.Background())
	p.prepareSpeculation(context.Background())
	if f.specCalls != 1 {
		t.Fatalf("failed attempt retried on the same base: calls=%d", f.specCalls)
	}
	got, _ := st.MergeQueue.Get(context.Background(), next.PipelineRunID)
	if got.Detail[detailSpecFailedOnto] != "rebased" {
		t.Fatalf("failed base not recorded: %#v", got.Detail)
	}
	// Head moved to a new base: the successor is tried again.
	if _, err := st.MergeQueue.Transition(context.Background(), store.MergeQueueTransition{ID: head.ID, From: store.MergeQueueAwaitingPipeline, To: store.MergeQueueAwaitingPipeline, CurrentSHA: "rebased-2", Detail: head.Detail}); err != nil {
		t.Fatal(err)
	}
	f.specErr = nil
	f.spec = SpeculativeHead{Ref: "mills-mq/spec-43-rebased-2", SHA: "spec-2", Pipeline: PipelineStatus{ID: 89, Found: true}}
	p.prepareSpeculation(context.Background())
	got, _ = st.MergeQueue.Get(context.Background(), next.PipelineRunID)
	if f.specCalls != 2 || got.Detail[detailSpecSHA] != "spec-2" {
		t.Fatalf("new base not retried: calls=%d detail=%#v", f.specCalls, got.Detail)
	}
}

func TestProcessor_ExternalAdoptionCooldownTransaction(t *testing.T) {
	ctx := context.Background()
	st := newQueueStore(t)
	runID := seedEscalatedRunWithMR(t, st, "atomic-adoption", 1004, "services/flexinfer")
	run, err := st.Pipeline.GetRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	item, err := st.Backlog.Get(ctx, run.BacklogID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Backlog.DeferEscalationRecheck(ctx, item.ID, 1004, time.Now()); err != nil {
		t.Fatal(err)
	}
	event := &store.Event{Actor: "test", Kind: "backlog.settled", SubjectKind: "backlog", SubjectID: item.ID, Payload: map[string]any{"invalid": make(chan int)}}
	if _, _, err := st.Backlog.TransitionStateWithEventOnce(ctx, item.ID, item.ClaimVersion, store.BacklogEscalated, store.BacklogMerged, event); err == nil {
		t.Fatal("expected event encoding failure")
	}
	got, err := st.Backlog.Get(ctx, item.ID)
	if err != nil || got.State != store.BacklogEscalated {
		t.Fatalf("failed transaction changed state: %+v %v", got, err)
	}
	if _, err := st.Backlog.EscalationRecheck(ctx, item.ID); err != nil {
		t.Fatalf("failed transaction cleared cooldown: %v", err)
	}
	event.Payload = map[string]any{"merged_sha": "atomic-sha"}
	for attempt := 0; attempt < 2; attempt++ {
		_, inserted, err := st.Backlog.TransitionStateWithEventOnce(ctx, item.ID, item.ClaimVersion, store.BacklogEscalated, store.BacklogMerged, event)
		if err != nil || inserted != (attempt == 0) {
			t.Fatalf("attempt %d inserted=%v err=%v", attempt, inserted, err)
		}
		if _, err := st.Backlog.EscalationRecheck(ctx, item.ID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("cooldown survived transaction: %v", err)
		}
	}
}
