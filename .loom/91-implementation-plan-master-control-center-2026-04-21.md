# Implementation Plan: HUD + Weaver Master Control Center

**Date**: 2026-04-21
**Research**: `.loom/89-research-master-control-center-2026-04-21.md`
**Product Spec**: `.loom/90-product-spec-master-control-center-2026-04-21.md`
**Substrate**: `.loom/87-product-spec-session-spawning-weaver-2026-04-19.md` + `.loom/88-implementation-plan-session-spawning-weaver-2026-04-19.md` (SESS / AUTH / WVR / OBS must ship first or alongside)

## Execution overview

This is a **program with 7 tracks** rather than a single branch of slices. Tracks are sized to ship behind a feature flag (`LOOM_HUD_CONTROL_CENTER=on`) and integrate one at a time into `main`. Default flag is **off** until Track F ships.

```
Substrate (spec 87 Slice 1–5) ──► must be green before Track B/C/D start
       │
       ▼
Track A: UX system + store consolidation (UX-*, SSE-*)
Track B: Mission Control (MC-*)                     ──┐
Track C: Sessions as primary entity (SES-*)           ├─ parallel once A is shipped
Track D: Weaver Console (WVC-*, API-001..003)         │
Track E: Cluster panel (CLU-*, API-004..005)          │
Track F: Auth panel + rotate flow (AUT-*, API-006)    ┘
Track G: Command palette evolution (CMP-*) + mobile peer (MOB-*)
```

**Rollout gate**: once all 7 tracks are behind `main`, flip the feature flag default to `on` and ship a minor version bump. Rollback = flip flag off + reload.

**Estimate (conservative)**: A 2.5d, B 2d, C 2.5d, D 4d, E 2.5d, F 2.5d, G 2d. Total ~18d of focused frontend + daemon work. Can be parallelized across two engineers after Track A ships.

---

## Track A — UX system + store consolidation (foundational)

**Spec refs**: UX-001..008, SSE-001..005

**Why first**: every other track consumes `FreshnessMeter`, `StandardStates`, `AutonomyDial`, the token file, and the polling convention. Ship these empty but working; retrofit existing panels in-place.

### A1 — Design tokens + typography

**Changes**
- `internal/hud/frontend/src/lib/design/tokens.css` (new). Emit CSS custom properties: color (per-agent palette + semantic roles), spacing scale (4/8/12/16/24/32/48), radii, motion durations + eases, shadow elevations.
- Import in `internal/hud/frontend/src/app.css` (existing root stylesheet — find via `grep -r "@import" internal/hud/frontend/src`).
- `lib/design/palette.ts` — agent-type → color map; consumed by badges and lane strips.

**Tests**
- Snapshot: token values enumerable from JS (CSS var readback via `getComputedStyle`).
- axe-core baseline across Mission, Sessions, Weaver, Cluster, Auth panels (empty states).

**Verify**
```bash
cd internal/hud/frontend && pnpm typecheck && pnpm build
pnpm test:a11y   # new script wrapping axe-core in Playwright
```

### A2 — Primitives

**Changes**
- `lib/widgets/FreshnessMeter.svelte` — `{ lastUpdated, source, warnAfterSeconds }`.
- `lib/widgets/StandardStates.svelte` — `{ kind: 'empty'|'loading'|'error', message?, action? }`. Replaces ad-hoc skeletons.
- `lib/widgets/AttentionRail.svelte` — `{ items: Alert[] }` with navigable intent actions.
- `lib/widgets/AutonomyDial.svelte` — segmented control. Emits `change`.
- `lib/widgets/KeyboardHint.svelte` — small pill for keybind discoverability.
- Storybook-style demo page at `/__dev/widgets` (dev-only route guarded by `import.meta.env.DEV`).

**Tests**
- Component-level Vitest tests for each primitive: render states, keyboard interaction, ARIA roles.

**Verify**
```bash
cd internal/hud/frontend && pnpm test -- lib/widgets
```

### A3 — Store consolidation

