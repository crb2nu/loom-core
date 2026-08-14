# Product Spec: Engram Tech Tree for Agentic Software Development

> Date: 2026-05-08
> Status: Draft
> Companion docs: [113-research](.loom/113-research-engram-tech-tree-2026-05-08.md), [115-implementation-plan](.loom/115-implementation-plan-engram-tech-tree-2026-05-08.md)

---

## 1. Goal

Give agents working in loom-core a **compact, prerequisite-aware, proof-gated** library of software patterns and algorithms that they can recall, recombine, and extend — improving consistency across sessions and reducing wasted context on re-deriving known solutions.

## 2. Non-Goals

- Replace skills (workflows). Skills orchestrate engrams; they are not engrams.
- Replace agent memory. Engrams are one tier of memory (`category="engram"`), promoted from recipes.
- Build a visual tech-tree UI as part of the MVP. CLI + JSON only.
- Encyclopedic coverage. The library grows from real session evidence.

---

## 3. Concept Model

### 3.1 What an engram is

An engram is a **named, versioned, proof-backed solution to one well-scoped problem**, with explicit links to engrams it depends on.

```
engram://<family>/<slug>           # stable identifier
  problem:        Why this engram exists.
  solution:       Code + prose. ≤ ~250 lines including snippets.
  proof:          file:lines OR runnable command OR canonical URL.
  proof_status:   unverified | verified | stale | failing
  tier:           1 (idiom) | 2 (composite) | 3 (system)
  prerequisites:  [engram://...]
  family:         logical group; same problem in another lang shares family.
  language:       go | python | typescript | rust | shell | polyglot
  scope:          project | workspace | universal
  tags:           [...]
  unlocked_in:    [repo/branch refs where proof has run green]
  created_by:     agent_id
  created_at:     timestamp
  last_verified:  timestamp
```

### 3.2 Tiers

| Tier | Examples | Required proof |
|------|----------|----------------|
| **1 — Idiom** | Error wrapping with `%w`, context propagation, structured logging keys, defensive nil-check ordering, retry-with-jitter formula | File reference (`pkg/.../foo.go:42-58`) |
| **2 — Composite** | Connection pool with healthcheck, debounce/coalesce queue, leader election via lease, idempotency-key write path | Runnable test (`go test ./pkg/x -run Y`) |
| **3 — System** | Saga coordinator, distributed tracing wiring, agent-handoff protocol, multi-region cache invalidation | Runnable test **and** observable artifact (benchmark, trace, metrics dashboard URL) |

A tier is not a difficulty rating — it is a **proof-quality contract**. Higher tiers cost more to verify and are gated more carefully.

### 3.3 Tech-tree semantics

- Engrams form a directed acyclic graph via `prerequisites`.
- Cycles are rejected at write time.
- An engram is **unlocked in a repo/branch** when its proof has been validated against that repo's HEAD within the last *N* days (default 14). Otherwise it is `stale` for that scope.
- An agent should not introduce a Tier-2/Tier-3 engram into a codebase where its prerequisites are not unlocked. The recall API surfaces this contract; enforcement is advisory in the MVP.

### 3.4 Relationship to existing primitives

| Existing | Becomes |
|----------|---------|
| `agent_recipe_add` | `agent_engram_add`; recipes auto-migrate as Tier-1 with empty prerequisites |
| `agent_recipe_recall` | `agent_engram_recall` (additive: optional graph traversal flags) |
| `agent_recipe_list` | `agent_engram_list` |
| Long-term memory item `category="recipe"` | `category="engram"`; old recipe items keep working via category alias |
| Skills | Unchanged. A skill MAY declare a list of engrams it expects to be unlocked. |

Backwards compatibility is non-negotiable: every existing call site must continue to work without code changes.

---

## 4. API Surface (MVP)

All tools live in `mcp-agent-context`, alongside `agent_recipe_*`.

### 4.1 `agent_engram_add`
Inputs (additive over `agent_recipe_add`):
- `prerequisites: string[]` (engram URIs; default `[]`)
- `tier: 1|2|3` (default `1`)
- `family: string` (default = slug)
- `language: string`
- All existing recipe fields.

Validation:
- `proof` required. Tier-2 must contain a `command:` line; Tier-3 must contain `command:` and either `benchmark:` or `dashboard:` line.
- Cycle check across `prerequisites` before persistence.
- Slug uniqueness within (`family`, `language`).

### 4.2 `agent_engram_recall`
Inputs:
- `query: string` (required)
- `depth: int` (0 = match only, 1 = include direct prerequisites, ≥2 = transitive; default `1`)
- `include_locked: bool` (default `true`; when `false`, omits engrams whose prerequisites are not unlocked in the current repo)
- `repo: string` (default = inferred from cwd via `git config`)
- `tier_max: int` (cap returned tier; default unbounded)
- `token_budget: int` (default 4000)

Output: ordered list, lowest tier first, with prerequisite refs resolved inline. JSON shape mirrors `agent_recipe_recall` plus a `prerequisites_resolved` array.

