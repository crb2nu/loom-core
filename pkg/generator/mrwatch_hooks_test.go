package generator

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/registry"
)

// mrHookCommandForEvent returns the first hook command string under the given
// event whose command references `mr-status`, or "" when none is present.
func mrHookCommandForEvent(hooks map[string]any, event string) string {
	entries, _ := hooks[event].([]map[string]any)
	for _, entry := range entries {
		inner, _ := entry["hooks"].([]map[string]any)
		for _, h := range inner {
			if cmd, _ := h["command"].(string); strings.Contains(cmd, "mr-status") {
				return cmd
			}
		}
	}
	return ""
}

// regWithMRWatch returns a registry that explicitly sets mrwatch_hook for each
// named platform.
func regWithMRWatch(value bool, platforms ...string) *registry.Registry {
	pp := map[string]*registry.PlatformPermission{}
	for _, p := range platforms {
		pp[p] = &registry.PlatformPermission{
			Settings: map[string]any{"mrwatch_hook": value},
		}
	}
	return &registry.Registry{PlatformPermissions: pp}
}

// TestMRWatchHook_ClaudeEmitsUserPromptSubmit asserts the Claude profile emits a
// UserPromptSubmit hook running `loom agent mr-status --hook claude`, and that
// the event survives the accepted-event validation (UserPromptSubmit is in the
// 2.1.61 baseline — a blind unknown event would silently disable ALL hooks).
func TestMRWatchHook_ClaudeEmitsUserPromptSubmit(t *testing.T) {
	profile, err := GetPlatformProfile("claude")
	if err != nil {
		t.Fatalf("GetPlatformProfile(claude): %v", err)
	}
	hooks := claudeHooks(&registry.Registry{}, profile, "loom")

	cmd := mrHookCommandForEvent(hooks, "UserPromptSubmit")
	if cmd == "" {
		t.Fatal("expected a UserPromptSubmit mr-status hook, found none (event may have been stripped by validation)")
	}
	if !strings.Contains(cmd, "agent mr-status --hook claude") {
		t.Errorf("UserPromptSubmit command does not invoke `agent mr-status --hook claude`: %q", cmd)
	}
	if !strings.HasSuffix(strings.TrimSpace(cmd), "|| true") {
		t.Errorf("mr-status hook must be best-effort (|| true), got: %q", cmd)
	}
}

// TestMRWatchHook_GeminiEmitsBeforeAgent asserts the Gemini profile emits a
// BeforeAgent hook (NOT BeforeModel — BeforeModel has no additionalContext
// output) running `loom agent mr-status --hook gemini`.
func TestMRWatchHook_GeminiEmitsBeforeAgent(t *testing.T) {
	profile, err := GetPlatformProfile("gemini")
	if err != nil {
		t.Fatalf("GetPlatformProfile(gemini): %v", err)
	}
	hooks := geminiHooks(&registry.Registry{}, profile, "loom")

	cmd := mrHookCommandForEvent(hooks, "BeforeAgent")
	if cmd == "" {
		t.Fatal("expected a BeforeAgent mr-status hook, found none")
	}
	if !strings.Contains(cmd, "agent mr-status --hook gemini") {
		t.Errorf("BeforeAgent command does not invoke `agent mr-status --hook gemini`: %q", cmd)
	}
	// BeforeModel must NOT carry the injection hook — it cannot inject context.
	if mrHookCommandForEvent(hooks, "BeforeModel") != "" {
		t.Error("mr-status hook wrongly wired to BeforeModel (no additionalContext support); must use BeforeAgent")
	}
}

// TestMRWatchHook_GeminiCommandNoUnsafeInterpolation guards the Gemini
// load-time ${VAR} constraint: the emitted command must not contain nested
// ${...${...}...} defaults, which the CLI's non-brace-aware resolver mangles.
// Only the shared ${TMPDIR:-/tmp} log redirect (env var + literal default) is
// allowed.
func TestMRWatchHook_GeminiCommandNoUnsafeInterpolation(t *testing.T) {
	profile, err := GetPlatformProfile("gemini")
	if err != nil {
		t.Fatalf("GetPlatformProfile(gemini): %v", err)
	}
	hooks := geminiHooks(&registry.Registry{}, profile, "loom")
	cmd := mrHookCommandForEvent(hooks, "BeforeAgent")
	if cmd == "" {
		t.Fatal("no BeforeAgent mr-status hook to inspect")
	}
	if strings.Contains(cmd, "${") {
		// Only ${TMPDIR:-/tmp} is permitted; reject any other ${...}.
		remainder := strings.ReplaceAll(cmd, "${TMPDIR:-/tmp}", "")
		if strings.Contains(remainder, "${") {
			t.Errorf("BeforeAgent command has a non-allowlisted ${...} interpolation (Gemini load-time hazard): %q", cmd)
		}
	}
	// Valid POSIX shell syntax.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "sh", "-n", "-c", cmd).CombinedOutput(); err != nil {
		t.Fatalf("BeforeAgent mr-status command invalid shell syntax: %v\n%s", err, out)
	}
}

