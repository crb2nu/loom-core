package council

import (
	"strings"
	"testing"

	"github.com/crb2nu/loom/pkg/mcperror"
)

func TestFormatExternalEscalation_KnownIncidentLabelsAndRoutesToRunbook(t *testing.T) {
	t.Parallel()

	got, ok := FormatExternalEscalation(ExternalEscalationRenderInput{
		Reason: "ci_watch failed: gitlab: GET /projects/47/pipelines: status 401: unauthorized",
	})
	if !ok {
		t.Fatal("FormatExternalEscalation returned ok=false")
	}
	if got.Incident.ID != mcperror.ExternalIncidentIDGitLabAuthFailure {
		t.Fatalf("Incident.ID = %q, want %q", got.Incident.ID, mcperror.ExternalIncidentIDGitLabAuthFailure)
	}
	assertContainsAll(t, got.Markdown,
		"### External dependency incident",
		"**Incident class**: `external_dependency_incident`",
		"**Disposition**: `wait_for_dependency_recovery`",
		"**Dependency**: `gitlab`",
		"**Signature**: `external_dependency.gitlab.auth_failure`",
		"**Summary**: GitLab API authentication failed",
		"**Local action**: `follow_external_dependency_runbook`",
		"**Runbook**: `docs/mills-escalation-and-dependency-failures.md`",
		"Do not create speculative in-repo remediation work",
	)
}

func TestFormatExternalEscalation_UsesLastLogTailEvidence(t *testing.T) {
	t.Parallel()

	got, ok := FormatExternalEscalation(ExternalEscalationRenderInput{
		Reason:      "stage ci_watch errored after retries",
		LastLogTail: "failed to solve: error writing manifest blob: failed commit on ref",
	})
	if !ok {
		t.Fatal("FormatExternalEscalation returned ok=false")
	}
	if got.Incident.Dependency != "container_registry_blob_storage" {
		t.Fatalf("Dependency = %q, want container_registry_blob_storage", got.Incident.Dependency)
	}
	assertContainsAll(t, got.Markdown,
		"container_registry_blob_storage",
		"container registry blob storage rejected manifest/cache writes",
		"follow_external_dependency_runbook",
		"error writing manifest blob",
	)
}

func TestFormatExternalEscalation_UnknownFailureIsNotSpeculative(t *testing.T) {
	t.Parallel()

	got, ok := FormatExternalEscalation(ExternalEscalationRenderInput{
		Reason:      "stage tests exceeded retries [class=code]: go test ./...",
		LastLogTail: "--- FAIL: TestWidget\nwidget.go:42: got false, want true",
	})
	if ok {
		t.Fatalf("FormatExternalEscalation returned ok=true for code failure: %+v", got)
	}
}

func assertContainsAll(t *testing.T, haystack string, needles ...string) {
	t.Helper()
	for _, needle := range needles {
		if !strings.Contains(haystack, needle) {
			t.Fatalf("markdown missing %q:\n%s", needle, haystack)
		}
	}
}
