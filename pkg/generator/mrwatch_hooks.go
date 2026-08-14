package generator

import (
	"fmt"

	"github.com/crb2nu/loom/pkg/registry"
)

// Branch-MR awareness hook injection (mrwatch M3a).
//
// A context-injection hook runs `loom agent mr-status --hook <vendor>` at the
// start of each user turn. The CLI gates injection twice — the MR must be in an
// attention-worthy state (conflict, ci_failed_*, automerge_unarmed,
// pipeline_skipped, stale_branch) AND that set must have changed since the last
// injection (cache under ~/.loom/mrwatch/) — then prints the vendor JSON
// envelope carrying an "MR needs attention" line into the model's context. The
// CLI is hook-safe: no-attention / unchanged / no-MR / HUD-unreachable → prints
// nothing, exit 0. So the hook runs every turn but injects rarely.
//
// Vendor contracts (re-verified against live docs 2026-08-07; cited in
// cmd/loom/cmd_agent_mrstatus.go mrStatusHookEnvelope):
//
//   - Claude Code — UserPromptSubmit. For UserPromptSubmit, hook stdout is
//     added to the model's context, and the parsed
//     hookSpecificOutput.additionalContext field carries it. UserPromptSubmit
//     is in the documented "no matcher support" class, so the emitted entry
//     deliberately carries no "matcher" key.
//     https://code.claude.com/docs/en/hooks
//
//   - Gemini CLI — BeforeAgent, NOT BeforeModel. BeforeModel does not support
//     additionalContext (it only overrides llm_request/llm_response);
//     BeforeAgent's hookSpecificOutput.additionalContext is "appended to the
//     prompt" for that turn — the true UserPromptSubmit analogue.
//     https://github.com/google-gemini/gemini-cli/blob/main/docs/hooks/reference.md
//     https://github.com/google-gemini/gemini-cli/blob/main/docs/hooks/writing-hooks.md
//
//     Gemini's docs show `"matcher": "*"` on non-tool events, but every
//     non-tool Gemini hook this generator already ships (SessionStart,
//     SessionEnd, the AfterTool event-emit entry) omits `matcher` and fires in
//     production, so BeforeAgent follows the same in-repo convention rather
//     than introducing a one-off shape. Note Claude and Gemini differ here:
//     Claude OMITS matcher for these events, Gemini's docs suggest "*" — do not
//     copy one vendor's convention onto the other.
//
// Codex is intentionally NOT wired here: Codex has no context-injection hook
// event (its UserPromptSubmit runs at turn scope but exposes no additionalContext
// output surface). Codex sessions are covered by the loom proxy trailer (M3b,
// merged), which appends the same MR line to git/gitlab tool results.
//
// PROBE SAFETY: UserPromptSubmit (Claude) and BeforeAgent (Gemini) are both
// present in their platform's accepted-event baseline, and the events emitted
// here still pass through validateClaudeHookEvents / validateGeminiHookEvents in
// the caller AFTER this append. If a future CLI drops either event from its
// probed enum, that validation strips this hook (with a loud warning) rather
// than emitting an unknown event that would silently disable ALL of the
// platform's hooks (the pre-!686 incident). The hook is therefore never emitted
// blind.

// mrwatchHookEvent maps a hook agent_id to its context-injection event name.
// Returns "" for platforms with no context-injection hook (i.e. everything
// other than Claude Code and Gemini CLI).
func mrwatchHookEvent(agentID string) string {
	switch agentID {
	case "claude-code":
		return "UserPromptSubmit"
	case "gemini-cli":
		return "BeforeAgent"
	}
	return ""
}

// mrwatchHookVendor maps a hook agent_id to the `--hook <vendor>` value passed
// to the CLI. Returns "" for unsupported platforms.
func mrwatchHookVendor(agentID string) string {
	switch agentID {
	case "claude-code":
		return "claude"
	case "gemini-cli":
		return "gemini"
	}
	return ""
}

// mrwatchHookEnabled reports whether the MR-status context-injection hook
// should be generated for the platform. It defaults to TRUE (absence of the
// key preserves the shipping default) and is disabled only when the registry
// explicitly sets platform_permissions.<platform>.settings.mrwatch_hook: false.
// This is the per-platform kill switch for M3a.
func mrwatchHookEnabled(reg *registry.Registry, platform string) bool {
	pp := registryPlatformPerms(reg, platform)
	if pp == nil || pp.Settings == nil {
		return true
	}
	v, ok := pp.Settings["mrwatch_hook"].(bool)
	if !ok {
		return true // key absent → default on
	}
	return v
}

// appendMRStatusHook appends the delta-gated MR-status context-injection hook
// for the platform, when supported and enabled. platform is the config key
// ("claude" / "gemini"); hp carries the agent_id that selects the vendor/event.
//
// The command is deliberately trivial (the JSON contract + delta gating live in
// the Go CLI, unit-tested in cmd/loom): consume stdin so the vendor's hook
// payload does not linger on the pipe, then run the CLI, always succeeding
// (`|| true`) so a slow or down loom daemon never breaks the user's turn.
//
// The only ${...} form is ${TMPDIR:-/tmp} in the shared log redirect, which is
// the same Gemini-safe env-var-with-literal-default already emitted into Gemini
// settings by buildPlatformHooks — no nested defaults, no ${shellvar:-...} on
// shell locals, per the Gemini load-time interpolation constraint.
//
// The entry is APPENDED to any existing hooks for the event (e.g. flightdeck
// capture on UserPromptSubmit) so foreign / earlier-generated entries are
// preserved, never clobbered.
func appendMRStatusHook(hooks map[string]any, reg *registry.Registry, platform string, hp HookProfile, loomBinary string) {
	if !mrwatchHookEnabled(reg, platform) {
		return
	}
	event := mrwatchHookEvent(hp.AgentID)
	vendor := mrwatchHookVendor(hp.AgentID)
	if event == "" || vendor == "" {
		return
	}

	loomCmd := shellQuote(normalizeLoomBinary(loomBinary))
	log := `2>>"${TMPDIR:-/tmp}/loom-agent-hooks.log"`
	command := fmt.Sprintf(
		`INPUT=$(cat); %s agent mr-status --hook %s %s || true`,
		loomCmd, vendor, log,
	)

	entry := map[string]any{
		"hooks": []map[string]any{
			{"type": "command", "command": command},
		},
	}
	existing, _ := hooks[event].([]map[string]any)
	hooks[event] = append(existing, entry)
}
