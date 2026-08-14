# Iteration Plan — Mills HUD action parity for day-2 ops (2026-06-02)

Executes **Slice 1 of `.loom/42-plan-mills-next-round-fixes-2026-05-18.md`**,
unblocked now that the Slice 0 operator kill-test **PASSED (2026-06-02)**.

> **STATUS 2026-06-03 — ALL 5 actions SHIPPED.** 4/5 via
> [loom-core!609](https://gitlab.flexinfer.ai/services/loom-core/-/merge_requests/609)
> (force-escalate, council run, council dryrun, audit-by-MR-iid). The 5th —
> **Slice 1b, the global pause/resume autonomy kill-switch** — shipped in a
> follow-up MR: operator endpoint `POST /api/mills/policy/kill-switch`
> (`handlers_policy.go`) opens a GitOps auto-PR flipping `policy.enabled` in
> `platform/gitops/k3s/mills/configmap-policy.yaml`, proxied by the HUD
> (`internal/hud/domain/mills/mills.go`) and driven by a pause/resume button on
> `OverviewPanel.svelte` (returned MR link shown in a toast).
>
> **Cross-repo creds resolved (kill-test 2026-06-03):** the pipeline GitLab token
> (`loom-mills-gitlab`, user `loom-hive-operator`) gets 404 on `platform/gitops` —
> it can't (and per the `platform/gitops/**` protected-path guardrail mustn't)
> write there. So the kill-switch uses a SEPARATE gitops-scoped project access
> token (`loom-mills-gitops` secret; env `GITOPS_GITLAB_TOKEN` /
> `GITOPS_GITLAB_PROJECT`). Verified end-to-end: a gitops-scoped token creates the
> exact 1-line `enabled:` flip as a branch+commit+MR via the commits API (probe MR
> closed, main untouched). Gotcha: the public GitLab edge Cloudflare-403s the
> default Go/urllib UA (code 1010) — the gitops client sets an explicit User-Agent;
> the in-cluster operator bypasses the edge anyway.
>
> Remaining: **1e live operator kill-test on the deployed HUD** after the gitops
> deployment MR merges + the `loom-mills-gitops` secret is installed out-of-band.

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

**Status**: passed (4/5 buttons shipped via !609 and work end-to-end).

### Second riskiest assumption (kill-switch, surfaced during 1b)

**Load-bearing assumption**: the mills GitLab token can create a
commit+MR in `platform/gitops`, so the kill-switch auto-PR is buildable
as drafted (single operator GitLab client).

**Kill test** (run 2026-06-03, before building): query the live mills
token (`loom-mills-gitlab`) against `platform/gitops`, and a gitops-scoped
token end-to-end.

**Result — FAILED for the pipeline token, PASSED for a dedicated token.**
The pipeline token (`loom-hive-operator`, Maintainer on services/loom-core)
gets HTTP 404 on `platform/gitops` (project id 1) — it is not a member and
cannot write. A freshly minted gitops-scoped project access token (api
scope, Developer) DID create the exact 1-line `enabled:` flip as a
branch+commit+MR (probe MR closed, branch deleted, main untouched).
**Design changed accordingly:** dedicated `loom-mills-gitops` token + a
second operator GitLab client. Had this not been kill-tested first, the
endpoint would have 404'd at runtime in prod.

**Status**: passed 2026-06-03 (dedicated-token path).

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

### 1b — Kill-switch GitOps auto-PR  *(SHIPPED 2026-06-03)*
**Routing corrected during implementation.** The HUD mills domain has NO
GitLab creds (pure proxy), so it can't open the MR itself — the auto-PR
lives in the **operator**, which already has a GitLab client. But that
client is hardwired to `services/loom-core` and the pipeline token can't
write `platform/gitops` (kill-test). Final design:
- New operator endpoint `POST /api/mills/policy/kill-switch` (admin-gated,
  `cmd/loom-mills-operator/handlers_policy.go`). Reads the policy file from
  gitops, flips the `# kill switch`-anchored `enabled:` line, creates a
  branch+commit (`repository/commits` actions[]) + MR, returns the MR URL.
  `action` ∈ pause|resume|toggle; no-op (already in desired state) opens no
  MR. Body carries an optional `reason` recorded in the commit + MR
  (the MR is the durable audit trail).
- New GitLab client methods `GetRawFile` + `CreateCommit` (+ `UserAgent`
  config) in `pkg/mills/clients/gitlab.go`. A SECOND client instance,
  scoped to `platform/gitops` via `GITOPS_GITLAB_TOKEN`/`GITOPS_GITLAB_PROJECT`
  (returns a nil interface when unconfigured → endpoint 503), is wired in
  `main.go` (`buildGitOpsGitLabClient`) + `server.go` (`withKillSwitch`).
- HUD proxies the route (`internal/hud/domain/mills/mills.go`); bearer
  injection is automatic for POSTs.
- Tests: `gitlab_test.go` (GetRawFile/CreateCommit), `handlers_policy_test.go`
  (pause flips only the switch line not nested `enabled:`, toggle direction,
  no-op skips MR, 503 unconfigured, 422 missing marker, 400 bad action, 502
  on commit error).
- **Done**: clicking pause/resume from the HUD opens a real GitOps MR that,
  when merged + reconciled, sets `policy.enabled` accordingly.

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
