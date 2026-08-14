// cmd_worktree.go implements `loom worktree gc`, a unified, sprawl-resistant
// garbage collector for agent git worktrees.
//
// The key idea is that EVERY agent's worktree is registered in its canonical
// repo's `.git/worktrees/` regardless of where the working directory physically
// lives, so a single `git worktree list` per canonical repo uniformly surfaces
// them all:
//
//   - Claude Code:  <repo>/.claude/worktrees/<name>
//   - manual/loom:  <repo>/.worktrees/<branch>
//   - Codex:        ~/.codex/worktrees/<hash>/<repo>   (.git -> <repo>/.git/worktrees/<name>)
//   - gemini/zed/…: <repo>/.<agent>/worktrees/<name>
//
// GC removes ONLY trees that are unambiguously safe to drop: the branch is
// merged into the repo's default branch, OR the working tree is clean AND every
// commit is already reachable from a remote. Dirty trees, trees with
// unpushed/at-risk commits, the main worktree, the worktree the command runs
// in, and recently-active trees are ALWAYS kept. Dry-run is the default; pass
// --execute to actually remove.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// worktreeBuckets are the workspace subdirectories scanned for canonical repos.
var worktreeBuckets = []string{"services", "libs", "labs", "platform", "private", "apps"}

// gitRunner runs `git -C <dir> <args...>` and returns trimmed stdout. It is a
// seam so tests can drive classification without a real repo.
type gitRunner func(ctx context.Context, dir string, args ...string) (string, error)

// statMtimeFn returns a path's modification time. Seam for tests.
type statMtimeFn func(path string) (time.Time, error)

func execGit(ctx context.Context, dir string, args ...string) (string, error) {
	full := append([]string{"-C", dir}, args...)
	out, err := exec.CommandContext(ctx, "git", full...).Output()
	return strings.TrimSpace(string(out)), err
}

func fileMtime(path string) (time.Time, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return time.Time{}, err
	}
	return fi.ModTime(), nil
}

// worktreeRec is one linked worktree plus its classification + GC decision.
type worktreeRec struct {
	Repo      string `json:"repo"`
	Path      string `json:"path"`
	Branch    string `json:"branch,omitempty"`
	Head      string `json:"head,omitempty"`
	Agent     string `json:"agent"`
	Dirty     bool   `json:"dirty"`
	OnRemote  bool   `json:"on_remote"`
	Merged    bool   `json:"merged"`
	Current   bool   `json:"current"`
	IdleAge   string `json:"idle_age,omitempty"`
	idleSince time.Time
	Remove    bool   `json:"remove"`
	Reason    string `json:"reason"`
}

// wtGCConfig holds the resolved knobs for one GC run.
type wtGCConfig struct {
	WorkspaceRoot string
	Repo          string // limit to a single repo (absolute); "" = all buckets
	Policy        string // "merged-or-pushed" (default) | "merged"
	MinIdle       time.Duration
	Execute       bool
	CurrentTop    string // toplevel of cwd; never removed
	Now           time.Time
}

type wtGC struct {
	cfg       wtGCConfig
	git       gitRunner
	statMtime statMtimeFn
}

// agentForPath infers the owning agent convention from a worktree path.
func agentForPath(p string) string {
	switch {
	case strings.Contains(p, "/.claude/worktrees/"):
		return "claude"
	case strings.Contains(p, "/.codex/worktrees/"), strings.Contains(p, "/.codex-worktrees/"):
		return "codex"
	case strings.Contains(p, "/.gemini/worktrees/"):
		return "gemini"
	case strings.Contains(p, "/.opencode/worktrees/"):
		return "opencode"
	case strings.Contains(p, "/.zed/worktrees/"):
		return "zed"
	case strings.Contains(p, "/.kilocode/worktrees/"):
		return "kilocode"
	case strings.Contains(p, "/.worktrees/"):
		return "manual"
	default:
		return "other"
	}
}

