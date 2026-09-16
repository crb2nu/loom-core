package main

import (
	"fmt"
	"os"
	"sort"

	"github.com/spf13/cobra"

	"github.com/crb2nu/loom/pkg/sync"
)

func runSyncCmd(cmd *cobra.Command, args []string) error {
	profile := args[0]
	regen, _ := cmd.Flags().GetBool("regen")
	repoOnly, _ := cmd.Flags().GetBool("repo-only")
	hubMode, _ := cmd.Flags().GetBool("hub-mode")
	hubURL, _ := cmd.Flags().GetString("hub-url")
	loomMode, _ := cmd.Flags().GetBool("loom-mode")
	loomBinary, _ := cmd.Flags().GetString("loom-binary")
	resolveSecrets, _ := cmd.Flags().GetBool("resolve-secrets")
	skipSkills, _ := cmd.Flags().GetBool("skip-skills")
	allProjects, _ := cmd.Flags().GetBool("all-projects")
	wsRoot, _ := cmd.Flags().GetString("workspace-root")
	skipWorktrees, _ := cmd.Flags().GetBool("skip-worktrees")
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	host, _ := cmd.Flags().GetString("host")

	// Propagate --host to the generator via env var. The generator
	// reads $LOOM_HOST (see pkg/generator/host.go) when applying
	// host_overrides from registry.yaml. Empty value clears any
	// inherited LOOM_HOST so a bare `loom sync` regenerates the base
	// config even in shells where the env var was exported.
	if cmd.Flags().Changed("host") {
		_ = os.Setenv("LOOM_HOST", host)
	}

	cwd, _ := os.Getwd()
	mgr, err := sync.NewManager(cwd)
	if err != nil {
		return err
	}
	mgr.SkipSkills = skipSkills

	// --dry-run must not touch the repo or home. SyncToHome/SyncAll have no
	// dry-run mode (and --regen writes generated files into the repo before
	// syncing), so we report the pending drift and stop here rather than
	// calling them. Previously the flag was parsed but only ever reached the
	// --all-projects propagation, so `loom sync <profile> --dry-run` silently
	// wrote to home — the opposite of what the flag advertises.
	if dryRun {
		if err := reportSyncDryRun(mgr, profile); err != nil {
			return err
		}
		if allProjects {
			return propagateSyncAllProjects(mgr, profile, cwd, wsRoot, skipWorktrees, dryRun)
		}
		return nil
	}

	if profile == "all" {
		// For "all", pass nil/explicit resolveSecrets and loomMode flag status
		// so SyncAll can apply per-profile defaults
		var rs *bool
		if cmd.Flags().Changed("resolve-secrets") {
			rs = &resolveSecrets
		}
		loomModeExplicit := cmd.Flags().Changed("loom-mode")
		// Auto-detect loom binary for profiles that default to loom mode
		loomBinary = resolveStableLoomBinary(loomBinary)
		if err := mgr.SyncAll(true, regen, repoOnly, hubMode, hubURL, loomMode, loomBinary, rs, loomModeExplicit); err != nil {
			return err
		}
	} else {
		// For single profile: apply per-profile defaults when flags not explicitly set
		if p := mgr.Get(profile); p != nil {
			if !cmd.Flags().Changed("loom-mode") {
				loomMode = p.DefaultLoomMode
			}
			if !cmd.Flags().Changed("resolve-secrets") {
				resolveSecrets = p.DefaultResolveSecrets
			}

			if p.GeneratedDirectToHome && cmd.Flags().Changed("loom-mode") && !loomMode {
				fmt.Fprintf(os.Stderr, "Warning: %s uses home-level config and does not resolve template syntax.\n", profile)
				fmt.Fprintf(os.Stderr, "         Individual server entries may have broken ${env:...} templates.\n")
				fmt.Fprintf(os.Stderr, "         Consider using the default --loom-mode=true (proxy) instead.\n")
			}
		}

		if loomMode {
			loomBinary = resolveStableLoomBinary(loomBinary)
		}

		if err := mgr.SyncToHome(profile, true, regen, repoOnly, hubMode, hubURL, loomMode, loomBinary, resolveSecrets); err != nil {
			return err
		}
	}

	if allProjects {
		return propagateSyncAllProjects(mgr, profile, cwd, wsRoot, skipWorktrees, dryRun)
	}
	return nil
}

// reportSyncDryRun prints the files that a real sync would rewrite, without
// writing anything. It reads the same drift machinery `loom sync status` uses,
// so the two commands can never disagree about what is out of sync.
func reportSyncDryRun(mgr *sync.Manager, profile string) error {
	names := []string{profile}
	if profile == "all" {
		names = mgr.List()
		sort.Strings(names)
	}

	for _, name := range names {
		status, err := mgr.GetSyncStatus(name)
		if err != nil {
			return err
		}
		if status == nil {
			return fmt.Errorf("unknown profile %q", name)
		}

		fmt.Printf("[dry-run] %s: %s -> %s\n", name, status.RepoPath, status.HomePath)
		if status.InSync {
			fmt.Println("[dry-run]   already in sync")
			continue
		}
		for _, item := range status.DriftDetails {
			if item.Status == sync.DriftInSync {
				continue
			}
			file := item.File
			if file == "" {
				file = "(profile)"
			}
			fmt.Printf("[dry-run]   %s: %s\n", file, item.Status)
		}
	}

	fmt.Println("[dry-run] no files written")
	return nil
}
