package generator

import (
	"strings"
	"testing"

	"github.com/crb2nu/loom/pkg/registry"
)

func regWithFlightdeckCapture(platforms ...string) *registry.Registry {
	pp := map[string]*registry.PlatformPermission{}
	for _, p := range platforms {
		pp[p] = &registry.PlatformPermission{
			Settings: map[string]any{"flightdeck_capture": true},
		}
	}
	return &registry.Registry{PlatformPermissions: pp}
}

// flightdeckEntriesForEvent returns the hook entries under event whose command
// references flightdeck-capture.
func flightdeckEntriesForEvent(t *testing.T, hooks map[string]any, event string) []map[string]any {
	t.Helper()
	entries, _ := hooks[event].([]map[string]any)
	var matched []map[string]any
	for _, entry := range entries {
		inner, _ := entry["hooks"].([]map[string]any)
		for _, h := range inner {
			cmd, _ := h["command"].(string)
			if strings.Contains(cmd, "flightdeck-capture") {
				matched = append(matched, entry)
			}
		}
	}
	return matched
}

func TestFlightdeckCaptureHooks_ClaudeNineEventSet(t *testing.T) {
	profile, err := GetPlatformProfile("claude")
	if err != nil {
		t.Fatalf("claude profile: %v", err)
	}
	hooks := claudeHooks(regWithFlightdeckCapture("claude"), profile, "loom")

	for _, event := range flightdeckClaudeEvents {
		matched := flightdeckEntriesForEvent(t, hooks, event)
		if len(matched) != 1 {
			t.Fatalf("event %s: want exactly 1 flightdeck entry, got %d", event, len(matched))
		}
		entry := matched[0]
		inner := entry["hooks"].([]map[string]any)
		if cmd := inner[0]["command"]; cmd != flightdeckCaptureBin {
			t.Errorf("event %s: command = %q, want %q", event, cmd, flightdeckCaptureBin)
		}
		matcher, hasMatcher := entry["matcher"]
		if flightdeckClaudeMatcherEvents[event] {
			if matcher != "*" {
				t.Errorf("event %s: matcher = %v, want \"*\"", event, matcher)
			}
		} else if hasMatcher {
			t.Errorf("event %s: unexpected matcher %v on lifecycle event", event, matcher)
		}
	}
}

func TestFlightdeckCaptureHooks_DisabledByDefault(t *testing.T) {
	claudeProfile, err := GetPlatformProfile("claude")
	if err != nil {
		t.Fatalf("claude profile: %v", err)
	}
	geminiProfile, err := GetPlatformProfile("gemini")
	if err != nil {
		t.Fatalf("gemini profile: %v", err)
	}
	for name, hooks := range map[string]map[string]any{
		"claude/empty-registry": claudeHooks(&registry.Registry{}, claudeProfile, "loom"),
		"gemini/empty-registry": geminiHooks(&registry.Registry{}, geminiProfile, "loom"),
		"gemini/nil-registry":   geminiHooks(nil, geminiProfile, "loom"),
	} {
		for event := range hooks {
			if matched := flightdeckEntriesForEvent(t, hooks, event); len(matched) > 0 {
				t.Errorf("%s: event %s has flightdeck entry without flightdeck_capture gate", name, event)
			}
		}
	}
}

func TestFlightdeckCaptureHooks_GeminiSevenEventSet(t *testing.T) {
	profile, err := GetPlatformProfile("gemini")
	if err != nil {
		t.Fatalf("gemini profile: %v", err)
	}
	hooks := geminiHooks(regWithFlightdeckCapture("gemini"), profile, "loom")

	wantCmd := flightdeckCaptureBin + " -platform gemini-cli"
	for _, event := range flightdeckGeminiEvents {
		matched := flightdeckEntriesForEvent(t, hooks, event)
		if len(matched) != 1 {
			t.Fatalf("event %s: want exactly 1 flightdeck entry, got %d", event, len(matched))
		}
		entry := matched[0]
		inner := entry["hooks"].([]map[string]any)
		if cmd := inner[0]["command"]; cmd != wantCmd {
			t.Errorf("event %s: command = %q, want %q", event, cmd, wantCmd)
		}
		if matcher, ok := entry["matcher"]; ok {
			t.Errorf("event %s: unexpected matcher %v (gemini capture entries take none)", event, matcher)
		}
	}
}

// One unknown event name silently disables ALL Claude Code hooks, so the
// flightdeck sets must stay inside each platform's pinned baseline.
func TestFlightdeckEvents_WithinPlatformBaselines(t *testing.T) {
	claudeBaseline := map[string]bool{}
	for _, e := range claudeHookEventBaseline {
		claudeBaseline[e] = true
	}
	for _, e := range flightdeckClaudeEvents {
		if !claudeBaseline[e] {
			t.Errorf("claude flightdeck event %q not in claudeHookEventBaseline", e)
		}
	}

	geminiBaseline := map[string]bool{}
	for _, e := range geminiHookEventBaseline {
		geminiBaseline[e] = true
	}
	for _, e := range flightdeckGeminiEvents {
		if !geminiBaseline[e] {
			t.Errorf("gemini flightdeck event %q not in geminiHookEventBaseline", e)
		}
	}
}
