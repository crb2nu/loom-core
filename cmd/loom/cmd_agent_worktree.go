// cmd_agent_worktree.go implements `loom agent worktree-cleanup-self`, the
// session-end companion to `loom worktree gc`.
//
// Generated SessionEnd hooks (pkg/generator/configs_hooks.go) invoke this to
// auto-release the harness-allocated worktree the agent worked in, but ONLY
// when it is unambiguously safe: the branch is merged into the repo's default
// branch, or the tree is clean and every commit is reachable from a remote.
// Dirty trees and trees with at-risk commits are always kept. The
// classification is shared with `loom worktree gc` (wtGC in cmd_worktree.go).
package main

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
)

// worktreeCleanupSelf classifies a single worktree with the standard GC
// policy and, when execute is set and the tree is safe, removes it. It
// returns whether the tree was (or would be) removed and the human-readable
// keep/remove reason.
//
// Unlike `loom worktree gc`, this intentionally does NOT protect the
// "current" worktree: the caller is a SessionEnd hook running from inside the
// tree it wants to release. Safety comes from the clean+pushed/merged
// classification, and MinIdle is forced to zero because session end is an
// explicit signal, not an idleness heuristic.
func worktreeCleanupSelf(ctx context.Context, g *wtGC, target string, execute bool) (removed bool, reason string, err error) {
	target = filepath.Clean(target)

	if agentForPath(target) == "other" {
		return false, "not an agent-convention worktree path", nil
	}

	// Resolve the canonical repo from the linked worktree's common git dir.
	commonDir, gerr := g.git(ctx, target, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if gerr != nil || commonDir == "" {
		return false, "not a git worktree", nil
	}
	mainRepo := commonDir
	if filepath.Base(commonDir) == ".git" {
		mainRepo = filepath.Dir(commonDir)
	}
	if filepath.Clean(mainRepo) == target {
		return false, "refusing to touch the main worktree", nil
	}

	g.cfg.Repo = mainRepo
	g.cfg.MinIdle = 0
	g.cfg.CurrentTop = "" // deliberate: self-cleanup runs from inside the target

	recs, err := g.plan(ctx)
	if err != nil {
		return false, "", err
	}
	for _, rec := range recs {
		if filepath.Clean(rec.Path) != target {
			continue
		}
		if !rec.Remove {
			return false, rec.Reason, nil
		}
		if !execute {
			return true, rec.Reason + " (dry-run)", nil
		}
		if err := g.remove(ctx, rec); err != nil {
			return false, rec.Reason, err
		}
		return true, rec.Reason, nil
	}
	return false, fmt.Sprintf("not a linked worktree of %s", mainRepo), nil
}

func newAgentWorktreeCleanupSelfCmd() *cobra.Command {
	var (
		worktreePath string
		quiet        bool
		dryRun       bool
	)
	cmd := &cobra.Command{
		Use:   "worktree-cleanup-self",
		Short: "Release the current agent worktree when it is merged or clean+pushed",
		Long: `Classify one agent worktree with the same safety policy as
'loom worktree gc' (merged into the default branch, or clean with every
commit on a remote) and remove it when safe. Dirty trees and trees with
unpushed commits are always kept.

Designed to be called from generated SessionEnd hooks; keep decisions exit 0
so hook chains never fail on a kept tree.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			target := worktreePath
			if target == "" {
				top, err := execGit(ctx, ".", "rev-parse", "--show-toplevel")
				if err != nil || top == "" {
					return fmt.Errorf("no --worktree-path and cwd is not a git worktree")
				}
				target = top
			}
			abs, err := filepath.Abs(target)
			if err != nil {
				return err
			}

			g := &wtGC{
				cfg: wtGCConfig{
					Policy: "merged-or-pushed",
					Now:    time.Now(),
				},
				git:       execGit,
				statMtime: fileMtime,
			}
			removed, reason, err := worktreeCleanupSelf(ctx, g, abs, !dryRun)
			if err != nil {
				return fmt.Errorf("worktree-cleanup-self %s: %w", abs, err)
			}
			if !quiet {
				verb := "kept"
				if removed {
					verb = "removed"
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s %s: %s\n", verb, abs, reason)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&worktreePath, "worktree-path", "", "Worktree to release (default: toplevel of cwd)")
	cmd.Flags().BoolVar(&quiet, "quiet", false, "Suppress output")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Classify only; do not remove")
	return cmd
}
