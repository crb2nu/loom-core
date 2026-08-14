package pipeline

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/gates"
	"github.com/crb2nu/loom/pkg/mills/store"
)

// judgeVerdictFixture builds a runner whose post_review_gate holds one
// LLM-judged gate backed by a canned score.
func judgeVerdictFixture(t *testing.T, score float64) (*store.Store, *Runner, *store.PipelineRun, Stage) {
	t.Helper()
	st, err := store.Open(context.Background(), store.Options{Path: filepath.Join(t.TempDir(), "h.db")})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	reg := gates.NewRegistry()
	reg.Register(&gates.LLMGate{
		GateName: "spec_conformance", RubricName: gates.SpecConformanceRubricName, Threshold: 0.8,
		Judge: &gates.FakeRubricJudge{Default: gates.RubricVerdict{Score: score, Model: "gemma"}},
	})

	run := &store.PipelineRun{ID: "PIPE-JV-1", BacklogID: "BL-JV-1", Attempts: 3}
	return st, &Runner{Store: st, Gates: reg}, run, Stage{ID: "post_review_gate", Gates: []string{"spec_conformance"}}
}

func judgeVerdictEvents(t *testing.T, st *store.Store) []*store.Event {
	t.Helper()
	all, err := st.Events.ListBySubject(context.Background(), store.JudgeVerdictSubjectKind, "PIPE-JV-1", 50)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	var out []*store.Event
	for _, e := range all {
		if e.Kind == store.JudgeVerdictEventKind {
			out = append(out, e)
		}
	}
	return out
}

// The score is discarded everywhere else: gate_outcomes has no score column
// and a passing gate never renders it into Reasons.
func TestRunGate_PersistsJudgeVerdict(t *testing.T) {
	st, r, run, stage := judgeVerdictFixture(t, 0.93)

	verdict, err := r.runGate(context.Background(), run, &store.BacklogItem{ID: run.BacklogID}, stage, nil, mills.Default())
	if err != nil {
		t.Fatalf("runGate: %v", err)
	}
	if !verdict.Pass {
		t.Fatalf("gate verdict = %+v, want pass", verdict)
	}

	events := judgeVerdictEvents(t, st)
	if len(events) != 1 {
		t.Fatalf("judge.verdict events = %d, want 1", len(events))
	}
	p := events[0].Payload
	for key, want := range map[string]any{
		"run_id":      "PIPE-JV-1",
		"backlog_id":  "BL-JV-1",
		"gate":        "spec_conformance",
		"judge_model": "gemma",
		"role":        gates.JudgeRolePrimary,
		"score":       0.93,
		"threshold":   0.8,
		"pass":        true,
		"attempt":     float64(3), // payloads round-trip through JSON.
	} {
		if got := p[key]; got != want {
			t.Errorf("payload[%q] = %v (%T), want %v", key, got, got, want)
		}
	}
}

// Calibration evidence is worth strictly less than a verdict: a store that
// cannot take the append must not turn a passing gate into a failure.
func TestRunGate_JudgeVerdictAppendFailureDoesNotFailGate(t *testing.T) {
	st, r, run, stage := judgeVerdictFixture(t, 0.93)
	if err := st.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	verdict, err := r.runGate(context.Background(), run, &store.BacklogItem{ID: run.BacklogID}, stage, nil, mills.Default())
	if err != nil {
		t.Fatalf("runGate must not surface a persistence error: %v", err)
	}
	if !verdict.Pass {
		t.Fatalf("gate verdict = %+v, want pass despite the dead store", verdict)
	}
}

// A gate that never reached a judge has no score to record; writing one would
// put a fabricated 0.0 into the calibration histogram.
func TestRunGate_PureGoGateWritesNoJudgeVerdict(t *testing.T) {
	st, r, run, _ := judgeVerdictFixture(t, 0.93)
	r.Gates = gates.Default()

	if _, err := r.runGate(context.Background(), run, &store.BacklogItem{ID: run.BacklogID},
		Stage{ID: "post_implement_gate", Gates: []string{"diff_size"}}, nil, mills.Default()); err != nil {
		t.Fatalf("runGate: %v", err)
	}
	if events := judgeVerdictEvents(t, st); len(events) != 0 {
		t.Fatalf("judge.verdict events = %d, want none", len(events))
	}
}
