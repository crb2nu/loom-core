package audit

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/pipeline"
	"github.com/crb2nu/loom/pkg/mills/store"
)

// fakeIssuer captures every CreateIssue call for assertion.
type fakeIssuer struct {
	mu      sync.Mutex
	calls   []pipeline.IssueRequest
	respIID int64
	respURL string
	err     error
}

func (f *fakeIssuer) CreateIssue(_ context.Context, req pipeline.IssueRequest) (pipeline.IssueResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, req)
	if f.err != nil {
		return pipeline.IssueResponse{}, f.err
	}
	return pipeline.IssueResponse{IID: f.respIID, URL: f.respURL}, nil
}

func (f *fakeIssuer) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeIssuer) lastCall() pipeline.IssueRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return pipeline.IssueRequest{}
	}
	return f.calls[len(f.calls)-1]
}

func newFinding(kind store.AuditSubjectKind, id string, score float64) *store.AuditFinding {
	return &store.AuditFinding{
		SubjectKind:   kind,
		SubjectID:     id,
		Severity:      store.AuditSeverityWarn,
		RubricID:      RubricID,
		SurvivalScore: score,
		Findings: []map[string]any{
			{"id": "F1", "title": "Hidden assumption", "severity": "warn", "detail": "Plan assumes X but never declares it."},
			{"id": "F2", "title": "Tests-vs-spec gap", "severity": "info", "detail": "Slice 2 has no test entry for case A."},
		},
		AuditorPool: []map[string]any{
			{"backend": "flexinfer", "model": "llama-4-70b-instruct", "role": "bulk"},
			{"backend": "flexinfer", "model": "qwen-3-32b", "role": "bulk"},
		},
		CostUSD:   0.12,
		CreatedAt: time.Now().UTC(),
	}
}

func TestFollowup_AboveThresholdNoOp(t *testing.T) {
	is := &fakeIssuer{respIID: 42}
	fu := NewFollowup(is)
	if err := fu.OnRecorded(context.Background(),
		newFinding(store.AuditSubjectCouncilArtifact, "C-OK", 0.92)); err != nil {
		t.Fatalf("OnRecorded: %v", err)
	}
	if is.callCount() != 0 {
		t.Errorf("score above threshold must not create an issue; got %d calls", is.callCount())
	}
}

func TestFollowup_BelowThresholdCreatesIssue(t *testing.T) {
	is := &fakeIssuer{respIID: 1234, respURL: "https://gitlab/example/-/issues/1234"}
	fu := NewFollowup(is)
	finding := newFinding(store.AuditSubjectPipelineMerge, "PIPE-RISKY", 0.45)
	finding.Severity = store.AuditSeverityCritical

	if err := fu.OnRecorded(context.Background(), finding); err != nil {
		t.Fatalf("OnRecorded: %v", err)
	}
	if is.callCount() != 1 {
		t.Fatalf("expected 1 issue, got %d", is.callCount())
	}
	got := is.lastCall()
	for _, want := range []string{
		"Audit follow-up",
		"PIPE-RISKY",
		"survival 0.45",
	} {
		if !strings.Contains(got.Title, want) {
			t.Errorf("title missing %q: %q", want, got.Title)
		}
	}
	for _, want := range []string{
		"Survival score",
		"`audit_v1`",
		"Hidden assumption",
		"Tests-vs-spec gap",
		"## Auditor pool",
		"llama-4-70b-instruct",
	} {
		if !strings.Contains(got.Description, want) {
			t.Errorf("body missing %q in:\n%s", want, got.Description)
		}
	}

	wantLabels := map[string]bool{
		"audit-followup":    true,
		"severity-critical": true,
		"pipeline-merge":    true,
	}
	for _, l := range got.Labels {
		if wantLabels[l] {
			delete(wantLabels, l)
		}
	}
	if len(wantLabels) != 0 {
		t.Errorf("missing labels in %v: still want %v", got.Labels, wantLabels)
	}
}

func TestFollowup_CouncilSubjectGetsCouncilLabel(t *testing.T) {
	is := &fakeIssuer{respIID: 1}
	fu := NewFollowup(is)
	finding := newFinding(store.AuditSubjectCouncilArtifact, "COUNCIL-RISKY", 0.30)
	finding.Severity = store.AuditSeverityCritical

	if err := fu.OnRecorded(context.Background(), finding); err != nil {
		t.Fatalf("OnRecorded: %v", err)
	}
	got := is.lastCall()
	hasCouncil := false
	for _, l := range got.Labels {
		if l == "council-artifact" {
			hasCouncil = true
		}
	}
	if !hasCouncil {
		t.Errorf("council subject must get council-artifact label; got %v", got.Labels)
	}
}

