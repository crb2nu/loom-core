package mills

import (
	"context"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/store"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestEscalationSweeper_ShutsDownOnCancel(t *testing.T) {
	env := newRecEnv(t, nil)
	sweeper := NewEscalationSweeper(env.rec, env.policy)
	sweeper.Interval = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sweeper.Run(ctx) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("sweeper did not stop after cancellation")
	}
}

func TestEscalationSweeper_EnabledFenceSkipsPass(t *testing.T) {
	env := newRecEnv(t, nil)
	sweeper := NewEscalationSweeper(env.rec, env.policy)
	sweeper.Enabled = func() bool { return false }
	sweeper.runPass(context.Background())
	events, err := env.store.Events.ListSince(context.Background(), time.Unix(0, 0), 20)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Kind == "reconciler.escalation_sweep" {
			t.Fatal("disabled pass emitted event")
		}
	}
}

func TestReconcilerTick_DoesNotRunEscalationLookups(t *testing.T) {
	env := newRecEnv(t, nil)
	client := &fakeMRStateClient{states: map[int64]string{1: "merged"}, errs: map[int64]error{}}
	env.rec.GhostSparkMRState = client
	if _, err := env.rec.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if got := client.callCount(); got != 0 {
		t.Fatalf("Tick performed %d GitLab escalation lookups; want zero", got)
	}
}

func TestEscalationSweeper_ReapsMergedBeforeAutoRequeue(t *testing.T) {
	env := newRecEnv(t, nil)
	ctx := context.Background()
	const id = "MILLS-SWEEP-ORDER"
	seedEscalatedGhostSpark(t, env, id, 91, env.now.Add(-time.Hour))
	runs, err := env.store.Pipeline.ListByBacklog(ctx, id)
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs: %v, %v", len(runs), err)
	}
	ended := env.now.Add(-time.Hour)
	runs[0].EndedAt = &ended
	runs[0].EscalationClass = autoRequeueClassInfra
	if err := env.store.Pipeline.PutRun(ctx, runs[0]); err != nil {
		t.Fatal(err)
	}
	env.rec.GhostSparkMRState = &fakeMRStateClient{states: map[int64]string{91: "merged"}, errs: map[int64]error{}}

	sweeper := NewEscalationSweeper(env.rec, env.policy)
	sweeper.runPass(ctx)
	if got := backlogState(t, env, id); got != store.BacklogMerged {
		t.Fatalf("state = %s, want merged", got)
	}
	if got := autoRequeueEventCount(t, env, id); got != 0 {
		t.Fatalf("auto-requeue events = %d, want zero", got)
	}
}

func TestEscalationSweeper_GhostTimeoutDoesNotStarveAutoRequeue(t *testing.T) {
	env := newRecEnv(t, nil)
	timeoutsBefore := testutil.ToFloat64(EscalationSweepTimeoutsTotal)
	seedEscalatedGhostSpark(t, env, "MILLS-SWEEP-TIMEOUT", 92, env.now.Add(-time.Hour))
	seedEscalatedForRequeue(t, env, seedRequeueSpec{
		id: "MILLS-SWEEP-RECOVER", class: autoRequeueClassInfra, endedAgo: time.Hour,
	})
	// A direct deadline error pins the failure path that previously guarded
	// away the second phase, without making this regression test sleep.
	env.rec.GhostSparkMRState = &deadlineExceededMRStateClient{}

	sweeper := NewEscalationSweeper(env.rec, env.policy)
	sweeper.runPass(context.Background())

	if got := backlogState(t, env, "MILLS-SWEEP-RECOVER"); got != store.BacklogQueued {
		t.Fatalf("auto-requeue state after ghost timeout = %s, want queued", got)
	}
	if got := autoRequeueEventCount(t, env, "MILLS-SWEEP-RECOVER"); got != 1 {
		t.Fatalf("auto-requeue events after ghost timeout = %d, want 1", got)
	}
	if got := testutil.ToFloat64(EscalationSweepTimeoutsTotal); got != timeoutsBefore+1 {
		t.Fatalf("timeout counter = %v, want %v", got, timeoutsBefore+1)
	}
}

func TestEscalationSweeper_ExpiredGhostAllocationDoesNotStarveAutoRequeue(t *testing.T) {
	env := newRecEnv(t, nil)
	seedEscalatedGhostSpark(t, env, "MILLS-SWEEP-BLOCKED", 93, env.now.Add(-time.Hour))
	seedEscalatedForRequeue(t, env, seedRequeueSpec{
		id: "MILLS-SWEEP-AFTER-DEADLINE", class: autoRequeueClassInfra, endedAgo: time.Hour,
	})
	env.rec.GhostSparkMRState = &waitForDeadlineMRStateClient{}

	sweeper := NewEscalationSweeper(env.rec, env.policy)
	// The fake blocks the ghost phase until its sub-deadline (budget*2/3), so
	// the auto-requeue phase gets only budget/3 of REAL wall clock for its
	// SQLite writes. 30ms left ~10ms for the requeue, which reliably expired
	// under -race on loaded CI runners (pipelines 22848/22888/22898 all red on
	// this assertion, 2026-08-09) while passing on fast local machines. 3s
	// keeps the ghost-allocation exhaustion the test exists to exercise and
	// gives the requeue a full second.
	sweeper.Budget = 3 * time.Second
	sweeper.runPass(context.Background())

	if got := backlogState(t, env, "MILLS-SWEEP-AFTER-DEADLINE"); got != store.BacklogQueued {
		t.Fatalf("auto-requeue state after ghost allocation expired = %s, want queued", got)
	}
}
