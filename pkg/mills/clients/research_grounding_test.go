package clients

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestExtractReferencedPaths(t *testing.T) {
	notes := "The orchestrator lives in `mills/core/council/orchestrator.py` and " +
		"calls pkg/mills/pipeline/runner.go. See https://example.com/docs/guide.html for context. " +
		"Score was 0.85 and version 1.2.3 shipped. Also github.com/crb2nu/loom is the module. " +
		"Duplicate: pkg/mills/pipeline/runner.go."
	got := ExtractReferencedPaths(notes)
	want := []string{
		"mills/core/council/orchestrator.py",
		"pkg/mills/pipeline/runner.go",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ExtractReferencedPaths()\n got=%v\nwant=%v", got, want)
	}
}

func TestExtractReferencedPaths_NoPaths(t *testing.T) {
	for _, in := range []string{
		"",
		"No file paths here, only prose about the council and the operator.",
		"Numbers like 0.85 and ratios such as a/b are not file paths.",
	} {
		if got := ExtractReferencedPaths(in); len(got) != 0 {
			t.Errorf("ExtractReferencedPaths(%q) = %v, want empty", in, got)
		}
	}
}

// existsSet builds a predicate over a fixed allowlist of real paths.
func existsSet(real ...string) func(string) bool {
	set := make(map[string]struct{}, len(real))
	for _, r := range real {
		set[r] = struct{}{}
	}
	return func(p string) bool {
		_, ok := set[p]
		return ok
	}
}

func TestSanitizeResearchNotes_AllReal_Unchanged(t *testing.T) {
	notes := "Implement in pkg/mills/pipeline/runner.go; tests in pkg/mills/pipeline/runner_test.go."
	out := SanitizeResearchNotes(notes, existsSet(
		"pkg/mills/pipeline/runner.go",
		"pkg/mills/pipeline/runner_test.go",
	), nil)
	if out.Notes != notes {
		t.Errorf("notes changed unexpectedly:\n%q", out.Notes)
	}
	if len(out.Dropped) != 0 || out.Withheld {
		t.Errorf("dropped = %v, withheld = %v, want none/false", out.Dropped, out.Withheld)
	}
}

func TestSanitizeResearchNotes_WholesaleHallucination_Withheld(t *testing.T) {
	// The observed failure: a Go repo described as Python — every
	// referenced path fabricated.
	notes := "The system is organized as mills/core/council/orchestrator.py, " +
		"mills/core/agent/execution_engine.py, mills/telemetry/metrics_collector.py, " +
		"and mills/state/transition_manager.py."
	out := SanitizeResearchNotes(notes, existsSet( /* nothing real */ ), nil)

	if !out.Withheld {
		t.Error("expected Withheld = true")
	}
	if !strings.Contains(out.Notes, "withheld") {
		t.Errorf("expected withheld notice, got:\n%q", out.Notes)
	}
	// The fabricated paths must not survive into the downstream context.
	for _, p := range []string{
		"orchestrator.py", "execution_engine.py",
		"metrics_collector.py", "transition_manager.py",
	} {
		if strings.Contains(out.Notes, p) {
			t.Errorf("withheld notice still leaks fabricated path %q:\n%q", p, out.Notes)
		}
	}
	if len(out.Dropped) != 4 {
		t.Errorf("dropped = %v, want 4 paths", out.Dropped)
	}
}

func TestSanitizeResearchNotes_PartialHallucination_Footer(t *testing.T) {
	// Mostly real (2 of 3) — below the withhold bar, so notes are kept
	// with a warning footer rather than suppressed.
	notes := "Edit pkg/mills/pipeline/runner.go and pkg/mills/clients/flexinfer.go; " +
		"helpers in mills/util/helpers.py."
	out := SanitizeResearchNotes(notes, existsSet(
		"pkg/mills/pipeline/runner.go",
		"pkg/mills/clients/flexinfer.go",
	), nil)
	if out.Withheld {
		t.Error("partial hallucination should not withhold")
	}
	if !strings.HasPrefix(out.Notes, notes) {
		t.Errorf("original notes should be preserved as prefix, got:\n%q", out.Notes)
	}
	if !strings.Contains(out.Notes, "Unverified paths") || !strings.Contains(out.Notes, "mills/util/helpers.py") {
		t.Errorf("expected footer naming the unverified path, got:\n%q", out.Notes)
	}
	if want := []string{"mills/util/helpers.py"}; !reflect.DeepEqual(out.Dropped, want) {
		t.Errorf("dropped = %v, want %v", out.Dropped, want)
	}
}

