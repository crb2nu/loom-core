package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gitlab.flexinfer.ai/libs/mcp-go"
)

// textResult builds a single-text-content CallToolResult.
func textResult(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{{Type: "text", Text: s}}}
}

// trailerOf returns the appended trailer text (everything after the base
// content), or "" if nothing was appended.
func trailerOf(base string, result *mcp.CallToolResult) string {
	if len(result.Content) == 0 {
		return ""
	}
	full := result.Content[len(result.Content)-1].Text
	return strings.TrimPrefix(full, base)
}

func TestMRTrailerActionHintFormatting(t *testing.T) {
	tr := newMRTrailer()
	clock := time.Now()
	tr.nowFn = func() time.Time { return clock }
	tr.branchFn = func() string { return "feat/x" }

	cases := []struct {
		name  string
		state string
		want  string
	}{
		{"conflict", "conflict", "[loom] MR !42 (feat/x): conflict — rebase onto target and resolve conflicts"},
		{"flaky", "ci_failed_flaky", "[loom] MR !42 (feat/x): ci_failed_flaky — retry the pipeline (transient failure)"},
		{"deterministic", "ci_failed_deterministic", "[loom] MR !42 (feat/x): ci_failed_deterministic — fix the failing CI job"},
		{"unarmed", "automerge_unarmed", "[loom] MR !42 (feat/x): automerge_unarmed — arm auto-merge (MWPS)"},
		{"skipped", "pipeline_skipped", "[loom] MR !42 (feat/x): pipeline_skipped — create a pipeline for the head ref"},
		{"stale", "stale_branch", "[loom] MR !42 (feat/x): stale_branch — rebase; branch is behind target"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Bust caches between subtests by advancing the clock.
			clock = clock.Add(time.Minute)
			state := tc.state
			tr.fetchFn = func(b string) ([]mrTrailerMR, bool) {
				return []mrTrailerMR{{IID: 42, SourceBranch: b, State: state}}, true
			}
			res := textResult("base output")
			if !tr.maybeAppend(res, "git", "git_push") {
				t.Fatalf("expected trailer to be appended for state %q", tc.state)
			}
			got := trailerOf("base output", res)
			if got != "\n"+tc.want {
				t.Fatalf("trailer mismatch:\n got:  %q\n want: %q", got, "\n"+tc.want)
			}
		})
	}
}

func TestMRTrailerMultipleMRsSortedByIID(t *testing.T) {
	tr := newMRTrailer()
	clock := time.Now()
	tr.nowFn = func() time.Time { return clock }
	tr.branchFn = func() string { return "feat/x" }
	tr.fetchFn = func(b string) ([]mrTrailerMR, bool) {
		return []mrTrailerMR{
			{IID: 9, SourceBranch: b, State: "ok"},
			{IID: 3, SourceBranch: b, State: "conflict"},
		}, true
	}
	res := textResult("base")
	if !tr.maybeAppend(res, "git", "git_status") {
		t.Fatal("expected trailer (branch has an unhealthy MR)")
	}
	want := "\n[loom] MR !3 (feat/x): conflict — rebase onto target and resolve conflicts" +
		"\n[loom] MR !9 (feat/x): ok — healthy — auto-merge armed"
	if got := trailerOf("base", res); got != want {
		t.Fatalf("trailer mismatch:\n got:  %q\n want: %q", got, want)
	}
}

func TestMRTrailerDeltaGating(t *testing.T) {
	tr := newMRTrailer()
	clock := time.Now()
	tr.nowFn = func() time.Time { return clock }
	tr.branchFn = func() string { return "feat/x" }

	state := "conflict"
	tr.fetchFn = func(b string) ([]mrTrailerMR, bool) {
		return []mrTrailerMR{{IID: 7, SourceBranch: b, State: state}}, true
	}

	call := func() bool {
		clock = clock.Add(time.Minute) // bust branch + HUD caches each call
		res := textResult("out")
		return tr.maybeAppend(res, "git", "git_commit")
	}

	// Unhealthy state shows every time it is unhealthy.
	if !call() {
		t.Fatal("first unhealthy call should show")
	}
	if !call() {
		t.Fatal("repeated unhealthy call should still show (always shows until it changes)")
	}

	// Transition to ok: shows once.
	state = "ok"
	if !call() {
		t.Fatal("transition to ok should show once")
	}
	// ok unchanged: omitted.
	if call() {
		t.Fatal("unchanged ok state should be omitted")
	}
	if call() {
		t.Fatal("unchanged ok state should stay omitted")
	}

	// Back to unhealthy: shows again.
	state = "ci_failed_flaky"
	if !call() {
		t.Fatal("transition back to unhealthy should show")
	}
}

