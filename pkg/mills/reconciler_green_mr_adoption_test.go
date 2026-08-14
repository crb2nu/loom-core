package mills

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/store"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// fakeGreenMRAdopter records adoption attempts and replays a scripted verdict
// per MR IID.
type fakeGreenMRAdopter struct {
	mu       sync.Mutex
	adopt    map[int64]bool
	reasons  map[int64]string
	errs     map[int64]error
	attempts []int64
}

func (f *fakeGreenMRAdopter) AdoptGreenMR(_ context.Context, mrIID int64) (bool, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attempts = append(f.attempts, mrIID)
	if err := f.errs[mrIID]; err != nil {
		return false, "", err
	}
	return f.adopt[mrIID], f.reasons[mrIID], nil
}

func (f *fakeGreenMRAdopter) attemptCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.attempts)
}

// TestGhostSparkAdoptsOpenGreenMR is the 2026-08-02 CI-storm shape: a run
// escalated because runner_system_failure killed its pipeline, a human retried
// the pipeline and it went green, and the MR was left open and mergeable with
// no live stage to merge it (!1390/!1391 waited ~7h). The sweep must merge it
// and close the item.
func TestGhostSparkAdoptsOpenGreenMR(t *testing.T) {
	env := newRecEnv(t, nil)
	ctx := context.Background()

	seedEscalatedGhostSpark(t, env, "MILLS-GREEN-1", 1390, env.now.Add(-time.Hour))
	seeded, _ := env.store.Backlog.Get(ctx, "MILLS-GREEN-1")

	// GitLab still reports the MR OPEN — the state that used to mean "leave it
	// for a human, forever".
	mrs := &fakeMRStateClient{states: map[int64]string{1390: "opened"}}
	adopter := &fakeGreenMRAdopter{
		adopt:   map[int64]bool{1390: true},
		reasons: map[int64]string{1390: "merged open green mr"},
	}
	resolver := &fakeGhostResolver{}
	env.rec.GhostSparkMRState = mrs
	env.rec.GhostSparkGreenMRAdopter = adopter
	env.rec.GhostSparkResolver = resolver

	before := testutil.ToFloat64(GhostSparksClosedTotal.WithLabelValues("adopted_green_mr"))

	res, err := env.rec.SweepGhostSparks(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.GreenAdopted != 1 || res.Merged != 1 || res.Errored != 0 || res.MRClosed != 0 {
		t.Fatalf("sweep result: %+v", res)
	}
	if adopter.attemptCount() != 1 {
		t.Fatalf("expected exactly one adoption attempt, got %d", adopter.attemptCount())
	}

	got, _ := env.store.Backlog.Get(ctx, "MILLS-GREEN-1")
	if got.State != store.BacklogMerged {
		t.Fatalf("adopted item state: got %s want merged", got.State)
	}
	if got.Revision <= seeded.Revision {
		t.Fatalf("expected version bump: seeded rev %d, got %d", seeded.Revision, got.Revision)
	}
	if resolver.count() != 1 {
		t.Fatalf("expected the escalation issue to be auto-closed once, got %d", resolver.count())
	}
	if _, err := env.store.Events.FirstBySubjectKind(ctx, "pipeline_run", "PIPE-MILLS-GREEN-1", "reconciler.ghost_spark_closed"); err != nil {
		t.Fatalf("expected ghost_spark_closed event: %v", err)
	}
	if delta := testutil.ToFloat64(GhostSparksClosedTotal.WithLabelValues("adopted_green_mr")) - before; delta != 1 {
		t.Fatalf("adopted_green_mr counter delta: got %v want 1", delta)
	}

	// Second sweep: the item left the escalated set, so there is no candidate,
	// no lookup, and above all no second merge attempt.
	res2, err := env.rec.SweepGhostSparks(ctx)
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if res2.GreenAdopted != 0 || res2.Inspected != 0 {
		t.Fatalf("second sweep should be a no-op: %+v", res2)
	}
	if adopter.attemptCount() != 1 {
		t.Fatalf("must not attempt a second merge, got %d attempts", adopter.attemptCount())
	}
}

// TestGhostSparkLeavesNonGreenOpenMRForHuman proves the adopter's refusal is
// respected: an open MR that is not green stays escalated and untouched, which
// is the whole safety property of merging without a human.
func TestGhostSparkLeavesNonGreenOpenMRForHuman(t *testing.T) {
	env := newRecEnv(t, nil)
	ctx := context.Background()

	seedEscalatedGhostSpark(t, env, "MILLS-GREEN-RED", 1391, env.now.Add(-time.Hour))

	mrs := &fakeMRStateClient{states: map[int64]string{1391: "opened"}}
	adopter := &fakeGreenMRAdopter{
		adopt:   map[int64]bool{1391: false},
		reasons: map[int64]string{1391: `head pipeline "failed" is not green`},
	}
	env.rec.GhostSparkMRState = mrs
	env.rec.GhostSparkGreenMRAdopter = adopter

	res, err := env.rec.SweepGhostSparks(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.GreenAdopted != 0 || res.Merged != 0 || res.Errored != 0 {
		t.Fatalf("refused adoption must change nothing: %+v", res)
	}
	got, _ := env.store.Backlog.Get(ctx, "MILLS-GREEN-RED")
	if got.State != store.BacklogEscalated {
		t.Fatalf("non-green item must stay escalated, got %s", got.State)
	}

	// The refusal also has to cool the item down, or a permanently-red MR would
	// burn a lookup AND a merge probe on every single tick.
	res2, err := env.rec.SweepGhostSparks(ctx)
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if res2.Inspected != 0 {
		t.Fatalf("refused item must be inside its re-check cooldown: %+v", res2)
	}
	if adopter.attemptCount() != 1 {
		t.Fatalf("expected one adoption probe, got %d", adopter.attemptCount())
	}
}

// TestGhostSparkAdoptionErrorLeavesItemEscalated proves a GitLab failure during
// adoption is not mistaken for a merge.
func TestGhostSparkAdoptionErrorLeavesItemEscalated(t *testing.T) {
	env := newRecEnv(t, nil)
	ctx := context.Background()

	seedEscalatedGhostSpark(t, env, "MILLS-GREEN-ERR", 1392, env.now.Add(-time.Hour))

	mrs := &fakeMRStateClient{states: map[int64]string{1392: "opened"}}
	adopter := &fakeGreenMRAdopter{errs: map[int64]error{1392: errors.New("gitlab 500")}}
	env.rec.GhostSparkMRState = mrs
	env.rec.GhostSparkGreenMRAdopter = adopter

	res, err := env.rec.SweepGhostSparks(ctx)
	if err != nil {
		t.Fatalf("sweep must absorb the adoption error: %v", err)
	}
	if res.GreenAdopted != 0 || res.Merged != 0 || res.Errored != 1 {
		t.Fatalf("errored adoption result: %+v", res)
	}
	got, _ := env.store.Backlog.Get(ctx, "MILLS-GREEN-ERR")
	if got.State != store.BacklogEscalated {
		t.Fatalf("item must stay escalated after an adoption error, got %s", got.State)
	}
}

// TestGhostSparkWithoutAdopterIsUnchanged pins the nil-adopter default: an open
// MR is left exactly as the pre-adoption sweep left it.
func TestGhostSparkWithoutAdopterIsUnchanged(t *testing.T) {
	env := newRecEnv(t, nil)
	ctx := context.Background()

	seedEscalatedGhostSpark(t, env, "MILLS-GREEN-NIL", 1393, env.now.Add(-time.Hour))

	env.rec.GhostSparkMRState = &fakeMRStateClient{states: map[int64]string{1393: "opened"}}
	env.rec.GhostSparkGreenMRAdopter = nil

	res, err := env.rec.SweepGhostSparks(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.GreenAdopted != 0 || res.Merged != 0 || res.Errored != 0 || res.Inspected != 1 {
		t.Fatalf("nil adopter must preserve legacy behavior: %+v", res)
	}
	got, _ := env.store.Backlog.Get(ctx, "MILLS-GREEN-NIL")
	if got.State != store.BacklogEscalated {
		t.Fatalf("item must stay escalated, got %s", got.State)
	}
}

// TestGhostSparkClosureSupersedesRunVerdict proves Trustworthy Verdicts S1:
// a ghost-spark closure appends the explicit run.verdict event exactly once,
// the resolver reads the corrected verdict from the run's event subject, and
// the run row itself keeps its immutable escalated terminal record.
func TestGhostSparkClosureSupersedesRunVerdict(t *testing.T) {
	env := newRecEnv(t, nil)
	ctx := context.Background()

	seedEscalatedGhostSpark(t, env, "MILLS-VERDICT-1", 1408, env.now.Add(-time.Hour))
	env.rec.GhostSparkMRState = &fakeMRStateClient{states: map[int64]string{1408: "opened"}}
	env.rec.GhostSparkGreenMRAdopter = &fakeGreenMRAdopter{
		adopt:   map[int64]bool{1408: true},
		reasons: map[int64]string{1408: "merged open green mr"},
	}

	if _, err := env.rec.SweepGhostSparks(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	ev, err := env.store.Events.FirstBySubjectKind(ctx, "pipeline_run", "PIPE-MILLS-VERDICT-1", RunVerdictKindGhostSparkMerged)
	if err != nil {
		t.Fatalf("expected explicit run.verdict event: %v", err)
	}
	if ev.Payload["class"] != RunVerdictClassMergedAfterEscalation {
		t.Fatalf("verdict event class: %+v", ev.Payload)
	}
	if ev.Payload["outcome"] != "adopted_green_mr" {
		t.Fatalf("verdict event outcome: %+v", ev.Payload)
	}

	run, err := env.store.Pipeline.GetRun(ctx, "PIPE-MILLS-VERDICT-1")
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if run.State != store.PipelineEscalated {
		t.Fatalf("run terminal state must stay immutable, got %s", run.State)
	}
	events, err := env.store.Events.ListBySubject(ctx, "pipeline_run", run.ID, 100)
	if err != nil {
		t.Fatalf("list run events: %v", err)
	}
	v := ResolveRunVerdict(run, events)
	if !v.Superseded || v.Class != RunVerdictClassMergedAfterEscalation || v.Source != "ghost_spark_merged" {
		t.Fatalf("resolved verdict wrong: %+v", v)
	}

	// A second sweep finds no escalated candidate and must not duplicate the
	// verdict event (append-once on the kind+subject).
	if _, err := env.rec.SweepGhostSparks(ctx); err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	n, err := env.store.Events.CountBySubjectKind(ctx, "pipeline_run", run.ID, RunVerdictKindGhostSparkMerged)
	if err != nil {
		t.Fatalf("count verdict events: %v", err)
	}
	if n != 1 {
		t.Fatalf("verdict event count: got %d want 1", n)
	}
}
