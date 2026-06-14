package generator

import (
	"fmt"
	"os"

	"github.com/crb2nu/loom/pkg/registry"
)

// geminiHooksConfig returns a Gemini CLI settings.json with lifecycle hooks
// and auto-approve settings. Uses the same three-level nesting as Claude Code
// but with Gemini event names:
//   - SessionStart -> SessionStart (same)
//   - SessionEnd -> SessionEnd (Gemini uses SessionEnd, not Stop)
//   - AfterTool -> AfterTool (Gemini uses AfterTool, not PostToolUse)
//
// Gemini tool names also differ (run_shell_command vs Bash).
func geminiHooksConfig() map[string]any {
	profile, _ := GetPlatformProfile("gemini")
	return geminiHooksConfigFromRegistry(nil, profile, "")
}

// geminiHooksConfigFromRegistry builds Gemini CLI settings.json, merging
// lifecycle hooks with auto-approve settings from the registry's
// platform_permissions.gemini section.
func geminiHooksConfigFromRegistry(reg *registry.Registry, profile *PlatformProfile, loomBinary string) map[string]any {
	// NOTE: do not add top-level keys here without verifying them against the
	// installed Gemini CLI's SETTINGS_SCHEMA (bundle) AND the upstream schema:
	// https://raw.githubusercontent.com/google-gemini/gemini-cli/main/schemas/settings.schema.json
	// A previous "agentConfig" key (commit 809562b6) existed in neither and was
	// silently ignored by the CLI. TestGeminiSettingsNoUnknownTopLevelKeys guards this.
	config := map[string]any{
		"hooks":       geminiHooks(reg, profile, loomBinary),
		"hooksConfig": map[string]any{"enabled": true},
	}

	// Merge auto-approve and tool settings from registry.
	pp := registryPlatformPerms(reg, "gemini")
	if pp != nil && pp.Settings != nil {
		general := map[string]any{}
		if v, ok := pp.Settings["approval_mode"].(string); ok && v != "" {
			general["defaultApprovalMode"] = v
		}
		if v, ok := pp.Settings["checkpointing"].(bool); ok && v {
			general["checkpointing"] = map[string]any{"enabled": true}
		}
		if len(general) > 0 {
			config["general"] = general
		}
		tools := map[string]any{}
		if allowed := coerceStringSlice(pp.Settings["tools_allowed"]); len(allowed) > 0 {
			tools["allowed"] = allowed
		}
		if core := coerceStringSlice(pp.Settings["tools_core"]); len(core) > 0 {
			tools["core"] = core
		}
		if exclude := coerceStringSlice(pp.Settings["tools_exclude"]); len(exclude) > 0 {
			tools["exclude"] = exclude
		}
		if len(tools) > 0 {
			config["tools"] = tools
		}

		security := map[string]any{}
		if v, ok := pp.Settings["enable_permanent_tool_approval"].(bool); ok {
			security["enablePermanentToolApproval"] = v
		}
		if v, ok := pp.Settings["folder_trust_enabled"].(bool); ok {
			security["folderTrust"] = map[string]any{
				"enabled": v,
			}
		}
		if len(security) > 0 {
			config["security"] = security
		}
	}

	return config
}

// geminiHooks returns the hooks block for Gemini CLI settings.json.
func geminiHooks(reg *registry.Registry, profile *PlatformProfile, loomBinary string) map[string]any {
	hooks := buildPlatformHooks(reg, profile.Hooks, loomBinary)

	// Append shared policy hooks (Gemini now has native enforcement via policy_refs).
	appendHookPolicies(hooks, reg, profile.Hooks)

	// withEventEmitGate drops telemetry_eventEmit when the registry gate is off
	// (Phase E HUD cutover; see flightdeck_hooks.go).
	appendHookExtras(hooks, withEventEmitGate(reg, "gemini", profile.Hooks), loomBinary)

	// Append flightdeck live-capture hooks when the registry gate is on
	// (see flightdeck_hooks.go).
	appendFlightdeckCaptureHooks(hooks, reg, "gemini")

	// Strip event names the Gemini CLI does not accept — same failure class
	// as Claude Code (see gemini_hook_events.go).
	if dropped, source := validateGeminiHookEvents(hooks); len(dropped) > 0 {
		fmt.Fprint(os.Stderr, geminiHookEventWarning(dropped, source))
	}
	return hooks
}
