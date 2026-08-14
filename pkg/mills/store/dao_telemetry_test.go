package store

import (
	"context"
	"testing"
	"time"
)

// telemetrySeed is a compact stage-row spec for the aggregation tests.
type telemetrySeed struct {
	stage   string
	attempt int
	dur     time.Duration
	outcome StageOutcome // "" ⇒ NULL outcome
	cost    float64
	logTail string
	model   string // "" ⇒ NULL model (aggregates as "unknown")
	backend string // "" ⇒ NULL backend
}

// seedTelemetryRun writes a pipeline run + its stage rows. startedAt anchors the
// run window; each stage row is offset a little so ORDER BY started_at is stable.
func seedTelemetryRun(t *testing.T, st *Store, backlogID, runID string, attempt int, state PipelineState, startedAt time.Time, stages []telemetrySeed) {
	t.Helper()
	ctx := context.Background()
	if err := st.Backlog.Put(ctx, &BacklogItem{
		ID: backlogID, Title: backlogID, State: BacklogRunning, Priority: P2, CreatedBy: "test",
	}); err != nil {
		t.Fatalf("seed backlog %s: %v", backlogID, err)
	}
	if err := st.Pipeline.PutRun(ctx, &PipelineRun{
		ID: runID, BacklogID: backlogID, Template: "t", State: state,
		Attempts: attempt, StartedAt: startedAt,
	}); err != nil {
		t.Fatalf("seed run %s: %v", runID, err)
	}
	for i, s := range stages {
		start := startedAt.Add(time.Duration(i) * time.Minute)
		sr := &StageResult{
			PipelineRunID: runID,
			Stage:         s.stage,
			Attempt:       s.attempt,
			StartedAt:     start,
			CostUSD:       s.cost,
			LogTail:       s.logTail,
			Model:         s.model,
			Backend:       s.backend,
		}
		if s.dur > 0 {
			end := start.Add(s.dur)
			sr.EndedAt = &end
		}
		if s.outcome != "" {
			oc := s.outcome
			sr.Outcome = &oc
		}
		if err := st.Pipeline.PutStage(ctx, sr); err != nil {
			t.Fatalf("seed stage %s/%s a%d: %v", runID, s.stage, s.attempt, err)
		}
	}
}

func findStage(t *testing.T, tel *StageTelemetry, name string) StageAgg {
	t.Helper()
	for _, s := range tel.Stages {
		if s.Stage == name {
			return s
		}
	}
	t.Fatalf("stage %q not in telemetry: %+v", name, tel.Stages)
	return StageAgg{}
}

func findGate(t *testing.T, tel *StageTelemetry, name string) GateAgg {
	t.Helper()
	for _, g := range tel.Gates {
		if g.Gate == name {
			return g
		}
	}
	t.Fatalf("gate %q not in telemetry: %+v", name, tel.Gates)
	return GateAgg{}
}

