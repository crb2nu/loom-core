package mills

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/crb2nu/loom/pkg/mills/store"
)

const (
	kpiWindow1d  = 24 * time.Hour
	kpiWindow7d  = 7 * 24 * time.Hour
	kpiWindow30d = 30 * 24 * time.Hour

	// regressionFixLabel marks a backlog item as a fix for a previously-
	// merged regression. The label is the canonical signal for the
	// regression_rate KPI; an upstream emitter (operator, integrator,
	// or human) must tag the backlog with this label for the change to
	// count as a regression. Until file-overlap detection lands the
	// metric is still considered a proxy and the HUD card flags it as
	// such.
	regressionFixLabel = "regression-fix"
)

// KPIWriter records rolling snapshots into the canonical kpi_snapshots table.
// It intentionally reads from the store, not Prometheus, so status/HUD/council
// briefs still have local KPI evidence if metrics scraping is degraded.
type KPIWriter struct {
	Store  *store.Store
	Policy *PolicyManager
	Logger *slog.Logger

	// Windows defaults to 1d, 7d, and 30d to match /api/mills/kpis.
	Windows []time.Duration

	// Clock makes snapshot timestamps deterministic in tests.
	Clock func() time.Time
}

// NewKPIWriter returns a writer configured for the REST-supported windows.
func NewKPIWriter(st *store.Store, pm *PolicyManager) *KPIWriter {
	return &KPIWriter{
		Store:   st,
		Policy:  pm,
		Logger:  slog.Default(),
		Windows: []time.Duration{kpiWindow1d, kpiWindow7d, kpiWindow30d},
		Clock:   time.Now,
	}
}

// Record appends one snapshot per configured window.
func (w *KPIWriter) Record(ctx context.Context) error {
	if w == nil || w.Store == nil || w.Store.KPI == nil {
		return fmt.Errorf("kpi writer: store not configured")
	}
	now := w.now().UTC()
	windows := w.Windows
	if len(windows) == 0 {
		windows = []time.Duration{kpiWindow1d, kpiWindow7d, kpiWindow30d}
	}
	for _, window := range windows {
		if window <= 0 {
			return fmt.Errorf("kpi writer: window must be positive")
		}
		snap, err := w.snapshot(ctx, now, window)
		if err != nil {
			return err
		}
		if err := w.Store.KPI.RecordSnapshot(ctx, snap); err != nil {
			return err
		}
		// Mirror the durable merged-run count into the restart-safe
		// AutonomousMerges gauge (snapshot() already computed it from the
		// store for this window). This refreshes the gauge each tick so it
		// decays correctly as merges age out of the window.
		if merged, ok := snap.Metrics["pipeline_merged_runs"].(int); ok {
			AutonomousMerges.WithLabelValues(windowLabel(window)).Set(float64(merged))
		}
		// Same refresh for the real-work (canary-excluded) north-star, so a
		// loop merging only heartbeat canaries reads real=0 while the headline
		// gauge looks healthy.
		if real, ok := snap.Metrics["pipeline_merged_real"].(int); ok {
			AutonomousMergesReal.WithLabelValues(windowLabel(window)).Set(float64(real))
		}
	}
	return nil
}

// SeedDurableGauges recomputes the AutonomousMerges gauge for every configured
// window directly from the store and publishes it, WITHOUT writing a KPI
// snapshot. The operator calls this once at startup so mills_autonomous_merges
// is correct the instant /metrics is first scraped after a pod roll — the
// scheduler's first Record (which also refreshes the gauge) can be up to
// IdleInterval (default 5min) away. This is the fix for the counter-reset trap
// where mills_pipeline_runs_total{state="done"} reads 0 after every restart and
// mis-frames the north-star as 0 (see .loom/126 STATUS RECONCILED banner).
func (w *KPIWriter) SeedDurableGauges(ctx context.Context) error {
	if w == nil || w.Store == nil {
		return fmt.Errorf("kpi writer: store not configured")
	}
	now := w.now().UTC()
	windows := w.Windows
	if len(windows) == 0 {
		windows = []time.Duration{kpiWindow1d, kpiWindow7d, kpiWindow30d}
	}
	for _, window := range windows {
		if window <= 0 {
			continue
		}
		merged, err := countPipelineStateSince(ctx, w.Store, store.PipelineDone, now.Add(-window))
		if err != nil {
			return err
		}
		AutonomousMerges.WithLabelValues(windowLabel(window)).Set(float64(merged))
		real, err := countRealMergedRunsSince(ctx, w.Store, now.Add(-window))
		if err != nil {
			return err
		}
		AutonomousMergesReal.WithLabelValues(windowLabel(window)).Set(float64(real))
	}
	return nil
}

