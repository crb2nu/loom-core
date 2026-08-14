package gates

import (
	"context"
	"fmt"
	"testing"

	"github.com/crb2nu/loom/pkg/mills/store"
)

// The numeric score exists nowhere but the judge call: gate_outcomes has no
// score column and a passing gate never renders it into Reasons. These tests
// pin it to the Outcome so the runner can persist it.
func TestLLMGate_PassCarriesPrimaryJudgement(t *testing.T) {
	g := &LLMGate{
		GateName: "spec_conformance", RubricName: SpecConformanceRubricName, Threshold: 0.8,
		Judge: &FakeRubricJudge{Default: RubricVerdict{Score: 0.91, Model: "gemma"}},
	}
	out, err := g.Evaluate(context.Background(), StageInput{})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if len(out.Judgements) != 1 {
		t.Fatalf("judgements = %+v, want exactly the primary", out.Judgements)
	}
	j := out.Judgements[0]
	if j.Role != JudgeRolePrimary || j.Model != "gemma" || j.Score != 0.91 || j.Threshold != 0.8 || !j.Pass {
		t.Fatalf("judgement = %+v", j)
	}
}

func TestLLMGate_FailCarriesPrimaryJudgement(t *testing.T) {
	g := &LLMGate{
		GateName: "spec_conformance", RubricName: SpecConformanceRubricName, Threshold: 0.8,
		Judge: &FakeRubricJudge{Default: RubricVerdict{Score: 0.42, Model: "gemma"}},
	}
	out, err := g.Evaluate(context.Background(), StageInput{})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if len(out.Judgements) != 1 || out.Judgements[0].Pass || out.Judgements[0].Score != 0.42 {
		t.Fatalf("judgements = %+v", out.Judgements)
	}
}

// An overrule must be attributable: folding both scores onto the primary
// would credit the local judge with the second opinion that corrected it.
func TestLLMGate_TiebreakerOverruleKeepsBothJudgements(t *testing.T) {
	g := &LLMGate{
		GateName: "pr_self_review", RubricName: PRSelfReviewRubricName, Threshold: 0.7,
		Judge:          &FakeRubricJudge{Default: RubricVerdict{Score: 0.5, Model: "gemma"}},
		Tiebreaker:     &FakeRubricJudge{Default: RubricVerdict{Score: 0.9, Model: "claude-sonnet-5"}},
		TiebreakerName: "anthropic",
	}
	out, err := g.Evaluate(context.Background(), StageInput{TestsPassed: true})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if !out.Pass {
		t.Fatalf("tiebreaker should overrule; got %+v", out)
	}
	if len(out.Judgements) != 2 {
		t.Fatalf("judgements = %+v, want primary + tiebreaker", out.Judgements)
	}
	primary, tie := out.Judgements[0], out.Judgements[1]
	if primary.Role != JudgeRolePrimary || primary.Model != "gemma" || primary.Pass {
		t.Errorf("primary = %+v, want the dissenting fail preserved", primary)
	}
	if tie.Role != JudgeRoleTiebreaker || tie.Model != "claude-sonnet-5" || tie.Score != 0.9 || !tie.Pass {
		t.Errorf("tiebreaker = %+v", tie)
	}
}

func TestLLMGate_TiebreakerCorroborationKeepsBothJudgements(t *testing.T) {
	g := &LLMGate{
		GateName: "pr_self_review", RubricName: PRSelfReviewRubricName, Threshold: 0.7,
		Judge:          &FakeRubricJudge{Default: RubricVerdict{Score: 0.5, Model: "gemma"}},
		Tiebreaker:     &FakeRubricJudge{Default: RubricVerdict{Score: 0.4, Model: "claude-sonnet-5"}},
		TiebreakerName: "anthropic",
	}
	out, err := g.Evaluate(context.Background(), StageInput{TestsPassed: true})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if len(out.Judgements) != 2 || out.Judgements[1].Role != JudgeRoleTiebreaker || out.Judgements[1].Pass {
		t.Fatalf("judgements = %+v", out.Judgements)
	}
}

// A verdict without a score is not a score of zero: the paths that never
// reached a judge must record nothing rather than poison the histogram.
func TestLLMGate_ScorelessPathsRecordNoJudgement(t *testing.T) {
	cases := []struct {
		name string
		gate *LLMGate
		in   StageInput
	}{
		{
			name: "disabled",
			gate: &LLMGate{GateName: "spec_conformance", Disabled: true, Judge: &FakeRubricJudge{}},
		},
		{
			name: "canary",
			gate: &LLMGate{GateName: "spec_conformance", Judge: &FakeRubricJudge{}},
			in:   StageInput{Item: &store.BacklogItem{Labels: []string{CanaryLabel}}},
		},
		{
			name: "unparseable",
			gate: &LLMGate{GateName: "spec_conformance", Judge: &FakeRubricJudge{Err: fmt.Errorf("judge: %w", ErrJudgeUnparseable)}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := tc.gate.Evaluate(context.Background(), tc.in)
			if err != nil {
				t.Fatalf("evaluate: %v", err)
			}
			if len(out.Judgements) != 0 {
				t.Fatalf("judgements = %+v, want none", out.Judgements)
			}
		})
	}
}
