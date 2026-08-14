package audit

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestIntakeEmitterFuncPreservesStructuredRejection(t *testing.T) {
	want := IntakeEvent{
		Decision: IntakeDecisionRejected,
		Project:  "services/unknown",
		Reason:   IntakeRejectionUnknownRepository,
	}
	var got IntakeEvent
	IntakeEmitterFunc(func(_ context.Context, event IntakeEvent) { got = event }).EmitIntake(context.Background(), want)
	if got != want {
		t.Fatalf("event = %+v, want %+v", got, want)
	}
}

type sweepClient struct {
	issues []DigestIssue
	closed []int64
	err    error
}

func (c *sweepClient) ListOpenIssues(context.Context) ([]DigestIssue, error) { return c.issues, c.err }
func (c *sweepClient) CloseIssue(_ context.Context, iid int64) error {
	c.closed = append(c.closed, iid)
	return nil
}

func TestSweepAuditAdvisoriesIsDryRunByDefaultAndSelectsNarrowly(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	valid := DigestIssue{IID: 7, State: "opened", Author: "mills-bot", Labels: []string{AuditAdvisoryDigestLabel}, CreatedAt: now.Add(-31 * 24 * time.Hour), Title: AuditAdvisoryDigestTitlePrefix + "2026-07-11" + AuditAdvisoryDigestTitleSuffix, Description: AuditAdvisoryDigestMarkerPrefix + "2026-07-11" + AuditAdvisoryDigestMarkerSuffix}
	wrongMarker := valid
	wrongMarker.IID, wrongMarker.Description = 8, "ordinary issue"
	atCutoff := valid
	atCutoff.IID, atCutoff.CreatedAt = 9, now.Add(-30*24*time.Hour)
	client := &sweepClient{issues: []DigestIssue{valid, wrongMarker, atCutoff}}

	got, err := SweepAuditAdvisories(context.Background(), client, AdvisorySweepOptions{Now: now, StaleAfter: 30 * 24 * time.Hour, Author: "mills-bot"})
	if err != nil {
		t.Fatalf("SweepAuditAdvisories() error = %v", err)
	}
	if len(got.Selected) != 1 || got.Selected[0].IID != valid.IID {
		t.Fatalf("Selected = %#v, want only issue %d", got.Selected, valid.IID)
	}
	if len(client.closed) != 0 || len(got.Closed) != 0 {
		t.Fatalf("default dry run closed issues: client=%v result=%v", client.closed, got.Closed)
	}
}

func TestSweepAuditAdvisoriesExecuteClosesSelected(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	issue := DigestIssue{IID: 17, State: "opened", Author: "mills-bot", Labels: []string{AuditAdvisoryDigestLabel}, CreatedAt: now.Add(-48 * time.Hour), Title: AuditAdvisoryDigestTitlePrefix + "2026-08-09" + AuditAdvisoryDigestTitleSuffix, Description: AuditAdvisoryDigestMarkerPrefix + "2026-08-09" + AuditAdvisoryDigestMarkerSuffix}
	client := &sweepClient{issues: []DigestIssue{issue}}

	got, err := SweepAuditAdvisories(context.Background(), client, AdvisorySweepOptions{Now: now, StaleAfter: 24 * time.Hour, Author: "mills-bot", Execute: true})
	if err != nil {
		t.Fatalf("SweepAuditAdvisories() error = %v", err)
	}
	if len(got.Closed) != 1 || got.Closed[0] != issue.IID || len(client.closed) != 1 {
		t.Fatalf("Closed = %v, client calls = %v", got.Closed, client.closed)
	}
}

func TestSweepAuditAdvisoriesFailsBeforeMutationWhenListingFails(t *testing.T) {
	t.Parallel()
	client := &sweepClient{err: errors.New("pagination failed")}
	_, err := SweepAuditAdvisories(context.Background(), client, AdvisorySweepOptions{StaleAfter: time.Hour, Author: "mills-bot", Execute: true})
	if err == nil || len(client.closed) != 0 {
		t.Fatalf("error = %v, close calls = %v", err, client.closed)
	}
}
