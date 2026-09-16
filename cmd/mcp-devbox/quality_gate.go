package main

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"gitlab.flexinfer.ai/libs/mcp-go"

	"github.com/crb2nu/loom/internal/devbox/backend"
	"github.com/crb2nu/loom/pkg/mills/gates"
	"github.com/crb2nu/loom/pkg/poll"
	telemetryredact "github.com/crb2nu/loom/pkg/telemetry/redact"
	"github.com/crb2nu/loom/pkg/validate"
)

// qualityCheckResult holds the result of a single quality gate check.
//
// OutputTail contains bounded, redacted stdout and stderr diagnostics.
type qualityCheckResult struct {
	Name             string `json:"name"`
	Passed           bool   `json:"passed"`
	ExitCode         int    `json:"exit_code"`
	DurationMs       int64  `json:"duration_ms"`
	OutputTail       string `json:"output_tail"`
	StderrTail       string `json:"stderr_tail"`
	Count1Injected   *bool  `json:"count1_injected,omitempty"`
	Degraded         bool   `json:"degraded,omitempty"`
	FailureSignature string `json:"failure_signature,omitempty"`
	Warning          string `json:"warning,omitempty"`
}

// qualityGateResult holds the aggregate result of the quality gate.
type qualityGateResult struct {
	Language        string               `json:"language"`
	Passed          bool                 `json:"passed"`
	Checks          []qualityCheckResult `json:"checks"`
	TotalDurationMs int64                `json:"total_duration_ms"`
	TestedSHA       string               `json:"tested_sha,omitempty"`
}

var (
	gitBranchPattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)
	gitSHAPattern        = regexp.MustCompile(`^[0-9a-fA-F]{40,64}$`)
	envAssignmentPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)
)

