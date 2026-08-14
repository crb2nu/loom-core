# Brainstorm: Skills Registry Optimization (2026-07-19)

Review + enhancement pass over `mcp/context/skills-registry.yaml` (81 skills, ~9.3k lines),
grounded in a web-research sweep of 2025–26 skill-authoring guidance and a full
line-level audit of the registry.

## Riskiest assumption + kill-test

**Load-bearing assumption**: The loom skills generator (`loom generate skills`)
faithfully propagates registry `common.description` into every platform's
frontmatter as *valid YAML*, so description/trigger improvements in the registry
actually reach agent skill listings.

**Kill test**: After regen, parse every generated `.claude/commands/*.md`,
`.codex`/`.gemini` SKILL.md frontmatter with a strict YAML parser
(`python3 -c "yaml.safe_load(...)"`) — zero parse failures, and spot-check that
3 rewritten descriptions appear verbatim in generated output. ≤10 minutes.

**Failure mode if wrong**: Descriptions edited in the registry never reach (or
corrupt) the agent-facing listings, so trigger-quality work is wasted.

**Status**: FAILED pre-fix 2026-07-19 (33/68 generated Claude command files had
YAML-invalid unquoted `description:` scalars) → generator fixed
(`pkg/skills/generator_claude_kilocode.go` now quotes + escapes) →
**passed 2026-07-19**: post-regen strict parse of 319 generated files across
claude/codex/gemini/antigravity/kilocode = 0 YAML failures, 0 missing
descriptions; rewritten descriptions verified verbatim in output.

## Research inputs (key sources)

- https://platform.claude.com/docs/en/agents-and-tools/agent-skills/best-practices
- https://code.claude.com/docs/en/skills (frontmatter extensions, 1536-char desc cap)
- https://agentskills.io/specification (SKILL.md open spec; <500-line body, 1024-char description)
- https://www.anthropic.com/engineering/equipping-agents-for-the-real-world-with-agent-skills
- https://learn.chatgpt.com/docs/build-skills (Codex: ~8k-char TOTAL metadata budget across all skills)
- https://geminicli.com/docs/cli/skills/

Distilled rules applied here:
1. Descriptions: third person, "what + when", concrete trigger keywords, pushy
   (agents under-trigger), exclusion clauses to disambiguate overlapping skills.
2. Codex shares one ~8k-char metadata budget across ALL skills → front-load the
   key use case; keep descriptions tight (icc-* were up to ~520 chars).
3. Body <500 lines / ~5k tokens; move deep detail to `references/` (progressive
   disclosure); file references one level deep.
4. Scripts for deterministic steps; make execution intent explicit; no
   time-sensitive or session-specific content in durable skills.
5. One default with an escape hatch — not menus of options.

## Framings considered

- **F1 "Pipeline first"**: the biggest quality loss is between registry and
  agent (invalid frontmatter, always_allow schema violations, dup delivery) —
  fix the generator/validator before touching prose. Bet: infrastructure
  correctness dominates prose quality. → Confirmed by audit: registry data was
  complete; the pipeline was dropping/corrupting it.
- **F2 "Trigger quality"**: skills are discovered via description strings;
  rewrite the ~18 trigger-poor descriptions using pushy what+when phrasing.
  Bet: better triggering > better bodies, since bodies only load post-trigger.
- **F3 "Consolidation"**: 4-skill memory cluster, research×2, quality-gate×3,
  9× repeated auto-ship tail — merge to cut drift. Bet: duplication is the main
  maintenance risk. → Real but higher-risk (behavior changes across platforms);
  defer to follow-up slices.
- **F4 "Modernize surface"**: move Claude output from legacy `.claude/commands`
  to `.claude/skills/<name>/SKILL.md` with new frontmatter (`when_to_use`,
  `disable-model-invocation`, `context: fork`). Bet: vendor surface has moved.
  → Real, but a generator rewrite; defer with a spec.

## Convergence (this pass)

Ship F1 fully + F2 for the worst offenders + safe slices of F3-adjacent
staleness fixes. Explicitly defer F3 merges and F4 surface migration.

Applied:
1. Generator: quote/escape Claude command frontmatter descriptions (+ regression test).
2. Validator: `>/dev/null`, `>&2`, `->`, `> 0` no longer count as writes;
   explicit `# loom-always-allow: <rationale>` opt-in marker (+ tests).
3. Registry: 133 → 0 validation errors — removed 6 ICC MCP-tool-name
   `always_allow` blocks (server-level approvals already in registry.yaml),
   removed `gitlab_green_loop.sh` (auto-commit/push/merge by default violates
   the safety bar), added rationale markers to 6 expected-write scripts.
4. Descriptions: rewrite trigger-poor / oversized descriptions (what + when +
   trigger keywords + exclusion clauses; Codex budget-aware).
5. Staleness: platform-config-sync Kilocode/Gemini config rows; quality-gate-loop
   dead `quality_*` tool names → `devbox_quality_gate`; drop dated
   session-specific references from icc-weekly-status-loop; un-gitignored-path
   citation in spec-riskiest-assumption; explicit `targets:` for
   auto-quality-gate + tdd-dev.

Deferred (follow-ups):
- Merge research → research-docs-workflow; agent-recipes → agent-engrams;
  consolidate agent-context / multi-agent-handoff / long-term-memory /
  memory-practices overlap.
- Extract shared auto-ship tail into `mcp/skills/_shared/` reference (9 copies today).
- Move rust-acceleration's 259-line body into `references/`.
- Claude surface migration to `.claude/skills/` SKILL.md format with
  `when_to_use` / `disable-model-invocation` (needs its own spec + kill-test).
- Single-scope Claude command delivery (project vs `~/.claude`) to end doubled
  skill listings; prune orphan `~/.claude/commands/README.md`.
