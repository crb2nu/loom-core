package store

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// JudgedByUnparseable is the judged_by marker the LLM judge stamps on a gate
// outcome whose model response could not be parsed into a score envelope
// (pkg/mills/gates/llm_judge.go). The telemetry aggregation counts these
// separately so operators can distinguish judge-parse false-fails from genuine
// gate fails. Duplicated here rather than imported from pkg/mills/gates because
// gates imports store, so a store→gates dependency would be an import cycle.
const JudgedByUnparseable = "flexinfer:unparseable"

// TelemetryDAO aggregates stage / gate / run telemetry over a rolling window.
// It is a read-only view over stage_results + gate_outcomes + pipeline_runs;
// nothing here mutates state. Percentiles are computed in Go because SQLite has
// no built-in percentile function and the window is bounded (30d max).
type TelemetryDAO struct {
	db *sql.DB
}

// Telemetry returns a TelemetryDAO bound to the store's database handle. Kept as
// an accessor (rather than a struct field on Store) so this slice adds no lines
// to store.go.
func (s *Store) Telemetry() *TelemetryDAO {
	return &TelemetryDAO{db: s.db}
}

// StageTelemetry is the roll-up served by GET /api/mills/telemetry/stages. Field
// names are the JSON contract the HUD telemetry panel builds against; the
// handler wraps this with window_seconds + generated_at.
type StageTelemetry struct {
	Runs             RunTelemetry          `json:"runs"`
	Stages           []StageAgg            `json:"stages"`
	Gates            []GateAgg             `json:"gates"`
	EscalationFunnel []EscalationFunnelRow `json:"escalation_funnel"`
	FailureClasses   []FailureClassRow     `json:"failure_classes"`
	ModelEconomics   []ModelEconomicsRow   `json:"model_economics"`
}

// RunTelemetry is the window's run-level summary. RetryBurn* sum over stage
// attempts > 1 (the money and wall-clock spent re-running stages).
type RunTelemetry struct {
	Total            int     `json:"total"`
	Done             int     `json:"done"`
	Escalated        int     `json:"escalated"`
	RetryBurnCostUSD float64 `json:"retry_burn_cost_usd"`
	RetryBurnSeconds int64   `json:"retry_burn_seconds"`
}

// StageAgg is one stage's aggregate across every run in the window.
type StageAgg struct {
	Stage         string  `json:"stage"`
	Attempts      int     `json:"attempts"`
	Errors        int     `json:"errors"`
	ErrorRate     float64 `json:"error_rate"`
	P50Seconds    int64   `json:"p50_seconds"`
	P90Seconds    int64   `json:"p90_seconds"`
	MaxSeconds    int64   `json:"max_seconds"`
	TotalSeconds  int64   `json:"total_seconds"`
	CostUSD       float64 `json:"cost_usd"`
	RetryAttempts int     `json:"retry_attempts"`
	RetryCostUSD  float64 `json:"retry_cost_usd"`
}

// GateAgg is one gate's pass/fail/skip counts plus the unparseable judge-fail
// count (a subset of the outcomes, keyed on judged_by).
type GateAgg struct {
	Gate        string `json:"gate"`
	Evaluations int    `json:"evaluations"`
	Passes      int    `json:"passes"`
	Fails       int    `json:"fails"`
	Skips       int    `json:"skips"`
	Unparseable int    `json:"unparseable"`
}

// EscalationFunnelRow counts escalated runs by the (stage, outcome) of their
// latest stage attempt — the "where did the run die" view.
type EscalationFunnelRow struct {
	LastStage string `json:"last_stage"`
	Outcome   string `json:"outcome"`
	Count     int    `json:"count"`
}

// FailureClassRow counts error-outcome stage rows by (stage, needle-classified
// failure class).
type FailureClassRow struct {
	Stage string `json:"stage"`
	Class string `json:"class"`
	Count int    `json:"count"`
}

// ModelEconomicsRow attributes stage cost + reliability to one (model, backend)
// tier over the window: the economics question "what does each model tier cost
// and how reliable is it". Calls is stage attempts, CostUSD sums the recorded
// per-stage cost (provider-reported where the litellm/OpenRouter backend
// supplies it, else the flat local-tier estimate), ErrorRate is errors/calls,
// and AvgSeconds is the mean wall-clock over attempts with a measurable
// duration. Stage rows whose model/backend are unattributed (historical rows,
// or a worker that does not surface identity) bucket under "unknown"/"unknown"
// so the tier totals stay complete.
type ModelEconomicsRow struct {
	Model      string  `json:"model"`
	Backend    string  `json:"backend"`
	Calls      int     `json:"calls"`
	CostUSD    float64 `json:"cost_usd"`
	Errors     int     `json:"errors"`
	ErrorRate  float64 `json:"error_rate"`
	AvgSeconds int64   `json:"avg_seconds"`
}