// TestTelemetry_StageAggregatesAndWindow exercises per-stage attempts/errors/
// error_rate/percentiles/cost/retry, plus the window boundary and retry-burn.
func TestTelemetry_StageAggregatesAndWindow(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	since := now.Add(-7 * 24 * time.Hour)

	// In-window run: implement stage with 5 attempts, durations 10/20/30/40/50s.
	// Attempts 2..5 are retries. One error outcome. Costs sum cleanly.
	seedTelemetryRun(t, st, "BACK-IN", "RUN-IN", 1, PipelineDone, now.Add(-2*time.Hour), []telemetrySeed{
		{stage: "implement", attempt: 1, dur: 10 * time.Second, outcome: StageOutcomeSuccess, cost: 1.00},
		{stage: "implement", attempt: 2, dur: 20 * time.Second, outcome: StageOutcomeError, cost: 2.00, logTail: "hud spawn: POST max retries exceeded"},
		{stage: "implement", attempt: 3, dur: 30 * time.Second, outcome: StageOutcomeSuccess, cost: 3.00},
		{stage: "implement", attempt: 4, dur: 40 * time.Second, outcome: StageOutcomeSuccess, cost: 4.00},
		{stage: "implement", attempt: 5, dur: 50 * time.Second, outcome: StageOutcomeSuccess, cost: 5.00},
	})

	// Out-of-window run (10 days old): must not contribute.
	seedTelemetryRun(t, st, "BACK-OLD", "RUN-OLD", 1, PipelineDone, now.Add(-10*24*time.Hour), []telemetrySeed{
		{stage: "implement", attempt: 1, dur: 999 * time.Second, outcome: StageOutcomeError, cost: 99.00, logTail: "hud spawn"},
		{stage: "implement", attempt: 2, dur: 999 * time.Second, outcome: StageOutcomeError, cost: 99.00},
	})

	tel, err := st.Telemetry().StageTelemetry(ctx, since)
	if err != nil {
		t.Fatalf("StageTelemetry: %v", err)
	}

	impl := findStage(t, tel, "implement")
	if impl.Attempts != 5 {
		t.Errorf("attempts = %d, want 5 (old run excluded)", impl.Attempts)
	}
	if impl.Errors != 1 {
		t.Errorf("errors = %d, want 1", impl.Errors)
	}
	if impl.ErrorRate != 0.2 {
		t.Errorf("error_rate = %v, want 0.2", impl.ErrorRate)
	}
	// Sorted durations [10,20,30,40,50]: nearest-rank p50 index=ceil(2.5)-1=2 →30,
	// p90 index=ceil(4.5)-1=4 →50, max 50, total 150.
	if impl.P50Seconds != 30 {
		t.Errorf("p50 = %d, want 30", impl.P50Seconds)
	}
	if impl.P90Seconds != 50 {
		t.Errorf("p90 = %d, want 50", impl.P90Seconds)
	}
	if impl.MaxSeconds != 50 {
		t.Errorf("max = %d, want 50", impl.MaxSeconds)
	}
	if impl.TotalSeconds != 150 {
		t.Errorf("total = %d, want 150", impl.TotalSeconds)
	}
	if impl.CostUSD != 15.00 {
		t.Errorf("cost_usd = %v, want 15.00", impl.CostUSD)
	}
	if impl.RetryAttempts != 4 {
		t.Errorf("retry_attempts = %d, want 4", impl.RetryAttempts)
	}
	if impl.RetryCostUSD != 14.00 {
		t.Errorf("retry_cost_usd = %v, want 14.00 (attempts 2..5)", impl.RetryCostUSD)
	}

	// Runs block: only the in-window run counts; retry burn is attempts 2..5.
	if tel.Runs.Total != 1 || tel.Runs.Done != 1 || tel.Runs.Escalated != 0 {
		t.Errorf("runs = %+v, want total=1 done=1 escalated=0", tel.Runs)
	}
	if tel.Runs.RetryBurnCostUSD != 14.00 {
		t.Errorf("retry_burn_cost_usd = %v, want 14.00", tel.Runs.RetryBurnCostUSD)
	}
	if tel.Runs.RetryBurnSeconds != 140 {
		t.Errorf("retry_burn_seconds = %d, want 140 (20+30+40+50)", tel.Runs.RetryBurnSeconds)
	}
}

// TestTelemetry_DurationSkipsRowsMissingTimestamps proves rows without an
// ended_at are excluded from the duration aggregates but still counted as
// attempts.
func TestTelemetry_DurationSkipsRowsMissingTimestamps(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	seedTelemetryRun(t, st, "BACK-DUR", "RUN-DUR", 1, PipelineDone, now.Add(-time.Hour), []telemetrySeed{
		{stage: "research", attempt: 1, dur: 40 * time.Second, outcome: StageOutcomeSuccess, cost: 0},
		// No dur ⇒ pending row with NULL ended_at: counts as an attempt but not a duration.
		{stage: "research", attempt: 2, outcome: StageOutcomeSuccess, cost: 0},
	})

	tel, err := st.Telemetry().StageTelemetry(ctx, now.Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("StageTelemetry: %v", err)
	}
	res := findStage(t, tel, "research")
	if res.Attempts != 2 {
		t.Errorf("attempts = %d, want 2", res.Attempts)
	}
	// Only the 40s row contributes to durations.
	if res.P50Seconds != 40 || res.MaxSeconds != 40 || res.TotalSeconds != 40 {
		t.Errorf("durations = p50 %d max %d total %d, want 40/40/40", res.P50Seconds, res.MaxSeconds, res.TotalSeconds)
	}
}

