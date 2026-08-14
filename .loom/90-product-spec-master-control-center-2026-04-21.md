# Product Spec: HUD + Weaver Master Control Center

**Date**: 2026-04-21
**Research**: `.loom/89-research-master-control-center-2026-04-21.md`
**Implementation Plan**: `.loom/91-implementation-plan-master-control-center-2026-04-21.md`
**Complements**: `.loom/87-product-spec-session-spawning-weaver-2026-04-19.md` (SESS / AUTH / WVR / OBS slices — substrate for this UI work)

## Goal

Turn the HUD and Weaver into a single coherent **operator control plane** — not a dashboard — that lets one person observe, command, and govern the full agent/session/spawn/weaver/K3s stack from one browser tab (and a scoped mobile peer). The substrate plumbing shipping via spec 87 (session status, cluster auth, weaver×spawn bridge, parent-session propagation) is a prerequisite; this spec is the IA, panel, and UX system that makes that substrate usable.

Success looks like: the HUD passes the *three-terminals test* — an operator on-call doesn't need a terminal, `kubectl`, or `loom auth …` to answer the three questions "is the cluster healthy?", "who/what is running right now?", and "can I safely spawn the next agent?"

## Non-Goals

- Full RBAC / multi-user role management (deferred; scope-gate on admin token only).
- Mobile authoring (no domain editor, no auth rotation, no workflow definition on mobile).
- Non-K3s backend (Harvester infra panel, host-local spawn view). K3s is the only runtime surface.
- Replacing existing Traces / Timeline / Memory / Knowledge panels. Those stay; this spec re-homes them under a new IA.
- New telemetry events beyond OBS-* from spec 87.
- Vault / SPIFFE / HashiCorp integration.
- Offline HUD; always live-connected.

## Architecture at a glance

```
┌───────────────────────────────────────────────────────────────────┐
│  Mission Control (home)                                           │
│  ┌──────────────┬──────────────┬──────────────┬──────────────┐    │
│  │ Session hero │ Weaver lanes │ Cluster pulse│ Attention rail│   │
│  └──────────────┴──────────────┴──────────────┴──────────────┘    │
└───────────────────────────────────────────────────────────────────┘
      │              │                │                │
      ▼              ▼                ▼                ▼
 Sessions view  Weaver Console   Cluster view      Auth view
 (session → sp.)(domains+queues) (nodes/pods/k3s)  (vendors/rotate)
      │              │                │                │
      ▼              ▼                ▼                ▼
 Spawn detail   Domain editor    Pod detail        Rotate flow
 (trace/turn)   (YAML/form)      (node/disk/logs)  (CLI-via-RPC)

 Command Palette (⌘K) ── Go (nav)  |  Do (command) ─ governed by autonomy dial
 Freshness Meter (shared primitive — every data source annotated sse|poll|stale)
 Attention Lane (shared — navigable; every alert maps to an action)
```

## Information Architecture changes

| Old | New | Rationale |
|-----|-----|-----------|
| `Overview` | **Mission Control** (same slot `,o` / `1`) | Rename + rebuild as session-centric hero. See §Mission Control below. |
| `Fleet` (agents + sessions + spawns + tasks + claims + worktrees as one table) | **Sessions** (session is the primary entity) + existing sub-tables move under session detail | One unit of work = one session. Surfaces parent-session correlation (spec 87 SESS-005) as a first-class visual. |
| `Weaver` (read-only) | **Weaver Console** (lanes, domain CRUD, dispatch flight, audit log) | Bi-directional, command-oriented. Uses WVR-001..006. |
| (new) | **Cluster** | Composes node inventory, pod inventory, daemon session status, Flux health. |
| (new) | **Auth** | Composes `cluster-agent-api-keys`, `cluster-agent-auth`, refresher health, per-vendor status. Surfaces AUTH-001..010. |
| `Catalog`, `Servers` | unchanged, refocused to MCP registry only | Avoid conflation with Cluster concerns. |
| `Presence`, `Traces`, `Timeline`, `Memory`, `Knowledge`, `Reasoning`, `Context Health`, `Sandbox`, `Dispatch`, `Workflows`, `Stream` | unchanged, re-homed under groups in router | Group by purpose in sidebar; IA only. |