// windowLabel maps a KPI window duration to the stable Prometheus label used by
// AutonomousMerges. Unknown windows fall back to Duration.String() so a custom
// Windows config still produces a (less pretty but valid) series.
func windowLabel(d time.Duration) string {
	switch d {
	case kpiWindow1d:
		return "1d"
	case kpiWindow7d:
		return "7d"
	case kpiWindow30d:
		return "30d"
	default:
		return d.String()
	}
}

func (w *KPIWriter) snapshot(ctx context.Context, now time.Time, window time.Duration) (*store.KPISnapshot, error) {
	since := now.Add(-window)

	queueDepth, err := countBacklogState(ctx, w.Store, store.BacklogQueued)
	if err != nil {
		return nil, err
	}
	active, err := w.Store.Pipeline.CountActive(ctx)
	if err != nil {
		return nil, err
	}
	councilRuns, err := w.Store.Council.CountSince(ctx, since)
	if err != nil {
		return nil, err
	}
	councilCost, err := w.Store.Council.SumCostSince(ctx, since)
	if err != nil {
		return nil, err
	}
	pipelineRuns, err := w.Store.Pipeline.CountSince(ctx, since)
	if err != nil {
		return nil, err
	}
	pipelineCost, err := w.Store.Pipeline.SumCostSince(ctx, since)
	if err != nil {
		return nil, err
	}
	mergedRuns, err := countPipelineStateSince(ctx, w.Store, store.PipelineDone, since)
	if err != nil {
		return nil, err
	}
	realMergedRuns, err := countRealMergedRunsSince(ctx, w.Store, since)
	if err != nil {
		return nil, err
	}
	escalatedRuns, err := countPipelineStateSince(ctx, w.Store, store.PipelineEscalated, since)
	if err != nil {
		return nil, err
	}
	// Superseded escalations (Trustworthy Verdicts S3): runs still recorded
	// escalated whose verdict was corrected because their MR merged. The raw
	// gauge keeps its historical meaning; _active is the number that should
	// drive attention.
	escalatedSuperseded := 0
	if w.Store.Events != nil {
		escalatedRows, err := w.Store.Pipeline.ListByStateSince(ctx, store.PipelineEscalated, since)
		if err != nil {
			return nil, err
		}
		superseded, err := SupersededRunIDsSince(ctx, w.Store.Events, since)
		if err != nil {
			return nil, err
		}
		for _, r := range escalatedRows {
			if r == nil {
				continue
			}
			if _, ok := superseded[r.ID]; ok {
				escalatedSuperseded++
			}
		}
	}
	escalationsByClass, err := w.Store.Pipeline.CountEscalationsByClassSince(ctx, since)
	if err != nil {
		return nil, err
	}
	autoRequeues, err := w.Store.Events.CountByKindSince(ctx, eventKindAutoRequeued, since)
	if err != nil {
		return nil, err
	}
	// Encode an empty breakdown as {} rather than null so the KPI JSON stays
	// consistent with the repo's []-not-null convention (the HUD reads
	// escalations_by_class as an object). The DAO already returns a non-nil
	// map, but this keeps the wire contract stable if that ever regresses.
	if escalationsByClass == nil {
		escalationsByClass = map[string]int{}
	}
	gatePass, gateTotal, err := countGateOutcomesSince(ctx, w.Store, since)
	if err != nil {
		return nil, err
	}
	gateUnparseable, err := countUnparseableGatesSince(ctx, w.Store, since)
	if err != nil {
		return nil, err
	}
	retryCost, err := retryStageCostSince(ctx, w.Store, since)
	if err != nil {
		return nil, err
	}
	avgEval, err := avgEvalScoreSince(ctx, w.Store, since)
	if err != nil {
		return nil, err
	}

	metrics := map[string]any{
		"policy_enabled":          w.policyEnabled(),
		"queue_depth":             queueDepth,
		"active_pipeline_runs":    active,
		"council_runs":            councilRuns,
		"council_cost_usd":        councilCost,
		"pipeline_runs":           pipelineRuns,
		"pipeline_cost_usd":       pipelineCost,
		"pipeline_merged_runs":    mergedRuns,
		"pipeline_merged_real":    realMergedRuns,
		"pipeline_escalated_runs": escalatedRuns,
		// _active nets out escalations whose verdict was superseded (MR
		// merged after escalation); _superseded is the discount itself.
		// The raw gauge above keeps its historical meaning for existing
		// dashboards; alerts and the foreman storm rule read _active.
		"pipeline_escalated_active":     escalatedRuns - escalatedSuperseded,
		"pipeline_escalated_superseded": escalatedSuperseded,
		"escalations_by_class":          escalationsByClass,
		// auto_requeues: escalated items the reconciler auto-requeued
		// (escalated→queued) in the window without a human hitting the requeue
		// endpoint. Windowed like the other counters; for the primary 1d
		// snapshot this is the rolling-24h fleet total the per-day cap bounds.
		"auto_requeues":      autoRequeues,
		"gate_evaluations":   gateTotal,
		"gate_passes":        gatePass,
		"eval_average_score": avgEval,
		// retry_cost_usd: dollars spent on stage attempts > 1 in the window
		// (the "retry burn" — money re-running stages that failed the first
		// time). Same started_at window mechanics as pipeline_cost_usd, but
		// summed over stage_results rather than pipeline_runs so it isolates the
		// retry share. Always emitted (0 when nothing retried).
		"retry_cost_usd": retryCost,
	}
	if gateTotal > 0 {
		metrics["gate_pass_rate"] = float64(gatePass) / float64(gateTotal)
		// gate_unparseable_rate: fraction of gate evaluations in the window that
		// the LLM judge could not parse into a score envelope (judged_by ==
		// "flexinfer:unparseable"). These are false-fails, not code-quality
		// signals; surfacing the rate lets the operator see judge-harness health
		// distinct from real gate failures. Same evaluated_at window as
		// gate_pass_rate so numerator and denominator align.
		metrics["gate_unparseable_rate"] = float64(gateUnparseable) / float64(gateTotal)
	}
	if mergedRuns > 0 {
		// cost_per_merged_pipeline_usd: total window pipeline cost
		// (includes escalated work) divided by the count of merged
		// pipeline_runs. Older of the two cost-per-merge keys; useful
		// when the operator wants the "raw burn rate" view that
		// includes work-in-progress money.
		metrics["cost_per_merged_pipeline_usd"] = pipelineCost / float64(mergedRuns)
	}
	// cost_per_merged_change_usd: total cost of pipeline_runs whose
	// backlog item ended up merged in the window, divided by the count
	// of distinct merged backlog items ("changes"). This collapses
	// multi-attempt runs (e.g. an escalated attempt followed by a
	// successful retry of the same backlog) into one change while
	// still attributing the failed attempts' cost — that is what an
	// operator means by "$ per merged change". Excludes the cost of
	// pipeline_runs whose backlog did not merge in window (those
	// remain in cost_per_merged_pipeline_usd as the "burn rate" view).
	changeCost, changeDenom, err := costPerMergedChange(ctx, w.Store, since)
	if err != nil {
		return nil, err
	}
	if changeDenom > 0 {
		metrics["cost_per_merged_change_usd"] = changeCost / float64(changeDenom)
	}
	// auto_merge_rate: fraction of terminal runs in the window that
	// reached `done` (vs `escalated`). Mills always uses the
	// auto-merge path so this measures autonomous-success rate, not
	// human vs auto merge selection. Denominator is mergedRuns +
	// escalatedRuns so the metric stays bounded to [0,1] over actual
	// outcomes.
	if denom := mergedRuns + escalatedRuns; denom > 0 {
		metrics["auto_merge_rate"] = float64(mergedRuns) / float64(denom)
		// escalation_rate: fraction of terminal runs in the window
		// that escalated (vs. reached done). Previously emitted under
		// the name "regression_rate" as a best-effort proxy; now
		// emitted under its honest name so operators see the
		// autonomous-pipeline-completion signal under a label that
		// matches what it measures. regression_rate (below) is now a
		// separate, label-driven signal.
		metrics["escalation_rate"] = float64(escalatedRuns) / float64(denom)
	}
	// regression_rate: fraction of merged changes in the window that
	// were themselves regression-fix work (per the
	// regression-fix label on the backlog item). Emitted when there
	// are any merged backlog items in the window so the operator can
	// see "0% regressions" as a distinct, intentional signal — but
	// the HUD card flags this metric as `(proxy)` until file-overlap
	// detection lands and removes the dependency on a hand-emitted
	// label.
	regressionNum, regressionDenom, err := regressionRateFromLabels(ctx, w.Store, since)
	if err != nil {
		return nil, err
	}
	if regressionDenom > 0 {
		metrics["regression_rate"] = float64(regressionNum) / float64(regressionDenom)
	}
	// council_roi: merged changes per dollar of council spend. A
	// value > 1 means each $1 of council deliberation produced > 1
	// merged change in the window. Omitted when councilCost <= 0 so
	// the frontend renders "—" rather than divide-by-zero infinity.
	if councilCost > 0 {
		metrics["council_roi"] = float64(mergedRuns) / councilCost
	}
	// slice_to_merge_p50_seconds: median wall-clock from started_at
	// to ended_at over runs that reached state=done in the window.
	// Computed in Go because SQLite lacks a built-in percentile
	// function.
	p50, err := mergedRunDurationP50(ctx, w.Store, since)
	if err != nil {
		return nil, err
	}
	if p50 > 0 {
		metrics["slice_to_merge_p50_seconds"] = p50
	}

	// Durable-workflow counters (plan .loom/134 §S4a). These give the HUD
	// step-log panel + the operator status a headline view of the imperative
	// runtime without a per-run scan. CRITICAL: workflow_avg_cost_per_step_usd
	// is branched on cost_source — it rolls up ONLY `real` cost over `real`
	// steps. Summing real + estimated + unavailable(=0) as if comparable would
	// produce a meaningless blended figure, so estimated cost is surfaced under
	// a separate key and never folded into the average.
	if err := w.recordWorkflowMetrics(ctx, metrics, since); err != nil {
		return nil, err
	}

	return &store.KPISnapshot{
		SnapshotAt:    now,
		WindowSeconds: int(window.Seconds()),
		Metrics:       metrics,
	}, nil
}

