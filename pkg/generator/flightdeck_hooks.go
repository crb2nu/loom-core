package generator

import (
	"github.com/crb2nu/loom/pkg/registry"
)

// Flightdeck live-capture hooks.
//
// `flightdeck-capture install` (services/loom-flightdeck, edge/internal/install)
// historically merged its capture hook entries into ~/.claude/settings.json and
// ~/.gemini/settings.json out-of-band. Every `loom sync <platform> --regen`
// then wiped them, because the regenerated hooks block replaced the home file's
// hooks wholesale — flightdeck lost all live capture until its platform-liveness
// canary noticed the silence 24-48h later. Rendering the capture entries as
// first-class generated hooks keeps capture alive across regens.
//
// Gated behind platform_permissions.<platform>.settings.flightdeck_capture in
// the registry so machines without the flightdeck edge binary (e.g. cluster
// code-server pods) don't get hooks that fail on every event.
//
// Event sets mirror the flightdeck installer's MVP sets and MUST stay within
// each platform's accepted enum: one unknown event name silently disables ALL
// Claude Code hooks (see claude_hook_events.go; Gemini equivalent in
// gemini_hook_events.go). Source of truth for the installer sets:
// services/loom-flightdeck edge/internal/install/{install,gemini}.go.

// flightdeckCaptureBin is the stable install location of the capture binary.
// The leading ~ is expanded by the shell that runs hook commands on both
// platforms. Kept free of ${VAR} forms: the Gemini settings loader interpolates
// ${...} at load time with a non-brace-aware regex.
const flightdeckCaptureBin = "~/.loom/flightdeck/bin/flightdeck-capture"

// flightdeckClaudeEvents is the Claude Code capture set (installer MVPEvents).
// All nine names are present in claudeHookEventBaseline (2.1.61).
var flightdeckClaudeEvents = []string{
	"SessionStart",
	"UserPromptSubmit",
	"PreToolUse",
	"PostToolUse",
	"PostToolUseFailure",
	"PermissionRequest",
	"Notification",
	"Stop",
	"SessionEnd",
}

// flightdeckClaudeMatcherEvents are the tool-scoped events that take a
// catch-all matcher; the remaining lifecycle events reject matchers.
var flightdeckClaudeMatcherEvents = map[string]bool{
	"PreToolUse":         true,
	"PostToolUse":        true,
	"PostToolUseFailure": true,
}

// flightdeckGeminiEvents is the Gemini CLI capture set (installer
// GeminiMVPEvents). All seven names are present in geminiHookEventBaseline.
// Gemini hook entries take no matcher — capture wants every tool.
var flightdeckGeminiEvents = []string{
	"SessionStart",
	"BeforeAgent",
	"BeforeTool",
	"AfterTool",
	"AfterAgent",
	"Notification",
	"SessionEnd",
}

// flightdeckCaptureEnabled reads the registry gate for the given platform.
func flightdeckCaptureEnabled(reg *registry.Registry, platform string) bool {
	pp := registryPlatformPerms(reg, platform)
	if pp == nil || pp.Settings == nil {
		return false
	}
	v, ok := pp.Settings["flightdeck_capture"].(bool)
	return ok && v
}

// appendFlightdeckCaptureHooks appends the flightdeck capture entries for the
// platform when the registry gate is on. Unlike extras, it bootstraps event
// slots that no other generated hook uses (UserPromptSubmit, Notification, …).
func appendFlightdeckCaptureHooks(hooks map[string]any, reg *registry.Registry, platform string) {
	if !flightdeckCaptureEnabled(reg, platform) {
		return
	}
	var events []string
	command := flightdeckCaptureBin
	var matcherEvents map[string]bool
	switch platform {
	case "claude":
		events = flightdeckClaudeEvents
		matcherEvents = flightdeckClaudeMatcherEvents
	case "gemini":
		events = flightdeckGeminiEvents
		command += " -platform gemini-cli"
	default:
		return
	}
	for _, event := range events {
		entry := map[string]any{
			"hooks": []map[string]any{
				{"type": "command", "command": command},
			},
		}
		if matcherEvents[event] {
			entry["matcher"] = "*"
		}
		existing, _ := hooks[event].([]map[string]any)
		hooks[event] = append(existing, entry)
	}
}
