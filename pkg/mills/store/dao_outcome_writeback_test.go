package store

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"
)

func seedOutcomeFixture(t *testing.T, st *Store, id, plan string, state BacklogState, grade string, attempts int, cost float64) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	if err := st.Backlog.Put(ctx, &BacklogItem{ID: id, Title: id, State: state, Priority: P2, PlanID: plan, Grade: grade, CreatedBy: "test", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	pstate := PipelineDone
	if state == BacklogEscalated {
		pstate = PipelineEscalated
	}
	ended := now
	if err := st.Pipeline.PutRun(ctx, &PipelineRun{ID: "run-" + id, BacklogID: id, Template: "test", State: pstate, Attempts: attempts, StartedAt: now.Add(-time.Hour), EndedAt: &ended, CostUSD: cost}); err != nil {
		t.Fatal(err)
	}
}

func TestOutcomeWritebackTerminalRoundTripAndIdempotency(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	seedOutcomeFixture(t, st, "merged", "plan-a", BacklogMerged, "keep", 3, 1.25)
	got, err := st.Outcomes.WriteTerminal(ctx, "merged", .8)
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != "merged" || got.Attempts != 3 || got.CostUSD != 1.25 || got.DispatchScore != .8 || got.Grade == nil || *got.Grade != "keep" {
		t.Fatalf("round trip: %+v", got)
	}
	if _, err = st.Outcomes.WriteTerminal(ctx, "merged", .8); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM outcome_writebacks WHERE backlog_id='merged'`).Scan(&n)
	if n != 1 {
		t.Fatalf("duplicate rows=%d", n)
	}
	seedOutcomeFixture(t, st, "escalated", "plan-a", BacklogEscalated, "", 2, .5)
	esc, err := st.Outcomes.WriteTerminal(ctx, "escalated", .2)
	if err != nil {
		t.Fatal(err)
	}
	if esc.Outcome != "escalated" || esc.Grade != nil {
		t.Fatalf("escalated: %+v", esc)
	}
	if _, err := st.Backlog.GradeRun(ctx, "run-escalated", "regret", "late grade", "operator", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	esc, err = st.Outcomes.Get(ctx, "escalated")
	if err != nil || esc.Grade == nil || *esc.Grade != "regret" {
		t.Fatalf("late grade join: row=%+v err=%v", esc, err)
	}
}

func TestOutcomeWritebackRejectsNonTerminal(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := st.Backlog.Put(ctx, &BacklogItem{ID: "queued", Title: "queued", State: BacklogQueued, Priority: P2, CreatedBy: "test", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Outcomes.WriteTerminal(ctx, "queued", .5); !errors.Is(err, ErrOutcomeNotTerminal) {
		t.Fatalf("err=%v", err)
	}
}

func TestOutcomeFeaturesAndCalibration(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	seedOutcomeFixture(t, st, "m", "plan-a", BacklogMerged, "keep", 1, 1)
	seedOutcomeFixture(t, st, "e", "plan-a", BacklogEscalated, "", 3, 3)
	if _, err := st.Outcomes.WriteTerminal(ctx, "m", .9); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Outcomes.WriteTerminal(ctx, "e", .2); err != nil {
		t.Fatal(err)
	}
	f, err := st.Outcomes.Features(ctx, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if f["plan-a"].Samples != 2 || f["plan-a"].MergeRate != .5 || f["plan-a"].AverageAttempts != 2 || f["plan-a"].AverageCostUSD != 2 {
		t.Fatalf("features=%+v", f)
	}
	r, err := st.Outcomes.Calibration(ctx, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if r.Samples != 2 || r.Merged != 1 || r.Escalated != 1 || r.Graded != 1 || r.GradeCoverage != .5 || len(r.Buckets) != 2 || math.Abs(r.CalibrationError-.15) > 1e-9 {
		t.Fatalf("report=%+v", r)
	}
	same := time.Now()
	if _, err := st.Outcomes.Calibration(ctx, same, same); err == nil {
		t.Fatal("invalid window accepted")
	}
}

func TestOutcomeMigrationDoesNotIndexEvents(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	var n int
	if err := st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version = 33`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("outcome migration version 33 applied=%d err=%v", n, err)
	}
	if err := st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND tbl_name='events' AND sql LIKE '%outcome%'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("event outcome indexes=%d err=%v", n, err)
	}
}

func TestOutcomeWritebackGradeBackfillIsSelectiveAndIdempotent(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	for _, fixture := range []struct{ id, grade string }{
		{"null-grade", "keep"},
		{"empty-grade", "meh"},
		{"preserved-grade", "keep"},
		{"ungraded", ""},
	} {
		seedOutcomeFixture(t, st, fixture.id, "plan", BacklogMerged, fixture.grade, 1, 1)
		if _, err := st.Outcomes.WriteTerminal(ctx, fixture.id, .5); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.DB().ExecContext(ctx, `UPDATE outcome_writebacks SET grade=NULL WHERE backlog_id IN ('null-grade','ungraded')`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().ExecContext(ctx, `PRAGMA ignore_check_constraints=ON`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().ExecContext(ctx, `UPDATE outcome_writebacks SET grade='' WHERE backlog_id='empty-grade'`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().ExecContext(ctx, `PRAGMA ignore_check_constraints=OFF`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().ExecContext(ctx, `UPDATE outcome_writebacks SET grade='regret' WHERE backlog_id='preserved-grade'; DELETE FROM schema_migrations WHERE version=40`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, st.DB()); err != nil {
		t.Fatal(err)
	}
	assertGrades := func() {
		t.Helper()
		for id, want := range map[string]*string{
			"null-grade": strPtr("keep"), "empty-grade": strPtr("meh"),
			"preserved-grade": strPtr("regret"), "ungraded": nil,
		} {
			got, err := st.Outcomes.Get(ctx, id)
			if err != nil || (want == nil) != (got.Grade == nil) || want != nil && *got.Grade != *want {
				t.Errorf("%s grade=%s want=%s err=%v", id, derefGrade(got.Grade), derefGrade(want), err)
			}
		}
	}
	assertGrades()
	if err := Migrate(ctx, st.DB()); err != nil {
		t.Fatal(err)
	}
	assertGrades()
}

func strPtr(v string) *string { return &v }

func derefGrade(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}
