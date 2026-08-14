package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFrag creates a fragment file in dir.
func writeFrag(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func TestLoadFragments_Valid(t *testing.T) {
	dir := t.TempDir()
	writeFrag(t, dir, "README.md", "# convention doc, not an entry")
	writeFrag(t, dir, ".gitkeep", "")
	writeFrag(t, dir, "feat-auth.added.md", "- **Auth**: new login flow.\n")
	writeFrag(t, dir, "bug-timeout.fixed.md", "- Fixed a timeout.\n")

	frags, errs := loadFragments(dir)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(frags) != 2 {
		t.Fatalf("want 2 fragments, got %d: %+v", len(frags), frags)
	}
	// Sorted by basename: bug-timeout before feat-auth.
	if frags[0].Base != "bug-timeout.fixed.md" || frags[1].Base != "feat-auth.added.md" {
		t.Errorf("fragments not sorted by basename: %s, %s", frags[0].Base, frags[1].Base)
	}
	if frags[0].Category != "fixed" || frags[0].Slug != "bug-timeout" {
		t.Errorf("bad parse: %+v", frags[0])
	}
	if frags[1].Body != "- **Auth**: new login flow." {
		t.Errorf("body not trimmed: %q", frags[1].Body)
	}
}

func TestLoadFragments_MissingDirIsClean(t *testing.T) {
	frags, errs := loadFragments(filepath.Join(t.TempDir(), "does-not-exist"))
	if len(errs) != 0 || len(frags) != 0 {
		t.Fatalf("missing dir should be clean: frags=%v errs=%v", frags, errs)
	}
}

func TestLoadFragments_BadNameEmptyBodyDupSlug(t *testing.T) {
	dir := t.TempDir()
	writeFrag(t, dir, "no-category.md", "- body")    // missing category
	writeFrag(t, dir, "bad.unknowncat.md", "- body") // unknown category
	writeFrag(t, dir, "empty.added.md", "   \n\n")   // empty body
	writeFrag(t, dir, "dup.added.md", "- one")       // slug "dup"
	writeFrag(t, dir, "dup.fixed.md", "- two")       // slug "dup" again

	_, errs := loadFragments(dir)
	joined := ""
	for _, e := range errs {
		joined += e.Error() + "\n"
	}
	for _, want := range []string{
		"no-category.md: filename must be",
		"bad.unknowncat.md: filename must be",
		"empty.added.md: fragment body is empty",
		"duplicate slug \"dup\"",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing error %q in:\n%s", want, joined)
		}
	}
}

func TestLoadFragments_DottedSlug(t *testing.T) {
	dir := t.TempDir()
	writeFrag(t, dir, "2026-07-17.topic.security.md", "- Patched CVE.")
	frags, errs := loadFragments(dir)
	if len(errs) != 0 {
		t.Fatalf("errs: %v", errs)
	}
	if len(frags) != 1 || frags[0].Category != "security" || frags[0].Slug != "2026-07-17.topic" {
		t.Fatalf("dotted slug misparsed: %+v", frags)
	}
}

// baseChangelog mirrors the real file's shape: an [Unreleased] section with
// duplicated headings (from past union merges) followed by a version header.
const baseChangelog = `# Changelog

## [Unreleased]

### Added
- First added entry.

### Changed
- First changed entry.

### Fixed
- First fixed entry.

### Changed
- Duplicate-heading changed entry.

## [0.9.7] - 2026-02-10

### Added
- Old release entry.
`

func TestFold_ExistingHeadingFoldsIntoFirst(t *testing.T) {
	frags := []Fragment{
		{Category: "added", Body: "- New added from fragment."},
		{Category: "changed", Body: "- New changed from fragment."},
	}
	out, err := foldIntoChangelog(baseChangelog, frags)
	if err != nil {
		t.Fatal(err)
	}
	// The added entry appends after "First added entry.".
	if !strings.Contains(out, "- First added entry.\n- New added from fragment.\n") {
		t.Errorf("added fragment not appended to first Added section:\n%s", out)
	}
	// The changed entry appends to the FIRST Changed section, not the duplicate.
	firstChanged := strings.Index(out, "- First changed entry.")
	dupChanged := strings.Index(out, "- Duplicate-heading changed entry.")
	newChanged := strings.Index(out, "- New changed from fragment.")
	if firstChanged >= newChanged || newChanged >= dupChanged {
		t.Errorf("changed fragment should fold into first Changed section:\n%s", out)
	}
	// The old release section is untouched.
	if !strings.Contains(out, "## [0.9.7] - 2026-02-10\n\n### Added\n- Old release entry.\n") {
		t.Errorf("release section altered:\n%s", out)
	}
}

