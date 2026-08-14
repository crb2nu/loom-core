package pipeline

import (
	"errors"
	"testing"
)

func TestClassifyPersistedFailureSignatureOpenRouterCreditsExhausted(t *testing.T) {
	if got, want := openRouterCreditsExhaustedSignatureID, "external_dependency.openrouter.credits_exhausted"; got != want {
		t.Fatalf("stable signature ID = %q, want %q", got, want)
	}

	tests := []string{
		`flexinfer chat: status 402: litellm.APIError: OpenrouterException - {"error":{"message":"This request requires more credits, or fewer max_tokens."}}`,
		`OpenRouter response: {"error":{"code":402,"message":"This request requires more credits"}}`,
		`provider=openrouter HTTP/1.1 402 Payment Required: This request requires more credits`,
		`OPENROUTER response status_code=402: this request REQUIRES MORE CREDITS`,
	}

	for _, evidence := range tests {
		t.Run(evidence, func(t *testing.T) {
			got, matched := ClassifyPersistedFailureSignature(evidence)
			if !matched {
				t.Fatal("ClassifyPersistedFailureSignature did not match")
			}
			if got.ID != openRouterCreditsExhaustedSignatureID ||
				got.Classification != ClassificationExternalDependencyIncident ||
				got.Dependency != openRouterDependency || got.Retryable {
				t.Fatalf("classification = %+v, want non-retryable OpenRouter external dependency incident", got)
			}

			record := ClassifyFailureRecord(errors.New(evidence))
			if record.ExternalDependencyID != openRouterCreditsExhaustedSignatureID ||
				record.ExternalDependency != openRouterDependency || record.Retryable {
				t.Fatalf("runtime classification = %+v, want persisted non-retryable OpenRouter incident", record)
			}
		})
	}
}

func TestClassifyPersistedFailureSignatureOpenRouterCreditsExhaustedNearMisses(t *testing.T) {
	tests := []string{
		`Stripe status 402: This request requires more credits`,
		`OpenRouter status 402: Payment Required`,
		`OpenRouter status 429: This request requires more credits`,
		`OpenRouter billing note: insufficient credits; please top up`,
		`status 402: This request requires more credits`,
		`Documentation mentions OpenRouter in section 402 and requires more credits for examples`,
	}

	for _, evidence := range tests {
		t.Run(evidence, func(t *testing.T) {
			if got, matched := ClassifyPersistedFailureSignature(evidence); matched {
				t.Fatalf("classification = %+v, matched = true; want no match", got)
			}
			if got := ClassifyFailureRecord(errors.New(evidence)); got.ExternalDependencyID == openRouterCreditsExhaustedSignatureID {
				t.Fatalf("runtime classification = %+v, want no OpenRouter credit-exhaustion match", got)
			}
		})
	}
}
