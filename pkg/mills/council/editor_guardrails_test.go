package council

import (
	"testing"

	"github.com/crb2nu/loom/pkg/mills/store"
)

func TestApplyEditorGuardrails_LabelsInRepoExternalDependencyFollowup(t *testing.T) {
	out := &EditorOutput{
		Documents: []ArtifactDoc{{
			Kind:  KindResearch,
			Title: "GitLab outage follow-up",
			Body:  "The incident was caused by a GitLab 503 external dependency failure.",
		}},
		BacklogProposals: []BacklogProposal{{
			Title: "Classify transient GitLab outages",
			PlanSlices: []PlanSliceSpec{{
				Name:  "classifier",
				Goal:  "record GitLab 503 as a dependency incident instead of a code defect",
				Files: []string{"pkg/mills/clients/gitlab.go"},
			}},
		}},
	}

	guard := ApplyEditorGuardrails(out)

	if !guard.ExternalDependencyIncident {
		t.Fatal("guard did not classify the dependency incident")
	}
	if guard.ExternalOnlyDropped != 0 {
		t.Fatalf("dropped %d proposals, want 0", guard.ExternalOnlyDropped)
	}
	if guard.LabelsAdded != 1 {
		t.Fatalf("labels added=%d, want 1", guard.LabelsAdded)
	}
	if len(out.BacklogProposals) != 1 {
		t.Fatalf("proposals=%d, want 1", len(out.BacklogProposals))
	}
	if !hasLabel(out.BacklogProposals[0].Labels, ExternalDependencyIncidentLabel) {
		t.Fatalf("proposal labels=%v, want %q", out.BacklogProposals[0].Labels, ExternalDependencyIncidentLabel)
	}
	if out.Sidecar.BacklogDeltas.Created != 1 {
		t.Fatalf("created=%d, want 1", out.Sidecar.BacklogDeltas.Created)
	}
}

func TestApplyEditorGuardrails_DropsExternalOnlyRemediation(t *testing.T) {
	out := &EditorOutput{
		Documents: []ArtifactDoc{{
			Kind:  KindImplementation,
			Title: "Provider incident",
			Body:  "OpenAI timed out during the incident; no repository defect was identified.",
		}},
		BacklogProposals: []BacklogProposal{
			{
				Title: "Ask provider support to increase OpenAI quota",
				Notes: "External dependency incident remediation with no repo files.",
			},
			{
				Title: "Document provider timeout triage",
				PlanSlices: []PlanSliceSpec{{
					Name:  "runbook",
					Goal:  "document OpenAI timeout handling for operators",
					Files: []string{"docs/mills-escalation-and-dependency-failures.md"},
				}},
			},
		},
		Sidecar: Sidecar{BacklogDeltas: SidecarBacklog{Created: 2}},
	}

	guard := ApplyEditorGuardrails(out)

	if guard.ExternalOnlyDropped != 1 {
		t.Fatalf("dropped=%d, want 1", guard.ExternalOnlyDropped)
	}
	if len(out.BacklogProposals) != 1 {
		t.Fatalf("proposals=%d, want 1", len(out.BacklogProposals))
	}
	if out.BacklogProposals[0].Title != "Document provider timeout triage" {
		t.Fatalf("kept proposal=%q", out.BacklogProposals[0].Title)
	}
	if !hasLabel(out.BacklogProposals[0].Labels, ExternalDependencyIncidentLabel) {
		t.Fatalf("kept proposal labels=%v, want incident label", out.BacklogProposals[0].Labels)
	}
	if out.Sidecar.BacklogDeltas.Created != 1 {
		t.Fatalf("created=%d, want 1 after drop", out.Sidecar.BacklogDeltas.Created)
	}
	if guard.Note() == "" {
		t.Fatal("guard note should describe the applied drop/label")
	}
}

