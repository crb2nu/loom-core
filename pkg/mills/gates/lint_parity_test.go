package gates

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestTouchedGoPackages(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"pkg/a/a.go", "pkg/b/b_test.go", "root.go"} {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("package p\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got := TouchedGoPackages(root, []string{
		"pkg/b/b_test.go", "pkg/a/a.go", "pkg/a/second.go", "pkg/a/a.go",
		"README.md", "deleted/gone.go", "../escape.go", "root.go",
	})
	want := []string{".", "./pkg/a", "./pkg/b"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("packages = %v, want %v", got, want)
	}
}

func TestLintParityCommand(t *testing.T) {
	got := LintParityCommand([]string{"./pkg/a", "./pkg/b"})
	want := "CGO_ENABLED=0 GOWORK=off golangci-lint run --config .golangci.yml --allow-parallel-runners './pkg/a' './pkg/b' 2>&1"
	if got != want {
		t.Fatalf("command = %q, want %q", got, want)
	}
	// The devbox gate and the dispatcher's baseline oracle recognise the
	// parity check by this substring; the parallel-runners flag must not
	// break it.
	if !strings.Contains(got, "golangci-lint run --config .golangci.yml ") {
		t.Fatalf("command %q lost the parity-check marker substring", got)
	}
	if got := LintParityCommand(nil); got != "" {
		t.Fatalf("empty command = %q", got)
	}
	got = LintParityCommand([]string{"./pkg/a b", "./pkg/x'; echo injected; '"})
	want = "CGO_ENABLED=0 GOWORK=off golangci-lint run --config .golangci.yml --allow-parallel-runners './pkg/a b' './pkg/x'\"'\"'; echo injected; '\"'\"'' 2>&1"
	if got != want {
		t.Fatalf("quoted command = %q, want %q", got, want)
	}
}

func TestLintParityCommandCapturesStderr(t *testing.T) {
	binDir := t.TempDir()
	linter := filepath.Join(binDir, "golangci-lint")
	script := "#!/bin/sh\nprintf 'pkg/x.go:1:1: finding (lint)\\n'\nprintf 'level=error msg=\"failed to load packages\"\\n' >&2\nexit 7\n"
	if err := os.WriteFile(linter, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.CommandContext(t.Context(), "sh", "-c", LintParityCommand([]string{"./pkg/x"}))
	cmd.Env = append(os.Environ(), "PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	output, err := cmd.Output()
	if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() != 7 {
		t.Fatalf("command error = %v, want exit 7", err)
	}
	for _, want := range []string{"finding (lint)", "failed to load packages"} {
		if !strings.Contains(string(output), want) {
			t.Fatalf("captured output %q missing %q", output, want)
		}
	}
}

func TestClassifyLintParity(t *testing.T) {
	tests := []struct {
		name             string
		exit             int
		output           string
		passed, degraded bool
		warning          string
	}{
		{name: "pass", passed: true},
		{name: "QueryRow noctx", exit: 1, output: "db.QueryRow must be QueryRowContext (noctx)"},
		{name: "Exec noctx", exit: 1, output: "db.Exec must be ExecContext (noctx)"},
		{name: "unused", exit: 1, output: "value is unused (unused)"},
		{name: "exit 7 load error with no findings", exit: 7, output: "level=error msg=\"Running error: context loading failed: failed to load packages\"\n0 issues.", passed: true, degraded: true, warning: LintParityInfraWarning},
		{name: "exit 7 typecheck error with no findings", exit: 7, output: "level=error msg=\"typechecking error: could not import example/module\"\n0 issues", passed: true, degraded: true, warning: LintParityInfraWarning},
		{name: "exit 7 with genuine finding", exit: 7, output: "pkg/x.go:12:2: value is unused (unused)\n1 issues", warning: ""},
		{name: "exit 7 with double-digit findings", exit: 7, output: "pkg/x.go:12:2: value is unused (unused)\n10 issues", warning: ""},
		{name: "exit 7 zero findings without infra evidence", exit: 7, output: "0 issues.", passed: true, degraded: true, warning: LintParityInfraWarning},
		{name: "exit 137 process kill", exit: 137, output: "command terminated with exit code 137", passed: true, degraded: true, warning: LintParityInfraWarning},
		{name: "unavailable exit", exit: 127, output: "sh: golangci-lint: not found", passed: true, degraded: true, warning: LintParityUnavailableWarning},
		// The shared per-repo sandbox pod serves concurrent runs; golangci-lint's
		// start-up lock makes the second runner exit 3 before linting anything.
		{name: "exit 3 parallel runner lock", exit: 3, output: "$ CGO_ENABLED=0 GOWORK=off golangci-lint run --config .golangci.yml './internal/hud' 2>&1\nError: parallel golangci-lint is running\nThe command is terminated due to an error: parallel golangci-lint is running", passed: true, degraded: true, warning: LintParityParallelWarning},
		{name: "exit 3 real config error stays a failure", exit: 3, output: "Error: can't load config: unsupported version of the configuration", warning: ""},
		{name: "exit 3 v1 binary with v2 config stays a failure", exit: 3, output: "Error: you are using a configuration file for golangci-lint v2 with golangci-lint v1: please use golangci-lint v2", warning: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyLintParity(tc.exit, tc.output)
			if got.Passed != tc.passed || got.Degraded != tc.degraded {
				t.Fatalf("result = %+v", got)
			}
			if tc.degraded && got.Warning == "" {
				t.Fatal("degradation must carry a warning")
			}
			if got.Warning != tc.warning {
				t.Fatalf("warning = %q, want %q", got.Warning, tc.warning)
			}
			if tc.name != "unavailable exit" && got.Output != tc.output {
				t.Fatalf("output = %q", got.Output)
			}
		})
	}
}

func TestClassifyLintParityEmptyFailure(t *testing.T) {
	for _, output := range []string{"", " \n\t"} {
		got := ClassifyLintParity(1, output)
		if got.Passed || !got.Degraded || got.Warning != LintParityInfraWarning || got.FailureSignature != LintParityNoOutput {
			t.Fatalf("result=%+v", got)
		}
	}
}
