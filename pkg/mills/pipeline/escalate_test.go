package pipeline

import (
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/gates"
	"github.com/crb2nu/loom/pkg/mills/store"
	"github.com/crb2nu/loom/pkg/telemetry"
)

type fakeIssue struct {
	mu    sync.Mutex
	calls []IssueRequest
	resp  IssueResponse
	err   error
}

func (f *fakeIssue) CreateIssue(_ context.Context, req IssueRequest) (IssueResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, req)
	if f.err != nil {
		return IssueResponse{}, f.err
	}
	return f.resp, nil
}

type fakeHandoff struct {
	mu    sync.Mutex
	calls []HandoffRequest
	resp  HandoffResponse
	err   error
}

func (f *fakeHandoff) CreateHandoff(_ context.Context, req HandoffRequest) (HandoffResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, req)
	if f.err != nil {
		return HandoffResponse{}, f.err
	}
	return f.resp, nil
}

type issueComment struct {
	IID  int64
	Body string
}

// fakeDedupIssue implements IssueClient, DedupIssueClient, and
// ClosableIssueClient so the escalator exercises the dedup + auto-close paths.
type fakeDedupIssue struct {
	mu                 sync.Mutex
	createCalls        []IssueRequest
	comments           []issueComment
	closed             []int64
	closeCalls         []int64
	findBacklogIDs     []string
	findFailureClasses []string
	findRef            IssueRef
	findFound          bool
	findByFailureClass map[string]IssueRef
	findErr            error
	listRefs           []IssueRef
	listErr            error
	createResp         IssueResponse
	createErr          error
	commentErr         error
	closeErr           error
	closeErrByIID      map[int64]error
}

// fakeLegacyDedupIssue intentionally implements only the pre-class-aware
// optional interfaces, proving additive capability detection remains backward
// compatible for external IssueClient implementations.
type fakeLegacyDedupIssue struct {
	createCalls []IssueRequest
	comments    []issueComment
	closed      []int64
	ref         IssueRef
	found       bool
}

func (f *fakeLegacyDedupIssue) CreateIssue(_ context.Context, req IssueRequest) (IssueResponse, error) {
	f.createCalls = append(f.createCalls, req)
	return IssueResponse{IID: 99, URL: "https://gl/issues/99"}, nil
}

func (f *fakeLegacyDedupIssue) FindOpenEscalation(_ context.Context, _ string) (IssueRef, bool, error) {
	return f.ref, f.found, nil
}

func (f *fakeLegacyDedupIssue) CommentIssue(_ context.Context, iid int64, body string) error {
	f.comments = append(f.comments, issueComment{IID: iid, Body: body})
	return nil
}

func (f *fakeLegacyDedupIssue) CloseIssue(_ context.Context, iid int64) error {
	f.closed = append(f.closed, iid)
	return nil
}

func (f *fakeDedupIssue) CreateIssue(_ context.Context, req IssueRequest) (IssueResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createCalls = append(f.createCalls, req)
	if f.createErr != nil {
		return IssueResponse{}, f.createErr
	}
	return f.createResp, nil
}

func (f *fakeDedupIssue) FindOpenEscalation(_ context.Context, backlogID string) (IssueRef, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.findBacklogIDs = append(f.findBacklogIDs, backlogID)
	return f.findRef, f.findFound, f.findErr
}

func (f *fakeDedupIssue) FindOpenEscalationByClass(_ context.Context, backlogID, failureClass string) (IssueRef, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.findBacklogIDs = append(f.findBacklogIDs, backlogID)
	f.findFailureClasses = append(f.findFailureClasses, failureClass)
	if f.findByFailureClass != nil {
		ref, found := f.findByFailureClass[failureClass]
		return ref, found, f.findErr
	}
	return f.findRef, f.findFound, f.findErr
}

func (f *fakeDedupIssue) CommentIssue(_ context.Context, iid int64, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.comments = append(f.comments, issueComment{IID: iid, Body: body})
	return f.commentErr
}

func (f *fakeDedupIssue) ListOpenEscalations(_ context.Context, _ string) ([]IssueRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	if f.listRefs != nil {
		return append([]IssueRef(nil), f.listRefs...), nil
	}
	if f.findFound {
		return []IssueRef{f.findRef}, nil
	}
	return nil, nil
}

func (f *fakeDedupIssue) CloseIssue(_ context.Context, iid int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closeCalls = append(f.closeCalls, iid)
	if err := f.closeErrByIID[iid]; err != nil {
		return err
	}
	if f.closeErr != nil {
		return f.closeErr
	}
	f.closed = append(f.closed, iid)
	return nil
}

func newEscalateEnv(t *testing.T) (*store.Store, *store.PipelineRun, *store.BacklogItem) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(context.Background(), store.Options{Path: filepath.Join(dir, "h.db")})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	item := &store.BacklogItem{
		ID: "BL-ESC-1", Title: "x", State: store.BacklogQueued, Priority: store.P1,
	}
	if err := st.Backlog.Put(ctx, item); err != nil {
		t.Fatalf("seed backlog: %v", err)
	}
	now := time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC)
	run := &store.PipelineRun{
		ID: "PIPE-ESC-1", BacklogID: item.ID, Template: "x",
		State: store.PipelineImplementing, Attempts: 1, StartedAt: now,
		CostUSD: 0.42,
	}
	if err := st.Pipeline.PutRun(ctx, run); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	// Seed two stage_results so the failure record has rows.
	out := store.StageOutcomeError
	end := now.Add(time.Second)
	for _, s := range []string{"plan_slice", "implement"} {
		oc := out
		if s == "plan_slice" {
			ok := store.StageOutcomeSuccess
			oc = ok
		}
		if err := st.Pipeline.PutStage(ctx, &store.StageResult{
			PipelineRunID: run.ID,
			Stage:         s,
			Attempt:       1,
			StartedAt:     now,
			EndedAt:       &end,
			Outcome:       &oc,
			CostUSD:       0.05,
			LogTail:       "line1\nline2\nline3\nfinal failure: boom",
		}); err != nil {
			t.Fatalf("seed stage %s: %v", s, err)
		}
	}
	if err := st.Pipeline.PutGate(ctx, &store.GateOutcome{
		PipelineRunID: run.ID, AfterStage: "post_implement_gate",
		GateName: "diff_size", Outcome: store.GateOutcomeFail,
		Reasons:     []string{"diff > 800 lines"},
		EvaluatedAt: now,
	}); err != nil {
		t.Fatalf("seed gate: %v", err)
	}
	return st, run, item
}

