package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ensureRepoRoot makes the operator-local checkout exist AND track
// origin/main's tip. The refresh runs on EVERY boot, existing clone or
// not: an earlier version fast-pathed out whenever repoRootReady()
// looked healthy, so a clone on the persistent state PVC was fetched
// exactly once — at first population — and then froze. Observed live
// 2026-08-01: the pod's /workspace/loom-core HEAD was 2026-05-10,
// nearly three months stale, which made the research grounding guard
// (clients.SanitizeResearchNotes, statting THIS tree) classify every
// repo path added since May as a hallucination and withhold the
// research notes on every run that touched recent code.
func ensureRepoRoot(ctx context.Context, cfg Config, logger *slog.Logger) error {
	if strings.TrimSpace(cfg.RepoRoot) == "" {
		return nil
	}
	hasClone := false
	if _, err := os.Stat(filepath.Join(cfg.RepoRoot, ".git")); err == nil {
		hasClone = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat repo git dir: %w", err)
	}
	if strings.TrimSpace(cfg.GitLabToken) == "" ||
		strings.TrimSpace(cfg.GitLabAPIURL) == "" ||
		strings.TrimSpace(cfg.GitLabProject) == "" {
		return nil // no origin credentials: keep whatever clone exists (possibly stale)
	}
	if _, err := exec.LookPath("git"); err != nil {
		if hasClone && repoRootReady(cfg.RepoRoot) {
			if logger != nil {
				logger.Warn("git unavailable; continuing with the existing (possibly stale) checkout", "repo_root", cfg.RepoRoot, "error", err)
			}
			return nil
		}
		return fmt.Errorf("git executable unavailable: %w", err)
	}
	repoURL, host, err := gitRepoURL(cfg.GitLabAPIURL, cfg.GitLabProject)
	if err != nil {
		return err
	}
	parent := filepath.Dir(cfg.RepoRoot)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("create repo parent: %w", err)
	}
	home, err := os.MkdirTemp(parent, ".git-home-*")
	if err != nil {
		return fmt.Errorf("create git home: %w", err)
	}
	defer func() { _ = os.RemoveAll(home) }()
	netrc := fmt.Sprintf("machine %s login oauth2 password %s\n", host, cfg.GitLabToken)
	if err := os.WriteFile(filepath.Join(home, ".netrc"), []byte(netrc), 0o600); err != nil {
		return fmt.Errorf("write git credentials: %w", err)
	}

	if hasClone {
		if err := runGit(ctx, home, "", "config", "--global", "--add", "safe.directory", cfg.RepoRoot); err != nil {
			return err
		}
		if err := runGit(ctx, home, cfg.RepoRoot, "remote", "set-url", "origin", repoURL); err != nil {
			return err
		}
		if err := refreshRepoRoot(ctx, home, cfg.RepoRoot); err != nil {
			// Degraded mode: origin unreachable at boot must not take the
			// operator down when a usable — merely possibly stale — checkout
			// exists. Consumers of this tree (research grounding, spawn git
			// capture) tolerate staleness; they cannot tolerate absence.
			if repoRootReady(cfg.RepoRoot) {
				if logger != nil {
					logger.Warn("repo root refresh failed; continuing with the existing (possibly stale) checkout", "repo_root", cfg.RepoRoot, "error", err)
				}
				return nil
			}
			return err
		}
		if logger != nil {
			logger.Info("repo root refreshed to origin/main", "repo_root", cfg.RepoRoot)
		}
		return nil
	}

	if err := os.RemoveAll(cfg.RepoRoot); err != nil {
		return fmt.Errorf("clear incomplete repo root: %w", err)
	}
	return runGit(ctx, home, "", "clone", "--depth=1", "--branch", "main", repoURL, cfg.RepoRoot)
}

// refreshRepoRoot aligns an existing clone's working tree with origin/main's
// current tip.
//
// The old `merge --ff-only` recipe could NEVER update a stale shallow clone:
// a --depth=1 fetch of a moved main delivers a tip whose ancestry does not
// connect to the old shallow HEAD, fast-forward detection fails, and the
// warn-and-continue path left the tree exactly as stale as before — on every
// boot, forever. `checkout -f -B main origin/main` hard-aligns instead: the
// local main branch is repointed at the fetched tip regardless of ancestry
// and the working tree is forced to match. Local tracked modifications are
// discarded by design — this clone is a disposable cache of origin (the
// merger re-fetches and re-creates its branches per transaction; spawn diff
// capture reads origin/ refs it fetches itself); origin is the source of
// truth. Untracked files survive.
//
// The fetch refspec is explicit for the same reason attachGitContext's is:
// the clone is minted `--depth=1 --branch main`, so its remote.origin.fetch
// covers only main and surprises are cheap to rule out.
func refreshRepoRoot(ctx context.Context, home, repoRoot string) error {
	if err := runGit(ctx, home, repoRoot, "fetch", "--depth=1", "origin", "+refs/heads/main:refs/remotes/origin/main"); err != nil {
		return err
	}
	return runGit(ctx, home, repoRoot, "checkout", "-f", "-B", "main", "origin/main")
}