// discoverRepos walks the workspace buckets for canonical repos (a directory
// containing a `.git` directory). A single explicit --repo short-circuits it.
func (g *wtGC) discoverRepos() ([]string, error) {
	if g.cfg.Repo != "" {
		return []string{g.cfg.Repo}, nil
	}
	var repos []string
	for _, bucket := range worktreeBuckets {
		bucketDir := filepath.Join(g.cfg.WorkspaceRoot, bucket)
		entries, err := os.ReadDir(bucketDir)
		if err != nil {
			continue // bucket may not exist on every machine
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			repo := filepath.Join(bucketDir, e.Name())
			// Canonical repo: `.git` is a directory (linked worktrees use a file).
			if fi, err := os.Stat(filepath.Join(repo, ".git")); err == nil && fi.IsDir() {
				repos = append(repos, repo)
			}
		}
	}
	sort.Strings(repos)
	return repos, nil
}

// listWorktrees returns the LINKED worktrees of a repo (the main tree is
// excluded). It parses `git worktree list --porcelain`.
func (g *wtGC) listWorktrees(ctx context.Context, repo string) ([]worktreeRec, error) {
	out, err := g.git(ctx, repo, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, fmt.Errorf("worktree list %s: %w", repo, err)
	}
	var recs []worktreeRec
	var cur *worktreeRec
	flush := func() {
		if cur != nil && cur.Path != "" && filepath.Clean(cur.Path) != filepath.Clean(repo) {
			recs = append(recs, *cur)
		}
		cur = nil
	}
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "worktree "):
			flush()
			cur = &worktreeRec{Repo: repo, Path: strings.TrimPrefix(line, "worktree ")}
		case cur == nil:
			continue
		case strings.HasPrefix(line, "HEAD "):
			cur.Head = strings.TrimPrefix(line, "HEAD ")
		case strings.HasPrefix(line, "branch "):
			cur.Branch = strings.TrimPrefix(strings.TrimPrefix(line, "branch "), "refs/heads/")
		}
	}
	flush()
	return recs, nil
}

// defaultRef resolves the repo's default branch remote ref (e.g.
// refs/remotes/origin/main).
func (g *wtGC) defaultRef(ctx context.Context, repo string) string {
	if out, err := g.git(ctx, repo, "symbolic-ref", "refs/remotes/origin/HEAD"); err == nil && out != "" {
		return strings.TrimSpace(out)
	}
	for _, r := range []string{"origin/main", "origin/master"} {
		if _, err := g.git(ctx, repo, "rev-parse", "--verify", "refs/remotes/"+r); err == nil {
			return "refs/remotes/" + r
		}
	}
	return "refs/remotes/origin/main"
}

// classify fills the dirty / on-remote / merged / idle fields for one worktree.
func (g *wtGC) classify(ctx context.Context, repo, defRef string, w *worktreeRec) {
	w.Agent = agentForPath(w.Path)
	w.Current = g.cfg.CurrentTop != "" && filepath.Clean(w.Path) == filepath.Clean(g.cfg.CurrentTop)

	if st, err := g.git(ctx, w.Path, "status", "--porcelain"); err == nil {
		w.Dirty = strings.TrimSpace(st) != ""
	} else {
		// Can't inspect the tree (corrupt/missing) — treat as dirty so we keep it.
		w.Dirty = true
	}

	// One call yields both on-remote and merged: which remote refs contain HEAD.
	if refs, err := g.git(ctx, w.Path, "for-each-ref", "--format=%(refname)", "--contains", "HEAD", "refs/remotes/"); err == nil {
		for _, r := range strings.Split(refs, "\n") {
			r = strings.TrimSpace(r)
			if r == "" {
				continue
			}
			w.OnRemote = true
			if r == defRef {
				w.Merged = true
			}
		}
	}

	// Idle age from the linked worktree's gitdir HEAD mtime, which git rewrites on
	// checkout/commit/reset (real activity) but NOT on `git status` — so our own
	// dirtiness probe above can't make a tree look freshly-touched. Best-effort;
	// absence => zero (treated as old / not active).
	if gd, err := g.git(ctx, w.Path, "rev-parse", "--absolute-git-dir"); err == nil && gd != "" {
		if mt, err := g.statMtime(filepath.Join(gd, "HEAD")); err == nil {
			w.idleSince = mt
			if age := g.cfg.Now.Sub(mt); age > 0 {
				w.IdleAge = age.Round(time.Minute).String()
			}
		}
	}
}

