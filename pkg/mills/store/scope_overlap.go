package store

import (
	"path/filepath"
	"sort"
	"strings"
	"unicode"
)

// backlogChangelogDir is excluded from the directory scope. Every item
// declares changelog.d/*.md and fragments are slug-unique per MR, so counting
// the directory made every queued item overlap every running item and
// serialized the whole queue (2026-08-09, witness "changelog.d"). The same
// exclusion lives in pkg/mills/scope_overlap.go, but THIS copy is the
// authoritative one — scopeOverlapBlocker delegates the real comparison here,
// which is how the first fix (6e65efa2) patched only the preflight
// empty-check and left the live collision in place. Identical literal
// fragment paths still collide via the files map.
const backlogChangelogDir = "changelog.d"

// BacklogScopesOverlap reports whether two backlog items can modify the same
// file envelope in the same repository. It is shared by the reconciler's cheap
// preflight and the store's authoritative in-transaction admission check.
func BacklogScopesOverlap(a, b *BacklogItem, homeProject string) (bool, string) {
	if a == nil || b == nil || !sameBacklogTarget(a, b, homeProject) {
		return false, ""
	}
	aScope, bScope := scopeForBacklog(a), scopeForBacklog(b)
	if aScope.empty() || bScope.empty() {
		return false, ""
	}
	return aScope.admissionOverlaps(bScope)
}

type backlogScope struct {
	files       map[string]struct{}
	dirs        map[string]struct{} // Broad envelope retained for merged-item coverage.
	globDirs    map[string]struct{}
	literalDirs map[string]struct{}
}

func scopeForBacklog(item *BacklogItem) backlogScope {
	out := backlogScope{files: map[string]struct{}{}, dirs: map[string]struct{}{}}
	if item == nil {
		return out
	}
	for _, slice := range item.Slices {
		for _, path := range slice.Files {
			out.add(path)
		}
		for _, path := range slice.Tests {
			if isPathLike(path) {
				out.add(path)
			}
		}
	}
	return out
}

func (s *backlogScope) add(path string) {
	path = strings.TrimSpace(path)
	if path == "" {
		return
	}
	if strings.ContainsAny(path, "*?[") {
		if dir := scopeGlobStaticDir(path); dir != "" && dir != backlogChangelogDir {
			if s.globDirs == nil {
				s.globDirs = map[string]struct{}{}
			}
			s.globDirs[dir] = struct{}{}
			s.dirs[dir] = struct{}{}
		}
		return
	}
	cleaned := filepath.Clean(path)
	if cleaned == backlogChangelogDir {
		return
	}
	s.files[cleaned] = struct{}{}
	if dir := filepath.Dir(cleaned); dir != "." && dir != "/" && dir != backlogChangelogDir {
		s.dirs[dir] = struct{}{}
		if s.literalDirs == nil {
			s.literalDirs = map[string]struct{}{}
		}
		s.literalDirs[dir] = struct{}{}
	}
}

func (s backlogScope) empty() bool { return len(s.files) == 0 && len(s.dirs) == 0 }

// admissionOverlaps requires exact literals or containment involving a glob.
// The broader overlaps method remains the merged-canonical coverage rule.
func (s backlogScope) admissionOverlaps(other backlogScope) (bool, string) {
	for _, path := range sortedScopeKeys(s.files) {
		if _, ok := other.files[path]; ok {
			return true, path
		}
	}
	for i, pair := range [][2]map[string]struct{}{
		{s.globDirs, other.globDirs}, {s.globDirs, other.literalDirs}, {other.globDirs, s.literalDirs},
	} {
		for _, glob := range sortedScopeKeys(pair[0]) {
			for _, dir := range sortedScopeKeys(pair[1]) {
				if glob == dir || strings.HasPrefix(dir, glob+"/") || strings.HasPrefix(glob, dir+"/") {
					if i == 0 && len(dir) < len(glob) {
						return true, dir
					}
					return true, glob
				}
			}
		}
	}
	return false, ""
}

func (s backlogScope) overlaps(other backlogScope) (bool, string) {
	for path := range s.files {
		if _, ok := other.files[path]; ok {
			return true, path
		}
	}
	for _, left := range sortedScopeKeys(s.dirs) {
		for _, right := range sortedScopeKeys(other.dirs) {
			switch {
			case left == right:
				return true, left
			case strings.HasPrefix(right, left+"/"):
				return true, left
			case strings.HasPrefix(left, right+"/"):
				return true, right
			}
		}
	}
	return false, ""
}

