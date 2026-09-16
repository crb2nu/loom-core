package clients

import (
	"errors"
	"strings"
	"testing"
)

func TestModelCredentialPreflight(t *testing.T) {
	tests := []struct {
		name      string
		providers []string
		env       map[string]string
		wantErr   bool
	}{
		{name: "local provider needs no credential", providers: []string{"flexinfer"}},
		{name: "OpenAI scoped key present", providers: []string{"openai-responses"}, env: map[string]string{"LOOM_RESPONSES_API_KEY": "secret"}},
		{name: "Anthropic fallback key present", providers: []string{"claude"}, env: map[string]string{"ANTHROPIC_API_KEY": "secret"}},
		{name: "LiteLLM key present", providers: []string{"litellm"}, env: map[string]string{"LOOM_MILLS_LITELLM_KEY": "secret"}},
		{name: "missing credential", providers: []string{"openai"}, wantErr: true},
		{name: "blank credential", providers: []string{"anthropic"}, env: map[string]string{"LOOM_ANTHROPIC_API_KEY": " \t"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			preflight := ModelCredentialPreflight{LookupEnv: func(name string) (string, bool) {
				value, ok := tt.env[name]
				return value, ok
			}}
			err := preflight.Check(tt.providers...)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Check() error = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil {
				if !IsModelAuthError(err) {
					t.Fatalf("error type = %T, want ModelAuthError", err)
				}
				var classified interface{ FailureClass() string }
				if !errors.As(err, &classified) || classified.FailureClass() != ModelAuthFailureClass {
					t.Fatalf("failure class = %v, want %q", classified, ModelAuthFailureClass)
				}
				for _, value := range tt.env {
					if strings.TrimSpace(value) != "" && strings.Contains(err.Error(), value) {
						t.Fatal("error exposed credential value")
					}
				}
			}
		})
	}
}

func TestModelCredentialPreflightDeduplicatesProviders(t *testing.T) {
	err := (ModelCredentialPreflight{LookupEnv: func(string) (string, bool) { return "", false }}).Check("claude", "anthropic")
	var authErr *ModelAuthError
	if !errors.As(err, &authErr) || len(authErr.Providers) != 1 || authErr.Providers[0] != "anthropic" {
		t.Fatalf("error = %#v, want one anthropic provider", err)
	}
}
