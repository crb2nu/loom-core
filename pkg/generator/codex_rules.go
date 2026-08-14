package generator

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/crb2nu/loom/pkg/registry"
)

// Codex execpolicy rules (~/.codex/rules/default.rules).
//
// Codex's workspace-write sandbox force-mounts .git (including the resolved
// gitdir behind worktree pointer files) read-only BY DESIGN, with no
// config.toml override (https://github.com/openai/codex/issues/15505), and
// our approval_policy = "never" removes the escalation prompt — so every git
// metadata write inside the sandbox fails with "cannot lock ref ... Operation
// not permitted" and autonomous shipping wedges at commit/push.
//
// The vendor-supported escape hatch is the execpolicy rules surface
// (https://learn.chatgpt.com/docs/agent-configuration/rules.md): Codex scans
// rules/ directories next to active config layers at startup, and a
// prefix_rule with decision = "allow" runs the matched command OUTSIDE the
// sandbox without prompting. Everything unmatched keeps the sandbox.
//
// The generator owns a marker-delimited block at the top of
// ~/.codex/rules/default.rules, rendered from
// platform_permissions.codex.settings.rules. Codex's TUI auto-appends
// approval rules to the SAME file, so sync must never overwrite it wholesale:
// pkg/sync merges the managed block in place and preserves everything outside
// the markers (same regen-survival requirement as the flightdeck hooks merge
// in pkg/sync/merge.go).

// CodexRulesBeginMarker and CodexRulesEndMarker delimit the loom-managed
// block inside ~/.codex/rules/default.rules. pkg/sync replaces exactly this
// span on regen; content outside it (user-authored rules, Codex TUI
// auto-appended approvals) is preserved. Changing either string orphans the
// previously-managed block in every synced home file — treat them as frozen.
const (
	CodexRulesBeginMarker = "# >>> loom-managed codex rules (loom sync codex --regen) — edits inside this block are overwritten"
	CodexRulesEndMarker   = "# <<< loom-managed codex rules — rules below this line (incl. Codex-appended approvals) are preserved"
)

// codexRulesFileRel is the generated file's path relative to the target
// output dir, matching its home-relative location under ~/.codex/.
//
// Deliberately NOT listed in the sync profile's ExtraGeneratedFiles: extras
// are plain-copied (which would clobber Codex-appended rules) and swept from
// workspace projects by CleanAllProjectsGenerated/cleanGeneratedAt (which
// would delete hand-authored repo-local .codex/rules/ files). pkg/sync
// handles this file through a dedicated marker-preserving merge instead.
const codexRulesFileRel = "rules/default.rules"

// codexExecRule is one resolved prefix_rule from the registry's
// platform_permissions.codex.settings.rules list.
type codexExecRule struct {
	// Pattern elements are either a literal string ("git") or a union of
	// literals (["view", "list"]), mirroring the vendor prefix_rule pattern
	// syntax.
	Pattern       []any
	Decision      string // allow | prompt | forbidden (vendor default: allow)
	Justification string
	Match         [][]string // example invocations that must match (execpolicy self-test)
	NotMatch      [][]string // example invocations that must NOT match
}

// buildCodexExecRules resolves platform_permissions.codex.settings.rules into
// renderable rules. Entries without a valid non-empty pattern are dropped with
// a warning — a malformed registry entry must not emit a rules file that
// codex's Starlark parser rejects (one bad file could disable the whole
// policy surface).
func buildCodexExecRules(pp *registry.PlatformPermission) []codexExecRule {
	if pp == nil || pp.Settings == nil {
		return nil
	}
	raw, ok := pp.Settings["rules"].([]any)
	if !ok || len(raw) == 0 {
		return nil
	}
	var rules []codexExecRule
	for i, entry := range raw {
		body, ok := entry.(map[string]any)
		if !ok {
			fmt.Fprintf(os.Stderr, "WARN  [codex] settings.rules[%d]: not a map, dropped\n", i)
			continue
		}
		rule := codexExecRule{Decision: "allow"}
		if pat, ok := body["pattern"].([]any); ok {
			for _, elem := range pat {
				switch v := elem.(type) {
				case string:
					rule.Pattern = append(rule.Pattern, v)
				case []any:
					var union []string
					for _, alt := range v {
						if s, ok := alt.(string); ok {
							union = append(union, s)
						}
					}
					if len(union) > 0 {
						rule.Pattern = append(rule.Pattern, union)
					}
				}
			}
		}
		if len(rule.Pattern) == 0 {
			fmt.Fprintf(os.Stderr, "WARN  [codex] settings.rules[%d]: empty or invalid pattern, dropped\n", i)
			continue
		}
		if v, ok := body["decision"].(string); ok && v != "" {
			rule.Decision = v
		}
		if v, ok := body["justification"].(string); ok {
			rule.Justification = v
		}
		rule.Match = stringMatrix(body["match"])
		rule.NotMatch = stringMatrix(body["not_match"])
		rules = append(rules, rule)
	}
	return rules
}

