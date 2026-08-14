package generator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crb2nu/loom/pkg/registry"
)

// TestGenerateConfigs_RealRegistryCodexModels is the end-to-end golden guard for
// the gpt-5.6-terra / gpt-5.6-sol wiring: it loads the SHIPPED
// mcp/context/registry.yaml and asserts `loom sync codex --regen` would emit the
// implementer model as the config.toml default and the sol planner profile as a
// standalone ~/.codex/sol.config.toml overlay. If a future registry edit drops
// either model, this fails loudly rather than silently regressing the operator.
func TestGenerateConfigs_RealRegistryCodexModels(t *testing.T) {
	const registryPath = "../../mcp/context/registry.yaml"
	reg, err := registry.Load(registryPath)
	if err != nil {
		t.Fatalf("load real registry: %v", err)
	}
	outDir := t.TempDir()
	if err := GenerateConfigsWithPath(reg, registryPath, outDir, []string{"codex"}, false, "", true, "", false); err != nil {
		t.Fatalf("GenerateConfigsWithPath(codex): %v", err)
	}

	cfg, err := os.ReadFile(filepath.Join(outDir, "codex", "config.toml"))
	if err != nil {
		t.Fatalf("read generated config.toml: %v", err)
	}
	if !strings.Contains(string(cfg), `model = "gpt-5.6-terra"`) {
		t.Errorf("config.toml missing default implementer model gpt-5.6-terra:\n%s", cfg)
	}

	sol, err := os.ReadFile(filepath.Join(outDir, "codex", "sol.config.toml"))
	if err != nil {
		t.Fatalf("read generated sol.config.toml: %v", err)
	}
	if !strings.Contains(string(sol), `model = "gpt-5.6-sol"`) {
		t.Errorf("sol.config.toml missing planner model gpt-5.6-sol:\n%s", sol)
	}
}

// codexModelRegistry builds a minimal registry whose codex settings carry a
// default model plus a named `sol` profile — the gpt-5.6-terra / gpt-5.6-sol
// end state this feature wires in.
func codexModelRegistry() *registry.Registry {
	return &registry.Registry{
		PlatformPermissions: map[string]*registry.PlatformPermission{
			"codex": {
				Settings: map[string]any{
					"model":                  "gpt-5.6-terra",
					"model_reasoning_effort": "high",
					"profiles": map[string]any{
						"sol": map[string]any{
							"model": "gpt-5.6-sol",
						},
					},
				},
			},
		},
	}
}

// TestBuildCodexContext_ModelFromRegistry pins the default-model emission: the
// top-level `model` key in ~/.codex/config.toml comes from
// platform_permissions.codex.settings.model.
func TestBuildCodexContext_ModelFromRegistry(t *testing.T) {
	ctx := buildCodexContext(codexModelRegistry(), "/tmp/workspace", "")
	if ctx.Model != "gpt-5.6-terra" {
		t.Errorf("ctx.Model = %q, want %q", ctx.Model, "gpt-5.6-terra")
	}
	rendered, err := renderCodexTemplate(ctx)
	if err != nil {
		t.Fatalf("renderCodexTemplate: %v", err)
	}
	if !strings.Contains(rendered, `model = "gpt-5.6-terra"`) {
		t.Errorf("rendered config.toml missing default model line:\n%s", rendered)
	}
}

// TestBuildCodexProfiles covers the registry→[]codexProfile resolution,
// including the drop of an overlay that would override nothing.
func TestBuildCodexProfiles(t *testing.T) {
	t.Run("nil registry", func(t *testing.T) {
		if got := buildCodexProfiles(nil); got != nil {
			t.Errorf("nil pp: got %v, want nil", got)
		}
	})

	t.Run("no profiles key", func(t *testing.T) {
		pp := &registry.PlatformPermission{Settings: map[string]any{"model": "gpt-5.6-terra"}}
		if got := buildCodexProfiles(pp); got != nil {
			t.Errorf("no profiles: got %v, want nil", got)
		}
	})

	t.Run("sol profile resolves", func(t *testing.T) {
		pp := registryPlatformPerms(codexModelRegistry(), "codex")
		got := buildCodexProfiles(pp)
		if len(got) != 1 {
			t.Fatalf("got %d profiles, want 1", len(got))
		}
		if got[0].Name != "sol" || got[0].Model != "gpt-5.6-sol" {
			t.Errorf("profile = %+v, want {Name:sol Model:gpt-5.6-sol}", got[0])
		}
	})

	t.Run("empty overlay dropped and sorted", func(t *testing.T) {
		pp := &registry.PlatformPermission{Settings: map[string]any{
			"profiles": map[string]any{
				"empty":  map[string]any{},                                  // no override keys → dropped
				"zeta":   map[string]any{"model": "z"},                      // sorts last
				"alpha":  map[string]any{"model": "a"},                      // sorts first
				"reason": map[string]any{"model_reasoning_effort": "xhigh"}, // reasoning-only is kept
			},
		}}
		got := buildCodexProfiles(pp)
		if len(got) != 3 {
			t.Fatalf("got %d profiles, want 3 (empty dropped): %+v", len(got), got)
		}
		if got[0].Name != "alpha" || got[len(got)-1].Name != "zeta" {
			t.Errorf("profiles not sorted by name: %+v", got)
		}
	})
}

