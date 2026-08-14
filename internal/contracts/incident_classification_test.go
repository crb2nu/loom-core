package contracts

import "testing"

func TestCouncilCIIncidentClassificationExternalDependencyContract(t *testing.T) {
	got := CouncilCIIncidentClassification{
		Class:                  CouncilCIIncidentExternalDependency,
		Disposition:            CouncilCIIncidentDispositionWaitDependency,
		Dependency:             "model_provider",
		Evidence:               "OpenAI request failed with 429 rate limit from model provider",
		Reason:                 "failure points to a shared dependency outside the branch diff",
		Confidence:             0.93,
		RetryAllowed:           false,
		Label:                  CouncilExternalDependencyIncidentLabel,
		OmitReason:             CouncilExternalIncidentNoInRepoFollowUpReason,
		InRepoFollowUpRequired: false,
	}

	assertGolden(t, "council_ci_incident_external_dependency", marshalIndent(t, got))
}
