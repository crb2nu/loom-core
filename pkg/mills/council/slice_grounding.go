package council

import (
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// slice_grounding.go is the OUTPUT-side defense for the council editor's
// proposal decomposition, symmetric with the research stage's
// SanitizeResearchNotes (pkg/mills/clients/research_grounding.go).
//
// MR !848 fixed the PROMPT side: RepoPackageLayout now injects the real
// loom-core package tree (pkg/mills/...) into the editor prompt so the
// model (gpt-5.4) stops inventing fictional slice file paths such as
// pkg/planning/ and pkg/pipeline/, which do not exist. That stopped the
// model from inventing paths, but nothing validated the OUTPUT: when the
// model still emitted a slice whose `files` all referenced a non-existent
// top-level package directory, the proposal proceeded to implement and
// escalated on an empty diff (the failure that escalated every council
// item on 2026-06-28/29).
//
// SanitizeProposalSlices closes that gap. For each proposal it validates
// every PlanSlice's `files` against the real repository:
//
//   - real slice  (every file's parent directory exists)  → kept untouched
//   - mixed slice (some real, some new-directory)          → kept but FLAGGED
//   - speculative slice (every file under a NEW directory) → kept but RECORDED
//
// FLAG-NEVER-DROP (2026-06-30): a slice whose files ALL live under a
// not-yet-existing directory is indistinguishable BY PATH from a legitimate
// new package — `pkg/mills/newpkg/x.go` (a real new sub-package) reads
// exactly like a fabricated `pkg/planning/x.go`. Earlier this case was
// DROPPED, which escalated real new-package work on an empty diff. The guard
// now KEEPS it and records it as speculative instead; the council editor's
// repo-layout prompt grounding (MR !848/!860) plus the downstream
// build/tests/CI gates catch genuine fabrication, and the speculative count
// keeps every new-directory creation observable. Nothing is dropped on
// grounding grounds anymore. Proposals that carry no PlanSlices (single-unit
// work) are never touched.
//
// The guard is fail-open: when no valid repo root is available the caller
// skips it and proposals pass through unchanged. Validation is FS-only via
// an injected predicate so the logic is pure and unit-tested in
// slice_grounding_test.go.

// SliceGuardOutcome is the audit footprint of one SanitizeProposalSlices
// pass. All counts are zero when the guard found nothing to drop or flag.
type SliceGuardOutcome struct {
	// SlicesSpeculative is the number of PlanSlices whose files ALL live
	// under a not-yet-existing directory. Kept (never dropped) under
	// flag-never-drop and recorded here so new-package creation stays
	// observable.
	SlicesSpeculative int
	// SlicesFlagged is the number of mixed PlanSlices (some real, some
	// new-directory) kept but recorded.
	SlicesFlagged int
	// SlicesDropped / ProposalsDropped are retained for metric-shape
	// compatibility; the guard no longer drops on grounding grounds
	// (flag-never-drop), so both stay zero.
	SlicesDropped    int
	ProposalsDropped int
	// DroppedPaths is the sorted, de-duplicated set of not-yet-existing file
	// paths (parent directory absent from the repo) across speculative and
	// flagged slices, for structured logging. The paths are KEPT, not
	// dropped; the field name is retained for compatibility.
	DroppedPaths []string
}

// guarded reports whether the outcome recorded any action.
func (o SliceGuardOutcome) guarded() bool {
	return o.SlicesSpeculative > 0 || o.SlicesFlagged > 0 || o.SlicesDropped > 0 || o.ProposalsDropped > 0
}

// sliceClass is the grounding verdict for one PlanSlice.
type sliceClass int

const (
	sliceReal        sliceClass = iota // no files, or every file's dir exists
	sliceMixed                         // some files real, some new-directory
	sliceSpeculative                   // has files, all under a NEW directory
)

// SanitizeProposalSlices validates each proposal's PlanSlices against
// dirExists (a repo-relative directory existence predicate) and returns
// the filtered proposals plus an audit outcome. Fail-open: a nil
// predicate returns the input unchanged.
//
// Only PlanSlices is validated — that is the carrier the council editor
// parser fills (parseCouncilProposals) and the fallback the flat-item
// path reads (proposalItemSlices). Proposals without PlanSlices pass
// through untouched.
func SanitizeProposalSlices(proposals []BacklogProposal, dirExists func(string) bool) ([]BacklogProposal, SliceGuardOutcome) {
	var outcome SliceGuardOutcome
	if dirExists == nil || len(proposals) == 0 {
		return proposals, outcome
	}
	droppedSet := map[string]struct{}{}
	out := make([]BacklogProposal, 0, len(proposals))
	for _, p := range proposals {
		if len(p.PlanSlices) == 0 {
			out = append(out, p)
			continue
		}
		kept := make([]PlanSliceSpec, 0, len(p.PlanSlices))
		for _, s := range p.PlanSlices {
			class, ungrounded := classifySliceFiles(s.Files, dirExists)
			for _, f := range ungrounded {
				droppedSet[f] = struct{}{}
			}
			switch class {
			case sliceSpeculative:
				// Every file lives under a NEW directory. Indistinguishable by
				// path from a legitimate new package, so KEEP it and record it
				// rather than dropping real new-package work (flag-never-drop).
				outcome.SlicesSpeculative++
				kept = append(kept, s)
			case sliceMixed:
				outcome.SlicesFlagged++
				kept = append(kept, s)
			default: // sliceReal (includes the no-files case)
				kept = append(kept, s)
			}
		}
		// Nothing is dropped on grounding grounds anymore, so for a proposal
		// that carried PlanSlices, kept is always non-empty here.
		p.PlanSlices = kept
		out = append(out, p)
	}
	if len(droppedSet) > 0 {
		outcome.DroppedPaths = make([]string, 0, len(droppedSet))
		for f := range droppedSet {
			outcome.DroppedPaths = append(outcome.DroppedPaths, f)
		}
		sort.Strings(outcome.DroppedPaths)
	}
	return out, outcome
}

// classifySliceFiles inspects a slice's file list and returns its grounding
// class plus the not-yet-existing paths it referenced (parent directory
// absent). Empty/whitespace entries are ignored. A slice with no usable files
// is treated as sliceReal (nothing to validate; the scope gate handles
// slice-less or tests-only items elsewhere).
func classifySliceFiles(files []string, dirExists func(string) bool) (sliceClass, []string) {
	var grounded int
	var ungrounded []string
	for _, f := range files {
		dir, ok := fileParentDir(f)
		if !ok {
			continue // empty entry — not a real reference
		}
		if dirExists(dir) {
			grounded++
			continue
		}
		ungrounded = append(ungrounded, normalizeSliceFile(f))
	}
	if len(ungrounded) == 0 {
		return sliceReal, nil
	}
	if grounded == 0 {
		return sliceSpeculative, ungrounded
	}
	return sliceMixed, ungrounded
}

// fileParentDir returns the repo-relative parent directory of a slice file
// path, and false when the entry is empty. A top-level file (no slash)
// yields ".", which resolves to the repo root. New files are expected, so
// the DIRECTORY — not the file — is what must exist.
func fileParentDir(file string) (string, bool) {
	f := normalizeSliceFile(file)
	if f == "" {
		return "", false
	}
	return path.Dir(f), true
}

// normalizeSliceFile trims whitespace, surrounding quotes/backticks, and a
// leading ./ or / from a raw file entry, returning a clean slash path.
func normalizeSliceFile(file string) string {
	f := strings.TrimSpace(file)
	f = strings.Trim(f, "`'\"")
	f = strings.TrimSpace(f)
	f = filepath.ToSlash(f)
	f = strings.TrimPrefix(f, "./")
	f = strings.TrimPrefix(f, "/")
	return f
}

// repoDirChecker returns a predicate reporting whether a repo-relative path
// is an existing directory under root. It refuses paths escaping root via
// "..". An empty or non-directory root yields a predicate that reports
// nothing exists, so callers must gate the guard on a valid root (otherwise
// every slice would read as fictional). Mirrors research_grounding.go's
// repoPathChecker, but requires a directory.
func repoDirChecker(root string) func(string) bool {
	root = strings.TrimSpace(root)
	return func(rel string) bool {
		if root == "" {
			return false
		}
		rel = strings.TrimSpace(rel)
		rel = strings.TrimPrefix(rel, "./")
		rel = strings.TrimPrefix(rel, "/")
		clean := filepath.Clean(rel) // "" and "." both clean to "."
		if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return false
		}
		fi, err := os.Stat(filepath.Join(root, clean))
		return err == nil && fi.IsDir()
	}
}

// repoRootIsDir reports whether root is a usable directory. The guard runs
// only when this is true; otherwise repoDirChecker would classify every
// slice as fictional and drop legitimate work.
func repoRootIsDir(root string) bool {
	root = strings.TrimSpace(root)
	if root == "" {
		return false
	}
	fi, err := os.Stat(root)
	return err == nil && fi.IsDir()
}
