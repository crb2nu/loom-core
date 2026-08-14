package council

import (
	"strings"
	"testing"
)

func TestApplyEditorGuardrails_PreservesGitLabCIIncidentClassificationInPlanningOutput(t *testing.T) {
	t.Parallel()

	out := &EditorOutput{
		Documents: []ArtifactDoc{{
			Kind:  KindResearch,
			Title: "GitLab CI dependency incident",
			Body:  "ci_watch failed while reading pipeline status: gitlab: GET /projects/47/pipelines: status 503: Service Unavailable.",
		}},
		BacklogProposals: []BacklogProposal{{
			Title: "Document GitLab CI dependency incident triage",
			PlanSlices: []PlanSliceSpec{{
				Name:  "runbook",
				Goal:  "preserve GitLab CI outage evidence as external dependency incident planning context",
				Files: []string{"docs/mills-escalation-and-dependency-failures.md"},
			}},
		}},
		Sidecar: Sidecar{BacklogDeltas: SidecarBacklog{Created: 1}},
	}

	guard := ApplyEditorGuardrails(out)

	if !guard.ExternalDependencyIncident {
		t.Fatal("guard did not classify the GitLab CI failure as an external dependency incident")
	}
	if guard.Incident.Class != CIIncidentExternalDependency {
		t.Fatalf("incident class = %q, want %q: %+v", guard.Incident.Class, CIIncidentExternalDependency, guard.Incident)
	}
	if guard.Incident.Dependency != "gitlab" {
		t.Fatalf("incident dependency = %q, want gitlab: %+v", guard.Incident.Dependency, guard.Incident)
	}
	if !strings.Contains(guard.Incident.Evidence, "503") {
		t.Fatalf("incident evidence = %q, want GitLab status evidence", guard.Incident.Evidence)
	}
	if !hasLabel(out.BacklogProposals[0].Labels, ExternalDependencyIncidentLabel) {
		t.Fatalf("proposal labels = %v, want %q", out.BacklogProposals[0].Labels, ExternalDependencyIncidentLabel)
	}

	note := guard.Note()
	for _, want := range []string{
		"class=`external_dependency_incident`",
		"external=`gitlab`",
		"503",
	} {
		if !strings.Contains(note, want) {
			t.Fatalf("guard note missing %q:\n%s", want, note)
		}
	}
}
