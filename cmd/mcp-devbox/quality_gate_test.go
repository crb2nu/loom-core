package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	mcp "gitlab.flexinfer.ai/libs/mcp-go"

	"github.com/crb2nu/loom/internal/devbox/backend"
	"github.com/crb2nu/loom/internal/devbox/detect"
	"github.com/crb2nu/loom/internal/devbox/state"
)

// qgFakeBackend is a fake backend that returns configurable exit codes per command.
type qgFakeBackend struct {
	fakeBackend
	commandResults   map[string]*backend.ExecResult
	calls            []backend.ExecOpts
	preparationCalls []backend.ExecOpts
	allCalls         []backend.ExecOpts
}

type qgScriptedBackend struct {
	fakeBackend
	results []*backend.ExecResult
	errors  []error
	calls   int
}

func (b *qgScriptedBackend) Exec(_ context.Context, _ backend.ExecOpts) (*backend.ExecResult, error) {
	i := b.calls
	b.calls++
	if i >= len(b.results) {
		i = len(b.results) - 1
	}
	return b.results[i], b.errors[i]
}

func (b *qgFakeBackend) Exec(_ context.Context, opts backend.ExecOpts) (*backend.ExecResult, error) {
	b.allCalls = append(b.allCalls, opts)
	if opts.Command == sharedGoCacheWarmup || opts.Command == staleTestsIndexLockCleanup {
		b.preparationCalls = append(b.preparationCalls, opts)
		return &backend.ExecResult{}, nil
	}
	b.calls = append(b.calls, opts)
	if r, ok := b.commandResults[opts.Command]; ok {
		return r, nil
	}
	return &backend.ExecResult{ExitCode: 0, StdoutTail: "ok"}, nil
}

// TestRunQualityCheck_TimeoutReportsThrottling pins that a check killed at
// its budget names the budget and the sandbox's CPU throttling in its tail,
// so a human (and the classifier) can tell a starved container from slow
// tests.
func TestRunQualityCheck_TimeoutReportsThrottling(t *testing.T) {
	fb := &qgFakeBackend{
		fakeBackend: fakeBackend{statuses: map[string]*fakeStatus{}},
		commandResults: map[string]*backend.ExecResult{
			"go test ./...":               {ExitCode: 124, StdoutTail: "command timed out"},
			"cat /sys/fs/cgroup/cpu.stat": {ExitCode: 0, StdoutTail: "usage_usec 100\nnr_periods 1385\nnr_throttled 536\nthrottled_usec 54840289\n"},
		},
	}
	mgr := newQGTestManager(t, fb, "go")

	got := mgr.runQualityCheck(context.Background(), "sandbox", "/project", "project", "go", "test:0", "go test ./...", nil, 10)
	if got.Passed || got.ExitCode != 124 {
		t.Fatalf("passed=%v exit=%d, want a failed timeout", got.Passed, got.ExitCode)
	}
	for _, want := range []string{"test:0 timed out after 10s", "cpu throttled 38% of periods (536/1385, 54s)"} {
		if !strings.Contains(got.OutputTail, want) {
			t.Fatalf("output tail %q should contain %q", got.OutputTail, want)
		}
	}
	if len(fb.calls) != 2 || fb.calls[1].Command != "cat /sys/fs/cgroup/cpu.stat" {
		t.Fatalf("expected the throttle read after the timeout, got calls %+v", fb.calls)
	}
}

func TestParseCPUThrottle(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"": "",
		"nr_periods 0\nnr_throttled 0\nthrottled_usec 0":   "",
		"nr_periods 100\nnr_throttled 0\nthrottled_usec 0": "",
		"garbage": "",
		"nr_periods 200\nnr_throttled 50\nthrottled_usec 3500000": "cpu throttled 25% of periods (50/200, 3s)",
	}
	for stat, want := range cases {
		if got := parseCPUThrottle(stat); got != want {
			t.Fatalf("parseCPUThrottle(%q) = %q, want %q", stat, got, want)
		}
	}
}

func TestRunQualityCheck_ExitZeroPassesDespiteBackendError(t *testing.T) {
	result := &backend.ExecResult{ExitCode: 0}
	fb := &qgScriptedBackend{
		fakeBackend: fakeBackend{statuses: map[string]*fakeStatus{}},
		results:     []*backend.ExecResult{result},
		errors:      []error{errors.New("cleanup failed")},
	}
	mgr := newQGTestManager(t, fb, "go")

	got := mgr.runQualityCheck(context.Background(), "sandbox", "/project", "project", "go", "test:0", "go test ./...", nil, 10)
	if fb.calls != 1 {
		t.Fatalf("exec calls = %d, want 1", fb.calls)
	}
	if !got.Passed || got.ExitCode != 0 || !strings.Contains(got.OutputTail, "cleanup failed") {
		t.Fatalf("exit-zero verdict = %#v", got)
	}
}

func TestRunQualityCheck_EveryVerdictHasBoundedRedactedOutput(t *testing.T) {
	secret := "super-secret-token"
	for _, tc := range []struct {
		name   string
		result *backend.ExecResult
		err    error
		passed bool
	}{
		{name: "pass", result: &backend.ExecResult{ExitCode: 0}, err: errors.New(strings.Repeat("x", 600) + secret), passed: true},
		{name: "fail", result: &backend.ExecResult{ExitCode: 2, StderrTail: strings.Repeat("界", 200) + secret}, passed: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fb := &qgScriptedBackend{fakeBackend: fakeBackend{statuses: map[string]*fakeStatus{}}, results: []*backend.ExecResult{tc.result}, errors: []error{tc.err}}
			got := newQGTestManager(t, fb, "go").runQualityCheck(context.Background(), "sandbox", "/project", "project", "go", "test:0", "go test ./...", map[string]string{"GIT_TOKEN": secret}, 10)
			if got.Passed != tc.passed || got.OutputTail == "" {
				t.Fatalf("verdict = %#v", got)
			}
			if strings.Contains(got.OutputTail, secret) || len(got.OutputTail) > 500 || !utf8.ValidString(got.OutputTail) {
				t.Fatalf("output_tail was not bounded, UTF-8 safe, and redacted: %q", got.OutputTail)
			}
		})
	}
}

func TestQualityGate_ExtraTestsRunAfterChecksWithSandboxEnv(t *testing.T) {
	t.Setenv("LOOM_MCP_OUTPUT_FORMAT", "json")
	fb := &qgFakeBackend{fakeBackend: fakeBackend{statuses: map[string]*fakeStatus{}}, commandResults: map[string]*backend.ExecResult{}}
	mgr := newQGTestManager(t, fb, "go")
	result, err := mgr.handleQualityGate(context.Background(), map[string]any{
		"project":             "test-project",
		"checks":              []any{"fmt"},
		"extra_test_commands": []any{"go test ./pkg/a", "go test ./pkg/b"},
		"env":                 map[string]any{"GIT_TOKEN": "secret-token", "GOPRIVATE": "gitlab.flexinfer.ai/private/*", "CUSTOM": "must-not-pass"},
	})
	if err != nil {
		t.Fatalf("quality gate: %v", err)
	}
	var qr qualityGateResult
	if err := json.Unmarshal([]byte(result.Content[0].Text), &qr); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if got := []string{qr.Checks[0].Name, qr.Checks[1].Name, qr.Checks[2].Name}; !equalStrings(got, []string{"fmt", "test:0", "test:1"}) {
		t.Fatalf("check order = %v", got)
	}
	if len(fb.calls) != 3 {
		t.Fatalf("exec calls = %d", len(fb.calls))
	}
	for _, call := range fb.calls[1:] {
		if call.TimeoutSec < 15*60 {
			t.Errorf("extra test timeout = %ds, want at least 900s", call.TimeoutSec)
		}
		for key, want := range map[string]string{
			"GOTOOLCHAIN": "auto", "GOWORK": "off", "GOPRIVATE": "gitlab.flexinfer.ai/private/*",
			"CGO_ENABLED": "0", "GIT_CONFIG_COUNT": "2", "GIT_CONFIG_KEY_1": "safe.directory",
			"GIT_CONFIG_VALUE_1": "*",
		} {
			if call.Env[key] != want {
				t.Errorf("env[%s] = %q, want %q", key, call.Env[key], want)
			}
		}
		if !strings.Contains(call.Env["GIT_CONFIG_KEY_0"], "secret-token") {
			t.Errorf("git auth config missing token")
		}
		if _, ok := call.Env["CUSTOM"]; ok {
			t.Fatalf("unapproved env reached backend: %#v", call.Env)
		}
	}
}

