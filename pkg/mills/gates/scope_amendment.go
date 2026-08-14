package gates

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/store"
)

// This file is the *evaluator* half of scope auto-amendment; the runner owns
// the *effect* half (CAS-append + continue, see Runner.maybeAmendScope).
//
// Why it exists (24h KPI 2026-07-26T20:23Z): 25 pipeline runs → 4 merged, 20
// escalated — an 83% escalation rate against a 95.7% gate_pass_rate and a zero
// regression rate. The fleet's work was good; runs died disproportionately at
// the ONE `scope` gate, and the handling (rewind → full implement respawn →
// escalate unclassified after 3 attempts) could never converge, because in
// every examined case the implementer NEEDED the violating file:
//
//	token-sweep  declared internal/hud/frontend/src/lib/components/mills/*;
//	             reached …/components/shared/{EmptyState,PanelShell}.svelte.
//	             Human resolution: widen scope, requeue → merged !1249.
//	stop-lever   declared cmd/loom-mills-operator + pkg/mills/pipeline +
//	             …/components/mills; reached pkg/mills/store/dao_pipeline.go,
//	             pkg/mills/clients/spawn.go, …/stores/mills.svelte.ts.
//	             Human resolution: widen scope, requeue.
//
// Both are SIBLING-DIRECTORY reaches inside the same module — exactly what a
// human widens by hand. This evaluator encodes that hand motion: a violation is
// admissible when its directory shares a deep-enough ancestor with a directory
// the item ALREADY declared. A reach into platform/gitops, another service, the
// repo root, or a protected path is not, and still escalates.
//
// The amendment never weakens judgement of the diff: path_policy, secret_scan,
// diff_size, docs_guardrail, spec_conformance and pr_self_review all still run
// over the same files afterwards.

// Amendment rule identifiers. These are persisted verbatim into the
// `scope_violations` run artifact, so the HUD/CLI can render a per-file verdict
// without parsing prose — treat them as a wire contract, not log text.
const (
	// AmendRuleSharedAncestor: admitted — the violation's directory shares at
	// least ScopeAmendmentPolicy.Depth() leading path segments with a
	// directory the item already declared.
	AmendRuleSharedAncestor = "shared-ancestor"
	// AmendRuleNoSharedAncestor: refused — the violation reaches a different
	// part of the tree than anything the item declared (the genuine
	// "unrelated detour" this gate exists to catch).
	AmendRuleNoSharedAncestor = "no-shared-ancestor"
	// AmendRuleSensitivePath: refused — the violation matches
	// pipeline.protected_paths. Defense in depth: the path_policy gate would
	// fail it anyway, but scope must never be the thing that quietly widens an
	// item's envelope onto a protected path.
	AmendRuleSensitivePath = "sensitive-path"
	// AmendRuleNoDeclaredScope: refused — the item declares no files at all,
	// so there is no anchor to measure an ancestor against. (Scope.Evaluate
	// already resolves slice-less items to an advisory skip, so this is the
	// belt to that suspenders.)
	AmendRuleNoDeclaredScope = "no-declared-scope"
	// AmendRulePolicyDisabled: refused — pipeline.scope_amendment.enabled is
	// explicitly false.
	AmendRulePolicyDisabled = "policy-disabled"
)

// ScopeViolationVerdict is the per-file result of the admissibility test.
type ScopeViolationVerdict struct {
	// File is the violating path exactly as the implement stage reported it
	// (repo-relative, or an absolute in-pod spawn path).
	File string `json:"file"`
	// Admitted reports whether policy would let this file be appended to the
	// item's declared scope.
	Admitted bool `json:"admitted"`
	// Rule names which rule produced the verdict (one of the AmendRule*
	// constants).
	Rule string `json:"rule"`
	// Ancestor is the shared directory prefix that admitted the file, e.g.
	// "pkg/mills". Empty on a refusal.
	Ancestor string `json:"ancestor,omitempty"`
	// SliceIndex is the index of the slice whose declared files produced the
	// deepest shared ancestor — where an admitted file gets appended. Ties go
	// to the FIRST such slice, so the choice is deterministic across replays.
	// -1 when nothing matched.
	SliceIndex int `json:"slice_index"`
}

// AmendmentDecision is the whole-run verdict plus the per-file evidence. It is
// persisted as the `scope_violations` artifact on a scope escalation so a human
// (or the HUD's widen-and-requeue affordance) sees exactly which reach was
// rejected and why, instead of a 500-char-truncated prose reason.
type AmendmentDecision struct {
	// Admitted is true only when EVERY violation is individually admissible
	// AND the count clears the policy cap. Partial amendment is deliberately
	// not a thing: a diff that is half-detour is a detour.
	Admitted bool `json:"admitted"`
	// Refusal explains a whole-decision refusal that no single verdict carries
	// (today: the max_files cap). Empty when Admitted, or when the refusal is
	// already visible in the per-file verdicts.
	Refusal string `json:"refusal,omitempty"`
	// Verdicts is one entry per (de-duplicated) violation, in input order.
	Verdicts []ScopeViolationVerdict `json:"verdicts"`
	// DeclaredDirs is the sorted set of directories the item declared, i.e.
	// the anchors the ancestor test measured against.
	DeclaredDirs []string `json:"declared_dirs"`
	// AncestorDepth and MaxFiles record the effective policy the decision was
	// made under, so a persisted artifact stays interpretable after a policy
	// hot-reload changes them.
	AncestorDepth int `json:"ancestor_depth"`
	MaxFiles      int `json:"max_files"`
}

