package council

import (
	"regexp"
	"strings"

	"github.com/crb2nu/loom/pkg/mills/store"
)

type recurringInfrastructureSignature struct {
	dependency string
	pattern    *regexp.Regexp
}

var recurringInfrastructureSignatures = []recurringInfrastructureSignature{
	{
		dependency: CIIncidentDependencyClickHouse,
		pattern:    regexp.MustCompile(`(?i)\b(?:clickhouse|mergetree|merge(?:d|ing)? parts?)\b[\s\S]*\bcode\s*:?\s*432\b|\bcode\s*:?\s*432\b[\s\S]*\b(?:clickhouse|mergetree|merge(?:d|ing)? parts?)\b`),
	},
	{
		dependency: CIIncidentDependencyLonghorn,
		pattern:    regexp.MustCompile(`(?i)\blonghorn\b[\s\S]*\b(?:failed|failure|unable)\s+to\s+schedule\s+(?:a\s+)?replica\b|\b(?:failed|failure|unable)\s+to\s+schedule\s+(?:a\s+)?replica\b[\s\S]*\blonghorn\b`),
	},
	{
		dependency: "litellm",
		pattern:    regexp.MustCompile(`(?i)\blitellm\b[\s\S]*(?:^|[^[:alnum:]])api[_ -]?key\b[\s\S]*\b(?:missing|not (?:found|provided|set)|required)\b|\blitellm\b[\s\S]*\b(?:missing|required)\b[\s\S]*(?:^|[^[:alnum:]])api[_ -]?key\b`),
	},
	{
		dependency: "postgres",
		pattern:    regexp.MustCompile(`(?i)\b(?:postgres(?:ql)?|psql)\b[\s\S]*\brole\s+[^\r\n]+?\s+does not exist\b|\brole\s+[^\r\n]+?\s+does not exist\b[\s\S]*\b(?:postgres(?:ql)?|psql)\b`),
	},
}

// ClassifyRecurringInfrastructureWorkspaceSignal recognizes only the recurring,
// operator-owned infrastructure signatures in the allowlist above. Requiring
// both dependency attribution and its precise failure phrase keeps ordinary
// repository errors from being suppressed as outside-system incidents.
func ClassifyRecurringInfrastructureWorkspaceSignal(signal WorkspaceSignal) (WorkspaceSignal, bool) {
	if signal.IncidentClass != "" {
		return signal, false
	}

	evidence := strings.TrimSpace(signal.Service + "\n" + signal.Sample)
	for _, signature := range recurringInfrastructureSignatures {
		if signature.pattern.MatchString(evidence) {
			signal.IncidentClass = CIIncidentClass(store.IncidentClassExternalDependency)
			signal.ExternalDependency = signature.dependency
			return signal, true
		}
	}
	return signal, false
}

// ClassifyExternalDependencyIncident returns an explicit external dependency
// incident classification for recurring infra evidence. It is a narrow facade
// over the full CI taxonomy for callers that only want to distinguish
// repo-owned failures from external systems.
func ClassifyExternalDependencyIncident(branch CIBranchEvidence, failures []CIFailureEvidence) (CIIncidentClassification, bool) {
	classification := ClassifyCIIncident(branch, failures)
	if classification.Class != CIIncidentExternalDependency {
		return CIIncidentClassification{}, false
	}
	return classification, true
}

// CIIncidentPolicyDedupKey returns the stable policy identity used to collapse
// repeated incidents that have the same remediation owner. Evidence and reason
// are intentionally excluded because they vary between jobs for the same
// dependency outage or infrastructure saturation event.
func CIIncidentPolicyDedupKey(classification CIIncidentClassification) string {
	class := strings.TrimSpace(string(classification.Class))
	if class == "" {
		class = string(CIIncidentUnclassified)
	}
	disposition := strings.TrimSpace(string(classification.Disposition))
	if disposition == "" {
		disposition = string(CIIncidentDispositionEscalateHuman)
	}
	dependency := strings.TrimSpace(classification.Dependency)
	if dependency == "" && classification.Class == CIIncidentRunnerInfrastructure {
		dependency = CIIncidentDependencyRunnerSaturation
	}
	if dependency == "" {
		return class + "|" + disposition
	}
	return class + "|" + dependency + "|" + disposition
}