Router grouping (left rail):

1. **Mission** — Mission Control
2. **Sessions** — Sessions, Presence, Traces
3. **Spawn** — Spawn, Spawn Detail, Sandbox
4. **Weaver** — Weaver Console, Weaver History
5. **Cluster** — Cluster, Auth, Catalog, Servers
6. **Knowledge** — Memory, Knowledge, Context Health, Reasoning
7. **Ops** — Workflows, Dispatch, Stream, Timeline
8. **Settings** — Keyboard, Theme, About

## Changes

### P0 — Mission Control (MC-*)

**MC-001: Rename `Overview` → `Mission` and rebuild as a 4-tile hero**

Tiles (top row):

1. **Session hero** — the "primary" session (most recent active proxy session with parent_session_id nullish) rendered as a big card: session id, epoch, age, attached spawns (with status dots), attached weaver queries, link to drill-down. Empty state: "No active sessions — start one".
2. **Weaver lanes** — horizontally laid per-backend lane strip (`flexinfer | claude-code | codex | gemini`) with recent-query sparkline + in-flight count + p50 latency. Click → Weaver Console with that backend pre-filtered.
3. **Cluster pulse** — ring of 5 pills: daemon epoch (session status), node pressure, auth (green if all vendors present+fresh), Flux sync, mcp-auth-refresher last-success. Click → Cluster view.
4. **Attention rail** — deduped, navigable alerts. Every alert has an intent action: "Approve gate", "Drain daemon", "Rotate OpenAI key". Alerts without an intent action are hidden (no purely informational rail).

**MC-002: Shared freshness footer**

Each tile footer shows `FreshnessMeter` badge (§UX-003 below): `SSE • 0s ago` or `POLL • 14s` or `STALE • 42s`. Stale state shades the tile subtly.

**MC-003: Keyboard model**

`,` (backtick) keeps taking you to Mission. `1`..`8` binds to the new router groups. Within Mission, `j/k` navigates attention rail; `Enter` fires the intent action (same Enter-to-confirm as palette Do tab).

### P0 — Sessions as primary entity (SES-*)

**SES-001: `Sessions` panel**

Replaces the session rows that currently live inside `FleetPanel`. Uses the same underlying store; IA change is the visible one.

Columns: `ID` (short), `Namespace`, `Agent`, `Root/Parent badge` (tree indent already landed `.loom/84` Slice B), `Proxy state` (active/draining/idle from SESS-001), `Spawns attached` (count + agent-type dots), `Weaver queries attached` (count), `Age`, `Cost`, `Status`.

Filters: by agent type, by namespace, by `parent_session_id` presence, by draining state.

Default sort: active first, then parent sessions before children.

**SES-002: Session detail drawer — 4-tab pivot**

When a row is selected, side drawer opens with tabs:

1. **Graph** — tree rendering of `root → parent → current → children` using `parent_session_id` correlation. Each node clickable.
2. **Spawns** — list of spawns with `parent_session_id` matching (uses `LOOM_PARENT_SESSION_ID` propagation from SESS-005). Each row deep-links to Spawn Detail.
3. **Weaver** — list of weaver queries tagged with this session. Uses `weaver_query_id` cross-reference (WVR-004).
4. **Trace** — existing trace drawer content, re-homed.

**SES-003: Session kill/drain actions**

Drawer action row: `End Session`, `Drain daemon` (admin-scoped), `Copy session ID`. End Session calls `loom/session/close`. Drain calls the spec-87 SESS-002 hook.

**SES-004: `parent_session_id` everywhere there's a session reference**