// recordWorkflowMetrics adds the durable-workflow KPI counters to metrics.
// Split out of snapshot() so the CostSource-branching logic has one home and a
// dedicated test can assert it. Counters:
//
//   - workflow_active_runs        — running imperative runs RIGHT NOW (state,
//     not window: an active run is a present-tense fact).
//   - workflow_quarantined_runs   — runs in state='quarantined' right now.
//   - workflow_completed_steps    — steps that reached status='success' with an
//     ended_at inside the window.
//   - workflow_failed_steps       — steps that reached status IN (error,
//     gate_fail) inside the window.
//   - workflow_avg_cost_per_step_usd — sum(real cost) / count(real steps) over
//     the window. ONLY the `real` bucket; estimated cost is surfaced separately
//     and never blended in. Omitted entirely when there are no real-cost steps
//     (no synthetic 0 that would read as "free").
//   - workflow_estimated_cost_usd — sum of `estimated`-source step cost in the
//     window, kept distinct so an operator sees estimated burn without it
//     contaminating the real average.
func (w *KPIWriter) recordWorkflowMetrics(ctx context.Context, metrics map[string]any, since time.Time) error {
	dao := w.Store.Workflow
	if dao == nil {
		return nil
	}

	activeRuns, err := dao.CountRunsByState(ctx, store.WorkflowRunRunning)
	if err != nil {
		return err
	}
	quarantinedRuns, err := dao.CountRunsByState(ctx, store.WorkflowRunQuarantined)
	if err != nil {
		return err
	}
	completedSteps, err := dao.CountStepsByStatusSince(ctx, store.WorkflowStepSuccess, since)
	if err != nil {
		return err
	}
	failedErr, err := dao.CountStepsByStatusSince(ctx, store.WorkflowStepError, since)
	if err != nil {
		return err
	}
	failedGate, err := dao.CountStepsByStatusSince(ctx, store.WorkflowStepGateFail, since)
	if err != nil {
		return err
	}
	rollup, err := dao.StepCostRollupSince(ctx, since)
	if err != nil {
		return err
	}

	metrics["workflow_active_runs"] = activeRuns
	metrics["workflow_quarantined_runs"] = quarantinedRuns
	metrics["workflow_completed_steps"] = completedSteps
	metrics["workflow_failed_steps"] = failedErr + failedGate

	// Branch on CostSource: average only over real-cost steps. Never divide a
	// blended (real+estimated) numerator — that is the precise mistake the
	// rollup's separate buckets exist to prevent.
	if rollup.RealSteps > 0 {
		metrics["workflow_avg_cost_per_step_usd"] = rollup.RealCostUSD / float64(rollup.RealSteps)
	}
	// Surface estimated burn separately so it is visible but isolated.
	if rollup.EstimatedCostUSD > 0 {
		metrics["workflow_estimated_cost_usd"] = rollup.EstimatedCostUSD
	}
	return nil
}

