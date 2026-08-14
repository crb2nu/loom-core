package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/crb2nu/loom/pkg/generator"
	"github.com/crb2nu/loom/pkg/registry"
	"github.com/crb2nu/loom/pkg/skills"
)

func newGenerateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "generate",
		Short: "Generate configurations and manifests",
	}
	cmd.AddCommand(
		newGenerateManifestsCmd(),
		newGenerateConfigsCmd(),
		newGenerateSkillsCmd(),
	)
	return cmd
}

// newGenerateManifestsCmd creates the generate manifests subcommand.
func newGenerateManifestsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "manifests",
		Short: "Generate Kubernetes manifests for MCP Hub",
		RunE: func(cmd *cobra.Command, args []string) error {
			outputDir, _ := cmd.Flags().GetString("output-dir")
			namespace, _ := cmd.Flags().GetString("namespace")
			imageRegistry, _ := cmd.Flags().GetString("image-registry")
			registryPath, _ := cmd.Flags().GetString("registry")

			includeGateway, _ := cmd.Flags().GetBool("gateway")
			gatewayHost, _ := cmd.Flags().GetString("gateway-host")
			gatewayClass, _ := cmd.Flags().GetString("gateway-ingress-class")
			gatewayTLS, _ := cmd.Flags().GetString("gateway-tls-secret")
			gatewayImage, _ := cmd.Flags().GetString("gateway-image")

			cwd, _ := os.Getwd()
			if registryPath == "" {
				registryPath = registry.FindRegistryOrDefault(filepath.Join(cwd, "mcp", "context", "registry.yaml"))
			}

			reg, err := registry.LoadWithDefaults(registryPath)
			if err != nil {
				return err
			}

			if !filepath.IsAbs(outputDir) {
				outputDir = filepath.Join(cwd, outputDir)
			}

			fmt.Printf("Generating manifests in %s...\n", outputDir)
			return generator.GenerateManifests(reg, outputDir, generator.ManifestsOptions{
				Namespace:     namespace,
				ImageRegistry: imageRegistry,
				Gateway: generator.GatewayManifests{
					Enabled:          includeGateway,
					Image:            gatewayImage,
					IngressHost:      gatewayHost,
					IngressClassName: gatewayClass,
					TLSSecretName:    gatewayTLS,
				},
			})
		},
	}
	cmd.Flags().String("output-dir", "k3s/mcp-hub/servers", "Output directory")
	cmd.Flags().String("namespace", "mcp-hub", "Kubernetes namespace")
	cmd.Flags().String("image-registry", "registry.harbor.lan/mcp", "Container image registry")
	cmd.Flags().String("registry", "", "Path to registry.yaml")
	cmd.Flags().Bool("gateway", true, "Include gateway manifests")
	cmd.Flags().String("gateway-host", "mcp.flexinfer.ai", "Gateway ingress host")
	cmd.Flags().String("gateway-ingress-class", "", "Gateway ingress class")
	cmd.Flags().String("gateway-tls-secret", "", "Gateway TLS secret")
	cmd.Flags().String("gateway-image", "", "Gateway container image")
	return cmd
}

// newGenerateConfigsCmd creates the generate configs subcommand.
func newGenerateConfigsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "configs",
		Short: "Generate client configurations (VS Code, Claude, etc.)",
		RunE: func(cmd *cobra.Command, args []string) error {
			outputDir, _ := cmd.Flags().GetString("output-dir")
			target, _ := cmd.Flags().GetString("target")
			hubMode, _ := cmd.Flags().GetBool("hub-mode")
			hubURL, _ := cmd.Flags().GetString("hub-url")
			loomMode, _ := cmd.Flags().GetBool("loom-mode")
			loomBinary, _ := cmd.Flags().GetString("loom-binary")
			registryPath, _ := cmd.Flags().GetString("registry")
			host, _ := cmd.Flags().GetString("host")

			if cmd.Flags().Changed("host") {
				_ = os.Setenv("LOOM_HOST", host)
			}

			cwd, _ := os.Getwd()
			if registryPath == "" {
				registryPath = registry.FindRegistryOrDefault(filepath.Join(cwd, "mcp", "context", "registry.yaml"))
			}

			reg, err := registry.LoadWithDefaults(registryPath)
			if err != nil {
				return err
			}

			if !filepath.IsAbs(outputDir) {
				outputDir = filepath.Join(cwd, outputDir)
			}

			targets := []string{target}
			if target == "all" {
				targets = []string{"all"}
			}

			if loomMode {
				loomBinary = resolveStableLoomBinary(loomBinary)
			}

			fmt.Printf("Generating configs in %s...\n", outputDir)
			workspaceRoot := registry.GetRepoRoot(registryPath)
			// Heuristic: if the registry lives under platform/gitops, ${repo} should still
			// expand to the monorepo root (where services/loom-core lives).
			if _, err := os.Stat(filepath.Join(workspaceRoot, "services", "loom-core")); err != nil {
				dir := workspaceRoot
				for range 6 {
					parent := filepath.Dir(dir)
					if parent == dir {
						break
					}
					dir = parent
					if _, err := os.Stat(filepath.Join(dir, "services", "loom-core")); err == nil {
						workspaceRoot = dir
						break
					}
				}
			}
			fmt.Printf("Using workspace root: %s\n", workspaceRoot)
			resolveSecrets, _ := cmd.Flags().GetBool("resolve-secrets")
			return generator.GenerateConfigsWithPath(reg, registryPath, outputDir, targets, hubMode, hubURL, loomMode, loomBinary, resolveSecrets)
		},
	}
	cmd.Flags().String("output-dir", "generated/mcp", "Output directory")
	cmd.Flags().String("target", "all", "Target config (all, vscode, codex, etc.)")
	cmd.Flags().Bool("hub-mode", false, "Generate configs for MCP Hub")
	cmd.Flags().String("hub-url", "wss://mcp.flexinfer.ai/ws", "MCP Hub WebSocket URL")
	cmd.Flags().Bool("loom-mode", true, "Generate single loom proxy entry")
	cmd.Flags().String("loom-binary", "", "Path to loom binary")
	cmd.Flags().String("registry", "", "Path to registry.yaml")
	cmd.Flags().Bool("emit", true, "Emit generated files (always true)")
	cmd.Flags().Bool("resolve-secrets", false, "Resolve secret templates to literal values")
	cmd.Flags().String("host", "", "Host profile for registry overrides (e.g. code-server). Sets $LOOM_HOST.")
	return cmd
}

