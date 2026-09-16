package pipeline

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/crb2nu/loom/pkg/telemetry"
)

func TestClassifierKnownExternalFailureSignatures(t *testing.T) {
	tests := []struct {
		name       string
		text       string
		id         string
		dependency string
	}{
		{name: "openrouter status", text: `OpenRouter status 402: This request requires more credits`, id: "external_dependency.openrouter.credits_exhausted", dependency: "openrouter"},
		{name: "openrouter http variant", text: `provider=OpenRouter HTTP/1.1 402 Payment Required: credits exhausted`, id: "external_dependency.openrouter.credits_exhausted", dependency: "openrouter"},
		{name: "litellm live message", text: `LiteLLM status 401: Authentication Error, No api key passed in.`, id: "external_dependency.litellm.missing_api_key", dependency: "litellm"},
		{name: "litellm case variant", text: `LITELLM HTTP/1.1 401 Unauthorized: NO API KEY PASSED IN`, id: "external_dependency.litellm.missing_api_key", dependency: "litellm"},
		{name: "clickhouse spaced code", text: `ClickHouse Code: 432. Merge task failed`, id: "external_dependency.clickhouse.merge_task", dependency: "clickhouse"},
		{name: "clickhouse compact code", text: `clickhouse Code:432 MergeTreeBackgroundExecutor failure`, id: "external_dependency.clickhouse.merge_task", dependency: "clickhouse"},
		{name: "clickhouse live message", text: `ClickHouse exception Code: 432. DB::Exception: Cannot merge parts because a merge with the same resulting part is already running`, id: "external_dependency.clickhouse.merge_task", dependency: "clickhouse"},
		{name: "gitlab agent", text: `GitLab-agent RPC error: Unauthenticated`, id: "external_dependency.gitlab.auth_failure", dependency: "gitlab"},
		{
			name: "persisted 2026-08-04 incident",
			text: `{"status":"failed","failure_reason":"runner_system_failure","retried":false}`,
			id:   "external_dependency.gitlab.ci_infrastructure", dependency: "gitlab",
		},
		{
			name: "pipeline diagnostic",
			text: "ci_watch pipeline 1842 job 991 failed: failure_reason=runner_system_failure",
			id:   "external_dependency.gitlab.ci_infrastructure", dependency: "gitlab",
		},
		{
			name: "spaced field",
			text: "job failed (failure_reason: runner_system_failure)",
			id:   "external_dependency.gitlab.ci_infrastructure", dependency: "gitlab",
		},
		{
			name: "multiline JSON whitespace",
			text: "{\n  \"failure_reason\":\n  \"runner_system_failure\"\n}",
			id:   "external_dependency.gitlab.ci_infrastructure", dependency: "gitlab",
		},
		{
			name: "job execution timeout",
			text: `{"status":"failed","failure_reason":"job_execution_timeout"}`,
			id:   "external_dependency.gitlab.ci_infrastructure", dependency: "gitlab",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			signature, matched := ClassifyExternalFailureSignature(tt.text)
			if !matched {
				t.Fatal("external signature did not match")
			}
			if signature.Class != ClassificationExternalDependencyIncident || signature.Disposition != ExternalFailureDispositionWaitForDependency {
				t.Fatalf("policy = %q/%q, want external dependency wait", signature.Class, signature.Disposition)
			}

			got, matched := ClassifyCIFailureSignature(tt.text)
			if !matched {
				t.Fatalf("matched = false; classification = %+v", got)
			}
			if got.Class != FailureConfiguration || got.Retryable || got.FreeRetry || !got.Terminal {
				t.Fatalf("classification = %+v, want non-code wait metadata", got)
			}
			if got.ExternalDependencyID != tt.id || got.ExternalDependency != tt.dependency {
				t.Fatalf("identity = %q/%q, want %q/%q", got.ExternalDependencyID, got.ExternalDependency, tt.id, tt.dependency)
			}
			if got.Classifier != FailureClassifierName {
				t.Fatalf("classifier = %q, want %q", got.Classifier, FailureClassifierName)
			}
		})
	}
}