**Changes**
- `lib/stores/weaver.svelte.ts` (new) — consumes `/api/weaver/*` + SSE `weaver.*` events. Polling fallback at 30s. Consumed by Weaver Console (Track D) and Mission tile (Track B).
- `lib/stores/cluster.svelte.ts` (new) — composes nodes + pods + daemon status + Flux status. Polls at 15s; SSE where available.
- `lib/stores/auth.svelte.ts` (new) — `/api/auth/status` + `auth.refresh.*` SSE.
- `lib/stores/mission.svelte.ts` (new) — aggregator. References other stores; doesn't own data.
- Audit existing stores for polling intervals. Standardize to `5s | 15s | 60s` per SSE-005. Update `fleet.svelte.ts`, `health.svelte.ts`, `tasks.svelte.ts`.
- Add `lastUpdated` + `source` to every store's exposed state for `FreshnessMeter`.

**Tests**
- `lib/stores/weaver.test.ts` — SSE consumption, polling fallback, disconnect handling.
- `lib/stores/polling-convention.test.ts` — enumerates all stores, asserts every interval is in `{5000, 15000, 60000}`.

**Verify**
```bash
cd internal/hud/frontend && pnpm test -- lib/stores
```

### A4 — Motion + a11y pass

**Changes**
- Global `prefers-reduced-motion` handling in `tokens.css`.
- Focus-visible ring defined as a token; applied in a single global selector.
- SSE-update "tint flash" helper in `lib/design/motion.ts`.
- Modal/drawer focus-trap helper (reuse if exists; otherwise `lib/utils/focusTrap.ts`).

**Tests**
- Playwright keyboard-nav smoke: tab through Mission, open a drawer, esc-close, assert focus returns.
- Reduced-motion snapshot: assert no transform animations fire with the OS setting on.

---

## Track B — Mission Control

**Spec refs**: MC-001..003

### B1 — Rename + router slot

**Changes**
- `App.svelte:32` — rename import `OverviewPanel` → `MissionPanel`. Rename file `lib/components/OverviewPanel.svelte` → `MissionPanel.svelte`.
- `lib/stores/router.svelte.ts` — rename view id `overview` → `mission`. Keep backward-compat alias that resolves `overview` → `mission` for one release (hash routing).
- `lib/stores/router.svelte.ts` — introduce router group ordering per spec §IA.

**Tests**
- `router.svelte.ts` test: old `#overview` hash redirects to `#mission`.

### B2 — 4-tile hero + freshness

**Changes**
- `MissionPanel.svelte` rebuilt. Imports `mission.svelte.ts` aggregator store.
- Tile 1 (`SessionHeroTile.svelte`) — primary session card; empty state links to "Start session" in palette.
- Tile 2 (`WeaverLanesTile.svelte`) — horizontal strip of 4 lanes with sparklines.
- Tile 3 (`ClusterPulseTile.svelte`) — 5-pill ring.
- Tile 4 (`AttentionRailTile.svelte`) — consumes `AttentionRail` primitive; items from mission store aggregating session/auth/daemon/cluster alerts.
- Each tile embeds `FreshnessMeter`.

**Tests**
- Vitest component: empty states render correctly.
- Playwright: mount Mission, no panels started yet → tiles show SSE `connecting` state; once backend comes up, pills resolve.

### B3 — Keyboard + intent actions

**Changes**
- `j/k` navigation across attention-rail items (global keybind when `router.view === 'mission'`).
- `Enter` on focused attention item dispatches the mapped command via palette `Do` pipeline (Track G dependency; stub a no-op dispatcher until Track G lands).

**Tests**
- Keyboard nav: focus movement + command dispatch stub assertions.

---

## Track C — Sessions as primary entity

**Spec refs**: SES-001..004

**Depends on**: spec 87 SESS-001, SESS-005. Also `.loom/84` Slice B (already shipped).

### C1 — Split Sessions panel out of Fleet

**Changes**
- New `lib/components/SessionsPanel.svelte`. Registered under router group `Sessions`.
- Reuse `fleetStore.sessions` for data; Fleet panel keeps its own agent/claim/worktree tables.
- Columns per SES-001. Filters (agent type, namespace, parent presence, draining).
- Uses `StandardStates` + `FreshnessMeter`.

**Tests**
- Vitest component render: filters compose correctly.
- Playwright: filter by draining → only draining sessions appear.

### C2 — Session detail drawer tabs

