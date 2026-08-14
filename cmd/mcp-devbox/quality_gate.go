package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"gitlab.flexinfer.ai/libs/mcp-go"

	"github.com/crb2nu/loom/internal/devbox/backend"
	"github.com/crb2nu/loom/internal/devbox/detect"
	"github.com/crb2nu/loom/pkg/poll"
	"github.com/crb2nu/loom/pkg/validate"
)

// qualityCheckResult holds the result of a single quality gate check.
//
// OutputTail is captured from the check's stdout. When stdout is empty
// (common for `make` errors and fallback commands that error to stderr),
// StderrTail surfaces the underlying failure so escalations are
// actionable instead of an unhelpful empty string.
type qualityCheckResult struct {
	Name       string `json:"name"`
	Passed     bool   `json:"passed"`
	ExitCode   int    `json:"exit_code,omitempty"`
	DurationMs int64  `json:"duration_ms"`
	OutputTail string `json:"output_tail,omitempty"`
	StderrTail string `json:"stderr_tail,omitempty"`
}

// qualityGateResult holds the aggregate result of the quality gate.
type qualityGateResult struct {
	Language        string               `json:"language"`
	Passed          bool                 `json:"passed"`
	Checks          []qualityCheckResult `json:"checks"`
	TotalDurationMs int64                `json:"total_duration_ms"`
}

// languageCommands maps language → check name → command.
var languageCommands = map[string]map[string]string{
	"go": {
		"fmt":  "gofmt -l .",
		"lint": "go vet ./...",
		"test": "go test ./...",
		"diff": "git diff --exit-code",
	},
	"python": {
		"fmt":  "black --check .",
		"lint": "ruff check .",
		"test": "pytest",
		"diff": "git diff --exit-code",
	},
	"node": {
		"fmt":  `npx prettier --check "src/**"`,
		"lint": "npx eslint src/",
		"test": "npm test",
		"diff": "git diff --exit-code",
	},
	"rust": {
		"fmt":  "cargo fmt --check",
		"lint": "cargo clippy -- -D warnings",
		"test": "cargo test",
		"diff": "git diff --exit-code",
	},
}

// fallbackCommands are Makefile-based fallbacks when no language is detected.
var fallbackCommands = map[string]string{
	"fmt":  "make fmt",
	"lint": "make lint",
	"test": "make test",
	"diff": "git diff --exit-code",
}

// sandboxLanguageProbeCommand inspects the cwd for the canonical
// marker file of each supported language. Trailing newlines keep
// k8s exec capture happy (no-newline output can race with stream
// close in some kubelet versions) and stays harmless under strings.TrimSpace.
const sandboxLanguageProbeCommand = `if [ -f go.mod ]; then echo go; elif [ -f package.json ]; then echo node; elif [ -f pyproject.toml ] || [ -f requirements.txt ]; then echo python; elif [ -f Cargo.toml ]; then echo rust; else echo unknown; fi`

// sandboxLanguageProbePaths is the ordered list of cwds the probe tries
// when the first probe returns unknown. In git-clone mode the sources
// land under projectWorkDir, but tar-pipe sandboxes that pre-date a
// syncMode flip and home-rolled mounts can leave the marker at the
// workspace root instead. The extra candidates are inert when missing.
var sandboxLanguageProbePaths = []string{"", "/workspace"}

