package gates

import (
	"context"
	"path/filepath"
	"sort"
	"strings"

	"github.com/crb2nu/loom/pkg/mills/store"
)

const (
	ScopeReasonInScope       = "scope.in_scope"
	ScopeReasonNoInput       = "scope.no_input"
	ScopeReasonBootstrapped  = "scope.bootstrapped"
	ScopeReasonNoDeclaration = "scope.no_declaration"
	ScopeReasonOutside       = "scope.outside_declared_scope"
)

// Scope fails when the implement stage modified files outside the slice's
// declared scope. The council emits each backlog item with a per-slice
// `files` + `tests` allowlist; the implement worker is supposed to stay
// inside that envelope. A scope violation usually means either:
//
//   - the worker took an unrelated detour (escalate; the plan needs to be
//     re-decomposed by the next council run), or
//   - the slice list was incomplete (escalate; humans should re-author it).
//
// Either way the right move is to stop the pipeline rather than auto-merge.
type Scope struct {
	// AllowTests opts out of the gate for files matching common test
	// extensions/paths even when they're not in the slice's `tests` list.
	// Keeps the gate from over-firing on incidental fixture renames; defaults
	// to true because the cost of a false-positive escalation is high.
	AllowTests bool
}

// Name returns the gate identifier.
func (g *Scope) Name() string { return "scope" }

// Evaluate compares in.FilesChanged to the union of every slice's files +
// tests on the backlog item.
func (g *Scope) Evaluate(_ context.Context, in StageInput) (Outcome, error) {
	if in.Item == nil {
		// Without an item we have no scope to compare against; treat as a
		// pass so the gate doesn't block stages that legitimately run
		// before the item is materialised (e.g. a dryrun smoke).
		return codedPass(ScopeReasonNoInput), nil
	}
	if len(in.FilesChanged) == 0 {
		return codedPass(ScopeReasonNoInput), nil
	}
	allowed := g.allowedFor(in.Item)
	if allowed.empty() {
		if in.ProjectBootstrapped {
			// The item targets a runtime-minted repo (bootstrapped_projects
			// registry). Its plan was authored before the repo existed, so
			// slices carry no file paths and the emitted item is slice-less
			// by construction — and a freshly-seeded repo has nothing for
			// the scope envelope to protect. Pass with a recorded warning
			// instead of wedging the whole bootstrapped plan (escalations
			// #272–#278: every item of the first bootstrapped project
			// burned 3 implement attempts on this fail).
			out := codedPass(ScopeReasonBootstrapped)
			out.Reasons = append(out.Reasons, "target project is bootstrapped; slice-less item allowed")
			return out, nil
		}
		// No slices and not a canary => no scope can be enforced. This is not
		// a code defect in the diff — the item simply carries nothing for the
		// envelope to protect — so it is advisory, not a fail: return a skip
		// with the drift reason so the operator can re-decompose without the
		// run escalating on a false scope failure. (Previously a terminal fail;
		// live 2026-07-16 it false-failed slice-less items whose diffs were
		// fine.) A skip does not block the pipeline and does not count against
		// gate_pass_rate.
		return codedSkip(ScopeReasonNoDeclaration, "backlog item has no slices; no scope to enforce"), nil
	}

	violations := g.violationsIn(in, allowed)
	if len(violations) == 0 {
		return codedPass(ScopeReasonInScope), nil
	}
	reasons := make([]string, len(violations))
	for i, violation := range violations {
		// Keep one path per reason so persisted verdict consumers can
		// identify every undeclared path without parsing or expanding a
		// capped prose summary.
		reasons[i] = "[" + ScopeReasonOutside + "] file outside slice scope: " + violation
	}
	return Outcome{Reasons: reasons, JudgedBy: "go"}, nil
}

func codedPass(code string) Outcome {
	out := pass()
	out.Reasons = []string{"[" + code + "]"}
	return out
}

func codedSkip(code, detail string) Outcome {
	out := skip(detail)
	out.Reasons = append(out.Reasons, "["+code+"]")
	return out
}

