package pipeline

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/crb2nu/loom/pkg/telemetry"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestClassifierRunnerSystemFailure(t *testing.T) {
	tests := []struct {
		name    string
		text    string
		matched bool
	}{
		{
			name:    "persisted 2026-08-04 incident",
			text:    `{"status":"failed","failure_reason":"runner_system_failure","retried":false}`,
			matched: true,
		},
		{
			name:    "pipeline diagnostic",
			text:    "ci_watch pipeline 1842 job 991 failed: failure_reason=runner_system_failure",
			matched: true,
		},
		{
			name:    "spaced field",
			text:    "job failed (failure_reason: runner_system_failure)",
			matched: true,
		},
		{
			name:    "multiline JSON whitespace",
			text:    "{\n  \"failure_reason\":\n  \"runner_system_failure\"\n}",
			matched: true,
		},
		{name: "repository script failure", text: `{"failure_reason":"script_failure"}`},
		{name: "mixed prose", text: "test output mentions runner_system_failure"},
		{name: "wrong field", text: "reason=runner_system_failure"},
		{name: "field suffix", text: "previous_failure_reason=runner_system_failure"},
		{name: "token suffix", text: "failure_reason=runner_system_failure_after_script"},
		{name: "case changed", text: "failure_reason=RUNNER_SYSTEM_FAILURE"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, matched := ClassifyCIFailureSignature(tt.text)
			if matched != tt.matched {
				t.Fatalf("matched = %v, want %v; classification = %+v", matched, tt.matched, got)
			}
			if !tt.matched {
				if got != (FailureClassification{}) {
					t.Fatalf("negative classification = %+v, want zero value", got)
				}
				return
			}
			if got.Class != FailureTransient || !got.Retryable || !got.FreeRetry || got.Terminal {
				t.Fatalf("classification = %+v, want transient retryable metadata", got)
			}
			if got.Classifier != FailureClassifierName {
				t.Fatalf("classifier = %q, want %q", got.Classifier, FailureClassifierName)
			}
		})
	}
}

func TestClassifierRunnerSystemFailureIntegrated(t *testing.T) {
	got := ClassifyFailureRecord(errors.New(
		`ci_watch failed: {"status":"failed","failure_reason":"runner_system_failure"}`,
	))
	if got.Class != FailureTransient || !got.Retryable || !got.FreeRetry || got.Terminal {
		t.Fatalf("classification = %+v, want integrated transient retryable metadata", got)
	}
}

func TestNormalizeSourceClassificationRecordsNormalizedClass(t *testing.T) {
	previous := classificationMetrics
	classificationMetrics = telemetry.NewClassificationMetrics(nil)
	t.Cleanup(func() { classificationMetrics = previous })

	inputs := []SourceClassification{
		{Source: "runner", Class: ClassificationExternalDependencyIncident},
		{Source: "ci", Class: ClassificationRepositoryRegression},
		{Source: "runner", Class: "unbounded_value"},
		{Source: "", Class: ClassificationRepositoryRegression},
	}
	for _, input := range inputs {
		normalizeSourceClassification(input)
	}

	for class, want := range map[string]float64{
		telemetry.ClassificationClassExternalDependencyIncident: 1,
		telemetry.ClassificationClassRepositoryRegression:       1,
		telemetry.ClassificationClassUnknown:                    2,
	} {
		if got := testutil.ToFloat64(classificationMetrics.ClassificationsTotal.WithLabelValues(class)); got != want {
			t.Errorf("classifications{%q} = %v, want %v", class, got, want)
		}
	}
}

func TestExternalDependencyIncidentRunbooks(t *testing.T) {
	want := map[string]string{
		"external_dependency.gitlab.auth_failure":          "docs/runbooks/gitlab-agent-unauthenticated.md",
		"external_dependency.clickhouse.merge_task":        "docs/runbooks/clickhouse-merge-failures.md",
		"external_dependency.longhorn.no_available_disk":   "docs/runbooks/longhorn-disk-exhaustion.md",
		"external_dependency.litellm.missing_api_key":      "docs/runbooks/litellm-auth-missing.md",
		"external_dependency.openrouter.credits_exhausted": "docs/runbooks/openrouter-credits-exhausted.md",
	}
	if len(externalDependencyIncidentRunbooks) != len(want) {
		t.Fatalf("runbook links = %d, want %d", len(externalDependencyIncidentRunbooks), len(want))
	}

	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate classifier_test.go")
	}
	repoRoot := filepath.Join(filepath.Dir(filename), "..", "..", "..")
	required := []string{"## Detection", "## Classification", "## Operator Action"}

	for patternID, wantPath := range want {
		t.Run(patternID, func(t *testing.T) {
			gotPath, ok := externalDependencyIncidentRunbooks[patternID]
			if !ok {
				t.Fatalf("pattern has no runbook link")
			}
			if gotPath != wantPath {
				t.Fatalf("runbook link = %q, want %q", gotPath, wantPath)
			}
			contents, err := os.ReadFile(filepath.Join(repoRoot, filepath.FromSlash(gotPath)))
			if err != nil {
				t.Fatalf("read linked runbook: %v", err)
			}
			text := string(contents)
			if !strings.Contains(text, "`"+patternID+"`") {
				t.Errorf("runbook does not name classifier pattern %q", patternID)
			}
			for _, heading := range required {
				if strings.Count(text, heading) != 1 {
					t.Errorf("heading %q must occur exactly once", heading)
				}
			}
		})
	}
}
