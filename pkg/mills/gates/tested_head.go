package gates

import (
	"context"
	"fmt"
	"strings"
)

// TestedHeadGateName is the registry name of TestedHeadGate. The runner keys
// its post-review re-test rewind on this exact name, so it lives here as a
// constant instead of as a string literal in two packages.
const TestedHeadGateName = "tested_head"

// TestedHeadGate is the deterministic post_review_gate check that the branch
// head the pipeline is about to open an MR for is the exact revision the
// tests stage verified.
//
// pr_self_review may push fixes, but a review-authored commit is untested by
// construction: on 2026-09-12 (PIPE-bl-mills-mergequeue-ci-lane-variables-…)
// the review pushed a second commit that broke a test the tests stage had
// just passed, and mr/ci_watch shipped that head straight into a red CI
// escalation. A mismatch fails with both SHAs so Runner.Drive can re-dispatch
// tests for the review head instead of respawning review or implement.
//
// Either SHA missing is an advisory skip, not a failure: legacy stage rows
// and workers without head resolution have nothing to compare, and failing
// them would rewind every such run into a re-test it can never satisfy.
type TestedHeadGate struct{}

func (*TestedHeadGate) Name() string { return TestedHeadGateName }

func (*TestedHeadGate) Evaluate(_ context.Context, in StageInput) (Outcome, error) {
	tested, head := normalizeTestedSHA(in.TestedSHA), normalizeTestedSHA(in.ReviewHeadSHA)
	if tested == "" || head == "" {
		return Outcome{
			Pass: true, Skip: true, JudgedBy: "go",
			Reasons: []string{fmt.Sprintf("tested_head unresolved: tested_sha=%q review_head_sha=%q", tested, head)},
		}, nil
	}
	if tested != head {
		return Outcome{
			Pass: false, JudgedBy: "go",
			Reasons: []string{fmt.Sprintf("tested_head mismatch: tested_sha=%s review_head_sha=%s", tested, head)},
		}, nil
	}
	return Outcome{Pass: true, JudgedBy: "go"}, nil
}

// normalizeTestedSHA lower-cases a git object id and maps the tests stage's
// "unresolved" sentinel (pipeline.testedSHAOrUnresolved) to the empty string
// so both spellings of "unknown" read the same.
func normalizeTestedSHA(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "unresolved" {
		return ""
	}
	return s
}
