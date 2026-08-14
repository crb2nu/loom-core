package mills

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/store"
)

// fakeMergedBranchClient answers MergedMRForBranch from a fixed branch→merge
// table and records every branch it was asked about.
type fakeMergedBranchClient struct {
	mu     sync.Mutex
	merged map[string]struct {
		iid  int64
		when time.Time
	}
	asked []string
	err   error
}

type waitForDeadlineMRStateClient struct {
	cancel context.CancelFunc
}

func (c *waitForDeadlineMRStateClient) MRState(ctx context.Context, _ int64) (string, error) {
	if c.cancel != nil {
		c.cancel()
	}
	<-ctx.Done()
	return "", ctx.Err()
}

// testDeadlineContext lets a test expire a context as DeadlineExceeded without
// waiting for the wall clock. It is only installed through ghostSparkIIDContext
// with a non-cancelable parent.
type testDeadlineContext struct {
	context.Context
	done chan struct{}
}

func (c *testDeadlineContext) Done() <-chan struct{} { return c.done }

func (c *testDeadlineContext) Err() error {
	select {
	case <-c.done:
		return context.DeadlineExceeded
	default:
		return c.Context.Err()
	}
}

func (f *fakeMergedBranchClient) MergedMRForBranch(_ context.Context, branch string) (int64, time.Time, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, branch)
	if f.err != nil {
		return 0, time.Time{}, false, f.err
	}
	hit, ok := f.merged[branch]
	if !ok {
		return 0, time.Time{}, false, nil
	}
	return hit.iid, hit.when, true, nil
}

func (f *fakeMergedBranchClient) askedBranches() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.asked...)
}

// seedGateEscalatedItem inserts an escalated item whose most-recent run carries
// NO MRIID — the shape produced by escalating at a scope or docs gate, before
// the mr stage ever runs. This is precisely the population the IID-driven sweep
// cannot see.
func seedGateEscalatedItem(t *testing.T, env *recTestEnv, id string, started time.Time, project string) *store.BacklogItem {
	t.Helper()
	if env.rec.HomeProject == "" {
		env.rec.HomeProject = "services/loom-core"
	}
	ctx := context.Background()
	item := &store.BacklogItem{
		ID:            id,
		Title:         "escalated at the docs gate, branch merged by hand later",
		State:         store.BacklogEscalated,
		Priority:      store.P2,
		Slices:        []store.Slice{{Name: "the-slice", Files: []string{"a.go"}}},
		Budget:        store.Budget{MaxCostUSD: 1.0, MaxTurns: 30},
		TargetProject: project,
		CreatedBy:     "test",
	}
	if err := env.store.Backlog.Put(ctx, item); err != nil {
		t.Fatalf("seed item %s: %v", id, err)
	}
	run := &store.PipelineRun{
		ID:        "PIPE-" + id,
		BacklogID: id,
		Template:  "mills-default-pipeline",
		State:     store.PipelineEscalated,
		Attempts:  1,
		// No MRIID: the gate fired before the mr stage.
		StartedAt: started,
	}
	if err := env.store.Pipeline.PutRun(ctx, run); err != nil {
		t.Fatalf("seed run %s: %v", id, err)
	}
	return item
}

// enableMergedBranchPass wires the pass with a branch resolver that mirrors the
// production contract's shape (slice branch first, then source branch) without
// importing pkg/mills/pipeline — which imports this package.
func enableMergedBranchPass(env *recTestEnv, client MergedBranchMRClient) {
	env.rec.GhostSparkMergedBranch = client
	env.rec.GhostSparkBranchesFor = func(item *store.BacklogItem) []string {
		if item == nil {
			return nil
		}
		var out []string
		for _, s := range item.Slices {
			out = append(out, "feat/"+item.ID+"/"+s.Name)
		}
		return append(out, "feat/"+item.ID)
	}
}