func TestSanitizeResearchNotes_SingleStray_NotWithheld(t *testing.T) {
	// One fabricated path among one reference is rate 1.0 but below the
	// minimum-count guard, so it takes the lighter footer path, not a
	// full withhold.
	notes := "The only file is mills/ghost.py."
	out := SanitizeResearchNotes(notes, existsSet(), nil)
	if out.Withheld {
		t.Errorf("single stray path should not withhold whole notes, got:\n%q", out.Notes)
	}
	if !strings.Contains(out.Notes, "Unverified paths") {
		t.Errorf("expected footer, got:\n%q", out.Notes)
	}
	if len(out.Dropped) != 1 {
		t.Errorf("dropped = %v, want 1", out.Dropped)
	}
}

func TestSanitizeResearchNotes_SliceDeclaredNewFiles_NotFabricated(t *testing.T) {
	// The 2026-08-01 live false positive: every referenced path came from the
	// item's own plan slices — one real file plus three files the slice
	// declares for CREATION — and the guard withheld the notes as a 4/4
	// "hallucination". Declared paths are the plan's intent, not invention.
	notes := "Extend internal/hud/frontend/src/lib/components/mills/FactoryPanel.svelte; " +
		"add internal/hud/frontend/src/lib/components/mills/MillEfficiencyStrip.svelte, " +
		"internal/hud/frontend/src/lib/utils/millEfficiencyHelpers.ts, and " +
		"internal/hud/frontend/src/lib/utils/millEfficiencyHelpers.test.ts."
	declared := []string{
		"internal/hud/frontend/src/lib/components/mills/FactoryPanel.svelte",
		"internal/hud/frontend/src/lib/components/mills/MillEfficiencyStrip.svelte",
		"internal/hud/frontend/src/lib/utils/millEfficiencyHelpers.ts",
		"internal/hud/frontend/src/lib/utils/millEfficiencyHelpers.test.ts",
	}
	out := SanitizeResearchNotes(notes, existsSet(
		"internal/hud/frontend/src/lib/components/mills/FactoryPanel.svelte",
	), declared)
	if out.Withheld || len(out.Dropped) != 0 || out.Notes != notes {
		t.Errorf("slice-declared paths must not count as fabrications: withheld=%v dropped=%v\nnotes=%q",
			out.Withheld, out.Dropped, out.Notes)
	}
}

func TestSanitizeResearchNotes_DeclaredDoesNotShieldOtherFabrications(t *testing.T) {
	// Declared paths are exempt, but genuinely invented paths beside them
	// must still be caught.
	notes := "Create pkg/new/feature.go per the slice, then wire mills/ghost/orchestrator.py " +
		"and mills/ghost/state_machine.py."
	out := SanitizeResearchNotes(notes, existsSet(), []string{"pkg/new/feature.go"})
	if !out.Withheld {
		t.Errorf("2/3 fabricated (non-declared) should still withhold, got dropped=%v withheld=%v", out.Dropped, out.Withheld)
	}
	if len(out.Dropped) != 2 {
		t.Errorf("dropped = %v, want the 2 ghost paths only", out.Dropped)
	}
	for _, d := range out.Dropped {
		if d == "pkg/new/feature.go" {
			t.Errorf("declared path leaked into dropped: %v", out.Dropped)
		}
	}
}

func TestSanitizeResearchNotes_NilGuardAndEmpty(t *testing.T) {
	if out := SanitizeResearchNotes("anything pkg/x.go", nil, nil); out.Notes != "anything pkg/x.go" || out.Dropped != nil {
		t.Errorf("nil exists should pass through, got %q / %v", out.Notes, out.Dropped)
	}
	if out := SanitizeResearchNotes("   ", existsSet(), nil); out.Notes != "   " || out.Dropped != nil {
		t.Errorf("blank notes should pass through, got %q / %v", out.Notes, out.Dropped)
	}
}