Every panel that shows a session id gains a small "parent" chevron if `parent_session_id != null`. Clicking opens the parent's detail drawer.

### P0 — Weaver Console (WVC-*)

**WVC-001: Weaver Console replaces `WeaverPanel`**

Layout: three-column:
- **Left** — domain list. Each domain row: name, `Backend` badge (flexinfer/claude/codex/gemini), `RequiresSpawn` lock icon, autonomy dial, status pill (healthy/errors/disabled). `New Domain` button at top.
- **Middle** — in-flight queries and history. Two tabs: `Live` (SSE-driven queue by lane) and `History` (existing history list). Each row: query id, domain, backend, status, duration, cost, tokens.
- **Right** — selected query or domain detail.

**WVC-002: Domain detail pane**

Tabs:
1. **Config** — form fields: name, description, tools (chips), system prompt (multiline), model, `Backend`, `RequiresSpawn`, `SpawnOverrides` (timeout/cost/turns/project/use_sdk_driver). Form commits via `PUT /api/weaver/domains/{name}` → daemon RPC → YAML write + audit entry.
2. **YAML** — raw-YAML editor (Monaco) for the same domain; syncs bidirectionally with the form. Commits via the same endpoint.
3. **Test** — fire a test query: prompt textarea + `Dispatch` button. Shows dispatch result inline. Forced to respect `RequiresSpawn` admin-scope.
4. **Audit** — list of `domain.changed` events (edited-by, when, diff).

**WVC-003: Dispatch lane view**

Middle column `Live` tab renders queries grouped by backend lane. Each lane:
- Header: backend + in-flight count + p50 latency + `Pause lane` toggle (admin-scoped).
- Rows: in-flight queries with elapsed timer, live token counter (SSE), per-row `Cancel` button.
- Empty-lane state: "No queries in flight for `claude-code`".

**WVC-004: Query detail pivot**

Right column when a query is selected:
1. **Trace** — weaver decision → (optional) spawn pod → session entries → tool calls.
2. **Pod** — if `Backend != flexinfer`, show the spawn pod detail (reuses `SpawnDetailPanel` content).
3. **Cost** — token + $ chart.
4. **Raw** — JSON event log.

**WVC-005: Autonomy dial per domain**

Three positions: **Manual** (every dispatch requires confirmation), **Guarded** (dispatches allowed but cost > `MaxCostUSD` prompts), **Auto** (full autonomy within `SpawnOverrides` budget). Default: **Manual** for `Backend != flexinfer`, **Auto** for flexinfer. Changes emit audit entry.

**WVC-006: Pause lane / pause domain**

Admin-scoped toggle. Pausing a domain makes the router reject new dispatches with a structured error + a visible lock icon. Pausing a lane does the same for all domains with that `Backend`. Unpause emits audit entry.

### P0 — Cluster panel (CLU-*)

**CLU-001: `Cluster` panel**

Three rows:

1. **Nodes** — table from k8s API: name, role, disk %, cpu %, mem %, ready, taints. Visualize disk pressure as a bar tinted per threshold (see memory "k3s DiskPressure runbook"). Click → node detail drawer.
2. **Pods** — tabbed by kind: `Spawn`, `Daemon` (loomd), `Refresher`, `MCP`, `Other`. Each row: name, node, status, age, restart count. Click → pod detail drawer (logs + describe + delete).
3. **Daemon** — session status card from `loom/session/status` (SESS-001): epoch, active sessions, draining flag, oldest in-flight age. `Drain` / `Undrain` buttons (admin-scoped, require confirm).

**CLU-002: Pod detail drawer**

Tabs:
1. **Overview** — describe-like output (status, labels, annotations).
2. **Logs** — live SSE logs (reuses `StreamPanel` log channel if possible).
3. **Events** — recent k8s events for this pod.
4. **Actions** — `Delete pod`, `Cordon/Uncordon node`, `Kill session` (if pod is a spawn with `LOOM_PARENT_SESSION_ID`).

