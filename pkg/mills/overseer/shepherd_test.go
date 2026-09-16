package overseer

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/store"
)

type shepherdEnv struct {
	store    *store.Store
	policy   *mills.Policy
	shepherd *Shepherd
	now      time.Time
}

func newShepherdEnv(t *testing.T, sp mills.ShepherdPolicy) *shepherdEnv {
	t.Helper()
	st, err := store.Open(context.Background(), store.Options{Path: filepath.Join(t.TempDir(), "s.db")})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	pol := mills.Default()
	pol.Overseers = mills.OverseersPolicy{Enabled: true, Shepherd: sp}
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	env := &shepherdEnv{store: st, policy: pol, now: now}
	env.shepherd = &Shepherd{
		Store:  st,
		Policy: func() *mills.Policy { return env.policy },
		Recorder: &ActionRecorder{
			Events: st.Events,
			Actor:  shepherdActor,
			DryRun: func() bool { return mills.DryRunOn(env.policy.Overseers.Shepherd.DryRun) },
		},
		HomeProject: "services/loom-core",
		Now:         func() time.Time { return env.now },
	}
	return env
}

// seedEscalated inserts an escalated item + terminal escalated run. mrIID 0
// means the run never opened an MR. endedAgo places the run's terminal time.
func (e *shepherdEnv) seedEscalated(t *testing.T, id string, mrIID int64, failureClass string, endedAgo time.Duration) *store.BacklogItem {
	t.Helper()
	ctx := context.Background()
	item := &store.BacklogItem{
		ID: id, Title: "escalated fixture " + id, State: store.BacklogEscalated,
		Priority: store.P2, CreatedBy: "test",
	}
	if err := e.store.Backlog.Put(ctx, item); err != nil {
		t.Fatalf("seed item %s: %v", id, err)
	}
	item, _ = e.store.Backlog.Get(ctx, id)
	retryable := true
	run := &store.PipelineRun{
		ID: "PIPE-" + id, BacklogID: id, Template: "mills-default-pipeline",
		State: store.PipelineEscalated, Attempts: 1,
		StartedAt: e.now.Add(-endedAgo - time.Hour), FailureClass: failureClass,
		EscalationRetryable: &retryable,
	}
	if mrIID > 0 {
		run.MRIID = &mrIID
	}
	ended := e.now.Add(-endedAgo)
	run.EndedAt = &ended
	if err := e.store.Pipeline.PutRun(ctx, run); err != nil {
		t.Fatalf("seed run %s: %v", id, err)
	}
	return item
}

// Dry-run: an aged MR-bearing escalation (auto-requeue-unreachable) yields a
// relaunch PROPOSAL event and no state change; a second tick is once-only.
func TestShepherd_DryRunProposesRelaunchOnce(t *testing.T) {
	env := newShepherdEnv(t, mills.ShepherdPolicy{Enabled: true})
	ctx := context.Background()
	env.seedEscalated(t, "BL-SHEP-1", 501, "code", 48*time.Hour)

	res, err := env.shepherd.Tick(ctx)
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if res.Planned != 1 || res.Acted != 0 {
		t.Fatalf("tick result: %+v", res)
	}
	item, _ := env.store.Backlog.Get(ctx, "BL-SHEP-1")
	if item.State != store.BacklogEscalated {
		t.Fatalf("dry-run mutated state to %s", item.State)
	}
	if _, err := env.store.Events.FirstBySubjectKind(ctx, shepherdSubjectKind, "BL-SHEP-1", "overseer.shepherd.relaunch.dryrun"); err != nil {
		t.Fatalf("expected dryrun proposal event: %v", err)
	}
	res2, err := env.shepherd.Tick(ctx)
	if err != nil {
		t.Fatalf("second tick: %v", err)
	}
	if res2.Planned != 0 || res2.Acted != 0 {
		t.Fatalf("second tick must be once-only: %+v", res2)
	}
}

