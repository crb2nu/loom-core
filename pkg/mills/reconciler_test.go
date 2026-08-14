package mills

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/crb2nu/loom/pkg/mills/store"
)

// recordingStarter captures pipeline-start invocations for assertion.
type recordingStarter struct {
	mu    sync.Mutex
	runs  []*store.PipelineRun
	items []*store.BacklogItem
	fail  error
}

type blockingStarter struct {
	mu      sync.Mutex
	calls   int
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func newBlockingStarter() *blockingStarter {
	return &blockingStarter{entered: make(chan struct{}), release: make(chan struct{})}
}

func (s *blockingStarter) Start(context.Context, *store.PipelineRun, *store.BacklogItem) error {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	s.once.Do(func() { close(s.entered) })
	<-s.release
	return nil
}

func (s *blockingStarter) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *recordingStarter) Start(_ context.Context, run *store.PipelineRun, item *store.BacklogItem) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return s.fail
	}
	s.runs = append(s.runs, run)
	s.items = append(s.items, item)
	return nil
}

func (s *recordingStarter) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.runs)
}

// recTestEnv wires a real SQLite store + a SkipWatch policy manager + a
// fake starter for fast in-process reconciler tests.
type recTestEnv struct {
	store   *store.Store
	policy  *PolicyManager
	starter *recordingStarter
	rec     *Reconciler
	now     time.Time
}

func newRecEnv(t *testing.T, policyMutator func(*Policy)) *recTestEnv {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(context.Background(), store.Options{Path: filepath.Join(dir, "h.db")})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	// Persist a baseline policy file so PolicyManager can load it. We use
	// SkipWatch so tests don't depend on fsnotify timing.
	p := Default()
	on := true
	p.Enabled = &on
	if policyMutator != nil {
		policyMutator(p)
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("policy: %v", err)
	}
	policyPath := filepath.Join(dir, "policy.yaml")
	writePolicyYAMLForTest(t, policyPath, p)
	pm, err := NewPolicyManager(context.Background(), policyPath, PolicyManagerOptions{SkipWatch: true})
	if err != nil {
		t.Fatalf("policy mgr: %v", err)
	}
	t.Cleanup(func() { _ = pm.Close() })

	starter := &recordingStarter{}
	rec := NewReconciler(st, pm, NewBudget(pm, NewStoreBudgetReader(st)), starter)
	now := time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC)
	rec.Clock = func() time.Time { return now }

	return &recTestEnv{store: st, policy: pm, starter: starter, rec: rec, now: now}
}

// writePolicyYAMLForTest is a tiny YAML writer that gets us a parseable
// policy file without pulling yaml.v3 marshaling into the test surface.
// We rely on the fixtureV1 string from policy_test.go; this helper just
// drops a known-valid file at path.
func writePolicyYAMLForTest(t *testing.T, path string, p *Policy) {
	t.Helper()
	body := fixtureV1
	// The fixture is a fixed string, so mutator-set fields NOT present in it
	// silently vanish on the file round-trip. Inject the ones tests actually
	// toggle. (A field forgotten here fails its test visibly — the flag reads
	// back false — rather than corrupting other tests.)
	if p.Pipeline.AutoRequeue.IncludeCodeConfig {
		body = strings.Replace(body,
			"pipeline:\n  default_template: mills-default-pipeline",
			"pipeline:\n  auto_requeue: { include_code_config: true }\n  default_template: mills-default-pipeline", 1)
	}
	if p.Enabled != nil && !*p.Enabled {
		body = "version: 1\nenabled: false\n" +
			"budgets:\n  council:  { max_usd_per_run: 1, max_usd_per_day: 1 }\n  pipeline: { max_usd_per_run: 1, max_usd_per_day: 1 }\n" +
			"pipeline:\n  retry: { max_attempts: 1, cooldown_seconds: 0 }\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write policy: %v", err)
	}
}

// ----- Tests -----

func TestReconciler_PolicyDisabledShortCircuits(t *testing.T) {
	env := newRecEnv(t, func(p *Policy) {
		off := false
		p.Enabled = &off
	})
	res, err := env.rec.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if res.SkipReason != "policy disabled" {
		t.Errorf("expected policy-disabled skip, got %+v", res)
	}
	if env.starter.calls() != 0 {
		t.Errorf("starter should not be invoked when policy is off")
	}
}

func TestReconciler_AutonomyGateBlocksStarts(t *testing.T) {
	env := newRecEnv(t, nil)
	env.rec.AutonomyGate = func(context.Context) (bool, []string) {
		return false, []string{"repo_root: .loom directory is missing under repo root"}
	}
	ctx := context.Background()

	item := &store.BacklogItem{
		ID:        "MILLS-GATED",
		Title:     "blocked by capability",
		State:     store.BacklogQueued,
		Priority:  store.P2,
		CreatedBy: "test",
	}
	if err := env.store.Backlog.Put(ctx, item); err != nil {
		t.Fatalf("seed: %v", err)
	}

	res, err := env.rec.Tick(ctx)
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if res.SkipReason != "autonomy blocked" {
		t.Fatalf("skip reason: got %q want autonomy blocked", res.SkipReason)
	}
	if env.starter.calls() != 0 {
		t.Fatalf("starter calls: got %d want 0", env.starter.calls())
	}
	got, err := env.store.Backlog.Get(ctx, item.ID)
	if err != nil {
		t.Fatalf("read backlog: %v", err)
	}
	if got.State != store.BacklogQueued {
		t.Fatalf("backlog state: got %q want %q", got.State, store.BacklogQueued)
	}
	runs, err := env.store.Pipeline.ListByBacklog(ctx, item.ID)
	if err != nil {
		t.Fatalf("read runs: %v", err)
	}
	if len(runs) != 0 {
		t.Fatalf("pipeline runs: got %d want 0", len(runs))
	}
}

func TestReconciler_StartsQueuedItem(t *testing.T) {
	env := newRecEnv(t, nil)
	ctx := context.Background()

	item := &store.BacklogItem{
		ID:        "MILLS-1",
		Title:     "first",
		State:     store.BacklogQueued,
		Priority:  store.P2,
		Slices:    []store.Slice{{Name: "x", Files: []string{"a.go"}}},
		Budget:    store.Budget{MaxCostUSD: 1.0, MaxTurns: 30},
		CreatedBy: "test",
	}
	if err := env.store.Backlog.Put(ctx, item); err != nil {
		t.Fatalf("seed: %v", err)
	}
	res, err := env.rec.Tick(ctx)
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if res.Started != 1 {
		t.Errorf("expected 1 started, got %+v", res)
	}
	if env.starter.calls() != 1 {
		t.Errorf("starter not invoked")
	}

	got, _ := env.store.Backlog.Get(ctx, item.ID)
	if got.State != store.BacklogRunning {
		t.Errorf("backlog item not transitioned: %v", got.State)
	}
	if got.ClaimVersion != 1 {
		t.Errorf("backlog claim version: got %d want 1", got.ClaimVersion)
	}
	runs, _ := env.store.Pipeline.ListByBacklog(ctx, item.ID)
	if len(runs) != 1 {
		t.Errorf("pipeline run not persisted; got %d", len(runs))
	}
	if len(runs) == 1 {
		wf, err := env.store.Workflow.GetWorkflowRun(ctx, runs[0].ID)
		if err != nil {
			t.Fatalf("workflow metadata not initialized with claim: %v", err)
		}
		if wf.Engine != store.WorkflowEngineDAG || wf.BacklogID != item.ID || wf.Template != runs[0].Template {
			t.Fatalf("workflow metadata mismatch: %+v", wf)
		}
	}
	if pending, err := env.store.CountPendingDispatches(ctx); err != nil || pending != 0 {
		t.Fatalf("dispatch outbox after starter acceptance: count=%d err=%v", pending, err)
	}
}

