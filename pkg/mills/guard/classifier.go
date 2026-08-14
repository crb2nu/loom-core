package guard

import (
	"regexp"
	"strings"
)

// FailureClassification is the deterministic classification returned for a
// recognized failure signature. An empty value means the classifier failed
// closed: unknown evidence is not promoted to an external incident.
type FailureClassification string

// ExternalDependencyIncident is the canonical classification for a matched
// failure caused by infrastructure outside the repository.
const ExternalDependencyIncident FailureClassification = "external_dependency_incident"

type failureSignature struct {
	needles []string
}

// externalDependencySignatures is intentionally ordered and append-only. A
// rule matches only when every normalized needle is present.
var externalDependencySignatures = []failureSignature{
	{needles: []string{"econnrefused"}},
	{needles: []string{"mergetreebackgroundexecutor"}},
	{needles: []string{"longhorn", "no available disk"}},
	{needles: []string{"circuit breaker open"}},
}

var signatureSeparators = regexp.MustCompile(`[^a-z0-9]+`)

// ClassifyFailure recognizes known external-dependency failure signatures.
// Unknown input returns an empty classification and matched=false; callers
// must leave it on the normal path rather than guessing from weak evidence.
func ClassifyFailure(evidence string) (FailureClassification, bool) {
	normalized := normalizeFailureEvidence(evidence)
	for _, signature := range externalDependencySignatures {
		matched := true
		for _, needle := range signature.needles {
			if !strings.Contains(normalized, needle) {
				matched = false
				break
			}
		}
		if matched {
			return ExternalDependencyIncident, true
		}
	}
	return "", false
}

func normalizeFailureEvidence(evidence string) string {
	return strings.Join(strings.Fields(signatureSeparators.ReplaceAllString(strings.ToLower(evidence), " ")), " ")
}