// TestTelemetry_GatesAndUnparseable pins the gate pass/fail/skip counts and the
// separate unparseable count keyed on judged_by.
func TestTelemetry_GatesAndUnparseable(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	if err := st.Backlog.Put(ctx, &BacklogItem{
		ID: "BACK-G", Title: "g", State: BacklogRunning, Priority: P2, CreatedBy: "test",
	}); err != nil {
		t.Fatalf("seed backlog: %v", err)
	}
	if err := st.Pipeline.PutRun(ctx, &PipelineRun{
		ID: "RUN-G", BacklogID: "BACK-G", Template: "t", State: PipelineEscalated,
		Attempts: 1, StartedAt: now.Add(-time.Hour),
	}); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	// A run whose started_at is outside the window; its gate must not count.
	if err := st.Backlog.Put(ctx, &BacklogItem{
		ID: "BACK-GOLD", Title: "gold", State: BacklogRunning, Priority: P2, CreatedBy: "test",
	}); err != nil {
		t.Fatalf("seed old backlog: %v", err)
	}
	if err := st.Pipeline.PutRun(ctx, &PipelineRun{
		ID: "RUN-GOLD", BacklogID: "BACK-GOLD", Template: "t", State: PipelineDone,
		Attempts: 1, StartedAt: now.Add(-10 * 24 * time.Hour),
	}); err != nil {
		t.Fatalf("seed old run: %v", err)
	}

	seedGate := func(runID, gate string, outcome GateOutcomeKind, judgedBy string) {
		t.Helper()
		if err := st.Pipeline.PutGate(ctx, &GateOutcome{
			PipelineRunID: runID, AfterStage: "pr_self_review", GateName: gate,
			Outcome: outcome, JudgedBy: judgedBy, EvaluatedAt: now.Add(-30 * time.Minute),
		}); err != nil {
			t.Fatalf("seed gate %s/%s: %v", runID, gate, err)
		}
	}
	// pr_self_review: 3 pass, 2 fail (1 of them unparseable), on the in-window run.
	seedGate("RUN-G", "pr_self_review", GateOutcomePass, "flexinfer:qwen")
	seedGate("RUN-G", "pr_self_review", GateOutcomePass, "flexinfer:qwen")
	seedGate("RUN-G", "pr_self_review", GateOutcomePass, "flexinfer:qwen")
	seedGate("RUN-G", "pr_self_review", GateOutcomeFail, "flexinfer:qwen")
	seedGate("RUN-G", "pr_self_review", GateOutcomeFail, JudgedByUnparseable)
	// scope: 1 skip.
	seedGate("RUN-G", "scope", GateOutcomeSkip, "go")
	// Out-of-window gate must be ignored.
	seedGate("RUN-GOLD", "pr_self_review", GateOutcomeFail, JudgedByUnparseable)

	tel, err := st.Telemetry().StageTelemetry(ctx, now.Add(-7*24*time.Hour))
	if err != nil {
		t.Fatalf("StageTelemetry: %v", err)
	}

	pr := findGate(t, tel, "pr_self_review")
	if pr.Evaluations != 5 {
		t.Errorf("evaluations = %d, want 5 (out-of-window gate excluded)", pr.Evaluations)
	}
	if pr.Passes != 3 || pr.Fails != 2 || pr.Skips != 0 {
		t.Errorf("pass/fail/skip = %d/%d/%d, want 3/2/0", pr.Passes, pr.Fails, pr.Skips)
	}
	if pr.Unparseable != 1 {
		t.Errorf("unparseable = %d, want 1", pr.Unparseable)
	}
	scope := findGate(t, tel, "scope")
	if scope.Skips != 1 || scope.Evaluations != 1 {
		t.Errorf("scope = %+v, want 1 eval / 1 skip", scope)
	}
	// Gates are sorted alphabetically.
	if len(tel.Gates) != 2 || tel.Gates[0].Gate != "pr_self_review" || tel.Gates[1].Gate != "scope" {
		t.Errorf("gate order = %+v, want [pr_self_review scope]", tel.Gates)
	}
}