func TestEscalationClassDedupMarker_NormalizesClassAndPreservesLegacy(t *testing.T) {
	if got, want := EscalationClassDedupMarker("BL-1", "  CODE  "), "<!-- mills-escalation:backlog=BL-1;class=code -->"; got != want {
		t.Fatalf("class marker = %q, want %q", got, want)
	}
	if got, want := EscalationClassDedupMarker("BL-1", ""), EscalationDedupMarker("BL-1"); got != want {
		t.Fatalf("empty-class marker = %q, want legacy %q", got, want)
	}
}

func TestEscalator_BuildRecord(t *testing.T) {
	st, run, item := newEscalateEnv(t)
	e := NewEscalator(st, nil, nil)
	rec, err := e.BuildRecord(context.Background(), run, item, "test reason")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if rec.BacklogID != item.ID || rec.PipelineRunID != run.ID {
		t.Errorf("ids wrong: %+v", rec)
	}
	if rec.Reason != "test reason" {
		t.Errorf("reason = %q", rec.Reason)
	}
	if len(rec.StageStack) != 2 {
		t.Errorf("stage stack len = %d, want 2", len(rec.StageStack))
	}
	if len(rec.GateVerdicts) != 1 || rec.GateVerdicts[0].Outcome != string(store.GateOutcomeFail) {
		t.Errorf("gate verdicts wrong: %+v", rec.GateVerdicts)
	}
	if !strings.Contains(rec.LastLogTail, "boom") {
		t.Errorf("log tail missing recent line: %q", rec.LastLogTail)
	}
}

func TestEscalator_BuildRecordEmitsStructuredClassification(t *testing.T) {
	st, run, item := newEscalateEnv(t)
	e := NewEscalator(st, nil, nil)
	reason := "stage ci_watch terminal config error (not retried) [class=config]: gitlab: GET /projects/47/pipelines: status 401: unauthorized"
	run.EscalationClass = "config" // stamped by escalateWithItem in production
	run.FailureClass = "configuration"
	run.ExternalDependencyID = "external_dependency.gitlab.auth_failure"
	run.ExternalDependency = "gitlab"
	retryable := false
	run.EscalationRetryable = &retryable
	exhausted := true
	run.RetryExhausted = &exhausted
	rec, err := e.BuildRecord(context.Background(), run, item, reason)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if rec.Classification == nil {
		t.Fatal("classification = nil, want structured payload")
	}
	if rec.RetryExhausted == nil || !*rec.RetryExhausted || rec.Classification.RetryExhausted == nil || !*rec.Classification.RetryExhausted {
		t.Fatalf("retry exhaustion not propagated: %+v", rec)
	}
	if rec.Classification.EscalationClass != telemetry.EscalationClassExternalDependency ||
		rec.Classification.FailureClass != "configuration" ||
		rec.Classification.Retryable == nil ||
		*rec.Classification.Retryable ||
		rec.Classification.ExternalDependencyID != "external_dependency.gitlab.auth_failure" ||
		rec.Classification.ExternalDependency != "gitlab" {
		t.Fatalf("classification = %+v", rec.Classification)
	}
	if rec.Classification.Classifier != FailureClassifierName {
		t.Fatalf("classifier = %q, want %q", rec.Classification.Classifier, FailureClassifierName)
	}
	if rec.Classification.FreeRetry == nil || *rec.Classification.FreeRetry ||
		rec.Classification.Terminal == nil || !*rec.Classification.Terminal {
		t.Fatalf("retry semantics = %+v", rec.Classification)
	}
	if rec.EscalationClass != rec.Classification.EscalationClass ||
		rec.FailureClass != rec.Classification.FailureClass ||
		rec.Retryable != rec.Classification.Retryable ||
		rec.ExternalDependencyID != rec.Classification.ExternalDependencyID ||
		rec.ExternalDependency != rec.Classification.ExternalDependency {
		t.Fatalf("legacy fields diverged from classification: rec=%+v", rec)
	}
	body, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{
		`"classification"`,
		`"classifier":"mills-failure-classifier"`,
		`"escalation_class":"external_dependency"`,
		`"failure_class":"configuration"`,
		`"retryable":false`,
		`"free_retry":false`,
		`"terminal":true`,
		`"external_dependency_id":"external_dependency.gitlab.auth_failure"`,
		`"external_dependency":"gitlab"`,
	} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("failure record JSON missing %s: %s", want, body)
		}
	}
}

func TestEscalationMetadataFromEvidence_RoutesExternalDependency(t *testing.T) {
	tests := []struct {
		name   string
		class  ErrorClass
		reason string
		want   string
	}{
		{
			name:   "tagged code-like failure",
			class:  ClassCode,
			reason: "job failed: external_dependency.gitlab.auth_failure",
			want:   telemetry.EscalationClassExternalDependency,
		},
		{name: "untagged code", class: ClassCode, reason: "compile failed", want: string(ClassCode)},
		{name: "untagged infrastructure", class: ClassInfra, reason: "worker failed", want: string(ClassInfra)},
		{name: "untagged configuration", class: ClassConfig, reason: "invalid project setting", want: string(ClassConfig)},
		{name: "untagged transient", class: ClassTransient, reason: "temporary failure", want: string(ClassTransient)},
		{name: "untagged unknown", reason: "", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			md := escalationMetadataFromEvidence(tt.class, tt.reason, "")
			if tt.want == telemetry.EscalationClassExternalDependency {
				md = routeExternalDependencyEscalation(md, "external_dependency.gitlab.auth_failure", "gitlab")
			}
			if md.EscalationClass != tt.want {
				t.Fatalf("EscalationClass = %q, want %q", md.EscalationClass, tt.want)
			}
			if tt.want == telemetry.EscalationClassExternalDependency {
				if md.ExternalDependencyID != "external_dependency.gitlab.auth_failure" || md.ExternalDependency != "gitlab" {
					t.Fatalf("classifier metadata = %q/%q, want preserved gitlab tag", md.ExternalDependencyID, md.ExternalDependency)
				}
				if got := escalationClassLabel(ErrorClass(md.EscalationClass), tt.reason, ""); got != telemetry.EscalationClassExternalDependency {
					t.Fatalf("metric label = %q, want %q", got, telemetry.EscalationClassExternalDependency)
				}
			}
		})
	}
}

