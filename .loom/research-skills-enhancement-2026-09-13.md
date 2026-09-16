# Loom skills review and engineering research — 2026-09-13

## Recommendation

Design the next sprint around **reliable skill delivery and measurable task performance**. Preserve the existing workflow coverage, repair source-to-install gaps, and evaluate a small pilot before changing the entire library. The implementation proposal is in the companion plan-store mirror, `plan-skills-enhancement-2026-09-13.md`.

This is a source review and research brief, not a completed behavioral benchmark. The proposal assumes two engineers over ten working days; actual usage frequency and capacity remain unverified.

## Scope and evidence

Repository: `services/loom-core`, checkout `96e63c4602f2984b08e528fc26ef46224b45b1ab`. The worktree was clean at intake. Reviewed the complete registry structurally; read the generator, validation, manifest, pruning, sync and import paths; inspected representative research, planning, delivery, quality, memory, handoff and retrospective workflows. Compared installed skill names in this session's two home roots.

The authoritative sources are `mcp/context/skills-registry.yaml` and declared resources under `mcp/skills/`. Generated home files provide deployment evidence, not an alternative authoring source. Prior work is recorded in `.loom/brainstorm-skills-registry-optimization-2026-07-19.md` and the skills optimization, consolidation, rule-demotion and manifest-prune changelog fragments.

External sources were accessed on 2026-09-13. Publication/revision dates below come from source pages, not search-engine relative dates. Engineering reports support practical patterns; preprints are emerging evidence, not established consensus. The newest versioned research used is SIGIL v2, 2026-08-26. No claim is made to an exhaustive September literature review.

### Inventory

| Measure | Observed |
|---|---:|
| Registry entries | 80 |
| Registry lines | 9,199 |
| Skills declaring scripts / references / assets | 19 / 26 / 21 |
| Largest common instruction body | 888 whitespace-delimited words |
| Largest common instruction body by lines | 259 lines |
| Names present in both home skill roots | 75 |
| Retired registry names still installed under Codex | `agent-recipes`, `auto-quality-gate` |

Individual bodies are already relatively compact. Broadly shortening every skill is not supported by this audit.

| Target | Enabled entries | Composite instruction entries | Words in common bodies of composite entries |
|---|---:|---:|---:|
| Codex | 80 | 5 | 1,152 |
| Claude | 80 | 4 | 766 |
| Kilocode | 79 | 4 | 766 |
| Gemini | 72 | 30 | 10,826 |
| Antigravity | 80 | 4 | 766 |

These are source counts, not measured prompt tokens. Target appendices, loading behavior and tokenization can change actual context cost. Output types are normalized by generation: e.g. Codex renders non-instruction entries as bundles, and Antigravity normalizes legacy command entries. Do not equate raw YAML type counts with host behavior. See `pkg/skills/generator.go:132`.

### Verification performed

- `loom generate skills --target all --registry mcp/context/skills-registry.yaml --validate`: passed; 80 skills, no reported resource errors. This command used installed Loom `v0.10.0-3241-g338c407a9`, so it is not evidence that the checkout's CLI was rebuilt.
- `GOWORK=off GOCACHE=/private/tmp/loom-skills-review-go-cache GOMAXPROCS=2 go test -p 2 ./pkg/skills`: passed against the reviewed checkout.
- The initial test attempt with workspace mode enabled failed because this linked tree cannot resolve relative sibling modules in `go.work`. Module mode resolved the focused test without changing source.
- Python/PyYAML inventory counted entries, resource declarations, composite words and installed names. Counts refer to `common.instructions.split()` and explicit enabled/type fields.
- Loom recall and the codebase index were reachable. Index reported 1,261 chunks; freshness was not established, so code claims use direct file reads.
- Plan create/get tools are callable. Plan search/render were discovered through `loom tools get` and are available through `loom tools call`, even though they are absent from this session's direct callable list. Absence from the direct list does not mean an API was removed.
- No live client trigger tests, paid eval batch, home synchronization, runtime deployment or full repository test suite was performed.