// Live + allow.relaunch: the guarded transition flips escalated→queued with
// the audit event riding it; the per-item once-ever guard holds.
func TestShepherd_LiveRelaunchTransitionsOnce(t *testing.T) {
	off := false
	env := newShepherdEnv(t, mills.ShepherdPolicy{
		Enabled: true, DryRun: &off,
		Allow: mills.ShepherdAllowPolicy{Relaunch: true},
	})
	ctx := context.Background()
	env.seedEscalated(t, "BL-SHEP-2", 502, "code", 48*time.Hour)

	res, err := env.shepherd.Tick(ctx)
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if res.Acted != 1 {
		t.Fatalf("tick result: %+v", res)
	}
	item, _ := env.store.Backlog.Get(ctx, "BL-SHEP-2")
	if item.State != store.BacklogQueued {
		t.Fatalf("item state: got %s want queued", item.State)
	}
	if _, err := env.store.Events.FirstBySubjectKind(ctx, shepherdSubjectKind, "BL-SHEP-2", "overseer.shepherd.relaunch"); err != nil {
		t.Fatalf("expected committed relaunch event: %v", err)
	}
	// Re-escalate: the shepherd must not touch it again (once ever).
	if _, err := env.store.Backlog.TransitionStateWithEvent(ctx, item.ID, item.ClaimVersion+1, store.BacklogQueued, store.BacklogEscalated,
		&store.Event{Actor: "test", Kind: "test.reescalate", SubjectKind: shepherdSubjectKind, SubjectID: item.ID, Payload: map[string]any{}}); err != nil {
		// Claim-version bookkeeping differs across helpers; a plain put is
		// an acceptable fixture fallback here.
		item.State = store.BacklogEscalated
		if perr := env.store.Backlog.Put(ctx, item); perr != nil {
			t.Fatalf("re-escalate: %v / %v", err, perr)
		}
	}
	res2, err := env.shepherd.Tick(ctx)
	if err != nil {
		t.Fatalf("second tick: %v", err)
	}
	if res2.Acted != 0 || res2.Planned != 0 {
		t.Fatalf("relaunch must be once-ever: %+v", res2)
	}
}

// Live WITHOUT allow.relaunch records the proposal and never mutates.
func TestShepherd_LiveWithoutAllowOnlyFlags(t *testing.T) {
	off := false
	env := newShepherdEnv(t, mills.ShepherdPolicy{Enabled: true, DryRun: &off})
	ctx := context.Background()
	env.seedEscalated(t, "BL-SHEP-3", 503, "code", 48*time.Hour)

	res, err := env.shepherd.Tick(ctx)
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if res.Acted != 0 || res.Planned != 1 {
		t.Fatalf("tick result: %+v", res)
	}
	item, _ := env.store.Backlog.Get(ctx, "BL-SHEP-3")
	if item.State != store.BacklogEscalated {
		t.Fatalf("no-allow mutated state to %s", item.State)
	}
}

// The young shelf and the auto-requeue lane are both out of bounds.
func TestShepherd_SkipsYoungAndAutoRequeueLane(t *testing.T) {
	env := newShepherdEnv(t, mills.ShepherdPolicy{Enabled: true})
	ctx := context.Background()
	// Young: aged 1h < default 24h min shelf age.
	env.seedEscalated(t, "BL-SHEP-YOUNG", 504, "code", time.Hour)
	// Auto-requeue lane: no MR, home project, infrastructure class.
	env.seedEscalated(t, "BL-SHEP-INFRA", 0, "infrastructure", 48*time.Hour)

	res, err := env.shepherd.Tick(ctx)
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if res.Planned != 0 || res.Acted != 0 || res.Skipped != 2 {
		t.Fatalf("tick result: %+v", res)
	}
}

