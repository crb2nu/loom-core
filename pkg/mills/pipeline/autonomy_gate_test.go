package pipeline

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/crb2nu/loom/pkg/mills"
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

func TestAutonomyHoldLifecycle(t *testing.T) {
	for _, mode := range []string{"recover", "expire", "config", "becomes_config", "cancel", "wait_error", "manual_stop"} {
		t.Run(mode, func(t *testing.T) {
			st, run, item := newRunnerEnv(t)
			r := New(st, newPassingGates(t), &fakeDispatcher{}, nil)
			now := time.Now()
			r.Clock = func() time.Time { return now }
			initialState, attempts := run.State, run.Attempts
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			r.AutonomyGate = func(context.Context, *store.PipelineRun, *store.BacklogItem, Stage) council.AutonomyGateDecision {
				calls++
				if mode == "recover" && calls == 3 {
					return council.AutonomyGateDecision{Allowed: true}
				}
				blocker := "mcp_hub_session: dial tcp: lookup hub: i/o timeout"
				if mode == "config" || (mode == "becomes_config" && calls == 2) {
					blocker = "mcp_hub_session: 401 unauthorized"
				}
				return council.AutonomyGateDecision{Blockers: []string{blocker}}
			}
			var waits []time.Duration
			gauge := testutil.ToFloat64(mills.AutonomyBreakerHeldRuns)
			counter := testutil.ToFloat64(mills.AutonomyBreakerHoldsTotal.WithLabelValues("mcp_hub_session"))
			r.AutonomyWait = func(ctx context.Context, d time.Duration) error {
				if run.State != initialState || run.Attempts != attempts {
					t.Fatal("hold changed run state/attempts")
				}
				if testutil.ToFloat64(mills.AutonomyBreakerHeldRuns) != gauge+1 {
					t.Fatal("missing held gauge")
				}
				waits = append(waits, d)
				now = now.Add(d)
				if mode == "manual_stop" {
					stopped := *run
					stopped.State = store.PipelinePaused
					if err := st.Pipeline.PutRun(ctx, &stopped); err != nil {
						return err
					}
				}
				if mode == "cancel" {
					cancel()
					return ctx.Err()
				}
				if mode == "wait_error" {
					return errors.New("wait failed")
				}
				return nil
			}
			allowed, err := r.enforceAutonomy(ctx, run, item, Stage{ID: "post_review_gate"})
			if (err != nil) != (mode == "cancel" || mode == "wait_error") {
				t.Fatalf("error: %v", err)
			}
			if allowed != (mode == "recover") {
				t.Fatalf("allowed=%v", allowed)
			}
			if testutil.ToFloat64(mills.AutonomyBreakerHeldRuns) != gauge {
				t.Fatal("gauge leaked")
			}
			increment := float64(1)
			if mode == "config" {
				increment = 0
			}
			if testutil.ToFloat64(mills.AutonomyBreakerHoldsTotal.WithLabelValues("mcp_hub_session")) != counter+increment {
				t.Fatal("counter counts polls")
			}
			switch mode {
			case "recover":
				if !reflect.DeepEqual(waits, []time.Duration{time.Minute, 2 * time.Minute}) || run.State != initialState {
					t.Fatalf("recovery: %v %+v", waits, run)
				}
			case "expire":
				want := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 5 * time.Minute, 5 * time.Minute, 5 * time.Minute, 5 * time.Minute, 3 * time.Minute}
				if !reflect.DeepEqual(waits, want) {
					t.Fatalf("backoff: %v", waits)
				}
				stored, e := st.Pipeline.GetRun(context.Background(), run.ID)
				if e != nil {
					t.Fatal(e)
				}
				if stored.EscalationClass != string(ClassInfra) || stored.EscalationRetryable == nil || !*stored.EscalationRetryable || FailureClassFromString(stored.FailureClass).Terminal() {
					t.Fatalf("expiry metadata: %+v", stored)
				}
			case "config", "becomes_config":
				if run.EscalationClass != string(ClassConfig) {
					t.Fatalf("class: %s", run.EscalationClass)
				}
			}
			events, e := st.Events.ListSince(context.Background(), time.Time{}, 100)
			if e != nil {
				t.Fatal(e)
			}
			held := 0
			for _, event := range events {
				if event.Kind == "pipeline.stage.held" {
					held++
				}
			}
			if held != int(increment) {
				t.Fatalf("held events=%d", held)
			}
		})
	}
}

