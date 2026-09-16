package mills

import (
	"context"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/crb2nu/loom/pkg/mills/store"
)

// Scope-overlap serialization (reconciler dispatch guard).
//
// Literal paths serialize only when they name the same cleaned file. Sibling
// files can run concurrently; textual conflicts are handled by the merge queue.
// Globs reserve their static directory against related literal-file and glob
// directories. This admission rule is separate from the broader scope gate.
// Comparisons apply only within the same target repository.
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
	// files holds cleaned literal paths for exact intersection.
	files map[string]struct{}
	// Literal directories participate only when compared with a glob.
	literalDirs map[string]struct{}
	globDirs    map[string]struct{}
}

// envelopeForItem builds the scope envelope from every slice's files+tests.
// Items without slices (canaries, bootstrapped-repo plans) yield an empty
// envelope, which never blocks and is never blocked.
func envelopeForItem(item *store.BacklogItem) scopeEnvelope {
	env := scopeEnvelope{files: map[string]struct{}{}, literalDirs: map[string]struct{}{}, globDirs: map[string]struct{}{}}
	if item == nil {
		return env
	}
	for _, s := range item.Slices {
		for _, f := range s.Files {
			env.add(f)
		}
		for _, t := range s.Tests {
			if isPathLike(t) {
				env.add(t)
			}
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
			e.globDirs[dir] = struct{}{}
		}
		return
	}
	cleaned := filepath.Clean(path)
	e.files[cleaned] = struct{}{}
	// "." (a repo-root file's parent) would make every pair of items with a
	// root file "overlap"; root files fall back to exact-file comparison.
	if dir := filepath.Dir(cleaned); dir != "." && dir != "/" && dir != changelogFragmentDir {
		e.literalDirs[dir] = struct{}{}
	}
}

func (e scopeEnvelope) empty() bool {
	return len(e.files) == 0 && len(e.literalDirs) == 0 && len(e.globDirs) == 0
}

// overlaps returns a shared literal file or an overlapping glob directory.
// Literal directories alone never reserve sibling or descendant files.
func (e scopeEnvelope) overlaps(other scopeEnvelope) (bool, string) {
	for _, f := range sortedKeys(e.files) {
		if _, ok := other.files[f]; ok {
			return true, f
		}
	}
	for i, pair := range [][2]map[string]struct{}{
		{e.globDirs, other.globDirs},
		{e.globDirs, other.literalDirs},
		{other.globDirs, e.literalDirs},
	} {
		for _, glob := range sortedKeys(pair[0]) {
			for _, dir := range sortedKeys(pair[1]) {
				if glob == dir || strings.HasPrefix(dir, glob+"/") || strings.HasPrefix(glob, dir+"/") {
					// For two globs, use their shallower directory.
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

func (r *Reconciler) scopeReservationBlocker(ctx context.Context, item *store.BacklogItem, policy *Policy, hold time.Duration) (string, string, error) {
	reservations, err := r.Store.Backlog.ScopeReservations(ctx)
	if err != nil {
		return "", "", err
	}
	now := r.now().UTC()
	type eligibleReservation struct {
		item       *store.BacklogItem
		reservedAt time.Time
		envelope   scopeEnvelope
	}
	eligible := make([]eligibleReservation, 0, len(reservations))
	for _, reservation := range reservations {
		if reservation.ReservedAt != nil && now.Sub(*reservation.ReservedAt) >= hold {
			if err := r.Store.Backlog.ResetScopeFairness(ctx, reservation.BacklogID); err != nil {
				return "", "", err
			}
			ScopeReservationCapReleasesTotal.Inc()
			r.append(ctx, "reconciler.scope_reservation_cap_released", "released", map[string]any{"item": reservation.BacklogID, "held_seconds": now.Sub(*reservation.ReservedAt).Seconds()})
			continue
		}
		other, err := r.Store.Backlog.Get(ctx, reservation.BacklogID)
		if err != nil {
			return "", "", err
		}
		reason, err := r.starvedCandidateExclusion(ctx, other, policy)
		if err != nil {
			return "", "", err
		}
		if reason != "" {
			r.appendStarvedCandidateExclusion(ctx, other.ID, reason)
			continue
		}
		blocker, _, err := r.scopeOverlapBlocker(ctx, other)
		if err != nil {
			return "", "", err
		}
		if blocker != "" {
			continue
		}
		eligible = append(eligible, eligibleReservation{item: other, reservedAt: *reservation.ReservedAt, envelope: envelopeForItem(other)})
	}
	sort.Slice(eligible, func(i, j int) bool {
		a, b := eligible[i], eligible[j]
		return store.ScopeReservationPrecedes(a.item, a.reservedAt, b.item, b.reservedAt)
	})
	// Include self in arbitration so lower-ranked reservations cannot block the
	// winner. A reservation overlapping any earlier eligible reservation yields
	// its entire envelope for this pass, including scopes the older one lacks.
	suppressed := make([]bool, len(eligible))
	for i, reservation := range eligible {
		for _, earlier := range eligible[:i] {
			if !sameScopeTarget(reservation.item, earlier.item, r.HomeProject) {
				continue
			}
			if hit, witness := reservation.envelope.overlaps(earlier.envelope); hit {
				suppressed[i] = true
				r.append(ctx, "reconciler.scope_reservation_yield", "older_reservation", map[string]any{
					"item": reservation.item.ID, "yields_to": earlier.item.ID, "shared_scope": witness,
				})
				break
			}
		}
	}
	itemEnv := envelopeForItem(item)
	for i, reservation := range eligible {
		other := reservation.item
		if suppressed[i] || other.ID == item.ID {
			continue
		}
		if !sameScopeTarget(item, other, r.HomeProject) {
			continue
		}
		if hit, witness := itemEnv.overlaps(reservation.envelope); hit {
			if item.Priority < other.Priority {
				r.append(ctx, "reconciler.scope_reservation_override", "higher_priority", map[string]any{
					"item": item.ID, "blocker": other.ID, "shared_scope": witness,
					"priority": item.Priority, "reserver_priority": other.Priority,
				})
				if r.Logger != nil {
					r.Logger.Info("reconciler: higher priority overrides scope reservation", "item", item.ID, "blocker", other.ID, "shared_scope", witness)
				}
				continue
			}
			return other.ID, witness, nil
		}
	}
	return "", "", nil
}

func sameScopeTarget(a, b *store.BacklogItem, homeProject string) bool {
	if a == nil || b == nil {
		return false
	}
	target := func(item *store.BacklogItem) string {
		if project := strings.TrimSpace(item.TargetProject); project != "" {
			return project
		}
		return strings.TrimSpace(homeProject)
	}
	left, right := target(a), target(b)
	if left == "" && right == "" {
		return true
	}
	return store.SameRepo(left, right)
}

// isPathLike distinguishes test paths/globs from commands in Slice.Tests.
// Files declarations deliberately bypass this filter. Whitespace rejects
// command prefixes such as "go ", "cd ", "pnpm ", "npm ", and "make ".
func isPathLike(s string) bool {
	return s != "" && strings.IndexFunc(s, unicode.IsSpace) < 0 && !strings.ContainsAny(s, "&|;<>()`")
}