**CLU-003: Flux sync tile**

One card below the daemon session card: Flux Kustomizations health (`flux get kustomizations` JSON via a new `/api/cluster/flux` endpoint that shells out to `flux` CLI on the daemon host — not via `kubectl`). Shows last-reconciled, ready status per kustomization. Not an edit surface — click-through to dashboard or docs.

**CLU-004: Cluster read-only by default**

Every mutating action (`Drain`, `Delete pod`, `Cordon`, `Undrain`) requires admin-token scope and a confirmation modal with the exact command it will run. Non-admin operators see the view read-only (no fail-hidden, just disabled buttons with tooltip explaining).

### P0 — Auth panel (AUT-*)

**AUT-001: `Auth` panel**

One table per vendor: rows `Anthropic`, `OpenAI`, `Google`. Columns:
- **Mode** — `API key`, `OAuth`, `Service account` (whichever is the cluster mode).
- **Present** — green/red pill.
- **Source** — `cluster-agent-api-keys` or `cluster-agent-auth` key name.
- **Expires** — rendered countdown for OAuth; `—` for API keys / SA.
- **Last refreshed** — refresher-CronJob success timestamp.
- **Last error** — hover to see full.
- **Actions** — `Rotate`, `Set key`, `Login (OAuth)`, `Test` (fires a minimal test spawn to verify).

**AUT-002: Rotate flow**

Click `Rotate` or `Set key` opens a modal:

- For `Set key`: input field for the key (password-masked), `Apply` submits to `POST /api/auth/cluster-set-key` → daemon RPC → SOPS edit + gitops commit + Flux reconcile. Progress indicator covers all 3 steps.
- For `Login (OAuth)`: opens vendor device-code flow in a new tab with the daemon-hosted callback. Modal polls the daemon for completion. On success, shows "Pushed to gitops; Flux reconciling" with live status.
- Service account: `Upload file` input, submits raw JSON to the same `cluster-set-key` endpoint.

**AUT-003: Refresher health card**

Above the vendor table: status of `mcp-auth-refresher` CronJob (AUTH-005). Last-run, next-run, success rate, `loom_auth_refresh_*` metrics spark. `Run now` button (admin-scoped).

**AUT-004: Spawn pre-flight banner**

New spawn form (`SpawnPanel.svelte`) adds a pre-flight badge showing the resolved `AuthMode` for the selected agent type **before submission**. Red state ("cluster auth missing — visit Auth panel") blocks the submit button. Green state ("cluster OAuth, expires in 4h") shows the countdown.

**AUT-005: Expiry warning rail**

OAuth tokens within 30 min of expiry emit an Attention-rail alert on Mission Control with intent action `Rotate` (opens the rotate flow directly).

### P1 — Command Palette evolution (CMP-*)

**CMP-001: Two-tab palette (`Go` / `Do`)**

Cmd+K opens the palette. Tab toggle at top: `Go` (instant nav — current behavior) and `Do` (command surface).

**CMP-002: `Do` commands**

Initial command set:

| Command | Arguments | Endpoint |
|---------|-----------|----------|
| `Spawn agent` | agent type, project, task | `POST /api/agent/spawn` |
| `Drain daemon` | grace (default 5s) | Admin RPC via `/api/daemon/drain` |
| `Undrain daemon` | — | `/api/daemon/undrain` |
| `Rotate auth` | vendor | open rotate flow modal |
| `Run weaver query` | domain, prompt | `POST /api/weaver/query` (WVR-005 scope-gated) |
| `Pause domain` | name | Weaver admin RPC |
| `End session` | id | `loom/session/close` |
| `Approve workflow gate` | workflow id | `agent_workflow_approve` MCP bridge |
| `Open worktree` | repo, branch | `agent_worktree_allocate` MCP bridge |
| `Set autonomy` | scope (session/domain/spawn), level | see WVC-005 |

