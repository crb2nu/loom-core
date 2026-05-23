package generator

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAntigravityHooksConfigShapeAndAutoAllow(t *testing.T) {
	profile, err := GetPlatformProfile("antigravity")
	if err != nil {
		t.Fatalf("GetPlatformProfile: %v", err)
	}

	cfg := antigravityHooksConfig(testRegistry(), profile, "/opt/loom/bin/loom")
	if _, found := cfg["hooks"]; found {
		t.Fatal("antigravity hooks.json must not use Claude/Codex top-level hooks key")
	}
	loom, ok := cfg["loom"].(map[string]any)
	if !ok {
		t.Fatalf("missing top-level loom hook namespace: %#v", cfg)
	}
	for _, event := range []string{"PreInvocation", "PreToolUse", "PostToolUse", "Stop"} {
		if _, found := loom[event]; !found {
			t.Errorf("expected Antigravity hooks to contain %q", event)
		}
	}

	preToolUse := mustHookBlocks(t, loom, "PreToolUse")
	autoAllow := findHookCommand(preToolUse, "mcp(loom/*)")
	if autoAllow == "" {
		t.Fatalf("expected ask_permission hook to autoallow mcp(loom/*) namespace: %#v", preToolUse)
	}
	if !strings.Contains(autoAllow, `"permissionOverrides":["mcp(loom/*)"]`) {
		t.Fatalf("autoallow hook missing permissionOverrides for loom namespace: %s", autoAllow)
	}
	if !strings.Contains(autoAllow, `mcp\(loom/*\)`) {
		t.Fatalf("autoallow hook must match concrete loom tools such as mcp(loom/git_status): %s", autoAllow)
	}
	if !strings.Contains(autoAllow, `"decision":"allow"`) {
		t.Fatalf("autoallow hook must emit Antigravity decision JSON: %s", autoAllow)
	}

	policy := findHookCommand(preToolUse, "GitOps policy")
	if policy == "" {
		t.Fatalf("expected native run_command policy guardrail in PreToolUse: %#v", preToolUse)
	}
	if !strings.Contains(policy, "CommandLine") {
		t.Fatalf("policy hook should read Antigravity CommandLine payloads: %s", policy)
	}

	for _, event := range []string{"PreInvocation", "PreToolUse", "PostToolUse", "Stop"} {
		for _, cmd := range allHookCommands(mustHookBlocks(t, loom, event)) {
			if strings.Contains(cmd, "systemMessage") {
				t.Fatalf("Antigravity hook command under %s emitted Claude systemMessage JSON: %s", event, cmd)
			}
		}
	}
}

func TestGenerateHooksConfig_AntigravityWritesHooksJSON(t *testing.T) {
	tmpDir := t.TempDir()
	profile, err := GetPlatformProfile("antigravity")
	if err != nil {
		t.Fatalf("GetPlatformProfile: %v", err)
	}

	if err := generateHooksConfig(testRegistry(), tmpDir, "antigravity", profile, "/opt/loom/bin/loom"); err != nil {
		t.Fatalf("generateHooksConfig(antigravity): %v", err)
	}
	content, err := os.ReadFile(filepath.Join(tmpDir, "antigravity", "hooks.json"))
	if err != nil {
		t.Fatalf("hooks.json not found: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(content, &parsed); err != nil {
		t.Fatalf("generated hooks.json is not valid JSON: %v", err)
	}
	if _, found := parsed["loom"]; !found {
		t.Fatalf("generated hooks.json missing top-level loom key: %s", content)
	}
	if strings.Contains(string(content), `"hooks": {`) {
		t.Fatalf("generated hooks.json unexpectedly uses Claude/Codex hooks wrapper: %s", content)
	}
	if !strings.Contains(string(content), `'/opt/loom/bin/loom' agent event-emit --hook pre-tool-use --platform antigravity`) {
		t.Fatalf("expected explicit loom binary and Antigravity event-emit wiring, got: %s", content)
	}
}

func mustHookBlocks(t *testing.T, hooks map[string]any, event string) []map[string]any {
	t.Helper()
	blocks, ok := hooks[event].([]map[string]any)
	if !ok || len(blocks) == 0 {
		t.Fatalf("event %q blocks = %#v", event, hooks[event])
	}
	return blocks
}

func findHookCommand(blocks []map[string]any, needle string) string {
	for _, cmd := range allHookCommands(blocks) {
		if strings.Contains(cmd, needle) {
			return cmd
		}
	}
	return ""
}

func allHookCommands(blocks []map[string]any) []string {
	var out []string
	for _, block := range blocks {
		hooks, _ := block["hooks"].([]map[string]any)
		for _, hook := range hooks {
			if cmd, ok := hook["command"].(string); ok {
				out = append(out, cmd)
			}
		}
	}
	return out
}
