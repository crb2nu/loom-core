package store

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var claimTestNow = time.Date(2026, 7, 12, 14, 0, 0, 0, time.UTC)

func seedClaimBacklog(t *testing.T, st *Store, id string) *BacklogItem {
	t.Helper()
	item := &BacklogItem{
		ID:        id,
		Title:     "transactional claim " + id,
		State:     BacklogQueued,
		Priority:  P1,
		CreatedBy: "test",
		Budget:    Budget{MaxCostUSD: 1},
	}
	if err := st.Backlog.Put(context.Background(), item); err != nil {
		t.Fatalf("seed backlog %s: %v", id, err)
	}
	return item
}

func claimTestRequest(id string) ClaimPipelineStartRequest {
	return ClaimPipelineStartRequest{
		BacklogID:                  id,
		ExpectedClaimVersion:       0,
		ExpectedRevision:           1,
		SerializeOverlappingScopes: true,
		HomeProject:                "services/loom-core",
		Template:                   "mills-default-pipeline",
		EstimateUSD:                1,
		Limits: PipelineStartLimits{
			MaxUSDPerRun:      10,
			MaxUSDPerDay:      1000,
			MaxRunsPerDay:     1000,
			MaxConcurrentRuns: 1000,
		},
		Now: claimTestNow,
	}
}

func TestClaimPipelineStart_ConcurrentExactlyOne(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	item := seedClaimBacklog(t, st, "MILLS-CLAIM-RACE")

	const racers = 100
	start := make(chan struct{})
	type outcome struct {
		result *ClaimPipelineStartResult
		err    error
	}
	outcomes := make(chan outcome, racers)
	var wg sync.WaitGroup
	for range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			result, err := st.ClaimPipelineStart(ctx, claimTestRequest(item.ID))
			outcomes <- outcome{result: result, err: err}
		}()
	}
	close(start)
	wg.Wait()
	close(outcomes)

	var winner *ClaimPipelineStartResult
	successes, conflicts := 0, 0
	for got := range outcomes {
		switch {
		case got.err == nil:
			successes++
			winner = got.result
		case errors.Is(got.err, ErrClaimConflict):
			conflicts++
		default:
			t.Fatalf("unexpected racer error: %v", got.err)
		}
	}
	if successes != 1 || conflicts != racers-1 {
		t.Fatalf("race outcomes: successes=%d conflicts=%d want 1/%d", successes, conflicts, racers-1)
	}
	if winner == nil || winner.Run == nil || winner.Dispatch == nil || winner.Reservation == nil || winner.WorkflowRun == nil {
		t.Fatalf("incomplete winning result: %+v", winner)
	}
	if !strings.HasPrefix(winner.Run.ID, "PIPE-"+item.ID+"-") {
		t.Fatalf("run id %q does not preserve backlog prefix", winner.Run.ID)
	}
	if winner.Run.AggregateVersion != 1 || winner.Run.Attempts != 1 {
		t.Fatalf("run aggregate/attempt: %+v", winner.Run)
	}

	claimed, err := st.Backlog.Get(ctx, item.ID)
	if err != nil {
		t.Fatalf("load claimed backlog: %v", err)
	}
	if claimed.State != BacklogRunning || claimed.ClaimVersion != 1 || claimed.Revision != 2 {
		t.Fatalf("claimed backlog: state=%s claim_version=%d revision=%d",
			claimed.State, claimed.ClaimVersion, claimed.Revision)
	}
	assertTableCount(t, st, "pipeline_runs", 1)
	assertTableCount(t, st, "pipeline_budget_reservations", 1)
	assertTableCount(t, st, "workflow_runs", 1)
	assertTableCount(t, st, "pipeline_transitions", 1)
	assertTableCount(t, st, "pending_dispatches", 1)

	pending, err := st.ListPendingDispatches(ctx, 10)
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}
	if len(pending) != 1 || pending[0].RunID != winner.Run.ID || pending[0].AggregateVersion != 1 {
		t.Fatalf("pending dispatch: %+v", pending)
	}
	loadedWorkflow, err := st.Workflow.GetWorkflowRun(ctx, winner.Run.ID)
	if err != nil {
		t.Fatalf("load DAG workflow: %v", err)
	}
	if loadedWorkflow.Engine != WorkflowEngineDAG || loadedWorkflow.Template != winner.Run.Template || loadedWorkflow.BacklogID != item.ID {
		t.Fatalf("DAG workflow metadata: %+v", loadedWorkflow)
	}
}

func TestClaimPipelineStart_FaultMatrixAtomicity(t *testing.T) {
	injected := errors.New("injected claim fault")
	for _, point := range claimPipelineStartFaultPoints {
		point := point
		t.Run(string(point), func(t *testing.T) {
			st := newTestStore(t)
			ctx := context.Background()
			item := seedClaimBacklog(t, st, "MILLS-FAULT-"+strings.ToUpper(string(point)))
			req := claimTestRequest(item.ID)
			req.FaultHook = func(got ClaimPipelineStartFaultPoint) error {
				if got == point {
					return injected
				}
				return nil
			}

			if _, err := st.ClaimPipelineStart(ctx, req); !errors.Is(err, injected) {
				t.Fatalf("claim error=%v want injected fault", err)
			}
			rolledBack, err := st.Backlog.Get(ctx, item.ID)
			if err != nil {
				t.Fatalf("load backlog after fault: %v", err)
			}
			if rolledBack.State != BacklogQueued || rolledBack.ClaimVersion != 0 || rolledBack.Revision != 1 {
				t.Fatalf("backlog did not roll back: state=%s claim_version=%d revision=%d",
					rolledBack.State, rolledBack.ClaimVersion, rolledBack.Revision)
			}
			for _, table := range []string{
				"pipeline_runs", "pipeline_budget_reservations", "workflow_runs",
				"pipeline_transitions", "pending_dispatches",
			} {
				assertTableCount(t, st, table, 0)
			}

			// Prove the failed transaction released its lock and left a claimable
			// aggregate, not merely a superficially empty set of dependent rows.
			req.FaultHook = nil
			if _, err := st.ClaimPipelineStart(ctx, req); err != nil {
				t.Fatalf("claim after rollback: %v", err)
			}
		})
	}
}

