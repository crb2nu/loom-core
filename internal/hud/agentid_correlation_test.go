package hud

import "testing"

func TestAgentIDMatchesSession(t *testing.T) {
	cases := []struct {
		name, eventAID, sessAID string
		want                    bool
	}{
		{"exact", "claude-code-552019522", "claude-code-552019522", true},
		{"base matches scoped session", "claude-code-552019522", "claude-code-552019522-665755975", true},
		{"scoped event matches base session", "claude-code-552019522-665755975", "claude-code-552019522", true},
		{"different workspace", "claude-code-552019522", "claude-code-999999999-1", false},
		{"different type same hash", "codex-552019522", "claude-code-552019522-1", false},
		{"prefix without boundary (hash 552 vs 5520195)", "claude-code-552", "claude-code-552019522", false},
		{"empty event", "", "claude-code-552019522", false},
		{"empty session", "claude-code-552019522", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := agentIDMatchesSession(c.eventAID, c.sessAID); got != c.want {
				t.Fatalf("agentIDMatchesSession(%q,%q)=%v want %v", c.eventAID, c.sessAID, got, c.want)
			}
		})
	}
}
