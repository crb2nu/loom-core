package hud

import (
	"strings"
	"testing"
)

func TestCompactTaskLabel(t *testing.T) {
	millsPrompt := "WHAT THIS PIPELINE HAS ALREADY DONE FOR THIS ITEM (recorded by the pipeline, in order — treat it as fact, not as a suggestion):\n\n" +
		"- research: found the defect in pkg/mills/pipeline/dispatcher.go\n" +
		"- plan_slice: two slices\n\n" +
		"## Task\n" +
		"Implement the ci_watch session-deadline fix described in the spec and push to the branch.\n"
	cases := []struct {
		name, in, want string
	}{
		{"plain sentence", "Fix the login timeout in the auth service", "Fix the login timeout in the auth service"},
		{"skips shouting header and heading, keeps first summary line", millsPrompt, "research: found the defect in pkg/mills/pipeline/dispatcher.go"},
		{"markdown heading then body", "# Cloth Hall S3\n\nRender server cards in the shift report.", "Cloth Hall S3"},
		{"only headings", "SECTION ONE:\n## Notes\n---", ""},
		{"empty", "  \n\n", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := compactTaskLabel(tc.in); got != tc.want {
				t.Fatalf("compactTaskLabel(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestCompactTaskLabel_Truncates(t *testing.T) {
	long := strings.Repeat("word ", 60)
	got := compactTaskLabel(long)
	if len(got) > spawnTaskLabelMax+3 {
		t.Fatalf("label too long (%d): %q", len(got), got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("truncated label should end with an ellipsis: %q", got)
	}
}
