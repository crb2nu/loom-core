package store

import (
	"context"
	"errors"
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
	aggregated, err := st.Incidents.ListAggregated(ctx)
	if err != nil {
		t.Fatalf("ListAggregated: %v", err)
	}
	if len(aggregated) != 1 || aggregated[0].Occurrences != 3 {
		t.Fatalf("ListAggregated = %+v, want one summary with 3 occurrences", aggregated)
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
