package pipeline

import (
	"strings"
	"testing"

	"github.com/crb2nu/loom/pkg/mills/store"
)

// TestClampMRTitle pins the 2026-08-01 mr-stage 400: GitLab rejects
// merge_requests.title over 255 characters, and council-minted backlog
// titles are unbounded free text.
func TestClampMRTitle(t *testing.T) {
	short := "feat: bounded title"
	if got := ClampMRTitle(short); got != short {
		t.Errorf("short title changed: %q", got)
	}
	if got := ClampMRTitle("  padded  "); got != "padded" {
		t.Errorf("trim: %q", got)
	}

	long := strings.Repeat("x", 300)
	got := ClampMRTitle(long)
	if n := len([]rune(got)); n > gitlabMRTitleMaxChars {
		t.Errorf("clamped length = %d runes, want <= %d", n, gitlabMRTitleMaxChars)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("clamped title should end with ellipsis: %q", got)
	}

	// Rune-safe: multibyte input must not be split mid-rune.
	multi := strings.Repeat("λ", 300)
	got = ClampMRTitle(multi)
	if n := len([]rune(got)); n > gitlabMRTitleMaxChars {
		t.Errorf("multibyte clamped length = %d runes, want <= %d", n, gitlabMRTitleMaxChars)
	}
	if !strings.HasPrefix(got, "λ") || !strings.HasSuffix(got, "…") {
		t.Errorf("multibyte clamp mangled runes: %q", got[:12])
	}
	// Exactly at the cap: untouched.
	exact := strings.Repeat("y", gitlabMRTitleMaxChars)
	if got := ClampMRTitle(exact); got != exact {
		t.Errorf("at-cap title changed (len %d)", len([]rune(got)))
	}
}

func TestDeclaredSlicePaths(t *testing.T) {
	if got := declaredSlicePaths(nil); got != nil {
		t.Errorf("nil item: %v", got)
	}
	item := &store.BacklogItem{
		Slices: []store.Slice{
			{Name: "a", Files: []string{"pkg/x/a.go", " ", "pkg/x/b.go"}},
			{Name: "b", Files: []string{"pkg/x/b.go", "internal/y/new_file.ts"}},
		},
	}
	got := declaredSlicePaths(item)
	want := []string{"pkg/x/a.go", "pkg/x/b.go", "internal/y/new_file.ts"}
	if len(got) != len(want) {
		t.Fatalf("declaredSlicePaths = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("declaredSlicePaths[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
