package daemon

import "testing"

func TestApplyOffLANRouting(t *testing.T) {
	prefs := map[string]RoutingPreference{
		"agent_context": RoutingLocalOnly,   // .loom/149 anti-storm pin
		"gitlab":        RoutingPreferLocal, // prefer-local pin
		"weaver":        RoutingHubOnly,     // already hub
		"prometheus":    RoutingHealthBased, // default — left to health fallback
		"already_hub":   RoutingPreferHub,
		"godot":         RoutingLocalOnly, // registry local-only (not hub-capable)
	}
	hubCapable := map[string]bool{
		"agent_context": true,
		"gitlab":        true,
		"weaver":        true,
		"prometheus":    true,
		"already_hub":   true,
		"godot":         false, // can never run on the hub
	}

	changed := applyOffLANRouting(prefs, hubCapable)

	if changed != 2 {
		t.Errorf("changed=%d want 2 (agent_context, gitlab)", changed)
	}
	want := map[string]RoutingPreference{
		"agent_context": RoutingPreferHub,   // upgraded
		"gitlab":        RoutingPreferHub,   // upgraded
		"weaver":        RoutingHubOnly,     // unchanged
		"prometheus":    RoutingHealthBased, // unchanged (no explicit pin)
		"already_hub":   RoutingPreferHub,   // unchanged
		"godot":         RoutingLocalOnly,   // unchanged (not hub-capable)
	}
	for name, w := range want {
		if got := prefs[name]; got != w {
			t.Errorf("prefs[%q]=%v want %v", name, got, w)
		}
	}
}

func TestApplyOffLANRouting_NoExplicitPrefsLeftAlone(t *testing.T) {
	// A server that is hub-capable but has no explicit preference must not be
	// forced to the hub — its health-based default already falls back off-LAN.
	prefs := map[string]RoutingPreference{}
	hubCapable := map[string]bool{"tavily": true}
	if changed := applyOffLANRouting(prefs, hubCapable); changed != 0 {
		t.Errorf("changed=%d want 0", changed)
	}
	if _, ok := prefs["tavily"]; ok {
		t.Errorf("tavily should not have been assigned a preference")
	}
}