func TestApplyEditorGuardrails_DropsFileBackedExternalIncidentOutsideAllowedFollowup(t *testing.T) {
	out := &EditorOutput{
		Documents: []ArtifactDoc{{
			Kind:  KindResearch,
			Title: "GitLab incident",
			Body:  "The council classified this as a GitLab external dependency incident.",
		}},
		BacklogProposals: []BacklogProposal{
			{
				Title: "Restart the GitLab runner pool",
				PlanSlices: []PlanSliceSpec{{
					Name:  "runner-restart",
					Goal:  "restart the external runner pool so CI turns green",
					Files: []string{"k8s/base/servers/gateway/deployment.yaml"},
				}},
			},
			{
				Title: "Patch unrelated parser behavior",
				PlanSlices: []PlanSliceSpec{{
					Name:  "parser",
					Goal:  "change parser behavior even though no repository defect was found",
					Files: []string{"pkg/mills/council/brief.go"},
				}},
			},
		},
		Sidecar: Sidecar{BacklogDeltas: SidecarBacklog{Created: 2}},
	}

	guard := ApplyEditorGuardrails(out)

	if guard.ExternalOnlyDropped != 2 {
		t.Fatalf("dropped=%d, want 2", guard.ExternalOnlyDropped)
	}
	if len(out.BacklogProposals) != 0 {
		t.Fatalf("proposals=%d, want empty fallback: %#v", len(out.BacklogProposals), out.BacklogProposals)
	}
	if out.Sidecar.BacklogDeltas.Created != 0 {
		t.Fatalf("created=%d, want 0 after drop", out.Sidecar.BacklogDeltas.Created)
	}
	if out.Sidecar.OmitReason != ExternalIncidentNoInRepoFollowUpReason {
		t.Fatalf("omit_reason=%q, want %q", out.Sidecar.OmitReason, ExternalIncidentNoInRepoFollowUpReason)
	}
}

func TestApplyEditorGuardrails_PreservesAllowedExternalIncidentFollowupCategories(t *testing.T) {
	out := &EditorOutput{
		Documents: []ArtifactDoc{{
			Kind:  KindResearch,
			Title: "Registry incident",
			Body:  "The run was blocked by a container registry external dependency incident.",
		}},
		BacklogProposals: []BacklogProposal{
			{
				Title: "Add registry outage guardrail",
				PlanSlices: []PlanSliceSpec{{
					Name:  "guardrail",
					Goal:  "stop autonomous retries when registry outage evidence repeats",
					Files: []string{"pkg/mills/council/editor_guardrails.go"},
				}},
			},
			{
				Title: "Document registry outage triage",
				PlanSlices: []PlanSliceSpec{{
					Name:  "docs",
					Goal:  "document operator handling for registry outage evidence",
					Files: []string{"docs/external-dependency-incidents.md"},
				}},
			},
			{
				Title: "Emit registry incident telemetry",
				Slices: []store.Slice{{
					Name:  "telemetry",
					Files: []string{"pkg/mills/council/telemetry.go"},
				}},
			},
			{
				Title: "Tune external incident configuration",
				PlanSlices: []PlanSliceSpec{{
					Name:  "config",
					Goal:  "add external incident config for registry failures",
					Files: []string{"k8s/base/kustomization.yaml"},
				}},
			},
		},
		Sidecar: Sidecar{BacklogDeltas: SidecarBacklog{Created: 4}},
	}

	guard := ApplyEditorGuardrails(out)

	if guard.ExternalOnlyDropped != 0 {
		t.Fatalf("dropped=%d, want 0", guard.ExternalOnlyDropped)
	}
	if len(out.BacklogProposals) != 4 {
		t.Fatalf("proposals=%d, want 4", len(out.BacklogProposals))
	}
	if guard.LabelsAdded != 4 {
		t.Fatalf("labels added=%d, want 4", guard.LabelsAdded)
	}
	for _, p := range out.BacklogProposals {
		if !hasLabel(p.Labels, ExternalDependencyIncidentLabel) {
			t.Fatalf("proposal %q labels=%v, want incident label", p.Title, p.Labels)
		}
	}
}

func TestApplyEditorGuardrails_AllowedTermsRequireDelimitedMatch(t *testing.T) {
	out := &EditorOutput{
		Documents: []ArtifactDoc{{
			Kind:  KindResearch,
			Title: "Provider incident",
			Body:  "The observed failure was an OpenAI external dependency incident.",
		}},
		BacklogProposals: []BacklogProposal{
			{
				Title: "Patch login behavior",
				PlanSlices: []PlanSliceSpec{{
					Name:  "login",
					Goal:  "change login behavior even though no repository defect was found",
					Files: []string{"pkg/mills/council/brief.go"},
				}},
			},
			{
				Title: "Add provider incident log context",
				PlanSlices: []PlanSliceSpec{{
					Name:  "log-context",
					Goal:  "log provider timeout evidence for operator triage",
					Files: []string{"pkg/mills/council/brief.go"},
				}},
			},
		},
		Sidecar: Sidecar{BacklogDeltas: SidecarBacklog{Created: 2}},
	}

	guard := ApplyEditorGuardrails(out)

	if guard.ExternalOnlyDropped != 1 {
		t.Fatalf("dropped=%d, want 1", guard.ExternalOnlyDropped)
	}
	if len(out.BacklogProposals) != 1 {
		t.Fatalf("proposals=%d, want 1: %#v", len(out.BacklogProposals), out.BacklogProposals)
	}
	if out.BacklogProposals[0].Title != "Add provider incident log context" {
		t.Fatalf("kept proposal=%q", out.BacklogProposals[0].Title)
	}
}

