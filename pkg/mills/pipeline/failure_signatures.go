package pipeline

import (
	"regexp"
	"strings"
)

const (
	openRouterCreditsExhaustedSignatureID = "external_dependency.openrouter.credits_exhausted"
	openRouterDependency                  = "openrouter"
	openRouterCreditsExhaustedPhrase      = "requires more credits"
)

// PersistedFailureSignature is a stable, policy-facing classification for a
// known provider failure. ID values may be persisted in incident records and
// therefore must not be renamed when matcher wording evolves.
type PersistedFailureSignature struct {
	ID             string
	Classification ClassificationClass
	Dependency     string
	Retryable      bool
}

var openRouterHTTP402Pattern = regexp.MustCompile(`(?:status(?:[ _-]?code)?|code|http(?:/\d(?:\.\d)?)?)\s*["']?\s*[:=]?\s*402\b|\b402\s+payment required\b`)

// ClassifyPersistedFailureSignature matches promoted failure signatures whose
// identity and retry policy must remain stable across runs. OpenRouter credit
// exhaustion requires provider attribution, HTTP 402 evidence, and the
// provider's credit-exhaustion wording so unrelated payment-required responses
// and generic billing prose cannot suppress retries.
func ClassifyPersistedFailureSignature(evidence string) (PersistedFailureSignature, bool) {
	normalized := strings.ToLower(evidence)
	if !strings.Contains(normalized, openRouterDependency) ||
		!openRouterHTTP402Pattern.MatchString(normalized) ||
		!strings.Contains(normalized, openRouterCreditsExhaustedPhrase) {
		return PersistedFailureSignature{}, false
	}

	return PersistedFailureSignature{
		ID:             openRouterCreditsExhaustedSignatureID,
		Classification: ClassificationExternalDependencyIncident,
		Dependency:     openRouterDependency,
		Retryable:      false,
	}, true
}