**Changes**
- `lib/components/session-detail/SessionDetailDrawer.svelte`.
- Tab 1 Graph: `SessionGraphTab.svelte`. Uses `sessionTree` helper (already in `fleet.svelte.ts` per `.loom/84` Slice B). Renders d3-style tree or simple nested DOM.
- Tab 2 Spawns: `SessionSpawnsTab.svelte`. Queries spawns where `Metadata.parent_session_id == session.id`. Needs `GET /api/agent/spawn?parent_session_id=<id>` filter — add in `internal/hud/domain/spawn/handlers.go`.
- Tab 3 Weaver: `SessionWeaverTab.svelte`. Queries weaver history where `weaver_query_id` links to a spawn in this session. Needs `GET /api/weaver/history?session_id=<id>` filter — add in `internal/hud/domain/weaver/handlers.go`.
- Tab 4 Trace: mount existing trace drawer content.

**Tests**
- Handler tests for new query filters.
- Component: tabbed navigation, focus trap.

### C3 — Session kill/drain actions

**Changes**
- Drawer footer actions row. `End Session` → `POST /api/sessions/{id}/close` (new; wraps `loom/session/close`). `Drain daemon` → `POST /api/daemon/drain` (Track E delivers the daemon endpoint; stub here if E not yet merged).

**Tests**
- Handler test for `close`. Admin-scope gate on `drain`.

### C4 — `parent_session_id` chevron everywhere

**Changes**
- `lib/widgets/SessionRef.svelte` (new) — renders a session id badge with a chevron if parent exists. Clicking opens parent's drawer.
- Replace hand-rolled session id renders across FleetPanel, SpawnDetailPanel, TracesPanel, etc.

**Tests**
- Component test: chevron appears/hides correctly; click dispatches drawer event.

---

## Track D — Weaver Console

**Spec refs**: WVC-001..006, API-001..003

**Depends on**: spec 87 Slices 3–5 (WVR-* + OBS-*).

### D1 — Three-column shell

**Changes**
- Rename `lib/components/WeaverPanel.svelte` → `WeaverConsole.svelte`. Keep default export alias for one release.
- Layout with CSS grid: `minmax(240px, 320px) 1fr minmax(320px, 420px)`.
- Left column: `WeaverDomainList.svelte` — consumes `weaverStore.domains`. Each row: name, backend badge, requires-spawn lock, autonomy dial (read), status pill. `+ New Domain` button at top.
- Middle column: `WeaverDispatchLanes.svelte` — `Live` + `History` tabs.
- Right column: `WeaverDetailPane.svelte` — switches between domain-selected and query-selected modes.

**Tests**
- Vitest: three-column render; responsive collapse to single-column on narrow width.

### D2 — Domain detail — Config / YAML / Test / Audit

**Changes**
- `WeaverDomainConfig.svelte` — form fields per WVC-002. On save, `PUT /api/weaver/domains/{name}`.
- `WeaverDomainYaml.svelte` — Monaco editor wrapping the same domain config serialized to YAML. Bidirectional sync with Config tab via derived state.
- `WeaverDomainTest.svelte` — prompt textarea + `Dispatch` button. Calls `POST /api/weaver/query` with this domain preselected. Streams result inline via `weaver.query.*` SSE.
- `WeaverDomainAudit.svelte` — consumes `/api/audit?kind=weaver.domain&name=<>`.
- **Backend**: `internal/hud/domain/weaver/handlers.go` — add `handleDomainGet`, `handleDomainPut`, `handleDomainDelete`, `handleQuery`. Each calls daemon RPC: `loom/weaver/domain.get|put|delete|query`. Admin-scope gate on mutations; `ScopeAgentSpawn` on query when `RequiresSpawn`.
- **Daemon**: add matching RPC handlers in `internal/daemon/weaver_handlers.go` (new if absent) — delegate to `pkg/weaver` + gitops write via `pkg/weaver/domain_yaml.go` updater.

**Tests**
- `handlers_test.go`: PUT respects admin scope; DELETE requires confirm header; GET returns full config.
- `weaver_handlers_test.go` (daemon): domain.put writes YAML atomically, emits `weaver.domain.changed` event, records audit.
- Component: Config form dirty-state guard ("Unsaved changes — discard?" on tab switch).
- Component: YAML editor syncs with Config on both directions without infinite loop.

### D3 — Dispatch lanes (Live tab)