func TestMRTrailerCIRunningShownOnceOnTransition(t *testing.T) {
	tr := newMRTrailer()
	clock := time.Now()
	tr.nowFn = func() time.Time { return clock }
	tr.branchFn = func() string { return "feat/x" }

	state := "ci_running"
	tr.fetchFn = func(b string) ([]mrTrailerMR, bool) {
		return []mrTrailerMR{{IID: 1, SourceBranch: b, State: state}}, true
	}
	call := func() bool {
		clock = clock.Add(time.Minute)
		return tr.maybeAppend(textResult("x"), "gitlab", "get_merge_request")
	}
	if !call() {
		t.Fatal("first ci_running (a transition) should show once")
	}
	if call() {
		t.Fatal("unchanged ci_running should be omitted")
	}
}

func TestMRTrailerFailOpen(t *testing.T) {
	t.Run("hud_error", func(t *testing.T) {
		tr := newMRTrailer()
		tr.branchFn = func() string { return "feat/x" }
		tr.fetchFn = func(string) ([]mrTrailerMR, bool) { return nil, false }
		res := textResult("out")
		if tr.maybeAppend(res, "git", "git_push") {
			t.Fatal("HUD error must not append a trailer")
		}
		if trailerOf("out", res) != "" {
			t.Fatal("result must be unmodified on HUD error")
		}
	})

	t.Run("unknown_branch", func(t *testing.T) {
		tr := newMRTrailer()
		tr.branchFn = func() string { return "" } // detached / non-repo
		called := false
		tr.fetchFn = func(string) ([]mrTrailerMR, bool) { called = true; return nil, true }
		if tr.maybeAppend(textResult("out"), "git", "git_push") {
			t.Fatal("unknown branch must not append a trailer")
		}
		if called {
			t.Fatal("unknown branch must not even hit the HUD")
		}
	})

	t.Run("no_mrs_on_branch", func(t *testing.T) {
		tr := newMRTrailer()
		tr.branchFn = func() string { return "feat/x" }
		tr.fetchFn = func(string) ([]mrTrailerMR, bool) { return []mrTrailerMR{}, true }
		if tr.maybeAppend(textResult("out"), "git", "git_push") {
			t.Fatal("no MRs on branch must not append a trailer")
		}
	})
}

func TestMRTrailerOnlyTriggerTools(t *testing.T) {
	tr := newMRTrailer()
	tr.branchFn = func() string { return "feat/x" }
	tr.fetchFn = func(b string) ([]mrTrailerMR, bool) {
		return []mrTrailerMR{{IID: 5, SourceBranch: b, State: "conflict"}}, true
	}

	cases := []struct {
		server, tool string
		want         bool
	}{
		{"git", "git_push", true},
		{"git", "git_status", true},
		{"git", "git_commit", true},
		{"gitlab", "create_merge_request", true},
		{"gitlab", "get_merge_request", true},
		{"gitlab", "merge_merge_request", true},
		{"gitlab", "list_merge_requests", true},
		// Not triggers:
		{"git", "git_diff", false},
		{"git", "git_log", false},
		{"gitlab", "create_issue", false},
		{"gitlab", "get_pipeline", false},
		{"k8s_apps_k3s", "k8s_get", false},
		{"", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.server+"__"+tc.tool, func(t *testing.T) {
			// Each subtest gets a fresh trailer so delta-gating never masks a
			// genuine trigger; only tool identity is under test here.
			fresh := newMRTrailer()
			fresh.branchFn = tr.branchFn
			fresh.fetchFn = tr.fetchFn
			got := fresh.maybeAppend(textResult("out"), tc.server, tc.tool)
			if got != tc.want {
				t.Fatalf("tool %s__%s: got appended=%v want %v", tc.server, tc.tool, got, tc.want)
			}
		})
	}
}

