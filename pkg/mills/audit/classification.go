package audit

import (
	"strings"

	"github.com/crb2nu/loom/pkg/mcperror"
	"github.com/crb2nu/loom/pkg/telemetry"
)

// ClassifyExternalCIFailureMessage maps GitLab CI-style dependency failures
// onto the shared external_dependency_incident contract.
func ClassifyExternalCIFailureMessage(message string) (CIFailureClassification, bool) {
	normalized := normalizeCIFailureMessage(message)
	if normalized == "" {
		return CIFailureClassification{}, false
	}
	incident, ok := mcperror.ClassifyExternalCIIncident(message)
	if !ok {
		return CIFailureClassification{}, false
	}
	return matchedExternalCIFailure(incident, stageFromMessage(normalized)), true
}

func matchedExternalCIFailure(incident mcperror.ExternalIncident, stage string) CIFailureClassification {
	reason := mcperror.ExternalIncidentReasonCode(incident)
	dependency := strings.TrimSpace(incident.Dependency)
	if dependency == "" {
		if code, ok := telemetry.LookupIncidentCodeByID(incident.ID); ok {
			dependency = code.Dependency
		}
	}
	return matchedCIFailure(ciFailureMatch{
		category:   CIFailureCategoryExternalDependency,
		stage:      stage,
		reason:     reason,
		dependency: dependency,
		retryable:  mcperror.ExternalIncidentRetryable(incident),
		terminal:   mcperror.ExternalIncidentTerminal(incident),
	})
}

func externalDependencyIncidentLabels(reason, dependency string) []string {
	labels := []string{
		"incident_class/" + string(telemetry.IncidentClassExternalDependency),
		"incident_code/" + reason,
	}
	if dependency != "" {
		labels = append(labels, "external_dependency/"+dependency)
	}
	return labels
}
