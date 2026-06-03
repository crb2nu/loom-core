# Iteration Plan — Mills HUD action parity for day-2 ops (2026-06-02)

Executes **Slice 1 of `.loom/42-plan-mills-next-round-fixes-2026-05-18.md`**,
unblocked now that the Slice 0 operator kill-test **PASSED (2026-06-02)**.

> **STATUS 2026-06-02 — 4 of 5 actions SHIPPED via [loom-core!609](https://gitlab.flexinfer.ai/services/loom-core/-/merge_requests/609)** (force-escalate,
> council run, council dryrun, audit-by-MR-iid). Backend 1a (`store.ListByMRIID` +
> `mr_iid` query param) landed with tests; frontend 1c (`shared/millsActions.ts`) +
> 1d (CouncilPanel, PipelineRunDetail, AuditPanel) landed. **Slice 1b — the global
> pause/resume autonomy kill-switch — is DEFERRED to a follow-up** (spawned task):
> it needs a cross-repo GitOps auto-PR the HUD proxy can't issue (no `platform/gitops`
> creds; operator GitLab client is single-project) and rests on an unverified
> token-scope assumption. Autonomy state is already read-only on the Overview panel.
> Remaining: **1e live operator kill-test on the deployed HUD** after merge.

- **Date**: 2026-06-02
- **Goal**: every Mills day-2 action reachable from the deployed HUD
  without dropping to a terminal — pause/resume autonomy, force-escalate
  a stuck run, replay/dryrun council, audit by MR iid.
- **Predecessor**: `.loom/42` (Slices 2–5 shipped 2026-05-19; Slice 1 was
  the only remaining one, kill-test-gated).

---

## Kill-test result (Slice 0 of plan 42)

Operator interview compressed from the 30-min walkthrough. Operator
selected **all five** day-2 actions as "needed in ~last 14 days AND would
click a HUD button" (pass gate was ≥3 of 5):

1. Pause/resume autonomy ✅
2. Force-escalate stuck run ✅
3. Replay council ✅
4. Council dryrun ✅
5. Audit by MR iid ✅

**Kill-switch routing decision**: GitOps auto-PR (not a write-through
endpoint). The HUD button opens a pre-filled MR against
`platform/gitops/k3s/mills/configmap-policy.yaml` rather than fighting
Flux. Resolves plan 42 Open Question 1.

---

## What grounding changed vs. plan 42 Slice 1

Plan 42 estimated Slice 1 at **3 days** assuming frontend **and** backend
endpoints had to be built. Inspection on 2026-06-02 shows the backend is
mostly already in place (built incrementally during Slices 2–5 and the
autonomy round), so the slice is **smaller and mostly frontend**.

| Action | Backend today | Remaining work |
| --- | --- | --- |
| Replay council | `POST /api/mills/council/run` (`server.go:135`, `requireAdmin`) | frontend button only |
| Council dryrun | `POST /api/mills/council/dryrun` (`server.go:136`) | frontend button only |
| Force-escalate run | `POST /api/mills/pipeline/runs/{id}/escalate` (`server.go:144`) | frontend button only |
| Per-run pause/resume | `.../{id}/pause`, `.../{id}/resume` (`server.go:142-143`) | frontend buttons (optional add) |
| **Audit by MR iid** | ❌ list endpoint lists *active* runs only (`handlers_pipeline.go` `handlePipelineRunsList`); `MRIID` field exists + round-trips (`store/types.go:385`) but no query-by-iid path | **new backend lookup** + frontend search |
| **Global pause autonomy** | `policy.Enabled` is the kill switch (`pkg/mills/budget.go:91`, `council_scheduler.go:106`); no HTTP endpoint | **GitOps auto-PR** helper (HUD side) + frontend button + read `policy_enabled` from status (`handlers_status.go:33`) |

**Admin-token plumbing is already DONE** — `internal/hud/domain/mills/proxy.go`
strips any browser-supplied admin header and injects the operator bearer
token on mutations, behind a HUD admin gate (tests:
`proxy_test.go` `TestProxy_InjectsBearerOnMutations`,
`TestProxy_HUDAdminGateBlocksUnauthorizedMutations`). The browser never
handles a token. **The plan's `MillsAdminActions` "admin-token entry"
requirement is obsolete** — the shared primitive only needs a confirm
dialog + error toast + a POST helper that hits `/api/mills/...` through
the existing proxy.

---

## Riskiest assumption + kill-test

**Load-bearing assumption** (internal-UI slice — low external risk): the
existing HUD→operator mills proxy injects the operator admin bearer on
*every* mutating route the new buttons call (`/council/run`,
`/council/dryrun`, `/pipeline/runs/{id}/escalate`, and the new
`?mr_iid=` GET), so a frontend `fetch('/api/mills/...', {method:'POST'})`
from an authenticated HUD session succeeds end-to-end without per-button
token handling.