### 4.3 `agent_engram_list`
Same filters as `agent_recipe_list`, plus `tier`, `family`, `proof_status`, `repo`.

### 4.4 `agent_engram_graph`
Inputs:
- `root: string` (engram URI or family)
- `direction: up | down | both` (default `down` = its dependents)
- `max_depth: int` (default `3`)

Output: adjacency list `{nodes:[...], edges:[{from,to}]}` suitable for Mermaid rendering by the caller.

### 4.5 `agent_engram_verify` (post-MVP S3)
Inputs: `engram` URI or `all: true`.
Effects: runs `proof.command` in devbox, updates `proof_status` and `last_verified`. Used by the scheduled CI job.

---

## 5. Storage

Engrams are long-term memory items with:
- `category = "engram"`
- `metadata.engram_tier`, `metadata.engram_prerequisites`, `metadata.engram_family`, `metadata.engram_language`, `metadata.engram_proof_status`, `metadata.engram_unlocked_in`, `metadata.engram_last_verified`.
- Tags: existing `recipe`-style plus `engram`, `tier:1|2|3`, `family:<slug>`.

This reuses the existing memory infrastructure ([svc_recipes.go:50-74](pkg/agentcontext/svc_recipes.go:50)) — no new tables. Migration of recipe → engram is a metadata-only update.

---

## 6. Authoring Workflow (the agent's experience)

When an agent solves a non-trivial problem in a session:

1. The session-end summarizer flags candidates: any committed change accompanied by a passing test that touches a previously absent pattern (heuristic: new package or new public function with ≥1 new test).
2. The agent (or a follow-up subagent) drafts an engram entry with `proof_status=unverified`.
3. The next CI run validates the proof and flips status to `verified`.
4. On subsequent sessions, `agent_engram_recall` surfaces it for similar problems.

Human review is optional but supported via a `pending_review` queue surfaced in HUD (post-MVP).

---

## 7. Acceptance Criteria

The MVP ships when:

- [ ] `agent_engram_add` validates tier/proof contracts and rejects cycles.
- [ ] `agent_engram_recall` returns transitively-resolved prerequisites within `token_budget`.
- [ ] All existing `agent_recipe_*` calls continue to work; existing recipe-tagged memory items appear in `agent_engram_list` as Tier-1 entries with empty prerequisites.
- [ ] Five seed engrams ship with the release, sourced from existing loom-core code (see Section 9).
- [ ] Unit tests cover: cycle detection, depth-limited traversal, tier proof validation, recipe back-compat.
- [ ] HUD `loom catalog list --kind engram` lists engrams with proof_status; no graphical UI required.
- [ ] Documentation: `mcp/skills/agent-recipes/` is renamed/extended to `agent-engrams/`, with a migration note pointing old skill name → new.

---

## 8. Metrics

Track in OTel under `engram.*`:
- `engram.add.count` (by tier)
- `engram.recall.count`, `engram.recall.depth.histogram`, `engram.recall.tokens_returned.histogram`
- `engram.proof.verify.count` and `.duration`, by `result={verified,stale,failing}`
- `engram.unlocked_in.count` (cardinality of repo/branch refs)

Initial success threshold (90 days post-launch): ≥ 25 engrams with `proof_status=verified`, ≥ 200 recalls/week across the fleet, ≥ 70% of recalls return at least one prerequisite (i.e. agents are using the graph, not just searching flat).

---

## 9. Seed Engrams (sourced from existing loom-core code)

These are concrete examples to ship with the release, demonstrating the schema with real proofs:

| Slug | Tier | Proof | Why |
|------|------|-------|-----|
| `engram://go/error-wrap-with-context` | 1 | `pkg/agentcontext/svc.go` (any `fmt.Errorf("%w", ...)` site) | Pervasive Go idiom |
| `engram://go/atomic-file-write` | 2 | [pkg/skills/fileops.go](pkg/skills/fileops.go) + `TestWriteFileAtomic_NoPartialReads` | Already in memory; proven via regression test for openai/codex#11495 |
| `engram://go/role-based-resolver` | 2 | [pkg/aimodels/](pkg/aimodels) + tests; commit `3927046d` | Recently adopted house pattern |
| `engram://go/mcp-tool-traced-handler` | 1 | `traced(tracer, "name", handler)` pattern in `cmd/mcp-agent-context/tools_recipes.go:65` | Repeated across all MCP tool registrations |
| `engram://go/agent-context-recipe-storage` | 2 | `pkg/agentcontext/svc_recipes.go:50-74` | Memory-item-as-typed-record pattern; meta-relevant |

Each seed engram is created by the migration script during S1 rollout.

---

## 10. Out-of-Scope (Deferred)

- Visual tech-tree explorer in HUD.
- Cross-workspace federation of engrams (universal scope is single-workspace for now).
- Engram "purchase cost" / "skill points" — the metaphor is borrowed; the gamification is not.
- LLM-driven engram synthesis (auto-generating engrams from arbitrary code). Authoring is human-or-agent-explicit only.
- Public engram registry. Internal use first; share later if it proves valuable.
