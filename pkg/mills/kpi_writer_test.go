package mills

import (
	"context"
	"encoding/json"
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

// TestKPIWriter_GatePassRateExcludesSkips pins the KPI half of the scope-gate
// skip change: a 'skip' outcome (advisory/not-applicable gate) must be excluded
// from BOTH the pass count and the total so it neither raises nor lowers
// gate_pass_rate — a skip is not a fail. Seeds 2 pass + 1 fail + 1 skip and
// expects rate = 2/3 over 3 counted evaluations.
func TestKPIWriter_GatePassRateExcludesSkips(t *testing.T) {
	env := newRecEnv(t, nil)
	ctx := context.Background()
	now := env.now

	if err := env.store.Backlog.Put(ctx, &store.BacklogItem{
		ID: "BACK-G", Title: "g", State: store.BacklogMerged,
		Priority: store.P2, CreatedBy: "test",
	}); err != nil {
		t.Fatalf("seed backlog: %v", err)
	}
	if err := env.store.Pipeline.PutRun(ctx, &store.PipelineRun{
		ID: "PIPE-G", BacklogID: "BACK-G", Template: "mills-default",
		State: store.PipelineDone, StartedAt: now.Add(-30 * time.Minute),
	}); err != nil {
		t.Fatalf("seed pipeline: %v", err)
	}
	for i, oc := range []store.GateOutcomeKind{
		store.GateOutcomePass, store.GateOutcomePass, store.GateOutcomeFail, store.GateOutcomeSkip,
	} {
		if err := env.store.Pipeline.PutGate(ctx, &store.GateOutcome{
			PipelineRunID: "PIPE-G", GateName: fmt.Sprintf("g%d", i), AfterStage: "post_implement_gate",
			Outcome: oc, JudgedBy: "go", EvaluatedAt: now.Add(-20 * time.Minute),
		}); err != nil {
			t.Fatalf("seed gate %d: %v", i, err)
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
	// skip excluded → 3 counted evaluations, 2 passes → 2/3.
	assertMetric(t, snap.Metrics, "gate_evaluations", float64(3))
	assertMetric(t, snap.Metrics, "gate_passes", float64(2))
	got, ok := snap.Metrics["gate_pass_rate"].(float64)
	if !ok {
		t.Fatalf("gate_pass_rate = %#v, want float64", snap.Metrics["gate_pass_rate"])
	}
	if want := 2.0 / 3.0; got < want-1e-9 || got > want+1e-9 {
		t.Errorf("gate_pass_rate = %v, want %v (skip must not count)", got, want)
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
	// escalations_by_class: the one escalated run (PIPE-ESC) carries no
	// fault-class marker, so it buckets under "unclassified". This pins the
	// full plumbing DAO GROUP BY → snapshot metrics → metrics_json round-trip
	// (nested object, counts survive as JSON float64).
	escByClass, ok := snap.Metrics["escalations_by_class"].(map[string]any)
	if !ok {
		t.Fatalf("escalations_by_class = %#v, want map", snap.Metrics["escalations_by_class"])
	}
	if got, _ := escByClass["unclassified"].(float64); got != 1 {
		t.Errorf("escalations_by_class[unclassified] = %v, want 1", escByClass["unclassified"])
	}
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

// TestKPIWriter_RealMergedRuns_ExcludesCanary proves the real-work north-star
// (mills_autonomous_merges_real) counts only non-canary merges, so a loop that
// merges nothing but heartbeat fixtures reads real=0 while the headline gauge
// looks healthy — the signal that was missing during the 2026-06-30 audit.
func TestKPIWriter_RealMergedRuns_ExcludesCanary(t *testing.T) {
	env := newRecEnv(t, nil)
	ctx := context.Background()
	now := env.now

	seed := func(id string, labels []string, state store.PipelineState, started time.Time) {
		if err := env.store.Backlog.Put(ctx, &store.BacklogItem{
			ID: id, Title: id, State: store.BacklogMerged,
			Priority: store.P2, CreatedBy: "test", Labels: labels,
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

	canaryLabels := []string{store.CanaryLabel, store.CanarySafeFixtureLabel}
	seed("PIPE-CANARY-1", canaryLabels, store.PipelineDone, now.Add(-2*time.Hour))
	seed("PIPE-CANARY-2", canaryLabels, store.PipelineDone, now.Add(-5*time.Hour))
	seed("PIPE-REAL-1", []string{"docs"}, store.PipelineDone, now.Add(-3*time.Hour))
	// A real merge with no labels still counts as real.
	seed("PIPE-REAL-2", nil, store.PipelineDone, now.Add(-4*time.Hour))
	// An escalated item counts toward neither gauge.
	seed("PIPE-REAL-ESC", []string{"docs"}, store.PipelineEscalated, now.Add(-1*time.Hour))
	// A canary-label SUPERSTRING must not be misclassified as a canary.
	seed("PIPE-SUPERSTRING", []string{store.CanaryLabel + "-x"}, store.PipelineDone, now.Add(-6*time.Hour))

	// 5 done runs; 2 are canaries → 3 real (incl the no-label and superstring).
	real, err := countRealMergedRunsSince(ctx, env.store, now.Add(-kpiWindow1d))
	if err != nil {
		t.Fatalf("countRealMergedRunsSince: %v", err)
	}
	if real != 3 {
		t.Fatalf("real merged = %d, want 3 (excl 2 canaries; incl no-label + superstring)", real)
	}

	AutonomousMerges.Reset()
	AutonomousMergesReal.Reset()
	writer := NewKPIWriter(env.store, env.policy)
	writer.Clock = func() time.Time { return now }
	writer.Windows = []time.Duration{kpiWindow1d}
	if err := writer.SeedDurableGauges(ctx); err != nil {
		t.Fatalf("seed durable gauges: %v", err)
	}

	if got := testutil.ToFloat64(AutonomousMerges.WithLabelValues("1d")); got != 5 {
		t.Errorf("mills_autonomous_merges{1d} = %v, want 5 (all done runs)", got)
	}
	if got := testutil.ToFloat64(AutonomousMergesReal.WithLabelValues("1d")); got != 3 {
		t.Errorf("mills_autonomous_merges_real{1d} = %v, want 3 (canaries excluded)", got)
	}

	// The snapshot carries the same real count for the HUD/status path.
	snap, err := writer.snapshot(ctx, now, kpiWindow1d)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if v, _ := snap.Metrics["pipeline_merged_real"].(int); v != 3 {
		t.Errorf("snapshot pipeline_merged_real = %v, want 3", snap.Metrics["pipeline_merged_real"])
	}
	if v, _ := snap.Metrics["pipeline_merged_runs"].(int); v != 5 {
		t.Errorf("snapshot pipeline_merged_runs = %v, want 5", snap.Metrics["pipeline_merged_runs"])
	}
}

// TestKPIWriter_RetryCostAndUnparseableRate pins the two S5 metrics:
// retry_cost_usd (sum of stage cost for attempt > 1 across windowed runs) and
// gate_unparseable_rate (fraction of gate outcomes the judge could not parse).
func TestKPIWriter_RetryCostAndUnparseableRate(t *testing.T) {
	env := newRecEnv(t, nil)
	ctx := context.Background()
	now := env.now

	if err := env.store.Backlog.Put(ctx, &store.BacklogItem{
		ID: "BACK-RC", Title: "rc", State: store.BacklogRunning, Priority: store.P2, CreatedBy: "test",
	}); err != nil {
		t.Fatalf("seed backlog: %v", err)
	}
	if err := env.store.Pipeline.PutRun(ctx, &store.PipelineRun{
		ID: "PIPE-RC", BacklogID: "BACK-RC", Template: "mills-default",
		State: store.PipelineEscalated, StartedAt: now.Add(-2 * time.Hour),
	}); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	// An out-of-window run whose retry cost must NOT be counted.
	if err := env.store.Backlog.Put(ctx, &store.BacklogItem{
		ID: "BACK-RC-OLD", Title: "old", State: store.BacklogMerged, Priority: store.P2, CreatedBy: "test",
	}); err != nil {
		t.Fatalf("seed old backlog: %v", err)
	}
	if err := env.store.Pipeline.PutRun(ctx, &store.PipelineRun{
		ID: "PIPE-RC-OLD", BacklogID: "BACK-RC-OLD", Template: "mills-default",
		State: store.PipelineDone, StartedAt: now.Add(-3 * 24 * time.Hour),
	}); err != nil {
		t.Fatalf("seed old run: %v", err)
	}
	putStage := func(runID, stage string, attempt int, cost float64) {
		t.Helper()
		out := store.StageOutcomeSuccess
		start := now.Add(-2 * time.Hour).Add(time.Duration(attempt) * time.Minute)
		end := start.Add(time.Minute)
		if err := env.store.Pipeline.PutStage(ctx, &store.StageResult{
			PipelineRunID: runID, Stage: stage, Attempt: attempt,
			StartedAt: start, EndedAt: &end, Outcome: &out, CostUSD: cost,
		}); err != nil {
			t.Fatalf("seed stage %s/%s a%d: %v", runID, stage, attempt, err)
		}
	}
	putStage("PIPE-RC", "implement", 1, 1.00) // first attempt, not retry burn
	putStage("PIPE-RC", "implement", 2, 2.50) // retry
	putStage("PIPE-RC", "implement", 3, 1.75) // retry
	putStage("PIPE-RC-OLD", "implement", 2, 9.99)

	putGate := func(runID string, outcome store.GateOutcomeKind, judgedBy string) {
		t.Helper()
		if err := env.store.Pipeline.PutGate(ctx, &store.GateOutcome{
			PipelineRunID: runID, AfterStage: "pr_self_review", GateName: "pr_self_review",
			Outcome: outcome, JudgedBy: judgedBy, EvaluatedAt: now.Add(-90 * time.Minute),
		}); err != nil {
			t.Fatalf("seed gate: %v", err)
		}
	}
	// 4 gate evaluations, 1 unparseable ⇒ rate 0.25.
	putGate("PIPE-RC", store.GateOutcomePass, "flexinfer:qwen")
	putGate("PIPE-RC", store.GateOutcomePass, "flexinfer:qwen")
	putGate("PIPE-RC", store.GateOutcomeFail, "flexinfer:qwen")
	putGate("PIPE-RC", store.GateOutcomeFail, store.JudgedByUnparseable)

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
	// retry_cost_usd sums attempts 2+3 of the in-window run (2.50 + 1.75), and
	// excludes the 3-day-old run's retry.
	assertMetricCloseTo(t, snap.Metrics, "retry_cost_usd", 4.25, 1e-9)
	assertMetric(t, snap.Metrics, "gate_unparseable_rate", 0.25)
}

// TestKPIWriter_EscalationsByClass_EmptyEncodesAsObject pins the wire contract
// for a window with zero escalations: escalations_by_class must serialize as an
// empty object ({}), never null. A nil map would marshal to null and force the
// HUD to null-check a field it treats as an object — the same []-not-null
// discipline the store's slice DAOs follow.
func TestKPIWriter_EscalationsByClass_EmptyEncodesAsObject(t *testing.T) {
	env := newRecEnv(t, nil)
	ctx := context.Background()
	now := env.now

	writer := NewKPIWriter(env.store, env.policy)
	writer.Clock = func() time.Time { return now }
	writer.Windows = []time.Duration{kpiWindow1d}

	// Empty store: no escalated runs in the window.
	snap, err := writer.snapshot(ctx, now, kpiWindow1d)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	raw, present := snap.Metrics["escalations_by_class"]
	if !present {
		t.Fatal("escalations_by_class absent; want an empty object")
	}
	byClass, ok := raw.(map[string]int)
	if !ok {
		t.Fatalf("escalations_by_class = %#v, want map[string]int", raw)
	}
	if byClass == nil {
		t.Fatal("escalations_by_class is nil; want a non-nil empty map")
	}
	if len(byClass) != 0 {
		t.Fatalf("escalations_by_class = %#v, want empty", byClass)
	}

	// The load-bearing assertion: JSON must be {} not null.
	encoded, err := json.Marshal(byClass)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(encoded) != "{}" {
		t.Fatalf("escalations_by_class JSON = %s, want {}", encoded)
	}
}

// TestKPIWriter_EscalatedActiveDiscountsSuperseded proves Trustworthy
// Verdicts S3: the raw escalated gauge keeps its historical meaning while
// _active nets out escalations whose verdict was superseded.
func TestKPIWriter_EscalatedActiveDiscountsSuperseded(t *testing.T) {
	env := newRecEnv(t, nil)
	ctx := context.Background()
	now := env.now

	for i, id := range []string{"KPI-E1", "KPI-E2", "KPI-E3"} {
		if err := env.store.Backlog.Put(ctx, &store.BacklogItem{
			ID: "BL-" + id, Title: id, State: store.BacklogEscalated,
			Priority: store.P2, CreatedBy: "test",
		}); err != nil {
			t.Fatalf("seed backlog: %v", err)
		}
		if err := env.store.Pipeline.PutRun(ctx, &store.PipelineRun{
			ID: id, BacklogID: "BL-" + id, Template: "t",
			State: store.PipelineEscalated, Attempts: 1,
			StartedAt: now.Add(-time.Duration(i+1) * time.Hour),
		}); err != nil {
			t.Fatalf("seed run: %v", err)
		}
	}
	if err := env.store.Events.Append(ctx, &store.Event{
		Actor: "reconciler", Kind: RunVerdictKindGhostSparkMerged,
		SubjectKind: "pipeline_run", SubjectID: "KPI-E2",
		OccurredAt: now.Add(-10 * time.Minute),
	}); err != nil {
		t.Fatalf("append correction: %v", err)
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
	assertMetric(t, snap.Metrics, "pipeline_escalated_runs", float64(3))
	assertMetric(t, snap.Metrics, "pipeline_escalated_active", float64(2))
	assertMetric(t, snap.Metrics, "pipeline_escalated_superseded", float64(1))
}
