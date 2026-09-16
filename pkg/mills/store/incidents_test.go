package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestIncidentDAO_FingerprintUpsertAndRoundTrip(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	when := time.Date(2026, 7, 29, 1, 2, 3, 0, time.UTC)
	want := &IncidentRecord{
		ID: "INC-first", Fingerprint: "fp-0123456789abcdef", Class: IncidentClassExternalDependency,
		Source: "gitlab-ci", Dependency: "gitlab", Shape: "service-unavailable",
		Summary: "GitLab CI service is unavailable", Evidence: "status 503",
		Retryable: true, OccurredAt: when, FirstSeen: when, LastSeen: when, OccurrenceCount: 1,
	}
	inserted, err := st.Incidents.Put(ctx, want)
	if err != nil || !inserted {
		t.Fatalf("Put = (%v, %v), want (true, nil)", inserted, err)
	}
	duplicate := *want
	duplicate.ID = "INC-second"
	duplicate.Summary = "must not replace first writer"
	duplicate.Evidence = "status 503 again"
	duplicate.OccurredAt = when.Add(time.Hour)
	duplicate.FirstSeen = time.Time{}
	duplicate.LastSeen = time.Time{}
	duplicate.OccurrenceCount = 0
	inserted, err = st.Incidents.Put(ctx, &duplicate)
	if err != nil || inserted {
		t.Fatalf("duplicate Put = (%v, %v), want (false, nil)", inserted, err)
	}
	got, err := st.Incidents.Get(ctx, want.Fingerprint)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != want.ID || got.Summary != want.Summary || got.Evidence != want.Evidence {
		t.Fatalf("upsert replaced first writer fields: %+v", got)
	}
	if got.Fingerprint != want.Fingerprint || got.FirstSeen != when || got.LastSeen != when.Add(time.Hour) ||
		got.OccurredAt != when.Add(time.Hour) || got.OccurrenceCount != 2 {
		t.Fatalf("upsert counters/times = %+v", got)
	}
	list, err := st.Incidents.ListSince(ctx, when.Add(-time.Minute), 10)
	if err != nil {
		t.Fatalf("ListSince: %v", err)
	}
	if len(list) != 1 || list[0] != *got {
		t.Fatalf("ListSince = %+v, want [%+v]", list, *got)
	}
}

func TestIncidentDAO_ConcurrentFingerprintUpsert(t *testing.T) {
	st := newTestStore(t)
	const writers = 12
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := st.Incidents.Put(context.Background(), &IncidentRecord{
				ID: "INC-concurrent", Fingerprint: "fp-concurrent",
				Class: IncidentClassExternalDependency, Source: "gitlab-ci",
				Dependency: "gitlab", Shape: "rate-limit", Summary: "rate limited",
			})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Put: %v", err)
		}
	}
	got, err := st.Incidents.Get(context.Background(), "fp-concurrent")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.OccurrenceCount != writers {
		t.Fatalf("OccurrenceCount = %d, want %d", got.OccurrenceCount, writers)
	}
}

