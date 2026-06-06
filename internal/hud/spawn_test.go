package hud

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crb2nu/loom/internal/devbox/backend"
	"github.com/crb2nu/loom/internal/spawn"
)

func TestSpawnOrchestrator_SpawnIsIdempotentForActiveMillsStage(t *testing.T) {
	ctx := context.Background()
	ctrl := spawn.NewK8sController(nil, "", nil, slog.Default())
	req := SpawnRequest{
		AgentType:       "claude-code",
		Project:         "loom-core",
		Branch:          "feat/MILLS-CANARY-1",
		TaskDescription: "plan",
		Metadata: map[string]string{
			"LOOM_MILLS_RUN_ID": "PIPE-MILLS-CANARY-1",
			"LOOM_MILLS_STAGE":  "plan_slice",
		},
	}
	existing, err := ctrl.Spawn(ctx, req)
	if err != nil {
		t.Fatalf("seed spawn: %v", err)
	}
	o := NewSpawnOrchestratorForTest(ctrl)

	got, err := o.Spawn(ctx, req)
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if got != existing {
		t.Fatalf("spawn id = %q, want existing %q", got, existing)
	}
	if got := ctrl.ActiveCount(); got != 1 {
		t.Fatalf("active spawns = %d, want 1", got)
	}
}

func TestSpawnOrchestrator_DoesNotReuseTerminalMillsSpawn(t *testing.T) {
	ctx := context.Background()
	ctrl := spawn.NewK8sController(nil, "", nil, slog.Default())
	req := SpawnRequest{
		AgentType:       "claude-code",
		Project:         "loom-core",
		Branch:          "feat/MILLS-CANARY-1",
		TaskDescription: "plan",
		Metadata: map[string]string{
			"LOOM_MILLS_RUN_ID": "PIPE-MILLS-CANARY-1",
			"LOOM_MILLS_STAGE":  "plan_slice",
		},
	}
	existing, err := ctrl.Spawn(ctx, req)
	if err != nil {
		t.Fatalf("seed spawn: %v", err)
	}
	state, ok := ctrl.Get(existing)
	if !ok {
		t.Fatalf("seeded spawn missing")
	}
	state.Status = spawn.StatusCompleted
	ctrl.UpdateState(ctx, state)

	o := NewSpawnOrchestratorForTest(ctrl)
	if got := o.existingActiveSpawnForRequest(req); got != "" {
		t.Fatalf("terminal spawn was reused: %q (existing %q)", got, existing)
	}
}

