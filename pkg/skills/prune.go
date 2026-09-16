// prune.go — manifest-diff pruning of previously generated skill files.
package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// PruneManifest removes stale files only when their bytes still match the
// previous delivery. Legacy manifests remain readable, but missing hashes
// cannot establish safe deletion: preserve those files and report a conflict.
// All candidates are checked before deleting any, so a known conflict leaves
// the stale set intact. Filesystem failures are returned to the caller, which
// must retain the previous manifest for a later retry.
func PruneManifest(dir string, previous *Manifest, newFiles []string) ([]string, error) {
	if previous == nil {
		return nil, nil
	}
	keep := make(map[string]bool, len(newFiles))
	for _, rel := range newFiles {
		keep[rel] = true
	}
	var stale []string
	for _, rel := range previous.Generated {
		if keep[rel] {
			continue
		}
		path, err := managedFilePath(dir, rel)
		if err != nil {
			return nil, err
		}
		actual, err := hashRegularFile(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("inspect stale skill file %s: %w", rel, err)
		}
		expected := previous.Hashes[rel]
		if expected == "" {
			return nil, fmt.Errorf("preserving stale skill file %s: legacy manifest has no ownership hash", rel)
		}
		if actual != expected {
			return nil, fmt.Errorf("preserving modified stale skill file %s", rel)
		}
		stale = append(stale, rel)
	}
	var removed []string
	parents := make(map[string]struct{})
	for _, rel := range stale {
		if err := os.Remove(filepath.Join(dir, rel)); err != nil && !os.IsNotExist(err) {
			return removed, fmt.Errorf("remove stale skill file %s: %w", rel, err)
		} else if err == nil {
			removed = append(removed, rel)
		}
		for parent := filepath.Dir(rel); parent != "."; parent = filepath.Dir(parent) {
			parents[parent] = struct{}{}
		}
	}
	pruneEmptyParents(dir, parents)
	return removed, nil
}

// PruneStaleGenerated removes files under dir that are listed in oldFiles but
// absent from newFiles, then removes any directories left empty by the
// deletions. It returns the relative paths of the files actually removed.
//
// This is what makes `loom sync skills <target>` converge when a skill is
// deleted from skills-registry.yaml: without it, generated bundles linger in
// home directories (e.g. ~/.claude/skills/<name>/, legacy
// ~/.claude/commands/<name>.md) forever.
//
// Safety: only paths recorded in the previous manifest are ever deleted, and
// only when they resolve to a location inside dir. Directory cleanup removes a
// directory only when it contains no files at all, so hosted-import or
// hand-authored neighbor directories (which are never in the manifest) are
// left untouched.
func PruneStaleGenerated(dir string, oldFiles, newFiles []string) []string {
	keep := make(map[string]struct{}, len(newFiles))
	for _, f := range newFiles {
		keep[filepath.Clean(f)] = struct{}{}
	}

	var removed []string
	parents := map[string]struct{}{}
	for _, f := range oldFiles {
		rel := filepath.Clean(f)
		if rel == "." || filepath.IsAbs(rel) || !filepath.IsLocal(rel) {
			continue
		}
		if _, ok := keep[rel]; ok {
			continue
		}
		if err := os.Remove(filepath.Join(dir, rel)); err == nil {
			removed = append(removed, rel)
		}
		for d := filepath.Dir(rel); d != "."; d = filepath.Dir(d) {
			parents[d] = struct{}{}
		}
	}

	pruneEmptyParents(dir, parents)
	return removed
}

func pruneEmptyParents(dir string, parents map[string]struct{}) {
	// Sweep parent directories of pruned files, deepest first, removing any
	// that no longer contain files (generation pre-creates empty scaffolding
	// dirs like scripts/ and references/ that the manifest never lists).
	dirs := make([]string, 0, len(parents))
	for d := range parents {
		dirs = append(dirs, d)
	}
	sort.Slice(dirs, func(i, j int) bool {
		return strings.Count(dirs[i], string(filepath.Separator)) > strings.Count(dirs[j], string(filepath.Separator))
	})
	for _, d := range dirs {
		removeDirIfNoFiles(filepath.Join(dir, d))
	}
}

// removeDirIfNoFiles removes dir when it contains no regular files, first
// clearing recursively file-free subdirectories. It reports whether dir was
// removed.
func removeDirIfNoFiles(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	empty := true
	for _, e := range entries {
		if e.IsDir() {
			if !removeDirIfNoFiles(filepath.Join(dir, e.Name())) {
				empty = false
			}
		} else {
			empty = false
		}
	}
	if !empty {
		return false
	}
	return os.Remove(dir) == nil
}