**Changes**
- `WeaverLive.svelte` — groups in-flight queries by backend. Subscribes to `weaver.query.started|completed|failed` SSE.
- Per-lane header: `WeaverLaneHeader.svelte` — in-flight count, p50 latency, `Pause lane` toggle. Pause calls `POST /api/weaver/lanes/{backend}/pause`.
- **Backend**: `/api/weaver/lanes/{backend}/pause|unpause`. Daemon-side lane-pause state in `pkg/weaver/router.go` — if lane paused, `runSubAgent` returns `lane_paused` structured error.

**Tests**
- Router test: pausing the `claude-code` lane rejects new dispatches with structured error.
- Component: live lanes update on SSE; empty-state renders per lane.

### D4 — Query detail (right column)

**Changes**
- `WeaverQueryDetail.svelte` with four tabs: Trace, Pod, Cost, Raw.
- Pod tab: if `query.metadata.backend != flexinfer`, resolve the spawn id via `weaver_query_id` index and render `SpawnDetailPanel` (without its own shell).
- Cost tab: sparkline of tokens + running $ per entry.
- Raw tab: JSON viewer.

**Tests**
- Component: backend=flexinfer hides Pod tab; others show it.

### D5 — Autonomy dial per domain

**Changes**
- `AutonomyDial` primitive (Track A) mounted in `WeaverDomainList` row + `WeaverDomainConfig`. Persists via `PUT /api/weaver/domains/{name}` with `autonomy` field. Emits `weaver.domain.changed` with before/after.
- Router respects autonomy:
  - `Manual`: every dispatch requires `ScopeAgentSpawn` *and* an `X-Loom-Autonomy-Override: confirm` header. HUD palette pipeline fires the header only after the Enter-confirm preview.
  - `Guarded`: dispatches proceed; queries that exceed `SpawnOverrides.MaxCostUSD` pre-estimate (using `spawn cost estimator`) require confirm.
  - `Auto`: no extra gating.

**Tests**
- `pkg/weaver/router_test.go`: autonomy=Manual without confirm header → error; with header → dispatches.
- Component: dial change triggers API call + refreshes list.

### D6 — Lane/domain pause

**Changes**
- UI gestures already in D3 + D5. Backend endpoints:
  - `/api/weaver/lanes/{backend}/pause|unpause`
  - `/api/weaver/domains/{name}/pause|unpause`
- Daemon `loom/weaver/pause` RPC + in-memory pause state (survives restart via gitops domain YAML with `paused: true` bit). Emits `weaver.lane.paused` / `weaver.domain.paused` SSE.

**Tests**
- Integration: pause domain `cluster-ops`; attempt dispatch → rejected; unpause; succeeds.

---

## Track E — Cluster panel

**Spec refs**: CLU-001..004, API-004..005

### E1 — Panel shell + Nodes

**Changes**
- `lib/components/ClusterPanel.svelte` (new). Registered under router group `Cluster`.
- Row 1 Nodes: `ClusterNodesTable.svelte`. Data from `GET /api/cluster/nodes`.
- **Backend**: `/api/cluster/nodes` handler at `internal/hud/domain/cluster/handlers.go` (new package). Shells out via existing k8s client; field set: name, role, disk%, cpu%, mem%, ready, taints. Cache 15s.
- Disk pressure bar uses color thresholds from memory doc (`reference_k3s_disk_pressure_runbook`).

**Tests**
- Handler: mocked k8s client returns nodes; response shape matches.
- Component: node with disk > 85% renders warning bar.

### E2 — Pods tabbed view

**Changes**
- `ClusterPodsTable.svelte` with tabs `Spawn | Daemon | Refresher | MCP | Other`.
- Classification logic in `internal/hud/domain/cluster/classify.go`: labels-based (`app.kubernetes.io/component`, `devbox/agent-id`, `loom/kind`).
- Pod detail drawer `ClusterPodDetail.svelte` with Overview / Logs / Events / Actions tabs.
- **Backend**: `/api/cluster/pods`, `/api/cluster/pods/{ns}/{name}` (describe), `/api/cluster/pods/{ns}/{name}/logs` (SSE stream), `/api/cluster/pods/{ns}/{name}/events`, `DELETE /api/cluster/pods/{ns}/{name}`.

**Tests**
- Classification unit tests covering all labels.
- Handler: log streaming emits SSE correctly; delete requires admin + confirm header.