**CMP-003: Confirm-before-side-effect**

`Do` commands with blast radius > self display a preview: "This will: drain daemon, reject new requests, wait 5s." Enter confirms; Escape cancels. Confirm-gate governed by the autonomy dial of the current session (MC/WVC).

**CMP-004: Palette is keyboard-complete**

Every mutating UI action in every panel has a palette equivalent. Trust: an operator can run the stack with the keyboard alone.

### P1 — Visual + interaction system (UX-*)

**UX-001: Design tokens**

Introduce `design/tokens.css` (CSS custom properties) that all panels consume:
- Color: palette per agent type (claude=indigo, codex=emerald, gemini=amber, flexinfer=slate) with verified AA contrast pairs.
- Density: 3 tiers (`dense` for Sessions/Traces, `standard` for Weaver/Cluster, `ambient` for Mission).
- Radii: `sm 4px`, `md 8px`, `lg 12px`, `pill 999px`.
- Motion: `duration-fast 120ms`, `duration-std 200ms`, `duration-slow 320ms`. `ease-standard`, `ease-emphasized`.
- Shadow: `elev-0..3`.

**UX-002: Standard states primitive**

`lib/widgets/StandardStates.svelte` — wraps panel content with `empty`, `error`, `loading` variants. Replaces ad-hoc skeletons. Every panel uses it.

**UX-003: `FreshnessMeter` primitive**

`lib/widgets/FreshnessMeter.svelte`. Props: `{ lastUpdated, source: 'sse'|'poll'|'stale', warnAfterSeconds }`. Renders pill + dot + tooltip. Every data source in every panel annotated.

**UX-004: `AttentionRail` primitive**

Promoted from Overview + Lifecycle. `{ items: Alert[] }` where `Alert = { id, severity, message, intentAction: { label, command } }`. Items without `intentAction` are dropped.

**UX-005: `AutonomyDial` primitive**

Three-position segmented control: `Manual`, `Guarded`, `Auto`. Same widget used in Mission (session scope), Weaver (domain scope), Spawn form (per-spawn override).

**UX-006: `KeyboardHint` primitive + palette-discoverability**

Every clickable-with-keyboard element shows a small hint on hover. Help overlay (`?` key) shows a full keyboard map generated from the registered shortcuts. The palette registers shortcuts; panels inherit.

**UX-007: Accessibility baseline**

- All colors meet AA contrast in both themes.
- All interactive elements focus-visible.
- All modals and drawers manage focus trap + Esc-close.
- All SSE-driven updates respect `prefers-reduced-motion` (no flashes; subtle tinting only).
- Screen-reader announcements for attention-rail alerts.

**UX-008: Motion grammar**

- State transitions: fade + translate-y(-2px) over `duration-std`.
- SSE flash: tint `accent/10` for 400ms then fade.
- Empty → populated: skeleton dissolves; no pop-in.
- Modal enter: scale(0.97) + fade over `duration-fast`.

### P1 — Store consolidation + SSE-first (SSE-*)

**SSE-001: Weaver store**

New `weaver.svelte.ts`. Consumes `agent.weaver.query.*` SSE events (added by OBS-002 + a new `weaver.query.*` envelope) for the Live lane. Falls back to polling at 30s when SSE disconnected.

**SSE-002: Cluster store**

New `cluster.svelte.ts`. Composes `/api/cluster/nodes`, `/api/cluster/pods`, `loom/session/status`, `/api/cluster/flux`. Dedupes polls (one store per HUD instance, not per panel).

**SSE-003: Auth store**

New `auth.svelte.ts`. Polls `/api/auth/status` at 30s. Subscribes to `auth.refresh.*` SSE events.

**SSE-004: Mission store**

New `mission.svelte.ts`. Aggregates the other stores into the MC tile data model. Does not own data; references upstream stores.

**SSE-005: Polling interval convention**