**Kill test** (≤15 min, do FIRST before writing any button): from the
deployed HUD's browser devtools console, `fetch('/api/mills/council/dryrun',
{method:'POST', headers:{'content-type':'application/json'},
body:'{"reason":"parity-killtest"}'})` and assert HTTP 200 + a
`council_runs.notes` entry. If it 401s, the proxy mutation-injection has a
gap and the shared primitive must handle auth — re-scope before building 5
buttons on a false assumption.

**Failure mode if wrong**: build 5 buttons that all 401 in the browser;
rework the shared primitive's auth path after the fact.

**Status**: not run (run as step 0 of implementation).

---

## Slices (execution order)

### 1a — Backend: audit-by-MR-iid lookup  *(TDD, ~0.5d)*
- Add `GET /api/mills/pipeline/runs?mr_iid=<n>` handling in
  `handlePipelineRunsList` (parse query param; when present, return the
  matching run(s) instead of the active-state union).
- New DAO `Pipeline.GetByMRIID` / `ListByMRIID` in
  `pkg/mills/store/dao_pipeline.go` (the `mr_iid` column already exists).
- Tests: `cmd/loom-mills-operator/handlers_pipeline*_test.go` (param parse +
  not-found), `pkg/mills/store/...` DAO round-trip.
- **Done when**: query by a known merged run's iid returns that run; bad
  iid → `[]`.

### 1b — Backend (HUD side): kill-switch GitOps auto-PR  *(~0.5–1d)*
- HUD-side action (new method in `internal/hud/domain/mills/`) that opens
  an MR via the GitLab MCP/client flipping `policy.enabled` in
  `platform/gitops/k3s/mills/configmap-policy.yaml` (toggle based on
  current `policy_enabled` from status). Returns the MR URL.
- No operator endpoint (per kill-switch routing decision).
- Tests: GitLab client call mocked; assert correct file/branch/title and
  the enabled-bool flip direction.
- **Done when**: clicking pause from the HUD produces a real GitOps MR
  that, when merged + reconciled, sets `policy.enabled: false`.

### 1c — Frontend: shared `MillsAdminActions.svelte` primitive  *(~1d)*
- New `internal/hud/frontend/src/lib/components/Mills/shared/MillsAdminActions.svelte`:
  confirm dialog for destructive ops + error toast + a `postAdmin(path, body)`
  helper hitting `/api/mills/...` through the proxy. **No token entry.**
- **Done when**: a single import gives any panel a confirm+POST+toast flow.

### 1d — Frontend: wire buttons onto existing panels  *(~1d)*
- `PipelineRunDetail.svelte`: **Force-escalate** (confirm) + per-run
  pause/resume.
- `CouncilPanel.svelte`: **Replay (run)** + **Dryrun** buttons (verify
  Slice 5 only wired the OverviewPanel banner, not CouncilPanel itself).
- `AuditPanel.svelte`: MR-iid search box → calls 1a → links result to its
  originating council run (Loop B attribution).
- `OverviewPanel.svelte`: global **Pause/Resume autonomy** button reading
  `policy_enabled` state, calling 1b (GitOps PR), showing the returned MR
  link in the toast.
- **Done when**: all 5 actions reachable from the HUD; each shows success/
  error feedback.

### 1e — Verification (the plan-42 Slice 1 kill criteria)  *(~0.5d)*
- On the **deployed** cluster (not localhost), operator pauses autonomy,
  replays council, force-escalates a run, queries by MR iid, triggers a
  council run — all from the HUD, no terminal.
- Confirm each mutation writes an audit-log entry (`pkg/mills/audit/`;
  verify it covers HUD-initiated actions — plan 42 Open Question 3).
- Evidence memo in `.loom/local/handoffs/`.

---

## Sequencing

```
  [step 0: proxy-injection kill-test in deployed HUD devtools]
        │ pass
        ▼
  1a backend audit-by-iid (TDD) ─┐
  1b backend kill-switch PR ──────┤ (independent, parallelizable)
                                  ▼
  1c shared MillsAdminActions ──► 1d wire 5 buttons ──► 1e live verify
```

1a/1b are independent backend units; 1c is the frontend foundation 1d
depends on; 1e is the live operator gate that closes plan 42 Slice 1.

---

## Non-goals (inherited from plan 42)
- No new Mills capability surface (no new gate types / ensemble modes /
  policy fields).
- No mobile companion changes for Mills.
- No write-through kill-switch endpoint (GitOps-PR decided).
- No visual overhaul beyond the small per-panel button diff.

## Open questions
1. **(1e)** Does `pkg/mills/audit/` already record HUD-initiated mutations,
   or is a `hud-initiated` audit-source tag needed? (plan 42 OQ3)
2. **(1d)** Did Slice 5 add run/dryrun buttons to `CouncilPanel.svelte`
   itself, or only the OverviewPanel banner? Verify before duplicating.

## Sources
- Parent plan + slice status: `.loom/42-plan-mills-next-round-fixes-2026-05-18.md`
- Day-2 action list: `mcp/skills/mills-ops/SKILL.md:67-97`
- Existing routes: `cmd/loom-mills-operator/server.go:132-144`
- Admin-token proxy: `internal/hud/domain/mills/proxy.go:16-68`, `proxy_test.go`
- Audit-by-iid gap: `cmd/loom-mills-operator/handlers_pipeline.go` `handlePipelineRunsList`; `pkg/mills/store/types.go:385`
- Kill-switch = policy.Enabled: `pkg/mills/budget.go:91`, `pkg/mills/council_scheduler.go:106`, `cmd/loom-mills-operator/handlers_status.go:33`
- GitOps policy ConfigMap: `platform/gitops/k3s/mills/configmap-policy.yaml`
