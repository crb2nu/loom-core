package audit

import (
	"fmt"
	"strings"
)

// stripChangelogFromDiff removes CHANGELOG.md file sections from a unified
// diff before it is audited, replacing each with a one-line omission marker.
//
// CHANGELOG.md is mechanical guardrail collateral (the docs-guardrail gate
// requires an entry on every code-facing MR), not audit signal — and after a
// union-merge cascade rescue the CHANGELOG hunk of a rebased branch carries
// sibling MRs' entries as apparent additions, which the rubric auditors flag
// as "massive scope creep" / "mega-commit" on otherwise clean merges
// (advisory issues #285, #302). Stripping the section removes the recurring
// false positive while the marker keeps the auditor aware a changelog change
// existed.
//
// Only the repo-root CHANGELOG.md is stripped; any nested path (e.g.
// docs/CHANGELOG.md) is real content and stays.
func stripChangelogFromDiff(diff string) string {
	if !strings.Contains(diff, "diff --git a/CHANGELOG.md b/CHANGELOG.md") {
		return diff
	}
	const header = "diff --git "
	var out []string
	lines := strings.Split(diff, "\n")
	skipping := false
	skipped := 0
	for _, line := range lines {
		if strings.HasPrefix(line, header) {
			if line == "diff --git a/CHANGELOG.md b/CHANGELOG.md" {
				skipping = true
				skipped = 0
				continue
			}
			if skipping {
				out = append(out, changelogOmissionMarker(skipped))
			}
			skipping = false
		}
		if skipping {
			skipped++
			continue
		}
		out = append(out, line)
	}
	if skipping {
		out = append(out, changelogOmissionMarker(skipped))
	}
	return strings.Join(out, "\n")
}

func changelogOmissionMarker(lines int) string {
	return fmt.Sprintf("[CHANGELOG.md diff omitted from audit (%d lines): changelog entries are docs-guardrail collateral, not audit scope]", lines)
}
