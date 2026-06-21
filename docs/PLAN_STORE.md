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
`source_session_id`). The Plan is the source of truth; the `.loom/*.md` mirror is
rendered from it.

## Tools (Slice 1)

| Tool | Purpose |
|------|---------|
| `agent_plan_create` | Create a plan; returns a stable `plan_id` (`plan-<slug>-<short>`). Optionally seed slices. |
| `agent_plan_get` | Fetch a plan by `plan_id`. Cross-agent, cross-worktree — not agent-scoped. |
| `agent_plan_list` | List plans by `project` / `namespace`. |

## Configuration

| Env | Default | Meaning |
|-----|---------|---------|
| `AGENT_CONTEXT_PLANS_COLLECTION` | `agent_plans_v1` | Qdrant collection for plans. |

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
