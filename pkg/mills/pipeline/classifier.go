package pipeline

import (
	"strings"

	"github.com/crb2nu/loom/pkg/telemetry"
)

const gitLabRunnerSystemFailureReason = "runner_system_failure"

var classificationMetrics = telemetry.DefaultClassificationMetrics()

// externalDependencyIncidentRunbooks links observed classifier pattern IDs to
// repository-relative operator procedures. Keep entries sorted by pattern ID
// and retain the registry markers and one-entry-per-line format: the repository
// coverage gate parses this block without compiling Go.
var externalDependencyIncidentRunbooks = map[string]string{
	// classifier-pattern-registry:begin
	"external_dependency.clickhouse.merge_task":        "docs/runbooks/clickhouse-merge-failures.md",
	"external_dependency.gitlab.auth_failure":          "docs/runbooks/gitlab-agent-unauthenticated.md",
	"external_dependency.litellm.missing_api_key":      "docs/runbooks/litellm-auth-missing.md",
	"external_dependency.longhorn.no_available_disk":   "docs/runbooks/longhorn-disk-exhaustion.md",
	"external_dependency.openrouter.credits_exhausted": "docs/runbooks/openrouter-credits-exhausted.md",
	// classifier-pattern-registry:end
}

// ClassificationClass is the policy-facing taxonomy shared by the two
// independent failure classifiers. The zero value is intentionally invalid.
type ClassificationClass string

const (
	ClassificationExternalDependencyIncident ClassificationClass = "external_dependency_incident"
	ClassificationRepositoryRegression       ClassificationClass = "repository_regression"
	ClassificationUnknown                    ClassificationClass = "unknown"
)

// SourceClassification is one classifier's independently produced opinion.
// Source identifies the classifier and Class is normalized before resolution.
type SourceClassification struct {
	Source string              `json:"source"`
	Class  ClassificationClass `json:"class"`
}

func (c ClassificationClass) valid() bool {
	return c == ClassificationExternalDependencyIncident ||
		c == ClassificationRepositoryRegression
}

func normalizeSourceClassification(in SourceClassification) SourceClassification {
	in.Source = strings.TrimSpace(in.Source)
	in.Class = ClassificationClass(strings.ToLower(strings.TrimSpace(string(in.Class))))
	if in.Source == "" || !in.Class.valid() {
		in.Class = ClassificationUnknown
	}
	classificationMetrics.RecordClassification(string(in.Class))
	return in
}

// ClassifyCIFailureSignature classifies structured failure-reason evidence
// emitted by CI providers. Unknown and free-form messages deliberately do not
// match: making a failure retryable is only safe when the provider attributed
// the failed job to its runner rather than to the repository's code.
func ClassifyCIFailureSignature(text string) (FailureClassification, bool) {
	if !hasStructuredFailureReason(text, gitLabRunnerSystemFailureReason) {
		return FailureClassification{}, false
	}
	return failureClassificationForClass(FailureTransient), true
}

// hasStructuredFailureReason accepts the representations used by persisted
// GitLab job records and pipeline diagnostics while requiring the complete
// failure_reason field name and exact reason token. In particular, prose that
// merely mentions runner_system_failure is not classification evidence.
func hasStructuredFailureReason(text, reason string) bool {
	const field = "failure_reason"
	for offset := 0; offset < len(text); {
		index := strings.Index(text[offset:], field)
		if index < 0 {
			return false
		}
		start := offset + index
		offset = start + len(field)
		if start > 0 && isSignatureIdentifierByte(text[start-1]) {
			continue
		}
		if offset < len(text) && isSignatureIdentifierByte(text[offset]) {
			continue
		}

		i := offset
		for i < len(text) && isSignatureSeparatorByte(text[i]) {
			i++
		}
		if i >= len(text) || (text[i] != ':' && text[i] != '=') {
			continue
		}
		i++
		for i < len(text) && isSignatureSeparatorByte(text[i]) {
			i++
		}
		if !strings.HasPrefix(text[i:], reason) {
			continue
		}
		end := i + len(reason)
		if end == len(text) || !isSignatureIdentifierByte(text[end]) {
			return true
		}
	}
	return false
}

func isSignatureIdentifierByte(b byte) bool {
	return b == '_' || b == '-' || b >= '0' && b <= '9' || b >= 'A' && b <= 'Z' || b >= 'a' && b <= 'z'
}

func isSignatureSeparatorByte(b byte) bool {
	return b == ' ' || b == '\t' || b == '\r' || b == '\n' || b == '"'
}
