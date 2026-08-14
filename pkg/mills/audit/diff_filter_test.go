package audit

import (
	"strings"
	"testing"
)

const changelogSection = `diff --git a/CHANGELOG.md b/CHANGELOG.md
index 05a54078..ae75503f 100644
--- a/CHANGELOG.md
+++ b/CHANGELOG.md
@@ -8,6 +8,8 @@
 ## [Unreleased]

 ### Added
+- **Unrelated sibling entry carried by a union-merge cascade** (huge scary text)
+- **This MR's actual entry**`

const codeSection = `diff --git a/pkg/mills/audit/followup.go b/pkg/mills/audit/followup.go
index 11111111..22222222 100644
--- a/pkg/mills/audit/followup.go
+++ b/pkg/mills/audit/followup.go
@@ -1,3 +1,4 @@
 package audit
+// real change`

func TestStripChangelogFromDiff_RemovesChangelogKeepsCode(t *testing.T) {
	diff := changelogSection + "\n" + codeSection + "\n"
	got := stripChangelogFromDiff(diff)
	if strings.Contains(got, "Unrelated sibling entry") {
		t.Fatalf("changelog content survived the strip:\n%s", got)
	}
	if !strings.Contains(got, "diff --git a/pkg/mills/audit/followup.go") {
		t.Fatalf("code section was lost:\n%s", got)
	}
	if !strings.Contains(got, "[CHANGELOG.md diff omitted from audit") {
		t.Fatalf("omission marker missing:\n%s", got)
	}
}

func TestStripChangelogFromDiff_ChangelogAtEnd(t *testing.T) {
	diff := codeSection + "\n" + changelogSection + "\n"
	got := stripChangelogFromDiff(diff)
	if strings.Contains(got, "Unrelated sibling entry") {
		t.Fatalf("changelog content survived the strip:\n%s", got)
	}
	if !strings.Contains(got, "[CHANGELOG.md diff omitted from audit") {
		t.Fatalf("omission marker missing:\n%s", got)
	}
}

func TestStripChangelogFromDiff_NoChangelogUnchanged(t *testing.T) {
	diff := codeSection + "\n"
	if got := stripChangelogFromDiff(diff); got != diff {
		t.Fatalf("diff without CHANGELOG was modified:\n%s", got)
	}
}

func TestStripChangelogFromDiff_NestedChangelogKept(t *testing.T) {
	nested := strings.ReplaceAll(changelogSection, "a/CHANGELOG.md b/CHANGELOG.md", "a/docs/CHANGELOG.md b/docs/CHANGELOG.md")
	if got := stripChangelogFromDiff(nested); got != nested {
		t.Fatalf("nested docs/CHANGELOG.md was stripped:\n%s", got)
	}
}

func TestStripChangelogFromDiff_ChangelogOnlyLeavesNoFileSections(t *testing.T) {
	got := stripChangelogFromDiff(changelogSection + "\n")
	if strings.Contains(got, "diff --git ") {
		t.Fatalf("changelog-only diff still has a file section:\n%s", got)
	}
}
