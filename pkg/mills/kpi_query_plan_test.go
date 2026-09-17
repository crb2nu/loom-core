package mills

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/store"
)

// A timestamp/run lookup is not enough: stage logs and gate reasons can span
// many pages. KPI aggregates must stay on covering indexes under a cold cache.
func TestKPIHistoryQueriesAvoidPayloadReads(t *testing.T) {
	env := newRecEnv(t, nil)
	for _, tc := range []struct {
		name, query, index string
		args               []any
	}{
		{"gate outcomes", kpiGateOutcomesQuery, "idx_gate_outcomes_evaluated", []any{kpiTime(env.now.Add(-kpiWindow30d))}},
		{"unparseable gates", kpiUnparseableGatesQuery, "idx_gate_outcomes_evaluated", []any{kpiTime(env.now.Add(-kpiWindow30d)), store.JudgedByUnparseable}},
		{"retry costs", kpiRetryCostQuery, "idx_stage_retry_cost", []any{kpiTime(env.now.Add(-kpiWindow30d))}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := env.store.DB().QueryContext(context.Background(), "EXPLAIN QUERY PLAN "+tc.query, tc.args...)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			var details []string
			for rows.Next() {
				var id, parent, unused int
				var detail string
				if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
					t.Fatal(err)
				}
				details = append(details, detail)
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			plan := strings.Join(details, "\n")
			if !strings.Contains(plan, "USING COVERING INDEX "+tc.index) {
				t.Fatalf("KPI history query must avoid payload-table reads:\n%s", plan)
			}
		})
	}
}

func TestKPIHistoryWindowsPreserveAccounting(t *testing.T) {
	env := newRecEnv(t, nil)
	ctx := context.Background()
	since := env.now.Add(-kpiWindow1d).Add(123456789)
	for i, started := range []time.Time{since.Add(-time.Nanosecond), since, since.Add(time.Nanosecond)} {
		id := fmt.Sprintf("kpi-window-%d", i)
		if err := env.store.Backlog.Put(ctx, &store.BacklogItem{ID: id, Title: id, State: store.BacklogRunning, Priority: store.P2, CreatedBy: "test"}); err != nil {
			t.Fatal(err)
		}
		if err := env.store.Pipeline.PutRun(ctx, &store.PipelineRun{ID: id, BacklogID: id, Template: "mills-default", State: store.PipelineEscalated, StartedAt: started}); err != nil {
			t.Fatal(err)
		}
		for attempt := 1; attempt <= 3; attempt++ {
			if err := env.store.Pipeline.PutStage(ctx, &store.StageResult{PipelineRunID: id, Stage: "implement", Attempt: attempt, StartedAt: since.Add(-48 * time.Hour), CostUSD: float64(attempt) / 4}); err != nil {
				t.Fatal(err)
			}
		}
		for _, g := range []struct {
			outcome store.GateOutcomeKind
			judge   string
		}{{store.GateOutcomePass, "judge"}, {store.GateOutcomeFail, store.JudgedByUnparseable}, {store.GateOutcomeSkip, "judge"}} {
			if err := env.store.Pipeline.PutGate(ctx, &store.GateOutcome{PipelineRunID: id, AfterStage: "implement", GateName: "quality", Outcome: g.outcome, JudgedBy: g.judge, EvaluatedAt: started}); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Include the exact boundary and later run, regardless of the stages' dates.
	cost, err := retryStageCostSince(ctx, env.store, since)
	if err != nil || cost != 2.5 {
		t.Fatalf("retry cost=%v err=%v, want 2.5", cost, err)
	}
	pass, total, err := countGateOutcomesSince(ctx, env.store, since)
	if err != nil || pass != 2 || total != 4 {
		t.Fatalf("gate counts=%d/%d err=%v, want 2/4", pass, total, err)
	}
	unparseable, err := countUnparseableGatesSince(ctx, env.store, since)
	if err != nil || unparseable != 2 {
		t.Fatalf("unparseable=%d err=%v, want 2", unparseable, err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := retryStageCostSince(canceled, env.store, since); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation=%v", err)
	}
}