func TestBuildAgentCommand(t *testing.T) {
	tests := []struct {
		agentType       string
		task            string
		agentID         string
		wantContains    []string
		wantNotContains []string
	}{
		{
			agentType: "claude-code",
			task:      "fix the tests",
			agentID:   "spawn-claude-code-abc123",
			wantContains: []string{
				"claude -p",
				"--output-format stream-json",
				"--max-turns 50",
				"--dangerously-skip-permissions",
			},
			wantNotContains: []string{
				"--output-format json ", // not the non-streaming format (trailing space distinguishes from stream-json)
			},
		},
		{
			agentType: "codex",
			task:      "fix the tests",
			agentID:   "spawn-codex-abc123",
			wantContains: []string{
				"codex exec",
				// --dangerously-bypass-approvals-and-sandbox is codex's
				// equivalent of claude --dangerously-skip-permissions.
				// Without it, headless codex pauses for approval on every
				// shell command (git add/commit/push, go test, …) and the
				// implement stage produces empty MRs. Pin the flag so the
				// silent-no-push failure mode can't return.
				"--dangerously-bypass-approvals-and-sandbox",
				"--skip-git-repo-check",
				"--json",
				// `< /dev/null` prevents the codex 0.120.0+ "Reading
				// additional input from stdin..." hang/exit when stdin is a
				// non-TTY pipe (both the K8s StreamExec Stdin:false path and
				// the harvester-vm SSH nil-stdin path). Refs openai/codex#20919.
				"< /dev/null",
				// --model pins a ChatGPT-account-supported codex model.
				// codex's default (gpt-5.3-codex) is deprecated for
				// ChatGPT sign-in and 400s ("not supported when using
				// Codex with a ChatGPT account") — the Mills A2 kill-test
				// failure mode (2026-06-06). See resolveCodexModel.
				"--model 'gpt-5.5'",
				"trap",
				"session-end",
				"spawn-codex-abc123",
			},
			wantNotContains: []string{
				// The deprecated default model must never be emitted — it
				// 400s under ChatGPT-account auth.
				"gpt-5.3-codex",
				// --full-auto was removed in codex 0.110+; verify we
				// don't regress to the deprecated flag.
				"--full-auto",
				// --sandbox workspace-write blocks network for shell
				// commands (git push silently fails).
				"--sandbox workspace-write",
				// --sandbox danger-full-access opens the sandbox but
				// still leaves the approval-prompt hang in headless
				// pods. Bypass-approvals-and-sandbox covers both.
				"--sandbox danger-full-access",
			},
		},
		{
			agentType: "gemini",
			task:      "fix the tests",
			agentID:   "spawn-gemini-abc123",
			wantContains: []string{
				"gemini -p",
				"--yolo",
			},
		},
		{
			agentType: "unsupported",
			task:      "anything",
			agentID:   "spawn-unsupported-abc123",
			wantContains: []string{
				"echo",
				"Unsupported",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.agentType, func(t *testing.T) {
			got := buildAgentCommand(tt.agentType, tt.task, tt.agentID)
			for _, s := range tt.wantContains {
				if !strings.Contains(got, s) {
					t.Errorf("buildAgentCommand(%q) = %q, want to contain %q", tt.agentType, got, s)
				}
			}
			for _, s := range tt.wantNotContains {
				if strings.Contains(got, s) {
					t.Errorf("buildAgentCommand(%q) = %q, want NOT to contain %q", tt.agentType, got, s)
				}
			}
		})
	}
}

func TestBuildAgentCommand_CodexTrapContainsAgentID(t *testing.T) {
	agentID := "spawn-codex-deadbeef"
	cmd := buildAgentCommand("codex", "do something", agentID)

	// The EXIT trap must reference the exact agent ID for session cleanup.
	if !strings.Contains(cmd, agentID) {
		t.Errorf("codex command missing agent ID in trap: %q", cmd)
	}

	// Verify the trap suppresses errors so missing loom binary is safe.
	if !strings.Contains(cmd, "2>/dev/null") {
		t.Errorf("codex command missing stderr suppression in trap: %q", cmd)
	}
}

func TestResolveCodexModel(t *testing.T) {
	// Default: the pinned ChatGPT-account-supported model. Must NOT be the
	// deprecated codex default (gpt-5.3-codex), which 400s under ChatGPT auth.
	t.Run("default", func(t *testing.T) {
		t.Setenv("SPAWN_CODEX_MODEL", "")
		if got := resolveCodexModel(); got != defaultCodexModel {
			t.Errorf("resolveCodexModel() = %q, want %q", got, defaultCodexModel)
		}
		if defaultCodexModel == "gpt-5.3-codex" {
			t.Fatalf("defaultCodexModel must not be the deprecated ChatGPT-unsupported model")
		}
	})

	// Env override lets operators retune without a rebuild when OpenAI shifts
	// the ChatGPT-supported model set again.
	t.Run("env override", func(t *testing.T) {
		t.Setenv("SPAWN_CODEX_MODEL", "gpt-5.4")
		if got := resolveCodexModel(); got != "gpt-5.4" {
			t.Errorf("resolveCodexModel() with override = %q, want %q", got, "gpt-5.4")
		}
	})

	// Whitespace-only override falls back to the default (not an empty --model).
	t.Run("blank override falls back", func(t *testing.T) {
		t.Setenv("SPAWN_CODEX_MODEL", "   ")
		if got := resolveCodexModel(); got != defaultCodexModel {
			t.Errorf("resolveCodexModel() with blank override = %q, want %q", got, defaultCodexModel)
		}
	})
}

func TestBuildAgentCommand_ClaudeStreamJSON(t *testing.T) {
	cmd := buildAgentCommand("claude-code", "refactor module", "spawn-claude-xyz")

	// Ensure we use stream-json, not plain json.
	if !strings.Contains(cmd, "stream-json") {
		t.Errorf("expected stream-json in claude command: %q", cmd)
	}

	// Count occurrences: "stream-json" should appear exactly once.
	if count := strings.Count(cmd, "stream-json"); count != 1 {
		t.Errorf("expected exactly 1 occurrence of stream-json, got %d in: %q", count, cmd)
	}
}

// TestBuildAgentCommand_PromptShellQuotedAgainstInjection is the regression
// for the Mills A2 plan_slice failure (2026-06-05, run
// PIPE-MILLS-CANARY-20260605-145334). The canary SpecDoc wraps the fixture
// path and backlog id in backticks (`testdata/mills-canary/heartbeat.md`,
// `MILLS-CANARY-...`). buildAgentCommand interpolated the prompt with Go %q
// and the result runs via `sh -c`, so the shell command-substituted those
// backticks before the agent CLI ran:
//
//	sh: testdata/mills-canary/heartbeat.md: Permission denied
//	sh: MILLS-CANARY-20260605-145334: not found
//
// and the agent received a prompt with the backtick spans silently stripped —
// blocking the first autonomous merge. The fix shell-quotes the prompt. This
// test executes the built command under a real shell with stubbed agent
// binaries and asserts the prompt reaches the CLI verbatim with no shell
// evaluation. It fails on the old %q construction (the $(...) marker fires and
// the captured prompt is mangled) and passes once shellQuote is used.
func TestBuildAgentCommand_PromptShellQuotedAgainstInjection(t *testing.T) {
	binDir := t.TempDir()
	argsOut := filepath.Join(t.TempDir(), "args.txt")
	pwnedMarker := filepath.Join(t.TempDir(), "pwned")

	// A canary-shaped prompt: backticks around the path + id, a $(...) command
	// substitution, a $VAR expansion, and an embedded single quote to exercise
	// the '\'' splice in shellQuote.
	task := "Plan `MILLS-CANARY-20260605-145334`: update only " +
		"`testdata/mills-canary/heartbeat.md`. $(touch " + pwnedMarker + ") " +
		"keep $HOME untouched; it's deterministic."

	// Stub every binary the commands may invoke (codex/claude/gemini run the
	// turn; loom fires in the codex EXIT trap). Each stub appends the args it
	// received so we can assert the agent saw the prompt verbatim.
	stub := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\" >> " + shellQuote(argsOut) + "; done\n"
	for _, name := range []string{"codex", "claude", "gemini", "loom"} {
		if err := os.WriteFile(filepath.Join(binDir, name), []byte(stub), 0o755); err != nil {
			t.Fatalf("write stub %s: %v", name, err)
		}
	}

	for _, agentType := range []string{"claude-code", "codex", "gemini"} {
		t.Run(agentType, func(t *testing.T) {
			_ = os.Remove(argsOut)
			cmd := buildAgentCommand(agentType, task, "spawn-"+agentType+"-abc123")

			c := exec.CommandContext(context.Background(), "sh", "-c", cmd)
			c.Env = append(os.Environ(),
				"PATH="+binDir+":"+os.Getenv("PATH"),
				"HOME=/home/stub-should-not-expand",
			)
			out, _ := c.CombinedOutput()

			// No $(...) side effect: the prompt must not be evaluated.
			if _, err := os.Stat(pwnedMarker); err == nil {
				_ = os.Remove(pwnedMarker)
				t.Fatalf("$(...) in the prompt was executed by the shell; cmd=%q", cmd)
			}
			// No prompt token executed as a command.
			if s := string(out); strings.Contains(s, "not found") || strings.Contains(s, "Permission denied") {
				t.Fatalf("prompt tokens ran as shell commands: %q\ncmd=%q", s, cmd)
			}

			recorded, err := os.ReadFile(argsOut)
			if err != nil {
				t.Fatalf("agent stub captured no args (prompt never reached the CLI): %v\ncmd=%q", err, cmd)
			}
			// The agent must receive the prompt byte-for-byte: backticks intact,
			// $(...) and $HOME unexpanded, single quote preserved.
			if !strings.Contains(string(recorded), task) {
				t.Fatalf("agent CLI received a mangled prompt.\n got: %q\nwant substring: %q\ncmd=%q", recorded, task, cmd)
			}
		})
	}
}

func TestAgentCLIInstallLines_EnsuresNPM(t *testing.T) {
	tests := []struct {
		agentType string
		pkgName   string
	}{
		{"claude-code", "@anthropic-ai/claude-code"},
		{"codex", "@openai/codex"},
		{"gemini", "@google/gemini-cli"},
	}
	for _, tt := range tests {
		t.Run(tt.agentType, func(t *testing.T) {
			lines := agentCLIInstallLines(tt.agentType)
			for _, want := range []string{
				"command -v npm",
				"apk add --no-cache nodejs npm",
				"apt-get install -y --no-install-recommends nodejs npm",
				"npm install -g " + tt.pkgName,
			} {
				if !strings.Contains(lines, want) {
					t.Fatalf("agentCLIInstallLines(%q) missing %q:\n%s", tt.agentType, want, lines)
				}
			}
		})
	}
}

func TestGenerateDockerfile_UsesLeanAgentRuntime(t *testing.T) {
	o := &SpawnOrchestrator{logger: slog.Default()}
	df, err := o.generateDockerfile(t.TempDir(), "claude-code")
	if err != nil {
		t.Fatalf("generateDockerfile: %v", err)
	}
	got := string(df)
	for _, want := range []string{
		"FROM golang:1.25.10-alpine",
		"apk add --no-cache ca-certificates git make bash curl nodejs npm python3",
		"npm install -g @anthropic-ai/claude-code@",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("generated Dockerfile missing %q:\n%s", want, got)
		}
	}
	for _, forbidden := range []string{
		"go mod download",
		"golangci-lint",
		"gosec",
	} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("generated Dockerfile contains slow project tooling %q:\n%s", forbidden, got)
		}
	}
}