## Findings ranked for the sprint

### F1 — Successful generation/sync can conceal incomplete or stale delivery (P1)

`Generator.Validate` checks declared resource existence, a Claude listing length cap and heuristic script approvals; it does not prove delivery. `copyBundleResources` can suppress resource-copy errors unless verbose logging is enabled, then returns success. Codex/Gemini generation has similar warning-only paths. See `pkg/skills/generator_validation.go:82`, `pkg/skills/generator_bundle.go:50`, `pkg/skills/generator_codex_gemini.go:45`.

Manifest-based pruning already exists and preserves unmanaged neighbors (`pkg/skills/prune.go:10`). However, the non-direct-to-home branch of `Manager.SyncSkills` copies the new manifest to home without comparing the old home manifest for stale entries. It also continues after copy failures and reports a successful sync. See `pkg/sync/ops_regen.go:334`. A removed generated file can therefore survive in home while its ownership record disappears. This code path warrants a regression fixture; it is not proven to be the sole cause of this machine's stale installations.

**Enhancement:** stage a complete bundle, fail on required copy errors, prune against the previous destination manifest, publish the manifest only after successful delivery, and test upgrade/removal/partial-failure paths.

### F2 — Installed catalog identity is ambiguous (P1)

The session exposes 75 names from both `~/.agents/skills` and `~/.codex/skills`. Their bytes differ, but platform text and resolved paths can legitimately explain differences. A raw hash comparison is insufficient to label all 75 as defects.

More concretely, `~/.codex/skills/agent-recipes/SKILL.md` still teaches removed `agent_recipe_*` calls, despite consolidation into `agent-engrams`. `auto-quality-gate` also remains after consolidation into `quality-gate-loop`. Current registry bodies correctly describe recipe removal; a linter must distinguish historical/deprecation mentions from executable examples.

**Enhancement:** report effective discovery roots, canonical identity, source revision and normalized content differences; migrate only provably Loom-owned stale files. Diagnose unowned duplicates without deleting custom or plugin skills.

### F3 — Authoring examples and runtime contracts have diverged (P1)

The scaffold generates Claude `type: command`, and the builder instructions teach that default, although the registry's documented preference and current builder entry use `type: skill`. Sources: `mcp/skills/loom-skill-builder/scripts/skill_scaffold.py:126`, `mcp/context/skills-registry.yaml:3395`.

The planning skill's create example advertises `riskiest_assumption`, `kill_test` and `success` (`mcp/context/skills-registry.yaml:715`), but the registered MCP create schema does not expose those fields (`cmd/mcp-agent-context/tools_plan.go:20`). The handler does parse them (`pkg/agentcontext/svc_plans.go:107`), making this a schema/documentation mismatch rather than missing storage support.

`Load` uses permissive YAML unmarshalling; validation does not provide a complete skill-name/description/target/schema contract (`pkg/skills/registry.go:71`). The script approval heuristic accepts the mere presence of `--dry-run` or an opt-in marker; that is a lint convention, not an execution permission boundary (`pkg/skills/generator_validation.go:61`).

**Enhancement:** executable example fixtures for the pilot, strict authoring diagnostics, updated scaffold defaults, and verified per-host capability rules. Preserve existing session authorization when describing landing; passing tests establishes quality, not additional authority.

### F4 — Skill-specific behavioral evaluation is missing (P1)

The `pkg/skills` suite covers escaping, generation, manifests, pruning, resources, imports and targeted registry contracts. It does not measure whether an agent chooses the appropriate skill or whether that skill improves a real task. A content change can pass the suite while introducing over-triggering, skipped proof, excessive setup or incorrect completion.

Loom is not starting from zero: `pkg/mills/eval/eval.go:1` provides council/per-merge/cross-run evaluation, and `pkg/llmusage` provides usage accounting. These are useful integration points, but the Mills rubric's input is a council artifact; it should not be relabeled a skill benchmark.

