package hud

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/crb2nu/loom/internal/devbox/backend"
	"github.com/crb2nu/loom/internal/hud/bridge"
	"github.com/crb2nu/loom/internal/spawn"
)

func TestClaudeAuthFixtures(t *testing.T) {
	for _, tc := range []struct {
		name string
		want AuthOutcome
	}{
		{"oauth-rejected", authOAuthRejected}, {"oauth-quota", authOAuthQuota}, {"api-billing", authAPIBilling}, {"api-401", authAPIAuth},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, err := os.ReadFile("testdata/claude-auth-" + tc.name + ".jsonl")
			if err != nil {
				t.Fatal(err)
			}
			// Prepend a realistic oversized init record: classification must see the
			// result after it on both the streaming and buffered paths.
			output := `{"type":"system","subtype":"init","padding":"` + strings.Repeat("x", 9000) + `"}` + "\n" + string(data)
			parser := NewClaudeJSONLParser(bridge.NewSpawnTelemetryAccumulator(), "a", "s", nil, nil)
			for _, line := range strings.Split(output, "\n") {
				parser.HandleLine([]byte(line))
			}
			for _, result := range []*claudeParseResult{parser.authSnapshot(), parseClaudeAuthLines(output)} {
				for _, exit := range []int{0, 1} {
					if got := classifyClaudeAuth(result, exit, []string{"conflicting credit balance"}); got != tc.want {
						t.Fatalf("exit=%d got=%s want=%s result=%+v", exit, got, tc.want, result)
					}
				}
			}
		})
	}
}

func TestClaudeAuthEvidencePrecedence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		parsed *claudeParseResult
		exit   int
		want   AuthOutcome
	}{
		{"success recovers retry", &claudeParseResult{sawResult: true, lastRetryNote: "401", result: "Invalid API key"}, 0, authUnknown},
		{"unknown result wins", &claudeParseResult{sawResult: true, isError: true, result: "tool crashed", lastRetryNote: "401"}, 1, authUnknown},
		{"retry wins stderr", &claudeParseResult{lastRetryNote: "401"}, 0, authAPIAuth},
		{"stderr failure", nil, 1, authAPIBilling},
		{"stderr clean exit", nil, 0, authUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyClaudeAuth(tc.parsed, tc.exit, []string{"credit balance"}); got != tc.want {
				t.Fatalf("got %s", got)
			}
		})
	}
}

type authReplayBackend struct {
	recordingBackend
	output string
}

func (b *authReplayBackend) Exec(_ context.Context, opts backend.ExecOpts) (*backend.ExecResult, error) {
	b.execCalls = append(b.execCalls, opts)
	return &backend.ExecResult{StdoutTail: b.output}, nil
}

func TestClaudeAuthRelaunch(t *testing.T) {
	for _, tc := range []struct {
		name, policy, message string
		from, to              spawn.AuthMode
		want                  bool
	}{
		{"oauth to api", "oauth-then-api", "Invalid API key · Please run /login", spawn.AuthModeClusterOAuth, spawn.AuthModeClusterAPIKey, true},
		{"api to oauth", "api-then-oauth", "Your credit balance is too low", spawn.AuthModeClusterAPIKey, spawn.AuthModeClusterOAuth, true},
		{"disabled", "none", "Invalid API key · Please run /login", spawn.AuthModeClusterOAuth, spawn.AuthModeClusterOAuth, false},
		{"api auth never retries", "api-then-oauth", "401 authentication_error", spawn.AuthModeClusterAPIKey, spawn.AuthModeClusterAPIKey, false},
		{"unknown never retries", "oauth-then-api", "broken tool", spawn.AuthModeClusterOAuth, spawn.AuthModeClusterOAuth, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SPAWN_CLAUDE_AUTH_FALLBACK", tc.policy)
			t.Setenv(ClaudeOAuthTokenKeysEnv, "claude-oauth-token")
			t.Setenv("SPAWN_CLAUDE_API_FALLBACK_MAX_PER_DAY", "6")
			o := newStopRaceOrchestrator(t, &recordingBackend{})
			id, _ := seedSupervisedRunningSpawn(t, o.ctrl, true)
			st, _ := o.ctrl.Get(id)
			st.AuthMode = tc.from
			if tc.from == spawn.AuthModeClusterOAuth {
				st.AuthAccount = "claude-oauth-token"
			}
			o.ctrl.UpdateState(t.Context(), st)
			calls := make(chan string, 2)
			o.redriveSpawn = func(id string, _ SpawnRequest) { calls <- id }
			owner, ok := o.acquireSpawnDriver(id)
			if !ok {
				t.Fatal("no owner")
			}
			got := o.handleClaudeAuthCompletion(t.Context(), st, &claudeParseResult{sawResult: true, isError: true, result: tc.message}, 0, nil)
			if got != tc.want {
				t.Fatalf("handled=%v", got)
			}
			select {
			case <-calls:
				t.Fatal("relaunched before owner release")
			default:
			}
			o.releaseSpawnDriver(id, owner)
			if !tc.want {
				return
			}
			select {
			case <-calls:
			case <-time.After(time.Second):
				t.Fatal("no relaunch")
			}
			after, _ := o.ctrl.Get(id)
			if after.AuthMode != tc.to || after.AuthFallbackAt == nil || !after.AuthRetryPending {
				t.Fatalf("state=%+v", after)
			}
			vars := agentSecretEnvVarsForMode("claude-code", after.AuthAccount, after.AuthMode)
			wantEnv := "ANTHROPIC_API_KEY"
			if tc.to == spawn.AuthModeClusterOAuth {
				wantEnv = "CLAUDE_CODE_OAUTH_TOKEN"
			}
			if len(vars) != 1 || vars[0].Name != wantEnv {
				t.Fatalf("credentials=%+v", vars)
			}
			after.AuthRetryPending = false
			o.ctrl.UpdateState(t.Context(), after)
			if o.handleClaudeAuthCompletion(t.Context(), after, &claudeParseResult{sawResult: true, isError: true, result: tc.message}, 0, nil) {
				t.Fatal("second retry")
			}
		})
	}
}

