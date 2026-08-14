package hud

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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

func TestExistingActiveSpawnForRequest_MillsAttemptCompatibility(t *testing.T) {
	tests := []struct {
		name             string
		requestAttempt   string
		candidateAttempt string
		wantMatch        bool
	}{
		{name: "same attempt dedupes", requestAttempt: "2", candidateAttempt: "2", wantMatch: true},
		{name: "higher attempt does not dedupe", requestAttempt: "3", candidateAttempt: "2", wantMatch: false},
		{name: "missing request attempt preserves legacy dedupe", candidateAttempt: "2", wantMatch: true},
		{name: "missing candidate attempt preserves legacy dedupe", requestAttempt: "2", wantMatch: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			ctrl := spawn.NewK8sController(nil, "", nil, slog.Default())
			candidate := SpawnRequest{
				AgentType:       "codex",
				Project:         "loom-core",
				Branch:          "feat/attempt-aware-dedupe",
				TaskDescription: "implement",
				Metadata: map[string]string{
					"LOOM_MILLS_RUN_ID": "PIPE-ATTEMPT",
					"LOOM_MILLS_STAGE":  "implement",
				},
			}
			if tc.candidateAttempt != "" {
				candidate.Metadata["LOOM_MILLS_ATTEMPT"] = tc.candidateAttempt
			}
			existing, err := ctrl.Spawn(ctx, candidate)
			if err != nil {
				t.Fatalf("seed spawn: %v", err)
			}
			request := SpawnRequest{
				Project: "loom-core",
				Branch:  "feat/attempt-aware-dedupe",
				Metadata: map[string]string{
					"LOOM_MILLS_RUN_ID": "PIPE-ATTEMPT",
					"LOOM_MILLS_STAGE":  "implement",
				},
			}
			if tc.requestAttempt != "" {
				request.Metadata["LOOM_MILLS_ATTEMPT"] = tc.requestAttempt
			}

			got := NewSpawnOrchestratorForTest(ctrl).existingActiveSpawnForRequest(request)
			if tc.wantMatch && got != existing {
				t.Errorf("existing spawn = %q, want %q", got, existing)
			}
			if !tc.wantMatch && got != "" {
				t.Errorf("existing spawn = %q, want no match", got)
			}
		})
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
				"--max-turns 100",
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
				// The auth preflight fails the spawn fast — before codex
				// starts an unauthenticated turn — when ~/.codex/auth.json is
				// dangling/empty and OPENAI_API_KEY is unset (escalation #368:
				// the 401 "Missing bearer" $0 spew during the 2026-07-22
				// rollout window). The message is the spawn-auth-missing
				// classifier contract (pkg/mills/pipeline/spawn_class.go).
				"codex auth preflight failed",
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
			got := buildAgentCommand(tt.agentType, tt.task, tt.agentID, "", 0)
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

func TestBuildAgentCommand_ClaudePerRequestModel(t *testing.T) {
	const model = "claude-opus-5"
	withModel := buildAgentCommand("claude-code", "do work", "spawn-claude-1", model, 0)
	if !strings.Contains(withModel, "--model '"+model+"'") {
		t.Errorf("claude command missing per-request model: %q", withModel)
	}

	withoutModel := buildAgentCommand("claude-code", "do work", "spawn-claude-2", "", 0)
	if strings.Contains(withoutModel, "--model") {
		t.Errorf("claude command unexpectedly pins a model without a request override: %q", withoutModel)
	}
}

func TestBuildAgentCommand_ClaudeMaxTurns(t *testing.T) {
	// A request-supplied turn budget must reach the CLI verbatim; without one
	// the default applies (the old hard-coded 50 killed review-stage spawns
	// mid-test-suite-wait at error_max_turns).
	explicit := buildAgentCommand("claude-code", "do work", "spawn-claude-3", "", 200)
	if !strings.Contains(explicit, "--max-turns 200") {
		t.Errorf("claude command missing request turn budget: %q", explicit)
	}
	defaulted := buildAgentCommand("claude-code", "do work", "spawn-claude-4", "", 0)
	if !strings.Contains(defaulted, "--max-turns 100") {
		t.Errorf("claude command missing default turn budget: %q", defaulted)
	}
}

// TestCodexAuthPreflight_ShellBehavior executes the preflight guard through a
// real POSIX sh against every credential state the pod can be in — the #368
// regression (2026-07-22): the Optional cluster-agent-auth mount materialized
// absent during a fleet-rollout window, leaving ~/.codex/auth.json a DANGLING
// symlink, and codex burned $0 attempts on unauthenticated 401 turns. The
// guard must fail fast (exit 78, contract message on stderr) on the dangling /
// missing / empty shapes and stay transparent when a credential exists.
func TestCodexAuthPreflight_ShellBehavior(t *testing.T) {
	run := func(t *testing.T, home string, extraEnv ...string) (int, string) {
		t.Helper()
		script := codexAuthPreflight(home) + `; echo REACHED`
		cmd := exec.CommandContext(context.Background(), "sh", "-c", script)
		// Pin the env so a developer machine's exported OPENAI_API_KEY can't
		// flip the guard.
		cmd.Env = append([]string{"PATH=/usr/bin:/bin"}, extraEnv...)
		var out bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &out
		err := cmd.Run()
		code := 0
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			code = exitErr.ExitCode()
		} else if err != nil {
			t.Fatalf("sh: %v", err)
		}
		return code, out.String()
	}

	mkHome := func(t *testing.T) string {
		t.Helper()
		home := t.TempDir()
		if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o755); err != nil {
			t.Fatal(err)
		}
		return home
	}
	authPath := func(home string) string { return filepath.Join(home, ".codex", "auth.json") }

	t.Run("missing auth.json fails fast", func(t *testing.T) {
		code, out := run(t, mkHome(t))
		if code != 78 {
			t.Fatalf("exit = %d, want 78\n%s", code, out)
		}
		if !strings.Contains(out, "codex auth preflight failed") {
			t.Fatalf("missing classifier contract message:\n%s", out)
		}
	})

	t.Run("dangling symlink fails fast (the #368 mount shape)", func(t *testing.T) {
		home := mkHome(t)
		if err := os.Symlink(filepath.Join(home, ".codex.auth", "auth.json"), authPath(home)); err != nil {
			t.Fatal(err)
		}
		code, out := run(t, home)
		if code != 78 || !strings.Contains(out, "codex auth preflight failed") {
			t.Fatalf("exit = %d, out = %q; want 78 + contract message", code, out)
		}
	})

	t.Run("empty auth.json fails fast", func(t *testing.T) {
		home := mkHome(t)
		if err := os.WriteFile(authPath(home), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		code, out := run(t, home)
		if code != 78 || !strings.Contains(out, "codex auth preflight failed") {
			t.Fatalf("exit = %d, out = %q; want 78 + contract message", code, out)
		}
	})

	t.Run("populated auth.json passes through", func(t *testing.T) {
		home := mkHome(t)
		if err := os.WriteFile(authPath(home), []byte(`{"tokens":{}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		code, out := run(t, home)
		if code != 0 || !strings.Contains(out, "REACHED") {
			t.Fatalf("exit = %d, out = %q; want 0 + REACHED", code, out)
		}
	})

	t.Run("OPENAI_API_KEY env fallback passes through", func(t *testing.T) {
		code, out := run(t, mkHome(t), "OPENAI_API_KEY=sk-test")
		if code != 0 || !strings.Contains(out, "REACHED") {
			t.Fatalf("exit = %d, out = %q; want 0 + REACHED", code, out)
		}
	})
}

func TestBuildAgentCommand_CodexTrapContainsAgentID(t *testing.T) {
	agentID := "spawn-codex-deadbeef"
	cmd := buildAgentCommand("codex", "do something", agentID, "", 0)

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
		if got := resolveCodexModel(""); got != defaultCodexModel {
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
		if got := resolveCodexModel(""); got != "gpt-5.4" {
			t.Errorf("resolveCodexModel() with override = %q, want %q", got, "gpt-5.4")
		}
	})

	// Whitespace-only override falls back to the default (not an empty --model).
	t.Run("blank override falls back", func(t *testing.T) {
		t.Setenv("SPAWN_CODEX_MODEL", "   ")
		if got := resolveCodexModel(""); got != defaultCodexModel {
			t.Errorf("resolveCodexModel() with blank override = %q, want %q", got, defaultCodexModel)
		}
	})

	// Per-request model (Mills stage_models) wins over both the env override and
	// the compiled default — this is how implement runs gpt-5.6-terra while
	// plan_slice runs gpt-5.6-sol without a global env flip.
	t.Run("request model wins over env", func(t *testing.T) {
		t.Setenv("SPAWN_CODEX_MODEL", "gpt-5.4")
		if got := resolveCodexModel("gpt-5.6-terra"); got != "gpt-5.6-terra" {
			t.Errorf("resolveCodexModel(req) = %q, want %q", got, "gpt-5.6-terra")
		}
	})

	// A whitespace-only request model is treated as unset and falls through to
	// the env / default chain (never an empty `--model`).
	t.Run("blank request model falls through to env", func(t *testing.T) {
		t.Setenv("SPAWN_CODEX_MODEL", "gpt-5.4")
		if got := resolveCodexModel("  "); got != "gpt-5.4" {
			t.Errorf("resolveCodexModel(blank req) = %q, want %q", got, "gpt-5.4")
		}
	})
}

// TestBuildAgentCommand_CodexPerRequestModel confirms a per-spawn model reaches
// the codex `--model` flag, and an empty model preserves the vendor default.
func TestBuildAgentCommand_CodexPerRequestModel(t *testing.T) {
	t.Setenv("SPAWN_CODEX_MODEL", "")
	withModel := buildAgentCommand("codex", "do work", "spawn-codex-1", "gpt-5.6-terra", 0)
	if !strings.Contains(withModel, "--model 'gpt-5.6-terra'") {
		t.Errorf("codex command missing per-request model: %q", withModel)
	}
	empty := buildAgentCommand("codex", "do work", "spawn-codex-2", "", 0)
	if !strings.Contains(empty, "--model '"+defaultCodexModel+"'") {
		t.Errorf("empty request model should fall back to default %q: %q", defaultCodexModel, empty)
	}
}

// TestBuildAgentCommand_CodexArgvPinned pins the EXACT codex argv so a future
// refactor cannot silently displace the prompt positional, drop the headless
// `< /dev/null` guard, or reorder the `exec` subcommand — the shapes that make
// codex read the never-closing exec pipe and wedge on stdin (or exit 1 with the
// misleading "Reading additional input from stdin..." tail). It also proves the
// per-request model (the !1125 pass-through that triggered the 2026-07-18
// outage) changes ONLY the --model token: every byte before and after is
// identical to the no-model default invocation, so the model value can never be
// the thing that mangles the command line.
func TestBuildAgentCommand_CodexArgvPinned(t *testing.T) {
	t.Setenv("SPAWN_CODEX_MODEL", "")
	t.Setenv("SPAWN_CODEX_VERSION", "")

	const (
		agentID = "spawn-codex-argv"
		task    = "do work"
	)

	want := func(model string) string {
		return "trap 'loom agent session-end --agent-id \"" + agentID +
			"\" --summarize --summary-async --quiet 2>/dev/null' EXIT; " +
			codexAuthPreflight(AgentHomeDir) + "; " +
			"codex exec --dangerously-bypass-approvals-and-sandbox " +
			"--skip-git-repo-check --model '" + model + "' --json '" + task + "' < /dev/null"
	}

	withModel := buildAgentCommand("codex", task, agentID, "gpt-5.6-sol", 0)
	if withModel != want("gpt-5.6-sol") {
		t.Errorf("per-request-model argv drift:\n got: %q\nwant: %q", withModel, want("gpt-5.6-sol"))
	}

	noModel := buildAgentCommand("codex", task, agentID, "", 0)
	if noModel != want(defaultCodexModel) {
		t.Errorf("default-model argv drift:\n got: %q\nwant: %q", noModel, want(defaultCodexModel))
	}

	// Headless-safety invariants: the non-interactive `exec` subcommand and the
	// stdin redirect must survive any refactor. Without `< /dev/null` codex reads
	// the never-closing K8s exec pipe (Stdin:false) and the turn wedges.
	for _, s := range []string{"codex exec ", " < /dev/null"} {
		if !strings.Contains(withModel, s) {
			t.Errorf("codex argv missing headless-safety token %q: %q", s, withModel)
		}
	}

	// The ONLY difference between the two commands is the model token.
	if got := strings.Replace(withModel, "'gpt-5.6-sol'", "'"+defaultCodexModel+"'", 1); got != noModel {
		t.Errorf("per-request model changed more than the --model token:\n normalized=%q\n   noModel=%q", got, noModel)
	}
}

// TestResolveCodexVersion covers the SPAWN_CODEX_VERSION override: it clears a
// future codex-version model gate without a loom-core rebuild (the env-flip
// resilience that mirrors SPAWN_CODEX_MODEL), while a malformed value — which
// would be interpolated into a root `npm install -g @openai/codex@<value>` at
// image-build time — must be rejected in favor of the compiled default to
// prevent shell injection.
func TestResolveCodexVersion(t *testing.T) {
	t.Run("default when unset", func(t *testing.T) {
		t.Setenv("SPAWN_CODEX_VERSION", "")
		if got := resolveCodexVersion(); got != codexVersion {
			t.Errorf("resolveCodexVersion() = %q, want default %q", got, codexVersion)
		}
	})
	t.Run("valid semver override honored", func(t *testing.T) {
		t.Setenv("SPAWN_CODEX_VERSION", "0.144.5")
		if got := resolveCodexVersion(); got != "0.144.5" {
			t.Errorf("resolveCodexVersion() = %q, want %q", got, "0.144.5")
		}
	})
	t.Run("prerelease override honored", func(t *testing.T) {
		t.Setenv("SPAWN_CODEX_VERSION", "0.145.0-rc.1")
		if got := resolveCodexVersion(); got != "0.145.0-rc.1" {
			t.Errorf("resolveCodexVersion() = %q, want %q", got, "0.145.0-rc.1")
		}
	})
	t.Run("blank falls back to default", func(t *testing.T) {
		t.Setenv("SPAWN_CODEX_VERSION", "   ")
		if got := resolveCodexVersion(); got != codexVersion {
			t.Errorf("resolveCodexVersion(blank) = %q, want %q", got, codexVersion)
		}
	})
	// Every malformed value must be rejected: a shell-metacharacter payload would
	// otherwise reach a root `npm install` at build; a bare dist-tag ("latest")
	// or partial semver defeats reproducible pins.
	for _, bad := range []string{
		"0.143.0; rm -rf /",
		"$(curl evil)",
		"0.143.0 && echo hi",
		"`id`",
		"latest",
		"0.143",
		"../../etc",
		"v0.143.0",
	} {
		t.Run("reject_"+bad, func(t *testing.T) {
			t.Setenv("SPAWN_CODEX_VERSION", bad)
			if got := resolveCodexVersion(); got != codexVersion {
				t.Errorf("resolveCodexVersion(%q) = %q, want default %q (malformed must be rejected)", bad, got, codexVersion)
			}
		})
	}
}

// TestAgentCLIInstallLines_CodexVersionGate is the regression for the 2026-07-18
// gpt-5.6 outage (issues #347/#349/#350/#351): the k8s spawn pod ran codex
// 0.130.0, which the OpenAI API rejected for gpt-5.6-* with HTTP 400 "requires a
// newer version of Codex". The install lines (both the k8s Dockerfile RUN and
// the harvester-vm SSH installer) must pin a version new enough for gpt-5.6,
// must never regress to 0.130.0, and must honor the SPAWN_CODEX_VERSION override.
func TestAgentCLIInstallLines_CodexVersionGate(t *testing.T) {
	t.Setenv("SPAWN_CODEX_VERSION", "")
	line := agentCLIInstallLines("codex")
	if !strings.Contains(line, "@openai/codex@"+codexVersion) {
		t.Errorf("codex install line should pin default %q: %q", codexVersion, line)
	}
	if strings.Contains(line, "@openai/codex@0.130.0") {
		t.Errorf("codex install line regressed to the gpt-5.6-incompatible 0.130.0: %q", line)
	}

	t.Setenv("SPAWN_CODEX_VERSION", "0.144.5")
	if got := agentCLIInstallLines("codex"); !strings.Contains(got, "@openai/codex@0.144.5") {
		t.Errorf("codex Dockerfile install should honor SPAWN_CODEX_VERSION override: %q", got)
	}
	if got := agentCLIInstallShell("codex"); !strings.Contains(got, "@openai/codex@0.144.5") {
		t.Errorf("codex vm install shell should honor SPAWN_CODEX_VERSION override: %q", got)
	}
}

// TestCodexVersionMeetsGpt56Floor guards the numeric floor: any pin below
// 0.143.0 reintroduces the OpenAI-side gpt-5.6 version gate that broke every
// per-stage-model spawn on 2026-07-18.
func TestCodexVersionMeetsGpt56Floor(t *testing.T) {
	var maj, min, patch int
	if _, err := fmt.Sscanf(codexVersion, "%d.%d.%d", &maj, &min, &patch); err != nil {
		t.Fatalf("codexVersion %q is not dotted semver: %v", codexVersion, err)
	}
	// gpt-5.6-terra/sol require codex >= 0.143.0.
	if maj == 0 && min < 143 {
		t.Errorf("codexVersion %q is below the gpt-5.6 floor 0.143.0", codexVersion)
	}
}

func TestBuildAgentCommand_ClaudeStreamJSON(t *testing.T) {
	cmd := buildAgentCommand("claude-code", "refactor module", "spawn-claude-xyz", "", 0)

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
			cmd := buildAgentCommand(agentType, task, "spawn-"+agentType+"-abc123", "", 0)

			c := exec.CommandContext(context.Background(), "sh", "-c", cmd)
			c.Env = append(os.Environ(),
				"PATH="+binDir+":"+os.Getenv("PATH"),
				"HOME=/home/stub-should-not-expand",
				// Satisfy the codex auth preflight via its documented env
				// fallback — the test machine has no /home/agent/.codex/
				// auth.json and this test is about prompt quoting, not auth
				// (TestCodexAuthPreflight_ShellBehavior covers that).
				"OPENAI_API_KEY=test-stub",
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
		fmt.Sprintf("FROM golang:%s-alpine", goVersion),
		"apk add --no-cache ca-certificates git make bash curl nodejs npm python3",
		// go/gofmt must live in /usr/local/bin, not just the golang image's
		// ENV PATH — vendor agent CLIs sanitize the env for spawned shells
		// and lose /usr/local/go/bin (escalations #272–#278: "go is not
		// installed" while /usr/local/go/bin/go existed).
		"ln -sf /usr/local/go/bin/go /usr/local/bin/go",
		"ln -sf /usr/local/go/bin/gofmt /usr/local/bin/gofmt",
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

// TestGoVersionCoversGoMod pins the spawn-runtime Go toolchain to the repo's
// go.mod directive. The golang images set GOTOOLCHAIN=local, so a runtime
// image OLDER than the directive makes every in-spawn `go` command fail with
// "go.mod requires go >= X (running Y; GOTOOLCHAIN=local)" — which is exactly
// what happened when !979 raised the directive to 1.26.4 while goVersion sat
// at 1.25.11. A newer runtime than the directive is fine (go 1.26.4 builds a
// `go 1.26.0` module), so the invariant is goVersion >= directive.
func TestGoVersionCoversGoMod(t *testing.T) {
	raw, err := os.ReadFile("../../go.mod")
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	var directive string
	for _, line := range strings.Split(string(raw), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "go "); ok {
			directive = strings.TrimSpace(v)
			break
		}
	}
	if directive == "" {
		t.Fatal("no go directive found in go.mod")
	}
	if compareGoVersions(goVersion, directive) < 0 {
		t.Fatalf("spawn goVersion = %q is OLDER than the go.mod directive %q — bump the goVersion const in internal/hud/spawn.go together with the toolchain (GOTOOLCHAIN=local makes an older runtime image fail every in-spawn go command)", goVersion, directive)
	}
}

// compareGoVersions compares dotted go versions ("1.26.4" vs "1.26"), returning
// -1/0/+1. Missing segments count as zero.
func compareGoVersions(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		var ai, bi int
		if i < len(as) {
			ai, _ = strconv.Atoi(as[i])
		}
		if i < len(bs) {
			bi, _ = strconv.Atoi(bs[i])
		}
		if ai != bi {
			if ai < bi {
				return -1
			}
			return 1
		}
	}
	return 0
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

func TestAgentSecretMounts_ClaudeUsesNativeOAuthToken(t *testing.T) {
	mounts := agentSecretMounts("claude-code")
	if len(mounts) != 0 {
		t.Fatalf("claude must use CLAUDE_CODE_OAUTH_TOKEN natively, got stale OAuth mounts: %#v", mounts)
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
	// sourced from cluster-agent-auth.claude-oauth-token is the default and
	// avoids silently falling through to API billing.
	t.Setenv("LOOM_SPAWN_CLAUDE_API_KEY_FALLBACK", "")
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
	if apiKeyVar != nil {
		t.Fatalf("ANTHROPIC_API_KEY must be absent by default, got %+v", vars)
	}
}

func TestAgentSecretEnvVars_ClaudeAPIKeyFallbackOptIn(t *testing.T) {
	t.Setenv("LOOM_SPAWN_CLAUDE_API_KEY_FALLBACK", "1")
	vars := agentSecretEnvVars("claude-code")
	for _, v := range vars {
		if v.Name == "ANTHROPIC_API_KEY" {
			if v.SecretName != ClusterAgentAPIKeysSecret || v.SecretKey != "ANTHROPIC_API_KEY" {
				t.Fatalf("unexpected API-key fallback source: %+v", v)
			}
			return
		}
	}
	t.Fatalf("expected ANTHROPIC_API_KEY only when LOOM_SPAWN_CLAUDE_API_KEY_FALLBACK=1, got %+v", vars)
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
		{
			// Every spawn carries the Go toolchain env so the agent can
			// `go build`/`go test` a single-repo clone whose go.work `use`s
			// absent private siblings. Mirrors Dockerfile + .gitlab-ci.yml.
			name: "go_module_env_present_for_all_spawns",
			req: SpawnRequest{
				AgentType: "codex",
				Namespace: "loom-mills",
			},
			wantHave: map[string]string{
				"GOWORK":      "off",
				"GOFLAGS":     "-buildvcs=false",
				"CGO_ENABLED": "0",
				"GOPRIVATE":   "gitlab.flexinfer.ai/*",
				"GONOSUMDB":   "gitlab.flexinfer.ai/*",
				"GONOPROXY":   "gitlab.flexinfer.ai/*",
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

// TestResolveSpawnGitPrivateHost covers the default, env-override, and
// disable (empty) paths for the private-module host resolver.
func TestResolveSpawnGitPrivateHost(t *testing.T) {
	t.Run("default", func(t *testing.T) {
		t.Setenv("SPAWN_GIT_PRIVATE_HOST", "")
		os.Unsetenv("SPAWN_GIT_PRIVATE_HOST")
		if got := resolveSpawnGitPrivateHost(); got != "gitlab.flexinfer.ai" {
			t.Fatalf("default host = %q; want gitlab.flexinfer.ai", got)
		}
	})
	t.Run("override", func(t *testing.T) {
		t.Setenv("SPAWN_GIT_PRIVATE_HOST", "  gitlab.example.test  ")
		if got := resolveSpawnGitPrivateHost(); got != "gitlab.example.test" {
			t.Fatalf("override host = %q; want gitlab.example.test", got)
		}
	})
	t.Run("empty_disables", func(t *testing.T) {
		t.Setenv("SPAWN_GIT_PRIVATE_HOST", "")
		if got := resolveSpawnGitPrivateHost(); got != "" {
			t.Fatalf("explicit empty host = %q; want empty", got)
		}
	})
}

// TestSpawnGoModuleEnv asserts the toolchain env trio is built from the
// resolved host and that an empty host drops the private-module vars while
// keeping the single-repo-clone defaults.
func TestSpawnGoModuleEnv(t *testing.T) {
	t.Run("default_host", func(t *testing.T) {
		os.Unsetenv("SPAWN_GIT_PRIVATE_HOST")
		env := spawnGoModuleEnv()
		for k, want := range map[string]string{
			"GOWORK": "off", "GOFLAGS": "-buildvcs=false", "CGO_ENABLED": "0",
			"GOPRIVATE": "gitlab.flexinfer.ai/*", "GONOSUMDB": "gitlab.flexinfer.ai/*",
			"GONOPROXY": "gitlab.flexinfer.ai/*",
		} {
			if env[k] != want {
				t.Errorf("env[%q] = %q; want %q", k, env[k], want)
			}
		}
	})
	t.Run("empty_host_keeps_defaults_drops_private", func(t *testing.T) {
		t.Setenv("SPAWN_GIT_PRIVATE_HOST", "")
		env := spawnGoModuleEnv()
		if env["GOWORK"] != "off" || env["CGO_ENABLED"] != "0" {
			t.Errorf("single-repo defaults missing: %#v", env)
		}
		for _, k := range []string{"GOPRIVATE", "GONOSUMDB", "GONOPROXY"} {
			if _, ok := env[k]; ok {
				t.Errorf("env[%q] should be absent when host is empty", k)
			}
		}
	})
}

// TestInjectAgentConfigGitPrivateAuth proves injectAgentConfig issues the
// url.insteadOf credential command (guarded on $GIT_TOKEN) so the spawned agent
// can fetch private modules. Uses the recordingBackend to capture Exec calls.
func TestInjectAgentConfigGitPrivateAuth(t *testing.T) {
	os.Unsetenv("SPAWN_GIT_PRIVATE_HOST")
	rb := &recordingBackend{}
	o := &SpawnOrchestrator{}
	if err := o.injectAgentConfig(context.Background(), rb, "pod-1", "codex", "/workspace/services/loom-core"); err != nil {
		t.Fatalf("injectAgentConfig: %v", err)
	}
	var found bool
	for _, call := range rb.execCalls {
		if strings.Contains(call.Command, `url."https://token:${GIT_TOKEN}@gitlab.flexinfer.ai/".insteadOf "https://gitlab.flexinfer.ai/"`) &&
			strings.Contains(call.Command, `[ -n "${GIT_TOKEN:-}" ]`) {
			found = true
		}
	}
	if !found {
		t.Fatalf("git private-module auth command not issued; calls=%+v", rb.execCalls)
	}
}

func TestInjectAgentConfigGitAuthorIdentity(t *testing.T) {
	tests := []struct {
		name      string
		author    string
		email     string
		wantName  string
		wantEmail string
	}{
		{name: "defaults", wantName: "loom-spawn", wantEmail: "loom-spawn@loom.local"},
		{name: "overrides", author: "Mills Agent", email: "mills@example.com", wantName: "Mills Agent", wantEmail: "mills@example.com"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SPAWN_GIT_AUTHOR_NAME", tc.author)
			t.Setenv("SPAWN_GIT_AUTHOR_EMAIL", tc.email)
			rb := &recordingBackend{}
			if err := (&SpawnOrchestrator{}).injectAgentConfig(context.Background(), rb, "pod-1", "codex", "/workspace/services/loom-core"); err != nil {
				t.Fatalf("injectAgentConfig: %v", err)
			}
			commands := make([]string, 0, len(rb.execCalls))
			for _, call := range rb.execCalls {
				commands = append(commands, call.Command)
			}
			joined := strings.Join(commands, "\n")
			for _, want := range []string{
				"git config --global user.name " + shellQuote(tc.wantName),
				"git config --global user.email " + shellQuote(tc.wantEmail),
			} {
				if !strings.Contains(joined, want) {
					t.Errorf("commands do not contain %q: %s", want, joined)
				}
			}
		})
	}
}

func TestInjectAgentConfigGitAuthorIdentityErrorPropagates(t *testing.T) {
	rb := &recordingBackend{execErr: errors.New("exec failed")}
	err := (&SpawnOrchestrator{}).injectAgentConfig(context.Background(), rb, "pod-1", "codex", "/workspace/services/loom-core")
	if err == nil || !strings.Contains(err.Error(), "configure git author user.name") {
		t.Fatalf("error = %v, want author configuration error", err)
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