// decide applies the keep/remove policy. Safety guards win over removal.
func (g *wtGC) decide(w *worktreeRec) {
	switch {
	case w.Current:
		w.Reason = "current worktree"
	case w.Dirty:
		w.Reason = "dirty (uncommitted changes)"
	case !w.OnRemote && !w.Merged:
		w.Reason = "at-risk (commits not on any remote)"
	case g.cfg.MinIdle > 0 && !w.idleSince.IsZero() && g.cfg.Now.Sub(w.idleSince) < g.cfg.MinIdle:
		w.Reason = "active (recently touched)"
	case w.Merged:
		w.Remove = true
		w.Reason = "merged into default branch"
	case g.cfg.Policy != "merged" && w.OnRemote:
		w.Remove = true
		w.Reason = "clean + fully pushed"
	default:
		w.Reason = "unmerged (clean but not pushed to default)"
	}
}

// plan discovers, classifies, and decides every linked worktree.
func (g *wtGC) plan(ctx context.Context) ([]worktreeRec, error) {
	repos, err := g.discoverRepos()
	if err != nil {
		return nil, err
	}
	var all []worktreeRec
	for _, repo := range repos {
		recs, err := g.listWorktrees(ctx, repo)
		if err != nil {
			continue // a single unreadable repo must not abort the sweep
		}
		if len(recs) == 0 {
			continue
		}
		defRef := g.defaultRef(ctx, repo)
		for i := range recs {
			g.classify(ctx, repo, defRef, &recs[i])
			g.decide(&recs[i])
			all = append(all, recs[i])
		}
	}
	return all, nil
}

// remove deletes a worktree's working dir + prunes metadata, and deletes the
// local branch when it was merged (safe).
func (g *wtGC) remove(ctx context.Context, w worktreeRec) error {
	if _, err := g.git(ctx, w.Repo, "worktree", "remove", "--force", w.Path); err != nil {
		return fmt.Errorf("remove %s: %w", w.Path, err)
	}
	if w.Merged && w.Branch != "" {
		_, _ = g.git(ctx, w.Repo, "branch", "-D", w.Branch) // best-effort
	}
	_, _ = g.git(ctx, w.Repo, "worktree", "prune")
	return nil
}

func newWorktreeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "worktree",
		Short: "Inspect and garbage-collect agent git worktrees",
		Long: `Manage the git worktrees created by coding agents (Claude, Codex, and
manual/loom trees) across the workspace's canonical repos.`,
	}
	cmd.AddCommand(newWorktreeGCCmd())
	return cmd
}

