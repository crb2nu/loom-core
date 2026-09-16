package gates

import (
	"context"
	"strings"
	"testing"
)

func TestDiffSizeItemOverride(t *testing.T) {
	gate := &DiffSize{}

	passed, err := gate.Evaluate(context.Background(), StageInput{
		MaxDiffLines: 4000,
		LinesRemoved: 3000,
	})
	if err != nil {
		t.Fatalf("evaluate 3,000-line deletion: %v", err)
	}
	if !passed.Pass {
		t.Fatalf("3,000-line deletion with 4,000 override failed: %v", passed.Reasons)
	}

	failed, err := gate.Evaluate(context.Background(), StageInput{
		MaxDiffLines: 4000,
		LinesRemoved: 5000,
	})
	if err != nil {
		t.Fatalf("evaluate 5,000-line deletion: %v", err)
	}
	if failed.Pass {
		t.Fatal("5,000-line deletion with 4,000 override passed")
	}
	if got := strings.Join(failed.Reasons, " "); !strings.Contains(got, "cap is 4000 (item override)") {
		t.Fatalf("failure reason %q does not identify item override", got)
	}
}

func TestDiffSizeAbsentOrNonPositiveOverrideUsesDefault(t *testing.T) {
	for _, tc := range []struct {
		name         string
		maxDiffLines int
	}{
		{name: "absent", maxDiffLines: 0},
		{name: "negative", maxDiffLines: -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := (&DiffSize{}).Evaluate(context.Background(), StageInput{
				MaxDiffLines: tc.maxDiffLines,
				LinesRemoved: defaultMaxDiffLines + 1,
			})
			if err != nil {
				t.Fatalf("evaluate: %v", err)
			}
			if out.Pass {
				t.Fatalf("override %d passed above default cap", tc.maxDiffLines)
			}
			got := strings.Join(out.Reasons, " ")
			if !strings.Contains(got, "cap is 800") || strings.Contains(got, "item override") {
				t.Fatalf("unexpected default failure reason %q", got)
			}
		})
	}
}