**Enhancement:** add separate selection and outcome datasets, compare current versus candidate bundles, capture traces and costs, and reuse existing reporting/accounting where it fits.

### F5 — Context and workflow scope need selective experiments (P2)

Gemini's 30 composite entries carry 10,826 common-body words before target additions. Meanwhile the July changes already moved many rules to on-demand loading elsewhere. This is a promising optimization target, not proof that Gemini performance is worse.

Research/planning has good local-first sourcing and explicit kill tests. Delivery skills preserve scoped commits and CI follow-through. Handoff already supports selective context. Retrospectives already queue failures and instruction gaps (`mcp/context/skills-registry.yaml:2780`, `:4382`, `:1397`, `:7127`).

The remaining opportunity is precise task boundaries and executable proof: avoid pulling a small edit into an unnecessary feature/worktree loop; ensure a planning-only request remains planning; distinguish unavailable evidence from failed implementation; require handoffs to carry current artifact revision, proof, unresolved decisions and next action.

**Enhancement:** test a compact always-loaded routing/index layer plus on-demand procedures; retain each platform's existing loading policy until its own evidence supports a change.

### F6 — Imported skill provenance is recorded but not immutable (P2, follow-on)

Hosted metadata records source URLs, file names and import time, but no immutable commit or content digest (`pkg/skills/hosted.go:48`). The skills.sh importer fetches a repository tree and files by default branch (`pkg/skills/skillssh.go:158`, `:188`).

**Enhancement:** follow this sprint with commit-resolved imports, bundle digests, staged upgrades and integrity verification. URL provenance alone cannot reproduce exactly what was installed. This is distinct from sandbox or permission enforcement.

## What current engineering sources support

