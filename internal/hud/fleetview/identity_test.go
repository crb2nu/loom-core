package fleetview

import "testing"

func TestAgentBase(t *testing.T) {
	cases := map[string]string{
		"codex-1713039686-2004540290":     "codex",
		"claude-code-552019522":           "claude-code",
		"codex-7b28":                      "codex-7b28",
		"gemini-cli-401508988-2992486099": "gemini-cli",
	}
	for in, want := range cases {
		if got := AgentBase(in); got != want {
			t.Errorf("AgentBase(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRootAgentID(t *testing.T) {
	cases := map[string]string{
		"claude-code-552019522-2804496862": "claude-code-552019522",
		"claude-code-552019522-3116397616": "claude-code-552019522",
		"codex-4188162495":                 "codex-4188162495",
		"codex-4188162495-2303882182":      "codex-4188162495",
		"codex-7b28":                       "codex-7b28",
	}
	for in, want := range cases {
		if got := RootAgentID(in); got != want {
			t.Errorf("RootAgentID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestConversationID(t *testing.T) {
	cases := map[string]string{
		// Claude: keep SESSION_SCOPE, drop WS_HASH — one chat across repos folds.
		"claude-code-3749726816-1105899468": "claude-code-1105899468",
		"claude-code-401508988-1105899468":  "claude-code-1105899468",
		// Codex: workspace-anchored — keep WS_HASH; scopeless + scoped fold.
		"codex-401508988-2992486099": "codex-401508988",
		"codex-401508988":            "codex-401508988",
		"codex-7b28":                 "codex-7b28",
	}
	for in, want := range cases {
		if got := ConversationID(in); got != want {
			t.Errorf("ConversationID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestConversationID_CodexTwinsFold(t *testing.T) {
	// The screenshot case: scopeless presence id and scoped session id for the
	// same codex app must share one conversation id.
	a := ConversationID("codex-1713039686")
	b := ConversationID("codex-1713039686-2004540290")
	if a != b {
		t.Fatalf("codex twins should fold: %q vs %q", a, b)
	}
}

func TestIsWorkspaceAnchored(t *testing.T) {
	if !IsWorkspaceAnchored("codex-1713039686-2004540290") {
		t.Error("codex should be workspace-anchored")
	}
	if IsWorkspaceAnchored("claude-code-552019522-2804496862") {
		t.Error("claude-code should not be workspace-anchored")
	}
}
