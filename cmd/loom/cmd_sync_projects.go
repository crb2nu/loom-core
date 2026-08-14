package main

import (
	"fmt"
	"sort"

	"github.com/crb2nu/loom/pkg/generator"
	"github.com/crb2nu/loom/pkg/sync"
)

func propagateSyncAllProjects(mgr *sync.Manager, profile, cwd, wsRoot string, skipWorktrees, dryRun bool) error {
	if wsRoot == "" {
		wsRoot = generator.InferWorkspaceRoot(cwd)
	}
	if wsRoot == "" {
		return fmt.Errorf("cannot detect workspace root; use --workspace-root")
	}

	if profile == "all" {
		names := mgr.List()
		sort.Strings(names)
		for _, name := range names {
			if err := propagateSyncProfileToProjects(mgr, name, wsRoot, skipWorktrees, dryRun); err != nil {
				return err
			}
		}
		return nil
	}
	return propagateSyncProfileToProjects(mgr, profile, wsRoot, skipWorktrees, dryRun)
}

func propagateSyncProfileToProjects(mgr *sync.Manager, pName, wsRoot string, skipWorktrees, dryRun bool) error {
	p := mgr.Get(pName)
	if p == nil {
		return nil
	}
	totalUpdated := 0

	if len(p.HomeManagedSettingsKeys) > 0 {
		fmt.Printf("\nStripping %s home-managed settings from workspace projects (approvals/hooks live at user level only):\n", pName)
		n, err := mgr.SyncAllProjects(pName, wsRoot, skipWorktrees, dryRun)
		if err != nil {
			return fmt.Errorf("propagate %s settings: %w", pName, err)
		}
		totalUpdated += n
	}

	if p.GeneratedDirectToHome {
		fmt.Printf("\nRemoving stale %s generated config from workspace projects (home-level config is authoritative):\n", pName)
		n, err := mgr.CleanAllProjectsGenerated(pName, wsRoot, skipWorktrees, dryRun)
		if err != nil {
			return fmt.Errorf("propagate %s generated cleanup: %w", pName, err)
		}
		totalUpdated += n
	}

	if totalUpdated == 0 {
		fmt.Println("  All projects already up-to-date.")
	} else if dryRun {
		fmt.Printf("  %d project(s) would be updated.\n", totalUpdated)
	} else {
		fmt.Printf("  %d project(s) updated.\n", totalUpdated)
	}
	return nil
}