func TestBacklogPut_RevisionCASAgainstPipelineClaim(t *testing.T) {
	t.Run("metadata write wins", func(t *testing.T) {
		st := newTestStore(t)
		ctx := context.Background()
		item := seedClaimBacklog(t, st, "MILLS-ROW-CAS-METADATA")
		staleAdmission := claimTestRequest(item.ID)

		item.Title = "fresh metadata"
		item.Policy.AutoMerge = true
		if err := st.Backlog.Put(ctx, item); err != nil {
			t.Fatalf("fresh metadata put: %v", err)
		}
		if item.Revision != 2 {
			t.Fatalf("metadata revision=%d want 2", item.Revision)
		}
		if _, err := st.ClaimPipelineStart(ctx, staleAdmission); !errors.Is(err, ErrClaimConflict) {
			t.Fatalf("stale admission error=%v want ErrClaimConflict", err)
		}
		got, err := st.Backlog.Get(ctx, item.ID)
		if err != nil {
			t.Fatalf("load metadata winner: %v", err)
		}
		if got.State != BacklogQueued || got.Title != "fresh metadata" || !got.Policy.AutoMerge ||
			got.ClaimVersion != 0 || got.Revision != 2 {
			t.Fatalf("metadata winner corrupted: %+v", got)
		}
	})

	t.Run("pipeline claim wins", func(t *testing.T) {
		st := newTestStore(t)
		ctx := context.Background()
		item := seedClaimBacklog(t, st, "MILLS-ROW-CAS-CLAIM")
		staleMetadata := *item
		if _, err := st.ClaimPipelineStart(ctx, claimTestRequest(item.ID)); err != nil {
			t.Fatalf("claim: %v", err)
		}
		staleMetadata.Title = "stale metadata"
		staleMetadata.State = BacklogQueued
		err := st.Backlog.Put(ctx, &staleMetadata)
		if !errors.Is(err, ErrStaleWrite) {
			t.Fatalf("stale metadata error=%v want ErrStaleWrite", err)
		}
		var stale *StaleWriteError
		if !errors.As(err, &stale) || stale.ExpectedRevision != 1 {
			t.Fatalf("typed stale error=%#v", err)
		}
		got, err := st.Backlog.Get(ctx, item.ID)
		if err != nil {
			t.Fatalf("load claim winner: %v", err)
		}
		if got.State != BacklogRunning || got.Title == "stale metadata" ||
			got.ClaimVersion != 1 || got.Revision != 2 {
			t.Fatalf("claim winner corrupted: %+v", got)
		}
	})
}

func TestClaimPipelineStart_RejectsNonfiniteOrNegativeAdmission(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*ClaimPipelineStartRequest)
	}{
		{name: "estimate_nan", mutate: func(r *ClaimPipelineStartRequest) { r.EstimateUSD = math.NaN() }},
		{name: "estimate_pos_inf", mutate: func(r *ClaimPipelineStartRequest) { r.EstimateUSD = math.Inf(1) }},
		{name: "estimate_neg_inf", mutate: func(r *ClaimPipelineStartRequest) { r.EstimateUSD = math.Inf(-1) }},
		{name: "estimate_negative", mutate: func(r *ClaimPipelineStartRequest) { r.EstimateUSD = -0.01 }},
		{name: "per_run_nan", mutate: func(r *ClaimPipelineStartRequest) { r.Limits.MaxUSDPerRun = math.NaN() }},
		{name: "per_run_inf", mutate: func(r *ClaimPipelineStartRequest) { r.Limits.MaxUSDPerRun = math.Inf(1) }},
		{name: "per_run_negative", mutate: func(r *ClaimPipelineStartRequest) { r.Limits.MaxUSDPerRun = -1 }},
		{name: "per_day_nan", mutate: func(r *ClaimPipelineStartRequest) { r.Limits.MaxUSDPerDay = math.NaN() }},
		{name: "per_day_inf", mutate: func(r *ClaimPipelineStartRequest) { r.Limits.MaxUSDPerDay = math.Inf(1) }},
		{name: "per_day_negative", mutate: func(r *ClaimPipelineStartRequest) { r.Limits.MaxUSDPerDay = -1 }},
		{name: "runs_negative", mutate: func(r *ClaimPipelineStartRequest) { r.Limits.MaxRunsPerDay = -1 }},
		{name: "concurrency_negative", mutate: func(r *ClaimPipelineStartRequest) { r.Limits.MaxConcurrentRuns = -1 }},
		{name: "claim_version_overflow", mutate: func(r *ClaimPipelineStartRequest) { r.ExpectedClaimVersion = math.MaxInt64 }},
		{name: "revision_overflow", mutate: func(r *ClaimPipelineStartRequest) { r.ExpectedRevision = math.MaxInt64 }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st := newTestStore(t)
			ctx := context.Background()
			item := seedClaimBacklog(t, st, "MILLS-INVALID-"+strings.ToUpper(tc.name))
			req := claimTestRequest(item.ID)
			tc.mutate(&req)
			if _, err := st.ClaimPipelineStart(ctx, req); !errors.Is(err, ErrInvalidClaim) {
				t.Fatalf("invalid admission error=%v want ErrInvalidClaim", err)
			}
			got, err := st.Backlog.Get(ctx, item.ID)
			if err != nil {
				t.Fatalf("load unchanged backlog: %v", err)
			}
			if got.State != BacklogQueued || got.ClaimVersion != 0 || got.Revision != 1 {
				t.Fatalf("invalid admission mutated backlog: %+v", got)
			}
		})
	}
}

func TestClaimPipelineStart_BudgetRace(t *testing.T) {
	tests := []struct {
		name   string
		limits PipelineStartLimits
		expect int
	}{
		{name: "daily_usd", limits: PipelineStartLimits{MaxUSDPerDay: 5}, expect: 5},
		{name: "daily_runs", limits: PipelineStartLimits{MaxRunsPerDay: 6}, expect: 6},
		{name: "active_slots", limits: PipelineStartLimits{MaxConcurrentRuns: 4}, expect: 4},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st := newTestStore(t)
			ctx := context.Background()
			const contenders = 24
			start := make(chan struct{})
			errs := make(chan error, contenders)
			var wg sync.WaitGroup
			for i := range contenders {
				id := fmt.Sprintf("MILLS-BUDGET-%s-%02d", tc.name, i)
				seedClaimBacklog(t, st, id)
				req := claimTestRequest(id)
				req.Limits = tc.limits
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					_, err := st.ClaimPipelineStart(ctx, req)
					errs <- err
				}()
			}
			close(start)
			wg.Wait()
			close(errs)

			successes, rejected := 0, 0
			for err := range errs {
				if err == nil {
					successes++
					continue
				}
				var exceeded *BudgetExceededError
				if errors.As(err, &exceeded) {
					rejected++
					continue
				}
				t.Fatalf("unexpected admission error: %v", err)
			}
			if successes != tc.expect || rejected != contenders-tc.expect {
				t.Fatalf("budget race: successes=%d rejected=%d want %d/%d",
					successes, rejected, tc.expect, contenders-tc.expect)
			}
			assertTableCount(t, st, "pipeline_runs", tc.expect)
			assertTableCount(t, st, "pipeline_budget_reservations", tc.expect)
			assertTableCount(t, st, "pending_dispatches", tc.expect)
		})
	}
}

