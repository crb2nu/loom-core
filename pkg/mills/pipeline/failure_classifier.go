package pipeline

import (
	"strings"

	"github.com/crb2nu/loom/pkg/mcperror"
)

// FailureClass is the closed taxonomy used by Mills escalation policy.
// Unknown failures intentionally classify as FailureCode so the pipeline
// fails closed and asks for human review instead of retrying forever.
type FailureClass string

const (
	FailureTransient      FailureClass = "transient"
	FailureTransientQuota FailureClass = "transient_quota"
	FailureInfrastructure FailureClass = "infrastructure"
	FailureCode           FailureClass = "code"
	FailureConfiguration  FailureClass = "configuration"
)

var allFailureClasses = []FailureClass{
	FailureTransient,
	FailureTransientQuota,
	FailureInfrastructure,
	FailureCode,
	FailureConfiguration,
}

// AllFailureClasses returns every valid value in the closed failure taxonomy.
func AllFailureClasses() []FailureClass {
	return append([]FailureClass(nil), allFailureClasses...)
}

// Valid reports whether c is part of the closed failure taxonomy.
func (c FailureClass) Valid() bool {
	for _, allowed := range allFailureClasses {
		if c == allowed {
			return true
		}
	}
	return false
}

// Retryable reports whether a failure class should be retried before human
// escalation. Configuration failures are terminal; code and infrastructure
// failures consume the normal retry budget; transient classes consume the
// transient budget.
func (c FailureClass) Retryable() bool {
	return c == FailureTransient ||
		c == FailureTransientQuota ||
		c == FailureInfrastructure ||
		c == FailureCode
}

// FreeRetry reports whether this class uses the transient retry budget instead
// of the normal max-attempts budget.
func (c FailureClass) FreeRetry() bool {
	return c == FailureTransient || c == FailureTransientQuota
}

// Terminal reports whether retrying the exact same work is known not to help.
func (c FailureClass) Terminal() bool {
	return c == FailureConfiguration
}

// ClassifyFailure maps an error to the closed failure taxonomy. It delegates to
// the runner's established classifier so the policy and runner agree while the
// new taxonomy keeps council-facing names stable.
func ClassifyFailure(err error) FailureClass {
	if err == nil {
		return ""
	}
	if classification, ok := ClassifyCIFailureSignature(err.Error()); ok {
		return classification.Class
	}
	if _, _, ok := classifyObservedExternalIncident(err.Error()); ok {
		return FailureConfiguration
	}
	if incident, ok := mcperror.ClassifyExternalCIIncident(err.Error()); ok {
		// Only a non-retryable external incident (auth failure, blob storage)
		// is a terminal configuration problem. Retryable incidents (GitLab
		// rate limit / service unavailable) keep their transient semantics —
		// the incident code table itself marks them Retryable — so they fall
		// through to the historical classifier (429 → transient_quota, 5xx →
		// transient) and stay on the free-retry/backoff path.
		if !mcperror.ExternalIncidentRetryable(incident) {
			return FailureConfiguration
		}
	}
	return FailureClassFromErrorClass(Classify(err))
}

// FailureClassFromErrorClass translates the runner's historical ErrorClass
// values into the closed taxonomy.
func FailureClassFromErrorClass(c ErrorClass) FailureClass {
	switch c {
	case ClassTransient:
		return FailureTransient
	case ClassTransientQuota:
		return FailureTransientQuota
	case ClassInfra:
		return FailureInfrastructure
	case ClassConfig:
		return FailureConfiguration
	case ClassCode:
		return FailureCode
	default:
		return FailureCode
	}
}

// FailureClassFromString parses persisted/policy-provided class names. Unknown
// or empty values fail closed to FailureCode.
func FailureClassFromString(s string) FailureClass {
	c := FailureClass(s)
	if c.Valid() {
		return c
	}
	return FailureCode
}

// IsRetryableFailure reports whether err classifies to a retryable class.
func IsRetryableFailure(err error) bool {
	if err == nil {
		return false
	}
	return ClassifyFailure(err).Retryable()
}

// FailureClassifierName identifies the deterministic Mills classifier as the
// provenance of a FailureClassification, so downstream consumers can tell this
// taxonomy apart from other classifiers that may stamp similar records.
const FailureClassifierName = "mills-failure-classifier"

// FailureClassification summarizes the class and retry semantics for an error.
// It is a wire contract (see internal/contracts/escalation.go); field renames
// are breaking changes for downstream planners and operators.
type FailureClassification struct {
	Classifier           string       `json:"classifier"`
	Class                FailureClass `json:"class"`
	Retryable            bool         `json:"retryable"`
	FreeRetry            bool         `json:"free_retry"`
	Terminal             bool         `json:"terminal"`
	ExternalDependencyID string       `json:"external_dependency_id,omitempty"`
	ExternalDependency   string       `json:"external_dependency,omitempty"`
}

// ClassifyFailureRecord returns the full policy-facing classification for err.
func ClassifyFailureRecord(err error) FailureClassification {
	classification := failureClassificationForClass(ClassifyFailure(err))
	if err == nil {
		return classification
	}
	if id, dependency, ok := classifyObservedExternalIncident(err.Error()); ok {
		classification.ExternalDependencyID = id
		classification.ExternalDependency = dependency
		return classification
	}
	if incident, ok := mcperror.ClassifyExternalCIIncident(err.Error()); ok {
		classification.ExternalDependencyID = incident.ID
		classification.ExternalDependency = incident.Dependency
	}
	return classification
}

// classifyObservedExternalIncident recognizes external service failures seen in
// live Mills runs. The signatures deliberately require both the dependency and
// its distinctive failure phrase so generic merge, disk, and API-key errors
// remain code/configuration failures instead of being over-classified.
func classifyObservedExternalIncident(text string) (id, dependency string, ok bool) {
	if signature, matched := ClassifyPersistedFailureSignature(text); matched {
		return signature.ID, signature.Dependency, true
	}

	lower := strings.ToLower(text)
	switch {
	case strings.Contains(lower, "clickhouse") &&
		(strings.Contains(lower, "merge task failed") ||
			strings.Contains(lower, "merge task failure") ||
			strings.Contains(lower, "failed to execute merge task")):
		return "external_dependency.clickhouse.merge_task", "clickhouse", true
	case strings.Contains(lower, "longhorn") &&
		(strings.Contains(lower, "no available disk") ||
			strings.Contains(lower, "no available disks")):
		return "external_dependency.longhorn.no_available_disk", "longhorn", true
	case strings.Contains(lower, "litellm") &&
		(strings.Contains(lower, "missing api key") ||
			strings.Contains(lower, "api key is missing") ||
			strings.Contains(lower, "no api key")):
		return "external_dependency.litellm.missing_api_key", "litellm", true
	default:
		return "", "", false
	}
}

func failureClassificationForClass(class FailureClass) FailureClassification {
	return FailureClassification{
		Classifier: FailureClassifierName,
		Class:      class,
		Retryable:  class.Retryable(),
		FreeRetry:  class.FreeRetry(),
		Terminal:   class.Terminal(),
	}
}
