package audit

import (
	"encoding/json"
	"errors"
	"io"
	"testing"
)

func TestClassifyCIFailureMessage_RecurringPipelineFixtures(t *testing.T) {
	cases := []struct {
		name           string
		message        string
		category       CIFailureCategory
		stage          string
		reason         string
		dependency     string
		retryable      bool
		terminal       bool
		wantLabelCount int
	}{
		{
			name:           "ci watch poll timeout",
			message:        "stage ci_watch attempt=1: gitlab: pipeline poll timed out after 30m0s (pipeline: https://gitlab.example/services/loom-core/-/pipelines/12345): pipeline: poll deadline exceeded",
			category:       CIFailureCategoryInfrastructure,
			stage:          "ci_watch",
			reason:         "ci-watch-poll-timeout",
			dependency:     "gitlab_ci",
			retryable:      true,
			terminal:       false,
			wantLabelCount: 7,
		},
		{
			name:           "terminal ci watch pipeline",
			message:        "stage ci_watch terminal code error [class=code]: ci pipeline failed for mr 42: pipeline: terminal non-success status",
			category:       CIFailureCategoryCode,
			stage:          "ci_watch",
			reason:         "ci-watch-terminal-pipeline",
			retryable:      false,
			terminal:       true,
			wantLabelCount: 6,
		},
		{
			name:           "gitlab merge configuration",
			message:        `stage merge terminal config error: gitlab: PUT /projects/services%2Floom-core/merge_requests/598/merge: status 405: {"message":"405 Method Not Allowed"}`,
			category:       CIFailureCategoryConfiguration,
			stage:          "merge",
			reason:         "gitlab-merge-configuration",
			dependency:     "gitlab",
			retryable:      false,
			terminal:       true,
			wantLabelCount: 7,
		},
		{
			name:           "docs guardrail missing changelog",
			message:        "guardrails:docs-cli failed: code-facing changes require a CHANGELOG.md entry; missing changelog for *.go diff",
			category:       CIFailureCategoryCode,
			stage:          "ci",
			reason:         "docs-guardrail-missing-entry",
			retryable:      false,
			terminal:       true,
			wantLabelCount: 6,
		},
		{
			name:           "gitlab service unavailable",
			message:        "gitlab: GET /projects/47/pipelines: status 503: service unavailable",
			category:       CIFailureCategoryExternalDependency,
			stage:          "ci",
			reason:         "gitlab-service-unavailable",
			dependency:     "gitlab",
			retryable:      true,
			terminal:       false,
			wantLabelCount: 10,
		},
		{
			name:           "gitlab rate limit",
			message:        "ci_watch: gitlab: GET /projects/47/pipelines: status 429: too many requests",
			category:       CIFailureCategoryExternalDependency,
			stage:          "ci_watch",
			reason:         "gitlab-rate-limit",
			dependency:     "gitlab",
			retryable:      true,
			terminal:       false,
			wantLabelCount: 10,
		},
		{
			name:           "gitlab runner infrastructure",
			message:        "ERROR: Job failed (system failure): runner system failure: Kubernetes executor pod stuck or timeout failure",
			category:       CIFailureCategoryInfrastructure,
			stage:          "ci",
			reason:         "gitlab-runner-infrastructure",
			dependency:     "gitlab_runner",
			retryable:      true,
			terminal:       false,
			wantLabelCount: 7,
		},
		{
			name:           "model provider rate limit preserves stage",
			message:        "stage pr_self_review failed: flexinfer chat status 429: too many requests",
			category:       CIFailureCategoryExternalDependency,
			stage:          "pr_self_review",
			reason:         "model-provider-rate-limit",
			dependency:     "model_provider",
			retryable:      true,
			terminal:       false,
			wantLabelCount: 10,
		},
		{
			name:           "model provider service unavailable",
			message:        "stage ci_watch failed: anthropic: status 503 service unavailable",
			category:       CIFailureCategoryExternalDependency,
			stage:          "ci_watch",
			reason:         "model-provider-service-unavailable",
			dependency:     "model_provider",
			retryable:      true,
			terminal:       false,
			wantLabelCount: 10,
		},
		{
			name:           "container registry blob storage incident",
			message:        "buildkit: registry cache export failed: error writing manifest blob: blob upload unknown",
			category:       CIFailureCategoryExternalDependency,
			stage:          "ci",
			reason:         "blob-storage-manifest-write",
			dependency:     "container_registry_blob_storage",
			retryable:      false,
			terminal:       true,
			wantLabelCount: 10,
		},
		{
			name:           "gitlab auth incident preserves ci_watch stage",
			message:        "stage ci_watch terminal config error (not retried) [class=config]: gitlab: GET /projects/47/pipelines: status 401: unauthorized",
			category:       CIFailureCategoryExternalDependency,
			stage:          "ci_watch",
			reason:         "gitlab-auth-failure",
			dependency:     "gitlab",
			retryable:      false,
			terminal:       true,
			wantLabelCount: 10,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyCIFailureMessage(tc.message)
			if !got.Matched {
				t.Fatalf("Matched = false, want true")
			}
			if got.Classifier != CIFailureClassifierName {
				t.Fatalf("Classifier = %q, want %q", got.Classifier, CIFailureClassifierName)
			}
			if got.Category != tc.category {
				t.Fatalf("Category = %q, want %q", got.Category, tc.category)
			}
			if got.Stage != tc.stage {
				t.Fatalf("Stage = %q, want %q", got.Stage, tc.stage)
			}
			if got.Reason != tc.reason {
				t.Fatalf("Reason = %q, want %q", got.Reason, tc.reason)
			}
			if got.Dependency != tc.dependency {
				t.Fatalf("Dependency = %q, want %q", got.Dependency, tc.dependency)
			}
			if got.Retryable != tc.retryable {
				t.Fatalf("Retryable = %v, want %v", got.Retryable, tc.retryable)
			}
			if got.Terminal != tc.terminal {
				t.Fatalf("Terminal = %v, want %v", got.Terminal, tc.terminal)
			}
			if len(got.Labels) != tc.wantLabelCount {
				t.Fatalf("Labels = %#v, want %d labels", got.Labels, tc.wantLabelCount)
			}
			assertHasLabel(t, got.Labels, "ci_failure_reason/"+tc.reason)
			if tc.category == CIFailureCategoryExternalDependency {
				assertHasLabel(t, got.Labels, "incident_class/external_dependency_incident")
				assertHasLabel(t, got.Labels, "incident_code/"+tc.reason)
				assertHasLabel(t, got.Labels, "external_dependency/"+tc.dependency)
			}
		})
	}
}

