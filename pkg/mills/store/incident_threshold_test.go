package store

import (
	"context"
	"testing"
	"time"
)

func TestCountExternalDependencyIncidentClustersPerRefAndWindow(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, Options{Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	appendIncident := func(ref string, at time.Time, kind string) {
		t.Helper()
		if err := st.Events.Append(ctx, &Event{
			OccurredAt: at, Actor: "test", Kind: kind,
			SubjectKind: ExternalDependencyIncidentRefSubject, SubjectID: ref,
		}); err != nil {
			t.Fatal(err)
		}
	}
	appendIncident("main", now.Add(-time.Hour), ExternalDependencyIncidentEventKind)
	appendIncident("main", now.Add(-24*time.Hour), ExternalDependencyIncidentEventKind)
	appendIncident("main", now.Add(-24*time.Hour-time.Nanosecond), ExternalDependencyIncidentEventKind)
	appendIncident("release", now.Add(-time.Hour), ExternalDependencyIncidentEventKind)
	appendIncident("main", now.Add(-time.Hour), "unrelated")

	got, err := st.CountExternalDependencyIncidentClusters(ctx, "main", now.Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if got != 2 {
		t.Fatalf("count = %d, want 2", got)
	}
}