// allowedFor builds the scope envelope for an item: every slice's files +
// tests, plus the canary fixture carve-out. Split out of Evaluate so the
// runner's scope-amendment path (ScopeViolations) computes the SAME envelope
// rather than a look-alike reimplementation of it.
func (g *Scope) allowedFor(item *store.BacklogItem) allowedSet {
	allowed := buildAllowedSet(item.Slices, g.AllowTests || true)
	if itemHasCanaryLabel(item) {
		// Deterministic canaries edit the heartbeat fixture by design;
		// council-emitted slice lists historically omit it, tripping the
		// gate on every canary run (escalations #151/#163), and a canary
		// may carry NO slices at all when plan_slice fails to persist one
		// (escalation 2026-06-23, PIPE-MILLS-CANARY-20260623-235142). Seed
		// the canary carve-out BEFORE the empty check so a slice-less
		// canary still enforces the narrow fixture allowlist instead of
		// escalating. The allowlist applies only to CanaryLabel items.
		// (Since the testdata/ carve-out in looksLikeTestFile, a real item
		// touching the fixture passes as test collateral anyway; the seed
		// here still matters for slice-less canaries, which would otherwise
		// hit the empty-allowlist terminal fail above.)
		for _, p := range canaryAllowedPaths {
			allowed.add(p)
		}
	}
	return allowed
}

// violationsIn returns the changed files that fall outside allowed, sorted and
// de-duplicated. Callers must have established that allowed is non-empty.
func (g *Scope) violationsIn(in StageInput, allowed allowedSet) []string {
	var violations []string
	for _, cleaned := range canonicalGatePaths(in.FilesChanged) {
		if isAllowed(cleaned, allowed, g.AllowTests || true) {
			continue
		}
		violations = append(violations, cleaned)
	}
	return violations
}

// ScopeViolations recomputes the full, uncapped list of changed files outside
// the item's declared slice envelope, using the EXACT rules Scope.Evaluate
// applies (same allowed set, same carve-outs, same de-duplication).
//
// The runner needs the whole list to decide whether a scope-only gate failure
// is auto-amendable (see EvaluateScopeAmendment), and the persisted gate
// Reasons are intended for verdict reporting rather than scope amendment.
// Parsing reason strings back into paths would be a second, drifting
// implementation of the envelope, so this returns the same slice Evaluate
// itself computed from.
//
// Returns nil when there is no enforceable envelope (no item, no changed
// files, or a slice-less item, all of which Evaluate resolves to pass/skip).
func ScopeViolations(in StageInput) []string {
	if in.Item == nil || len(in.FilesChanged) == 0 {
		return nil
	}
	g := &Scope{AllowTests: true}
	allowed := g.allowedFor(in.Item)
	if allowed.empty() {
		return nil
	}
	return g.violationsIn(in, allowed)
}

// canaryAllowedPaths are the fixture files a deterministic Mills canary
// (an item carrying CanaryLabel) modifies by design. Kept to exact
// repo-relative paths — no globs — so the canary carve-out stays as
// narrow as the canary itself.
var canaryAllowedPaths = []string{
	"testdata/mills-canary/heartbeat.md",
}

// allowedSet keeps the literal allowlisted paths plus any directories
// implied by glob patterns. Lookups are O(1) for literal hits; glob
// fallbacks (filepath.Match) are quadratic but n is small.
type allowedSet struct {
	literals map[string]struct{}
	globs    []string
	globSet  map[string]struct{}
	// dirs holds the parent directory of every literal allowlisted path.
	// Slice `files` name files that often DO NOT EXIST YET — the council
	// decomposes work before it is written, and its guessed basenames are
	// systematically near-misses of what the implement agent actually
	// creates (2026-07-08: slice said pkg/mills/pipeline/escalation.go,
	// the agent correctly edited escalate.go; slice said
	// pkg/mills/council/classifier.go, the agent created
	// ci_incident_classifier.go — 8 of the day's 19 escalations were this
	// exact-basename mismatch). The council can pin a directory reliably
	// (SanitizeProposalSlices grounds parents against the real tree) but
	// not a basename, so the enforceable envelope is the directory: a
	// changed file inside or below a slice-declared file's parent directory
	// is in scope. Parent and unrelated directories still violate,
	// preserving the gate's unrelated-detour purpose.
	dirs map[string]struct{}
}

func buildAllowedSet(slices []store.Slice, includeTests bool) allowedSet {
	set := allowedSet{
		literals: make(map[string]struct{}),
		globSet:  make(map[string]struct{}),
		dirs:     make(map[string]struct{}),
	}
	for _, s := range slices {
		for _, f := range s.Files {
			set.add(f)
		}
		if includeTests {
			for _, t := range s.Tests {
				set.add(t)
			}
		}
	}
	sort.Strings(set.globs)
	return set
}

func (s *allowedSet) empty() bool {
	return len(s.literals) == 0 && len(s.globs) == 0
}