// AdmittedFiles returns the violating paths the decision admitted, in verdict
// order.
func (d AmendmentDecision) AdmittedFiles() []string {
	var out []string
	for _, v := range d.Verdicts {
		if v.Admitted {
			out = append(out, v.File)
		}
	}
	return out
}

// Summary renders the one-line reason recorded on the amended gate outcome,
// e.g. `scope amended: a.svelte, b.svelte (shared ancestor …/components)`.
func (d AmendmentDecision) Summary() string {
	files := d.AdmittedFiles()
	if len(files) == 0 {
		return "scope amended: no files"
	}
	ancestors := map[string]struct{}{}
	for _, v := range d.Verdicts {
		if v.Admitted && v.Ancestor != "" {
			ancestors[v.Ancestor] = struct{}{}
		}
	}
	list := make([]string, 0, len(ancestors))
	for a := range ancestors {
		list = append(list, a)
	}
	sort.Strings(list)
	return fmt.Sprintf("scope amended: %s (shared ancestor %s)",
		strings.Join(files, ", "), strings.Join(list, ", "))
}

// EvaluateScopeAmendment decides whether a scope-gate failure can be resolved
// by widening the item's declared scope instead of respawning the implementer.
//
// It is a pure function of (item, violations, policy) — no store, no clock, no
// I/O — so the Drive-level wiring stays a thin effect and every rule here is
// table-testable against real production fixtures.
//
// protectedPaths is pipeline.protected_paths, threaded in so the refusal reuses
// mills.ProtectedPathsMatch — the identical matcher the path_policy gate runs —
// rather than a second copy of the sensitive-path rules. (Deviation from the
// original brief's 3-arg signature: the sensitive-path refusal cannot be
// expressed from ScopeAmendmentPolicy alone, and passing the whole *mills.Policy
// would hand a pure evaluator far more authority than it needs.)
func EvaluateScopeAmendment(
	item *store.BacklogItem,
	violations []string,
	pol mills.ScopeAmendmentPolicy,
	protectedPaths []string,
) AmendmentDecision {
	depth, fileCap := pol.Depth(), pol.FileCap()
	d := AmendmentDecision{AncestorDepth: depth, MaxFiles: fileCap}

	enabled := pol.Enabled == nil || *pol.Enabled
	anchors, dirs := declaredAnchors(item)
	d.DeclaredDirs = dirs

	seen := make(map[string]struct{}, len(violations))
	for _, raw := range violations {
		f := strings.TrimSpace(raw)
		if f == "" {
			continue
		}
		if _, dup := seen[f]; dup {
			continue
		}
		seen[f] = struct{}{}
		d.Verdicts = append(d.Verdicts, verdictFor(f, anchors, depth, enabled, protectedPaths))
	}
	if len(d.Verdicts) == 0 {
		// Nothing to amend. Not an admission — the caller must not treat an
		// empty violation list as a licence to continue past a failing gate.
		d.Refusal = "no violations to evaluate"
		return d
	}

	admitted := 0
	for _, v := range d.Verdicts {
		if !v.Admitted {
			return d
		}
		admitted++
	}
	if admitted > fileCap {
		// Past the cap this is no longer "the author forgot a sibling"; it is
		// a decomposition failure, and a human should see the artifact.
		d.Refusal = fmt.Sprintf("%d admissible files exceeds max_files=%d", admitted, fileCap)
		return d
	}
	d.Admitted = true
	return d
}

