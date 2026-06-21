# Loom Plan Store

The **plan store** makes a *plan* a first-class, durable entity in the
agent-context MCP server, addressable by a stable `plan_id` and reachable by any
agent — Claude Code, Codex, or a Mills-spawned pod — from any worktree or repo.

## Why it exists

Plans used to live only as git-tracked `.loom/*.md` files. Since `git worktree
add` checks out a **frozen copy** of `.loom/` at the branch's HEAD, a plan
written on `main` (or in worktree A) is invisible to a fresh sub-agent in
worktree B until it is committed *and* merged/rebased. Sub-agents in the
parallel-slice-ship and Mills flows therefore could not reliably find the plan
they were implementing.

The plan store fixes this by putting plans in the same shared global Qdrant that
already backs sessions, tasks, handoffs, worktrees, and memory. A plan is looked
up by `plan_id`; the `.loom/*.md` file becomes a *rendered mirror* for human/MR
review (store-canonical, file-mirrored).

See the design spec and slice plan in `.loom/160-product-spec-loom-plan-store-*`
and `.loom/161-implementation-plan-loom-plan-store-*`.

## Scoping invariant

Plan reads are scoped by `plan_id` / `project` / `namespace` and are **never**
filtered by `agent_id`. The default context-recall path is agent-scoped, which
would hide a plan from exactly the parallel and Mills agents that need it. Writes
are attributed (`created_by`) but not gated by identity — any agent may read a
project's plans.

## Entities

### Plan (`agent_plans_v1`)

A unit of planned work: `id`, `slug`, `title`, `project`, `namespace`, `phase`,
`spec_doc` (canonical markdown body), and provenance (`created_by`,
`source_session_id`). Slice 2 adds the planning contract (`success`, `budget`,
`riskiest_assumption`, `kill_test`/`kill_test_status`, `dependencies`), lifecycle
pointers (`mr_refs`, `pipeline_refs`, `deploy_refs`), cross-system links
(`mirror_path`, `mills_backlog_id`, `gitlab_issue_iid`), and `phase_history`.
These fields align with the Mills `BacklogItem` so the two converge (Slice 7).
The Plan is the source of truth; the `.loom/*.md` mirror is rendered from it.

### Slice (`agent_plan_slices_v1`)

An independently shippable slice of a plan, stored as its **own** record so
parallel `slice-implementer`s update status/decisions without racing on the
plan. `id` is `<plan_id>#<order>`. Fields: `name`, `goal`, `files` (the disjoint
set — basis for Slice 4 claim enforcement), `acceptance_criteria`,
`test_strategy`, `interface_contracts`, `branch_name`, `depends_on`, `phase`,
`assigned_agent_id`, `worktree_id`, `commit_refs`, `mr_ref`, and `decisions`
(blockers/decisions anchored to the slice rather than lost to a context window).
A fresh implementer resolves its work with `agent_plan_slice_get(slice_id)`.

## Lifecycle

A plan advances through a validated phase DAG; `agent_plan_lifecycle_advance`
rejects illegal hops and records each transition in `phase_history`:

```
draft → planned → in_progress → in_review → merging → merged → deployed → done
```

`abandoned` is reachable from any non-terminal phase; `done`/`abandoned` are
terminal. Slice phases: `pending → claimed → implementing → implemented →
in_review → integrated → merged`.

## Tools

| Tool | Purpose |
|------|---------|
| `agent_plan_create` | Create a plan; returns a stable `plan_id`. Optionally seed slices. |
| `agent_plan_get` | Fetch a plan (+ aggregated slices) by `plan_id`. Cross-agent, cross-worktree. |
| `agent_plan_list` | List plans by `project` / `namespace` / `phase`. |
| `agent_plan_update` | Patch spec/title/success and append `mr`/`pipeline`/`deploy` refs. |
| `agent_plan_search` | Semantic search over title+spec (best-effort; falls back to a list). |
| `agent_plan_lifecycle_advance` | Validated phase transition with recorded history. |
| `agent_plan_slice_add` | Append a slice to a plan. |
| `agent_plan_slice_get` | Fetch one slice by `slice_id` (how an implementer finds its scope). |
| `agent_plan_slice_list` | List a plan's slices, ordered. |
| `agent_plan_slice_update` | Update phase/refs; append decisions/commit refs. |
| `agent_plan_slice_claim` | Claim a slice for an agent (conflict unless `force`). |

Embedding for `agent_plan_search` is **best-effort**: a failed embedder never
blocks a write (a deterministic fallback vector keeps the point valid), avoiding
the embed-coupling outage class that previously blanked the task lane.

## Configuration

| Env | Default | Meaning |
|-----|---------|---------|
| `AGENT_CONTEXT_PLANS_COLLECTION` | `agent_plans_v1` | Qdrant collection for plans. |
| `AGENT_CONTEXT_PLAN_SLICES_COLLECTION` | `agent_plan_slices_v1` | Qdrant collection for slices. |

## Verifying cross-process reach (kill-test)

The load-bearing assumption — a plan written by one process/agent is retrievable
byte-identical by a *separate* process using only `plan_id`, with no `agent_id` —
is exercised against the real shared Qdrant:

```bash
RUN_PLAN_STORE_IT=1 QDRANT_URL=... QDRANT_API_KEY=... \
  go test ./pkg/agentcontext/ -run TestPlan_KillTest -count=1 -v
```

This proves the cross-worktree and cross-vendor (Codex) legs, since both are just
"another process pointed at the same Qdrant".
