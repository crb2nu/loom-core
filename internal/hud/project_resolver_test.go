package hud

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestResolveProjectPath_BucketedLayout is the regression test for
// spawn_id=spawn-091119fc0ae5 on 2026-04-21 where runSpawn reached
// "/Users/cblevins/workspace/loom-core" (missing services/ prefix) and
// detect.Fingerprint returned "no languages detected". The resolver
// must discover monorepo repos under the standard buckets.
func TestResolveProjectPath_BucketedLayout(t *testing.T) {
	ws := t.TempDir()
	repo := filepath.Join(ws, "services", "loom-core")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	abs, rel, err := resolveProjectPath(ws, "loom-core")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if abs != repo {
		t.Errorf("abs = %q, want %q", abs, repo)
	}
	if rel != "services/loom-core" {
		t.Errorf("rel = %q, want %q (pod WorkDir becomes /workspace/%s)", rel, "services/loom-core", rel)
	}
}

func TestResolveProjectPath_SearchOrder(t *testing.T) {
	// Each bucket in projectBuckets should resolve when a project name
	// matches a directory under it. Platform and libs buckets keep
	// mcp-devbox's resolver and the HUD spawn resolver in sync.
	ws := t.TempDir()
	for _, bucket := range projectBuckets {
		bucketDir := filepath.Join(ws, bucket, "demo-"+bucket)
		if err := os.MkdirAll(bucketDir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", bucketDir, err)
		}
	}
	for _, bucket := range projectBuckets {
		name := "demo-" + bucket
		_, rel, err := resolveProjectPath(ws, name)
		if err != nil {
			t.Errorf("bucket=%s: %v", bucket, err)
			continue
		}
		want := bucket + "/" + name
		if rel != want {
			t.Errorf("bucket=%s rel=%q want=%q", bucket, rel, want)
		}
	}
}

func TestResolveProjectPath_WorkspaceRelativeAccepted(t *testing.T) {
	// Callers passing an explicit workspace-relative path like
	// "services/loom-core" should get the same resolution as the bare
	// project name — no double prefix, no search.
	ws := t.TempDir()
	repo := filepath.Join(ws, "services", "loom-core")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	abs, rel, err := resolveProjectPath(ws, "services/loom-core")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if abs != repo {
		t.Errorf("abs = %q, want %q", abs, repo)
	}
	if rel != "services/loom-core" {
		t.Errorf("rel = %q, want %q", rel, "services/loom-core")
	}
}

func TestResolveProjectPath_RootLevelRepo(t *testing.T) {
	// Backwards compatibility: a repo directly under the workspace root
	// (no bucket prefix) must still resolve so existing tooling that
	// clones into ~/workspace/<repo> keeps working.
	ws := t.TempDir()
	repo := filepath.Join(ws, "flat-repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	abs, rel, err := resolveProjectPath(ws, "flat-repo")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if abs != repo {
		t.Errorf("abs = %q, want %q", abs, repo)
	}
	if rel != "flat-repo" {
		t.Errorf("rel = %q, want %q", rel, "flat-repo")
	}
}

func TestResolveProjectPath_AbsolutePathMustBeInWorkspace(t *testing.T) {
	ws := t.TempDir()
	outside := t.TempDir()
	_, _, err := resolveProjectPath(ws, outside)
	if err == nil {
		t.Fatal("expected error for path outside workspace root")
	}
	if !strings.Contains(err.Error(), "not under workspace root") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestResolveProjectPath_TraversalRejected(t *testing.T) {
	ws := t.TempDir()
	cases := []string{
		"../escape",
		"../../etc/passwd",
		"/etc/passwd",
	}
	for _, project := range cases {
		_, _, err := resolveProjectPath(ws, project)
		if err == nil {
			t.Errorf("project=%q expected error, got nil", project)
		}
	}
}

func TestResolveProjectPath_NotFound(t *testing.T) {
	ws := t.TempDir()
	_, _, err := resolveProjectPath(ws, "does-not-exist")
	if err == nil {
		t.Fatal("expected error for missing project")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestResolveProjectPath_EmptyInputs(t *testing.T) {
	if _, _, err := resolveProjectPath("", "loom-core"); err == nil {
		t.Error("expected error when workspace root is empty")
	}
	if _, _, err := resolveProjectPath(t.TempDir(), ""); err == nil {
		t.Error("expected error when project is empty")
	}
}

// TestLexicalProjectRel covers the git-clone fallback used by runSpawn when a
// target repo is not staged on the workspace mount. A bare name resolves under
// the default (services) bucket; a bucket-qualified name is returned verbatim;
// unsafe inputs are rejected.
func TestLexicalProjectRel(t *testing.T) {
	cases := []struct {
		project string
		want    string
		wantErr bool
	}{
		{"flexdeck", "services/flexdeck", false},
		{"loom-core", "services/loom-core", false},
		{"services/loom-core", "services/loom-core", false},
		{"libs/svg-sdk", "libs/svg-sdk", false},
		{"platform/gitops", "platform/gitops", false},
		{"services/nested/repo", "services/nested/repo", false},
		{"", "", true},
		{"/etc/passwd", "", true},
		{"../escape", "", true},
		{"../../etc/passwd", "", true},
		{".", "", true},
	}
	for _, tc := range cases {
		got, err := lexicalProjectRel(tc.project)
		if tc.wantErr {
			if err == nil {
				t.Errorf("project=%q: expected error, got %q", tc.project, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("project=%q: unexpected error: %v", tc.project, err)
			continue
		}
		if got != tc.want {
			t.Errorf("project=%q: got %q, want %q", tc.project, got, tc.want)
		}
	}
}

// TestGitCloneMode verifies the sync-mode gate that decides whether project
// resolution may fall back to a lexical path. Only "git-clone" opts in; empty
// and other modes keep the strict hard-fail.
func TestGitCloneMode(t *testing.T) {
	cases := []struct {
		mode string
		want bool
	}{
		{"git-clone", true},
		{"nfs", false},
		{"tar-pipe", false},
		{"", false},
	}
	for _, tc := range cases {
		o := &SpawnOrchestrator{syncMode: tc.mode}
		if got := o.gitCloneMode(); got != tc.want {
			t.Errorf("syncMode=%q: gitCloneMode()=%v, want %v", tc.mode, got, tc.want)
		}
	}
}