func TestRunnerAutonomyHoldPreservesCompletedWork(t *testing.T) {
	st, run, item := newRunnerEnv(t)
	disp := &fakeDispatcher{}
	r := New(st, newPassingGates(t), disp, nil)
	now := time.Now()
	r.Clock = func() time.Time { return now }
	r.AutonomyWait = func(_ context.Context, d time.Duration) error { now = now.Add(d); return nil }
	checks := 0
	r.AutonomyGate = func(_ context.Context, _ *store.PipelineRun, _ *store.BacklogItem, stage Stage) council.AutonomyGateDecision {
		if stage.ID == "implement" {
			checks++
			if checks < 3 {
				return council.AutonomyGateDecision{Blockers: []string{"mcp_hub_session: EOF"}}
			}
		}
		return council.AutonomyGateDecision{Allowed: true}
	}
	if err := r.Drive(context.Background(), run, item); err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, stage := range disp.callsList() {
		counts[stage]++
	}
	for _, stage := range []string{"plan_slice", "research", "implement"} {
		if counts[stage] != 1 {
			t.Fatalf("replayed/skipped stage: %v", counts)
		}
	}
	if run.State == store.PipelineEscalated {
		t.Fatal("recovery escalated")
	}
}

// TestAutonomyHoldOnHubCallSlotTimeout pins the 2026-09-13 evidence
// (PIPE-bl-devbox-tests-checkout-shared-clone-…): the operator's agent_context
// probe timed out waiting for the hub call slot under load (four runs plus the
// reconciler on one hub) and the exact blocker text escalated a run as
// terminal config one stage from its MR. A busy hub is substrate: the run must
// be HELD (stage held, state and attempts untouched, no escalation) and resume
// when the gate clears — never capability_red on the first failure.
func TestAutonomyHoldOnHubCallSlotTimeout(t *testing.T) {
	st, run, item := newRunnerEnv(t)
	r := New(st, newPassingGates(t), &fakeDispatcher{}, nil)
	now := time.Now()
	r.Clock = func() time.Time { return now }
	r.AutonomyWait = func(_ context.Context, d time.Duration) error { now = now.Add(d); return nil }
	const blocker = "mcp_hub_session: MCP hub agent_context unavailable after 1 consecutive failure(s): wait for call slot: context deadline exceeded"
	calls := 0
	r.AutonomyGate = func(context.Context, *store.PipelineRun, *store.BacklogItem, Stage) council.AutonomyGateDecision {
		calls++
		if calls > 1 {
			return council.AutonomyGateDecision{Allowed: true}
		}
		return council.AutonomyGateDecision{Blockers: []string{blocker}}
	}
	initialState, attempts := run.State, run.Attempts
	allowed, err := r.enforceAutonomy(context.Background(), run, item, Stage{ID: "mr"})
	if err != nil || !allowed {
		t.Fatalf("allowed=%v err=%v, want the held run to resume once the hub answers", allowed, err)
	}
	if run.State != initialState || run.Attempts != attempts || run.EscalationClass != "" {
		t.Fatalf("hold changed the run: state=%v attempts=%d class=%q", run.State, run.Attempts, run.EscalationClass)
	}
	events, err := st.Events.ListSince(context.Background(), time.Time{}, 100)
	if err != nil {
		t.Fatal(err)
	}
	held, blocked := 0, 0
	for _, event := range events {
		switch event.Kind {
		case "pipeline.stage.held":
			held++
		case "pipeline.autonomy_blocked":
			blocked++
		}
	}
	if held != 1 || blocked != 0 {
		t.Fatalf("held=%d blocked=%d, want one hold and no escalation", held, blocked)
	}
}
