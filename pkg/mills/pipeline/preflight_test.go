package pipeline

import (
	"context"
	"errors"
	"testing"
	"time"

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
