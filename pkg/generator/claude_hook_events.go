package generator

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Claude Code SILENTLY DISABLES THE ENTIRE hooks config when settings.json
// contains even one unknown hook event name — zero events fire, with no error
// or warning anywhere (live probe against CLI 2.1.61 on 2026-06-09, documented
// in ~/workspace/.loom/60-product-spec-loom-flightdeck.md, "Risks"). A single
// typo'd event key in platform_profiles.yaml (session_end_event,
// heartbeat_event) or a generator regression would therefore kill session
// tracking in every synced repo. To prevent that, claudeHooks() validates all
// emitted event names against the set accepted by the locally installed CLI
// and strips unknown ones with a loud warning.
//
// The accepted set is version-dependent and the public docs run AHEAD of
// installed binaries: https://code.claude.com/docs/en/hooks listed 30 events
// on 2026-06-09 (including PermissionDenied), while the installed 2.1.61
// binary's enum accepts only the 18 below — PermissionDenied is NOT among
// them, and writing it disables all hooks. Validation therefore probes the
// installed binary for its own enum and only falls back to this pinned
// baseline when no binary is available (e.g. CI).
//
// claudeHookEventBaseline reproduces the event-name enum embedded in the
// Claude Code 2.1.61 binary, extracted with:
//
//	strings -a ~/.local/share/claude/versions/2.1.61 | \
//	  grep -oE '"PreToolUse"[,"A-Za-z]{20,400}'
var claudeHookEventBaseline = []string{
	"PreToolUse",
	"PostToolUse",
	"PostToolUseFailure",
	"Notification",
	"UserPromptSubmit",
	"SessionStart",
	"SessionEnd",
	"Stop",
	"SubagentStart",
	"SubagentStop",
	"PreCompact",
	"PermissionRequest",
	"Setup",
	"TeammateIdle",
	"TaskCompleted",
	"ConfigChange",
	"WorktreeCreate",
	"WorktreeRemove",
}

// claudeHookEventSanityCore is the subset every plausible Claude CLI enum must
// contain. A probe result missing any of these is treated as a parsing
// artifact and discarded in favor of the pinned baseline, so a probe bug can
// never strip the lifecycle hooks loom depends on.
var claudeHookEventSanityCore = []string{
	"PreToolUse", "PostToolUse", "SessionStart", "SessionEnd", "Stop",
}

var (
	claudeHookEventsOnce   sync.Once
	claudeHookEventsSet    map[string]bool
	claudeHookEventsSource string
)

// claudeAcceptedHookEvents returns the set of hook event names accepted by the
// locally installed Claude Code CLI, plus a human-readable description of how
// the set was obtained (for warning messages). When no claude binary can be
// found or probed, it returns the pinned 2.1.61 baseline. The result is
// computed once per process.
func claudeAcceptedHookEvents() (map[string]bool, string) {
	claudeHookEventsOnce.Do(func() {
		claudeHookEventsSet = make(map[string]bool, len(claudeHookEventBaseline))
		for _, e := range claudeHookEventBaseline {
			claudeHookEventsSet[e] = true
		}
		claudeHookEventsSource = "pinned baseline extracted from Claude Code 2.1.61 (claude binary not probeable)"

		path, err := exec.LookPath("claude")
		if err != nil {
			return
		}
		if resolved, err := filepath.EvalSymlinks(path); err == nil {
			path = resolved
		}
		probed := probeClaudeHookEvents(path)
		if !claudeProbeLooksSane(probed) {
			return
		}
		claudeHookEventsSet = probed
		version := claudeBinaryVersion()
		if version == "" {
			claudeHookEventsSource = "probed from installed claude binary"
		} else {
			claudeHookEventsSource = "probed from installed claude " + version
		}
	})
	return claudeHookEventsSet, claudeHookEventsSource
}

// claudeProbeLooksSane rejects probe results that are implausibly small or
// missing core lifecycle events, which would indicate the binary layout
// changed and the extraction heuristic broke.
func claudeProbeLooksSane(probed map[string]bool) bool {
	if len(probed) < 8 {
		return false
	}
	for _, core := range claudeHookEventSanityCore {
		if !probed[core] {
			return false
		}
	}
	return true
}

var claudeVersionRE = regexp.MustCompile(`\d+\.\d+\.\d+`)

// claudeBinaryVersion runs `claude --version` with a short timeout and
// extracts the semver portion (e.g. "2.1.61"). Returns "" on any failure —
// the version is only used to label warning messages.
func claudeBinaryVersion() string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "claude", "--version").Output()
	if err != nil {
		return ""
	}
	return claudeVersionRE.FindString(string(out))
}

