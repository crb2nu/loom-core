# Plan — Make the HUD Mills tables useful: clickable rows → detail drawers

**Date**: 2026-06-05
**Author**: claude-code (plan-loom-core)
**Trigger**: "figure out how to actually make this UI useful, it still doesn't let me
click row entries for details, this is still mostly useless" — screenshot of
`hud.flexinfer.ai` → **Mills → Backlog** tab, a dead static table.

---

## TL;DR

The Mills section was built **list-first**; the row→detail drill-down was only
ever finished for **one** of nine tabs (Pipelines). The backend already proxies
per-entity detail endpoints, and a reusable drawer pattern already exists. So
"make it useful" is overwhelmingly a **frontend-wiring gap**, not missing
capability. The fix is to extend the proven Pipelines pattern to Backlog, Eval,
and the rest — and to make a backlog detail genuinely actionable (cost
breakdown + jump to / start its pipeline run).

---

## Riskiest assumption + kill-test

This is mostly an internal UI-wiring task, but it rests on one external claim
about the **operator's** detail responses (the HUD only proxies them), so it
gets a kill-test.

**Load-bearing assumption**: The Mills operator's detail endpoints that the HUD
already routes — `GET /api/mills/backlog/{id}`, `/audit/findings/{id}`,
`/council/runs/{id}`, `/squads/{name}` — return a **richer payload than the
list row** (or at least a stable 200 body), so a detail drawer has something
worth showing. If these 404 or just echo the list row, a drawer adds chrome but
no information.

**Kill test** (≤30 min, against deployed operator):
```bash
# Pick a real backlog id visible in the screenshot, e.g. gl-47-154.
# Hit the HUD proxy directly and compare detail vs list row.
curl -s https://hud.flexinfer.ai/api/mills/backlog | jq '.[] | select(.ID=="gl-47-154")'
curl -s https://hud.flexinfer.ai/api/mills/backlog/gl-47-154 | jq .
```
Pass = detail body returns 200 AND carries fields beyond {ID,Title,State,
Priority,Labels} (e.g. description, source MR/issue, history, linked runs).
Partial = 200 but identical to list row → drawer still useful via **cross-links
+ actions** (cost preview, start pipeline), just not via new fields.
Fail = 404/501 → backlog drawer must render from list data only; file an
operator ticket to enrich `GET /backlog/{id}`.

**Failure mode if wrong**: We wire drawers that open to a near-empty pane,
reproducing "mostly useless" with extra clicks. Mitigation is baked into the
design: even on Partial/Fail, the backlog drawer earns its place through the
already-fetched **cost-estimate breakdown** and the **cross-link to the
pipeline run executing the item** (PipelineRun.BacklogID === item.ID) plus a
**Start pipeline** action — none of which need a new endpoint.

**Status**: **PASSED 2026-06-05** (via source — deployed HUD is behind
Cloudflare Access, returns 302, so verified against the operator handler
instead). `cmd/loom-mills-operator/handlers_backlog.go:111` `handleBacklogGet`
does `writeJSON(w, 200, item)` returning the **full `BacklogItem`**
(`pkg/mills/store/types.go`): far richer than the 5 list columns — includes
`SpecDoc`/`SpecAnchor`, `Slices[]` (name/files/tests/parallel_with),
`Success` criteria, `Budget` (max cost/turns/minutes), `Policy`
(human-review/auto-merge/protected-paths), `Dependencies[]`, `CouncilRunID`
(→ council run cross-link), `GitLabIssueIID` (→ GitLab issue link),
`CreatedBy/At`, `UpdatedAt`. Backlog drawer is firmly justified.

---

## Evidence (what's actually there today)

Row-interactivity audit across the nine Mills panels
(`internal/hud/frontend/src/lib/components/Mills/`):

| Tab (screenshot order) | Panel file | Rows clickable → detail? | Evidence |
|---|---|---|---|
| Overview | `OverviewPanel.svelte` | n/a (cards) | 12 onclick |
| Pipelines | `PipelinesPanel.svelte` | **YES (reference impl)** | rows `role="button"`, `onclick={openRun}`, `<PipelineRunDetail/>` drawer — `PipelinesPanel.svelte:220-267` |
| **Backlog** | `BacklogPanel.svelte` | **NO — dead table** | plain `<tr>` no handler — `BacklogPanel.svelte:100-130` |
| Council | `CouncilPanel.svelte` | partial (debate expander) | 4 onclick, no row drawer |
| **Eval** | `EvalPanel.svelte` | **NO** | 0 onclick (114 lines, list only) |
| Squads | `SquadsPanel.svelte` | partial | 1 onclick |
| Audit | `AuditPanel.svelte` | partial | 2 onclick |
| Cross-Repo | `CrossRepoCard.svelte` | partial | 4 onclick |
| Policy | `PolicyProposalsCard.svelte` | n/a (apply/reject) | — |

