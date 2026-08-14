package store

import (
	"context"
	"testing"
	"time"
)

// TestEventDAO_CountBySubjectKind covers the per-subject all-time counter that
// backs the auto-requeue per-item lifetime cap.
func TestEventDAO_CountBySubjectKind(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	const kind = "reconciler.auto_requeued"

	// Two events for item A, one for item B, one of a different kind for A.
	seed := []Event{
		{Actor: "reconciler", Kind: kind, SubjectKind: "backlog_item", SubjectID: "A"},
		{Actor: "reconciler", Kind: kind, SubjectKind: "backlog_item", SubjectID: "A"},
		{Actor: "reconciler", Kind: kind, SubjectKind: "backlog_item", SubjectID: "B"},
		{Actor: "reconciler", Kind: "reconciler.tick", SubjectKind: "backlog_item", SubjectID: "A"},
	}
	for i := range seed {
		if err := st.Events.Append(ctx, &seed[i]); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	cases := []struct {
		subjectID string
		kind      string
		want      int
	}{
		{"A", kind, 2},
		{"B", kind, 1},
		{"C", kind, 0},
		{"A", "reconciler.tick", 1},
	}
	for _, tc := range cases {
		got, err := st.Events.CountBySubjectKind(ctx, "backlog_item", tc.subjectID, tc.kind)
		if err != nil {
			t.Fatalf("count %s/%s: %v", tc.subjectID, tc.kind, err)
		}
		if got != tc.want {
			t.Errorf("CountBySubjectKind(%s,%s) = %d, want %d", tc.subjectID, tc.kind, got, tc.want)
		}
	}

	if _, err := st.Events.CountBySubjectKind(ctx, "", "A", kind); err == nil {
		t.Error("empty subjectKind must error")
	}
}

// TestEventDAO_CountByKindSince covers the fleet-wide rolling-window counter that
// backs the auto-requeue per-day cap.
func TestEventDAO_CountByKindSince(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	const kind = "reconciler.auto_requeued"
	now := time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC)

	// 3 in the last 24h, 2 older, 1 of a different kind inside the window.
	seed := []Event{
		{OccurredAt: now.Add(-1 * time.Hour), Actor: "reconciler", Kind: kind, SubjectKind: "backlog_item", SubjectID: "A"},
		{OccurredAt: now.Add(-2 * time.Hour), Actor: "reconciler", Kind: kind, SubjectKind: "backlog_item", SubjectID: "B"},
		{OccurredAt: now.Add(-23 * time.Hour), Actor: "reconciler", Kind: kind, SubjectKind: "backlog_item", SubjectID: "C"},
		{OccurredAt: now.Add(-25 * time.Hour), Actor: "reconciler", Kind: kind, SubjectKind: "backlog_item", SubjectID: "D"},
		{OccurredAt: now.Add(-48 * time.Hour), Actor: "reconciler", Kind: kind, SubjectKind: "backlog_item", SubjectID: "E"},
		{OccurredAt: now.Add(-1 * time.Hour), Actor: "reconciler", Kind: "other", SubjectKind: "backlog_item", SubjectID: "F"},
	}
	for i := range seed {
		if err := st.Events.Append(ctx, &seed[i]); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	got, err := st.Events.CountByKindSince(ctx, kind, now.Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("count-since: %v", err)
	}
	if got != 3 {
		t.Errorf("CountByKindSince(24h) = %d, want 3", got)
	}

	if _, err := st.Events.CountByKindSince(ctx, "", now); err == nil {
		t.Error("empty kind must error")
	}
}
