package hud

import (
	"strings"
	"unicode"
)

// spawnTaskLabelMax bounds the presence heartbeat's current_task so a spawn
// shows up on the fleet/Deck as a one-line label, not a prompt.
const spawnTaskLabelMax = 140

// compactTaskLabel reduces a spawn's task description (often a multi-section
// prompt: Mills implement prompts open with "WHAT THIS PIPELINE HAS ALREADY
// DONE FOR THIS ITEM (…)") to the first line that reads like a task summary.
// Heading-shaped lines are skipped: markdown headings, lines ending in a
// colon, and shouting section titles. Returns "" when nothing in the first
// few lines qualifies, so callers fall back to the branch — the Deck's agent
// rows already render "on <branch>" for an empty current_task.
func compactTaskLabel(desc string) string {
	const scan = 8
	seen := 0
	for _, raw := range strings.Split(desc, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		seen++
		if seen > scan {
			break
		}
		line = strings.TrimLeft(line, "#*-> ")
		line = strings.TrimSpace(line)
		if line == "" || looksLikeHeading(line) {
			continue
		}
		return truncateLabel(line, spawnTaskLabelMax)
	}
	return ""
}

// looksLikeHeading: ends with a colon, opens with three or more shouting
// words ("WHAT THIS PIPELINE …"), is mostly upper-case letters (a section
// title), or is a bare markdown/prompt delimiter.
func looksLikeHeading(line string) bool {
	if strings.HasSuffix(line, ":") {
		return true
	}
	// A one-word line ("Notes", "Task") is a section label, never a summary.
	if len(strings.Fields(line)) < 2 {
		return true
	}
	shouting := 0
	for _, w := range strings.Fields(line) {
		if len(w) >= 2 && strings.ToUpper(w) == w && strings.IndexFunc(w, unicode.IsLetter) >= 0 {
			shouting++
			if shouting >= 3 {
				return true
			}
			continue
		}
		break
	}
	letters, upper := 0, 0
	for _, r := range line {
		if unicode.IsLetter(r) {
			letters++
			if unicode.IsUpper(r) {
				upper++
			}
		}
	}
	if letters == 0 {
		return true
	}
	return letters >= 8 && upper*10 >= letters*7
}

func truncateLabel(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := strings.LastIndex(s[:n], " ")
	if cut < n/2 {
		cut = n
	}
	return strings.TrimRight(s[:cut], " ,;") + "…"
}
