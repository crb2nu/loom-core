package overseer

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/pipeline"
	"github.com/crb2nu/loom/pkg/mills/store"
)

// scriptedProbe fails while failing is true.
type scriptedProbe struct {
	name    string
	failing bool
}

func (p *scriptedProbe) Name() string { return p.name }
func (p *scriptedProbe) Check(context.Context) error {
	if p.failing {
		return errors.New("connection refused")
	}
	return nil
}

// fakeIssues records issue interactions and implements the dedup + closable
// capabilities the sentinel type-asserts for.
type fakeIssues struct {
	created   []pipeline.IssueRequest
	comments  map[int64][]string
	closed    []int64
	openByKey map[string]int64
}

func (f *fakeIssues) CreateIssue(_ context.Context, req pipeline.IssueRequest) (pipeline.IssueResponse, error) {
	f.created = append(f.created, req)
	iid := int64(100 + len(f.created))
	f.openByKey[req.BacklogID] = iid
	return pipeline.IssueResponse{IID: iid, URL: "https://gitlab.example/issues"}, nil
}

func (f *fakeIssues) FindOpenEscalation(_ context.Context, backlogID string) (pipeline.IssueRef, bool, error) {
	if iid, ok := f.openByKey[backlogID]; ok {
		return pipeline.IssueRef{IID: iid}, true, nil
	}
	return pipeline.IssueRef{}, false, nil
}

func (f *fakeIssues) CommentIssue(_ context.Context, iid int64, body string) error {
	f.comments[iid] = append(f.comments[iid], body)
	return nil
}

func (f *fakeIssues) CloseIssue(_ context.Context, iid int64) error {
	f.closed = append(f.closed, iid)
	return nil
}

type sentinelEnv struct {
	store    *store.Store
	sentinel *Sentinel
	policy   *mills.Policy
	probe    *scriptedProbe
	issues   *fakeIssues
	now      time.Time
}

func newSentinelEnv(t *testing.T, sp mills.SentinelPolicy) *sentinelEnv {
	t.Helper()
	st, err := store.Open(context.Background(), store.Options{Path: filepath.Join(t.TempDir(), "s.db")})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	pol := mills.Default()
	pol.Overseers = mills.OverseersPolicy{Enabled: true, Sentinel: sp}
	env := &sentinelEnv{
		store:  st,
		policy: pol,
		probe:  &scriptedProbe{name: "flexinfer", failing: false},
		issues: &fakeIssues{comments: map[int64][]string{}, openByKey: map[string]int64{}},
		now:    time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC),
	}
	env.sentinel = &Sentinel{
		Probes: []Probe{env.probe},
		Policy: func() *mills.Policy { return env.policy },
		Recorder: &ActionRecorder{
			Events: st.Events,
			Actor:  sentinelActor,
			DryRun: func() bool { return mills.DryRunOn(env.policy.Overseers.Sentinel.DryRun) },
		},
		Issues: env.issues,
		Now:    func() time.Time { return env.now },
	}
	return env
}

func (e *sentinelEnv) tick(t *testing.T) TickResult {
	t.Helper()
	res, err := e.sentinel.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	return res
}