func TestClaimPipelineStart_BudgetCountsActualAboveReservation(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	first := seedClaimBacklog(t, st, "MILLS-BUDGET-HEADROOM-A")
	req := claimTestRequest(first.ID)
	req.Limits = PipelineStartLimits{MaxUSDPerDay: 3}
	claim, err := st.ClaimPipelineStart(ctx, req)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	claim.Run.State = PipelineImplementing
	claim.Run.CostUSD = 2
	if err := st.Pipeline.PutRun(ctx, claim.Run); err != nil {
		t.Fatalf("record actual cost: %v", err)
	}

	second := seedClaimBacklog(t, st, "MILLS-BUDGET-HEADROOM-B")
	req = claimTestRequest(second.ID)
	req.EstimateUSD = 1.01
	req.Limits = PipelineStartLimits{MaxUSDPerDay: 3}
	_, err = st.ClaimPipelineStart(ctx, req)
	var exceeded *BudgetExceededError
	if !errors.As(err, &exceeded) {
		t.Fatalf("second claim error=%v want BudgetExceededError", err)
	}
	if exceeded.SpentUSD != 2 || exceeded.ReservedUSD != 0 {
		t.Fatalf("budget snapshot spent=%v reserved=%v want 2/0", exceeded.SpentUSD, exceeded.ReservedUSD)
	}
	got, err := st.Backlog.Get(ctx, second.ID)
	if err != nil {
		t.Fatalf("load rejected backlog: %v", err)
	}
	if got.State != BacklogQueued || got.ClaimVersion != 0 || got.Revision != 1 {
		t.Fatalf("rejected backlog did not roll back: %+v", got)
	}
}