func TestExternalFailureSignatureOrderAndDisposition(t *testing.T) {
	if got := externalFailureSignatures[0].ID; got != "external_dependency.openrouter.credits_exhausted" {
		t.Fatalf("first signature = %q, want promoted OpenRouter signature", got)
	}
	evidence := `OpenRouter status 402: requires more credits; {"failure_reason":"runner_system_failure"}`
	got, matched := ClassifyExternalFailureSignature(evidence)
	if !matched || got.ID != externalFailureSignatures[0].ID {
		t.Fatalf("first-match result = %+v, %v", got, matched)
	}
	if got.Class != ClassificationExternalDependencyIncident || got.Disposition != ExternalFailureDispositionWaitForDependency {
		t.Fatalf("policy = %q/%q, want external dependency wait", got.Class, got.Disposition)
	}
}

func TestOpenRouter402CreditExhaustionSignatureMatrix(t *testing.T) {
	statusVariants := []string{
		"OpenRouter HTTP 402",
		"OpenRouter HTTP/1.1 402 Payment Required",
		"OpenRouter status 402",
		"OpenRouter status_code=402",
	}
	for _, status := range statusVariants {
		for _, phrase := range openRouterCreditExhaustionPhrases {
			evidence := status + ": " + phrase
			t.Run(status+"/"+phrase, func(t *testing.T) {
				got, matched := ClassifyExternalFailureSignature(evidence)
				if !matched {
					t.Fatalf("signature did not match %q", evidence)
				}
				if got.ID != "external_dependency.openrouter.credits_exhausted" || got.Dependency != "openrouter" {
					t.Fatalf("identity = %q/%q, want stable OpenRouter identity", got.ID, got.Dependency)
				}
				if got.Class != ClassificationExternalDependencyIncident || got.Disposition != ExternalFailureDispositionWaitForDependency {
					t.Fatalf("policy = %q/%q, want external dependency wait", got.Class, got.Disposition)
				}
			})
		}
	}
}

func TestExternalFailureSignatureNearMisses(t *testing.T) {
	for _, evidence := range []string{
		`Stripe HTTP 402 Payment Required: requires more credits`,
		`OpenRouter status 429: requires more credits`,
		`OpenRouter status 402: payment authorization failed`,
		`Gateway status 401: No api key passed in`,
		`LiteLLM request failed: No api key passed in`,
		`LiteLLM status 500: No api key passed in`,
		`LiteLLM status 401: API key rejected`,
		`Code: 432 merge task failed`,
		`ClickHouse Code: 431 merge task failed`,
		`ClickHouse Code: 432 query failed`,
		`RPC error: Unauthenticated`,
		`GitLab agent authentication succeeded`,
		`{"failure_reason":"script_failure"}`,
		`test output mentions runner_system_failure`,
		`test output mentions job_execution_timeout`,
	} {
		t.Run(evidence, func(t *testing.T) {
			if got, matched := ClassifyExternalFailureSignature(evidence); matched {
				t.Fatalf("near miss matched: %+v", got)
			}
		})
	}
}

func TestClassifierOpenRouter402Integrated(t *testing.T) {
	got := ClassifyFailureRecord(errors.New(
		`model request failed: provider=OpenRouter status 402: insufficient credits`,
	))
	if got.Class != FailureConfiguration || got.Retryable || got.FreeRetry || !got.Terminal {
		t.Fatalf("classification = %+v, want terminal non-code classification", got)
	}
}

func TestClassifierRunnerSystemFailureIntegrated(t *testing.T) {
	got := ClassifyFailureRecord(errors.New(
		`ci_watch failed: {"status":"failed","failure_reason":"runner_system_failure"}`,
	))
	if got.Class != FailureConfiguration || got.Retryable || got.FreeRetry || !got.Terminal {
		t.Fatalf("classification = %+v, want integrated external non-code metadata", got)
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