func TestClaudeAuthSupervisedDurableCompletion(t *testing.T) {
	t.Setenv("SPAWN_CLAUDE_AUTH_FALLBACK", "oauth-then-api")
	t.Setenv(ClaudeOAuthTokenKeysEnv, "claude-oauth-token")
	be := &authReplayBackend{output: `{"type":"result","subtype":"success","is_error":true,"result":"Invalid API key · Please run /login"}`}
	o := newStopRaceOrchestrator(t, be)
	id, _ := seedSupervisedRunningSpawn(t, o.ctrl, true)
	st, _ := o.ctrl.Get(id)
	st.AuthMode = spawn.AuthModeClusterOAuth
	st.AuthAccount = "claude-oauth-token"
	o.ctrl.UpdateState(t.Context(), st)
	calls := make(chan string, 1)
	o.redriveSpawn = func(id string, _ SpawnRequest) { calls <- id }
	o.finishSupervisedOutcome(t.Context(), st, 0)
	select {
	case <-calls:
	case <-time.After(time.Second):
		t.Fatal("no supervised relaunch")
	}
	after, _ := o.ctrl.Get(id)
	if after.AuthOutcome != string(authOAuthRejected) || after.Status != spawn.StatusRunning {
		t.Fatalf("state=%+v", after)
	}
	if len(be.execCalls) != 1 || !strings.Contains(be.execCalls[0].Command, "agent.log") {
		t.Fatal("missing durable log replay")
	}
}

func TestClaudeAuthAccountExclusion(t *testing.T) {
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	accounts := parseClaudeAccounts("a,b")
	state := &spawn.State{Request: spawn.Request{AgentType: "claude-code"}, AuthFailures: []spawn.AuthFailure{{Account: "a", Outcome: string(authOAuthRejected), At: now, ResetAt: now.Add(10 * time.Minute)}}}
	if got := claudeHealthyAccounts(accounts, []*spawn.State{state}, now, nil); len(got) != 1 || got[0].Key != "b" {
		t.Fatalf("healthy=%+v", got)
	}
	if got := claudeHealthyAccounts(accounts, []*spawn.State{state}, now.Add(10*time.Minute), nil); len(got) != 2 {
		t.Fatal("TTL did not expire")
	}
	t.Setenv("SPAWN_CLAUDE_AUTH_CLEAR_BEFORE", now.Add(time.Second).Format(time.RFC3339))
	if len(claudeHealthyAccounts(accounts, []*spawn.State{state}, now, nil)) != 2 {
		t.Fatal("clear did not release account")
	}
	reset := claudeAuthReset("You've hit your limit · resets 8am (America/Los_Angeles)", "a", now)
	if !reset.Equal(time.Date(2026, 9, 14, 15, 0, 0, 0, time.UTC)) {
		t.Fatalf("reset=%s", reset)
	}
	if reset = claudeAuthReset("no printed reset", "a", now); !reset.Equal(now.Add(7 * 24 * time.Hour)) {
		t.Fatalf("fallback reset=%s", reset)
	}
}

