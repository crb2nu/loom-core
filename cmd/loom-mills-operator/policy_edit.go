package main

// policy_edit.go -- anchored text edits to the gitops Mills policy ConfigMap
// for repo onboarding (POST /api/mills/projects/policy-mr).
//
// The policy file is a heavily commented YAML document whose comments ARE the
// audit trail (every entry says when and why it landed). A YAML round-trip
// would destroy them, so — like the kill-switch committer — edits are
// line-anchored text insertions that leave every other byte intact:
//
//   - append an item to a YAML sequence (cross_repo.demand_projects,
//     intake.gitlab.projects) after its last existing item
//   - append a mapping entry to a YAML map (pipeline.protected_paths_per_repo,
//     pipeline.per_repo_overrides) after its last existing entry
//   - bump the deployment's policy-checksum annotation to the sha256 of the
//     new ConfigMap text (fsnotify misses ConfigMap ..data swaps, so the
//     operator only re-reads snapshot-at-start keys on a pod Recreate)
//
// Every edit is idempotent: an entry already present is reported as skipped,
// never duplicated. A missing anchor is an error (fail closed) rather than a
// guess at where the block should go.

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// onboardPolicyEdit is one repo's onboarding delta.
type onboardPolicyEdit struct {
	Project        string
	IntakeIssues   bool
	ProtectedPaths []string
	MaxUSDPerRun   float64
	MaxRunsPerDay  int
	// Note is the one-line provenance comment written above each inserted
	// entry (date + origin), e.g. "2026-09-04: onboarded from the HUD".
	Note string
}

// policyEditResult carries the edited text plus a ledger of what changed.
type policyEditResult struct {
	Content string
	Changed []string
	Skipped []string
}

var errPolicyAnchor = errors.New("policy edit: anchor not found")

// applyOnboardPolicyEdit applies every requested insertion to content.
func applyOnboardPolicyEdit(content string, e onboardPolicyEdit) (policyEditResult, error) {
	project := strings.Trim(strings.TrimSpace(e.Project), "/")
	if project == "" {
		return policyEditResult{}, errors.New("policy edit: project required")
	}
	res := policyEditResult{}
	lines := strings.Split(content, "\n")

	var err error
	var did bool
	if lines, did, err = appendSequenceItem(lines, "demand_projects", -1, project, e.Note); err != nil {
		return res, fmt.Errorf("cross_repo.demand_projects: %w", err)
	}
	res.record("cross_repo.demand_projects", did)

	if e.IntakeIssues {
		anchor := findKeyLine(lines, "eligible_label", 0)
		if anchor < 0 {
			return res, fmt.Errorf("intake.gitlab.projects: %w (eligible_label)", errPolicyAnchor)
		}
		if lines, did, err = appendSequenceItem(lines, "projects", anchor, project, e.Note); err != nil {
			return res, fmt.Errorf("intake.gitlab.projects: %w", err)
		}
		res.record("intake.gitlab.projects", did)
	}

	if len(e.ProtectedPaths) > 0 {
		body := make([]string, 0, len(e.ProtectedPaths))
		for _, p := range e.ProtectedPaths {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			body = append(body, fmt.Sprintf("- %s", yamlQuote(p)))
		}
		if lines, did, err = appendMapEntry(lines, "protected_paths_per_repo", project, body, e.Note); err != nil {
			return res, fmt.Errorf("pipeline.protected_paths_per_repo: %w", err)
		}
		res.record("pipeline.protected_paths_per_repo", did)
	}

	if e.MaxUSDPerRun > 0 || e.MaxRunsPerDay > 0 {
		var body []string
		if e.MaxUSDPerRun > 0 {
			body = append(body, fmt.Sprintf("max_usd_per_run: %s", trimFloat(e.MaxUSDPerRun)))
		}
		if e.MaxRunsPerDay > 0 {
			body = append(body, fmt.Sprintf("max_runs_per_day: %d", e.MaxRunsPerDay))
		}
		if lines, did, err = appendMapEntry(lines, "per_repo_overrides", project, body, e.Note); err != nil {
			return res, fmt.Errorf("pipeline.per_repo_overrides: %w", err)
		}
		res.record("pipeline.per_repo_overrides", did)
	}

	res.Content = strings.Join(lines, "\n")
	return res, nil
}

func (r *policyEditResult) record(key string, changed bool) {
	if changed {
		r.Changed = append(r.Changed, key)
	} else {
		r.Skipped = append(r.Skipped, key)
	}
}

// findKeyLine returns the index of the first line at or after `from` that
// opens `key:` — bare (a block follows), with an inline scalar/flow value, or
// with a trailing comment — or -1. Callers that need a block reject inline
// flow values (`{}` / `[]`) themselves.
func findKeyLine(lines []string, key string, from int) int {
	if from < 0 {
		from = 0
	}
	re := regexp.MustCompile(`^\s*` + regexp.QuoteMeta(key) + `:(\s.*)?$`)
	for i := from; i < len(lines); i++ {
		if re.MatchString(lines[i]) {
			return i
		}
	}
	return -1
}

func indentOf(s string) int {
	return len(s) - len(strings.TrimLeft(s, " "))
}

func isBlankOrComment(s string) bool {
	t := strings.TrimSpace(s)
	return t == "" || strings.HasPrefix(t, "#")
}

// blockEnd returns the index of the first line after keyIdx that terminates
// the block opened by the key line: a non-blank, non-comment line indented
// no deeper than the key. Trailing blank/comment lines at the end of the
// block are NOT part of it (they usually introduce the next key).
// lastBody is the index of the last non-blank, non-comment line inside the
// block (keyIdx when the block is empty).
func blockEnd(lines []string, keyIdx int) (end, lastBody int) {
	keyIndent := indentOf(lines[keyIdx])
	lastBody = keyIdx
	for i := keyIdx + 1; i < len(lines); i++ {
		if isBlankOrComment(lines[i]) {
			continue
		}
		if indentOf(lines[i]) <= keyIndent {
			return i, lastBody
		}
		lastBody = i
	}
	return len(lines), lastBody
}

