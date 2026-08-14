package store

import (
	"context"
	"math"
	"testing"
	"time"
)

func TestBacklogTasteAggregates(t *testing.T) {
	st := newTestStore(t)
	now := time.Now().UTC()
	items := []BacklogItem{
		{ID: "taste-1", Title: "keep", State: BacklogMerged, Priority: P2, CreatedBy: "test", PlanID: "plan-a", Grade: "keep", UpdatedAt: now},
		{ID: "taste-2", Title: "regret", State: BacklogMerged, Priority: P2, CreatedBy: "test", PlanID: "plan-a", Grade: "regret", UpdatedAt: now},
		{ID: "taste-3", Title: "ungraded", State: BacklogMerged, Priority: P2, CreatedBy: "test", PlanID: "plan-a", UpdatedAt: now},
		{ID: "taste-old", Title: "old", State: BacklogMerged, Priority: P2, CreatedBy: "test", PlanID: "plan-b", Grade: "meh", UpdatedAt: now.Add(-15 * 24 * time.Hour)},
	}
	for i := range items {
		if err := st.Backlog.Put(context.Background(), &items[i]); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.DB().ExecContext(context.Background(), `UPDATE backlog_items SET updated_at = ? WHERE id = 'taste-old'`, timeRFC3339(now.Add(-15*24*time.Hour))); err != nil {
		t.Fatal(err)
	}
	got, err := st.Backlog.TasteAggregates(context.Background(), now, 14*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Plans) != 2 || got.Plans[0].Merged != 3 || got.Plans[0].Graded != 2 || got.Plans[0].RegretRate != .5 || got.Plans[0].GradeCoverage != 2.0/3.0 {
		t.Fatalf("aggregate = %+v", got)
	}
	if got.OverallMerged14d != 3 || got.OverallGraded14d != 2 || got.OverallCoverage14d != 2.0/3.0 {
		t.Fatalf("overall = %+v", got)
	}
	if math.IsNaN(got.OverallCoverage14d) {
		t.Fatal("coverage is NaN")
	}
}

func TestBacklogTasteAggregatesEmpty(t *testing.T) {
	st := newTestStore(t)
	got, err := st.Backlog.TasteAggregates(context.Background(), time.Now(), 14*24*time.Hour)
	if err != nil || got.Plans == nil || got.OverallCoverage14d != 0 {
		t.Fatalf("got=%+v err=%v", got, err)
	}
}
