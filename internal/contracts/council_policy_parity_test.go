package contracts_test

import (
	"testing"

	"github.com/crb2nu/loom/internal/contracts"
	"github.com/crb2nu/loom/pkg/mills/council"
)

// council mirrors these contract values as local literals because importing
// internal/contracts from pkg/mills/council closes an import cycle
// (contracts/escalation.go → pipeline → council). This external test package
// can see both sides, so drift between the mirror and the canonical constant
// fails the build here instead of silently forking the wire values.
func TestCouncilClassificationPolicyMirrorsContractValues(t *testing.T) {
	if council.ExternalDependencyIncidentLabel != contracts.CouncilExternalDependencyIncidentLabel {
		t.Fatalf("council label mirror = %q, contract = %q",
			council.ExternalDependencyIncidentLabel, contracts.CouncilExternalDependencyIncidentLabel)
	}
	if council.ExternalIncidentNoInRepoFollowUpReason != contracts.CouncilExternalIncidentNoInRepoFollowUpReason {
		t.Fatalf("council reason mirror = %q, contract = %q",
			council.ExternalIncidentNoInRepoFollowUpReason, contracts.CouncilExternalIncidentNoInRepoFollowUpReason)
	}
	if got := string(contracts.CouncilCIIncidentExternalDependency); got != "external_dependency_incident" {
		t.Fatalf("external dependency class = %q, council mirror expects %q", got, "external_dependency_incident")
	}
}
