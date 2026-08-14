package store

import (
	"context"
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
}