// TestTelemetry_EscalationFunnelAndFailureClasses proves the funnel keys off the
// latest stage of each escalated run (NULL outcome ⇒ "none"), and that failure
// classes come from log-tail needle classification of error rows.
func TestTelemetry_EscalationFunnelAndFailureClasses(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	since := now.Add(-7 * 24 * time.Hour)

	// Escalated run A: last stage ci_watch/error (poll timed out ⇒ ci_poll_timeout).
	seedTelemetryRun(t, st, "BACK-A", "RUN-A", 1, PipelineEscalated, now.Add(-3*time.Hour), []telemetrySeed{
		{stage: "implement", attempt: 1, dur: 60 * time.Second, outcome: StageOutcomeSuccess, cost: 1},
		{stage: "ci_watch", attempt: 1, dur: 100 * time.Second, outcome: StageOutcomeError, cost: 0, logTail: "gitlab: pipeline poll timed out after 30m0s"},
	})
	// Escalated run B: last stage ci_watch/error too (same key ⇒ count 2).
	seedTelemetryRun(t, st, "BACK-B", "RUN-B", 1, PipelineEscalated, now.Add(-2*time.Hour), []telemetrySeed{
		{stage: "ci_watch", attempt: 1, dur: 90 * time.Second, outcome: StageOutcomeError, cost: 0, logTail: "pipeline poll timed out after 30m0s"},
	})
	// Escalated run C: last stage pr_self_review with a NULL outcome ⇒ "none".
	seedTelemetryRun(t, st, "BACK-C", "RUN-C", 1, PipelineEscalated, now.Add(-90*time.Minute), []telemetrySeed{
		{stage: "implement", attempt: 1, dur: 30 * time.Second, outcome: StageOutcomeSuccess, cost: 1},
		{stage: "pr_self_review", attempt: 1, dur: 10 * time.Second, outcome: "", cost: 0},
	})
	// A DONE run: contributes error rows to failure_classes but NOT to the funnel.
	seedTelemetryRun(t, st, "BACK-D", "RUN-D", 1, PipelineDone, now.Add(-time.Hour), []telemetrySeed{
		{stage: "research", attempt: 1, dur: 20 * time.Second, outcome: StageOutcomeError, cost: 0, logTail: "flexinfer chat: status 503 service_unavailable"},
		{stage: "research", attempt: 2, dur: 20 * time.Second, outcome: StageOutcomeError, cost: 0, logTail: "model is parked behind higher-priority primary"},
		{stage: "tests", attempt: 1, dur: 5 * time.Second, outcome: StageOutcomeError, cost: 0, logTail: "devbox quality gate failed (0/0 checks marked failed)"},
		{stage: "cleanup", attempt: 1, dur: 1 * time.Second, outcome: StageOutcomeError, cost: 0, logTail: "gitlab: DELETE /projects/1/branches/x: status 400"},
		{stage: "merge", attempt: 1, dur: 2 * time.Second, outcome: StageOutcomeError, cost: 0, logTail: "some novel failure with no known needle"},
	})

	tel, err := st.Telemetry().StageTelemetry(ctx, since)
	if err != nil {
		t.Fatalf("StageTelemetry: %v", err)
	}

	// Funnel: ci_watch/error count 2 (runs A+B), pr_self_review/none count 1 (run C).
	// The DONE run D never appears. Ordered by count desc.
	if len(tel.EscalationFunnel) != 2 {
		t.Fatalf("funnel len = %d, want 2: %+v", len(tel.EscalationFunnel), tel.EscalationFunnel)
	}
	top := tel.EscalationFunnel[0]
	if top.LastStage != "ci_watch" || top.Outcome != "error" || top.Count != 2 {
		t.Errorf("funnel[0] = %+v, want ci_watch/error/2", top)
	}
	second := tel.EscalationFunnel[1]
	if second.LastStage != "pr_self_review" || second.Outcome != "none" || second.Count != 1 {
		t.Errorf("funnel[1] = %+v, want pr_self_review/none/1", second)
	}

	// Failure classes: needle classification of every error row across the window.
	fc := map[string]FailureClassRow{}
	for _, row := range tel.FailureClasses {
		fc[row.Stage+"/"+row.Class] = row
	}
	want := map[string]int{
		"research/model_unavailable": 2, // both research errors (503 + parked)
		"ci_watch/ci_poll_timeout":   2, // runs A+B
		"tests/devbox_gate_empty":    1,
		"cleanup/gitlab_api":         1,
		"merge/other":                1,
	}
	for key, count := range want {
		got, ok := fc[key]
		if !ok {
			t.Errorf("failure_classes missing %q: %+v", key, tel.FailureClasses)
			continue
		}
		if got.Count != count {
			t.Errorf("failure_classes[%q].count = %d, want %d", key, got.Count, count)
		}
	}
	// Deterministic order: count desc, then stage asc, then class asc. The two
	// count-2 rows come first, sorted by stage (ci_watch before research).
	if len(tel.FailureClasses) < 2 {
		t.Fatalf("failure_classes len = %d, want >=2", len(tel.FailureClasses))
	}
	if tel.FailureClasses[0].Stage != "ci_watch" || tel.FailureClasses[1].Stage != "research" {
		t.Errorf("failure_classes order = %+v, want ci_watch then research first", tel.FailureClasses[:2])
	}
}

