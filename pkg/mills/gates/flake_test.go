package gates

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/crb2nu/loom/pkg/mills/store"
	"github.com/crb2nu/loom/pkg/telemetry"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

type classifiedErrorGate struct{ err error }

func (g classifiedErrorGate) Name() string { return "classified_error" }
func (g classifiedErrorGate) Evaluate(context.Context, StageInput) (Outcome, error) {
	return Outcome{Pass: false, JudgedBy: "go"}, g.err
}

func TestGateTelemetryClassifiesGitLabUnreachableFailClosed(t *testing.T) {
	tests := []struct {
		name         string
		err          error
		wantCategory FailureCategory
		wantMetric   float64
	}{
		{name: "connection refused", err: fmt.Errorf("gitlab: list merge requests: %w", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connect: connection refused")}), wantCategory: FailureCategoryExternalDependency, wantMetric: 1},
		{name: "dns failure", err: fmt.Errorf("gitlab: query pipeline: %w", &net.DNSError{Err: "no such host", Name: "gitlab.example"}), wantCategory: FailureCategoryExternalDependency, wantMetric: 1},
		{name: "timeout", err: fmt.Errorf("gitlab: discussions: %w", context.DeadlineExceeded), wantCategory: FailureCategoryExternalDependency, wantMetric: 1},
		{name: "edge 503", err: errors.New("gitlab: GET /merge_requests: status 503: service unavailable"), wantCategory: FailureCategoryExternalDependency, wantMetric: 1},
		{name: "ordinary gate error", err: errors.New("gate rubric rejected input"), wantCategory: FailureCategoryInfrastructureError},
		{name: "generic infrastructure", err: &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}, wantCategory: FailureCategoryInfrastructureError},
		{name: "caller cancellation", err: fmt.Errorf("gitlab: request: %w", context.Canceled), wantCategory: FailureCategoryInfrastructureError},
		{name: "gitlab auth", err: errors.New("gitlab: status 401: unauthorized"), wantCategory: FailureCategoryInfrastructureError},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			harness := &telemetry.GateDeterminismHarness{}
			registry := NewRegistry()
			registry.SetTelemetrySink(harness)
			registry.Register(classifiedErrorGate{err: tc.err})
			before := testutil.ToFloat64(gitLabUnreachableGateEvaluationsTotal)

			outcomes, passed, err := registry.EvaluateAll(context.Background(), []string{"classified_error"}, StageInput{RunID: "run-extdep"})
			if err == nil || passed {
				t.Fatalf("EvaluateAll = outcomes=%+v passed=%v err=%v; want fail-closed error", outcomes, passed, err)
			}
			if len(outcomes) != 0 {
				t.Fatalf("erroring gate returned outcomes %+v; want none", outcomes)
			}
			records := harness.Records()
			if len(records) != 1 || records[0].Verdict != telemetry.GateVerdictError || records[0].FailureCategory != tc.wantCategory {
				t.Fatalf("records = %+v; want error category %q", records, tc.wantCategory)
			}
			if delta := testutil.ToFloat64(gitLabUnreachableGateEvaluationsTotal) - before; delta != tc.wantMetric {
				t.Fatalf("dedicated metric delta = %v, want %v", delta, tc.wantMetric)
			}
			if tc.wantCategory == FailureCategoryExternalDependency && !strings.Contains(err.Error(), "gitlab: service unavailable") {
				t.Fatalf("classified error %q lacks runner parking signal", err)
			}
		})
	}
}

func TestGateTelemetryDocsGuardrailAndScope(t *testing.T) {
	harness := &telemetry.GateDeterminismHarness{}
	registry := Default()
	registry.SetTelemetrySink(harness)

	in := StageInput{
		RunID:        "run-1",
		Item:         fixtureItem(store.Slice{Name: "telemetry", Files: []string{"pkg/example.go"}}),
		FilesChanged: []string{"pkg/example.go", "changelog.d/telemetry.added.md"},
	}
	outcomes, passed, err := registry.EvaluateAll(
		context.Background(), []string{"docs_guardrail", "scope"}, in)
	if err != nil {
		t.Fatalf("EvaluateAll: %v", err)
	}
	if !passed || len(outcomes) != 2 {
		t.Fatalf("outcomes = %+v, passed = %v", outcomes, passed)
	}

	records := harness.Records()
	if len(records) != 2 {
		t.Fatalf("records = %+v, want 2", records)
	}
	for i, gateID := range []string{"docs_guardrail", "scope"} {
		record := records[i]
		if record.GateID != gateID || record.RunID != "run-1" ||
			record.InputDigest == "" || record.Verdict != telemetry.GateVerdictPass {
			t.Errorf("record[%d] = %+v", i, record)
		}
	}
	for _, record := range records {
		if got := inputDigestForGate(record.GateID, in); record.InputDigest != got {
			t.Errorf("%s digest = %q, want stable %q",
				record.GateID, record.InputDigest, got)
		}
	}
}

