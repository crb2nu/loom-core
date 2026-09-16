// generator.go contains the core Generator struct, constructor, and top-level dispatch logic.
package skills

import (
	"fmt"
	"os"
	"path/filepath"
)

// Generator handles skill generation for different platforms.
type Generator struct {
	Registry     *Registry
	RegistryPath string // Path to skills-registry.yaml on disk
	SourceDir    string // Where skill source files live (mcp/skills/)
	Target       string // "codex" | "claude" | "kilocode" | "gemini" | "all"
	OutputDir    string // Where to generate skills (overrides platform defaults)
	// CodexRootDir overrides the Codex platform root (normally ~/.codex) for
	// instructions.md and manifest generation.
	CodexRootDir string
	// CodexSkillsDir overrides the default Codex skills directory (normally ~/.codex/skills).
	// This is intentionally separate from OutputDir so callers (like `loom sync`) can
	// generate Codex skills into the repo while keeping manifest/instructions rooted
	// at the repo's .codex/ directory.
	CodexSkillsDir string
	RepoRoot       string // Base directory containing .claude/, .codex/, .kilocode/, .gemini/
	CodexHome      string // ~/.codex
	// GeminiSkillsHome controls the ${SKILL_PATH} base for Gemini-formatted
	// skill bundles. Defaults to $HOME/.gemini/skills when empty.
	GeminiSkillsHome string
	WorkspaceRoot    string // For Claude: workspace root for .agents/skills/
	DryRun           bool
	Verbose          bool
}

// GeneratorOptions configures the generator.
type GeneratorOptions struct {
	RegistryPath     string
	Target           string
	OutputDir        string
	RepoRoot         string
	CodexHome        string
	CodexRootDir     string
	GeminiSkillsHome string
	CodexSkillsDir   string
	WorkspaceRoot    string
	DryRun           bool
	Verbose          bool
}

// AllTargets lists all supported skill generation targets.
var AllTargets = []string{"codex", "claude", "kilocode", "gemini", "antigravity"}

// NewGenerator creates a new skill generator.
func NewGenerator(opts GeneratorOptions) (*Generator, error) {
	reg, err := Load(opts.RegistryPath)
	if err != nil {
		return nil, err
	}

	sourceDir := FindSkillsSourceDir(opts.RegistryPath)

	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("get home directory: %w", err)
	}
	codexHome := opts.CodexHome
	if codexHome == "" {
		codexHome = filepath.Join(home, ".codex")
	}

	workspaceRoot := opts.WorkspaceRoot
	if workspaceRoot == "" {
		workspaceRoot, err = os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("get working directory: %w", err)
		}
	}

	repoRoot := opts.RepoRoot
	if repoRoot == "" {
		repoRoot = workspaceRoot
	}

	return &Generator{
		Registry:         reg,
		RegistryPath:     opts.RegistryPath,
		SourceDir:        sourceDir,
		Target:           opts.Target,
		OutputDir:        opts.OutputDir,
		CodexRootDir:     opts.CodexRootDir,
		CodexSkillsDir:   opts.CodexSkillsDir,
		RepoRoot:         repoRoot,
		CodexHome:        codexHome,
		GeminiSkillsHome: opts.GeminiSkillsHome,
		WorkspaceRoot:    workspaceRoot,
		DryRun:           opts.DryRun,
		Verbose:          opts.Verbose,
	}, nil
}

// Generate generates skills for the configured target(s).
func (g *Generator) Generate() error {
	targets := []string{g.Target}
	if g.Target == "all" {
		targets = AllTargets
	}

	for _, target := range targets {
		if err := g.generateForTarget(target); err != nil {
			return fmt.Errorf("generate %s: %w", target, err)
		}
	}

	// Update the registry's `updated:` date after successful generation.
	if !g.DryRun {
		if err := g.UpdateRegistryDate(); err != nil {
			if g.Verbose {
				fmt.Printf("Warning: could not update registry date: %v\n", err)
			}
		}
	}

	return nil
}