// A closed-MR orphan past the orphan age earns a once-only attention flag.
func TestShepherd_FlagsClosedMROrphan(t *testing.T) {
	env := newShepherdEnv(t, mills.ShepherdPolicy{Enabled: true})
	ctx := context.Background()
	env.seedEscalated(t, "BL-SHEP-ORPHAN", 505, "code", 10*24*time.Hour)
	// The ghost sweep's closed marker, recorded 5 days ago (> default 3d).
	if err := env.store.Events.Append(ctx, &store.Event{
		Actor: "reconciler", Kind: ghostSparkMRClosedKind,
		SubjectKind: "pipeline_run", SubjectID: "PIPE-BL-SHEP-ORPHAN",
		OccurredAt: env.now.Add(-5 * 24 * time.Hour),
		Payload:    map[string]any{"mr_iid": 505},
	}); err != nil {
		t.Fatalf("seed closed marker: %v", err)
	}

	res, err := env.shepherd.Tick(ctx)
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	// One orphan flag + one relaunch proposal (dry-run default).
	if res.Planned != 2 {
		t.Fatalf("tick result: %+v", res)
	}
	if _, err := env.store.Events.FirstBySubjectKind(ctx, shepherdSubjectKind, "BL-SHEP-ORPHAN", "overseer.shepherd.orphan_flagged.dryrun"); err != nil {
		t.Fatalf("expected orphan flag event: %v", err)
	}
	res2, err := env.shepherd.Tick(ctx)
	if err != nil {
		t.Fatalf("second tick: %v", err)
	}
	if res2.Planned != 0 {
		t.Fatalf("orphan flag must be once-only: %+v", res2)
	}
}

// seedEscalatedWithFingerprint seeds an escalated item whose run carries a
// failure-shape fingerprint (B2 stamps).
func (e *shepherdEnv) seedEscalatedWithFingerprint(t *testing.T, id string, mrIID int64, fp string, endedAgo time.Duration) *store.BacklogItem {
	t.Helper()
	item := e.seedEscalated(t, id, mrIID, "code", endedAgo)
	if err := e.store.Pipeline.SetEscalationMetadata(context.Background(), "PIPE-"+id, store.EscalationMetadata{FailureSignature: fp}); err != nil {
		t.Fatalf("stamp %s: %v", id, err)
	}
	return item
}

// B2: a stamped item proposes ONLY when a cohort sibling with the same
// fingerprint merged after the escalation; an active same-fingerprint class
// or a silent cohort yields no proposal at all.
func TestShepherd_EnvironmentDeltaPredicate(t *testing.T) {
	env := newShepherdEnv(t, mills.ShepherdPolicy{Enabled: true})
	ctx := context.Background()
	const fp = "aaaabbbbccccdddd"

	// Candidate: escalated 48h ago with fingerprint.
	env.seedEscalatedWithFingerprint(t, "BL-FP-CAND", 601, fp, 48*time.Hour)
	// Silent cohort: no sibling merged, none active → no proposal.
	res, err := env.shepherd.Tick(ctx)
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if res.Planned != 0 || res.Acted != 0 {
		t.Fatalf("silent cohort must not propose: %+v", res)
	}

	// Cohort sibling with the same fingerprint whose item MERGED after the
	// candidate escalated → reason-to-believe appears.
	sibling := env.seedEscalatedWithFingerprint(t, "BL-FP-SIB", 602, fp, 24*time.Hour)
	sibling.State = store.BacklogMerged
	if err := env.store.Backlog.Put(ctx, sibling); err != nil {
		t.Fatalf("merge sibling: %v", err)
	}
	res, err = env.shepherd.Tick(ctx)
	if err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	if res.Planned != 1 {
		t.Fatalf("cleared cohort must propose: %+v", res)
	}
	ev, err := env.store.Events.FirstBySubjectKind(ctx, shepherdSubjectKind, "BL-FP-CAND", "overseer.shepherd.relaunch.dryrun")
	if err != nil {
		t.Fatalf("proposal event: %v", err)
	}
	if ev.Payload["failure_signature"] != fp || ev.Payload["cleared_by"] != "BL-FP-SIB" {
		t.Fatalf("proposal payload = %#v", ev.Payload)
	}
}