// The kill test. An item that escalated at a gate — no MR IID anywhere — whose
// branch was merged by hand afterwards must transition escalated→merged. Before
// this pass existed the item stayed escalated forever: no IID for the MR-state
// sweep to look up, and no later run for the "later run succeeds" auto-close.
func TestMergedBranchSparkClosesGateEscalatedItem(t *testing.T) {
	env := newRecEnv(t, nil)
	ctx := context.Background()
	started := env.now.Add(-2 * time.Hour)
	item := seedGateEscalatedItem(t, env, "MILLS-DOCS-GATE", started, "")

	client := &fakeMergedBranchClient{merged: map[string]struct {
		iid  int64
		when time.Time
	}{
		"feat/MILLS-DOCS-GATE/the-slice": {iid: 1301, when: started.Add(time.Hour)},
	}}
	enableMergedBranchPass(env, client)

	res, err := env.rec.SweepGhostSparks(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.Merged != 1 {
		t.Fatalf("merged = %d, want 1 (%+v)", res.Merged, res)
	}
	got, err := env.store.Backlog.Get(ctx, item.ID)
	if err != nil {
		t.Fatalf("get item: %v", err)
	}
	if got.State != store.BacklogMerged {
		t.Fatalf("item state = %s, want merged", got.State)
	}

	// Idempotent: the item has left the escalated set, so a second sweep must
	// neither re-transition nor re-look-up.
	before := len(client.askedBranches())
	res2, err := env.rec.SweepGhostSparks(ctx)
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if res2.Merged != 0 {
		t.Fatalf("second sweep merged = %d, want 0", res2.Merged)
	}
	if after := len(client.askedBranches()); after != before {
		t.Fatalf("second sweep issued %d extra lookups, want 0", after-before)
	}
}

// A branch can carry a merged MR from an EARLIER attempt that was then requeued
// and escalated again for further work. Closing on that stale merge would
// discard a live escalation, so the merge must postdate the escalated attempt.
func TestMergedBranchSparkIgnoresMergeOlderThanAttempt(t *testing.T) {
	env := newRecEnv(t, nil)
	ctx := context.Background()
	started := env.now.Add(-time.Hour)
	item := seedGateEscalatedItem(t, env, "MILLS-STALE-MERGE", started, "")

	client := &fakeMergedBranchClient{merged: map[string]struct {
		iid  int64
		when time.Time
	}{
		// Merged two hours before this attempt even started.
		"feat/MILLS-STALE-MERGE/the-slice": {iid: 900, when: started.Add(-2 * time.Hour)},
	}}
	enableMergedBranchPass(env, client)

	res, err := env.rec.SweepGhostSparks(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.Merged != 0 {
		t.Fatalf("merged = %d, want 0 — a pre-attempt merge must not close a live escalation", res.Merged)
	}
	if got, _ := env.store.Backlog.Get(ctx, item.ID); got.State != store.BacklogEscalated {
		t.Fatalf("item state = %s, want escalated", got.State)
	}
}

// Without a per-project client the pass is home-only: cross-repo items are
// left alone rather than routed through mutable target_project. (With the
// client wired, the escalation-time binding event authorizes the lookup — see
// reconciler_cross_repo_merged_branch_test.go.)
func TestMergedBranchSparkSkipsForeignProject(t *testing.T) {
	env := newRecEnv(t, nil)
	ctx := context.Background()
	started := env.now.Add(-time.Hour)
	item := seedGateEscalatedItem(t, env, "MILLS-XREPO", started, "services/flexdeck")

	client := &fakeMergedBranchClient{merged: map[string]struct {
		iid  int64
		when time.Time
	}{
		"feat/MILLS-XREPO/the-slice": {iid: 77, when: started.Add(time.Minute)},
	}}
	enableMergedBranchPass(env, client)

	res, err := env.rec.SweepGhostSparks(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.Merged != 0 {
		t.Fatalf("merged = %d, want 0 for a foreign-project item", res.Merged)
	}
	if n := len(client.askedBranches()); n != 0 {
		t.Fatalf("issued %d lookups for a foreign-project item, want 0", n)
	}
	if got, _ := env.store.Backlog.Get(ctx, item.ID); got.State != store.BacklogEscalated {
		t.Fatalf("item state = %s, want escalated", got.State)
	}
}

// Nil wiring must leave the sweep byte-for-byte as it was, so enabling the pass
// is opt-in and a rollback is a config change rather than a revert.
func TestMergedBranchSparkDisabledWithoutWiring(t *testing.T) {
	env := newRecEnv(t, nil)
	ctx := context.Background()
	seedGateEscalatedItem(t, env, "MILLS-NOWIRE", env.now.Add(-time.Hour), "")

	// MR-state client present (so the sweep runs at all) but no merged-branch wiring.
	env.rec.GhostSparkMRState = &fakeMRStateClient{states: map[int64]string{}}

	res, err := env.rec.SweepGhostSparks(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.Merged != 0 || res.Inspected != 0 {
		t.Fatalf("disabled pass must do nothing, got %+v", res)
	}
	if got, _ := env.store.Backlog.Get(ctx, "MILLS-NOWIRE"); got.State != store.BacklogEscalated {
		t.Fatalf("item state = %s, want escalated", got.State)
	}
}

// A lookup failure defers the item instead of closing it or aborting the sweep.
func TestMergedBranchSparkLookupErrorDefersItem(t *testing.T) {
	env := newRecEnv(t, nil)
	ctx := context.Background()
	seedGateEscalatedItem(t, env, "MILLS-LOOKUP-ERR", env.now.Add(-time.Hour), "")

	client := &fakeMergedBranchClient{err: errors.New("gitlab: 502")}
	enableMergedBranchPass(env, client)

	res, err := env.rec.SweepGhostSparks(ctx)
	if err != nil {
		t.Fatalf("sweep must not fail on a per-item lookup error: %v", err)
	}
	if res.Merged != 0 {
		t.Fatalf("merged = %d, want 0", res.Merged)
	}
	if res.Errored != 1 {
		t.Fatalf("errored = %d, want 1", res.Errored)
	}
	if got, _ := env.store.Backlog.Get(ctx, "MILLS-LOOKUP-ERR"); got.State != store.BacklogEscalated {
		t.Fatalf("item state = %s, want escalated", got.State)
	}
}

// Production regression: the merged-branch pass originally shared the IID
// pass's per-pass lookup counter, so with a large escalated-with-MR pile the
// IID pass spent the whole allowance first and the branch pass returned
// immediately on every tick — it closed nothing across 16 consecutive
// reconciler ticks. Its budget is now reserved, so a saturated IID pass cannot
// starve it.
func TestMergedBranchSparkNotStarvedByIIDPass(t *testing.T) {
	env := newRecEnv(t, nil)
	ctx := context.Background()
	env.rec.HomeProject = "services/loom-core"
	started := env.now.Add(-2 * time.Hour)

	// Saturate the IID pass: more escalated-with-MR items than its per-tick cap.
	for i := 0; i < ghostSparkGitLabLookupsPerPass+4; i++ {
		seedEscalatedGhostSpark(t, env, fmt.Sprintf("MILLS-IID-%02d", i), int64(2000+i), started)
	}
	// Every one of those MRs is still open, so the IID pass burns a lookup on
	// each and closes nothing — exactly the production shape.
	states := map[int64]string{}
	for i := 0; i < ghostSparkGitLabLookupsPerPass+4; i++ {
		states[int64(2000+i)] = "opened"
	}
	env.rec.GhostSparkMRState = &fakeMRStateClient{states: states}

	// One gate-escalated item whose branch merged — the branch pass must still
	// get a turn and close it.
	seedGateEscalatedItem(t, env, "MILLS-BRANCH-VICTIM", started, "")
	client := &fakeMergedBranchClient{merged: map[string]struct {
		iid  int64
		when time.Time
	}{
		"feat/MILLS-BRANCH-VICTIM/the-slice": {iid: 1301, when: started.Add(time.Hour)},
	}}
	enableMergedBranchPass(env, client)

	res, err := env.rec.SweepGhostSparks(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.BranchCandidates == 0 {
		t.Fatalf("branch pass saw no candidates; it never ran (%+v)", res)
	}
	if res.BranchMerged != 1 {
		t.Fatalf("branch_merged = %d, want 1 — a saturated IID pass must not starve the branch pass (%+v)",
			res.BranchMerged, res)
	}
	if got, _ := env.store.Backlog.Get(ctx, "MILLS-BRANCH-VICTIM"); got.State != store.BacklogMerged {
		t.Fatalf("state = %s, want merged", got.State)
	}
}

func TestMergedBranchSparkRetainsReservedDeadlineAfterIIDTimeout(t *testing.T) {
	env := newRecEnv(t, nil)
	env.rec.HomeProject = "services/loom-core"
	started := env.now.Add(-2 * time.Hour)
	seedEscalatedGhostSpark(t, env, "MILLS-IID-SLOW", 2300, started)
	var expireIIDContext context.CancelFunc
	env.rec.ghostSparkIIDContext = func(parent context.Context) (context.Context, context.CancelFunc) {
		ctx := &testDeadlineContext{Context: parent, done: make(chan struct{})}
		var once sync.Once
		cancel := func() { once.Do(func() { close(ctx.done) }) }
		expireIIDContext = cancel
		return ctx, cancel
	}
	env.rec.GhostSparkMRState = &waitForDeadlineMRStateClient{cancel: func() {
		expireIIDContext()
	}}
	seedGateEscalatedItem(t, env, "MILLS-BRANCH-AFTER-TIMEOUT", started, "")
	client := &fakeMergedBranchClient{merged: map[string]struct {
		iid  int64
		when time.Time
	}{
		"feat/MILLS-BRANCH-AFTER-TIMEOUT/the-slice": {iid: 2301, when: started.Add(time.Hour)},
	}}
	enableMergedBranchPass(env, client)

	res, err := env.rec.SweepGhostSparks(context.Background())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.BranchMerged != 1 {
		t.Fatalf("branch merged = %d, want 1 after IID sub-deadline (%+v)", res.BranchMerged, res)
	}
}

// One heavily-sliced item must not spend the whole tick's GitLab budget.
func TestMergedBranchSparkCapsLookupsPerItem(t *testing.T) {
	env := newRecEnv(t, nil)
	ctx := context.Background()
	env.rec.HomeProject = "services/loom-core"
	item := &store.BacklogItem{
		ID: "MILLS-MANY-SLICES", Title: "many slices", State: store.BacklogEscalated,
		Priority: store.P2, CreatedBy: "test",
	}
	for i := 0; i < 12; i++ {
		item.Slices = append(item.Slices, store.Slice{Name: string(rune('a' + i))})
	}
	if err := env.store.Backlog.Put(ctx, item); err != nil {
		t.Fatalf("seed item: %v", err)
	}
	if err := env.store.Pipeline.PutRun(ctx, &store.PipelineRun{
		ID: "PIPE-MANY", BacklogID: item.ID, Template: "mills-default-pipeline",
		State: store.PipelineEscalated, Attempts: 1, StartedAt: env.now.Add(-time.Hour),
	}); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	client := &fakeMergedBranchClient{}
	enableMergedBranchPass(env, client)

	if _, err := env.rec.SweepGhostSparks(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	// Exactly the cap, not merely "at most": zero would also satisfy <=, and
	// would mean the pass never ran rather than that it capped correctly.
	if n := len(client.askedBranches()); n != ghostSparkBranchLookupsPerItem {
		t.Fatalf("issued %d lookups for a 12-slice item, want exactly %d", n, ghostSparkBranchLookupsPerItem)
	}
}
