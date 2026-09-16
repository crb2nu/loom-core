package overseer

import (
	"context"
	"testing"
)

func TestTriageNilSafety(t *testing.T) {
	var nilTriage *Triage
	if nilTriage.Available() {
		t.Fatal("nil triage reports available")
	}
	empty := &Triage{}
	if empty.Available() {
		t.Fatal("clientless triage reports available")
	}
	var out dupVerdict
	if _, err := empty.Verdict(context.Background(), "p", &out); err == nil {
		t.Fatal("clientless verdict did not error")
	}
}

func TestTriageVerdictParsesFencedJSON(t *testing.T) {
	tr := &Triage{Client: &fakeChat{replies: []string{
		"Reasoning first.\n```json\n{\"verdict\":\"duplicate\",\"confidence\":0.9,\"reason\":\"same\"}\n```\ndone",
	}}}
	var out dupVerdict
	if _, err := tr.Verdict(context.Background(), "p", &out); err != nil {
		t.Fatalf("verdict: %v", err)
	}
	if out.Verdict != "duplicate" || out.Confidence != 0.9 {
		t.Fatalf("out = %+v", out)
	}
}

func TestTriageVerdictRejectsGarbage(t *testing.T) {
	tr := &Triage{Client: &fakeChat{replies: []string{"no json here at all"}}}
	var out dupVerdict
	if _, err := tr.Verdict(context.Background(), "p", &out); err == nil {
		t.Fatal("garbage reply produced a verdict")
	}
}

// TestTriageModelOverridesJudgeModel pins the MILLS_TRIAGE_BACKEND contract:
// an explicit Triage.Model is what every verdict dials; empty keeps the
// client's JudgeModel() (the historical wiring).
func TestTriageModelOverridesJudgeModel(t *testing.T) {
	cases := map[string]struct {
		model string
		want  string
	}{
		"override wins":        {model: "qwen38-27b-xtx-warm-canary", want: "qwen38-27b-xtx-warm-canary"},
		"empty inherits judge": {model: "", want: "fake-judge"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			fc := &fakeChat{replies: []string{verdictJSON(t, "duplicate", 0.9)}}
			tr := &Triage{Client: fc, Model: tc.model}
			var out dupVerdict
			if _, err := tr.Verdict(context.Background(), "p", &out); err != nil {
				t.Fatalf("Verdict: %v", err)
			}
			if len(fc.models) != 1 || fc.models[0] != tc.want {
				t.Fatalf("dialed models = %v, want [%s]", fc.models, tc.want)
			}
			if out.Verdict != "duplicate" {
				t.Fatalf("verdict = %q, want duplicate", out.Verdict)
			}
		})
	}
}

// TestForemanIssueBodyDialsTriageModel covers the second triage call site:
// the foreman's LLM-composed issue body follows the same override.
func TestForemanIssueBodyDialsTriageModel(t *testing.T) {
	fc := &fakeChatClient{reply: "Summary: x\nImpact: y\nSuggested action: z"}
	f := &Foreman{Triage: &Triage{Client: fc, Model: "qwen38-27b-xtx-warm-canary"}}
	body := f.composeIssueBody(context.Background(), &Anomaly{Rule: "r", Severity: "warn", Evidence: map[string]any{"k": 1}})
	if body != "Summary: x\nImpact: y\nSuggested action: z" {
		t.Fatalf("body = %q", body)
	}
	if len(fc.models) != 1 || fc.models[0] != "qwen38-27b-xtx-warm-canary" {
		t.Fatalf("dialed models = %v, want the triage model", fc.models)
	}
}
