package main

import (
	"strings"
	"testing"
)

func TestFormatBytes(t *testing.T) {
	tests := []struct {
		input    int64
		expected string
	}{
		{0, "0 B"},
		{100, "100 B"},
		{1023, "1023 B"},
		{1024, "1.0 KB"},
		{1536, "1.5 KB"},
		{1048576, "1.0 MB"},
		{1073741824, "1.0 GB"},
	}
	for _, tc := range tests {
		got := formatBytes(tc.input)
		if got != tc.expected {
			t.Errorf("formatBytes(%d) = %q, want %q", tc.input, got, tc.expected)
		}
	}
}

func TestSplitToolName(t *testing.T) {
	tests := []struct {
		input    string
		expected []string
	}{
		{"server__tool", []string{"server", "tool"}},
		{"simple", []string{"simple"}},
		{"a__b__c", []string{"a", "b__c"}},
		{"__leading", []string{"", "leading"}},
		{"trailing__", []string{"trailing", ""}},
	}
	for _, tc := range tests {
		got := splitToolName(tc.input)
		if len(got) != len(tc.expected) {
			t.Fatalf("splitToolName(%q) = %v, want %v", tc.input, got, tc.expected)
		}
		for i := range got {
			if got[i] != tc.expected[i] {
				t.Errorf("splitToolName(%q)[%d] = %q, want %q", tc.input, i, got[i], tc.expected[i])
			}
		}
	}
}

func TestStripProxyToolNamespace(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"agent_context__agent_session_start", "agent_context__agent_session_start"},
		{"loom/agent_context__agent_session_start", "agent_context__agent_session_start"},
		{" loom/agent_context__agent_presence_register ", "agent_context__agent_presence_register"},
		{"loom/", "loom/"},
		{"", ""},
	}

	for _, tc := range tests {
		if got := stripProxyToolNamespace(tc.input); got != tc.want {
			t.Errorf("stripProxyToolNamespace(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestFormatCheck(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"ok", "ok"},
		{"stale", "STALE"},
		{"drift", "DRIFT"},
		{"errors", "ERRORS"},
		{"missing", "missing"},
		{"n/a", "n/a"},
		{"unknown", "unknown"},
	}
	for _, tc := range tests {
		got := formatCheck(tc.input)
		if got != tc.expected {
			t.Errorf("formatCheck(%q) = %q, want %q", tc.input, got, tc.expected)
		}
	}
}

func TestNormalizeJSON(t *testing.T) {
	tests := []struct {
		name  string
		input string
		check func(string) bool
	}{
		{"valid json normalizes", `{"key":"val"}`, func(s string) bool { return s == `{"key":"val"}` }},
		{"invalid json passthrough", "not json", func(s string) bool { return s == "not json" }},
		{"empty object", "{}", func(s string) bool { return s == "{}" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeJSON([]byte(tc.input))
			if !tc.check(got) {
				t.Errorf("normalizeJSON(%q) = %q", tc.input, got)
			}
		})
	}
}

func TestKeepalivePIDPath(t *testing.T) {
	got := keepalivePIDPath("test-agent")
	if !strings.Contains(got, "loom-keepalive-test-agent.pid") {
		t.Errorf("keepalivePIDPath('test-agent') = %q, expected to contain PID filename", got)
	}
}

func TestInferGitNamespace(t *testing.T) {
	// This calls git, so just verify it returns something sensible in a git repo
	got := inferGitNamespace()
	if got == "" {
		t.Skip("not in a git repo")
	}
	// Should be "parent/repo/branch" (workspace-relative), e.g. "services/loom-core/main"
	parts := strings.Split(got, "/")
	if len(parts) < 2 {
		t.Errorf("inferGitNamespace() = %q, want at least parent/repo", got)
	}
	t.Logf("inferGitNamespace() = %q", got)
	// A real inferred namespace must never contain empty path segments — that
	// was the "////main" bug. Each "/"-delimited part should be non-empty.
	for _, p := range strings.Split(got, "/") {
		if p == "" {
			t.Errorf("inferGitNamespace() = %q has an empty path segment", got)
		}
	}
}

func TestProjectFromRemoteURL(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"https with .git", "https://gitlab.flexinfer.ai/services/xfiles.git", "services/xfiles"},
		{"https without .git", "https://gitlab.flexinfer.ai/services/news-analyzer", "services/news-analyzer"},
		{"https with port", "https://gitlab.flexinfer.ai:8443/services/loom-core.git", "services/loom-core"},
		{"scp-like", "git@gitlab.flexinfer.ai:services/xfiles.git", "services/xfiles"},
		{"ssh url", "ssh://git@gitlab.flexinfer.ai/services/loom-core.git", "services/loom-core"},
		{"ssh url with port", "ssh://git@gitlab.flexinfer.ai:2222/services/loom-core.git", "services/loom-core"},
		{"nested group keeps last two", "https://gitlab.flexinfer.ai/group/subgroup/repo.git", "subgroup/repo"},
		{"host without user scp", "gitlab.flexinfer.ai:services/loom-core.git", "services/loom-core"},
		{"trailing slash", "https://gitlab.flexinfer.ai/services/loom-core/", "services/loom-core"},
		{"single segment", "https://gitlab.flexinfer.ai/loom-core.git", "loom-core"},
		{"empty", "", ""},
		{"pathless url", "https://gitlab.flexinfer.ai", ""},
		{"whitespace", "   ", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := projectFromRemoteURL(tc.in); got != tc.want {
				t.Errorf("projectFromRemoteURL(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestIsMalformedNamespace(t *testing.T) {
	malformed := []string{"////main", "///", "a//b", "/services/loom-core", "services/loom-core/", "services//main"}
	for _, ns := range malformed {
		if !isMalformedNamespace(ns) {
			t.Errorf("isMalformedNamespace(%q) = false, want true", ns)
		}
	}
	// Empty/whitespace is "absent", not malformed; well-formed namespaces pass.
	wellFormed := []string{"", "   ", "services/loom-core", "services/loom-core/main", "3ef2/conspiracy-files", "services/flexinfer/feat/rbac"}
	for _, ns := range wellFormed {
		if isMalformedNamespace(ns) {
			t.Errorf("isMalformedNamespace(%q) = true, want false", ns)
		}
	}
}

func TestIsDegeneratePathSegment(t *testing.T) {
	for _, s := range []string{"", "/", ".", ".."} {
		if !isDegeneratePathSegment(s) {
			t.Errorf("isDegeneratePathSegment(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"loom-core", "services", "main", "feat-x"} {
		if isDegeneratePathSegment(s) {
			t.Errorf("isDegeneratePathSegment(%q) = true, want false", s)
		}
	}
}

func TestStripWorktreeFromRepoRoot(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "main checkout passthrough",
			in:   "/Users/me/workspace/services/loom-core",
			want: "/Users/me/workspace/services/loom-core",
		},
		{
			name: "workspace-standard worktree collapses",
			in:   "/Users/me/workspace/services/loom-core/.worktrees/feat-xyz",
			want: "/Users/me/workspace/services/loom-core",
		},
		{
			name: "claude-managed worktree collapses",
			in:   "/Users/me/workspace/services/loom-core/.claude/worktrees/competent-allen-d7252d",
			want: "/Users/me/workspace/services/loom-core",
		},
		{
			name: "claude-managed wins when both patterns appear",
			in:   "/repo/.claude/worktrees/wt1/.worktrees/inner",
			want: "/repo",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stripWorktreeFromRepoRoot(tt.in)
			if got != tt.want {
				t.Errorf("stripWorktreeFromRepoRoot(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
