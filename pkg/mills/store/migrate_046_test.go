package store

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

func TestMigration046PreservesHistoryAndReopens(t *testing.T) {
	db := openSchemaAt(t, 45)
	ctx := context.Background()
	now := time.Date(2026, 9, 17, 0, 0, 0, 123456789, time.UTC)
	backlog := &BacklogDAO{db: db}
	pipeline := &PipelineDAO{db: db}
	if err := backlog.Put(ctx, &BacklogItem{ID: "kpi", Title: "history", State: BacklogRunning, Priority: P2, CreatedBy: "test"}); err != nil {
		t.Fatal(err)
	}
	if err := pipeline.PutRun(ctx, &PipelineRun{ID: "run", BacklogID: "kpi", Template: "mills-default", State: PipelineRunning(), StartedAt: now}); err != nil {
		t.Fatal(err)
	}
	putStage := func(dao *PipelineDAO, attempt int) {
		t.Helper()
		if err := dao.PutStage(ctx, &StageResult{PipelineRunID: "run", Stage: "implement", Attempt: attempt, StartedAt: now, CostUSD: float64(attempt) / 4, LogTail: "retain this log"}); err != nil {
			t.Fatal(err)
		}
	}
	putStage(pipeline, 1)
	putStage(pipeline, 2)
	if err := pipeline.PutGate(ctx, &GateOutcome{PipelineRunID: "run", AfterStage: "implement", GateName: "quality", Outcome: GateOutcomeFail, JudgedBy: JudgedByUnparseable, Reasons: []string{"retain these reasons"}, EvaluatedAt: now}); err != nil {
		t.Fatal(err)
	}
	snapshot := func(db *sql.DB) string {
		t.Helper()
		var stages, gates string
		for _, p := range []struct {
			query string
			dest  *string
		}{{`SELECT json_group_array(json_array(id,pipeline_run_id,stage,attempt,started_at,cost_usd,artifacts_json,log_tail)) FROM (SELECT * FROM stage_results ORDER BY id)`, &stages}, {`SELECT json_group_array(json_array(id,pipeline_run_id,outcome,judged_by,evaluated_at,reasons_json)) FROM (SELECT * FROM gate_outcomes ORDER BY id)`, &gates}} {
			if err := db.QueryRowContext(ctx, p.query).Scan(p.dest); err != nil {
				t.Fatal(err)
			}
		}
		return stages + gates
	}
	before := snapshot(db)
	for range 2 {
		if err := Migrate(ctx, db); err != nil {
			t.Fatal(err)
		}
	}
	var path string
	if err := db.QueryRowContext(ctx, `SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&path); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	st, err := Open(ctx, Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if after := snapshot(st.DB()); after != before {
		t.Fatalf("migration changed historical KPI evidence: before=%s after=%s", before, after)
	}
	putStage(st.Pipeline, 3)
	var count int
	var cost float64
	if err := st.DB().QueryRowContext(ctx, `SELECT count(*),sum(cost_usd) FROM stage_results INDEXED BY idx_stage_retry_cost WHERE pipeline_run_id='run' AND attempt>1`).Scan(&count, &cost); err != nil || count != 2 || cost != 1.25 {
		t.Fatalf("retry index append: count=%d cost=%v err=%v", count, cost, err)
	}
}

func TestMigration046FailureRestoresPriorIndexes(t *testing.T) {
	db := openSchemaAt(t, 45)
	ctx := context.Background()
	definition := func() string {
		t.Helper()
		var s string
		if err := db.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE name='idx_gate_outcomes_evaluated'`).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	before := definition()
	broken := migrationByVersion(t, 46)
	broken.sql += `; SELECT missing_kpi_probe_column FROM stage_results;`
	if err := applyOne(ctx, db, broken); err == nil {
		t.Fatal("injected migration failure succeeded")
	}
	if definition() != before {
		t.Fatal("rollback did not restore gate window index")
	}
	for _, q := range []string{`SELECT count(*) FROM sqlite_master WHERE name='idx_stage_retry_cost'`, `SELECT count(*) FROM schema_migrations WHERE version=46`} {
		var n int
		if err := db.QueryRowContext(ctx, q).Scan(&n); err != nil || n != 0 {
			t.Fatalf("failed migration left state: n=%d err=%v", n, err)
		}
	}
	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	if definition() == before {
		t.Fatal("retry did not replace gate index")
	}
}