### E3 — Daemon + Flux tiles

**Changes**
- `ClusterDaemonCard.svelte` — consumes `loom/session/status` (SESS-001). Drain/Undrain buttons.
- `ClusterFluxCard.svelte` — consumes `/api/cluster/flux` (new; shells out to `flux get kustomizations -A -o json` on daemon host).
- **Backend**: `/api/cluster/flux` handler; parses JSON; returns `{ kustomizations: [{ name, ns, ready, lastReconciled }] }`.
- **Backend**: `/api/daemon/drain`, `/api/daemon/undrain` — admin-scope gate. Calls `SessionManager.Drain()` (spec 87 SESS-002) / undrain.

**Tests**
- Handler: Flux parse covers success and `kustomization-not-ready` shapes.
- Integration: drain via endpoint flips `loom/session/status.draining` to true.

### E4 — Admin-scope + confirmation gates

**Changes**
- All mutating endpoints require `Authorization: Bearer <admin-token>`. HUD UI disables buttons with a lock tooltip if the token is missing; server is authoritative.
- Confirmation modal `lib/components/shared/ConfirmModal.svelte` (new or reuse if exists) for destructive actions; includes the exact command about to run.

**Tests**
- E2E: non-admin → button disabled; admin → modal appears with command preview.

---

## Track F — Auth panel + rotate flow

**Spec refs**: AUT-001..005, API-006

**Depends on**: spec 87 AUTH-002..009.

### F1 — Panel shell + per-vendor table

**Changes**
- `lib/components/AuthPanel.svelte`. Registered under `Cluster` group (sibling to ClusterPanel).
- Consumes `authStore` (Track A). One row per vendor × mode. Columns per AUT-001.
- **Backend**: `/api/auth/status` aggregates `cluster-agent-api-keys` + `cluster-agent-auth` presence, plus refresher CronJob last-success from Prometheus.

**Tests**
- Handler: returns merged view; missing keys show `present=false`.
- Component: row renders all states.

### F2 — Rotate flow modal

**Changes**
- `lib/components/auth/RotateAuthModal.svelte`. Three sub-modes: API key input, OAuth device flow, service account JSON upload.
- Submits to `/api/auth/cluster-set-key` or `/api/auth/cluster-login` endpoints (spec 87 AUTH-003/004). HUD polls status during the gitops commit + Flux reconcile stages; shows progress.
- **Backend**: endpoints shell out to the same CLI logic used by `cmd/loom/cmd_auth.go` (refactor to expose a reusable `ClusterAuth` service).

**Tests**
- Handler: dry-run mode doesn't commit.
- Component: error state renders vendor error verbatim; progress advances through `validate → commit → reconcile → verify`.

### F3 — Refresher health card + expiry warnings

**Changes**
- `RefresherHealthCard.svelte` — mounts above the vendor table. Reads Prometheus metrics via a daemon-proxied endpoint `/api/auth/refresher-health`.
- `Run now` button triggers the CronJob's `kubectl create job --from=cronjob/mcp-auth-refresher manual-<ts>` via daemon admin RPC.
- Attention rail integration: when any OAuth token expires within 30min, emit an alert into the mission attention rail with intent `Rotate <vendor>`.

**Tests**
- Handler: `refresher-health` aggregates correctly when CronJob missing.
- Component: 30-min expiry triggers attention alert; rotating clears it.

### F4 — Spawn pre-flight badge

**Changes**
- `SpawnPanel.svelte` form gains a live-updated `AuthPreflightBadge.svelte`. Consumes `authStore`. When agent-type changes, resolves expected `AuthMode` via the same logic `resolveAuthMode()` in `internal/hud/spawn.go`.
- Red `missing` state blocks the `Spawn` button.

**Tests**
- Component: changing agent type swaps the resolved AuthMode; missing state disables submit.

---

## Track G — Command Palette + Mobile

**Spec refs**: CMP-001..004, MOB-001..005

### G1 — Palette Go/Do tabs

**Changes**
- `CommandPalette.svelte` refactored. `Go` tab is today's behavior (view jumps). `Do` tab registers commands via a `registerCommand(def: CommandDef)` API; each panel registers its own.
- Palette state: `mode: 'go' | 'do'`. Tab toggle via top-level tabs + keyboard (`Tab` key within palette).
- `lib/palette/registry.ts` — singleton registry consumed by panels on mount.

