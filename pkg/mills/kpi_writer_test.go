package mills

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/crb2nu/loom/pkg/mills/store"
)

func TestKPIWriter_RecordWritesRollingSnapshot(t *testing.T) {
	env := newRecEnv(t, nil)
	ctx := context.Background()
	now := env.now

	if err := env.store.Backlog.Put(ctx, &store.BacklogItem{
		ID: "BACK-QUEUED", Title: "queued", State: store.BacklogQueued,
		Priority: store.P2, CreatedBy: "test",
	}); err != nil {
		t.Fatalf("seed backlog: %v", err)
	}
	if err := env.store.Backlog.Put(ctx, &store.BacklogItem{
		ID: "BACK-DONE", Title: "done", State: store.BacklogMerged,
		Priority: store.P2, CreatedBy: "test",
	}); err != nil {
		t.Fatalf("seed done backlog: %v", err)
	}
	if err := env.store.Council.Put(ctx, &store.CouncilRun{
		ID: "COUNCIL-1", Trigger: store.CouncilTriggerManual,
		StartedAt: now.Add(-time.Hour), Outcome: store.CouncilOutcomeSuccess,
		CostFrontierUSD: 0.7, CostLocalUSD: 0.3,
	}); err != nil {
		t.Fatalf("seed council: %v", err)
	}
	if err := env.store.Pipeline.PutRun(ctx, &store.PipelineRun{
		ID: "PIPE-1", BacklogID: "BACK-DONE", Template: "mills-default",
		State: store.PipelineDone, StartedAt: now.Add(-30 * time.Minute),
		CostUSD: 2.5,
	}); err != nil {
		t.Fatalf("seed pipeline: %v", err)
	}
	if err := env.store.Pipeline.PutGate(ctx, &store.GateOutcome{
		PipelineRunID: "PIPE-1", GateName: "diff_size",
		Outcome: store.GateOutcomePass, EvaluatedAt: now.Add(-20 * time.Minute),
	}); err != nil {
		t.Fatalf("seed gate: %v", err)
	}
	if err := env.store.Eval.RecordScore(ctx, &store.EvalScore{
		SubjectKind: store.EvalSubjectPipelineRun, SubjectID: "PIPE-1",
		Rubric: "pipeline_outcome_v1", Score: 0.8,
		EvaluatedAt: now.Add(-10 * time.Minute),
	}); err != nil {
		t.Fatalf("seed eval: %v", err)
	}

	writer := NewKPIWriter(env.store, env.policy)
	writer.Clock = func() time.Time { return now }
	writer.Windows = []time.Duration{kpiWindow1d}

	if err := writer.Record(ctx); err != nil {
		t.Fatalf("record: %v", err)
	}

	snap, err := env.store.KPI.Latest(ctx, int(kpiWindow1d.Seconds()))
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	if snap.SnapshotAt != now {
		t.Fatalf("snapshot_at = %s, want %s", snap.SnapshotAt, now)
	}
	assertMetric(t, snap.Metrics, "queue_depth", float64(1))
	assertMetric(t, snap.Metrics, "active_pipeline_runs", float64(0))
	assertMetric(t, snap.Metrics, "council_runs", float64(1))
	assertMetric(t, snap.Metrics, "council_cost_usd", float64(1))
	assertMetric(t, snap.Metrics, "pipeline_runs", float64(1))
	assertMetric(t, snap.Metrics, "pipeline_merged_runs", float64(1))
	assertMetric(t, snap.Metrics, "pipeline_cost_usd", 2.5)
	assertMetric(t, snap.Metrics, "gate_pass_rate", float64(1))
	assertMetric(t, snap.Metrics, "eval_average_score", 0.8)
	if got, ok := snap.Metrics["policy_enabled"].(bool); !ok || !got {
		t.Fatalf("policy_enabled = %#v, want true", snap.Metrics["policy_enabled"])
	}
}

