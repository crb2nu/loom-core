package mrwatch

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/clients"
)

// fakeActor is a deterministic in-memory Actor. It records every call and can be
// programmed to return a specific error per action kind (keyed by Action).
type fakeActor struct {
	mu    sync.Mutex
	calls []fakeCall
	errs  map[Action]error
	newID int64
}

type fakeCall struct {
	kind       Action
	repo       string
	ref        string
	pipelineID int64
	iid        int64
	sha        string
	source     string
	target     string
}

func newFakeActor() *fakeActor {
	return &fakeActor{errs: map[Action]error{}, newID: 9000}
}

func (f *fakeActor) fail(kind Action, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.errs[kind] = err
}

func (f *fakeActor) RetryPipeline(_ context.Context, repo string, pipelineID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fakeCall{kind: ActionRetryPipeline, repo: repo, pipelineID: pipelineID})
	return f.errs[ActionRetryPipeline]
}

func (f *fakeActor) CreatePipeline(_ context.Context, repo, ref string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fakeCall{kind: ActionCreatePipeline, repo: repo, ref: ref})
	if err := f.errs[ActionCreatePipeline]; err != nil {
		return 0, err
	}
	f.newID++
	return f.newID, nil
}

func (f *fakeActor) ArmAutoMerge(_ context.Context, repo string, mrIID int64, source, target, headSHA string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fakeCall{kind: ActionArmAutoMerge, repo: repo, iid: mrIID, sha: headSHA, source: source, target: target})
	return f.errs[ActionArmAutoMerge]
}

