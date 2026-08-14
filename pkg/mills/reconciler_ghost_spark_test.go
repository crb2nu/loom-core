package mills

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/crb2nu/loom/pkg/mills/store"
)

// fakeMRStateClient is a deterministic MRStateClient: states maps an MR iid to
// the lifecycle state MRState returns; errs maps an iid to an error to return
// instead. It counts calls so the per-pass lookup cap is assertable.
type fakeMRStateClient struct {
	mu     sync.Mutex
	states map[int64]string
	errs   map[int64]error
	calls  int
}

func (f *fakeMRStateClient) MRState(_ context.Context, mrIID int64) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if err := f.errs[mrIID]; err != nil {
		return "", err
	}
	return f.states[mrIID], nil
}

func (f *fakeMRStateClient) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

type cancelOnFirstMRStateClient struct {
	mu     sync.Mutex
	cancel context.CancelFunc
	calls  int
}

func (f *cancelOnFirstMRStateClient) MRState(ctx context.Context, _ int64) (string, error) {
	f.mu.Lock()
	f.calls++
	first := f.calls == 1
	f.mu.Unlock()
	if first && f.cancel != nil {
		f.cancel()
	}
	return "", ctx.Err()
}

func (f *cancelOnFirstMRStateClient) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

type deadlineMRStateClient struct {
	deadlineOK bool
	remaining  time.Duration
}

type deadlineExceededMRStateClient struct {
	mu    sync.Mutex
	calls int
}

func (f *deadlineExceededMRStateClient) MRState(_ context.Context, _ int64) (string, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	// Model a child request timeout without relying on a wall-clock deadline;
	// race instrumentation can otherwise consume the sweep budget in SQLite
	// setup before this fake is reached.
	return "", context.DeadlineExceeded
}

func (f *deadlineExceededMRStateClient) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

type poisonMRStateClient struct {
	mu     sync.Mutex
	poison int64
	calls  []int64
}

func (f *poisonMRStateClient) MRState(_ context.Context, mrIID int64) (string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, mrIID)
	f.mu.Unlock()
	if mrIID == f.poison {
		// Keep poison-candidate progress deterministic under race builds. The
		// production deadline wiring is covered independently by
		// TestReconcilerTickBoundsGhostSparkSweep.
		return "", context.DeadlineExceeded
	}
	return "merged", nil
}

func (f *poisonMRStateClient) callIIDs() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int64(nil), f.calls...)
}

func (f *deadlineMRStateClient) MRState(ctx context.Context, _ int64) (string, error) {
	deadline, ok := ctx.Deadline()
	f.deadlineOK = ok
	if ok {
		f.remaining = time.Until(deadline)
	}
	return "merged", nil
}

// fakeGhostResolver records the backlog ids whose escalation issue it was asked
// to auto-close, standing in for the pipeline Escalator's ResolveOnSuccess.
type fakeGhostResolver struct {
	mu           sync.Mutex
	calls        []string
	fail         error
	deadlineOK   bool
	remaining    time.Duration
	contextError error
}

func (f *fakeGhostResolver) ResolveOnSuccess(ctx context.Context, _ *store.PipelineRun, item *store.BacklogItem) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, item.ID)
	f.contextError = ctx.Err()
	if deadline, ok := ctx.Deadline(); ok {
		f.deadlineOK = true
		f.remaining = time.Until(deadline)
	}
	return f.fail
}

func (f *fakeGhostResolver) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeGhostResolver) budget() (bool, time.Duration, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.deadlineOK, f.remaining, f.contextError
}

// seedEscalatedGhostSpark inserts an escalated backlog item plus an escalated
// pipeline run carrying mrIID, mimicking a run that escalated at the merge stage.
func seedEscalatedGhostSpark(t *testing.T, env *recTestEnv, id string, mrIID int64, started time.Time) {
	seedEscalatedGhostSparkProject(t, env, id, mrIID, started, "services/loom-core")
}

