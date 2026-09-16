package main

import "testing"

// The daemon-hosted server runs with the loomd environment, which names the
// operator admin token LOOM_MILLS_OPERATOR_TOKEN. Every mutation failed
// NotConfigured until that alias was part of the resolution chain.
func TestResolveOperatorToken_AcceptsLoomdAlias(t *testing.T) {
	for _, name := range operatorTokenEnvs {
		t.Setenv(name, "")
	}
	if got := resolveOperatorToken(); got != "" {
		t.Fatalf("resolveOperatorToken() with nothing set = %q, want empty", got)
	}

	t.Setenv("LOOM_MILLS_OPERATOR_TOKEN", "operator-token")
	if got := resolveOperatorToken(); got != "operator-token" {
		t.Fatalf("resolveOperatorToken() = %q, want the LOOM_MILLS_OPERATOR_TOKEN alias", got)
	}

	t.Setenv("LOOM_MILLS_TOKEN", "primary-token")
	if got := resolveOperatorToken(); got != "primary-token" {
		t.Fatalf("resolveOperatorToken() = %q, want LOOM_MILLS_TOKEN to win over the alias", got)
	}
}