func (g *Generator) generateForTarget(target string) error {
	manifestDir := g.resolveTargetDir(target)
	var prev *Manifest
	if !g.DryRun && manifestDir != "" {
		var err error
		prev, err = ReadManifest(manifestDir)
		if err != nil {
			return fmt.Errorf("read previous %s manifest: %w", target, err)
		}
	}

	var generatedFiles []string
	var instructionSkills []*Skill
	var bundleSkills []*Skill

	for _, skill := range g.Registry.Skills {
		if !skill.IsEnabled(target) {
			if g.Verbose {
				fmt.Printf("Skipping %s for %s (disabled)\n", skill.Name, target)
			}
			continue
		}

		skillType := skill.GetType(target)
		if target == "antigravity" && skillType != "instruction" {
			// Antigravity uses Gemini-style skill bundles, not Claude-style
			// slash-command files. Older registry entries used type=command
			// for Antigravity; normalize them at generation time so the
			// Antigravity target overrides are still honored.
			skillType = "skill"
		}

		// Collect instruction-type skills for composite instructions.md / GEMINI.md
		if skillType == "instruction" {
			instructionSkills = append(instructionSkills, skill)
			continue
		}
		bundleSkills = append(bundleSkills, skill)

		var files []string
		var err error

		switch target {
		case "codex":
			prefix, pathErr := filepath.Rel(manifestDir, g.codexSkillsOutputDir())
			if pathErr != nil || !filepath.IsLocal(prefix) {
				return fmt.Errorf("codex skills output %s must be within manifest root %s", g.codexSkillsOutputDir(), manifestDir)
			}
			files = bundleManifestFiles(skill, prefix)
			err = g.generateCodexSkill(skill)
		case "claude":
			files, err = g.generateClaudeSkillByType(skill)
		case "kilocode":
			files, err = g.generateKilocodeSkill(skill)
		case "gemini":
			err = g.generateGeminiSkill(skill)
			if err == nil {
				files = append(files, g.geminiManifestFiles(skill)...)
			}
		case "antigravity":
			err = g.generateAntigravitySkill(skill)
			if err == nil {
				files = append(files, g.geminiManifestFiles(skill)...)
			}
		case "zed", "opencode":
			err = g.generateBundleSkill(skill, target)
			if err == nil {
				files = append(files, g.codexManifestFiles(skill)...)
			}
		default:
			return fmt.Errorf("unknown target: %s", target)
		}

		if err != nil {
			return fmt.Errorf("generate %s skill %s: %w", target, skill.Name, err)
		}
		generatedFiles = append(generatedFiles, files...)
	}

	// Generate composite instructions.md for platforms that support it
	if len(instructionSkills) > 0 {
		sortSkillsByPriority(instructionSkills)

		filename := "instructions.md"
		if target == "gemini" || target == "antigravity" {
			filename = "GEMINI.md"
		}
		files, err := g.generateInstructionsFile(target, instructionSkills, bundleSkills)
		if err != nil {
			return fmt.Errorf("generate %s %s: %w", target, filename, err)
		}
		generatedFiles = append(generatedFiles, files...)
	}

	// Publish only after delivery and ownership-aware stale pruning succeed.
	if !g.DryRun && manifestDir != "" {
		removed, err := PruneManifest(manifestDir, prev, generatedFiles)
		if err != nil {
			return fmt.Errorf("prune stale %s skill files: %w", target, err)
		}
		if g.Verbose {
			for _, rel := range removed {
				fmt.Printf("Pruned stale %s skill file: %s\n", target, rel)
			}
		}
		// An empty desired set still needs a manifest so a fresh repo mirror
		// can retire files from an older home installation.
		if err := WriteManifest(manifestDir, target, generatedFiles); err != nil {
			return fmt.Errorf("write %s manifest: %w", target, err)
		}
	}

	return nil
}

// resolveTargetDir returns the platform repo directory for writing generated files.
func (g *Generator) resolveTargetDir(target string) string {
	if target == "codex" {
		if g.CodexRootDir != "" {
			return g.CodexRootDir
		}
		if g.OutputDir != "" || g.CodexSkillsDir != "" {
			return filepath.Dir(g.codexSkillsOutputDir())
		}
		if g.CodexHome != "" {
			return g.CodexHome
		}
	}
	if g.OutputDir != "" {
		return g.OutputDir
	}
	if g.RepoRoot != "" {
		switch target {
		case "claude":
			return filepath.Join(g.RepoRoot, ".claude")
		case "kilocode":
			return filepath.Join(g.RepoRoot, ".kilocode")
		case "gemini":
			return filepath.Join(g.RepoRoot, ".gemini")
		case "antigravity":
			return filepath.Join(g.RepoRoot, ".gemini", "antigravity")
		}
	}
	return ""
}

func (g *Generator) codexManifestFiles(skill *Skill) []string {
	return bundleManifestFiles(skill, "skills")
}

func bundleManifestFiles(skill *Skill, prefix string) []string {
	files := []string{filepath.Join(prefix, skill.Name, "SKILL.md")}
	seen := map[string]struct{}{files[0]: {}}

	add := func(path string) {
		if path == "" {
			return
		}
		if _, ok := seen[path]; ok {
			return
		}
		seen[path] = struct{}{}
		files = append(files, path)
	}

	if skill.Common != nil {
		for _, script := range skill.Common.Scripts {
			if script == nil || script.Path == "" {
				continue
			}
			add(filepath.Join(prefix, skill.Name, script.Path))
		}
		for _, ref := range skill.Common.References {
			add(filepath.Join(prefix, skill.Name, "references", ref))
		}
		for _, asset := range skill.Common.Assets {
			add(filepath.Join(prefix, skill.Name, "assets", asset))
		}
	}

	return files
}

func (g *Generator) geminiManifestFiles(skill *Skill) []string {
	return g.codexManifestFiles(skill)
}
