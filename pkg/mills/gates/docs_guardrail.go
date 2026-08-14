package gates

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// DocsGuardrail fails when the implement stage produced a code-facing change
// without a matching documentation update — the exact condition the repo's CI
// `guardrails:docs-cli` job (scripts/ci/check_docs_guardrails.sh) enforces at
// the ci_watch stage. Historically every real council run that touched code
// (e.g. *.go) but did not also add a CHANGELOG/README/docs entry sailed through
// the post-implement gates, opened an MR, and then failed CI on
// `guardrails:docs-cli` — the run escalated at ci_watch after burning the whole
// pipeline (MRs !1111/!1112, $2.02, 2026-07-17). This gate replicates the CI
// script's classification locally so the failure is caught in
// post_implement_gate BEFORE the MR is opened; with the stage's RetryFrom:
// "implement" the gate-fail retry re-runs implement (which is standingly
// instructed to add a CHANGELOG entry — see implementDocsDiscipline) instead of
// paying for a doomed MR + CI round.
//
// The gate is deliberately a RETRYABLE fail (Terminal stays false): the fix is
// a one-line CHANGELOG entry the retry can add, not a re-plan.
//
// Semantics mirror scripts/ci/check_docs_guardrails.sh exactly:
//   - a change is "code-facing" if it matches docsGuardrailCodeRE
//     (cmd/, internal/, pkg/, scripts/, Makefile, go.mod, go.sum,
//     .gitlab-ci.yml, .github/workflows/);
//   - generated/build artifacts and test-only files (docsGuardrailGeneratedRE:
//     /dist/, /testdata/, *_golden.*, *.min.js/css, *.snap, *_test.go,
//     *_test.py, test_*.py, *_mock.go, /mocks/) are excluded — they are not
//     user-visible, so the CI log for MR !1111 showed health_policy_test.go and
//     golden fixtures ignored;
//   - the requirement is satisfied by any change to README.md, CHANGELOG.md,
//     ROADMAP.md, AGENTS.md, docs/**, or a per-MR changelog fragment under
//     changelog.d/** (docsGuardrailSatisfyRE) — the preferred fix, since
//     fragments are the collision-free replacement for direct CHANGELOG.md
//     edits (see changelog.d/README.md), folded into CHANGELOG.md at release;
//   - a `[skip-docs-check]` token in any commit message opts the change out.
type DocsGuardrail struct {
	// classify is a test seam for exercising quorum disagreement. Production
	// instances leave it nil and use classifyDocsGuardrail.
	classify func(StageInput) docsGuardrailVerdict
}

type docsGuardrailStatus string

const (
	docsGuardrailPass   docsGuardrailStatus = "pass"
	docsGuardrailFail   docsGuardrailStatus = "fail"
	docsGuardrailOptOut docsGuardrailStatus = "opt_out"

	DocsGuardrailReasonNoCodeChanges  = "docs_guardrail.no_code_changes"
	DocsGuardrailReasonDocsPresent    = "docs_guardrail.docs_present"
	DocsGuardrailReasonOptOut         = "docs_guardrail.opt_out"
	DocsGuardrailReasonMissingDocs    = "docs_guardrail.missing_docs"
	DocsGuardrailReasonQuorumDisagree = "docs_guardrail.quorum_disagreement"
)

// docsGuardrailVerdict is the deterministic, structured result produced before
// it is adapted to the shared Outcome contract. Borderline marks inputs whose
// classification required absolute-path suffix matching; those receive exactly
// one independent re-evaluation before an Outcome is returned.
type docsGuardrailVerdict struct {
	Status     docsGuardrailStatus
	ReasonCode string
	Detail     string
	Borderline bool
}

// Name identifies the gate in persistence + logs.
func (g *DocsGuardrail) Name() string { return "docs_guardrail" }