func TestRepoPathChecker(t *testing.T) {
	root := t.TempDir()
	mustMkdirAll(t, filepath.Join(root, "pkg", "mills"))
	mustWrite(t, filepath.Join(root, "pkg", "mills", "runner.go"), "package mills\n")

	check := repoPathChecker(root)
	cases := map[string]bool{
		"pkg/mills/runner.go":   true,
		"./pkg/mills/runner.go": true,
		"/pkg/mills/runner.go":  true,
		"pkg/mills":             true, // directory exists
		"mills/core/ghost.py":   false,
		"../escape.go":          false,
		"":                      false,
	}
	for in, want := range cases {
		if got := check(in); got != want {
			t.Errorf("checker(%q) = %v, want %v", in, got, want)
		}
	}

	// Empty root → nothing exists.
	empty := repoPathChecker("")
	if empty("pkg/mills/runner.go") {
		t.Error("empty-root checker should report nothing exists")
	}
}

func TestRepoTreeDigest(t *testing.T) {
	root := t.TempDir()
	mustMkdirAll(t, filepath.Join(root, "pkg", "mills", "pipeline"))
	mustMkdirAll(t, filepath.Join(root, "cmd", "loom-mills-operator"))
	mustMkdirAll(t, filepath.Join(root, ".git", "refs"))
	mustMkdirAll(t, filepath.Join(root, "node_modules", "left-pad"))
	mustWrite(t, filepath.Join(root, "go.mod"), "module x\n")
	mustWrite(t, filepath.Join(root, "pkg", "mills", "runner.go"), "package mills\n")
	mustWrite(t, filepath.Join(root, ".env"), "SECRET=1\n")

	digest := RepoTreeDigest(root, 2, 80)
	if digest == "" {
		t.Fatal("expected non-empty digest")
	}
	mustContain(t, digest, "go.mod")
	mustContain(t, digest, "pkg/")
	mustContain(t, digest, "cmd/")
	mustContain(t, digest, "pkg/mills/")
	mustContain(t, digest, "cmd/loom-mills-operator/")

	// Noise + dotfiles pruned.
	for _, bad := range []string{".git", "node_modules", ".env", "left-pad"} {
		if strings.Contains(digest, bad) {
			t.Errorf("digest should not contain %q:\n%s", bad, digest)
		}
	}

	// Depth cap: depth-3 file under pipeline/ should not appear.
	mustWrite(t, filepath.Join(root, "pkg", "mills", "pipeline", "deep.go"), "package pipeline\n")
	digest2 := RepoTreeDigest(root, 2, 80)
	if strings.Contains(digest2, "pkg/mills/pipeline/deep.go") {
		t.Errorf("depth-3 file leaked past maxDepth=2:\n%s", digest2)
	}
}

func TestRepoTreeDigest_EmptyOrMissingRoot(t *testing.T) {
	if got := RepoTreeDigest("", 2, 80); got != "" {
		t.Errorf("empty root = %q, want empty", got)
	}
	if got := RepoTreeDigest(filepath.Join(t.TempDir(), "does-not-exist"), 2, 80); got != "" {
		t.Errorf("missing root = %q, want empty", got)
	}
}

func TestRepoTreeDigest_TruncationMarker(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		mustWrite(t, filepath.Join(root, name+".go"), "package x\n")
	}
	digest := RepoTreeDigest(root, 1, 3)
	if !strings.Contains(digest, "truncated") {
		t.Errorf("expected truncation marker with maxEntries=3, got:\n%s", digest)
	}
	// Deterministic + sorted: first kept entries are the alphabetic head.
	lines := strings.Split(digest, "\n")
	got := lines[:3]
	want := []string{"a.go", "b.go", "c.go"}
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("first entries = %v, want %v", got, want)
	}
}

func mustMkdirAll(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func mustContain(t *testing.T, haystack, needle string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Errorf("expected to contain %q:\n%s", needle, haystack)
	}
}