func TestFollowup_IssuerErrorIsSwallowed(t *testing.T) {
	is := &fakeIssuer{err: errors.New("upstream 500")}
	fu := NewFollowup(is)
	finding := newFinding(store.AuditSubjectPipelineMerge, "PIPE-X", 0.20)
	if err := fu.OnRecorded(context.Background(), finding); err != nil {
		t.Errorf("Issuer error should not surface; got %v", err)
	}
}

func TestFollowup_NilIssuerIsNoOpBelowThreshold(t *testing.T) {
	fu := NewFollowup(nil)
	finding := newFinding(store.AuditSubjectPipelineMerge, "PIPE-X", 0.20)
	if err := fu.OnRecorded(context.Background(), finding); err != nil {
		t.Errorf("nil issuer must be a no-op, not an error: %v", err)
	}
}

func TestFollowup_ZeroThresholdFallsBackToDefault(t *testing.T) {
	is := &fakeIssuer{respIID: 1}
	fu := &Followup{Issuer: is /* Threshold left zero */}

	// 0.55 < 0.6 default, so this should fire even when struct field is zero.
	finding := newFinding(store.AuditSubjectPipelineMerge, "PIPE-Z", 0.55)
	if err := fu.OnRecorded(context.Background(), finding); err != nil {
		t.Fatalf("OnRecorded: %v", err)
	}
	if is.callCount() != 1 {
		t.Errorf("zero threshold should fall back to %v; expected issue, got %d calls",
			DefaultFollowupThreshold, is.callCount())
	}
}

func TestFollowup_NilFindingNoOp(t *testing.T) {
	is := &fakeIssuer{}
	fu := NewFollowup(is)
	if err := fu.OnRecorded(context.Background(), nil); err != nil {
		t.Errorf("nil finding must be no-op: %v", err)
	}
	if is.callCount() != 0 {
		t.Errorf("nil finding must not create an issue; got %d", is.callCount())
	}
}