// StageTelemetry aggregates every stage/gate/run row belonging to a pipeline run
// whose started_at is at-or-after `since`. The three sub-queries share that
// window predicate so the counts are internally consistent.
func (d *TelemetryDAO) StageTelemetry(ctx context.Context, since time.Time) (*StageTelemetry, error) {
	out := &StageTelemetry{
		Stages:           []StageAgg{},
		Gates:            []GateAgg{},
		EscalationFunnel: []EscalationFunnelRow{},
		FailureClasses:   []FailureClassRow{},
		ModelEconomics:   []ModelEconomicsRow{},
	}
	sinceStr := timeRFC3339(since)

	if err := d.aggregateRuns(ctx, sinceStr, out); err != nil {
		return nil, err
	}
	if err := d.aggregateStages(ctx, sinceStr, out); err != nil {
		return nil, err
	}
	if err := d.aggregateGates(ctx, sinceStr, out); err != nil {
		return nil, err
	}
	return out, nil
}

// aggregateRuns fills the total/done/escalated counts. Total is every run in the
// window regardless of state; done/escalated are the two terminal states the
// panel headlines.
func (d *TelemetryDAO) aggregateRuns(ctx context.Context, since string, out *StageTelemetry) error {
	rows, err := d.db.QueryContext(ctx, `
		SELECT state, COUNT(*)
		FROM pipeline_runs
		WHERE started_at >= ?
		GROUP BY state
	`, since)
	if err != nil {
		return fmt.Errorf("telemetry runs: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			state string
			n     int
		)
		if err := rows.Scan(&state, &n); err != nil {
			return fmt.Errorf("telemetry runs scan: %w", err)
		}
		out.Runs.Total += n
		switch PipelineState(state) {
		case PipelineDone:
			out.Runs.Done = n
		case PipelineEscalated:
			out.Runs.Escalated = n
		}
	}
	return rows.Err()
}

type stageAccumulator struct {
	attempts      int
	errors        int
	retryAttempts int
	durations     []int64
	costUSD       float64
	retryCostUSD  float64
}

// modelBackendKey groups stage rows by the (model, backend) tier that produced
// them. Blank identity folds into the "unknown" bucket (see modelBucket).
type modelBackendKey struct {
	model   string
	backend string
}

type modelEconAccumulator struct {
	calls     int
	errors    int
	costUSD   float64
	durations []int64 // whole-second durations for the rows that have one
}

type funnelKey struct {
	stage   string
	outcome string
}

type lastStageRow struct {
	stage   string
	outcome string
	state   string
}

