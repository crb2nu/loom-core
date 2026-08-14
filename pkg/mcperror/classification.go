package mcperror

import (
	"strings"

	"github.com/crb2nu/loom/pkg/telemetry"
)

const (
	ExternalIncidentClassExternalDependency = string(telemetry.IncidentClassExternalDependency)
)

// ClassifyExternalCIIncident recognizes external dependency failures that
// commonly surface in GitLab CI polling, merge, and job-log paths.
func ClassifyExternalCIIncident(text string) (ExternalIncident, bool) {
	if incident, ok := ClassifyExternalIncident(text); ok {
		return incident, true
	}
	for _, line := range strings.Split(text, "\n") {
		evidence := strings.TrimSpace(line)
		if evidence == "" {
			continue
		}
		lower := strings.ToLower(evidence)
		switch {
		case isGitLabAgentUnauthenticatedIncident(lower):
			return externalIncidentFromCode(telemetry.IncidentReasonGitLabAuthFailure, evidence), true
		case isGitLabRateLimitIncident(lower):
			return externalIncidentFromCode(telemetry.IncidentReasonGitLabRateLimit, evidence), true
		case isGitLabServiceUnavailableIncident(lower):
			return externalIncidentFromCode(telemetry.IncidentReasonGitLabServiceUnavailable, evidence), true
		case isGitLabCIPipelineFailureIncident(lower):
			return externalIncidentFromCode(telemetry.IncidentReasonGitLabServiceUnavailable, evidence), true
		}
	}
	return ExternalIncident{}, false
}

// ExternalIncidentReasonCode returns the stable reason code associated with a
// classified external incident.
func ExternalIncidentReasonCode(incident ExternalIncident) string {
	if code, ok := telemetry.LookupIncidentCodeByID(incident.ID); ok {
		return string(code.Reason)
	}
	reason := strings.TrimPrefix(incident.ID, "external_dependency.")
	reason = strings.ReplaceAll(reason, ".", "-")
	reason = strings.ReplaceAll(reason, "_", "-")
	if reason == "" {
		return "external-dependency"
	}
	return reason
}

// ExternalIncidentRetryable reports whether retrying the owning CI stage is an
// appropriate bounded response for this external incident.
func ExternalIncidentRetryable(incident ExternalIncident) bool {
	if code, ok := telemetry.LookupIncidentCodeByID(incident.ID); ok {
		return code.Retryable
	}
	return false
}

// ExternalIncidentTerminal reports whether the incident should stop local
// repository retry loops until the dependency recovers or credentials/config
// are fixed outside this branch.
func ExternalIncidentTerminal(incident ExternalIncident) bool {
	if code, ok := telemetry.LookupIncidentCodeByID(incident.ID); ok {
		return code.Terminal
	}
	return true
}

func externalIncidentFromCode(reason telemetry.IncidentReasonCode, evidence string) ExternalIncident {
	code, _ := telemetry.LookupIncidentCode(reason)
	return ExternalIncident{
		ID:         code.ID,
		Kind:       code.Kind,
		Dependency: code.Dependency,
		Summary:    code.Summary,
		Evidence:   evidence,
	}
}

func isGitLabRateLimitIncident(line string) bool {
	if !strings.Contains(line, "gitlab") {
		return false
	}
	for _, needle := range []string{"status 429", "429 too many requests", "rate limit", "rate_limit", "too many requests"} {
		if strings.Contains(line, needle) {
			return true
		}
	}
	return false
}

func isGitLabAgentUnauthenticatedIncident(line string) bool {
	if !strings.Contains(line, "gitlab") {
		return false
	}
	for _, needle := range []string{
		"agent unauthenticated",
		"agent is unauthenticated",
		"agent not authenticated",
		"gitlab agent unauthenticated",
	} {
		if strings.Contains(line, needle) {
			return true
		}
	}
	return false
}

func isGitLabServiceUnavailableIncident(line string) bool {
	if !strings.Contains(line, "gitlab") {
		return false
	}
	for _, needle := range []string{
		"status 500",
		"status 502",
		"status 503",
		"status 504",
		"bad gateway",
		"service unavailable",
		"gateway timeout",
	} {
		if strings.Contains(line, needle) {
			return true
		}
	}
	return false
}

func isGitLabCIPipelineFailureIncident(line string) bool {
	if !strings.Contains(line, "gitlab") {
		return false
	}
	for _, needle := range []string{
		"gitlab ci pipeline failed",
		"gitlab pipeline failed",
		"gitlab ci pipeline failure",
		"gitlab pipeline failure",
	} {
		if strings.Contains(line, needle) {
			return true
		}
	}
	return false
}