func TestClaimPipelineStart_TenThousandRunBudgetWindow(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	history := seedClaimBacklog(t, st, "MILLS-BUDGET-HISTORY")
	history.State = BacklogMerged
	if err := st.Backlog.Put(ctx, history); err != nil {
		t.Fatalf("mark history backlog merged: %v", err)
	}

	tx, err := st.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin history seed: %v", err)
	}
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO pipeline_runs (
			id, backlog_id, aggregate_version, template, state, attempts,
			started_at, ended_at, cost_usd, depth
		) VALUES (?, ?, 0, 'history', 'done', ?, ?, ?, 0.01, 0)
	`)
	if err != nil {
		t.Fatalf("prepare history seed: %v", err)
	}
	for i := range 10_000 {
		started := claimTestNow.Add(-48 * time.Hour)
		if i >= 5_000 {
			started = claimTestNow.Add(-12*time.Hour + time.Duration(i-5_000)*time.Millisecond)
		}
		ended := started.Add(time.Minute)
		if _, err := stmt.ExecContext(ctx, fmt.Sprintf("PIPE-HISTORY-%05d", i), history.ID,
			i+1, timeRFC3339(started), timeRFC3339(ended)); err != nil {
			t.Fatalf("seed history row %d: %v", i, err)
		}
	}
	if err := stmt.Close(); err != nil {
		t.Fatalf("close history statement: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit history seed: %v", err)
	}

	planRows, err := st.DB().QueryContext(ctx, `
		EXPLAIN QUERY PLAN
		SELECT COALESCE(SUM(cost_usd), 0)
		FROM pipeline_runs
		WHERE started_at >= ?
	`, timeRFC3339(claimTestNow.Add(-24*time.Hour)))
	if err != nil {
		t.Fatalf("explain budget query: %v", err)
	}
	defer planRows.Close()
	usedWindowIndex := false
	for planRows.Next() {
		var id, parent, unused int
		var detail string
		if err := planRows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatalf("scan query plan: %v", err)
		}
		if strings.Contains(detail, "idx_pipeline_started_at") {
			usedWindowIndex = true
		}
	}
	if !usedWindowIndex {
		t.Fatal("rolling budget query did not use idx_pipeline_started_at")
	}

	item := seedClaimBacklog(t, st, "MILLS-BUDGET-10K-CLAIM")
	req := claimTestRequest(item.ID)
	req.Limits = PipelineStartLimits{MaxConcurrentRuns: 10}
	budgetQuery, budgetArgs := buildPipelineStartBudgetSnapshotQuery(
		claimTestNow.Add(-24*time.Hour), req.Limits,
	)
	if !queryPlanUsesIndex(t, st, "EXPLAIN QUERY PLAN "+budgetQuery, budgetArgs, "idx_pipeline_state") {
		t.Fatal("active-run snapshot did not use idx_pipeline_state")
	}
	started := time.Now()
	if _, err := st.ClaimPipelineStart(ctx, req); err != nil {
		t.Fatalf("claim against 10k history: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("claim against 10k history took %s, want <=2s", elapsed)
	}
}

// deepQueueClaimRatioBound caps how much a 10k-row queue may inflate claim
// latency relative to an otherwise identical shallow queue. ClaimPipelineStart
// reaches backlog_items only by primary key plus one idx_backlog_state search
// over `running` rows, so queued depth must not register at all; a ratio past
// this is a new depth-dependent scan rather than scheduler noise.
//
// Deliberately NOT paired with an absolute noise floor. A `3 * max(shallow,
// floor)` allowance sounds prudent and is how this started, but at a 20ms floor
// it swallowed the very regression it exists to catch: with idx_backlog_state
// dropped the deep series ran 4.85x the shallow one (344µs -> 1.67ms at p50) and
// the floor still allowed 60ms. Both series are sampled in lockstep in one
// process, so the ratio holds without a floor: measured 0.86x-1.34x across
// -race and plain builds, idle and at load average 45, against 4.83x-4.86x at
// p50 for the dropped-index regression.
const deepQueueClaimRatioBound = 3

// seedClaimQueue inserts n queued backlog rows as `<prefix>-%05d` in one
// transaction. Callers claim a prefix of that range; the rows past it exist only
// to give the queue depth.
func seedClaimQueue(t *testing.T, st *Store, prefix string, n int) {
	t.Helper()
	ctx := context.Background()
	tx, err := st.DB().BeginTx(ctx, nil)
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
	for i := range n {
		id := fmt.Sprintf("%s-%05d", prefix, i)
		created := claimTestNow.Add(time.Duration(i) * time.Microsecond)
		if _, err := stmt.ExecContext(ctx, id, "queued item "+id,
			timeRFC3339(created), timeRFC3339(created)); err != nil {
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
}

// claimSample times one admission and asserts its fixed statement budget. The
// statement count is the contention-independent half of the bounded-work
// contract: it holds at any queue depth on any machine, busy or idle.
func claimSample(t *testing.T, st *Store, label, id string) time.Duration {
	t.Helper()
	req := claimTestRequest(id)
	req.ExpectedRevision = 0
	req.Limits = PipelineStartLimits{}
	var statements atomic.Int64
	req.FaultHook = func(ClaimPipelineStartFaultPoint) error {
		statements.Add(1)
		return nil
	}
	started := time.Now()
	if _, err := st.ClaimPipelineStart(context.Background(), req); err != nil {
		t.Fatalf("%s claim %s: %v", label, id, err)
	}
	elapsed := time.Since(started)
	if got := statements.Load(); got != int64(len(claimPipelineStartFaultPoints)) || got >= 20 {
		t.Fatalf("%s claim %s observed %d SQL boundaries; want %d and <20",
			label, id, got, len(claimPipelineStartFaultPoints))
	}
	return elapsed
}

// durationPercentile reuses the production ranking in percentile() so the test
// and the telemetry DAO agree on what "p95" means.
func durationPercentile(durations []time.Duration, pct float64) time.Duration {
	sorted := make([]int64, len(durations))
	for i, d := range durations {
		sorted[i] = int64(d)
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return time.Duration(percentile(sorted, pct))
}

// A 10k-row queue must not make one admission cost more than a shallow queue
// does, and one admission must stay inside its fixed statement budget.
//
// The depth half of that contract used to be an absolute `p95 < 100ms`, which
// measured the runner rather than the claim path. On 2026-08-09 it failed on
// main in test:race (p95=129.7ms, job 225918) and test:reliability (152.9ms, job
// 226771) while three concurrent main pipelines shared the runner, and it
// reproduces locally at load average 45 with GOMAXPROCS=2. It was also loose in
// the only condition where it could have caught anything: on an idle machine the
// claim path came in at 13ms, so a 7x depth regression would have passed.
//
// The depth claim is differential instead. Two stores that differ ONLY in queued
// depth are sampled in lockstep, so a CPU steal hits both series in the same
// window and cancels in the ratio, while a genuinely depth-dependent claim path
// moves the deep series alone. Statement counts and the query plan stay
// absolute — neither reads the clock.
func TestClaimPipelineStart_TenThousandQueueDepthNeutralAndStatementBound(t *testing.T) {
	const samples = 100
	shallow := newTestStore(t)
	deep := newTestStore(t)
	seedClaimQueue(t, shallow, "MILLS-QUEUE", samples)
	seedClaimQueue(t, deep, "MILLS-QUEUE", 10_000)

	// Structural witness for depth-neutrality, independent of any clock: the
	// only claim-path read not keyed by primary key is the scope-conflict scan,
	// and it is confined to `running` rows by idx_backlog_state. Were it to fall
	// back to a table scan, all 10k queued rows would land in every admission.
	scopeQuery := `EXPLAIN QUERY PLAN SELECT ` + backlogColumns + `
		FROM backlog_items WHERE state = ? AND id <> ? ORDER BY id ASC`
	if !queryPlanUsesIndex(t, deep, scopeQuery,
		[]any{string(BacklogRunning), "MILLS-QUEUE-00000"}, "idx_backlog_state") {
		t.Fatal("scope-conflict scan did not use idx_backlog_state")
	}

	// Sampled in lockstep on the same backlog id, alternating which store goes
	// first so neither series systematically pays the other's cache-warm cost.
	// Both stores therefore also grow their `running` set identically, keeping
	// the scope-conflict scan equal on both sides — queued depth stays the one
	// variable between them.
	shallowDurations := make([]time.Duration, 0, samples)
	deepDurations := make([]time.Duration, 0, samples)
	for i := range samples {
		id := fmt.Sprintf("MILLS-QUEUE-%05d", i)
		if i%2 == 0 {
			shallowDurations = append(shallowDurations, claimSample(t, shallow, "shallow", id))
			deepDurations = append(deepDurations, claimSample(t, deep, "deep", id))
			continue
		}
		deepDurations = append(deepDurations, claimSample(t, deep, "deep", id))
		shallowDurations = append(shallowDurations, claimSample(t, shallow, "shallow", id))
	}

	// Both the middle and the tail of the distribution have to stay depth-neutral.
	// p50 is the sensitive one — 100 samples make it steady enough to catch a
	// small constant inflation — and p95 catches a cost that only shows up
	// occasionally. Neither is compared against a wall-clock constant.
	for _, cut := range []struct {
		name string
		pct  float64
	}{{name: "p50", pct: 50}, {name: "p95", pct: 95}} {
		shallowCut := durationPercentile(shallowDurations, cut.pct)
		deepCut := durationPercentile(deepDurations, cut.pct)
		ratio := float64(deepCut) / float64(max(shallowCut, time.Nanosecond))
		t.Logf("claim %s: shallow(%d rows)=%s deep(10k rows)=%s ratio=%.2fx allowed=%dx",
			cut.name, samples, shallowCut, deepCut, ratio, deepQueueClaimRatioBound)
		if deepCut > deepQueueClaimRatioBound*shallowCut {
			t.Errorf("10k-queue claim %s=%s vs shallow-queue %s=%s (%.2fx, allowed %dx): "+
				"queue depth is reaching the claim path",
				cut.name, deepCut, cut.name, shallowCut, ratio, deepQueueClaimRatioBound)
		}
	}
}

func TestPipelineStartBudgetSnapshot_SkipsUncappedHistory(t *testing.T) {
	q, args := buildPipelineStartBudgetSnapshotQuery(claimTestNow, PipelineStartLimits{})
	if len(args) != 0 || strings.Contains(q, "pipeline_runs") || strings.Contains(q, "pipeline_budget_reservations") {
		t.Fatalf("uncapped snapshot scans history: query=%q args=%v", q, args)
	}

	q, args = buildPipelineStartBudgetSnapshotQuery(claimTestNow, PipelineStartLimits{MaxConcurrentRuns: 2})
	if len(args) != 0 || !strings.Contains(q, "state IN") || strings.Contains(q, "state NOT IN") ||
		strings.Contains(q, "started_at") {
		t.Fatalf("concurrency-only snapshot is not indexed/isolated: query=%q args=%v", q, args)
	}
}

func TestPipelineTerminal_ReleasesReservationAndSyncsDAGWorkflow(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	item := seedClaimBacklog(t, st, "MILLS-TERMINAL-SYNC")
	claim, err := st.ClaimPipelineStart(ctx, claimTestRequest(item.ID))
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	ended := claimTestNow.Add(10 * time.Minute)
	claim.Run.State = PipelineDone
	claim.Run.CostUSD = 1.25
	claim.Run.EndedAt = &ended
	if err := st.Pipeline.PutRun(ctx, claim.Run); err != nil {
		t.Fatalf("terminal put: %v", err)
	}
	// A repeated terminal rollup is safe and cannot reactivate or double-release.
	if err := st.Pipeline.PutRun(ctx, claim.Run); err != nil {
		t.Fatalf("repeated terminal put: %v", err)
	}

	var reservationState string
	var releasedAt string
	if err := st.DB().QueryRowContext(ctx, `
		SELECT state, released_at FROM pipeline_budget_reservations WHERE run_id = ?
	`, claim.Run.ID).Scan(&reservationState, &releasedAt); err != nil {
		t.Fatalf("load reservation: %v", err)
	}
	if reservationState != reservationStateReleased || releasedAt == "" {
		t.Fatalf("reservation state=%q released_at=%q", reservationState, releasedAt)
	}
	workflow, err := st.Workflow.GetWorkflowRun(ctx, claim.Run.ID)
	if err != nil {
		t.Fatalf("load workflow: %v", err)
	}
	if workflow.State != WorkflowRunDone || workflow.CostUSD != claim.Run.CostUSD || workflow.EndedAt == nil {
		t.Fatalf("terminal workflow not synchronized: %+v", workflow)
	}
}

func TestPipelinePutRun_AggregateAndTerminalInvariants(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	item := seedClaimBacklog(t, st, "MILLS-PIPELINE-CAS")
	run := &PipelineRun{
		ID:               "PIPELINE-CAS-RUN",
		BacklogID:        item.ID,
		AggregateVersion: 7,
		Template:         "test",
		State:            PipelineImplementing,
		Attempts:         1,
		StartedAt:        claimTestNow,
		CostUSD:          0.5,
	}
	if err := st.Pipeline.PutRun(ctx, run); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	tampered := *run
	tampered.AggregateVersion = 8
	tampered.State = PipelineTesting
	tampered.CostUSD = 1
	if err := st.Pipeline.PutRun(ctx, &tampered); !errors.Is(err, ErrStaleWrite) {
		t.Fatalf("aggregate tamper error=%v want ErrStaleWrite", err)
	}
	stored, err := st.Pipeline.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("load after aggregate tamper: %v", err)
	}
	if stored.AggregateVersion != 7 || stored.State != PipelineImplementing || stored.CostUSD != 0.5 {
		t.Fatalf("aggregate tamper changed run: %+v", stored)
	}

	ended := claimTestNow.Add(time.Minute)
	terminal := *run
	terminal.State = PipelineDone
	terminal.EndedAt = &ended
	terminal.CostUSD = 2
	if err := st.Pipeline.PutRun(ctx, &terminal); err != nil {
		t.Fatalf("terminal update: %v", err)
	}
	stale := *run
	stale.State = PipelineTesting
	stale.CostUSD = 3
	if err := st.Pipeline.PutRun(ctx, &stale); !errors.Is(err, ErrStaleWrite) {
		t.Fatalf("terminal reopen error=%v want ErrStaleWrite", err)
	}

	// A zero-version legacy writer cannot mutate a nonzero transactional run.
	terminal.AggregateVersion = 0
	terminal.CostUSD = 2.5
	if err := st.Pipeline.PutRun(ctx, &terminal); !errors.Is(err, ErrStaleWrite) {
		t.Fatalf("zero-version terminal rollup error=%v want ErrStaleWrite", err)
	}
	stored, err = st.Pipeline.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("load terminal run: %v", err)
	}
	if stored.AggregateVersion != 7 || stored.State != PipelineDone || stored.CostUSD != 2 {
		t.Fatalf("terminal invariant changed: %+v", stored)
	}

	// A true pre-migration row keeps aggregate_version=0 and may still receive
	// same-terminal cost rollups from legacy callers.
	legacy := &PipelineRun{
		ID: "PIPELINE-CAS-LEGACY", BacklogID: item.ID, AggregateVersion: 0,
		Template: "legacy", State: PipelineQueued, Attempts: 2, StartedAt: claimTestNow,
	}
	if err := st.Pipeline.PutRun(ctx, legacy); err != nil {
		t.Fatalf("seed legacy run: %v", err)
	}
	legacy.State = PipelineDone
	legacy.CostUSD = 1
	if err := st.Pipeline.PutRun(ctx, legacy); err != nil {
		t.Fatalf("terminalize legacy run: %v", err)
	}
	legacy.CostUSD = 1.5
	if err := st.Pipeline.PutRun(ctx, legacy); err != nil {
		t.Fatalf("same-terminal legacy rollup: %v", err)
	}
}

func TestPipelinePutRun_ConcurrentTerminalCannotReopen(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	const rounds = 50
	for i := range rounds {
		item := seedClaimBacklog(t, st, fmt.Sprintf("MILLS-PIPELINE-RACE-%02d", i))
		base := PipelineRun{
			ID:               fmt.Sprintf("PIPELINE-RACE-%02d", i),
			BacklogID:        item.ID,
			AggregateVersion: 1,
			Template:         "test",
			State:            PipelineImplementing,
			Attempts:         1,
			StartedAt:        claimTestNow,
		}
		if err := st.Pipeline.PutRun(ctx, &base); err != nil {
			t.Fatalf("round %d seed: %v", i, err)
		}
		terminal := base
		terminal.State = PipelineDone
		ended := claimTestNow.Add(time.Minute)
		terminal.EndedAt = &ended
		stale := base
		stale.State = PipelineTesting

		start := make(chan struct{})
		var terminalErr, staleErr error
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			terminalErr = st.Pipeline.PutRun(ctx, &terminal)
		}()
		go func() {
			defer wg.Done()
			<-start
			staleErr = st.Pipeline.PutRun(ctx, &stale)
		}()
		close(start)
		wg.Wait()
		if terminalErr != nil && !errors.Is(terminalErr, ErrStaleWrite) {
			t.Fatalf("round %d terminal update: %v", i, terminalErr)
		}
		if staleErr != nil && !errors.Is(staleErr, ErrStaleWrite) {
			t.Fatalf("round %d stale update: %v", i, staleErr)
		}
		if (terminalErr == nil) == (staleErr == nil) {
			t.Fatalf("round %d outcomes terminal=%v stale=%v; want exactly one CAS winner",
				i, terminalErr, staleErr)
		}
		got, err := st.Pipeline.GetRun(ctx, base.ID)
		if err != nil {
			t.Fatalf("round %d load: %v", i, err)
		}
		if got.State != PipelineDone {
			got.State = PipelineDone
			got.EndedAt = &ended
			if err := st.Pipeline.PutRun(ctx, got); err != nil {
				t.Fatalf("round %d terminal retry with current revision: %v", i, err)
			}
		}
		staleAgain := base
		staleAgain.State = PipelineTesting
		if err := st.Pipeline.PutRun(ctx, &staleAgain); !errors.Is(err, ErrStaleWrite) {
			t.Fatalf("round %d stale reopen after terminal error=%v want ErrStaleWrite", i, err)
		}
		got, err = st.Pipeline.GetRun(ctx, base.ID)
		if err != nil {
			t.Fatalf("round %d reload: %v", i, err)
		}
		if got.State != PipelineDone || got.AggregateVersion != 1 {
			t.Fatalf("round %d final run reopened: %+v", i, got)
		}
	}
}

func TestPipelinePutRun_ConcurrentConflictingTerminalHasSingleWinner(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	const rounds = 50
	for i := range rounds {
		item := seedClaimBacklog(t, st, fmt.Sprintf("MILLS-PIPELINE-TERM-RACE-%02d", i))
		base := PipelineRun{
			ID:               fmt.Sprintf("PIPELINE-TERM-RACE-%02d", i),
			BacklogID:        item.ID,
			AggregateVersion: 1,
			Template:         "test",
			State:            PipelineImplementing,
			Attempts:         1,
			StartedAt:        claimTestNow,
		}
		if err := st.Pipeline.PutRun(ctx, &base); err != nil {
			t.Fatalf("round %d seed: %v", i, err)
		}
		ended := claimTestNow.Add(time.Minute)
		done := base
		done.State = PipelineDone
		done.EndedAt = &ended
		escalated := base
		escalated.State = PipelineEscalated
		escalated.EndedAt = &ended

		start := make(chan struct{})
		var doneErr, escalatedErr error
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			doneErr = st.Pipeline.PutRun(ctx, &done)
		}()
		go func() {
			defer wg.Done()
			<-start
			escalatedErr = st.Pipeline.PutRun(ctx, &escalated)
		}()
		close(start)
		wg.Wait()
		if doneErr != nil && !errors.Is(doneErr, ErrStaleWrite) {
			t.Fatalf("round %d done terminal update: %v", i, doneErr)
		}
		if escalatedErr != nil && !errors.Is(escalatedErr, ErrStaleWrite) {
			t.Fatalf("round %d escalated terminal update: %v", i, escalatedErr)
		}
		if (doneErr == nil) == (escalatedErr == nil) {
			t.Fatalf("round %d terminal outcomes done=%v escalated=%v; want exactly one winner",
				i, doneErr, escalatedErr)
		}
		got, err := st.Pipeline.GetRun(ctx, base.ID)
		if err != nil {
			t.Fatalf("round %d load: %v", i, err)
		}
		loser := base
		switch got.State {
		case PipelineDone:
			loser.State = PipelineEscalated
		case PipelineEscalated:
			loser.State = PipelineDone
		default:
			t.Fatalf("round %d final state=%s, want done or escalated", i, got.State)
		}
		loser.EndedAt = &ended
		if err := st.Pipeline.PutRun(ctx, &loser); !errors.Is(err, ErrStaleWrite) {
			t.Fatalf("round %d losing terminal retry error=%v want ErrStaleWrite", i, err)
		}
		got, err = st.Pipeline.GetRun(ctx, base.ID)
		if err != nil {
			t.Fatalf("round %d reload: %v", i, err)
		}
		if got.State != PipelineDone && got.State != PipelineEscalated {
			t.Fatalf("round %d final run changed to non-terminal: %+v", i, got)
		}
	}
}