// TestMRWatchHook_DisabledByRegistry asserts the per-platform kill switch
// (mrwatch_hook: false) suppresses the hook without disturbing other hooks.
func TestMRWatchHook_DisabledByRegistry(t *testing.T) {
	claude, err := GetPlatformProfile("claude")
	if err != nil {
		t.Fatalf("claude profile: %v", err)
	}
	gemini, err := GetPlatformProfile("gemini")
	if err != nil {
		t.Fatalf("gemini profile: %v", err)
	}

	chooks := claudeHooks(regWithMRWatch(false, "claude"), claude, "loom")
	if mrHookCommandForEvent(chooks, "UserPromptSubmit") != "" {
		t.Error("mrwatch_hook:false should suppress the Claude UserPromptSubmit mr-status hook")
	}
	// The rest of the hooks block must remain intact.
	if _, ok := chooks["SessionStart"]; !ok {
		t.Error("disabling mrwatch_hook must not drop SessionStart")
	}

	ghooks := geminiHooks(regWithMRWatch(false, "gemini"), gemini, "loom")
	if mrHookCommandForEvent(ghooks, "BeforeAgent") != "" {
		t.Error("mrwatch_hook:false should suppress the Gemini BeforeAgent mr-status hook")
	}
}

// TestMRWatchHook_DefaultOn asserts absence of the key keeps the hook on (the
// shipping default) — parallels the telemetry_event_emit default-on contract.
func TestMRWatchHook_DefaultOn(t *testing.T) {
	claude, err := GetPlatformProfile("claude")
	if err != nil {
		t.Fatalf("claude profile: %v", err)
	}
	// Empty registry => key absent => default on.
	hooks := claudeHooks(&registry.Registry{}, claude, "loom")
	if mrHookCommandForEvent(hooks, "UserPromptSubmit") == "" {
		t.Error("mr-status hook should default ON when mrwatch_hook is unset")
	}
}

// TestMRWatchHook_PreservesExistingEntries asserts appendMRStatusHook appends to
// (never clobbers) foreign / earlier-generated entries on the same event — the
// foreign-entry-preserving-merge invariant (flightdeck capture also lands on
// UserPromptSubmit).
func TestMRWatchHook_PreservesExistingEntries(t *testing.T) {
	claude, err := GetPlatformProfile("claude")
	if err != nil {
		t.Fatalf("claude profile: %v", err)
	}
	foreign := map[string]any{
		"command": "~/.loom/flightdeck/bin/flightdeck-capture",
		"type":    "command",
	}
	hooks := map[string]any{
		"UserPromptSubmit": []map[string]any{
			{"hooks": []map[string]any{foreign}},
		},
	}
	appendMRStatusHook(hooks, &registry.Registry{}, "claude", claude.Hooks, "loom")

	entries, _ := hooks["UserPromptSubmit"].([]map[string]any)
	if len(entries) != 2 {
		t.Fatalf("expected foreign entry preserved + mr-status appended (2 entries), got %d", len(entries))
	}
	// Foreign entry must still be first and unchanged.
	firstInner, _ := entries[0]["hooks"].([]map[string]any)
	if len(firstInner) != 1 || !strings.Contains(firstInner[0]["command"].(string), "flightdeck-capture") {
		t.Errorf("foreign UserPromptSubmit entry was clobbered: %+v", entries[0])
	}
	if mrHookCommandForEvent(hooks, "UserPromptSubmit") == "" {
		t.Error("mr-status hook not appended to UserPromptSubmit")
	}
}

// TestMRWatchHook_NonInjectingPlatformsSkip asserts platforms with no
// context-injection event (e.g. codex) get no mr-status hook.
func TestMRWatchHook_NonInjectingPlatformsSkip(t *testing.T) {
	hp := HookProfile{AgentID: "codex"}
	hooks := map[string]any{}
	appendMRStatusHook(hooks, &registry.Registry{}, "codex", hp, "loom")
	if len(hooks) != 0 {
		t.Errorf("codex (no context-injection hook) must get no mr-status hook, got: %v", hooks)
	}
}

// TestMRWatchHook_ClaudeSurvivesEventValidation is a focused regression: the
// full claudeHooks pipeline (which strips unknown events) must retain the
// UserPromptSubmit event when the installed CLI accepts it. Uses a short
// timeout only to keep the probe bounded; result asserted structurally.
func TestMRWatchHook_ClaudeSurvivesEventValidation(t *testing.T) {
	claude, err := GetPlatformProfile("claude")
	if err != nil {
		t.Fatalf("claude profile: %v", err)
	}
	hooks := claudeHooks(testRegistry(), claude, "loom")
	if _, ok := hooks["UserPromptSubmit"]; !ok {
		t.Skip("UserPromptSubmit not accepted by the locally probed claude CLI — hook correctly omitted (not a failure)")
	}
	if mrHookCommandForEvent(hooks, "UserPromptSubmit") == "" {
		t.Error("UserPromptSubmit present but mr-status hook missing from it")
	}
}
