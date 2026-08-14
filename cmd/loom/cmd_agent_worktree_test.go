package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

// newCleanupFakeGit builds a gitRunner over a single repo with the given
// linked worktrees. states is keyed by worktree path.
func newCleanupFakeGit(repo, porcelain string, states map[string]wtState, removed *[]string) gitRunner {
	return func(_ context.Context, dir string, args ...string) (string, error) {
		a := strings.Join(args, " ")
		switch {
		case a == "rev-parse --path-format=absolute --git-common-dir":
			return repo + "/.git", nil
		case dir == repo && a == "worktree list --porcelain":
			return porcelain, nil
		case a == "symbolic-ref refs/remotes/origin/HEAD":
			return "refs/remotes/origin/main", nil
		case strings.HasPrefix(a, "status --porcelain"):
			return states[dir].status, nil
		case strings.HasPrefix(a, "for-each-ref"):
			return states[dir].remoteRefs, nil
		case a == "rev-parse --absolute-git-dir":
			return dir + "/.git", nil
		case strings.HasPrefix(a, "worktree remove"):
			if removed != nil {
				*removed = append(*removed, args[len(args)-1])
			}
			return "", nil
		}
		return "", nil
	}
}

func TestWorktreeCleanupSelf(t *testing.T) {
	repo := "/ws/services/foo"
	porcelain := strings.Join([]string{
		"worktree " + repo,
		"HEAD aaaa",
		"branch refs/heads/main",
		"",
		"worktree " + repo + "/.claude/worktrees/pushed",
		"HEAD bbbb",
		"branch refs/heads/claude/pushed",
		"",
		"worktree " + repo + "/.claude/worktrees/dirty",
		"HEAD cccc",
		"branch refs/heads/claude/dirty",
		"",
		"worktree " + repo + "/.claude/worktrees/at-risk",
		"HEAD dddd",
		"branch refs/heads/claude/at-risk",
		"",
	}, "\n")
	states := map[string]wtState{
		repo + "/.claude/worktrees/pushed":  {status: "", remoteRefs: "refs/remotes/origin/claude/pushed"},
		repo + "/.claude/worktrees/dirty":   {status: " M main.go", remoteRefs: "refs/remotes/origin/claude/dirty"},
		repo + "/.claude/worktrees/at-risk": {status: "", remoteRefs: ""},
	}

	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	newGC := func(removed *[]string) *wtGC {
		return &wtGC{
			cfg:       wtGCConfig{Policy: "merged-or-pushed", Now: now},
			git:       newCleanupFakeGit(repo, porcelain, states, removed),
			statMtime: func(string) (time.Time, error) { return now.Add(-time.Minute), nil },
		}
	}

	t.Run("clean and pushed tree is removed even when recently active", func(t *testing.T) {
		var removed []string
		ok, reason, err := worktreeCleanupSelf(context.Background(), newGC(&removed), repo+"/.claude/worktrees/pushed", true)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !ok {
			t.Fatalf("expected removal, kept: %s", reason)
		}
		if len(removed) != 1 || removed[0] != repo+"/.claude/worktrees/pushed" {
			t.Fatalf("expected git worktree remove of target, got %v", removed)
		}
	})

	t.Run("dirty tree is kept", func(t *testing.T) {
		var removed []string
		ok, reason, err := worktreeCleanupSelf(context.Background(), newGC(&removed), repo+"/.claude/worktrees/dirty", true)
		if err != nil || ok {
			t.Fatalf("expected keep, got removed=%v err=%v", ok, err)
		}
		if !strings.Contains(reason, "dirty") {
			t.Fatalf("expected dirty reason, got %q", reason)
		}
		if len(removed) != 0 {
			t.Fatalf("expected no removal, got %v", removed)
		}
	})

	t.Run("at-risk commits are kept", func(t *testing.T) {
		var removed []string
		ok, reason, err := worktreeCleanupSelf(context.Background(), newGC(&removed), repo+"/.claude/worktrees/at-risk", true)
		if err != nil || ok {
			t.Fatalf("expected keep, got removed=%v err=%v", ok, err)
		}
		if !strings.Contains(reason, "at-risk") {
			t.Fatalf("expected at-risk reason, got %q", reason)
		}
	})

	t.Run("dry-run reports removable without removing", func(t *testing.T) {
		var removed []string
		ok, reason, err := worktreeCleanupSelf(context.Background(), newGC(&removed), repo+"/.claude/worktrees/pushed", false)
		if err != nil || !ok {
			t.Fatalf("expected removable, got removed=%v err=%v reason=%s", ok, err, reason)
		}
		if len(removed) != 0 {
			t.Fatalf("dry-run must not remove, got %v", removed)
		}
	})

	t.Run("non-agent path is a no-op", func(t *testing.T) {
		ok, reason, err := worktreeCleanupSelf(context.Background(), newGC(nil), "/ws/services/foo-copy", true)
		if err != nil || ok {
			t.Fatalf("expected no-op, got removed=%v err=%v", ok, err)
		}
		if !strings.Contains(reason, "not an agent-convention") {
			t.Fatalf("unexpected reason %q", reason)
		}
	})

	t.Run("main worktree is refused", func(t *testing.T) {
		// A path that matches an agent convention but resolves to the main repo.
		g := newGC(nil)
		g.git = func(_ context.Context, dir string, args ...string) (string, error) {
			if strings.Join(args, " ") == "rev-parse --path-format=absolute --git-common-dir" {
				return dir + "/.git", nil // common dir inside the target => main tree
			}
			return "", nil
		}
		ok, reason, err := worktreeCleanupSelf(context.Background(), g, repo+"/.claude/worktrees/pushed", true)
		if err != nil || ok {
			t.Fatalf("expected refusal, got removed=%v err=%v", ok, err)
		}
		if !strings.Contains(reason, "main worktree") {
			t.Fatalf("unexpected reason %q", reason)
		}
	})
}

// TestSessionEndHookEmitsExistingSubcommand locks the generated hook command
// name to a subcommand that is actually registered, so the SessionEnd
// auto-cleanup can never silently rot again (it shipped inert for months
// because `worktree-cleanup-self` did not exist).
func TestSessionEndHookEmitsExistingSubcommand(t *testing.T) {
	agentCmd := newAgentCmd()
	var found bool
	for _, c := range agentCmd.Commands() {
		if c.Name() == "worktree-cleanup-self" {
			found = true
		}
	}
	if !found {
		t.Fatal("loom agent worktree-cleanup-self is not registered; generated SessionEnd hooks depend on it")
	}
}