// B2 suppression twin: another item escalating with the same fingerprint
// inside the active window blocks even a cleared cohort — the class still
// burns.
func TestShepherd_ActiveClassSuppressesRelaunch(t *testing.T) {
	env := newShepherdEnv(t, mills.ShepherdPolicy{Enabled: true})
	ctx := context.Background()
	const fp = "eeeeffff00001111"

	env.seedEscalatedWithFingerprint(t, "BL-FP-CAND2", 611, fp, 48*time.Hour)
	// Cleared sibling...
	sib := env.seedEscalatedWithFingerprint(t, "BL-FP-SIB2", 612, fp, 24*time.Hour)
	sib.State = store.BacklogMerged
	if err := env.store.Backlog.Put(ctx, sib); err != nil {
		t.Fatalf("merge sibling: %v", err)
	}
	// ...but ALSO a fresh same-fingerprint escalation 1h ago (inside 6h window).
	env.seedEscalatedWithFingerprint(t, "BL-FP-FRESH", 613, fp, time.Hour)

	res, err := env.shepherd.Tick(ctx)
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	// The fresh item itself is young-shelf-skipped; the candidate and the
	// merged sibling produce no proposals because the class is active.
	if res.Planned != 0 || res.Acted != 0 {
		t.Fatalf("active class must suppress: %+v", res)
	}
}

// seedScopeEscalated seeds a config-class scope escalation with a persisted
// scope_violations verdict table. declared controls the item's slice files
// (the anchors); violations are the recorded out-of-scope files.
func (e *shepherdEnv) seedScopeEscalated(t *testing.T, id string, declared, violations []string, endedAgo time.Duration) *store.BacklogItem {
	t.Helper()
	ctx := context.Background()
	item := &store.BacklogItem{
		ID: id, Title: "scope escalated " + id, State: store.BacklogEscalated,
		Priority: store.P2, CreatedBy: "test",
		Slices: []store.Slice{{Name: "s1", Files: declared}},
	}
	if err := e.store.Backlog.Put(ctx, item); err != nil {
		t.Fatalf("seed item %s: %v", id, err)
	}
	item, _ = e.store.Backlog.Get(ctx, id)
	run := &store.PipelineRun{
		ID: "PIPE-" + id, BacklogID: id, Template: "mills-default-pipeline",
		State: store.PipelineEscalated, Attempts: 1,
		StartedAt: e.now.Add(-endedAgo - time.Hour), EscalationClass: "config",
	}
	ended := e.now.Add(-endedAgo)
	run.EndedAt = &ended
	if err := e.store.Pipeline.PutRun(ctx, run); err != nil {
		t.Fatalf("seed run %s: %v", id, err)
	}
	verdicts := make([]map[string]any, 0, len(violations))
	for _, f := range violations {
		verdicts = append(verdicts, map[string]any{"file": f, "admitted": false, "rule": "no-shared-ancestor"})
	}
	outcome := store.StageOutcomeGateFail
	if err := e.store.Pipeline.PutStage(ctx, &store.StageResult{
		PipelineRunID: run.ID, Stage: "post_implement_gate", Attempt: 1,
		StartedAt: run.StartedAt, EndedAt: &ended, Outcome: &outcome,
		Artifacts: map[string]any{"scope_violations": map[string]any{"admitted": false, "verdicts": verdicts}},
	}); err != nil {
		t.Fatalf("seed violations %s: %v", id, err)
	}
	return item
}

