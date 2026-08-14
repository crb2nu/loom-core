package council

import (
	"strings"
)

const storageHealthPlanningBlockedReason = "planning blocked: storage health is unhealthy or unknown"

// externalDependencyIncidentClass mirrors the string value of
// contracts.CouncilCIIncidentExternalDependency. council cannot import
// internal/contracts: contracts/escalation.go imports pkg/mills/pipeline, and
// pipeline imports council, so the contracts import closes an import cycle.
// The label/reason mirrors already live in external_incident_rules.go; the
// parity test in internal/contracts/council_policy_parity_test.go pins all of
// them to the canonical constants so drift fails the build.
const externalDependencyIncidentClass = "external_dependency_incident"

// StorageHealthVerdict is the minimal storage-health evidence the planning
// policy consumes: whether planning is allowed and the probe status word. It
// deliberately mirrors the two fields read from gates.HealthDecision instead
// of importing pkg/mills/gates — that import closes the cycle
// gates(test) → pipeline → council → gates. The wiring seam adapts the full
// gate verdict down to this type.
type StorageHealthVerdict struct {
	Allowed bool
	Status  string
}

// ClassificationPolicyInput is the structured evidence a planner needs before
// it can turn a classified incident into backlog work. StorageHealth is
// required: a missing verdict is unknown and therefore blocks planning.
type ClassificationPolicyInput struct {
	IncidentClass string
	StorageHealth *StorageHealthVerdict
}

// ClassificationPolicyOutcome records deterministic proposal suppression
// performed from the incident classification contract.
type ClassificationPolicyOutcome struct {
	PlanningAllowed         bool
	FailClosed              bool
	OutsideSystemSuppressed int
	OmitReason              string
}

// ApplyClassificationPolicy mutates editor output before backlog mutation.
// It fails closed when storage health is missing or unsafe. For external
// dependency incidents it retains only repository-owned guardrail, classifier,
// telemetry, documentation, configuration, policy, or runbook follow-ups.
func ApplyClassificationPolicy(out *EditorOutput, in ClassificationPolicyInput) ClassificationPolicyOutcome {
	if !planningStorageHealthy(in.StorageHealth) {
		outcome := ClassificationPolicyOutcome{
			FailClosed:      true,
			OmitReason:      storageHealthPlanningBlockedReason,
			PlanningAllowed: false,
		}
		if out != nil {
			out.BacklogProposals = nil
			out.Sidecar.BacklogDeltas.Created = 0
			out.Sidecar.OmitReason = outcome.OmitReason
		}
		return outcome
	}

	outcome := ClassificationPolicyOutcome{PlanningAllowed: true}
	if out == nil || in.IncidentClass != externalDependencyIncidentClass {
		return outcome
	}

	kept := out.BacklogProposals[:0]
	for _, proposal := range out.BacklogProposals {
		if isNonActionableExternalProposal(proposal, true) {
			outcome.OutsideSystemSuppressed++
			continue
		}
		if !hasLabel(proposal.Labels, ExternalDependencyIncidentLabel) {
			proposal.Labels = append(proposal.Labels, ExternalDependencyIncidentLabel)
		}
		kept = append(kept, proposal)
	}
	out.BacklogProposals = kept
	out.Sidecar.BacklogDeltas.Created = len(kept)
	if len(kept) == 0 {
		outcome.OmitReason = ExternalIncidentNoInRepoFollowUpReason
		out.Sidecar.OmitReason = outcome.OmitReason
	}
	return outcome
}

func planningStorageHealthy(health *StorageHealthVerdict) bool {
	if health == nil || !health.Allowed {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(health.Status), "") ||
		strings.EqualFold(strings.TrimSpace(health.Status), "pass")
}