func TestEscalator_BuildRecordClassificationRetrySemantics(t *testing.T) {
	st, run, item := newEscalateEnv(t)
	e := NewEscalator(st, nil, nil)
	// escalateWithItem stamps the class onto the run before the escalator
	// runs; BuildRecord reads it from there now, not from the prose.
	run.EscalationClass = "transient_quota"
	rec, err := e.BuildRecord(context.Background(), run, item, "stage plan_slice errored after cap [class=transient_quota]: spawn pool busy")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	c := rec.Classification
	if c == nil {
		t.Fatal("classification = nil, want structured payload")
	}
	if c.Classifier != FailureClassifierName || c.FailureClass != string(FailureTransientQuota) {
		t.Fatalf("classification = %+v", c)
	}
	if c.Retryable == nil || !*c.Retryable ||
		c.FreeRetry == nil || !*c.FreeRetry ||
		c.Terminal == nil || *c.Terminal {
		t.Fatalf("retry semantics = %+v", c)
	}
}

func TestEscalator_HandlePostsIssueAndHandoff(t *testing.T) {
	st, run, item := newEscalateEnv(t)
	issue := &fakeIssue{resp: IssueResponse{IID: 99, URL: "https://gl/issues/99"}}
	handoff := &fakeHandoff{resp: HandoffResponse{HandoffID: "h-1"}}
	e := NewEscalator(st, issue, handoff)
	e.HandTo = "human-on-call"
	if err := e.Handle(context.Background(), run, item, "stage X exceeded retries"); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if len(issue.calls) != 1 {
		t.Fatalf("issue calls = %d", len(issue.calls))
	}
	got := issue.calls[0]
	if !strings.Contains(got.Title, run.ID) {
		t.Errorf("issue title missing run id: %q", got.Title)
	}
	if !containsAll(got.Labels, "mills-escalation", "kind/incident", "priority/P1") {
		t.Errorf("labels missing: %v", got.Labels)
	}
	if !strings.Contains(got.Description, "Stage history") {
		t.Errorf("issue body missing stage history")
	}
	if !strings.Contains(got.Description, "diff_size") {
		t.Errorf("issue body missing gate verdicts")
	}
	if len(handoff.calls) != 1 {
		t.Fatalf("handoff calls = %d", len(handoff.calls))
	}
	hr := handoff.calls[0]
	if hr.To != "human-on-call" {
		t.Errorf("handoff to = %q", hr.To)
	}
	if hr.IssueURL != "https://gl/issues/99" {
		t.Errorf("issue url not propagated to handoff")
	}
}

func TestEscalator_IssueBodyFormatsExternalDependencyIncident(t *testing.T) {
	st, run, item := newEscalateEnv(t)
	issue := &fakeIssue{resp: IssueResponse{IID: 99, URL: "https://gl/issues/99"}}
	handoff := &fakeHandoff{}
	e := NewEscalator(st, issue, handoff)

	reason := "stage ci_watch terminal config error (not retried) [class=config]: gitlab: GET /projects/47/pipelines: status 401: unauthorized"
	run.EscalationClass = "config" // stamped by escalateWithItem in production
	if err := e.Handle(context.Background(), run, item, reason); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if len(issue.calls) != 1 {
		t.Fatalf("issue calls = %d, want 1", len(issue.calls))
	}
	body := issue.calls[0].Description
	for _, want := range []string{
		"### External dependency incident",
		"**Incident class**: `external_dependency_incident`",
		"**Disposition**: `wait_for_dependency_recovery`",
		"**Failure class**: `configuration`",
		"**Retryable**: `false`",
		"**Free retry**: `false`",
		"**Terminal**: `true`",
		"**Classifier**: `mills-failure-classifier`",
		"**External dependency**: `gitlab`",
		"**Runbook**: `docs/mills-escalation-and-dependency-failures.md`",
		"Do not create speculative in-repo remediation work",
		"### Stage history",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("issue body missing %q:\n%s", want, body)
		}
	}
	if len(handoff.calls) != 1 {
		t.Fatalf("handoff calls = %d, want 1", len(handoff.calls))
	}
	rec, ok := handoff.calls[0].Context["failure_record"].(*FailureRecord)
	if !ok {
		t.Fatalf("handoff failure_record = %#v, want *FailureRecord", handoff.calls[0].Context["failure_record"])
	}
	if rec.FailureClass != "configuration" || rec.Retryable == nil || *rec.Retryable ||
		rec.ExternalDependencyID != "external_dependency.gitlab.auth_failure" ||
		rec.ExternalDependency != "gitlab" {
		t.Fatalf("handoff metadata = %+v", rec)
	}
	cls, ok := handoff.calls[0].Context["classification"].(EscalationClassification)
	if !ok {
		t.Fatalf("handoff classification = %#v, want EscalationClassification", handoff.calls[0].Context["classification"])
	}
	if rec.Classification == nil || cls != *rec.Classification {
		t.Fatalf("handoff classification = %+v, record = %+v", cls, rec.Classification)
	}
	if cls.Classifier != FailureClassifierName ||
		cls.FreeRetry == nil || *cls.FreeRetry ||
		cls.Terminal == nil || !*cls.Terminal {
		t.Fatalf("handoff classification semantics = %+v", cls)
	}
	persisted, err := st.Pipeline.GetRun(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if persisted.EscalationClass != "config" ||
		persisted.FailureClass != "configuration" ||
		persisted.ExternalDependencyID != "external_dependency.gitlab.auth_failure" ||
		persisted.ExternalDependency != "gitlab" ||
		persisted.EscalationRetryable == nil ||
		*persisted.EscalationRetryable {
		t.Fatalf("persisted metadata = %+v", persisted)
	}
}