// aggregateStages scans every stage row for a windowed run once and derives the
// per-stage aggregates, the retry-burn totals (attempt > 1), the failure-class
// histogram (error rows), and the escalation funnel (latest stage per escalated
// run). The rows arrive ordered by (run, started_at, id) so the last row seen
// for a run is its latest attempt.
func (d *TelemetryDAO) aggregateStages(ctx context.Context, since string, out *StageTelemetry) error {
	rows, err := d.db.QueryContext(ctx, `
		SELECT sr.pipeline_run_id, pr.state, sr.stage, sr.attempt,
		       sr.started_at, sr.ended_at, sr.outcome, sr.cost_usd,
		       sr.model, sr.backend, sr.log_tail
		FROM stage_results sr
		JOIN pipeline_runs pr ON pr.id = sr.pipeline_run_id
		WHERE pr.started_at >= ?
		ORDER BY sr.pipeline_run_id, sr.started_at, sr.id
	`, since)
	if err != nil {
		return fmt.Errorf("telemetry stages: %w", err)
	}
	defer rows.Close()

	stages := map[string]*stageAccumulator{}
	failClasses := map[string]map[string]int{}               // stage -> class -> count
	modelEcon := map[modelBackendKey]*modelEconAccumulator{} // (model,backend) -> agg
	lastByRun := map[string]*lastStageRow{}

	for rows.Next() {
		var (
			runID     string
			state     string
			stage     string
			attempt   int
			startedAt string
			endedAt   sql.NullString
			outcome   sql.NullString
			cost      float64
			model     sql.NullString
			backend   sql.NullString
			logTail   sql.NullString
		)
		if err := rows.Scan(&runID, &state, &stage, &attempt,
			&startedAt, &endedAt, &outcome, &cost, &model, &backend, &logTail); err != nil {
			return fmt.Errorf("telemetry stages scan: %w", err)
		}

		acc := stages[stage]
		if acc == nil {
			acc = &stageAccumulator{}
			stages[stage] = acc
		}
		acc.attempts++
		acc.costUSD += cost

		outStr := ""
		if outcome.Valid {
			outStr = outcome.String
		}
		isError := outStr == string(StageOutcomeError)
		if isError {
			acc.errors++
		}

		durSec, durOK := stageDurationSeconds(startedAt, endedAt)
		if durOK {
			acc.durations = append(acc.durations, durSec)
		}

		// Per-model economics accumulate off the same windowed scan so the
		// tier counts stay internally consistent with the per-stage aggregates.
		// A blank/NULL model or backend folds into the "unknown" bucket.
		mk := modelBackendKey{model: modelBucket(model), backend: modelBucket(backend)}
		mAcc := modelEcon[mk]
		if mAcc == nil {
			mAcc = &modelEconAccumulator{}
			modelEcon[mk] = mAcc
		}
		mAcc.calls++
		mAcc.costUSD += cost
		if isError {
			mAcc.errors++
		}
		if durOK {
			mAcc.durations = append(mAcc.durations, durSec)
		}

		if attempt > 1 {
			acc.retryAttempts++
			acc.retryCostUSD += cost
			out.Runs.RetryBurnCostUSD += cost
			if durOK {
				out.Runs.RetryBurnSeconds += durSec
			}
		}

		if isError {
			tail := ""
			if logTail.Valid {
				tail = logTail.String
			}
			class := classifyStageFailure(tail)
			byClass := failClasses[stage]
			if byClass == nil {
				byClass = map[string]int{}
				failClasses[stage] = byClass
			}
			byClass[class]++
		}

		lr := lastByRun[runID]
		if lr == nil {
			lr = &lastStageRow{}
			lastByRun[runID] = lr
		}
		lr.stage = stage
		lr.outcome = outStr
		lr.state = state
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("telemetry stages rows: %w", err)
	}

	out.Runs.RetryBurnCostUSD = round2(out.Runs.RetryBurnCostUSD)
	out.Stages = buildStageAggs(stages)
	out.FailureClasses = buildFailureClasses(failClasses)
	out.EscalationFunnel = buildEscalationFunnel(lastByRun)
	out.ModelEconomics = buildModelEconomics(modelEcon)
	return nil
}

// modelUnknownBucket is the label unattributed rows (blank model/backend)
// aggregate under so per-model totals stay complete.
const modelUnknownBucket = "unknown"

// modelBucket maps a nullable model/backend column to its economics bucket,
// folding NULL and blank values into "unknown".
func modelBucket(v sql.NullString) string {
	if !v.Valid {
		return modelUnknownBucket
	}
	s := strings.TrimSpace(v.String)
	if s == "" {
		return modelUnknownBucket
	}
	return s
}

// buildModelEconomics turns the (model, backend) accumulators into a slice
// sorted by cost descending (the money view the panel headlines), breaking ties
// by calls then model then backend so the ordering is deterministic. Never
// returns nil — an empty window yields []ModelEconomicsRow{} so the JSON array
// can never encode as null (which would crash the HUD effect tree).
func buildModelEconomics(econ map[modelBackendKey]*modelEconAccumulator) []ModelEconomicsRow {
	rows := make([]ModelEconomicsRow, 0, len(econ))
	for key, acc := range econ {
		errorRate := 0.0
		if acc.calls > 0 {
			errorRate = round3(float64(acc.errors) / float64(acc.calls))
		}
		var total int64
		for _, d := range acc.durations {
			total += d
		}
		var avg int64
		if n := len(acc.durations); n > 0 {
			avg = int64(math.Round(float64(total) / float64(n)))
		}
		rows = append(rows, ModelEconomicsRow{
			Model:      key.model,
			Backend:    key.backend,
			Calls:      acc.calls,
			CostUSD:    round2(acc.costUSD),
			Errors:     acc.errors,
			ErrorRate:  errorRate,
			AvgSeconds: avg,
		})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].CostUSD != rows[j].CostUSD {
			return rows[i].CostUSD > rows[j].CostUSD
		}
		if rows[i].Calls != rows[j].Calls {
			return rows[i].Calls > rows[j].Calls
		}
		if rows[i].Model != rows[j].Model {
			return rows[i].Model < rows[j].Model
		}
		return rows[i].Backend < rows[j].Backend
	})
	if len(rows) == 0 {
		return []ModelEconomicsRow{}
	}
	return rows
}

