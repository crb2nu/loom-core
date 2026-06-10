package sync

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func hookEntryJSON(command, matcher string) string {
	entry := map[string]any{
		"hooks": []map[string]any{{"type": "command", "command": command}},
	}
	if matcher != "" {
		entry["matcher"] = matcher
	}
	b, _ := json.Marshal(entry)
	return string(b)
}

// Regression for the 2026-06-10 flightdeck wipe: `loom sync <platform>
// --regen` produces a repo settings.json WITH hooks, and the home merge used
// to replace the home hooks block wholesale — removing every entry a foreign
// installer (flightdeck-capture install) had merged in.
func TestMergeSettingsForHome_ForeignHookSurvivesRegen(t *testing.T) {
	home := []byte(`{
		"hooks": {
			"Stop": [` + hookEntryJSON("/Users/someone/.local/bin/flightdeck-capture", "") + `],
			"PostToolUse": [` + hookEntryJSON("my-custom-audit.sh", "*") + `],
			"SessionStart": [` + hookEntryJSON(`INPUT=$(cat); HOOK_SESSION_ID=stale-loom-variant`, "") + `]
		},
		"model": "opus"
	}`)
	repo := []byte(`{
		"hooks": {
			"SessionStart": [` + hookEntryJSON(`INPUT=$(cat); HOOK_SESSION_ID=current-loom-variant`, "") + `]
		},
		"permissions": {"allow": ["Bash"]}
	}`)

	merged, _, err := MergeSettingsForHome(home, repo)
	if err != nil {
		t.Fatalf("MergeSettingsForHome: %v", err)
	}

	var out struct {
		Hooks map[string][]map[string]any `json:"hooks"`
	}
	if err := json.Unmarshal(merged, &out); err != nil {
		t.Fatalf("parse merged: %v", err)
	}

	// Foreign entries survive under their events.
	if cmds := hookEntryCommands(out.Hooks["Stop"]); len(cmds) != 1 || !strings.Contains(cmds[0], "flightdeck-capture") {
		t.Errorf("Stop foreign flightdeck entry lost: %v", cmds)
	}
	ptu := out.Hooks["PostToolUse"]
	if cmds := hookEntryCommands(ptu); len(cmds) != 1 || cmds[0] != "my-custom-audit.sh" {
		t.Errorf("PostToolUse foreign entry lost: %v", cmds)
	}
	if len(ptu) == 1 {
		if m, _ := ptu[0]["matcher"].(string); m != "*" {
			t.Errorf("foreign entry matcher not preserved: %v", ptu[0]["matcher"])
		}
	}

	// Canonical loom entry wins; the stale loom-managed home variant is dropped.
	ssCmds := hookEntryCommands(out.Hooks["SessionStart"])
	if len(ssCmds) != 1 || !strings.Contains(ssCmds[0], "current-loom-variant") {
		t.Errorf("SessionStart = %v, want only the regenerated loom entry", ssCmds)
	}
}

// When the registry's flightdeck_capture gate is on, the generated hooks
// contain ~/.loom/flightdeck/bin/flightdeck-capture. A pre-existing
// installer-written entry uses the ABSOLUTE home path; it must dedupe against
// the generated entry, not double-fire every event.
func TestMergeSettingsForHome_FlightdeckAbsolutePathDedupes(t *testing.T) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home dir: %v", err)
	}
	abs := homeDir + "/.loom/flightdeck/bin/flightdeck-capture"
	home := []byte(`{"hooks": {"Stop": [` + hookEntryJSON(abs, "") + `]}}`)
	repo := []byte(`{"hooks": {"Stop": [` + hookEntryJSON("~/.loom/flightdeck/bin/flightdeck-capture", "") + `]}}`)

	merged, _, err := MergeSettingsForHome(home, repo)
	if err != nil {
		t.Fatalf("MergeSettingsForHome: %v", err)
	}
	var out struct {
		Hooks map[string][]map[string]any `json:"hooks"`
	}
	if err := json.Unmarshal(merged, &out); err != nil {
		t.Fatalf("parse merged: %v", err)
	}
	cmds := hookEntryCommands(out.Hooks["Stop"])
	if len(cmds) != 1 {
		t.Fatalf("Stop has %d flightdeck entries, want 1 (deduped): %v", len(cmds), cmds)
	}
	if cmds[0] != "~/.loom/flightdeck/bin/flightdeck-capture" {
		t.Errorf("surviving entry = %q, want the generated canonical one", cmds[0])
	}
}

// Re-running the merge on its own output must be a no-op, so repeated regen
// cycles never accumulate duplicate entries.
func TestMergeSettingsForHome_ForeignMergeIdempotent(t *testing.T) {
	home := []byte(`{"hooks": {
		"Stop": [` + hookEntryJSON("/opt/foreign/capture-tool", "") + `],
		"SessionStart": [` + hookEntryJSON("another-foreign.sh", "") + `]
	}}`)
	repo := []byte(`{"hooks": {
		"SessionStart": [` + hookEntryJSON(`INPUT=$(cat); HOOK_SESSION_ID=loom`, "") + `]
	}}`)

	first, _, err := MergeSettingsForHome(home, repo)
	if err != nil {
		t.Fatalf("first merge: %v", err)
	}
	second, changed, err := MergeSettingsForHome(first, repo)
	if err != nil {
		t.Fatalf("second merge: %v", err)
	}
	if changed {
		t.Error("second merge reported changed=true, want idempotent")
	}
	if !bytes.Equal(first, second) {
		t.Errorf("second merge output differs from first:\nfirst:  %s\nsecond: %s", first, second)
	}
}
