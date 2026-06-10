package generator

import (
	"sort"
	"strings"
)

// Gemini CLI is the same failure class as Claude Code for hook event names
// (see claude_hook_events.go): an unrecognized event key in settings.json can
// disable hook execution without any error surfaced to the user. Unlike
// Claude, the installed Gemini binary is not cheaply probeable for its enum,
// so validation uses a pinned baseline only.
//
// geminiHookEventBaseline reproduces the HookEventName enum from Gemini CLI
// 0.43.0 (mirrors GeminiAllowlist0430 in services/loom-flightdeck
// edge/internal/install/gemini.go, which was extracted from the installed
// CLI's bundle).
var geminiHookEventBaseline = []string{
	"SessionStart",
	"SessionEnd",
	"BeforeTool",
	"AfterTool",
	"BeforeToolSelection",
	"BeforeAgent",
	"AfterAgent",
	"BeforeModel",
	"AfterModel",
	"Notification",
	"PreCompress",
}

// geminiAcceptedHookEvents returns the pinned accepted set plus a provenance
// label for warning messages.
func geminiAcceptedHookEvents() (map[string]bool, string) {
	set := make(map[string]bool, len(geminiHookEventBaseline))
	for _, e := range geminiHookEventBaseline {
		set[e] = true
	}
	return set, "pinned baseline from Gemini CLI 0.43.0"
}

// validateGeminiHookEvents strips hook events the Gemini CLI does not
// recognize and returns the dropped names plus the provenance of the accepted
// set. Callers must surface dropped names loudly.
func validateGeminiHookEvents(hooks map[string]any) (dropped []string, source string) {
	accepted, source := geminiAcceptedHookEvents()
	for event := range hooks {
		if !accepted[event] {
			dropped = append(dropped, event)
			delete(hooks, event)
		}
	}
	sort.Strings(dropped)
	return dropped, source
}

// geminiHookEventWarning formats the stderr warning for dropped hook events.
func geminiHookEventWarning(dropped []string, source string) string {
	return "WARN  [gemini] dropping hook events not accepted by the Gemini CLI (" +
		source + "): " + strings.Join(dropped, ", ") +
		" — unknown event names can disable Gemini hooks without any error\n"
}