func sortedScopeKeys(values map[string]struct{}) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func scopeGlobStaticDir(pattern string) string {
	segments := strings.Split(filepath.ToSlash(pattern), "/")
	static := make([]string, 0, len(segments))
	for _, segment := range segments {
		if strings.ContainsAny(segment, "*?[") {
			break
		}
		static = append(static, segment)
	}
	if len(static) == 0 {
		return ""
	}
	dir := filepath.Clean(strings.Join(static, "/"))
	if dir == "." || dir == "/" {
		return ""
	}
	return dir
}

// ScopeCoverage classifies the file evidence between an escalated dedup
// candidate and the merged canonical that would retire it.
type ScopeCoverage string

const (
	// ScopeCoverageUnknown: the candidate declares no slice files, so file
	// evidence can neither support nor veto a title-based retire.
	ScopeCoverageUnknown ScopeCoverage = "unknown"
	// ScopeCoverageOverlap: the canonical's delivered (or, absent capture,
	// declared) scope touches at least one path the candidate declares.
	ScopeCoverageOverlap ScopeCoverage = "overlap"
	// ScopeCoverageDisjoint: the candidate declares concrete files and the
	// canonical's evidence does not touch any of them — either provably
	// (delivered/declared scopes don't intersect, or different target repos)
	// or because the canonical carries no file evidence at all, in which case
	// "the candidate's work is on main" is unverifiable.
	ScopeCoverageDisjoint ScopeCoverage = "disjoint"
)

// MergedCanonicalCovers reports whether a merged canonical's file evidence
// covers any of an escalated candidate's declared slice files. Near-identical
// titles are how sibling slices of one plan family read (…-s-1 vs …-s-2), so
// title similarity alone cannot distinguish "this work already merged under
// the canonical" from "the sibling merged, this slice never did" — the shape
// that produced a false shipped claim for
// …spawn-state-pruning-with-hud-pressure-s-2 against
// bl-hud-spawn-state-pressure-prune-20260726 (!1241 merged only the prune
// mechanics; the HUD metrics slice never landed).
//
// delivered is the canonical's run-captured files_changed (authoritative for
// what its branch actually merged); when empty, the canonical's declared
// slices are the fallback evidence. The second return is the overlap witness
// path, or a disjoint reason: "target_project_mismatch",
// "canonical_delivery_unverifiable", or "no_scope_intersection".
func MergedCanonicalCovers(candidate, canonical *BacklogItem, delivered []string, homeProject string) (ScopeCoverage, string) {
	if candidate == nil || canonical == nil {
		return ScopeCoverageUnknown, ""
	}
	candScope := scopeForBacklog(candidate)
	if candScope.empty() {
		return ScopeCoverageUnknown, ""
	}
	if !sameBacklogTarget(candidate, canonical, homeProject) {
		return ScopeCoverageDisjoint, "target_project_mismatch"
	}
	canonScope := backlogScope{files: map[string]struct{}{}, dirs: map[string]struct{}{}}
	for _, path := range delivered {
		canonScope.add(path)
	}
	if canonScope.empty() {
		// No capture (pre-capture merge, or externally merged): the declared
		// plan is weaker evidence than a captured diff, but still names the
		// surface the canonical was about.
		canonScope = scopeForBacklog(canonical)
	}
	if canonScope.empty() {
		return ScopeCoverageDisjoint, "canonical_delivery_unverifiable"
	}
	if ok, witness := candScope.overlaps(canonScope); ok {
		return ScopeCoverageOverlap, witness
	}
	return ScopeCoverageDisjoint, "no_scope_intersection"
}

func sameBacklogTarget(a, b *BacklogItem, homeProject string) bool {
	left := strings.TrimSpace(a.TargetProject)
	right := strings.TrimSpace(b.TargetProject)
	if left == "" {
		left = homeProject
	}
	if right == "" {
		right = homeProject
	}
	if left == "" && right == "" {
		return true
	}
	return SameRepo(left, right)
}

// isPathLike distinguishes test paths/globs from commands in Slice.Tests.
// Files declarations deliberately bypass this filter. Whitespace rejects
// command prefixes such as "go ", "cd ", "pnpm ", "npm ", and "make ".
func isPathLike(s string) bool {
	return s != "" && strings.IndexFunc(s, unicode.IsSpace) < 0 && !strings.ContainsAny(s, "&|;<>()`")
}