**The working reference pattern** (`PipelinesPanel.svelte`):
- Row markup: `class="clickable"`, `role="button"`, `tabindex="0"`,
  `aria-label`, `onclick={() => openRun(r.ID)}`, `onkeydown` (Enter/Space).
- Selection highlight: `selectedID = $derived(millsStore.selectedRunID)` +
  `.selected` CSS (inset accent bar).
- Store drives the drawer: `openRunDetail(id)` sets `selectedRunID`, caches +
  fetches detail, `refreshOpenPipelineDetail()` keeps it live on the 15s poll.
- Drawer component `<PipelineRunDetail />` rendered once at panel bottom.

**The backend already proxies every detail endpoint we need**
(`internal/hud/domain/mills/mills.go:41-101`):
- `GET /api/mills/backlog/{id}` ✓
- `GET /api/mills/council/runs/{id}` ✓ and `/debate` ✓
- `GET /api/mills/pipeline/runs/{id}` ✓ (the one in use)
- `GET /api/mills/audit/findings/{id}` ✓
- `GET /api/mills/squads/{name}` ✓ + `/memory` + `/outcomes`
- `GET /api/mills/cross-repo/runs/{id}` ✓
- Gap: **`/api/mills/eval/scores` is list-only** — no `/{id}` route. Eval
  detail renders from the list row (or needs a small operator addition).
- Actions already proxied: `POST /api/mills/pipeline/runs/{backlog_id}/start`,
  `/{id}/pause|resume|escalate` — so a backlog/run drawer can DO things.

**Two drawer implementations exist** (consistency debt, not a blocker):
- Shared `shared/DetailDrawer.svelte` (slide-in, focus-trap, Esc/backdrop
  close) — used by Fleet `SessionDetail`, Servers `ServerDetail`, Tasks
  `TaskDetail`, Memory, Graph.
- Bespoke `Mills/PipelineRunDetail.svelte` (769 lines) — does NOT use the
  shared drawer. New Mills drawers should wrap `shared/DetailDrawer` to
  converge, and PipelineRunDetail can be migrated opportunistically.

---

## Root cause (one sentence)

Drill-down was treated as per-tab bespoke work, finished only for Pipelines, so
every other Mills table is a read-only wall — the data and endpoints exist, the
interaction was never wired.

---

## Design: one drilldown contract for every Mills table

Establish a single, reusable row→detail interaction so this never has to be
re-solved per tab.

1. **Generic selection state in the store.** Add a small, entity-typed
   selection slot to `millsStore` (mirrors `selectedRunID`):
   `selectedDetail: {kind: 'backlog'|'council'|'audit'|'squad'|'eval', id} | null`,
   plus `openDetail(kind,id)` / `closeDetail()` and a per-kind cache map with
   `{idle|loading|loaded|error}` (copy the `pipelineDetailByRun` shape).
   Keep `selectedRunID` as-is to avoid churn on the working tab.
2. **One reusable detail drawer** `Mills/EntityDetailDrawer.svelte` wrapping
   `shared/DetailDrawer.svelte`, with a `{#if kind === ...}` body per entity.
   Live-refresh the open detail on the existing 15s poll (reuse the
   `refreshOpenPipelineDetail` idea generically).
3. **A tiny `clickableRow` helper / snippet** so each panel's `<tr>` gets
   `role/tabindex/aria/onclick/onkeydown` + `.selected` consistently — kills
   copy-paste drift and guarantees keyboard a11y everywhere.
4. **Cross-links make it actually useful**, not just informative:
   - Backlog detail → list pipeline runs where `BacklogID === item.ID`; click
     one to open `PipelineRunDetail`. If none, show **Start pipeline** button
     (`POST /pipeline/runs/{backlog_id}/start`, admin-gated, confirm dialog).
   - Backlog detail → full cost-estimate breakdown (path_class, sample_size,
     confidence, capped_by_policy) — already fetched in `BacklogPanel`.
   - Council detail → reuse existing `loadDebate()` transcript in the drawer.
   - Audit finding detail → finding body + linked run/MR.

