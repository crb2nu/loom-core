package main

import (
	"os"

	"github.com/spf13/cobra"

	"github.com/crb2nu/loom/pkg/sync"
)

func newBackupCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "backup [profile]",
		Short: "Backup configuration",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			profile := args[0]
			cwd, _ := os.Getwd()
			mgr, err := sync.NewManager(cwd)
			if err != nil {
				return err
			}
			return mgr.Backup(profile, "home")
		},
	}
}
