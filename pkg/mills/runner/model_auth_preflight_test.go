package runner

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/crb2nu/loom/pkg/mills/clients"
)

func TestRunModelCredentialPreflightFailsBeforeAdmission(t *testing.T) {
	env := newRunnerEnv(t, sampleProposals(1))
	policy := env.policy.Current()
	policy.Council.Ensemble.Editor.Backend = "openai-responses"
	env.runner.ModelCredentials = clients.ModelCredentialPreflight{
		LookupEnv: func(string) (string, bool) { return "", false },
	}

	res, err := env.runner.Run(context.Background(), RunInput{})
	if !errors.Is(err, ErrModelAuth) {
		t.Fatalf("Run() error = %v, want ErrModelAuth", err)
	}
	if !clients.IsModelAuthError(err) {
		t.Fatalf("Run() error = %v, want wrapped ModelAuthError", err)
	}
	if res != nil {
		t.Fatalf("Run() result = %#v, want nil before run-id mint", res)
	}
	if strings.Contains(err.Error(), "secret") {
		t.Fatalf("Run() error exposed credential: %v", err)
	}
	runs, listErr := env.store.Council.List(context.Background(), 10)
	if listErr != nil {
		t.Fatalf("list council runs: %v", listErr)
	}
	if len(runs) != 0 {
		t.Fatalf("durable runs = %d, want 0", len(runs))
	}
}

func TestConfiguredModelProvidersIncludesEveryCouncilRole(t *testing.T) {
	env := newRunnerEnv(t, nil)
	policy := env.policy.Current()
	policy.Council.Ensemble.Editor.Backend = "anthropic"
	policy.Council.Ensemble.Reviewers[0].Backend = "litellm"
	policy.Council.Ensemble.Judge.Backend = "openai"

	got := strings.Join(configuredModelProviders(policy), ",")
	if !strings.Contains(got, "anthropic") || !strings.Contains(got, "litellm") || !strings.Contains(got, "openai") {
		t.Fatalf("configured providers = %q, want editor, reviewer, and judge", got)
	}
}
