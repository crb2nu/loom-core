package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/crb2nu/loom/internal/hud/bridge"
)

func startSessionWithFallback(cmd *cobra.Command, port string, p bridge.SessionStartParams) (json.RawMessage, error) {
	return withAgentFallback(
		"agent session-start",
		func() (json.RawMessage, error) {
			// Skip slow HUD POST when HUD is clearly not reachable.
			if _, err := hudGetFast(port, "/api/ping", sessionStartHUDPingTimeout); err != nil {
				return nil, err
			}
			return hudPostFast(port, bridge.AgentSessionStartEndpoint, p, sessionStartHUDPostTimeout)
		},
		func() (json.RawMessage, error) {
			return withAgentBridge(cmd, func(agentBridge *bridge.AgentBridge) (json.RawMessage, error) {
				result, err := agentBridge.StartSession(p)
				if err != nil {
					return nil, err
				}
				return json.Marshal(result)
			})
		},
	)
}

func endSessionWithFallback(cmd *cobra.Command, port string, p bridge.SessionEndParams) (json.RawMessage, error) {
	return withAgentFallback(
		"agent session-end",
		func() (json.RawMessage, error) {
			return hudPost(port, bridge.AgentSessionEndEndpoint, p)
		},
		func() (json.RawMessage, error) {
			return withAgentBridge(cmd, func(agentBridge *bridge.AgentBridge) (json.RawMessage, error) {
				_, err := agentBridge.EndSession(p)
				if err != nil {
					return nil, err
				}
				return json.Marshal(map[string]bool{"ok": true})
			})
		},
	)
}

func ensureDaemonSession(cmd *cobra.Command, p bridge.SessionStartParams) error {
	_, err := withAgentBridge(cmd, func(agentBridge *bridge.AgentBridge) (json.RawMessage, error) {
		result, err := agentBridge.StartSession(p)
		if err != nil {
			return nil, err
		}
		return json.Marshal(result)
	})
	return err
}

func activeSessionWithFallback(cmd *cobra.Command, port, agentID string) (json.RawMessage, error) {
	path, err := (bridge.SessionRequest{AgentID: agentID}).Path()
	if err != nil {
		return nil, err
	}
	return withAgentFallback(
		"agent session",
		func() (json.RawMessage, error) {
			return hudGet(port, path)
		},
		func() (json.RawMessage, error) {
			return withAgentBridge(cmd, func(agentBridge *bridge.AgentBridge) (json.RawMessage, error) {
				session, err := agentBridge.GetActiveSession(agentID)
				if err != nil {
					return nil, err
				}
				return json.Marshal(map[string]any{"session": session})
			})
		},
	)
}