func TestAgentRuntimeBuildTag_StableForDockerfile(t *testing.T) {
	df := []byte("FROM scratch\n")
	got := agentRuntimeBuildTag("claude-code", df)
	again := agentRuntimeBuildTag("claude-code", df)
	changed := agentRuntimeBuildTag("claude-code", []byte("FROM busybox\n"))

	if got != again {
		t.Fatalf("expected stable tag for identical runtime Dockerfile, got %q then %q", got, again)
	}
	if got == changed {
		t.Fatalf("expected Dockerfile content to affect runtime tag, got %q", got)
	}
	if !strings.HasPrefix(got, "spawn-runtime-claude-code:") {
		t.Fatalf("unexpected runtime tag: %q", got)
	}
	if strings.Contains(got, " ") || strings.Contains(got, "_") {
		t.Fatalf("runtime tag should be registry-safe, got %q", got)
	}
}

func TestIsPreRuntimeSpawnStatus(t *testing.T) {
	if !isPreRuntimeSpawnStatus(spawn.StatusPending) || !isPreRuntimeSpawnStatus(spawn.StatusBuilding) {
		t.Fatal("pending/building spawns should be resumable before a runtime pod exists")
	}
	for _, status := range []spawn.Status{spawn.StatusRunning, spawn.StatusCompleted, spawn.StatusFailed, spawn.StatusStopped} {
		if isPreRuntimeSpawnStatus(status) {
			t.Fatalf("status %q should not be treated as pre-runtime", status)
		}
	}
}