func (s *allowedSet) add(path string) {
	if path == "" {
		return
	}
	path = canonicalGatePath(path)
	if strings.ContainsAny(path, "*?[") {
		if _, exists := s.globSet[path]; exists {
			return
		}
		s.globSet[path] = struct{}{}
		s.globs = append(s.globs, path)
		return
	}
	cleaned := path
	s.literals[cleaned] = struct{}{}
	// "." (a top-level file's parent) would turn the envelope into an
	// allow-anything-at-repo-root grant; root files are covered by the
	// docs-guardrail and dep-manifest carve-outs instead.
	if dir := filepath.Dir(cleaned); dir != "." && dir != "/" {
		s.dirs[dir] = struct{}{}
	}
}

// canonicalGatePath gives gate inputs one stable spelling before comparison,
// de-duplication, sorting, or rendering. Upstream implementations can report
// the same repository file as a relative path, a cleaned relative path, or an
// absolute spawn-worktree path. The gates have no explicit repository root, so
// absolute paths are reduced at the first repository-shaped top-level segment.
// Unknown absolute paths remain absolute rather than guessing.
func canonicalGatePath(path string) string {
	// Git and slice metadata use slash-separated repository paths, but spawn
	// telemetry can originate on Windows and retain backslashes after it is
	// shipped to a Unix runner. Normalize separators before Clean so the gate's
	// verdict does not depend on which host captured the path.
	path = strings.ReplaceAll(path, `\`, "/")
	cleaned := filepath.Clean(path)
	if !isAbsoluteGatePath(cleaned) {
		return cleaned
	}

	segments := strings.Split(strings.TrimPrefix(cleaned, "/"), "/")
	if len(segments) > 0 && isRepositoryRootFile(segments[len(segments)-1]) {
		return segments[len(segments)-1]
	}
	for i, segment := range segments {
		if isRepositoryPathRoot(segment) {
			return filepath.Join(segments[i:]...)
		}
	}
	return cleaned
}

// isAbsoluteGatePath recognizes both native absolute paths and Windows drive
// paths after separator normalization. filepath.IsAbs intentionally follows
// the runner OS, which otherwise makes C:/... relative when evaluated on
// Linux.
func isAbsoluteGatePath(path string) bool {
	return filepath.IsAbs(path) ||
		(len(path) >= 3 && path[1] == ':' && path[2] == '/' &&
			((path[0] >= 'a' && path[0] <= 'z') || (path[0] >= 'A' && path[0] <= 'Z')))
}

// canonicalGatePaths returns one sorted spelling for every distinct input
// path. Both deterministic gates use this before classification so telemetry
// order, duplicate events, and relative/absolute spellings cannot affect a
// verdict or its rendered reason order.
func canonicalGatePaths(paths []string) []string {
	canonical := make([]string, 0, len(paths))
	seen := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		cleaned := canonicalGatePath(path)
		if _, duplicate := seen[cleaned]; duplicate {
			continue
		}
		seen[cleaned] = struct{}{}
		canonical = append(canonical, cleaned)
	}
	sort.Strings(canonical)
	return canonical
}

func isRepositoryRootFile(name string) bool {
	switch name {
	case "Makefile", "go.mod", "go.sum", "go.work", "go.work.sum",
		".gitlab-ci.yml", "README.md", "CHANGELOG.md", "ROADMAP.md", "AGENTS.md":
		return true
	default:
		return false
	}
}

func isRepositoryPathRoot(segment string) bool {
	switch segment {
	case "cmd", "internal", "pkg", "scripts", ".github", "docs", "changelog.d", "testdata", "tests", "test":
		return true
	default:
		return false
	}
}

func isAllowed(path string, allowed allowedSet, allowTests bool) bool {
	cleaned := filepath.Clean(path)
	if _, ok := allowed.literals[cleaned]; ok {
		return true
	}
	// Directory envelope: in scope when the changed file sits in — or under —
	// the directory of a slice-declared file (see allowedSet.dirs for why
	// exact basenames cannot be enforced). Descendants are included because
	// the council cannot predict a package's internal layout either: the
	// first run under the same-dir-only envelope (2026-07-08,
	// PIPE-…emit-classification…-1783538288) failed on a REQUIRED SQL
	// migration in pkg/mills/store/migrations/ while the slice declared
	// pkg/mills/store/store.go — a file the agent could neither omit nor
	// relocate, so the run could only escalate. Unrelated packages still
	// violate. Absolute spawn paths match a repo-relative allowed dir on a
	// path-segment boundary, mirroring the literal fallback below.
	if dir := filepath.Dir(cleaned); dir != "." && dir != "/" {
		for d := range allowed.dirs {
			if dir == d || strings.HasPrefix(dir, d+"/") {
				return true
			}
			if isAbsoluteGatePath(cleaned) && !isAbsoluteGatePath(d) &&
				(strings.HasSuffix(dir, "/"+d) || strings.Contains(dir, "/"+d+"/")) {
				return true
			}
		}
	}
	// Spawn-driven stages (k8s pod / harvester-vm) report ABSOLUTE changed
	// paths under the in-pod/VM workdir (e.g.
	// /workspace/services/loom-core/testdata/mills-canary/heartbeat.md), but
	// slice.files + canaryAllowedPaths are repo-relative
	// (testdata/mills-canary/heartbeat.md) and run.WorktreePath is empty for
	// spawns, so the literal compare never matches and the gate false-fails on
	// every spawn implement (Mills A2 canary 2026-06-19: real heartbeat.md
	// commit flagged "outside slice scope" → retried → empty-diff cascade →
	// escalation). Match an absolute changed path when it ends with a
	// repo-relative allowed path on a path-segment boundary.
	if isAbsoluteGatePath(cleaned) {
		for lit := range allowed.literals {
			if !isAbsoluteGatePath(lit) && lit != "." && strings.HasSuffix(cleaned, "/"+lit) {
				return true
			}
		}
	}
	for _, pat := range allowed.globs {
		if matchGlobMaybeAbs(pat, cleaned) {
			return true
		}
	}
	if allowTests && looksLikeTestFile(cleaned) {
		return true
	}
	// Docs-guardrail carve-out (always on): the repo's CI docs guardrail
	// (scripts/ci/check_docs_guardrails.sh, enforced at ci_watch) REQUIRES any
	// code-facing MR to also touch README/CHANGELOG/ROADMAP/AGENTS or docs/,
	// and the implement spawn is instructed to add a CHANGELOG entry to satisfy
	// it (implementDocsDiscipline). Council slice `files` lists never include
	// that CHANGELOG entry, so without this carve-out the two gates deadlock:
	// the docs file is mandatory downstream but "outside slice scope" here
	// (observed on PIPE-MILLS-2026-06-30-001-…487, which escalated on
	// "1 file(s) outside slice scope: CHANGELOG.md" right after implement added
	// it — exactly as the docs fix intended). Mirrors the guardrail's allowlist.
	if isDocsGuardrailFile(cleaned) {
		return true
	}
	// Auto-managed dependency manifests carve-out (always on): a legitimate
	// code change that adds/removes an import makes the toolchain rewrite
	// go.sum/go.mod (via `go build`/`go test`/`go mod tidy`), and the same is
	// true of npm/cargo/etc. lockfiles. The implement agent does not "author"
	// these — they are deterministic side-effects of building the in-scope code
	// — yet a council/plan slice's `files` never lists them, so without this a
	// real feature that touches imports escalates at the scope gate. Out-of-
	// scope *source* files still fail (the carve-out is manifest-only).
	if isAutoManagedDepFile(cleaned) {
		return true
	}
	return false
}

// matchGlobMaybeAbs reports whether a changed path matches a repo-relative glob
// pattern, trying the absolute path's repo-relative suffixes too. Spawn stages
// report ABSOLUTE in-pod/VM paths (…/loom-core/cmd/deploy.go) while slice globs
// are repo-relative (cmd/*.go), and filepath.Match never crosses "/", so a
// repo-relative glob can never match an absolute path directly — the same
// false-fail the literal absolute-path fallback in isAllowed guards against, but
// for globs. Match the pattern against each path-segment suffix of an absolute
// path.
func matchGlobMaybeAbs(pat, cleaned string) bool {
	if matched, _ := filepath.Match(pat, cleaned); matched {
		return true
	}
	for _, suffix := range pathSegmentSuffixes(cleaned) {
		if matched, _ := filepath.Match(pat, suffix); matched {
			return true
		}
	}
	return false
}

// pathSegmentSuffixes returns every repo-relative READING of an absolute spawn
// path: each path-segment suffix, longest first. A spawn stage reports
// /workspace/services/loom-core/pkg/mills/store/dao.go while every declared
// slice path is repo-relative (pkg/mills/store/dao.go), and nothing in the
// StageInput tells the gate where the repo root sits inside the pod — so the
// only sound comparison is "does some segment-aligned suffix match?".
// Returns nil for an already-relative path (the caller compares that directly).
func pathSegmentSuffixes(cleaned string) []string {
	if !isAbsoluteGatePath(cleaned) {
		return nil
	}
	segs := strings.Split(cleaned, "/")
	out := make([]string, 0, len(segs))
	for i := 1; i < len(segs); i++ {
		out = append(out, strings.Join(segs[i:], "/"))
	}
	return out
}

// autoManagedDepFiles are package-manager manifests/lockfiles that build tools
// rewrite as a side-effect of an in-scope source change. Keyed by basename so
// the absolute spawn paths the gate sees (…/loom-core/go.sum) match too.
var autoManagedDepFiles = map[string]struct{}{
	"go.mod": {}, "go.sum": {}, "go.work": {}, "go.work.sum": {},
	"package-lock.json": {}, "npm-shrinkwrap.json": {}, "yarn.lock": {}, "pnpm-lock.yaml": {},
	"Cargo.lock": {}, "poetry.lock": {}, "Pipfile.lock": {}, "uv.lock": {},
	"composer.lock": {}, "Gemfile.lock": {},
}

// isAutoManagedDepFile reports whether a changed path is a toolchain-managed
// dependency manifest/lockfile (see autoManagedDepFiles).
func isAutoManagedDepFile(path string) bool {
	_, ok := autoManagedDepFiles[filepath.Base(filepath.Clean(path))]
	return ok
}

// docsGuardrailBasenames are the repo-root documentation files the CI docs
// guardrail accepts as a "doc update" for a code-facing change. Mirrors the
// `docs_pattern` in scripts/ci/check_docs_guardrails.sh.
var docsGuardrailBasenames = map[string]struct{}{
	"README.md":    {},
	"CHANGELOG.md": {},
	"ROADMAP.md":   {},
	"AGENTS.md":    {},
}

// isDocsGuardrailFile reports whether a changed path is one the docs guardrail
// mandates: one of the named root docs, anything under a docs/ directory, or a
// per-MR changelog fragment under changelog.d/. The changelog.d/ carve-out is
// load-bearing: the implement spawn is now instructed (implementDocsDiscipline)
// to satisfy the docs guardrail by WRITING a changelog.d/<slug>.<category>.md
// fragment, and a council/plan slice's `files` list never enumerates that
// fragment — so without this carve-out the docs_guardrail and scope gates would
// deadlock exactly as they did for the CHANGELOG.md entry (see the carve-out
// call site above). Accepts absolute spawn paths (e.g.
// /workspace/.../loom-core/changelog.d/x.added.md) by matching on the basename /
// a docs- or changelog.d/ path segment — a nested doc that wouldn't actually
// satisfy the root-anchored guardrail still fails downstream at ci_watch, so a
// generous scope carve-out here can't smuggle a non-doc change through (the cost
// of a false-positive escalation, which this prevents, is the higher risk per
// this gate's design).
func isDocsGuardrailFile(path string) bool {
	cleaned := filepath.Clean(path)
	if _, ok := docsGuardrailBasenames[filepath.Base(cleaned)]; ok {
		return true
	}
	if cleaned == "docs" || strings.HasPrefix(cleaned, "docs/") || strings.Contains(cleaned, "/docs/") {
		return true
	}
	if strings.HasPrefix(cleaned, "changelog.d/") || strings.Contains(cleaned, "/changelog.d/") {
		return true
	}
	return false
}

// looksLikeTestFile recognises the common test-file conventions across the
// languages this workspace uses so a slice that adds a fixture under
// `_test.go` / `*.test.ts` / `tests/...` doesn't trip the gate.
func looksLikeTestFile(path string) bool {
	switch {
	case strings.HasSuffix(path, "_test.go"):
		return true
	case strings.HasSuffix(path, ".test.ts"), strings.HasSuffix(path, ".test.tsx"),
		strings.HasSuffix(path, ".spec.ts"), strings.HasSuffix(path, ".spec.tsx"):
		return true
	case strings.HasSuffix(path, ".test.js"), strings.HasSuffix(path, ".spec.js"):
		return true
	case strings.HasPrefix(path, "tests/"), strings.Contains(path, "/tests/"),
		strings.HasPrefix(path, "test/"), strings.Contains(path, "/test/"):
		return true
	case strings.HasPrefix(path, "testdata/"), strings.Contains(path, "/testdata/"):
		// Go test fixtures (golden files etc.). Slices never list them and
		// the toolchain ignores them at build time; without this a golden
		// added next to an in-scope test escalated at the gate
		// (PIPE-…-propagate-failure-classification…, 2026-07-08:
		// internal/contracts/testdata/escalation_failure_classification.golden).
		return true
	case strings.HasSuffix(path, "_test.py"), strings.HasPrefix(filepath.Base(path), "test_"):
		return true
	}
	return false
}