// TestReconciler_StampsOperatorSessionOnStartedRun proves the backlog-driven
// start path threads the operator's agent-context session onto the run as
// ParentSessionID. Without it the field stayed empty for every production run
// (only the integrator's fan-out path set it), so stage spawns got no
// LOOM_PARENT_SESSION_ID and started with no continuity to the operator.
func TestReconciler_StampsOperatorSessionOnStartedRun(t *testing.T) {
	env := newRecEnv(t, nil)
	ctx := context.Background()

	// Resolved per claim, not frozen at construction: the operator re-establishes
	// its session after a hub outage and the reconciler must pick up the new id.
	current := ""
	env.rec.OperatorSessionID = func() string { return current }

	seed := func(id string) {
		t.Helper()
		item := &store.BacklogItem{
			ID:        id,
			Title:     id,
			State:     store.BacklogQueued,
			Priority:  store.P2,
			Slices:    []store.Slice{{Name: "x", Files: []string{id + ".go"}}},
			Budget:    store.Budget{MaxCostUSD: 1.0, MaxTurns: 30},
			CreatedBy: "test",
		}
		if err := env.store.Backlog.Put(ctx, item); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	parentOf := func(backlogID string) string {
		t.Helper()
		runs, err := env.store.Pipeline.ListByBacklog(ctx, backlogID)
		if err != nil || len(runs) != 1 {
			t.Fatalf("expected 1 run for %s; got %d err=%v", backlogID, len(runs), err)
		}
		return runs[0].ParentSessionID
	}

	// Hub down at boot: no session yet, so runs must start unstamped rather
	// than fail.
	seed("MILLS-NOSESSION")
	if _, err := env.rec.Tick(ctx); err != nil {
		t.Fatalf("tick (no session): %v", err)
	}
	if got := parentOf("MILLS-NOSESSION"); got != "" {
		t.Errorf("ParentSessionID = %q, want empty when the operator has no session", got)
	}

	// Session established (or re-established) by the maintainer.
	current = "session-op-1"
	seed("MILLS-SESSION")
	if _, err := env.rec.Tick(ctx); err != nil {
		t.Fatalf("tick (session): %v", err)
	}
	if got := parentOf("MILLS-SESSION"); got != "session-op-1" {
		t.Errorf("ParentSessionID = %q, want session-op-1", got)
	}

	// The starter sees the same stamped run the store persisted, which is what
	// the dispatcher reads to build LOOM_PARENT_SESSION_ID.
	var started *store.PipelineRun
	for _, run := range env.starter.runs {
		if run.BacklogID == "MILLS-SESSION" {
			started = run
		}
	}
	if started == nil {
		t.Fatal("starter never saw the MILLS-SESSION run")
	}
	if started.ParentSessionID != "session-op-1" {
		t.Errorf("starter run ParentSessionID = %q, want session-op-1", started.ParentSessionID)
	}
}

func TestReconciler_TenThousandQueueTickIsBounded(t *testing.T) {
	env := newRecEnv(t, nil)
	ctx := context.Background()

	tx, err := env.store.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin queue seed: %v", err)
	}
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO backlog_items (
			id, title, labels_json, state, priority, success_json, budget_json,
			policy_json, slices_json, dependencies_json, created_by, created_at,
			updated_at, claim_version
		) VALUES (?, ?, '[]', 'queued', 'P1', '{}', '{}', '{}', '[]', '[]',
			'test', ?, ?, 0)
	`)
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("prepare queue seed: %v", err)
	}
	for i := range 10_000 {
		id := fmt.Sprintf("MILLS-TICK-QUEUE-%05d", i)
		created := env.now.Add(time.Duration(i) * time.Microsecond).Format(time.RFC3339Nano)
		if _, err := stmt.ExecContext(ctx, id, "queued item "+id, created, created); err != nil {
			_ = stmt.Close()
			_ = tx.Rollback()
			t.Fatalf("seed queue row %d: %v", i, err)
		}
	}
	if err := stmt.Close(); err != nil {
		_ = tx.Rollback()
		t.Fatalf("close queue statement: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit queue seed: %v", err)
	}

	limit := queuedAdmissionBatchSize(env.policy.Current())
	if limit != 4 {
		t.Fatalf("fixture admission limit=%d want 4", limit)
	}
	var claimAttempts atomic.Int64
	var claimBoundaries atomic.Int64
	env.rec.ClaimFaultHook = func(point store.ClaimPipelineStartFaultPoint) error {
		claimBoundaries.Add(1)
		if point == store.ClaimFaultAfterBacklogCAS {
			claimAttempts.Add(1)
		}
		return nil
	}

	started := time.Now()
	result, err := env.rec.Tick(ctx)
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("10k queue tick: %v", err)
	}
	if result.Inspected != limit || result.Started != limit || result.Errored != 0 {
		t.Fatalf("bounded tick result=%+v want inspected/started=%d", result, limit)
	}
	if got := int(claimAttempts.Load()); got != limit {
		t.Fatalf("claim attempts=%d want %d", got, limit)
	}
	boundaries := claimBoundaries.Load()
	perClaim := boundaries / claimAttempts.Load()
	if boundaries != claimAttempts.Load()*10 || perClaim >= 20 {
		t.Fatalf("claim boundaries=%d attempts=%d per_claim=%d; want 10 and <20 per claim",
			boundaries, claimAttempts.Load(), perClaim)
	}
	var runCount, queuedCount int
	if err := env.store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM pipeline_runs`).Scan(&runCount); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if err := env.store.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM backlog_items WHERE state = 'queued'`).Scan(&queuedCount); err != nil {
		t.Fatalf("count queued: %v", err)
	}
	if runCount != limit || queuedCount != 10_000-limit || env.starter.calls() != limit {
		t.Fatalf("runs=%d queued=%d starter_calls=%d want %d/%d/%d",
			runCount, queuedCount, env.starter.calls(), limit, 10_000-limit, limit)
	}
	// Latency is reported, not asserted. Every bound this test actually owns is
	// a count: it inspects `limit` items out of 10k, claims exactly `limit`, and
	// crosses a fixed number of SQL boundaries per claim, all of which hold on
	// any machine at any load. The one regression a wall-clock bound added — the
	// admission read degrading into a full sort of the queue before its LIMIT
	// applies — is pinned deterministically by the query plan in
	// TestListByStateLimit_AdmissionReadStaysIndexOrderedAtTenThousandRows
	// (pkg/mills/store). The `elapsed >= 2*time.Second` assertion that used to
	// live here measured the runner instead: it failed on main in test:race at
	// 2.481s (job 225918, 2026-08-09) under three concurrent main pipelines,
	// passed the same commit at 784ms in test:reliability, and runs at ~60ms on
	// an idle laptop — 33x of headroom it could never spend catching a bug.
	t.Logf("10k queue tick: inspected=%d claims=%d claim_boundaries=%d per_claim=%d latency=%s",
		result.Inspected, claimAttempts.Load(), boundaries, perClaim, elapsed)
}

// TestReconciler_RequeuedItemStartsNextAttempt pins the escalation
// auto-retry contract: when a transiently-escalated run is left in place
// and the backlog item is re-queued, the next reconciler tick must spin
// up a fresh pipeline_run with an incremented attempt number rather than
// colliding on the pipeline_runs UNIQUE(backlog_id, attempts) constraint.
//
// Regression: tryStart previously hardcoded Attempts=1, so the fresh run
// failed PutRun every tick and the item wedged queued forever (errored
// ticks, never started).
func TestReconciler_RequeuedItemStartsNextAttempt(t *testing.T) {
	env := newRecEnv(t, nil)
	ctx := context.Background()

	item := &store.BacklogItem{
		ID:        "MILLS-REQUEUE",
		Title:     "re-queued after transient escalation",
		State:     store.BacklogQueued,
		Priority:  store.P2,
		Slices:    []store.Slice{{Name: "x", Files: []string{"a.go"}}},
		Budget:    store.Budget{MaxCostUSD: 1.0, MaxTurns: 30},
		CreatedBy: "test",
	}
	if err := env.store.Backlog.Put(ctx, item); err != nil {
		t.Fatalf("seed item: %v", err)
	}

	// Simulate the post-auto-retry state: a prior run at attempts=1 sits
	// escalated while the backlog item is back in the queued state.
	prior := &store.PipelineRun{
		ID:        "PIPE-MILLS-REQUEUE-1",
		BacklogID: item.ID,
		Template:  "mills-default-pipeline",
		State:     store.PipelineEscalated,
		Attempts:  1,
		StartedAt: env.now.Add(-time.Hour),
	}
	if err := env.store.Pipeline.PutRun(ctx, prior); err != nil {
		t.Fatalf("seed prior run: %v", err)
	}

	res, err := env.rec.Tick(ctx)
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if res.Started != 1 || res.Errored != 0 {
		t.Fatalf("expected started=1 errored=0, got %+v", res)
	}
	if env.starter.calls() != 1 {
		t.Fatalf("starter not invoked: calls=%d", env.starter.calls())
	}

	runs, err := env.store.Pipeline.ListByBacklog(ctx, item.ID)
	if err != nil {
		t.Fatalf("read runs: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("expected 2 runs (prior + fresh), got %d", len(runs))
	}
	// ListByBacklog orders by attempts ASC; the fresh run must be attempt 2.
	if got := runs[len(runs)-1].Attempts; got != 2 {
		t.Fatalf("fresh run attempt: got %d want 2", got)
	}
	got, _ := env.store.Backlog.Get(ctx, item.ID)
	if got.State != store.BacklogRunning {
		t.Fatalf("backlog item not transitioned: %v", got.State)
	}
}

// TestReconciler_StartQueuedItemRequeuesEscalated pins the human
// re-run-after-escalation contract: StartQueuedItemOpts with
// RequeueEscalated flips an escalated item back to queued and starts a
// fresh run; without the option the same item still refuses with
// ErrBacklogNotQueued (the prior 409 the HUD hand-off dead-ended on).
// Merged items refuse even with the option — requeue is escalated-only.
func TestReconciler_StartQueuedItemRequeuesEscalated(t *testing.T) {
	env := newRecEnv(t, nil)
	ctx := context.Background()

	item := &store.BacklogItem{
		ID:        "MILLS-ESC",
		Title:     "escalated by a prior run",
		State:     store.BacklogEscalated,
		Priority:  store.P2,
		Slices:    []store.Slice{{Name: "x", Files: []string{"a.go"}}},
		Budget:    store.Budget{MaxCostUSD: 1.0, MaxTurns: 30},
		CreatedBy: "test",
	}
	if err := env.store.Backlog.Put(ctx, item); err != nil {
		t.Fatalf("seed item: %v", err)
	}
	prior := &store.PipelineRun{
		ID:        "PIPE-MILLS-ESC-1",
		BacklogID: item.ID,
		Template:  "mills-default-pipeline",
		State:     store.PipelineEscalated,
		Attempts:  1,
		StartedAt: env.now.Add(-time.Hour),
	}
	if err := env.store.Pipeline.PutRun(ctx, prior); err != nil {
		t.Fatalf("seed prior run: %v", err)
	}

	// Without the option: unchanged 409 behavior.
	if _, err := env.rec.StartQueuedItem(ctx, item.ID); !errors.Is(err, ErrBacklogNotQueued) {
		t.Fatalf("expected ErrBacklogNotQueued without requeue, got %v", err)
	}

	// With the option: requeued and started at the next attempt number.
	res, err := env.rec.StartQueuedItemOpts(ctx, item.ID, StartQueuedOptions{RequeueEscalated: true})
	if err != nil {
		t.Fatalf("requeue start: %v", err)
	}
	if res.Run == nil || res.Run.Attempts != 2 {
		t.Fatalf("expected fresh run at attempt 2, got %+v", res.Run)
	}
	got, _ := env.store.Backlog.Get(ctx, item.ID)
	if got.State != store.BacklogRunning {
		t.Fatalf("backlog item not transitioned: %v", got.State)
	}

	// Merged items refuse even with requeue.
	merged := &store.BacklogItem{
		ID: "MILLS-MERGED", Title: "done", State: store.BacklogMerged,
		Priority: store.P3, CreatedBy: "test",
	}
	if err := env.store.Backlog.Put(ctx, merged); err != nil {
		t.Fatalf("seed merged: %v", err)
	}
	if _, err := env.rec.StartQueuedItemOpts(ctx, merged.ID, StartQueuedOptions{RequeueEscalated: true}); !errors.Is(err, ErrBacklogNotQueued) {
		t.Fatalf("expected ErrBacklogNotQueued for merged item, got %v", err)
	}
}

// TestReconciler_StartQueuedItemSurfacesDeferralReason pins the
// reason-surfacing contract: a deferred/skipped start must carry the
// concrete gate verdict in StartQueuedResult.Reason instead of the old
// opaque "not started" (the procmodel hand-off 409 gave operators no
// hint that the daily run cap was the blocker).
func TestReconciler_StartQueuedItemSurfacesDeferralReason(t *testing.T) {
	env := newRecEnv(t, nil)
	ctx := context.Background()

	parent := &store.BacklogItem{
		ID: "MILLS-DEP-PARENT", Title: "parent", State: store.BacklogQueued,
		Priority: store.P2, CreatedBy: "test",
	}
	child := &store.BacklogItem{
		ID: "MILLS-DEP-CHILD", Title: "child", State: store.BacklogQueued,
		Priority: store.P2, CreatedBy: "test",
		Dependencies: []string{parent.ID},
	}
	for _, it := range []*store.BacklogItem{parent, child} {
		if err := env.store.Backlog.Put(ctx, it); err != nil {
			t.Fatalf("seed %s: %v", it.ID, err)
		}
	}

	res, err := env.rec.StartQueuedItem(ctx, child.ID)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if res.Run != nil {
		t.Fatalf("child must not start while parent is unmerged")
	}
	if want := "blocked by dependency MILLS-DEP-PARENT (not merged)"; res.Reason != want {
		t.Fatalf("reason: got %q want %q", res.Reason, want)
	}
}

func TestReconciler_DefersOnUnmetDependency(t *testing.T) {
	env := newRecEnv(t, nil)
	ctx := context.Background()

	parent := &store.BacklogItem{
		ID: "MILLS-PARENT", Title: "parent", State: store.BacklogQueued,
		Priority: store.P2, CreatedBy: "test",
	}
	child := &store.BacklogItem{
		ID: "MILLS-CHILD", Title: "child", State: store.BacklogQueued,
		Priority: store.P2, CreatedBy: "test",
		Dependencies: []string{parent.ID},
	}
	if err := env.store.Backlog.Put(ctx, parent); err != nil {
		t.Fatalf("seed parent: %v", err)
	}
	if err := env.store.Backlog.Put(ctx, child); err != nil {
		t.Fatalf("seed child: %v", err)
	}

	res, err := env.rec.Tick(ctx)
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	// Parent should start; child should defer because parent isn't merged.
	if res.Started != 1 || res.Deferred != 1 {
		t.Errorf("expected started=1 deferred=1, got %+v", res)
	}

	// Mark parent merged + retry. Child must now start.
	parent, err = env.store.Backlog.Get(ctx, parent.ID)
	if err != nil {
		t.Fatalf("reload parent: %v", err)
	}
	parent.State = store.BacklogMerged
	if err := env.store.Backlog.Put(ctx, parent); err != nil {
		t.Fatalf("merge parent: %v", err)
	}
	res2, err := env.rec.Tick(ctx)
	if err != nil {
		t.Fatalf("tick2: %v", err)
	}
	if res2.Started != 1 {
		t.Errorf("expected child to start after parent merged, got %+v", res2)
	}
}

func TestReconciler_RespectsHumanReviewPolicy(t *testing.T) {
	env := newRecEnv(t, nil)
	ctx := context.Background()

	item := &store.BacklogItem{
		ID: "MILLS-HR", Title: "needs human", State: store.BacklogQueued,
		Priority: store.P2, CreatedBy: "test",
		Policy: store.ItemPolicy{RequireHumanReview: true},
	}
	if err := env.store.Backlog.Put(ctx, item); err != nil {
		t.Fatalf("seed: %v", err)
	}
	res, err := env.rec.Tick(ctx)
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if res.Skipped != 1 || res.Started != 0 {
		t.Errorf("expected skipped=1 started=0, got %+v", res)
	}
	if env.starter.calls() != 0 {
		t.Errorf("starter must not run for human-review items")
	}
}

func TestReconciler_StarterFailureLeavesCommittedDispatchForRetry(t *testing.T) {
	env := newRecEnv(t, nil)
	env.starter.fail = errors.New("boom")
	ctx := context.Background()

	item := &store.BacklogItem{
		ID: "MILLS-FAIL", Title: "fail", State: store.BacklogQueued,
		Priority: store.P2, CreatedBy: "test",
		Slices: []store.Slice{{Name: "x", Files: []string{"a.go"}}},
		Budget: store.Budget{MaxCostUSD: 1},
	}
	if err := env.store.Backlog.Put(ctx, item); err != nil {
		t.Fatalf("seed: %v", err)
	}
	res, _ := env.rec.Tick(ctx)
	// The claim commits before Starter. A failure is therefore an errored
	// delivery with one durable pending intent, not a rollback to queued.
	if res.Errored != 1 {
		t.Errorf("expected errored=1, got %+v", res)
	}
	got, err := env.store.Backlog.Get(ctx, item.ID)
	if err != nil {
		t.Fatalf("get backlog: %v", err)
	}
	if got.State != store.BacklogRunning || got.ClaimVersion != 1 {
		t.Fatalf("committed backlog claim: %+v", got)
	}
	if pending, err := env.store.CountPendingDispatches(ctx); err != nil || pending != 1 {
		t.Fatalf("pending dispatches after starter failure: count=%d err=%v", pending, err)
	}
	runs, err := env.store.Pipeline.ListByBacklog(ctx, item.ID)
	if err != nil || len(runs) != 1 {
		t.Fatalf("committed pipeline run: count=%d err=%v", len(runs), err)
	}

	// Model an operator restart with a healthy starter. The next tick drains
	// the committed outbox intent exactly once without creating another run.
	recoveredStarter := &recordingStarter{}
	recovered := NewReconciler(env.store, env.policy, NewBudget(env.policy, NewStoreBudgetReader(env.store)), recoveredStarter)
	recovered.Clock = func() time.Time { return env.now.Add(2 * time.Second) }
	recovery, err := recovered.Tick(ctx)
	if err != nil {
		t.Fatalf("recovery tick: %v", err)
	}
	if recovery.Started != 1 || recovery.Errored != 0 || recoveredStarter.calls() != 1 {
		t.Fatalf("recovery result=%+v starter_calls=%d", recovery, recoveredStarter.calls())
	}
	if pending, err := env.store.CountPendingDispatches(ctx); err != nil || pending != 0 {
		t.Fatalf("pending dispatches after recovery: count=%d err=%v", pending, err)
	}
	runs, err = env.store.Pipeline.ListByBacklog(ctx, item.ID)
	if err != nil || len(runs) != 1 {
		t.Fatalf("recovery created duplicate run: count=%d err=%v", len(runs), err)
	}
}

func TestReconciler_DispatchDeadLetterRecordsTerminalMetrics(t *testing.T) {
	env := newRecEnv(t, nil)
	ctx := context.Background()
	item := &store.BacklogItem{
		ID: "MILLS-DISPATCH-METRICS", Title: "dispatch metrics",
		State: store.BacklogQueued, Priority: store.P1, CreatedBy: "test",
		Budget: store.Budget{MaxCostUSD: 1},
	}
	if err := env.store.Backlog.Put(ctx, item); err != nil {
		t.Fatalf("seed backlog: %v", err)
	}
	claim, err := env.store.ClaimPipelineStart(ctx, store.ClaimPipelineStartRequest{
		BacklogID: item.ID, ExpectedClaimVersion: item.ClaimVersion, ExpectedRevision: item.Revision,
		Template: "mills-default-pipeline", EstimateUSD: 1, Now: env.now,
	})
	if err != nil {
		t.Fatalf("claim pipeline: %v", err)
	}
	intent, err := env.store.ClaimPendingDispatch(
		ctx, claim.Dispatch.ID, env.now, store.DefaultDispatchLeaseDuration(),
	)
	if err != nil {
		t.Fatalf("claim dispatch: %v", err)
	}

	runMetric := PipelineRunsTotal.WithLabelValues(string(store.PipelineEscalated))
	reasonMetric := EscalationsTotal.WithLabelValues("dispatch_dead_letter")
	classMetric := EscalationClassTotal.WithLabelValues("config")
	beforeRun := testutil.ToFloat64(runMetric)
	beforeReason := testutil.ToFloat64(reasonMetric)
	beforeClass := testutil.ToFloat64(classMetric)

	env.rec.DispatchRetryPolicy = store.DispatchRetryPolicy{
		BaseDelay: time.Second, MaxDelay: time.Second, MaxAttempts: 1,
	}
	if err := env.rec.recordDispatchFailure(ctx, intent, "starter unavailable"); err != nil {
		t.Fatalf("record dispatch failure: %v", err)
	}
	if got := testutil.ToFloat64(runMetric) - beforeRun; got != 1 {
		t.Fatalf("escalated run metric delta=%v want 1", got)
	}
	if got := testutil.ToFloat64(reasonMetric) - beforeReason; got != 1 {
		t.Fatalf("dispatch dead-letter metric delta=%v want 1", got)
	}
	if got := testutil.ToFloat64(classMetric) - beforeClass; got != 1 {
		t.Fatalf("config escalation metric delta=%v want 1", got)
	}
}

func TestReconciler_ConcurrentOutboxConsumersStartIntentOnce(t *testing.T) {
	env := newRecEnv(t, nil)
	ctx := context.Background()
	item := &store.BacklogItem{
		ID: "MILLS-DISPATCH-RACE", Title: "dispatch race", State: store.BacklogQueued,
		Priority: store.P1, CreatedBy: "test", Budget: store.Budget{MaxCostUSD: 1},
	}
	if err := env.store.Backlog.Put(ctx, item); err != nil {
		t.Fatalf("seed backlog: %v", err)
	}
	if _, err := env.store.ClaimPipelineStart(ctx, store.ClaimPipelineStartRequest{
		BacklogID: item.ID, ExpectedClaimVersion: item.ClaimVersion, ExpectedRevision: item.Revision,
		Template: "mills-default-pipeline", EstimateUSD: 1, Now: env.now,
	}); err != nil {
		t.Fatalf("seed committed dispatch: %v", err)
	}

	starter := newBlockingStarter()
	first := NewReconciler(env.store, env.policy, NewBudget(env.policy, NewStoreBudgetReader(env.store)), starter)
	second := NewReconciler(env.store, env.policy, NewBudget(env.policy, NewStoreBudgetReader(env.store)), starter)
	first.Clock = env.rec.Clock
	second.Clock = env.rec.Clock
	done := make(chan struct{})
	go func() {
		defer close(done)
		first.pickupPendingDispatches(ctx, map[string]bool{})
	}()
	select {
	case <-starter.entered:
	case <-time.After(5 * time.Second):
		close(starter.release)
		t.Fatal("first consumer did not reach blocked starter")
	}
	started, errored := second.pickupPendingDispatches(ctx, map[string]bool{})
	if started != 0 || errored != 0 {
		t.Fatalf("competing consumer result started=%d errored=%d", started, errored)
	}
	close(starter.release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("first consumer did not finish after starter release")
	}
	if starter.callCount() != 1 {
		t.Fatalf("starter calls=%d want 1", starter.callCount())
	}
	if pending, err := env.store.CountPendingDispatches(ctx); err != nil || pending != 0 {
		t.Fatalf("pending dispatches=%d err=%v", pending, err)
	}
}

func TestReconciler_PolicyDisabledStillRecoversCommittedDispatch(t *testing.T) {
	env := newRecEnv(t, func(policy *Policy) {
		off := false
		policy.Enabled = &off
	})
	ctx := context.Background()
	item := &store.BacklogItem{
		ID: "MILLS-DISPATCH-DISABLED", Title: "committed before disable",
		State: store.BacklogQueued, Priority: store.P1, CreatedBy: "test",
		Budget: store.Budget{MaxCostUSD: 1},
	}
	if err := env.store.Backlog.Put(ctx, item); err != nil {
		t.Fatalf("seed backlog: %v", err)
	}
	if _, err := env.store.ClaimPipelineStart(ctx, store.ClaimPipelineStartRequest{
		BacklogID: item.ID, ExpectedClaimVersion: item.ClaimVersion, ExpectedRevision: item.Revision,
		Template: "mills-default-pipeline", EstimateUSD: 1, Now: env.now,
	}); err != nil {
		t.Fatalf("seed committed dispatch: %v", err)
	}
	result, err := env.rec.Tick(ctx)
	if err != nil {
		t.Fatalf("disabled recovery tick: %v", err)
	}
	if result.Started != 1 || result.Errored != 0 || result.SkipReason != "policy disabled" {
		t.Fatalf("disabled recovery result: %+v", result)
	}
	if env.starter.calls() != 1 {
		t.Fatalf("starter calls=%d want 1", env.starter.calls())
	}
	if pending, err := env.store.CountPendingDispatches(ctx); err != nil || pending != 0 {
		t.Fatalf("pending dispatches=%d err=%v", pending, err)
	}
}

func TestReconciler_AdvancedRunAcknowledgesPendingDispatchWithoutStarter(t *testing.T) {
	env := newRecEnv(t, nil)
	ctx := context.Background()
	item := &store.BacklogItem{
		ID: "MILLS-DISPATCH-ADOPT", Title: "adopt", State: store.BacklogQueued,
		Priority: store.P1, CreatedBy: "test", Budget: store.Budget{MaxCostUSD: 1},
	}
	if err := env.store.Backlog.Put(ctx, item); err != nil {
		t.Fatalf("seed backlog: %v", err)
	}
	claim, err := env.store.ClaimPipelineStart(ctx, store.ClaimPipelineStartRequest{
		BacklogID: item.ID, ExpectedClaimVersion: item.ClaimVersion, ExpectedRevision: item.Revision,
		Template: "mills-default-pipeline", EstimateUSD: 1, Now: env.now,
	})
	if err != nil {
		t.Fatalf("claim pipeline: %v", err)
	}
	claim.Run.State = store.PipelinePlanning
	claim.Run.CurrentStage = "plan_slice"
	if err := env.store.Pipeline.PutRun(ctx, claim.Run); err != nil {
		t.Fatalf("advance run before ack: %v", err)
	}
	result, err := env.rec.Tick(ctx)
	if err != nil {
		t.Fatalf("adoption tick: %v", err)
	}
	if result.Started != 1 || result.Errored != 0 || env.starter.calls() != 0 {
		t.Fatalf("adoption result=%+v starter_calls=%d", result, env.starter.calls())
	}
	if pending, err := env.store.CountPendingDispatches(ctx); err != nil || pending != 0 {
		t.Fatalf("pending dispatches=%d err=%v", pending, err)
	}
}

func TestReconciler_CrashAfterDispatchAckRecoversQueuedTopLevelRun(t *testing.T) {
	env := newRecEnv(t, nil)
	ctx := context.Background()
	item := &store.BacklogItem{
		ID: "MILLS-DISPATCH-ACK-CRASH", Title: "ack crash", State: store.BacklogQueued,
		Priority: store.P1, CreatedBy: "test", Budget: store.Budget{MaxCostUSD: 1},
	}
	if err := env.store.Backlog.Put(ctx, item); err != nil {
		t.Fatalf("seed backlog: %v", err)
	}
	claim, err := env.store.ClaimPipelineStart(ctx, store.ClaimPipelineStartRequest{
		BacklogID: item.ID, ExpectedClaimVersion: item.ClaimVersion, ExpectedRevision: item.Revision,
		Template: "mills-default-pipeline", EstimateUSD: 1, Now: env.now,
	})
	if err != nil {
		t.Fatalf("claim pipeline: %v", err)
	}
	intent, err := env.store.ClaimPendingDispatch(
		ctx, claim.Dispatch.ID, env.now, store.DefaultDispatchLeaseDuration(),
	)
	if err != nil {
		t.Fatalf("claim dispatch: %v", err)
	}
	if err := env.store.MarkDispatchDelivered(ctx, intent.ID, intent.LeaseToken, env.now); err != nil {
		t.Fatalf("ack dispatch: %v", err)
	}

	// Model a process crash before Runner persisted planning. The durable run is
	// still queued, but its delivered intent proves it must be recovered through
	// ListInFlight rather than ignored as an undelivered outbox row.
	recoveredStarter := &recordingStarter{}
	recovered := NewReconciler(
		env.store, env.policy, NewBudget(env.policy, NewStoreBudgetReader(env.store)), recoveredStarter,
	)
	recovered.Clock = env.rec.Clock
	result, err := recovered.Tick(ctx)
	if err != nil {
		t.Fatalf("recovery tick: %v", err)
	}
	if result.Started != 1 || result.Errored != 0 || recoveredStarter.calls() != 1 {
		t.Fatalf("recovery result=%+v starter_calls=%d", result, recoveredStarter.calls())
	}
	if recoveredStarter.runs[0].ID != claim.Run.ID {
		t.Fatalf("recovered run=%s want %s", recoveredStarter.runs[0].ID, claim.Run.ID)
	}
}

func TestReconciler_ObsoleteQueuedDispatchNeverReachesStarter(t *testing.T) {
	env := newRecEnv(t, nil)
	ctx := context.Background()
	item := &store.BacklogItem{
		ID: "MILLS-DISPATCH-OLD", Title: "old", State: store.BacklogQueued,
		Priority: store.P1, CreatedBy: "test", Budget: store.Budget{MaxCostUSD: 1},
	}
	if err := env.store.Backlog.Put(ctx, item); err != nil {
		t.Fatalf("seed backlog: %v", err)
	}
	first, err := env.store.ClaimPipelineStart(ctx, store.ClaimPipelineStartRequest{
		BacklogID: item.ID, ExpectedClaimVersion: item.ClaimVersion, ExpectedRevision: item.Revision,
		Template: "mills-default-pipeline", EstimateUSD: 1, Now: env.now,
	})
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	requeued, err := env.store.Backlog.Get(ctx, item.ID)
	if err != nil {
		t.Fatalf("load first aggregate: %v", err)
	}
	requeued.State = store.BacklogQueued
	if err := env.store.Backlog.Put(ctx, requeued); err != nil {
		t.Fatalf("requeue: %v", err)
	}
	second, err := env.store.ClaimPipelineStart(ctx, store.ClaimPipelineStartRequest{
		BacklogID: item.ID, ExpectedClaimVersion: requeued.ClaimVersion, ExpectedRevision: requeued.Revision,
		Template: "mills-default-pipeline", EstimateUSD: 1, Now: env.now.Add(time.Second),
	})
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	env.rec.Clock = func() time.Time { return env.now.Add(time.Second) }
	result, err := env.rec.Tick(ctx)
	if err != nil {
		t.Fatalf("recovery tick: %v", err)
	}
	if result.Started != 1 || result.Errored != 0 || env.starter.calls() != 1 {
		t.Fatalf("recovery result=%+v starter_calls=%d", result, env.starter.calls())
	}
	if env.starter.runs[0].ID != second.Run.ID {
		t.Fatalf("starter received obsolete run %s want current %s", env.starter.runs[0].ID, second.Run.ID)
	}
	oldRun, err := env.store.Pipeline.GetRun(ctx, first.Run.ID)
	if err != nil || oldRun.State != store.PipelineEscalated {
		t.Fatalf("obsolete run=%+v err=%v", oldRun, err)
	}
}

func TestReconciler_TickIsAuditable(t *testing.T) {
	env := newRecEnv(t, nil)
	ctx := context.Background()

	if _, err := env.rec.Tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}
	events, err := env.store.Events.ListSince(ctx, env.now.Add(-time.Hour), 100)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	var sawTick bool
	for _, e := range events {
		if e.Kind == "reconciler.tick" {
			sawTick = true
			break
		}
	}
	if !sawTick {
		t.Errorf("expected a reconciler.tick event in the audit log")
	}
}

// ----- Scheduler -----

func TestScheduler_RunStopsOnContextCancel(t *testing.T) {
	env := newRecEnv(t, nil)
	sch := NewScheduler(env.rec)
	sch.Interval = 50 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sch.Run(ctx) }()

	time.Sleep(120 * time.Millisecond) // let at least 2 ticks fire
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("scheduler returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("scheduler did not exit within 2s of cancel")
	}
}

func TestScheduler_StopMethodEndsRun(t *testing.T) {
	env := newRecEnv(t, nil)
	sch := NewScheduler(env.rec)
	sch.Interval = 50 * time.Millisecond

	done := make(chan error, 1)
	go func() { done <- sch.Run(context.Background()) }()
	time.Sleep(80 * time.Millisecond)
	sch.Stop()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("scheduler did not exit within 2s of Stop()")
	}
}

// TestReconciler_PicksUpQueuedSubrun pins the slice-6.2 dispatcher
// pickup contract: a pipeline_run row in state=queued with a non-null
// parent_run_id and attempts=0 (i.e. created by recursion.SubrunGuard
// but never started) is picked up by the next reconcile tick and
// handed to PipelineStarter — exactly the way a fresh-from-backlog
// run is. The starter sees the same Run + Item shape, so the runner
// downstream needs no recursion-specific branch.
func TestReconciler_PicksUpQueuedSubrun(t *testing.T) {
	env := newRecEnv(t, nil)
	ctx := context.Background()

	// Seed a parent backlog item + parent pipeline run.
	parentItem := &store.BacklogItem{
		ID: "BACK-PARENT", Title: "parent", State: store.BacklogRunning,
		Priority: store.P2, CreatedBy: "test",
	}
	if err := env.store.Backlog.Put(ctx, parentItem); err != nil {
		t.Fatalf("seed parent backlog: %v", err)
	}
	parentRun := &store.PipelineRun{
		ID: "PIPE-P", BacklogID: "BACK-PARENT", Template: "mills-default",
		State: store.PipelineImplementing, StartedAt: env.now, Depth: 0,
	}
	if err := env.store.Pipeline.PutRun(ctx, parentRun); err != nil {
		t.Fatalf("seed parent run: %v", err)
	}

	// Seed a child backlog + a queued subrun row pointing at the
	// parent. This is what recursion.SubrunGuard.SubrunCreate would
	// have produced just before this tick — backlog state Running
	// because SubrunGuard claims the item to prevent the main
	// reconciler loop from double-picking.
	childItem := &store.BacklogItem{
		ID: "BACK-CHILD", Title: "child slice", State: store.BacklogRunning,
		Priority: store.P2, CreatedBy: "test",
	}
	if err := env.store.Backlog.Put(ctx, childItem); err != nil {
		t.Fatalf("seed child backlog: %v", err)
	}
	parentID := "PIPE-P"
	subrun := &store.PipelineRun{
		ID: "PIPE-C", BacklogID: "BACK-CHILD", Template: "mills-default",
		State: store.PipelineQueued, StartedAt: env.now,
		Depth: 1, ParentRunID: &parentID,
	}
	if err := env.store.Pipeline.CreateSubrun(ctx, subrun); err != nil {
		t.Fatalf("seed subrun: %v", err)
	}

	res, err := env.rec.Tick(ctx)
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	// Tick invokes the starter twice now: once for the queued subrun
	// (PIPE-C), and once for the in-flight parent (PIPE-P, state=
	// implementing). The M7 in-flight re-driver re-spawns Drive for
	// any non-terminal run; idempotency is enforced downstream by
	// Runner.Start.
	if env.starter.calls() != 2 {
		t.Fatalf("starter calls: got %d want 2", env.starter.calls())
	}
	sawSubrun, sawInflight := false, false
	for i, r := range env.starter.runs {
		switch r.ID {
		case "PIPE-C":
			sawSubrun = true
			if r.Depth != 1 {
				t.Errorf("subrun depth: got %d want 1", r.Depth)
			}
			if env.starter.items[i].ID != "BACK-CHILD" {
				t.Errorf("subrun item id: got %q want BACK-CHILD", env.starter.items[i].ID)
			}
		case "PIPE-P":
			sawInflight = true
		default:
			t.Errorf("unexpected starter run id %q", r.ID)
		}
	}
	if !sawSubrun {
		t.Errorf("starter never saw the subrun PIPE-C")
	}
	if !sawInflight {
		t.Errorf("starter never saw the in-flight parent PIPE-P")
	}
	// res.Started should reflect both the subrun pickup and the in-flight re-drive.
	if res.Started != 2 {
		t.Errorf("res.Started: got %d want 2", res.Started)
	}
	if res.Errored != 0 {
		t.Errorf("res.Errored: got %d want 0", res.Errored)
	}
}

func TestReconciler_ResumeInFlightRunsRestartsStartedRun(t *testing.T) {
	env := newRecEnv(t, nil)
	ctx := context.Background()

	item := &store.BacklogItem{
		ID: "BACK-R", Title: "resume me", State: store.BacklogRunning,
		Priority: store.P2, CreatedBy: "test",
	}
	if err := env.store.Backlog.Put(ctx, item); err != nil {
		t.Fatalf("seed backlog: %v", err)
	}
	run := &store.PipelineRun{
		ID: "PIPE-R", BacklogID: item.ID, Template: "mills-default",
		State: store.PipelinePlanning, CurrentStage: "plan_slice",
		Attempts: 1, StartedAt: env.now,
	}
	if err := env.store.Pipeline.PutRun(ctx, run); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	if err := env.store.Pipeline.PutRun(ctx, &store.PipelineRun{
		ID: "PIPE-Q", BacklogID: item.ID, Template: "mills-default",
		State: store.PipelineQueued, Attempts: 2, StartedAt: env.now,
	}); err != nil {
		t.Fatalf("seed queued run: %v", err)
	}

	res, err := env.rec.ResumeInFlightRuns(ctx)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if res.Inspected != 1 || res.Resumed != 1 || res.Errored != 0 {
		t.Fatalf("resume result = %+v, want inspected=1 resumed=1 errored=0", res)
	}
	if env.starter.calls() != 1 {
		t.Fatalf("starter calls: got %d want 1", env.starter.calls())
	}
	if env.starter.runs[0].ID != "PIPE-R" {
		t.Fatalf("resumed run = %s, want PIPE-R", env.starter.runs[0].ID)
	}
}

// TestReconciler_DoesNotResumeOrReDriveTerminalRuns encodes the end goal of
// fix direction 2 (2026-07-01 "escalate isn't durable"): once a run's row is
// terminal (escalated here), neither the startup resume nor the per-tick
// in-flight re-driver may touch it again. ListInFlight is the single gate for
// both paths, so a terminal row must stay out of it — otherwise the operator
// resurrects a run the operator explicitly escalated on every restart.
func TestReconciler_DoesNotResumeOrReDriveTerminalRuns(t *testing.T) {
	env := newRecEnv(t, nil)
	ctx := context.Background()

	item := &store.BacklogItem{
		ID: "BACK-ESCALATED", Title: "escalated, leave me alone",
		State: store.BacklogEscalated, Priority: store.P2, CreatedBy: "test",
	}
	if err := env.store.Backlog.Put(ctx, item); err != nil {
		t.Fatalf("seed backlog: %v", err)
	}
	ended := env.now
	if err := env.store.Pipeline.PutRun(ctx, &store.PipelineRun{
		ID: "PIPE-ESCALATED", BacklogID: item.ID, Template: "mills-default",
		State: store.PipelineEscalated, CurrentStage: "implement",
		Attempts: 1, StartedAt: env.now, EndedAt: &ended,
	}); err != nil {
		t.Fatalf("seed escalated run: %v", err)
	}

	res, err := env.rec.ResumeInFlightRuns(ctx)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if res.Inspected != 0 || res.Resumed != 0 {
		t.Fatalf("resume inspected an escalated run: %+v", res)
	}

	// A full tick (which runs pickupInFlightRuns) must also leave it alone.
	if _, err := env.rec.Tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if n := env.starter.calls(); n != 0 {
		t.Fatalf("starter invoked %d times for terminal runs; want 0", n)
	}
}

func TestReconciler_SyncTerminalBacklogsClosesStaleRunningItems(t *testing.T) {
	env := newRecEnv(t, nil)
	ctx := context.Background()

	cases := []struct {
		id        string
		runState  store.PipelineState
		wantState store.BacklogState
	}{
		{id: "BACK-DONE", runState: store.PipelineDone, wantState: store.BacklogMerged},
		{id: "BACK-ESC", runState: store.PipelineEscalated, wantState: store.BacklogEscalated},
		{id: "BACK-PAUSE", runState: store.PipelinePaused, wantState: store.BacklogPaused},
	}
	for i, tc := range cases {
		item := &store.BacklogItem{
			ID: tc.id, Title: tc.id, State: store.BacklogRunning,
			Priority: store.P2, CreatedBy: "test",
		}
		if err := env.store.Backlog.Put(ctx, item); err != nil {
			t.Fatalf("seed backlog %s: %v", tc.id, err)
		}
		if err := env.store.Pipeline.PutRun(ctx, &store.PipelineRun{
			ID: "PIPE-" + tc.id, BacklogID: tc.id, Template: "mills-default",
			State: tc.runState, Attempts: i + 1, StartedAt: env.now.Add(time.Duration(i) * time.Minute),
		}); err != nil {
			t.Fatalf("seed run %s: %v", tc.id, err)
		}
	}

	res, err := env.rec.SyncTerminalBacklogs(ctx)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if res.Inspected != len(cases) || res.Updated != len(cases) || res.Skipped != 0 || res.Errored != 0 {
		t.Fatalf("sync result = %+v", res)
	}
	for _, tc := range cases {
		got, err := env.store.Backlog.Get(ctx, tc.id)
		if err != nil {
			t.Fatalf("read backlog %s: %v", tc.id, err)
		}
		if got.State != tc.wantState {
			t.Fatalf("%s state: got %q want %q", tc.id, got.State, tc.wantState)
		}
	}
}

func TestReconciler_TickRepairsTerminalBacklogWhilePolicyDisabled(t *testing.T) {
	env := newRecEnv(t, func(policy *Policy) {
		off := false
		policy.Enabled = &off
	})
	ctx := context.Background()
	item := &store.BacklogItem{
		ID: "BACK-DISABLED-TERMINAL", Title: "terminal repair",
		State: store.BacklogRunning, Priority: store.P1, CreatedBy: "test",
	}
	if err := env.store.Backlog.Put(ctx, item); err != nil {
		t.Fatalf("seed backlog: %v", err)
	}
	if err := env.store.Pipeline.PutRun(ctx, &store.PipelineRun{
		ID: "PIPE-DISABLED-TERMINAL", BacklogID: item.ID, Template: "mills-default",
		State: store.PipelineDone, Attempts: 1, StartedAt: env.now,
	}); err != nil {
		t.Fatalf("seed terminal run: %v", err)
	}
	result, err := env.rec.Tick(ctx)
	if err != nil {
		t.Fatalf("disabled tick: %v", err)
	}
	if result.SkipReason != "policy disabled" || result.Errored != 0 || result.Inspected != 1 {
		t.Fatalf("disabled terminal repair result: %+v", result)
	}
	got, err := env.store.Backlog.Get(ctx, item.ID)
	if err != nil {
		t.Fatalf("load repaired backlog: %v", err)
	}
	if got.State != store.BacklogMerged {
		t.Fatalf("repaired backlog state=%s want merged", got.State)
	}
	if env.starter.calls() != 0 {
		t.Fatalf("terminal repair invoked starter %d times", env.starter.calls())
	}
}

func TestReconciler_SyncTerminalBacklogsSkipsActiveRuns(t *testing.T) {
	env := newRecEnv(t, nil)
	ctx := context.Background()

	item := &store.BacklogItem{
		ID: "BACK-ACTIVE", Title: "active", State: store.BacklogRunning,
		Priority: store.P2, CreatedBy: "test",
	}
	if err := env.store.Backlog.Put(ctx, item); err != nil {
		t.Fatalf("seed backlog: %v", err)
	}
	if err := env.store.Pipeline.PutRun(ctx, &store.PipelineRun{
		ID: "PIPE-DONE", BacklogID: item.ID, Template: "mills-default",
		State: store.PipelineDone, Attempts: 1, StartedAt: env.now.Add(-time.Minute),
	}); err != nil {
		t.Fatalf("seed done run: %v", err)
	}
	if err := env.store.Pipeline.PutRun(ctx, &store.PipelineRun{
		ID: "PIPE-ACTIVE", BacklogID: item.ID, Template: "mills-default",
		State: store.PipelinePlanning, Attempts: 2, StartedAt: env.now,
	}); err != nil {
		t.Fatalf("seed active run: %v", err)
	}

	res, err := env.rec.SyncTerminalBacklogs(ctx)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if res.Inspected != 0 || res.Updated != 0 || res.Skipped != 0 || res.Errored != 0 {
		t.Fatalf("sync result = %+v", res)
	}
	got, err := env.store.Backlog.Get(ctx, item.ID)
	if err != nil {
		t.Fatalf("read backlog: %v", err)
	}
	if got.State != store.BacklogRunning {
		t.Fatalf("backlog state: got %q want %q", got.State, store.BacklogRunning)
	}
}

func TestReconciler_TerminalRepairBypassesMoreThanBatchOfActiveHeads(t *testing.T) {
	env := newRecEnv(t, nil)
	ctx := context.Background()
	for i := 0; i <= terminalBacklogSyncBatchSize; i++ {
		id := fmt.Sprintf("BACK-ACTIVE-HEAD-%03d", i)
		item := &store.BacklogItem{
			ID: id, Title: id, State: store.BacklogRunning, Priority: store.P1,
			CreatedBy: "test", CreatedAt: env.now.Add(time.Duration(i) * time.Millisecond),
		}
		if err := env.store.Backlog.Put(ctx, item); err != nil {
			t.Fatalf("seed active backlog %d: %v", i, err)
		}
		if err := env.store.Pipeline.PutRun(ctx, &store.PipelineRun{
			ID: "PIPE-" + id, BacklogID: id, Template: "mills-default",
			State: store.PipelinePlanning, Attempts: 1, StartedAt: item.CreatedAt,
		}); err != nil {
			t.Fatalf("seed active run %d: %v", i, err)
		}
	}
	tail := &store.BacklogItem{
		ID: "BACK-TERMINAL-TAIL", Title: "terminal tail", State: store.BacklogRunning,
		Priority: store.P3, CreatedBy: "test", CreatedAt: env.now.Add(time.Hour),
	}
	if err := env.store.Backlog.Put(ctx, tail); err != nil {
		t.Fatalf("seed terminal backlog: %v", err)
	}
	if err := env.store.Pipeline.PutRun(ctx, &store.PipelineRun{
		ID: "PIPE-TERMINAL-TAIL", BacklogID: tail.ID, Template: "mills-default",
		State: store.PipelineDone, Attempts: 1, StartedAt: tail.CreatedAt,
	}); err != nil {
		t.Fatalf("seed terminal run: %v", err)
	}

	result, err := env.rec.SyncTerminalBacklogs(ctx)
	if err != nil {
		t.Fatalf("terminal repair: %v", err)
	}
	if result.Inspected != 1 || result.Updated != 1 || result.Skipped != 0 || result.Errored != 0 {
		t.Fatalf("terminal repair result: %+v", result)
	}
	repaired, err := env.store.Backlog.Get(ctx, tail.ID)
	if err != nil || repaired.State != store.BacklogMerged {
		t.Fatalf("terminal tail=%+v err=%v", repaired, err)
	}
	active, err := env.store.Backlog.Get(ctx, "BACK-ACTIVE-HEAD-000")
	if err != nil || active.State != store.BacklogRunning {
		t.Fatalf("active head=%+v err=%v", active, err)
	}
}

// TestReconciler_SubrunPickupSurvivesStarterError pins that a single
// failing subrun start doesn't block the rest of the tick.
func TestReconciler_SubrunPickupSurvivesStarterError(t *testing.T) {
	env := newRecEnv(t, nil)
	ctx := context.Background()
	env.starter.fail = errors.New("simulated starter failure")

	if err := env.store.Backlog.Put(ctx, &store.BacklogItem{
		ID: "BACK-P", Title: "p", State: store.BacklogRunning,
		Priority: store.P2, CreatedBy: "test",
	}); err != nil {
		t.Fatal(err)
	}
	if err := env.store.Pipeline.PutRun(ctx, &store.PipelineRun{
		ID: "PIPE-P", BacklogID: "BACK-P", Template: "mills-default",
		State: store.PipelineImplementing, StartedAt: env.now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := env.store.Backlog.Put(ctx, &store.BacklogItem{
		ID: "BACK-C", Title: "c", State: store.BacklogRunning,
		Priority: store.P2, CreatedBy: "test",
	}); err != nil {
		t.Fatal(err)
	}
	parentID := "PIPE-P"
	if err := env.store.Pipeline.CreateSubrun(ctx, &store.PipelineRun{
		ID: "PIPE-C", BacklogID: "BACK-C", Template: "mills-default",
		State: store.PipelineQueued, StartedAt: env.now,
		Depth: 1, ParentRunID: &parentID,
	}); err != nil {
		t.Fatal(err)
	}

	res, err := env.rec.Tick(ctx)
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	// Both the queued subrun and the in-flight parent (PIPE-P) are
	// handed to the (failing) starter; each surfaces as one Errored.
	// What this test pins is that neither failure blocks the other —
	// the tick completes and the per-row error counts roll up.
	if res.Errored != 2 {
		t.Errorf("expected two errored starts (subrun + in-flight parent), got Errored=%d", res.Errored)
	}
}

// TestReconciler_TickReDrivesInFlightRuns pins the M7 contract: a
// non-terminal pipeline_runs row whose runner goroutine exited (e.g.,
// after errStagePending) is re-driven by every subsequent Tick. The
// smoking-gun scenario from the 2026-05-17 wedge: state=planning,
// current_stage=plan_slice, ended_at=NULL, with a pending stage_results
// row carrying spawn_id but outcome=NULL. The wedge held for 9 hours
// because nothing scanned non-terminal runs after startup; this test
// proves the new Tick path would have unstuck it.
func TestReconciler_TickReDrivesInFlightRuns(t *testing.T) {
	env := newRecEnv(t, nil)
	ctx := context.Background()

	item := &store.BacklogItem{
		ID: "BACK-WEDGED", Title: "wedged run", State: store.BacklogRunning,
		Priority: store.P2, CreatedBy: "test",
	}
	if err := env.store.Backlog.Put(ctx, item); err != nil {
		t.Fatalf("seed backlog: %v", err)
	}
	run := &store.PipelineRun{
		ID: "PIPE-WEDGED", BacklogID: item.ID, Template: "mills-default",
		State: store.PipelinePlanning, CurrentStage: "plan_slice",
		Attempts: 1, StartedAt: env.now,
	}
	if err := env.store.Pipeline.PutRun(ctx, run); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	// Seed the pending stage_results row that mirrors the live wedge:
	// outcome=NULL, spawn_id set. The reconciler does not read this row,
	// but Runner.Drive will use it (via pendingStage) when re-spawned.
	if err := env.store.Pipeline.PutStage(ctx, &store.StageResult{
		PipelineRunID: run.ID, Stage: "plan_slice", Attempt: 1,
		StartedAt: env.now, SpawnID: "spawn-b7bc071ff949",
	}); err != nil {
		t.Fatalf("seed pending stage: %v", err)
	}

	res, err := env.rec.Tick(ctx)
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if env.starter.calls() != 1 {
		t.Fatalf("after tick 1 starter calls: got %d want 1", env.starter.calls())
	}
	if env.starter.runs[0].ID != run.ID {
		t.Fatalf("tick 1 redrove %q want %q", env.starter.runs[0].ID, run.ID)
	}
	if res.Started != 1 {
		t.Errorf("tick 1 res.Started: got %d want 1", res.Started)
	}

	// Second tick: re-drive again. Production idempotency lives in
	// Runner.Start's r.active guard; the recording starter is dumb and
	// records every call, which is the right shape for this assertion —
	// we want to know the Reconciler does its part on every tick.
	if _, err := env.rec.Tick(ctx); err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	if env.starter.calls() != 2 {
		t.Fatalf("after tick 2 starter calls: got %d want 2", env.starter.calls())
	}
}

// idempotentStarter simulates Runner.Start's r.active.LoadOrStore guard:
// the first call for a given run id records the call AND marks the run
// "active"; subsequent calls return nil without re-recording, mimicking
// the production no-op-on-active path. This lets us verify that even
// when a real Runner is still driving a previously-dispatched run, the
// Reconciler's repeated Tick traffic is harmless.
type idempotentStarter struct {
	mu     sync.Mutex
	active map[string]struct{}
	runs   []*store.PipelineRun
}

func newIdempotentStarter() *idempotentStarter {
	return &idempotentStarter{active: map[string]struct{}{}}
}

func (s *idempotentStarter) Start(_ context.Context, run *store.PipelineRun, _ *store.BacklogItem) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, loaded := s.active[run.ID]; loaded {
		// Mirror Runner.Start contract: nil + (logged) warn, no
		// new dispatch.
		return nil
	}
	s.active[run.ID] = struct{}{}
	s.runs = append(s.runs, run)
	return nil
}

func (s *idempotentStarter) dispatches() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.runs)
}

// TestReconciler_TickReDriveIsIdempotent pins that repeated Ticks against
// the same in-flight run do not produce duplicate dispatches when the
// downstream Starter is idempotent — the production wiring path
// (Runner.Start uses r.active.LoadOrStore).
func TestReconciler_TickReDriveIsIdempotent(t *testing.T) {
	env := newRecEnv(t, nil)
	ctx := context.Background()

	starter := newIdempotentStarter()
	env.rec.Starter = starter

	item := &store.BacklogItem{
		ID: "BACK-IDEM", Title: "idem", State: store.BacklogRunning,
		Priority: store.P2, CreatedBy: "test",
	}
	if err := env.store.Backlog.Put(ctx, item); err != nil {
		t.Fatalf("seed backlog: %v", err)
	}
	run := &store.PipelineRun{
		ID: "PIPE-IDEM", BacklogID: item.ID, Template: "mills-default",
		State: store.PipelineImplementing, CurrentStage: "implement",
		Attempts: 1, StartedAt: env.now,
	}
	if err := env.store.Pipeline.PutRun(ctx, run); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	for i := 0; i < 3; i++ {
		if _, err := env.rec.Tick(ctx); err != nil {
			t.Fatalf("tick %d: %v", i, err)
		}
	}
	if got := starter.dispatches(); got != 1 {
		t.Fatalf("idempotent starter dispatches: got %d want 1 (re-drive must not duplicate live goroutines)", got)
	}
}

// TestReconciler_TickDoesNotDoubleStartFreshlyQueuedItem pins DEBT-079's
// root cause: a queued item started by the queued-loop in a tick is
// non-terminal, so the same tick's pickupInFlightRuns used to re-invoke
// Start on it and re-count it (res.Started==2). The recordingStarter does
// not drive the run forward, so the run stays non-terminal after Start —
// exactly the window that exposed the double-count. With the same-tick skip
// set, the item is started (and counted) exactly once.
func TestReconciler_TickDoesNotDoubleStartFreshlyQueuedItem(t *testing.T) {
	env := newRecEnv(t, nil)
	ctx := context.Background()

	item := &store.BacklogItem{
		ID: "BACK-FRESH", Title: "fresh queued", State: store.BacklogQueued,
		Priority: store.P2, CreatedBy: "test",
		Budget: store.Budget{MaxCostUSD: 1.0, MaxPipelineMinutes: 60},
	}
	if err := env.store.Backlog.Put(ctx, item); err != nil {
		t.Fatalf("seed backlog: %v", err)
	}

	res, err := env.rec.Tick(ctx)
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if res.Started != 1 {
		t.Fatalf("res.Started = %d, want 1 (same-tick pickup must not re-count the just-started run)", res.Started)
	}
	if got := env.starter.calls(); got != 1 {
		t.Fatalf("starter calls = %d, want 1 (no same-tick re-dispatch)", got)
	}
}

// TestReconciler_TickReDriveSkipsWhenAutonomyBlocked pins that the new
// in-flight pickup honors the same autonomy gate the backlog pickup
// already does. A paused operator must stay paused — no re-drive bursts.
func TestReconciler_TickReDriveSkipsWhenAutonomyBlocked(t *testing.T) {
	env := newRecEnv(t, nil)
	env.rec.AutonomyGate = func(context.Context) (bool, []string) {
		return false, []string{"paused"}
	}
	ctx := context.Background()

	item := &store.BacklogItem{
		ID: "BACK-PAUSED", Title: "paused", State: store.BacklogRunning,
		Priority: store.P2, CreatedBy: "test",
	}
	if err := env.store.Backlog.Put(ctx, item); err != nil {
		t.Fatalf("seed backlog: %v", err)
	}
	if err := env.store.Pipeline.PutRun(ctx, &store.PipelineRun{
		ID: "PIPE-PAUSED", BacklogID: item.ID, Template: "mills-default",
		State: store.PipelinePlanning, CurrentStage: "plan_slice",
		Attempts: 1, StartedAt: env.now,
	}); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	res, err := env.rec.Tick(ctx)
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if res.SkipReason != "autonomy blocked" {
		t.Fatalf("skip reason: got %q want autonomy blocked", res.SkipReason)
	}
	if env.starter.calls() != 0 {
		t.Fatalf("starter calls: got %d want 0 (paused operator must not re-drive)", env.starter.calls())
	}
}

// TestReconciler_TickReDriveSkipsTerminalRuns pins the contract boundary:
// done/escalated/paused runs are excluded by ListInFlight, so the new
// re-driver never touches them. This guards against a regression where
// a future ListInFlight refactor broadens the filter.
func TestReconciler_TickReDriveSkipsTerminalRuns(t *testing.T) {
	env := newRecEnv(t, nil)
	ctx := context.Background()

	cases := []struct {
		id        string
		state     store.PipelineState
		backlog   store.BacklogState
		backlogID string
	}{
		{id: "PIPE-DONE", state: store.PipelineDone, backlog: store.BacklogMerged, backlogID: "BACK-DONE"},
		{id: "PIPE-ESC", state: store.PipelineEscalated, backlog: store.BacklogEscalated, backlogID: "BACK-ESC"},
		{id: "PIPE-PSE", state: store.PipelinePaused, backlog: store.BacklogPaused, backlogID: "BACK-PSE"},
	}
	for _, tc := range cases {
		if err := env.store.Backlog.Put(ctx, &store.BacklogItem{
			ID: tc.backlogID, Title: tc.backlogID, State: tc.backlog,
			Priority: store.P2, CreatedBy: "test",
		}); err != nil {
			t.Fatalf("seed backlog %s: %v", tc.backlogID, err)
		}
		if err := env.store.Pipeline.PutRun(ctx, &store.PipelineRun{
			ID: tc.id, BacklogID: tc.backlogID, Template: "mills-default",
			State: tc.state, Attempts: 1, StartedAt: env.now,
		}); err != nil {
			t.Fatalf("seed run %s: %v", tc.id, err)
		}
	}

	if _, err := env.rec.Tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if env.starter.calls() != 0 {
		t.Fatalf("starter calls: got %d want 0 (terminal runs must not re-drive); runs=%v", env.starter.calls(), env.starter.runs)
	}
}

func TestScheduler_DoubleRunErrors(t *testing.T) {
	env := newRecEnv(t, nil)
	sch := NewScheduler(env.rec)
	sch.Interval = time.Hour

	go func() { _ = sch.Run(context.Background()) }()
	time.Sleep(20 * time.Millisecond)

	if err := sch.Run(context.Background()); err == nil {
		t.Errorf("expected error on second Run()")
	}
	sch.Stop()
}