func TestEscalator_RepeatedExternalDependencyIncidentsUseDegradedMode(t *testing.T) {
	st, run, item := newEscalateEnv(t)
	now := time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC)
	seedExternalIncidentRun(t, st, "PIPE-OLD-1", item.ID, 2, now.Add(-2*time.Hour))
	seedExternalIncidentRun(t, st, "PIPE-OLD-2", item.ID, 3, now.Add(-1*time.Hour))

	issue := &fakeIssue{resp: IssueResponse{IID: 99, URL: "https://gl/issues/99"}}
	handoff := &fakeHandoff{}
	e := NewEscalator(st, issue, handoff)
	e.Clock = func() time.Time { return now }

	reason := "stage ci_watch terminal config error (not retried) [class=config]: gitlab: GET /projects/47/pipelines: status 401: unauthorized"
	run.EscalationClass = "config" // stamped by escalateWithItem in production
	if err := e.Handle(context.Background(), run, item, reason); err != nil {
		t.Fatalf("handle: %v", err)
	}

	if len(issue.calls) != 1 {
		t.Fatalf("issue calls = %d, want 1", len(issue.calls))
	}
	gotIssue := issue.calls[0]
	if !containsAll(gotIssue.Labels, "mode/degraded", "incident/external-dependency", "dependency/gitlab") {
		t.Fatalf("issue labels = %v, want degraded external dependency labels", gotIssue.Labels)
	}
	for _, want := range []string{
		"### Degraded external dependency mode",
		"**Escalation mode**: `external_dependency_degraded`",
		"**Matching incidents**: `3` in `24h`",
		"This dependency has produced `3` matching escalations in `24h`",
		"Pause local retry churn for this dependency",
		"### External dependency incident",
	} {
		if !strings.Contains(gotIssue.Description, want) {
			t.Fatalf("issue body missing %q:\n%s", want, gotIssue.Description)
		}
	}

	if len(handoff.calls) != 1 {
		t.Fatalf("handoff calls = %d, want 1", len(handoff.calls))
	}
	rec, ok := handoff.calls[0].Context["failure_record"].(*FailureRecord)
	if !ok {
		t.Fatalf("handoff failure_record = %#v, want *FailureRecord", handoff.calls[0].Context["failure_record"])
	}
	if rec.EscalationMode != ExternalIncidentDegradedMode ||
		rec.IncidentCount != 3 ||
		rec.IncidentWindow != "24h" ||
		rec.Classification == nil ||
		rec.Classification.EscalationMode != ExternalIncidentDegradedMode ||
		rec.Classification.IncidentCount != 3 ||
		rec.Classification.IncidentWindow != "24h" {
		t.Fatalf("handoff degraded metadata = record %+v classification %+v", rec, rec.Classification)
	}
	cls, ok := handoff.calls[0].Context["classification"].(EscalationClassification)
	if !ok {
		t.Fatalf("handoff classification = %#v, want EscalationClassification", handoff.calls[0].Context["classification"])
	}
	if cls.EscalationMode != ExternalIncidentDegradedMode || cls.IncidentCount != 3 || cls.IncidentWindow != "24h" {
		t.Fatalf("handoff classification degraded metadata = %+v", cls)
	}

	events, err := st.Events.ListSince(context.Background(), time.Time{}, 10)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	var payload map[string]any
	for _, ev := range events {
		if ev.Kind == "pipeline.escalation.published" {
			payload = ev.Payload
			break
		}
	}
	if payload == nil {
		t.Fatalf("missing pipeline.escalation.published event in %+v", events)
	}
	if payload["escalation_mode"] != ExternalIncidentDegradedMode ||
		payload["incident_count"] != float64(3) ||
		payload["incident_window"] != "24h" {
		t.Fatalf("event degraded fields = %+v", payload)
	}
	classification, ok := payload["classification"].(map[string]any)
	if !ok {
		t.Fatalf("event classification = %#v, want object", payload["classification"])
	}
	if classification["escalation_mode"] != ExternalIncidentDegradedMode ||
		classification["incident_count"] != float64(3) ||
		classification["incident_window"] != "24h" {
		t.Fatalf("event classification degraded fields = %+v", classification)
	}
}

func TestEscalator_HandlePublishesClassificationOnEvent(t *testing.T) {
	st, run, item := newEscalateEnv(t)
	issue := &fakeIssue{resp: IssueResponse{IID: 99, URL: "https://gl/issues/99"}}
	e := NewEscalator(st, issue, nil)

	reason := "stage ci_watch terminal config error (not retried) [class=config]: gitlab: GET /projects/47/pipelines: status 401: unauthorized"
	run.EscalationClass = "config" // stamped by escalateWithItem in production
	if err := e.Handle(context.Background(), run, item, reason); err != nil {
		t.Fatalf("handle: %v", err)
	}

	events, err := st.Events.ListSince(context.Background(), time.Time{}, 10)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	var payload map[string]any
	for _, ev := range events {
		if ev.Kind == "pipeline.escalation.published" {
			payload = ev.Payload
			break
		}
	}
	if payload == nil {
		t.Fatalf("missing pipeline.escalation.published event in %+v", events)
	}
	if payload["failure_class"] != "configuration" ||
		payload["escalation_class"] != "config" ||
		payload["retryable"] != false ||
		payload["free_retry"] != false ||
		payload["terminal"] != true ||
		payload["classifier"] != FailureClassifierName ||
		payload["external_dependency_id"] != "external_dependency.gitlab.auth_failure" ||
		payload["external_dependency"] != "gitlab" {
		t.Fatalf("event classification fields = %+v", payload)
	}
	classification, ok := payload["classification"].(map[string]any)
	if !ok {
		t.Fatalf("event classification = %#v, want object", payload["classification"])
	}
	if classification["failure_class"] != "configuration" ||
		classification["escalation_class"] != "config" ||
		classification["retryable"] != false ||
		classification["free_retry"] != false ||
		classification["terminal"] != true ||
		classification["classifier"] != FailureClassifierName ||
		classification["external_dependency_id"] != "external_dependency.gitlab.auth_failure" ||
		classification["external_dependency"] != "gitlab" {
		t.Fatalf("event classification = %+v", classification)
	}
}