func assertMetric(t *testing.T, metrics map[string]any, key string, want float64) {
	t.Helper()
	got, ok := metrics[key].(float64)
	if !ok {
		t.Fatalf("%s = %#v, want float64", key, metrics[key])
	}
	if got != want {
		t.Fatalf("%s = %v, want %v", key, got, want)
	}
}

// TestKPIWriter_FrontendKeys_PopulatedWhenDataAvailable pins the
// five keys MillsKPIRow.svelte renders. The cards stay dark when
// the snapshot is missing any of them, so the writer must produce
// these whenever the underlying counts make them defined. Each
// metric has a comment in kpi_writer.go documenting its proxy
// definition; this test pins that contract end-to-end.
func TestKPIWriter_FrontendKeys_PopulatedWhenDataAvailable(t *testing.T) {
	env := newRecEnv(t, nil)
	ctx := context.Background()
	now := env.now

	// Two merged runs (one fast, one slow) and one escalated run so
	// every ratio is non-degenerate: mergedRuns=2, escalatedRuns=1,
	// total terminal=3, durations=[60s, 180s] for merged,
	// total pipelineCost = $2 + $3 + $0.5 = $5.50 (window-wide,
	// not merged-only), councilCost=$1.
	mustPutRun := func(id string, st store.PipelineState, started time.Time, dur time.Duration, cost float64) {
		t.Helper()
		backlogID := "BACK-" + id
		if err := env.store.Backlog.Put(ctx, &store.BacklogItem{
			ID: backlogID, Title: id, State: store.BacklogMerged,
			Priority: store.P2, CreatedBy: "test",
		}); err != nil {
			t.Fatalf("seed backlog %s: %v", backlogID, err)
		}
		end := started.Add(dur)
		var endPtr *time.Time
		if dur > 0 {
			endPtr = &end
		}
		if err := env.store.Pipeline.PutRun(ctx, &store.PipelineRun{
			ID: id, BacklogID: backlogID, Template: "mills-default",
			State: st, StartedAt: started, EndedAt: endPtr, CostUSD: cost,
		}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	mustPutRun("PIPE-FAST", store.PipelineDone, now.Add(-2*time.Hour), 60*time.Second, 2.0)
	mustPutRun("PIPE-SLOW", store.PipelineDone, now.Add(-90*time.Minute), 180*time.Second, 3.0)
	mustPutRun("PIPE-ESC", store.PipelineEscalated, now.Add(-time.Hour), 30*time.Second, 0.5)

	if err := env.store.Council.Put(ctx, &store.CouncilRun{
		ID: "COUNCIL-1", Trigger: store.CouncilTriggerManual,
		StartedAt:       now.Add(-time.Hour),
		Outcome:         store.CouncilOutcomeSuccess,
		CostFrontierUSD: 0.7, CostLocalUSD: 0.3,
	}); err != nil {
		t.Fatalf("seed council: %v", err)
	}

	writer := NewKPIWriter(env.store, env.policy)
	writer.Clock = func() time.Time { return now }
	writer.Windows = []time.Duration{kpiWindow1d}
	if err := writer.Record(ctx); err != nil {
		t.Fatalf("record: %v", err)
	}
	snap, err := env.store.KPI.Latest(ctx, int(kpiWindow1d.Seconds()))
	if err != nil {
		t.Fatalf("latest: %v", err)
	}

	// cost_per_merged_pipeline_usd = $5.50 total window cost / 2 merged = $2.75.
	// Numerator is window-wide pipeline cost (includes escalated runs),
	// reflecting "$ spent per successful merge" including the cost of
	// failed work.
	assertMetric(t, snap.Metrics, "cost_per_merged_pipeline_usd", 2.75)
	// cost_per_merged_change_usd: backlog-grouped attribution. Distinct
	// merged backlogs = {BACK-PIPE-FAST, BACK-PIPE-SLOW} → 2.
	// PIPE-ESC's backlog (BACK-PIPE-ESC) is NOT in the merged set, so
	// its $0.50 is excluded. Numerator = $2 + $3 = $5; denom = 2;
	// cost_per_merged_change = $2.50. Diverges from per-pipeline
	// ($2.75) because escalated-only backlogs no longer pollute the
	// per-change view.
	assertMetric(t, snap.Metrics, "cost_per_merged_change_usd", 2.5)
	// auto_merge_rate = 2 done / (2 done + 1 escalated) = 0.6666...
	assertMetricCloseTo(t, snap.Metrics, "auto_merge_rate", 2.0/3.0, 1e-9)
	// escalation_rate = 1 escalated / (2 done + 1 escalated) = 0.3333...
	// Renamed from the old `regression_rate` proxy so the autonomous-
	// pipeline-completion signal is preserved under its honest name.
	assertMetricCloseTo(t, snap.Metrics, "escalation_rate", 1.0/3.0, 1e-9)
	// regression_rate: label-driven. No backlog in this fixture has
	// the regression-fix label so num = 0; denom = 2 merged backlogs.
	// Metric is emitted as 0% (an intentional "0 regressions" signal)
	// — the UI flags it as `(proxy)` until file-overlap detection
	// lands.
	assertMetric(t, snap.Metrics, "regression_rate", 0.0)
	// council_roi = 2 merged / $1 council cost = 2.0
	assertMetric(t, snap.Metrics, "council_roi", 2.0)
	// slice_to_merge_p50_seconds: median of [60, 180] = 120
	assertMetric(t, snap.Metrics, "slice_to_merge_p50_seconds", 120.0)
}

// TestKPIWriter_FrontendKeys_OmittedWhenInsufficientData pins the
// negative half of the contract: when there's no data to compute
// a ratio, the key is omitted entirely so MillsKPIRow shows "—"
// rather than "$0" or "0%" (which would imply efficient operation
// when reality is "no activity yet").
func TestKPIWriter_FrontendKeys_OmittedWhenInsufficientData(t *testing.T) {
	env := newRecEnv(t, nil)
	ctx := context.Background()
	now := env.now

	writer := NewKPIWriter(env.store, env.policy)
	writer.Clock = func() time.Time { return now }
	writer.Windows = []time.Duration{kpiWindow1d}
	if err := writer.Record(ctx); err != nil {
		t.Fatalf("record: %v", err)
	}
	snap, err := env.store.KPI.Latest(ctx, int(kpiWindow1d.Seconds()))
	if err != nil {
		t.Fatalf("latest: %v", err)
	}

	for _, key := range []string{
		"cost_per_merged_change_usd",
		"cost_per_merged_pipeline_usd",
		"auto_merge_rate",
		"escalation_rate",
		"regression_rate",
		"council_roi",
		"slice_to_merge_p50_seconds",
	} {
		if _, present := snap.Metrics[key]; present {
			t.Errorf("%s present with no data; want omitted to render '—'", key)
		}
	}
}

// TestKPIWriter_CostPerMergedChange_BacklogGroupedAttribution pins
// the multi-attempt case where backlog-grouped attribution diverges
// from the per-pipeline value. One backlog has an escalated attempt
// followed by a successful merge; both attempts' cost must be
// attributed to the single merged change, while the per-pipeline
// metric divides by the count of merged pipeline_runs (which is just
// the successful one). This is the load-bearing semantic difference
// between cost_per_merged_change_usd and cost_per_merged_pipeline_usd.
func TestKPIWriter_CostPerMergedChange_BacklogGroupedAttribution(t *testing.T) {
	env := newRecEnv(t, nil)
	ctx := context.Background()
	now := env.now

	// One backlog item with two attempts: first escalated ($4),
	// retry merged ($2). Plus a second, single-attempt merged
	// backlog ($1).
	if err := env.store.Backlog.Put(ctx, &store.BacklogItem{
		ID: "BACK-RETRY", Title: "retried", State: store.BacklogMerged,
		Priority: store.P2, CreatedBy: "test",
	}); err != nil {
		t.Fatalf("seed backlog retry: %v", err)
	}
	if err := env.store.Backlog.Put(ctx, &store.BacklogItem{
		ID: "BACK-SOLO", Title: "solo", State: store.BacklogMerged,
		Priority: store.P2, CreatedBy: "test",
	}); err != nil {
		t.Fatalf("seed backlog solo: %v", err)
	}
	mustPut := func(id, backlog string, attempts int, st store.PipelineState, cost float64) {
		t.Helper()
		started := now.Add(-time.Hour)
		end := started.Add(60 * time.Second)
		if err := env.store.Pipeline.PutRun(ctx, &store.PipelineRun{
			ID: id, BacklogID: backlog, Template: "mills-default",
			Attempts: attempts,
			State:    st, StartedAt: started, EndedAt: &end, CostUSD: cost,
		}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	mustPut("PIPE-RETRY-1", "BACK-RETRY", 0, store.PipelineEscalated, 4.0)
	mustPut("PIPE-RETRY-2", "BACK-RETRY", 1, store.PipelineDone, 2.0)
	mustPut("PIPE-SOLO", "BACK-SOLO", 0, store.PipelineDone, 1.0)

	writer := NewKPIWriter(env.store, env.policy)
	writer.Clock = func() time.Time { return now }
	writer.Windows = []time.Duration{kpiWindow1d}
	if err := writer.Record(ctx); err != nil {
		t.Fatalf("record: %v", err)
	}
	snap, err := env.store.KPI.Latest(ctx, int(kpiWindow1d.Seconds()))
	if err != nil {
		t.Fatalf("latest: %v", err)
	}

	// cost_per_merged_pipeline_usd = total window cost / merged runs
	//   = ($4 + $2 + $1) / 2 = $3.50
	// (BACK-RETRY's escalated $4 IS in window cost, both done runs
	//  count toward mergedRuns denominator)
	assertMetric(t, snap.Metrics, "cost_per_merged_pipeline_usd", 3.5)
	// cost_per_merged_change_usd: distinct merged backlogs = 2
	// (BACK-RETRY, BACK-SOLO). Pipeline runs whose backlog is in
	// that set: all 3 runs (both BACK-RETRY attempts + BACK-SOLO).
	// Numerator = $4 + $2 + $1 = $7. Denominator = 2.
	//   = $3.50 in this fixture (matches per-pipeline because both
	//   backlogs happen to be in the merged set and PIPE-ESC of the
	//   other test isn't here)
	// The key insight: if a third backlog had an escalated-only run,
	// per-pipeline would pollute its denominator with that cost while
	// per-change would correctly exclude it. See
	// TestKPIWriter_CostPerMergedChange_ExcludesNonMergedBacklog
	// below.
	assertMetric(t, snap.Metrics, "cost_per_merged_change_usd", 3.5)
}

// TestKPIWriter_CostPerMergedChange_ExcludesNonMergedBacklog pins
// the case where the two cost metrics diverge: an escalated-only
// backlog (no merged attempt) inflates the per-pipeline burn rate
// but should NOT contribute to per-change attribution.
func TestKPIWriter_CostPerMergedChange_ExcludesNonMergedBacklog(t *testing.T) {
	env := newRecEnv(t, nil)
	ctx := context.Background()
	now := env.now

	put := func(id, backlog string, st store.PipelineState, cost float64) {
		t.Helper()
		if err := env.store.Backlog.Put(ctx, &store.BacklogItem{
			ID: backlog, Title: backlog,
			State:    store.BacklogMerged,
			Priority: store.P2, CreatedBy: "test",
		}); err != nil {
			t.Fatalf("seed %s: %v", backlog, err)
		}
		started := now.Add(-time.Hour)
		end := started.Add(60 * time.Second)
		if err := env.store.Pipeline.PutRun(ctx, &store.PipelineRun{
			ID: id, BacklogID: backlog, Template: "mills-default",
			State: st, StartedAt: started, EndedAt: &end, CostUSD: cost,
		}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	put("PIPE-OK", "BACK-OK", store.PipelineDone, 1.0)
	put("PIPE-STUCK", "BACK-STUCK", store.PipelineEscalated, 9.0)

	writer := NewKPIWriter(env.store, env.policy)
	writer.Clock = func() time.Time { return now }
	writer.Windows = []time.Duration{kpiWindow1d}
	if err := writer.Record(ctx); err != nil {
		t.Fatalf("record: %v", err)
	}
	snap, err := env.store.KPI.Latest(ctx, int(kpiWindow1d.Seconds()))
	if err != nil {
		t.Fatalf("latest: %v", err)
	}

	// cost_per_merged_pipeline = $10 total / 1 merged = $10.
	assertMetric(t, snap.Metrics, "cost_per_merged_pipeline_usd", 10.0)
	// cost_per_merged_change = $1 (only BACK-OK's run, since
	// BACK-STUCK never merged) / 1 distinct merged backlog = $1.
	// 10x cheaper than per-pipeline because stuck work isn't
	// attributed to merged changes.
	assertMetric(t, snap.Metrics, "cost_per_merged_change_usd", 1.0)
}

// TestKPIWriter_RegressionRate_LabelDriven pins the new label-based
// regression rate: count distinct merged backlogs with the
// regression-fix label, divide by total merged backlogs in window.
func TestKPIWriter_RegressionRate_LabelDriven(t *testing.T) {
	env := newRecEnv(t, nil)
	ctx := context.Background()
	now := env.now

	put := func(id, backlog string, labels []string, cost float64) {
		t.Helper()
		if err := env.store.Backlog.Put(ctx, &store.BacklogItem{
			ID: backlog, Title: backlog, Labels: labels,
			State:    store.BacklogMerged,
			Priority: store.P2, CreatedBy: "test",
		}); err != nil {
			t.Fatalf("seed %s: %v", backlog, err)
		}
		started := now.Add(-time.Hour)
		end := started.Add(60 * time.Second)
		if err := env.store.Pipeline.PutRun(ctx, &store.PipelineRun{
			ID: id, BacklogID: backlog, Template: "mills-default",
			State: store.PipelineDone, StartedAt: started, EndedAt: &end,
			CostUSD: cost,
		}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	put("PIPE-FIX", "BACK-FIX", []string{"regression-fix"}, 1.0)
	put("PIPE-FEAT-A", "BACK-FEAT-A", []string{"feature"}, 1.0)
	put("PIPE-FEAT-B", "BACK-FEAT-B", nil, 1.0)

	writer := NewKPIWriter(env.store, env.policy)
	writer.Clock = func() time.Time { return now }
	writer.Windows = []time.Duration{kpiWindow1d}
	if err := writer.Record(ctx); err != nil {
		t.Fatalf("record: %v", err)
	}
	snap, err := env.store.KPI.Latest(ctx, int(kpiWindow1d.Seconds()))
	if err != nil {
		t.Fatalf("latest: %v", err)
	}

	// 1 of 3 merged backlogs carry the regression-fix label.
	assertMetricCloseTo(t, snap.Metrics, "regression_rate", 1.0/3.0, 1e-9)
}

// TestKPIWriter_P50_OddCountReturnsExactMiddle pins the odd-n
// median branch since the FrontendKeys test exercises only the
// even-n interpolated case.
func TestKPIWriter_P50_OddCountReturnsExactMiddle(t *testing.T) {
	env := newRecEnv(t, nil)
	ctx := context.Background()
	now := env.now

	for i, dur := range []time.Duration{30 * time.Second, 90 * time.Second, 240 * time.Second} {
		backlogID := fmt.Sprintf("BACK-%d", i)
		if err := env.store.Backlog.Put(ctx, &store.BacklogItem{
			ID: backlogID, Title: backlogID, State: store.BacklogMerged,
			Priority: store.P2, CreatedBy: "test",
		}); err != nil {
			t.Fatalf("seed backlog %d: %v", i, err)
		}
		end := now.Add(-time.Hour).Add(dur)
		if err := env.store.Pipeline.PutRun(ctx, &store.PipelineRun{
			ID: fmt.Sprintf("PIPE-%d", i), BacklogID: backlogID,
			Template: "mills-default", State: store.PipelineDone,
			StartedAt: now.Add(-time.Hour), EndedAt: &end, CostUSD: 1,
		}); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	writer := NewKPIWriter(env.store, env.policy)
	writer.Clock = func() time.Time { return now }
	writer.Windows = []time.Duration{kpiWindow1d}
	if err := writer.Record(ctx); err != nil {
		t.Fatalf("record: %v", err)
	}
	snap, err := env.store.KPI.Latest(ctx, int(kpiWindow1d.Seconds()))
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	// Median of [30, 90, 240] = 90 (exact middle, no interpolation)
	assertMetric(t, snap.Metrics, "slice_to_merge_p50_seconds", 90.0)
}

func assertMetricCloseTo(t *testing.T, metrics map[string]any, key string, want, tol float64) {
	t.Helper()
	got, ok := metrics[key].(float64)
	if !ok {
		t.Fatalf("%s = %#v, want float64", key, metrics[key])
	}
	if diff := got - want; diff < -tol || diff > tol {
		t.Fatalf("%s = %v, want within %v of %v", key, got, tol, want)
	}
}

// TestKPIWriter_SeedDurableGauges_RecomputesFromStore is the W1.1 restart-
// durability proof: a FRESH KPIWriter (simulating the operator process after a
// pod roll, with all in-memory counters reset) must publish the correct
// mills_autonomous_merges gauge by re-deriving it from the durable store alone.
// This is the guarantee that the north-star survives the constant operator
// rolls that reset mills_pipeline_runs_total{state="done"} to 0.
func TestKPIWriter_SeedDurableGauges_RecomputesFromStore(t *testing.T) {
	env := newRecEnv(t, nil)
	ctx := context.Background()
	now := env.now

	// pipeline_runs.backlog_id has a FOREIGN KEY to backlog_items, so seed a
	// backlog item per run before the run.
	seedRun := func(id string, state store.PipelineState, started time.Time) {
		if err := env.store.Backlog.Put(ctx, &store.BacklogItem{
			ID: id, Title: id, State: store.BacklogMerged,
			Priority: store.P2, CreatedBy: "test",
		}); err != nil {
			t.Fatalf("seed backlog %s: %v", id, err)
		}
		if err := env.store.Pipeline.PutRun(ctx, &store.PipelineRun{
			ID: id, BacklogID: id, Template: "mills-default",
			State: state, StartedAt: started, CostUSD: 1,
		}); err != nil {
			t.Fatalf("seed run %s: %v", id, err)
		}
	}
	// Two merges inside the 24h window, one older (inside 7d, outside 1d).
	seedRun("PIPE-RECENT-1", store.PipelineDone, now.Add(-2*time.Hour))
	seedRun("PIPE-RECENT-2", store.PipelineDone, now.Add(-10*time.Hour))
	seedRun("PIPE-OLD", store.PipelineDone, now.Add(-3*24*time.Hour))
	// An escalated run inside 24h must NOT count toward autonomous merges.
	seedRun("PIPE-ESC", store.PipelineEscalated, now.Add(-1*time.Hour))

	AutonomousMerges.Reset()

	writer := NewKPIWriter(env.store, env.policy)
	writer.Clock = func() time.Time { return now }
	writer.Windows = []time.Duration{kpiWindow1d, kpiWindow7d}

	if err := writer.SeedDurableGauges(ctx); err != nil {
		t.Fatalf("seed durable gauges: %v", err)
	}

	if got := testutil.ToFloat64(AutonomousMerges.WithLabelValues("1d")); got != 2 {
		t.Errorf("mills_autonomous_merges{window=1d} = %v, want 2", got)
	}
	if got := testutil.ToFloat64(AutonomousMerges.WithLabelValues("7d")); got != 3 {
		t.Errorf("mills_autonomous_merges{window=7d} = %v, want 3", got)
	}
}