var seqItemRe = regexp.MustCompile(`^\s*-\s+(.*?)\s*(#.*)?$`)

// appendSequenceItem appends `- "value"` to the sequence under the first
// `key:` line at or after `after`. Returns changed=false when the value is
// already an item. The item goes right after the last existing item so any
// trailing comment that introduces the next key stays where it was.
func appendSequenceItem(lines []string, key string, after int, value, note string) ([]string, bool, error) {
	keyIdx := findKeyLine(lines, key, after)
	if keyIdx < 0 {
		return lines, false, fmt.Errorf("%w: %s", errPolicyAnchor, key)
	}
	// An inline flow sequence (`key: []` / `key: [a, b]`) cannot take items
	// by line insertion.
	if strings.Contains(lines[keyIdx], "[") {
		return lines, false, fmt.Errorf("%s is an inline sequence; expand it to a block first", key)
	}
	_, lastBody := blockEnd(lines, keyIdx)
	// Presence check over the block's items.
	for i := keyIdx + 1; i <= lastBody; i++ {
		m := seqItemRe.FindStringSubmatch(lines[i])
		if m == nil {
			continue
		}
		if yamlUnquote(m[1]) == value {
			return lines, false, nil
		}
	}
	itemIndent := indentOf(lines[keyIdx]) + 2
	if lastBody > keyIdx {
		itemIndent = indentOf(lines[lastBody])
	}
	pad := strings.Repeat(" ", itemIndent)
	ins := []string{}
	if strings.TrimSpace(note) != "" {
		ins = append(ins, pad+"# "+strings.TrimSpace(note))
	}
	ins = append(ins, pad+"- "+yamlQuote(value))
	return insertAfter(lines, lastBody, ins), true, nil
}

var mapKeyRe = regexp.MustCompile(`^\s*("?)([^"#:][^:"]*)("?):\s*(#.*)?$`)

// appendMapEntry appends `"entry":` + body lines (each indented one level
// deeper) to the mapping under the first `key:` line. Returns changed=false
// when the entry key already exists in the block.
func appendMapEntry(lines []string, key, entry string, body []string, note string) ([]string, bool, error) {
	keyIdx := findKeyLine(lines, key, 0)
	if keyIdx < 0 {
		return lines, false, fmt.Errorf("%w: %s", errPolicyAnchor, key)
	}
	// An empty map rendered as `key: {}` cannot take entries by insertion.
	if strings.Contains(lines[keyIdx], "{}") {
		return lines, false, fmt.Errorf("%s is an inline empty map ({}); expand it to a block first", key)
	}
	keyIndent := indentOf(lines[keyIdx])
	end, lastBody := blockEnd(lines, keyIdx)
	// The block's entry indent is learned from its first entry; the first
	// non-blank line after the key is by construction an entry key.
	entryIndent := -1
	for i := keyIdx + 1; i < end; i++ {
		if isBlankOrComment(lines[i]) {
			continue
		}
		ind := indentOf(lines[i])
		if entryIndent < 0 {
			entryIndent = ind
		}
		if ind != entryIndent {
			continue
		}
		if m := mapKeyRe.FindStringSubmatch(lines[i]); m != nil && strings.TrimSpace(m[2]) == entry {
			return lines, false, nil
		}
	}
	if entryIndent < 0 {
		entryIndent = keyIndent + 2
	}
	pad := strings.Repeat(" ", entryIndent)
	inner := strings.Repeat(" ", entryIndent+2)
	ins := []string{}
	if strings.TrimSpace(note) != "" {
		ins = append(ins, pad+"# "+strings.TrimSpace(note))
	}
	ins = append(ins, pad+yamlQuote(entry)+":")
	for _, b := range body {
		ins = append(ins, inner+b)
	}
	return insertAfter(lines, lastBody, ins), true, nil
}

func insertAfter(lines []string, idx int, ins []string) []string {
	out := make([]string, 0, len(lines)+len(ins))
	out = append(out, lines[:idx+1]...)
	out = append(out, ins...)
	out = append(out, lines[idx+1:]...)
	return out
}

func yamlQuote(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
}

func yamlUnquote(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && ((s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'')) {
		return s[1 : len(s)-1]
	}
	return s
}

func trimFloat(f float64) string {
	s := fmt.Sprintf("%.4f", f)
	s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	if s == "" {
		return "0"
	}
	return s
}

// policyChecksumLine matches the deployment annotation the operator's
// Recreate rides on. The value is the sha256 of the whole ConfigMap file.
var policyChecksumLine = regexp.MustCompile(`(?m)^(\s*loom\.flexinfer\.ai/policy-checksum:\s*)([0-9a-fA-F]{64})\s*$`)

// bumpPolicyChecksum rewrites the deployment's policy-checksum annotation to
// the sha256 of policyContent. Returns the new deployment text and the
// checksum. Errors when the annotation line is absent (fail closed).
func bumpPolicyChecksum(deployment, policyContent string) (string, string, error) {
	sum := sha256.Sum256([]byte(policyContent))
	hexsum := hex.EncodeToString(sum[:])
	m := policyChecksumLine.FindStringSubmatch(deployment)
	if m == nil {
		return "", "", errors.New("deployment: loom.flexinfer.ai/policy-checksum annotation not found")
	}
	if m[2] == hexsum {
		return deployment, hexsum, nil
	}
	return strings.Replace(deployment, m[0], m[1]+hexsum, 1), hexsum, nil
}
