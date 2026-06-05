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

func TestAgentCLIInstallShell_UnknownIsEmpty(t *testing.T) {
	if got := agentCLIInstallShell("mystery-agent"); got != "" {
		t.Errorf("unknown agent type returned %q, want empty", got)
	}
}
