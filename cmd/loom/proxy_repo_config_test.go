package main

import (
	"os"
	"path/filepath"
	"testing"
)

func writeRepoProxyConfig(t *testing.T, dir, content string) {
	t.Helper()
	loomDir := filepath.Join(dir, ".loom")
	if err := os.MkdirAll(loomDir, 0o755); err != nil {
		t.Fatalf("mkdir .loom: %v", err)
	}
	if err := os.WriteFile(filepath.Join(loomDir, "proxy.yaml"), []byte(content), 0o644); err != nil {
		t.Fatalf("write proxy.yaml: %v", err)
	}
}

func TestApplyProxyProfileOverridesNoFile(t *testing.T) {
	t.Chdir(t.TempDir())

	profile, cap := applyProxyProfileOverrides("antigravity", "antigravity-core", 100)
	if profile != "antigravity-core" || cap != 100 {
		t.Errorf("got (%q, %d), want CLI passthrough (antigravity-core, 100)", profile, cap)
	}
}

func TestApplyProxyProfileOverridesAgentScoped(t *testing.T) {
	dir := t.TempDir()
	writeRepoProxyConfig(t, dir, "agents:\n  antigravity:\n    tool_profile: icc-core\n")
	t.Chdir(dir)

	// Matching hint flips to icc-core with the cap reset (0 = profile default).
	profile, cap := applyProxyProfileOverrides("antigravity", "antigravity-core", 100)
	if profile != "icc-core" || cap != 0 {
		t.Errorf("antigravity: got (%q, %d), want (icc-core, 0)", profile, cap)
	}

	// Non-matching hint keeps its CLI profile untouched.
	profile, cap = applyProxyProfileOverrides("claude", "llm-core", 0)
	if profile != "llm-core" || cap != 0 {
		t.Errorf("claude: got (%q, %d), want (llm-core, 0)", profile, cap)
	}
}

func TestApplyProxyProfileOverridesTopLevel(t *testing.T) {
	dir := t.TempDir()
	writeRepoProxyConfig(t, dir, "tool_profile: icc-core\nmax_tools: 90\n")
	t.Chdir(dir)

	// Top-level override applies regardless of agent hint.
	profile, cap := applyProxyProfileOverrides("claude", "llm-core", 160)
	if profile != "icc-core" || cap != 90 {
		t.Errorf("got (%q, %d), want (icc-core, 90)", profile, cap)
	}
}

func TestApplyProxyProfileOverridesAgentBeatsTopLevel(t *testing.T) {
	dir := t.TempDir()
	writeRepoProxyConfig(t, dir, "tool_profile: llm-core\nagents:\n  antigravity:\n    tool_profile: icc-core\n")
	t.Chdir(dir)

	profile, _ := applyProxyProfileOverrides("antigravity", "antigravity-core", 100)
	if profile != "icc-core" {
		t.Errorf("got %q, want agent-scoped icc-core over top-level llm-core", profile)
	}
}

func TestApplyProxyProfileOverridesUnknownProfileIgnored(t *testing.T) {
	dir := t.TempDir()
	writeRepoProxyConfig(t, dir, "tool_profile: not-a-profile\n")
	t.Chdir(dir)

	profile, cap := applyProxyProfileOverrides("antigravity", "antigravity-core", 100)
	if profile != "antigravity-core" || cap != 100 {
		t.Errorf("got (%q, %d), want unknown profile ignored (antigravity-core, 100)", profile, cap)
	}
}

func TestApplyProxyProfileOverridesMalformedIgnored(t *testing.T) {
	dir := t.TempDir()
	writeRepoProxyConfig(t, dir, "tool_profile: [unclosed\n")
	t.Chdir(dir)

	profile, cap := applyProxyProfileOverrides("antigravity", "antigravity-core", 100)
	if profile != "antigravity-core" || cap != 100 {
		t.Errorf("got (%q, %d), want malformed file ignored (antigravity-core, 100)", profile, cap)
	}
}

func TestApplyProxyProfileOverridesWalksUp(t *testing.T) {
	dir := t.TempDir()
	writeRepoProxyConfig(t, dir, "agents:\n  antigravity:\n    tool_profile: icc-core\n")
	sub := filepath.Join(dir, "projects", "some-project")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}
	t.Chdir(sub)

	profile, _ := applyProxyProfileOverrides("antigravity", "antigravity-core", 100)
	if profile != "icc-core" {
		t.Errorf("got %q, want icc-core found via upward walk", profile)
	}
}

func TestApplyProxyProfileOverridesEnvWins(t *testing.T) {
	dir := t.TempDir()
	writeRepoProxyConfig(t, dir, "tool_profile: llm-core\n")
	t.Chdir(dir)
	t.Setenv(proxyToolProfileEnv, "icc-core")

	profile, cap := applyProxyProfileOverrides("antigravity", "antigravity-core", 100)
	if profile != "icc-core" || cap != 0 {
		t.Errorf("got (%q, %d), want env override (icc-core, 0)", profile, cap)
	}
}

func TestApplyProxyProfileOverridesEnvUnknownFallsThrough(t *testing.T) {
	dir := t.TempDir()
	writeRepoProxyConfig(t, dir, "tool_profile: icc-core\n")
	t.Chdir(dir)
	t.Setenv(proxyToolProfileEnv, "bogus")

	profile, _ := applyProxyProfileOverrides("antigravity", "antigravity-core", 100)
	if profile != "icc-core" {
		t.Errorf("got %q, want unknown env ignored and file override applied (icc-core)", profile)
	}
}

func TestApplyProxyProfileOverridesEmptyFileNoop(t *testing.T) {
	dir := t.TempDir()
	writeRepoProxyConfig(t, dir, "# no overrides here\nagents: {}\n")
	t.Chdir(dir)

	profile, cap := applyProxyProfileOverrides("antigravity", "antigravity-core", 100)
	if profile != "antigravity-core" || cap != 100 {
		t.Errorf("got (%q, %d), want empty config passthrough (antigravity-core, 100)", profile, cap)
	}
}
