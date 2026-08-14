package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// categories is the canonical Keep a Changelog category set, in the order they
// should appear under a version heading. A fragment's category is carried by
// its filename (`<slug>.<category>.md`), lowercased.
var categories = []string{"added", "changed", "deprecated", "removed", "fixed", "security"}

// categoryTitle maps a lowercase category to its `### <Title>` heading form.
var categoryTitle = map[string]string{
	"added":      "Added",
	"changed":    "Changed",
	"deprecated": "Deprecated",
	"removed":    "Removed",
	"fixed":      "Fixed",
	"security":   "Security",
}

// categoryRank gives each category its canonical ordinal, used to place a
// newly-created `### <Category>` heading relative to existing ones.
var categoryRank = map[string]int{}

func init() {
	for i, c := range categories {
		categoryRank[c] = i
	}
}

// fragmentNameRE matches a fragment filename: a non-empty slug, a known
// category, and the `.md` suffix. The slug may contain dots (e.g. a date), so
// the category is anchored as the LAST dotted segment before `.md`.
var fragmentNameRE = regexp.MustCompile(
	`^(.+)\.(added|changed|deprecated|removed|fixed|security)\.md$`)

// unreleasedRE matches the `## [Unreleased]` section header.
var unreleasedRE = regexp.MustCompile(`^##\s+\[Unreleased\]\s*$`)

// h2RE matches any level-2 (`## `) heading — used to bound the Unreleased
// section (it ends at the next version header).
var h2RE = regexp.MustCompile(`^##\s`)

// h3RE matches any level-3 (`### `) heading — used to bound a category section.
var h3RE = regexp.MustCompile(`^###\s`)

// Fragment is one changelog entry pending assembly.
type Fragment struct {
	Path     string // full path on disk
	Base     string // basename, e.g. "feat-auth.added.md"
	Slug     string // "feat-auth"
	Category string // lowercase, e.g. "added"
	Body     string // entry markdown, trailing whitespace trimmed
}

// loadFragments reads every fragment file under dir (excluding README.md and
// non-.md files), sorted by basename for deterministic output. Validation
// errors (bad filename, empty body, duplicate slug) are collected and returned;
// well-formed fragments are still returned so callers can inspect both.
func loadFragments(dir string) ([]Fragment, []error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // no changelog.d/ yet is valid: nothing to fold
		}
		return nil, []error{fmt.Errorf("read %s: %w", dir, err)}
	}

	var frags []Fragment
	var errs []error
	slugSeen := map[string]string{} // slug -> first basename that used it

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)

	for _, name := range names {
		if name == "README.md" {
			continue // the convention doc is not an entry
		}
		if !strings.HasSuffix(name, ".md") {
			continue // ignore stray non-markdown files (e.g. .gitkeep)
		}
		m := fragmentNameRE.FindStringSubmatch(name)
		if m == nil {
			errs = append(errs, fmt.Errorf(
				"%s: filename must be <slug>.<category>.md where category is one of %s",
				name, strings.Join(categories, "|")))
			continue
		}
		slug, cat := m[1], m[2]
		raw, rerr := os.ReadFile(filepath.Join(dir, name))
		if rerr != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, rerr))
			continue
		}
		body := strings.TrimRight(strings.TrimSpace(string(raw)), "\n")
		if body == "" {
			errs = append(errs, fmt.Errorf("%s: fragment body is empty", name))
			continue
		}
		if prev, dup := slugSeen[slug]; dup {
			errs = append(errs, fmt.Errorf(
				"%s: duplicate slug %q (already used by %s); give each fragment a unique slug",
				name, slug, prev))
			continue
		}
		slugSeen[slug] = name
		frags = append(frags, Fragment{
			Path:     filepath.Join(dir, name),
			Base:     name,
			Slug:     slug,
			Category: cat,
			Body:     body,
		})
	}
	return frags, errs
}

// foldIntoChangelog inserts each fragment's body under the matching `### Category`
// heading inside the `## [Unreleased]` section of the changelog content, creating
// category headings in canonical order when absent. Fragments are grouped by
// category and inserted in their pre-sorted (by-filename) order. It tolerates a
// CHANGELOG whose Unreleased section has duplicated category headings (an
// artifact of past union merges): a fragment folds into the FIRST matching
// heading. The content's trailing newline is preserved.
func foldIntoChangelog(content string, frags []Fragment) (string, error) {
	if len(frags) == 0 {
		return content, nil
	}

	hadTrailingNewline := strings.HasSuffix(content, "\n")
	lines := strings.Split(strings.TrimSuffix(content, "\n"), "\n")

	byCat := map[string][]string{}
	for _, f := range frags {
		byCat[f.Category] = append(byCat[f.Category], f.Body)
	}

	// Process in canonical order so a run of new-heading creations lands in the
	// right relative order.
	for _, cat := range categories {
		bodies := byCat[cat]
		if len(bodies) == 0 {
			continue
		}
		var err error
		lines, err = insertUnderCategory(lines, cat, bodies)
		if err != nil {
			return "", err
		}
	}

	out := strings.Join(lines, "\n")
	if hadTrailingNewline {
		out += "\n"
	}
	return out, nil
}

