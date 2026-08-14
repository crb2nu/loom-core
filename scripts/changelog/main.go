// Command changelog assembles per-MR changelog fragments from changelog.d/ into
// CHANGELOG.md, replacing direct edits of the shared [Unreleased] section (which
// collide across concurrent merge requests).
//
// Fragments are files named `changelog.d/<slug>.<category>.md` whose body is the
// markdown bullet exactly as it should appear in CHANGELOG.md. The category
// (one of added|changed|deprecated|removed|fixed|security) is carried by the
// filename; changelog.d/README.md documents the convention and is never folded.
//
// Modes:
//
//	--check   Validate fragments (known category, non-empty body, unique slugs).
//	          Fast CI lint; makes no changes. Exit non-zero on any problem.
//
//	--fold    Insert each fragment under the matching `### Category` heading in
//	          the CHANGELOG's [Unreleased] section (creating headings in canonical
//	          order as needed), then delete the folded fragment files. With
//	          --version, also cut a release: rename [Unreleased] to the version
//	          and add a fresh empty [Unreleased] above it. Run at RELEASE time,
//	          not per-merge.
//
// Flags:
//
//	--dir        fragments directory (default "changelog.d")
//	--changelog  target changelog file (default "CHANGELOG.md")
//	--version    vX.Y.Z; when set with --fold, cut a release section
//	--date       YYYY-MM-DD for the cut release (default: today, UTC)
//	--dry-run    with --fold, print the result to stdout; do not write or delete
package main

import (
	"flag"
	"fmt"
	"os"
	"time"
)

func main() {
	var (
		check     = flag.Bool("check", false, "validate fragments without modifying anything")
		fold      = flag.Bool("fold", false, "fold fragments into the changelog and delete them")
		dir       = flag.String("dir", "changelog.d", "fragments directory")
		changelog = flag.String("changelog", "CHANGELOG.md", "target changelog file")
		version   = flag.String("version", "", "vX.Y.Z: with --fold, cut a release section")
		date      = flag.String("date", "", "YYYY-MM-DD for the cut release (default: today UTC)")
		dryRun    = flag.Bool("dry-run", false, "with --fold, print result to stdout without writing")
	)
	flag.Parse()

	switch {
	case *check == *fold:
		fmt.Fprintln(os.Stderr, "error: exactly one of --check or --fold is required")
		flag.Usage()
		os.Exit(2)
	case *check:
		os.Exit(runCheck(*dir))
	default:
		os.Exit(runFold(*dir, *changelog, *version, *date, *dryRun))
	}
}

// runCheck validates the fragments directory and returns a process exit code.
func runCheck(dir string) int {
	frags, errs := loadFragments(dir)
	if len(errs) > 0 {
		fmt.Fprintf(os.Stderr, "changelog: %d fragment problem(s) in %s/:\n", len(errs), dir)
		for _, e := range errs {
			fmt.Fprintf(os.Stderr, "  - %v\n", e)
		}
		return 1
	}
	fmt.Printf("changelog: %d fragment(s) OK in %s/\n", len(frags), dir)
	return 0
}

// runFold folds fragments into the changelog (optionally cutting a release) and
// returns a process exit code.
func runFold(dir, changelogPath, version, date string, dryRun bool) int {
	frags, errs := loadFragments(dir)
	if len(errs) > 0 {
		fmt.Fprintf(os.Stderr, "changelog: refusing to fold with %d invalid fragment(s):\n", len(errs))
		for _, e := range errs {
			fmt.Fprintf(os.Stderr, "  - %v\n", e)
		}
		return 1
	}

	raw, err := os.ReadFile(changelogPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "changelog: %v\n", err)
		return 1
	}

	folded, err := foldIntoChangelog(string(raw), frags)
	if err != nil {
		fmt.Fprintf(os.Stderr, "changelog: %v\n", err)
		return 1
	}

	if version != "" {
		if date == "" {
			date = time.Now().UTC().Format("2006-01-02")
		}
		folded, err = cutRelease(folded, version, date)
		if err != nil {
			fmt.Fprintf(os.Stderr, "changelog: %v\n", err)
			return 1
		}
	}

	if dryRun {
		fmt.Print(folded)
		return 0
	}

	if err := os.WriteFile(changelogPath, []byte(folded), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "changelog: %v\n", err)
		return 1
	}
	for _, f := range frags {
		if err := os.Remove(f.Path); err != nil {
			fmt.Fprintf(os.Stderr, "changelog: folded but could not delete %s: %v\n", f.Path, err)
			return 1
		}
	}
	msg := fmt.Sprintf("changelog: folded %d fragment(s) into %s", len(frags), changelogPath)
	if version != "" {
		msg += fmt.Sprintf(" and cut release [%s] - %s", version, date)
	}
	fmt.Println(msg)
	return 0
}