func (m *manager) handleQualityGate(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	v := validate.NewArgs(args)
	project := v.Required("project")
	agentID := v.String("agent_id", "")
	failFast := v.Bool("fail_fast", true)
	if err := v.Validate(); err != nil {
		return mcp.ErrorResult(err), nil
	}

	// Parse command selectors and caller-supplied environment.
	checks := []string{"fmt", "lint", "test"}
	if checksRaw, ok := args["checks"]; ok {
		checks = stringSliceArg(checksRaw)
	}
	extraTestCommands := stringSliceArg(args["extra_test_commands"])
	execEnv := stringMapArg(args["env"])

	projectDir, projectName, err := m.resolveProject(project)
	if err != nil {
		return mcp.ErrorResult(err), nil
	}

	// Ensure sandbox is running. On the K8s backend a cold/stale sandbox
	// triggers an async image build that returns a "build in progress"
	// signal immediately (so quick exec calls aren't hung). The quality
	// gate, however, is a CI step with a multi-minute budget: returning the
	// build-in-progress error here surfaces to the operator's tests stage as
	// an opaque `0 checks` failure that it retries within milliseconds —
	// never giving the build time to finish. Await the build instead.
	key := storeKey(projectName, agentID)
	containerID, err := m.ensureRunningAwaitBuild(ctx, projectDir, projectName, agentID)
	if err != nil {
		return mcp.ErrorResult(fmt.Errorf("ensure sandbox: %w", err)), nil
	}

	// Re-sync workspace
	if err := m.syncIfNeeded(ctx, containerID, projectDir); err != nil {
		m.logger.Warn("pre-quality-gate sync failed", "project", projectName, "error", err)
	}

	_ = m.store.TouchLastUsed(key)
	m.incActiveExecs(key)
	defer m.decActiveExecs(key)

	// Detect language
	fp, err := detect.Fingerprint(projectDir)
	if err != nil {
		return mcp.ErrorResult(fmt.Errorf("fingerprint: %w", err)), nil
	}

	lang := "unknown"
	if len(fp.Languages) > 0 {
		lang = fp.Languages[0].Language
	}
	if lang == "unknown" {
		if detected := m.detectSandboxLanguage(ctx, containerID, projectDir); detected != "" {
			lang = detected
		}
	}

	// Look up commands
	cmds := languageCommands[lang]
	if cmds == nil {
		cmds = fallbackCommands
	}

	gateStart := time.Now()
	allPassed := true
	results := make([]qualityCheckResult, 0, len(checks)+len(extraTestCommands))
	stopped := false

	for _, check := range checks {
		cmd, ok := cmds[check]
		if !ok {
			cmd = fallbackCommands[check]
		}
		if cmd == "" {
			continue
		}

		cr := m.runQualityCheck(ctx, containerID, projectDir, projectName, lang, check, cmd, execEnv, 300)
		results = append(results, cr)
		if !cr.Passed {
			allPassed = false
		}
		if !cr.Passed && failFast {
			stopped = true
			break
		}
	}

	if !stopped {
		testEnv := extraTestEnv(execEnv)
		for index, cmd := range extraTestCommands {
			name := fmt.Sprintf("test:%d", index)
			cr := m.runQualityCheck(ctx, containerID, projectDir, projectName, lang, name, cmd, testEnv, 900)
			results = append(results, cr)
			if !cr.Passed {
				allPassed = false
				if failFast {
					break
				}
			}
		}
	}

	if len(results) == 0 {
		// No check executed: the checks list was empty or every requested
		// name resolved to no command. A verdict requires at least one
		// executed check — without this guard the gate reported a definite
		// passed=true from zero evidence, silently waving through a typo'd
		// checks selector. The explicit error also keeps the wire contract
		// honest for the Mills tests stage, which treats "not-passed with
		// zero checks" as an infrastructure contract violation.
		return mcp.ErrorResult(fmt.Errorf("quality gate executed no checks (requested %v, language %s); refusing to report a verdict", checks, lang)), nil
	}

	gateResult := qualityGateResult{
		Language:        lang,
		Passed:          allPassed,
		Checks:          results,
		TotalDurationMs: time.Since(gateStart).Milliseconds(),
	}

	m.logger.Info("quality gate", "project", projectName, "language", lang,
		"passed", allPassed, "duration_ms", gateResult.TotalDurationMs)

	if m.events != nil {
		m.events.Emit(ctx, "quality_gate", projectName,
			fmt.Sprintf("passed=%v language=%s duration=%dms", allPassed, lang, gateResult.TotalDurationMs))
	}

	return mcp.JSONResult(gateResult)
}