// unreleasedRange returns the [start, end) line index range of the Unreleased
// section: start is the `## [Unreleased]` line; end is the next `## ` heading
// (a version header) or len(lines). Returns start=-1 when no Unreleased header
// is present.
func unreleasedRange(lines []string) (start, end int) {
	start = -1
	for i, l := range lines {
		if unreleasedRE.MatchString(l) {
			start = i
			break
		}
	}
	if start < 0 {
		return -1, -1
	}
	end = len(lines)
	for i := start + 1; i < len(lines); i++ {
		if h2RE.MatchString(lines[i]) {
			end = i
			break
		}
	}
	return start, end
}

// insertUnderCategory inserts the given entry bodies under category cat inside
// the Unreleased section, returning the updated line slice.
func insertUnderCategory(lines []string, cat string, bodies []string) ([]string, error) {
	start, end := unreleasedRange(lines)
	if start < 0 {
		return nil, fmt.Errorf("no \"## [Unreleased]\" section found in CHANGELOG")
	}

	// Flatten every body (each may be a multi-line bullet) into a line block.
	var block []string
	for _, b := range bodies {
		block = append(block, strings.Split(b, "\n")...)
	}

	title := categoryTitle[cat]
	headingRE := regexp.MustCompile(`^###\s+` + regexp.QuoteMeta(title) + `\s*$`)

	// Find the FIRST matching category heading within the Unreleased section.
	h := -1
	for i := start + 1; i < end; i++ {
		if headingRE.MatchString(lines[i]) {
			h = i
			break
		}
	}

	if h >= 0 {
		// Append after the last non-blank line of that heading's section.
		secEnd := end
		for i := h + 1; i < end; i++ {
			if h3RE.MatchString(lines[i]) || h2RE.MatchString(lines[i]) {
				secEnd = i
				break
			}
		}
		insAt := h + 1
		for i := h + 1; i < secEnd; i++ {
			if strings.TrimSpace(lines[i]) != "" {
				insAt = i + 1
			}
		}
		return insertAt(lines, insAt, block), nil
	}

	// No heading yet: create one in canonical order. Find the first existing
	// category heading whose rank is higher than cat's and insert immediately
	// before it (the pre-existing blank line before that heading becomes our
	// separator, and we add a trailing blank line before it).
	for i := start + 1; i < end; i++ {
		if r, ok := headingRank(lines[i]); ok && r > categoryRank[cat] {
			toInsert := append([]string{"### " + title}, block...)
			toInsert = append(toInsert, "")
			return insertAt(lines, i, toInsert), nil
		}
	}

	// Otherwise append at the end of the Unreleased section, after its last
	// non-blank line, with a leading blank separator.
	insAt := start + 1
	for i := start + 1; i < end; i++ {
		if strings.TrimSpace(lines[i]) != "" {
			insAt = i + 1
		}
	}
	toInsert := append([]string{"", "### " + title}, block...)
	return insertAt(lines, insAt, toInsert), nil
}

// headingRank returns the canonical rank of a `### <Category>` heading line and
// true, or (0, false) when the line is not a recognized category heading.
func headingRank(line string) (int, bool) {
	if !h3RE.MatchString(line) {
		return 0, false
	}
	title := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "###"))
	r, ok := categoryRank[strings.ToLower(title)]
	return r, ok
}

// insertAt returns a new slice with block spliced into lines at index i.
func insertAt(lines []string, i int, block []string) []string {
	out := make([]string, 0, len(lines)+len(block))
	out = append(out, lines[:i]...)
	out = append(out, block...)
	out = append(out, lines[i:]...)
	return out
}

// cutRelease renames the `## [Unreleased]` header to a versioned header and
// inserts a fresh empty `## [Unreleased]` above it. A leading "v" on version is
// stripped to match the repo's `## [0.9.7]` heading style. date defaults are the
// caller's responsibility.
func cutRelease(content, version, date string) (string, error) {
	version = strings.TrimPrefix(version, "v")
	hadTrailingNewline := strings.HasSuffix(content, "\n")
	lines := strings.Split(strings.TrimSuffix(content, "\n"), "\n")

	idx := -1
	for i, l := range lines {
		if unreleasedRE.MatchString(l) {
			idx = i
			break
		}
	}
	if idx < 0 {
		return "", fmt.Errorf("no \"## [Unreleased]\" section found in CHANGELOG")
	}

	repl := []string{"## [Unreleased]", "", fmt.Sprintf("## [%s] - %s", version, date)}
	out := make([]string, 0, len(lines)+len(repl))
	out = append(out, lines[:idx]...)
	out = append(out, repl...)
	out = append(out, lines[idx+1:]...)

	joined := strings.Join(out, "\n")
	if hadTrailingNewline {
		joined += "\n"
	}
	return joined, nil
}
