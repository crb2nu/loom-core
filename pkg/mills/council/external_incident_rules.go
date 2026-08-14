package council

import (
	"strings"

	"github.com/crb2nu/loom/pkg/mcperror"
)

const (
	// ExternalDependencyIncidentLabel marks council follow-up generated from an
	// incident whose root cause is outside this repository.
	ExternalDependencyIncidentLabel = "external-dependency-incident"

	// ExternalIncidentNoInRepoFollowUpReason is the stable omit_reason used when
	// an external incident has no conservative repository-owned action.
	ExternalIncidentNoInRepoFollowUpReason = "external dependency incident; no actionable in-repo follow-up"
)

// ExternalIncidentPlanningInput is the council-facing evidence bundle used to
// classify third-party CI failures before planning mints follow-up work.
type ExternalIncidentPlanningInput struct {
	Source     string
	Title      string
	Body       string
	Service    string
	JobName    string
	Stage      string
	ErrorLine  string
	LogExcerpt string
}

// ExternalIncidentPlanningDecision is the deterministic planning rule result
// for external dependency incidents.
type ExternalIncidentPlanningDecision struct {
	Class                  CIIncidentClass
	Disposition            CIIncidentDisposition
	Dependency             string
	Evidence               string
	Reason                 string
	Label                  string
	OmitReason             string
	InRepoFollowUpRequired bool
	RetryAllowed           bool
}

// ExternalIncidentPlanningRulesPromptSection returns the reusable council
// planning contract for third-party CI and provider incidents.
func ExternalIncidentPlanningRulesPromptSection() string {
	return `
## External dependency incidents

If the brief, CI evidence, or reviewer notes indicate that the observed failure
was caused by an external dependency (for example GitLab CI, GitLab APIs,
OpenAI, FlexInfer, a model provider, Kubernetes, container registry, network,
storage, or a third-party API), classify it explicitly as
"external_dependency_incident" and label any related backlog proposal with
"external-dependency-incident".

Backlog proposals MUST be actionable in this repository. Do not propose
"remediate GitLab", "rerun GitLab CI until green", "increase provider quota",
"restart the external service", "change credentials", or similar outside-system
actions unless the proposal's files are repo files that implement a local
guardrail, classifier, retry, telemetry, documentation, config, or operator
runbook update. File-backed proposals that do not fit those local follow-up
classes must be omitted, even if they name repository files. If there is no
in-repo follow-up, emit {"proposals": [],
"omit_reason": "external dependency incident; no actionable in-repo follow-up"}.
`
}

// ClassifyExternalIncidentPlanning applies the council's external-incident
// planning rule to free-form CI/provider evidence. It intentionally returns only
// external classifications; callers should use ClassifyCIIncident for the full
// incident taxonomy.
func ClassifyExternalIncidentPlanning(input ExternalIncidentPlanningInput) ExternalIncidentPlanningDecision {
	text := externalIncidentPlanningText(input)
	if strings.TrimSpace(text) == "" {
		return ExternalIncidentPlanningDecision{}
	}

	if inc, ok := mcperror.ClassifyExternalCIIncident(text); ok {
		return externalIncidentDecision(inc.Dependency, inc.Evidence, inc.Summary)
	}
	if dep, evidence, ok := classifyExternalDependency(text); ok {
		return externalIncidentDecision(dep, evidence, "failure points to a shared dependency outside the repository")
	}
	if isExternalDependencyIncidentText(text) {
		return externalIncidentDecision("", firstExternalIncidentEvidenceLine(text), "evidence describes an external dependency incident")
	}
	return ExternalIncidentPlanningDecision{}
}

func externalIncidentDecision(dependency, evidence, reason string) ExternalIncidentPlanningDecision {
	return ExternalIncidentPlanningDecision{
		Class:                  CIIncidentExternalDependency,
		Disposition:            CIIncidentDispositionWaitDependency,
		Dependency:             dependency,
		Evidence:               strings.TrimSpace(evidence),
		Reason:                 strings.TrimSpace(reason),
		Label:                  ExternalDependencyIncidentLabel,
		OmitReason:             ExternalIncidentNoInRepoFollowUpReason,
		InRepoFollowUpRequired: false,
		RetryAllowed:           false,
	}
}

func externalIncidentPlanningText(input ExternalIncidentPlanningInput) string {
	var b strings.Builder
	for _, part := range []string{
		input.Source,
		input.Title,
		input.Body,
		input.Service,
		input.JobName,
		input.Stage,
		input.ErrorLine,
		input.LogExcerpt,
	} {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(part)
	}
	return b.String()
}

func firstExternalIncidentEvidenceLine(text string) string {
	for _, line := range evidenceLines(text) {
		if isExternalDependencyIncidentText(line) {
			return line
		}
	}
	return ""
}