func seedExternalIncidentRun(t *testing.T, st *store.Store, id, backlogID string, attempt int, startedAt time.Time) {
	t.Helper()
	retryable := false
	if err := st.Pipeline.PutRun(context.Background(), &store.PipelineRun{
		ID:                   id,
		BacklogID:            backlogID,
		Template:             "x",
		State:                store.PipelineEscalated,
		Attempts:             attempt,
		StartedAt:            startedAt,
		CostUSD:              0.10,
		EscalationClass:      "config",
		FailureClass:         "configuration",
		ExternalDependencyID: "external_dependency.gitlab.auth_failure",
		ExternalDependency:   "gitlab",
		EscalationRetryable:  &retryable,
	}); err != nil {
		t.Fatalf("seed external incident run %s: %v", id, err)
	}
}

func TestFailureRecord_AddClassificationToPayloadEnrichesLegacyRecord(t *testing.T) {
	retryable := true
	rec := &FailureRecord{
		EscalationClass:      string(ClassInfra),
		FailureClass:         string(FailureInfrastructure),
		Retryable:            &retryable,
		ExternalDependencyID: "external_dependency.gitlab.api_unavailable",
		ExternalDependency:   "gitlab",
	}
	payload := map[string]any{}

	rec.addClassificationToPayload(payload)

	if payload["failure_class"] != string(FailureInfrastructure) ||
		payload["escalation_class"] != string(ClassInfra) ||
		payload["retryable"] != true ||
		payload["free_retry"] != false ||
		payload["terminal"] != false ||
		payload["classifier"] != FailureClassifierName ||
		payload["external_dependency_id"] != "external_dependency.gitlab.api_unavailable" ||
		payload["external_dependency"] != "gitlab" {
		t.Fatalf("payload classification fields = %+v", payload)
	}
	classification, ok := payload["classification"].(EscalationClassification)
	if !ok {
		t.Fatalf("classification = %#v, want EscalationClassification", payload["classification"])
	}
	if classification.FailureClass != string(FailureInfrastructure) ||
		classification.EscalationClass != string(ClassInfra) ||
		classification.Retryable == nil ||
		!*classification.Retryable ||
		classification.FreeRetry == nil ||
		*classification.FreeRetry ||
		classification.Terminal == nil ||
		*classification.Terminal ||
		classification.Classifier != FailureClassifierName ||
		classification.ExternalDependencyID != "external_dependency.gitlab.api_unavailable" ||
		classification.ExternalDependency != "gitlab" {
		t.Fatalf("classification = %+v", classification)
	}
}

func TestEscalator_RecurrenceNoteIncludesClassification(t *testing.T) {
	st, run, item := newEscalateEnv(t)
	issue := &fakeDedupIssue{findFound: true, findRef: IssueRef{IID: 42, URL: "https://gl/issues/42"}}
	e := NewEscalator(st, issue, nil)
	reason := "stage ci_watch terminal config error (not retried) [class=config]: gitlab: GET /projects/47/pipelines: status 401: unauthorized"
	run.EscalationClass = "config" // stamped by escalateWithItem in production
	if err := e.Handle(context.Background(), run, item, reason); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if len(issue.comments) != 1 {
		t.Fatalf("expected 1 recurrence comment, got %d", len(issue.comments))
	}
	body := issue.comments[0].Body
	for _, want := range []string{
		"**Failure class**: `configuration`",
		"**Retryable**: `false`",
		"**Free retry**: `false`",
		"**Terminal**: `true`",
		"**Classifier**: `mills-failure-classifier`",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("recurrence note missing %q:\n%s", want, body)
		}
	}
}

func TestEscalator_IssueFailureDoesNotBlockHandoff(t *testing.T) {
	st, run, item := newEscalateEnv(t)
	issue := &fakeIssue{err: errIssueDown}
	handoff := &fakeHandoff{}
	e := NewEscalator(st, issue, handoff)
	if err := e.Handle(context.Background(), run, item, "x"); err != nil {
		t.Fatalf("handle should swallow issue errors: %v", err)
	}
	if len(handoff.calls) != 1 {
		t.Errorf("handoff should still fire when issue fails")
	}
}

// TestEscalator_DedupReusesOpenIssue guards DEBT-073 (#167 criterion e): when an
// OPEN escalation issue already exists for the backlog item, the escalator must
// append a recurrence note to it and reuse its URL rather than file a duplicate.
func TestEscalator_DedupReusesOpenIssue(t *testing.T) {
	st, run, item := newEscalateEnv(t)
	issue := &fakeDedupIssue{findFound: true, findRef: IssueRef{IID: 42, URL: "https://gl/issues/42"}}
	handoff := &fakeHandoff{}
	e := NewEscalator(st, issue, handoff)
	reason := "stage tests errored deterministically (not retried) [class=code]: assertion failed"
	run.EscalationClass = "code" // stamped by escalateWithItem in production
	if err := e.Handle(context.Background(), run, item, reason); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if len(issue.createCalls) != 0 {
		t.Errorf("must NOT file a duplicate when an issue is open; got %d creates", len(issue.createCalls))
	}
	if len(issue.comments) != 1 {
		t.Fatalf("expected 1 recurrence comment, got %d", len(issue.comments))
	}
	if issue.comments[0].IID != 42 {
		t.Errorf("commented on wrong issue: iid=%d", issue.comments[0].IID)
	}
	if !strings.Contains(issue.comments[0].Body, run.ID) {
		t.Errorf("recurrence note missing run id: %q", issue.comments[0].Body)
	}
	if len(issue.findFailureClasses) != 1 || issue.findFailureClasses[0] != string(FailureCode) {
		t.Errorf("dedup lookup classes = %v, want [%s]", issue.findFailureClasses, FailureCode)
	}
	if len(handoff.calls) != 1 || handoff.calls[0].IssueURL != "https://gl/issues/42" {
		t.Errorf("existing issue URL not propagated to handoff: %+v", handoff.calls)
	}
}