func TestApplyEditorGuardrails_DropsSpeculativeExternalRemediationPhrases(t *testing.T) {
	out := &EditorOutput{
		BacklogProposals: []BacklogProposal{
			{Title: "Remediate GitLab"},
			{Title: "Rerun GitLab CI until green"},
			{Title: "Restart the external service"},
			{Title: "Change OpenAI credentials"},
			{Title: "Open support ticket to increase provider quota"},
		},
		Sidecar: Sidecar{BacklogDeltas: SidecarBacklog{Created: 5}},
	}

	guard := ApplyEditorGuardrails(out)

	if guard.ExternalOnlyDropped != 5 {
		t.Fatalf("dropped=%d, want 5", guard.ExternalOnlyDropped)
	}
	if len(out.BacklogProposals) != 0 {
		t.Fatalf("proposals=%d, want 0: %#v", len(out.BacklogProposals), out.BacklogProposals)
	}
	if out.Sidecar.BacklogDeltas.Created != 0 {
		t.Fatalf("created=%d, want 0 after dropping external-only proposals", out.Sidecar.BacklogDeltas.Created)
	}
	if guard.OmitReason != ExternalIncidentNoInRepoFollowUpReason {
		t.Fatalf("omit reason=%q, want %q", guard.OmitReason, ExternalIncidentNoInRepoFollowUpReason)
	}
	if out.Sidecar.OmitReason != ExternalIncidentNoInRepoFollowUpReason {
		t.Fatalf("sidecar omit_reason=%q, want %q", out.Sidecar.OmitReason, ExternalIncidentNoInRepoFollowUpReason)
	}
	if !containsAny(guard.Note(), []string{ExternalIncidentNoInRepoFollowUpReason}) {
		t.Fatalf("guard note should include omit reason, got %q", guard.Note())
	}
}

func TestApplyEditorGuardrails_ExternalIncidentDropsFilelessExternalFollowup(t *testing.T) {
	out := &EditorOutput{
		Documents: []ArtifactDoc{{
			Kind:  KindResearch,
			Title: "GitLab CI incident",
			Body:  "GitLab CI returned 503 during the external dependency incident.",
		}},
		BacklogProposals: []BacklogProposal{{
			Title: "Add GitLab CI external dependency incident classification to Mills",
			Notes: "Sounds repo-local, but the editor supplied no files or slices.",
		}},
		Sidecar: Sidecar{BacklogDeltas: SidecarBacklog{Created: 1}},
	}

	guard := ApplyEditorGuardrails(out)

	if !guard.ExternalDependencyIncident {
		t.Fatal("guard did not classify the external dependency incident")
	}
	if guard.ExternalOnlyDropped != 1 {
		t.Fatalf("dropped=%d, want 1", guard.ExternalOnlyDropped)
	}
	if len(out.BacklogProposals) != 0 {
		t.Fatalf("proposals=%d, want empty fallback: %#v", len(out.BacklogProposals), out.BacklogProposals)
	}
	if out.Sidecar.BacklogDeltas.Created != 0 {
		t.Fatalf("created=%d, want 0 after empty fallback", out.Sidecar.BacklogDeltas.Created)
	}
	if guard.OmitReason != ExternalIncidentNoInRepoFollowUpReason {
		t.Fatalf("omit reason=%q, want %q", guard.OmitReason, ExternalIncidentNoInRepoFollowUpReason)
	}
}

