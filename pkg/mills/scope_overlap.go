package mills

import (
	"context"
	"path/filepath"
	"sort"
	"strings"

	"github.com/crb2nu/loom/pkg/mills/store"
)

// Scope-overlap serialization (reconciler dispatch guard).
//
// The council decomposes a theme into several backlog items whose slices
// frequently name files in the SAME package (2026-07-08/09: ten
// failure-classification items all declared pkg/mills/pipeline files; the
// reconciler dispatched them concurrently and every resulting MR conflicted
// with its siblings — seven open MRs, zero merged, ~$12 burned across their
// escalations #290–#305). Two runs editing one package can only race to a
// merge conflict, so the reconciler now defers a queued item while another
// RUNNING item holds an intersecting scope envelope. The deferred item stays
// queued; the on-merge KickNow dispatches it as soon as the blocker clears.
//
// The envelope mirrors pkg/mills/gates/scope.go: council-guessed basenames
// are systematically near-misses of what the implement agent writes, so the
// enforceable unit is the parent DIRECTORY of each slice-declared file, and
// two envelopes intersect when any directory of one equals — or is an
// ancestor/descendant of — a directory of the other. Serialization compares
// only items resolved to the same target repo; cross-repo runs cannot
// produce competing MRs.
//
// Escalated items do NOT block: their MRs may be open too, but serializing
// behind a wedged escalation would starve the queue behind work that needs
// a human anyway.

// changelogFragmentDir is excluded from the directory envelope. Every item
// declares changelog.d/*.md, and fragments are slug-unique per MR precisely
// so concurrent branches never collide there (the docs_guardrail gate's
// stated rationale). Counting the directory would make every queued item
// overlap every running item — total queue serialization behind a single
// run (observed 2026-08-09: all admissions deferred with witness
// "changelog.d"). Two items declaring the SAME literal fragment path still
// collide via the files map.
const changelogFragmentDir = "changelog.d"

// scopeEnvelope is the comparable footprint of one backlog item's slices.
type scopeEnvelope struct {
	// files holds the cleaned slice-declared paths (exact-match fallback for
	// repo-root files whose parent directory "." is deliberately excluded).
	files map[string]struct{}
	// dirs holds the parent directory of every declared file plus the static
	// directory prefix of every glob pattern.
	dirs map[string]struct{}
}

// envelopeForItem builds the scope envelope from every slice's files+tests.
// Items without slices (canaries, bootstrapped-repo plans) yield an empty
// envelope, which never blocks and is never blocked.
func envelopeForItem(item *store.BacklogItem) scopeEnvelope {
	env := scopeEnvelope{files: map[string]struct{}{}, dirs: map[string]struct{}{}}
	if item == nil {
		return env
	}
	for _, s := range item.Slices {
		for _, f := range s.Files {
			env.add(f)
		}
		for _, t := range s.Tests {
			env.add(t)
		}
	}
	return env
}

func (e scopeEnvelope) add(path string) {
	path = strings.TrimSpace(path)
	if path == "" {
		return
	}
	if strings.ContainsAny(path, "*?[") {
		// Globs contribute their static directory prefix ("cmd/*.go" → cmd).
		// A pattern that wildcards its first segment has no enforceable
		// prefix and is skipped — treating it as repo-root would make the
		// envelope collide with everything, the same allow-anything hazard
		// the "." exclusion below avoids.
		if dir := globStaticDir(path); dir != "" && dir != changelogFragmentDir {
			e.dirs[dir] = struct{}{}
		}
		return
	}
	cleaned := filepath.Clean(path)
	e.files[cleaned] = struct{}{}
	// "." (a repo-root file's parent) would make every pair of items with a
	// root file "overlap"; root files fall back to exact-file comparison.
	if dir := filepath.Dir(cleaned); dir != "." && dir != "/" && dir != changelogFragmentDir {
		e.dirs[dir] = struct{}{}
	}
}

func (e scopeEnvelope) empty() bool {
	return len(e.files) == 0 && len(e.dirs) == 0
}

// overlaps reports whether the two envelopes intersect, returning a witness
// path (a shared file or the shallower of the two related directories) for
// the defer reason. Directory containment counts both ways: a slice pinning
// pkg/mills/pipeline must serialize against one pinning
// pkg/mills/pipeline/subdir, because the scope gate's envelope
// (gates/scope.go isAllowed) lets each run modify the other's files.
func (e scopeEnvelope) overlaps(other scopeEnvelope) (bool, string) {
	for f := range e.files {
		if _, ok := other.files[f]; ok {
			return true, f
		}
	}
	// Deterministic witness: scan sorted so repeated ticks log the same path.
	for _, d := range sortedKeys(e.dirs) {
		for _, o := range sortedKeys(other.dirs) {
			if d == o {
				return true, d
			}
			if strings.HasPrefix(o, d+"/") {
				return true, d
			}
			if strings.HasPrefix(d, o+"/") {
				return true, o
			}
		}
	}
	return false, ""
}

func sortedKeys(m map[string]struct{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// globStaticDir returns the directory formed by the path segments before the
// first wildcard-bearing segment ("pkg/mills/*.go" → pkg/mills; "*.go" → "").
func globStaticDir(pattern string) string {
	segs := strings.Split(filepath.ToSlash(pattern), "/")
	var static []string
	for _, seg := range segs {
		if strings.ContainsAny(seg, "*?[") {
			break
		}
		static = append(static, seg)
	}
	// The final static segment before the wildcard is a directory by
	// construction (the wildcard names its children).
	if len(static) == 0 {
		return ""
	}
	dir := filepath.Clean(strings.Join(static, "/"))
	if dir == "." || dir == "/" {
		return ""
	}
	return dir
}

// scopeOverlapBlocker returns the id of a running backlog item whose scope
// envelope intersects item's (plus the witness path), or "" when the item is
// clear to start. Store read errors propagate so Tick counts the item
// errored rather than silently starting a conflicting run.
func (r *Reconciler) scopeOverlapBlocker(ctx context.Context, item *store.BacklogItem) (string, string, error) {
	env := envelopeForItem(item)
	if env.empty() {
		return "", "", nil
	}
	running, err := r.Store.Backlog.ListByState(ctx, store.BacklogRunning)
	if err != nil {
		return "", "", err
	}
	for _, other := range running {
		if other == nil || other.ID == item.ID {
			continue
		}
		if hit, witness := store.BacklogScopesOverlap(item, other, r.HomeProject); hit {
			return other.ID, witness, nil
		}
	}
	return "", "", nil
}