// C1: a scope escalation whose violations are admissible under CURRENT
// policy/slices earns a widen proposal in dry-run and the full widen+requeue
// when allowed; one that stays inadmissible earns the once-only decision flag.
func TestShepherd_ScopeRescueWidensOrFlags(t *testing.T) {
	off := false
	env := newShepherdEnv(t, mills.ShepherdPolicy{
		Enabled: true, DryRun: &off,
		Allow: mills.ShepherdAllowPolicy{ScopeWiden: true},
	})
	ctx := context.Background()

	// Admissible: violation shares the pkg/mills ancestor with the declared
	// slice at default depth 2.
	env.seedScopeEscalated(t, "BL-SCOPE-OK", []string{"pkg/mills/store/dao.go"}, []string{"pkg/mills/reconciler.go"}, 48*time.Hour)
	// Inadmissible: cmd/ root file, no shared ancestor with the slice.
	env.seedScopeEscalated(t, "BL-SCOPE-NO", []string{"pkg/mills/store/dao.go"}, []string{"cmd/loom-mills-operator/main.go"}, 48*time.Hour)

	res, err := env.shepherd.Tick(ctx)
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if res.Acted != 1 || res.Planned != 1 {
		t.Fatalf("tick result: %+v", res)
	}
	ok, _ := env.store.Backlog.Get(ctx, "BL-SCOPE-OK")
	if ok.State != store.BacklogQueued {
		t.Fatalf("admissible item state: got %s want queued", ok.State)
	}
	widened := false
	for _, sl := range ok.Slices {
		for _, f := range sl.Files {
			if f == "pkg/mills/reconciler.go" {
				widened = true
			}
		}
	}
	if !widened {
		t.Fatalf("slice scope not widened: %+v", ok.Slices)
	}
	if _, err := env.store.Events.FirstBySubjectKind(ctx, shepherdSubjectKind, "BL-SCOPE-OK", "overseer.shepherd.scope_widen"); err != nil {
		t.Fatalf("widen event: %v", err)
	}
	no, _ := env.store.Backlog.Get(ctx, "BL-SCOPE-NO")
	if no.State != store.BacklogEscalated {
		t.Fatalf("inadmissible item must stay escalated, got %s", no.State)
	}
	if _, err := env.store.Events.FirstBySubjectKind(ctx, shepherdSubjectKind, "BL-SCOPE-NO", "overseer.shepherd.scope_decision_flagged"); err != nil {
		t.Fatalf("decision flag event: %v", err)
	}

	// Second tick: widen is once-ever; the flag is once-only.
	res2, err := env.shepherd.Tick(ctx)
	if err != nil {
		t.Fatalf("second tick: %v", err)
	}
	if res2.Acted != 0 || res2.Planned != 0 {
		t.Fatalf("second tick must be a no-op: %+v", res2)
	}
}

// C1 dry-run: the admissible case proposes without mutating state or slices.
func TestShepherd_ScopeRescueDryRunProposesOnly(t *testing.T) {
	env := newShepherdEnv(t, mills.ShepherdPolicy{Enabled: true})
	ctx := context.Background()
	env.seedScopeEscalated(t, "BL-SCOPE-DRY", []string{"pkg/mills/store/dao.go"}, []string{"pkg/mills/reconciler.go"}, 48*time.Hour)

	res, err := env.shepherd.Tick(ctx)
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if res.Acted != 0 || res.Planned != 1 {
		t.Fatalf("tick result: %+v", res)
	}
	item, _ := env.store.Backlog.Get(ctx, "BL-SCOPE-DRY")
	if item.State != store.BacklogEscalated || len(item.Slices) != 1 || len(item.Slices[0].Files) != 1 {
		t.Fatalf("dry-run mutated item: state=%s slices=%+v", item.State, item.Slices)
	}
	if _, err := env.store.Events.FirstBySubjectKind(ctx, shepherdSubjectKind, "BL-SCOPE-DRY", "overseer.shepherd.scope_widen.dryrun"); err != nil {
		t.Fatalf("dryrun proposal event: %v", err)
	}
}