func TestAgentSecretMounts_ClaudeOAuthFromClusterSecret(t *testing.T) {
	mounts := agentSecretMounts("claude-code")
	if len(mounts) != 1 {
		t.Fatalf("expected 1 mount, got %d", len(mounts))
	}
	m := mounts[0]
	if m.SecretName != ClusterAgentAuthSecret {
		t.Fatalf("SecretName = %q, want %q", m.SecretName, ClusterAgentAuthSecret)
	}
	wantClaudeMount := AgentHomeDir + "/.claude.auth"
	if m.MountPath != wantClaudeMount {
		t.Fatalf("MountPath = %q, want %q (staging dir, not %s/.claude)", m.MountPath, wantClaudeMount, AgentHomeDir)
	}
	if strings.HasPrefix(m.MountPath, AgentHomeDir+"/.claude/") || m.MountPath == AgentHomeDir+"/.claude" {
		t.Fatalf("Claude mount must NOT shadow writable .claude/ config dir: %q", m.MountPath)
	}
	if len(m.Items) != 1 || m.Items[0].Key != "claude-oauth-json" || m.Items[0].Path != "oauth.json" {
		t.Fatalf("unexpected items: %#v", m.Items)
	}
}

func TestAgentSecretMounts_CodexOAuthFromClusterSecret(t *testing.T) {
	mounts := agentSecretMounts("codex")
	if len(mounts) != 1 {
		t.Fatalf("expected 1 mount, got %d", len(mounts))
	}
	m := mounts[0]
	if m.SecretName != ClusterAgentAuthSecret {
		t.Fatalf("SecretName = %q, want %q", m.SecretName, ClusterAgentAuthSecret)
	}
	wantCodexMount := AgentHomeDir + "/.codex.auth"
	if m.MountPath != wantCodexMount {
		t.Fatalf("MountPath = %q, want %q (staging dir, not %s/.codex)", m.MountPath, wantCodexMount, AgentHomeDir)
	}
	if strings.HasPrefix(m.MountPath, AgentHomeDir+"/.codex/") || m.MountPath == AgentHomeDir+"/.codex" {
		t.Fatalf("Codex mount must NOT shadow writable .codex/ config dir: %q", m.MountPath)
	}
	if len(m.Items) != 1 || m.Items[0].Key != "codex-auth-json" || m.Items[0].Path != "auth.json" {
		t.Fatalf("unexpected items: %#v", m.Items)
	}
}

