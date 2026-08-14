package sync

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// ExtractHooksFromSettings parses a settings.json and returns the "hooks"
// value as raw JSON. Returns nil if the key is absent.
func ExtractHooksFromSettings(data []byte) (json.RawMessage, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse settings: %w", err)
	}
	hooks, ok := m["hooks"]
	if !ok {
		return nil, nil
	}
	return hooks, nil
}

// MergeHooksIntoSettings reads an existing settings.json at existingPath,
// replaces the "hooks" key with canonicalHooks, and preserves all other
// top-level keys (permissions, mcpServers, etc.).
//
// Returns:
//   - merged JSON bytes (indented)
//   - changed: true if the output differs from the existing content
//   - error on parse/write failure
//
// If the file is missing or empty, returns {"hooks": <canonicalHooks>}.
// If canonicalHooks is nil, the hooks key is removed from the output.
func MergeHooksIntoSettings(existingData []byte, canonicalHooks json.RawMessage) ([]byte, bool, error) {
	var m map[string]json.RawMessage

	if len(existingData) > 0 {
		if err := json.Unmarshal(existingData, &m); err != nil {
			// Existing file is invalid JSON -- start fresh
			m = make(map[string]json.RawMessage)
		}
	} else {
		m = make(map[string]json.RawMessage)
	}

	// Replace hooks
	if canonicalHooks != nil {
		m["hooks"] = canonicalHooks
	} else {
		delete(m, "hooks")
	}

	// Serialize with deterministic key ordering:
	// "hooks" first, "permissions" second, then alphabetical remainder.
	out, err := marshalOrderedSettings(m)
	if err != nil {
		return nil, false, fmt.Errorf("marshal settings: %w", err)
	}

	changed := !bytes.Equal(normalizeJSON(existingData), normalizeJSON(out))
	return out, changed, nil
}

// StripSettingsKeys removes selected top-level keys from a settings.json blob.
// Missing or invalid JSON is treated as an empty object.
func StripSettingsKeys(existingData []byte, keys ...string) ([]byte, bool, error) {
	var m map[string]json.RawMessage

	if len(existingData) > 0 {
		if err := json.Unmarshal(existingData, &m); err != nil {
			m = make(map[string]json.RawMessage)
		}
	} else {
		m = make(map[string]json.RawMessage)
	}

	for _, key := range keys {
		delete(m, key)
	}

	out, err := marshalOrderedSettings(m)
	if err != nil {
		return nil, false, fmt.Errorf("marshal settings: %w", err)
	}

	changed := !bytes.Equal(normalizeJSON(existingData), normalizeJSON(out))
	return out, changed, nil
}

// marshalOrderedSettings produces indented JSON with deterministic key order:
// hooks, permissions, experimental, general, tools, security, then remaining
// keys alphabetically.
func marshalOrderedSettings(m map[string]json.RawMessage) ([]byte, error) {
	// Collect keys in priority order
	priority := []string{"hooks", "permissions", "experimental", "general", "tools", "security"}
	var remaining []string
	seen := map[string]bool{}
	for _, k := range priority {
		if _, ok := m[k]; ok {
			seen[k] = true
		}
	}
	for k := range m {
		if !seen[k] {
			remaining = append(remaining, k)
		}
	}
	sort.Strings(remaining)

	var ordered []string
	for _, k := range priority {
		if seen[k] {
			ordered = append(ordered, k)
		}
	}
	ordered = append(ordered, remaining...)

	// Build JSON manually for key ordering
	var buf bytes.Buffer
	buf.WriteString("{\n")
	for i, key := range ordered {
		// Re-indent the value
		var indented bytes.Buffer
		if err := json.Indent(&indented, m[key], "  ", "  "); err != nil {
			// Fallback: use raw value
			indented.Reset()
			indented.Write(m[key])
		}
		fmt.Fprintf(&buf, "  %s: %s", jsonQuote(key), indented.String())
		if i < len(ordered)-1 {
			buf.WriteByte(',')
		}
		buf.WriteByte('\n')
	}
	buf.WriteString("}\n")
	return buf.Bytes(), nil
}

func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// MergeSettingsForHome merges repo settings into existing home settings.
// Non-hook keys (permissions, etc.) are taken from the repo copy.
//
// Hooks:
//   - repo copy has no hooks (sync without --regen; hooks were stripped to
//     avoid duplicate execution at multiple hierarchy levels): home hooks are
//     preserved as-is.
//   - repo copy has hooks (--regen): the canonical repo hooks win, but home
//     entries loom did not generate (e.g. flightdeck-capture, user-custom
//     hooks) are preserved per event instead of being wiped wholesale. See
//     mergeHooksPreservingForeign.
func MergeSettingsForHome(homeData, repoData []byte) ([]byte, bool, error) {
	var repoMap map[string]json.RawMessage
	if err := json.Unmarshal(repoData, &repoMap); err != nil {
		return nil, false, fmt.Errorf("parse repo settings: %w", err)
	}

	// Start with repo settings (latest permissions, etc.)
	result := repoMap

	repoHooks, hasRepoHooks := repoMap["hooks"]
	if len(homeData) > 0 {
		var homeMap map[string]json.RawMessage
		if err := json.Unmarshal(homeData, &homeMap); err == nil {
			if homeHooks, ok := homeMap["hooks"]; ok {
				if !hasRepoHooks {
					result["hooks"] = homeHooks
				} else if merged, err := mergeHooksPreservingForeign(repoHooks, homeHooks); err == nil {
					result["hooks"] = merged
				}
			}
		}
	}

	out, err := marshalOrderedSettings(result)
	if err != nil {
		return nil, false, fmt.Errorf("marshal settings: %w", err)
	}

	changed := !bytes.Equal(normalizeJSON(homeData), normalizeJSON(out))
	return out, changed, nil
}