// mergedRunDurationP50 returns the median (started_at → ended_at)
// duration in seconds across runs that reached state=done in the
// window. Returns 0 when there are no completed runs (caller treats
// 0 as "omit the metric").
func mergedRunDurationP50(ctx context.Context, st *store.Store, since time.Time) (float64, error) {
	runs, err := st.Pipeline.ListByStateSince(ctx, store.PipelineDone, since)
	if err != nil {
		return 0, fmt.Errorf("kpi p50 list: %w", err)
	}
	durations := make([]float64, 0, len(runs))
	for _, r := range runs {
		if r == nil || r.EndedAt == nil {
			continue
		}
		d := r.EndedAt.Sub(r.StartedAt).Seconds()
		if d <= 0 {
			continue
		}
		durations = append(durations, d)
	}
	if len(durations) == 0 {
		return 0, nil
	}
	sort.Float64s(durations)
	n := len(durations)
	if n%2 == 1 {
		return durations[n/2], nil
	}
	mid := n / 2
	return (durations[mid-1] + durations[mid]) / 2, nil
}

func (w *KPIWriter) policyEnabled() bool {
	if w == nil || w.Policy == nil || w.Policy.Current() == nil {
		return false
	}
	return w.Policy.Current().IsEnabled()
}

func (w *KPIWriter) now() time.Time {
	if w.Clock != nil {
		return w.Clock()
	}
	return time.Now()
}

