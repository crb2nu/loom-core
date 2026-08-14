package projectmeta

import "testing"

func TestFromNamespace(t *testing.T) {
	tests := []struct {
		namespace string
		want      string
	}{
		{namespace: "loom-core/feat/orchestration", want: "loom-core"},
		{namespace: "services/loom-core", want: "services/loom-core"},
		{namespace: "services/loom-core/feat/orchestration", want: "services/loom-core"},
		{namespace: "platform/gitops/flux", want: "platform/gitops"},
		// labs/ and private/ are workspace roots (AGENTS.md) — must resolve to
		// the 2-level project, not the bare root segment.
		{namespace: "labs/fractal-agents/feat/x", want: "labs/fractal-agents"},
		{namespace: "private/secrets-tool/main", want: "private/secrets-tool"},
		{namespace: "loom-core", want: "loom-core"},
		{namespace: "", want: ""},
		{namespace: "/broken", want: ""},
		// Degenerate "////main" (codex detached-keepalive shape) → no project.
		{namespace: "////main", want: ""},
	}

	for _, tc := range tests {
		if got := FromNamespace(tc.namespace); got != tc.want {
			t.Fatalf("FromNamespace(%q) = %q, want %q", tc.namespace, got, tc.want)
		}
	}
}

func TestCanonical(t *testing.T) {
	if got := Canonical("services/loom-core", "loom-core/feat/orchestration"); got != "services/loom-core" {
		t.Fatalf("Canonical(explicit) = %q, want services/loom-core", got)
	}
	if got := Canonical("", "loom-core/feat/orchestration"); got != "loom-core" {
		t.Fatalf("Canonical(namespace) = %q, want loom-core", got)
	}
}

func TestLooksLikeBareRepo(t *testing.T) {
	tests := []struct {
		project string
		want    bool
	}{
		{project: "loom-core", want: true},           // bare repo name — the bug
		{project: "services/loom-core", want: false}, // workspace-bucketed
		{project: "group/loom-core", want: false},    // GitLab path_with_namespace
		{project: "platform/gitops", want: false},
		{project: "  loom-core  ", want: true}, // trimmed, still bare
		{project: "", want: false},             // empty is not "bare" — nothing to warn about
		{project: "   ", want: false},
	}

	for _, tc := range tests {
		if got := LooksLikeBareRepo(tc.project); got != tc.want {
			t.Fatalf("LooksLikeBareRepo(%q) = %v, want %v", tc.project, got, tc.want)
		}
	}
}

func TestFromPath(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{path: "/Users/cblevins/workspace/services/loom-core", want: "services/loom-core"},
		{path: "/Users/cblevins/workspace/services/loom-core/.worktrees/mobile-ui", want: "services/loom-core"},
		{path: "platform/gitops/clusters/k3s", want: "platform/gitops"},
		{path: `C:\workspace\apps\loom\Sources\App.swift`, want: "apps/loom"},
		{path: "", want: ""},
		{path: "/tmp/not-a-workspace/path", want: ""},
	}

	for _, tc := range tests {
		if got := FromPath(tc.path); got != tc.want {
			t.Fatalf("FromPath(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}
