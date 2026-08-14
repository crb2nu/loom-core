package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/crb2nu/loom/pkg/skills"
	"github.com/crb2nu/loom/pkg/sync"
)

func newSyncSkillsCmd() *cobra.Command {
	syncSkillsCmd := &cobra.Command{
		Use:   "skills [profile]",
		Short: "Generate, discover, and sync skills",
		Long: `Generate skill files from skills-registry.yaml, browse skills.sh, discover well-known hosted skill catalogs, and sync them to home directories.

Example:
  loom sync skills claude     # Generate + sync skills for Claude
  loom sync skills all        # Generate + sync skills for all profiles
  loom sync skills all --repo-only  # Regenerate repo-local skills only
  loom sync skills browse openai
  loom sync skills install codex openai/skills/openai-docs`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			profile := args[0]
			repoOnly, _ := cmd.Flags().GetBool("repo-only")

			cwd, _ := os.Getwd()
			mgr, err := sync.NewManager(cwd)
			if err != nil {
				return err
			}

			if profile == "all" {
				for _, name := range mgr.List() {
					p, _ := mgr.GetProfile(name)
					if p.SkillsTarget == "" {
						continue
					}
					fmt.Printf("=== %s ===\n", name)
					if err := mgr.SyncSkills(name, repoOnly); err != nil {
						fmt.Fprintf(os.Stderr, "Warning: skills sync failed for %s: %v\n", name, err)
					}
				}
				return nil
			}
			return mgr.SyncSkills(profile, repoOnly)
		},
	}
	syncSkillsCmd.Flags().Bool("repo-only", false, "Only update repository skill files, do not sync to home")

	syncSkillsBrowseCmd := &cobra.Command{
		Use:   "browse <query>",
		Short: "Search skills.sh for installable skills",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			limit, _ := cmd.Flags().GetInt("limit")
			results, err := skills.SearchSkillsSH(args[0], limit)
			if err != nil {
				return err
			}

			if len(results) == 0 {
				fmt.Printf("No skills found on skills.sh for %q\n", args[0])
				return nil
			}

			fmt.Printf("Found %d skill(s) on skills.sh for %q\n", len(results), args[0])
			for i, result := range results {
				displayName := strings.TrimSpace(result.Name)
				if displayName == "" {
					displayName = result.SkillID
				}

				fmt.Printf("%2d. %s\n", i+1, displayName)
				fmt.Printf("    id: %s\n", result.ID)
				if strings.TrimSpace(result.Source) != "" {
					fmt.Printf("    source: %s\n", result.Source)
				}
				if result.Installs > 0 {
					fmt.Printf("    installs: %s\n", formatInstalledCount(result.Installs))
				}
				fmt.Printf("    install: loom sync skills install <profile> %s\n", result.ID)
			}
			return nil
		},
	}
	syncSkillsBrowseCmd.Flags().Int("limit", 10, "Maximum number of skills.sh results to show")
	syncSkillsCmd.AddCommand(syncSkillsBrowseCmd)

	syncSkillsDiscoverCmd := &cobra.Command{
		Use:   "discover <source>",
		Short: "Discover skills from a well-known hosted source",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			source := args[0]
			catalog, err := skills.DiscoverHostedCatalog(source)
			if err != nil {
				return err
			}

			fmt.Printf("Discovered %d skill(s) from %s\n", len(catalog.Skills), catalog.IndexURL)
			for _, skill := range catalog.Skills {
				name := strings.TrimSpace(skill.Name)
				if name == "" {
					continue
				}
				desc := strings.TrimSpace(skill.Description)
				if desc != "" {
					fmt.Printf("  - %s: %s\n", name, desc)
				} else {
					fmt.Printf("  - %s\n", name)
				}
			}
			return nil
		},
	}
	syncSkillsCmd.AddCommand(syncSkillsDiscoverCmd)

	syncSkillsInstallCmd := &cobra.Command{
		Use:   "install <profile|all> <skill-ref>",
		Short: "Install one selected skill from skills.sh into a profile home directory",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			profile := args[0]
			skillRef := args[1]

			cwd, _ := os.Getwd()
			mgr, err := sync.NewManager(cwd)
			if err != nil {
				return err
			}

			destinations, err := resolveHostedImportDestinations(mgr, profile)
			if err != nil {
				return err
			}

			totalInstalled := 0
			for _, dest := range destinations {
				result, err := skills.ImportSkillsSHSkill(skillRef, dest.Root)
				if err != nil {
					return fmt.Errorf("install skills.sh skill for %s: %w", dest.Profile, err)
				}
				if result == nil {
					fmt.Printf("No skills.sh skill installed for %s from %s\n", dest.Profile, skillRef)
					continue
				}

				totalInstalled++
				fmt.Printf("Installed %s into %s for %s\n", result.Name, dest.Root, dest.Profile)
				fmt.Printf("  - %s (%d file(s))\n", result.Name, len(result.Files))
			}

			if totalInstalled == 0 {
				fmt.Printf("No skills.sh skills installed from %s\n", skillRef)
			}
			return nil
		},
	}
	syncSkillsCmd.AddCommand(syncSkillsInstallCmd)

	syncSkillsImportCmd := &cobra.Command{
		Use:   "import <profile> <source>",
		Short: "Import skill bundles from a well-known hosted source into a profile home directory",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			profile := args[0]
			source := args[1]
			selectedSkills, _ := cmd.Flags().GetStringArray("skill")

			cwd, _ := os.Getwd()
			mgr, err := sync.NewManager(cwd)
			if err != nil {
				return err
			}

			destinations, err := resolveHostedImportDestinations(mgr, profile)
			if err != nil {
				return err
			}

			totalImported := 0
			for _, dest := range destinations {
				results, err := skills.ImportHostedSkills(source, dest.Root, selectedSkills)
				if err != nil {
					return fmt.Errorf("import hosted skills for %s: %w", dest.Profile, err)
				}

				if len(results) == 0 {
					fmt.Printf("No hosted skills imported for %s from %s\n", dest.Profile, source)
					continue
				}

				totalImported += len(results)
				fmt.Printf("Imported %d skill bundle(s) into %s for %s\n", len(results), dest.Root, dest.Profile)
				for _, result := range results {
					fmt.Printf("  - %s (%d file(s))\n", result.Name, len(result.Files))
				}
			}
			if totalImported == 0 {
				fmt.Printf("No hosted skills imported from %s\n", source)
			}
			return nil
		},
	}
	syncSkillsImportCmd.Flags().StringArray("skill", nil, "Only import matching skill names (repeatable)")
	syncSkillsCmd.AddCommand(syncSkillsImportCmd)

	syncSkillsInstalledCmd := &cobra.Command{
		Use:   "installed <profile|all>",
		Short: "List Loom-managed imported skills installed in home directories",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cwd, _ := os.Getwd()
			mgr, err := sync.NewManager(cwd)
			if err != nil {
				return err
			}

			destinations, err := resolveHostedImportDestinations(mgr, args[0])
			if err != nil {
				return err
			}

			total := 0
			for _, dest := range destinations {
				installed, err := skills.ListHostedSkills(dest.Root)
				if err != nil {
					return fmt.Errorf("list hosted skills for %s: %w", dest.Profile, err)
				}

				fmt.Printf("%s (%s)\n", dest.Profile, dest.Root)
				if len(installed) == 0 {
					fmt.Println("  no Loom-managed hosted skills installed")
					continue
				}
				for _, skill := range installed {
					total++
					line := fmt.Sprintf("  - %s", skill.Name)
					if strings.TrimSpace(skill.SourceURL) != "" {
						line += fmt.Sprintf(" [%s]", skill.SourceURL)
					}
					fmt.Println(line)
				}
			}
			if total == 0 {
				fmt.Println("No Loom-managed hosted skills found.")
			}
			return nil
		},
	}
	syncSkillsCmd.AddCommand(syncSkillsInstalledCmd)

	syncSkillsRemoveCmd := &cobra.Command{
		Use:   "remove <profile|all>",
		Short: "Remove Loom-managed imported skills from home directories",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			selectedSkills, _ := cmd.Flags().GetStringArray("skill")
			removeAll, _ := cmd.Flags().GetBool("all")
			if !removeAll && len(selectedSkills) == 0 {
				return fmt.Errorf("specify --skill or --all")
			}

			cwd, _ := os.Getwd()
			mgr, err := sync.NewManager(cwd)
			if err != nil {
				return err
			}

			destinations, err := resolveHostedImportDestinations(mgr, args[0])
			if err != nil {
				return err
			}

			total := 0
			for _, dest := range destinations {
				removed, err := skills.RemoveHostedSkills(dest.Root, selectedSkills, removeAll)
				if err != nil {
					return fmt.Errorf("remove hosted skills for %s: %w", dest.Profile, err)
				}

				if len(removed) == 0 {
					fmt.Printf("No Loom-managed hosted skills removed for %s\n", dest.Profile)
					continue
				}
				total += len(removed)
				fmt.Printf("Removed %d hosted skill(s) for %s\n", len(removed), dest.Profile)
				for _, skill := range removed {
					fmt.Printf("  - %s\n", skill.Name)
				}
			}
			if total == 0 {
				fmt.Println("No Loom-managed hosted skills removed.")
			}
			return nil
		},
	}
	syncSkillsRemoveCmd.Flags().StringArray("skill", nil, "Hosted skill names to remove (repeatable)")
	syncSkillsRemoveCmd.Flags().Bool("all", false, "Remove all Loom-managed hosted skills for the selected profile(s)")
	syncSkillsCmd.AddCommand(syncSkillsRemoveCmd)
	return syncSkillsCmd
}