// TestTelemetry_EmptyStoreReturnsEmptyArrays guards the never-null contract: an
// empty store yields empty slices, not nil (which would marshal as JSON null).
func TestTelemetry_EmptyStoreReturnsEmptyArrays(t *testing.T) {
	st := newTestStore(t)
	tel, err := st.Telemetry().StageTelemetry(context.Background(), time.Now().Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("StageTelemetry: %v", err)
	}
	if tel.Stages == nil || tel.Gates == nil || tel.EscalationFunnel == nil || tel.FailureClasses == nil {
		t.Fatalf("empty telemetry has a nil slice (must be []): %+v", tel)
	}
	if len(tel.Stages) != 0 || len(tel.Gates) != 0 || len(tel.EscalationFunnel) != 0 || len(tel.FailureClasses) != 0 {
		t.Errorf("expected all-empty telemetry, got %+v", tel)
	}
	if tel.Runs.Total != 0 {
		t.Errorf("runs.total = %d, want 0", tel.Runs.Total)
	}
}

func TestClassifyStageFailure(t *testing.T) {
	cases := []struct {
		log  string
		want string
	}{
		{"flexinfer chat: status 503 service_unavailable", "model_unavailable"},
		{"qwen35 is parked behind higher-priority primary", "model_unavailable"},
		{"hud spawn: POST max retries exceeded", "spawn_infra"},
		{"image build failed: buildah", "spawn_infra"},
		{"pod creation failed", "spawn_infra"},
		{"agent turn driver lost across restart", "spawn_infra"},
		// Live 2026-07-19 plan_slice "other" shapes (codex 0.143.0 gpt-5.6
		// version-gate wedge): exec-deadline kill and controller expiry.
		{"stage=plan_slice attempt=1 spawn=spawn-x: error=agent CLI exited 124 (no stderr; stdout: command timed out)", "spawn_infra"},
		{"stage=plan_slice attempt=2 spawn=spawn-x: spawn deadline exceeded during reconciliation", "spawn_infra"},
		{"spawn spawn-x stalled: no agent output for 15m0s (>= 15m0s stall timeout)", "spawn_infra"},
		{"pipeline poll timed out after 30m0s", "ci_poll_timeout"},
		{"spawn: poll deadline exceeded", "ci_poll_timeout"},
		{"gitlab: POST /projects: status 409", "gitlab_api"},
		{"gitlab: PUT /merge: status 422", "gitlab_api"},
		{"gitlab: DELETE /branch: status 400", "gitlab_api"},
		{"devbox quality gate failed (0/0 checks marked failed)", "devbox_gate_empty"},
		{"something entirely different", "other"},
		{"", "other"},
	}
	for _, c := range cases {
		if got := classifyStageFailure(c.log); got != c.want {
			t.Errorf("classifyStageFailure(%q) = %q, want %q", c.log, got, c.want)
		}
	}
}

func TestTelemetryPercentile(t *testing.T) {
	cases := []struct {
		sorted []int64
		p      float64
		want   int64
	}{
		{nil, 50, 0},
		{[]int64{7}, 50, 7},
		{[]int64{7}, 90, 7},
		{[]int64{10, 20, 30, 40, 50}, 50, 30},
		{[]int64{10, 20, 30, 40, 50}, 90, 50},
		{[]int64{10, 20, 30, 40}, 50, 20}, // ceil(2)=2 → index 1
		{[]int64{10, 20, 30, 40}, 90, 40}, // ceil(3.6)=4 → index 3
		{[]int64{5, 5, 5, 5}, 50, 5},
	}
	for _, c := range cases {
		if got := percentile(c.sorted, c.p); got != c.want {
			t.Errorf("percentile(%v, %v) = %d, want %d", c.sorted, c.p, got, c.want)
		}
	}
}

func findModelEcon(t *testing.T, tel *StageTelemetry, model, backend string) ModelEconomicsRow {
	t.Helper()
	for _, m := range tel.ModelEconomics {
		if m.Model == model && m.Backend == backend {
			return m
		}
	}
	t.Fatalf("model economics (%q,%q) not in telemetry: %+v", model, backend, tel.ModelEconomics)
	return ModelEconomicsRow{}
}