func countBacklogState(ctx context.Context, st *store.Store, state store.BacklogState) (int, error) {
	items, err := st.Backlog.ListByState(ctx, state)
	if err != nil {
		return 0, err
	}
	return len(items), nil
}

func countPipelineStateSince(ctx context.Context, st *store.Store, state store.PipelineState, since time.Time) (int, error) {
	row := st.DB().QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM pipeline_runs
		WHERE state = ? AND started_at >= ?
	`, string(state), kpiTime(since))
	var n int
	if err := row.Scan(&n); err != nil {
		return 0, fmt.Errorf("kpi pipeline state count: %w", err)
	}
	return n, nil
}

// countRealMergedRunsSince counts merged pipeline runs in the window EXCLUDING
// mills-canary heartbeat fixtures, by joining each run to its backlog item and
// filtering out items carrying the canary label. This is the denominator-free
// "real work landed" signal behind mills_autonomous_merges_real. The label is
// stored as a JSON array (e.g. `["mills-canary","safe-fixture"]`), so the
// quoted-token LIKE pattern matches the exact label without snagging a
// superstring like "mills-canary-x".
func countRealMergedRunsSince(ctx context.Context, st *store.Store, since time.Time) (int, error) {
	canaryPattern := `%"` + store.CanaryLabel + `"%`
	row := st.DB().QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM pipeline_runs pr
		JOIN backlog_items bi ON bi.id = pr.backlog_id
		WHERE pr.state = ? AND pr.started_at >= ?
		  AND bi.labels_json NOT LIKE ?
	`, string(store.PipelineDone), kpiTime(since), canaryPattern)
	var n int
	if err := row.Scan(&n); err != nil {
		return 0, fmt.Errorf("kpi real merged count: %w", err)
	}
	return n, nil
}

func countGateOutcomesSince(ctx context.Context, st *store.Store, since time.Time) (passes, total int, err error) {
	// total counts pass + fail only; 'skip' outcomes (advisory/not-applicable
	// gates, e.g. a slice-less item's scope) are excluded from BOTH numerator
	// and denominator so a skip neither raises nor lowers gate_pass_rate —
	// matching adaptive.gateFailRate. Without this a skip would drag the rate
	// down exactly like a fail.
	row := st.DB().QueryRowContext(ctx, `
		SELECT
			COALESCE(SUM(CASE WHEN outcome = 'pass' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN outcome != 'skip' THEN 1 ELSE 0 END), 0)
		FROM gate_outcomes
		WHERE evaluated_at >= ?
	`, kpiTime(since))
	if err := row.Scan(&passes, &total); err != nil {
		return 0, 0, fmt.Errorf("kpi gate outcome count: %w", err)
	}
	return passes, total, nil
}