// stringMatrix coerces a YAML list-of-lists-of-strings ([]any of []any) into
// [][]string, dropping non-string elements.
func stringMatrix(v any) [][]string {
	rows, ok := v.([]any)
	if !ok {
		return nil
	}
	var out [][]string
	for _, row := range rows {
		cells, ok := row.([]any)
		if !ok {
			continue
		}
		var strs []string
		for _, c := range cells {
			if s, ok := c.(string); ok {
				strs = append(strs, s)
			}
		}
		if len(strs) > 0 {
			out = append(out, strs)
		}
	}
	return out
}

// renderCodexRulesBlock renders the loom-managed block for
// ~/.codex/rules/default.rules: begin marker, provenance/why header,
// prefix_rule stanzas, end marker.
func renderCodexRulesBlock(rules []codexExecRule) string {
	var sb strings.Builder
	sb.WriteString(CodexRulesBeginMarker + "\n")
	sb.WriteString("# Source: mcp/context/registry.yaml (platform_permissions.codex.settings.rules)\n")
	sb.WriteString("# Why: sandbox_mode=\"workspace-write\" force-mounts .git (and resolved worktree\n")
	sb.WriteString("# gitdirs) read-only with no override, and approval_policy=\"never\" removes the\n")
	sb.WriteString("# escalation path — git metadata writes fail with \"cannot lock ref ...\".\n")
	sb.WriteString("# \"allow\" runs the matched command OUTSIDE the sandbox without prompting;\n")
	sb.WriteString("# everything unmatched keeps the workspace-write sandbox.\n")
	sb.WriteString("# Docs: https://learn.chatgpt.com/docs/agent-configuration/rules.md\n")
	sb.WriteString("# Limitation: https://github.com/openai/codex/issues/15505\n")
	sb.WriteString("# Validate: codex execpolicy check --pretty --rules ~/.codex/rules/default.rules -- git commit -m msg\n")
	for _, rule := range rules {
		sb.WriteString("\n")
		sb.WriteString(renderCodexPrefixRule(rule))
	}
	sb.WriteString("\n" + CodexRulesEndMarker + "\n")
	return sb.String()
}

// renderCodexPrefixRule renders one prefix_rule(...) stanza in the vendor's
// Starlark syntax. %q's Go string quoting is escape-compatible with Starlark
// double-quoted strings.
func renderCodexPrefixRule(rule codexExecRule) string {
	var sb strings.Builder
	sb.WriteString("prefix_rule(\n")
	sb.WriteString("    pattern = " + codexPatternLiteral(rule.Pattern) + ",\n")
	fmt.Fprintf(&sb, "    decision = %q,\n", rule.Decision)
	if rule.Justification != "" {
		fmt.Fprintf(&sb, "    justification = %q,\n", rule.Justification)
	}
	writeMatrix := func(key string, rows [][]string) {
		if len(rows) == 0 {
			return
		}
		sb.WriteString("    " + key + " = [\n")
		for _, row := range rows {
			sb.WriteString("        " + codexStringListLiteral(row) + ",\n")
		}
		sb.WriteString("    ],\n")
	}
	writeMatrix("match", rule.Match)
	writeMatrix("not_match", rule.NotMatch)
	sb.WriteString(")\n")
	return sb.String()
}

// codexPatternLiteral renders a pattern list whose elements are strings or
// string unions, e.g. ["gh", ["view", "list"]].
func codexPatternLiteral(pattern []any) string {
	parts := make([]string, 0, len(pattern))
	for _, elem := range pattern {
		switch v := elem.(type) {
		case string:
			parts = append(parts, fmt.Sprintf("%q", v))
		case []string:
			parts = append(parts, codexStringListLiteral(v))
		}
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

func codexStringListLiteral(strs []string) string {
	parts := make([]string, 0, len(strs))
	for _, s := range strs {
		parts = append(parts, fmt.Sprintf("%q", s))
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// generateCodexRulesConfig writes the loom-managed execpolicy block to
// <outputDir>/<target>/rules/default.rules. No-op for non-codex targets and
// for a registry with no codex rules declared. Called from
// GenerateConfigsWithPath alongside generateCodexProfileConfigs; pkg/sync
// merges (never copies) this file into ~/.codex/rules/default.rules so
// Codex-appended approval rules outside the markers survive every regen.
func generateCodexRulesConfig(reg *registry.Registry, outputDir, target string, profile *PlatformProfile) error {
	// Only codex (the requires_preamble TOML platform) has the execpolicy surface.
	if profile == nil || !profile.Features.RequiresPreamble {
		return nil
	}
	rules := buildCodexExecRules(registryPlatformPerms(reg, target))
	if len(rules) == 0 {
		return nil
	}
	path := filepath.Join(outputDir, target, codexRulesFileRel)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(renderCodexRulesBlock(rules)), 0644); err != nil {
		return fmt.Errorf("write codex rules: %w", err)
	}
	return nil
}
