package pipeline

import (
	"regexp"
	"strings"

	"github.com/crb2nu/loom/pkg/telemetry"
)

const ExternalFailureDispositionWaitForDependency = "wait_for_dependency_recovery"

// ExternalFailureSignature is the stable policy identity of a known failure
// outside the repository. Signature IDs are persisted in incident records and
// must remain stable when matcher wording evolves.
type ExternalFailureSignature struct {
	ID          string
	Dependency  string
	Class       ClassificationClass
	Disposition string
	match       func(string) bool
}

var http402Pattern = regexp.MustCompile(`(?:\bhttp(?:/\d(?:\.\d)?)?\s*402\b|\b(?:status(?:[ _-]?code)?|code)\s*["']?\s*[:=]?\s*402\b|\b402\s+payment required\b)`)
var http401Pattern = regexp.MustCompile(`(?:\bhttp(?:/\d(?:\.\d)?)?\s*401\b|\b(?:status(?:[ _-]?code)?|code)\s*["']?\s*[:=]?\s*401\b|\b401\s+unauthorized\b)`)
var clickHouseCode432Pattern = regexp.MustCompile(`\bcode\s*:\s*432\b`)

var openRouterCreditExhaustionPhrases = []string{
	"requires more credits",
	"insufficient credits",
	"credits exhausted",
}

// externalFailureSignatures is ordered and first-match-wins. Keep the
// OpenRouter credit signature first: provider exhaustion is the most specific
// promoted shape and must win evidence containing several incident messages.
var externalFailureSignatures = []ExternalFailureSignature{
	{
		ID: "external_dependency.openrouter.credits_exhausted", Dependency: "openrouter",
		Class: ClassificationExternalDependencyIncident, Disposition: ExternalFailureDispositionWaitForDependency,
		match: func(text string) bool {
			return strings.Contains(text, "openrouter") && http402Pattern.MatchString(text) &&
				containsAnyPhrase(text, openRouterCreditExhaustionPhrases)
		},
	},
	{
		ID: "external_dependency.litellm.missing_api_key", Dependency: "litellm",
		Class: ClassificationExternalDependencyIncident, Disposition: ExternalFailureDispositionWaitForDependency,
		match: func(text string) bool {
			return strings.Contains(text, "litellm") && http401Pattern.MatchString(text) &&
				strings.Contains(text, "no api key passed in")
		},
	},
	{
		ID: "external_dependency.clickhouse.merge_task", Dependency: "clickhouse",
		Class: ClassificationExternalDependencyIncident, Disposition: ExternalFailureDispositionWaitForDependency,
		match: func(text string) bool {
			return strings.Contains(text, "clickhouse") && clickHouseCode432Pattern.MatchString(text) &&
				(strings.Contains(text, "merge task") || strings.Contains(text, "cannot merge parts") ||
					strings.Contains(text, "mergetreebackgroundexecutor"))
		},
	},
	{
		ID: "external_dependency.gitlab.auth_failure", Dependency: "gitlab",
		Class: ClassificationExternalDependencyIncident, Disposition: ExternalFailureDispositionWaitForDependency,
		match: func(text string) bool {
			return (strings.Contains(text, "gitlab agent") || strings.Contains(text, "gitlab-agent") || strings.Contains(text, "agentk")) &&
				strings.Contains(text, "unauthenticated")
		},
	},
	{
		ID: "external_dependency.gitlab.ci_infrastructure", Dependency: "gitlab",
		Class: ClassificationExternalDependencyIncident, Disposition: ExternalFailureDispositionWaitForDependency,
		match: hasStructuredRunnerLevelFailureReason,
	},
}

func containsAnyPhrase(text string, phrases []string) bool {
	for _, phrase := range phrases {
		if strings.Contains(text, phrase) {
			return true
		}
	}
	return false
}

// ClassifyExternalFailureSignature recognizes known external failures before
// repository-code classification. Unknown evidence deliberately falls through.
func ClassifyExternalFailureSignature(evidence string) (ExternalFailureSignature, bool) {
	normalized := strings.ToLower(evidence)
	for _, signature := range externalFailureSignatures {
		if signature.match(normalized) {
			signature.match = nil
			return signature, true
		}
	}
	return ExternalFailureSignature{}, false
}

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

// ClassifyCIFailureSignature classifies known external failure evidence before
// ordinary code-failure rules. Unknown and free-form messages deliberately do
// not match, so repository failures continue through the existing classifier.
func ClassifyCIFailureSignature(text string) (FailureClassification, bool) {
	signature, matched := ClassifyExternalFailureSignature(text)
	if !matched {
		return FailureClassification{}, false
	}
	classification := failureClassificationForClass(FailureConfiguration)
	classification.ExternalDependencyID = signature.ID
	classification.ExternalDependency = signature.Dependency
	return classification, true
}

// hasStructuredFailureReason accepts the representations used by persisted
// GitLab job records and pipeline diagnostics while requiring the complete
// failure_reason field name and exact reason token. In particular, prose that
// merely mentions a runner-level reason is not classification evidence.
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

func hasStructuredRunnerLevelFailureReason(text string) bool {
	for reason := range gitLabRunnerLevelFailureReasons {
		if hasStructuredFailureReason(text, reason) {
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
