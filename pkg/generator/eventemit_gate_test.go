package generator

import (
	"strings"
	"testing"

	"github.com/crb2nu/loom/pkg/registry"
)

// regWithEventEmit returns a registry that explicitly sets
// telemetry_event_emit for each named platform.
func regWithEventEmit(value bool, platforms ...string) *registry.Registry {
	pp := map[string]*registry.PlatformPermission{}
	for _, p := range platforms {
		pp[p] = &registry.PlatformPermission{
			Settings: map[string]any{"telemetry_event_emit": value},
		}
	}
	return &registry.Registry{PlatformPermissions: pp}
}

// hasEventEmit reports whether any hook command across all events references
// `agent event-emit`.
func hasEventEmit(hooks map[string]any) bool {
	for _, v := range hooks {
		entries, _ := v.([]map[string]any)
		for _, entry := range entries {
			inner, _ := entry["hooks"].([]map[string]any)
			for _, h := range inner {
				if cmd, _ := h["command"].(string); strings.Contains(cmd, "agent event-emit") {
					return true
				}
			}
		}
	}
	return false
}

// Default (key absent) and explicit-true keep event-emit wired — the gate must
// not change historical behavior unless explicitly disabled.
func TestEventEmitGate_DefaultAndExplicitOnKeepHooks(t *testing.T) {
	claude, err := GetPlatformProfile("claude")
	if err != nil {
		t.Fatalf("claude profile: %v", err)
	}
	gemini, err := GetPlatformProfile("gemini")
	if err != nil {
		t.Fatalf("gemini profile: %v", err)
	}
	cases := map[string]map[string]any{
		"claude/empty-registry": claudeHooks(&registry.Registry{}, claude, "loom"),
		"claude/explicit-true":  claudeHooks(regWithEventEmit(true, "claude"), claude, "loom"),
		"gemini/empty-registry": geminiHooks(&registry.Registry{}, gemini, "loom"),
		"gemini/explicit-true":  geminiHooks(regWithEventEmit(true, "gemini"), gemini, "loom"),
	}
	for name, hooks := range cases {
		if !hasEventEmit(hooks) {
			t.Errorf("%s: expected event-emit hooks present (default/explicit-on)", name)
		}
	}
}

// telemetry_event_emit: false drops every event-emit hook (the Phase E cutover
// switch), without disturbing the rest of the hooks block.
func TestEventEmitGate_DisabledDropsHooks(t *testing.T) {
	claude, err := GetPlatformProfile("claude")
	if err != nil {
		t.Fatalf("claude profile: %v", err)
	}
	gemini, err := GetPlatformProfile("gemini")
	if err != nil {
		t.Fatalf("gemini profile: %v", err)
	}

	claudeHooksMap := claudeHooks(regWithEventEmit(false, "claude"), claude, "loom")
	if hasEventEmit(claudeHooksMap) {
		t.Error("claude: event-emit hooks present despite telemetry_event_emit:false")
	}
	// The hooks block is otherwise intact (e.g. PostToolUse still exists with
	// its non-telemetry entries), so the gate only removed event-emit.
	if _, ok := claudeHooksMap["PostToolUse"]; !ok {
		t.Error("claude: gate removed the whole PostToolUse slot, want only event-emit dropped")
	}

	geminiHooksMap := geminiHooks(regWithEventEmit(false, "gemini"), gemini, "loom")
	if hasEventEmit(geminiHooksMap) {
		t.Error("gemini: event-emit hooks present despite telemetry_event_emit:false")
	}
}

// The gate is per-platform: disabling claude must not disable gemini.
func TestEventEmitGate_PerPlatformIsolation(t *testing.T) {
	gemini, err := GetPlatformProfile("gemini")
	if err != nil {
		t.Fatalf("gemini profile: %v", err)
	}
	// Registry disables only claude; gemini has no setting → default on.
	reg := regWithEventEmit(false, "claude")
	if !hasEventEmit(geminiHooks(reg, gemini, "loom")) {
		t.Error("gemini lost event-emit when only claude was gated off")
	}
}
