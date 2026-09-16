// ops_regen.go — Config and skill regeneration: Regenerate, regenerateSkills,
// SyncSkills, and cleanup helpers (cleanRepoSkills, cleanSkillsAt,
// cleanRepoGenerated, cleanGeneratedAt, discoverSkillsRegistryPath).
package sync

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/crb2nu/loom/pkg/generator"
	"github.com/crb2nu/loom/pkg/registry"
	"github.com/crb2nu/loom/pkg/skills"
)

// Regenerate generates the configuration for a profile and updates the repo directory.
func (m *Manager) Regenerate(p *Profile, hubMode bool, hubURL string, loomMode bool, loomBinary string, resolveSecrets bool) error {
	if p.GeneratorTarget == "" {
		return fmt.Errorf("profile %s has no generator target", p.Name)
	}

	// Load registry - prefer local override, then home directory
	regPath := discoverRegistryPath(m.RepoRoot)
	reg, err := registry.LoadWithDefaults(regPath)
	if err != nil {
		return fmt.Errorf("load registry from %s: %w", regPath, err)
	}
	fmt.Printf("Using registry: %s\n", regPath)
	if err := syncAgentsSafetyPolicy(m.RepoRoot, reg); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: AGENTS.md safety policy sync failed: %v\n", err)
	}

	// Create temp dir
	tmpDir, err := os.MkdirTemp("", "loom-gen")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	// Generate
	fmt.Printf("Regenerating config for %s...\n", p.Name)
	err = generator.GenerateConfigsWithPath(reg, regPath, tmpDir, []string{p.GeneratorTarget}, hubMode, hubURL, loomMode, loomBinary, resolveSecrets)
	if err != nil {
		return err
	}

	// Copy generated file to the profile destination.
	genPath := filepath.Join(tmpDir, p.GeneratorTarget, p.GeneratedFile)
	if !Exists(genPath) {
		return fmt.Errorf("generated file not found: %s", genPath)
	}

	destRoot := m.ResolveRepoPath(p)
	primaryDestName := p.GeneratedFile
	if p.GeneratedDirectToHome {
		m.cleanRepoGenerated(p)
		// Also clean stale copies across all workspace projects.
		if m.WorkspaceRoot != "" {
			n, err := m.CleanAllProjectsGenerated(p.Name, m.WorkspaceRoot, false, false)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Warning: workspace-wide cleanup for %s: %v\n", p.Name, err)
			} else if n > 0 {
				fmt.Printf("Cleaned %d stale %s config(s) from workspace projects\n", n, p.Name)
			}
		}
		destRoot = m.ResolveHomePath(p)
		primaryDestName = primaryHomeGeneratedFile(p)
	}
	if err := os.MkdirAll(destRoot, 0755); err != nil {
		return err
	}

	destFile := filepath.Join(destRoot, primaryDestName)
	if err := CopyFile(genPath, destFile); err != nil {
		return err
	}

	fmt.Printf("Updated %s\n", destFile)

	// Copy extra generated files (e.g. settings.json for hooks).
	for _, extra := range p.ExtraGeneratedFiles {
		extraGen := filepath.Join(tmpDir, p.GeneratorTarget, extra)
		if Exists(extraGen) {
			extraDest := filepath.Join(destRoot, extra)
			if err := CopyFile(extraGen, extraDest); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: could not copy extra file %s: %v\n", extra, err)
			} else {
				fmt.Printf("Updated %s\n", extraDest)
			}
		}
	}

	// Codex execpolicy rules: merge (never copy) the loom-managed block into
	// ~/.codex/rules/default.rules. Not routed through ExtraGeneratedFiles on
	// purpose — extras are plain-copied (would clobber the approval rules the
	// Codex TUI auto-appends to this file) and swept from workspace projects
	// by CleanAllProjectsGenerated (would delete hand-authored repo-local
	// .codex/rules/ files).
	if p.Name == "codex" && p.GeneratedDirectToHome {
		genRules := filepath.Join(tmpDir, p.GeneratorTarget, codexRulesHomeRel)
		if Exists(genRules) {
			rulesDest := filepath.Join(destRoot, filepath.FromSlash(codexRulesHomeRel))
			if err := syncCodexRulesGenerated(genRules, rulesDest); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: codex rules merge failed: %v\n", err)
			}
		}
	}

	// Generate skills if the profile has a skills target (unless SkipSkills is set)
	if p.SkillsTarget != "" && !m.SkipSkills {
		if err := m.regenerateSkills(p); err != nil {
			return fmt.Errorf("skills generation failed for %s: %w", p.Name, err)
		}
	}

	return nil
}

