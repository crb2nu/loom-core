package gates

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type chainTestJudge func(context.Context, string, StageInput) (RubricVerdict, error)

func (f chainTestJudge) Judge(c context.Context, r string, in StageInput) (RubricVerdict, error) {
	return f(c, r, in)
}

func TestChainRubricJudgeFallback(t *testing.T) {
	for _, kind := range []string{"billing", "auth", "rate_limit", "overloaded", "transport", "parse", "refusal", "invalid_request"} {
		t.Run(kind, func(t *testing.T) {
			calls := 0
			j := &ChainRubricJudge{Classify: func(error) string { return kind }, Hops: []RubricJudgeHop{
				{Vendor: "anthropic", Model: "claude", Judge: &FakeRubricJudge{Err: errors.New(kind)}},
				{Vendor: "openai", Model: "gpt", Judge: chainTestJudge(func(context.Context, string, StageInput) (RubricVerdict, error) {
					calls++
					return RubricVerdict{Score: .9}, nil
				})},
			}}
			v, err := j.Judge(context.Background(), "spec_conformance", StageInput{})
			if kind == "invalid_request" {
				if err == nil || calls != 0 {
					t.Fatal(v, err, calls)
				}
				return
			}
			if err != nil || calls != 1 || v.JudgedBy != "tiebreak:openai/gpt" || len(v.Skipped) != 1 || !strings.Contains(v.Skipped[0], kind) {
				t.Fatal(v, err, calls)
			}
		})
	}
}

func TestChainRubricJudgeRecoveryBound(t *testing.T) {
	for _, first := range []string{"parse", "refusal", "billing"} {
		t.Run(first, func(t *testing.T) {
			calls := 0
			j := &ChainRubricJudge{Classify: func(e error) string { return e.Error() }, BreakerReason: func(v string) string {
				if v == "openai" {
					return "breaker_open:billing"
				}
				return ""
			}, Hops: []RubricJudgeHop{
				{Vendor: "anthropic", Judge: &FakeRubricJudge{Err: errors.New(first)}},
				{Vendor: "openai", Judge: chainTestJudge(func(context.Context, string, StageInput) (RubricVerdict, error) {
					t.Fatal("open breaker called")
					return RubricVerdict{}, nil
				})},
				{Vendor: "flexinfer", Judge: &FakeRubricJudge{Err: errors.New("transport")}},
				{Vendor: "anthropic", Model: "last", Judge: chainTestJudge(func(context.Context, string, StageInput) (RubricVerdict, error) {
					calls++
					return RubricVerdict{Score: .2}, nil
				})},
			}}
			v, err := j.Judge(context.Background(), "x", StageInput{})
			if first == "billing" {
				if err != nil || calls != 1 || v.Score != .2 {
					t.Fatal(v, err, calls)
				}
			} else if err == nil || calls != 0 {
				t.Fatal(v, err, calls)
			}
			if len(v.Skipped) != 3 {
				t.Fatal(v)
			}
		})
	}
}

func TestChainRubricJudgeCancellationAndFailVerdict(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	j := &ChainRubricJudge{Hops: []RubricJudgeHop{{Vendor: "a", Judge: chainTestJudge(func(context.Context, string, StageInput) (RubricVerdict, error) {
		calls++
		cancel()
		return RubricVerdict{}, context.Canceled
	})}, {Vendor: "b", Judge: &FakeRubricJudge{Default: RubricVerdict{Score: 1}}}}}
	if _, err := j.Judge(ctx, "x", StageInput{}); !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatal(err, calls)
	}
	if _, err := j.Judge(ctx, "x", StageInput{}); !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatal(err, calls)
	}
	j.Hops[0].Judge = &FakeRubricJudge{Default: RubricVerdict{Score: .1, Model: "low"}}
	v, err := j.Judge(context.Background(), "x", StageInput{})
	if err != nil || v.Score != .1 || v.JudgedBy != "tiebreak:a/low" {
		t.Fatal(v, err)
	}
}

func TestChainRubricJudgeLastVendor(t *testing.T) {
	j := &ChainRubricJudge{Classify: func(error) string { return "billing" }, Hops: []RubricJudgeHop{
		{Vendor: "anthropic", Judge: &FakeRubricJudge{Err: errors.New("billing")}},
		{Vendor: "openai", Judge: &FakeRubricJudge{Err: errors.New("billing")}},
		{Vendor: "flexinfer", Model: "local", Judge: &FakeRubricJudge{Default: RubricVerdict{Score: .9}}},
	}}
	v, err := j.Judge(context.Background(), "x", StageInput{})
	if err != nil || v.JudgedBy != "tiebreak:flexinfer/local" || len(v.Skipped) != 2 {
		t.Fatal(v, err)
	}
	// Missing credentials are skips, and do not consume parse recovery.
	j.Hops[0].Judge = &FakeRubricJudge{Err: ErrJudgeUnparseable}
	j.Hops[1].Judge = nil
	v, err = j.Judge(context.Background(), "x", StageInput{})
	if err != nil || v.JudgedBy != "tiebreak:flexinfer/local" || !strings.Contains(v.Skipped[1], "not_configured") {
		t.Fatal(v, err)
	}
}