// TestRenderCodexProfileTOML asserts the standalone overlay shape for Codex
// 0.134.0+: top-level keys, NO [profiles.<name>] table, and a --profile hint.
func TestRenderCodexProfileTOML(t *testing.T) {
	out := renderCodexProfileTOML(codexProfile{Name: "sol", Model: "gpt-5.6-sol"})
	for _, want := range []string{
		`model = "gpt-5.6-sol"`,
		"codex --profile sol",
		"~/.codex/<name>.config.toml",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("profile overlay missing %q:\n%s", want, out)
		}
	}
	// The deprecated table form must never appear on an ACTIVE (non-comment)
	// line — codex 0.134.0+ ignores [profiles.<name>] tables in config.toml.
	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue // comment lines may reference the deprecated form
		}
		if strings.Contains(trimmed, "[profiles") {
			t.Errorf("profile overlay must not emit a [profiles.<name>] table line: %q\nfull:\n%s", line, out)
		}
	}
	// Reasoning effort only when set.
	if strings.Contains(out, "model_reasoning_effort") {
		t.Errorf("sol overlay set no reasoning effort but emitted the key:\n%s", out)
	}
	withEffort := renderCodexProfileTOML(codexProfile{Name: "deep", Model: "m", ModelReasoningEffort: "xhigh"})
	if !strings.Contains(withEffort, `model_reasoning_effort = "xhigh"`) {
		t.Errorf("expected reasoning-effort line when set:\n%s", withEffort)
	}
}

// TestGenerateCodexProfileConfigs_WritesFile is the golden file-shape test: a
// codex target with a sol profile lands ~/.codex/sol.config.toml; a non-codex
// target and a profile-less registry are both no-ops.
func TestGenerateCodexProfileConfigs_WritesFile(t *testing.T) {
	codexProfileDef, err := GetPlatformProfile("codex")
	if err != nil {
		t.Fatalf("GetPlatformProfile(codex): %v", err)
	}

	t.Run("codex with sol profile", func(t *testing.T) {
		outDir := t.TempDir()
		if err := generateCodexProfileConfigs(codexModelRegistry(), outDir, "codex", codexProfileDef); err != nil {
			t.Fatalf("generateCodexProfileConfigs: %v", err)
		}
		path := filepath.Join(outDir, "codex", "sol.config.toml")
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read sol.config.toml: %v", err)
		}
		if !strings.Contains(string(data), `model = "gpt-5.6-sol"`) {
			t.Errorf("sol.config.toml missing planner model:\n%s", data)
		}
	})

	t.Run("non-codex target is a no-op", func(t *testing.T) {
		claudeProfile, err := GetPlatformProfile("claude")
		if err != nil {
			t.Fatalf("GetPlatformProfile(claude): %v", err)
		}
		outDir := t.TempDir()
		if err := generateCodexProfileConfigs(codexModelRegistry(), outDir, "claude", claudeProfile); err != nil {
			t.Fatalf("generateCodexProfileConfigs(claude): %v", err)
		}
		if entries, _ := os.ReadDir(outDir); len(entries) != 0 {
			t.Errorf("non-codex target wrote files: %v", entries)
		}
	})

	t.Run("codex with no profiles is a no-op", func(t *testing.T) {
		reg := &registry.Registry{PlatformPermissions: map[string]*registry.PlatformPermission{
			"codex": {Settings: map[string]any{"model": "gpt-5.6-terra"}},
		}}
		outDir := t.TempDir()
		if err := generateCodexProfileConfigs(reg, outDir, "codex", codexProfileDef); err != nil {
			t.Fatalf("generateCodexProfileConfigs: %v", err)
		}
		// destDir may be created but must be empty of profile files.
		matches, _ := filepath.Glob(filepath.Join(outDir, "codex", "*.config.toml"))
		if len(matches) != 0 {
			t.Errorf("expected no profile files, got %v", matches)
		}
	})
}
