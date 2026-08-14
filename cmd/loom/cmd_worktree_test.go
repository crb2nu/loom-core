package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestAgentForPath(t *testing.T) {
	cases := map[string]string{
		"/ws/services/foo/.claude/worktrees/x":    "claude",
		"/Users/u/.codex/worktrees/1c38/flexdeck": "codex",
		"/ws/services/foo/.worktrees/feat-x":      "manual",
		"/ws/services/foo/.gemini/worktrees/y":    "gemini",
		"/ws/services/foo/.opencode/worktrees/z":  "opencode",
		"/some/random/path":                       "other",
	}
	for path, want := range cases {
		if got := agentForPath(path); got != want {
			t.Errorf("agentForPath(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestWorktreeDecide(t *testing.T) {
	now := time.Date(2026, 6, 27, 12, 0, 0, 0, time.UTC)
	old := now.Add(-2 * time.Hour)
	recent := now.Add(-5 * time.Minute)

	mk := func(policy string) *wtGC {
		return &wtGC{cfg: wtGCConfig{Policy: policy, MinIdle: 30 * time.Minute, Now: now}}
	}

	tests := []struct {
		name       string
		policy     string
		w          worktreeRec
		wantRemove bool
		reasonHas  string
	}{
		{"merged is removed", "merged-or-pushed",
			worktreeRec{Merged: true, OnRemote: true, idleSince: old}, true, "merged"},
		{"clean+pushed removed under merged-or-pushed", "merged-or-pushed",
			worktreeRec{OnRemote: true, Merged: false, idleSince: old}, true, "fully pushed"},
		{"clean+pushed KEPT under merged-only", "merged",
			worktreeRec{OnRemote: true, Merged: false, idleSince: old}, false, "unmerged"},
		{"dirty always kept (even if merged)", "merged-or-pushed",
			worktreeRec{Dirty: true, Merged: true, OnRemote: true, idleSince: old}, false, "dirty"},
		{"at-risk kept (not on remote, not merged)", "merged-or-pushed",
			worktreeRec{OnRemote: false, Merged: false, idleSince: old}, false, "at-risk"},
		{"current worktree kept (even if merged)", "merged-or-pushed",
			worktreeRec{Current: true, Merged: true, OnRemote: true, idleSince: old}, false, "current"},
		{"recently-active kept (even if merged)", "merged-or-pushed",
			worktreeRec{Merged: true, OnRemote: true, idleSince: recent}, false, "active"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := mk(tt.policy)
			w := tt.w
			g.decide(&w)
			if w.Remove != tt.wantRemove {
				t.Fatalf("Remove = %v, want %v (reason %q)", w.Remove, tt.wantRemove, w.Reason)
			}
			if !strings.Contains(w.Reason, tt.reasonHas) {
				t.Fatalf("Reason = %q, want substring %q", w.Reason, tt.reasonHas)
			}
		})
	}
}

// wtState is the canned git responses for one worktree path.
type wtState struct {
	status     string // `status --porcelain`
	remoteRefs string // `for-each-ref ... --contains HEAD refs/remotes/`
}

func TestWorktreePlanClassifies(t *testing.T) {
	repo := "/ws/services/foo"
	porcelain := strings.Join([]string{
		"worktree /ws/services/foo",
		"HEAD aaaa",
		"branch refs/heads/main",
		"",
		"worktree /ws/services/foo/.claude/worktrees/merged-one",
		"HEAD bbbb",
		"branch refs/heads/claude/merged-one",
		"",
		"worktree /Users/u/.codex/worktrees/1c38/foo",
		"HEAD cccc",
		"branch refs/heads/codex/dirty-one",
		"",
		"worktree /ws/services/foo/.worktrees/at-risk",
		"HEAD dddd",
		"branch refs/heads/feat/at-risk",
		"",
		"worktree /ws/services/foo/.claude/worktrees/pushed-unmerged",
		"HEAD eeee",
		"branch refs/heads/claude/pushed-unmerged",
		"",
	}, "\n")

	states := map[string]wtState{
		"/ws/services/foo/.claude/worktrees/merged-one": {
			status: "", remoteRefs: "refs/remotes/origin/main\nrefs/remotes/origin/claude/merged-one",
		},
		"/Users/u/.codex/worktrees/1c38/foo": {
			status: " M main.go", remoteRefs: "",
		},
		"/ws/services/foo/.worktrees/at-risk": {
			status: "", remoteRefs: "", // not on any remote, not merged
		},
		"/ws/services/foo/.claude/worktrees/pushed-unmerged": {
			status: "", remoteRefs: "refs/remotes/origin/claude/pushed-unmerged", // pushed, not merged
		},
	}

	now := time.Date(2026, 6, 27, 12, 0, 0, 0, time.UTC)
	git := func(_ context.Context, dir string, args ...string) (string, error) {
		a := strings.Join(args, " ")
		switch {
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
		}
		return "", nil
	}
	g := &wtGC{
		cfg: wtGCConfig{
			Repo: repo, Policy: "merged-or-pushed", MinIdle: 30 * time.Minute, Now: now,
		},
		git:       git,
		statMtime: func(string) (time.Time, error) { return now.Add(-2 * time.Hour), nil },
	}

	recs, err := g.plan(context.Background())
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(recs) != 4 {
		t.Fatalf("got %d linked worktrees, want 4 (main excluded): %+v", len(recs), recs)
	}

	byPath := map[string]worktreeRec{}
	for _, r := range recs {
		byPath[r.Path] = r
	}

	check := func(path, wantAgent string, wantRemove bool, reasonHas string) {
		t.Helper()
		r, ok := byPath[path]
		if !ok {
			t.Fatalf("missing worktree %s", path)
		}
		if r.Agent != wantAgent {
			t.Errorf("%s agent = %q, want %q", path, r.Agent, wantAgent)
		}
		if r.Remove != wantRemove {
			t.Errorf("%s Remove = %v, want %v (reason %q)", path, r.Remove, wantRemove, r.Reason)
		}
		if !strings.Contains(r.Reason, reasonHas) {
			t.Errorf("%s Reason = %q, want substring %q", path, r.Reason, reasonHas)
		}
	}

	check("/ws/services/foo/.claude/worktrees/merged-one", "claude", true, "merged")
	check("/Users/u/.codex/worktrees/1c38/foo", "codex", false, "dirty")
	check("/ws/services/foo/.worktrees/at-risk", "manual", false, "at-risk")
	check("/ws/services/foo/.claude/worktrees/pushed-unmerged", "claude", true, "fully pushed")
}
