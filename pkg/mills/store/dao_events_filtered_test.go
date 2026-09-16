package store

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

// Coverage for the filtered window scans that back the guard reports. The
// contract under test: the LIMIT applies to the FILTERED set, so a firehose
// of unrelated events can no longer saturate a report's truncation cap.

func seedFilteredEvents(t *testing.T, st *Store) {
	t.Helper()
	ctx := context.Background()
	base := time.Now().UTC().Add(-time.Hour)
	seed := []Event{
		{Actor: "overseer.foreman", Kind: "anomaly_opened"},
		{Actor: "overseer.groomer", Kind: "tick"},
		{Actor: "council.mutator", Kind: "demand.suppressed"},
		{Actor: "overseerish", Kind: "tick"}, // prefix must not match without the dot
		{Actor: "pipeline", Kind: "judge.verdict"},
		{Actor: "pipeline", Kind: "run.provenance"},
		{Actor: "pipeline", Kind: "stage.started"},
		{Actor: "pipeline", Kind: "stage.started"},
	}
	for i := range seed {
		seed[i].OccurredAt = base.Add(time.Duration(i) * time.Minute)
		if err := st.Events.Append(ctx, &seed[i]); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
}

func TestEventDAO_ListSinceByActorPrefix(t *testing.T) {
	st := newTestStore(t)
	seedFilteredEvents(t, st)
	ctx := context.Background()
	since := time.Now().UTC().Add(-2 * time.Hour)

	got, err := st.Events.ListSinceByActorPrefix(ctx, "overseer.", since, 100)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 overseer.* events, got %d", len(got))
	}
	for _, e := range got {
		if e.Actor != "overseer.foreman" && e.Actor != "overseer.groomer" {
			t.Fatalf("unexpected actor %q", e.Actor)
		}
	}

	// The limit bounds the filtered set: three unrelated pipeline events must
	// not consume it.
	got, err = st.Events.ListSinceByActorPrefix(ctx, "overseer.", since, 2)
	if err != nil {
		t.Fatalf("list limited: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("limited list: want 2, got %d", len(got))
	}

	if _, err := st.Events.ListSinceByActorPrefix(ctx, "", since, 10); err == nil {
		t.Fatal("empty prefix must error")
	}
}

func TestEventDAO_ListSinceByKinds(t *testing.T) {
	st := newTestStore(t)
	seedFilteredEvents(t, st)
	ctx := context.Background()
	since := time.Now().UTC().Add(-2 * time.Hour)

	got, err := st.Events.ListSinceByKinds(ctx, []string{"judge.verdict", "run.provenance"}, since, 100)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 events, got %d", len(got))
	}
	for _, e := range got {
		if e.Kind != "judge.verdict" && e.Kind != "run.provenance" {
			t.Fatalf("unexpected kind %q", e.Kind)
		}
	}

	// Window bound still applies.
	got, err = st.Events.ListSinceByKinds(ctx, []string{"judge.verdict"}, time.Now().UTC().Add(time.Hour), 100)
	if err != nil {
		t.Fatalf("list future-since: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("future since: want 0, got %d", len(got))
	}

	if _, err := st.Events.ListSinceByKinds(ctx, nil, since, 10); err == nil {
		t.Fatal("empty kinds must error")
	}
	if _, err := st.Events.ListSinceByKinds(ctx, []string{"judge.verdict"}, since, 0); err == nil {
		t.Fatal("non-positive limit must error")
	}
}

func TestEventDAO_WalkActorWindow(t *testing.T) {
	st := newTestStore(t)
	st.DB().SetMaxOpenConns(1)
	ctx := context.Background()
	since := time.Date(2026, 9, 16, 0, 0, 0, 0, time.UTC)
	until := since.Add(time.Hour)
	seed := []Event{
		{Actor: "review_%", Kind: "at-start", OccurredAt: since, SubjectKind: "backlog", SubjectID: "one"},
		{Actor: "review_%.groomer", Kind: "at-end", OccurredAt: until},
		{Actor: "review_%", Kind: "too-old", OccurredAt: since.Add(-time.Nanosecond)},
		{Actor: "review_%", Kind: "future", OccurredAt: until.Add(time.Nanosecond)},
		{Actor: "review_%.historical", Kind: "historical-only", OccurredAt: since.Add(-24 * time.Hour)},
		{Actor: "review_%.future", Kind: "future-only", OccurredAt: until.Add(24 * time.Hour)},
		{Actor: "REVIEW_%", Kind: "wrong-case", OccurredAt: since},
		{Actor: "review-other", Kind: "wildcard-mismatch", OccurredAt: since},
	}
	for i := range seed {
		seed[i].Payload = map[string]any{"ignored": "report needs metadata only"}
		if err := st.Events.Append(ctx, &seed[i]); err != nil {
			t.Fatal(err)
		}
	}
	var kinds []string
	err := st.Events.WalkActorWindow(ctx, "review_%", since, until, func(e *Event) error {
		kinds = append(kinds, e.Kind)
		if e.Payload != nil {
			t.Fatal("metadata walk must not load payloads")
		}
		if e.Kind == "at-start" && (e.SubjectKind != "backlog" || e.SubjectID != "one" || e.ID != seed[0].ID) {
			t.Fatalf("lost event metadata: %+v", e)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(kinds)
	if want := []string{"at-end", "at-start"}; !reflect.DeepEqual(kinds, want) {
		t.Fatalf("got %v, want %v", kinds, want)
	}
	for _, prefix := range []string{"missing.", "review_%.historical", "review_%.future"} {
		if err := st.Events.WalkActorWindow(ctx, prefix, since, until, func(e *Event) error {
			t.Fatalf("empty window for %q returned event %d", prefix, e.ID)
			return nil
		}); err != nil {
			t.Fatalf("empty window for %q: %v", prefix, err)
		}
	}

	stop := errors.New("visitor stopped")
	calls := 0
	err = st.Events.WalkActorWindow(ctx, "review_%", since, until, func(*Event) error {
		calls++
		return stop
	})
	if !errors.Is(err, stop) || calls != 1 {
		t.Fatalf("visitor error: calls=%d err=%v", calls, err)
	}
	canceled, cancel := context.WithCancel(ctx)
	calls = 0
	err = st.Events.WalkActorWindow(canceled, "review_%", since, until, func(*Event) error {
		calls++
		cancel()
		return nil
	})
	cancel()
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("canceled walk: calls=%d err=%v", calls, err)
	}
	// A failed walk must close its rows and leave the connection usable.
	if err := st.DB().PingContext(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestEventDAO_WalkActorWindowSeeksActorAndTimeWithoutSort(t *testing.T) {
	st := newTestStore(t)
	until := time.Now().UTC()
	plan := queryPlan(t, st, "EXPLAIN QUERY PLAN "+walkActorWindowQuery,
		[]any{"overseer.", "overseer.\U0010FFFF", timeRFC3339(until.Add(-7 * 24 * time.Hour)), timeRFC3339(until)})
	assertPlanUsesIndexWithoutTableScan(t, plan, "idx_events_actor_occurred", "events")
	// An actor-prefix range uses the index but still walks every historical
	// entry for those actors. Each event read must seek both time bounds too.
	if !strings.Contains(strings.Join(plan, "\n"), "USING COVERING INDEX idx_events_actor_occurred (actor=? AND occurred_at>? AND occurred_at<?)") {
		t.Fatalf("metadata walk must read its actor/time window without fetching payload-bearing table pages: %v", plan)
	}
	for _, detail := range plan {
		if strings.Contains(detail, "TEMP B-TREE") {
			t.Fatalf("metadata walk must not sort events: %v", plan)
		}
	}
}