func TestFollowup_DoubleFireCreatesTwoIssues(t *testing.T) {
	// Documented v2.0 limitation: idempotency is not enforced. Test
	// pinned so a future change toward dedup is caught.
	is := &fakeIssuer{respIID: 1}
	fu := NewFollowup(is)
	finding := newFinding(store.AuditSubjectPipelineMerge, "PIPE-DBL", 0.40)

	for i := 0; i < 2; i++ {
		if err := fu.OnRecorded(context.Background(), finding); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if is.callCount() != 2 {
		t.Errorf("v2.0 has no dedup; expected 2 issues, got %d", is.callCount())
	}
}

func TestFollowup_NoFindingsRendersFallbackBody(t *testing.T) {
	is := &fakeIssuer{respIID: 1}
	fu := NewFollowup(is)
	finding := newFinding(store.AuditSubjectPipelineMerge, "PIPE-EMPTY", 0.30)
	finding.Findings = nil

	if err := fu.OnRecorded(context.Background(), finding); err != nil {
		t.Fatalf("OnRecorded: %v", err)
	}
	got := is.lastCall()
	if !strings.Contains(got.Description, "_No structured findings") {
		t.Errorf("empty findings should render fallback body; got:\n%s", got.Description)
	}
}

// Compile-time assertion that the production *clients.GitLabClient
// satisfies our Issuer surface. We don't import clients here (avoid an
// extra package dependency); the operator's test surface will catch
// the integration. This guard mirrors spawner_flexinfer_test.go.
var _ Issuer = (*fakeIssuer)(nil)

// fakeDigestIssuer implements DigestIssuer, simulating GitLab's find-or-create
// digest surface. It tracks which UTC day already has an open digest so
// same-day findings resolve to a comment and new days open a fresh digest —
// exactly the collapse the real GitLab client provides via marker matching.
type fakeDigestIssuer struct {
	mu           sync.Mutex
	creates      []pipeline.IssueRequest
	comments     []digestComment
	openByPeriod map[string]pipeline.IssueRef
	nextIID      int64
	findErr      error
	createErr    error
	commentErr   error
}

type digestComment struct {
	iid  int64
	body string
}

func newFakeDigestIssuer() *fakeDigestIssuer {
	return &fakeDigestIssuer{openByPeriod: map[string]pipeline.IssueRef{}, nextIID: 500}
}

// periodFromDigestMarker extracts the "YYYY-MM-DD" period the writer embedded
// via pipeline.AuditDigestMarker, so the fake can register the created digest
// as open for that day — and, as a side effect, assert the body carries a
// parseable marker.
func periodFromDigestMarker(desc string) string {
	const pfx = "<!-- mills-audit-digest:period="
	i := strings.Index(desc, pfx)
	if i < 0 {
		return ""
	}
	rest := desc[i+len(pfx):]
	j := strings.Index(rest, " -->")
	if j < 0 {
		return ""
	}
	return strings.TrimSpace(rest[:j])
}

func (f *fakeDigestIssuer) CreateIssue(_ context.Context, req pipeline.IssueRequest) (pipeline.IssueResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.creates = append(f.creates, req)
	if f.createErr != nil {
		return pipeline.IssueResponse{}, f.createErr
	}
	iid := f.nextIID
	f.nextIID++
	ref := pipeline.IssueRef{IID: iid, URL: fmt.Sprintf("https://gitlab/example/-/issues/%d", iid)}
	if p := periodFromDigestMarker(req.Description); p != "" {
		f.openByPeriod[p] = ref
	}
	return pipeline.IssueResponse(ref), nil
}

func (f *fakeDigestIssuer) FindOpenAuditDigest(_ context.Context, period string) (pipeline.IssueRef, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.findErr != nil {
		return pipeline.IssueRef{}, false, f.findErr
	}
	ref, ok := f.openByPeriod[period]
	return ref, ok, nil
}

func (f *fakeDigestIssuer) CommentIssue(_ context.Context, iid int64, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.commentErr != nil {
		return f.commentErr
	}
	f.comments = append(f.comments, digestComment{iid: iid, body: body})
	return nil
}

func (f *fakeDigestIssuer) createCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.creates)
}

func (f *fakeDigestIssuer) commentCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.comments)
}