| Source and date | Evidence and limits | Implication for Loom |
|---|---|---|
| [Agent Skills specification](https://agentskills.io/specification), living spec accessed Sep 13 | Standard bundle layout, required name/description constraints, optional compatibility and progressive loading. Experimental tool allowlists are host-dependent. | Validate portable structure separately from host extensions. Keep authorization enforcement outside prose. |
| [Agent Skills authoring guidance](https://agentskills.io/skill-creation/best-practices), living guide accessed Sep 13 | Recommends domain-specific procedures, concise bodies, concrete gotchas and staged references. A coherent unit can be too broad or too narrow. | Preserve Loom's compact bodies and refine from observed failures; avoid adding generic advice or indiscriminate fragmentation. |
| [Description evaluation guidance](https://agentskills.io/skill-creation/optimizing-descriptions), living guide accessed Sep 13 | Treats activation as an evaluable behavior, using realistic should/should-not-trigger prompts. | Build negative and near-neighbor routing cases, not only happy-path demonstrations. |
| [Output evaluation guidance](https://agentskills.io/skill-creation/evaluating-skills), living guide accessed Sep 13 | Compares with-skill and baseline runs, reviews artifacts and traces, and captures tokens and duration. | Separate selection from execution quality; test against the previous version and a no-skill control. |
| [Anthropic: Demystifying evals](https://www.anthropic.com/engineering/demystifying-evals-for-ai-agents), Jan 9, 2026 | Combines deterministic checks, calibrated model judgment and human review; distinguishes capability from regression suites. | Grade actual state and delivery evidence; reserve model judges for semantic quality and report uncertainty. |
| [Vercel: AGENTS.md versus skills](https://vercel.com/blog/agents-md-outperforms-skills-in-our-agent-evals), Jan 27, 2026 | On its Next.js 16 benchmark, a compact docs index scored 100%, explicit skill invocation 79%, and the default skill 53%; 56% of default cases never invoked the skill. Vendor/task-specific, not a universal ranking. | This disconfirms “on-demand skills always win.” Test a small index variant for Loom rather than assuming activation works. |
| [OpenAI: Harness engineering](https://openai.com/index/harness-engineering/), Feb 11, 2026 | Engineering case study emphasizes a navigable repository knowledge base and mechanical feedback for constraints and drift. Its productivity estimates are not transferable benchmarks. | Keep the index small and connect workflow instructions to executable checks. |
| [Anthropic: Long-running harness design](https://www.anthropic.com/engineering/harness-design-long-running-apps), Mar 24, 2026 | Reports benefits from bounded work, structured handoffs and a separately calibrated evaluator; also notes orchestration costs and model-specific context behavior. | Pilot interrupted-task recovery and independent artifact grading. Do not add multiple agents to every task. |
| [Birgitta Böckeler / Thoughtworks](https://martinfowler.com/articles/harness-engineering.html), Apr 2, 2026 | Distinguishes advance guidance from feedback and deterministic checks from model judgment. Practitioner framework, not a controlled trial. | Every important workflow promise should have a corresponding observable check where practical. |
| [Anthropic: Agentic coding and expertise](https://www.anthropic.com/research/claude-code-expertise), Jun 16, 2026 | Observational analysis of approximately 400,000 sessions associates domain expertise with effective agent use. Success is inferred from transcripts and evidence, not directly observed user outcomes. | Capture concrete project decisions and recurring corrections. Avoid treating generic procedural volume as expertise. |
| [Destefanis: Authoring Agent Skills](https://arxiv.org/abs/2607.25032v1), Jul 27, 2026 | A design note applies cohesion, interface/implementation separation, low coupling and behavioral evaluation to skills. Preprint; not an empirical proof of gains. | Treat descriptions as the selection interface and bundles as versioned engineering artifacts. |
| [SIGIL v2](https://arxiv.org/abs/2607.27309v2), revised Aug 26, 2026 | Reports applicable-mandate compliance increasing from 66.0% to 88.6% across 33 skills and three runtime models when procedural structure is compiled. Self-reported preprint results; not replicated here. | Move a few fragile deterministic steps into scripts/tools and evaluate them. A new general skill compiler is outside the sprint. |

The synthesis has support from independent engineering organizations: evaluate selection (Agent Skills + Vercel), connect guidance to checks (OpenAI + Thoughtworks + Anthropic), and use task-specific context rather than instruction volume (Agent Skills + OpenAI). The preprints strengthen the research agenda but do not set Loom's expected improvement.

### Research exclusions and uncertainty

A search result for arXiv `2609.00006` exposed inconsistent recency metadata: the identifier suggests September while the retrieved submission history says July. It was not used to claim a September development or justify the sprint. Secondary popularity lists and install counts were also excluded as quality evidence.

Open questions for the sprint's first slice: which skills account for actual usage; which clients expose reliable activation traces; whether the two home roots are independently managed; and what task/cost baseline a representative benchmark produces.

## Proposed design choices

Use an explicitly labeled **observed / inferred / unavailable** state for skill loading. A model saying it used a skill is not proof. File-read/tool traces and pinned bundle identities are stronger evidence, but host adapters must state their limitations.

Start with five pilot skills: `research`, `plan-loom-core`, `small-change-loop`, `quality-gate-loop`, `multi-agent-handoff`. These cover research, technical writing, testing/delivery, and coordination; add troubleshooting boundary cases through `ci-failure-recovery` and a deferred `k8s-debug` pilot. Selection is based on workflow coverage, not measured popularity.

Maintain an evidence chain from skill revision to invocation evidence, artifact/diff, verification result and terminal outcome. Preserve unknown usage/cache fields as unknown. Count retries, user corrections and time to verified result alongside tokens. Compare the same model, host, permissions and fixture when attributing a difference to a skill.

Use two release tiers: deterministic conformance checks on every relevant change; bounded live evaluations for pilot changes and release candidates. Do not gate ordinary documentation edits on a fleet-wide paid benchmark.
