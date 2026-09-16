package gates

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const LintParityNoOutput = "lint_parity_no_output"

const LintParityCheckName = "lint:parity"

const LintParityUnavailableWarning = "golangci-lint unavailable; CI-parity lint skipped"

const LintParityInfraWarning = "golangci-lint did not produce a lint verdict; CI-parity lint degraded"

// LintParityParallelWarning is the degradation reason when golangci-lint
// refused to start because another instance held its start-up file lock in
// the same sandbox. The shared per-repo sandbox pod serves every concurrent
// Mills run, so two tests stages linting different packages collide on that
// lock; the command in LintParityCommand disables the lock, and this
// classification covers the skew window where an older operator image still
// emits the lock-taking command.
const LintParityParallelWarning = "golangci-lint refused to start: another instance held its lock in the shared sandbox; CI-parity lint degraded"

// lintParityParallelNeedle is golangci-lint's verbatim refusal (exit 3):
// "Error: parallel golangci-lint is running".
const lintParityParallelNeedle = "parallel golangci-lint is running"

// LintParityResult distinguishes a real lint verdict from degradation
// when the sandbox cannot produce a verdict.
type LintParityResult struct {
	FailureSignature string
	Passed           bool
	Degraded         bool
	Warning          string
	Output           string
}

// ClassifyLintParity converts the sandbox process result into the gate's
// stable pass/fail/degraded contract. Absence of the executable, exit 7 runs
// that report zero findings, exit 137 process kills, and the parallel-runner
// lock refusal degrade instead of becoming code failures: none produced a
// lint verdict. Empty nonzero results degrade with a retryable infrastructure
// signature and remain non-passing so they cannot silently satisfy the gate.
// Genuine findings, config errors, timeouts, noctx, unused, and
// every other lint failure remain failures.
func ClassifyLintParity(exitCode int, output string) LintParityResult {
	trimmed := strings.TrimSpace(output)
	if exitCode == 127 || strings.Contains(trimmed, "golangci-lint: not found") {
		return LintParityResult{Passed: true, Degraded: true, Warning: LintParityUnavailableWarning, Output: LintParityUnavailableWarning}
	}
	if exitCode != 0 && trimmed == "" {
		return LintParityResult{Degraded: true, Warning: LintParityInfraWarning, FailureSignature: LintParityNoOutput, Output: LintParityNoOutput + ": " + LintParityInfraWarning}
	}
	if exitCode == 7 && lintParityZeroFindings(trimmed) {
		return LintParityResult{Passed: true, Degraded: true, Warning: LintParityInfraWarning, Output: trimmed}
	}
	if exitCode == 137 {
		return LintParityResult{Passed: true, Degraded: true, Warning: LintParityInfraWarning, Output: trimmed}
	}
	if exitCode != 0 && strings.Contains(trimmed, lintParityParallelNeedle) {
		return LintParityResult{Passed: true, Degraded: true, Warning: LintParityParallelWarning, Output: trimmed}
	}
	return LintParityResult{Passed: exitCode == 0, Output: trimmed}
}

func lintParityZeroFindings(output string) bool {
	fields := strings.Fields(strings.ToLower(output))
	for i := 0; i+1 < len(fields); i++ {
		if fields[i] == "0" && strings.TrimRight(fields[i+1], ".:;,!") == "issues" {
			return true
		}
	}
	return false
}

// TouchedGoPackages resolves changed Go files to the smallest set of package
// arguments golangci-lint accepts. Deleted files and paths outside repoRoot are
// ignored; callers may pass an empty repoRoot when existence checks are not
// available (for example before a remote sandbox clone is addressed).
func TouchedGoPackages(repoRoot string, changed []string) []string {
	seen := make(map[string]struct{})
	for _, name := range changed {
		name = filepath.ToSlash(filepath.Clean(strings.TrimSpace(name)))
		if name == "." || strings.HasPrefix(name, "../") || filepath.IsAbs(name) || !strings.HasSuffix(name, ".go") {
			continue
		}
		if repoRoot != "" {
			if _, err := os.Stat(filepath.Join(repoRoot, filepath.FromSlash(name))); err != nil {
				continue
			}
		}
		dir := filepath.ToSlash(filepath.Dir(name))
		pattern := "."
		if dir != "." {
			pattern = "./" + dir
		}
		seen[pattern] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for pattern := range seen {
		out = append(out, pattern)
	}
	sort.Strings(out)
	return out
}

// LintParityCommand uses the checkout's canonical config and only package
// arguments derived from the implement diff. Package patterns are produced by
// TouchedGoPackages, so no shell quoting of user-controlled input is needed.
// CGO_ENABLED=0 rides in the command itself, not just the sandbox exec env:
// the command must stay correct on sandbox images whose environment plumbing
// predates the env default, or typechecking any cgo-bearing dependency (e.g.
// fi-accel's fi_accel.h) fails on a missing C toolchain header.
//
// --allow-parallel-runners disables golangci-lint's start-up file lock. Every
// concurrent Mills run executes its tests stage in the same per-repo sandbox
// pod, and with the lock in place the second runner exits 3 with "parallel
// golangci-lint is running" before linting anything (21 tests attempts
// 2026-09-06..12, each escalated as a code failure). The flag is accepted by
// both golangci-lint v1 and v2, and the build cache underneath is safe to
// share. The flag follows the config path on purpose: cmd/mcp-devbox and the
// dispatcher recognise the parity check by the substring
// "golangci-lint run --config .golangci.yml ".
func LintParityCommand(packages []string) string {
	if len(packages) == 0 {
		return ""
	}
	quoted := make([]string, 0, len(packages))
	for _, pkg := range packages {
		quoted = append(quoted, shellQuote(pkg))
	}
	return fmt.Sprintf("CGO_ENABLED=0 GOWORK=off golangci-lint run --config .golangci.yml --allow-parallel-runners %s 2>&1", strings.Join(quoted, " "))
}

// shellQuote preserves package paths as single arguments when the devbox
// backend executes the command through sh -c. Git permits whitespace and shell
// metacharacters in path names, so even internally-derived paths must be quoted.
func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