func extraTestEnv(base map[string]string) map[string]string {
	result := make(map[string]string, len(base)+8)
	for key, value := range base {
		result[key] = value
	}
	token := result["GIT_TOKEN"]
	if token == "" {
		return result
	}
	result["GOTOOLCHAIN"] = "auto"
	result["GIT_CONFIG_COUNT"] = "2"
	result["GIT_CONFIG_KEY_0"] = "url.https://token:" + token + "@gitlab.flexinfer.ai/.insteadOf"
	result["GIT_CONFIG_VALUE_0"] = "https://gitlab.flexinfer.ai/"
	result["GIT_CONFIG_KEY_1"] = "safe.directory"
	result["GIT_CONFIG_VALUE_1"] = "*"
	result["GOPRIVATE"] = "gitlab.flexinfer.ai/*"
	result["GOWORK"] = "off"
	result["CGO_ENABLED"] = "0"
	return result
}

func (m *manager) runQualityCheck(ctx context.Context, containerID, projectDir, projectName, lang, name, cmd string, execEnv map[string]string, timeoutSec int) qualityCheckResult {
	checkStart := time.Now()
	var result *backend.ExecResult
	execFn := func(ctx context.Context) error {
		var execErr error
		result, execErr = m.backend.Exec(ctx, backend.ExecOpts{
			ContainerID: containerID,
			Command:     cmd,
			WorkDir:     m.projectWorkDir(projectDir),
			Env:         execEnv,
			TimeoutSec:  timeoutSec,
			MaxLines:    50,
		})
		return execErr
	}
	err := poll.RetryWithBackoff(ctx, 2, time.Second, 4*time.Second, execFn)
	cr := qualityCheckResult{Name: name, DurationMs: time.Since(checkStart).Milliseconds()}
	if err != nil {
		msg := strings.TrimSpace(err.Error())
		if msg == "" {
			msg = fmt.Sprintf("exec failed (%T) for `%s`", err, cmd)
		} else {
			msg = fmt.Sprintf("%s: %s", name, msg)
		}
		cr.OutputTail = msg
		if result != nil && result.StderrTail != "" {
			cr.StderrTail = truncateOutput(result.StderrTail, 500)
		}
	} else {
		cr.ExitCode = result.ExitCode
		cr.Passed = result.ExitCode == 0
		if !cr.Passed {
			cr.OutputTail = truncateOutput(result.StdoutTail, 500)
			cr.StderrTail = truncateOutput(result.StderrTail, 500)
			if cr.OutputTail == "" {
				cr.OutputTail = cr.StderrTail
			}
			if cr.OutputTail == "" {
				cr.OutputTail = fmt.Sprintf("%s exited %d (no output)", name, result.ExitCode)
			}
		}
	}
	cr.OutputTail = redactQualitySecrets(cr.OutputTail, execEnv)
	cr.StderrTail = redactQualitySecrets(cr.StderrTail, execEnv)
	if m.logger != nil {
		m.logger.Info("quality gate check", "project", projectName, "check", name,
			"language", lang, "cmd", cmd, "passed", cr.Passed, "exit", cr.ExitCode,
			"duration_ms", cr.DurationMs, "stdout_tail", truncateOutput(cr.OutputTail, 240),
			"stderr_tail", truncateOutput(cr.StderrTail, 240))
	}
	return cr
}

func redactQualitySecrets(text string, execEnv map[string]string) string {
	if token := execEnv["GIT_TOKEN"]; token != "" {
		return strings.ReplaceAll(text, token, "[REDACTED]")
	}
	return text
}

// ensureRunningAwaitBuild calls ensureRunning, and when the K8s async
// builder reports the sandbox image is still building, polls (with bounded
// backoff) until the build completes instead of bubbling up an immediate
// "build in progress" error.
//
// Rationale: the quality gate is a CI step with a multi-minute budget. The
// async build exists so quick exec calls don't hang, but for the gate the
// build-in-progress signal otherwise reaches the Mills operator as an opaque
// `devbox quality gate failed: 0 checks` that it retries within milliseconds
// — exhausting its attempts long before a cold build (go mod download, apk
// installs, image push) can finish. Awaiting the build here lets the very
// first gate call run real checks. The per-iteration lock acquire/release
// keeps other project operations unblocked during the wait, and a context
// cancellation or the maxBuildWait ceiling still bounds the call.
// maxBuildWait bounds how long the quality gate blocks awaiting a cold
// sandbox image build before giving up; initialBuildBackoff is the first
// poll interval (it grows to maxBuildBackoff).
const (
	maxBuildWait        = 8 * time.Minute
	initialBuildBackoff = 3 * time.Second
	maxBuildBackoff     = 15 * time.Second
)

