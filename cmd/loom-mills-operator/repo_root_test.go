package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGitRepoURL_DerivesHTTPSCloneURL(t *testing.T) {
	got, host, err := gitRepoURL("https://gitlab.flexinfer.ai/api/v4", "services/loom-core")
	if err != nil {
		t.Fatalf("gitRepoURL: %v", err)
	}
	if got != "https://gitlab.flexinfer.ai/services/loom-core.git" {
		t.Errorf("url = %q", got)
	}
	if host != "gitlab.flexinfer.ai" {
		t.Errorf("host = %q", host)
	}
}

func TestGitRepoURL_RejectsNumericProject(t *testing.T) {
	if _, _, err := gitRepoURL("https://gitlab.flexinfer.ai/api/v4", "47"); err == nil {
		t.Fatal("expected numeric project id to be rejected")
	}
}

// TestInstallRepoGitAuth_PersistsCredentialStore pins the issue #224 auth
// fix: ensureRepoRoot's boot .netrc lives in a temp HOME that is deleted on
// return, so every post-boot fetch (the spawn client's cumulative diff
// capture) failed with "could not read Username" and gates judged empty
// diffs. The durable credential store must survive in the repo's .git dir
// and be wired via repo-local config.
func TestInstallRepoGitAuth_PersistsCredentialStore(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	repo := t.TempDir()
	if out, err := exec.CommandContext(context.Background(), "git", "init", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	cfg := Config{
		RepoRoot:      repo,
		GitLabToken:   "glpat-secret/with+chars",
		GitLabAPIURL:  "https://gitlab.flexinfer.ai/api/v4",
		GitLabProject: "services/loom-core",
	}
	if err := installRepoGitAuth(context.Background(), cfg); err != nil {
		t.Fatalf("installRepoGitAuth: %v", err)
	}
	credPath := filepath.Join(repo, ".git", "mills-credentials")
	cred, err := os.ReadFile(credPath)
	if err != nil {
		t.Fatalf("credential store missing: %v", err)
	}
	if got := strings.TrimSpace(string(cred)); !strings.HasPrefix(got, "https://oauth2:") || !strings.HasSuffix(got, "@gitlab.flexinfer.ai") {
		t.Errorf("credential line = %q", got)
	}
	if strings.Contains(string(cred), "glpat-secret/with+chars") {
		t.Errorf("token must be URL-encoded in store format: %q", cred)
	}
	helper, err := exec.CommandContext(context.Background(), "git", "-C", repo, "config", "credential.helper").Output()
	if err != nil {
		t.Fatalf("read credential.helper: %v", err)
	}
	if got := strings.TrimSpace(string(helper)); got != "store --file="+credPath {
		t.Errorf("credential.helper = %q", got)
	}
	fi, err := os.Stat(credPath)
	if err != nil {
		t.Fatalf("stat credential store: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("credential store mode = %v, want 0600", fi.Mode().Perm())
	}
}

// gitT runs git in dir with a throwaway identity, failing the test on error.
func gitT(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git",
		append([]string{"-c", "user.email=mills@test", "-c", "user.name=mills-test"}, args...)...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

func gitOutT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

func writeT(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestRefreshRepoRoot_AlignsStaleShallowClone pins the 2026-08-01 staleness
// fix: the operator's PVC clone froze at first population (repoRootReady
// fast-pathed the fetch on every later boot), and even the not-ready path's
// `merge --ff-only` could never advance a stale shallow clone (a depth-1
// fetch of a moved main has no ancestry link to the old shallow HEAD). The
// refresh must land origin's current tree regardless of how far behind —
// or locally diverged — the clone is.
func TestRefreshRepoRoot_AlignsStaleShallowClone(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	ctx := context.Background()
	origin := t.TempDir()
	gitT(t, origin, "init", "-b", "main", ".")
	writeT(t, filepath.Join(origin, "a.txt"), "v1\n")
	gitT(t, origin, "add", ".")
	gitT(t, origin, "commit", "-m", "first")

	parent := t.TempDir()
	clone := filepath.Join(parent, "repo")
	// file:// forces the real transport so --depth=1 is honored, matching
	// the operator's shallow clone shape.
	gitT(t, parent, "clone", "--depth=1", "--branch", "main", "file://"+origin, clone)

	// Origin moves on (several commits, so the shallow clone's history has
	// no connection to the new tip) and a tracked file drifts locally.
	writeT(t, filepath.Join(origin, "a.txt"), "v2\n")
	gitT(t, origin, "add", ".")
	gitT(t, origin, "commit", "-m", "second")
	writeT(t, filepath.Join(origin, "b.txt"), "new\n")
	gitT(t, origin, "add", ".")
	gitT(t, origin, "commit", "-m", "third")
	writeT(t, filepath.Join(clone, "a.txt"), "local drift\n")

	if err := refreshRepoRoot(ctx, t.TempDir(), clone); err != nil {
		t.Fatalf("refreshRepoRoot: %v", err)
	}
	if _, err := os.Stat(filepath.Join(clone, "b.txt")); err != nil {
		t.Errorf("clone missing origin's new file after refresh: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(clone, "a.txt"))
	if err != nil || string(got) != "v2\n" {
		t.Errorf("tracked drift must be hard-aligned to origin: content=%q err=%v", got, err)
	}
	if cloneHead, originHead := gitOutT(t, clone, "rev-parse", "HEAD"), gitOutT(t, origin, "rev-parse", "main"); cloneHead != originHead {
		t.Errorf("HEAD = %s, want origin main %s", cloneHead, originHead)
	}
}

// TestInstallRepoGitAuth_NoCloneNoop: a missing .git dir (ensureRepoRoot
// skipped or failed) must not error or create files.
func TestInstallRepoGitAuth_NoCloneNoop(t *testing.T) {
	repo := t.TempDir()
	cfg := Config{
		RepoRoot:      repo,
		GitLabToken:   "tok",
		GitLabAPIURL:  "https://gitlab.flexinfer.ai/api/v4",
		GitLabProject: "services/loom-core",
	}
	if err := installRepoGitAuth(context.Background(), cfg); err != nil {
		t.Fatalf("installRepoGitAuth on non-repo: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, ".git", "mills-credentials")); !os.IsNotExist(err) {
		t.Errorf("credential store should not exist without a clone")
	}
}