type countingTerminalConflictRecorder struct {
	count atomic.Int64
}

func (r *countingTerminalConflictRecorder) RecordTerminalStateConflict(string) {
	r.count.Add(1)
}

func TestPipelinePutRun_TerminalRejectionRecordsOneConflict(t *testing.T) {
	st := newTestStore(t)
	recorder := &countingTerminalConflictRecorder{}
	st.Pipeline.terminalConflictRecorder = recorder
	ctx := context.Background()
	item := seedClaimBacklog(t, st, "MILLS-PIPELINE-TERM-METRIC")
	run := PipelineRun{
		ID:               "PIPELINE-TERM-METRIC",
		BacklogID:        item.ID,
		AggregateVersion: 1,
		Template:         "test",
		State:            PipelineImplementing,
		Attempts:         1,
		StartedAt:        claimTestNow,
	}
	if err := st.Pipeline.PutRun(ctx, &run); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	done := run
	done.State = PipelineDone
	if err := st.Pipeline.PutRun(ctx, &done); err != nil {
		t.Fatalf("terminal winner: %v", err)
	}
	if got := recorder.count.Load(); got != 0 {
		t.Fatalf("conflicts after winner = %d, want 0", got)
	}

	escalated := run
	escalated.State = PipelineEscalated
	if err := st.Pipeline.PutRun(ctx, &escalated); !errors.Is(err, ErrStaleWrite) {
		t.Fatalf("conflicting terminal write error = %v, want ErrStaleWrite", err)
	}
	if got := recorder.count.Load(); got != 1 {
		t.Fatalf("conflicts after rejection = %d, want 1", got)
	}
	if err := st.Pipeline.PutRun(ctx, &escalated); !errors.Is(err, ErrStaleWrite) {
		t.Fatalf("repeated terminal write error = %v, want ErrStaleWrite", err)
	}
	if got := recorder.count.Load(); got != 2 {
		t.Fatalf("conflicts after repeated rejection = %d, want 2", got)
	}
}