func TestAgentSecretMounts_GeminiServiceAccount(t *testing.T) {
	mounts := agentSecretMounts("gemini")
	if len(mounts) != 1 {
		t.Fatalf("expected 1 mount, got %d", len(mounts))
	}
	m := mounts[0]
	if m.SecretName != ClusterAgentAPIKeysSecret {
		t.Fatalf("SecretName = %q, want %q", m.SecretName, ClusterAgentAPIKeysSecret)
	}
	if m.MountPath != GeminiSAMountPath {
		t.Fatalf("MountPath = %q, want %q", m.MountPath, GeminiSAMountPath)
	}
	if len(m.Items) != 1 {
		t.Fatalf("expected 1 mount item, got %d", len(m.Items))
	}
	if m.Items[0].Key != GeminiSAKeyName {
		t.Fatalf("item key = %q, want %q", m.Items[0].Key, GeminiSAKeyName)
	}
	if m.Items[0].Path != GeminiSAFilename {
		t.Fatalf("item path = %q, want %q", m.Items[0].Path, GeminiSAFilename)
	}
}

func TestAgentSecretMounts_NoLegacySecretReferences(t *testing.T) {
	// After Slice 2a, no mount should reference the Mac-sourced
	// agent-auth-tokens secret. This is a correctness guard to prevent
	// accidental reintroduction of the Mac->cluster credential bridge.
	for _, agentType := range []string{"claude-code", "codex", "gemini"} {
		t.Run(agentType, func(t *testing.T) {
			for _, m := range agentSecretMounts(agentType) {
				if m.SecretName == "agent-auth-tokens" {
					t.Fatalf("%s still references legacy agent-auth-tokens secret", agentType)
				}
			}
		})
	}
}

func TestAgentSecretEnvVars_UsesClusterSecret(t *testing.T) {
	allowed := map[string]bool{
		ClusterAgentAPIKeysSecret: true,
		ClusterAgentAuthSecret:    true,
	}
	// codex intentionally omits API-key env vars to avoid overriding the
	// mounted auth.json — pinned by TestAgentSecretEnvVars_CodexOmitsAPIKeyEnv.
	for _, agentType := range []string{"claude-code", "gemini"} {
		t.Run(agentType, func(t *testing.T) {
			vars := agentSecretEnvVars(agentType)
			if len(vars) == 0 {
				t.Fatalf("expected non-empty env vars for %s", agentType)
			}
			for _, v := range vars {
				if !allowed[v.SecretName] {
					t.Fatalf("%s env %q uses secret %q, want one of %v",
						agentType, v.Name, v.SecretName, allowed)
				}
			}
		})
	}
}

// TestAgentSecretEnvVars_CodexOmitsAPIKeyEnv pins the regression that
// every codex spawn under MILLS-CANARY-* produced an empty MR before this
// fix landed. The codex CLI treats OPENAI_API_KEY as an override that
// takes precedence over auth.json — so a placeholder secret value
// ("PLACEHOLDER" in clusters without a static API key) returned
// `401 Unauthorized: Incorrect API key provided: PLACEHOLDER` from
// api.openai.com and the agent silently produced no work. The fix is to
// stop wiring those env vars; codex falls back to the OAuth auth.json
// mounted at ~/.codex/auth.json by agentSecretMounts.
func TestAgentSecretEnvVars_CodexOmitsAPIKeyEnv(t *testing.T) {
	vars := agentSecretEnvVars("codex")
	for _, v := range vars {
		if v.Name == "OPENAI_API_KEY" || v.Name == "CODEX_API_KEY" {
			t.Fatalf("codex must NOT receive %q env (overrides auth.json), got %+v",
				v.Name, vars)
		}
	}
}

