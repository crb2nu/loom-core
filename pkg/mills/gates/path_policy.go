package gates

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/crb2nu/loom/pkg/mills/store"
)

// PathPolicy fails when the implement stage touched a path matched by the
// active policy's `pipeline.protected_paths` globs *and* the item didn't
// already declare the touch via policy.protected_paths_touched (which the
// council uses to opt an item into protected scope explicitly).
//
// The runtime's response is policy-driven: a fail here doesn't necessarily
// kill the run — for items with `policy.require_human_review = true` the
// reconciler swaps the post_ci_gate auto_gate for a human_gate. But the
// gate's job is to surface the touch deterministically; the runtime decides
// what to do about it.
type PathPolicy struct{}

// Name returns the gate identifier.
func (g *PathPolicy) Name() string { return "path_policy" }

// Evaluate consults the active policy's protected_paths globs against
// in.FilesChanged. Pre-declared touches (item.Policy.ProtectedPathsTouched)
// are removed from the violation list so the gate doesn't double-fire on
// items where the council already flagged the protected scope.
func (g *PathPolicy) Evaluate(_ context.Context, in StageInput) (Outcome, error) {
	if in.Policy == nil {
		// Without a policy snapshot we can't enforce; refuse to silently
		// pass since this is a security-adjacent gate. Treat as
		// infrastructure error in the gates layer; the reconciler will
		// surface it as such.
		return fail("path_policy: no policy snapshot supplied"), nil
	}
	if len(in.FilesChanged) == 0 {
		return pass(), nil
	}
	hits := in.Policy.ProtectedPathsHit(in.FilesChanged)
	if len(hits) == 0 {
		return pass(), nil
	}

	// Subtract any pre-declared touches the item already opted into.
	var undeclared []string
	for _, h := range hits {
		if protectedHitDeclared(h, in.Item) {
			continue
		}
		undeclared = append(undeclared, h)
	}
	if len(undeclared) == 0 {
		// Every protected hit was pre-declared; the item is operating
		// under explicit human-review opt-in.
		return pass(), nil
	}
	sort.Strings(undeclared)
	return fail(fmt.Sprintf(
		"%d undeclared protected-path touch(es): %s",
		len(undeclared), strings.Join(undeclared, ", "),
	)), nil
}

// protectedHitDeclared reports whether a protected-path hit was pre-declared by
// the item. Pre-declarations (item.Policy.ProtectedPathsTouched) are
// repo-relative — the council / plan-slice emitter records them from the slice's
// declared `files` — but spawn stages report ABSOLUTE changed paths
// (…/loom-core/pkg/x/auth.go), so an exact compare never matches and the gate
// would re-fire on an already-declared touch. Match an absolute hit when it ends
// with a declared repo-relative path on a path-segment boundary (mirrors the
// scope gate's absolute-suffix handling). An UNDECLARED protected touch — one
// the implement stage introduced that the slice never declared — still fails.
func protectedHitDeclared(hit string, item *store.BacklogItem) bool {
	if item == nil {
		return false
	}
	for _, d := range item.Policy.ProtectedPathsTouched {
		if d == "" {
			continue
		}
		if hit == d {
			return true
		}
		if filepath.IsAbs(hit) && !filepath.IsAbs(d) && strings.HasSuffix(hit, "/"+d) {
			return true
		}
	}
	return false
}