// verdictFor applies the admissibility rules to one violating path, in refusal
// precedence order: policy off, then protected path, then shared ancestor.
func verdictFor(file string, anchors []declaredAnchor, depth int, enabled bool, protectedPaths []string) ScopeViolationVerdict {
	v := ScopeViolationVerdict{File: file, SliceIndex: -1}
	if !enabled {
		v.Rule = AmendRulePolicyDisabled
		return v
	}
	// Match protected globs against every repo-relative reading of the path,
	// not just the literal: a spawn stage reports
	// /workspace/services/loom-core/platform/gitops/x.yaml, which the
	// repo-relative glob "platform/gitops/**" would otherwise miss entirely.
	// A false positive here only refuses an amendment (the run escalates with
	// full evidence); a false negative would widen scope onto a protected
	// path, so this side errs deliberately.
	cleaned := filepath.Clean(file)
	candidates := append([]string{cleaned}, pathSegmentSuffixes(cleaned)...)
	if len(mills.ProtectedPathsMatch(protectedPaths, candidates)) > 0 {
		v.Rule = AmendRuleSensitivePath
		return v
	}
	if len(anchors) == 0 {
		v.Rule = AmendRuleNoDeclaredScope
		return v
	}
	bestDepth, bestAncestor, bestSlice := 0, "", -1
	for _, cand := range candidates {
		dir := filepath.Dir(cand)
		if dir == "." || dir == "/" {
			// A repo-root file has no directory ancestor to share. Root files
			// are covered by the docs-guardrail / dep-manifest carve-outs in
			// the gate itself; widening scope to the repo root is never what
			// "the author forgot a sibling" means.
			continue
		}
		for _, a := range anchors {
			n := commonSegmentDepth(dir, a.Dir)
			// Strictly-greater keeps the FIRST slice on a tie, which is what
			// makes the chosen slice deterministic across replays.
			if n > bestDepth {
				bestDepth = n
				bestAncestor = strings.Join(strings.Split(a.Dir, "/")[:n], "/")
				bestSlice = a.SliceIndex
			}
		}
	}
	if bestDepth >= depth && bestSlice >= 0 {
		v.Admitted = true
		v.Rule = AmendRuleSharedAncestor
		v.Ancestor = bestAncestor
		v.SliceIndex = bestSlice
		return v
	}
	v.Rule = AmendRuleNoSharedAncestor
	return v
}

// declaredAnchor is one directory the item already declared, tagged with the
// slice that declared it.
type declaredAnchor struct {
	Dir        string
	SliceIndex int
}

// declaredAnchors collects the directory of every file (and test) the item's
// slices declare. Tests count as declared scope because buildAllowedSet already
// treats them as part of the envelope — the amendment must measure against the
// same envelope the gate enforced, or it would refuse reaches the gate had
// already been willing to allow.
//
// Returns the anchors (first-declaring slice wins per directory, so the append
// target is stable) plus the sorted unique directory list for the artifact.
func declaredAnchors(item *store.BacklogItem) ([]declaredAnchor, []string) {
	if item == nil {
		return nil, nil
	}
	var anchors []declaredAnchor
	seen := map[string]struct{}{}
	add := func(path string, idx int) {
		p := strings.TrimSpace(path)
		if p == "" || strings.ContainsAny(p, "*?[") {
			// A glob declares no single directory to anchor on. The gate's
			// glob branch already admits whatever it matches, so a violation
			// here is by definition NOT covered by the glob.
			return
		}
		dir := filepath.Dir(filepath.Clean(p))
		if dir == "." || dir == "/" {
			return
		}
		if _, dup := seen[dir]; dup {
			return
		}
		seen[dir] = struct{}{}
		anchors = append(anchors, declaredAnchor{Dir: dir, SliceIndex: idx})
	}
	for i, s := range item.Slices {
		for _, f := range s.Files {
			add(f, i)
		}
		for _, t := range s.Tests {
			add(t, i)
		}
	}
	dirs := make([]string, 0, len(anchors))
	for _, a := range anchors {
		dirs = append(dirs, a.Dir)
	}
	sort.Strings(dirs)
	return anchors, dirs
}

// commonSegmentDepth returns how many leading path SEGMENTS a and b share.
// Segment-aligned by construction, so "pkg/millsy" and "pkg/mills" share 1, not
// a string prefix of 9 characters.
func commonSegmentDepth(a, b string) int {
	as, bs := strings.Split(a, "/"), strings.Split(b, "/")
	n := 0
	for n < len(as) && n < len(bs) && as[n] == bs[n] {
		n++
	}
	return n
}

// ApplyAmendment returns a copy of slices with every admitted violation
// appended to its chosen slice's Files. Pure: the caller owns the CAS write, so
// a lost race can simply re-evaluate and re-apply against the fresh row.
//
// Already-declared paths are skipped (an amendment must be idempotent under
// the runner's CAS retry) and the appended path is stored exactly as the
// implement stage reported it — including an absolute spawn path, which the
// gate's absolute-suffix matching resolves on the next evaluation just as it
// does for every other declared entry.
func ApplyAmendment(slices []store.Slice, d AmendmentDecision) []store.Slice {
	if !d.Admitted {
		return slices
	}
	out := make([]store.Slice, len(slices))
	copy(out, slices)
	for i := range out {
		out[i].Files = append([]string(nil), slices[i].Files...)
	}
	for _, v := range d.Verdicts {
		if !v.Admitted || v.SliceIndex < 0 || v.SliceIndex >= len(out) {
			continue
		}
		s := &out[v.SliceIndex]
		exists := false
		for _, f := range s.Files {
			if filepath.Clean(f) == filepath.Clean(v.File) {
				exists = true
				break
			}
		}
		if !exists {
			s.Files = append(s.Files, v.File)
		}
	}
	return out
}