// probeClaudeHookEvents scans the claude binary (a bundled JS executable) for
// the JSON array literal(s) enumerating valid hook event names — e.g.
// ["PreToolUse","PostToolUse",...] — and returns their union. The binary is
// large (~180MB for 2.1.61), so it is read in chunks with an overlap window
// rather than loaded whole. Returns nil when the file can't be read or no
// enum is found.
func probeClaudeHookEvents(binaryPath string) map[string]bool {
	f, err := os.Open(binaryPath)
	if err != nil {
		return nil
	}
	defer f.Close()

	const (
		chunkSize = 4 << 20 // 4MB read window
		overlap   = 8 << 10 // 8KB carry-over so literals spanning chunks survive
	)
	events := map[string]bool{}
	buf := make([]byte, chunkSize)
	carry := []byte{}
	for {
		n, err := io.ReadFull(f, buf)
		if n > 0 {
			window := append(carry, buf[:n]...)
			extractClaudeHookEventArrays(window, events)
			if len(window) > overlap {
				carry = append(carry[:0], window[len(window)-overlap:]...)
			} else {
				carry = append(carry[:0], window...)
			}
		}
		if err != nil {
			break
		}
	}
	if len(events) == 0 {
		return nil
	}
	return events
}

// extractClaudeHookEventArrays finds JSON string-array literals containing
// "PreToolUse" in data and unions their PascalCase entries into out. Anchoring
// on "PreToolUse" and expanding to the enclosing brackets is order-independent
// (a future CLI may reorder the enum) and far cheaper than running a regexp
// over the whole binary.
func extractClaudeHookEventArrays(data []byte, out map[string]bool) {
	anchor := []byte(`"PreToolUse"`)
	const maxSpan = 4096 // generous bound; the 2.1.61 enum literal is ~350 bytes
	for start := 0; ; {
		i := bytes.Index(data[start:], anchor)
		if i < 0 {
			return
		}
		i += start
		start = i + len(anchor)

		left := i
		for left > 0 && i-left < maxSpan && isClaudeHookArrayByte(data[left-1]) {
			left--
		}
		right := i + len(anchor)
		for right < len(data) && right-i < maxSpan && isClaudeHookArrayByte(data[right]) {
			right++
		}
		if left == 0 || data[left-1] != '[' || right >= len(data) || data[right] != ']' {
			continue
		}
		var names []string
		if err := json.Unmarshal(data[left-1:right+1], &names); err != nil {
			continue
		}
		for _, n := range names {
			if isClaudeHookEventName(n) {
				out[n] = true
			}
		}
	}
}

// isClaudeHookArrayByte reports whether b can appear inside a hook-event array
// literal between its brackets: quoted alphabetic names separated by commas.
func isClaudeHookArrayByte(b byte) bool {
	return b == '"' || b == ',' ||
		(b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z')
}

// isClaudeHookEventName reports whether s looks like a PascalCase hook event
// name. Filters out non-event strings that could share an array with the
// anchor in some future binary layout.
func isClaudeHookEventName(s string) bool {
	if len(s) < 3 || len(s) > 64 {
		return false
	}
	if s[0] < 'A' || s[0] > 'Z' {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		if (c < 'A' || c > 'Z') && (c < 'a' || c > 'z') {
			return false
		}
	}
	return true
}

// filterClaudeHookEvents deletes from hooks every event key not present in
// accepted and returns the sorted list of dropped names. The hooks map is the
// settings.json "hooks" block, whose keys are exclusively event names.
func filterClaudeHookEvents(hooks map[string]any, accepted map[string]bool) []string {
	var dropped []string
	for event := range hooks {
		if !accepted[event] {
			dropped = append(dropped, event)
			delete(hooks, event)
		}
	}
	sort.Strings(dropped)
	return dropped
}

// validateClaudeHookEvents strips hook events the installed Claude Code CLI
// does not recognize and returns the dropped names plus the provenance of the
// accepted set. Callers must surface dropped names loudly: one unknown event
// name silently disables ALL Claude Code hooks.
func validateClaudeHookEvents(hooks map[string]any) (dropped []string, source string) {
	accepted, source := claudeAcceptedHookEvents()
	return filterClaudeHookEvents(hooks, accepted), source
}

// claudeHookEventWarning formats the stderr warning for dropped hook events.
func claudeHookEventWarning(dropped []string, source string) string {
	return "WARN  [claude] dropping hook events not accepted by the installed Claude Code CLI (" +
		source + "): " + strings.Join(dropped, ", ") +
		" — a single unknown event name silently disables ALL Claude Code hooks\n"
}
