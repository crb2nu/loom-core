package main

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/crb2nu/loom/pkg/sync"
)

// newSyncMirrorCmd returns the `loom sync mirror` command. It supports:
//   - default (no flag): report drift, exit 0 always
//   - --check: report drift, exit 1 if drift (CI/pre-commit gate)
//   - --apply: copy canonical files into the mirror
//   - --dry-run with --apply: show what would change without writing
func newSyncMirrorCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mirror",
		Short: "Sync canonical mcp/context registries to platform/gitops mirror",
		Long: `Keep platform/gitops/mcp/context/{registry.yaml,skills-registry.yaml} in
sync with the canonical services/loom-core/mcp/context source.

By default, prints a drift summary. Use --check to exit non-zero on drift
(for pre-commit / CI gates) or --apply to write the canonical files into
the mirror. The canonical source is always the winner: mirror-only
additions are overwritten.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			check, _ := cmd.Flags().GetBool("check")
			apply, _ := cmd.Flags().GetBool("apply")
			dryRun, _ := cmd.Flags().GetBool("dry-run")

			cwd, _ := os.Getwd()
			mgr, err := sync.NewManager(cwd)
			if err != nil {
				return err
			}

			if apply {
				updated, status, syncErr := mgr.SyncMirror(dryRun)
				if syncErr != nil {
					return syncErr
				}
				action := "synced"
				if dryRun {
					action = "would sync"
				}
				printMirrorStatus(cmd.OutOrStdout(), status)
				fmt.Fprintf(cmd.OutOrStdout(), "\n%s %d file(s) (canonical → %s)\n", action, updated, status.MirrorRoot)
				return nil
			}

			status, err := mgr.GetMirrorStatus()
			if err != nil {
				return err
			}
			printMirrorStatus(cmd.OutOrStdout(), status)

			if check && !status.InSync {
				return fmt.Errorf("mirror drift detected; run `loom sync mirror --apply` to fix")
			}
			return nil
		},
	}
	cmd.Flags().Bool("check", false, "Exit non-zero if mirror is out of sync (CI/pre-commit gate)")
	cmd.Flags().Bool("apply", false, "Copy canonical files into the platform/gitops mirror")
	cmd.Flags().Bool("dry-run", false, "With --apply, show what would change without writing")
	return cmd
}

func printMirrorStatus(w io.Writer, status *sync.MirrorStatus) {
	fmt.Fprintf(w, "Source: %s\nMirror: %s\n\n", status.SourceRoot, status.MirrorRoot)
	fmt.Fprintf(w, "%-40s %-10s %s\n", "FILE", "STATUS", "DETAIL")
	fmt.Fprintf(w, "%-40s %-10s %s\n", "----", "------", "------")
	for _, f := range status.Files {
		state := "in-sync"
		detail := ""
		switch {
		case !f.SourceExists:
			state = "missing-source"
		case !f.MirrorExists:
			state = "missing-mirror"
			detail = "(canonical → mirror needed)"
		case !f.InSync:
			state = "drift"
			detail = fmt.Sprintf("(~%d line diff)", f.DiffLineCount)
		}
		fmt.Fprintf(w, "%-40s %-10s %s\n", f.RelPath, state, detail)
	}
}