func newWorktreeGCCmd() *cobra.Command {
	var (
		execute       bool
		jsonOut       bool
		repo          string
		workspaceRoot string
		policy        string
		minIdle       time.Duration
		allTrees      bool
	)
	cmd := &cobra.Command{
		Use:   "gc",
		Short: "Garbage-collect safe-to-remove agent worktrees (dry-run by default)",
		Long: `Sweep every canonical repo in the workspace, classify each linked
worktree (Claude / Codex / manual alike), and remove only the unambiguously
safe ones: branch merged into the repo's default branch, OR a clean working
tree whose commits are all on a remote.

Always kept: dirty trees, trees with unpushed/at-risk commits, the main tree,
the worktree you are currently in, and recently-active trees (--min-idle).

Dry-run by default; pass --execute to actually remove.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			root := workspaceRoot
			if root == "" {
				root = filepath.Join(os.Getenv("HOME"), "workspace")
			}
			repoAbs := repo
			if repoAbs != "" && !filepath.IsAbs(repoAbs) {
				if abs, err := filepath.Abs(repoAbs); err == nil {
					repoAbs = abs
				}
			}
			curTop, _ := execGit(ctx, ".", "rev-parse", "--show-toplevel")

			g := &wtGC{
				cfg: wtGCConfig{
					WorkspaceRoot: root,
					Repo:          repoAbs,
					Policy:        policy,
					MinIdle:       minIdle,
					Execute:       execute,
					CurrentTop:    curTop,
					Now:           time.Now(),
				},
				git:       execGit,
				statMtime: fileMtime,
			}

			recs, err := g.plan(ctx)
			if err != nil {
				return err
			}
			return runWorktreeGC(cmd.OutOrStdout(), g, recs, jsonOut, allTrees)
		},
	}
	cmd.Flags().BoolVar(&execute, "execute", false, "Actually remove safe worktrees (default: dry-run report)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit machine-readable JSON")
	cmd.Flags().StringVar(&repo, "repo", "", "Limit to a single repo path (default: all workspace buckets)")
	cmd.Flags().StringVar(&workspaceRoot, "workspace", "", "Workspace root (default: $HOME/workspace)")
	cmd.Flags().StringVar(&policy, "policy", "merged-or-pushed", "Removal policy: merged-or-pushed | merged")
	cmd.Flags().DurationVar(&minIdle, "min-idle", 30*time.Minute, "Keep worktrees touched more recently than this (0 disables)")
	cmd.Flags().BoolVar(&allTrees, "all", false, "In the report, also list kept worktrees (not just removable)")
	return cmd
}

// runWorktreeGC renders the plan and, when execute is set, removes the safe trees.
func runWorktreeGC(w io.Writer, g *wtGC, recs []worktreeRec, jsonOut, allTrees bool) error {
	sort.Slice(recs, func(i, j int) bool {
		if recs[i].Remove != recs[j].Remove {
			return recs[i].Remove // removable first
		}
		if recs[i].Agent != recs[j].Agent {
			return recs[i].Agent < recs[j].Agent
		}
		return recs[i].Path < recs[j].Path
	})

	var removable []worktreeRec
	for _, r := range recs {
		if r.Remove {
			removable = append(removable, r)
		}
	}

	type result struct {
		w   worktreeRec
		err error
	}
	var executed []result
	if g.cfg.Execute {
		for _, r := range removable {
			err := g.remove(context.Background(), r)
			executed = append(executed, result{w: r, err: err})
		}
	}

	if jsonOut {
		out := map[string]any{
			"policy":    g.cfg.Policy,
			"executed":  g.cfg.Execute,
			"total":     len(recs),
			"removable": len(removable),
			"worktrees": recs,
		}
		if g.cfg.Execute {
			removed := 0
			failed := 0
			for _, e := range executed {
				if e.err == nil {
					removed++
				} else {
					failed++
				}
			}
			out["removed"] = removed
			out["failed"] = failed
		}
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}

	byAgent := map[string]int{}
	for _, r := range recs {
		byAgent[r.Agent]++
	}
	fmt.Fprintf(w, "Worktree GC — policy=%s, %d worktrees across the workspace\n", g.cfg.Policy, len(recs))
	agents := make([]string, 0, len(byAgent))
	for a := range byAgent {
		agents = append(agents, a)
	}
	sort.Strings(agents)
	parts := make([]string, 0, len(agents))
	for _, a := range agents {
		parts = append(parts, fmt.Sprintf("%s=%d", a, byAgent[a]))
	}
	fmt.Fprintf(w, "  by agent: %s\n\n", strings.Join(parts, " "))

	verb := "WOULD REMOVE"
	if g.cfg.Execute {
		verb = "REMOVED"
	}
	if len(removable) == 0 {
		fmt.Fprintln(w, "Nothing safe to remove. ✓")
	} else {
		fmt.Fprintf(w, "%s (%d):\n", verb, len(removable))
		for _, r := range removable {
			status := ""
			if g.cfg.Execute {
				status = "  ✓"
				for _, e := range executed {
					if e.w.Path == r.Path && e.err != nil {
						status = "  ✗ " + e.err.Error()
					}
				}
			}
			fmt.Fprintf(w, "  [%s] %s (%s) — %s%s\n", r.Agent, r.Path, branchLabel(r), r.Reason, status)
		}
	}

	if allTrees {
		var kept []worktreeRec
		for _, r := range recs {
			if !r.Remove {
				kept = append(kept, r)
			}
		}
		if len(kept) > 0 {
			fmt.Fprintf(w, "\nKEPT (%d):\n", len(kept))
			for _, r := range kept {
				fmt.Fprintf(w, "  [%s] %s (%s) — %s\n", r.Agent, r.Path, branchLabel(r), r.Reason)
			}
		}
	}

	if !g.cfg.Execute && len(removable) > 0 {
		fmt.Fprintf(w, "\nDry-run. Re-run with --execute to remove the %d worktree(s) above.\n", len(removable))
	}
	return nil
}

func branchLabel(r worktreeRec) string {
	if r.Branch == "" {
		return "detached"
	}
	return r.Branch
}