func seedEscalatedGhostSparkProject(t *testing.T, env *recTestEnv, id string, mrIID int64, started time.Time, project string) {
	t.Helper()
	if env.rec.HomeProject == "" {
		env.rec.HomeProject = "services/loom-core"
	}
	ctx := context.Background()
	item := &store.BacklogItem{
		ID:        id,
		Title:     "escalated at merge stage",
		State:     store.BacklogEscalated,
		Priority:  store.P2,
		Slices:    []store.Slice{{Name: "x", Files: []string{"a.go"}}},
		Budget:    store.Budget{MaxCostUSD: 1.0, MaxTurns: 30},
		CreatedBy: "test",
	}
	if err := env.store.Backlog.Put(ctx, item); err != nil {
		t.Fatalf("seed item %s: %v", id, err)
	}
	iid := mrIID
	run := &store.PipelineRun{
		ID:        "PIPE-" + id,
		BacklogID: id,
		Template:  "mills-default-pipeline",
		State:     store.PipelineEscalated,
		Attempts:  1,
		MRIID:     &iid,
		StartedAt: started,
	}
	if err := env.store.Pipeline.PutRun(ctx, run); err != nil {
		t.Fatalf("seed run %s: %v", id, err)
	}
	success := store.StageOutcomeSuccess
	ended := started.Add(time.Minute)
	if err := env.store.Pipeline.PutStage(ctx, &store.StageResult{
		PipelineRunID: run.ID,
		Stage:         "mr",
		Attempt:       1,
		StartedAt:     started,
		EndedAt:       &ended,
		Outcome:       &success,
		Artifacts: map[string]any{
			"mr_iid":     mrIID,
			"mr_project": project,
		},
	}); err != nil {
		t.Fatalf("seed run %s mr provenance: %v", id, err)
	}
}

