package health

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakeClient struct {
	pipes []Pipeline
	err   error
	calls int
}

func (f *fakeClient) Pipelines(context.Context, string) ([]Pipeline, error) {
	f.calls++
	return f.pipes, f.err
}
func tp(v time.Time) *time.Time { return &v }

func TestPollerFullStatusMatrixDoesNotFlipOnNonTerminal(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	for _, prior := range []string{"success", "failed"} {
		for _, status := range []string{"created", "waiting_for_resource", "preparing", "pending", "running", "scheduled", "manual", "canceled", "skipped"} {
			t.Run(prior+"_then_"+status, func(t *testing.T) {
				old, newer := now.Add(-2*time.Hour), now.Add(-time.Hour)
				p := &Poller{Ref: "main", Now: func() time.Time { return now }, Client: &fakeClient{pipes: []Pipeline{{Ref: "main", Status: status, FinishedAt: tp(newer)}, {Ref: "main", Status: prior, FinishedAt: tp(old)}}}}
				got, err := p.Poll(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				if got.Green != (prior == "success") {
					t.Fatalf("green=%v after %s must retain %s", got.Green, status, prior)
				}
			})
		}
	}
}
func TestPollerOrdersByFinishedAtAndRetainsOnError(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	old, latest := now.Add(-2*time.Hour), now.Add(-time.Hour)
	f := &fakeClient{pipes: []Pipeline{{Ref: "main", Status: "failed", FinishedAt: tp(old)}, {Ref: "main", Status: "success", FinishedAt: tp(latest)}}}
	p := &Poller{Ref: "main", Now: func() time.Time { return now }, Client: f}
	got, _ := p.Poll(context.Background())
	if !got.Green {
		t.Fatal("latest finished pipeline should win")
	}
	f.err = errors.New("down")
	got, err := p.Poll(context.Background())
	if err == nil || !got.Green {
		t.Fatal("client error must retain observation")
	}
}

func TestPollerRedDurationRecoveryAndImageLag(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	failedAt := now.Add(-90 * time.Minute)
	latestFailureAt := now.Add(-30 * time.Minute)
	millsCommitAt := now.Add(-30 * time.Minute)
	f := &fakeClient{pipes: []Pipeline{
		{Ref: "main", Status: "failed", FinishedAt: tp(latestFailureAt)},
		{Ref: "main", Status: "failed", FinishedAt: tp(failedAt)},
		{Ref: "main", Status: "success", FinishedAt: tp(now.Add(-2 * time.Hour)), MillsCommitAt: tp(millsCommitAt)},
	}}
	p := &Poller{Ref: "main", Now: func() time.Time { return now }, OperatorBuiltAt: now.Add(-2 * time.Hour), Client: f}
	got, err := p.Poll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.Green || got.RedDuration != 90*time.Minute || got.OperatorImageLag != 90*time.Minute {
		t.Fatalf("red observation = %+v", got)
	}

	succeededAt := now.Add(-time.Minute)
	f.pipes = []Pipeline{{Ref: "main", Status: "success", FinishedAt: tp(succeededAt)}}
	got, err = p.Poll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !got.Green || got.RedDuration != 0 || got.OperatorImageLag != 90*time.Minute {
		t.Fatalf("recovered observation = %+v", got)
	}
}

func TestPollerImageLagUsesNewestMillsCommitNotFinishOrder(t *testing.T) {
	// A rerun of an old commit finishes latest: pipeline finish order is not
	// commit order. The lag must come from the maximum Mills commit time in
	// the window, not the first Mills-touching pipeline by finished_at.
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	oldCommit := now.Add(-3 * time.Hour)
	newCommit := now.Add(-20 * time.Minute)
	f := &fakeClient{pipes: []Pipeline{
		// Rerun of the OLD commit, finished most recently.
		{Ref: "main", Status: "success", FinishedAt: tp(now.Add(-5 * time.Minute)), MillsCommitAt: tp(oldCommit)},
		// The newest Mills commit, finished earlier.
		{Ref: "main", Status: "success", FinishedAt: tp(now.Add(-15 * time.Minute)), MillsCommitAt: tp(newCommit)},
	}}
	p := &Poller{Ref: "main", Now: func() time.Time { return now }, OperatorBuiltAt: now.Add(-time.Hour), Client: f}
	got, err := p.Poll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.OperatorImageLag != 40*time.Minute {
		t.Fatalf("image lag = %s, want 40m (newest commit), not the rerun's older commit", got.OperatorImageLag)
	}
}