func (f *fakeActor) callsOf(kind Action) []fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []fakeCall
	for _, c := range f.calls {
		if c.kind == kind {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeActor) total() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// headSHA / movedSHA are the observed and post-push head commits used by the
// arm tests. Real 40-hex shapes so the audit's short form is exercised.
const (
	headSHA  = "a1b2c3d4e5f60718293a4b5c6d7e8f9012345678"
	movedSHA = "ffee0011223344556677889900aabbccddeeff11"
)

// httpErr builds a *clients.GitLabHTTPError so tests can exercise the shepherd's
// 405-on-arm defer path (and the 409 head-moved path) via clients.GitLabHTTPStatus.
func httpErr(code int) error {
	return &clients.GitLabHTTPError{Method: "PUT", Path: "/merge", StatusCode: code, Body: "checking"}
}

func newTestShepherd(actor Actor, now time.Time, budget int) *Shepherd {
	return NewShepherd(actor, ShepherdOptions{
		Enabled: true,
		Budget:  budget,
		Now:     func() time.Time { return now },
	})
}

func snapOf(mrs ...MergeRequest) Snapshot {
	for i := range mrs {
		if mrs[i].TargetBranch == "" {
			mrs[i].TargetBranch = "main"
		}
	}
	return Snapshot{MergeRequests: mrs}
}

// TestShepherd_ActionSelection is a table-driven check that each stall class
// maps to the right (or no) bounded action, and that age gates hold.
func TestShepherd_ActionSelection(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	old := now.Add(-time.Hour)     // clears both 10m and 30m gates
	young := now.Add(-time.Minute) // clears neither gate

	cases := []struct {
		name string
		mr   MergeRequest
		want Action // "" means no action expected
	}{
		{
			name: "flaky retries pipeline",
			mr:   MergeRequest{Repo: "r", IID: 1, SourceBranch: "b", State: StateCIFailedFlaky, PipelineID: 42, CreatedAt: old},
			want: ActionRetryPipeline,
		},
		{
			name: "flaky without pipeline id: no action",
			mr:   MergeRequest{Repo: "r", IID: 1, SourceBranch: "b", State: StateCIFailedFlaky, PipelineID: 0, CreatedAt: old},
			want: "",
		},
		{
			name: "skipped creates pipeline when old enough",
			mr:   MergeRequest{Repo: "r", IID: 2, SourceBranch: "b", State: StatePipelineSkipped, PipelineID: 7, CreatedAt: old},
			want: ActionCreatePipeline,
		},
		{
			name: "skipped but too young: no action",
			mr:   MergeRequest{Repo: "r", IID: 2, SourceBranch: "b", State: StatePipelineSkipped, PipelineID: 7, CreatedAt: young},
			want: "",
		},
		{
			name: "awaiting with no head pipeline creates pipeline",
			mr:   MergeRequest{Repo: "r", IID: 3, SourceBranch: "b", State: StateAwaitingPipeline, PipelineID: 0, CreatedAt: old},
			want: ActionCreatePipeline,
		},
		{
			name: "awaiting WITH a pipeline id: no action (unknown-status, poll again)",
			mr:   MergeRequest{Repo: "r", IID: 3, SourceBranch: "b", State: StateAwaitingPipeline, PipelineID: 5, CreatedAt: old},
			want: "",
		},
		{
			name: "automerge_unarmed arms when >30m",
			mr:   MergeRequest{Repo: "r", IID: 4, SourceBranch: "b", State: StateAutomergeUnarmed, SHA: headSHA, CreatedAt: old},
			want: ActionArmAutoMerge,
		},
		{
			name: "automerge_unarmed too young (<30m): no action",
			mr:   MergeRequest{Repo: "r", IID: 4, SourceBranch: "b", State: StateAutomergeUnarmed, SHA: headSHA, CreatedAt: young},
			want: "",
		},
		{
			name: "conflict is never touched",
			mr:   MergeRequest{Repo: "r", IID: 5, SourceBranch: "b", State: StateConflict, CreatedAt: old},
			want: "",
		},
		{
			name: "deterministic failure is never touched",
			mr:   MergeRequest{Repo: "r", IID: 6, SourceBranch: "b", State: StateCIFailedDeterministic, PipelineID: 9, CreatedAt: old},
			want: "",
		},
		{
			name: "ok is left alone",
			mr:   MergeRequest{Repo: "r", IID: 7, SourceBranch: "b", State: StateOK, CreatedAt: old},
			want: "",
		},
		{
			name: "draft_idle is left alone",
			mr:   MergeRequest{Repo: "r", IID: 8, SourceBranch: "b", State: StateDraftIdle, CreatedAt: old},
			want: "",
		},
		{
			name: "merged (retained) is left alone",
			mr:   MergeRequest{Repo: "r", IID: 9, SourceBranch: "b", State: StateMerged, Merged: true, SHA: headSHA, CreatedAt: old},
			want: "",
		},
		{
			name: "closed is left alone",
			mr:   MergeRequest{Repo: "r", IID: 10, SourceBranch: "b", State: StateClosed, SHA: headSHA, CreatedAt: old},
			want: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			actor := newFakeActor()
			s := newTestShepherd(actor, now, 2)
			s.Reconcile(context.Background(), snapOf(tc.mr))

			if tc.want == "" {
				if actor.total() != 0 {
					t.Fatalf("expected no action, got %d calls: %+v", actor.total(), actor.calls)
				}
				if len(s.Actions()) != 0 {
					t.Errorf("expected no audit entries, got %d", len(s.Actions()))
				}
				return
			}
			calls := actor.callsOf(tc.want)
			if len(calls) != 1 {
				t.Fatalf("want 1 %s call, got %d (all: %+v)", tc.want, len(calls), actor.calls)
			}
			recs := s.Actions()
			if len(recs) != 1 {
				t.Fatalf("want 1 audit entry, got %d", len(recs))
			}
			if recs[0].Action != string(tc.want) || recs[0].Outcome != string(OutcomeOK) {
				t.Errorf("audit = %+v, want action=%s outcome=ok", recs[0], tc.want)
			}
		})
	}
}

