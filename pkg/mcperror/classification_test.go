package mcperror

import (
	"testing"

	"github.com/crb2nu/loom/pkg/telemetry"
)

func TestClassifyExternalCIIncident_GitLabServiceUnavailable(t *testing.T) {
	t.Parallel()

	got, ok := ClassifyExternalCIIncident("ci_watch: gitlab: GET /projects/47/pipelines: status 503: Service Unavailable")
	if !ok {
		t.Fatal("ClassifyExternalCIIncident returned ok=false")
	}
	if got.ID != telemetry.IncidentExternalIDGitLabServiceUnavailable {
		t.Fatalf("ID = %q, want %q", got.ID, telemetry.IncidentExternalIDGitLabServiceUnavailable)
	}
	if got.Dependency != "gitlab" {
		t.Fatalf("Dependency = %q, want gitlab", got.Dependency)
	}
	if got.Evidence == "" {
		t.Fatal("Evidence is empty")
	}
	if code := ExternalIncidentReasonCode(got); code != string(telemetry.IncidentReasonGitLabServiceUnavailable) {
		t.Fatalf("ReasonCode = %q, want %q", code, telemetry.IncidentReasonGitLabServiceUnavailable)
	}
	if !ExternalIncidentRetryable(got) {
		t.Fatal("Retryable = false, want true")
	}
	if ExternalIncidentTerminal(got) {
		t.Fatal("Terminal = true, want false")
	}
}

func TestClassifyExternalCIIncident_GitLabRateLimit(t *testing.T) {
	t.Parallel()

	got, ok := ClassifyExternalCIIncident("gitlab ci_watch: status 429: too many requests")
	if !ok {
		t.Fatal("ClassifyExternalCIIncident returned ok=false")
	}
	if got.ID != telemetry.IncidentExternalIDGitLabRateLimit {
		t.Fatalf("ID = %q, want %q", got.ID, telemetry.IncidentExternalIDGitLabRateLimit)
	}
	if code := ExternalIncidentReasonCode(got); code != string(telemetry.IncidentReasonGitLabRateLimit) {
		t.Fatalf("ReasonCode = %q, want %q", code, telemetry.IncidentReasonGitLabRateLimit)
	}
}

func TestClassifyExternalCIIncident_GitLabAgentUnauthenticated(t *testing.T) {
	t.Parallel()

	got, ok := ClassifyExternalCIIncident("starting GitLab agent\nGitLab agent is unauthenticated; run glab auth login")
	if !ok {
		t.Fatal("ClassifyExternalCIIncident returned ok=false")
	}
	if got.ID != telemetry.IncidentExternalIDGitLabAuthFailure {
		t.Fatalf("ID = %q, want %q", got.ID, telemetry.IncidentExternalIDGitLabAuthFailure)
	}
	if got.Evidence != "GitLab agent is unauthenticated; run glab auth login" {
		t.Fatalf("Evidence = %q, want matching line", got.Evidence)
	}
	if code := ExternalIncidentReasonCode(got); code != string(telemetry.IncidentReasonGitLabAuthFailure) {
		t.Fatalf("ReasonCode = %q, want %q", code, telemetry.IncidentReasonGitLabAuthFailure)
	}
	if ExternalIncidentRetryable(got) {
		t.Fatal("Retryable = true, want false")
	}
	if !ExternalIncidentTerminal(got) {
		t.Fatal("Terminal = false, want true")
	}
}

func TestClassifyExternalCIIncident_GitLabCIPipelineFailure(t *testing.T) {
	t.Parallel()

	got, ok := ClassifyExternalCIIncident("ci_watch: GitLab CI pipeline failed while waiting for pipeline 814")
	if !ok {
		t.Fatal("ClassifyExternalCIIncident returned ok=false")
	}
	if got.ID != telemetry.IncidentExternalIDGitLabServiceUnavailable {
		t.Fatalf("ID = %q, want %q", got.ID, telemetry.IncidentExternalIDGitLabServiceUnavailable)
	}
	if code := ExternalIncidentReasonCode(got); code != string(telemetry.IncidentReasonGitLabServiceUnavailable) {
		t.Fatalf("ReasonCode = %q, want %q", code, telemetry.IncidentReasonGitLabServiceUnavailable)
	}
	if !ExternalIncidentRetryable(got) {
		t.Fatal("Retryable = false, want true")
	}
	if ExternalIncidentTerminal(got) {
		t.Fatal("Terminal = true, want false")
	}
}

func TestClassifyExternalCIIncident_DoesNotClassifyUnrelatedPipelineFailures(t *testing.T) {
	t.Parallel()

	for _, input := range []string{
		"CI pipeline failed while running go test ./...",
		"GitLab CI pipeline passed",
		"GitLab agent connected",
	} {
		input := input
		t.Run(input, func(t *testing.T) {
			t.Parallel()
			if got, ok := ClassifyExternalCIIncident(input); ok {
				t.Fatalf("ClassifyExternalCIIncident(%q) = %+v, true; want false", input, got)
			}
		})
	}
}

func TestClassifyExternalCIIncident_ReusesKnownExternalIncidentRules(t *testing.T) {
	t.Parallel()

	got, ok := ClassifyExternalCIIncident("registry cache export failed: error writing manifest blob: blob upload unknown")
	if !ok {
		t.Fatal("ClassifyExternalCIIncident returned ok=false")
	}
	if got.ID != ExternalIncidentIDBlobStorageManifestWrite {
		t.Fatalf("ID = %q, want %q", got.ID, ExternalIncidentIDBlobStorageManifestWrite)
	}
	if code := ExternalIncidentReasonCode(got); code != string(telemetry.IncidentReasonBlobStorageManifestWrite) {
		t.Fatalf("ReasonCode = %q, want %q", code, telemetry.IncidentReasonBlobStorageManifestWrite)
	}
	if ExternalIncidentRetryable(got) {
		t.Fatal("Retryable = true, want false")
	}
	if !ExternalIncidentTerminal(got) {
		t.Fatal("Terminal = false, want true")
	}
}