func TestQualityGate_RefusesUnpinnedMillsRun(t *testing.T) {
	fb := &qgFakeBackend{fakeBackend: fakeBackend{statuses: map[string]*fakeStatus{}}}
	result, err := newQGTestManager(t, fb, "go").handleQualityGate(context.Background(), map[string]any{
		"project": "test-project", "env": map[string]any{"LOOM_MILLS_RUN_ID": "PIPE-X-1"},
	})
	if err != nil || !result.IsError || !strings.Contains(result.Content[0].Text, "mills run requested an unpinned gate") {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if len(fb.calls) != 0 {
		t.Fatalf("backend called: %#v", fb.calls)
	}
}

func TestFreshGoTestCommand(t *testing.T) {
	for _, tc := range []struct {
		input    string
		want     string
		injected bool
	}{
		{"go test ./pkg/x/...", "go test -count=1 ./pkg/x/...", true},
		{"GOWORK=off go test ./x/...", "GOWORK=off go test -count=1 ./x/...", true},
		{"env CGO_ENABLED=0 go test ./...", "env CGO_ENABLED=0 go test -count=1 ./...", true},
		{"go test -count=3 ./pkg/x/...", "go test -count=3 ./pkg/x/...", false},
		{"go test -count 3 ./pkg/x/...", "go test -count 3 ./pkg/x/...", false},
		{"gotestsum ./...", "gotestsum ./...", false},
		{"FOO=bar make test", "FOO=bar make test", false},
		{"go test ./a && go test ./b", "go test ./a && go test ./b", false},
		{"go test './quoted path'", "go test './quoted path'", false},
		{"npm test", "npm test", false},
	} {
		got, injected := freshGoTestCommand(tc.input)
		if got != tc.want || injected != tc.injected {
			t.Errorf("freshGoTestCommand(%q) = (%q, %t), want (%q, %t)", tc.input, got, injected, tc.want, tc.injected)
		}
	}
}

func TestQualityGate_EnvPrefixedGoTestReportsInjection(t *testing.T) {
	t.Setenv("LOOM_MCP_OUTPUT_FORMAT", "json")
	fb := &qgFakeBackend{fakeBackend: fakeBackend{statuses: map[string]*fakeStatus{}}, commandResults: map[string]*backend.ExecResult{}}
	result, err := newQGTestManager(t, fb, "go").handleQualityGate(context.Background(), map[string]any{
		"project":             "test-project",
		"checks":              []any{},
		"extra_test_commands": []any{"GOWORK=off go test ./internal/api/graphql/...", "go test ./a && go test ./b"},
		"fail_fast":           false,
	})
	if err != nil {
		t.Fatalf("quality gate: %v", err)
	}
	var qr qualityGateResult
	if err := json.Unmarshal([]byte(result.Content[0].Text), &qr); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if len(qr.Checks) != 2 {
		t.Fatalf("checks = %#v", qr.Checks)
	}
	if qr.Checks[0].Count1Injected == nil || !*qr.Checks[0].Count1Injected || !strings.HasPrefix(qr.Checks[0].OutputTail, "$ GOWORK=off go test -count=1 ./internal/api/graphql/...\n") {
		t.Fatalf("injected check = %#v", qr.Checks[0])
	}
	if qr.Checks[1].Count1Injected == nil || *qr.Checks[1].Count1Injected || !strings.HasPrefix(qr.Checks[1].OutputTail, "$ go test ./a && go test ./b\n") {
		t.Fatalf("unsafe check = %#v", qr.Checks[1])
	}
}

func TestRunQualityCheck_OutputStartsWithEffectiveCommand(t *testing.T) {
	fb := &qgFakeBackend{fakeBackend: fakeBackend{statuses: map[string]*fakeStatus{}}, commandResults: map[string]*backend.ExecResult{}}
	got := newQGTestManager(t, fb, "go").runQualityCheck(context.Background(), "sandbox", "/project", "project", "go", "test", "go test -count=1 ./...", nil, 10)
	if !strings.HasPrefix(got.OutputTail, "$ go test -count=1 ./...\n") {
		t.Fatalf("output_tail = %q", got.OutputTail)
	}
}

func TestQualityGate_ChecksOutDeclaredBranchBeforeBranchOnlyFailure(t *testing.T) {
	t.Setenv("LOOM_MCP_OUTPUT_FORMAT", "json")
	const sha = "2222222222222222222222222222222222222222"
	workDir := "/workspace/services/test-project.mills-tests-pipe-x-1"
	checkout := "if [ ! -d " + workDir + "/.git ]; then git clone --shared --no-checkout /workspace/services/test-project " + workDir + " && git -C " + workDir + " remote set-url origin \"$(git -C /workspace/services/test-project remote get-url origin)\"; fi && git -C " + workDir + " fetch --no-tags origin feat/branch-only && git -C " + workDir + " checkout --detach " + sha + " && git -C " + workDir + " reset --hard " + sha + " && git -C " + workDir + " clean -fdx && git -C " + workDir + " rev-parse HEAD"
	fb := &qgFakeBackend{
		fakeBackend: fakeBackend{statuses: map[string]*fakeStatus{}},
		commandResults: map[string]*backend.ExecResult{
			checkout:                 {ExitCode: 0, StdoutTail: sha},
			"go test -count=1 ./...": {ExitCode: 1, StderrTail: "branch-only test failed"},
		},
	}
	mgr := newQGTestManager(t, fb, "go")
	result, err := mgr.handleQualityGate(context.Background(), map[string]any{
		"project": "test-project", "checks": []any{},
		"extra_test_commands": []any{"go test ./..."},
		"env":                 map[string]any{"GIT_TOKEN": "secret-token", "LOOM_MILLS_BRANCH": "feat/branch-only", "LOOM_MILLS_EXPECTED_SHA": sha, "LOOM_MILLS_RUN_ID": "PIPE-X-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var qr qualityGateResult
	if err := json.Unmarshal([]byte(result.Content[0].Text), &qr); err != nil {
		t.Fatal(err)
	}
	if qr.Passed || qr.TestedSHA != sha || !strings.Contains(qr.Checks[0].OutputTail, "branch-only") {
		t.Fatalf("result = %+v", qr)
	}
	if len(fb.allCalls) != 4 || fb.allCalls[0].Command != staleTestsIndexLockCleanup || fb.allCalls[1].Command != checkout || fb.allCalls[2].Command != sharedGoCacheWarmup || fb.allCalls[2].WorkDir != workDir {
		t.Fatalf("preparation ordering/workdir = %#v", fb.allCalls)
	}
	if len(fb.calls) != 2 || fb.calls[0].Command != checkout || fb.calls[0].TimeoutSec != 900 {
		t.Fatalf("calls = %#v", fb.calls)
	}
	if fb.calls[0].WorkDir != "/workspace/services/test-project" || fb.calls[1].WorkDir != workDir {
		t.Fatalf("checkout/test workdirs = %q / %q", fb.calls[0].WorkDir, fb.calls[1].WorkDir)
	}
	if strings.Contains(fb.calls[0].Command, "main") || strings.Contains(fb.calls[0].Command, "--all") {
		t.Fatalf("fetch was not bounded to declared branch: %q", fb.calls[0].Command)
	}
	if !strings.Contains(fb.calls[0].Command, "git clone --shared") || strings.Contains(fb.calls[0].Command, "git init") {
		t.Fatalf("checkout did not use a shared clone: %q", fb.calls[0].Command)
	}
	if !strings.Contains(fb.calls[0].Env["GIT_CONFIG_KEY_0"], "secret-token") {
		t.Fatalf("checkout git auth config missing token: %#v", fb.calls[0].Env)
	}
}

func TestQualityGate_TestedSHAMismatchStopsBeforeChecks(t *testing.T) {
	const expected = "2222222222222222222222222222222222222222"
	const actual = "3333333333333333333333333333333333333333"
	workDir := "/workspace/services/test-project.mills-tests-pipe-x-2"
	checkout := "if [ ! -d " + workDir + "/.git ]; then git clone --shared --no-checkout /workspace/services/test-project " + workDir + " && git -C " + workDir + " remote set-url origin \"$(git -C /workspace/services/test-project remote get-url origin)\"; fi && git -C " + workDir + " fetch --no-tags origin feat/mismatch && git -C " + workDir + " checkout --detach " + expected + " && git -C " + workDir + " reset --hard " + expected + " && git -C " + workDir + " clean -fdx && git -C " + workDir + " rev-parse HEAD"
	fb := &qgFakeBackend{fakeBackend: fakeBackend{statuses: map[string]*fakeStatus{}}, commandResults: map[string]*backend.ExecResult{
		checkout: {ExitCode: 0, StdoutTail: actual},
	}}
	mgr := newQGTestManager(t, fb, "go")
	result, err := mgr.handleQualityGate(context.Background(), map[string]any{
		"project": "test-project", "checks": []any{"fmt"},
		"env": map[string]any{"LOOM_MILLS_BRANCH": "feat/mismatch", "LOOM_MILLS_EXPECTED_SHA": expected, "LOOM_MILLS_RUN_ID": "PIPE-X-2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError || !strings.Contains(result.Content[0].Text, "tested sha mismatch") {
		t.Fatalf("result = %#v", result)
	}
	if len(fb.calls) != 1 {
		t.Fatalf("checks ran after mismatch: %#v", fb.calls)
	}
}

func TestQualityGate_ExtraTestsWithoutTokenDoNotAddGoEnvAndFailFast(t *testing.T) {
	t.Setenv("LOOM_MCP_OUTPUT_FORMAT", "json")
	fb := &qgFakeBackend{
		fakeBackend: fakeBackend{statuses: map[string]*fakeStatus{}},
		commandResults: map[string]*backend.ExecResult{
			"go test -count=1 ./bad": {ExitCode: 1, StderrTail: "compile failed"},
		},
	}
	mgr := newQGTestManager(t, fb, "go")
	result, err := mgr.handleQualityGate(context.Background(), map[string]any{
		"project": "test-project", "checks": []any{"fmt"},
		"extra_test_commands": []any{"go test ./bad", "go test ./never"},
		"env":                 map[string]any{"CUSTOM": "value"},
	})
	if err != nil {
		t.Fatalf("quality gate: %v", err)
	}
	var qr qualityGateResult
	if err := json.Unmarshal([]byte(result.Content[0].Text), &qr); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if len(qr.Checks) != 2 || qr.Checks[1].Name != "test:0" || !strings.Contains(qr.Checks[1].OutputTail, "compile failed") {
		t.Fatalf("checks = %#v", qr.Checks)
	}
	if len(fb.calls) != 2 || fb.calls[1].Command != "go test -count=1 ./bad" {
		t.Fatalf("calls = %#v", fb.calls)
	}
	if _, ok := fb.calls[1].Env["GOWORK"]; ok {
		t.Fatalf("Go sandbox env added without GIT_TOKEN: %#v", fb.calls[1].Env)
	}
}

func TestQualityGate_LintParityUsesRepoConfigScopeAndBoundedTimeout(t *testing.T) {
	t.Setenv("LOOM_MCP_OUTPUT_FORMAT", "json")
	cmd := "GOWORK=off golangci-lint run --config .golangci.yml './pkg/a' './pkg/b'"
	fb := &qgFakeBackend{fakeBackend: fakeBackend{statuses: map[string]*fakeStatus{}}, commandResults: map[string]*backend.ExecResult{cmd: {ExitCode: 1, StdoutTail: "db.QueryRow must be QueryRowContext (noctx)\ndb.Exec must be ExecContext (noctx)\nunused value"}}}
	mgr := newQGTestManager(t, fb, "go")
	result, err := mgr.handleQualityGate(context.Background(), map[string]any{"project": "test-project", "checks": []any{"fmt"}, "extra_test_commands": []any{cmd}})
	if err != nil {
		t.Fatal(err)
	}
	var qr qualityGateResult
	if err := json.Unmarshal([]byte(result.Content[0].Text), &qr); err != nil {
		t.Fatal(err)
	}
	if qr.Passed || qr.Checks[1].Name != "lint:parity" || !strings.Contains(qr.Checks[1].OutputTail, "QueryRowContext") {
		t.Fatalf("result = %+v", qr)
	}
	if call := fb.calls[1]; call.Command != cmd || call.TimeoutSec != 900 {
		t.Fatalf("lint call = %+v", call)
	}
}

func TestQualityGate_LintParityMissingBinaryWarnsAndPasses(t *testing.T) {
	t.Setenv("LOOM_MCP_OUTPUT_FORMAT", "json")
	cmd := "GOWORK=off golangci-lint run --config .golangci.yml './pkg/a'"
	fb := &qgFakeBackend{fakeBackend: fakeBackend{statuses: map[string]*fakeStatus{}}, commandResults: map[string]*backend.ExecResult{cmd: {ExitCode: 127, StderrTail: "sh: golangci-lint: not found"}}}
	mgr := newQGTestManager(t, fb, "go")
	result, err := mgr.handleQualityGate(context.Background(), map[string]any{"project": "test-project", "checks": []any{"fmt"}, "extra_test_commands": []any{cmd}})
	if err != nil {
		t.Fatal(err)
	}
	var qr qualityGateResult
	if err := json.Unmarshal([]byte(result.Content[0].Text), &qr); err != nil {
		t.Fatal(err)
	}
	if !qr.Passed || !qr.Checks[1].Passed || !strings.Contains(qr.Checks[1].Warning, "unavailable") {
		t.Fatalf("result = %+v", qr)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func newQGTestManager(t *testing.T, b backend.Backend, lang string) *manager {
	t.Helper()

	tmpDir := t.TempDir()
	projectDir := filepath.Join(tmpDir, "workspace", "services", "test-project")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatalf("create project dir: %v", err)
	}

	// Create language marker file
	switch lang {
	case "go":
		os.WriteFile(filepath.Join(projectDir, "go.mod"), []byte("module test\n\ngo 1.22\n"), 0o644)
	case "python":
		os.WriteFile(filepath.Join(projectDir, "pyproject.toml"), []byte("[project]\nname=\"test\"\n"), 0o644)
	case "node":
		os.WriteFile(filepath.Join(projectDir, "package.json"), []byte(`{"name":"test"}`), 0o644)
	case "rust":
		os.WriteFile(filepath.Join(projectDir, "Cargo.toml"), []byte("[package]\nname=\"test\"\n"), 0o644)
	default:
		os.WriteFile(filepath.Join(projectDir, "Makefile"), []byte("fmt:\nlint:\ntest:\n"), 0o644)
	}

	store, err := state.NewStore(filepath.Join(tmpDir, "cache"))
	if err != nil {
		t.Fatalf("create store: %v", err)
	}

	return &manager{
		cfg: managerConfig{
			workspaceRoot: filepath.Join(tmpDir, "workspace"),
			cacheDir:      filepath.Join(tmpDir, "cache"),
			backendType:   "docker",
			imagePrefix:   "test/devbox",
			maxTailLines:  20,
			idleTimeout:   5 * time.Minute,
			defaultCPU:    1.0,
			defaultMemMB:  512,
		},
		backend: b,
		store:   store,
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func TestQualityGate_GoAllPass(t *testing.T) {
	t.Setenv("LOOM_MCP_OUTPUT_FORMAT", "json")
	fb := &qgFakeBackend{
		fakeBackend:    fakeBackend{statuses: map[string]*fakeStatus{}},
		commandResults: map[string]*backend.ExecResult{},
	}
	mgr := newQGTestManager(t, fb, "go")

	result, err := mgr.handleQualityGate(context.Background(), map[string]any{
		"project": "test-project",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}

	// Parse the JSON result
	var qr qualityGateResult
	if len(result.Content) > 0 {
		if err := json.Unmarshal([]byte(result.Content[0].Text), &qr); err != nil {
			t.Fatalf("unmarshal result: %v", err)
		}
	}

	if qr.Language != "go" {
		t.Errorf("language = %q, want %q", qr.Language, "go")
	}
	if !qr.Passed {
		t.Error("expected quality gate to pass")
	}
	if len(qr.Checks) != 3 {
		t.Errorf("expected 3 checks, got %d", len(qr.Checks))
	}
}

func TestQualityGate_FailFast(t *testing.T) {
	t.Setenv("LOOM_MCP_OUTPUT_FORMAT", "json")
	fb := &qgFakeBackend{
		fakeBackend: fakeBackend{statuses: map[string]*fakeStatus{}},
		commandResults: map[string]*backend.ExecResult{
			"go vet ./...": {ExitCode: 1, StdoutTail: "vet: found issues"},
		},
	}
	mgr := newQGTestManager(t, fb, "go")

	result, err := mgr.handleQualityGate(context.Background(), map[string]any{
		"project":   "test-project",
		"fail_fast": true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}

	var qr qualityGateResult
	if len(result.Content) > 0 {
		json.Unmarshal([]byte(result.Content[0].Text), &qr)
	}

	if qr.Passed {
		t.Error("expected quality gate to fail")
	}
	// fail_fast should stop after lint (fmt passes, lint fails)
	if len(qr.Checks) != 2 {
		t.Errorf("expected 2 checks (fail_fast after lint), got %d", len(qr.Checks))
	}
}

func TestQualityGate_CustomChecks(t *testing.T) {
	t.Setenv("LOOM_MCP_OUTPUT_FORMAT", "json")
	fb := &qgFakeBackend{
		fakeBackend:    fakeBackend{statuses: map[string]*fakeStatus{}},
		commandResults: map[string]*backend.ExecResult{},
	}
	mgr := newQGTestManager(t, fb, "go")

	result, err := mgr.handleQualityGate(context.Background(), map[string]any{
		"project": "test-project",
		"checks":  []any{"test"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var qr qualityGateResult
	if len(result.Content) > 0 {
		json.Unmarshal([]byte(result.Content[0].Text), &qr)
	}

	if len(qr.Checks) != 1 {
		t.Errorf("expected 1 check, got %d", len(qr.Checks))
	}
	if len(qr.Checks) > 0 && qr.Checks[0].Name != "test" {
		t.Errorf("check name = %q, want %q", qr.Checks[0].Name, "test")
	}
}

// TestQualityGate_NoExecutedChecksErrors verifies the gate refuses to
// mint a verdict when zero checks executed (empty or unresolvable
// checks selector). Before the guard it returned passed=true with an
// empty checks array — a definite verdict from zero evidence.
func TestQualityGate_NoExecutedChecksErrors(t *testing.T) {
	t.Setenv("LOOM_MCP_OUTPUT_FORMAT", "json")
	for name, checksArg := range map[string]any{
		"unknown check name": []any{"bogus"},
		"empty checks list":  []any{},
	} {
		fb := &qgFakeBackend{
			fakeBackend:    fakeBackend{statuses: map[string]*fakeStatus{}},
			commandResults: map[string]*backend.ExecResult{},
		}
		mgr := newQGTestManager(t, fb, "go")

		result, err := mgr.handleQualityGate(context.Background(), map[string]any{
			"project": "test-project",
			"checks":  checksArg,
		})
		if err != nil {
			t.Fatalf("%s: unexpected transport error: %v", name, err)
		}
		if result == nil || !result.IsError {
			t.Fatalf("%s: expected IsError result, got %+v", name, result)
		}
		if len(result.Content) == 0 || !strings.Contains(result.Content[0].Text, "executed no checks") {
			t.Errorf("%s: error should name the zero-checks condition: %+v", name, result.Content)
		}
	}
}

func TestQualityGate_GitCloneProbesSandboxLanguage(t *testing.T) {
	t.Setenv("LOOM_MCP_OUTPUT_FORMAT", "json")
	fb := &qgFakeBackend{
		fakeBackend: fakeBackend{statuses: map[string]*fakeStatus{}},
		commandResults: map[string]*backend.ExecResult{
			sandboxLanguageProbeCommand: {ExitCode: 0, StdoutTail: "go"},
		},
	}
	mgr := newQGTestManager(t, fb, "unknown")
	mgr.cfg.syncMode = "git-clone"

	result, err := mgr.handleQualityGate(context.Background(), map[string]any{
		"project": "test-project",
		"checks":  []string{"fmt"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var qr qualityGateResult
	if len(result.Content) > 0 {
		if err := json.Unmarshal([]byte(result.Content[0].Text), &qr); err != nil {
			t.Fatalf("unmarshal result: %v", err)
		}
	}

	if qr.Language != "go" {
		t.Fatalf("language = %q, want go", qr.Language)
	}
	if len(qr.Checks) != 1 || qr.Checks[0].Name != "fmt" {
		t.Fatalf("checks = %#v", qr.Checks)
	}
}

// TestDetectSandboxLanguage_FallsBackToWorkspaceRoot asserts the
// probe iterates past the per-project workdir and inspects /workspace
// when the first probe comes back unknown. The historical canary
// failures fingerprint as unknown at the project path (git-clone
// dropped source under a different prefix than expected) and the
// fallback path is what keeps `make fmt` from running blind.
func TestDetectSandboxLanguage_FallsBackToWorkspaceRoot(t *testing.T) {
	calls := []backend.ExecOpts{}
	fb := &fakeProbeBackend{
		results: map[string]*backend.ExecResult{
			"/workspace": {ExitCode: 0, StdoutTail: "go\n"},
		},
		calls: &calls,
	}
	tmp := t.TempDir()
	mgr := &manager{
		cfg:    managerConfig{workspaceRoot: tmp},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	mgr.backend = fb

	got := mgr.detectSandboxLanguage(context.Background(), "ctr", filepath.Join(tmp, "services", "loom-core"))
	if got != "go" {
		t.Fatalf("language = %q, want go", got)
	}
	if len(calls) != 2 {
		t.Fatalf("expected 2 probe attempts (workdir + workspace root), got %d", len(calls))
	}
	if calls[0].WorkDir == calls[1].WorkDir {
		t.Fatalf("expected distinct probe paths, got both = %q", calls[0].WorkDir)
	}
}

// fakeProbeBackend records every Exec call and returns workdir-keyed
// results so detectSandboxLanguage path iteration is observable.
type fakeProbeBackend struct {
	fakeBackend
	results map[string]*backend.ExecResult
	calls   *[]backend.ExecOpts
}

func (b *fakeProbeBackend) Exec(_ context.Context, opts backend.ExecOpts) (*backend.ExecResult, error) {
	*b.calls = append(*b.calls, opts)
	if r, ok := b.results[opts.WorkDir]; ok {
		return r, nil
	}
	return &backend.ExecResult{ExitCode: 0, StdoutTail: "unknown\n"}, nil
}

// TestQualityGate_StderrSurfacedWhenStdoutEmpty mirrors the canary
// failure mode where `make fmt` returns exit=1 with empty stdout
// but a useful stderr line. The artifact's OutputTail must reflect
// stderr so escalations show the actual error.
// emptyErrorBackend returns a non-nil error whose Error() is empty
// for every Exec call — the empty-err.Error() failure mode observed in
// production canary attempts when go-toolchain exec wrappers swallow
// their message. Verifies the gate now synthesizes a fallback so the
// check artifact never reaches the operator with a blank Output.
type emptyErrorBackend struct{ fakeBackend }

type emptyErr struct{}

func (emptyErr) Error() string { return "" }

func (b *emptyErrorBackend) Exec(_ context.Context, _ backend.ExecOpts) (*backend.ExecResult, error) {
	return nil, emptyErr{}
}

func TestQualityGate_EmptyErrorStillProducesOutput(t *testing.T) {
	t.Setenv("LOOM_MCP_OUTPUT_FORMAT", "json")
	fb := &emptyErrorBackend{fakeBackend{statuses: map[string]*fakeStatus{}}}
	mgr := newQGTestManager(t, fb, "go")

	result, err := mgr.handleQualityGate(context.Background(), map[string]any{
		"project": "test-project",
		"checks":  []string{"fmt"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var qr qualityGateResult
	if len(result.Content) > 0 {
		if err := json.Unmarshal([]byte(result.Content[0].Text), &qr); err != nil {
			t.Fatalf("unmarshal: %v (raw=%q)", err, result.Content[0].Text)
		}
	}
	if len(qr.Checks) != 1 {
		t.Fatalf("expected 1 check, got %d", len(qr.Checks))
	}
	got := qr.Checks[0]
	if got.Passed {
		t.Fatalf("expected fmt check to fail when exec returns error")
	}
	if got.OutputTail == "" {
		t.Fatalf("OutputTail must never be empty when exec errored; got blank")
	}
	if !strings.Contains(got.OutputTail, "fmt") && !strings.Contains(got.OutputTail, "exec failed") {
		t.Fatalf("OutputTail = %q, expected a fallback that names the check or exec", got.OutputTail)
	}
}

func TestQualityGate_StderrSurfacedWhenStdoutEmpty(t *testing.T) {
	t.Setenv("LOOM_MCP_OUTPUT_FORMAT", "json")
	const stderrMsg = "make: *** No rule to make target 'fmt'. Stop."
	fb := &qgFakeBackend{
		fakeBackend: fakeBackend{statuses: map[string]*fakeStatus{}},
		commandResults: map[string]*backend.ExecResult{
			"make fmt": {ExitCode: 1, StdoutTail: "", StderrTail: stderrMsg},
		},
	}
	mgr := newQGTestManager(t, fb, "unknown")
	// Without a language marker on the host (lang="unknown"), the
	// only way ensureRunning's dockerfile generation succeeds is via
	// the git-clone genericGitCloneDockerfile fallback.
	mgr.cfg.syncMode = "git-clone"

	result, err := mgr.handleQualityGate(context.Background(), map[string]any{
		"project": "test-project",
		"checks":  []string{"fmt"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var qr qualityGateResult
	if len(result.Content) > 0 {
		if err := json.Unmarshal([]byte(result.Content[0].Text), &qr); err != nil {
			t.Fatalf("unmarshal result: %v (raw=%q)", err, result.Content[0].Text)
		}
	}
	if len(qr.Checks) != 1 {
		t.Fatalf("expected 1 check, got %d", len(qr.Checks))
	}
	got := qr.Checks[0]
	if got.Passed {
		t.Fatalf("expected fmt check to fail")
	}
	if got.StderrTail != stderrMsg {
		t.Fatalf("StderrTail = %q, want %q", got.StderrTail, stderrMsg)
	}
	if got.OutputTail != "$ make fmt\n"+stderrMsg {
		t.Fatalf("OutputTail = %q, want stderr fallback %q", got.OutputTail, stderrMsg)
	}
}

func TestQualityGate_DiffCheckAndStringChecks(t *testing.T) {
	t.Setenv("LOOM_MCP_OUTPUT_FORMAT", "json")
	fb := &qgFakeBackend{
		fakeBackend: fakeBackend{statuses: map[string]*fakeStatus{}},
		commandResults: map[string]*backend.ExecResult{
			"git diff --exit-code": {ExitCode: 1, StdoutTail: "generated files drifted"},
		},
	}
	mgr := newQGTestManager(t, fb, "go")

	result, err := mgr.handleQualityGate(context.Background(), map[string]any{
		"project": "test-project",
		"checks":  []string{"diff"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var qr qualityGateResult
	if len(result.Content) > 0 {
		if err := json.Unmarshal([]byte(result.Content[0].Text), &qr); err != nil {
			t.Fatalf("unmarshal result: %v", err)
		}
	}

	if qr.Passed {
		t.Fatal("expected diff check to fail")
	}
	if len(qr.Checks) != 1 {
		t.Fatalf("expected 1 check, got %d", len(qr.Checks))
	}
	if qr.Checks[0].Name != "diff" {
		t.Fatalf("check name = %q, want diff", qr.Checks[0].Name)
	}
	if qr.Checks[0].OutputTail != "$ git diff --exit-code\ngenerated files drifted" {
		t.Fatalf("output tail = %q, want diff failure output", qr.Checks[0].OutputTail)
	}
}

func TestTruncateOutput(t *testing.T) {
	t.Parallel()

	short := "hello"
	if got := truncateOutput(short, 100); got != short {
		t.Errorf("short string should not be truncated: got %q", got)
	}

	long := "line1\nline2\nline3\nline4\nline5"
	truncated := truncateOutput(long, 15)
	if len(truncated) > 20 { // 15 + "..." prefix
		t.Errorf("truncated output too long: %d bytes", len(truncated))
	}
}

// TestAwaitSandboxBuild covers the quality gate's behavior of polling past the
// async builder's "build in progress" signal instead of failing instantly
// (the root cause of the Mills canary tests stage recording `0 checks`).
func TestAwaitSandboxBuild(t *testing.T) {
	building := &buildInProgressError{tag: "t", project: "p"}

	t.Run("returns once build completes", func(t *testing.T) {
		calls := 0
		ensure := func() (string, error) {
			calls++
			if calls < 3 {
				return "", building
			}
			return "container-123", nil
		}
		id, err := awaitSandboxBuild(context.Background(), time.Second, time.Millisecond, ensure)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if id != "container-123" {
			t.Fatalf("id = %q, want container-123", id)
		}
		if calls != 3 {
			t.Fatalf("ensure called %d times, want 3", calls)
		}
	})

	t.Run("passes through non-building errors immediately", func(t *testing.T) {
		calls := 0
		boom := errors.New("boom")
		ensure := func() (string, error) {
			calls++
			return "", boom
		}
		_, err := awaitSandboxBuild(context.Background(), time.Second, time.Millisecond, ensure)
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v, want boom", err)
		}
		if calls != 1 {
			t.Fatalf("ensure called %d times, want 1 (no retry on non-building error)", calls)
		}
	})

	t.Run("times out while still building", func(t *testing.T) {
		ensure := func() (string, error) { return "", building }
		_, err := awaitSandboxBuild(context.Background(), 20*time.Millisecond, time.Millisecond, ensure)
		if err == nil || !strings.Contains(err.Error(), "still building") {
			t.Fatalf("err = %v, want 'still building' timeout", err)
		}
		if _, ok := asBuildInProgress(err); !ok {
			t.Fatalf("timeout error should wrap buildInProgressError, got %v", err)
		}
	})

	t.Run("honors context cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		calls := 0
		ensure := func() (string, error) {
			calls++
			if calls == 1 {
				cancel()
			}
			return "", building
		}
		_, err := awaitSandboxBuild(ctx, time.Minute, 5*time.Millisecond, ensure)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	})
}

func runGateScript(t *testing.T, script, dir string, env ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "sh", "-c", script)
	command.Dir = dir
	command.Env = append(os.Environ(), env...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("script: %v: %s", err, output)
	}
	return string(output)
}

// requireFlock skips when the flock binary the warm-up script depends on is
// absent (macOS developer machines); CI images and the sandbox image have it.
func requireFlock(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("flock"); err != nil {
		t.Skip("flock unavailable")
	}
}

// holdWarmupLock takes an exclusive flock on path from the test process and
// keeps it until the test ends. The script's `flock -n 9` runs in a child with
// its own open file description, so it observes the lock as held.
func holdWarmupLock(t *testing.T, path string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
}

func TestSharedGoCacheWarmup(t *testing.T) {
	requireFlock(t)
	for _, mode := range []string{"cold", "warm", "failure", "timeout", "no-mod", "replacement", "local-replacement"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			cache := filepath.Join(root, "cache")
			if err := os.MkdirAll(filepath.Join(cache, "gomod"), 0755); err != nil {
				t.Fatal(err)
			}
			script := strings.ReplaceAll(sharedGoCacheWarmup, "/gocache", cache)
			script = strings.ReplaceAll(script, "lock_ticks=300", "lock_ticks=2")
			// What `go mod edit -print` emits: a require block with an indirect
			// comment for the plain case, single-line replace directives (one
			// version-pinned, one unpinned) for the replacement cases.
			manifest := "module test\n\ngo 1.26\n\nrequire (\n\texample.com/Module v1.0.0 // indirect\n)\n"
			module := "example.com/!module@v1.0.0"
			if mode == "replacement" {
				manifest = "module test\n\nrequire example.com/Module v1.0.0\n\nreplace example.com/Module v1.0.0 => example.com/Other v2.0.0\n"
				module = "example.com/!other@v2.0.0"
			}
			if mode == "local-replacement" {
				manifest = "module test\n\nrequire example.com/Module v1.0.0\n\nreplace example.com/Module => ../local\n"
			}
			if mode != "no-mod" {
				if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module test\n"), 0644); err != nil {
					t.Fatal(err)
				}
			}
			fakeGo := fmt.Sprintf("#!/bin/sh\nif [ \"$2\" = edit ]; then\ncat <<'GOMOD'\n%sGOMOD\nexit 0\nfi\necho download >> '%s/downloads'\n", manifest, root)
			if mode == "failure" {
				fakeGo += "exit 1\n"
			} else {
				fakeGo += fmt.Sprintf("mkdir -p '%s/gomod/%s'\ntouch '%s/gomod/%s/go.mod'\n", cache, module, cache, module)
			}
			if err := os.WriteFile(filepath.Join(root, "go"), []byte(fakeGo), 0755); err != nil {
				t.Fatal(err)
			}
			env := []string{"PATH=" + root + ":" + os.Getenv("PATH"), "GOMODCACHE=" + cache + "/gomod"}
			if mode == "warm" {
				if err := os.MkdirAll(filepath.Join(cache, "gomod", module), 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(cache, "gomod", module, "go.mod"), nil, 0644); err != nil {
					t.Fatal(err)
				}
			}
			// Another open file description holds the same inode, proving both
			// the timeout and the warm bypass (a warm cache never touches the lock).
			if mode == "timeout" || mode == "warm" {
				holdWarmupLock(t, filepath.Join(cache, ".warmup.lock"))
			}
			output := runGateScript(t, script, root, env...)
			_, err := os.Stat(filepath.Join(root, "downloads"))
			wantDownload := mode == "cold" || mode == "failure" || mode == "replacement"
			if (err == nil) != wantDownload {
				t.Fatalf("download=%v want=%v output=%s", err == nil, wantDownload, output)
			}
			if mode == "timeout" && !strings.Contains(output, "lock timeout; proceeding to gate") {
				t.Fatal(output)
			}
			if wantDownload {
				// A second execution would time out if success/error failed to release.
				output = runGateScript(t, script, root, env...)
				if strings.Contains(output, "lock timeout") {
					t.Fatal(output)
				}
			}
		})
	}
}

func TestSharedGoCacheWarmupsSerialize(t *testing.T) {
	requireFlock(t)
	root := t.TempDir()
	cache := filepath.Join(root, "cache")
	if err := os.MkdirAll(filepath.Join(cache, "gomod"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "go.mod"), nil, 0644); err != nil {
		t.Fatal(err)
	}
	// Deliberately leave the required module absent so both contenders download.
	fake := `#!/bin/sh
if [ "$2" = edit ]; then
 printf 'module test\n\nrequire example.com/m v1.0.0\n'
 exit 0
fi
mkdir active || { echo overlap >> overlap; exit 1; }
sleep 0.3
rmdir active
echo done >> downloads
`
	if err := os.WriteFile(filepath.Join(root, "go"), []byte(fake), 0755); err != nil {
		t.Fatal(err)
	}
	script := strings.ReplaceAll(sharedGoCacheWarmup, "/gocache", cache)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	commands := make([]*exec.Cmd, 2)
	for i := range commands {
		commands[i] = exec.CommandContext(ctx, "sh", "-c", script)
		commands[i].Dir = root
		commands[i].Env = append(os.Environ(), "PATH="+root+":"+os.Getenv("PATH"), "GOMODCACHE="+cache+"/gomod")
		if err := commands[i].Start(); err != nil {
			t.Fatal(err)
		}
	}
	for _, command := range commands {
		if err := command.Wait(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "overlap")); !os.IsNotExist(err) {
		t.Fatal("downloads overlapped")
	}
	data, err := os.ReadFile(filepath.Join(root, "downloads"))
	if err != nil || string(data) != "done\ndone\n" {
		t.Fatalf("downloads %q: %v", data, err)
	}
}

func TestStaleTestsIndexLockCleanup(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 unavailable")
	}
	for _, mode := range []string{"stale", "legacy-stale", "fresh", "active", "git", "unknown", "missing", "not-tests"} {
		t.Run(mode, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "repo.mills-tests")
			if mode == "legacy-stale" {
				root += "-run"
			}
			if mode == "not-tests" {
				root = filepath.Join(t.TempDir(), "repo")
			}
			if err := os.MkdirAll(filepath.Join(root, ".git"), 0755); err != nil {
				t.Fatal(err)
			}
			lock := filepath.Join(root, ".git", "index.lock")
			if mode != "missing" {
				if err := os.WriteFile(lock, nil, 0644); err != nil {
					t.Fatal(err)
				}
				if mode != "fresh" {
					old := time.Now().Add(-2 * time.Minute)
					if err := os.Chtimes(lock, old, old); err != nil {
						t.Fatal(err)
					}
				}
			}
			proc := filepath.Join(t.TempDir(), "proc")
			for _, dir := range []string{"self/fd", "123/fd"} {
				if err := os.MkdirAll(filepath.Join(proc, dir), 0755); err != nil {
					t.Fatal(err)
				}
			}
			comm := "worker"
			if mode == "git" {
				comm = "git"
			}
			if err := os.WriteFile(filepath.Join(proc, "123/comm"), []byte(comm), 0644); err != nil {
				t.Fatal(err)
			}
			if mode == "active" {
				if err := os.Symlink(lock, filepath.Join(proc, "123/fd/3")); err != nil {
					t.Fatal(err)
				}
			}
			script := strings.ReplaceAll(staleTestsIndexLockCleanup, "pathlib.Path('/proc')", "pathlib.Path('"+proc+"')")
			if mode == "unknown" {
				script = strings.ReplaceAll(script, "comm = (process / 'comm').read_text().strip()", "raise PermissionError('hidden process')")
			}
			output := runGateScript(t, script, root)
			_, err := os.Stat(lock)
			wantExists := mode != "stale" && mode != "legacy-stale" && mode != "missing"
			if (err == nil) != wantExists {
				t.Fatalf("lock exists=%v want=%v: %s", err == nil, wantExists, output)
			}
			if mode == "stale" && !strings.Contains(output, "removed stale") {
				t.Fatal(output)
			}
		})
	}
}

func TestQualityDiagnosticTail(t *testing.T) {
	diagnostic := "typecheck: could not import example.com/m (/gocache/gomod/example.com/m@v1.0.0/file.go: no such file or directory)"
	input := diagnostic + "\n" + strings.Repeat("later noise\n", 100)
	got := qualityDiagnosticTail(input, 500)
	if !strings.Contains(got, diagnostic) || len(got) > 500 {
		t.Fatalf("lost diagnostic: %s", got)
	}
}

func TestRunQualityCheck_PreservesRedactedCacheDiagnostic(t *testing.T) {
	const secret = "private-example-token"
	diagnostic := "typecheck: could not import example.com/m (/gocache/gomod/example.com/m@v1.0.0/file.go: no such file or directory)"
	fb := &qgFakeBackend{commandResults: map[string]*backend.ExecResult{
		"go test ./...": {ExitCode: 1, StdoutTail: diagnostic + " " + secret + "\n" + strings.Repeat("noise\n", 200)},
	}}
	got := newQGTestManager(t, fb, "go").runQualityCheck(context.Background(), "sandbox", "/project", "project", "go", "test", "go test ./...", map[string]string{"GIT_TOKEN": secret}, 10)
	if !strings.Contains(got.OutputTail, diagnostic) || strings.Contains(got.OutputTail, secret) || len(got.OutputTail) > 500 {
		t.Fatalf("diagnostic not preserved safely: %s", got.OutputTail)
	}
}

func TestTestRevisionWorkDir(t *testing.T) {
	for _, tc := range []struct{ backend, mode, project, run, want string }{
		{"k8s", "git-clone", "a", "PIPE-1", "/workspace/a.mills-tests"},
		{"k8s", "git-clone", "a", "PIPE-2", "/workspace/a.mills-tests"},
		{"k8s", "tar-pipe", "a", "PIPE-2", "/workspace/a.mills-tests"},
		{"k8s", "git-clone", "b", "PIPE-1", "/workspace/b.mills-tests"},
		{"k8s", "nfs", "a", "PIPE-1", "/workspace/a.mills-tests-pipe-1"},
		{"k8s", "nfs", "a", "PIPE-2", "/workspace/a.mills-tests-pipe-2"},
		{"k8s", "", "a", "PIPE-1", "/workspace/a.mills-tests-pipe-1"},
		{"docker", "", "a", "PIPE-1", "/workspace/a.mills-tests-pipe-1"},
	} {
		t.Run(tc.backend+"/"+tc.mode+"/"+tc.project+"/"+tc.run, func(t *testing.T) {
			m := &manager{cfg: managerConfig{workspaceRoot: "/projects", backendType: tc.backend, syncMode: tc.mode}}
			if tc.mode == "git-clone" {
				m.cfg.gitBaseURL = "https://git.example"
				m.cfg.gitSecret = "git"
			}
			if got := m.testRevisionWorkDir("/projects/"+tc.project, tc.run); got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestCheckoutTestRevisionRequiresRunIDForStablePath(t *testing.T) {
	m := &manager{cfg: managerConfig{backendType: "k8s", syncMode: "git-clone"}}
	_, _, err := m.checkoutTestRevision(context.Background(), "pod", "/repo", "branch", strings.Repeat("a", 40), nil)
	if err == nil || !strings.Contains(err.Error(), "run id missing") {
		t.Fatalf("error = %v", err)
	}
}

func TestQualityGateMemoryEnv(t *testing.T) {
	for _, tc := range []struct {
		name             string
		memory, override int
		want             string
	}{
		{"six-gib", 6144, 0, "5120MiB"},
		{"small", 512, 0, "426MiB"},
		{"override", 6144, 3072, "2560MiB"},
		{"invalid-override", 6144, -1, "5120MiB"},
		{"unset", 0, 0, ""},
		{"invalid", -1, 0, ""},
	} {
		for _, token := range []string{"", "token"} {
			t.Run(tc.name+"/"+token, func(t *testing.T) {
				m := &manager{cfg: managerConfig{defaultMemMB: tc.memory}}
				fp := &detect.EnvFingerprint{Overrides: &detect.ManifestOverride{Limits: &detect.LimitOverride{MemoryMB: tc.override}}}
				base := map[string]string{"GIT_TOKEN": token, "GOCACHE": "/gocache/go-build"}
				got := extraTestEnv(qualityGateMemoryEnv(base, m.sandboxMemoryMB(fp)))
				if got["GOMEMLIMIT"] != tc.want || got["GOCACHE"] != base["GOCACHE"] {
					t.Fatalf("env = %#v", got)
				}
				if _, ok := base["GOMEMLIMIT"]; ok {
					t.Fatal("mutated base environment")
				}
				if len(base) != 2 {
					t.Fatal("mutated input map")
				}
			})
		}
	}
}

func TestQualityGateAllCommandsReceiveMemoryBudget(t *testing.T) {
	for _, token := range []string{"", "token"} {
		t.Run(token, func(t *testing.T) {
			fb := &qgFakeBackend{fakeBackend: fakeBackend{statuses: map[string]*fakeStatus{}}, commandResults: map[string]*backend.ExecResult{}}
			m := newQGTestManager(t, fb, "go")
			m.cfg.defaultMemMB = 6144
			result, err := m.handleQualityGate(context.Background(), map[string]any{
				"project": "test-project", "checks": []any{"lint", "test"},
				"extra_test_commands": []any{"golangci-lint run --config .golangci.yml ./cmd/test", "go test ./cmd/test"},
				"env":                 map[string]any{"GIT_TOKEN": token},
			})
			if err != nil || result.IsError {
				t.Fatalf("result=%+v error=%v", result, err)
			}
			if len(fb.calls) != 4 {
				t.Fatalf("calls = %d", len(fb.calls))
			}
			for _, call := range fb.calls {
				if call.Env["GOMEMLIMIT"] != "5120MiB" {
					t.Fatalf("%s env = %#v", call.Command, call.Env)
				}
			}
		})
	}
}

func TestQualityDiagnosticTailStableCheckout(t *testing.T) {
	line := "fatal: Unable to create '/workspace/repo.mills-tests/.git/index.lock': File exists."
	got := qualityDiagnosticTail(line+"\n"+strings.Repeat("noise\n", 100), 200)
	if !strings.Contains(got, line) {
		t.Fatalf("lost diagnostic: %s", got)
	}
}

func TestTestRevisionWorkDirUnconfiguredGitCloneUsesSharedIsolation(t *testing.T) {
	m := &manager{cfg: managerConfig{backendType: "k8s", syncMode: "git-clone", workspaceRoot: "/projects"}}
	if got := m.testRevisionWorkDir("/projects/a", "run"); got != "/workspace/a.mills-tests-run" {
		t.Fatal(got)
	}
}

func TestCheckoutTestRevisionStablePathVerifiesSHA(t *testing.T) {
	const expected = "2222222222222222222222222222222222222222"
	for _, actual := range []string{expected, strings.Repeat("3", 40)} {
		t.Run(actual, func(t *testing.T) {
			fb := &qgScriptedBackend{results: []*backend.ExecResult{{StdoutTail: actual}}, errors: []error{nil}}
			m := &manager{
				cfg:     managerConfig{backendType: "k8s", syncMode: "git-clone", gitBaseURL: "https://git.example", gitSecret: "git", workspaceRoot: "/projects"},
				backend: fb,
			}
			for _, run := range []string{"PIPE-1", "PIPE-2"} {
				sha, dir, err := m.checkoutTestRevision(context.Background(), run, "/projects/a", "feat/test", expected, map[string]string{"LOOM_MILLS_RUN_ID": run})
				if sha != actual || dir != "/workspace/a.mills-tests" {
					t.Fatalf("sha=%q dir=%q", sha, dir)
				}
				if (err != nil) != (actual != expected) {
					t.Fatalf("error = %v", err)
				}
			}
		})
	}
}

func TestQualityGate_ParityOutputContract(t *testing.T) {
	for _, tc := range []struct {
		name, stdout, stderr string
		exit                 int
		degraded             bool
	}{
		{"both streams", "handler.go:1: use CommandContext (noctx)", "stderr diagnostic", 1, false},
		{"empty", "", "", 1, true},
		{"whitespace", " \n", "\t", 1, true},
		{"missing binary", "", "", 127, true},
		{"zero findings", "0 issues.", "", 7, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := "golangci-lint run --config .golangci.yml './pkg/a'"
			fb := &qgFakeBackend{fakeBackend: fakeBackend{statuses: map[string]*fakeStatus{}}, commandResults: map[string]*backend.ExecResult{cmd: {ExitCode: tc.exit, StdoutTail: tc.stdout, StderrTail: tc.stderr}}}
			got := newQGTestManager(t, fb, "go").runQualityCheck(t.Context(), "sandbox", "/project", "project", "go", "test:0", cmd, nil, 10)
			if got.Degraded != tc.degraded {
				t.Fatalf("result = %+v", got)
			}
			if tc.exit == 1 && tc.degraded && (got.Passed || got.FailureSignature != "lint_parity_no_output" || got.Warning == "") {
				t.Fatalf("empty failure = %+v", got)
			}
			if !tc.degraded && (!strings.Contains(got.OutputTail, "noctx") || !strings.Contains(got.OutputTail, tc.stderr)) {
				t.Fatalf("streams lost: %+v", got)
			}
		})
	}
}

func TestQualityGate_ParityTailBoundedAndRedacted(t *testing.T) {
	const secret = "sensitive-test-token"
	cmd := "golangci-lint run --config .golangci.yml './pkg/a'"
	fb := &qgFakeBackend{fakeBackend: fakeBackend{statuses: map[string]*fakeStatus{}}, commandResults: map[string]*backend.ExecResult{cmd: {ExitCode: 1, StdoutTail: "old-output\n" + strings.Repeat("界", 4000) + "\nuse CommandContext (noctx)", StderrTail: "diagnostic " + secret}}}
	got := newQGTestManager(t, fb, "go").runQualityCheck(t.Context(), "sandbox", "/project", "project", "go", "test:0", cmd, map[string]string{"GIT_TOKEN": secret}, 10)
	if len(got.OutputTail) > 8*1024 || !utf8.ValidString(got.OutputTail) || strings.Contains(got.OutputTail, secret) || strings.Contains(got.OutputTail, "old-output") || !strings.Contains(got.OutputTail, "noctx") || !strings.Contains(got.OutputTail, "diagnostic") {
		t.Fatalf("bad tail: %q", got.OutputTail)
	}
}

// This backend models Kubernetes exec: only sandbox termination releases it.
type blockingGateBackend struct {
	fakeBackend
	entered     chan struct{}
	terminated  chan struct{}
	stopRelease chan struct{}
	calls       int
	once        sync.Once
}

func (b *blockingGateBackend) Exec(_ context.Context, _ backend.ExecOpts) (*backend.ExecResult, error) {
	b.calls++
	if b.calls == 1 {
		close(b.entered)
	}
	<-b.terminated
	return &backend.ExecResult{ExitCode: 0}, nil
}
func (b *blockingGateBackend) Stop(ctx context.Context, _ string) error {
	select {
	case <-b.entered:
	default:
		return nil
	}
	b.once.Do(func() { close(b.terminated) })
	if b.stopRelease != nil {
		select {
		case <-b.stopRelease:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}
func TestQualityGate_CancelTerminatesDetachedExecAndDrains(t *testing.T) {
	for _, scenario := range []struct {
		stopTool bool
		lang     string
	}{{false, "python"}, {true, "python"}, {false, "go"}, {true, "go"}} {
		stopTool := scenario.stopTool
		t.Run(fmt.Sprint(stopTool)+scenario.lang, func(t *testing.T) {
			t.Setenv("LOOM_MCP_OUTPUT_FORMAT", "json")
			b := &blockingGateBackend{fakeBackend: fakeBackend{statuses: map[string]*fakeStatus{}}, entered: make(chan struct{}), terminated: make(chan struct{}), stopRelease: make(chan struct{})}
			m := newQGTestManager(t, b, scenario.lang)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan *mcp.CallToolResult, 1)
			go func() {
				r, _ := m.handleQualityGate(ctx, map[string]any{"project": "test-project", "fail_fast": false})
				done <- r
			}()
			select {
			case <-b.entered:
			case <-time.After(5 * time.Second):
				t.Fatal("exec did not start")
			}
			stopDone := make(chan *mcp.CallToolResult, 1)
			if stopTool {
				go func() {
					r, _ := m.handleStop(context.Background(), map[string]any{"project": "test-project"})
					stopDone <- r
				}()
			} else {
				cancel()
			}
			select {
			case <-b.terminated:
			case <-time.After(5 * time.Second):
				t.Fatal("cancellation did not terminate exec")
			}
			if m.projectLock(storeKey("test-project", "")).TryLock() {
				m.projectLock(storeKey("test-project", "")).Unlock()
				t.Fatal("replacement allowed before cleanup")
			}
			other := m.projectLock(storeKey("test-project", "unrelated-agent"))
			if !other.TryLock() {
				t.Fatal("unrelated sandbox blocked")
			}
			other.Unlock()
			select {
			case <-done:
				t.Fatal("gate returned before cleanup completed")
			default:
			}
			close(b.stopRelease)
			select {
			case r := <-done:
				data, _ := json.Marshal(r)
				if !strings.Contains(string(data), "cancelled") {
					t.Fatalf("missing cancellation: %s", data)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("gate did not drain")
			}
			if stopTool {
				select {
				case r := <-stopDone:
					if r.IsError {
						t.Fatalf("stop: %+v", r)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("stop did not drain")
				}
			}
			if b.calls != 1 {
				t.Fatalf("continued after cancel: %d execs", b.calls)
			}
		})
	}
}

func TestQualityGate_UnconfirmedCleanupBlocksReuse(t *testing.T) {
	b := &qgFakeBackend{fakeBackend: fakeBackend{statuses: map[string]*fakeStatus{}}}
	m := newQGTestManager(t, b, "python")
	key := storeKey("test-project", "")
	m.gateCleanupFailures.Store(key, errors.New("delete denied"))
	r, err := m.handleQualityGate(context.Background(), map[string]any{"project": "test-project"})
	if err != nil || r == nil || !r.IsError || len(b.calls) != 0 {
		t.Fatalf("gate reused unconfirmed sandbox: %+v, %v", r, err)
	}
	r, err = m.handleStop(context.Background(), map[string]any{"project": "test-project"})
	if err != nil || r.IsError {
		t.Fatalf("stop: %+v, %v", r, err)
	}
	if _, ok := m.gateCleanupFailures.Load(key); ok {
		t.Fatal("confirmed stop did not clear failure")
	}
	r, err = m.handleQualityGate(context.Background(), map[string]any{"project": "test-project"})
	if err != nil || r.IsError || len(b.calls) == 0 {
		t.Fatalf("gate did not recover after stop: %+v, %v", r, err)
	}
}