// Two failures stay below the default threshold of 3; the third opens the
// incident and asserts suppression.
func TestSentinelTripCounting(t *testing.T) {
	env := newSentinelEnv(t, mills.SentinelPolicy{
		Enabled: true, DryRun: boolPtr(false),
		Allow: mills.SentinelAllowPolicy{SuppressAdmission: true},
	})
	env.probe.failing = true

	env.tick(t)
	env.tick(t)
	if env.sentinel.SuppressAdmission() {
		t.Fatal("suppressed before trip threshold")
	}
	env.tick(t)
	if !env.sentinel.SuppressAdmission() {
		t.Fatal("not suppressed after 3 consecutive failures")
	}
	sup := env.sentinel.CurrentSuppression()
	if sup == nil || sup.Reason != "unhealthy: flexinfer" {
		t.Fatalf("suppression = %+v", sup)
	}

	// Recovery clears everything on the first healthy probe.
	env.probe.failing = false
	env.tick(t)
	if env.sentinel.SuppressAdmission() {
		t.Fatal("still suppressed after recovery")
	}
	n, err := env.store.Events.CountByKindSince(context.Background(),
		"overseer.sentinel.incident_cleared", env.now.Add(-time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("incident_cleared events = %d err=%v", n, err)
	}
}

// Kill-test (d): the suppression lease self-expires. A sentinel that dies
// mid-incident (no more ticks re-asserting) can never suppress admission
// past its TTL.
func TestSentinelSuppressionDeadMan(t *testing.T) {
	env := newSentinelEnv(t, mills.SentinelPolicy{
		Enabled: true, DryRun: boolPtr(false),
		SuppressionTTLMinutes: 30,
		Allow:                 mills.SentinelAllowPolicy{SuppressAdmission: true},
	})
	env.probe.failing = true
	env.tick(t)
	env.tick(t)
	env.tick(t)
	if !env.sentinel.SuppressAdmission() {
		t.Fatal("not suppressed")
	}
	// The sentinel "dies": no further ticks. The clock passes the TTL.
	env.now = env.now.Add(31 * time.Minute)
	if env.sentinel.SuppressAdmission() {
		t.Fatal("dead sentinel still suppressing past TTL")
	}
}

// Withdrawing the allow flag (policy hot-reload) releases suppression
// immediately, even with a live lease.
func TestSentinelSuppressionPolicyWithdrawal(t *testing.T) {
	env := newSentinelEnv(t, mills.SentinelPolicy{
		Enabled: true, DryRun: boolPtr(false),
		Allow: mills.SentinelAllowPolicy{SuppressAdmission: true},
	})
	env.probe.failing = true
	env.tick(t)
	env.tick(t)
	env.tick(t)
	if !env.sentinel.SuppressAdmission() {
		t.Fatal("not suppressed")
	}
	env.policy.Overseers.Sentinel.Allow.SuppressAdmission = false
	if env.sentinel.SuppressAdmission() {
		t.Fatal("suppression survived allow-flag withdrawal")
	}
}

// Dry-run: incidents are observed, the suppression is planned (.dryrun
// event), no live lease is held, and no issue is filed.
func TestSentinelDryRunPlansOnly(t *testing.T) {
	env := newSentinelEnv(t, mills.SentinelPolicy{
		Enabled: true, // DryRun nil ⇒ ON
		Allow:   mills.SentinelAllowPolicy{SuppressAdmission: true, FileIssue: true},
	})
	env.probe.failing = true
	env.tick(t)
	env.tick(t)
	res := env.tick(t)
	if res.Planned == 0 {
		t.Fatalf("no planned actions in dry-run: %+v", res)
	}
	if env.sentinel.SuppressAdmission() {
		t.Fatal("dry-run held a live suppression")
	}
	if len(env.issues.created) != 0 {
		t.Fatalf("dry-run filed %d issues", len(env.issues.created))
	}
	// Further unhealthy ticks must not re-plan the same episode.
	env.tick(t)
	env.tick(t)
	n, err := env.store.Events.CountByKindSince(context.Background(),
		"overseer.sentinel.suppress_admission.dryrun", env.now.Add(-time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("planned suppression events = %d err=%v, want 1", n, err)
	}
	// Observations record under committed kinds even in dry-run.
	n, err = env.store.Events.CountByKindSince(context.Background(),
		"overseer.sentinel.incident_opened", env.now.Add(-time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("incident_opened events = %d err=%v", n, err)
	}
}

// Issue filing dedups through the marker key and auto-closes on recovery.
func TestSentinelIssueDedupAndAutoClose(t *testing.T) {
	env := newSentinelEnv(t, mills.SentinelPolicy{
		Enabled: true, DryRun: boolPtr(false),
		Allow: mills.SentinelAllowPolicy{FileIssue: true},
	})
	env.probe.failing = true
	env.tick(t)
	env.tick(t)
	env.tick(t) // opens incident, files issue
	if len(env.issues.created) != 1 {
		t.Fatalf("issues created = %d, want 1", len(env.issues.created))
	}
	req := env.issues.created[0]
	if req.BacklogID != "overseer-sentinel:flexinfer" {
		t.Fatalf("issue key = %s", req.BacklogID)
	}

	// Recover, then trip again: the still-open issue gets a recurrence
	// comment, not a duplicate.
	env.probe.failing = false
	env.tick(t)
	// Simulate the issue staying open (auto-close removed it from the fake's
	// open set only if CloseIssue deletes; it doesn't, so re-add semantics
	// hold: the fake still reports it open).
	env.probe.failing = true
	env.tick(t)
	env.tick(t)
	env.tick(t)
	if len(env.issues.created) != 1 {
		t.Fatalf("duplicate issue filed: %d", len(env.issues.created))
	}
	if len(env.issues.comments) == 0 {
		t.Fatal("no recurrence comment on the existing issue")
	}
	if len(env.issues.closed) != 1 {
		t.Fatalf("issue closes = %d, want 1 (from the recovery)", len(env.issues.closed))
	}
}

// A disabled sentinel never suppresses, even with a live lease stored.
func TestSentinelDisabledReleasesSuppression(t *testing.T) {
	env := newSentinelEnv(t, mills.SentinelPolicy{
		Enabled: true, DryRun: boolPtr(false),
		Allow: mills.SentinelAllowPolicy{SuppressAdmission: true},
	})
	env.probe.failing = true
	env.tick(t)
	env.tick(t)
	env.tick(t)
	if !env.sentinel.SuppressAdmission() {
		t.Fatal("not suppressed")
	}
	env.policy.Overseers.Sentinel.Enabled = false
	if env.sentinel.SuppressAdmission() {
		t.Fatal("disabled sentinel still suppressing")
	}
}
