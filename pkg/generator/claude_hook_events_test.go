package generator

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// claude2161EnumLiteral is the exact event-name enum literal embedded in the
// Claude Code 2.1.61 binary (see claudeHookEventBaseline's extraction recipe).
const claude2161EnumLiteral = `["PreToolUse","PostToolUse","PostToolUseFailure","Notification","UserPromptSubmit","SessionStart","SessionEnd","Stop","SubagentStart","SubagentStop","PreCompact","PermissionRequest","Setup","TeammateIdle","TaskCompleted","ConfigChange","WorktreeCreate","WorktreeRemove"]`

func baselineSet() map[string]bool {
	set := make(map[string]bool, len(claudeHookEventBaseline))
	for _, e := range claudeHookEventBaseline {
		set[e] = true
	}
	return set
}

// claudeDocsOnlyEvents are the hook events documented at
// code.claude.com/docs/en/hooks on 2026-08-07 that are absent from the 2.1.61
// binary enum. Newer CLIs accept them; the fallback baseline must include them
// so hooks configured on these events survive validation when no binary is
// probeable.
var claudeDocsOnlyEvents = []string{
	"UserPromptExpansion",
	"PermissionDenied",
	"PostToolBatch",
	"MessageDisplay",
	"TaskCreated",
	"StopFailure",
	"InstructionsLoaded",
	"CwdChanged",
	"DirectoryAdded",
	"FileChanged",
	"PostCompact",
	"Elicitation",
	"ElicitationResult",
}

// TestClaudeHookEventBaseline_BinaryEnumPlusDocs pins the baseline to exactly
// the union of the enum extracted from the 2.1.61 binary and the docs-only
// events verified against https://code.claude.com/docs/en/hooks on 2026-08-07
// (31 total).
func TestClaudeHookEventBaseline_BinaryEnumPlusDocs(t *testing.T) {
	extracted := map[string]bool{}
	extractClaudeHookEventArrays([]byte(claude2161EnumLiteral), extracted)
	if len(extracted) != 18 {
		t.Fatalf("extracted %d events from 2.1.61 enum literal, want 18: %v", len(extracted), extracted)
	}
	want := map[string]bool{}
	for e := range extracted {
		want[e] = true
	}
	for _, e := range claudeDocsOnlyEvents {
		if want[e] {
			t.Errorf("claudeDocsOnlyEvents entry %q is already in the 2.1.61 binary enum", e)
		}
		want[e] = true
	}
	base := baselineSet()
	if len(base) != 31 || len(want) != 31 {
		t.Fatalf("baseline has %d events, docs+binary union has %d, want 31", len(base), len(want))
	}
	for e := range want {
		if !base[e] {
			t.Errorf("event %q missing from claudeHookEventBaseline", e)
		}
	}
}

// TestClaudeHooks_EmittedEventsAreAccepted is the core regression test: every
// hook event key the generator emits for the real Claude profile must be in
// the pinned baseline. A typo in platform_profiles.yaml (session_end_event,
// heartbeat_event) or a Go-side regression that introduces an unknown event
// name fails here instead of silently killing every synced repo's hooks after
// `loom sync claude --regen`.
func TestClaudeHooks_EmittedEventsAreAccepted(t *testing.T) {
	profile, err := GetPlatformProfile("claude")
	if err != nil {
		t.Fatalf("GetPlatformProfile(claude): %v", err)
	}
	hooks := claudeHooks(testRegistry(), profile, "")
	if len(hooks) == 0 {
		t.Fatal("claudeHooks returned no events")
	}
	base := baselineSet()
	for event := range hooks {
		if !base[event] {
			t.Errorf("generator emits hook event %q which is not in the pinned baseline — this would silently disable ALL hooks", event)
		}
	}
	// The lifecycle events session tracking depends on must survive validation.
	for _, want := range []string{"SessionStart", "SessionEnd", "PostToolUse", "PreToolUse", "SubagentStart"} {
		if _, ok := hooks[want]; !ok {
			t.Errorf("expected hook event %q missing from claudeHooks output", want)
		}
	}
}

func TestFilterClaudeHookEvents_DropsUnknownKeepsKnown(t *testing.T) {
	hooks := map[string]any{
		"SessionStart":      []map[string]any{{}},
		"SessionEnd":        []map[string]any{{}},
		"PermissionsDenied": []map[string]any{{}}, // misspelling of PermissionDenied
		"sessionEnd":        []map[string]any{{}}, // wrong case = unknown
	}
	dropped := filterClaudeHookEvents(hooks, baselineSet())
	if got, want := len(dropped), 2; got != want {
		t.Fatalf("dropped %d events (%v), want %d", got, dropped, want)
	}
	if dropped[0] != "PermissionsDenied" || dropped[1] != "sessionEnd" {
		t.Errorf("dropped = %v, want [PermissionsDenied sessionEnd]", dropped)
	}
	if _, ok := hooks["SessionStart"]; !ok {
		t.Error("SessionStart was wrongly removed")
	}
	if _, ok := hooks["SessionEnd"]; !ok {
		t.Error("SessionEnd was wrongly removed")
	}
	if _, ok := hooks["PermissionsDenied"]; ok {
		t.Error("PermissionsDenied survived filtering")
	}
}

