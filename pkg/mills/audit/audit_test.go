package audit

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/store"
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

type executeSweepClient struct {
	issues     map[int64]DigestIssue
	calls      []string
	commentErr int64
	closeErr   int64
}

func (c *executeSweepClient) GetIssue(_ context.Context, iid int64) (DigestIssue, error) {
	c.calls = append(c.calls, fmt.Sprintf("get:%d", iid))
	issue, ok := c.issues[iid]
	if !ok {
		return DigestIssue{}, errors.New("not found")
	}
	return issue, nil
}
func (c *executeSweepClient) CommentIssue(_ context.Context, iid int64, body string) error {
	c.calls = append(c.calls, fmt.Sprintf("comment:%d:%s", iid, body))
	if iid == c.commentErr {
		return errors.New("comment failed")
	}
	return nil
}
func (c *executeSweepClient) CloseIssue(_ context.Context, iid int64) error {
	c.calls = append(c.calls, fmt.Sprintf("close:%d", iid))
	if iid == c.closeErr {
		return errors.New("close failed")
	}
	issue := c.issues[iid]
	issue.State = "closed"
	c.issues[iid] = issue
	return nil
}

func executableDigest(iid int64, now time.Time) DigestIssue {
	return DigestIssue{IID: iid, State: "opened", Author: "mills-bot", Labels: []string{AuditAdvisoryDigestLabel}, CreatedAt: now.Add(-31 * 24 * time.Hour), UpdatedAt: now.Add(-25 * time.Hour), Title: AuditAdvisoryDigestTitlePrefix + "2026-07-11" + AuditAdvisoryDigestTitleSuffix, Description: AuditAdvisoryDigestMarkerPrefix + "2026-07-11" + AuditAdvisoryDigestMarkerSuffix}
}

func TestExecuteApprovedSweepCommentsBeforeClose(t *testing.T) {
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	issue := executableDigest(41, now)
	client := &executeSweepClient{issues: map[int64]DigestIssue{41: issue}}
	got, err := ExecuteApprovedSweep(context.Background(), client, SweepReport{ID: "sweep-2026-08", Author: "mills-bot", Issues: []DigestIssue{issue}}, SweepApproval{SweepID: "sweep-2026-08", ApprovedBy: "operator", Approved: true}, now)
	if err != nil || !reflect.DeepEqual(got.Closed, []int64{41}) {
		t.Fatalf("result=%+v error=%v", got, err)
	}
	if len(client.calls) != 3 || client.calls[0] != "get:41" || !strings.HasPrefix(client.calls[1], "comment:41:") || client.calls[2] != "close:41" || !strings.Contains(client.calls[1], "sweep-2026-08") {
		t.Fatalf("calls=%v", client.calls)
	}
}

func TestExecuteApprovedSweepRequiresMatchingHumanApproval(t *testing.T) {
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	issue := executableDigest(42, now)
	for _, approval := range []SweepApproval{{}, {SweepID: "other", ApprovedBy: "operator", Approved: true}, {SweepID: "sweep", Approved: true}} {
		client := &executeSweepClient{issues: map[int64]DigestIssue{42: issue}}
		if _, err := ExecuteApprovedSweep(context.Background(), client, SweepReport{ID: "sweep", Author: "mills-bot", Issues: []DigestIssue{issue}}, approval, now); err == nil || len(client.calls) != 0 {
			t.Fatalf("approval=%+v error=%v calls=%v", approval, err, client.calls)
		}
	}
}

func TestExecuteApprovedSweepRefusesRecentModificationBoundary(t *testing.T) {
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	for _, updated := range []time.Time{now.Add(-time.Hour), now.Add(-24 * time.Hour)} {
		issue := executableDigest(43, now)
		issue.UpdatedAt = updated
		client := &executeSweepClient{issues: map[int64]DigestIssue{43: issue}}
		_, err := ExecuteApprovedSweep(context.Background(), client, SweepReport{ID: "sweep", Author: "mills-bot", Issues: []DigestIssue{issue}}, SweepApproval{SweepID: "sweep", ApprovedBy: "operator", Approved: true}, now)
		if err == nil || len(client.calls) != 1 {
			t.Fatalf("updated=%s error=%v calls=%v", updated, err, client.calls)
		}
	}
}