// TestGhostSparkClosesMergedItem is the W1 kill test: an escalated item whose
// run's MR merged out-of-band is transitioned escalated→merged in one sweep with
// a version bump, the escalation issue auto-closed, and an annotation event
// written; a second sweep is a no-op (no double transition, no second close).
func TestGhostSparkClosesMergedItem(t *testing.T) {
	env := newRecEnv(t, nil)
	ctx := context.Background()

	seedEscalatedGhostSpark(t, env, "MILLS-GHOST-1", 1037, env.now.Add(-time.Hour))
	seeded, _ := env.store.Backlog.Get(ctx, "MILLS-GHOST-1")

	mrs := &fakeMRStateClient{states: map[int64]string{1037: "merged"}}
	resolver := &fakeGhostResolver{}
	env.rec.GhostSparkMRState = mrs
	env.rec.GhostSparkResolver = resolver

	before := testutil.ToFloat64(GhostSparksClosedTotal.WithLabelValues("merged"))

	res, err := env.rec.SweepGhostSparks(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.Merged != 1 || res.Inspected != 1 || res.Errored != 0 || res.MRClosed != 0 {
		t.Fatalf("first sweep result: %+v", res)
	}

	got, _ := env.store.Backlog.Get(ctx, "MILLS-GHOST-1")
	if got.State != store.BacklogMerged {
		t.Fatalf("item state: got %s want merged", got.State)
	}
	if got.Revision <= seeded.Revision {
		t.Fatalf("expected version bump: seeded rev %d, got %d", seeded.Revision, got.Revision)
	}
	if resolver.count() != 1 || resolver.calls[0] != "MILLS-GHOST-1" {
		t.Fatalf("expected one issue auto-close for MILLS-GHOST-1, got %v", resolver.calls)
	}
	deadlineOK, remaining, resolverCtxErr := resolver.budget()
	if !deadlineOK || resolverCtxErr != nil || remaining < ghostSparkResolverTimeout-time.Second || remaining > ghostSparkResolverTimeout+time.Second {
		t.Fatalf("resolver budget: deadline=%v remaining=%s ctx_err=%v", deadlineOK, remaining, resolverCtxErr)
	}
	if _, err := env.store.Events.FirstBySubjectKind(ctx, "pipeline_run", "PIPE-MILLS-GHOST-1", "reconciler.ghost_spark_closed"); err != nil {
		t.Fatalf("expected ghost_spark_closed event: %v", err)
	}
	if delta := testutil.ToFloat64(GhostSparksClosedTotal.WithLabelValues("merged")) - before; delta != 1 {
		t.Fatalf("merged counter delta: got %v want 1", delta)
	}
	// The pipeline run stays escalated: it is a terminal pipeline state (one-way
	// per store.IsPipelineTerminalState); the sweep never mutates it, only the
	// backlog item + an annotation event.
	gotRun, _ := env.store.Pipeline.GetRun(ctx, "PIPE-MILLS-GHOST-1")
	if gotRun.State != store.PipelineEscalated {
		t.Fatalf("pipeline run state: got %s want escalated (must not be resurrected)", gotRun.State)
	}

	// Second sweep: the item is no longer escalated, so it is not even a
	// candidate — no lookup, no transition, no second close.
	res2, err := env.rec.SweepGhostSparks(ctx)
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if res2.Merged != 0 || res2.Inspected != 0 {
		t.Fatalf("second sweep should be a no-op, got %+v", res2)
	}
	if resolver.count() != 1 {
		t.Fatalf("issue must not be closed twice, got %d calls", resolver.count())
	}
	if delta := testutil.ToFloat64(GhostSparksClosedTotal.WithLabelValues("merged")) - before; delta != 1 {
		t.Fatalf("merged counter must not double-count: got %v want 1", delta)
	}
}

// TestGhostSparkMRClosedStaysEscalated proves an abandoned (closed, not merged)
// MR leaves the item escalated and is counted exactly once even across re-checks.
func TestGhostSparkMRClosedStaysEscalated(t *testing.T) {
	env := newRecEnv(t, nil)
	ctx := context.Background()

	seedEscalatedGhostSpark(t, env, "MILLS-GHOST-CLOSED", 1050, env.now.Add(-time.Hour))

	mrs := &fakeMRStateClient{states: map[int64]string{1050: "closed"}}
	env.rec.GhostSparkMRState = mrs

	before := testutil.ToFloat64(GhostSparksClosedTotal.WithLabelValues("mr_closed"))

	res, err := env.rec.SweepGhostSparks(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.MRClosed != 1 || res.Merged != 0 || res.Inspected != 1 {
		t.Fatalf("mr_closed sweep result: %+v", res)
	}
	got, _ := env.store.Backlog.Get(ctx, "MILLS-GHOST-CLOSED")
	if got.State != store.BacklogEscalated {
		t.Fatalf("abandoned-MR item must stay escalated, got %s", got.State)
	}
	if delta := testutil.ToFloat64(GhostSparksClosedTotal.WithLabelValues("mr_closed")) - before; delta != 1 {
		t.Fatalf("mr_closed counter delta: got %v want 1", delta)
	}

	// Second sweep: the item is still escalated but inside its re-check
	// cooldown (non-resolving candidates must not monopolize the per-tick
	// lookup budget), so it is skipped entirely — which also keeps the count
	// idempotent. The first-writer event still guards recounting whenever the
	// cooldown lapses and the item is re-checked.
	res2, err := env.rec.SweepGhostSparks(ctx)
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if res2.MRClosed != 0 || res2.Inspected != 0 {
		t.Fatalf("second sweep inside the cooldown should skip the item: %+v", res2)
	}
	if delta := testutil.ToFloat64(GhostSparksClosedTotal.WithLabelValues("mr_closed")) - before; delta != 1 {
		t.Fatalf("mr_closed counter must not double-count: got %v want 1", delta)
	}
}

// TestGhostSparkGitLabErrorSkips proves a GitLab lookup error skips and cools
// down the item without failing the sweep. It is retried after the cooldown.
func TestGhostSparkGitLabErrorSkips(t *testing.T) {
	env := newRecEnv(t, nil)
	ctx := context.Background()

	seedEscalatedGhostSpark(t, env, "MILLS-GHOST-ERR", 1060, env.now.Add(-time.Hour))

	mrs := &fakeMRStateClient{
		states: map[int64]string{1060: "merged"},
		errs:   map[int64]error{1060: errors.New("gitlab: 502 bad gateway")},
	}
	env.rec.GhostSparkMRState = mrs

	res, err := env.rec.SweepGhostSparks(ctx)
	if err != nil {
		t.Fatalf("sweep must not fail on a lookup error: %v", err)
	}
	if res.Errored != 1 || res.Merged != 0 {
		t.Fatalf("error sweep result: %+v", res)
	}
	if got, _ := env.store.Backlog.Get(ctx, "MILLS-GHOST-ERR"); got.State != store.BacklogEscalated {
		t.Fatalf("item must stay escalated after a lookup error, got %s", got.State)
	}

	// An immediate next tick skips the poison candidate so it cannot consume the
	// bounded lookup budget again.
	delete(mrs.errs, 1060)
	res2, err := env.rec.SweepGhostSparks(ctx)
	if err != nil {
		t.Fatalf("cooldown sweep: %v", err)
	}
	if res2.Inspected != 0 || res2.Merged != 0 {
		t.Fatalf("immediate retry should skip the cooled-down item, got %+v", res2)
	}

	// Once the cooldown expires, the cleared transient error is retried and the
	// item reconciles normally.
	env.rec.ghostSparkRecheck["MILLS-GHOST-ERR"] = time.Now().Add(-time.Second)
	if err := env.store.Backlog.ClearEscalationRecheck(ctx, "MILLS-GHOST-ERR"); err != nil {
		t.Fatalf("clear durable cooldown: %v", err)
	}
	res3, err := env.rec.SweepGhostSparks(ctx)
	if err != nil {
		t.Fatalf("post-cooldown retry sweep: %v", err)
	}
	if res3.Merged != 1 {
		t.Fatalf("post-cooldown retry should reap the item, got %+v", res3)
	}
	if got, _ := env.store.Backlog.Get(ctx, "MILLS-GHOST-ERR"); got.State != store.BacklogMerged {
		t.Fatalf("item should be merged after retry, got %s", got.State)
	}
}

func TestGhostSparkLookupErrorsCannotStarveMergedTail(t *testing.T) {
	env := newRecEnv(t, nil)
	ctx := context.Background()
	states := make(map[int64]string, ghostSparkGitLabLookupsPerPass+1)
	errs := make(map[int64]error, ghostSparkGitLabLookupsPerPass)
	for i := 0; i < ghostSparkGitLabLookupsPerPass; i++ {
		iid := int64(3000 + i)
		id := fmt.Sprintf("MILLS-GHOST-LOOKUP-ERROR-%02d", i)
		seedEscalatedGhostSpark(t, env, id, iid, env.now.Add(-time.Duration(ghostSparkGitLabLookupsPerPass-i+2)*time.Hour))
		errs[iid] = errors.New("gitlab: permanently inaccessible MR")
	}
	const mergedIID = int64(3999)
	seedEscalatedGhostSpark(t, env, "MILLS-GHOST-MERGED-TAIL", mergedIID, env.now.Add(-time.Hour))
	states[mergedIID] = "merged"
	mrs := &fakeMRStateClient{states: states, errs: errs}
	env.rec.GhostSparkMRState = mrs

	first, err := env.rec.SweepGhostSparks(ctx)
	if err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	if first.Inspected != ghostSparkGitLabLookupsPerPass || first.Errored != ghostSparkGitLabLookupsPerPass || first.Merged != 0 {
		t.Fatalf("first sweep = %+v, want lookup cap consumed by errors", first)
	}
	if got := backlogState(t, env, "MILLS-GHOST-MERGED-TAIL"); got != store.BacklogEscalated {
		t.Fatalf("merged tail state after first sweep = %s, want escalated", got)
	}

	second, err := env.rec.SweepGhostSparks(ctx)
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if second.Inspected != 1 || second.Merged != 1 || second.Errored != 0 {
		t.Fatalf("second sweep = %+v, want cooled poison head skipped and tail merged", second)
	}
	if got := backlogState(t, env, "MILLS-GHOST-MERGED-TAIL"); got != store.BacklogMerged {
		t.Fatalf("merged tail state = %s, want merged", got)
	}
}

func TestGhostSparkCancellationStopsAfterFirstLookupWithoutEventStorm(t *testing.T) {
	env := newRecEnv(t, nil)
	for i := 0; i < 3; i++ {
		seedEscalatedGhostSpark(
			t, env, fmt.Sprintf("MILLS-GHOST-CANCEL-%d", i), int64(1600+i),
			env.now.Add(-time.Duration(i+1)*time.Hour),
		)
	}

	var logs bytes.Buffer
	env.rec.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	ctx, cancel := context.WithCancel(context.Background())
	mrs := &cancelOnFirstMRStateClient{cancel: cancel}
	env.rec.GhostSparkMRState = mrs

	res, err := env.rec.SweepGhostSparks(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("sweep error = %v, want context canceled", err)
	}
	if got := mrs.callCount(); got != 1 {
		t.Fatalf("MRState calls after cancellation = %d, want 1", got)
	}
	if res.Inspected != 1 || res.Errored != 0 {
		t.Fatalf("canceled sweep result = %+v, want one lookup and no retry error", res)
	}
	if strings.Contains(logs.String(), "append event failed") {
		t.Fatalf("cancellation triggered event-write warnings:\n%s", logs.String())
	}

	events, listErr := env.store.Events.ListSince(context.Background(), time.Unix(0, 0), 100)
	if listErr != nil {
		t.Fatalf("list events: %v", listErr)
	}
	for _, event := range events {
		if strings.HasPrefix(event.Kind, "reconciler.ghost_spark") {
			t.Fatalf("ghost-spark event %q emitted after cancellation", event.Kind)
		}
	}
}

func TestGhostSparkTimeoutDefersPoisonCandidateAndAdvancesNextSweep(t *testing.T) {
	env := newRecEnv(t, nil)
	const (
		poisonIID = int64(1620)
		nextIID   = int64(1621)
	)
	seedEscalatedGhostSpark(t, env, "MILLS-GHOST-POISON", poisonIID, env.now.Add(-2*time.Hour))
	seedEscalatedGhostSpark(t, env, "MILLS-GHOST-AFTER-POISON", nextIID, env.now.Add(-time.Hour))
	mrs := &poisonMRStateClient{poison: poisonIID}
	env.rec.GhostSparkMRState = mrs

	first, err := env.rec.SweepGhostSparks(context.Background())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first sweep error = %v, want deadline exceeded", err)
	}
	if first.Inspected != 1 || first.Merged != 0 {
		t.Fatalf("first sweep = %+v, want one timed-out inspection", first)
	}
	if got := mrs.callIIDs(); !reflect.DeepEqual(got, []int64{poisonIID}) {
		t.Fatalf("first sweep MR calls = %v, want poison only", got)
	}

	secondCtx, secondCancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	second, err := env.rec.SweepGhostSparks(secondCtx)
	secondCancel()
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if second.Inspected != 1 || second.Merged != 1 {
		t.Fatalf("second sweep = %+v, want next candidate merged", second)
	}
	if got := mrs.callIIDs(); !reflect.DeepEqual(got, []int64{poisonIID, nextIID}) {
		t.Fatalf("calls after second sweep = %v, want poison then next candidate", got)
	}
	if got := backlogState(t, env, "MILLS-GHOST-POISON"); got != store.BacklogEscalated {
		t.Fatalf("poison candidate state = %s, want escalated", got)
	}
	if got := backlogState(t, env, "MILLS-GHOST-AFTER-POISON"); got != store.BacklogMerged {
		t.Fatalf("next candidate state = %s, want merged", got)
	}
}

func TestGhostSparkRecheck_MRIIDChangeBypassesProcessCache(t *testing.T) {
	env := newRecEnv(t, nil)
	const id = "MILLS-GHOST-NEW-MR"
	seedEscalatedGhostSpark(t, env, id, 1700, env.now.Add(-time.Hour))

	if err := env.rec.deferGhostSparkRecheck(context.Background(), id, env.now, 1700); err != nil {
		t.Fatal(err)
	}
	due, err := env.rec.ghostSparkRecheckDue(context.Background(), id, env.now, 1700)
	if err != nil {
		t.Fatal(err)
	}
	if due {
		t.Fatal("unchanged MR IID should remain deferred")
	}
	due, err = env.rec.ghostSparkRecheckDue(context.Background(), id, env.now, 1701)
	if err != nil {
		t.Fatal(err)
	}
	if !due {
		t.Fatal("changed MR IID must bypass the stale process cache")
	}
}

func TestGhostSparkCancellationImmediatelyAfterAtomicCommitRunsBoundedResolver(t *testing.T) {
	env := newRecEnv(t, nil)
	seedEscalatedGhostSpark(t, env, "MILLS-GHOST-ATOMIC-CANCEL", 1650, env.now.Add(-time.Hour))
	mrs := &fakeMRStateClient{states: map[int64]string{1650: "merged"}}
	resolver := &fakeGhostResolver{}
	env.rec.GhostSparkMRState = mrs
	env.rec.GhostSparkResolver = resolver
	var logs bytes.Buffer
	env.rec.Logger = slog.New(slog.NewTextHandler(&logs, nil))

	ctx, cancel := context.WithCancel(context.Background())
	env.rec.ghostSparkTransition = func(
		transitionCtx context.Context,
		id string,
		expectedClaimVersion int64,
		from, to store.BacklogState,
		event *store.Event,
	) (*store.BacklogItem, bool, error) {
		updated, inserted, err := env.store.Backlog.TransitionStateWithEventOnce(
			transitionCtx, id, expectedClaimVersion, from, to, event,
		)
		if err == nil {
			cancel() // commit succeeded; parent dies before resolver follow-up
		}
		return updated, inserted, err
	}
	res, err := env.rec.SweepGhostSparks(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("sweep error = %v, want context canceled", err)
	}
	if res.Merged != 1 {
		t.Fatalf("sweep merged count after committed close = %d, want 1", res.Merged)
	}
	item, err := env.store.Backlog.Get(context.Background(), "MILLS-GHOST-ATOMIC-CANCEL")
	if err != nil || item.State != store.BacklogMerged {
		t.Fatalf("backlog after committed atomic close = %+v, err=%v", item, err)
	}
	if _, err = env.store.Events.FirstBySubjectKind(
		context.Background(), "pipeline_run", "PIPE-MILLS-GHOST-ATOMIC-CANCEL", "reconciler.ghost_spark_closed",
	); err != nil {
		t.Fatalf("event after committed atomic close: %v", err)
	}
	if resolver.count() != 1 {
		t.Fatalf("resolver calls after committed atomic close = %d, want 1", resolver.count())
	}
	deadlineOK, remaining, resolverCtxErr := resolver.budget()
	if !deadlineOK || resolverCtxErr != nil || remaining < ghostSparkResolverTimeout-time.Second || remaining > ghostSparkResolverTimeout+time.Second {
		t.Fatalf("resolver cleanup budget: deadline=%v remaining=%s ctx_err=%v", deadlineOK, remaining, resolverCtxErr)
	}
	if strings.Contains(logs.String(), "ghost-spark") {
		t.Fatalf("post-commit cancellation emitted warnings:\n%s", logs.String())
	}

	// The merged item is no longer a candidate; the durable event and resolver
	// follow-up must both remain exactly-once on later sweeps.
	res, err = env.rec.SweepGhostSparks(context.Background())
	if err != nil || res.Inspected != 0 || res.Merged != 0 {
		t.Fatalf("repeat sweep: res=%+v err=%v", res, err)
	}
	eventCount, err := env.store.Events.CountBySubjectKind(
		context.Background(), "pipeline_run", "PIPE-MILLS-GHOST-ATOMIC-CANCEL", "reconciler.ghost_spark_closed",
	)
	if err != nil || eventCount != 1 {
		t.Fatalf("event count after repeat = %d, err=%v, want 1", eventCount, err)
	}
	if resolver.count() != 1 {
		t.Fatalf("resolver calls after repeat = %d, want 1", resolver.count())
	}
}

func TestEscalationSweeperBoundsGhostSparkSweep(t *testing.T) {
	env := newRecEnv(t, nil)
	seedEscalatedGhostSpark(t, env, "MILLS-GHOST-BUDGET", 1700, env.now.Add(-time.Hour))
	mrs := &deadlineMRStateClient{}
	env.rec.GhostSparkMRState = mrs

	sweeper := NewEscalationSweeper(env.rec, env.policy)
	sweeper.runPass(context.Background())
	if !mrs.deadlineOK {
		t.Fatal("ghost-spark MR lookup had no deadline")
	}
	// The sweeper reserves one-third of the pass for auto-requeue, then the
	// ghost sweep reserves one-third of its allocation for branch lookups.
	want := defaultEscalationSweepBudget * 2 / 3 * 2 / 3
	if mrs.remaining < want-time.Second || mrs.remaining > want+time.Second {
		t.Fatalf("ghost-spark IID budget = %s, want near %s", mrs.remaining, want)
	}
}

// TestGhostSparkCapRespected proves the per-pass GitLab lookup cap bounds the
// number of MR-state lookups even when the escalated-with-MR pile exceeds it.
func TestGhostSparkCapRespected(t *testing.T) {
	env := newRecEnv(t, nil)
	ctx := context.Background()

	const seeded = ghostSparkGitLabLookupsPerPass + 5
	states := make(map[int64]string, seeded)
	for i := 0; i < seeded; i++ {
		mrIID := int64(2000 + i)
		// "opened" keeps every item escalated so none leave the candidate set —
		// the cap is the only thing bounding lookups.
		states[mrIID] = "opened"
		seedEscalatedGhostSpark(t, env, fmt.Sprintf("MILLS-CAP-%02d", i), mrIID, env.now.Add(-time.Duration(i)*time.Minute))
	}
	mrs := &fakeMRStateClient{states: states}
	env.rec.GhostSparkMRState = mrs

	res, err := env.rec.SweepGhostSparks(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if mrs.callCount() != ghostSparkGitLabLookupsPerPass {
		t.Fatalf("GitLab lookups: got %d want cap %d", mrs.callCount(), ghostSparkGitLabLookupsPerPass)
	}
	if res.Inspected != ghostSparkGitLabLookupsPerPass {
		t.Fatalf("inspected: got %d want cap %d", res.Inspected, ghostSparkGitLabLookupsPerPass)
	}
	if res.Merged != 0 || res.MRClosed != 0 {
		t.Fatalf("opened MRs must not resolve, got %+v", res)
	}
}

// TestGhostSparkDisabledWithoutClient proves the sweep is inert when no GitLab
// client is wired (nil GhostSparkMRState) — the default operator behavior when
// GitLab is unconfigured.
func TestGhostSparkDisabledWithoutClient(t *testing.T) {
	env := newRecEnv(t, nil)
	ctx := context.Background()
	seedEscalatedGhostSpark(t, env, "MILLS-GHOST-OFF", 1070, env.now.Add(-time.Hour))

	res, err := env.rec.SweepGhostSparks(ctx)
	if err != nil {
		t.Fatalf("disabled sweep: %v", err)
	}
	if res != (GhostSparkSweepResult{}) {
		t.Fatalf("disabled sweep must be a no-op, got %+v", res)
	}
	if got, _ := env.store.Backlog.Get(ctx, "MILLS-GHOST-OFF"); got.State != store.BacklogEscalated {
		t.Fatalf("item must be untouched when sweep disabled, got %s", got.State)
	}
}

// TestGhostSparkSkipsWhenLatestRunLacksMR proves the sweep keys off the
// MOST-RECENT run's MRIID: a requeued item whose newest attempt escalated
// before opening an MR is not ghost-closed on a stale earlier MR.
func TestGhostSparkSkipsWhenLatestRunLacksMR(t *testing.T) {
	env := newRecEnv(t, nil)
	ctx := context.Background()

	item := &store.BacklogItem{
		ID: "MILLS-REQUEUED", Title: "requeued then re-escalated",
		State: store.BacklogEscalated, Priority: store.P2, CreatedBy: "test",
	}
	if err := env.store.Backlog.Put(ctx, item); err != nil {
		t.Fatalf("seed item: %v", err)
	}
	// Attempt 1 opened MR !1037 (later merged) then escalated.
	iid := int64(1037)
	run1 := &store.PipelineRun{
		ID: "PIPE-REQ-1", BacklogID: item.ID, Template: "mills-default-pipeline",
		State: store.PipelineEscalated, Attempts: 1, MRIID: &iid,
		StartedAt: env.now.Add(-2 * time.Hour),
	}
	if err := env.store.Pipeline.PutRun(ctx, run1); err != nil {
		t.Fatalf("seed run1: %v", err)
	}
	// Attempt 2 (the most recent) escalated at implement — no MR.
	run2 := &store.PipelineRun{
		ID: "PIPE-REQ-2", BacklogID: item.ID, Template: "mills-default-pipeline",
		State: store.PipelineEscalated, Attempts: 2,
		StartedAt: env.now.Add(-time.Hour),
	}
	if err := env.store.Pipeline.PutRun(ctx, run2); err != nil {
		t.Fatalf("seed run2: %v", err)
	}

	mrs := &fakeMRStateClient{states: map[int64]string{1037: "merged"}}
	env.rec.GhostSparkMRState = mrs

	res, err := env.rec.SweepGhostSparks(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.Inspected != 0 || res.Merged != 0 {
		t.Fatalf("most-recent run lacks an MR — must skip: %+v", res)
	}
	if mrs.callCount() != 0 {
		t.Fatalf("no GitLab lookup expected when latest run has no MR, got %d", mrs.callCount())
	}
	if got, _ := env.store.Backlog.Get(ctx, item.ID); got.State != store.BacklogEscalated {
		t.Fatalf("item must stay escalated, got %s", got.State)
	}
}

func TestGhostSparkUsesPersistedProjectAfterItemReroute(t *testing.T) {
	tests := []struct {
		name             string
		persistedProject string
		mutatedTarget    string
	}{
		{name: "cross to home", persistedProject: "services/flexdeck", mutatedTarget: ""},
		{name: "home to cross", persistedProject: "services/loom-core", mutatedTarget: "services/flexdeck"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newRecEnv(t, nil)
			ctx := context.Background()
			seedEscalatedGhostSparkProject(t, env, "MILLS-REROUTE", 244, env.now.Add(-time.Hour), tt.persistedProject)
			item, _ := env.store.Backlog.Get(ctx, "MILLS-REROUTE")
			item.TargetProject = tt.mutatedTarget
			if err := env.store.Backlog.Put(ctx, item); err != nil {
				t.Fatalf("mutate target project: %v", err)
			}

			home := &fakeMRStateClient{states: map[int64]string{244: "opened"}}
			cross := &fakeMRStateClient{states: map[int64]string{244: "opened"}}
			var selected string
			env.rec.GhostSparkMRState = home
			env.rec.GhostSparkMRStateForProject = func(project string) MRStateClient {
				selected = project
				if project == "services/flexdeck" {
					return cross
				}
				return home
			}

			res, err := env.rec.SweepGhostSparks(ctx)
			if err != nil {
				t.Fatalf("sweep: %v", err)
			}
			if res.Inspected != 1 || res.Merged != 0 || selected != tt.persistedProject {
				t.Fatalf("sweep=%+v selected=%q, want persisted %q", res, selected, tt.persistedProject)
			}
			if tt.persistedProject == "services/flexdeck" && (cross.callCount() != 1 || home.callCount() != 0) {
				t.Fatalf("cross/home calls = %d/%d, want 1/0", cross.callCount(), home.callCount())
			}
			if tt.persistedProject == "services/loom-core" && (home.callCount() != 1 || cross.callCount() != 0) {
				t.Fatalf("home/cross calls = %d/%d, want 1/0", home.callCount(), cross.callCount())
			}
		})
	}
}

// TestGhostSparkNonResolvingCooldown guards the starvation fix: an open-MR
// candidate consumes a lookup once, then sits out the re-check cooldown so it
// cannot monopolize the per-tick budget across consecutive sweeps.
func TestGhostSparkNonResolvingCooldown(t *testing.T) {
	env := newRecEnv(t, nil)
	ctx := context.Background()

	seedEscalatedGhostSpark(t, env, "MILLS-OPEN-1", 501, env.now.Add(-2*time.Hour))
	mrs := &fakeMRStateClient{states: map[int64]string{501: "opened"}}
	env.rec.GhostSparkMRState = mrs

	if res, err := env.rec.SweepGhostSparks(ctx); err != nil || res.Inspected != 1 {
		t.Fatalf("first sweep should inspect the open-MR item once: res=%+v err=%v", res, err)
	}
	if res, err := env.rec.SweepGhostSparks(ctx); err != nil || res.Inspected != 0 {
		t.Fatalf("second sweep inside the cooldown must skip it: res=%+v err=%v", res, err)
	}
	if mrs.callCount() != 1 {
		t.Fatalf("expected exactly 1 GitLab lookup across both sweeps, got %d", mrs.callCount())
	}
}

// interposeMRStateClient runs a hook before delegating, letting a test mutate
// store state between the sweep's candidate listing and its transition — the
// optimistic-concurrency race the aggregate claim version must fence.
type interposeMRStateClient struct {
	inner *fakeMRStateClient
	hook  func()
}

func (f *interposeMRStateClient) MRState(ctx context.Context, mrIID int64) (string, error) {
	if f.hook != nil {
		f.hook()
	}
	return f.inner.MRState(ctx, mrIID)
}

// TestGhostSparkStaleClaimDoesNotResurrect exercises the riskiest-assumption
// race: a human requeues the item (escalated→queued, bumping the claim) after
// the sweep listed it but before it transitions. The stale transition must
// fail cleanly — the requeued item keeps running; nothing is force-merged.
func TestGhostSparkStaleClaimDoesNotResurrect(t *testing.T) {
	env := newRecEnv(t, nil)
	ctx := context.Background()

	seedEscalatedGhostSpark(t, env, "MILLS-RACE-1", 601, env.now.Add(-time.Hour))
	inner := &fakeMRStateClient{states: map[int64]string{601: "merged"}}
	env.rec.GhostSparkMRState = &interposeMRStateClient{inner: inner, hook: func() {
		item, err := env.store.Backlog.Get(ctx, "MILLS-RACE-1")
		if err != nil {
			t.Fatalf("race hook get: %v", err)
		}
		if _, err := env.store.Backlog.TransitionState(
			ctx, item.ID, item.ClaimVersion, store.BacklogEscalated, store.BacklogQueued,
		); err != nil {
			t.Fatalf("race hook requeue: %v", err)
		}
	}}
	resolver := &fakeGhostResolver{}
	env.rec.GhostSparkResolver = resolver

	res, err := env.rec.SweepGhostSparks(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.Merged != 0 {
		t.Fatalf("stale transition must not count as merged: %+v", res)
	}
	got, _ := env.store.Backlog.Get(ctx, "MILLS-RACE-1")
	if got.State != store.BacklogQueued {
		t.Fatalf("requeued item state: got %s want queued (sweep must not clobber the requeue)", got.State)
	}
	if resolver.count() != 0 {
		t.Fatalf("no issue close for a fenced-off transition")
	}
}

// TestGhostSparkReapPrunesRecheckEntry proves the process-local recheck map is
// pruned when an item is finally reaped as merged. A prior open/closed sweep
// leaves a cooldown entry; once the item transitions escalated→merged it can
// never be a candidate again, so the entry is dead weight and must be removed.
func TestGhostSparkReapPrunesRecheckEntry(t *testing.T) {
	env := newRecEnv(t, nil)
	ctx := context.Background()

	seedEscalatedGhostSpark(t, env, "MILLS-GHOST-PRUNE", 707, env.now.Add(-time.Hour))
	// Simulate a lapsed cooldown left behind by an earlier open/closed check: a
	// PAST deadline so the item is re-checked (not skipped) this sweep.
	env.rec.ghostSparkRecheck = map[string]time.Time{
		"MILLS-GHOST-PRUNE": time.Now().Add(-time.Hour),
	}

	mrs := &fakeMRStateClient{states: map[int64]string{707: "merged"}}
	env.rec.GhostSparkMRState = mrs

	res, err := env.rec.SweepGhostSparks(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.Merged != 1 {
		t.Fatalf("expected the item reaped as merged, got %+v", res)
	}
	got, _ := env.store.Backlog.Get(ctx, "MILLS-GHOST-PRUNE")
	if got.State != store.BacklogMerged {
		t.Fatalf("item state: got %s want merged", got.State)
	}
	if _, ok := env.rec.ghostSparkRecheck["MILLS-GHOST-PRUNE"]; ok {
		t.Fatalf("recheck entry must be pruned after the item is reaped as merged")
	}
}