func TestPipelinePutRun_StaleTerminalWriteDoesNotRecordStateConflict(t *testing.T) {
	st := newTestStore(t)
	recorder := &countingTerminalConflictRecorder{}
	st.Pipeline.terminalConflictRecorder = recorder
	ctx := context.Background()
	item := seedClaimBacklog(t, st, "MILLS-PIPELINE-TERM-STALE-METRIC")
	run := PipelineRun{
		ID:               "PIPELINE-TERM-STALE-METRIC",
		BacklogID:        item.ID,
		AggregateVersion: 1,
		Template:         "test",
		State:            PipelineImplementing,
		Attempts:         1,
		StartedAt:        claimTestNow,
	}
	if err := st.Pipeline.PutRun(ctx, &run); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	stale := run
	stale.Revision--
	stale.State = PipelineDone
	if err := st.Pipeline.PutRun(ctx, &stale); !errors.Is(err, ErrStaleWrite) {
		t.Fatalf("stale terminal write error = %v, want ErrStaleWrite", err)
	}
	if got := recorder.count.Load(); got != 0 {
		t.Fatalf("conflicts after revision-only rejection = %d, want 0", got)
	}
}

func TestPipelinePutRun_RejectsNonfiniteOrNegativeCost(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	item := seedClaimBacklog(t, st, "MILLS-PIPELINE-COST")
	tests := []struct {
		name string
		cost float64
	}{
		{name: "nan", cost: math.NaN()},
		{name: "positive_inf", cost: math.Inf(1)},
		{name: "negative_inf", cost: math.Inf(-1)},
		{name: "negative", cost: -0.01},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			run := &PipelineRun{
				ID:        fmt.Sprintf("PIPELINE-BAD-COST-%d", i),
				BacklogID: item.ID,
				Template:  "test",
				State:     PipelineQueued,
				StartedAt: claimTestNow,
				CostUSD:   tc.cost,
			}
			if err := st.Pipeline.PutRun(ctx, run); err == nil {
				t.Fatal("invalid pipeline cost was accepted")
			}
		})
	}
	assertTableCount(t, st, "pipeline_runs", 0)
}

