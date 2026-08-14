// merge_zed.go — Merge generated Zed context servers into a user-owned
// ~/.config/zed/settings.json.
//
// Zed configures MCP servers via the "context_servers" key in settings.json
// (flat {"command", "args", "env", "timeout"} stdio shape; remote servers use
// "url"/"headers"). See
// https://github.com/zed-industries/zed/blob/main/docs/src/ai/mcp.md and
// ContextServerCommand in crates/settings_content/src/project.rs.
//
// The file is user-owned and may contain comments (Zed settings are JSONC),
// so the merge uses hujson: only the loom-managed context server entries are
// patched, and comments/formatting elsewhere are preserved. Entries with
// other names, and every other top-level settings key, are never touched.
package sync

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/tailscale/hujson"
)

// zedContextServersKey is the Zed settings key holding MCP server entries.
const zedContextServersKey = "context_servers"

// MergeZedContextServers merges the generated context_servers fragment into
// the contents of a Zed settings.json (JSONC tolerated, comments preserved).
//
// Semantics:
//   - Entries named in the fragment are replaced with the canonical value.
//     This migrates legacy shapes in place (the pre-2026-07 hand-maintained
//     nested {"command": {"path", "arguments"}} shape no longer parses as a
//     stdio server in current Zed, and extension-shaped {"settings": ...}
//     entries belong to the dormant loom-zed extension).
//   - An explicit "enabled": false on an existing managed entry is carried
//     over so sync does not silently re-enable a server the user disabled.
//   - Foreign entries and all other top-level keys are untouched.
//   - If the existing file cannot be parsed even as JSONC, an error is
//     returned; the caller must never fall back to overwriting the file.
//
// Returns the merged bytes and whether they differ semantically from
// homeData. When changed is false, callers should skip the write to avoid
// reformatting churn.
func MergeZedContextServers(homeData, fragmentData []byte) ([]byte, bool, error) {
	servers, err := parseZedFragment(fragmentData)
	if err != nil {
		return nil, false, err
	}

	names := make([]string, 0, len(servers))
	for name := range servers {
		names = append(names, name)
	}
	sort.Strings(names)

	if len(bytes.TrimSpace(homeData)) == 0 {
		out, err := json.MarshalIndent(map[string]any{
			zedContextServersKey: rawMap(servers),
		}, "", "  ")
		if err != nil {
			return nil, false, err
		}
		return append(out, '\n'), true, nil
	}

	value, err := hujson.Parse(homeData)
	if err != nil {
		return nil, false, fmt.Errorf("parse existing Zed settings: %w (refusing to rewrite the file)", err)
	}

	existing, err := zedExistingContextServers(homeData)
	if err != nil {
		return nil, false, err
	}

	var ops []string
	if existing == nil {
		// No context_servers key yet — add the whole block.
		val, err := json.Marshal(rawMap(servers))
		if err != nil {
			return nil, false, err
		}
		ops = append(ops, fmt.Sprintf(`{"op":"add","path":%q,"value":%s}`, "/"+zedContextServersKey, val))
	} else {
		for _, name := range names {
			desired := servers[name]
			prior, exists := existing[name]
			if exists {
				desired = carryZedDisabledFlag(desired, prior)
				if jsonEqual(prior, desired) {
					continue
				}
			}
			op := "add"
			if exists {
				op = "replace"
			}
			path := "/" + zedContextServersKey + "/" + jsonPointerEscape(name)
			ops = append(ops, fmt.Sprintf(`{"op":%q,"path":%q,"value":%s}`, op, path, desired))
		}
	}

	if len(ops) == 0 {
		return homeData, false, nil
	}

	patch := "[" + strings.Join(ops, ",") + "]"
	if err := value.Patch([]byte(patch)); err != nil {
		return nil, false, fmt.Errorf("patch Zed settings: %w", err)
	}
	value.Format()
	return value.Pack(), true, nil
}

// parseZedFragment extracts the context_servers map from a generated
// fragment file ({"context_servers": {...}}, plus ignored metadata keys).
func parseZedFragment(fragmentData []byte) (map[string]json.RawMessage, error) {
	var fragment struct {
		ContextServers map[string]json.RawMessage `json:"context_servers"`
	}
	if err := json.Unmarshal(fragmentData, &fragment); err != nil {
		return nil, fmt.Errorf("parse generated context_servers fragment: %w", err)
	}
	if len(fragment.ContextServers) == 0 {
		return nil, fmt.Errorf("generated fragment has no %s entries", zedContextServersKey)
	}
	return fragment.ContextServers, nil
}

// zedExistingContextServers returns the current context_servers map from a
// JSONC settings blob, or nil if the key is absent.
func zedExistingContextServers(homeData []byte) (map[string]json.RawMessage, error) {
	// hujson.Standardize blanks comments in the input buffer IN PLACE; a
	// hujson.Value parsed from the same bytes aliases them, so pass a copy
	// to keep the caller's comments intact.
	std, err := hujson.Standardize(append([]byte(nil), homeData...))
	if err != nil {
		return nil, fmt.Errorf("standardize existing Zed settings: %w", err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(std, &top); err != nil {
		return nil, fmt.Errorf("parse existing Zed settings: %w", err)
	}
	raw, ok := top[zedContextServersKey]
	if !ok {
		return nil, nil
	}
	var servers map[string]json.RawMessage
	if err := json.Unmarshal(raw, &servers); err != nil {
		return nil, fmt.Errorf("parse existing %s: %w", zedContextServersKey, err)
	}
	if servers == nil {
		servers = map[string]json.RawMessage{}
	}
	return servers, nil
}

// carryZedDisabledFlag preserves an explicit "enabled": false from the prior
// entry on the desired replacement value.
func carryZedDisabledFlag(desired, prior json.RawMessage) json.RawMessage {
	var priorObj map[string]any
	if err := json.Unmarshal(prior, &priorObj); err != nil {
		return desired
	}
	enabled, ok := priorObj["enabled"].(bool)
	if !ok || enabled {
		return desired
	}
	var desiredObj map[string]any
	if err := json.Unmarshal(desired, &desiredObj); err != nil {
		return desired
	}
	desiredObj["enabled"] = false
	out, err := json.Marshal(desiredObj)
	if err != nil {
		return desired
	}
	return out
}

func rawMap(m map[string]json.RawMessage) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// jsonEqual compares two JSON values structurally.
func jsonEqual(a, b json.RawMessage) bool {
	var av, bv any
	if err := json.Unmarshal(a, &av); err != nil {
		return false
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		return false
	}
	aj, _ := json.Marshal(av)
	bj, _ := json.Marshal(bv)
	return bytes.Equal(aj, bj)
}

// jsonPointerEscape escapes a key for use in an RFC 6901 JSON pointer.
func jsonPointerEscape(s string) string {
	s = strings.ReplaceAll(s, "~", "~0")
	return strings.ReplaceAll(s, "/", "~1")
}