func TestAgentSecretEnvVars_ClaudeOAuthToken(t *testing.T) {
	// Per vendor-sanctioned headless auth path
	// (https://code.claude.com/docs/en/authentication), CLAUDE_CODE_OAUTH_TOKEN
	// sourced from cluster-agent-auth.claude-oauth-token takes precedence over
	// ANTHROPIC_API_KEY when set. Both must be emitted so the pod gracefully
	// falls back when the OAuth key is absent.
	vars := agentSecretEnvVars("claude-code")
	var oauthVar, apiKeyVar *backend.SecretEnvVar
	for i := range vars {
		switch vars[i].Name {
		case "CLAUDE_CODE_OAUTH_TOKEN":
			oauthVar = &vars[i]
		case "ANTHROPIC_API_KEY":
			apiKeyVar = &vars[i]
		}
	}
	if oauthVar == nil {
		t.Fatalf("expected CLAUDE_CODE_OAUTH_TOKEN env var, got %+v", vars)
	}
	if oauthVar.SecretName != ClusterAgentAuthSecret {
		t.Fatalf("CLAUDE_CODE_OAUTH_TOKEN from secret %q, want %q",
			oauthVar.SecretName, ClusterAgentAuthSecret)
	}
	if oauthVar.SecretKey != "claude-oauth-token" {
		t.Fatalf("CLAUDE_CODE_OAUTH_TOKEN key %q, want claude-oauth-token", oauthVar.SecretKey)
	}
	if apiKeyVar == nil {
		t.Fatalf("expected ANTHROPIC_API_KEY fallback env var, got %+v", vars)
	}
}

func TestResolveAuthMode(t *testing.T) {
	tests := []struct {
		agentType string
		want      string
	}{
		// Claude/Codex configured for OAuth via cluster-agent-auth; runtime
		// fallback to API-key env is not reflected here (see resolveAuthMode
		// docstring).
		{"claude-code", "cluster_oauth"},
		{"codex", "cluster_oauth"},
		{"gemini", "cluster_service_account"},
		{"unknown", ""},
	}
	for _, tc := range tests {
		t.Run(tc.agentType, func(t *testing.T) {
			got := string(resolveAuthMode(tc.agentType))
			if got != tc.want {
				t.Fatalf("resolveAuthMode(%q) = %q, want %q", tc.agentType, got, tc.want)
			}
		})
	}
}

// newWaitTestOrchestrator builds a minimal SpawnOrchestrator with just a
// controller attached, sufficient for exercising Wait() without pulling
// in the full K8s backend. Pre-seeds any provided states.
func newWaitTestOrchestrator(t *testing.T, seed ...*spawn.State) *SpawnOrchestrator {
	t.Helper()
	ctrl := spawn.NewK8sController(nil, "test", nil, slog.Default())
	for _, st := range seed {
		ctrl.UpdateState(context.Background(), st)
	}
	return &SpawnOrchestrator{ctrl: ctrl}
}