func TestIncidentDAO_DistinctFingerprintsAndAggregation(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	when := time.Date(2026, 7, 29, 1, 2, 3, 0, time.UTC)
	for _, fingerprint := range []string{"fp-one", "fp-one", "fp-two"} {
		record := &IncidentRecord{
			ID: fingerprint, Fingerprint: fingerprint, Class: IncidentClassExternalDependency,
			Source: "gitlab-ci", Dependency: "gitlab", Shape: "service-unavailable",
			Summary: "GitLab CI service is unavailable", Retryable: true, OccurredAt: when,
		}
		if _, err := st.Incidents.Put(ctx, record); err != nil {
			t.Fatalf("Put(%s): %v", fingerprint, err)
		}
	}
	list, err := st.Incidents.ListSince(ctx, when.Add(-time.Minute), 10)
	if err != nil {
		t.Fatalf("ListSince: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("ListSince len = %d, want 2", len(list))
	}
	aggregated, err := st.Incidents.ListAggregated(ctx, when.Add(-time.Minute), 10)
	if err != nil {
		t.Fatalf("ListAggregated: %v", err)
	}
	if len(aggregated) != 1 || aggregated[0].Occurrences != 3 {
		t.Fatalf("ListAggregated = %+v, want one summary with 3 occurrences", aggregated)
	}
}

func TestIncidentDAO_ListAggregatedRequiresBounds(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	if _, err := st.Incidents.ListAggregated(ctx, time.Time{}, 10); err == nil {
		t.Fatal("zero since should fail")
	}
	if _, err := st.Incidents.ListAggregated(ctx, time.Now().UTC(), 0); err == nil {
		t.Fatal("non-positive limit should fail")
	}
}

func TestIncidentDAO_ListSinceRequiresLimit(t *testing.T) {
	st := newTestStore(t)
	if _, err := st.Incidents.ListSince(context.Background(), time.Now().Add(-time.Hour), 0); err == nil {
		t.Fatal("non-positive limit should fail")
	}
}

// TestIncidentDAO_ListSinceUsesKindIndex runs the ListSince shape UNPINNED and
// asserts the planner picks migration 029's (kind, occurred_at) index. The
// previous version of this test pinned idx_events_occurred via INDEXED BY and
// so certified the exact plan that walked the whole 24h window and timed the
// council brief out (2026-09-03..07); a pin in a test proves nothing about
// what production runs.
func TestIncidentDAO_ListSinceUsesKindIndex(t *testing.T) {
	st := newTestStore(t)
	details := queryPlan(t, st, `EXPLAIN QUERY PLAN
		SELECT `+eventColumns+` FROM events
		WHERE kind = ? AND occurred_at >= ?
		ORDER BY occurred_at DESC, id DESC LIMIT ?`,
		[]any{IncidentEventKind, timeRFC3339(time.Now().Add(-24 * time.Hour)), 200})
	assertPlanUsesIndexWithoutTableScan(t, details, "idx_events_kind_occurred", "events")
	if plan := strings.Join(details, "\n"); strings.Contains(plan, "idx_events_occurred ") || strings.HasSuffix(plan, "idx_events_occurred") {
		t.Fatalf("incident window read regressed to the time-only index:\n%s", plan)
	}
}

func TestIncidentDAO_BackwardCompatibleIDFingerprint(t *testing.T) {
	st := newTestStore(t)
	record := &IncidentRecord{
		ID: "INC-legacy", Class: IncidentClassExternalDependency, Source: "storage",
		Dependency: "storage", Shape: "capacity", Summary: "capacity exhausted",
	}
	if inserted, err := st.Incidents.Put(context.Background(), record); err != nil || !inserted {
		t.Fatalf("Put = (%t, %v), want (true, nil)", inserted, err)
	}
	if record.Fingerprint != record.ID {
		t.Fatalf("legacy fingerprint = %q, want %q", record.Fingerprint, record.ID)
	}
}

func TestIncidentDAO_ValidationAndNotFound(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	if _, err := st.Incidents.Put(ctx, nil); err == nil {
		t.Fatal("nil record should fail")
	}
	if _, err := st.Incidents.Put(ctx, &IncidentRecord{ID: "INC-incomplete"}); err == nil {
		t.Fatal("incomplete record should fail")
	}
	if _, err := st.Incidents.Get(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get missing = %v, want ErrNotFound", err)
	}
}

func TestIncidentDAO_ListBoundsAggregationAndDeadline(t *testing.T) {
	st := newTestStoreWithOptions(t, Options{IncidentReadLimit: 3})
	ctx := context.Background()
	base := time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		record := &IncidentRecord{
			ID: fmt.Sprintf("INC-%d", i), Fingerprint: fmt.Sprintf("fp-%d", i),
			Class: IncidentClassExternalDependency, Source: "test", Dependency: "dep",
			Shape: fmt.Sprintf("shape-%d", i), Summary: "failure", OccurredAt: base.Add(time.Duration(i) * time.Minute),
		}
		if _, err := st.Incidents.Put(ctx, record); err != nil {
			t.Fatal(err)
		}
	}
	got, err := st.Incidents.ListSince(ctx, base.Add(-time.Minute), 100)
	if err != nil || len(got) != 3 {
		t.Fatalf("ListSince = %d rows, %v; want 3", len(got), err)
	}
	if !got[0].OccurredAt.After(got[1].OccurredAt) || !got[1].OccurredAt.After(got[2].OccurredAt) {
		t.Fatalf("ListSince ordering = %+v", got)
	}
	aggregated, err := st.Incidents.ListAggregated(ctx, base.Add(-time.Minute), 100)
	if err != nil || len(aggregated) != 3 {
		t.Fatalf("ListAggregated = %d rows, %v; want bounded 3", len(aggregated), err)
	}
	expired, cancel := context.WithDeadline(ctx, time.Now().Add(-time.Second))
	defer cancel()
	if _, err := st.Incidents.ListSince(expired, base, 10); !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "incident list") {
		t.Fatalf("expired ListSince error = %v", err)
	}
}
