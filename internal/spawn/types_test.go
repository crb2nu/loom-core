package spawn

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/util/validation"
)

func TestKubernetesLabelValue(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "project path", input: "services/loom-core", want: "services-loom-core"},
		{name: "already valid", input: "claude-code", want: "claude-code"},
		{name: "trim separators", input: "/services/loom-core/", want: "services-loom-core"},
		{name: "collapse invalid run", input: "services///loom-core", want: "services-loom-core"},
		{name: "empty", input: "  ", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := KubernetesLabelValue(tt.input); got != tt.want {
				t.Fatalf("KubernetesLabelValue(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestKubernetesLabelValueBoundsAndValidates(t *testing.T) {
	t.Parallel()

	for _, input := range []string{
		strings.Repeat("project-", 20),
		"///",
		"services/loom core",
		"services/loom_core.release",
	} {
		got := KubernetesLabelValue(input)
		if len(got) > 63 {
			t.Errorf("KubernetesLabelValue(%q) length = %d, want <= 63", input, len(got))
		}
		if problems := validation.IsValidLabelValue(got); len(problems) != 0 {
			t.Errorf("KubernetesLabelValue(%q) = %q is invalid: %v", input, got, problems)
		}
	}

	first := KubernetesLabelValue(strings.Repeat("a", 80))
	second := KubernetesLabelValue(strings.Repeat("a", 79) + "b")
	if first == second {
		t.Fatalf("distinct truncated values collided: %q", first)
	}
}
