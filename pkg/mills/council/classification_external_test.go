package council

import (
	"strings"
	"testing"

	"github.com/crb2nu/loom/pkg/mills/store"
)

func TestClassifyExternalWorkspaceSignals_NormalizesCanonicalIncident(t *testing.T) {
	t.Parallel()

	got := ClassifyExternalWorkspaceSignals([]WorkspaceSignal{
		{
			Source:  "loki",
			Service: "mcp-gitlab",
			Count:   7,
			Sample:  "gitlab: GET /projects/47/pipelines: status 401: unauthorized",
		},
		{
			Source:  "loki",
			Service: "mcp-memory",
			Count:   2,
			Sample:  "repository validation failed for malformed JSON",
		},
	})

	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].IncidentClass != CIIncidentExternalDependency {
		t.Fatalf("IncidentClass = %q, want %q: %+v", got[0].IncidentClass, CIIncidentExternalDependency, got[0])
	}
	if got[0].ExternalDependency != "gitlab" {
		t.Fatalf("ExternalDependency = %q, want gitlab", got[0].ExternalDependency)
	}
	if got[1].IncidentClass != "" {
		t.Fatalf("ordinary signal classified as %q: %+v", got[1].IncidentClass, got[1])
	}
}

func TestRenderSignals_IncludesCanonicalExternalIncidentClass(t *testing.T) {
	t.Parallel()

	signals := ClassifyExternalWorkspaceSignals([]WorkspaceSignal{{
		Source:  "loki",
		Service: "model-worker",
		Count:   3,
		Sample:  "OpenAI request failed with 429 rate limit from model provider",
	}})

	got := renderSignals(signals, workspaceSignalDefaultWindow)

	for _, want := range []string{
		"class=`external_dependency_incident`",
		"external=`model_provider`",
		"model-worker",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("rendered signals missing %q:\n%s", want, got)
		}
	}
}

func TestRenderClassifiedCIFailures_ExternalDependencyWinsCanonicalClass(t *testing.T) {
	t.Parallel()

	got := renderClassifiedCIFailures([]*store.ClassifiedCIFailureSummary{{
		RunID:                "PIPE-CI-AUTH",
		BacklogID:            "MILLS-CI-1",
		BacklogTitle:         "GitLab auth outage",
		FailureClass:         "configuration",
		ExternalDependencyID: "external_dependency.gitlab.auth_failure",
		ExternalDependency:   "gitlab",
	}})

	for _, want := range []string{
		"class=`external_dependency_incident`",
		"external=`gitlab`",
		"GitLab auth outage",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("classified CI render missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "class=`configuration`") {
		t.Fatalf("classified CI render kept low-level class instead of canonical external incident:\n%s", got)
	}
}

func TestApplyEditorGuardrails_ClassifiesKnownExternalIncidentWithoutIncidentPhrase(t *testing.T) {
	t.Parallel()

	out := &EditorOutput{
		Documents: []ArtifactDoc{{
			Kind:  KindResearch,
			Title: "GitLab API failure",
			Body:  "ci_watch saw gitlab: GET /projects/47/pipelines: status 401: unauthorized.",
		}},
		BacklogProposals: []BacklogProposal{{
			Title: "Add GitLab auth failure runbook",
			PlanSlices: []PlanSliceSpec{{
				Name:  "runbook",
				Goal:  "document local handling for GitLab auth failures",
				Files: []string{"docs/council-external-dependency-incidents.md"},
			}},
		}},
	}

	guard := ApplyEditorGuardrails(out)

	if !guard.ExternalDependencyIncident {
		t.Fatal("guard did not classify the known external dependency incident")
	}
	if guard.LabelsAdded != 1 {
		t.Fatalf("LabelsAdded = %d, want 1", guard.LabelsAdded)
	}
	if !hasLabel(out.BacklogProposals[0].Labels, ExternalDependencyIncidentLabel) {
		t.Fatalf("proposal labels = %v, want %q", out.BacklogProposals[0].Labels, ExternalDependencyIncidentLabel)
	}
}
