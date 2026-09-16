package forge

// seed.go -- the root commit for a project created from the HUD. Mirrors the
// operator's plan→repo bootstrap seed (self-contained green CI so MR gates
// pass from commit 1) and adds the workspace conventions a hand-made repo is
// expected to carry: AGENTS.md, ROADMAP.md, CHANGELOG.md and a .loom/ note.
// Templates: "generic" (default) and "go" (module + cmd/<name> + Makefile).

import (
	"fmt"
	"strings"
)

// CommitAction is one file in the seed commit (GitLab commits API shape).
type CommitAction struct {
	Action   string `json:"action"`
	FilePath string `json:"file_path"`
	Content  string `json:"content"`
}

// SeedTemplates lists the accepted template names.
var SeedTemplates = []string{"generic", "go"}

func validTemplate(t string) bool {
	for _, s := range SeedTemplates {
		if s == t {
			return true
		}
	}
	return false
}

const seedCI = `# Seed pipeline — replace with the project's real stages as the toolchain
# lands. Self-contained on purpose: cross-project includes break merge-bot
# pipelines on private repos (0-job config_error). When the repo is ready
# for the fleet radar, add the tech-radar include:
#   include:
#     - project: "services/tech-radar"
#       file: "/ci/radar.yml"
stages:
  - test

seed:
  stage: test
  image: alpine:3.20
  script:
    - echo "seed pipeline — replace me with real checks"
`

const seedGitignore = `# Build output
bin/
dist/
*.exe

# Editor and agent scratch
.idea/
.vscode/
.loom/local/
.loom/archive/
.worktrees/
`

// SeedActions builds the root commit for a new project.
func SeedActions(group, name, description, template, mirror string) []CommitAction {
	full := group + "/" + name
	acts := []CommitAction{
		{Action: "create", FilePath: "README.md", Content: seedReadme(full, name, description, mirror)},
		{Action: "create", FilePath: ".gitlab-ci.yml", Content: seedCI},
		{Action: "create", FilePath: ".gitignore", Content: seedGitignore},
		{Action: "create", FilePath: "AGENTS.md", Content: seedAgents(full, name)},
		{Action: "create", FilePath: "ROADMAP.md", Content: seedRoadmap(name)},
		{Action: "create", FilePath: "CHANGELOG.md", Content: "# Changelog\n\nAll notable changes to this project are documented here.\n\n## [Unreleased]\n\n- Repository created from the Loom HUD (Mills intake).\n"},
		{Action: "create", FilePath: ".loom/README.md", Content: seedLoom(full)},
	}
	if template == "go" {
		acts = append(acts,
			CommitAction{Action: "create", FilePath: "go.mod", Content: fmt.Sprintf("module gitlab.flexinfer.ai/%s\n\ngo 1.26\n", full)},
			CommitAction{Action: "create", FilePath: "cmd/" + name + "/main.go", Content: seedGoMain(name)},
			CommitAction{Action: "create", FilePath: "Makefile", Content: seedMakefile(name)},
		)
	}
	return acts
}

// SeedPaths lists the files SeedActions creates, for the caller's ledger.
func SeedPaths(template, name string) []string {
	out := []string{"README.md", ".gitlab-ci.yml", ".gitignore", "AGENTS.md", "ROADMAP.md", "CHANGELOG.md", ".loom/README.md"}
	if template == "go" {
		out = append(out, "go.mod", "cmd/"+name+"/main.go", "Makefile")
	}
	return out
}

func seedReadme(full, name, description, mirror string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", name)
	if d := strings.TrimSpace(description); d != "" {
		b.WriteString(d + "\n\n")
	}
	fmt.Fprintf(&b, "Canonical repository: `%s` on GitLab.", full)
	if mirror != "" {
		fmt.Fprintf(&b, " Mirrored to GitHub at `%s` (push mirror; GitLab is the source of truth).", mirror)
	}
	b.WriteString("\n\nCreated from the Loom HUD (Mills intake). The seed pipeline is a placeholder green job; the first slice replaces it with real fmt/lint/test stages.\n")
	return b.String()
}

func seedAgents(full, name string) string {
	var b strings.Builder
	b.WriteString("# AGENTS.md\n\n")
	fmt.Fprintf(&b, "Guidance for AI coding agents working in `%s`.\n\n", full)
	b.WriteString("## State\n\n")
	b.WriteString("- Created empty from the Loom HUD (Mills intake). `.gitlab-ci.yml` is a placeholder green pipeline; add real fmt/lint/test stages with the first toolchain slice.\n")
	b.WriteString("- `README.md`, `ROADMAP.md`, and `CHANGELOG.md` are stubs — keep them truthful as work lands.\n\n")
	b.WriteString("## Conventions (workspace)\n\n")
	b.WriteString("- Branch prefixes: `feat/`, `fix/`, `refactor/`, `docs/`, `ci/`, `debt/`.\n")
	b.WriteString("- Conventional commit messages; one changelog entry per user-visible change.\n")
	b.WriteString("- Planning notes live under `.loom/` (root files tracked, `.loom/local/` and `.loom/archive/` ignored).\n")
	b.WriteString("- Never commit secrets; keep `**/*auth*` and `**/secret*` changes reviewable by a human.\n\n")
	b.WriteString("## Mills\n\n")
	fmt.Fprintf(&b, "This repo is woven by the Mills operator once it is admitted in policy. Backlog items target `%s`; slices declare the files they touch, and the scope gate holds them to it.\n", full)
	_ = name
	return b.String()
}

func seedRoadmap(name string) string {
	return fmt.Sprintf("# %s — Roadmap\n\n## Now\n\n- Replace the seed pipeline with real fmt/lint/test stages.\n- Write the first feature slice.\n\n## Next\n\n- (fill in)\n\n## Later\n\n- (fill in)\n", name)
}

func seedLoom(full string) string {
	return fmt.Sprintf("# .loom\n\nLocal Loom planning artifacts for `%s`.\n\n- Root-level `*.md` / `*.json` here are tracked for multi-agent visibility.\n- `local/` and `archive/` are gitignored.\n", full)
}

func seedGoMain(name string) string {
	return fmt.Sprintf("package main\n\nimport \"fmt\"\n\nfunc main() {\n\tfmt.Println(%q)\n}\n", name+": hello")
}

func seedMakefile(name string) string {
	return fmt.Sprintf(".PHONY: build test lint\n\nbuild:\n\tgo build ./...\n\ntest:\n\tgo test ./...\n\nlint:\n\tgo vet ./...\n\nrun:\n\tgo run ./cmd/%s\n", name)
}