// retryStageCostSince sums stage_results.cost_usd for retry attempts (attempt
// > 1) across pipeline runs started at-or-after `since`. Joins on the run's
// started_at so the window matches the run-level cost metrics rather than each
// stage's own timestamp — the retry burn is attributed to the run's window.
func retryStageCostSince(ctx context.Context, st *store.Store, since time.Time) (float64, error) {
	row := st.DB().QueryRowContext(ctx, `
		SELECT COALESCE(SUM(sr.cost_usd), 0)
		FROM stage_results sr
		JOIN pipeline_runs pr ON pr.id = sr.pipeline_run_id
		WHERE pr.started_at >= ? AND sr.attempt > 1
	`, kpiTime(since))
	var cost float64
	if err := row.Scan(&cost); err != nil {
		return 0, fmt.Errorf("kpi retry-cost: %w", err)
	}
	return cost, nil
}

// countUnparseableGatesSince counts gate outcomes in the window that the LLM
// judge could not parse into a score envelope. Uses the same evaluated_at
// window as countGateOutcomesSince so it forms a valid ratio with the gate
// total.
func countUnparseableGatesSince(ctx context.Context, st *store.Store, since time.Time) (int, error) {
	row := st.DB().QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM gate_outcomes
		WHERE evaluated_at >= ? AND judged_by = ?
	`, kpiTime(since), store.JudgedByUnparseable)
	var n int
	if err := row.Scan(&n); err != nil {
		return 0, fmt.Errorf("kpi unparseable-gate: %w", err)
	}
	return n, nil
}

func avgEvalScoreSince(ctx context.Context, st *store.Store, since time.Time) (any, error) {
	row := st.DB().QueryRowContext(ctx, `
		SELECT AVG(score)
		FROM eval_scores
		WHERE evaluated_at >= ?
	`, kpiTime(since))
	var avg sql.NullFloat64
	if err := row.Scan(&avg); err != nil {
		return nil, fmt.Errorf("kpi eval average: %w", err)
	}
	if !avg.Valid {
		return nil, nil
	}
	return avg.Float64, nil
}

func kpiTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

// costPerMergedChange returns (sum_cost, distinct_merged_backlog_count)
// for the window. The sum is over pipeline_runs whose backlog ended
// up merged in the window — escalated/retried attempts of the same
// backlog count toward the same "change", but pipeline_runs whose
// backlog did NOT merge are excluded entirely.
//
// Caller divides; we return both values so the caller can omit the
// metric when denominator is zero.
func costPerMergedChange(ctx context.Context, st *store.Store, since time.Time) (float64, int, error) {
	row := st.DB().QueryRowContext(ctx, `
		SELECT
			COALESCE(SUM(cost_usd), 0),
			COUNT(DISTINCT backlog_id)
		FROM pipeline_runs
		WHERE backlog_id IN (
			SELECT DISTINCT backlog_id
			FROM pipeline_runs
			WHERE state = ? AND started_at >= ?
		)
		AND started_at >= ?
	`, string(store.PipelineDone), kpiTime(since), kpiTime(since))
	var cost float64
	var denom int
	if err := row.Scan(&cost, &denom); err != nil {
		return 0, 0, fmt.Errorf("kpi cost-per-change: %w", err)
	}
	return cost, denom, nil
}

// regressionRateFromLabels returns (regressions, merged_backlog_items)
// for the window. A "regression" is a distinct merged backlog item
// whose Labels JSON contains the canonical regressionFixLabel string.
// The match is a substring check on the JSON-encoded labels column —
// safe for the standard json.Marshal output of a []string, where each
// label is wrapped in quotes (so the match cannot collide with a
// label that contains regressionFixLabel as a suffix or prefix
// unless someone deliberately names a label like that).
//
// Caller divides; we return both values so the caller can omit the
// metric when denominator is zero.
func regressionRateFromLabels(ctx context.Context, st *store.Store, since time.Time) (int, int, error) {
	row := st.DB().QueryRowContext(ctx, `
		SELECT
			COUNT(DISTINCT CASE WHEN bi.labels_json LIKE ? THEN pr.backlog_id END),
			COUNT(DISTINCT pr.backlog_id)
		FROM pipeline_runs pr
		JOIN backlog_items bi ON pr.backlog_id = bi.id
		WHERE pr.state = ? AND pr.started_at >= ?
	`, "%\""+regressionFixLabel+"\"%", string(store.PipelineDone), kpiTime(since))
	var num, denom int
	if err := row.Scan(&num, &denom); err != nil {
		return 0, 0, fmt.Errorf("kpi regression-rate: %w", err)
	}
	return num, denom, nil
}
