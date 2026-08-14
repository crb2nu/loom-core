package gates

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// ---------- DocsGuardrail ----------

// TestDocsGuardrail_Table exercises the CI-mirrored classification from
// scripts/ci/check_docs_guardrails.sh: a code-facing change without a doc
// update fails, the same change plus any doc update passes, and docs-only,
// test-only, generated-only, and opted-out diffs all pass.
func TestDocsGuardrail_Table(t *testing.T) {
	cases := []struct {
		name     string
		files    []string
		commits  []string
		wantPass bool
	}{
		{
			name:     "code-facing without docs fails",
			files:    []string{"pkg/mills/gates/docs_guardrail.go", "cmd/loom-mills-operator/main.go"},
			wantPass: false,
		},
		{
			name:     "code plus CHANGELOG passes",
			files:    []string{"pkg/mills/gates/docs_guardrail.go", "CHANGELOG.md"},
			wantPass: true,
		},
		{
			name:     "code plus docs dir file passes",
			files:    []string{"pkg/mills/x.go", "docs/MILLS.md"},
			wantPass: true,
		},
		{
			name:     "code plus changelog fragment passes",
			files:    []string{"pkg/mills/x.go", "changelog.d/feat-x.added.md"},
			wantPass: true,
		},
		{
			name:     "changelog fragment only passes",
			files:    []string{"changelog.d/2026-07-17-topic.fixed.md"},
			wantPass: true,
		},
		{
			name:     "docs-only passes",
			files:    []string{"CHANGELOG.md", "README.md"},
			wantPass: true,
		},
		{
			name:     "test-only Go change is excluded and passes",
			files:    []string{"pkg/mills/gates/docs_guardrail_test.go", "pkg/mills/health_policy_test.go"},
			wantPass: true,
		},
		{
			name:     "generated-only change is excluded and passes",
			files:    []string{"internal/contracts/testdata/escalation.golden", "pkg/foo/bar_golden.json"},
			wantPass: true,
		},
		{
			name:     "python test files excluded and pass",
			files:    []string{"scripts/test_thing.py", "scripts/pkg_test.py"},
			wantPass: true,
		},
		{
			name:     "mock files excluded and pass",
			files:    []string{"pkg/mills/store/db_mock.go"},
			wantPass: true,
		},
		{
			name:     "non-code paths never require docs",
			files:    []string{"platform/gitops/mills.yaml", "some/random/file.txt"},
			wantPass: true,
		},
		{
			name:     "empty diff passes",
			files:    nil,
			wantPass: true,
		},
		{
			name:     "skip-docs-check opts out",
			files:    []string{"pkg/mills/gates/docs_guardrail.go"},
			commits:  []string{"feat(mills): risky change [skip-docs-check]"},
			wantPass: true,
		},
		{
			name:     "skip-docs-check is case-insensitive",
			files:    []string{"pkg/mills/gates/docs_guardrail.go"},
			commits:  []string{"chore: infra tweak [SKIP-DOCS-CHECK]"},
			wantPass: true,
		},
		{
			name:     "root manifest change without docs fails",
			files:    []string{"go.mod", "Makefile"},
			wantPass: false,
		},
		{
			name:     "mixed significant + generated without docs still fails",
			files:    []string{"pkg/mills/gates/docs_guardrail.go", "pkg/mills/gates/docs_guardrail_test.go"},
			wantPass: false,
		},
	}

	g := &DocsGuardrail{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := g.Evaluate(context.Background(), StageInput{
				FilesChanged:   tc.files,
				CommitMessages: tc.commits,
			})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if out.Pass != tc.wantPass {
				t.Fatalf("Pass = %v, want %v (reasons: %v)", out.Pass, tc.wantPass, out.Reasons)
			}
			// A docs-guardrail fail must be RETRYABLE, never terminal — the
			// whole point is the gate-retry implement attempt adds the doc line.
			if !out.Pass && out.Terminal {
				t.Errorf("docs_guardrail fail must not be terminal: %+v", out)
			}
		})
	}
}