func TestGateTelemetryRecordsReasonDurationAndEvaluationErrors(t *testing.T) {
	harness := &telemetry.GateDeterminismHarness{}
	registry := NewRegistry()
	registry.SetTelemetrySink(harness)
	registry.Register(errorTelemetryGate{name: "broken"})

	_, _, err := registry.EvaluateAll(context.Background(), []string{"broken"}, StageInput{RunID: "run-error"})
	if err == nil {
		t.Fatal("EvaluateAll should return the evaluation error")
	}
	records := harness.Records()
	if len(records) != 1 {
		t.Fatalf("records = %+v, want one", records)
	}
	got := records[0]
	if got.RunID != "run-error" || got.GateID != "broken" || got.Verdict != telemetry.GateVerdictError {
		t.Errorf("record = %+v", got)
	}
	if got.Reason != "evaluation failed" || got.DurationMS < 0 {
		t.Errorf("reason/duration = %q/%d", got.Reason, got.DurationMS)
	}
	if got.FailureCategory != FailureCategoryInfrastructureError {
		t.Errorf("failure category = %q, want %q", got.FailureCategory, FailureCategoryInfrastructureError)
	}
}

func TestGateFailureCategoriesAtEvaluationBoundary(t *testing.T) {
	collector := &telemetry.GateFailureCollector{}
	registry := NewRegistry()
	registry.SetTelemetrySink(collector)
	registry.Register(&NonEmptyDiff{})

	_, _, err := registry.EvaluateAll(context.Background(), []string{"nonempty_diff", "not_registered"}, StageInput{RunID: "run-7"})
	if err != nil {
		t.Fatalf("EvaluateAll: %v", err)
	}
	got := collector.Snapshot().Failures
	if len(got) != 2 {
		t.Fatalf("failures = %+v, want gate fail and unknown gate", got)
	}
	if got[0].GateID != "nonempty_diff" || got[0].Category != FailureCategoryFail ||
		got[1].GateID != "not_registered" || got[1].Category != FailureCategoryUnknownGate {
		t.Fatalf("failure categories = %+v", got)
	}
	for _, failure := range got {
		if len(failure.RecentRunIDs) != 1 || failure.RecentRunIDs[0] != "run-7" {
			t.Errorf("run IDs for %+v", failure)
		}
	}
}

func TestGateInputDigestStableAndSensitive(t *testing.T) {
	base := StageInput{
		RunID:        "run-a",
		Item:         fixtureItem(store.Slice{Name: "x", Files: []string{"a.go"}}),
		FilesChanged: []string{"a.go"},
		LinesAdded:   1,
	}
	same := base
	same.RunID = "run-b"
	if inputDigest(base) != inputDigest(same) {
		t.Fatal("run metadata changed semantic input digest")
	}

	changed := base
	changed.LinesAdded++
	if inputDigest(base) == inputDigest(changed) {
		t.Fatal("meaningful input change did not change digest")
	}
}

func TestGateInputDigestCanonicalizesSemanticSets(t *testing.T) {
	itemA := fixtureItem(store.Slice{
		Name:  "first",
		Files: []string{"pkg/b.go", "pkg/a.go"},
		Tests: []string{"pkg/b_test.go"},
	})
	itemB := fixtureItem(store.Slice{
		Name:  "renamed-without-semantic-effect",
		Files: []string{"pkg/a.go", "pkg/./b.go", "pkg/a.go"},
		Tests: []string{"pkg/b_test.go"},
	})
	first := StageInput{
		RunID:          "run-a",
		Item:           itemA,
		FilesChanged:   []string{"pkg/b.go", "pkg/a.go", "pkg/a.go"},
		CommitMessages: []string{"fix: one", "docs: two"},
	}
	reordered := StageInput{
		RunID:          "run-b",
		Item:           itemB,
		FilesChanged:   []string{"pkg/./a.go", "pkg/b.go"},
		CommitMessages: []string{"docs: two", "fix: one"},
	}

	for _, gateID := range []string{"docs_guardrail", "scope"} {
		if got, want := inputDigestForGate(gateID, reordered), inputDigestForGate(gateID, first); got != want {
			t.Errorf("%s digest = %q, want %q", gateID, got, want)
		}
	}
}

func TestGateDeterminismHarnessFlagsDivergentVerdicts(t *testing.T) {
	harness := &telemetry.GateDeterminismHarness{}
	first := telemetry.GateEvaluation{
		GateID: "scope", RunID: "run-1", InputDigest: "abc",
		Verdict: telemetry.GateVerdictPass,
	}
	harness.RecordGateEvaluation(first)
	first.RunID = "run-2"
	harness.RecordGateEvaluation(first)
	if flakes := harness.Flakes(); len(flakes) != 0 {
		t.Fatalf("consistent verdict flagged as flake: %+v", flakes)
	}

	first.RunID = "run-3"
	first.Verdict = telemetry.GateVerdictFail
	harness.RecordGateEvaluation(first)
	flakes := harness.Flakes()
	if len(flakes) != 1 {
		t.Fatalf("flakes = %+v, want one", flakes)
	}
	if flakes[0].GateID != "scope" || flakes[0].InputDigest != "abc" ||
		flakes[0].FirstRunID != "run-1" || flakes[0].RunID != "run-3" {
		t.Errorf("incomplete flake report: %+v", flakes[0])
	}
}