Standardize all polling to one of three intervals: `5s` (live-ish), `15s` (ambient), `60s` (background). Document in `internal/hud/frontend/src/lib/stores/README.md` (or equivalent comment).

### P1 — Mobile peer scope (MOB-*)

**MOB-001: Mobile home = shrunk Mission**

iOS + future Android companion show the 4 MC tiles stacked. Each tile taps through to a scoped detail.

**MOB-002: Mobile approvals**

Workflow gates requiring approval (`RequiresSpawn` dispatches, autonomy-Manual dispatches) push a notification to the companion. Tap → approve/reject from notification + view context.

**MOB-003: Mobile kill switches**

One-tap on Spawn tile → list of running spawns → swipe-left to kill. Admin-scoped.

**MOB-004: Mobile cluster tile**

Read-only status: daemon epoch, node pressure dot, auth presence dots. No mutation.

**MOB-005: No authoring on mobile**

Spec-enforced: no weaver domain editor, no auth rotation, no workflow definition. Mobile surfaces only approvals, kills, and at-a-glance status.

### P2 — REST surface additions (API-*)

**API-001: `/api/weaver/domains/{name}`**

`GET` — domain config. `PUT` — update (commits YAML + audit). `DELETE` — remove (admin + confirmation).

**API-002: `/api/weaver/query`**

`POST` — fire a query. Body: `{ domain, prompt, autonomy_override? }`. Respects `RequiresSpawn` admin scope (WVR-005).

**API-003: `/api/weaver/domains/{name}/pause`**

`POST` — pause. `DELETE` — unpause. Admin.

**API-004: `/api/cluster/nodes`, `/api/cluster/pods`, `/api/cluster/flux`**

`GET` — read-only. `POST /api/cluster/pods/{ns}/{name}/delete` — admin.

**API-005: `/api/daemon/drain`, `/api/daemon/undrain`**

Admin.

**API-006: `/api/auth/status`, `/api/auth/cluster-set-key`, `/api/auth/cluster-login`**

Maps to spec 87 AUTH-003/004/009 endpoints; CLI and HUD share them.

**API-007: `/api/audit`**

`GET` — paged audit log. Populates the Audit tabs in Weaver domain detail + elsewhere.

**API-008: SSE envelope additions**

- `weaver.query.accepted|started|completed|failed`
- `weaver.domain.changed|paused|unpaused`
- `auth.refresh.started|completed|failed`
- `cluster.node.pressure|recovered`
- `daemon.draining|undrained`

## Success Criteria

1. An on-call operator answers "is the cluster healthy?" from Mission Control in under 5 seconds without opening a terminal.
2. An operator spawns a Claude agent, watches its dispatch via Weaver Console, drills into Session detail, and kills it — entirely from the HUD.
3. An operator rotates the OpenAI API key via Auth panel; the change lands in gitops, Flux reconciles, next Codex spawn uses the new key. Timeline < 60s.
4. An operator pauses the `claude-code` Weaver lane; new dispatches to any domain with `Backend: claude-code` are rejected with the structured error; unpause restores.
5. An operator edits the `cluster-ops` Weaver domain's system prompt via the Config form, sees the change in the YAML tab, commits, and the next dispatch uses the new prompt. An audit entry is visible.
6. The `FreshnessMeter` in every panel shows `SSE • 0s` during normal operation and degrades to `POLL` or `STALE` legibly when SSE drops.
7. Command palette `Do` tab exposes every mutating action; keyboard-only operation across the entire HUD is possible.
8. A mobile operator receives an approval push, approves, and the corresponding spawn starts — without touching the laptop.
9. Accessibility baseline: axe-core zero serious violations across Mission, Sessions, Weaver Console, Cluster, Auth.
10. All existing spec 87 tests still pass. Panel-level tests (Playwright or Vitest-component) for Mission, Sessions drawer, Weaver Console domain edit, Cluster drain, Auth rotate all green.

## Acceptance

