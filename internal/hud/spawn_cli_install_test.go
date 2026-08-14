package hud

import (
	"fmt"
	"strings"
	"testing"
)

// TestAgentCLIInstallShell_GuardedAndPinned asserts the live install snippet is
// guarded (idempotent / no-op when the CLI exists), uses sudo for the system
// npm prefix, and pins the same versions as the Dockerfile-form installer.
func TestAgentCLIInstallShell_GuardedAndPinned(t *testing.T) {
	cases := []struct {
		agentType string
		guard     string
		pkg       string
		version   string
	}{
		{"codex", "command -v codex", "@openai/codex", codexVersion},
		{"claude-code", "command -v claude", "@anthropic-ai/claude-code", claudeCodeVersion},
		{"gemini", "command -v gemini", "@google/gemini-cli", geminiVersion},
	}
	for _, tc := range cases {
		t.Run(tc.agentType, func(t *testing.T) {
			got := agentCLIInstallShell(tc.agentType)
			wantPkg := fmt.Sprintf("sudo npm install -g %s@%s", tc.pkg, tc.version)
			for _, want := range []string{tc.guard, wantPkg, "command -v npm"} {
				if !strings.Contains(got, want) {
					t.Errorf("snippet for %s missing %q:\n%s", tc.agentType, want, got)
				}
			}
		})
	}
}

func TestClaudeCodeVersion_CurrentRuntimeFloor(t *testing.T) {
	if claudeCodeVersion != "2.1.220" {
		t.Fatalf("claudeCodeVersion = %q, want 2.1.220", claudeCodeVersion)
	}
	for _, install := range []string{agentCLIInstallLines("claude-code"), agentCLIInstallShell("claude-code")} {
		if !strings.Contains(install, "@anthropic-ai/claude-code@2.1.220") {
			t.Errorf("Claude install did not pin 2.1.220: %s", install)
		}
	}
}

// TestAgentCLIInstallShell_AptWaitsForDpkgLock guards the Mills A2 probe
// 2026-06-18 regression: on the harvester-vm path this snippet runs over SSH at
// Start and races cloud-init's first-boot apt for the dpkg lock. The npm
// bootstrap apt-get must pass DPkg::Lock::Timeout so it waits rather than
// failing with "Could not get lock /var/lib/dpkg/lock-frontend".
func TestAgentCLIInstallShell_AptWaitsForDpkgLock(t *testing.T) {
	for _, agentType := range []string{"codex", "claude-code", "gemini"} {
		got := agentCLIInstallShell(agentType)
		for _, want := range []string{
			"apt-get -o DPkg::Lock::Timeout=300 update",
			"apt-get -o DPkg::Lock::Timeout=300 install -y nodejs npm",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("snippet for %s missing dpkg-lock wait %q:\n%s", agentType, want, got)
			}
		}
	}
}

// TestAgentCLIInstallShell_InstallsGoToolchain asserts each known agent's
// snippet also installs the pinned Go toolchain. The harvester-vm substrate
// boots a stock Ubuntu cloud image with no Go, so a Mills implement spawn that
// lands Go changes would hit "go: not found" and could not `go build`/`go test`
// the way the k8s backend (golang base image) does — the explicit out-of-scope
// follow-up from the k8s build-env fix (!843). The install must be guarded
// (no-op when go exists), pin goVersion (matching go.mod/CI so no GOTOOLCHAIN
// auto-download), and wire go onto PATH via /usr/local/bin.
func TestAgentCLIInstallShell_InstallsGoToolchain(t *testing.T) {
	wantDownload := fmt.Sprintf("https://go.dev/dl/go%s.linux-${goarch}.tar.gz", goVersion)
	for _, agentType := range []string{"codex", "claude-code", "gemini"} {
		got := agentCLIInstallShell(agentType)
		for _, want := range []string{
			"command -v go",          // idempotent guard
			wantDownload,             // pinned official tarball
			"tar -C /usr/local -xzf", // canonical install location
			"ln -sf /usr/local/go/bin/go /usr/local/bin/go", // go onto PATH
		} {
			if !strings.Contains(got, want) {
				t.Errorf("snippet for %s missing Go install %q:\n%s", agentType, want, got)
			}
		}
	}
}

func TestAgentCLIInstallShell_UnknownIsEmpty(t *testing.T) {
	if got := agentCLIInstallShell("mystery-agent"); got != "" {
		t.Errorf("unknown agent type returned %q, want empty", got)
	}
}
