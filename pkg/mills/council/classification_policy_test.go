package council

import (
	"testing"
)

func TestApplyClassificationPolicy_SuppressesOutsideSystemExternalRemediation(t *testing.T) {
	t.Parallel()
	out := &EditorOutput{BacklogProposals: []BacklogProposal{
		{Title: "Ask GitLab to restart its runners", PlanSlices: []PlanSliceSpec{{Files: []string{"k8s/base/servers/gateway/deployment.yaml"}}}},
		{Title: "Document GitLab outage triage", PlanSlices: []PlanSliceSpec{{Files: []string{"docs/council-external-dependency-incidents.md"}}}},
	}, Sidecar: Sidecar{BacklogDeltas: SidecarBacklog{Created: 2}}}

	got := ApplyClassificationPolicy(out, ClassificationPolicyInput{
		IncidentClass: externalDependencyIncidentClass,
		StorageHealth: &StorageHealthVerdict{Allowed: true, Status: "pass"},
	})

	if !got.PlanningAllowed || got.FailClosed {
		t.Fatalf("outcome = %+v, want allowed planning", got)
	}
	if got.OutsideSystemSuppressed != 1 {
		t.Fatalf("suppressed = %d, want 1", got.OutsideSystemSuppressed)
	}
	if len(out.BacklogProposals) != 1 || out.BacklogProposals[0].Title != "Document GitLab outage triage" {
		t.Fatalf("proposals = %#v, want only local documentation follow-up", out.BacklogProposals)
	}
	if !hasLabel(out.BacklogProposals[0].Labels, ExternalDependencyIncidentLabel) {
		t.Fatalf("labels = %v, want %q", out.BacklogProposals[0].Labels, ExternalDependencyIncidentLabel)
	}
}

func TestApplyClassificationPolicy_ExternalIncidentWithNoLocalFollowupSetsContractOmitReason(t *testing.T) {
	t.Parallel()
	out := &EditorOutput{BacklogProposals: []BacklogProposal{{Title: "Increase provider quota"}}}

	got := ApplyClassificationPolicy(out, ClassificationPolicyInput{
		IncidentClass: externalDependencyIncidentClass,
		StorageHealth: &StorageHealthVerdict{Allowed: true, Status: "pass"},
	})

	if got.OmitReason != ExternalIncidentNoInRepoFollowUpReason {
		t.Fatalf("omit reason = %q, want %q", got.OmitReason, ExternalIncidentNoInRepoFollowUpReason)
	}
	if len(out.BacklogProposals) != 0 || out.Sidecar.OmitReason != got.OmitReason {
		t.Fatalf("output = %+v, want suppressed proposals and contract omit reason", out)
	}
}

func TestApplyClassificationPolicy_FailsClosedForUnknownOrUnhealthyStorage(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		health *StorageHealthVerdict
	}{
		{name: "unknown", health: nil},
		{name: "unhealthy", health: &StorageHealthVerdict{Allowed: false, Status: "block"}},
		{name: "non-passing status", health: &StorageHealthVerdict{Allowed: true, Status: "degraded"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := &EditorOutput{BacklogProposals: []BacklogProposal{{Title: "Repository change"}}, Sidecar: Sidecar{BacklogDeltas: SidecarBacklog{Created: 1}}}
			got := ApplyClassificationPolicy(out, ClassificationPolicyInput{StorageHealth: tc.health})
			if got.PlanningAllowed || !got.FailClosed || got.OmitReason != storageHealthPlanningBlockedReason {
				t.Fatalf("outcome = %+v, want fail-closed storage block", got)
			}
			if len(out.BacklogProposals) != 0 || out.Sidecar.BacklogDeltas.Created != 0 || out.Sidecar.OmitReason != got.OmitReason {
				t.Fatalf("output = %+v, want cleared proposals and block reason", out)
			}
		})
	}
}

func TestApplyClassificationPolicy_PreservesOrdinaryClassification(t *testing.T) {
	t.Parallel()
	out := &EditorOutput{BacklogProposals: []BacklogProposal{{Title: "Fix branch regression"}}}

	got := ApplyClassificationPolicy(out, ClassificationPolicyInput{
		IncidentClass: "repository_regression",
		StorageHealth: &StorageHealthVerdict{Allowed: true},
	})

	if !got.PlanningAllowed || len(out.BacklogProposals) != 1 {
		t.Fatalf("outcome/output = %+v/%+v, want ordinary proposal preserved", got, out)
	}
}
