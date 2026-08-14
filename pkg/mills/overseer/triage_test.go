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