**Tests**
- Registry test: duplicate commands collapse.
- Component: Tab switches modes; Enter on Go navigates; Enter on Do fires command.

### G2 — Initial command set

**Changes**
- Register commands per spec CMP-002. Each command is a small file under `lib/palette/commands/*.ts`.
- Side-effectful commands display a preview block before Enter-confirm. The preview is data-driven from the command def (`dryRun?: (args) => Promise<PreviewBlock>`).

**Tests**
- E2E: `Cmd+K`, type `spawn`, select "Spawn agent," fill form, see preview, Enter → POST to `/api/agent/spawn`.

### G3 — Autonomy integration

**Changes**
- Session-level autonomy default persists in `sessionStorage`. Domain-level from weaver. Per-spawn override from form.
- Palette reads the effective autonomy level for the command's target; renders the dial choice with override option.

**Tests**
- Unit: autonomy resolution chain (session → domain → spawn).

### G4 — Mobile home

**Changes**
- `internal/hud/web/mobile/*` (or wherever mobile static assets live — verify via `grep -r "mobile" internal/hud/web`). Mobile home renders the 4 MC tiles stacked.
- Reuse `mission.svelte.ts` store; shell is mobile-specific.

**Tests**
- Mobile Playwright viewport: tiles stack, tap-through works.

### G5 — Mobile approvals + kills

**Changes**
- Push notification route: `/api/hud/mobile/push/register`, `/api/hud/mobile/push/unregister`. Uses APNs via existing companion infrastructure.
- Approval push template: `"<agent> wants to run <command> — approve / reject"`. Tap → HTTP to approve endpoint.
- Kill-switch swipe: `DELETE /api/agent/spawn/{id}` (existing endpoint, admin-gate already present).

**Tests**
- Approval E2E mocked against a test agent-context gate.
- Kill-switch confirms then fires DELETE.

---

## Cross-cutting: SSE envelope additions

**Spec refs**: API-008

**Changes**
- `internal/hud/sse.go` — add broadcast helpers for: `weaver.query.accepted|started|completed|failed`, `weaver.domain.changed|paused|unpaused`, `weaver.lane.paused|unpaused`, `auth.refresh.started|completed|failed`, `cluster.node.pressure|recovered`, `daemon.draining|undrained`.
- Wire each helper into the appropriate producing code path (daemon weaver handlers, auth-refresher CronJob via metric-scrape-on-demand, session manager draining hook, node-pressure reconciler if we add one).

**Tests**
- SSE subscribers assert each envelope type is received after the triggering action.

---

## Cross-cutting: audit log

**Spec refs**: API-007

**Changes**
- New `pkg/audit/` package: append-only log with structured entries `{ when, actor, scope, kind, payload, source }`. Persisted to Qdrant (existing agent-context storage) under `namespace="audit"` for durability, with an in-memory ring buffer for fast HUD reads.
- Emission points:
  - Weaver domain put/delete/pause.
  - Auth rotate.
  - Daemon drain.
  - Pod delete.
  - Workflow gate approve/reject.
- `/api/audit?kind=&actor=&since=` — filtered query.

**Tests**
- Pkg tests for append + query.
- Integration: weaver domain put emits one audit entry.

---

## Rollout gate + feature flag

**Flag**: `LOOM_HUD_CONTROL_CENTER` (env var read by the HUD Go binary, forwarded to frontend via `/api/config`). Default `off` until Track F complete.

When `off`:
- New panels (Sessions, Weaver Console, Cluster, Auth) hidden from router sidebar.
- Old `Overview` panel retained under `#overview`; `#mission` resolves to it for compatibility.
- Old `WeaverPanel` used.
- Palette `Do` tab hidden.

When `on`:
- New IA active.
- `#overview` redirects to `#mission`.
- Palette Go/Do tabs visible.

**Ops**: flag flipped in platform/gitops ConfigMap; default propagates via Flux.

---

## Quality gates (per track)

Every track ships with:

