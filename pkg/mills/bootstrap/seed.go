package bootstrap

// seed.go -- the minted repo's initial commit. The seed's job is to make the
// repo immediately workable by the Mills pipeline: a non-empty default branch
// (the clone step fails on an empty repo), a SELF-CONTAINED .gitlab-ci.yml
// that is green on commit 1 (no cross-project includes — the merge bot can't
// read private includes, which yields 0-job config_error pipelines), and a
// README carrying the plan's spec so the first spawned agent has the product
// brief in-repo.

import (
	"fmt"
	"strings"

	"github.com/crb2nu/loom/pkg/mills/clients"
)

// seedCI is deliberately minimal AND self-contained: one always-green job so
// MR gates (pipeline-success) pass from the first branch, replaced by real
// stages as the plan's slices land toolchain files.
const seedCI = `# Bootstrap seed pipeline — replace with the project's real stages as the
# toolchain lands. Self-contained on purpose: cross-project includes break
# merge-bot pipelines on private repos (0-job config_error).
stages:
  - test

seed:
  stage: test
  image: alpine:3.20
  script:
    - echo "bootstrap seed pipeline — replace me with real checks"
`

const seedGitignore = `# Binaries / build output
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

// seedCommitGeneric builds the root commit for a repo minted WITHOUT a plan
// (the EnsureRepo pre-flight for a new-project handoff). It mirrors seedCommit
// — same self-contained green CI + gitignore so the pipeline's clone and MR
// gate work from commit 1 — but seeds a generic README/AGENTS instead of a
// plan spec. reason is the provenance stamped into the commit + README
// (typically the backlog item id that triggered the mint).
func seedCommitGeneric(branch, slug, reason string) clients.CreateCommitRequest {
	if strings.TrimSpace(branch) == "" {
		branch = "main"
	}
	if strings.TrimSpace(reason) == "" {
		reason = createdBy
	}
	return clients.CreateCommitRequest{
		Branch:        branch,
		CommitMessage: fmt.Sprintf("chore: bootstrap repo for %s [%s]", reason, createdBy),
		Actions: []clients.CommitAction{
			{Action: "create", FilePath: seedPathReadme, Content: seedReadmeGeneric(slug, reason)},
			{Action: "create", FilePath: seedPathCI, Content: seedCI},
			{Action: "create", FilePath: seedPathGitignore, Content: seedGitignore},
			{Action: "create", FilePath: seedPathAgents, Content: seedAgentsGeneric(slug, reason)},
		},
	}
}

// seedReadmeGeneric is the README for a planless bootstrap: a truthful stub
// naming the slug and the handoff that minted it. The first spawned agent
// replaces it as the implementation lands.
func seedReadmeGeneric(slug, reason string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", slug)
	fmt.Fprintf(&b, "Bootstrapped by the Mills operator for handoff `%s`.\n\n", reason)
	b.WriteString("This repo was minted empty for a new-project handoff. The seed pipeline is\n")
	b.WriteString("a placeholder green job; the first spawned agent replaces it with the\n")
	b.WriteString("project's real toolchain, stages, and documentation.\n")
	return b.String()
}

// seedAgentsGeneric gives the first spawned agent the repo's operating context
// when there is no Plan Store plan behind the mint.
func seedAgentsGeneric(slug, reason string) string {
	var b strings.Builder
	b.WriteString("# AGENTS.md\n\n")
	fmt.Fprintf(&b, "This repo (%s) was bootstrapped by the Mills operator for handoff `%s`; it\n", slug, reason)
	b.WriteString("was minted empty (no Spinning Room plan). The handoff/backlog item that\n")
	b.WriteString("triggered the mint carries the acceptance criteria — resolve it before\n")
	b.WriteString("implementing.\n\n")
	b.WriteString("Seed state (replace as work lands):\n\n")
	b.WriteString("- `.gitlab-ci.yml` is a placeholder green pipeline; add real fmt/lint/test stages with the first toolchain slice.\n")
	b.WriteString("- `README.md` is a stub; keep it truthful as the implementation lands.\n")
	return b.String()
}

// seedPathReadme … seedPathAgents name every file the root commit creates. They
// are the single source of truth for BOTH halves of the seed contract: the
// commit actions below, and SeedPaths, which the reconciler stamps into the
// minted item's declared scope. A file seeded here but missing from SeedPaths
// re-creates the 2026-07-27 wedge — see SeedPaths.
const (
	seedPathReadme    = "README.md"
	seedPathCI        = ".gitlab-ci.yml"
	seedPathGitignore = ".gitignore"
	seedPathAgents    = "AGENTS.md"
)

// SeedPaths returns the repo-root files a bootstrap root commit creates, in
// commit order.
//
// These are exported because the scope gate needs them. The seeded AGENTS.md
// tells the first implementer, in as many words, to "add real fmt/lint/test
// stages" to the placeholder .gitlab-ci.yml — and then the scope gate failed
// the run for touching a file the item's slices could not possibly declare,
// because the plan was authored before the repo existed. The amendment
// evaluator correctly refuses a repo-root reach (no shared ancestor with any
// declared directory), so the run escalated on a verdict no retry could move:
// bl-let-s-brainstorm-… on services/housemd, `1 file(s) outside slice scope:
// .gitlab-ci.yml`, twice on an identical diff.
//
// The fix is to declare what we seeded rather than to weaken the gate: the
// reconciler appends these to the item's scope at mint, so the ordinary scope
// gate passes for exactly these four paths on exactly the run that created
// them, and every established repo keeps the unchanged envelope.
func SeedPaths() []string {
	return []string{seedPathReadme, seedPathCI, seedPathGitignore, seedPathAgents}
}