// TestEscalator_DedupCreatesWhenNoOpenIssue verifies the no-match path still
// files a fresh issue, and that the new body carries the dedup marker so the
// NEXT run for the same item can find it.
func TestEscalator_DedupCreatesWhenNoOpenIssue(t *testing.T) {
	st, run, item := newEscalateEnv(t)
	issue := &fakeDedupIssue{findFound: false, createResp: IssueResponse{IID: 7, URL: "https://gl/issues/7"}}
	e := NewEscalator(st, issue, &fakeHandoff{})
	if err := e.Handle(context.Background(), run, item, "x"); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if len(issue.createCalls) != 1 {
		t.Fatalf("expected 1 create when no open issue, got %d", len(issue.createCalls))
	}
	if len(issue.comments) != 0 {
		t.Errorf("must not comment when filing fresh; got %d", len(issue.comments))
	}
	// EXPECTATION CHANGED (unclassified-escalation fix): an unmarked reason
	// used to persist NO classification, so this body carried the legacy
	// backlog-only marker. Unmarked reasons now classify from their evidence
	// ("x" matches no infra needle → the conservative ClassCode default), so
	// the body carries the class-aware marker. The legacy marker is still
	// emitted for records that genuinely have no failure class.
	wantMarker := EscalationClassDedupMarker(item.ID, string(FailureCode))
	if !strings.Contains(issue.createCalls[0].Description, wantMarker) {
		t.Errorf("created issue body missing dedup marker %q for %q: %q", wantMarker, item.ID, issue.createCalls[0].Description)
	}
}

// TestEscalator_DedupFallsBackToLegacyMarkerAcrossTheClassificationUpgrade
// pins the upgrade seam. Before unmarked escalations classified, an item's
// open issue was filed with the LEGACY backlog-only marker. Once the same item
// re-escalates with a real class, a class-aware lookup misses that issue —
// without a fallback the escalator would file a duplicate for work already
// being tracked. The recurrence must land on the existing thread instead.
func TestEscalator_DedupFallsBackToLegacyMarkerAcrossTheClassificationUpgrade(t *testing.T) {
	st, run, item := newEscalateEnv(t)
	issue := &fakeDedupIssue{
		// Class-aware lookup finds nothing (no issue was ever filed under a
		// class), but a legacy backlog-only issue is open.
		findByFailureClass: map[string]IssueRef{},
		findFound:          true,
		findRef:            IssueRef{IID: 99, URL: "https://gl/issues/99"},
	}
	e := NewEscalator(st, issue, &fakeHandoff{})
	if err := e.Handle(context.Background(), run, item, "x"); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if len(issue.createCalls) != 0 {
		t.Fatalf("must reuse the open legacy issue, not file a duplicate; got %d creates", len(issue.createCalls))
	}
	if len(issue.comments) != 1 || issue.comments[0].IID != 99 {
		t.Fatalf("recurrence must land on the legacy issue 99; comments=%+v", issue.comments)
	}
}

func TestEscalator_DedupSeparatesFailureClasses(t *testing.T) {
	st, run, item := newEscalateEnv(t)
	issue := &fakeDedupIssue{
		findByFailureClass: map[string]IssueRef{
			string(FailureCode): {IID: 6, URL: "https://gl/issues/6"},
		},
		createResp: IssueResponse{IID: 7, URL: "https://gl/issues/7"},
	}
	e := NewEscalator(st, issue, &fakeHandoff{})

	codeReason := "stage tests errored deterministically (not retried) [class=code]: assertion failed"
	if err := e.Handle(context.Background(), run, item, codeReason); err != nil {
		t.Fatalf("handle code escalation: %v", err)
	}
	configReason := "stage merge errored terminally (not retried) [class=config]: status 405 method not allowed"
	if err := e.Handle(context.Background(), run, item, configReason); err != nil {
		t.Fatalf("handle config escalation: %v", err)
	}

	if len(issue.findFailureClasses) != 2 {
		t.Fatalf("dedup lookup classes = %v, want code and configuration", issue.findFailureClasses)
	}
	if issue.findFailureClasses[0] != string(FailureCode) || issue.findFailureClasses[1] != string(FailureConfiguration) {
		t.Errorf("dedup lookup classes = %v, want [%s %s]", issue.findFailureClasses, FailureCode, FailureConfiguration)
	}
	if len(issue.comments) != 1 || issue.comments[0].IID != 6 {
		t.Fatalf("same failure class must reuse issue 6; comments = %+v", issue.comments)
	}
	if len(issue.createCalls) != 1 {
		t.Fatalf("different failure class must create one separate issue; got %d creates", len(issue.createCalls))
	}
	marker := EscalationClassDedupMarker(item.ID, string(FailureConfiguration))
	if !strings.Contains(issue.createCalls[0].Description, marker) {
		t.Errorf("created configuration issue missing marker %q", marker)
	}
}

func TestEscalator_LegacyDedupClientStillReusesAndCloses(t *testing.T) {
	st, run, item := newEscalateEnv(t)
	issue := &fakeLegacyDedupIssue{
		ref:   IssueRef{IID: 42, URL: "https://gl/issues/42"},
		found: true,
	}
	e := NewEscalator(st, issue, &fakeHandoff{})
	reason := "stage tests errored deterministically (not retried) [class=code]: assertion failed"
	run.EscalationClass = "code" // stamped by escalateWithItem in production
	if err := e.Handle(context.Background(), run, item, reason); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if len(issue.createCalls) != 0 || len(issue.comments) != 1 {
		t.Fatalf("legacy dedup path created=%d comments=%d, want 0/1", len(issue.createCalls), len(issue.comments))
	}
	if err := e.ResolveOnSuccess(context.Background(), run, item); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got, want := issue.closed, []int64{42}; !slices.Equal(got, want) {
		t.Fatalf("legacy close = %v, want %v", got, want)
	}
}

// TestEscalator_DedupFailsOpenOnLookupError guards the fail-open contract: a
// lookup error must never drop the escalation — it falls back to filing a
// fresh issue.
func TestEscalator_DedupFailsOpenOnLookupError(t *testing.T) {
	st, run, item := newEscalateEnv(t)
	issue := &fakeDedupIssue{findErr: errIssueDown, createResp: IssueResponse{IID: 9, URL: "https://gl/issues/9"}}
	e := NewEscalator(st, issue, &fakeHandoff{})
	if err := e.Handle(context.Background(), run, item, "x"); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if len(issue.createCalls) != 1 {
		t.Errorf("lookup error must fail open to a fresh create; got %d creates", len(issue.createCalls))
	}
}

