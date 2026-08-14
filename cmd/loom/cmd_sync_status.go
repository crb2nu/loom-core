package main

import (
	"fmt"
	"os"
	"sort"

	"github.com/spf13/cobra"

	"github.com/crb2nu/loom/pkg/sync"
)

func newSyncStatusCmd() *cobra.Command {
	syncStatusCmd := &cobra.Command{
		Use:   "status",
		Short: "Show sync status for all profiles",
		RunE: func(cmd *cobra.Command, args []string) error {
			cwd, _ := os.Getwd()
			mgr, err := sync.NewManager(cwd)
			if err != nil {
				return err
			}

			statuses, err := mgr.GetAllSyncStatus()
			if err != nil {
				return err
			}

			fmt.Printf("%-16s %-8s %-8s %s\n", "Profile", "Repo", "Home", "Status")
			fmt.Printf("%-16s %-8s %-8s %s\n", "-------", "----", "----", "------")

			names := make([]string, 0, len(statuses))
			for name := range statuses {
				names = append(names, name)
			}
			sort.Strings(names)

			for _, name := range names {
				s := statuses[name]
				repoStatus := "missing"
				if s.RepoExists {
					repoStatus = "ok"
				}
				homeStatus := "missing"
				if s.HomeExists {
					homeStatus = "ok"
				}
				syncStatus := "in-sync"
				if !s.InSync {
					syncStatus = "drift"
				}
				fmt.Printf("%-16s %-8s %-8s %s\n", name, repoStatus, homeStatus, syncStatus)
			}

			// Also surface mirror drift for the canonical mcp/context registries
			// → platform/gitops mirror. Best-effort: if the mirror isn't present
			// (no platform/gitops checkout), skip silently.
			if mirrorStatus, mErr := mgr.GetMirrorStatus(); mErr == nil {
				state := "in-sync"
				if !mirrorStatus.InSync {
					state = "drift"
				}
				fmt.Printf("%-16s %-8s %-8s %s\n", "gitops-mirror", "ok", "ok", state)
			}

			return nil
		},
	}
	return syncStatusCmd
}
