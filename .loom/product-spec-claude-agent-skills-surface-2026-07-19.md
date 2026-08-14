# Product Spec: Claude Agent Skills Surface Migration (2026-07-19)

Migrate the skills generator's Claude target from legacy `.claude/commands/<name>.md`
slash-command files to the Agent Skills format `.claude/skills/<name>/SKILL.md`,
with support for the newer Claude Code frontmatter fields (`when_to_use`,
`disable-model-invocation`, `context: fork`). This is the deferred F4 framing from
`.loom/brainstorm-skills-registry-optimization-2026-07-19.md` (shipped as !1166).

## Riskiest assumption + kill-test

**Load-bearing assumption**: Claude Code CLI (v2.1.152+, the version in local use)
discovers loom-generated skills at `~/.claude/skills/<name>/SKILL.md`, lists them
using the frontmatter `description` (+ `when_to_use`), and loads the body when
invoked — and legacy `.claude/commands/<name>.md` files coexist during transition
with the skill taking precedence on a name collision.

**Kill test**: Write a probe skill to `~/.claude/skills/loom-skills-migration-probe/SKILL.md`
with a distinctive body marker. Verify (a) it appears in a live Claude Code
session's skill listing with the frontmatter description, and (b) invoking it
returns the marker verbatim. ≤10 minutes. Re-run with **generator-produced**
output (a pilot registry skill flipped to `type: skill`, regenerated via
`loom sync skills claude`) before converting all 81 skills.

**Failure mode if wrong**: We rewrite the generator toward a surface Claude Code
does not actually read (the exact failure class that produced the
spec-riskiest-assumption rule), and 68 slash commands silently vanish for the
primary daily-driver platform.

**Status**:
- Hand-written probe: **passed 2026-07-19** — probe skill appeared in the live
  session skill listing and returned `LOOM_PROBE_MARKER_88431_OK` on invocation.
- Generator-produced pilot: **passed 2026-07-19** — see "Kill-test evidence".