func TestExecuteApprovedSweepReportsFailurePrefix(t *testing.T) {
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	one, two := executableDigest(51, now), executableDigest(52, now)
	report := SweepReport{ID: "sweep", Author: "mills-bot", Issues: []DigestIssue{one, two}}
	approval := SweepApproval{SweepID: "sweep", ApprovedBy: "operator", Approved: true}
	client := &executeSweepClient{issues: map[int64]DigestIssue{51: one, 52: two}, closeErr: 52}
	got, err := ExecuteApprovedSweep(context.Background(), client, report, approval, now)
	if err == nil || !reflect.DeepEqual(got.Closed, []int64{51}) {
		t.Fatalf("result=%+v error=%v", got, err)
	}
	client = &executeSweepClient{issues: map[int64]DigestIssue{51: one}, commentErr: 51}
	got, err = ExecuteApprovedSweep(context.Background(), client, SweepReport{ID: "sweep", Author: "mills-bot", Issues: []DigestIssue{one}}, approval, now)
	if err == nil || len(got.Closed) != 0 || client.calls[len(client.calls)-1] == "close:51" {
		t.Fatalf("result=%+v error=%v calls=%v", got, err, client.calls)
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
	if got.Report.ID == "" || got.Report.Author != "mills-bot" || len(got.Report.Issues) != 1 || got.Report.Issues[0].IID != valid.IID {
		t.Fatalf("dry-run report = %+v", got.Report)
	}
}

func TestSweepAuditAdvisoriesExecuteClosesSelected(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	issue := DigestIssue{IID: 17, State: "opened", Author: "mills-bot", Labels: []string{AuditAdvisoryDigestLabel}, CreatedAt: now.Add(-9 * 24 * time.Hour), Title: AuditAdvisoryDigestTitlePrefix + "2026-08-09" + AuditAdvisoryDigestTitleSuffix, Description: AuditAdvisoryDigestMarkerPrefix + "2026-08-09" + AuditAdvisoryDigestMarkerSuffix}
	client := &sweepClient{issues: []DigestIssue{issue}}

	got, err := SweepAuditAdvisories(context.Background(), client, AdvisorySweepOptions{Now: now, StaleAfter: 8 * 24 * time.Hour, Author: "mills-bot", Execute: true})
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

func TestSweepCensusIncludesRecentAndNonBotDigests(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	client := &sweepClient{}
	for i, offset := range []time.Duration{-time.Nanosecond, 0, time.Nanosecond} {
		client.issues = append(client.issues, DigestIssue{IID: int64(i + 1), State: "opened", Labels: []string{AuditAdvisoryDigestLabel}, CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-24 * time.Hour).Add(offset)})
	}
	got, err := SweepAuditAdvisories(context.Background(), client, AdvisorySweepOptions{Now: now})
	if err != nil || len(got.Digests) != 3 || len(got.Selected) != 0 || len(client.closed) != 0 {
		t.Fatalf("result=%+v err=%v", got, err)
	}
	for i, digest := range got.Digests {
		if digest.Age != time.Hour || digest.RecentlyModified != (i != 0) {
			t.Fatalf("digest=%+v", digest)
		}
	}
}

type previousDigestIssuer struct {
	*fakeDigestIssuer
	body   string
	found  bool
	err    error
	period string
}

func (d *previousDigestIssuer) FindAuditDigestBody(_ context.Context, period string) (string, bool, error) {
	d.period = period
	return d.body, d.found, d.err
}

func TestPreviousDayDigestDedupe(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		found, changed, failed bool
		want                   int
	}{
		{"identical", true, false, false, 0},
		{"changed", true, true, false, 1},
		{"missing", false, false, false, 1},
		{"lookup failure", true, false, true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			issuer := &previousDigestIssuer{fakeDigestIssuer: newFakeDigestIssuer(), found: tc.found}
			f := NewFollowup(issuer)
			f.Clock = fixedClock(time.Date(2026, 9, 15, 0, 30, 0, 0, time.UTC))
			finding := newFinding(store.AuditSubjectCouncilArtifact, "same-finding", 0.45)
			f.Clock = fixedClock(time.Date(2026, 9, 14, 1, 0, 0, 0, time.UTC))
			issuer.body = f.digestBody("2026-09-14", finding)
			f.Clock = fixedClock(time.Date(2026, 9, 15, 0, 30, 0, 0, time.UTC))
			if tc.changed {
				issuer.body += "material change"
			}
			if tc.failed {
				issuer.err = errors.New("unavailable")
			}
			if err := f.OnRecorded(context.Background(), finding); err != nil {
				t.Fatal(err)
			}
			if issuer.createCount() != tc.want || issuer.period != "2026-09-14" {
				t.Fatalf("creates=%d period=%s", issuer.createCount(), issuer.period)
			}
		})
	}
}

func TestDigestNormalizationPreservesFindingDates(t *testing.T) {
	if NormalizeDigestBody("finding on 2026-09-14") == NormalizeDigestBody("finding on 2026-09-15") {
		t.Fatal("finding dates must remain significant")
	}
	for _, args := range [][]string{{"--dry-run"}, {"--dry-run", "--apply"}} {
		_, err := ParseAdvisorySweepFlags(args, time.Now())
		if (err != nil) != (len(args) == 2) {
			t.Fatalf("args=%v err=%v", args, err)
		}
	}
}