// aggregateGates counts gate outcomes for windowed runs by gate name, tracking
// the unparseable judge-fail subset separately.
func (d *TelemetryDAO) aggregateGates(ctx context.Context, since string, out *StageTelemetry) error {
	rows, err := d.db.QueryContext(ctx, `
		SELECT g.gate_name, g.outcome, g.judged_by
		FROM gate_outcomes g
		JOIN pipeline_runs pr ON pr.id = g.pipeline_run_id
		WHERE pr.started_at >= ?
	`, since)
	if err != nil {
		return fmt.Errorf("telemetry gates: %w", err)
	}
	defer rows.Close()

	type gateAccumulator struct {
		evaluations int
		passes      int
		fails       int
		skips       int
		unparseable int
	}
	gates := map[string]*gateAccumulator{}
	for rows.Next() {
		var gate, outcome, judgedBy string
		if err := rows.Scan(&gate, &outcome, &judgedBy); err != nil {
			return fmt.Errorf("telemetry gates scan: %w", err)
		}
		acc := gates[gate]
		if acc == nil {
			acc = &gateAccumulator{}
			gates[gate] = acc
		}
		acc.evaluations++
		switch GateOutcomeKind(outcome) {
		case GateOutcomePass:
			acc.passes++
		case GateOutcomeFail:
			acc.fails++
		case GateOutcomeSkip:
			acc.skips++
		}
		if judgedBy == JudgedByUnparseable {
			acc.unparseable++
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("telemetry gates rows: %w", err)
	}

	names := make([]string, 0, len(gates))
	for name := range gates {
		names = append(names, name)
	}
	sort.Strings(names)
	agg := make([]GateAgg, 0, len(names))
	for _, name := range names {
		a := gates[name]
		agg = append(agg, GateAgg{
			Gate:        name,
			Evaluations: a.evaluations,
			Passes:      a.passes,
			Fails:       a.fails,
			Skips:       a.skips,
			Unparseable: a.unparseable,
		})
	}
	out.Gates = agg
	return nil
}

// buildStageAggs turns the stage accumulators into a stable, alphabetically-
// ordered slice with percentiles computed in Go.
func buildStageAggs(stages map[string]*stageAccumulator) []StageAgg {
	names := make([]string, 0, len(stages))
	for name := range stages {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]StageAgg, 0, len(names))
	for _, name := range names {
		acc := stages[name]
		sort.Slice(acc.durations, func(i, j int) bool { return acc.durations[i] < acc.durations[j] })
		var total int64
		for _, d := range acc.durations {
			total += d
		}
		errorRate := 0.0
		if acc.attempts > 0 {
			errorRate = round3(float64(acc.errors) / float64(acc.attempts))
		}
		out = append(out, StageAgg{
			Stage:         name,
			Attempts:      acc.attempts,
			Errors:        acc.errors,
			ErrorRate:     errorRate,
			P50Seconds:    percentile(acc.durations, 50),
			P90Seconds:    percentile(acc.durations, 90),
			MaxSeconds:    maxInt64(acc.durations),
			TotalSeconds:  total,
			CostUSD:       round2(acc.costUSD),
			RetryAttempts: acc.retryAttempts,
			RetryCostUSD:  round2(acc.retryCostUSD),
		})
	}
	if len(out) == 0 {
		return []StageAgg{}
	}
	return out
}

// buildFailureClasses flattens the stage→class histogram, ordered by count
// descending then stage then class so the output is deterministic.
func buildFailureClasses(failClasses map[string]map[string]int) []FailureClassRow {
	rows := make([]FailureClassRow, 0)
	for stage, byClass := range failClasses {
		for class, count := range byClass {
			rows = append(rows, FailureClassRow{Stage: stage, Class: class, Count: count})
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Count != rows[j].Count {
			return rows[i].Count > rows[j].Count
		}
		if rows[i].Stage != rows[j].Stage {
			return rows[i].Stage < rows[j].Stage
		}
		return rows[i].Class < rows[j].Class
	})
	if len(rows) == 0 {
		return []FailureClassRow{}
	}
	return rows
}

