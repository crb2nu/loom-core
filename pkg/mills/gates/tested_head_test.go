package gates

import (
	"context"
	"strings"
	"testing"
)

func TestTestedHeadGate(t *testing.T) {
	tests := []struct {
		name, tested, head string
		pass, skip         bool
	}{
		{name: "match is case-insensitive", tested: "abc123", head: "ABC123", pass: true},
		{name: "mismatch fails", tested: "aaaa", head: "bbbb"},
		{name: "missing tested sha skips", tested: "", head: "bbbb", pass: true, skip: true},
		{name: "unresolved tested sha skips", tested: "unresolved", head: "bbbb", pass: true, skip: true},
		{name: "unresolved head skips", tested: "aaaa", head: "unresolved", pass: true, skip: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := (&TestedHeadGate{}).Evaluate(context.Background(), StageInput{TestedSHA: tt.tested, ReviewHeadSHA: tt.head})
			if err != nil {
				t.Fatalf("Evaluate: %v", err)
			}
			if out.Pass != tt.pass || out.Skip != tt.skip {
				t.Fatalf("pass=%v skip=%v, want pass=%v skip=%v (%+v)", out.Pass, out.Skip, tt.pass, tt.skip, out)
			}
			if out.JudgedBy != "go" {
				t.Fatalf("judged_by = %q, want go", out.JudgedBy)
			}
			if !tt.pass && (!strings.Contains(out.Reasons[0], tt.tested) || !strings.Contains(out.Reasons[0], tt.head)) {
				t.Fatalf("reason does not name both SHAs: %q", out.Reasons[0])
			}
			if tt.skip && !strings.Contains(out.Reasons[0], "unresolved") {
				t.Fatalf("skip reason should say why: %q", out.Reasons[0])
			}
		})
	}
}

func TestTestedHeadGate_RegisteredByDefault(t *testing.T) {
	g, err := Default().Get(TestedHeadGateName)
	if err != nil {
		t.Fatalf("default registry lacks %s: %v", TestedHeadGateName, err)
	}
	if _, ok := g.(*TestedHeadGate); !ok {
		t.Fatalf("%s registered as %T", TestedHeadGateName, g)
	}
}
