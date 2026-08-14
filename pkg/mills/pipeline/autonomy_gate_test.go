package pipeline

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/council"
	"github.com/crb2nu/loom/pkg/mills/store"
)

func TestRunner_AutonomyGateBlocksBeforeContinuation(t *testing.T) {
	st, run, item := newRunnerEnv(t)
	disp := &fakeDispatcher{}
	r := New(st, newPassingGates(t), disp, nil)
	r.Clock = func() time.Time { return time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC) }
	r.AutonomyGate = func(_ context.Context, _ *store.PipelineRun, _ *store.BacklogItem, stage Stage) council.AutonomyGateDecision {
		if stage.ID == "implement" {
			return council.AutonomyGateDecision{
				Allowed:  false,
				Code:     "capability_red",
				Blockers: []string{"hud_spawn unavailable"},
			}
		}
		return council.AutonomyGateDecision{Allowed: true}
	}

	if err := r.Drive(context.Background(), run, item); err != nil {
		t.Fatalf("drive: %v", err)
	}
	if got := disp.callsList(); strings.Join(got, ",") != "plan_slice,research" {
		t.Fatalf("dispatch calls = %v, want only stages before blocked implement", got)
	}
	gotRun, err := st.Pipeline.GetRun(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if gotRun.State != store.PipelineEscalated {
		t.Fatalf("run state = %s, want escalated", gotRun.State)
	}
	gotItem, err := st.Backlog.Get(context.Background(), item.ID)
	if err != nil {
		t.Fatalf("get item: %v", err)
	}
	if gotItem.State != store.BacklogEscalated {
		t.Fatalf("item state = %s, want escalated", gotItem.State)
	}

	events, err := st.Events.ListSince(context.Background(), time.Time{}, 50)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	var found bool
	for _, ev := range events {
		if ev.Kind != "pipeline.autonomy_blocked" {
			continue
		}
		found = true
		if ev.Payload["reason_code"] != "capability_red" {
			t.Fatalf("reason_code = %#v, want capability_red", ev.Payload["reason_code"])
		}
		if ev.Payload["stage"] != "implement" {
			t.Fatalf("stage = %#v, want implement", ev.Payload["stage"])
		}
	}
	if !found {
		t.Fatalf("missing pipeline.autonomy_blocked event in %#v", events)
	}
}

func TestAutonomyGateFromCouncilAdaptsDecision(t *testing.T) {
	gate := council.AutonomyGateFunc(func(context.Context) council.AutonomyGateDecision {
		return council.AutonomyGateDecision{Blockers: []string{"policy disabled"}}
	})
	got := AutonomyGateFromCouncil(gate)(context.Background(), nil, nil, Stage{ID: "merge"})
	if got.Allowed {
		t.Fatal("Allowed = true, want blocked")
	}
	if got.Code != council.AutonomyReasonPolicyDisabled {
		t.Fatalf("Code = %q, want %q", got.Code, council.AutonomyReasonPolicyDisabled)
	}
}

type rawAutonomyGate struct {
	decision council.AutonomyGateDecision
}

func (g rawAutonomyGate) CheckAutonomy(context.Context) council.AutonomyGateDecision {
	return g.decision
}

func TestAutonomyGateFromCouncilNormalizesRawGateDecision(t *testing.T) {
	gate := rawAutonomyGate{
		decision: council.AutonomyGateDecision{
			Code:     "Manual Review.Required",
			Blockers: []string{" repeated blocker ", "repeated blocker"},
		},
	}
	got := AutonomyGateFromCouncil(gate)(context.Background(), nil, nil, Stage{ID: "merge"})
	if got.Allowed {
		t.Fatal("Allowed = true, want blocked")
	}
	if got.Code != "manual_review_required" {
		t.Fatalf("Code = %q, want manual_review_required", got.Code)
	}
	if strings.Join(got.Blockers, ",") != "repeated blocker" {
		t.Fatalf("Blockers = %#v, want deduplicated blocker", got.Blockers)
	}
}
