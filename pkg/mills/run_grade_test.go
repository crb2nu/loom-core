package mills

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/store"
)

func gradeTestStore(t *testing.T, state store.BacklogState) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), store.Options{Path: filepath.Join(t.TempDir(), "grade.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Backlog.Put(context.Background(), &store.BacklogItem{ID: "BL-GRADE", Title: "taste", State: state, Priority: store.P2, CreatedBy: "test", PlanID: "PLAN-1"}); err != nil {
		t.Fatal(err)
	}
	if err := st.Pipeline.PutRun(context.Background(), &store.PipelineRun{ID: "RUN-GRADE", BacklogID: "BL-GRADE", Template: "test", State: store.PipelineDone, StartedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	return st
}

func writeGradeOutcome(t *testing.T, st *store.Store) {
	t.Helper()
	if _, err := st.Outcomes.WriteTerminal(context.Background(), "BL-GRADE", 0.8); err != nil {
		t.Fatal(err)
	}
}

func TestGradeRun_AppendsSupersedeChain(t *testing.T) {
	st := gradeTestStore(t, store.BacklogMerged)
	ctx := context.Background()
	for _, tc := range []struct{ grade, note string }{{"keep", "ship more"}, {"regret", "changed my mind"}} {
		item, err := GradeRun(ctx, st, "RUN-GRADE", tc.grade, tc.note, "operator.manual")
		if err != nil {
			t.Fatal(err)
		}
		if item.Grade != tc.grade || item.GradeNote != tc.note || item.GradedAt == nil {
			t.Fatalf("grade head = %+v", item)
		}
	}
	events, err := st.Events.ListBySubject(ctx, "pipeline_run", "RUN-GRADE", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Payload["grade"] != "regret" || events[0].Payload["prior_grade"] != "keep" || events[1].Payload["prior_grade"] != "" {
		t.Fatalf("grade events = %#v", events)
	}
	for _, key := range []string{"note", "actor", "run_id", "item_id", "plan_id"} {
		if _, ok := events[0].Payload[key]; !ok {
			t.Errorf("event missing %s: %#v", key, events[0].Payload)
		}
	}
	if events[0].Payload["note"] != "changed my mind" || events[1].Payload["note"] != "ship more" {
		t.Errorf("grade event notes = %#v, want append-only note history", events)
	}
}

func TestGradeRun_AcceptsEscalatedTerminalWork(t *testing.T) {
	st := gradeTestStore(t, store.BacklogEscalated)
	item, err := GradeRun(context.Background(), st, "RUN-GRADE", "meh", "useful failure", "operator.manual")
	if err != nil {
		t.Fatal(err)
	}
	if item.Grade != "meh" || item.GradeNote != "useful failure" {
		t.Fatalf("grade head = %+v", item)
	}
}

func TestGradeRunAndItem_PropagateOutcomeWritebackGrade(t *testing.T) {
	for _, tc := range []struct {
		name  string
		grade func(context.Context, *store.Store) error
	}{
		{"run", func(ctx context.Context, st *store.Store) error {
			_, err := GradeRun(ctx, st, "RUN-GRADE", "keep", "", "operator.manual")
			return err
		}},
		{"item", func(ctx context.Context, st *store.Store) error {
			_, err := GradeItem(ctx, st, "BL-GRADE", "meh", "", "operator.manual")
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := gradeTestStore(t, store.BacklogMerged)
			writeGradeOutcome(t, st)
			if err := tc.grade(context.Background(), st); err != nil {
				t.Fatal(err)
			}
			got, err := st.Outcomes.Get(context.Background(), "BL-GRADE")
			if err != nil || got.Grade == nil {
				t.Fatalf("writeback grade: row=%+v err=%v", got, err)
			}
			want := map[string]string{"run": "keep", "item": "meh"}[tc.name]
			if *got.Grade != want {
				t.Fatalf("writeback grade=%q want %q", *got.Grade, want)
			}
		})
	}
}

func TestGradeItem_RollsBackHeadAndWritebackWhenEventFails(t *testing.T) {
	st := gradeTestStore(t, store.BacklogMerged)
	writeGradeOutcome(t, st)
	ctx := context.Background()
	if _, err := st.DB().ExecContext(ctx, `CREATE TRIGGER reject_bolt_graded BEFORE INSERT ON events
		WHEN NEW.kind = 'bolt.graded' BEGIN SELECT RAISE(ABORT, 'reject grade event'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := GradeItem(ctx, st, "BL-GRADE", "regret", "", "operator.manual"); err == nil {
		t.Fatal("grade unexpectedly succeeded")
	}
	item, err := st.Backlog.Get(ctx, "BL-GRADE")
	if err != nil || item.Grade != "" {
		t.Fatalf("backlog head changed after rollback: item=%+v err=%v", item, err)
	}
	writeback, err := st.Outcomes.Get(ctx, "BL-GRADE")
	if err != nil || writeback.Grade != nil {
		t.Fatalf("writeback changed after rollback: row=%+v err=%v", writeback, err)
	}
	events, err := st.Events.ListBySubject(ctx, "backlog_item", "BL-GRADE", 10)
	if err != nil || len(events) != 0 {
		t.Fatalf("grade event persisted after rollback: events=%+v err=%v", events, err)
	}
}

func TestGradeRun_Validation(t *testing.T) {
	st := gradeTestStore(t, store.BacklogRunning)
	if _, err := GradeRun(context.Background(), st, "RUN-GRADE", "great", "", "operator.manual"); !errors.Is(err, ErrInvalidGrade) {
		t.Fatalf("invalid grade error = %v", err)
	}
	if _, err := GradeRun(context.Background(), st, "RUN-GRADE", "meh", "line one\nline two", "operator.manual"); !errors.Is(err, ErrInvalidGradeNote) {
		t.Fatalf("multiline note error = %v", err)
	}
	if _, err := GradeRun(context.Background(), st, "RUN-GRADE", "meh", "", "operator.manual"); !errors.Is(err, ErrNotGradable) {
		t.Fatalf("non-terminal error = %v", err)
	}
}
