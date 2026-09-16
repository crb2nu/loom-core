package gates

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/store"
)

// countingJudge is a RubricJudge that counts calls; the shadow tests use it
// to prove the shadow is (or is not) consulted.
type countingJudge struct {
	calls int32
	v     RubricVerdict
	err   error
}

func (c *countingJudge) Judge(_ context.Context, _ string, _ StageInput) (RubricVerdict, error) {
	atomic.AddInt32(&c.calls, 1)
	if c.err != nil {
		return RubricVerdict{}, c.err
	}
	return c.v, nil
}

// blockingJudge never answers until its context ends.
type blockingJudge struct{}

func (blockingJudge) Judge(ctx context.Context, _ string, _ StageInput) (RubricVerdict, error) {
	<-ctx.Done()
	return RubricVerdict{}, ctx.Err()
}

func roles(js []Judgement) []string {
	out := make([]string, 0, len(js))
	for _, j := range js {
		out = append(out, j.Role)
	}
	return out
}

func equalRoles(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// TestLLMGate_ShadowJudgementRidesAlong: the shadow's score is recorded beside
// the primary's under its own role and touches nothing else.
func TestLLMGate_ShadowJudgementRidesAlong(t *testing.T) {
	shadow := &countingJudge{v: RubricVerdict{Score: 0.55, Model: "qwen38-27b"}}
	g := &LLMGate{
		GateName: "spec_conformance", RubricName: SpecConformanceRubricName, Threshold: 0.8,
		Judge:  &FakeRubricJudge{Default: RubricVerdict{Score: 0.91, Model: "luna"}},
		Shadow: shadow,
	}
	out, err := g.Evaluate(context.Background(), StageInput{})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if !out.Pass || out.JudgedBy != "flexinfer:luna" || len(out.Reasons) != 0 {
		t.Fatalf("shadow changed the outcome: %+v", out)
	}
	if !equalRoles(roles(out.Judgements), []string{JudgeRolePrimary, JudgeRoleShadow}) {
		t.Fatalf("judgements = %+v, want primary then shadow", out.Judgements)
	}
	s := out.Judgements[1]
	if s.Model != "qwen38-27b" || s.Score != 0.55 || s.Threshold != 0.8 || s.Pass {
		t.Fatalf("shadow judgement = %+v", s)
	}
	if atomic.LoadInt32(&shadow.calls) != 1 {
		t.Fatalf("shadow calls = %d, want 1", shadow.calls)
	}
}

// A failing primary still records the shadow (this is exactly the evidence a
// calibration read wants), and the shadow's pass does not rescue the gate.
func TestLLMGate_ShadowDoesNotRescueFailingPrimary(t *testing.T) {
	g := &LLMGate{
		GateName: "spec_conformance", RubricName: SpecConformanceRubricName, Threshold: 0.8,
		Judge:  &FakeRubricJudge{Default: RubricVerdict{Score: 0.40, Model: "luna", Reasons: []string{"missing tests"}}},
		Shadow: &countingJudge{v: RubricVerdict{Score: 0.95, Model: "qwen38-27b"}},
	}
	out, err := g.Evaluate(context.Background(), StageInput{})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if out.Pass || out.JudgedBy != "flexinfer:luna" {
		t.Fatalf("shadow rescued the gate: %+v", out)
	}
	if len(out.Reasons) != 2 || out.Reasons[1] != "missing tests" {
		t.Fatalf("reasons = %v, want the primary's untouched", out.Reasons)
	}
	if !equalRoles(roles(out.Judgements), []string{JudgeRolePrimary, JudgeRoleShadow}) || !out.Judgements[1].Pass {
		t.Fatalf("judgements = %+v", out.Judgements)
	}
}

func TestLLMGate_ShadowErrorLeavesOutcomeUntouched(t *testing.T) {
	g := &LLMGate{
		GateName: "spec_conformance", RubricName: SpecConformanceRubricName, Threshold: 0.8,
		Judge:  &FakeRubricJudge{Default: RubricVerdict{Score: 0.91, Model: "luna"}},
		Shadow: &countingJudge{err: errors.New("lane cold")},
	}
	out, err := g.Evaluate(context.Background(), StageInput{})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if !out.Pass || !equalRoles(roles(out.Judgements), []string{JudgeRolePrimary}) {
		t.Fatalf("outcome = %+v, want the primary alone", out)
	}
}

// A hung shadow lane is bounded by ShadowTimeout and then ignored.
func TestLLMGate_ShadowTimeoutLeavesOutcomeUntouched(t *testing.T) {
	g := &LLMGate{
		GateName: "spec_conformance", RubricName: SpecConformanceRubricName, Threshold: 0.8,
		Judge:         &FakeRubricJudge{Default: RubricVerdict{Score: 0.91, Model: "luna"}},
		Shadow:        blockingJudge{},
		ShadowTimeout: 30 * time.Millisecond,
	}
	start := time.Now()
	out, err := g.Evaluate(context.Background(), StageInput{})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if !out.Pass || !equalRoles(roles(out.Judgements), []string{JudgeRolePrimary}) {
		t.Fatalf("outcome = %+v, want the primary alone", out)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("evaluate took %s; the shadow timeout did not bound the wait", elapsed)
	}
}

// The short circuits never consult the shadow, and a scoreless primary path
// records nothing for it either.
func TestLLMGate_ShadowSkippedOnCanaryDisabledAndPrimaryError(t *testing.T) {
	shadow := &countingJudge{v: RubricVerdict{Score: 0.5, Model: "qwen38-27b"}}
	canary := &LLMGate{
		GateName: "spec_conformance", RubricName: SpecConformanceRubricName,
		Judge: &FakeRubricJudge{Default: RubricVerdict{Score: 0.9}}, Shadow: shadow,
	}
	out, err := canary.Evaluate(context.Background(), StageInput{Item: &store.BacklogItem{Labels: []string{CanaryLabel}}})
	if err != nil || !out.Pass || len(out.Judgements) != 0 {
		t.Fatalf("canary: out=%+v err=%v", out, err)
	}
	disabled := &LLMGate{GateName: "spec_conformance", RubricName: SpecConformanceRubricName, Disabled: true, Shadow: shadow}
	out, err = disabled.Evaluate(context.Background(), StageInput{})
	if err != nil || !out.Pass || len(out.Judgements) != 0 {
		t.Fatalf("disabled: out=%+v err=%v", out, err)
	}
	if atomic.LoadInt32(&shadow.calls) != 0 {
		t.Fatalf("shadow consulted %d times on short circuits, want 0", shadow.calls)
	}
	broken := &LLMGate{
		GateName: "spec_conformance", RubricName: SpecConformanceRubricName,
		Judge: &FakeRubricJudge{Err: errors.New("proxy down")}, Shadow: shadow,
	}
	if _, err := broken.Evaluate(context.Background(), StageInput{}); err == nil {
		t.Fatal("primary transport error must still bubble")
	}
	unparseable := &LLMGate{
		GateName: "spec_conformance", RubricName: SpecConformanceRubricName,
		Judge: &FakeRubricJudge{Err: ErrJudgeUnparseable}, Shadow: shadow,
	}
	out, err = unparseable.Evaluate(context.Background(), StageInput{})
	if err != nil || out.Pass || len(out.Judgements) != 0 {
		t.Fatalf("unparseable: out=%+v err=%v, want a scoreless soft fail", out, err)
	}
}

// Persisted order is primary, shadow, tiebreaker on both tiebreak branches, and
// the tiebreaker still decides.
func TestLLMGate_ShadowSurvivesTiebreaker(t *testing.T) {
	for name, tie := range map[string]RubricVerdict{
		"overrule":    {Score: 0.9, Model: "claude-sonnet-5"},
		"corroborate": {Score: 0.4, Model: "claude-sonnet-5"},
	} {
		t.Run(name, func(t *testing.T) {
			g := &LLMGate{
				GateName: "pr_self_review", RubricName: PRSelfReviewRubricName, Threshold: 0.7,
				Judge:          &FakeRubricJudge{Default: RubricVerdict{Score: 0.5, Model: "luna"}},
				Tiebreaker:     &FakeRubricJudge{Default: tie},
				TiebreakerName: "anthropic",
				Shadow:         &countingJudge{v: RubricVerdict{Score: 0.6, Model: "qwen38-27b"}},
			}
			out, err := g.Evaluate(context.Background(), StageInput{TestsPassed: true})
			if err != nil {
				t.Fatalf("evaluate: %v", err)
			}
			if out.Pass != (tie.Score >= 0.7) {
				t.Fatalf("pass = %v, want the tiebreaker's decision; out=%+v", out.Pass, out)
			}
			if !equalRoles(roles(out.Judgements), []string{JudgeRolePrimary, JudgeRoleShadow, JudgeRoleTiebreaker}) {
				t.Fatalf("judgements = %+v", out.Judgements)
			}
		})
	}
}

func TestRegisterLLMGatesWired_SetsShadowOnBothGates(t *testing.T) {
	r := NewRegistry()
	shadow := &countingJudge{v: RubricVerdict{Score: 0.5, Model: "qwen38-27b"}}
	RegisterLLMGatesWired(r, LLMGateWiring{
		Judge: &FakeRubricJudge{Default: RubricVerdict{Score: 0.9, Model: "luna"}}, Shadow: shadow, ShadowTimeout: time.Second,
	})
	for _, name := range []string{"spec_conformance", "pr_self_review"} {
		gate, err := r.Get(name)
		if err != nil {
			t.Fatalf("%s not registered: %v", name, err)
		}
		out, err := gate.Evaluate(context.Background(), StageInput{})
		if err != nil {
			t.Fatalf("%s evaluate: %v", name, err)
		}
		if !equalRoles(roles(out.Judgements), []string{JudgeRolePrimary, JudgeRoleShadow}) {
			t.Fatalf("%s judgements = %+v", name, out.Judgements)
		}
	}
	if atomic.LoadInt32(&shadow.calls) != 2 {
		t.Fatalf("shadow calls = %d, want one per gate", shadow.calls)
	}
}