// TestEscalator_ResolveOnSuccessClosesOpenIssue guards DEBT-073 (#167 auto-close):
// a successful run for an item with an open escalation issue must comment the
// resolution and close that issue.
func TestEscalator_ResolveOnSuccessClosesOpenIssue(t *testing.T) {
	st, run, item := newEscalateEnv(t)
	issue := &fakeDedupIssue{findFound: true, findRef: IssueRef{IID: 51, URL: "https://gl/issues/51"}}
	e := NewEscalator(st, issue, &fakeHandoff{})
	if err := e.ResolveOnSuccess(context.Background(), run, item); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(issue.closed) != 1 || issue.closed[0] != 51 {
		t.Fatalf("expected issue 51 closed, got %v", issue.closed)
	}
	if len(issue.comments) != 1 || issue.comments[0].IID != 51 {
		t.Errorf("expected one resolution comment on issue 51, got %+v", issue.comments)
	}
	if !strings.Contains(issue.comments[0].Body, run.ID) {
		t.Errorf("resolution comment missing run id: %q", issue.comments[0].Body)
	}
}

func TestEscalator_ResolveOnSuccessClosesEveryClassIssue(t *testing.T) {
	st, run, item := newEscalateEnv(t)
	issue := &fakeDedupIssue{listRefs: []IssueRef{
		{IID: 51, URL: "https://gl/issues/51"},
		{IID: 52, URL: "https://gl/issues/52"},
		{IID: 53, URL: "https://gl/issues/53"},
	}}
	e := NewEscalator(st, issue, &fakeHandoff{})
	if err := e.ResolveOnSuccess(context.Background(), run, item); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got, want := issue.closed, []int64{51, 52, 53}; !slices.Equal(got, want) {
		t.Fatalf("closed issues = %v, want %v", got, want)
	}
	if len(issue.comments) != 3 {
		t.Fatalf("resolution comments = %d, want 3", len(issue.comments))
	}
}

func TestEscalator_ResolveOnSuccessContinuesAfterCloseFailure(t *testing.T) {
	st, run, item := newEscalateEnv(t)
	issue := &fakeDedupIssue{
		listRefs:      []IssueRef{{IID: 51}, {IID: 52}, {IID: 53}},
		closeErrByIID: map[int64]error{52: errIssueDown},
	}
	e := NewEscalator(st, issue, &fakeHandoff{})
	err := e.ResolveOnSuccess(context.Background(), run, item)
	if err == nil || !strings.Contains(err.Error(), "close issue 52") {
		t.Fatalf("resolve error = %v, want issue 52 close failure", err)
	}
	if got, want := issue.closeCalls, []int64{51, 52, 53}; !slices.Equal(got, want) {
		t.Fatalf("close attempts = %v, want %v", got, want)
	}
	if got, want := issue.closed, []int64{51, 53}; !slices.Equal(got, want) {
		t.Fatalf("successfully closed issues = %v, want %v", got, want)
	}
}

// TestEscalator_ResolveOnSuccessNoOpWhenNoOpenIssue: an item that never
// escalated (no open issue) closes nothing.
func TestEscalator_ResolveOnSuccessNoOpWhenNoOpenIssue(t *testing.T) {
	st, run, item := newEscalateEnv(t)
	issue := &fakeDedupIssue{findFound: false}
	e := NewEscalator(st, issue, &fakeHandoff{})
	if err := e.ResolveOnSuccess(context.Background(), run, item); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(issue.closed) != 0 {
		t.Errorf("must not close anything when no open escalation exists; got %v", issue.closed)
	}
}

// TestEscalator_ResolveOnSuccessNoOpWithoutDedupClient: an issue client that
// can't look up / close issues (only implements IssueClient) is a safe no-op.
func TestEscalator_ResolveOnSuccessNoOpWithoutDedupClient(t *testing.T) {
	st, run, item := newEscalateEnv(t)
	e := NewEscalator(st, &fakeIssue{}, &fakeHandoff{})
	if err := e.ResolveOnSuccess(context.Background(), run, item); err != nil {
		t.Errorf("resolve must be a no-op (nil) for a non-dedup client, got %v", err)
	}
}

// TestEscalator_ResolveOnSuccessClosesDespiteCommentError: the resolution
// comment is soft — a comment failure must not prevent the close.
func TestEscalator_ResolveOnSuccessClosesDespiteCommentError(t *testing.T) {
	st, run, item := newEscalateEnv(t)
	issue := &fakeDedupIssue{findFound: true, findRef: IssueRef{IID: 7}, commentErr: errIssueDown}
	e := NewEscalator(st, issue, &fakeHandoff{})
	if err := e.ResolveOnSuccess(context.Background(), run, item); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(issue.closed) != 1 {
		t.Errorf("comment failure must not block the close; closed=%v", issue.closed)
	}
}

func TestRunner_EscalatorIsInvokedOnEscalation(t *testing.T) {
	st, run, item := newRunnerEnv(t)
	disp := &fakeDispatcher{}
	// Build a registry where one gate fails; the rest pass. This drives
	// the post_implement_gate to fail and exhaust retries.
	gr := newGateRegistryWithOneFailure(t, "scope")
	issue := &fakeIssue{resp: IssueResponse{IID: 1, URL: "https://gl/i/1"}}
	handoff := &fakeHandoff{}
	e := NewEscalator(st, issue, handoff)
	r := New(st, gr, disp, nil)
	r.Escalator = e
	if err := r.Drive(context.Background(), run, item); err != nil {
		t.Fatalf("drive: %v", err)
	}
	if len(issue.calls) == 0 {
		t.Errorf("expected escalator to post issue on retry-cap exceed")
	}
	if len(handoff.calls) == 0 {
		t.Errorf("expected escalator to create handoff")
	}
}

