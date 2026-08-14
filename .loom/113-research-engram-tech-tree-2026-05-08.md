# Research: Engrams as a Tech Tree for Agentic Software Development

> Date: 2026-05-08
> Status: Draft — exploratory
> Scope: Evaluate the Ark Survival Evolved "engram" metaphor as a framing for a structured, prerequisite-aware library of software patterns/algorithms in loom-core.

---

## 1. The Source Metaphor (Ark)

In Ark Survival Evolved, an **engram** is a learnable blueprint:

- Each engram is **compact**: one icon, one recipe, one purpose ("Stone Hatchet", "Industrial Forge").
- Engrams form a **DAG of prerequisites**: you cannot learn the Industrial Forge until you have unlocked smelting, refined metal, and electricity.
- Engrams are **proof-gated**: you spend hard-earned points and must reach a level threshold; you cannot bluff your way past requirements.
- Engrams are **persistent and recombinable**: once learned, an engram is always available, and complex items are crafted by combining many engram outputs.
- The tech tree is **discoverable**: the player can browse what they have not yet unlocked and see the path to get there.

Mapped to software: an engram is a **vetted, compact, reusable solution to a specific problem class**, with explicit prerequisites and proof of correctness, that can be combined with other engrams to build larger systems.

---

## 2. What Already Exists in Loom-Core

The metaphor is not landing on bare ground. Several primitives already approximate parts of an engram system:

### 2.1 `agent_recipe_*` MCP tools (closest analogue)
- Source: [tools_recipes.go](cmd/mcp-agent-context/tools_recipes.go), [svc_recipes.go](pkg/agentcontext/svc_recipes.go)
- Schema: [recipe-schema.md](mcp/skills/agent-recipes/references/recipe-schema.md)
- Fields today: `title`, `problem`, `solution`, `proof` (required), `tags`, `language`, `scope` (`project`|`workspace`|`universal`).
- Storage: long-term memory item with `category="recipe"` ([svc_recipes.go:50-74](pkg/agentcontext/svc_recipes.go:50)).
- Tools: `agent_recipe_add`, `agent_recipe_recall`, `agent_recipe_list`.

**Gap relative to engrams:** flat namespace. No prerequisite links, no tiering, no graph query, no concept of "unlocked vs locked" for an agent in a given context.

### 2.2 Agent memory tiers
- Source: [agent_memory_*](cmd/mcp-agent-context/) tools (working / episodic / long_term, with `importance` and `category`).
- Provides the storage substrate engrams would sit on. A recipe is already a `long_term + category=recipe` item.

### 2.3 `mcp/skills/` library
- Source: [skills-registry.yaml](mcp/context/skills-registry.yaml), `mcp/skills/*/SKILL.md`.
- Skills are workflow-level (e.g. `feature-dev`, `bugfix`, `code-review`) — they are *macros over many tool calls*, not encapsulations of a single algorithm/pattern.
- Skills already have a generated registry pattern that could be reused for engrams.

### 2.4 `pkg/aimodels` role-based resolver
- Recently added (commit `3927046d`, `f25b4719`). Demonstrates the loom-core house pattern of **central registry → role-based lookup → consumers stay decoupled from concrete implementations**.
- Engrams should follow the same shape: registry of engrams, lookup by role/problem-class, consumer code does not need to know which engram fulfilled the role.

### 2.5 Prior planning docs that touch this space
- [.loom/77-research-agentic-engineering-patterns-2026-04-05.md](.loom/77-research-agentic-engineering-patterns-2026-04-05.md) — Section 1.3 ("Knowledge Hoarding & Recombination") explicitly identifies the missing "structured trick library" gap and proposed the original recipe schema.
- [.loom/73-planning-productivity-unlocks-2026-04-03.md](.loom/73-planning-productivity-unlocks-2026-04-03.md) — Section 2 ranks "Agent recall reranking" #4; engram graph traversal would feed into that.
- [.loom/64-planning-next-gen-skills-agents-orchestration-2026-03-29.md](.loom/64-planning-next-gen-skills-agents-orchestration-2026-03-29.md) — orchestration of skills as the next layer above tools.

**The recipe system landed (Q1 2026); engrams are the natural next iteration.**

---

## 3. External Prior Art

| System | What it does well | Why it is not enough alone |
|--------|------------------|----------------------------|
| Refactoring catalogs (Fowler) | Named, recombinable patterns with before/after examples | Static text; no executable proof; no graph traversal |
| Design Patterns (GoF) | Stable shared vocabulary across decades | No machine-readable form; agents re-derive each time |
| RosettaCode | Cross-language manifestations of the same algorithm | No prerequisites; quality varies; no reliability gate |
| Cookiecutter / yeoman generators | Compact, runnable scaffolds | Coarse-grained (project-level), not pattern-level |
| Skills (Anthropic) / Claude commands | Workflow macros with permission scopes | Procedural, not declarative; no DAG over patterns |
| Code search (Sourcegraph, GitHub) | Find arbitrary examples | No quality signal; no proof-of-correctness contract |
| Tactic/lemma libraries (Coq, Lean, Mathlib) | Proof-gated, prerequisite-aware DAG | Heavyweight; only practical inside a proof assistant |
| LangChain / LangGraph "agent recipes" | Pre-baked graphs for common agent tasks | Frozen; not authored by the local agent; opinionated framework lock-in |

