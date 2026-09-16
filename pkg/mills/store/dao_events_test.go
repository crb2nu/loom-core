package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestEventHotReadsUseWindowIndexes(t *testing.T) {
	st := newTestStore(t)
	since := timeRFC3339(time.Now().Add(-24 * time.Hour))
	tests := []struct {
		name, query, index string
		args               []any
	}{
		{
			// Unpinned since 2026-09-02: the ListSinceByKinds pin to the time
			// index outlived migration 029's kind index and walked the whole
			// window per call (the KPI writer's "event scan: context deadline
			// exceeded"). The planner must pick the kind index on its own.
			name: "kind window",
			query: `EXPLAIN QUERY PLAN SELECT ` + eventColumns + ` FROM events
				WHERE kind = ? AND occurred_at >= ? ORDER BY occurred_at DESC LIMIT ?`,
			index: "idx_events_kind_occurred", args: []any{"learning.signal", since, 200},
		},
		{
			name: "subject window",
			query: `EXPLAIN QUERY PLAN SELECT ` + eventColumns + ` FROM events INDEXED BY idx_events_occurred
				WHERE subject_kind = ? AND subject_id = ? AND occurred_at >= ?
				ORDER BY occurred_at DESC LIMIT ?`,
			index: "idx_events_occurred", args: []any{"pipeline", "PIPE-1", since, 200},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			details := queryPlan(t, st, tc.query, tc.args)
			assertPlanUsesIndexWithoutTableScan(t, details, tc.index, "events")
		})
	}
}

func assertPlanUsesIndexWithoutTableScan(t *testing.T, details []string, index, table string) {
	t.Helper()
	plan := strings.Join(details, "\n")
	if !strings.Contains(plan, index) {
		t.Fatalf("query plan does not use %s:\n%s", index, plan)
	}
	if strings.Contains(plan, "SCAN "+table) {
		t.Fatalf("query plan full-scans %s:\n%s", table, plan)
	}
}

// TestEventReadIndexes_QueryPlans proves the migration-029 covering indexes
// actually serve the query shapes that congested the operator on 2026-08-15
// (bl-mills-event-read-index-congestion-20260816). Unlike
// TestEventHotReadsUseWindowIndexes above (which pins plans via INDEXED BY),
// these run UNPINNED so they assert what the planner actually chooses.
func TestEventReadIndexes_QueryPlans(t *testing.T) {
	st := newTestStore(t)
	since := timeRFC3339(time.Now().Add(-14 * 24 * time.Hour))

	actorPlan := queryPlan(t, st,
		`EXPLAIN QUERY PLAN SELECT `+eventColumns+` FROM events
		 WHERE actor = ? AND occurred_at >= ?
		 ORDER BY occurred_at DESC, id DESC LIMIT ?`,
		[]any{"overseer.groomer", since, 200})
	assertPlanUsesIndexWithoutTableScan(t, actorPlan, "idx_events_actor_occurred", "events")
	if plan := strings.Join(actorPlan, "\n"); strings.Contains(plan, "TEMP B-TREE") {
		t.Fatalf("recent-actor reads must retain timestamp/id index ordering:\n%s", plan)
	}

	kindPlan := queryPlan(t, st,
		`EXPLAIN QUERY PLAN SELECT `+eventColumns+` FROM events
		 WHERE kind IN (?, ?) AND occurred_at >= ?
		 ORDER BY occurred_at DESC, id DESC LIMIT ?`,
		[]any{"gate.verdict", "pipeline.stage", since, 200})
	assertPlanUsesIndexWithoutTableScan(t, kindPlan, "idx_events_kind_occurred", "events")

	// The promotion report's actor-prefix read is spelled as a half-open actor
	// range so it SEARCHes idx_events_actor_occurred instead of walking the
	// time window with a substr() predicate (ListSinceByActorPrefix).
	prefixPlan := queryPlan(t, st,
		`EXPLAIN QUERY PLAN SELECT `+eventColumns+` FROM events
		 WHERE occurred_at >= ? AND actor >= ? AND actor < ?
		 ORDER BY occurred_at DESC LIMIT ?`,
		[]any{since, "overseer.", "overseer.􏿿", 200})
	assertPlanUsesIndexWithoutTableScan(t, prefixPlan, "idx_events_actor_occurred", "events")

	// The finished-goods digest's latest-day read (LatestByKind) is an
	// equality seek on kind plus a one-row backward walk of the same index:
	// no time window and no sort.
	latestPlan := queryPlan(t, st,
		`EXPLAIN QUERY PLAN SELECT `+eventColumns+` FROM events
		 WHERE kind = ? ORDER BY occurred_at DESC, id DESC LIMIT 1`,
		[]any{"finishing.digest"})
	assertPlanUsesIndexWithoutTableScan(t, latestPlan, "idx_events_kind_occurred", "events")
	if plan := strings.Join(latestPlan, "\n"); strings.Contains(plan, "TEMP B-TREE") {
		t.Fatalf("latest-by-kind plan sorts instead of walking the index:\n%s", plan)
	}
}

func TestEventLatestByKind(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 9, 10, 6, 0, 0, 0, time.UTC)
	seed := []Event{
		{OccurredAt: base, Actor: "finishing-digest", Kind: "finishing.digest", SubjectKind: "finishing.digest", SubjectID: "2026-09-09"},
		{OccurredAt: base.AddDate(0, 0, 1), Actor: "finishing-digest", Kind: "finishing.digest", SubjectKind: "finishing.digest", SubjectID: "2026-09-10", Payload: map[string]any{"day": "2026-09-10"}},
		{OccurredAt: base.AddDate(0, 0, 2), Actor: "reconciler", Kind: "pipeline.stage", SubjectKind: "pipeline", SubjectID: "RUN-1"},
	}
	for i := range seed {
		if err := st.Events.Append(ctx, &seed[i]); err != nil {
			t.Fatal(err)
		}
	}
	got, err := st.Events.LatestByKind(ctx, "finishing.digest")
	if err != nil {
		t.Fatal(err)
	}
	if got.SubjectID != "2026-09-10" || !got.OccurredAt.Equal(base.AddDate(0, 0, 1)) || got.Payload["day"] != "2026-09-10" {
		t.Fatalf("latest = %+v", got)
	}
	if _, err := st.Events.LatestByKind(ctx, "finishing.missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing kind err = %v, want ErrNotFound", err)
	}
	if _, err := st.Events.LatestByKind(ctx, ""); err == nil {
		t.Fatal("empty kind must be rejected")
	}
}