// regenerateSkills generates skill files for a profile from the skills registry.
func (m *Manager) regenerateSkills(p *Profile) error {
	skillsRegPath := discoverSkillsRegistryPath(m.RepoRoot)
	if skillsRegPath == "" {
		return nil // No skills registry found, skip silently
	}

	repoPath := m.ResolveRepoPath(p)
	fmt.Printf("Generating skills for %s from %s...\n", p.Name, skillsRegPath)

	// When SkillsDirectToHome, generate directly into the home directory
	// so skills exist in only one place (avoiding duplication warnings).
	outputDir := ""
	if p.SkillsDirectToHome {
		outputDir = m.ResolveHomePath(p)
		if p.SkillsTarget == "codex" {
			outputDir = ""
		}
		// SKILL.md bundle targets write under <SkillsHomePath>, which may
		// differ from the MCP-config HomeDir (Antigravity config lives under
		// ~/.gemini but its skills live under ~/.gemini/antigravity/skills).
		// The generator appends skills/<name>, so point OutputDir at the parent
		// of SkillsHomePath.
		if skillTargetUsesHomePathParent(p) {
			outputDir = filepath.Dir(os.ExpandEnv(p.SkillsHomePath))
		}
	}

	gen, err := skills.NewGenerator(skills.GeneratorOptions{
		RegistryPath:  skillsRegPath,
		Target:        p.SkillsTarget,
		RepoRoot:      m.RepoRoot,
		WorkspaceRoot: m.WorkspaceRoot,
		OutputDir:     outputDir,
		GeminiSkillsHome: func() string {
			if p.SkillsTarget != "gemini" && p.SkillsTarget != "antigravity" {
				return ""
			}
			return p.SkillsHomePath
		}(),
		// Codex defaults to ~/.codex/skills, but callers can still override the
		// skills root when they explicitly need a repo-local mirror.
		CodexSkillsDir: func() string {
			if p.SkillsTarget != "codex" {
				return ""
			}
			if p.SkillsDirectToHome {
				return filepath.Join(m.ResolveHomePath(p), "skills")
			}
			return filepath.Join(repoPath, "skills")
		}(),
		CodexRootDir: func() string {
			if p.SkillsTarget != "codex" {
				return ""
			}
			if p.SkillsDirectToHome {
				return m.ResolveHomePath(p)
			}
			return repoPath
		}(),
	})
	if err != nil {
		return fmt.Errorf("create skills generator: %w", err)
	}

	if err := gen.Generate(); err != nil {
		return fmt.Errorf("generate skills: %w", err)
	}

	// Retain the last usable repo mirror until home delivery succeeds. Cleanup
	// uses manifest ownership so user-authored neighbors remain untouched.
	if p.SkillsDirectToHome {
		if err := m.cleanRepoSkills(p); err != nil {
			return fmt.Errorf("clean repo skills after home delivery: %w", err)
		}
	}

	// Read manifest from the directory where skills were generated.
	manifestDir := repoPath
	if p.SkillsDirectToHome {
		manifestDir = skillsHomeManifestDir(p, m.ResolveHomePath(p))
	}
	manifest, err := skills.ReadManifest(manifestDir)
	if err != nil {
		return fmt.Errorf("read generated skill manifest: %w", err)
	}
	if manifest != nil {
		fmt.Printf("Generated %d skill files for %s\n", len(manifest.Generated), p.Name)
	}

	return nil
}

func skillTargetUsesHomePathParent(p *Profile) bool {
	if p == nil || p.SkillsHomePath == "" {
		return false
	}
	switch p.SkillsTarget {
	case "gemini", "antigravity", "zed", "opencode", "kilocode":
		// Kilocode: MCP config HomeDir is ~/.config/kilo but skills
		// (rules/workflows) still live under ~/.kilocode.
		return true
	default:
		return false
	}
}

func skillsHomeManifestDir(p *Profile, homePath string) string {
	if skillTargetUsesHomePathParent(p) {
		return filepath.Dir(os.ExpandEnv(p.SkillsHomePath))
	}
	return homePath
}

// cleanRepoSkills removes managed repo mirrors after successful home delivery.
func (m *Manager) cleanRepoSkills(p *Profile) error {
	dirs := []string{m.ResolveRepoPath(p)}
	if m.WorkspaceRoot != "" && m.WorkspaceRoot != m.RepoRoot {
		dirs = append(dirs, filepath.Join(m.WorkspaceRoot, p.RepoDir))
	}
	homePath := skillsHomeManifestDir(p, m.ResolveHomePath(p))
	homeInfo, homeErr := os.Stat(homePath)
	for _, dir := range dirs {
		if filepath.Clean(dir) == filepath.Clean(homePath) {
			continue
		}
		// A repo/workspace root can be a symlink to the home root. Compare
		// filesystem identity before deleting through a differently named alias.
		if info, err := os.Stat(dir); err == nil && homeErr == nil && os.SameFile(info, homeInfo) {
			continue
		}
		if err := m.cleanSkillsAt(dir); err != nil {
			return err
		}
	}
	return nil
}

