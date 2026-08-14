package audit

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/pipeline"
)

type advisorySweepFake struct {
	issues     []DigestIssue
	listErr    error
	closeErrAt int64
	closeCalls []int64
}

func (f *advisorySweepFake) ListOpenIssues(context.Context) ([]DigestIssue, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	var open []DigestIssue
	for _, issue := range f.issues {
		if issue.State == "opened" {
			open = append(open, issue)
		}
	}
	return open, nil
}

func (f *advisorySweepFake) CloseIssue(_ context.Context, iid int64) error {
	f.closeCalls = append(f.closeCalls, iid)
	if iid == f.closeErrAt {
		return errors.New("rate limited")
	}
	for i := range f.issues {
		if f.issues[i].IID == iid {
			f.issues[i].State = "closed"
		}
	}
	return nil
}

func digestFixture(iid int64, created time.Time) DigestIssue {
	const period = "2026-06-01"
	return DigestIssue{
		IID: iid, State: "opened", Author: AuditAdvisoryDigestAuthor,
		Labels: []string{AuditAdvisoryDigestLabel}, CreatedAt: created,
		Title:       AuditAdvisoryDigestTitlePrefix + period + AuditAdvisoryDigestTitleSuffix,
		Description: "digest\n" + AuditAdvisoryDigestMarkerPrefix + period + AuditAdvisoryDigestMarkerSuffix,
	}
}

func TestAuditAdvisoryIdentityMatchesProducer(t *testing.T) {
	t.Parallel()
	if AuditAdvisoryDigestLabel != pipeline.AuditDigestLabel {
		t.Fatalf("sweep label %q drifted from producer label %q", AuditAdvisoryDigestLabel, pipeline.AuditDigestLabel)
	}
	const period = "2026-06-01"
	if AuditAdvisoryDigestMarkerPrefix+period+AuditAdvisoryDigestMarkerSuffix != pipeline.AuditDigestMarker(period) {
		t.Fatal("sweep marker drifted from producer marker")
	}
}

func TestSweepSelectionExclusions(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	cutoff := now.Add(-AuditAdvisoryDefaultStaleAfter)
	valid := digestFixture(1, cutoff.Add(-time.Second))
	boundary := digestFixture(2, cutoff)
	malformed := digestFixture(3, cutoff.Add(-time.Hour))
	malformed.Description = "<!-- mills-audit-digest:period=not-a-date -->"
	wrongLabel := digestFixture(4, cutoff.Add(-time.Hour))
	wrongLabel.Labels = []string{"audit-followup"}
	wrongAuthor := digestFixture(5, cutoff.Add(-time.Hour))
	wrongAuthor.Author = "human"
	closed := digestFixture(6, cutoff.Add(-time.Hour))
	closed.State = "closed"

	fake := &advisorySweepFake{issues: []DigestIssue{valid, boundary, malformed, wrongLabel, wrongAuthor, closed}}
	got, err := SweepAuditAdvisories(context.Background(), fake, AdvisorySweepOptions{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Selected) != 1 || got.Selected[0].IID != valid.IID {
		t.Fatalf("selected = %+v, want only %d", got.Selected, valid.IID)
	}
	if len(fake.closeCalls) != 0 {
		t.Fatalf("dry-run close calls = %v", fake.closeCalls)
	}
	if !got.Cutoff.Equal(cutoff) {
		t.Fatalf("cutoff = %s, want %s", got.Cutoff, cutoff)
	}
}

func TestSweepApplyMatchesDryRunSelection(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	issues := []DigestIssue{digestFixture(10, now.Add(-31*24*time.Hour)), digestFixture(11, now.Add(-32*24*time.Hour))}
	dryFake := &advisorySweepFake{issues: append([]DigestIssue(nil), issues...)}
	dry, err := SweepAuditAdvisories(context.Background(), dryFake, AdvisorySweepOptions{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	applyFake := &advisorySweepFake{issues: append([]DigestIssue(nil), issues...)}
	applied, err := SweepAuditAdvisories(context.Background(), applyFake, AdvisorySweepOptions{Now: now, Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	want := []int64{dry.Selected[0].IID, dry.Selected[1].IID}
	if !reflect.DeepEqual(applied.Closed, want) || !reflect.DeepEqual(applyFake.closeCalls, want) {
		t.Fatalf("closed = %v, calls = %v, want %v", applied.Closed, applyFake.closeCalls, want)
	}
}

func TestSweepListFailureMakesNoMutation(t *testing.T) {
	t.Parallel()
	fake := &advisorySweepFake{listErr: errors.New("page two failed")}
	_, err := SweepAuditAdvisories(context.Background(), fake, AdvisorySweepOptions{Apply: true})
	if err == nil || !strings.Contains(err.Error(), "list issues") {
		t.Fatalf("error = %v", err)
	}
	if len(fake.closeCalls) != 0 {
		t.Fatalf("close calls = %v", fake.closeCalls)
	}
}

func TestSweepPartialFailureReportsPrefixAndRerunsRemainder(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	fake := &advisorySweepFake{
		issues:     []DigestIssue{digestFixture(20, now.Add(-33*24*time.Hour)), digestFixture(21, now.Add(-32*24*time.Hour)), digestFixture(22, now.Add(-31*24*time.Hour))},
		closeErrAt: 21,
	}
	first, err := SweepAuditAdvisories(context.Background(), fake, AdvisorySweepOptions{Now: now, Apply: true})
	if err == nil || !reflect.DeepEqual(first.Closed, []int64{20}) {
		t.Fatalf("error = %v, closed prefix = %v", err, first.Closed)
	}
	fake.closeErrAt = 0
	fake.closeCalls = nil
	second, err := SweepAuditAdvisories(context.Background(), fake, AdvisorySweepOptions{Now: now, Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(second.Closed, []int64{21, 22}) || !reflect.DeepEqual(fake.closeCalls, []int64{21, 22}) {
		t.Fatalf("rerun closed = %v, calls = %v", second.Closed, fake.closeCalls)
	}
}

func TestParseAdvisorySweepFlags(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	dry, err := ParseAdvisorySweepFlags(nil, now)
	if err != nil || dry.Apply || dry.Author != AuditAdvisoryDigestAuthor || dry.StaleAfter != AuditAdvisoryDefaultStaleAfter {
		t.Fatalf("default flags = %+v, error = %v", dry, err)
	}
	apply, err := ParseAdvisorySweepFlags([]string{"-apply", "-author", "audit-bot", "-cutoff", "2026-07-01T00:00:00Z"}, now)
	if err != nil || !apply.Apply || apply.Author != "audit-bot" || !apply.Cutoff.Equal(time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("explicit flags = %+v, error = %v", apply, err)
	}
	if _, err := ParseAdvisorySweepFlags([]string{"-cutoff", "2026-07-01"}, now); err == nil {
		t.Fatal("invalid cutoff accepted")
	}
}
