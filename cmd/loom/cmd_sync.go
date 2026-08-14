package main

import "github.com/spf13/cobra"

// newSyncCmd creates the sync command and its subcommands.
func newSyncCmd() *cobra.Command {
	syncCmd := &cobra.Command{
		Use:   "sync [profile]",
		Short: "Sync configuration from repo to home",
		Args:  cobra.ExactArgs(1),
		RunE:  runSyncCmd,
	}

	syncCmd.Flags().Bool("regen", false, "Regenerate configuration from registry before syncing")
	syncCmd.Flags().Bool("repo-only", false, "Only update repository configuration, do not sync to home")
	syncCmd.Flags().Bool("hub-mode", false, "Generate configs for MCP Hub")
	syncCmd.Flags().String("hub-url", "wss://mcp.flexinfer.ai/ws", "MCP Hub WebSocket URL")
	syncCmd.Flags().Bool("loom-mode", false, "Generate single loom proxy entry")
	syncCmd.Flags().String("loom-binary", "", "Path to loom binary")
	syncCmd.Flags().Bool("skip-skills", false, "Skip skills generation during --regen")
	syncCmd.Flags().Bool("resolve-secrets", false, "Resolve secret templates to literal values")
	syncCmd.Flags().Bool("all-projects", false, "Propagate hooks to all workspace projects")
	syncCmd.Flags().String("workspace-root", "", "Explicit workspace root (default: auto-detect)")
	syncCmd.Flags().Bool("skip-worktrees", false, "Skip .worktrees/ during project discovery")
	syncCmd.Flags().String("host", "", "Host profile for registry overrides (e.g. code-server). Sets $LOOM_HOST.")
	syncCmd.Flags().Bool("dry-run", false, "Show what would change without writing")

	syncCmd.AddCommand(newSyncSkillsCmd())

	syncCmd.AddCommand(newSyncStatusCmd())

	// Agent token sync subcommand
	syncCmd.AddCommand(newSyncAgentTokensCmd())

	// Mirror sync subcommand: keep platform/gitops/mcp/context/* in lockstep
	// with the canonical services/loom-core/mcp/context/* source.
	syncCmd.AddCommand(newSyncMirrorCmd())

	return syncCmd
}