// languageCommands maps language → check name → command.
var languageCommands = map[string]map[string]string{
	"go": {
		"fmt":  "gofmt -l .",
		"lint": "go vet ./...",
		"test": "go test -count=1 ./...",
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
	rawEnv := stringMapArg(args["env"])
	branch := rawEnv["LOOM_MILLS_BRANCH"]
	expectedSHA := rawEnv["LOOM_MILLS_EXPECTED_SHA"]
	checkoutRequested := branch != "" || expectedSHA != ""
	if rawEnv["LOOM_MILLS_RUN_ID"] != "" && !checkoutRequested {
		return mcp.ErrorResult(fmt.Errorf("tests checkout infrastructure: mills run requested an unpinned gate")), nil
	}
	if checkoutRequested && (!gitBranchPattern.MatchString(branch) || !gitSHAPattern.MatchString(expectedSHA)) {
		return mcp.ErrorResult(fmt.Errorf("tests checkout infrastructure: invalid branch or expected sha")), nil
	}
	execEnv := allowedQualityGateEnv(rawEnv)

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
	// Hold the lifecycle lock until both the handler and cancellation cleanup
	// finish. No same-name replacement can be created under stale cleanup.
	mu := m.projectLock(key)
	containerID, err := awaitSandboxBuild(ctx, maxBuildWait, initialBuildBackoff, func() (string, error) {
		if err := m.lockGateLifecycle(ctx, key, false); err != nil {
			return "", err
		}
		if failure, ok := m.gateCleanupFailures.Load(key); ok {
			mu.Unlock()
			return "", fmt.Errorf("previous gate termination unconfirmed; devbox_stop required: %v", failure)
		}
		id, err := m.ensureRunning(ctx, projectDir, projectName, agentID)
		if err != nil {
			mu.Unlock()
		}
		return id, err
	})
	if err != nil {
		return mcp.ErrorResult(fmt.Errorf("ensure sandbox: %w", err)), nil
	}

	defer mu.Unlock()
	ctx, cancelGate := context.WithCancel(ctx)
	m.gateCancels.Store(key, cancelGate)
	defer m.gateCancels.Delete(key)
	defer cancelGate()
	finished := make(chan struct{})
	cleaned := make(chan struct{})
	go func() {
		defer close(cleaned)
		select {
		case <-finished:
			if ctx.Err() == nil {
				return
			}
		case <-ctx.Done():
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		m.logger.Info("shutting down sandbox", "key", key, "reason", "quality gate cancelled")
		if err := m.backend.Stop(cleanupCtx, containerID); err != nil {
			m.gateCleanupFailures.Store(key, err)
			m.logger.Error("cancelled quality gate cleanup failed", "key", key, "error", err)
		} else if entry := m.store.Get(key); entry != nil {
			entry.Status = "stopped"
			_ = m.store.Set(key, entry)
		}
	}()
	defer func() { close(finished); <-cleaned }()

	// Re-sync workspace
	if err := m.syncIfNeeded(ctx, containerID, projectDir); err != nil {
		m.logger.Warn("pre-quality-gate sync failed", "project", projectName, "error", err)
	}

	_ = m.store.TouchLastUsed(key)
	m.incActiveExecs(key)
	defer m.decActiveExecs(key)

	testedSHA := ""
	gateWorkDir := m.projectWorkDir(projectDir)
	if checkoutRequested {
		testedSHA, gateWorkDir, err = m.checkoutTestRevision(ctx, containerID, projectDir, branch, expectedSHA, execEnv)
		if err != nil {
			return mcp.ErrorResult(err), nil
		}
	}

	// Detect language (git-clone mode hydrates from the git host, so the
	// sandbox probe below is only the last resort)
	fp, err := m.fingerprintProject(ctx, projectDir)
	if err != nil {
		return mcp.ErrorResult(fmt.Errorf("fingerprint: %w", err)), nil
	}

	execEnv = qualityGateMemoryEnv(execEnv, m.sandboxMemoryMB(fp))

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

	if lang == "go" {
		m.prepareQualityGate(ctx, containerID, gateWorkDir, sharedGoCacheWarmup, execEnv, 285)
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

		cr := m.runQualityCheck(ctx, containerID, gateWorkDir, projectName, lang, check, cmd, execEnv, m.gateCheckTimeout())
		results = append(results, cr)
		if !cr.Passed {
			allPassed = false
		}
		if ctx.Err() != nil || (!cr.Passed && failFast) {
			stopped = true
			break
		}
	}

	if !stopped {
		testEnv := extraTestEnv(execEnv)
		for index, cmd := range extraTestCommands {
			var count1Injected bool
			cmd, count1Injected = freshGoTestCommand(cmd)
			name := fmt.Sprintf("test:%d", index)
			cr := m.runQualityCheck(ctx, containerID, gateWorkDir, projectName, lang, name, cmd, testEnv, m.gateTestTimeout())
			cr.Count1Injected = &count1Injected
			// Substring, not prefix: the operator-generated command has grown
			// env assignments over time (CGO_ENABLED=0 GOWORK=off …), and a
			// version skew between operator and devbox must not silently
			// demote the parity check back to an anonymous test:N slot.
			if ctx.Err() == nil && strings.Contains(cmd, "golangci-lint run --config .golangci.yml ") {
				cr.Name = gates.LintParityCheckName
				if cr.Degraded && m.events != nil {
					m.events.Emit(ctx, "quality_gate_warning", projectName, cr.Warning)
				}
			}
			results = append(results, cr)
			if !cr.Passed {
				allPassed = false
				if failFast || ctx.Err() != nil {
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
		TestedSHA:       testedSHA,
	}

	m.logger.Info("quality gate", "project", projectName, "language", lang,
		"passed", allPassed, "duration_ms", gateResult.TotalDurationMs)

	if m.events != nil {
		m.events.Emit(ctx, "quality_gate", projectName,
			fmt.Sprintf("passed=%v language=%s duration=%dms", allPassed, lang, gateResult.TotalDurationMs))
	}

	return mcp.JSONResult(gateResult)
}

func allowedQualityGateEnv(input map[string]string) map[string]string {
	result := make(map[string]string, 3)
	for _, key := range []string{"GIT_TOKEN", "GOPRIVATE", "LOOM_MILLS_RUN_ID"} {
		if value := input[key]; value != "" {
			result[key] = value
		}
	}
	return result
}

func (m *manager) checkoutTestRevision(ctx context.Context, containerID, projectDir, branch, expectedSHA string, execEnv map[string]string) (string, string, error) {
	rawRunID := strings.TrimSpace(execEnv["LOOM_MILLS_RUN_ID"])
	if rawRunID == "" {
		return "", "", fmt.Errorf("tests checkout infrastructure: run id missing")
	}
	sourceDir := m.projectWorkDir(projectDir)
	workDir := m.testRevisionWorkDir(projectDir, rawRunID)
	m.prepareQualityGate(ctx, containerID, workDir, staleTestsIndexLockCleanup, execEnv, 15)
	if err := ctx.Err(); err != nil {
		return "", "", err
	}
	cmd := fmt.Sprintf("if [ ! -d %s/.git ]; then git clone --shared --no-checkout %s %s && git -C %s remote set-url origin \"$(git -C %s remote get-url origin)\"; fi && git -C %s fetch --no-tags origin %s && git -C %s checkout --detach %s && git -C %s reset --hard %s && git -C %s clean -fdx && git -C %s rev-parse HEAD",
		workDir, sourceDir, workDir, workDir, sourceDir, workDir, branch, workDir, expectedSHA, workDir, expectedSHA, workDir, workDir)
	result, err := m.backend.Exec(ctx, backend.ExecOpts{
		ContainerID: containerID,
		Command:     cmd,
		WorkDir:     sourceDir,
		Env:         extraTestEnv(execEnv),
		TimeoutSec:  900,
		MaxLines:    50,
	})
	if err != nil || result == nil || result.ExitCode != 0 {
		detail := "backend returned no result"
		if err != nil {
			detail = err.Error()
		} else if result != nil {
			detail = fmt.Sprintf("exit %d: %s", result.ExitCode, strings.TrimSpace(result.StderrTail))
		}
		return "", "", fmt.Errorf("tests checkout infrastructure: fetch/checkout branch %q at %s failed: %s", branch, expectedSHA, detail)
	}
	lines := strings.Fields(result.StdoutTail)
	if len(lines) == 0 {
		return "", "", fmt.Errorf("tests checkout infrastructure: tested sha missing after checkout")
	}
	testedSHA := lines[len(lines)-1]
	if !strings.EqualFold(testedSHA, expectedSHA) {
		return testedSHA, workDir, fmt.Errorf("tests checkout infrastructure: tested sha mismatch: got %q want %q", testedSHA, expectedSHA)
	}
	return testedSHA, workDir, nil
}

// Only explicit pod-local workspace modes can share a path across run IDs.
// NFS and host-mounted workspaces still need an independent checkout per run.
func (m *manager) testRevisionWorkDir(projectDir, runID string) string {
	workDir := m.projectWorkDir(projectDir) + ".mills-tests"
	// Match K8s workspacePlan: git hydration requires both URL and secret.
	gitWorkspace := m.cfg.gitBaseURL != "" && m.cfg.gitSecret != ""
	if m.cfg.backendType == "k8s" && (gitWorkspace || m.cfg.syncMode == "tar-pipe") {
		return workDir
	}
	return workDir + "-" + sanitizeContainerName(runID)
}

// Reserve at least one sixth of the sandbox for non-Go allocations and other
// processes (1 GiB at 6144 MiB). This is a per-process soft GC limit, not a
// combined memory cap for a process tree.
func qualityGateMemoryEnv(base map[string]string, memoryMB int) map[string]string {
	result := make(map[string]string, len(base)+1)
	for key, value := range base {
		result[key] = value
	}
	if memoryMB > 0 {
		budget := memoryMB - (memoryMB+5)/6
		if budget > 0 {
			result["GOMEMLIMIT"] = fmt.Sprintf("%dMiB", budget)
		}
	}
	return result
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
	if result["GOPRIVATE"] == "" {
		result["GOPRIVATE"] = "gitlab.flexinfer.ai/*"
	}
	result["GOWORK"] = "off"
	result["CGO_ENABLED"] = "0"
	return result
}

func (m *manager) runQualityCheck(ctx context.Context, containerID, workDir, projectName, lang, name, cmd string, execEnv map[string]string, timeoutSec int) qualityCheckResult {
	checkStart := time.Now()
	var result *backend.ExecResult
	var backendErr error
	execFn := func(ctx context.Context) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		result, backendErr = m.backend.Exec(ctx, backend.ExecOpts{
			ContainerID: containerID,
			Command:     cmd,
			WorkDir:     workDir,
			Env:         execEnv,
			TimeoutSec:  timeoutSec,
			MaxLines:    50,
		})
		// An execution result carries the command's authoritative verdict.
		// Backends may also report cleanup or transport noise after collecting
		// that result; preserve it in output_tail without overriding ExitCode.
		if result != nil {
			return nil
		}
		return backendErr
	}
	err := poll.RetryWithBackoff(ctx, 2, time.Second, 4*time.Second, execFn)
	cr := qualityCheckResult{Name: name, DurationMs: time.Since(checkStart).Milliseconds()}
	if ctx.Err() != nil {
		cr.ExitCode = 130
		cr.OutputTail = name + ": cancelled: " + ctx.Err().Error()
	} else if result == nil {
		msg := ""
		if err != nil {
			msg = strings.TrimSpace(err.Error())
		}
		if msg == "" {
			msg = fmt.Sprintf("exec failed (%T) for `%s`", err, cmd)
		} else {
			msg = fmt.Sprintf("%s: %s", name, msg)
		}
		cr.OutputTail = msg
	} else {
		cr.ExitCode = result.ExitCode
		cr.Passed = result.ExitCode == 0
		cr.OutputTail = strings.TrimSpace(result.StdoutTail)
		cr.StderrTail = strings.TrimSpace(result.StderrTail)
		if cr.StderrTail != "" {
			cr.OutputTail = strings.TrimSpace(cr.OutputTail + "\n" + cr.StderrTail)
		}
		// Classify actual process output before headers or fallback diagnostics.
		if strings.Contains(cmd, "golangci-lint run --config .golangci.yml ") {
			verdict := gates.ClassifyLintParity(cr.ExitCode, cr.OutputTail)
			cr.Passed, cr.Degraded, cr.Warning = verdict.Passed, verdict.Degraded, verdict.Warning
			cr.FailureSignature = verdict.FailureSignature
			if verdict.Degraded {
				cr.OutputTail = verdict.Output
			}
		}
		if backendErr != nil {
			diagnostic := fmt.Sprintf("%s exited %d; backend diagnostic: %v", name, result.ExitCode, backendErr)
			if cr.OutputTail == "" {
				cr.OutputTail = diagnostic
			} else {
				cr.OutputTail += "\n" + diagnostic
			}
		}
		if cr.OutputTail == "" {
			cr.OutputTail = fmt.Sprintf("%s exited %d (no output)", name, result.ExitCode)
		}
		// A timed-out check is ambiguous: slow tests or a starved container.
		// Read the sandbox cgroup's throttling so the tail says which, and the
		// classifier and the human can tell substrate from code.
		if result.ExitCode == execTimeoutExitCode {
			detail := fmt.Sprintf("%s timed out after %ds", name, timeoutSec)
			if throttle := m.cpuThrottleSummary(ctx, containerID); throttle != "" {
				detail += "; " + throttle
			}
			cr.OutputTail = detail + "\n" + cr.OutputTail
		}
	}
	commandHeader := redactQualitySecrets("$ "+cmd+"\n", execEnv)
	output := redactQualitySecrets(cr.OutputTail, execEnv)
	limit := 500
	if strings.Contains(cmd, "golangci-lint run --config .golangci.yml ") {
		limit = 8 * 1024
		commandHeader = qualityDiagnosticTail(commandHeader, limit/2)
	}
	if remaining := limit - len(commandHeader); remaining > 0 {
		cr.OutputTail = commandHeader + qualityDiagnosticTail(output, remaining)
	} else {
		cr.OutputTail = truncateOutput(commandHeader, 500)
	}
	cr.StderrTail = qualityDiagnosticTail(redactQualitySecrets(cr.StderrTail, execEnv), 500)
	if m.logger != nil {
		m.logger.Info("quality gate check", "project", projectName, "check", name,
			"language", lang, "cmd", cmd, "passed", cr.Passed, "exit", cr.ExitCode,
			"duration_ms", cr.DurationMs, "stdout_tail", truncateOutput(cr.OutputTail, 240),
			"stderr_tail", truncateOutput(cr.StderrTail, 240))
	}
	return cr
}

const (
	// execTimeoutExitCode is what the backends report when a command hits its
	// TimeoutSec budget (the coreutils `timeout` convention).
	execTimeoutExitCode = 124
	// Compiled-in gate budgets, overridable through managerConfig.
	defaultGateCheckTimeoutSec = 300
	defaultGateTestTimeoutSec  = 900
)

// gateCheckTimeout is the per-check budget for the fmt/lint style checks.
func (m *manager) gateCheckTimeout() int {
	if m.cfg.gateCheckTimeoutSec > 0 {
		return m.cfg.gateCheckTimeoutSec
	}
	return defaultGateCheckTimeoutSec
}

// gateTestTimeout is the per-command budget for the operator's test commands.
func (m *manager) gateTestTimeout() int {
	if m.cfg.gateTestTimeoutSec > 0 {
		return m.cfg.gateTestTimeoutSec
	}
	return defaultGateTestTimeoutSec
}

// cpuThrottleSummary reads the sandbox's cgroup v2 cpu.stat and renders the
// CPU throttling it has seen, e.g. "cpu throttled 39% of periods (536/1385,
// 55s)". Empty when the file is unavailable (cgroup v1, docker desktop) or the
// container has not been throttled at all.
func (m *manager) cpuThrottleSummary(ctx context.Context, containerID string) string {
	res, err := m.backend.Exec(ctx, backend.ExecOpts{
		ContainerID: containerID,
		Command:     "cat /sys/fs/cgroup/cpu.stat",
		TimeoutSec:  10,
		MaxLines:    12,
	})
	if err != nil || res == nil || res.ExitCode != 0 {
		return ""
	}
	return parseCPUThrottle(res.StdoutTail)
}

// parseCPUThrottle renders cgroup v2 cpu.stat counters as one phrase.
func parseCPUThrottle(stat string) string {
	var periods, throttled, throttledUsec int64
	for _, line := range strings.Split(stat, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		var v int64
		if _, err := fmt.Sscanf(fields[1], "%d", &v); err != nil {
			continue
		}
		switch fields[0] {
		case "nr_periods":
			periods = v
		case "nr_throttled":
			throttled = v
		case "throttled_usec":
			throttledUsec = v
		}
	}
	if periods <= 0 || throttled <= 0 {
		return ""
	}
	return fmt.Sprintf("cpu throttled %d%% of periods (%d/%d, %ds)", throttled*100/periods, throttled, periods, throttledUsec/1_000_000)
}

func freshGoTestCommand(command string) (string, bool) {
	// Command chains and quoting require a real shell parser to rewrite safely.
	// Keep those commands untouched and surface the skipped injection in the
	// check artifact instead.
	if strings.Contains(command, "&&") || strings.ContainsAny(command, ";|'\"") {
		return command, false
	}

	fields := strings.Fields(command)
	index := 0
	if index < len(fields) && fields[index] == "env" {
		index++
	}
	for index < len(fields) && envAssignmentPattern.MatchString(fields[index]) {
		index++
	}
	if index+1 >= len(fields) || fields[index] != "go" || fields[index+1] != "test" {
		return command, false
	}
	for _, field := range fields[index+2:] {
		if strings.HasPrefix(field, "-count=") || field == "-count" {
			return command, false
		}
	}

	fields = append(fields, "")
	copy(fields[index+3:], fields[index+2:])
	fields[index+2] = "-count=1"
	return strings.Join(fields, " "), true
}

func redactQualitySecrets(text string, execEnv map[string]string) string {
	text = telemetryredact.MaskSecrets(text)
	for key, value := range execEnv {
		upperKey := strings.ToUpper(key)
		isSecret := strings.Contains(upperKey, "TOKEN") || strings.Contains(upperKey, "SECRET") ||
			strings.Contains(upperKey, "PASSWORD") || strings.Contains(upperKey, "API_KEY") ||
			strings.Contains(upperKey, "CREDENTIAL")
		if isSecret && value != "" {
			text = strings.ReplaceAll(text, value, telemetryredact.RedactionMarker)
		}
	}
	return text
}

// handleQualityGate awaits the sandbox build through awaitSandboxBuild: when
// the K8s async builder reports the sandbox image is still building, it polls
// (with bounded backoff) until the build completes instead of bubbling up an
// immediate "build in progress" error.
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

// awaitSandboxBuild repeatedly invokes ensure until it returns success, a
// non-build-in-progress error, the context is cancelled, or maxWait elapses.
// A buildInProgressError is the async builder's "retry shortly" signal, so it
// is polled (with bounded exponential backoff) rather than surfaced. Kept
// separate from handleQualityGate so the wait/backoff/timeout logic is unit
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
		if ctx.Err() != nil {
			return ""
		}
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

// truncateOutput returns at most the last maxBytes of output without splitting
// a UTF-8 sequence. Truncated values retain an ellipsis within the byte bound.
func truncateOutput(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(s) <= maxBytes {
		return s
	}
	const marker = "..."
	if maxBytes <= len(marker) {
		return marker[:maxBytes]
	}
	start := len(s) - (maxBytes - len(marker))
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	return marker + s[start:]
}

// sharedGoCacheWarmup runs inside the sandbox, where the PVC and the checkout
// are visible. The Go sandbox image is Alpine + busybox with no python3 (the
// python3 heredoc this replaced exited 127 on every gate from 2026-09-14
// 04:29Z and never serialized a download), so the script uses only POSIX sh,
// awk and flock. Missing tooling degrades to a logged no-op; it must never
// start an unlocked download.
//
// Flow: skip unless GOMODCACHE is a direct child of /gocache and go.mod
// exists; parse `go mod edit -print` (local manifest only, no graph
// resolution or network) into the module-cache directories the build list
// needs, honouring replace directives and skipping local replacements; when
// every directory holds a go.mod there is nothing to do. Otherwise take an
// exclusive flock on /gocache/.warmup.lock (never unlinked: every pod must
// lock the same inode), re-check, and run `go mod download` while holding the
// descriptor — the child inherits it, so cancellation cannot release the lock
// while a surviving download still writes to the cache.
//
// The unit tests rewrite the literal `/gocache` and `lock_ticks=300`.
const sharedGoCacheWarmup = `warmup() {
cache="${GOMODCACHE:-/gocache/gomod}"
[ "${cache%/*}" = "/gocache" ] || return 0
[ -f go.mod ] || return 0
command -v flock >/dev/null 2>&1 || { echo "shared Go cache warm-up unavailable; proceeding to gate: flock missing"; return 0; }
manifest="$(GOTOOLCHAIN=local GOWORK=off go mod edit -print 2>&1)" || { echo "shared Go cache warm-up unavailable; proceeding to gate: go mod edit: $manifest"; return 0; }
complete() {
printf '%s\n' "$manifest" | awk '
function esc(s,  out, i, c) {
	out = ""
	for (i = 1; i <= length(s); i++) {
		c = substr(s, i, 1)
		if (c ~ /[A-Z]/) out = out "!" tolower(c)
		else out = out c
	}
	return out
}
function rep(i,  old, oldver, new, newver) {
	old = $i; i++; oldver = ""
	if (i <= NF && $i != "=>") { oldver = $i; i++ }
	if (i > NF || $i != "=>") return
	i++; new = $i; i++; newver = ""
	if (i <= NF && $i ~ /^v[0-9]/) newver = $i
	repnew[old " " oldver] = new " " newver
}
{
	sub(/\/\/.*/, "")
	if (NF == 0) next
	if ($1 == "require" && $2 == "(") { block = "require"; next }
	if ($1 == "replace" && $2 == "(") { block = "replace"; next }
	if ($1 == ")") { block = ""; next }
	if ($1 == "require" && NF >= 3) { req[nreq++] = $2 " " $3; next }
	if ($1 == "replace") { rep(2); next }
	if (block == "require" && NF >= 2) { req[nreq++] = $1 " " $2; next }
	if (block == "replace") rep(1)
}
END {
	for (i = 0; i < nreq; i++) {
		split(req[i], r, " ")
		path = r[1]; ver = r[2]
		if ((path " " ver) in repnew) { split(repnew[path " " ver], n, " "); path = n[1]; ver = n[2] }
		else if ((path " ") in repnew) { split(repnew[path " "], n, " "); path = n[1]; ver = n[2] }
		if (ver == "") continue
		print esc(path) "@" esc(ver)
	}
}' | while IFS= read -r dir; do
[ -d "$cache/$dir" ] && [ -f "$cache/$dir/go.mod" ] || exit 1
done
}
complete && return 0
exec 9>>"${cache%/*}/.warmup.lock" || { echo "shared Go cache warm-up unavailable; proceeding to gate: cannot open .warmup.lock"; return 0; }
lock_ticks=300
until flock -n 9; do
lock_ticks=$((lock_ticks - 1))
if [ "$lock_ticks" -le 0 ]; then echo "shared Go cache warm-up lock timeout; proceeding to gate"; return 0; fi
sleep 0.1
done
complete && return 0
echo "shared Go cache warm-up: go mod download under .warmup.lock"
GOWORK=off go mod download || echo "shared Go cache warm-up unavailable; proceeding to gate: go mod download exit $?"
}
warmup
`

// staleTestsIndexLockCleanup is deliberately conservative. /proc only sees this
// sandbox's processes, so this relies on the existing per-run checkout ownership
// (one sandbox per run), not a claim of cross-pod process visibility. Any git
// process, open lock descriptor, or unreadable process information preserves it.
const staleTestsIndexLockCleanup = `python3 - <<'LOOM_INDEX_LOCK'
import os, pathlib, time

def cleanup():
    checkout = pathlib.Path.cwd()
    if not (checkout.name.endswith('.mills-tests') or '.mills-tests-' in checkout.name):
        return
    lock = checkout / '.git' / 'index.lock'
    try:
        before = lock.lstat()
    except FileNotFoundError:
        return
    if lock.is_symlink() or time.time() - before.st_mtime <= 60:
        return
    proc = pathlib.Path('/proc')
    if not (proc / 'self' / 'fd').is_dir():
        return
    for process in proc.iterdir():
        if not process.name.isdigit():
            continue
        try:
            comm = (process / 'comm').read_text().strip()
            if comm == 'git' or comm.startswith('git-'):
                return
            for fd in (process / 'fd').iterdir():
                try:
                    if os.path.samefile(fd, lock):
                        return
                except FileNotFoundError:
                    continue
        except FileNotFoundError:
            continue  # exited during inspection
        except (PermissionError, OSError):
            print('preserving tests index.lock: process ownership unknown', flush=True)
            return
    try:
        after = lock.lstat()
        if (before.st_dev, before.st_ino, before.st_mtime_ns) != (after.st_dev, after.st_ino, after.st_mtime_ns):
            return
        lock.unlink()
        print('removed stale tests checkout .git/index.lock', flush=True)
    except FileNotFoundError:
        pass
try:
    cleanup()
except Exception as error:
    print('preserving tests index.lock: ' + str(error), flush=True)
LOOM_INDEX_LOCK
`

func (m *manager) prepareQualityGate(ctx context.Context, containerID, workDir, command string, execEnv map[string]string, timeout int) {
	if ctx.Err() != nil {
		return
	}
	result, err := m.backend.Exec(ctx, backend.ExecOpts{
		ContainerID: containerID, WorkDir: workDir, Command: command,
		Env: extraTestEnv(execEnv), TimeoutSec: timeout, MaxLines: 50,
	})
	if m.logger != nil {
		detail := ""
		if result != nil {
			detail = result.StdoutTail + "\n" + result.StderrTail
		}
		if err != nil {
			detail += "\n" + err.Error()
		}
		m.logger.Info("quality gate preparation", "workdir", workDir,
			"diagnostic", truncateOutput(redactQualitySecrets(detail, execEnv), 2000))
	}
}

// Keep the diagnostic line even when subsequent build output fills the tail.
func qualityDiagnosticTail(output string, limit int) string {
	if len(output) <= limit {
		return output
	}
	for _, line := range strings.Split(output, "\n") {
		if (strings.Contains(line, "/gocache/gomod/") && strings.Contains(line, "no such file or directory")) ||
			(strings.Contains(line, ".mills-tests") && strings.Contains(line, "index.lock") && strings.Contains(strings.ToLower(line), "file exists")) {
			diagnostic := truncateOutput(line, limit)
			if remaining := limit - len(diagnostic) - 1; remaining > 0 && len(output) > limit {
				return diagnostic + "\n" + truncateOutput(output, remaining)
			}
			return diagnostic
		}
	}
	return truncateOutput(output, limit)
}

// lockGateLifecycle cancels a gate before waiting for its lifecycle lock.
// Polling closes the registration race and keeps stop bounded by its caller.
func (m *manager) lockGateLifecycle(ctx context.Context, key string, stop bool) error {
	mu := m.projectLock(key)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if stop {
			if cancel, ok := m.gateCancels.Load(key); ok {
				cancel.(context.CancelFunc)()
			}
		}
		if mu.TryLock() {
			return nil
		}
		if err := poll.WaitWithContext(ctx, 10*time.Millisecond); err != nil {
			return err
		}
	}
}