- **Backward compatible API.** Existing `/api/weaver/*` endpoints keep their GET shape; new endpoints are additive. Existing SSE event names unchanged; new envelopes are purely additive.
- **No new required dependencies.** Monaco is already present (used elsewhere) or replaceable with a lightweight textarea fallback.
- **No new vendor integrations.** Flux CLI + kubectl already on the daemon host; auth panel shells via the same mechanisms spec 87 introduces.
- **RBAC gate is scope-based.** All mutating endpoints require `ScopeAdmin` (or `ScopeAgentSpawn` for weaver dispatch per WVR-005). HUD reflects in disabled buttons + locked icons; the server is authoritative.

## Decisions (carried from research)

| # | Decision | Rationale |
|---|----------|-----------|
| D1 | **Rename Overview → Mission** (not a new panel) | One "home" surface; preserve keyboard slot. |
| D2 | **Session is the primary entity**, not agent | Session is the only layer that can correlate proxy + spawn + weaver + agent-context. |
| D3 | **Weaver Console is bi-directional** from day one — CRUD + YAML + test + audit | A control plane must command, not merely observe. |
| D4 | **Cluster is its own top-level panel**, not merged with Catalog | Different concern (runtime) from MCP registry. |
| D5 | **Auth is its own top-level panel** | Trust requires visibility; hiding this behind settings would obscure the AUTH-* work. |
| D6 | **Autonomy dial is three-tier** (Manual / Guarded / Auto) | Simple mental model; maps cleanly to `RequiresSpawn` + budget + unchecked. |
| D7 | **Command palette has `Go` / `Do` tabs** | Keeps nav instant; gates side-effects behind an Enter-confirm with preview. |
| D8 | **Mobile is a scoped peer**, not a port | Approvals + kills + at-a-glance only; authoring belongs to desktop. |
| D9 | **`FreshnessMeter` is shared primitive** | State coherence is the #1 UX debt; visible trust model. |
| D10 | **RBAC is admin-scope gate for now**, not full roles | Unlock control-plane ergonomics without the role-management tar pit. |
| D11 | **Traces / Timeline / Memory / Knowledge panels are re-homed, not rewritten** | Avoid scope creep; concentrate build on the new surfaces. |
| D12 | **Polling standardized to 5s / 15s / 60s** with SSE-first | Ends `.loom/84` P1 #7 recurrence pattern. |

## Dependencies

- **Hard prerequisites** (from spec 87):
  - SESS-001 (`loom/session/status`) — Cluster panel daemon tile, Mission pulse.
  - SESS-003 (session metrics) — Mission freshness + cluster pulse.
  - SESS-005 (`parent_session_id` propagation) — Session graph tab.
  - AUTH-002, AUTH-003, AUTH-006, AUTH-008 (cluster secrets + CLI + AuthMode) — Auth panel.
  - AUTH-005 (mcp-auth-refresher) — refresher health card.
  - WVR-001, WVR-002, WVR-003, WVR-005, WVR-006 — Weaver Console.
  - OBS-001, OBS-002, OBS-003 — store subscriptions.
- **Soft prerequisites**:
  - `.loom/84` Wave 1 Slice A (spawn↔session bridge) — shipped.
  - `.loom/84` Wave 2 Slice D (Presence loading states) — shipped/superseded by UX-002.

## Out of Scope (explicit)

- Multi-user role management / RBAC beyond admin-token scope.
- Theme customization (stays on existing dark/light toggle).
- Full mobile authoring parity.
- Harvester cluster panel (K3s only).
- Browser-extension integration.
- Per-user preference persistence beyond localStorage (no server-side profile).
- Real-time collaboration (shared cursors, etc.).
- Scheduled commands / cron-from-HUD (use existing `scheduled-tasks` skill).
- Full Prometheus dashboard embed (stay with existing KPI surfacing).
- Log search UI beyond pod-log tail (use Loki MCP directly).
