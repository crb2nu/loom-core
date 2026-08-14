package store

import (
	"path/filepath"
	"sort"
	"strings"
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
	return aScope.overlaps(bScope)
}

type backlogScope struct {
	files map[string]struct{}
	dirs  map[string]struct{}
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
			out.add(path)
		}
	}
	return out
}

func (s backlogScope) add(path string) {
	path = strings.TrimSpace(path)
	if path == "" {
		return
	}
	if strings.ContainsAny(path, "*?[") {
		if dir := scopeGlobStaticDir(path); dir != "" && dir != backlogChangelogDir {
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
	}
}

func (s backlogScope) empty() bool { return len(s.files) == 0 && len(s.dirs) == 0 }

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