// TestTelemetry_ModelEconomics exercises the per-(model,backend) roll-up: calls,
// summed cost, error count/rate, avg duration, the "unknown" bucket for
// unattributed rows, the window boundary, and cost-descending ordering.
func TestTelemetry_ModelEconomics(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	since := now.Add(-7 * 24 * time.Hour)

	// In-window run mixing three tiers plus an unattributed row:
	//  - (qwen, flexinfer): 2 calls, durations 10/30s, costs 0.01+0.03, 1 error
	//  - (claude, spawn):   1 call,  duration 100s,     cost 5.00,      0 errors
	//  - unattributed:      1 call,  duration 20s,      cost 0.00,      0 errors
	seedTelemetryRun(t, st, "BACK-ME", "RUN-ME", 1, PipelineDone, now.Add(-3*time.Hour), []telemetrySeed{
		{stage: "research", attempt: 1, dur: 10 * time.Second, outcome: StageOutcomeError, cost: 0.01, model: "qwen", backend: "flexinfer"},
		{stage: "research", attempt: 2, dur: 30 * time.Second, outcome: StageOutcomeSuccess, cost: 0.03, model: "qwen", backend: "flexinfer"},
		{stage: "implement", attempt: 1, dur: 100 * time.Second, outcome: StageOutcomeSuccess, cost: 5.00, model: "claude", backend: "spawn"},
		{stage: "cleanup", attempt: 1, dur: 20 * time.Second, outcome: StageOutcomeSuccess, cost: 0.00},
	})

	// Out-of-window run must not contribute.
	seedTelemetryRun(t, st, "BACK-ME-OLD", "RUN-ME-OLD", 1, PipelineDone, now.Add(-10*24*time.Hour), []telemetrySeed{
		{stage: "implement", attempt: 1, dur: 999 * time.Second, outcome: StageOutcomeError, cost: 99.00, model: "claude", backend: "spawn"},
	})

	tel, err := st.Telemetry().StageTelemetry(ctx, since)
	if err != nil {
		t.Fatalf("StageTelemetry: %v", err)
	}

	if len(tel.ModelEconomics) != 3 {
		t.Fatalf("model economics rows = %d, want 3: %+v", len(tel.ModelEconomics), tel.ModelEconomics)
	}

	// Cost-descending order: claude/spawn (5.00) > qwen/flexinfer (0.04) > unknown (0).
	if got := tel.ModelEconomics[0]; got.Model != "claude" || got.Backend != "spawn" {
		t.Errorf("row[0] = %+v, want claude/spawn first (highest cost)", got)
	}
	if got := tel.ModelEconomics[2]; got.Model != "unknown" || got.Backend != "unknown" {
		t.Errorf("row[2] = %+v, want unknown/unknown last", got)
	}

	qwen := findModelEcon(t, tel, "qwen", "flexinfer")
	if qwen.Calls != 2 || qwen.Errors != 1 {
		t.Errorf("qwen calls/errors = %d/%d, want 2/1", qwen.Calls, qwen.Errors)
	}
	if qwen.ErrorRate != 0.5 {
		t.Errorf("qwen error_rate = %v, want 0.5", qwen.ErrorRate)
	}
	if qwen.CostUSD != 0.04 {
		t.Errorf("qwen cost = %v, want 0.04", qwen.CostUSD)
	}
	if qwen.AvgSeconds != 20 { // mean(10,30)
		t.Errorf("qwen avg_seconds = %d, want 20", qwen.AvgSeconds)
	}

	claude := findModelEcon(t, tel, "claude", "spawn")
	if claude.Calls != 1 || claude.Errors != 0 || claude.CostUSD != 5.00 || claude.AvgSeconds != 100 {
		t.Errorf("claude = %+v, want 1 call / 0 err / 5.00 / 100s (old run excluded)", claude)
	}

	unknown := findModelEcon(t, tel, "unknown", "unknown")
	if unknown.Calls != 1 || unknown.CostUSD != 0.00 {
		t.Errorf("unknown bucket = %+v, want 1 call / 0 cost", unknown)
	}
}

// TestTelemetry_ModelEconomicsEmptyWindow guards the never-null contract: an
// empty window yields a non-nil empty slice, not nil.
func TestTelemetry_ModelEconomicsEmptyWindow(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tel, err := st.Telemetry().StageTelemetry(ctx, time.Now().UTC().Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("StageTelemetry: %v", err)
	}
	if tel.ModelEconomics == nil {
		t.Fatal("ModelEconomics is nil; must be an empty slice for the JSON never-null contract")
	}
	if len(tel.ModelEconomics) != 0 {
		t.Errorf("ModelEconomics = %+v, want empty", tel.ModelEconomics)
	}
}