// cleanSkillsAt deletes only files whose manifest hashes still match. Untracked
// files, legacy files without ownership hashes, and modified files are preserved.
func (m *Manager) cleanSkillsAt(dir string) error {
	manifest, err := skills.ReadManifest(dir)
	if err != nil {
		return fmt.Errorf("read stale repo manifest at %s: %w", dir, err)
	}
	if manifest == nil {
		return nil
	}
	removed, err := skills.PruneManifest(dir, manifest, nil)
	if err != nil {
		return fmt.Errorf("prune stale repo skills at %s: %w", dir, err)
	}
	if err := os.Remove(filepath.Join(dir, skills.ManifestFilename)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove stale repo manifest at %s: %w", dir, err)
	}
	if len(removed) > 0 {
		fmt.Printf("Cleaned %d managed repo skill files from %s\n", len(removed), dir)
	}
	return nil
}

// cleanRepoGenerated removes stale generated config files from the repo
// directory when a profile now writes them directly to home.
// Also cleans the workspace root if it differs from the repo root.
func (m *Manager) cleanRepoGenerated(p *Profile) {
	m.cleanGeneratedAt(m.ResolveRepoPath(p), p)

	// Also clean workspace root if different from repo root
	if m.WorkspaceRoot != "" && m.WorkspaceRoot != m.RepoRoot {
		wsPath := filepath.Join(m.WorkspaceRoot, p.RepoDir)
		if wsPath != m.ResolveHomePath(p) {
			m.cleanGeneratedAt(wsPath, p)
		}
	}
}

// cleanGeneratedAt removes stale generated config files from the given directory.
func (m *Manager) cleanGeneratedAt(dir string, p *Profile) {
	files := []string{p.GeneratedFile}
	files = append(files, p.ExtraGeneratedFiles...)

	for _, rel := range files {
		if rel == "" {
			continue
		}
		path := filepath.Join(dir, rel)
		if !Exists(path) {
			continue
		}
		if err := os.Remove(path); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: could not remove stale generated file %s: %v\n", path, err)
		}
	}
}

// SyncSkills generates and syncs skill files for a profile.
func (m *Manager) SyncSkills(profileName string, repoOnly bool) error {
	p, err := m.GetProfile(profileName)
	if err != nil {
		return err
	}
	if p.SkillsTarget == "" {
		return fmt.Errorf("profile %s does not have a skills target", profileName)
	}

	if repoOnly && p.SkillsDirectToHome {
		fmt.Printf("Skipping skill sync for %s (repo-only incompatible with home-only skills)\n", profileName)
		return nil
	}

	// Generate skills
	if err := m.regenerateSkills(p); err != nil {
		return err
	}

	if repoOnly {
		fmt.Printf("Skipping skill sync to home for %s (repo-only)\n", profileName)
		return nil
	}

	// When skills are generated directly to home, no copy step needed.
	if p.SkillsDirectToHome {
		homePath := m.ResolveHomePath(p)
		manifestDir := skillsHomeManifestDir(p, homePath)
		manifest, err := skills.ReadManifest(manifestDir)
		if err != nil {
			return fmt.Errorf("read generated skill manifest: %w", err)
		}
		if manifest != nil {
			fmt.Printf("Generated %d skill files directly to %s\n", len(manifest.Generated), manifestDir)
		}
		return nil
	}

	// Sync skill files from repo to home
	repoPath := m.ResolveRepoPath(p)
	homePath := m.ResolveHomePath(p)

	count, err := skills.SyncGeneratedFiles(repoPath, homePath)
	if err != nil {
		return fmt.Errorf("sync skills for %s: %w", profileName, err)
	}
	fmt.Printf("Synced %d skill files for %s\n", count, profileName)
	return nil
}

// discoverSkillsRegistryPath locates the skills-registry.yaml file.
func discoverSkillsRegistryPath(repoRoot string) string {
	if local := discoverWorkspaceContextFile(repoRoot, "skills-registry.yaml"); local != "" {
		return local
	}
	// Try the skills package finder as fallback
	if path, found := skills.FindRegistry(); found {
		return path
	}
	return ""
}