func TestPendingDispatch_MarksAreIdempotent(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	item := seedClaimBacklog(t, st, "MILLS-DISPATCH-MARK")
	claim, err := st.ClaimPipelineStart(ctx, claimTestRequest(item.ID))
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	first, err := st.ClaimPendingDispatch(ctx, claim.Dispatch.ID, claimTestNow, time.Minute)
	if err != nil {
		t.Fatalf("claim first delivery: %v", err)
	}
	failure, err := st.MarkDispatchFailed(ctx, first.ID, first.LeaseToken,
		"starter unavailable", claimTestNow, DefaultDispatchRetryPolicy())
	if err != nil {
		t.Fatalf("mark failed: %v", err)
	}
	if failure == nil || failure.NextAttemptAt == nil {
		t.Fatalf("failure did not schedule retry: %+v", failure)
	}
	second, err := st.ClaimPendingDispatch(ctx, claim.Dispatch.ID, *failure.NextAttemptAt, time.Minute)
	if err != nil {
		t.Fatalf("claim retry delivery: %v", err)
	}

	const acknowledgers = 16
	var wg sync.WaitGroup
	errs := make(chan error, acknowledgers)
	for range acknowledgers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- st.MarkDispatchDelivered(ctx, second.ID, second.LeaseToken, *failure.NextAttemptAt)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("mark delivered: %v", err)
		}
	}
	if _, err := st.MarkDispatchFailed(ctx, second.ID, second.LeaseToken,
		"late failure", *failure.NextAttemptAt, DefaultDispatchRetryPolicy()); !errors.Is(err, ErrDispatchClaimConflict) {
		t.Fatalf("late failure error=%v want token conflict", err)
	}

	var status string
	var attempts int
	var lastError *string
	if err := st.DB().QueryRowContext(ctx, `
		SELECT status, attempts, last_error FROM pending_dispatches WHERE id = ?
	`, claim.Dispatch.ID).Scan(&status, &attempts, &lastError); err != nil {
		t.Fatalf("load dispatch: %v", err)
	}
	if status != string(DispatchDelivered) || attempts != 2 || lastError != nil {
		t.Fatalf("dispatch status=%s attempts=%d last_error=%v want delivered/2/nil", status, attempts, lastError)
	}
	if pending, err := st.CountPendingDispatches(ctx); err != nil || pending != 0 {
		t.Fatalf("pending count=%d err=%v", pending, err)
	}
}

func TestPendingDispatch_ListUsesReadyIndex(t *testing.T) {
	st := newTestStore(t)
	if !queryPlanUsesIndex(t, st, `
		EXPLAIN QUERY PLAN
		SELECT `+pendingDispatchColumns+`
		FROM pending_dispatches
		WHERE status = 'pending' AND next_attempt_at <= ?
		ORDER BY attempts ASC, next_attempt_at ASC, id ASC
		LIMIT ?
	`, []any{timeRFC3339(claimTestNow), 100}, "idx_pending_dispatches_ready") {
		t.Fatal("pending dispatch list did not use idx_pending_dispatches_ready")
	}
}

func TestPendingDispatch_LeaseExpiryRecoversWithFencedToken(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	item := seedClaimBacklog(t, st, "MILLS-DISPATCH-LEASE")
	claim, err := st.ClaimPipelineStart(ctx, claimTestRequest(item.ID))
	if err != nil {
		t.Fatalf("claim pipeline: %v", err)
	}
	lease := 10 * time.Second
	first, err := st.ClaimPendingDispatch(ctx, claim.Dispatch.ID, claimTestNow, lease)
	if err != nil {
		t.Fatalf("first delivery claim: %v", err)
	}
	if _, err := st.ClaimPendingDispatch(ctx, claim.Dispatch.ID, claimTestNow.Add(lease-time.Millisecond), lease); !errors.Is(err, ErrDispatchClaimConflict) {
		t.Fatalf("unexpired lease error=%v want ErrDispatchClaimConflict", err)
	}
	second, err := st.ClaimPendingDispatch(ctx, claim.Dispatch.ID, claimTestNow.Add(lease), lease)
	if err != nil {
		t.Fatalf("expired lease was not recoverable: %v", err)
	}
	if first.LeaseToken == second.LeaseToken || second.Attempts != 2 {
		t.Fatalf("recovered lease=%+v first_token=%q", second, first.LeaseToken)
	}
	if err := st.MarkDispatchDelivered(ctx, first.ID, first.LeaseToken, claimTestNow.Add(lease)); !errors.Is(err, ErrDispatchClaimConflict) {
		t.Fatalf("stale token ack error=%v want ErrDispatchClaimConflict", err)
	}
	if err := st.MarkDispatchDelivered(ctx, second.ID, second.LeaseToken, claimTestNow.Add(lease)); err != nil {
		t.Fatalf("current token ack: %v", err)
	}
}

