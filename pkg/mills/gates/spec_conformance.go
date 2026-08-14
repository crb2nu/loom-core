package gates

import (
	"bytes"
	"context"
	"sort"
	"strconv"
	"strings"
)

// SpecConformanceRubricName is the stable rubric id the FlexInfer-backed
// RubricJudge resolves to a prompt template. The version suffix lets us
// roll the rubric forward without changing the gate name.
const SpecConformanceRubricName = "spec_conformance_v1"

const (
	SpecConformanceReasonPassed      = "spec_conformance.passed"
	SpecConformanceReasonBelowScore  = "spec_conformance.below_threshold"
	SpecConformanceReasonUnavailable = "spec_conformance.evaluation_unavailable"
)

// SpecConformanceRubric is the prompt body the production judge wraps
// around a stage diff + spec doc to produce a numeric score in [0,1].
//
// The exact text is part of the persisted contract — changing it must
// rev SpecConformanceRubricName so eval rows stay comparable.
//
// The template ends with `rubricGroundingInstructions` (anti-hallucination
// boilerplate shared with pr_self_review_v1) followed by the
// structural-output envelope so gemma4-26b returns parseable JSON even
// for small / fixture-only diffs. Live evidence from canary
// PIPE-MILLS-CANARY-M6-164007-1779036007 (2026-05-17): without these
// instructions the judge fabricated file:line references and replied
// with prose instead of the score envelope.
const SpecConformanceRubric = `You are a strict reviewer for a software engineering pipeline.

Given:
1. The spec document (or anchor) the change is supposed to implement.
2. The unified diff produced by the implementation stage.
3. The list of files changed and their slice scope.

Score how well the diff conforms to the spec on a [0.0, 1.0] scale where:
- 1.0 = every spec requirement has matching code; no extra unrelated changes.
- 0.8 = all critical requirements satisfied; minor scope deviations.
- 0.5 = partial implementation; major requirement(s) unmet.
- 0.0 = unrelated change, breaking change, or contradicts the spec.

Be terse in reasons[]. List concrete deviations only — do not restate
what the spec says.

` + rubricGroundingInstructions + `

` + rubricStructuralOutputInstructions

// NewSpecConformanceGate constructs the spec_conformance gate using the
// supplied judge. Default threshold 0.8 — verdicts at or above are pass.
//
// The spec is explicit that LLM-judged gates always run on FlexInfer;
// production callers wire a FlexInfer-backed RubricJudge here.
func NewSpecConformanceGate(judge RubricJudge) *reasonCodedLLMGate {
	var pinnedJudge RubricJudge
	if judge != nil {
		pinnedJudge = specConformanceSnapshotJudge{judge: judge}
	}
	return &reasonCodedLLMGate{LLMGate: &LLMGate{
		GateName:   "spec_conformance",
		RubricName: SpecConformanceRubricName,
		Threshold:  0.8,
		Judge:      pinnedJudge,
	}, passCode: SpecConformanceReasonPassed, failCode: SpecConformanceReasonBelowScore,
		unavailableCode: SpecConformanceReasonUnavailable}
}

// reasonCodedLLMGate adapts the shared LLM evaluator to the stable reason-code
// contract used by persisted gate verdicts. Embedding preserves the existing
// configuration surface (Threshold, Tiebreaker, Disabled, and Logger).
type reasonCodedLLMGate struct {
	*LLMGate
	passCode, failCode, unavailableCode string
}

func (g *reasonCodedLLMGate) Evaluate(ctx context.Context, in StageInput) (Outcome, error) {
	out, err := g.LLMGate.Evaluate(ctx, in)
	if err != nil {
		return out, err
	}
	code := g.failCode
	if out.Pass {
		code = g.passCode
	} else if out.JudgedBy == "flexinfer:unparseable" {
		code = g.unavailableCode
	}
	// Keep the legacy human diagnosis first (several retry consumers display
	// Reasons[0]) and append the bounded machine token as an independent field.
	out.Reasons = append(out.Reasons, "["+code+"]")
	return out, nil
}

// specConformanceSnapshotJudge pins the mutable parts of StageInput before the
// judge builds its prompt. StageInput is shared by gates, so sorting either
// caller-owned field in place would introduce both ordering flakes and races.
type specConformanceSnapshotJudge struct {
	judge RubricJudge
}

func (j specConformanceSnapshotJudge) Judge(ctx context.Context, rubric string, in StageInput) (RubricVerdict, error) {
	in.FilesChanged = append([]string(nil), in.FilesChanged...)
	sort.Strings(in.FilesChanged)
	in.DiffPatch = canonicalizeUnifiedDiff(in.DiffPatch)
	return j.judge.Judge(ctx, rubric, in)
}

type diffFileSection struct {
	path string
	body []byte
}

// canonicalizeUnifiedDiff sorts file sections by their repository path. It
// deliberately treats each section as opaque so hunk and binary-patch order is
// retained. If any diff header is malformed, the input order is preserved.
func canonicalizeUnifiedDiff(patch []byte) []byte {
	snapshot := append([]byte(nil), patch...)
	marker := []byte("diff --git ")
	starts := make([]int, 0)
	for offset := 0; offset < len(snapshot); {
		rel := bytes.Index(snapshot[offset:], marker)
		if rel < 0 {
			break
		}
		start := offset + rel
		if start == 0 || snapshot[start-1] == '\n' {
			starts = append(starts, start)
		}
		offset = start + len(marker)
	}
	if len(starts) == 0 {
		return snapshot
	}

	sections := make([]diffFileSection, 0, len(starts))
	for i, start := range starts {
		end := len(snapshot)
		if i+1 < len(starts) {
			end = starts[i+1]
		}
		lineEnd := bytes.IndexByte(snapshot[start:end], '\n')
		if lineEnd < 0 {
			lineEnd = end - start
		}
		path, ok := diffHeaderRepoPath(string(snapshot[start : start+lineEnd]))
		if !ok {
			return snapshot
		}
		sections = append(sections, diffFileSection{path: path, body: snapshot[start:end]})
	}
	sort.Slice(sections, func(i, k int) bool {
		if sections[i].path != sections[k].path {
			return sections[i].path < sections[k].path
		}
		return bytes.Compare(sections[i].body, sections[k].body) < 0
	})

	result := append([]byte(nil), snapshot[:starts[0]]...)
	for _, section := range sections {
		result = append(result, section.body...)
	}
	return result
}

func diffHeaderRepoPath(header string) (string, bool) {
	rest := strings.TrimPrefix(header, "diff --git ")
	if rest == header {
		return "", false
	}
	left, rest, ok := nextDiffHeaderPath(rest)
	if !ok {
		return "", false
	}
	right, trailing, ok := nextDiffHeaderPath(rest)
	if !ok || strings.TrimSpace(trailing) != "" {
		return "", false
	}
	path := right
	if path == "/dev/null" {
		path = left
	}
	path = strings.TrimPrefix(strings.TrimPrefix(path, "a/"), "b/")
	return path, path != "" && path != "/dev/null"
}

func nextDiffHeaderPath(input string) (string, string, bool) {
	input = strings.TrimLeft(input, " \t")
	if input == "" {
		return "", "", false
	}
	if input[0] != '"' {
		if end := strings.IndexAny(input, " \t"); end >= 0 {
			return input[:end], input[end:], true
		}
		return input, "", true
	}
	for i := 1; i < len(input); i++ {
		if input[i] == '\\' {
			i++
			continue
		}
		if input[i] == '"' {
			path, err := strconv.Unquote(input[:i+1])
			return path, input[i+1:], err == nil
		}
	}
	return "", input, false
}