// newGenerateSkillsCmd creates the generate skills subcommand.
func newGenerateSkillsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "skills",
		Short: "Generate skill configurations for all AI coding platforms",
		Long: `Generate skill configurations from the unified skills registry.

This command reads skills-registry.yaml and generates platform-specific
skill configurations for AI coding assistants.

Platform output formats:

  Codex:    ~/.codex/skills/<name>/SKILL.md + scripts/ + references/ + assets/
  Claude:   .claude/commands/<name>.md (slash commands with frontmatter)
            .claude/rules/<name>.md (rules without frontmatter)
  Kilocode: .kilocode/rules/<name>.md (rules)
            .kilocode/workflows/<name>.yaml (workflows)
  Gemini:   .gemini/skills/<name>/SKILL.md + scripts/ + references/ + assets/
            .gemini/GEMINI.md (composite from instruction-type skills)
  Antigravity:
            .gemini/antigravity/skills/<name>/SKILL.md + resources
            .gemini/antigravity/GEMINI.md (composite from instruction-type skills)

Skills with type=instruction are assembled into a composite instructions.md (or GEMINI.md for Gemini).

Example:
  loom generate skills --target all
  loom generate skills --target codex
  loom generate skills --target kilocode --dry-run
  loom generate skills --target gemini --verbose
  loom generate skills --target antigravity --verbose`,
		RunE: func(cmd *cobra.Command, args []string) error {
			target, _ := cmd.Flags().GetString("target")
			outputDir, _ := cmd.Flags().GetString("output-dir")
			registryPath, _ := cmd.Flags().GetString("registry")
			codexHome, _ := cmd.Flags().GetString("codex-home")
			workspaceRoot, _ := cmd.Flags().GetString("workspace")
			dryRun, _ := cmd.Flags().GetBool("dry-run")
			verbose, _ := cmd.Flags().GetBool("verbose")
			validate, _ := cmd.Flags().GetBool("validate")

			cwd, _ := os.Getwd()

			// Find skills registry
			if registryPath == "" {
				var found bool
				registryPath, found = skills.FindRegistry()
				if !found {
					// Try standard location
					registryPath = filepath.Join(cwd, "mcp", "context", "skills-registry.yaml")
					if _, err := os.Stat(registryPath); os.IsNotExist(err) {
						registryPath = filepath.Join(cwd, "services", "loom-core", "mcp", "context", "skills-registry.yaml")
						if _, err := os.Stat(registryPath); os.IsNotExist(err) {
							registryPath = filepath.Join(cwd, "platform", "gitops", "mcp", "context", "skills-registry.yaml")
						}
					}
				}
			}

			if _, err := os.Stat(registryPath); os.IsNotExist(err) {
				return fmt.Errorf("skills registry not found at %s", registryPath)
			}

			// Validate only mode
			if validate {
				gen, err := skills.NewGenerator(skills.GeneratorOptions{
					RegistryPath:  registryPath,
					Target:        target,
					WorkspaceRoot: workspaceRoot,
					CodexHome:     codexHome,
				})
				if err != nil {
					return fmt.Errorf("validation failed: %w", err)
				}
				errs := gen.Validate()
				fmt.Printf("Skills registry: %d skills defined\n", len(gen.Registry.Skills))
				for _, skill := range gen.Registry.Skills {
					fmt.Printf("  - %s (%s)\n", skill.Name, strings.Join(skill.Categories, ", "))
				}
				if len(errs) > 0 {
					fmt.Printf("\n✗ %d validation error(s):\n", len(errs))
					for _, e := range errs {
						fmt.Printf("  ✗ %s\n", e.Error())
					}
					return fmt.Errorf("validation failed with %d error(s)", len(errs))
				}
				fmt.Printf("\n✓ All scripts, references, and assets exist on disk\n")
				return nil
			}

			if workspaceRoot == "" {
				workspaceRoot = cwd
			}

			gen, err := skills.NewGenerator(skills.GeneratorOptions{
				RegistryPath:  registryPath,
				Target:        target,
				OutputDir:     outputDir,
				CodexHome:     codexHome,
				WorkspaceRoot: workspaceRoot,
				DryRun:        dryRun,
				Verbose:       verbose,
			})
			if err != nil {
				return err
			}

			fmt.Printf("Generating skills from %s...\n", registryPath)
			return gen.Generate()
		},
	}
	cmd.Flags().String("target", "all", "Target platform (all, codex, claude, kilocode, gemini, antigravity)")
	cmd.Flags().String("output-dir", "", "Output directory (default: platform-specific)")
	cmd.Flags().String("registry", "", "Path to skills-registry.yaml")
	cmd.Flags().String("codex-home", "", "Codex home directory (default: ~/.codex)")
	cmd.Flags().String("workspace", "", "Workspace root for Claude skills")
	cmd.Flags().Bool("dry-run", false, "Show what would be generated without writing")
	cmd.Flags().Bool("verbose", false, "Verbose output")
	cmd.Flags().Bool("validate", false, "Only validate the registry, don't generate")
	return cmd
}