// TestMRTrailerFetchFromFakeHUD exercises the real fetch path
// (fetchMRStatusFromHUD → hudGetFast) against a fake HUD HTTP server, proving
// the proxy parses the M1 BranchStatusResponse wire format end-to-end.
func TestMRTrailerFetchFromFakeHUD(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/agent/mr-status" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if got := r.URL.Query().Get("branch"); got != "feat/x" {
			t.Errorf("unexpected branch query: %q", got)
		}
		// Mirror internal/hud/domain/mrwatch.BranchStatusResponse.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"branch": "feat/x",
			"merge_requests": []map[string]any{
				{"iid": 123, "source_branch": "feat/x", "state": "automerge_unarmed", "web_url": "http://x/123"},
			},
			"count": 1,
		})
	}))
	defer srv.Close()

	t.Setenv("LOOM_HUD_URL", srv.URL)

	mrs, ok := fetchMRStatusFromHUD("feat/x")
	if !ok {
		t.Fatal("expected ok fetch from fake HUD")
	}
	if len(mrs) != 1 || mrs[0].IID != 123 || mrs[0].State != "automerge_unarmed" {
		t.Fatalf("unexpected decode: %+v", mrs)
	}

	// And through the full maybeAppend path (default fetch seam).
	tr := newMRTrailer()
	tr.branchFn = func() string { return "feat/x" }
	res := textResult("pushed")
	if !tr.maybeAppend(res, "git", "git_push") {
		t.Fatal("expected trailer via real fetch path")
	}
	want := "\n[loom] MR !123 (feat/x): automerge_unarmed — arm auto-merge (MWPS)"
	if got := trailerOf("pushed", res); got != want {
		t.Fatalf("trailer mismatch:\n got:  %q\n want: %q", got, want)
	}
}

func TestMRTrailerAppendToImageOnlyResult(t *testing.T) {
	tr := newMRTrailer()
	tr.branchFn = func() string { return "feat/x" }
	tr.fetchFn = func(b string) ([]mrTrailerMR, bool) {
		return []mrTrailerMR{{IID: 8, SourceBranch: b, State: "conflict"}}, true
	}
	res := &mcp.CallToolResult{Content: []mcp.Content{{Type: "image", MimeType: "image/png", Data: "abc"}}}
	if !tr.maybeAppend(res, "git", "git_status") {
		t.Fatal("expected trailer appended as a new text content item")
	}
	if len(res.Content) != 2 || res.Content[1].Type != "text" {
		t.Fatalf("expected a new text content item, got %+v", res.Content)
	}
	if !strings.HasPrefix(res.Content[1].Text, "[loom] MR !8") {
		t.Fatalf("unexpected new content text: %q", res.Content[1].Text)
	}
}

func TestReadGitBranch(t *testing.T) {
	t.Run("normal_repo", func(t *testing.T) {
		dir := t.TempDir()
		mustMkdir(t, filepath.Join(dir, ".git"))
		mustWrite(t, filepath.Join(dir, ".git", "HEAD"), "ref: refs/heads/feat/my-branch\n")
		if got := readGitBranch(dir); got != "feat/my-branch" {
			t.Fatalf("got %q want feat/my-branch", got)
		}
	})

	t.Run("nested_cwd", func(t *testing.T) {
		dir := t.TempDir()
		mustMkdir(t, filepath.Join(dir, ".git"))
		mustWrite(t, filepath.Join(dir, ".git", "HEAD"), "ref: refs/heads/main\n")
		nested := filepath.Join(dir, "a", "b")
		mustMkdir(t, nested)
		if got := readGitBranch(nested); got != "main" {
			t.Fatalf("got %q want main", got)
		}
	})

	t.Run("detached_head", func(t *testing.T) {
		dir := t.TempDir()
		mustMkdir(t, filepath.Join(dir, ".git"))
		mustWrite(t, filepath.Join(dir, ".git", "HEAD"), "0123456789abcdef0123456789abcdef01234567\n")
		if got := readGitBranch(dir); got != "" {
			t.Fatalf("detached HEAD should yield empty, got %q", got)
		}
	})

	t.Run("linked_worktree", func(t *testing.T) {
		root := t.TempDir()
		// Simulate a linked worktree: <root>/wt/.git is a file pointing at the
		// gitdir, which holds HEAD.
		gitdir := filepath.Join(root, "gitcommon", "worktrees", "wt")
		mustMkdir(t, gitdir)
		mustWrite(t, filepath.Join(gitdir, "HEAD"), "ref: refs/heads/feat/wt-branch\n")
		wt := filepath.Join(root, "wt")
		mustMkdir(t, wt)
		mustWrite(t, filepath.Join(wt, ".git"), "gitdir: "+gitdir+"\n")
		if got := readGitBranch(wt); got != "feat/wt-branch" {
			t.Fatalf("got %q want feat/wt-branch", got)
		}
	})

	t.Run("no_repo", func(t *testing.T) {
		dir := t.TempDir()
		if got := readGitBranch(dir); got != "" {
			t.Fatalf("non-repo dir should yield empty, got %q", got)
		}
	})
}

func mustMkdir(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, p, content string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
