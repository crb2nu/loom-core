package main

import (
	"strings"
	"testing"
)

func TestNewSyncCmdPreservesRootFlags(t *testing.T) {
	cmd := newSyncCmd()

	tests := []struct {
		name      string
		defValue  string
		usagePart string
	}{
		{name: "regen", defValue: "false", usagePart: "Regenerate configuration from registry before syncing"},
		{name: "repo-only", defValue: "false", usagePart: "Only update repository configuration, do not sync to home"},
		{name: "hub-mode", defValue: "false", usagePart: "Generate configs for MCP Hub"},
		{name: "hub-url", defValue: "wss://mcp.flexinfer.ai/ws", usagePart: "MCP Hub WebSocket URL"},
		{name: "loom-mode", defValue: "false", usagePart: "Generate single loom proxy entry"},
		{name: "loom-binary", defValue: "", usagePart: "Path to loom binary"},
		{name: "skip-skills", defValue: "false", usagePart: "Skip skills generation during --regen"},
		{name: "resolve-secrets", defValue: "false", usagePart: "Resolve secret templates to literal values"},
		{name: "all-projects", defValue: "false", usagePart: "Propagate hooks to all workspace projects"},
		{name: "workspace-root", defValue: "", usagePart: "Explicit workspace root (default: auto-detect)"},
		{name: "skip-worktrees", defValue: "false", usagePart: "Skip .worktrees/ during project discovery"},
		{name: "host", defValue: "", usagePart: "Host profile for registry overrides (e.g. code-server). Sets $LOOM_HOST."},
		{name: "dry-run", defValue: "false", usagePart: "Show what would change without writing"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			flag := cmd.Flags().Lookup(tt.name)
			if flag == nil {
				t.Fatalf("missing flag %q", tt.name)
			}
			if flag.DefValue != tt.defValue {
				t.Fatalf("flag %q default = %q, want %q", tt.name, flag.DefValue, tt.defValue)
			}
			if !strings.Contains(flag.Usage, tt.usagePart) {
				t.Fatalf("flag %q usage = %q, want to contain %q", tt.name, flag.Usage, tt.usagePart)
			}
		})
	}
}

func TestNewSyncCmdPreservesSubcommandWiring(t *testing.T) {
	cmd := newSyncCmd()

	for _, name := range []string{"skills", "status", "agent-tokens", "mirror"} {
		if sub, _, err := cmd.Find([]string{name}); err != nil || sub == cmd || sub.Name() != name {
			t.Fatalf("expected sync subcommand %q, got sub=%v err=%v", name, sub, err)
		}
	}
}