// installRepoGitAuth persists git credentials for the operator-local
// clone so post-boot git commands can reach origin. ensureRepoRoot
// authenticates its clone/fetch via a .netrc in a TEMPORARY home that is
// deleted on return, so every later fetch — in particular the spawn
// client's cumulative diff capture (clients.attachGitContext) — failed
// with "could not read Username for <host>", the capture stayed empty,
// and nonempty_diff escalated finished branch work (issue #224; observed
// live on the 2026-07-08 kill-test run). The credentials go in
// git-credential-store format inside the repo's own .git dir (same pod
// trust boundary as the GITLAB_TOKEN env the operator already carries)
// and are wired via repo-LOCAL config, so nothing outside this clone is
// affected. Runs on every boot — including the repoRootReady fast path —
// so a rotated token heals on the next roll.
func installRepoGitAuth(ctx context.Context, cfg Config) error {
	if strings.TrimSpace(cfg.RepoRoot) == "" ||
		strings.TrimSpace(cfg.GitLabToken) == "" ||
		strings.TrimSpace(cfg.GitLabAPIURL) == "" ||
		strings.TrimSpace(cfg.GitLabProject) == "" {
		return nil
	}
	gitDir := filepath.Join(cfg.RepoRoot, ".git")
	if _, err := os.Stat(gitDir); err != nil {
		return nil // no clone to authenticate (ensureRepoRoot skipped or failed)
	}
	_, host, err := gitRepoURL(cfg.GitLabAPIURL, cfg.GitLabProject)
	if err != nil {
		return err
	}
	credPath := filepath.Join(gitDir, "mills-credentials")
	line := fmt.Sprintf("https://oauth2:%s@%s\n", url.QueryEscape(cfg.GitLabToken), host)
	if err := os.WriteFile(credPath, []byte(line), 0o600); err != nil {
		return fmt.Errorf("write git credential store: %w", err)
	}
	// The temp home lives beside the repo, not in os.TempDir(): the
	// operator pod runs with a read-only root filesystem (/tmp included);
	// only the state PVC (/workspace, /var/lib/loom-mills) is writable.
	// Mirrors ensureRepoRoot's git-home placement.
	home, err := os.MkdirTemp(filepath.Dir(cfg.RepoRoot), ".git-auth-home-*")
	if err != nil {
		return fmt.Errorf("create git home: %w", err)
	}
	defer func() { _ = os.RemoveAll(home) }()
	// Repo-local config persists in .git/config; the temp HOME only
	// shields this one invocation from prompting.
	return runGit(ctx, home, cfg.RepoRoot, "config", "credential.helper", "store --file="+credPath)
}

func repoRootReady(repoRoot string) bool {
	if _, err := os.Stat(filepath.Join(repoRoot, ".git")); err != nil {
		return false
	}
	loomDir := filepath.Join(repoRoot, ".loom")
	info, err := os.Stat(loomDir)
	if err != nil || !info.IsDir() {
		return false
	}
	f, err := os.CreateTemp(loomDir, ".repo-root-check-*")
	if err != nil {
		return false
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return true
}

func gitRepoURL(apiURL, project string) (repoURL string, host string, err error) {
	project = strings.TrimSpace(project)
	if project == "" {
		return "", "", errors.New("git repo url: project required")
	}
	if strings.IndexFunc(project, func(r rune) bool { return r < '0' || r > '9' }) == -1 {
		return "", "", fmt.Errorf("git repo url: project path required, got numeric project %q", project)
	}
	unescapedProject, err := url.PathUnescape(project)
	if err != nil {
		return "", "", fmt.Errorf("git repo url: decode project: %w", err)
	}
	base := strings.TrimRight(strings.TrimSpace(apiURL), "/")
	base = strings.TrimSuffix(base, "/api/v4")
	u, err := url.Parse(base)
	if err != nil {
		return "", "", fmt.Errorf("git repo url: parse api url: %w", err)
	}
	if u.Scheme == "" || u.Host == "" {
		return "", "", fmt.Errorf("git repo url: api url must include scheme and host")
	}
	u.User = nil
	u.Path = strings.TrimRight(u.Path, "/") + "/" + strings.Trim(strings.TrimSuffix(unescapedProject, ".git"), "/") + ".git"
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), u.Hostname(), nil
}

func runGit(ctx context.Context, home, dir string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(), "HOME="+home, "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}
