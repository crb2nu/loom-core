package generator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crb2nu/loom/pkg/registry"
)

// codexRulesRegistry builds a minimal registry whose codex settings carry the
// execpolicy rules list, shaped exactly as YAML decoding shapes it
// ([]any of map[string]any, nested lists as []any).
func codexRulesRegistry() *registry.Registry {
	return &registry.Registry{
		PlatformPermissions: map[string]*registry.PlatformPermission{
			"codex": {
				Settings: map[string]any{
					"rules": []any{
						map[string]any{
							"pattern":       []any{"git"},
							"decision":      "allow",
							"justification": "sandbox blocks .git writes",
							"match": []any{
								[]any{"git", "status"},
								[]any{"git", "push", "origin", "main"},
							},
						},
						map[string]any{
							"pattern": []any{"gh", []any{"view", "list"}},
							// no decision → vendor default "allow"
							"not_match": []any{
								[]any{"gh", "pr", "merge"},
							},
						},
					},
				},
			},
		},
	}
}

func TestBuildCodexExecRules(t *testing.T) {
	t.Run("nil and empty", func(t *testing.T) {
		if got := buildCodexExecRules(nil); got != nil {
			t.Errorf("nil pp: got %v, want nil", got)
		}
		pp := &registry.PlatformPermission{Settings: map[string]any{"model": "m"}}
		if got := buildCodexExecRules(pp); got != nil {
			t.Errorf("no rules key: got %v, want nil", got)
		}
	})

	t.Run("resolves patterns, decisions, matrices", func(t *testing.T) {
		pp := registryPlatformPerms(codexRulesRegistry(), "codex")
		rules := buildCodexExecRules(pp)
		if len(rules) != 2 {
			t.Fatalf("got %d rules, want 2", len(rules))
		}
		if rules[0].Decision != "allow" || rules[0].Justification == "" {
			t.Errorf("rule[0] = %+v, want explicit allow with justification", rules[0])
		}
		if len(rules[0].Match) != 2 || rules[0].Match[1][2] != "origin" {
			t.Errorf("rule[0].Match = %v, want two example rows", rules[0].Match)
		}
		if rules[1].Decision != "allow" {
			t.Errorf("rule[1].Decision = %q, want vendor default allow", rules[1].Decision)
		}
		union, ok := rules[1].Pattern[1].([]string)
		if !ok || len(union) != 2 || union[0] != "view" {
			t.Errorf("rule[1].Pattern[1] = %#v, want union [view list]", rules[1].Pattern[1])
		}
		if len(rules[1].NotMatch) != 1 {
			t.Errorf("rule[1].NotMatch = %v, want one row", rules[1].NotMatch)
		}
	})

	t.Run("invalid entries dropped", func(t *testing.T) {
		pp := &registry.PlatformPermission{Settings: map[string]any{
			"rules": []any{
				"not-a-map",
				map[string]any{"decision": "allow"},       // no pattern
				map[string]any{"pattern": []any{}},        // empty pattern
				map[string]any{"pattern": []any{"rsync"}}, // valid
			},
		}}
		rules := buildCodexExecRules(pp)
		if len(rules) != 1 || rules[0].Pattern[0] != "rsync" {
			t.Errorf("got %+v, want single rsync rule", rules)
		}
	})
}

// TestRenderCodexRulesBlock pins the emitted Starlark shape: markers wrap the
// block, doc/limitation URLs are cited, and prefix_rule stanzas carry
// pattern/decision/justification/match in vendor syntax.
func TestRenderCodexRulesBlock(t *testing.T) {
	rules := buildCodexExecRules(registryPlatformPerms(codexRulesRegistry(), "codex"))
	out := renderCodexRulesBlock(rules)

	if !strings.HasPrefix(out, CodexRulesBeginMarker+"\n") {
		t.Errorf("block must start with begin marker:\n%s", out)
	}
	if !strings.HasSuffix(out, CodexRulesEndMarker+"\n") {
		t.Errorf("block must end with end marker:\n%s", out)
	}
	for _, want := range []string{
		"https://learn.chatgpt.com/docs/agent-configuration/rules.md",
		"https://github.com/openai/codex/issues/15505",
		"codex execpolicy check",
		`pattern = ["git"],`,
		`decision = "allow",`,
		`justification = "sandbox blocks .git writes",`,
		`["git", "push", "origin", "main"],`,
		`pattern = ["gh", ["view", "list"]],`,
		"not_match = [",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered block missing %q:\n%s", want, out)
		}
	}
	// The markers are single lines — a marker string containing a newline
	// would break pkg/sync's line-extension logic in MergeMarkerBlock.
	for _, marker := range []string{CodexRulesBeginMarker, CodexRulesEndMarker} {
		if strings.Contains(marker, "\n") || !strings.HasPrefix(marker, "#") {
			t.Errorf("marker must be a single comment line: %q", marker)
		}
	}
}

