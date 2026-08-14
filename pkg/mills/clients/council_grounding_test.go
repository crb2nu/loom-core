package clients

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crb2nu/loom/pkg/mills/council"
)

// --- Repo-layout grounding (council editor decomposition) ------------------
//
// Regression for the live failure where every council-created backlog item
// escalated because the editor invented fictional slice file paths
// (pkg/planning/, pkg/pipeline/ — neither exists in loom-core). The fix grounds
// the editor prompt with the real repo tree, mirroring the research stage.

func TestBuildCouncilEditorPrompt_GroundsRepoLayout(t *testing.T) {
	brief := &council.Brief{Markdown: "Ship the thing."}
	repoTree := "cmd/\ncmd/loom-mills-operator/\npkg/\npkg/mills/\npkg/mills/clients/"
	prompt := buildCouncilEditorPrompt(brief, nil, nil, repoTree, "")

	if !strings.Contains(prompt, "## Repository layout") {
		t.Fatalf("prompt missing repo-layout grounding section:\n%s", prompt)
	}
	// The actual tree must be embedded so the model can scope to real paths.
	if !strings.Contains(prompt, "pkg/mills/clients/") {
		t.Errorf("prompt did not embed the repo tree digest:\n%s", prompt)
	}
	// The anti-invention instruction (the whole point) must be present.
	if !strings.Contains(prompt, "Do NOT invent directories") {
		t.Errorf("prompt missing the do-not-invent instruction:\n%s", prompt)
	}
	// Grounding must not clobber the rest of the prompt.
	if !strings.Contains(prompt, "Ship the thing.") {
		t.Errorf("grounding dropped the brief body:\n%s", prompt)
	}
	if !strings.Contains(prompt, "## Backlog Proposals") {
		t.Errorf("grounding dropped the proposals instruction:\n%s", prompt)
	}
}

func TestBuildCouncilEditorPrompt_OmitsLayoutWhenNoRepoTree(t *testing.T) {
	brief := &council.Brief{Markdown: "Ship the thing."}
	// Empty repoTree (no RepoRoot configured) ⇒ prior prompt verbatim.
	prompt := buildCouncilEditorPrompt(brief, nil, nil, "", "")

	if strings.Contains(prompt, "## Repository layout") {
		t.Errorf("empty repo tree should not render the layout section:\n%s", prompt)
	}
	if !strings.Contains(prompt, "Ship the thing.") {
		t.Errorf("prompt dropped brief body:\n%s", prompt)
	}
}

// RepoPackageLayout must surface the real Go packages and omit invented ones,
// so the grounding instruction is anchored on truth rather than the model's
// guess. The alphabetically-early apps/ subtree is the regression guard: a
// whole-tree digest would let it exhaust the entry budget before pkg/ appears
// (the live failure mode); the code-rooted layout must still show pkg/mills/.
func TestRepoPackageLayout_FromRealTree(t *testing.T) {
	root := t.TempDir()
	mustMkFile(t, root, "cmd/loom-mills-operator/main.go")
	mustMkFile(t, root, "pkg/mills/clients/council.go")
	mustMkFile(t, root, "pkg/mills/pipeline/dispatcher.go")
	mustMkFile(t, root, "internal/hud/api.go")
	// A large, alphabetically-early subtree that a flat whole-tree digest
	// would let bury pkg/. Deliberately NO pkg/planning or pkg/pipeline.
	for i := 0; i < 40; i++ {
		mustMkFile(t, root, "apps/loom-companion-ios/Sources/file"+string(rune('a'+i%26))+".swift")
	}

	digest := RepoPackageLayout(root, councilLayoutMaxEntries)
	if digest == "" {
		t.Fatal("expected a non-empty digest for a populated tree")
	}
	// Real code packages present — including the deep one apps/ would have buried.
	for _, want := range []string{"pkg/", "pkg/mills/", "pkg/mills/clients/", "cmd/loom-mills-operator/", "internal/hud/", "apps/"} {
		if !strings.Contains(digest, want) {
			t.Errorf("digest missing real directory %q:\n%s", want, digest)
		}
	}
	// Invented top-level packages must be absent (the actual bug).
	if strings.Contains(digest, "pkg/planning") || strings.Contains(digest, "pkg/pipeline") {
		t.Errorf("digest contains a directory that does not exist:\n%s", digest)
	}
	// Bounded depth: never descends past two levels under a source root.
	if strings.Contains(digest, "apps/loom-companion-ios/Sources/") {
		t.Errorf("apps/ should only be listed at the top level, not walked deep:\n%s", digest)
	}

	section := buildRepoLayoutSection(digest)
	if !strings.Contains(section, "pkg/mills/clients/") {
		t.Errorf("section did not embed the digest:\n%s", section)
	}
	// Empty digest ⇒ empty section (back-compat with no RepoRoot).
	if got := buildRepoLayoutSection(""); got != "" {
		t.Errorf("empty digest should yield empty section, got %q", got)
	}
	// Missing root ⇒ empty digest (grounding inert, never panics).
	if got := RepoPackageLayout(filepath.Join(root, "does-not-exist"), councilLayoutMaxEntries); got != "" {
		t.Errorf("missing root should yield empty digest, got %q", got)
	}
}

func mustMkFile(t *testing.T, root, rel string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", rel, err)
	}
	if err := os.WriteFile(p, []byte("package x\n"), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}