func TestDocsGuardrail_DeterministicReplay(t *testing.T) {
	g := &DocsGuardrail{}
	const failureReason = "[" + DocsGuardrailReasonMissingDocs + "] code-facing changes lack a documentation update: 7 file(s) require a doc entry " +
		"(pkg/mills/gates/a.go, pkg/mills/gates/b.go, pkg/mills/gates/c.go, pkg/mills/gates/d.go, " +
		"pkg/mills/gates/e.go, pkg/mills/gates/f.go (and 1 more)). Add a changelog fragment file at " +
		"changelog.d/<slug>.<category>.md (category is one of added|changed|deprecated|removed|fixed|security; " +
		"slug is unique per MR, e.g. the branch name; body is the Keep a Changelog bullet) describing this " +
		"change — this is the preferred fix and avoids CHANGELOG.md merge collisions. Do NOT edit CHANGELOG.md " +
		"directly. Alternatively include [skip-docs-check] in the commit message if the change is intentionally " +
		"undocumented. Expected a change under changelog.d/, or in README.md, CHANGELOG.md, ROADMAP.md, AGENTS.md, or docs/."

	fixtures := []struct {
		name string
		in   StageInput
		want Outcome
	}{
		{
			name: "reordered duplicated and capped failure",
			in: StageInput{FilesChanged: []string{
				"pkg/mills/gates/g.go", "./pkg/mills/gates/c.go",
				"/workspace/services/loom-core/pkg/mills/gates/a.go",
				"pkg/mills/gates/f.go", "pkg/mills/gates/b.go",
				"pkg/mills/gates/e.go", "pkg/mills/gates/d.go",
				"pkg/mills/gates/a.go", "pkg/mills/gates/z_test.go",
			}},
			want: Outcome{Reasons: []string{failureReason}, JudgedBy: "go"},
		},
		{
			name: "absolute documentation equivalent passes",
			in: StageInput{FilesChanged: []string{
				"/workspace/services/loom-core/pkg/mills/gates/a.go",
				"/workspace/services/loom-core/changelog.d/gate.fixed.md",
				"pkg/mills/gates/a.go",
			},
			},
			want: Outcome{
				Pass: true, Reasons: []string{
					"[" + DocsGuardrailReasonDocsPresent + "] documentation update accompanies code-facing changes",
				}, JudgedBy: "go",
			},
		},
		{
			name: "generated files pass",
			in: StageInput{FilesChanged: []string{
				"pkg/mills/gates/z_test.go", "internal/contracts/testdata/gate_golden.json",
			},
			},
			want: Outcome{
				Pass: true, Reasons: []string{
					"[" + DocsGuardrailReasonNoCodeChanges + "] no code-facing changes require documentation",
				}, JudgedBy: "go",
			},
		},
		{
			name: "reordered skip trailers have pinned reason",
			in: StageInput{
				FilesChanged:   []string{"pkg/mills/gates/a.go"},
				CommitMessages: []string{"z", "fix: intentional [SKIP-DOCS-CHECK]", "a"},
			},
			want: Outcome{
				Pass: true, Reasons: []string{
					"[" + DocsGuardrailReasonOptOut + "] skipped via [skip-docs-check] commit trailer",
				}, JudgedBy: "go",
			},
		},
	}

	for _, fixture := range fixtures {
		t.Run(fixture.name, func(t *testing.T) {
			wantBytes, err := json.Marshal(fixture.want)
			if err != nil {
				t.Fatal(err)
			}
			for evaluation := 1; evaluation <= 3; evaluation++ {
				got, err := g.Evaluate(context.Background(), fixture.in)
				if err != nil {
					t.Fatalf("evaluation %d: %v", evaluation, err)
				}
				gotBytes, err := json.Marshal(got)
				if err != nil {
					t.Fatalf("evaluation %d marshal: %v", evaluation, err)
				}
				if string(gotBytes) != string(wantBytes) {
					t.Fatalf("evaluation %d differs:\n got: %s\nwant: %s", evaluation, gotBytes, wantBytes)
				}
			}
		})
	}
}

