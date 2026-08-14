package generator

import (
	"reflect"
	"testing"
)

func TestValidateGeminiHookEvents_DropsUnknownKeepsKnown(t *testing.T) {
	hooks := map[string]any{
		"SessionStart": []map[string]any{{"hooks": []map[string]any{}}},
		"AfterTool":    []map[string]any{{"hooks": []map[string]any{}}},
		"PostToolUse":  []map[string]any{{"hooks": []map[string]any{}}}, // Claude name, not Gemini
		"Bogus":        []map[string]any{{"hooks": []map[string]any{}}},
	}
	dropped, source := validateGeminiHookEvents(hooks)
	if want := []string{"Bogus", "PostToolUse"}; !reflect.DeepEqual(dropped, want) {
		t.Errorf("dropped = %v, want %v", dropped, want)
	}
	if source == "" {
		t.Error("source is empty")
	}
	if _, ok := hooks["SessionStart"]; !ok {
		t.Error("known event SessionStart was stripped")
	}
	if _, ok := hooks["AfterTool"]; !ok {
		t.Error("known event AfterTool was stripped")
	}
	if len(hooks) != 2 {
		t.Errorf("hooks has %d keys after filtering, want 2", len(hooks))
	}
}

// All events the generator emits for Gemini (lifecycle + policies + extras +
// flightdeck capture) must pass the pinned baseline — i.e. validation inside
// geminiHooks must not have dropped anything a second pass would also drop.
func TestGeminiHooks_EmittedEventsAreAccepted(t *testing.T) {
	profile, err := GetPlatformProfile("gemini")
	if err != nil {
		t.Fatalf("gemini profile: %v", err)
	}
	hooks := geminiHooks(regWithFlightdeckCapture("gemini"), profile, "loom")
	accepted, _ := geminiAcceptedHookEvents()
	for event := range hooks {
		if !accepted[event] {
			t.Errorf("geminiHooks emitted event %q not in gemini baseline", event)
		}
	}
	// The flightdeck set must have survived validation intact.
	for _, event := range flightdeckGeminiEvents {
		if len(flightdeckEntriesForEvent(t, hooks, event)) != 1 {
			t.Errorf("flightdeck event %q missing after validation", event)
		}
	}
}