// TestShepherd_IgnoresRetainedMergedEntries is the scope guard for the merged
// signal: retaining merged MRs in the snapshot must not give the shepherd new
// work. It plans only for the open stall classes it already handled, so a
// snapshot full of retained merged (and closed) entries — including ones whose
// fields would otherwise satisfy an arm or a pipeline create — yields zero
// GitLab writes and zero audit records.
func TestShepherd_IgnoresRetainedMergedEntries(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	old := now.Add(-time.Hour) // clears every age gate
	actor := newFakeActor()
	s := newTestShepherd(actor, now, 5)

	merged := []MergeRequest{
		// Every shape a retained entry can take, each carrying the fields the
		// shepherd's actions consume (sha, pipeline id, branch).
		{Repo: "r", IID: 1, SourceBranch: "b1", State: StateMerged, Merged: true, SHA: headSHA, PipelineID: 42, CreatedAt: old, MergedAt: old},
		{Repo: "r", IID: 2, SourceBranch: "b2", State: StateMerged, Merged: true, CreatedAt: old, MergedAt: old},
		{Repo: "r", IID: 3, SourceBranch: "b3", State: StateClosed, SHA: headSHA, PipelineID: 7, CreatedAt: old},
	}
	s.Reconcile(context.Background(), snapOf(merged...))

	if actor.total() != 0 {
		t.Fatalf("shepherd acted on merged/closed entries: %d calls %+v", actor.total(), actor.calls)
	}
	if len(s.Actions()) != 0 {
		t.Fatalf("shepherd audited %d actions for merged/closed entries, want 0", len(s.Actions()))
	}

	// And an open, actionable MR in the SAME snapshot is still handled: the
	// guard must not have muted the shepherd wholesale.
	unarmed := MergeRequest{Repo: "r", IID: 4, SourceBranch: "b4", State: StateAutomergeUnarmed, SHA: headSHA, CreatedAt: old}
	s.Reconcile(context.Background(), snapOf(append(merged, unarmed)...))
	if got := len(actor.callsOf(ActionArmAutoMerge)); got != 1 {
		t.Errorf("arm calls = %d, want 1 (only the open unarmed MR)", got)
	}
	if actor.total() != 1 {
		t.Errorf("total calls = %d, want 1: %+v", actor.total(), actor.calls)
	}
}

// TestShepherd_KillSwitch: a disabled shepherd takes no actions.
func TestShepherd_KillSwitch(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	actor := newFakeActor()
	s := NewShepherd(actor, ShepherdOptions{Enabled: false, Budget: 2, Now: func() time.Time { return now }})

	mr := MergeRequest{Repo: "r", IID: 1, SourceBranch: "b", State: StateAutomergeUnarmed, SHA: headSHA, CreatedAt: now.Add(-time.Hour)}
	s.Reconcile(context.Background(), snapOf(mr))

	if actor.total() != 0 {
		t.Fatalf("disabled shepherd must take no action, got %d", actor.total())
	}
	if s.Enabled() {
		t.Error("Enabled() should report false")
	}
}

// TestShepherd_NilActorForcesDisabled: constructing with a nil actor disables.
func TestShepherd_NilActorForcesDisabled(t *testing.T) {
	s := NewShepherd(nil, ShepherdOptions{Enabled: true, Budget: 2})
	if s.Enabled() {
		t.Fatal("nil actor must force disabled")
	}
	// Reconcile must be a safe no-op.
	s.Reconcile(context.Background(), snapOf(MergeRequest{State: StateAutomergeUnarmed}))
	if len(s.Actions()) != 0 {
		t.Error("nil-actor shepherd should record nothing")
	}
}

// TestShepherd_BudgetExhaustion: a per-MR budget bounds repeated reconciles.
func TestShepherd_BudgetExhaustion(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	actor := newFakeActor()
	s := newTestShepherd(actor, now, 2)

	mr := MergeRequest{Repo: "r", IID: 1, SourceBranch: "b", State: StateCIFailedFlaky, PipelineID: 42, CreatedAt: now.Add(-time.Hour)}

	// Four reconciles, but the daily budget is 2 → only 2 actions taken.
	for i := 0; i < 4; i++ {
		s.Reconcile(context.Background(), snapOf(mr))
	}
	if got := len(actor.callsOf(ActionRetryPipeline)); got != 2 {
		t.Fatalf("budget=2 must cap retries at 2, got %d", got)
	}
	if got := len(s.Actions()); got != 2 {
		t.Errorf("audit entries = %d, want 2 (exhaustion skips are not recorded)", got)
	}
}

