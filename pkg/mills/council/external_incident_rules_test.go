package council

import (
	"strings"
	"testing"
)

func TestClassifyExternalIncidentPlanning_GitLabCIThirdPartyFailure(t *testing.T) {
	t.Parallel()

	got := ClassifyExternalIncidentPlanning(ExternalIncidentPlanningInput{
		Title:     "ci_watch failed before merge",
		JobName:   "ci_watch",
		ErrorLine: "gitlab: GET /projects/47/pipelines: status 503: Service Unavailable",
	})

	assertExternalIncidentPlanning(t, got)
	if got.Dependency != "gitlab" {
		t.Fatalf("Dependency = %q, want gitlab", got.Dependency)
	}
	if !strings.Contains(got.Evidence, "503") {
		t.Fatalf("Evidence = %q, want GitLab status evidence", got.Evidence)
	}
}

func TestClassifyExternalIncidentPlanning_ModelProviderRateLimit(t *testing.T) {
	t.Parallel()

	got := ClassifyExternalIncidentPlanning(ExternalIncidentPlanningInput{
		Body:       "The implementation was blocked by an external dependency incident.",
		LogExcerpt: "OpenAI request failed with 429 rate limit from model provider",
	})

	assertExternalIncidentPlanning(t, got)
	if got.Dependency != "model_provider" {
		t.Fatalf("Dependency = %q, want model_provider", got.Dependency)
	}
}

func TestClassifyExternalIncidentPlanning_IgnoresRepositoryFailure(t *testing.T) {
	t.Parallel()

	got := ClassifyExternalIncidentPlanning(ExternalIncidentPlanningInput{
		JobName:   "test",
		ErrorLine: "--- FAIL: TestCouncilPlanner (0.00s)",
	})

	if got.Class != "" {
		t.Fatalf("Class = %q, want no external classification: %+v", got.Class, got)
	}
	if got.OmitReason != "" {
		t.Fatalf("OmitReason = %q, want empty", got.OmitReason)
	}
}

func TestExternalIncidentPlanningRulesPromptSection_CodifiesConservativeFollowup(t *testing.T) {
	t.Parallel()

	section := ExternalIncidentPlanningRulesPromptSection()
	for _, want := range []string{
		"external_dependency_incident",
		ExternalDependencyIncidentLabel,
		"Backlog proposals MUST be actionable in this repository",
		"rerun GitLab CI until green",
		ExternalIncidentNoInRepoFollowUpReason,
	} {
		if !strings.Contains(section, want) {
			t.Fatalf("prompt section missing %q:\n%s", want, section)
		}
	}
}

func assertExternalIncidentPlanning(t *testing.T, got ExternalIncidentPlanningDecision) {
	t.Helper()
	if got.Class != CIIncidentExternalDependency {
		t.Fatalf("Class = %q, want %q: %+v", got.Class, CIIncidentExternalDependency, got)
	}
	if got.Disposition != CIIncidentDispositionWaitDependency {
		t.Fatalf("Disposition = %q, want %q: %+v", got.Disposition, CIIncidentDispositionWaitDependency, got)
	}
	if got.Label != ExternalDependencyIncidentLabel {
		t.Fatalf("Label = %q, want %q", got.Label, ExternalDependencyIncidentLabel)
	}
	if got.OmitReason != ExternalIncidentNoInRepoFollowUpReason {
		t.Fatalf("OmitReason = %q, want %q", got.OmitReason, ExternalIncidentNoInRepoFollowUpReason)
	}
	if got.InRepoFollowUpRequired {
		t.Fatal("InRepoFollowUpRequired = true, want false for default no-action external incident")
	}
	if got.RetryAllowed {
		t.Fatal("RetryAllowed = true, want false")
	}
}