// docsGuardrailCodeRE is the `code_pattern` from
// scripts/ci/check_docs_guardrails.sh, anchored at a repo-relative path start.
var docsGuardrailCodeRE = regexp.MustCompile(
	`^(cmd/|internal/|pkg/|scripts/|Makefile$|go\.mod$|go\.sum$|\.gitlab-ci\.yml$|\.github/workflows/)`)

// docsGuardrailSatisfyRE is the `docs_pattern` from the CI script: the set of
// paths that count as a documentation update. Root-anchored, matching the CI
// exactly, so it is stricter than the scope gate's isDocsGuardrailFile (which
// matches a doc basename anywhere): being faithful here avoids passing a diff CI
// would fail. (Absolute spawn paths are handled by matchDocsGuardrailAnchored,
// which necessarily degrades to a generous suffix match — but that only widens
// what satisfies the requirement, and CI remains the backstop.)
var docsGuardrailSatisfyRE = regexp.MustCompile(
	`^(README\.md$|CHANGELOG\.md$|ROADMAP\.md$|AGENTS\.md$|docs/|changelog\.d/)`)

// docsGuardrailGeneratedRE is the `generated_pattern` from the CI script:
// build artifacts and test-only files that match code_pattern but need no
// user-facing docs. Unanchored (substring/suffix), so it applies identically to
// repo-relative and absolute spawn paths.
var docsGuardrailGeneratedRE = regexp.MustCompile(
	`(/dist/|/testdata/|_golden\.|\.min\.js$|\.min\.css$|\.snap$|_test\.go$|_test\.py$|(^|/)test_[^/]+\.py$|_mock\.go$|/mocks/)`)

// docsSkipTokenRE matches the `[skip-docs-check]` escape hatch
// case-insensitively, mirroring the CI script's `grep -qi '\[skip-docs-check\]'`.
var docsSkipTokenRE = regexp.MustCompile(`(?i)\[skip-docs-check\]`)

// Evaluate replicates check_docs_guardrails.sh over the run's diff. It passes
// when there are no significant (non-generated) code-facing changes, when a
// documentation file is also touched, or when a commit opts out via
// `[skip-docs-check]`; it fails — retryably, with a remediation-oriented reason
// that the gate-retry implement attempt sees via StageRetryContext — when
// code-facing changes carry no documentation update.
func (g *DocsGuardrail) Evaluate(_ context.Context, in StageInput) (Outcome, error) {
	classify := g.classify
	if classify == nil {
		classify = classifyDocsGuardrail
	}

	first := classify(in)
	if !first.Borderline {
		return docsGuardrailOutcome(first), nil
	}

	// A borderline verdict gets one, and only one, re-evaluation. Two equal
	// votes form the bounded quorum; any disagreement fails closed.
	second := classify(in)
	if !sameDocsGuardrailVerdict(first, second) {
		return docsGuardrailOutcome(docsGuardrailVerdict{
			Status:     docsGuardrailFail,
			ReasonCode: DocsGuardrailReasonQuorumDisagree,
			Detail: fmt.Sprintf(
				"borderline docs classification disagreed on re-evaluation: first=%s/%s second=%s/%s",
				first.Status, first.ReasonCode, second.Status, second.ReasonCode,
			),
		}), nil
	}
	return docsGuardrailOutcome(first), nil
}