func TestSpawnOrchestrator_Wait_ReturnsImmediatelyForTerminalState(t *testing.T) {
	o := newWaitTestOrchestrator(t, &spawn.State{
		SpawnID: "already-done",
		Status:  spawn.StatusCompleted,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	state, err := o.Wait(ctx, "already-done")
	if err != nil {
		t.Fatalf("Wait error: %v", err)
	}
	if state.Status != spawn.StatusCompleted {
		t.Fatalf("Status = %q, want completed", state.Status)
	}
}

func TestSpawnOrchestrator_Wait_BlocksUntilTerminal(t *testing.T) {
	state := &spawn.State{
		SpawnID: "running-then-done",
		Status:  spawn.StatusRunning,
	}
	o := newWaitTestOrchestrator(t, state)

	// Flip to terminal after a short delay.
	go func() {
		time.Sleep(200 * time.Millisecond)
		done := *state
		done.Status = spawn.StatusCompleted
		o.ctrl.UpdateState(context.Background(), &done)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	got, err := o.Wait(ctx, "running-then-done")
	if err != nil {
		t.Fatalf("Wait error: %v", err)
	}
	if got.Status != spawn.StatusCompleted {
		t.Fatalf("Status = %q, want completed", got.Status)
	}
}

func TestSpawnOrchestrator_Wait_NotFound(t *testing.T) {
	o := newWaitTestOrchestrator(t)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	_, err := o.Wait(ctx, "nope")
	if err == nil {
		t.Fatal("expected error for missing spawn")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected 'not found' error, got %q", err.Error())
	}
}

func TestSpawnOrchestrator_Wait_ContextCancellation(t *testing.T) {
	o := newWaitTestOrchestrator(t, &spawn.State{
		SpawnID: "stuck-running",
		Status:  spawn.StatusRunning,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err := o.Wait(ctx, "stuck-running")
	if err == nil {
		t.Fatal("expected context cancellation error")
	}
	if !strings.Contains(err.Error(), "deadline exceeded") && !strings.Contains(err.Error(), "canceled") {
		t.Errorf("expected ctx error, got %q", err.Error())
	}
}

func TestSpawnOrchestrator_BroadcastWeaverParent_EmitsSidecar(t *testing.T) {
	hub := NewSSEHub(slog.Default())
	o := &SpawnOrchestrator{sseHub: hub}

	state := &SpawnState{
		SpawnID: "spawn-weaver-1",
		Request: spawn.Request{
			AgentType: "claude-code",
			Metadata: map[string]string{
				"weaver_query_id": "qid-42",
				"weaver_domain":   "cluster-ops-claude",
			},
		},
	}

	// Subscribe BEFORE broadcast so we catch both events.
	_, ch := hub.Subscribe()

	o.broadcastSpawnEvent("agent.spawn.building", state)

	seen := map[string]bool{}
	timeout := time.After(200 * time.Millisecond)
	for len(seen) < 2 {
		select {
		case ev := <-ch:
			seen[ev.Type] = true
		case <-timeout:
			t.Fatalf("timed out waiting for events; got %v", seen)
		}
	}

	if !seen["agent.spawn.building"] {
		t.Error("expected agent.spawn.building event")
	}
	if !seen["agent.spawn.weaver_parent"] {
		t.Error("expected agent.spawn.weaver_parent sidecar event")
	}
}

func TestSpawnOrchestrator_BroadcastWeaverParent_SkipsNonWeaverSpawn(t *testing.T) {
	hub := NewSSEHub(slog.Default())
	o := &SpawnOrchestrator{sseHub: hub}

	state := &SpawnState{
		SpawnID: "spawn-direct-1",
		Request: spawn.Request{AgentType: "claude-code"}, // no weaver metadata
	}

	_, ch := hub.Subscribe()
	o.broadcastSpawnEvent("agent.spawn.building", state)

	// Give the broadcast a moment; assert no weaver_parent event fires.
	deadline := time.After(150 * time.Millisecond)
	for {
		select {
		case ev := <-ch:
			if ev.Type == "agent.spawn.weaver_parent" {
				t.Fatal("unexpected weaver_parent event for direct spawn")
			}
		case <-deadline:
			return // clean exit — no weaver_parent ever showed
		}
	}
}

func TestSpawnOrchestrator_BroadcastWeaverParent_OnlyOnFirstEvent(t *testing.T) {
	hub := NewSSEHub(slog.Default())
	o := &SpawnOrchestrator{sseHub: hub}

	state := &SpawnState{
		SpawnID: "spawn-weaver-2",
		Request: spawn.Request{
			AgentType: "codex",
			Metadata:  map[string]string{"weaver_query_id": "qid-99"},
		},
	}

	_, ch := hub.Subscribe()
	// running is a later lifecycle event; weaver_parent should NOT fire here.
	o.broadcastSpawnEvent("agent.spawn.running", state)

	deadline := time.After(150 * time.Millisecond)
	for {
		select {
		case ev := <-ch:
			if ev.Type == "agent.spawn.weaver_parent" {
				t.Fatal("weaver_parent should only fire on first broadcast (agent.spawn.building)")
			}
		case <-deadline:
			return
		}
	}
}

// TestBuildSpawnPodEnv covers the Slice 2c env-propagation helper. The
// helper is what runSpawn calls before backend.Start, so its output
// shape IS the pod env contract.
func TestBuildSpawnPodEnv(t *testing.T) {
	tests := []struct {
		name     string
		req      SpawnRequest
		wantHave map[string]string // keys that must equal these values
		wantMiss []string          // keys that must NOT be present
	}{
		{
			name: "baseline_no_substrate_no_parent",
			req: SpawnRequest{
				AgentType: "claude-code",
				Namespace: "loom-mills",
			},
			wantHave: map[string]string{
				"AGENT_ID":  "agent-1",
				"SPAWN_ID":  "spawn-1",
				"NAMESPACE": "loom-mills",
			},
			wantMiss: []string{
				"LOOM_PARENT_SESSION_ID",
				"GOOGLE_APPLICATION_CREDENTIALS",
				"DEVBOX_BACKEND",
			},
		},
		{
			name: "substrate_harvester_vm_lands_as_DEVBOX_BACKEND",
			req: SpawnRequest{
				AgentType: "claude-code",
				Namespace: "loom-mills",
				Substrate: "harvester-vm",
			},
			wantHave: map[string]string{
				"DEVBOX_BACKEND": "harvester-vm",
			},
		},
		{
			name: "substrate_k8s_explicit",
			req: SpawnRequest{
				AgentType: "claude-code",
				Substrate: "k8s",
			},
			wantHave: map[string]string{
				"DEVBOX_BACKEND": "k8s",
			},
		},
		{
			name: "parent_session_id_propagates",
			req: SpawnRequest{
				AgentType:       "claude-code",
				ParentSessionID: "sess-abc",
			},
			wantHave: map[string]string{
				"LOOM_PARENT_SESSION_ID": "sess-abc",
			},
			wantMiss: []string{"DEVBOX_BACKEND"},
		},
		{
			name: "gemini_gets_GOOGLE_APPLICATION_CREDENTIALS",
			req: SpawnRequest{
				AgentType: "gemini",
			},
			wantHave: map[string]string{
				"GOOGLE_APPLICATION_CREDENTIALS": GeminiSAMountPath + "/" + GeminiSAFilename,
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := buildSpawnPodEnv(tc.req, "agent-1", "spawn-1")
			for k, want := range tc.wantHave {
				if got := env[k]; got != want {
					t.Errorf("env[%q] = %q; want %q", k, got, want)
				}
			}
			for _, k := range tc.wantMiss {
				if v, ok := env[k]; ok {
					t.Errorf("env[%q] unexpectedly present (= %q)", k, v)
				}
			}
		})
	}
}

// TestSubstrateBackend covers the Slice 2d substrate routing helper.
// Empty substrate → default backend (current pre-Slice-2c behavior).
// Known substrate → its registered backend.
// Unknown substrate → default backend (clean fallback, not nil).
func TestSubstrateBackend(t *testing.T) {
	k8s := &recordingBackend{}
	hvm := &recordingBackend{}
	o := &SpawnOrchestrator{
		backends: map[string]backend.Backend{
			DefaultSubstrate: k8s,
			"harvester-vm":   hvm,
		},
		defaultSubstrate: DefaultSubstrate,
		logger:           slog.Default(),
	}

	tests := []struct {
		name      string
		substrate string
		want      backend.Backend
	}{
		{"empty substrate falls back to default", "", k8s},
		{"explicit k8s lookup", "k8s", k8s},
		{"explicit harvester-vm lookup", "harvester-vm", hvm},
		{"unknown substrate falls back to default with warn log", "podman", k8s},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := o.substrateBackend(tc.substrate)
			if got != tc.want {
				t.Errorf("substrateBackend(%q) = %v; want %v", tc.substrate, got, tc.want)
			}
		})
	}
}

// TestSubstrateBackend_NilSafe covers the nil-orchestrator + empty-map
// edge cases so callers in test code (NewSpawnOrchestratorForTest) and
// the early-init resumePreRuntimeSpawns guard don't panic.
func TestSubstrateBackend_NilSafe(t *testing.T) {
	var nilOrch *SpawnOrchestrator
	if got := nilOrch.substrateBackend("anything"); got != nil {
		t.Errorf("nil orchestrator: substrateBackend → %v; want nil", got)
	}

	empty := &SpawnOrchestrator{
		backends:         map[string]backend.Backend{},
		defaultSubstrate: DefaultSubstrate,
		logger:           slog.Default(),
	}
	if got := empty.substrateBackend(""); got != nil {
		t.Errorf("empty backends: substrateBackend(\"\") → %v; want nil", got)
	}
}

// TestNewSpawnOrchestratorSingleBackend covers the legacy convenience
// wrapper so existing single-backend tests + callers keep their shape.
func TestNewSpawnOrchestratorSingleBackend(t *testing.T) {
	rb := &recordingBackend{}
	o := NewSpawnOrchestratorSingleBackend(
		rb, nil, nil, nil, nil, slog.Default(),
		SpawnOrchestratorConfig{},
	)
	if o == nil {
		t.Fatal("nil orchestrator")
	}
	if got := o.substrateBackend(""); got != rb {
		t.Errorf("substrateBackend(\"\") = %v; want %v", got, rb)
	}
	if got := o.substrateBackend(DefaultSubstrate); got != rb {
		t.Errorf("substrateBackend(%q) = %v; want %v", DefaultSubstrate, got, rb)
	}
	if got := o.substrateBackend("harvester-vm"); got != rb {
		t.Errorf("unknown substrate must fall back to default; got %v want %v", got, rb)
	}
}