```bash
# frontend
cd internal/hud/frontend && pnpm typecheck && pnpm build && pnpm test
pnpm test:a11y
pnpm test:e2e -- <track>

# backend
go build ./... && go test ./internal/hud/... ./internal/daemon/... ./pkg/weaver/... ./pkg/audit/... -count=1

# integration smoke
bin/workspace-clean --report   # no stray worktrees
make docker   # build images
```

Track-specific manual smokes:
- **B**: Mission loads in <1s cold; all four tiles resolve within 2s of SSE connect.
- **C**: open a session drawer with 3 nested child sessions; tree renders correctly.
- **D**: edit `cluster-ops` domain; YAML + form stay in sync; save emits audit entry; dispatch a test query end-to-end.
- **E**: drain daemon via button; spawn attempt returns `draining` error; undrain; spawn succeeds.
- **F**: rotate OpenAI API key; next Codex spawn uses new key (verify by inspecting `cluster-agent-api-keys` via kubectl).
- **G**: palette Do tab lets you spawn an agent via keyboard alone.

---

## Risks + mitigations

| Risk | Mitigation |
|------|-----------|
| Flag-gated parallel UI drift | Keep old `Overview` + `WeaverPanel` intact; dual-maintain tests for both until cutover. Time-box to one release cycle. |
| Daemon admin-scope gap on new endpoints | Track-E introduces `AdminScopeMiddleware`; every new mutating endpoint must be added there. Add a go vet lint that fails PRs adding endpoints without scope assertion. |
| Autonomy dial + palette confirm creates friction | Ship `Auto` as default for flexinfer domains; make `Manual` opt-in for non-flexinfer. Default posture matches today's behavior. |
| Gitops-write latency from Auth rotate (30s+ reconcile) | Surface each phase of the flow (commit → push → reconcile → verify). Don't block the UI; poll. |
| Mobile notification storms during high spawn volume | Coalesce approvals; rate-limit to one push per 10s per device; provide `Silence for 15min` from inside the notification. |
| SSE envelope proliferation causing HUD re-render churn | Store-level debouncing (100ms) for high-frequency events. Monitor via `lib/stores/metrics.ts`. |
| K3s API cost from Cluster panel polling | Cluster store polls at 15s; node/pod describe on-demand only; use k8s informer if polling ever exceeds budget. |
| Monaco bundle weight for YAML editor | Lazy-load the editor (dynamic import on tab-click); HUD initial bundle unaffected. Fallback: plain textarea if the import fails. |

---

## Explicit deferrals

- Dashboard embedding of Prometheus panels (keep existing KPI surfacing).
- Real-time collaboration (shared cursors / presence inside HUD).
- Theming beyond existing dark/light toggle.
- Browser-extension integration.
- Log search beyond per-pod tail (Loki MCP is the search path).
- Fully offline HUD.
- Multi-user RBAC beyond admin token.
- Mobile authoring.
- Harvester cluster view.

---

## Dependency matrix

| Track | Hard deps | Soft deps |
|-------|-----------|-----------|
| A | — | — |
| B | A | spec 87 SESS-001 |
| C | A | spec 87 SESS-005; `.loom/84` Slice B (shipped) |
| D | A, spec 87 WVR-001..006, OBS-001..002 | E (cluster pulse for Mission tile 3 cross-check) |
| E | A, spec 87 SESS-001, SESS-002 | — |
| F | A, spec 87 AUTH-002..009 | E (Cluster panel shares group) |
| G | A, D (command targets), E (command targets), F (rotate command) | — |

---

## Sources

- `.loom/89-research-master-control-center-2026-04-21.md`
- `.loom/90-product-spec-master-control-center-2026-04-21.md`
- `.loom/87-product-spec-session-spawning-weaver-2026-04-19.md` — substrate
- `.loom/88-implementation-plan-session-spawning-weaver-2026-04-19.md` — substrate sequencing
- `.loom/84-plan-hud-ux-polish-2026-04-11.md` — shipped polish baseline
- `internal/hud/frontend/src/App.svelte:1-60`
- `internal/hud/frontend/src/lib/components/WeaverPanel.svelte:14-50`
- `internal/hud/domain/weaver/handlers.go:9-151`
- `internal/hud/spawn.go:641-661, 1117-1163`
- `internal/daemon/session.go:22-255`
- `cmd/loom/proxy_session.go:18-119`
- `pkg/weaver/domain.go:8-21`
- `pkg/weaver/router.go`