// seedCommit builds the single root commit for the minted repo.
func seedCommit(branch string, plan clients.PlanDetail) clients.CreateCommitRequest {
	if strings.TrimSpace(branch) == "" {
		branch = "main"
	}
	return clients.CreateCommitRequest{
		Branch:        branch,
		CommitMessage: fmt.Sprintf("chore: bootstrap repo from plan %s [%s]", plan.ID, createdBy),
		Actions: []clients.CommitAction{
			{Action: "create", FilePath: seedPathReadme, Content: seedReadme(plan)},
			{Action: "create", FilePath: seedPathCI, Content: seedCI},
			{Action: "create", FilePath: seedPathGitignore, Content: seedGitignore},
			{Action: "create", FilePath: seedPathAgents, Content: seedAgents(plan)},
		},
	}
}

// seedReadme carries the plan's title + spec_doc into the repo so the brief
// travels with the code (spawned agents read the repo, not the Plan Store,
// first).
func seedReadme(plan clients.PlanDetail) string {
	var b strings.Builder
	title := strings.TrimSpace(plan.Title)
	if title == "" {
		title = plan.ID
	}
	fmt.Fprintf(&b, "# %s\n\n", title)
	fmt.Fprintf(&b, "Bootstrapped by the Mills operator from Spinning Room plan `%s`.\n\n", plan.ID)
	if spec := strings.TrimSpace(plan.SpecDoc); spec != "" {
		b.WriteString("## Plan\n\n")
		b.WriteString(spec)
		if !strings.HasSuffix(spec, "\n") {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// seedAgents gives the first spawned agent the repo's operating context: where
// the plan lives and what the seed left unfinished.
func seedAgents(plan clients.PlanDetail) string {
	var b strings.Builder
	b.WriteString("# AGENTS.md\n\n")
	fmt.Fprintf(&b, "This repo was bootstrapped from Loom plan `%s`; the live plan (slices, phases,\n", plan.ID)
	b.WriteString("acceptance criteria) is in the agent-context Plan Store — resolve it with\n")
	fmt.Fprintf(&b, "`agent_plan_get(plan_id=%q)` before implementing.\n\n", plan.ID)
	b.WriteString("Seed state (replace as slices land):\n\n")
	b.WriteString("- `.gitlab-ci.yml` is a placeholder green pipeline; add real fmt/lint/test stages with the first toolchain slice.\n")
	b.WriteString("- `README.md` carries the plan spec verbatim; keep it truthful as the implementation diverges.\n")
	return b.String()
}