func fixedClock(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

func TestFollowup_DigestFoldsSameDayFindings(t *testing.T) {
	dg := newFakeDigestIssuer()
	fu := NewFollowup(dg)
	fu.Clock = fixedClock(time.Date(2026, 7, 20, 6, 3, 55, 0, time.UTC))

	kinds := []store.AuditSubjectKind{
		store.AuditSubjectCouncilArtifact,
		store.AuditSubjectPipelineMerge,
		store.AuditSubjectCouncilArtifact,
	}
	for i, id := range []string{"COUNCIL-A", "PIPE-B", "COUNCIL-C"} {
		if err := fu.OnRecorded(context.Background(), newFinding(kinds[i], id, 0.45)); err != nil {
			t.Fatalf("OnRecorded %s: %v", id, err)
		}
	}
	if got := dg.createCount(); got != 1 {
		t.Fatalf("same-day findings must open exactly one digest; got %d creates", got)
	}
	if got := dg.commentCount(); got != 2 {
		t.Fatalf("findings after the first must append as comments; got %d", got)
	}
	// The two comments must reference the same digest IID and preserve subject.
	for _, c := range dg.comments {
		if c.iid != 500 {
			t.Errorf("comment targeted iid %d, want the day's digest 500", c.iid)
		}
	}
	if !strings.Contains(dg.comments[0].body, "PIPE-B") {
		t.Errorf("first comment should carry its subject id; got:\n%s", dg.comments[0].body)
	}
}

func TestFollowup_DigestBodyCarriesMarkerLabelsAndEntry(t *testing.T) {
	dg := newFakeDigestIssuer()
	fu := NewFollowup(dg)
	fu.Clock = fixedClock(time.Date(2026, 7, 20, 6, 3, 55, 0, time.UTC))

	finding := newFinding(store.AuditSubjectPipelineMerge, "PIPE-RISKY", 0.45)
	finding.Severity = store.AuditSeverityCritical
	if err := fu.OnRecorded(context.Background(), finding); err != nil {
		t.Fatalf("OnRecorded: %v", err)
	}
	req := dg.creates[0]
	if !strings.Contains(req.Title, "Audit advisory digest — 2026-07-20 (UTC)") {
		t.Errorf("unexpected digest title: %q", req.Title)
	}
	for _, want := range []string{
		"<!-- mills-audit-digest:period=2026-07-20 -->",
		"PIPE-RISKY",
		"survival 0.45",
		"Hidden assumption",
		"Auditor pool",
		"llama-4-70b-instruct",
	} {
		if !strings.Contains(req.Description, want) {
			t.Errorf("digest body missing %q in:\n%s", want, req.Description)
		}
	}
	labels := map[string]bool{}
	for _, l := range req.Labels {
		labels[l] = true
	}
	if !labels["audit-followup"] || !labels["audit-digest"] {
		t.Errorf("digest must carry audit-followup + audit-digest labels; got %v", req.Labels)
	}
}

func TestFollowup_DigestRollsOverByDay(t *testing.T) {
	dg := newFakeDigestIssuer()
	fu := NewFollowup(dg)

	fu.Clock = fixedClock(time.Date(2026, 7, 20, 23, 59, 0, 0, time.UTC))
	if err := fu.OnRecorded(context.Background(),
		newFinding(store.AuditSubjectPipelineMerge, "PIPE-DAY1", 0.40)); err != nil {
		t.Fatalf("day1: %v", err)
	}
	fu.Clock = fixedClock(time.Date(2026, 7, 21, 0, 1, 0, 0, time.UTC))
	if err := fu.OnRecorded(context.Background(),
		newFinding(store.AuditSubjectPipelineMerge, "PIPE-DAY2", 0.40)); err != nil {
		t.Fatalf("day2: %v", err)
	}
	if got := dg.createCount(); got != 2 {
		t.Fatalf("distinct UTC days must open distinct digests; got %d creates", got)
	}
	if got := dg.commentCount(); got != 0 {
		t.Fatalf("no same-day repeat, so no comments; got %d", got)
	}
}

func TestFollowup_DigestAboveThresholdNoOp(t *testing.T) {
	dg := newFakeDigestIssuer()
	fu := NewFollowup(dg)
	fu.Clock = fixedClock(time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC))
	if err := fu.OnRecorded(context.Background(),
		newFinding(store.AuditSubjectCouncilArtifact, "C-OK", 0.90)); err != nil {
		t.Fatalf("OnRecorded: %v", err)
	}
	if dg.createCount() != 0 || dg.commentCount() != 0 {
		t.Errorf("above threshold must not touch the digest; creates=%d comments=%d",
			dg.createCount(), dg.commentCount())
	}
}

func TestFollowup_DigestLookupErrorOpensFreshDigest(t *testing.T) {
	dg := newFakeDigestIssuer()
	dg.findErr = errors.New("gitlab 500")
	fu := NewFollowup(dg)
	fu.Clock = fixedClock(time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC))
	if err := fu.OnRecorded(context.Background(),
		newFinding(store.AuditSubjectPipelineMerge, "PIPE-X", 0.30)); err != nil {
		t.Fatalf("lookup error must not surface: %v", err)
	}
	if dg.createCount() != 1 {
		t.Errorf("lookup failure must fail open to a fresh digest (never drop); got %d creates",
			dg.createCount())
	}
}

func TestFollowup_DigestCommentErrorSwallowed(t *testing.T) {
	dg := newFakeDigestIssuer()
	dg.commentErr = errors.New("notes 500")
	fu := NewFollowup(dg)
	fu.Clock = fixedClock(time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC))

	if err := fu.OnRecorded(context.Background(),
		newFinding(store.AuditSubjectPipelineMerge, "PIPE-1", 0.30)); err != nil {
		t.Fatalf("open digest: %v", err)
	}
	if err := fu.OnRecorded(context.Background(),
		newFinding(store.AuditSubjectPipelineMerge, "PIPE-2", 0.30)); err != nil {
		t.Fatalf("comment error must not surface: %v", err)
	}
	if dg.createCount() != 1 {
		t.Errorf("second same-day finding must not open a new digest; got %d creates", dg.createCount())
	}
}

// Compile-time assertion that the fake satisfies the optional DigestIssuer
// capability the production GitLab client provides. The operator build carries
// the real *clients.GitLabClient guard (var _ audit.DigestIssuer ...).
var _ DigestIssuer = (*fakeDigestIssuer)(nil)
