package pipeline

import (
	"context"
	"strings"
	"testing"

	"github.com/crb2nu/loom/pkg/mills/spin"
	"github.com/crb2nu/loom/pkg/mills/store"
)

type invocationPreflightDispatcher struct {
	result        spin.PreflightResult
	dispatchCalls int
}

func (d *invocationPreflightDispatcher) PreflightImplement(context.Context, *store.PipelineRun, *store.BacklogItem, Stage, map[string]StageOutput) spin.PreflightResult {
	return d.result
}

func (d *invocationPreflightDispatcher) Dispatch(context.Context, *store.PipelineRun, *store.BacklogItem, Stage, map[string]StageOutput) (StageOutput, error) {
	d.dispatchCalls++
	return StageOutput{}, nil
}

func TestDrive_ImplementPreflightFailureIsDurableAndDeduplicated(t *testing.T) {
	for _, reason := range []spin.PreflightReason{spin.PreflightWorkdirMissing, spin.PreflightCredentialMissing, spin.PreflightPromptEmpty, spin.PreflightPromptUndeliverable} {
		t.Run(string(reason), func(t *testing.T) {
			st, run, item := newRunnerEnv(t)
			d := &invocationPreflightDispatcher{result: spin.PreflightResult{Reason: reason, Detail: "required credentials absent: API_TOKEN", Missing: []string{"API_TOKEN"}}}
			esc := &reasonCapturingEscalator{}
			r := New(st, nil, d, nil)
			r.Stages = []Stage{{ID: "implement"}, {ID: "mr"}}
			r.Escalator = esc
			for i := 0; i < 2; i++ {
				if err := r.Drive(context.Background(), run, item); err != nil {
					t.Fatalf("Drive attempt %d: %v", i+1, err)
				}
			}
			if d.dispatchCalls != 0 {
				t.Fatalf("dispatch calls = %d, want 0", d.dispatchCalls)
			}
			got, err := st.Pipeline.GetRun(context.Background(), run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.State != store.PipelinePreflightFailed || got.CurrentStage != preflightFailedStage {
				t.Fatalf("run state/stage = %s/%s", got.State, got.CurrentStage)
			}
			if len(esc.reasons) != 1 {
				t.Fatalf("escalations = %d, want 1", len(esc.reasons))
			}
			if !strings.Contains(esc.reasons[0], "[reason="+string(reason)+"]") || strings.Contains(esc.reasons[0], "secret-value") {
				t.Fatalf("unsafe or unstable reason: %q", esc.reasons[0])
			}
		})
	}
}

func TestDrive_ImplementPreflightPassesToDispatch(t *testing.T) {
	st, run, item := newRunnerEnv(t)
	d := &invocationPreflightDispatcher{result: spin.PreflightResult{OK: true}}
	r := New(st, nil, d, nil)
	r.Stages = []Stage{{ID: "implement"}}
	if err := r.Drive(context.Background(), run, item); err != nil {
		t.Fatal(err)
	}
	if d.dispatchCalls != 1 {
		t.Fatalf("dispatch calls = %d, want 1", d.dispatchCalls)
	}
}

func TestDrive_PendingImplementResumesWithoutNewInvocationPreflight(t *testing.T) {
	st, run, item := newRunnerEnv(t)
	if err := st.Pipeline.PutStage(context.Background(), &store.StageResult{
		PipelineRunID: run.ID,
		Stage:         "implement",
		Attempt:       1,
		StartedAt:     run.StartedAt,
		SpawnID:       "spawn-existing",
	}); err != nil {
		t.Fatal(err)
	}
	d := &invocationPreflightDispatcher{result: spin.PreflightResult{
		Reason: spin.PreflightPromptEmpty,
		Detail: "implement prompt is empty",
	}}
	r := New(st, nil, d, nil)
	r.Stages = []Stage{{ID: "implement"}}
	if err := r.Drive(context.Background(), run, item); err != nil {
		t.Fatal(err)
	}
	if d.dispatchCalls != 1 {
		t.Fatalf("resume dispatch calls = %d, want 1", d.dispatchCalls)
	}
	got, err := st.Pipeline.GetRun(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State == store.PipelinePreflightFailed {
		t.Fatal("pending accepted spawn was incorrectly rejected by new-invocation preflight")
	}
}