// buildEscalationFunnel counts each escalated run's latest (stage, outcome),
// mapping a NULL outcome to "none". Ordered by count descending then stage then
// outcome for a deterministic result.
func buildEscalationFunnel(lastByRun map[string]*lastStageRow) []EscalationFunnelRow {
	counts := map[funnelKey]int{}
	for _, lr := range lastByRun {
		if PipelineState(lr.state) != PipelineEscalated {
			continue
		}
		outcome := lr.outcome
		if outcome == "" {
			outcome = "none"
		}
		counts[funnelKey{stage: lr.stage, outcome: outcome}]++
	}
	rows := make([]EscalationFunnelRow, 0, len(counts))
	for key, count := range counts {
		rows = append(rows, EscalationFunnelRow{LastStage: key.stage, Outcome: key.outcome, Count: count})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Count != rows[j].Count {
			return rows[i].Count > rows[j].Count
		}
		if rows[i].LastStage != rows[j].LastStage {
			return rows[i].LastStage < rows[j].LastStage
		}
		return rows[i].Outcome < rows[j].Outcome
	})
	if len(rows) == 0 {
		return []EscalationFunnelRow{}
	}
	return rows
}

// failureClassNeedle maps a telemetry failure class to the case-folded
// substrings that identify it in a stage's log tail. Order is significant: the
// first class with a matching needle wins, so more-specific classes precede
// generic ones. The taxonomy is telemetry-panel-specific (distinct from
// pkg/mills/pipeline's ErrorClass), and lives in the store package to avoid a
// store→pipeline import cycle.
type failureClassNeedle struct {
	class   string
	needles []string
}

var failureClassNeedles = []failureClassNeedle{
	{class: "model_unavailable", needles: []string{"service_unavailable", "is parked behind"}},
	// "exited 124"/"command timed out" is the agent CLI killed by the spawn
	// exec deadline; "deadline exceeded during reconciliation" is the spawn
	// controller expiring a wedged spawn (internal/spawn/controller.go). Both
	// are spawn-layer wedges, not code failures: live 2026-07-19 telemetry had
	// 29 plan_slice rows in "other" that were all one of these two shapes
	// (codex 0.143.0 gpt-5.6 version-gate hang, escalations #356-#359 class).
	{class: "spawn_infra", needles: []string{"hud spawn", "image build failed", "pod creation failed", "turn driver lost", "exited 124", "exited 143", "command timed out", "deadline exceeded during reconciliation", "no agent output for"}},
	{class: "ci_poll_timeout", needles: []string{"poll deadline exceeded", "poll timed out"}},
	{class: "gitlab_api", needles: []string{"gitlab: post", "gitlab: put", "gitlab: delete"}},
	{class: "devbox_gate_empty", needles: []string{"0/0 checks"}},
}

// classifyStageFailure maps a stage's log tail to a failure class via
// first-match substring needles, falling back to "other".
func classifyStageFailure(logTail string) string {
	lower := strings.ToLower(logTail)
	for _, fc := range failureClassNeedles {
		for _, needle := range fc.needles {
			if strings.Contains(lower, needle) {
				return fc.class
			}
		}
	}
	return "other"
}

// stageDurationSeconds returns the stage's wall-clock in whole seconds (rounded)
// and true when both timestamps are present and ended is not before started.
// Rows missing either timestamp — or with a negative span from clock skew — are
// skipped from the duration aggregates.
func stageDurationSeconds(startedAt string, endedAt sql.NullString) (int64, bool) {
	if !endedAt.Valid || endedAt.String == "" {
		return 0, false
	}
	start, err := parseTime(startedAt)
	if err != nil {
		return 0, false
	}
	end, err := parseTime(endedAt.String)
	if err != nil {
		return 0, false
	}
	if end.Before(start) {
		return 0, false
	}
	return int64(math.Round(end.Sub(start).Seconds())), true
}

// percentile returns the nearest-rank pth percentile of a sorted ascending
// slice. Returns 0 for an empty slice.
func percentile(sorted []int64, p float64) int64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	rank := int(math.Ceil(p / 100 * float64(n)))
	if rank < 1 {
		rank = 1
	}
	if rank > n {
		rank = n
	}
	return sorted[rank-1]
}

func maxInt64(sorted []int64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	return sorted[len(sorted)-1]
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }
func round3(v float64) float64 { return math.Round(v*1000) / 1000 }