func (m *manager) ensureRunningAwaitBuild(ctx context.Context, projectDir, projectName, agentID string) (string, error) {
	key := storeKey(projectName, agentID)
	return awaitSandboxBuild(ctx, maxBuildWait, initialBuildBackoff, func() (string, error) {
		mu := m.projectLock(key)
		mu.Lock()
		defer mu.Unlock()
		return m.ensureRunning(ctx, projectDir, projectName, agentID)
	})
}

// awaitSandboxBuild repeatedly invokes ensure until it returns success, a
// non-build-in-progress error, the context is cancelled, or maxWait elapses.
// A buildInProgressError is the async builder's "retry shortly" signal, so it
// is polled (with bounded exponential backoff) rather than surfaced. Extracted
// from ensureRunningAwaitBuild so the wait/backoff/timeout logic is unit
// testable without a full sandbox backend.
func awaitSandboxBuild(ctx context.Context, maxWait, initialBackoff time.Duration, ensure func() (string, error)) (string, error) {
	deadline := time.Now().Add(maxWait)
	backoff := initialBackoff
	for {
		id, err := ensure()
		if err == nil {
			return id, nil
		}
		if _, building := asBuildInProgress(err); !building {
			return "", err
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("sandbox image still building after %s: %w", maxWait, err)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < maxBuildBackoff {
			backoff += initialBackoff
			if backoff > maxBuildBackoff {
				backoff = maxBuildBackoff
			}
		}
	}
}

// detectSandboxLanguage probes the running sandbox for a language marker.
//
// It tries projectWorkDir first (where git-clone deposits source) and
// falls back to /workspace so tar-pipe-era sandboxes and any layout
// drift still resolve. Every attempt is logged with stdout/stderr/exit
// so empty results don't disappear into the void.
func (m *manager) detectSandboxLanguage(ctx context.Context, containerID, projectDir string) string {
	project := filepath.Base(projectDir)
	defaultWorkDir := m.projectWorkDir(projectDir)
	for _, wd := range sandboxLanguageProbePaths {
		if wd == "" {
			wd = defaultWorkDir
		}
		result, err := m.backend.Exec(ctx, backend.ExecOpts{
			ContainerID: containerID,
			Command:     sandboxLanguageProbeCommand,
			WorkDir:     wd,
			TimeoutSec:  10,
			MaxLines:    1,
		})
		if m.logger != nil {
			exit := -1
			var stdout, stderr string
			if result != nil {
				exit = result.ExitCode
				stdout = strings.TrimSpace(result.StdoutTail)
				stderr = strings.TrimSpace(result.StderrTail)
			}
			errStr := ""
			if err != nil {
				errStr = err.Error()
			}
			m.logger.Info("sandbox language probe",
				"project", project,
				"workdir", wd,
				"exit", exit,
				"stdout", stdout,
				"stderr", stderr,
				"error", errStr,
			)
		}
		if err != nil || result == nil || result.ExitCode != 0 {
			continue
		}
		switch strings.TrimSpace(result.StdoutTail) {
		case "go", "python", "node", "rust":
			return strings.TrimSpace(result.StdoutTail)
		}
	}
	return ""
}

// truncateOutput returns the last N bytes of output.
func truncateOutput(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	// Find a newline boundary near the cut point
	cut := s[len(s)-maxBytes:]
	if idx := strings.Index(cut, "\n"); idx > 0 {
		return "..." + cut[idx:]
	}
	return "..." + cut
}