// TestBuildPlatformHooks_TypoSessionEndEvent_Stripped simulates the YAML-typo
// failure mode end to end: a profile whose session_end_event is not a real
// Claude event must have that key stripped while lifecycle hooks survive.
func TestBuildPlatformHooks_TypoSessionEndEvent_Stripped(t *testing.T) {
	hp := HookProfile{
		Enabled:         true,
		Events:          []string{"sessionStart", "sessionEnd", "preToolUse", "postToolUse"},
		AgentID:         "claude-code",
		AgentType:       "claude-code",
		Description:     "Claude Code session",
		SessionEndEvent: "SesionEnd", // the real-world typo class
	}
	hooks := buildPlatformHooks(testRegistry(), hp, "")
	if _, ok := hooks["SesionEnd"]; !ok {
		t.Fatal("test setup: expected SesionEnd slot before filtering")
	}
	dropped := filterClaudeHookEvents(hooks, baselineSet())
	if len(dropped) != 1 || dropped[0] != "SesionEnd" {
		t.Errorf("dropped = %v, want [SesionEnd]", dropped)
	}
	if _, ok := hooks["SessionStart"]; !ok {
		t.Error("SessionStart must survive filtering")
	}
}

// TestProbeClaudeHookEvents_ExtractsEnumFromBinary exercises the chunked
// binary scan against a synthetic file: 5MB of junk (forcing the literal past
// the first 4MB read window) with the real 2.1.61 enum bytes embedded.
func TestProbeClaudeHookEvents_ExtractsEnumFromBinary(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fake-claude")
	junk := bytes.Repeat([]byte{0xCC, 'x'}, (5<<20)/2)
	blob := append(junk, []byte(`var Zk1=`+claude2161EnumLiteral+`;`)...)
	blob = append(blob, junk[:1024]...)
	if err := os.WriteFile(path, blob, 0o755); err != nil {
		t.Fatal(err)
	}
	events := probeClaudeHookEvents(path)
	if len(events) != 18 {
		t.Fatalf("probed %d events, want 18: %v", len(events), events)
	}
	want := map[string]bool{}
	extractClaudeHookEventArrays([]byte(claude2161EnumLiteral), want)
	for e := range want {
		if !events[e] {
			t.Errorf("probe missed event %q", e)
		}
	}
	if !claudeProbeLooksSane(events) {
		t.Error("probe of full enum should pass the sanity gate")
	}
}

func TestProbeClaudeHookEvents_NoEnumReturnsNil(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "not-claude")
	if err := os.WriteFile(path, []byte("no enum here"), 0o644); err != nil {
		t.Fatal(err)
	}
	if events := probeClaudeHookEvents(path); events != nil {
		t.Errorf("expected nil for binary without enum, got %v", events)
	}
}

func TestClaudeProbeLooksSane_RejectsPartialExtraction(t *testing.T) {
	partial := map[string]bool{"PreToolUse": true, "PostToolUse": true}
	if claudeProbeLooksSane(partial) {
		t.Error("tiny probe result must fail the sanity gate")
	}
	missingCore := baselineSet()
	delete(missingCore, "SessionEnd")
	if claudeProbeLooksSane(missingCore) {
		t.Error("probe result missing a core lifecycle event must fail the sanity gate")
	}
}

// TestExtractClaudeHookEventArrays_OrderIndependent guards the anchor-and-
// expand heuristic against a future CLI reordering the enum so PreToolUse is
// no longer first.
func TestExtractClaudeHookEventArrays_OrderIndependent(t *testing.T) {
	literal := `["SessionStart","PreToolUse","PostToolUse","SessionEnd","Stop","Notification","PreCompact","SubagentStop","UserPromptSubmit"]`
	out := map[string]bool{}
	extractClaudeHookEventArrays([]byte("junk"+literal+"junk"), out)
	if len(out) != 9 {
		t.Fatalf("extracted %d events, want 9: %v", len(out), out)
	}
	if !out["SessionStart"] || !out["UserPromptSubmit"] {
		t.Errorf("boundary elements missing: %v", out)
	}
}