func TestClaudeAuthPoolRotationBounded(t *testing.T) {
	t.Setenv("SPAWN_CLAUDE_AUTH_FALLBACK", "oauth-then-api")
	t.Setenv(ClaudeOAuthTokenKeysEnv, "a,b")
	o := newStopRaceOrchestrator(t, &recordingBackend{})
	id, _ := seedSupervisedRunningSpawn(t, o.ctrl, true)
	st, _ := o.ctrl.Get(id)
	st.AuthMode = spawn.AuthModeClusterOAuth
	st.AuthAccount = "a"
	o.ctrl.UpdateState(t.Context(), st)
	calls := make(chan string, 3)
	o.redriveSpawn = func(id string, _ SpawnRequest) { calls <- id }
	for attempt := 0; attempt < 2; attempt++ {
		st, _ = o.ctrl.Get(id)
		if !o.handleClaudeAuthCompletion(t.Context(), st, &claudeParseResult{sawResult: true, isError: true, result: "Invalid API key · Please run /login"}, 0, nil) {
			t.Fatal("no retry")
		}
		select {
		case <-calls:
		case <-time.After(time.Second):
			t.Fatal("no launch")
		}
		st, _ = o.ctrl.Get(id)
		if attempt == 0 && (st.AuthAccount != "b" || st.AuthFallbackAt != nil) {
			t.Fatal("did not rotate within pool first")
		}
		if attempt == 1 && (st.AuthMode != spawn.AuthModeClusterAPIKey || st.AuthFallbackAt == nil) {
			t.Fatal("did not cross after pool exhaustion")
		}
		st.AuthRetryPending = false
		o.ctrl.UpdateState(t.Context(), st)
	}
	if o.handleClaudeAuthCompletion(t.Context(), st, &claudeParseResult{sawResult: true, isError: true, result: "credit balance"}, 0, nil) {
		t.Fatal("retry after cross-mode transition")
	}
}

func TestClaudeAuthPolicyDefaultsAndInvalid(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want int
	}{{"", 6}, {"0", 0}, {"-1", 0}, {"oops", 0}, {"2", 2}} {
		t.Setenv("SPAWN_CLAUDE_API_FALLBACK_MAX_PER_DAY", tc.raw)
		if got := claudeAuthDailyCap(); got != tc.want {
			t.Fatalf("cap %q = %d", tc.raw, got)
		}
	}
	t.Setenv("SPAWN_CLAUDE_AUTH_FALLBACK", "")
	if claudeAuthPolicy() != "oauth-then-api" {
		t.Fatal("default policy")
	}
	t.Setenv("SPAWN_CLAUDE_AUTH_FALLBACK", "typo")
	if claudeAuthPolicy() != "none" {
		t.Fatal("invalid policy must fail closed")
	}
}

type missingAuthBackend struct{ recordingBackend }

func (*missingAuthBackend) ResolveSecretEnv(context.Context, []backend.SecretEnvVar) (map[string]string, error) {
	return nil, nil
}
func (*missingAuthBackend) ResolveSecretMounts(context.Context, []backend.SecretMount) ([]backend.ResolvedSecretFile, error) {
	return nil, nil
}
func TestClaudeAuthMissingAlternative(t *testing.T) {
	t.Setenv(ClaudeOAuthTokenKeysEnv, "a")
	t.Setenv("SPAWN_CLAUDE_AUTH_FALLBACK", "oauth-then-api")
	o := newStopRaceOrchestrator(t, &missingAuthBackend{})
	id, _ := seedSupervisedRunningSpawn(t, o.ctrl, true)
	st, _ := o.ctrl.Get(id)
	st.AuthMode = spawn.AuthModeClusterOAuth
	st.AuthAccount = "a"
	o.ctrl.UpdateState(t.Context(), st)
	if o.handleClaudeAuthCompletion(t.Context(), st, &claudeParseResult{sawResult: true, isError: true, result: "Please run /login"}, 0, nil) {
		t.Fatal("missing credential retried")
	}
	after, _ := o.ctrl.Get(id)
	if after.AuthFallbackAt != nil || len(after.AuthFailures) != 1 {
		t.Fatal("missing credential spent cap or lost failure")
	}
}
