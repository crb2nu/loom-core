package clients

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
)

// ModelAuthFailureClass is the stable failure class emitted when a configured
// model provider has no credential. It is intentionally narrower than the
// pipeline's general configuration class so operators can route credential
// incidents without parsing error prose.
const ModelAuthFailureClass = "model_auth"

// ModelAuthError reports missing model-provider authentication without ever
// retaining or rendering a credential value.
type ModelAuthError struct {
	Providers []string
}

func (e *ModelAuthError) Error() string {
	providers := append([]string(nil), e.Providers...)
	sort.Strings(providers)
	return fmt.Sprintf("%s: credentials missing for configured model provider(s): %s", ModelAuthFailureClass, strings.Join(providers, ", "))
}

// FailureClass lets callers classify this error without matching Error text.
func (e *ModelAuthError) FailureClass() string { return ModelAuthFailureClass }

// IsModelAuthError reports whether err is, or wraps, a credential-preflight
// failure.
func IsModelAuthError(err error) bool {
	var target *ModelAuthError
	return errors.As(err, &target)
}

// ModelCredentialPreflight performs a presence-only check for credentials
// required by configured model-provider backends. LookupEnv is injectable for
// tests; nil uses the process environment.
type ModelCredentialPreflight struct {
	LookupEnv func(string) (string, bool)
}

// Check validates all configured providers. Local backends do not require a
// credential and unknown backends are ignored: their client constructors own
// validation because this preflight cannot safely guess their auth contract.
func (p ModelCredentialPreflight) Check(providers ...string) error {
	lookup := p.LookupEnv
	if lookup == nil {
		lookup = os.LookupEnv
	}

	missing := make(map[string]struct{})
	for _, provider := range providers {
		provider = normalizeModelProvider(provider)
		envVars := modelProviderCredentialEnvVars(provider)
		if len(envVars) == 0 || anyPresentCredential(lookup, envVars) {
			continue
		}
		missing[provider] = struct{}{}
	}
	if len(missing) == 0 {
		return nil
	}
	providers = providers[:0]
	for provider := range missing {
		providers = append(providers, provider)
	}
	return &ModelAuthError{Providers: providers}
}

func normalizeModelProvider(provider string) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "openai-responses":
		return "openai"
	case "claude":
		return "anthropic"
	default:
		return strings.ToLower(strings.TrimSpace(provider))
	}
}

func modelProviderCredentialEnvVars(provider string) []string {
	switch provider {
	case "openai":
		return []string{"LOOM_RESPONSES_API_KEY", "OPENAI_API_KEY"}
	case "anthropic":
		return []string{AnthropicAPIKeyEnvVar, "ANTHROPIC_API_KEY"}
	case "litellm":
		return []string{"LOOM_MILLS_LITELLM_KEY"}
	default:
		return nil
	}
}

func anyPresentCredential(lookup func(string) (string, bool), names []string) bool {
	for _, name := range names {
		value, ok := lookup(name)
		if ok && strings.TrimSpace(value) != "" {
			return true
		}
	}
	return false
}