func TestIntegrator_EscalatorIsInvokedOnConflict(t *testing.T) {
	st, run, item := newIntegratorEnv(t)
	issue := &fakeIssue{}
	handoff := &fakeHandoff{}
	e := NewEscalator(st, issue, handoff)
	itg := NewIntegrator(st, &recordingSubRunner{store: st}, &fakeAllocator{}, &fakeMerger{conflict: true, files: []string{"a.go"}})
	itg.Escalator = e
	if err := itg.Run(context.Background(), run, item); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(issue.calls) != 1 || len(handoff.calls) != 1 {
		t.Errorf("expected one issue + one handoff on integrator escalation; got issue=%d handoff=%d", len(issue.calls), len(handoff.calls))
	}
}

func newGateRegistryWithOneFailure(t *testing.T, failName string) *gates.Registry {
	t.Helper()
	r := gates.NewRegistry()
	for _, name := range []string{"diff_size", "scope", "path_policy", "secret_scan", "commit_format"} {
		if name == failName {
			r.Register(&alwaysFailGate{name: name})
			continue
		}
		r.Register(&alwaysPassGate{name: name})
	}
	return r
}

func containsAll(haystack []string, needles ...string) bool {
	set := make(map[string]bool, len(haystack))
	for _, h := range haystack {
		set[h] = true
	}
	for _, n := range needles {
		if !set[n] {
			return false
		}
	}
	return true
}

var errIssueDown = newSentinelError("issue service down")

type sentinelError struct{ msg string }

func (e *sentinelError) Error() string  { return e.msg }
func newSentinelError(msg string) error { return &sentinelError{msg: msg} }

// recordedEntry is one AddContextEntry call captured by fakeContextRecorder.
type recordedEntry struct {
	SessionID string
	EntryType string
	Title     string
	Content   string
	Tags      []string
	Seq       int
}

// fakeContextRecorder records agent-context writes. The shared seq counter lets
// a test assert the recorder ran before the handoff.
type fakeContextRecorder struct {
	mu      sync.Mutex
	entries []recordedEntry
	err     error
	seq     *int
}

func (f *fakeContextRecorder) AddContextEntry(_ context.Context, sessionID, entryType, title, content string, tags []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	if f.seq != nil {
		*f.seq++
		n = *f.seq
	}
	f.entries = append(f.entries, recordedEntry{
		SessionID: sessionID, EntryType: entryType, Title: title,
		Content: content, Tags: tags, Seq: n,
	})
	return f.err
}

// orderingHandoff is fakeHandoff plus a stamp from the shared seq counter.
type orderingHandoff struct {
	fakeHandoff
	seq  *int
	seen int
}

func (f *orderingHandoff) CreateHandoff(ctx context.Context, req HandoffRequest) (HandoffResponse, error) {
	if f.seq != nil {
		*f.seq++
		f.seen = *f.seq
	}
	return f.fakeHandoff.CreateHandoff(ctx, req)
}

// TestEscalator_RecordsDecisionBeforeHandoff is the core of the citizenship
// contract: the operator must write its reasoning into the agent-context
// session BEFORE the handoff packages that session, otherwise the handoff
// ships with entry_count: 0 (the pre-existing behaviour).
func TestEscalator_RecordsDecisionBeforeHandoff(t *testing.T) {
	st, run, item := newEscalateEnv(t)
	run.CurrentStage = "implement"
	mrIID := int64(1234)
	run.MRIID = &mrIID

	seq := 0
	recorder := &fakeContextRecorder{seq: &seq}
	handoff := &orderingHandoff{seq: &seq}
	handoff.resp = HandoffResponse{HandoffID: "h-1"}

	e := NewEscalator(st, &fakeIssue{resp: IssueResponse{IID: 99, URL: "https://gl/issues/99"}}, handoff)
	e.Recorder = recorder
	e.HandTo = "human-on-call"

	if err := e.Handle(context.Background(), run, item, "post_implement_gate failed: diff > 800 lines"); err != nil {
		t.Fatalf("handle: %v", err)
	}

	if len(recorder.entries) != 1 {
		t.Fatalf("context entries = %d, want 1", len(recorder.entries))
	}
	got := recorder.entries[0]
	if got.Seq >= handoff.seen {
		t.Errorf("entry recorded at seq %d, handoff at %d; the entry must land first", got.Seq, handoff.seen)
	}
	if got.EntryType != "decision" {
		t.Errorf("entry_type = %q, want decision", got.EntryType)
	}
	if got.SessionID != "" {
		t.Errorf("session_id = %q, want empty (the recorder resolves the operator session)", got.SessionID)
	}
	if !strings.Contains(got.Title, item.ID) {
		t.Errorf("title missing backlog id: %q", got.Title)
	}
	for _, want := range []string{item.ID, run.ID, "Stage: implement", "diff_size=fail", "!1234", "https://gl/issues/99"} {
		if !strings.Contains(got.Content, want) {
			t.Errorf("content missing %q; full=\n%s", want, got.Content)
		}
	}
	if !containsAll(got.Tags, "mills", "escalation", "backlog:"+item.ID) {
		t.Errorf("tags = %v", got.Tags)
	}
}

// TestEscalator_RecorderFailureDoesNotBlockEscalation proves the recorder is
// advisory: a hub outage must not cost us the issue or the handoff.
func TestEscalator_RecorderFailureDoesNotBlockEscalation(t *testing.T) {
	st, run, item := newEscalateEnv(t)
	issue := &fakeIssue{resp: IssueResponse{IID: 7, URL: "https://gl/issues/7"}}
	handoff := &fakeHandoff{}
	e := NewEscalator(st, issue, handoff)
	e.Recorder = &fakeContextRecorder{err: newSentinelError("hub unreachable")}

	if err := e.Handle(context.Background(), run, item, "boom"); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if len(issue.calls) != 1 || len(handoff.calls) != 1 {
		t.Fatalf("issue=%d handoff=%d; both must still publish", len(issue.calls), len(handoff.calls))
	}
}

// TestEscalator_NoRecorderIsANoOp keeps the pre-Recorder wiring valid.
func TestEscalator_NoRecorderIsANoOp(t *testing.T) {
	st, run, item := newEscalateEnv(t)
	handoff := &fakeHandoff{}
	e := NewEscalator(st, &fakeIssue{}, handoff)
	if err := e.Handle(context.Background(), run, item, "boom"); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if len(handoff.calls) != 1 {
		t.Fatalf("handoff calls = %d", len(handoff.calls))
	}
}
