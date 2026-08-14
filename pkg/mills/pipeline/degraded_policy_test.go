package pipeline

import (
	"context"
	"testing"

	"github.com/crb2nu/loom/pkg/mills/council"
	"github.com/crb2nu/loom/pkg/mills/gates"
	"github.com/crb2nu/loom/pkg/mills/store"
	"github.com/crb2nu/loom/pkg/telemetry"
)

func TestDrive_DegradedPolicyBlockSkipsDispatch(t *testing.T) {
	st, run, item := newRunnerEnv(t)
	dispatcher := &fakeDispatcher{canned: map[string]StageOutput{}}
	r := New(st, gates.NewRegistry(), dispatcher, nil)
	r.Stages = []Stage{{ID: "implement", Type: "agent_spawn", State: store.PipelineImplementing}}
	r.DegradedPolicy = func(context.Context, *store.PipelineRun, *store.BacklogItem, Stage) council.DegradedModeDecision {
		return council.DegradedModeDecision{
			Allowed:  false,
			Code:     council.DegradedPolicyCodeEmbedderUnavailable,
			Blockers: []string{"embedding provider unavailable with no fallback vector"},
		}
	}

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

func TestDrive_DegradedPolicyAllowsFallbackVector(t *testing.T) {
	st, run, item := newRunnerEnv(t)
	dispatcher := &fakeDispatcher{canned: map[string]StageOutput{}}
	r := New(st, gates.NewRegistry(), dispatcher, nil)
	r.Stages = []Stage{{ID: "implement", Type: "agent_spawn", State: store.PipelineImplementing}}
	r.DegradedPolicy = DegradedPolicyFromSignals(council.DefaultDegradedModePolicy(), func(context.Context) []council.DegradedModeSignal {
		return []council.DegradedModeSignal{{
			Path:           telemetry.EmbeddingPathDocuments,
			Outcome:        telemetry.EmbeddingOutcomeFallbackSuccess,
			Reason:         telemetry.EmbeddingReasonProviderOverload,
			FallbackVector: true,
		}}
	})

	if err := r.Drive(context.Background(), run, item); err != nil {
		t.Fatalf("Drive() error = %v", err)
	}
	if calls := dispatcher.callsList(); len(calls) != 1 || calls[0] != "implement" {
		t.Fatalf("dispatch calls = %+v, want implement", calls)
	}
	got, err := st.Pipeline.GetRun(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if got.State != store.PipelineDone {
		t.Fatalf("state = %s, want done", got.State)
	}
}