// TestShepherd_BudgetResetsNextDay: the budget resets at the UTC day boundary.
func TestShepherd_BudgetResetsNextDay(t *testing.T) {
	day1 := time.Date(2026, 7, 18, 23, 0, 0, 0, time.UTC)
	actor := newFakeActor()
	nowVal := day1
	s := NewShepherd(actor, ShepherdOptions{Enabled: true, Budget: 1, Now: func() time.Time { return nowVal }})

	mr := MergeRequest{Repo: "r", IID: 1, SourceBranch: "b", State: StateCIFailedFlaky, PipelineID: 42, CreatedAt: day1.Add(-time.Hour)}

	s.Reconcile(context.Background(), snapOf(mr)) // day1: 1 action
	s.Reconcile(context.Background(), snapOf(mr)) // day1: budget exhausted
	if got := len(actor.callsOf(ActionRetryPipeline)); got != 1 {
		t.Fatalf("day1 should allow 1 action, got %d", got)
	}

	nowVal = day1.Add(2 * time.Hour) // crosses into 2026-07-19
	s.Reconcile(context.Background(), snapOf(mr))
	if got := len(actor.callsOf(ActionRetryPipeline)); got != 2 {
		t.Fatalf("new day should grant a fresh budget, got %d total", got)
	}
}

// TestShepherd_ArmDeferredOn405: a 405 on arm is retry-next-poll, not a failure,
// and does NOT consume budget — so a later poll can still arm.
func TestShepherd_ArmDeferredOn405(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	actor := newFakeActor()
	actor.fail(ActionArmAutoMerge, httpErr(405))
	s := newTestShepherd(actor, now, 1) // budget 1 to prove 405 didn't consume it

	mr := MergeRequest{Repo: "r", IID: 1, SourceBranch: "b", State: StateAutomergeUnarmed, SHA: headSHA, CreatedAt: now.Add(-time.Hour)}

	// First poll: 405 → deferred, budget untouched.
	s.Reconcile(context.Background(), snapOf(mr))
	recs := s.Actions()
	if len(recs) != 1 || recs[0].Outcome != string(OutcomeDeferred) {
		t.Fatalf("first arm should record deferred_405, got %+v", recs)
	}

	// GitLab finishes checking; second poll arms successfully (budget was NOT
	// spent on the 405).
	actor.fail(ActionArmAutoMerge, nil)
	s.Reconcile(context.Background(), snapOf(mr))
	if got := len(actor.callsOf(ActionArmAutoMerge)); got != 2 {
		t.Fatalf("want 2 arm attempts across polls, got %d", got)
	}
	recs = s.Actions()
	if recs[len(recs)-1].Outcome != string(OutcomeOK) {
		t.Errorf("second arm should succeed, got %+v", recs[len(recs)-1])
	}
}

// TestShepherd_HardErrorConsumesBudget: a non-405 error is recorded and DOES
// consume budget (so a persistently-broken endpoint is not hammered).
func TestShepherd_HardErrorConsumesBudget(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	actor := newFakeActor()
	actor.fail(ActionArmAutoMerge, errors.New("boom"))
	s := newTestShepherd(actor, now, 1)

	mr := MergeRequest{Repo: "r", IID: 1, SourceBranch: "b", State: StateAutomergeUnarmed, SHA: headSHA, CreatedAt: now.Add(-time.Hour)}
	s.Reconcile(context.Background(), snapOf(mr))
	s.Reconcile(context.Background(), snapOf(mr)) // budget already spent by the errored attempt

	if got := len(actor.callsOf(ActionArmAutoMerge)); got != 1 {
		t.Fatalf("hard error must consume budget: want 1 attempt, got %d", got)
	}
	recs := s.Actions()
	if len(recs) != 1 || recs[0].Outcome != string(OutcomeError) {
		t.Fatalf("want 1 error audit entry, got %+v", recs)
	}
}

// TestShepherd_ForProjectRouting: actions route to the MR's own repo.
func TestShepherd_MultiRepoRouting(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	actor := newFakeActor()
	s := newTestShepherd(actor, now, 2)

	s.Reconcile(context.Background(), snapOf(
		MergeRequest{Repo: "services/a", IID: 1, SourceBranch: "b1", State: StateAutomergeUnarmed, SHA: headSHA, CreatedAt: now.Add(-time.Hour)},
		MergeRequest{Repo: "services/b", IID: 2, SourceBranch: "b2", State: StateCIFailedFlaky, PipelineID: 3, CreatedAt: now.Add(-time.Hour)},
	))

	arms := actor.callsOf(ActionArmAutoMerge)
	if len(arms) != 1 || arms[0].repo != "services/a" || arms[0].iid != 1 {
		t.Errorf("arm routing wrong: %+v", arms)
	}
	retries := actor.callsOf(ActionRetryPipeline)
	if len(retries) != 1 || retries[0].repo != "services/b" || retries[0].pipelineID != 3 {
		t.Errorf("retry routing wrong: %+v", retries)
	}
}

