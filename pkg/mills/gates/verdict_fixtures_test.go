package gates

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/crb2nu/loom/pkg/mills/store"
)

// TestGateVerdictFixtures is the canonical cross-gate verdict corpus. Keep
// these cases table driven: changes to verdict precedence or reason codes must
// be reviewed consistently across all three post-implementation gates.
func TestGateVerdictFixtures(t *testing.T) {
	t.Parallel()

	type fixture struct {
		name         string
		gate         Gate
		input        StageInput
		wantPass     bool
		wantSkip     bool
		wantJudgedBy string
		wantCode     string
		wantReason   string
	}

	item := func(files ...string) *store.BacklogItem {
		return &store.BacklogItem{Slices: []store.Slice{{Files: files}}}
	}
	judge := func(score float64, reasons ...string) *FakeRubricJudge {
		return &FakeRubricJudge{Default: RubricVerdict{
			Score: score, Reasons: reasons, Model: "fixture",
		}}
	}
	malformed := func() *FakeRubricJudge {
		return &FakeRubricJudge{Err: errors.Join(
			ErrJudgeUnparseable,
			errors.New("truncated verdict envelope"),
		)}
	}

	fixtures := []fixture{
		{
			name: "spec_conformance/clean_pass", gate: NewSpecConformanceGate(judge(0.95)),
			wantPass: true, wantJudgedBy: "flexinfer:fixture", wantCode: SpecConformanceReasonPassed,
		},
		{
			name: "spec_conformance/clean_fail", gate: NewSpecConformanceGate(judge(0.25, "required behavior is absent")),
			wantJudgedBy: "flexinfer:fixture", wantCode: SpecConformanceReasonBelowScore, wantReason: "score=0.25 below threshold=0.80",
		},
		{
			name: "spec_conformance/malformed_truncated", gate: NewSpecConformanceGate(malformed()),
			wantJudgedBy: "flexinfer:unparseable", wantCode: SpecConformanceReasonUnavailable, wantReason: "could not be parsed",
		},
		{
			name: "spec_conformance/mixed_signal_score_wins", gate: NewSpecConformanceGate(judge(0.90, "non-blocking concern remains")),
			wantPass: true, wantJudgedBy: "flexinfer:fixture", wantCode: SpecConformanceReasonPassed,
		},
		{
			name: "pr_self_review/clean_pass", gate: NewPRSelfReviewGate(judge(0.90)),
			wantPass: true, wantJudgedBy: "flexinfer:fixture", wantCode: PRSelfReviewReasonPassed,
		},
		{
			name: "pr_self_review/clean_fail", gate: NewPRSelfReviewGate(judge(0.20, "review missed a regression")),
			wantJudgedBy: "flexinfer:fixture", wantCode: PRSelfReviewReasonBelowScore, wantReason: "score=0.20 below threshold=0.70",
		},
		{
			name: "pr_self_review/malformed_truncated", gate: NewPRSelfReviewGate(malformed()),
			wantJudgedBy: "flexinfer:unparseable", wantCode: PRSelfReviewReasonUnavailable, wantReason: "could not be parsed",
		},
		{
			name: "pr_self_review/mixed_signal_score_wins", gate: NewPRSelfReviewGate(judge(0.75, "minor cleanup suggested")),
			wantPass: true, wantJudgedBy: "flexinfer:fixture", wantCode: PRSelfReviewReasonPassed,
		},
		{
			name: "scope/clean_pass", gate: &Scope{},
			input:    StageInput{Item: item("pkg/mills/gates/fixture.go"), FilesChanged: []string{"pkg/mills/gates/fixture.go"}},
			wantPass: true, wantCode: ScopeReasonInScope,
		},
		{
			name: "scope/clean_fail", gate: &Scope{},
			input:    StageInput{Item: item("pkg/mills/gates/fixture.go"), FilesChanged: []string{"pkg/mills/runner/outside.go"}},
			wantCode: ScopeReasonOutside, wantReason: "pkg/mills/runner/outside.go",
		},
		{
			name: "scope/malformed_truncated_declaration", gate: &Scope{},
			input:    StageInput{Item: &store.BacklogItem{Slices: []store.Slice{{Files: []string{""}}}}, FilesChanged: []string{"pkg/mills/gates/fixture.go"}},
			wantPass: true, wantSkip: true, wantCode: ScopeReasonNoDeclaration,
		},
		{
			name: "scope/mixed_signal_any_violation_fails", gate: &Scope{},
			input:    StageInput{Item: item("pkg/mills/gates/fixture.go"), FilesChanged: []string{"pkg/mills/gates/fixture.go", "pkg/mills/runner/outside.go"}},
			wantCode: ScopeReasonOutside, wantReason: "pkg/mills/runner/outside.go",
		},
	}

	for _, tc := range fixtures {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out, err := tc.gate.Evaluate(context.Background(), tc.input)
			if err != nil {
				t.Fatalf("Evaluate() error = %v; malformed verdicts must be handled without an error", err)
			}
			if out.Pass != tc.wantPass || out.Skip != tc.wantSkip {
				t.Fatalf("verdict = {Pass:%v Skip:%v}, want {Pass:%v Skip:%v}; outcome=%+v", out.Pass, out.Skip, tc.wantPass, tc.wantSkip, out)
			}

			joinedReasons := strings.Join(out.Reasons, "\n")
			if tc.wantJudgedBy != "" && out.JudgedBy != tc.wantJudgedBy {
				t.Errorf("JudgedBy = %q, want %q", out.JudgedBy, tc.wantJudgedBy)
			}
			if !strings.Contains(joinedReasons, "["+tc.wantCode+"]") {
				t.Errorf("Reasons = %q, want reason code [%s]", joinedReasons, tc.wantCode)
			}
			if tc.wantReason != "" && !strings.Contains(joinedReasons, tc.wantReason) {
				t.Errorf("Reasons = %q, want substring %q", joinedReasons, tc.wantReason)
			}
		})
	}
}