func TestPendingDispatch_RetryCeilingDeadLettersCurrentAggregate(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	item := seedClaimBacklog(t, st, "MILLS-DISPATCH-DEAD")
	claim, err := st.ClaimPipelineStart(ctx, claimTestRequest(item.ID))
	if err != nil {
		t.Fatalf("claim pipeline: %v", err)
	}
	policy := DispatchRetryPolicy{BaseDelay: time.Second, MaxDelay: 4 * time.Second, MaxAttempts: 3}
	now := claimTestNow
	var final *DispatchFailureResult
	for attempt := 1; attempt <= policy.MaxAttempts; attempt++ {
		intent, err := st.ClaimPendingDispatch(ctx, claim.Dispatch.ID, now, time.Minute)
		if err != nil {
			t.Fatalf("claim delivery %d: %v", attempt, err)
		}
		final, err = st.MarkDispatchFailed(ctx, intent.ID, intent.LeaseToken,
			"starter unavailable", now, policy)
		if err != nil {
			t.Fatalf("fail delivery %d: %v", attempt, err)
		}
		if final.NextAttemptAt != nil {
			now = *final.NextAttemptAt
		}
	}
	if final == nil || !final.DeadLettered || final.Attempts != policy.MaxAttempts {
		t.Fatalf("final failure result: %+v", final)
	}
	backlog, err := st.Backlog.Get(ctx, item.ID)
	if err != nil {
		t.Fatalf("load backlog: %v", err)
	}
	if backlog.State != BacklogEscalated || backlog.ClaimVersion != 2 || backlog.Revision != 3 {
		t.Fatalf("dead-letter backlog: %+v", backlog)
	}
	run, err := st.Pipeline.GetRun(ctx, claim.Run.ID)
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if run.State != PipelineEscalated || run.Revision != 2 {
		t.Fatalf("dead-letter run state=%s revision=%d want escalated/2", run.State, run.Revision)
	}
	stale := *claim.Run
	stale.State = PipelineEscalated
	if err := st.Pipeline.PutRun(ctx, &stale); !errors.Is(err, ErrStaleWrite) {
		t.Fatalf("stale same-terminal write error=%v want ErrStaleWrite", err)
	}
	workflow, err := st.Workflow.GetWorkflowRun(ctx, run.ID)
	if err != nil || workflow.State != WorkflowRunEscalated {
		t.Fatalf("dead-letter workflow=%+v err=%v", workflow, err)
	}
	var reservationState, dispatchStatus string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT state FROM pipeline_budget_reservations WHERE run_id = ?`, run.ID).Scan(&reservationState); err != nil {
		t.Fatalf("load reservation: %v", err)
	}
	if err := st.DB().QueryRowContext(ctx,
		`SELECT status FROM pending_dispatches WHERE id = ?`, claim.Dispatch.ID).Scan(&dispatchStatus); err != nil {
		t.Fatalf("load dispatch: %v", err)
	}
	if reservationState != reservationStateReleased || dispatchStatus != string(DispatchDeadLetter) {
		t.Fatalf("reservation=%s dispatch=%s", reservationState, dispatchStatus)
	}
	assertTableCount(t, st, "pipeline_transitions", 2)
}

func TestPendingDispatch_ObsoleteIntentCannotEscalateNewAggregate(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	item := seedClaimBacklog(t, st, "MILLS-DISPATCH-OBSOLETE")
	first, err := st.ClaimPipelineStart(ctx, claimTestRequest(item.ID))
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	requeued, err := st.Backlog.Get(ctx, item.ID)
	if err != nil {
		t.Fatalf("load first aggregate: %v", err)
	}
	requeued.State = BacklogQueued
	if err := st.Backlog.Put(ctx, requeued); err != nil {
		t.Fatalf("requeue: %v", err)
	}
	secondReq := claimTestRequest(item.ID)
	secondReq.ExpectedClaimVersion = 1
	secondReq.ExpectedRevision = requeued.Revision
	secondReq.Now = claimTestNow.Add(time.Second)
	second, err := st.ClaimPipelineStart(ctx, secondReq)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}

	intent, err := st.ClaimPendingDispatch(ctx, first.Dispatch.ID, claimTestNow.Add(time.Second), time.Minute)
	if err != nil {
		t.Fatalf("claim obsolete dispatch: %v", err)
	}
	result, err := st.MarkDispatchFailed(ctx, intent.ID, intent.LeaseToken, "obsolete",
		claimTestNow.Add(time.Second), DispatchRetryPolicy{MaxAttempts: 1})
	if err != nil {
		t.Fatalf("dead-letter obsolete dispatch: %v", err)
	}
	if result == nil || !result.DeadLettered {
		t.Fatalf("obsolete result: %+v", result)
	}
	backlog, err := st.Backlog.Get(ctx, item.ID)
	if err != nil {
		t.Fatalf("load current backlog: %v", err)
	}
	if backlog.State != BacklogRunning || backlog.ClaimVersion != 2 || backlog.Revision != 4 {
		t.Fatalf("obsolete intent mutated current aggregate: %+v", backlog)
	}
	currentRun, err := st.Pipeline.GetRun(ctx, second.Run.ID)
	if err != nil || currentRun.State != PipelineQueued {
		t.Fatalf("current run=%+v err=%v", currentRun, err)
	}
	oldRun, err := st.Pipeline.GetRun(ctx, first.Run.ID)
	if err != nil || oldRun.State != PipelineEscalated {
		t.Fatalf("obsolete run=%+v err=%v", oldRun, err)
	}
	assertTableCount(t, st, "pipeline_transitions", 2)
}

func TestPendingDispatch_DuePoisonBatchDoesNotStarveFreshTail(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	const poisonCount = 128
	claims := make([]*ClaimPipelineStartResult, 0, poisonCount+1)
	for i := 0; i <= poisonCount; i++ {
		id := fmt.Sprintf("MILLS-DISPATCH-QUEUE-%03d", i)
		seedClaimBacklog(t, st, id)
		claim, err := st.ClaimPipelineStart(ctx, claimTestRequest(id))
		if err != nil {
			t.Fatalf("claim %d: %v", i, err)
		}
		claims = append(claims, claim)
	}
	policy := DispatchRetryPolicy{BaseDelay: time.Second, MaxDelay: time.Second, MaxAttempts: 1000}
	for i := 0; i < poisonCount; i++ {
		intent, err := st.ClaimPendingDispatch(ctx, claims[i].Dispatch.ID, claimTestNow, time.Minute)
		if err != nil {
			t.Fatalf("claim poison %d: %v", i, err)
		}
		if _, err := st.MarkDispatchFailed(ctx, intent.ID, intent.LeaseToken,
			"poison", claimTestNow, policy); err != nil {
			t.Fatalf("schedule poison %d: %v", i, err)
		}
	}
	claimed, err := st.ClaimPendingDispatches(ctx, 1, claimTestNow.Add(time.Second), time.Minute)
	if err != nil {
		t.Fatalf("claim fresh tail: %v", err)
	}
	if len(claimed) != 1 || claimed[0].ID != claims[poisonCount].Dispatch.ID || claimed[0].Attempts != 1 {
		t.Fatalf("fresh tail starved by due poison batch: %+v", claimed)
	}
}

// queryPlan returns the detail column of every EXPLAIN QUERY PLAN row. Callers
// assert on plan shape when the property under test is "this read cannot grow
// with the table" — a claim a wall clock can only approximate.
func queryPlan(t *testing.T, st *Store, query string, args []any) []string {
	t.Helper()
	rows, err := st.DB().QueryContext(context.Background(), query, args...)
	if err != nil {
		t.Fatalf("explain query plan: %v", err)
	}
	defer rows.Close()
	var details []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatalf("scan query plan: %v", err)
		}
		details = append(details, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read query plan: %v", err)
	}
	return details
}

func queryPlanUsesIndex(t *testing.T, st *Store, query string, args []any, index string) bool {
	t.Helper()
	for _, detail := range queryPlan(t, st, query, args) {
		if strings.Contains(detail, index) {
			return true
		}
	}
	return false
}

func assertTableCount(t *testing.T, st *Store, table string, want int) {
	t.Helper()
	var got int
	if err := st.DB().QueryRowContext(context.Background(), `SELECT COUNT(*) FROM `+table).Scan(&got); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	if got != want {
		t.Fatalf("count %s=%d want %d", table, got, want)
	}
}