func classifyDocsGuardrail(in StageInput) docsGuardrailVerdict {
	// Escape hatch: an explicit [skip-docs-check] in any commit message opts the
	// change out exactly as the CI job does. Recorded as a pass carrying the
	// reason so the opt-out stays auditable in gate_outcomes.
	commitMessages := append([]string(nil), in.CommitMessages...)
	sort.Strings(commitMessages)
	for _, msg := range commitMessages {
		if docsSkipTokenRE.MatchString(msg) {
			return docsGuardrailVerdict{
				Status:     docsGuardrailOptOut,
				ReasonCode: DocsGuardrailReasonOptOut,
				Detail:     "skipped via [skip-docs-check] commit trailer",
			}
		}
	}

	var significant []string
	docsChanged := false
	borderline := false
	for _, changed := range in.FilesChanged {
		borderline = borderline || filepath.IsAbs(filepath.Clean(changed))
	}
	for _, cleaned := range canonicalGatePaths(in.FilesChanged) {
		if matchDocsGuardrailAnchored(docsGuardrailSatisfyRE, cleaned) {
			docsChanged = true
		}
		if matchDocsGuardrailAnchored(docsGuardrailCodeRE, cleaned) &&
			!docsGuardrailGeneratedRE.MatchString(cleaned) {
			significant = append(significant, cleaned)
		}
	}

	if len(significant) == 0 {
		return docsGuardrailVerdict{
			Status:     docsGuardrailPass,
			ReasonCode: DocsGuardrailReasonNoCodeChanges,
			Detail:     "no code-facing changes require documentation",
			Borderline: borderline,
		}
	}
	if docsChanged {
		return docsGuardrailVerdict{
			Status:     docsGuardrailPass,
			ReasonCode: DocsGuardrailReasonDocsPresent,
			Detail:     "documentation update accompanies code-facing changes",
			Borderline: borderline,
		}
	}

	const maxRendered = 6
	rendered := significant
	suffix := ""
	if len(rendered) > maxRendered {
		suffix = fmt.Sprintf(" (and %d more)", len(rendered)-maxRendered)
		rendered = rendered[:maxRendered]
	}
	return docsGuardrailVerdict{
		Status:     docsGuardrailFail,
		ReasonCode: DocsGuardrailReasonMissingDocs,
		Detail: fmt.Sprintf(
			"code-facing changes lack a documentation update: %d file(s) require a doc entry (%s%s). "+
				"Add a changelog fragment file at changelog.d/<slug>.<category>.md (category is one of "+
				"added|changed|deprecated|removed|fixed|security; slug is unique per MR, e.g. the branch name; "+
				"body is the Keep a Changelog bullet) describing this change — this is the preferred fix and "+
				"avoids CHANGELOG.md merge collisions. Do NOT edit CHANGELOG.md directly. "+
				"Alternatively include [skip-docs-check] in the commit message if the change is intentionally "+
				"undocumented. Expected a change under changelog.d/, or in README.md, CHANGELOG.md, ROADMAP.md, "+
				"AGENTS.md, or docs/.",
			len(significant), strings.Join(rendered, ", "), suffix,
		),
		Borderline: borderline,
	}
}

func sameDocsGuardrailVerdict(a, b docsGuardrailVerdict) bool {
	return a.Status == b.Status &&
		a.ReasonCode == b.ReasonCode &&
		a.Detail == b.Detail &&
		a.Borderline == b.Borderline
}

func docsGuardrailOutcome(verdict docsGuardrailVerdict) Outcome {
	reason := fmt.Sprintf("[%s] %s", verdict.ReasonCode, verdict.Detail)
	switch verdict.Status {
	case docsGuardrailPass, docsGuardrailOptOut:
		out := pass()
		out.Reasons = []string{reason}
		return out
	default:
		return fail(reason)
	}
}

// matchDocsGuardrailAnchored reports whether a changed path matches an
// ^-anchored, repo-relative CI pattern. Spawn stages (k8s pod / harvester-vm)
// report ABSOLUTE in-pod/VM paths (…/loom-core/pkg/mills/x.go) while the CI
// patterns anchor to repo-relative paths, so an absolute path is tested against
// each of its path-segment suffixes — mirroring the scope gate's
// matchGlobMaybeAbs. Without this the code check would silently pass every
// spawn-driven implement (the exact substrate Mills runs on), defeating the
// gate.
func matchDocsGuardrailAnchored(re *regexp.Regexp, cleaned string) bool {
	if re.MatchString(cleaned) {
		return true
	}
	if !filepath.IsAbs(cleaned) {
		return false
	}
	segs := strings.Split(cleaned, "/")
	for i := 1; i < len(segs); i++ {
		if re.MatchString(strings.Join(segs[i:], "/")) {
			return true
		}
	}
	return false
}