func TestClassifyCIFailure_UnknownAndNilDoNotMatch(t *testing.T) {
	for _, got := range []CIFailureClassification{
		ClassifyCIFailure(nil),
		ClassifyCIFailureMessage(""),
		ClassifyCIFailureMessage("go test FAIL: TestFoo not equal to bar"),
		ClassifyCIFailureMessage("provider package unit test failed: expected status 429 in fixture"),
	} {
		if got.Classifier != CIFailureClassifierName {
			t.Fatalf("Classifier = %q, want %q", got.Classifier, CIFailureClassifierName)
		}
		if got.Matched {
			t.Fatalf("Matched = true for %#v, want false", got)
		}
		if len(got.Labels) != 0 {
			t.Fatalf("Labels = %#v, want none", got.Labels)
		}
	}
}

func TestClassifyCIFailure_EOFUsesTransportContract(t *testing.T) {
	got := ClassifyCIFailure(io.ErrUnexpectedEOF)
	if got.Category != CIFailureCategoryInfrastructure {
		t.Fatalf("Category = %q, want %q", got.Category, CIFailureCategoryInfrastructure)
	}
	if got.Stage != "ci" {
		t.Fatalf("Stage = %q, want ci", got.Stage)
	}
	if got.Reason != "ci-transport-eof" {
		t.Fatalf("Reason = %q, want ci-transport-eof", got.Reason)
	}
	if got.Dependency != "network" {
		t.Fatalf("Dependency = %q, want network", got.Dependency)
	}
	if !got.Retryable {
		t.Fatal("Retryable = false, want true")
	}
	if got.Terminal {
		t.Fatal("Terminal = true, want false")
	}
}

func TestCIFailureClassificationJSONContract(t *testing.T) {
	got := ClassifyCIFailure(errors.New("guardrails:docs-cli failed: missing changelog for code-facing changes"))
	data, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	want := `{"classifier":"ci-failure-classifier","matched":true,"category":"code","stage":"ci","reason":"docs-guardrail-missing-entry","retryable":false,"terminal":true,"labels":["kind/ci-failure","ci_failure/code","ci_failure_reason/docs-guardrail-missing-entry","retryable/false","terminal/true","stage/ci"]}`
	if string(data) != want {
		t.Fatalf("JSON = %s, want %s", data, want)
	}
}

func assertHasLabel(t *testing.T, labels []string, want string) {
	t.Helper()
	for _, label := range labels {
		if label == want {
			return
		}
	}
	t.Fatalf("Labels = %#v, want label %q", labels, want)
}
