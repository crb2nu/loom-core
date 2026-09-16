package pipeline

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/council"
	"github.com/crb2nu/loom/pkg/mills/gates"
	"github.com/crb2nu/loom/pkg/mills/store"
)

func TestRunPreflight_AllowsWhenHealthGatesPass(t *testing.T) {
	r := &Runner{HealthGates: StaticHealthPreflight{Decision: gates.HealthDecision{Allowed: true, Status: "pass"}}}
	blocked, err := r.runPreflight(context.Background(), &store.PipelineRun{ID: "run-1"}, &store.BacklogItem{ID: "item-1"})
	if err != nil {
		t.Fatalf("runPreflight() error = %v", err)
	}
	if blocked {
		t.Fatal("runPreflight() blocked, want allow")
	}
}

func TestRunPreflight_BlocksWithFailClosedReason(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)
	st, run, item := newRunnerEnv(t)
	r := &Runner{
		Store: st,
		HealthGates: StaticHealthPreflight{Decision: gates.HealthDecision{
			Allowed: false, FailClosed: true, Status: "block",
			Reasons: []string{"critical dependency gitlab is down"},
		}},
		Clock: func() time.Time { return now },
	}
	blocked, err := r.runPreflight(ctx, run, item)
	if err != nil {
		t.Fatalf("runPreflight() error = %v", err)
	}
	if !blocked {
		t.Fatal("runPreflight() blocked = false, want true")
	}
	got, err := st.Pipeline.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if got.State != store.PipelineEscalated {
		t.Fatalf("state = %s, want escalated", got.State)
	}
	if got.EndedAt == nil {
		t.Fatal("expected ended_at to be set")
	}
	itemGot, err := st.Backlog.Get(ctx, item.ID)
	if err != nil {
		t.Fatalf("get item: %v", err)
	}
	if itemGot.State != store.BacklogEscalated {
		t.Fatalf("item state = %s, want escalated", itemGot.State)
	}
}

func TestRunPreflight_FailsClosedWhenHealthGateProviderErrors(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)
	st, run, item := newRunnerEnv(t)
	r := &Runner{
		Store:       st,
		HealthGates: StaticHealthPreflight{Err: errors.New("mcp hub unavailable")},
		Clock:       func() time.Time { return now },
	}
	blocked, err := r.runPreflight(ctx, run, item)
	if err != nil {
		t.Fatalf("runPreflight() error = %v", err)
	}
	if !blocked {
		t.Fatal("runPreflight() blocked = false, want true")
	}
	got, err := st.Pipeline.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if got.State != store.PipelineEscalated {
		t.Fatalf("state = %s, want escalated", got.State)
	}
}

func TestDrive_HealthGateBlockSkipsDispatch(t *testing.T) {
	st, run, item := newRunnerEnv(t)
	dispatcher := &fakeDispatcher{canned: map[string]StageOutput{}}
	r := New(st, gates.Default(), dispatcher, nil)
	r.HealthGates = StaticHealthPreflight{Decision: gates.HealthDecision{
		Allowed: false, FailClosed: true, Status: "block", Reasons: []string{"no infrastructure health evidence available"},
	}}
	if err := r.Drive(context.Background(), run, item); err != nil {
		t.Fatalf("Drive() error = %v", err)
	}
	if calls := dispatcher.callsList(); len(calls) != 0 {
		t.Fatalf("dispatch calls = %+v, want none", calls)
	}
	got, err := st.Pipeline.GetRun(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if got.State != store.PipelineEscalated {
		t.Fatalf("state = %s, want escalated", got.State)
	}
}

type recoveringHubHealth struct{ remaining, calls int }

func (h *recoveringHubHealth) DecideHealthGates(context.Context) (gates.HealthDecision, error) {
	h.calls++
	if h.remaining != 0 {
		h.remaining--
		return gates.HealthDecision{Reasons: []string{"mcp_hub_session unavailable"}, FailClosed: true}, nil
	}
	return gates.HealthDecision{Allowed: true}, nil
}

func TestRunner_ResumeHubReadinessRetries(t *testing.T) {
	for _, permanent := range []bool{false, true} {
		t.Run(fmt.Sprint(permanent), func(t *testing.T) {
			st, run, item := newRunnerEnv(t)
			ctx := context.Background()
			run.CurrentStage, run.State = "tests", store.PipelineTesting
			if err := st.Pipeline.PutRun(ctx, run); err != nil {
				t.Fatal(err)
			}
			disp := &fakeDispatcher{}
			r := New(st, nil, disp, nil)
			r.Stages = []Stage{{ID: "tests", State: store.PipelineTesting}}
			health := &recoveringHubHealth{remaining: 2}
			if permanent {
				health.remaining = -1
			}
			r.HealthGates = health
			waits := 0
			r.RetryWait = func(context.Context, time.Duration) error { waits++; return nil }
			if err := r.Drive(ctx, run, item); err != nil {
				t.Fatal(err)
			}
			got, err := st.Pipeline.GetRun(ctx, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			rows, err := st.Pipeline.ListStages(ctx, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if permanent {
				if len(rows) != 3 || waits != 2 || len(disp.calls) != 0 || got.EscalationClass != "substrate" || got.EscalationRetryable == nil || !*got.EscalationRetryable {
					t.Fatalf("unavailable: run=%+v stages=%d waits=%d calls=%v", got, len(rows), waits, disp.calls)
				}
			} else if got.State != store.PipelineDone || waits != 1 || len(disp.calls) != 1 {
				t.Fatalf("recovery: run=%+v waits=%d calls=%v", got, waits, disp.calls)
			}
		})
	}
}

func TestRunner_ResumeAutonomyHubBudgetSurvivesRestart(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		t.Run(fmt.Sprint(mixed), func(t *testing.T) {
			st, run, item := newRunnerEnv(t)
			ctx := context.Background()
			run.CurrentStage, run.State = "tests", store.PipelineTesting
			if err := st.Pipeline.PutRun(ctx, run); err != nil {
				t.Fatal(err)
			}
			if err := st.Pipeline.PutStage(ctx, &store.StageResult{PipelineRunID: run.ID, Stage: "tests", Attempt: 2, StartedAt: run.StartedAt}); err != nil {
				t.Fatal(err)
			}
			r := New(st, nil, &fakeDispatcher{}, nil)
			r.Stages = []Stage{{ID: "tests", State: store.PipelineTesting}}
			r.AutonomyGate = func(context.Context, *store.PipelineRun, *store.BacklogItem, Stage) council.AutonomyGateDecision {
				blockers := []string{"mcp_hub_session unavailable"}
				if mixed {
					blockers = append(blockers, "policy disabled")
				}
				return council.AutonomyGateDecision{Blockers: blockers}
			}
			r.RetryWait = func(context.Context, time.Duration) error { t.Error("exhausted budget must not wait"); return nil }
			if err := r.Drive(ctx, run, item); err != nil {
				t.Fatal(err)
			}
			got, err := st.Pipeline.GetRun(ctx, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			want := "substrate"
			if mixed {
				want = "config"
			}
			if got.EscalationClass != want {
				t.Fatalf("class=%q want %q", got.EscalationClass, want)
			}
		})
	}
}