func TestDocsGuardrail_StructuredReasonCodes(t *testing.T) {
	tests := []struct {
		name string
		in   StageInput
		code string
	}{
		{"pass", StageInput{}, DocsGuardrailReasonNoCodeChanges},
		{"documented", StageInput{FilesChanged: []string{"pkg/x.go", "docs/x.md"}}, DocsGuardrailReasonDocsPresent},
		{"fail", StageInput{FilesChanged: []string{"pkg/x.go"}}, DocsGuardrailReasonMissingDocs},
		{"opt out", StageInput{CommitMessages: []string{"fix: x [skip-docs-check]"}}, DocsGuardrailReasonOptOut},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := (&DocsGuardrail{}).Evaluate(context.Background(), tt.in)
			if err != nil {
				t.Fatal(err)
			}
			if len(out.Reasons) != 1 || !strings.HasPrefix(out.Reasons[0], "["+tt.code+"] ") {
				t.Fatalf("Reasons = %v, want stable code %q", out.Reasons, tt.code)
			}
		})
	}
}

func TestDocsGuardrail_BorderlineQuorum(t *testing.T) {
	t.Run("absolute path is evaluated exactly twice", func(t *testing.T) {
		calls := 0
		g := &DocsGuardrail{classify: func(in StageInput) docsGuardrailVerdict {
			calls++
			return classifyDocsGuardrail(in)
		}}
		out, err := g.Evaluate(context.Background(), StageInput{
			FilesChanged: []string{"/workspace/services/loom-core/pkg/x.go"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if calls != 2 {
			t.Fatalf("classification calls = %d, want exactly 2", calls)
		}
		if out.Pass || !strings.HasPrefix(out.Reasons[0], "["+DocsGuardrailReasonMissingDocs+"] ") {
			t.Fatalf("unexpected quorum outcome: %+v", out)
		}
	})

	t.Run("disagreement fails closed after one re-evaluation", func(t *testing.T) {
		calls := 0
		g := &DocsGuardrail{classify: func(StageInput) docsGuardrailVerdict {
			calls++
			if calls == 1 {
				return docsGuardrailVerdict{
					Status: docsGuardrailPass, ReasonCode: DocsGuardrailReasonDocsPresent,
					Detail: "first vote", Borderline: true,
				}
			}
			return docsGuardrailVerdict{
				Status: docsGuardrailFail, ReasonCode: DocsGuardrailReasonMissingDocs,
				Detail: "second vote", Borderline: true,
			}
		}}
		out, err := g.Evaluate(context.Background(), StageInput{})
		if err != nil {
			t.Fatal(err)
		}
		if calls != 2 {
			t.Fatalf("classification calls = %d, want exactly 2", calls)
		}
		if out.Pass || out.Terminal {
			t.Fatalf("disagreement must be retryable fail-closed: %+v", out)
		}
		if !strings.HasPrefix(out.Reasons[0], "["+DocsGuardrailReasonQuorumDisagree+"] ") {
			t.Fatalf("unexpected disagreement reason: %v", out.Reasons)
		}
	})
}

// TestDocsGuardrail_FailMessageIsRemediationOriented pins that the failure
// reason names the concrete fix (a changelog.d fragment), the escape hatch, and
// the offending file — so it is actionable when the runner threads it into the
// gate-retry implement prompt via StageRetryContext.FirstFailure. The fragment
// is the preferred fix (collision-free), so the message must steer there and
// away from a direct CHANGELOG.md edit.
func TestDocsGuardrail_FailMessageIsRemediationOriented(t *testing.T) {
	g := &DocsGuardrail{}
	out, err := g.Evaluate(context.Background(), StageInput{
		FilesChanged: []string{"pkg/mills/gates/docs_guardrail.go"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Pass {
		t.Fatalf("expected fail, got pass")
	}
	if len(out.Reasons) != 1 {
		t.Fatalf("expected exactly one reason, got %v", out.Reasons)
	}
	reason := out.Reasons[0]
	for _, want := range []string{
		"changelog.d/",
		"[skip-docs-check]",
		"pkg/mills/gates/docs_guardrail.go",
	} {
		if !strings.Contains(reason, want) {
			t.Errorf("reason %q should mention %q", reason, want)
		}
	}
	// The preferred fix is a fragment, not a CHANGELOG.md edit.
	if !strings.Contains(reason, "Do NOT edit CHANGELOG.md") {
		t.Errorf("reason %q should steer away from direct CHANGELOG.md edits", reason)
	}
}

// TestDocsGuardrail_SkipReasonRecorded pins that the escape-hatch pass carries
// an auditable reason (persisted into gate_outcomes.reasons) rather than an
// opaque pass.
func TestDocsGuardrail_SkipReasonRecorded(t *testing.T) {
	g := &DocsGuardrail{}
	out, _ := g.Evaluate(context.Background(), StageInput{
		FilesChanged:   []string{"pkg/mills/x.go"},
		CommitMessages: []string{"fix: thing [skip-docs-check]"},
	})
	if !out.Pass {
		t.Fatalf("expected pass on skip-docs-check")
	}
	if len(out.Reasons) == 0 || !strings.Contains(out.Reasons[0], "skip-docs-check") {
		t.Errorf("expected recorded opt-out reason, got %v", out.Reasons)
	}
}

// TestDocsGuardrail_AbsoluteSpawnPaths pins the absolute-path handling: spawn
// stages (k8s pod / harvester-vm) report absolute in-pod paths, and the gate
// must still classify a code-facing change under an absolute repo path and
// recognise an absolute doc path as satisfying the requirement.
func TestDocsGuardrail_AbsoluteSpawnPaths(t *testing.T) {
	g := &DocsGuardrail{}

	// Absolute code path, no docs → fail (must not silently pass every spawn).
	out, _ := g.Evaluate(context.Background(), StageInput{
		FilesChanged: []string{"/workspace/services/loom-core/pkg/mills/x.go"},
	})
	if out.Pass {
		t.Errorf("absolute code path without docs should fail, got pass")
	}

	// Absolute code path + absolute CHANGELOG → pass.
	out, _ = g.Evaluate(context.Background(), StageInput{
		FilesChanged: []string{
			"/workspace/services/loom-core/pkg/mills/x.go",
			"/workspace/services/loom-core/CHANGELOG.md",
		},
	})
	if !out.Pass {
		t.Errorf("absolute code path with CHANGELOG should pass, got %+v", out)
	}

	// Absolute test-only path → excluded → pass.
	out, _ = g.Evaluate(context.Background(), StageInput{
		FilesChanged: []string{"/workspace/services/loom-core/pkg/mills/x_test.go"},
	})
	if !out.Pass {
		t.Errorf("absolute test-only path should pass, got %+v", out)
	}

	// Absolute code path + absolute changelog.d fragment → pass (spawn stages
	// write fragments at absolute in-pod paths).
	out, _ = g.Evaluate(context.Background(), StageInput{
		FilesChanged: []string{
			"/workspace/services/loom-core/pkg/mills/x.go",
			"/workspace/services/loom-core/changelog.d/feat-x.added.md",
		},
	})
	if !out.Pass {
		t.Errorf("absolute code path with changelog.d fragment should pass, got %+v", out)
	}
}

// TestDocsGuardrail_RegisteredAndRecordedViaRegistry pins that the gate ships in
// the default registry and that its verdict flows through the same
// EvaluateAll → NamedOutcome path the runner persists into gate_outcomes.
func TestDocsGuardrail_RegisteredAndRecordedViaRegistry(t *testing.T) {
	reg := Default()
	if _, err := reg.Get("docs_guardrail"); err != nil {
		t.Fatalf("docs_guardrail not registered in Default(): %v", err)
	}

	outcomes, allPass, err := reg.EvaluateAll(context.Background(),
		[]string{"docs_guardrail"},
		StageInput{FilesChanged: []string{"pkg/mills/x.go"}})
	if err != nil {
		t.Fatalf("EvaluateAll: %v", err)
	}
	if allPass {
		t.Fatalf("expected aggregate fail for undocumented code change")
	}
	if len(outcomes) != 1 || outcomes[0].Name != "docs_guardrail" {
		t.Fatalf("expected one docs_guardrail outcome, got %+v", outcomes)
	}
	if outcomes[0].Outcome.JudgedBy != "go" {
		t.Errorf("JudgedBy = %q, want go", outcomes[0].Outcome.JudgedBy)
	}
}