func resolveSkillsHomePath(raw, homeDir string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	raw = strings.ReplaceAll(raw, "$HOME", homeDir)
	raw = strings.ReplaceAll(raw, "${HOME}", homeDir)
	if filepath.IsAbs(raw) {
		return filepath.Clean(raw)
	}
	return filepath.Join(homeDir, raw)
}

type hostedImportDestination struct {
	Profile string
	Root    string
}

func resolveHostedImportDestinations(mgr *sync.Manager, profile string) ([]hostedImportDestination, error) {
	if profile == "all" {
		names := mgr.List()
		sort.Strings(names)

		var destinations []hostedImportDestination
		for _, name := range names {
			p := mgr.Get(name)
			if p == nil {
				continue
			}
			root := resolveHostedImportRoot(p, mgr.HomeDir)
			if root == "" {
				continue
			}
			destinations = append(destinations, hostedImportDestination{
				Profile: name,
				Root:    root,
			})
		}
		if len(destinations) == 0 {
			return nil, fmt.Errorf("no profiles support hosted skill imports")
		}
		return destinations, nil
	}

	p, err := mgr.GetProfile(profile)
	if err != nil {
		return nil, err
	}

	root := resolveHostedImportRoot(p, mgr.HomeDir)
	if root == "" {
		return nil, fmt.Errorf("profile %s does not define a hosted skills home path", profile)
	}

	return []hostedImportDestination{{
		Profile: profile,
		Root:    root,
	}}, nil
}

func resolveHostedImportRoot(p *sync.Profile, homeDir string) string {
	if p == nil {
		return ""
	}

	switch p.Name {
	case "claude":
		return filepath.Join(homeDir, ".claude", "skills")
	}

	if p.SkillsHomePath == "" {
		return ""
	}
	return resolveSkillsHomePath(p.SkillsHomePath, homeDir)
}

func formatInstalledCount(count int) string {
	switch {
	case count >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(count)/1_000_000)
	case count >= 1_000:
		return fmt.Sprintf("%.1fK", float64(count)/1_000)
	default:
		return fmt.Sprintf("%d", count)
	}
}