// TestGenerateCodexRulesConfig covers the file-shape contract: codex target
// with rules lands rules/default.rules; non-codex targets and rule-less
// registries are no-ops.
func TestGenerateCodexRulesConfig(t *testing.T) {
	codexProfileDef, err := GetPlatformProfile("codex")
	if err != nil {
		t.Fatalf("GetPlatformProfile(codex): %v", err)
	}

	t.Run("codex with rules writes file", func(t *testing.T) {
		outDir := t.TempDir()
		if err := generateCodexRulesConfig(codexRulesRegistry(), outDir, "codex", codexProfileDef); err != nil {
			t.Fatalf("generateCodexRulesConfig: %v", err)
		}
		data, err := os.ReadFile(filepath.Join(outDir, "codex", "rules", "default.rules"))
		if err != nil {
			t.Fatalf("read rules/default.rules: %v", err)
		}
		if !strings.Contains(string(data), CodexRulesBeginMarker) {
			t.Errorf("generated rules missing begin marker:\n%s", data)
		}
	})

	t.Run("non-codex target is a no-op", func(t *testing.T) {
		claudeProfile, err := GetPlatformProfile("claude")
		if err != nil {
			t.Fatalf("GetPlatformProfile(claude): %v", err)
		}
		outDir := t.TempDir()
		if err := generateCodexRulesConfig(codexRulesRegistry(), outDir, "claude", claudeProfile); err != nil {
			t.Fatalf("generateCodexRulesConfig(claude): %v", err)
		}
		if entries, _ := os.ReadDir(outDir); len(entries) != 0 {
			t.Errorf("non-codex target wrote files: %v", entries)
		}
	})

	t.Run("no rules is a no-op", func(t *testing.T) {
		reg := &registry.Registry{PlatformPermissions: map[string]*registry.PlatformPermission{
			"codex": {Settings: map[string]any{"model": "m"}},
		}}
		outDir := t.TempDir()
		if err := generateCodexRulesConfig(reg, outDir, "codex", codexProfileDef); err != nil {
			t.Fatalf("generateCodexRulesConfig: %v", err)
		}
		if _, err := os.Stat(filepath.Join(outDir, "codex", "rules", "default.rules")); err == nil {
			t.Error("expected no rules file for rule-less registry")
		}
	})
}

// TestGenerateConfigs_RealRegistryCodexRules is the end-to-end golden guard:
// the SHIPPED mcp/context/registry.yaml must emit the git prefix_rule that
// unwedges commit/push under the workspace-write sandbox (the 2026-07-25
// manual fix this generator now owns). If a registry edit drops the rules
// section, this fails loudly.
func TestGenerateConfigs_RealRegistryCodexRules(t *testing.T) {
	const registryPath = "../../mcp/context/registry.yaml"
	reg, err := registry.Load(registryPath)
	if err != nil {
		t.Fatalf("load real registry: %v", err)
	}
	outDir := t.TempDir()
	if err := GenerateConfigsWithPath(reg, registryPath, outDir, []string{"codex"}, false, "", true, "", false); err != nil {
		t.Fatalf("GenerateConfigsWithPath(codex): %v", err)
	}
	data, err := os.ReadFile(filepath.Join(outDir, "codex", "rules", "default.rules"))
	if err != nil {
		t.Fatalf("read generated rules/default.rules: %v", err)
	}
	for _, want := range []string{
		CodexRulesBeginMarker,
		CodexRulesEndMarker,
		`pattern = ["git"],`,
		`pattern = ["/usr/bin/git"],`,
	} {
		if !strings.Contains(string(data), want) {
			t.Errorf("real-registry rules missing %q:\n%s", want, data)
		}
	}
}