// TestShepherd_AuditRingBounded: the audit log never exceeds its ring size.
func TestShepherd_AuditRingBounded(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	actor := newFakeActor()
	s := NewShepherd(actor, ShepherdOptions{
		Enabled: true, Budget: 1000, RingSize: 3, Now: func() time.Time { return now },
	})
	// 5 distinct MRs each get one flaky retry → 5 records, ring holds 3.
	var mrs []MergeRequest
	for i := int64(1); i <= 5; i++ {
		mrs = append(mrs, MergeRequest{Repo: "r", IID: i, SourceBranch: "b", State: StateCIFailedFlaky, PipelineID: i, CreatedAt: now.Add(-time.Hour)})
	}
	s.Reconcile(context.Background(), snapOf(mrs...))

	recs := s.Actions()
	if len(recs) != 3 {
		t.Fatalf("ring size 3 must bound audit log, got %d", len(recs))
	}
	// Newest retained: MRs 3,4,5 (oldest 1,2 evicted).
	if recs[0].MRIID != 3 || recs[2].MRIID != 5 {
		t.Errorf("ring should keep newest 3 (iids 3,4,5), got %d..%d", recs[0].MRIID, recs[2].MRIID)
	}
}

// TestShepherd_ActionsNilSafe: a nil shepherd yields an empty, non-nil slice.
func TestShepherd_ActionsNilSafe(t *testing.T) {
	var s *Shepherd
	if got := s.Actions(); got == nil || len(got) != 0 {
		t.Fatalf("nil shepherd Actions() = %v, want empty non-nil", got)
	}
	if s.Enabled() {
		t.Error("nil shepherd must not be enabled")
	}
}

// TestShepherd_AgeFallsBackToTransition: when created_at is absent, the age gate
// uses last_transition_at.
func TestShepherd_AgeFallsBackToTransition(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	actor := newFakeActor()
	s := newTestShepherd(actor, now, 2)

	// No CreatedAt; LastTransitionAt is old enough → arm.
	mr := MergeRequest{Repo: "r", IID: 1, SourceBranch: "b", State: StateAutomergeUnarmed, SHA: headSHA, LastTransitionAt: now.Add(-40 * time.Minute)}
	s.Reconcile(context.Background(), snapOf(mr))
	if len(actor.callsOf(ActionArmAutoMerge)) != 1 {
		t.Fatalf("age should fall back to last_transition_at, got %d arms", len(actor.callsOf(ActionArmAutoMerge)))
	}
}

// ----- head-SHA binding of the auto-merge arm (issue #375) -----

// TestShepherd_ArmBindsObservedHeadSHA: the arm carries the SHA the poll
// observed, so GitLab can reject it if the branch moved since.
func TestShepherd_ArmBindsObservedHeadSHA(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	actor := newFakeActor()
	s := newTestShepherd(actor, now, 2)

	mr := MergeRequest{Repo: "r", IID: 1, SourceBranch: "b", TargetBranch: "release", State: StateAutomergeUnarmed, SHA: headSHA, CreatedAt: now.Add(-time.Hour)}
	s.Reconcile(context.Background(), snapOf(mr))

	arms := actor.callsOf(ActionArmAutoMerge)
	if len(arms) != 1 || arms[0].sha != headSHA || arms[0].source != "b" || arms[0].target != "release" {
		t.Fatalf("arm must carry the observed head sha, got %+v", arms)
	}
	recs := s.Actions()
	if len(recs) != 1 || recs[0].Outcome != string(OutcomeOK) {
		t.Fatalf("want one ok audit entry, got %+v", recs)
	}
	if !strings.Contains(recs[0].Detail, headSHA[:8]) {
		t.Errorf("audit detail should name the armed sha, got %q", recs[0].Detail)
	}
}

func TestShepherd_ArmRefusedWithoutBranches(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		source string
		target string
	}{
		{name: "source", target: "main"},
		{name: "target", source: "feat/x"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			actor := newFakeActor()
			s := newTestShepherd(actor, now, 1)
			mr := MergeRequest{Repo: "r", IID: 1, SourceBranch: tc.source, TargetBranch: tc.target, State: StateAutomergeUnarmed, SHA: headSHA, CreatedAt: now.Add(-time.Hour)}
			plan, ok := s.plan(mr, now)
			if !ok {
				t.Fatal("eligible MR was not planned")
			}
			outcome, _ := s.act(context.Background(), plan)
			if outcome != OutcomeSkipped || actor.total() != 0 {
				t.Fatalf("outcome=%s calls=%d, want skipped with no enqueue", outcome, actor.total())
			}
		})
	}
}

