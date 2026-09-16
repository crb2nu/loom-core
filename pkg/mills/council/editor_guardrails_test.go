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

// The incident contract is scoped to proposals that address the external
// dependency. A file-backed proposal about the outside system that is not an
// allowed follow-up is dropped; a file-backed proposal that never mentions the
// outside system is ordinary repo work and survives, unlabeled.
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

	if guard.ExternalOnlyDropped != 1 {
		t.Fatalf("dropped=%d, want 1 (only the runner restart)", guard.ExternalOnlyDropped)
	}
	if guard.RepoScopedPreserved != 1 {
		t.Fatalf("repo-scoped preserved=%d, want 1", guard.RepoScopedPreserved)
	}
	if len(out.BacklogProposals) != 1 || out.BacklogProposals[0].Title != "Patch unrelated parser behavior" {
		t.Fatalf("proposals=%#v, want only the unrelated parser proposal", out.BacklogProposals)
	}
	if hasLabel(out.BacklogProposals[0].Labels, ExternalDependencyIncidentLabel) {
		t.Fatalf("unrelated repo work was labeled as an incident follow-up: %v", out.BacklogProposals[0].Labels)
	}
	if out.Sidecar.BacklogDeltas.Created != 1 {
		t.Fatalf("created=%d, want 1 after drop", out.Sidecar.BacklogDeltas.Created)
	}
	if out.Sidecar.OmitReason != "" {
		t.Fatalf("omit_reason=%q, want empty because a proposal survived", out.Sidecar.OmitReason)
	}
	if !containsAny(guard.Note(), []string{"1 repo-scoped proposal preserved"}) {
		t.Fatalf("guard note should count the preserved proposal, got %q", guard.Note())
	}
}

// TestApplyEditorGuardrails_PreservesRepoScopedWorkDuringIncident replays the
// live 2026-09-02 12:00Z council run: the research section classified the
// workspace's GitLab CI / Longhorn error clusters as an external dependency
// incident, and the twelve-step implementation plan it emitted alongside was
// entirely repo-scoped. The guard used to drop every step as "external-only";
// `council_yield` then read 50 runs / $133 with no backlog delta.
func TestApplyEditorGuardrails_PreservesRepoScopedWorkDuringIncident(t *testing.T) {
	out := &EditorOutput{
		Documents: []ArtifactDoc{{
			Kind: KindResearch,
			Body: "### External dependency incidents (classified, no outside-system remediation proposed)\n" +
				"`ci/main` GitLab CI pipeline failures and Longhorn replica-scheduler storage errors are `external_dependency_incident`.",
		}},
		BacklogProposals: []BacklogProposal{
			{
				Title: "Cross-repo stamp target project",
				PlanSlices: []PlanSliceSpec{{
					Name:  "schema",
					Goal:  "add a target project field to the stamp type and honor it on apply",
					Files: []string{"pkg/mills/store/stamp.go", "pkg/mills/crossrepo/apply.go"},
				}},
			},
			{
				Title: "Split internal/hud/spawn.go",
				PlanSlices: []PlanSliceSpec{{
					Name:  "extract",
					Goal:  "mechanical extraction, no API change",
					Files: []string{"internal/hud/spawn.go"},
				}},
			},
			{
				Title: "Rerun the ci/main GitLab pipeline until green",
				Notes: "external remediation with no repo files",
			},
		},
		Sidecar: Sidecar{BacklogDeltas: SidecarBacklog{Created: 3}},
	}

	guard := ApplyEditorGuardrails(out)

	if !guard.ExternalDependencyIncident {
		t.Fatal("guard did not classify the run's external dependency incident")
	}
	if guard.ExternalOnlyDropped != 1 {
		t.Fatalf("dropped=%d, want 1 (the pipeline rerun)", guard.ExternalOnlyDropped)
	}
	if guard.RepoScopedPreserved != 2 {
		t.Fatalf("repo-scoped preserved=%d, want 2", guard.RepoScopedPreserved)
	}
	if guard.LabelsAdded != 0 {
		t.Fatalf("labels added=%d, want 0 — unrelated work must not carry the incident label", guard.LabelsAdded)
	}
	if len(out.BacklogProposals) != 2 {
		t.Fatalf("proposals=%d, want 2: %#v", len(out.BacklogProposals), out.BacklogProposals)
	}
	if out.Sidecar.BacklogDeltas.Created != 2 {
		t.Fatalf("created=%d, want 2", out.Sidecar.BacklogDeltas.Created)
	}
	if out.Sidecar.OmitReason != "" {
		t.Fatalf("omit_reason=%q, want empty", out.Sidecar.OmitReason)
	}
	if !containsAny(guard.Note(), []string{"2 repo-scoped proposals preserved"}) {
		t.Fatalf("guard note = %q, want the preserved count", guard.Note())
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
				// References the provider (so the incident contract applies)
				// but "login" must not satisfy the "log" follow-up term.
				Title: "Patch OpenAI login behavior",
				PlanSlices: []PlanSliceSpec{{
					Name:  "login",
					Goal:  "change OpenAI login behavior even though no repository defect was found",
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