// loomManagedHookMarkers identify hook commands the loom generator emits (or
// emitted in past versions). Home entries matching a marker are NOT preserved
// when the canonical hooks are regenerated: the current canonical block
// already contains their up-to-date variant, and carrying stale variants
// forward would duplicate execution on every regen.
//
// flightdeck-capture is deliberately NOT a marker: when the registry's
// flightdeck_capture gate is off, installer-merged capture hooks must survive
// regen (the 2026-06-10 incident). When the gate is on, the generated entry's
// command matches the installer's after home-prefix normalization, so the
// duplicate-command check below drops the installer copy instead.
var loomManagedHookMarkers = []string{
	"HOOK_SESSION_ID",                     // telemetry/lifecycle wrappers
	"loom agent ",                         // direct loom CLI invocations
	"loom-agent-hooks.log",                // telemetry event-emit sink
	".tool_input.file_path",               // formatter snippets
	".tool_input.command",                 // guardrail snippets
	".tool_input.new_string",              // content guardrail snippets
	"git rev-parse --is-inside-work-tree", // session-start nudges
}

// mergeHooksPreservingForeign merges a freshly generated canonical hooks block
// with the existing home hooks block. Canonical entries are authoritative;
// home entries are appended per event when they are foreign — i.e. their
// command neither appears in the canonical block (after home-directory
// normalization) nor matches a loom-managed marker.
func mergeHooksPreservingForeign(canonicalRaw, homeRaw json.RawMessage) (json.RawMessage, error) {
	var canonical map[string][]map[string]any
	if err := json.Unmarshal(canonicalRaw, &canonical); err != nil {
		return nil, fmt.Errorf("parse canonical hooks: %w", err)
	}
	var home map[string][]map[string]any
	if err := json.Unmarshal(homeRaw, &home); err != nil {
		return nil, fmt.Errorf("parse home hooks: %w", err)
	}

	homeDir, _ := os.UserHomeDir()
	canonicalCommands := map[string]bool{}
	for _, entries := range canonical {
		for _, cmd := range hookEntryCommands(entries) {
			canonicalCommands[normalizeHookCommand(cmd, homeDir)] = true
		}
	}

	events := make([]string, 0, len(home))
	for event := range home {
		events = append(events, event)
	}
	sort.Strings(events)
	for _, event := range events {
		for _, entry := range home[event] {
			cmds := hookEntryCommands([]map[string]any{entry})
			if len(cmds) == 0 {
				continue
			}
			foreign := true
			for _, cmd := range cmds {
				if canonicalCommands[normalizeHookCommand(cmd, homeDir)] || isLoomManagedHookCommand(cmd) {
					foreign = false
					break
				}
			}
			if foreign {
				canonical[event] = append(canonical[event], entry)
			}
		}
	}

	return json.Marshal(canonical)
}

// hookEntryCommands extracts every "command" string from the nested hooks
// arrays of the given entries.
func hookEntryCommands(entries []map[string]any) []string {
	var cmds []string
	for _, entry := range entries {
		inner, _ := entry["hooks"].([]any)
		for _, h := range inner {
			hm, _ := h.(map[string]any)
			if cmd, ok := hm["command"].(string); ok && cmd != "" {
				cmds = append(cmds, cmd)
			}
		}
	}
	return cmds
}

// normalizeHookCommand maps home-directory prefixes to ~ so a generated
// "~/.loom/..." command and an installer-written absolute
// "/Users/x/.loom/..." command compare equal.
func normalizeHookCommand(cmd, homeDir string) string {
	cmd = strings.TrimSpace(cmd)
	if homeDir != "" {
		cmd = strings.ReplaceAll(cmd, homeDir+"/", "~/")
	}
	return strings.ReplaceAll(cmd, "$HOME/", "~/")
}

// isLoomManagedHookCommand reports whether cmd looks like a loom-generated
// hook command (current or stale variant).
func isLoomManagedHookCommand(cmd string) bool {
	for _, marker := range loomManagedHookMarkers {
		if strings.Contains(cmd, marker) {
			return true
		}
	}
	return false
}

// StripHooksFromFile reads a settings.json file, removes the hooks key,
// and writes it back. Returns true if the file was modified.
func StripHooksFromFile(path string) (bool, error) {
	return StripSettingsKeysFromFile(path, "hooks")
}

// StripSettingsKeysFromFile reads a settings.json file, removes the provided
// top-level keys, and writes it back. Returns true if the file was modified.
func StripSettingsKeysFromFile(path string, keys ...string) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	stripped, changed, err := StripSettingsKeys(data, keys...)
	if err != nil {
		return false, err
	}
	if !changed {
		return false, nil
	}
	return true, writeFileAtomic(path, stripped, 0o644)
}

// normalizeJSON compacts JSON for comparison. Returns nil on error.
func normalizeJSON(data []byte) []byte {
	if len(data) == 0 {
		return nil
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, data); err != nil {
		return data // return as-is if not valid JSON
	}
	return buf.Bytes()
}