// TestShepherd_ArmRefusedWithoutObservedSHA covers the restart/snapshot case: a
// snapshot carrying no head SHA (retained across a restart, degraded poll, or an
// older source that never reported one) must NOT be armed. The refusal is
// audited, no GitLab call is made, and budget is left intact so a later poll
// that does observe a SHA can still arm.
func TestShepherd_ArmRefusedWithoutObservedSHA(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	actor := newFakeActor()
	s := newTestShepherd(actor, now, 1) // budget 1 to prove the refusal didn't spend it

	shaless := MergeRequest{Repo: "r", IID: 1, SourceBranch: "b", State: StateAutomergeUnarmed, CreatedAt: now.Add(-time.Hour)}
	s.Reconcile(context.Background(), snapOf(shaless))

	if actor.total() != 0 {
		t.Fatalf("sha-less MR must never reach GitLab, got %+v", actor.calls)
	}
	recs := s.Actions()
	if len(recs) != 1 {
		t.Fatalf("refusal must be audited exactly once, got %d: %+v", len(recs), recs)
	}
	if recs[0].Action != string(ActionArmAutoMerge) || recs[0].Outcome != string(OutcomeSkipped) {
		t.Fatalf("audit = %+v, want arm_auto_merge/%s", recs[0], OutcomeSkipped)
	}
	if !strings.Contains(recs[0].Detail, "no observed head sha") {
		t.Errorf("refusal detail = %q, want the missing-sha reason", recs[0].Detail)
	}

	// Whitespace-only is treated the same as absent.
	s.Reconcile(context.Background(), snapOf(MergeRequest{Repo: "r", IID: 2, SourceBranch: "b", State: StateAutomergeUnarmed, SHA: "   ", CreatedAt: now.Add(-time.Hour)}))
	if actor.total() != 0 {
		t.Fatalf("blank sha must also be refused, got %+v", actor.calls)
	}

	// The next poll observes a SHA: budget was never consumed, so it arms.
	armed := shaless
	armed.SHA = headSHA
	s.Reconcile(context.Background(), snapOf(armed))
	arms := actor.callsOf(ActionArmAutoMerge)
	if len(arms) != 1 || arms[0].sha != headSHA {
		t.Fatalf("refusal must leave budget intact for a later armed poll, got %+v", arms)
	}
}

// TestShepherd_ArmRefusalAuditedOncePerDay: a permanently sha-less MR is polled
// repeatedly but must not flood the bounded audit ring.
func TestShepherd_ArmRefusalAuditedOncePerDay(t *testing.T) {
	day1 := time.Date(2026, 7, 18, 22, 0, 0, 0, time.UTC)
	nowVal := day1
	actor := newFakeActor()
	s := NewShepherd(actor, ShepherdOptions{Enabled: true, Budget: 2, Now: func() time.Time { return nowVal }})

	mr := MergeRequest{Repo: "r", IID: 1, SourceBranch: "b", State: StateAutomergeUnarmed, CreatedAt: day1.Add(-time.Hour)}
	for i := 0; i < 5; i++ {
		s.Reconcile(context.Background(), snapOf(mr))
	}
	if got := len(s.Actions()); got != 1 {
		t.Fatalf("repeated refusals must be audited once per day, got %d entries", got)
	}

	nowVal = day1.Add(4 * time.Hour) // crosses into 2026-07-19
	s.Reconcile(context.Background(), snapOf(mr))
	if got := len(s.Actions()); got != 2 {
		t.Fatalf("a new day should re-surface the refusal, got %d entries", got)
	}
	if actor.total() != 0 {
		t.Errorf("refusals must never call GitLab, got %+v", actor.calls)
	}
}