// newAgentSessionStartCmd creates the `loom agent session-start` command.
func newAgentSessionStartCmd() *cobra.Command {
	var (
		namespace             string
		agentID               string
		agentType             string
		description           string
		autoRecall            bool
		autoRecallStrategy    string
		autoRecallQuery       string
		autoRecallTokenBudget int
		parentSessionID       string
		quiet                 bool
	)

	cmd := &cobra.Command{
		Use:   "session-start",
		Short: "Start an agent session (idempotent)",
		Long: `Start a new agent session, register presence, and optionally recall context.

This command is idempotent: if the agent already has an active session in the
same namespace, the existing session ID is returned without creating a duplicate.

Designed for use in Claude Code SessionStart hooks.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			port := resolvePort(cmd)

			params, err := (bridge.SessionStartParams{
				Namespace:             namespace,
				AgentID:               agentID,
				AgentType:             agentType,
				Description:           description,
				AutoRecall:            autoRecall,
				AutoRecallStrategy:    autoRecallStrategy,
				AutoRecallQuery:       autoRecallQuery,
				AutoRecallTokenBudget: autoRecallTokenBudget,
				ParentSessionID:       parentSessionID,
			}).ToParams()
			if err != nil {
				if quiet {
					return nil
				}
				return err
			}

			result, err := startSessionWithFallback(cmd, port, params)
			if err != nil {
				if quiet {
					return nil // Silent failure for hooks.
				}
				return err
			}

			if !quiet {
				fmt.Println(string(result))
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&namespace, "namespace", "", "Session namespace (e.g., project/feature-branch)")
	cmd.Flags().StringVar(&agentID, "agent-id", "", "Agent identifier (e.g., claude-code)")
	cmd.Flags().StringVar(&agentType, "agent-type", "", "Agent type (e.g., claude-code)")
	cmd.Flags().StringVar(&description, "description", "", "Session description")
	cmd.Flags().BoolVar(&autoRecall, "auto-recall", false, "Auto-recall context on start")
	cmd.Flags().StringVar(&autoRecallStrategy, "auto-recall-strategy", "balanced", "Auto-recall depth profile: fast, balanced, deep")
	cmd.Flags().StringVar(&autoRecallQuery, "auto-recall-query", "", "Override auto-recall query (defaults to description, then namespace)")
	cmd.Flags().IntVar(&autoRecallTokenBudget, "auto-recall-token-budget", 0, "Override auto-recall token budget (256-32000)")
	cmd.Flags().StringVar(&parentSessionID, "parent-session-id", "", "Parent session ID (for subagent session grouping)")
	cmd.Flags().BoolVar(&quiet, "quiet", false, "Suppress output (for hooks)")

	return cmd
}

// newAgentSessionEndCmd creates the `loom agent session-end` command.
func newAgentSessionEndCmd() *cobra.Command {
	var (
		sessionID    string
		agentID      string
		summarize    = true
		summaryAsync bool
		quiet        bool
	)

	cmd := &cobra.Command{
		Use:   "session-end",
		Short: "End an agent session",
		Long: `End the active session, optionally compress context, and deregister presence.

If --session-id is not provided, finds the active session by --agent-id.

Designed for use in Claude Code Stop hooks.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			port := resolvePort(cmd)

			params, err := (bridge.SessionEndParams{
				SessionID:    sessionID,
				AgentID:      agentID,
				Summarize:    &summarize,
				SummaryAsync: summaryAsync,
			}).ToParams()
			if err != nil {
				if quiet {
					return nil
				}
				return err
			}

			result, err := endSessionWithFallback(cmd, port, params)
			if err != nil {
				if quiet {
					return nil
				}
				return err
			}

			if !quiet {
				fmt.Println(string(result))
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&sessionID, "session-id", "", "Session ID to end (optional; finds by agent-id)")
	cmd.Flags().StringVar(&agentID, "agent-id", "", "Agent identifier")
	cmd.Flags().BoolVar(&summarize, "summarize", true, "Summarize and compress context on end")
	cmd.Flags().BoolVar(&summaryAsync, "summary-async", false, "Queue summarization in background and return immediately")
	cmd.Flags().BoolVar(&quiet, "quiet", false, "Suppress output (for hooks)")

	return cmd
}

// newAgentSessionCmd creates the `loom agent session` command.
func newAgentSessionCmd() *cobra.Command {
	var (
		agentID string
		quiet   bool
	)

	cmd := &cobra.Command{
		Use:   "session",
		Short: "Get the active session for an agent",
		Long:  `Query the HUD for the currently active session. Useful for scripts and hooks that need the session ID.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			port := resolvePort(cmd)

			result, err := activeSessionWithFallback(cmd, port, agentID)
			if err != nil {
				if quiet {
					return nil
				}
				return err
			}

			if !quiet {
				fmt.Println(string(result))
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&agentID, "agent-id", "", "Agent identifier")
	cmd.Flags().BoolVar(&quiet, "quiet", false, "Suppress output (for hooks)")

	return cmd
}

// newAgentSessionListCmd creates the `loom agent session-list` command.
func newAgentSessionListCmd() *cobra.Command {
	var (
		namespace string
		agentID   string
		status    string
		limit     int
	)

	cmd := &cobra.Command{
		Use:   "session-list",
		Short: "List agent sessions",
		Long: `List sessions, optionally filtered by agent, namespace, or status.

Example:
  loom agent session-list --status summarized --limit 50
  loom agent session-list --agent-id claude-code --status active`,
		RunE: func(cmd *cobra.Command, args []string) error {
			port := resolvePort(cmd)

			params, err := bridge.SessionListRequest{
				AgentID:   agentID,
				Namespace: namespace,
				Status:    status,
				Limit:     limit,
			}.Params()
			if err != nil {
				return err
			}

			result, err := withAgentFallback(
				"agent session-list",
				func() (json.RawMessage, error) {
					return hudPost(port, bridge.AgentSessionListEndpoint, params)
				},
				func() (json.RawMessage, error) {
					return withAgentBridge(cmd, func(b *bridge.AgentBridge) (json.RawMessage, error) {
						return b.ListSessions(params)
					})
				},
			)
			if err != nil {
				return err
			}
			fmt.Println(string(result))
			return nil
		},
	}

	cmd.Flags().StringVar(&agentID, "agent-id", "", "Filter by agent ID")
	cmd.Flags().StringVar(&namespace, "namespace", "", "Filter by namespace")
	cmd.Flags().StringVar(&status, "status", "", "Filter by status (active, ended, summarized)")
	cmd.Flags().IntVar(&limit, "limit", 20, "Maximum sessions to return")

	return cmd
}

// newAgentSessionPruneCmd creates the `loom agent session-prune` command.
func newAgentSessionPruneCmd() *cobra.Command {
	var (
		maxAge string
		status string
		dryRun bool
	)

	cmd := &cobra.Command{
		Use:   "session-prune",
		Short: "Prune stale sessions",
		Long: `Delete stale sessions matching status and age criteria.

Example:
  loom agent session-prune --max-age 72h --dry-run
  loom agent session-prune --max-age 72h --status summarized,ended`,
		RunE: func(cmd *cobra.Command, args []string) error {
			port := resolvePort(cmd)

			// Parse max-age duration to hours
			dur, err := time.ParseDuration(maxAge)
			if err != nil {
				return fmt.Errorf("invalid --max-age: %w", err)
			}
			maxAgeHours := int(dur.Hours())
			if maxAgeHours <= 0 {
				maxAgeHours = 1
			}

			params, err := bridge.SessionPruneRequest{
				MaxAgeHours: maxAgeHours,
				Status:      status,
				DryRun:      dryRun,
			}.Params()
			if err != nil {
				return err
			}

			result, err := withAgentFallback(
				"agent session-prune",
				func() (json.RawMessage, error) {
					return hudPost(port, bridge.AgentSessionPruneEndpoint, params)
				},
				func() (json.RawMessage, error) {
					return withAgentBridge(cmd, func(b *bridge.AgentBridge) (json.RawMessage, error) {
						return b.PruneSessions(params)
					})
				},
			)
			if err != nil {
				return err
			}
			fmt.Println(string(result))
			return nil
		},
	}

	cmd.Flags().StringVar(&maxAge, "max-age", "72h", "Maximum session age (e.g., 72h, 168h)")
	cmd.Flags().StringVar(&status, "status", "ended,summarized", "Comma-separated status filter")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Preview what would be pruned without deleting")

	return cmd
}

// stripWorktreeFromRepoRoot returns the main repo root for a given git
// repoRoot, collapsing both workspace-standard (<repo>/.worktrees/<branch>)
// and Claude Code tool-managed (<repo>/.claude/worktrees/<branch>) linked
// tree layouts back to <repo>. Returns the input unchanged when neither
// pattern is present.
func stripWorktreeFromRepoRoot(repoRoot string) string {
	if idx := strings.Index(repoRoot, "/.claude/worktrees/"); idx >= 0 {
		return repoRoot[:idx]
	}
	if idx := strings.Index(repoRoot, "/.worktrees/"); idx >= 0 {
		return repoRoot[:idx]
	}
	return repoRoot
}

// inferGitNamespace derives a namespace from the current git repository and branch.
// It prefers the origin remote's "group/repo" identity, which is stable regardless
// of where the repo is physically checked out (canonical path, worktree, or a
// hash-named clone/spawn directory), and falls back to the filesystem path when no
// usable remote is configured. Returns "group/repo/branch", "group/repo", or empty
// string when git context is unavailable.
//
// The remote-first order fixes codex agents running in cloned/spawned workspaces at
// paths like ".../3ef2/conspiracy-files", where the old path-only derivation leaked
// the clone-dir hash ("3ef2") as the project's parent segment.
func inferGitNamespace() string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	project := namespaceProjectFromRemote(ctx)
	if project == "" {
		project = namespaceProjectFromPath(ctx)
	}
	if project == "" {
		return ""
	}

	// Get current branch.
	branch, err := exec.CommandContext(ctx, "git", "branch", "--show-current").Output()
	if err != nil {
		return project
	}
	branchName := strings.TrimSpace(string(branch))
	if branchName == "" {
		return project
	}

	return project + "/" + branchName
}

// namespaceProjectFromRemote derives a stable "group/repo" project identity from the
// origin remote URL. Returns empty when no usable remote is configured.
func namespaceProjectFromRemote(ctx context.Context) string {
	out, err := exec.CommandContext(ctx, "git", "config", "--get", "remote.origin.url").Output()
	if err != nil {
		return ""
	}
	return projectFromRemoteURL(strings.TrimSpace(string(out)))
}

// projectFromRemoteURL parses a git remote URL into a workspace-relative "group/repo"
// project (the last two non-degenerate path segments, matching the 2-level convention
// the filesystem-path derivation uses). Handles URL form
// (https://host[:port]/group/repo.git, ssh://git@host/group/repo.git) and scp-like
// form (git@host:group/repo.git). Returns empty for an unparseable or pathless URL.
func projectFromRemoteURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}

	path := raw
	if i := strings.Index(path, "://"); i >= 0 {
		// scheme://[user@]host[:port]/group/repo(.git)
		rest := path[i+3:]
		slash := strings.IndexByte(rest, '/')
		if slash < 0 {
			return ""
		}
		path = rest[slash+1:]
	} else if at := strings.IndexByte(path, '@'); at >= 0 {
		// scp-like user@host:group/repo(.git)
		if colon := strings.IndexByte(path[at:], ':'); colon >= 0 {
			path = path[at+colon+1:]
		}
	} else if colon := strings.IndexByte(path, ':'); colon >= 0 {
		// host:group/repo(.git) without an explicit user
		path = path[colon+1:]
	}

	path = strings.Trim(path, "/")
	path = strings.TrimSuffix(path, ".git")
	path = strings.Trim(path, "/")
	if path == "" {
		return ""
	}

	// Keep the last two non-degenerate segments (e.g. "services/loom-core",
	// or "subgroup/repo" for a nested group).
	var segs []string
	for _, s := range strings.Split(path, "/") {
		if !isDegeneratePathSegment(s) {
			segs = append(segs, s)
		}
	}
	if len(segs) == 0 {
		return ""
	}
	if len(segs) > 2 {
		segs = segs[len(segs)-2:]
	}
	return strings.Join(segs, "/")
}

// namespaceProjectFromPath derives a "parent/repo" project from the repository's
// filesystem location (worktree-aware), used as a fallback when no origin remote is
// configured. For worktrees under <repo>/.worktrees/ or <repo>/.claude/worktrees/
// (Claude Code tool-managed worktrees), resolves to the parent repo path so
// namespaces stay consistent across main and worktree checkouts. Returns empty when
// git context is unavailable or the path yields degenerate segments.
func namespaceProjectFromPath(ctx context.Context) string {
	toplevel, err := exec.CommandContext(ctx, "git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return ""
	}
	repoRoot := stripWorktreeFromRepoRoot(strings.TrimSpace(string(toplevel)))
	if repoRoot == "" || repoRoot == "/" {
		return ""
	}

	// Use parent/basename for workspace-relative namespacing
	// (e.g. "services/loom-core" instead of just "loom-core").
	parent := filepath.Base(filepath.Dir(repoRoot))
	name := filepath.Base(repoRoot)
	// Reject degenerate path components (root "/", cwd ".", empty) that would
	// otherwise yield malformed namespaces like "////main".
	if isDegeneratePathSegment(parent) || isDegeneratePathSegment(name) {
		return ""
	}
	return parent + "/" + name
}

// isMalformedNamespace reports whether ns contains empty or degenerate path segments
// (e.g. "////main", "a//b") that should never be stored verbatim. An empty or
// whitespace-only namespace is treated as "absent" (not malformed) and reported
// false, so callers apply their normal infer-or-skip handling instead.
func isMalformedNamespace(ns string) bool {
	ns = strings.TrimSpace(ns)
	if ns == "" {
		return false
	}
	for _, seg := range strings.Split(ns, "/") {
		if isDegeneratePathSegment(seg) {
			return true
		}
	}
	return false
}

// isDegeneratePathSegment reports whether a filepath.Base result is a
// non-meaningful path component ("", "/", ".", "..") that should not be used
// to build a namespace.
func isDegeneratePathSegment(s string) bool {
	switch s {
	case "", "/", ".", "..":
		return true
	}
	return false
}