func TestApplyEditorGuardrails_SetsMandatedOmitReasonForExternalIncidentWithNoProposals(t *testing.T) {
	out := &EditorOutput{
		Documents: []ArtifactDoc{{
			Kind:  KindResearch,
			Title: "GitLab incident",
			Body:  "The run was blocked by a GitLab external dependency incident and no repository defect was found.",
		}},
		Sidecar: Sidecar{OmitReason: "provider issue"},
	}

	guard := ApplyEditorGuardrails(out)

	if !guard.ExternalDependencyIncident {
		t.Fatal("guard did not classify the external dependency incident")
	}
	if guard.OmitReason != ExternalIncidentNoInRepoFollowUpReason {
		t.Fatalf("guard omit reason=%q, want %q", guard.OmitReason, ExternalIncidentNoInRepoFollowUpReason)
	}
	if out.Sidecar.OmitReason != ExternalIncidentNoInRepoFollowUpReason {
		t.Fatalf("omit_reason=%q, want %q", out.Sidecar.OmitReason, ExternalIncidentNoInRepoFollowUpReason)
	}
	if out.Sidecar.BacklogDeltas.Created != 0 {
		t.Fatalf("created=%d, want 0", out.Sidecar.BacklogDeltas.Created)
	}
}

func TestApplyEditorGuardrails_DoesNotRewriteOrdinaryOmitReason(t *testing.T) {
	out := &EditorOutput{
		Documents: []ArtifactDoc{{
			Kind:  KindImplementation,
			Title: "Single unit",
			Body:  "This parser hardening plan is one merge-sized unit.",
		}},
		Sidecar: Sidecar{OmitReason: "single merge-sized unit"},
	}

	guard := ApplyEditorGuardrails(out)

	if guard.Applied() {
		t.Fatalf("guard applied unexpectedly: %+v", guard)
	}
	if out.Sidecar.OmitReason != "single merge-sized unit" {
		t.Fatalf("omit_reason=%q, want ordinary reason preserved", out.Sidecar.OmitReason)
	}
}

func TestApplyEditorGuardrails_PreservesLegacySliceFileBackedFollowup(t *testing.T) {
	out := &EditorOutput{
		Documents: []ArtifactDoc{{
			Kind:  KindResearch,
			Title: "Registry outage",
			Body:  "The release was blocked by an external dependency incident in the container registry.",
		}},
		BacklogProposals: []BacklogProposal{{
			Title: "Add registry outage classifier",
			Slices: []store.Slice{{
				Name:  "classifier",
				Files: []string{"pkg/mills/pipeline/error_class.go"},
				Tests: []string{"go test ./pkg/mills/pipeline"},
			}},
		}},
		Sidecar: Sidecar{BacklogDeltas: SidecarBacklog{Created: 1}},
	}

	guard := ApplyEditorGuardrails(out)

	if guard.ExternalOnlyDropped != 0 {
		t.Fatalf("dropped=%d, want 0", guard.ExternalOnlyDropped)
	}
	if len(out.BacklogProposals) != 1 {
		t.Fatalf("proposals=%d, want 1", len(out.BacklogProposals))
	}
	if !hasLabel(out.BacklogProposals[0].Labels, ExternalDependencyIncidentLabel) {
		t.Fatalf("proposal labels=%v, want %q", out.BacklogProposals[0].Labels, ExternalDependencyIncidentLabel)
	}
}

func TestApplyEditorGuardrails_DoesNotLabelOrdinaryRepoWork(t *testing.T) {
	out := &EditorOutput{
		Documents: []ArtifactDoc{{
			Kind:  KindProductSpec,
			Title: "Parser hardening",
			Body:  "Tighten validation for malformed backlog JSON.",
		}},
		BacklogProposals: []BacklogProposal{{
			Title: "Harden backlog parser",
			PlanSlices: []PlanSliceSpec{{
				Name:  "parser",
				Goal:  "reject malformed JSON with clear diagnostics",
				Files: []string{"pkg/mills/clients/council_proposals.go"},
			}},
		}},
	}

	guard := ApplyEditorGuardrails(out)

	if guard.Applied() {
		t.Fatalf("guard applied unexpectedly: %+v", guard)
	}
	if hasLabel(out.BacklogProposals[0].Labels, ExternalDependencyIncidentLabel) {
		t.Fatalf("ordinary work was labeled as dependency incident: %v", out.BacklogProposals[0].Labels)
	}
}

func TestEditorGuardrailsPromptSection_InstructsInRepoOnlyFollowup(t *testing.T) {
	section := EditorGuardrailsPromptSection()
	for _, want := range []string{
		ExternalDependencyIncidentLabel,
		"Backlog proposals MUST be actionable in this repository",
		"no actionable in-repo follow-up",
		// 2026-07-26: the scope-authoring contract rides the same seam into
		// the stable prompt prefix (slice_scope_rules.go).
		"Slice scope — list every directory the work touches",
	} {
		if !containsAny(section, []string{want}) {
			t.Fatalf("prompt section missing %q:\n%s", want, section)
		}
	}
}