// TestShepherd_ArmHeadMovedOn409 covers poll-to-action movement: GitLab rejects
// the sha-pinned arm with 409 because the branch moved after the poll. The
// shepherd records head_moved (never re-arms the new head in the same pass) and
// only arms once a fresh poll has observed and re-classified the new head.
func TestShepherd_ArmHeadMovedOn409(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	actor := newFakeActor()
	actor.fail(ActionArmAutoMerge, httpErr(409))
	s := newTestShepherd(actor, now, 2)

	stale := MergeRequest{Repo: "r", IID: 1, SourceBranch: "b", State: StateAutomergeUnarmed, SHA: headSHA, CreatedAt: now.Add(-time.Hour)}
	s.Reconcile(context.Background(), snapOf(stale))

	arms := actor.callsOf(ActionArmAutoMerge)
	if len(arms) != 1 || arms[0].sha != headSHA {
		t.Fatalf("want exactly one arm attempt pinned to the stale sha, got %+v", arms)
	}
	recs := s.Actions()
	if len(recs) != 1 || recs[0].Outcome != string(OutcomeHeadMoved) {
		t.Fatalf("409 must record %s, got %+v", OutcomeHeadMoved, recs)
	}
	if !strings.Contains(recs[0].Detail, headSHA[:8]) {
		t.Errorf("head-moved detail should name the stale sha, got %q", recs[0].Detail)
	}

	// The next poll observes the new head; only then may it be armed — and it is
	// armed against the NEW sha, never the stale one.
	actor.fail(ActionArmAutoMerge, nil)
	moved := stale
	moved.SHA = movedSHA
	s.Reconcile(context.Background(), snapOf(moved))

	arms = actor.callsOf(ActionArmAutoMerge)
	if len(arms) != 2 || arms[1].sha != movedSHA {
		t.Fatalf("re-plan must bind the freshly observed sha, got %+v", arms)
	}
	if last := s.Actions(); last[len(last)-1].Outcome != string(OutcomeOK) {
		t.Errorf("second arm should succeed, got %+v", last[len(last)-1])
	}
}

// TestShepherd_HeadMovedConsumesBudget: a 409 is a real (rejected) write, so it
// consumes budget — a branch pushed on every poll cannot loop the shepherd.
func TestShepherd_HeadMovedConsumesBudget(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	actor := newFakeActor()
	actor.fail(ActionArmAutoMerge, httpErr(409))
	s := newTestShepherd(actor, now, 1)

	mr := MergeRequest{Repo: "r", IID: 1, SourceBranch: "b", State: StateAutomergeUnarmed, SHA: headSHA, CreatedAt: now.Add(-time.Hour)}
	s.Reconcile(context.Background(), snapOf(mr))
	mr.SHA = movedSHA
	s.Reconcile(context.Background(), snapOf(mr))

	if got := len(actor.callsOf(ActionArmAutoMerge)); got != 1 {
		t.Fatalf("head_moved must consume budget: want 1 attempt, got %d", got)
	}
}

func TestShepherdEnabledFromEnv(t *testing.T) {
	cases := map[string]bool{
		"":         false,
		"off":      false,
		"0":        false,
		"false":    false,
		"nonsense": false,
		"on":       true,
		"1":        true,
		"true":     true,
		"enabled":  true,
		"yes":      true,
	}
	for val, want := range cases {
		t.Setenv(EnvShepherd, val)
		if got := ShepherdEnabledFromEnv(); got != want {
			t.Errorf("LOOM_MRWATCH_SHEPHERD=%q → %v, want %v", val, got, want)
		}
	}
}

func TestShepherdBudgetFromEnv(t *testing.T) {
	t.Setenv(EnvShepherdBudget, "")
	if got := ShepherdBudgetFromEnv(nil); got != DefaultShepherdBudget {
		t.Errorf("unset budget = %d, want %d", got, DefaultShepherdBudget)
	}
	t.Setenv(EnvShepherdBudget, "5")
	if got := ShepherdBudgetFromEnv(nil); got != 5 {
		t.Errorf("budget = %d, want 5", got)
	}
	t.Setenv(EnvShepherdBudget, "-3")
	if got := ShepherdBudgetFromEnv(nil); got != 0 {
		t.Errorf("negative budget should clamp to 0, got %d", got)
	}
	t.Setenv(EnvShepherdBudget, "abc")
	if got := ShepherdBudgetFromEnv(nil); got != DefaultShepherdBudget {
		t.Errorf("unparseable budget = %d, want default %d", got, DefaultShepherdBudget)
	}
}
