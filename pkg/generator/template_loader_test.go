package generator

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRenderHookTemplate_NilProfile confirms the no-op path: when the
// profile is nil or has no Hooks.Template, the renderer returns ok=false
// without error so the caller falls through to the legacy Go-builder
// switch.
func TestRenderHookTemplate_NilProfile(t *testing.T) {
	config, ok, err := renderHookTemplate(nil, nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Fatalf("expected ok=false for nil profile, got true (config=%v)", config)
	}
}

func TestRenderHookTemplate_NoTemplateField(t *testing.T) {
	profile, err := GetPlatformProfile("claude")
	if err != nil {
		t.Fatalf("get claude profile: %v", err)
	}
	// Claude has no hooks.template set — template path should be skipped.
	config, ok, err := renderHookTemplate(nil, profile, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Fatalf("expected ok=false when profile.Hooks.Template is empty, got true (config=%v)", config)
	}
}

// TestRenderHookTemplate_GenericTemplate exercises the end-to-end
// template path against the embedded templates/hooks.tmpl.
// Confirms that a profile with Template="hooks.tmpl" produces
// a valid map[string]any with a "hooks" key.
func TestRenderHookTemplate_GenericTemplate(t *testing.T) {
	profile, err := GetPlatformProfile("claude")
	if err != nil {
		t.Fatalf("get claude profile: %v", err)
	}
	// Clone the profile and point it at the generic template. Don't mutate
	// the cached registry copy.
	cloned := *profile
	clonedHooks := cloned.Hooks
	clonedHooks.Template = "hooks.tmpl"
	cloned.Hooks = clonedHooks

	config, ok, err := renderHookTemplate(testRegistry(), &cloned, "/usr/local/bin/loom")
	if err != nil {
		t.Fatalf("render generic template: %v", err)
	}
	if !ok {
		t.Fatalf("expected ok=true, got false")
	}
	if config == nil {
		t.Fatalf("expected non-nil config map")
	}
	hooks, ok := config["hooks"].(map[string]any)
	if !ok {
		t.Fatalf("expected config.hooks to be map[string]any, got %T", config["hooks"])
	}
	if len(hooks) == 0 {
		t.Fatalf("expected hooks map to be non-empty (buildHooks should produce SessionStart, etc.)")
	}
}

func TestGenerateHooksConfig_CodexTemplateMatchesLegacyBytes(t *testing.T) {
	profile, err := GetPlatformProfile("codex")
	if err != nil {
		t.Fatalf("get codex profile: %v", err)
	}
	if profile.Hooks.Template != "hooks.tmpl" {
		t.Fatalf("codex hooks template = %q, want hooks.tmpl", profile.Hooks.Template)
	}

	reg := testRegistry()
	const loomBinary = "/opt/loom/bin/loom"
	templateDir := t.TempDir()
	if err := generateHooksConfig(reg, templateDir, "codex", profile, loomBinary); err != nil {
		t.Fatalf("generate templated codex hooks: %v", err)
	}

	legacyProfile := *profile
	legacyHooks := legacyProfile.Hooks
	legacyHooks.Template = ""
	legacyProfile.Hooks = legacyHooks
	legacyDir := t.TempDir()
	if err := generateHooksConfig(reg, legacyDir, "codex", &legacyProfile, loomBinary); err != nil {
		t.Fatalf("generate legacy codex hooks: %v", err)
	}

	readHooks := func(dir string) []byte {
		t.Helper()
		content, err := os.ReadFile(filepath.Join(dir, "codex", "hooks.json"))
		if err != nil {
			t.Fatalf("read generated codex hooks: %v", err)
		}
		return content
	}
	got := readHooks(templateDir)
	want := readHooks(legacyDir)
	if !bytes.Equal(got, want) {
		t.Fatalf("template output differs byte-for-byte from legacy output:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

// TestRenderHookTemplate_BadTemplate confirms that a missing template
// path produces a clear error with the path included.
func TestRenderHookTemplate_BadTemplate(t *testing.T) {
	profile, err := GetPlatformProfile("claude")
	if err != nil {
		t.Fatalf("get claude profile: %v", err)
	}
	cloned := *profile
	clonedHooks := cloned.Hooks
	clonedHooks.Template = "hooks/does-not-exist.json.tmpl"
	cloned.Hooks = clonedHooks

	_, _, err = renderHookTemplate(nil, &cloned, "")
	if err == nil {
		t.Fatalf("expected error for missing template, got nil")
	}
	if !strings.Contains(err.Error(), "does-not-exist.json.tmpl") {
		t.Fatalf("expected error to mention template path, got: %v", err)
	}
}

// TestHookTemplateFuncs_JSONFunc spot-checks the json funcmap helper since
// it's the most-used helper in templates and must produce compact JSON.
func TestHookTemplateFuncs_JSONFunc(t *testing.T) {
	funcs := hookTemplateFuncs(nil, nil, "")
	jsonFn, ok := funcs["json"].(func(any) (string, error))
	if !ok {
		t.Fatalf("expected funcs[\"json\"] to be func(any) (string, error), got %T", funcs["json"])
	}
	out, err := jsonFn(map[string]any{"a": 1, "b": "two"})
	if err != nil {
		t.Fatalf("json func errored: %v", err)
	}
	// json.Marshal sorts map keys alphabetically: a comes before b.
	wantPrefix := `{"a":1,`
	if !strings.HasPrefix(out, wantPrefix) {
		t.Fatalf("expected output to start with %q, got %q", wantPrefix, out)
	}
}