func TestFold_CreatesHeadingInCanonicalOrder(t *testing.T) {
	// Removed (rank 3) has no heading; it must be created before Fixed (rank 4).
	frags := []Fragment{{Category: "removed", Body: "- Removed a thing."}}
	out, err := foldIntoChangelog(baseChangelog, frags)
	if err != nil {
		t.Fatal(err)
	}
	changed := strings.Index(out, "### Changed")
	removed := strings.Index(out, "### Removed")
	fixed := strings.Index(out, "### Fixed")
	if removed < 0 {
		t.Fatalf("Removed heading not created:\n%s", out)
	}
	if changed >= removed || removed >= fixed {
		t.Errorf("Removed should sit between Changed and Fixed:\n%s", out)
	}
	// New heading has its entry and a trailing blank before ### Fixed.
	if !strings.Contains(out, "### Removed\n- Removed a thing.\n\n### Fixed") {
		t.Errorf("Removed section malformed:\n%s", out)
	}
}

func TestFold_AppendsAtEndWhenHighestRank(t *testing.T) {
	// Security (rank 5) is higher than every existing heading, so it appends at
	// the end of the Unreleased section, before the version header.
	frags := []Fragment{{Category: "security", Body: "- Patched something."}}
	out, err := foldIntoChangelog(baseChangelog, frags)
	if err != nil {
		t.Fatal(err)
	}
	sec := strings.Index(out, "### Security")
	ver := strings.Index(out, "## [0.9.7]")
	if sec < 0 || sec > ver {
		t.Fatalf("Security should be inside Unreleased, before the version header:\n%s", out)
	}
	if !strings.Contains(out, "### Security\n- Patched something.\n\n## [0.9.7]") {
		t.Errorf("Security section malformed:\n%s", out)
	}
}

func TestFold_MultiLineBody(t *testing.T) {
	frags := []Fragment{{Category: "added", Body: "- **Feature**: line one.\n  - sub bullet\n  continued."}}
	out, err := foldIntoChangelog(baseChangelog, frags)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "- **Feature**: line one.\n  - sub bullet\n  continued.") {
		t.Errorf("multi-line body not preserved verbatim:\n%s", out)
	}
}

func TestFold_NoFragmentsIsNoOp(t *testing.T) {
	out, err := foldIntoChangelog(baseChangelog, nil)
	if err != nil {
		t.Fatal(err)
	}
	if out != baseChangelog {
		t.Errorf("expected no-op, content changed")
	}
}

func TestFold_MissingUnreleasedErrors(t *testing.T) {
	_, err := foldIntoChangelog("# Changelog\n\n## [0.9.0] - 2026-01-01\n", []Fragment{
		{Category: "added", Body: "- x"},
	})
	if err == nil || !strings.Contains(err.Error(), "Unreleased") {
		t.Fatalf("expected Unreleased error, got %v", err)
	}
}

func TestFold_PreservesTrailingNewline(t *testing.T) {
	out, _ := foldIntoChangelog(baseChangelog, []Fragment{{Category: "added", Body: "- x"}})
	if !strings.HasSuffix(out, "\n") {
		t.Errorf("trailing newline dropped")
	}
	noNL := strings.TrimSuffix(baseChangelog, "\n")
	out2, _ := foldIntoChangelog(noNL, []Fragment{{Category: "added", Body: "- x"}})
	if strings.HasSuffix(out2, "\n") {
		t.Errorf("trailing newline added where none existed")
	}
}

func TestCutRelease(t *testing.T) {
	folded, err := foldIntoChangelog(baseChangelog, []Fragment{{Category: "added", Body: "- x"}})
	if err != nil {
		t.Fatal(err)
	}
	out, err := cutRelease(folded, "v0.10.0", "2026-07-17")
	if err != nil {
		t.Fatal(err)
	}
	// Fresh empty Unreleased on top, version header directly below (v stripped).
	if !strings.Contains(out, "## [Unreleased]\n\n## [0.10.0] - 2026-07-17\n") {
		t.Errorf("release not cut correctly:\n%s", out)
	}
	// The folded entry rode down into the new version section.
	relIdx := strings.Index(out, "## [0.10.0] - 2026-07-17")
	entryIdx := strings.Index(out, "- x")
	if entryIdx < relIdx {
		t.Errorf("folded entry should be under the new version section:\n%s", out)
	}
	// Exactly one [Unreleased] remains.
	if n := strings.Count(out, "## [Unreleased]"); n != 1 {
		t.Errorf("want exactly 1 [Unreleased], got %d", n)
	}
}

// TestFold_EndToEndRoundTrip loads real fragment files, folds, and confirms the
// files would be the ones deleted.
func TestFold_EndToEndRoundTrip(t *testing.T) {
	dir := t.TempDir()
	writeFrag(t, dir, "README.md", "docs")
	writeFrag(t, dir, "a-thing.added.md", "- Added a thing.")
	writeFrag(t, dir, "b-thing.fixed.md", "- Fixed b thing.")
	frags, errs := loadFragments(dir)
	if len(errs) != 0 {
		t.Fatalf("errs: %v", errs)
	}
	out, err := foldIntoChangelog(baseChangelog, frags)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "- Added a thing.") || !strings.Contains(out, "- Fixed b thing.") {
		t.Errorf("fragments not folded:\n%s", out)
	}
}