---

## Slices (ship-sized, independently mergeable)

**Slice 0 — Kill-test (30 min, blocking Slice 2 content).**
Run the curl kill-test above against deployed operator. Record actual
`/backlog/{id}` payload in this doc. Decides whether backlog drawer leans on
new fields vs. cross-links/actions.

**Slice 1 — Drilldown primitives (frontend only).**
Add generic `selectedDetail` + `openDetail/closeDetail` + cache to
`mills.svelte.ts`; build `Mills/EntityDetailDrawer.svelte` (wraps shared
DetailDrawer) and the `clickableRow` snippet. No behavior change yet (no panel
imports it). Unit-test the store transitions.

**Slice 2 — Backlog rows clickable (the user's exact complaint).**
Wire `BacklogPanel.svelte` rows to `openDetail('backlog', item.ID)`; render
EntityDetailDrawer with: metadata, cost breakdown, linked pipeline run(s) +
Start-pipeline action. This alone resolves the screenshot grievance.
**Demo gate**: on the deployed HUD, click `gl-47-154` → drawer opens with its
escalation context and a path to its pipeline run.

**Slice 3 — Eval rows clickable.**
Wire `EvalPanel.svelte`. If kill-test showed no `/eval/scores/{id}`, render from
the list row + cross-link to the run that produced the score; otherwise add the
detail fetch. (May need a 1-line operator route — track separately.)

**Slice 4 — Council / Audit / Squads: LEAVE AS-IS (already interactive).**
Reality check after reading the code: these three already implement working
row→detail drill-down via **inline expanders**, not dead tables:
- `CouncilPanel.svelte:188` — row `onclick={toggle}` → debate transcript
  (`loadDebate`).
- `AuditPanel.svelte:260` — finding `onclick={toggle}` → finding detail, plus
  `openRun()` cross-link into `PipelineRunDetail` (`:343`).
- `SquadsPanel.svelte:77` — squad card `onclick={toggle}` → memory + outcomes.
Rebuilding working UI onto a drawer for cosmetic consistency is risky churn with
near-zero user value — explicitly OUT of scope. Convergence onto a single drawer
contract is deferred to a future cleanup if ever justified. **The user's
complaint ("can't click rows for details") is literally true only for Backlog +
Eval — the two tabs with zero click handlers.**

**Slice 5 (optional) — Converge Pipelines + deep-linking.**
Migrate `PipelineRunDetail` to wrap `shared/DetailDrawer`; reflect
`selectedDetail` in the URL/router (`router.svelte.ts`) so a drilldown is
shareable/reload-safe. Pure polish.

---

## Scope / non-goals

- Not redesigning Mills information architecture or the tab set.
- Not adding new operator capabilities; at most one tiny `eval/scores/{id}`
  route if Slice 3 needs it.
- Frontend is `go:embed`'d: every UI change requires `make hud-frontend` +
  committing `internal/hud/frontend/dist` (CI does NOT rebuild it — see memory
  "Mills HUD monitoring").

## Verification

- Per slice: `pnpm build` clean; Svelte a11y lint clean (role/tabindex/keydown
  present on every clickable row); store unit tests for selection/cache.
- Deployed demo gate per slice (click a real row, drawer renders real data).
- Keyboard: Tab to row, Enter opens, Esc closes, focus returns to the row.

## Open questions

1. Does `GET /api/mills/backlog/{id}` enrich beyond the list row? → Slice 0.
2. Eval: add `/eval/scores/{id}` to operator, or render from list row only?
3. Converge on `shared/DetailDrawer` now (migrate PipelineRunDetail) or leave
   the bespoke drawer and only build new ones on the shared base?

## Key files

- `internal/hud/frontend/src/lib/components/Mills/BacklogPanel.svelte` (dead table)
- `internal/hud/frontend/src/lib/components/Mills/PipelinesPanel.svelte` (reference)
- `internal/hud/frontend/src/lib/components/Mills/PipelineRunDetail.svelte` (bespoke drawer)
- `internal/hud/frontend/src/lib/components/shared/DetailDrawer.svelte` (reusable base)
- `internal/hud/frontend/src/lib/stores/mills.svelte.ts` (selection/cache mechanism)
- `internal/hud/domain/mills/mills.go:41-101` (proxied detail + action endpoints)
- `internal/hud/frontend/src/App.svelte:400-467` (lazy tab mounting)