**The engram framing borrows the most from Mathlib (DAG + proof gate) and refactoring catalogs (named, recombinable, cross-language), but specialized to agentic codegen.**

---

## 4. Why the Game Metaphor Earns Its Keep

A fancy name is only useful if it changes behavior. The engram framing does, in three concrete ways:

1. **Prerequisites become first-class.** A flat recipe library has no answer to "before you suggest a CRDT-backed cache, can you confirm the agent (and the codebase) understands eventual-consistency primitives?" Engrams force the question because every engram lists its prerequisites, and the planning step traverses the DAG.
2. **Proof scales with tier.** Tier-1 engrams (idioms, error wrapping, retry/backoff) require a code reference. Tier-3 engrams (consensus, sagas, distributed tracing) require a runnable test plus a benchmark. The same `proof` field exists today, but the *required quality bar* is implicit. Tiering makes it explicit.
3. **"Locked" is meaningful.** When an agent is told "this codebase has not yet adopted engram `circuit-breaker`," it knows not to silently introduce one — it must either unlock it (file a small PR that adds the primitive plus a test) or pick an alternative engram. This converts a vague "be consistent with the codebase" instruction into a checkable contract.

If the metaphor did not produce those three behaviors, it would just be branding. It does, so it is worth the rename.

---

## 5. Risks and Anti-patterns

- **Metaphor inflation.** Don't model XP points, character classes, or crafting time. Take only `prerequisites`, `tier`, and `unlocked` from the metaphor; everything else is dressing.
- **Over-encyclopedization.** The library should be earned, not exhaustive. An engram only exists if (a) at least one real loom-core or workspace task triggered its creation, and (b) it has passing proof. A 500-engram library that no one uses is failure; a 30-engram library where every entry has been recalled and recombined is success.
- **Re-implementing pattern.com.** This is not a textbook. Engrams must point at *this workspace's* code where possible (`pkg/aimodels` for "role-based resolver", not a Wikipedia link).
- **Bypassing recipes.** If we ship a parallel system, recipes will rot and engrams will compete with them. The plan must be: engrams subsume recipes; old recipes auto-migrate as Tier-1 untiered engrams.
- **Premature graph UX.** A Mermaid render in HUD is appealing but optional. The MVP only needs the underlying DAG and a recall API; visualization is a follow-up.

---

## 6. Open Questions to Resolve in the Spec

1. **Identity.** Are engrams identified by stable slugs (`engram://retry-with-jitter`) or by content-hash? Slugs win for human readability and rename-stability across content edits.
2. **Authoring.** Who can `agent_engram_add`? Any agent, or only after a proof gate (e.g. test must already pass)? Likely: any agent, but `proof_status` defaults to `unverified` until a CI job validates the proof.
3. **Scope inheritance.** A `workspace`-scope engram should be visible to all repos. What happens when a `project`-scope engram in repo A would be useful in repo B — is there a promotion flow? Reuse the existing `agent_memory_promote` pattern.
4. **Combination semantics.** When an agent says "I want to combine engrams X and Y," is that a free-form prompt augmentation, or a structured `agent_engram_compose` call that returns a merged spec? MVP: prompt augmentation only. v2: structured compose.
5. **Stale-proof detection.** A proof that points at `pkg/foo/bar.go:42-58` rots when that file changes. CI needs a periodic job that re-validates engram proofs and flips status to `stale`.
6. **Cross-language manifestations.** Is one engram allowed to have multiple `solution` blocks (one per language), or is it one engram per (pattern, language)? Recommend: one engram per (pattern, language); a `family` field links siblings.

---

## 7. Recommendation

Proceed with engrams as **a typed extension of the existing recipe system**, not a parallel one. Land in three slices:

- **S1** — Schema extension on `Recipe` struct: add `prerequisites []string`, `tier int`, `family string`, `proof_status enum`, `unlocked_in []string` (repos/branches). Ship behind a feature flag so existing recipes stay valid.
- **S2** — Graph-aware recall: `agent_engram_recall(query, depth, include_locked)` returns the matching engrams plus their transitive prerequisites, ordered by tier. Reuse the recall reranking work proposed in [.loom/73:Section 2](.loom/73-planning-productivity-unlocks-2026-04-03.md).
- **S3** — Proof validation job: a scheduled CI task that runs each engram's proof and flips `proof_status` (`verified` / `stale` / `failing`). Surface counts in HUD.

Defer until proven valuable: visualization in HUD, structured compose API, cross-agent unlock notifications.

The detailed schema, API surface, and acceptance criteria live in [.loom/114-product-spec-engram-tech-tree-2026-05-08.md](.loom/114-product-spec-engram-tech-tree-2026-05-08.md). The phased build plan lives in [.loom/115-implementation-plan-engram-tech-tree-2026-05-08.md](.loom/115-implementation-plan-engram-tech-tree-2026-05-08.md).