**Positive evidence**: https://code.claude.com/docs/en/skills.md — frontmatter
reference documents `when_to_use` ("Appended to `description` in the skill
listing and counts toward the 1,536-character cap"), `disable-model-invocation`,
`context: fork`, and personal-skills discovery at `~/.claude/skills/`.
Live probe above.

**Negative search**: "claude code when_to_use not supported" surfaced no
conflation — `when_to_use` is a Claude Code extension absent from the
agentskills.io open spec, so it must be emitted only for the Claude target
(Codex/Gemini/Zed/OpenCode bundles keep the portable field set). Known gotchas
found are activation-rate realities (description quality, 1,536-char listing
truncation), not missing support. One caveat that shapes the kill-test: **new
skill directories are not hot-reloaded mid-session** (require `/reload-skills`
or a fresh session) — our live session detected the new dir anyway, but the
kill-test design does not depend on hot-reload.

## Context

- `loom sync skills claude` generates 68 command-type skills directly into
  `~/.claude/commands/*.md` (SkillsDirectToHome; `pkg/sync/manager.go:149-152`),
  9 rule-type into rules, 4 instruction-type into composite output.
- The command path drops bundled resources: `generateClaudeCommand`
  (`pkg/skills/generator_claude_kilocode.go:27`) writes a single .md and never
  copies `scripts/`, `references/`, `assets/` — 34 of 81 skills have resources
  that Claude never receives today (Codex/Gemini get full bundles).
- Claude Code's current-generation surface is Agent Skills
  (`.claude/skills/<name>/SKILL.md`); `commands/` is legacy-compatible.
- Bundle machinery already exists and is proven for codex/gemini/zed/opencode:
  `generateBundleSkill` (`pkg/skills/generator_bundle.go:19`),
  `codexManifestFiles` (`pkg/skills/generator.go:254`).

## Goals

1. Claude target emits Agent Skills bundles: `skills/<name>/SKILL.md` + copied
   scripts/references/assets, delivered direct-to-home (`~/.claude/skills/`).
2. New frontmatter fields, registry-driven:
   - `when_to_use` — extra trigger phrases; Claude-only emission; combined
     description+when_to_use validated against the 1,536-char listing cap.
   - `disable-model-invocation: true` — user-only skills (side-effectful
     ship/commit loops) stop being model-invokable and stop costing context.
   - `context: fork` — supported as pass-through for task-type skills.
3. Dual-layout transition: `type: command` keeps generating legacy files;
   `type: skill` generates bundles. Both work in one registry, one sync run.
4. Stale-file hygiene: migrating a skill prunes its orphaned
   `commands/<name>.md` (home + repo/workspace copies).

## Non-goals

- No change to codex/gemini/kilocode/zed/opencode/antigravity outputs.
- No change to rule-type (9) or instruction-type (4) Claude skills.
- No skill-family consolidation (separate deferred follow-up from !1166).
- Cloud/cowork sessions load only project `.claude/skills/`, not
  `~/.claude/skills/` — same limitation the legacy home commands had; out of
  scope here.

## Design

### Registry schema (`pkg/skills/registry.go`)

```yaml
common:
  when_to_use: "extra trigger phrases"     # optional, new
targets:
  claude:
    type: skill                            # was: command
    when_to_use: "claude-specific triggers" # optional override of common
    disable_model_invocation: true          # optional, new
    context: fork                           # optional, new ("" | "fork")
```

- `SkillSpec.WhenToUse` (`when_to_use`) + `TargetSpec.WhenToUse` override.
  Rationale for the override: Codex shares one ~8k-char metadata budget across
  all skills, so descriptions were deliberately trimmed in !1166; Claude-only
  `when_to_use` restores trigger richness without re-inflating Codex.
- `TargetSpec.DisableModelInvocation *bool`, `TargetSpec.Context string` —
  read only by the Claude generator; other targets ignore them.

### Generator (`pkg/skills/generator_claude_kilocode.go`)

- `generateClaudeSkillByType` routes `type: skill` → new
  `generateClaudeAgentSkill` (mirrors `generateBundleSkill`): writes
  `<baseDir>/skills/<name>/SKILL.md` atomically, copies scripts/references/
  assets, returns `codexManifestFiles(skill)` so the manifest carries
  `skills/<name>/...` paths.
- Frontmatter emitted (quoted+escaped, strict-YAML valid):
  `name`, `description`, then `when_to_use`, `disable-model-invocation`,
  `context` only when set.
- `${SKILL_PATH}` resolves to the generated bundle dir
  (`<baseDir>/skills/<name>`) — the existing `claude` case in
  `ResolveInstructions` substitutes whatever path is passed, so the bundle
  generator passes the final skill dir instead of the mcp/skills source dir.
- After a successful bundle write, prune stale `<baseDir>/commands/<name>.md`.
- `type: command` path is untouched (dual layout).

### Sync (`pkg/sync/`)

- Claude profile `SkillsHomePath` updated `$HOME/.claude/commands` →
  `$HOME/.claude/skills` (descriptive only for Claude; delivery is
  generator-write-driven because `SkillsDirectToHome=true`).
- `cleanSkillsAt` additionally prunes registry-derived `commands/<name>.md`
  from repo/workspace `.claude/` copies (name-scoped — never `RemoveAll` on
  `commands/`, which may hold hand-authored files). The existing
  `RemoveAll(<dir>/skills)` on repo copies is unchanged and now also covers
  stale repo-local bundles.
- No changes to the direct-to-home flow: generator writes bundles straight to
  `~/.claude/skills/`, manifest to `~/.claude/.loom-skills-manifest.json`.
- Hazard noted: hosted-skill imports also land in `~/.claude/skills`
  (`cmd_sync_skills.go` import root). Generated bundles co-locate by design;
  nothing does `RemoveAll` on the home skills dir, and pruning stays
  manifest/name-scoped.

### Validation (`pkg/skills/generator_validation.go`)

- New check: for each Claude-enabled skill, effective
  `len(description) + len(when_to_use)` ≤ 1,536 chars (listing truncation cap),
  else validation error.

### Field policy for the 81 skills

- **68 command-type → `type: skill`.**
- **`disable_model_invocation: true`** initial set (explicitly user-triggered,
  side-effectful ship loops; vendor guidance: don't let the model fire deploy/
  commit workflows autonomously): `commit`, `caveman-commit`,
  `ci-green-merge-loop`. Note: this removes their descriptions from model
  context entirely — "ship it" phrasing routing stops being skill-driven; the
  user invokes `/commit` etc. Easily extended per-skill later.
- **`when_to_use`**: populated opportunistically where !1166 trimmed triggers
  for the Codex budget; not bulk-added (descriptions already carry triggers).
- **`context: fork`**: pass-through supported, **applied to zero skills in
  this pass**. Forked skills run in a subagent and only return a summary —
  right for read-heavy task skills (candidates: `backlog-triage`, `research`,
  `deep-research`-style loops) but a behavior change to evaluate per-skill in
  a follow-up.

## Slices

1. **S1 — generator + schema + tests** (no registry flip): everything under
   Design above; unit tests for bundle layout, frontmatter fields, manifest
   paths, `${SKILL_PATH}`, command pruning, 1,536-cap validation; existing
   command-path tests stay green.
2. **S2 — pilot + live kill-test (GATE)**: flip one skill to `type: skill`,
   `loom sync skills claude`, verify generated bundle loads/invokes in a live
   Claude Code session. S3 is blocked until this passes.
3. **S3 — registry flip + ship**: convert the 68 entries, set
   `disable_model_invocation` for the initial set, regen, strict-YAML parse
   of all generated frontmatter (the !1166 kill-test recipe), changelog.d
   fragment, MR with auto-merge. Mirror registry change to
   `platform/gitops/mcp/context/skills-registry.yaml` (separate gitops MR).

## Rollback

Per-skill and reversible: flip `targets.claude.type` back to `command` and
re-run `loom sync skills claude` — the command file regenerates and the next
regen cycle prunes nothing (bundle pruning is tied to bundle generation).
Claude Code reads both layouts, so a mixed state is safe indefinitely.

## Kill-test evidence (S2)

- 2026-07-19 hand-written probe: listed + invoked in live session, marker
  returned verbatim (pre-implementation mechanism proof).
- 2026-07-19 generator-produced pilot (`auto-quality-gate`, script-bearing):
  registry entry flipped to `type: skill`, `loom sync skills claude` run from
  the feature worktree. Result: `~/.claude/skills/auto-quality-gate/SKILL.md`
  written with strict-YAML-valid frontmatter, `scripts/auto-quality-gate.sh`
  bundled alongside, stale `~/.claude/commands/auto-quality-gate.md` pruned,
  and a live Claude Code session listed the skill from the new location and
  loaded its body on invocation (skill base directory reported as
  `~/.claude/skills/auto-quality-gate`). Gate for S3: **open**.
