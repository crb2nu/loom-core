# Research: HUD + Weaver as Master Control Center

**Date**: 2026-04-21
**Scope**: `internal/hud/**`, `pkg/weaver/**`, `internal/daemon/session*.go`, `internal/spawn/`, `internal/hud/domain/**`, `cmd/loom/cmd_auth.go` (planned), `cmd/mcp-auth-refresher/` (planned)
**Drives**: `.loom/90-product-spec-master-control-center-2026-04-21.md`, `.loom/91-implementation-plan-master-control-center-2026-04-21.md`
**Predecessors**:
- `.loom/86-research-session-spawning-weaver-integration-2026-04-19.md` — session + auth + weaver backend research (still the foundation)
- `.loom/87-product-spec-session-spawning-weaver-2026-04-19.md` — SESS-*, AUTH-*, WVR-*, OBS-* slices (spec frozen; implementation in flight)
- `.loom/88-implementation-plan-session-spawning-weaver-2026-04-19.md` — Slice-1 through Slice-6 sequencing
- `.loom/84-plan-hud-ux-polish-2026-04-11.md` — ten-gap HUD polish plan, Waves 1–2 shipped
- `.loom/76-implementation-plan-weaver-hardening-2026-04-04.md` — ORCHESTRA→WEAVER rename + `query_id` telemetry

## 1. What the user asked for

> "Let's plan the next round of feature improvements pulling together all the recent work we've done with agent/session management and k3s based instances. Let's really work to turn the HUD and weaver into a master control center. /frontend-ux-craft"

Two layered asks:

| Layer | Target |
|-------|--------|
| **Substrate** | Unify the four in-flight tracks — session lease, cluster auth, weaver bridge, K3s spawn — into a single operator surface instead of four loosely coupled slices. |
| **Experience** | Treat the HUD + Weaver panel as a *master control center*: command & observe the whole agent/session/spawn/weaver/cluster stack from one coherent UI. Apply `/frontend-ux-craft` discipline (IA, visual system, motion, states, a11y). |

This research pulls together what's shipped, what's planned (86-88), and what's *still missing* to credibly call the HUD a control plane rather than a dashboard.

## 2. Current state (sourced)

### 2.1 HUD frontend inventory

Svelte 5 runes SPA under `internal/hud/frontend/src/`. Top-level router wires 20+ panels (`App.svelte:11-34`):

| Cluster | Panels | Notes |
|---------|--------|-------|
| **Ops/home** | `OverviewPanel`, `CommandPalette`, `LifecyclePanel` | Overview has known dual-data-source staleness (see `.loom/84` P0 #3) |
| **Agents/sessions** | `FleetPanel`, `PresencePanel`, `SpawnPanel`, `SpawnDetailPanel`, `TasksPanel` | Fleet + Spawn cross-linked after `.loom/84` Wave 1; session hierarchy surfaced after Slice B |
| **Observability** | `TracesPanel`, `TimelinePanel`, `StreamPanel`, `DispatchPanel` | Traces now the "drill-down" surface for any agent row |
| **Knowledge/memory** | `MemoryPanel`, `KnowledgePanel`, `ReasoningPanel`, `ContextHealthPanel` | Read-only — no compaction/prune buttons today |
| **Control/plumbing** | `CatalogPanel`, `ServersPanel`, `SandboxPanel`, `WorkflowsPanel`, `ShuttlePanel` | Sandbox start/stop works; workflow authoring missing |
| **Coordination** | `WeaverPanel` | Read-only (§2.3 below) |

State stores in `internal/hud/frontend/src/lib/stores/` (runes-based): `fleet`, `events` (SSE), `health`, `tasks`, `stream`, `overlay`, `router`, `spawn`.

### 2.2 HUD REST surface

Entry: `internal/hud/routes.go`. Groups:

| Domain | Endpoints | Readiness |
|--------|-----------|-----------|
| Core | `/api/status`, `/api/health`, `/api/ping` | GA |
| Fleet/presence | `/api/fleet`, `/api/agents`, `/api/presence`, `/api/claims`, `/api/worktrees` | GA |
| Sessions/tasks | `/api/sessions{,/{id}/entries,/{id}/trace}`, `/api/tasks` | GA; trace drawer recent |
| Spawn | `/api/agent/spawn*` (create, list, detail, telemetry, stop, delete, message, interrupt) | GA |
| **Weaver** | `/api/weaver/{status,domains,history,metrics}` | **Read-only — no mutations** |
| Observability | `/api/traces`, `/api/timeline`, `/api/kpis`, `/api/cost`, `/api/otel` | GA |
| Catalog | `/api/catalog`, `/api/catalog/{name}/{enable,disable}`, `/api/daemon-metrics` | GA |
| Topology | `/api/topology` | GA |
| Agent context | `/api/agent/context-telemetry`, `/api/agent/metrics` | GA telemetry; no mutations |
| Realtime | `GET /api/events` (SSE hub) | GA |

### 2.3 Weaver in the HUD today

`WeaverPanel.svelte:14-50` polls four endpoints every 5s and renders. `handlers.go:9-28` proxies `loom/weaver/status` verbatim. Capabilities:

- ✅ Read domain list, router/subagent model, query history, lifetime metrics
- ❌ Add/edit/delete a domain from the UI
- ❌ Switch a domain's `Backend` (blocker: WVR-001 ships the field)
- ❌ Fire a test query against a domain
- ❌ See which in-flight spawn came from which weaver query (blocker: WVR-004)
- ❌ Approve/reject a domain that is `RequiresSpawn: true` before it fires (blocker: WVR-005)

The panel is a passive observer. There is no equivalent of "run this now," "route this there," or "lock this domain down."

### 2.4 K3s / spawn integration

`internal/hud/spawn.go` orchestrates K8s pod spawns per agent type (`buildAgentCommand` at `:641-661`, secret plumbing at `:1117-1163`). `internal/spawn/` contains the state machine, budget watcher, SSE emission. The HUD surfaces per-spawn lifecycle cleanly, but:

- ❌ No node-level view: pod → node affinity, node disk pressure, node CPU/mem headroom are invisible in HUD.
- ❌ No cluster-auth view: whether `cluster-agent-api-keys` / `cluster-agent-auth` (spec 87 AUTH-*) are present, fresh, and wired is invisible.
- ❌ No "drain mode" view: when `SessionManager.IsDraining()` flips true (SESS-002), the HUD shows a banner only if the connection drops — there is no proactive "daemon entering drain in 10s" surface.
- ❌ No refresher-health view: the planned `mcp-auth-refresher` CronJob (AUTH-005) will emit `loom_auth_refresh_*` metrics, but there's no panel for them.
- Spawn detail does not yet show `AuthMode`, `parent_session_id`, or `weaver_query_id` — all added by spec 87 but UI-side not wired.

### 2.5 Session management today

Proxy-lease architecture shipped (`internal/daemon/session.go:22-255`, `session_handlers.go:12-116`, `cmd/loom/proxy_session.go:18-119`). Agent-context sessions (`pkg/agentcontext/svc_sessions.go`) are orthogonal. Slice-1 of spec 87 adds `loom/session/status`, draining, metrics, parent-session propagation, and presence join. What's still missing from a UX angle even *after* Slice-1 lands:

- No first-class **Sessions panel** in the HUD. Today the Fleet panel shows sessions as rows of a table; there is no dashboard view oriented around the session as the unit of work.
- No **session-graph view** — when one proxy session spawns three pods which each spawn a sub-pod, there is no tree or DAG rendering. Fleet Slice B surfaces root/parent/child as indent, but for multi-agent worktrees that is not enough.
- No **session timeline view** crossing proxy / agent-context / spawn layers on one x-axis.

### 2.6 External reference (2026 patterns)

Scan of 2026 agent control-plane designs and articles ([mission-control, builderz-labs](https://github.com/builderz-labs/mission-control); [GitHub Copilot mission control](https://github.blog/ai-and-ml/github-copilot/how-to-orchestrate-agents-using-mission-control/); [Fiddler AI](https://www.fiddler.ai/control-plane); [Smashing UX patterns](https://www.smashingmagazine.com/2026/02/designing-agentic-ai-practical-ux-patterns/); [Microsoft Agent 365](https://blog.admindroid.com/microsoft-agent-365-unified-control-plane-to-manage-ai-agents/)) surfaces a consistent vocabulary:

| Pattern | What it means | Our closest equivalent |
|---------|---------------|------------------------|
| **Autonomy dial** | Explicit per-agent/per-domain guardrail between "full auto" and "confirm each step" | Implicit in `RequiresSpawn`, `MaxCostUSD`, `MaxTurns` — not yet composed into one UI control |
| **Governance + approval flow** | Human-in-loop gate on sensitive actions, with decision trail | `agent_workflow_approve` exists in agent-context but is not surfaced in HUD |
| **Activity history / audit log** | Reconstruct what happened, when, who initiated | Traces + session trace drawer exist; no cross-session audit view |
| **Lifecycle management UI** | Create / inspect / manage all agents from one surface | Spawn panel covers pods; no parity for proxy sessions or weaver queries |
| **Unified runtime visibility** | Sessions + logs + memory + token usage in one place without tool-stitching | Closest to our `TracesPanel` but missing memory + token sidebars |
| **Kanban / lane UX for in-flight work** | Inbox → Assigned → In Progress → Review → Done | Our "attention rails" in Overview/Lifecycle gesture at this but are not navigable (`.loom/84` P1 #6) |

The gap between our HUD and these reference designs is not features — we already have the data — it is **orchestration of what we have into a control-oriented frame** (autonomy, governance, lifecycle).

Sources:
- [mission-control (builderz-labs) — self-hosted orchestration dashboard](https://github.com/builderz-labs/mission-control)
- [openclaw-mission-control — AI agent orchestration](https://github.com/abhi1693/openclaw-mission-control)
- [GitHub Copilot: orchestrate agents using mission control](https://github.blog/ai-and-ml/github-copilot/how-to-orchestrate-agents-using-mission-control/)
- [Architecting the AI Agent Control Plane: 3 Design Patterns for 2026 — Paul Serban](https://www.paulserban.eu/blog/post/architecting-the-ai-agent-control-plane-3-design-patterns-for-2026/)
- [Designing for Agentic AI: Practical UX Patterns for Control, Consent, and Accountability — Smashing Magazine](https://www.smashingmagazine.com/2026/02/designing-agentic-ai-practical-ux-patterns/)
- [What is an AI Agent Control Plane? — Amurg Blog](https://amurg.ai/blog/what-is-an-ai-agent-control-plane/)
- [Control Plane for AI Agents — Fiddler AI](https://www.fiddler.ai/control-plane)
- [Microsoft Agent 365 — unified control plane](https://blog.admindroid.com/microsoft-agent-365-unified-control-plane-to-manage-ai-agents/)

## 3. Problem statement

The HUD today is an **observability dashboard wearing control-plane branding**. It reads excellently. It commands poorly. The recent substrate work (86-88) lands the plumbing for *actual* control — drain mode, cluster auth, weaver backend routing, parent-session propagation — but does not prescribe the UI surfaces that expose those controls. Without those surfaces:

| Symptom | Root cause |
|---------|-----------|
| Operators still shell out to `loom auth cluster-set-key`, `loom sync`, `kubectl` to do control actions | No HUD surface for cluster auth, secret status, daemon drain, weaver mutation |
| Weaver is a "what happened" panel, not a "what is routing where" panel | Read-only endpoints, no writes, no flight controller visualization |
| Session, spawn, weaver-query, and K8s-pod are four mental models the operator must stitch | No single-object detail view that pivots across those four layers |
| Attention rails and alerts show up but don't actuate anything | No command-to-action binding on alerts |
| Cluster identity and auth mode are invisible until a spawn fails with a credential error | No auth-health panel; `AuthMode` not surfaced in spawn detail |

## 4. Key findings

### 4.1 "Master control center" is an IA change before it is a feature list

The dominant pattern across 2026 designs is a **two-axis information architecture**:

- **X-axis (entity)**: What am I operating on? — agent, session, spawn, weaver query, domain, cluster, auth.
- **Y-axis (action)**: What am I doing? — observe, command, approve, audit.

Our HUD is strong on observe-per-entity. It is weak on command, approve, audit, and on cross-entity pivoting. The cheapest win is not new panels; it is **promoting the session to the primary entity** (because it is the only layer that can correlate all the others now that spec 87 ships `parent_session_id` + `weaver_query_id`) and reorganizing the existing panels around that.

### 4.2 Weaver is the best candidate for "flight controller" framing

Weaver is the only subsystem in the stack that both *decides routing* and *holds domain configuration as data*. Making it bi-directional unlocks a pattern that feels like a control plane:

- A top-level **Weaver Console** view lists domains as "lanes" (flexinfer, claude-code, codex, gemini). Each lane has a queue of recent queries, a health pill, a routing rule, and an autonomy dial.
- Clicking a lane drills into the domain: tools, system prompt, backend, `RequiresSpawn` gate, `SpawnOverrides` defaults. Editable (with audit log + diff preview).
- Clicking a query drills into its end-to-end trace: weaver decision → spawn pod (if any) → session entries → tool calls → cost.

This is a concrete realization of the "orchestration + governance + audit" pattern from the 2026 reference scan, built on data we already have plus the WVR-* work in flight.

### 4.3 K3s visibility is a missing primary surface

Right now K3s reality leaks into the HUD only through spawn pod status and the devbox panel. There is no "Cluster" panel that composes:

- Node inventory + disk pressure (extends 7e93b7 memory rule — k3s DiskPressure runbook)
- Pod inventory (spawn pods + daemon pods + mcp-auth-refresher + HUD itself)
- Cluster auth secret health (presence + expiry + last refresh for `cluster-agent-auth` / `cluster-agent-api-keys`)
- Daemon session status (epoch, draining, active session count from SESS-001)
- Flux/GitOps reconcile status (is the cluster in-sync with platform/gitops?)

Each of those facts exists somewhere — in `loom/session/status`, Prometheus exports, k8s API, Flux CRDs — but no panel composes them. Operators who want to answer "is the cluster healthy enough to spawn another Claude?" today have to ask three terminals.

### 4.4 Cluster auth must be observable before operators trust it

Spec 87 ships the plumbing (`cluster-agent-api-keys`, `cluster-agent-auth`, `mcp-auth-refresher`, `AuthMode` on spawn state) but leaves the HUD side open. From a UX-trust perspective, operators need:

- An **Auth** view that lists each vendor (Anthropic, OpenAI, Google) × each mode (API key, OAuth, service account) with present/expires/last-refreshed.
- A **one-click rotate** button that fires `loom auth cluster-set-key` / `cluster-login` from the HUD (shells out via daemon RPC).
- An **expiry warning rail** that joins into the Overview attention lanes.
- A **spawn pre-flight** badge on the spawn form showing the resolved `AuthMode` before submission (prevents the fail-fast path from surprising the user).

Without this, spec 87's AUTH-* work is invisible and the trust case for cluster-owned auth is hard to make.

### 4.5 State coherence is still the #1 UX debt

`.loom/84` called this out as P0 #3 (dual data sources on Overview) and P1 #7 (polling interval inconsistency). Those were addressed for Fleet, but the same pattern recurs for:

- Weaver: `WeaverPanel` polls at 5s on its own; no store; no SSE; if weaver is offline the panel just shows an error for 5s cycles.
- Catalog: polls independently; no cross-reference with Fleet's server awareness.
- Context health: separate store, separate monitor, duplicated data with Memory panel.

A master control center cannot tolerate the three-panel disagreement pattern. The fix is store unification + SSE-first (polling as fallback), applied consistently.

### 4.6 Command palette is the fastest path to "feels like a control plane"

`CommandPalette.svelte` exists today as a navigation overlay (Cmd+K → jump to panel). In a true control plane it is the **primary command surface**. Adding commands like:

- "Spawn a Claude on project loom-core"
- "Rotate cluster OpenAI API key"
- "Drain the daemon and restart"
- "Fire weaver query 'ship the feature' on domain cluster-ops"
- "Approve gate for workflow <id>"

…turns the palette into the single keyboard-driven operator shell. Every command maps to a HUD REST endpoint that already exists or is being added by spec 87. The palette also becomes the natural home for the autonomy dial ("Arm Claude-code for 30 minutes of autonomous work on this worktree" — emits a confirmation modal + starts a spawn with scoped budget).

### 4.7 Mobile is now a first-class peer, not a mirror

Recent work (feat/hud-traces, feat/mobile-session-trace-parity, feat/mobile-hud-polish) established a mobile companion surface hitting `/api/hud/mobile/*`. The master control-center plan should treat mobile as a **deliberately scoped subset** — approvals, alerts, spawn status at-a-glance, kill switches — not a straight port. Spec should include a mobile IA appendix.

### 4.8 Visual + interaction system is still ad-hoc

Panels share `lib/widgets/` primitives (Badge, ViewShell, etc.) but there is no documented design system. Recurring inconsistencies:

- Density: Fleet is dense; Overview is sparse; Weaver is middle. No intentional density tier.
- Motion: SSE-driven updates flash in; polling-driven ones pop. No shared motion grammar.
- Empty/error/loading: PresencePanel has no loading state (`.loom/84` Slice D pending); Weaver has no empty state per domain.
- Keyboard: App-level shortcuts (`,o,1-9`) exist; panel-level shortcuts are inconsistent.
- Color: lane colors per agent type appear in multiple panels (Fleet, Spawn, Lifecycle) but are not palette-unified; a11y contrast unverified.

A control plane earns trust through visual consistency as much as through feature completeness. `/frontend-ux-craft` discipline here is not polish — it is the signal that separates "dashboard" from "console."

## 5. Open questions / decisions needed

1. **Do we introduce a new top-level "Mission Control" home view, or re-home the existing `OverviewPanel`?**
   **Recommendation:** re-home — rename `Overview` → `Mission`, rebuild as session-centric hero + attention lanes, keep the slot/keyboard shortcut. Avoids a proliferation of "home" surfaces.

2. **How deep should weaver domain editing go in the HUD? CRUD? Full YAML editor? Approval-gated?**
   **Recommendation:** CRUD + inline form fields + raw-YAML fallback editor, wrapped in an approval-gated commit (`daemon.Admin.ApplyDomainChange`) that emits an audit entry. Avoids both "barely editable" and "YAML-editor-only" extremes.

3. **Should the "Cluster" view be a new panel or a pane inside the existing `Catalog`/`Servers`?**
   **Recommendation:** new top-level panel. Conflating node/pod state with MCP server catalog state muddles two different concerns. Keep Catalog focused on MCP registry.

4. **Autonomy dial: per-domain, per-spawn, or per-session?**
   **Recommendation:** three tiers layered: **per-session** (default autonomy for anything spawned from this session), **per-domain** (weaver domain overrides), **per-spawn** (explicit override at spawn form). Session > domain > spawn as inheritance chain.

5. **Command palette scope: navigation only, or full command surface?**
   **Recommendation:** full command surface, but **split into two tabs** within the palette (`Go` / `Do`). `Go` is instant nav (current behavior). `Do` requires Enter-to-confirm and shows a preview of the side-effect. Prevents accidental spawns.

6. **Mobile parity: how far?**
   **Recommendation:** approvals + kill switches + at-a-glance session/spawn status + one-tap cluster health. No mobile authoring (no weaver domain editor, no auth rotation on mobile). Scoped to "what you'd want on a phone while on call."

7. **How do we prove state coherence?**
   **Recommendation:** introduce a `FreshnessMeter` primitive (shared widget) that every panel surfaces. Every data source becomes { `lastUpdated`, `source` (sse|poll|stale) }. A master control center exposes its own trust model; this is the visual embodiment.

8. **Do we enforce RBAC in the HUD now, or defer?**
   **Recommendation:** defer full RBAC; ship the **admin-token scope check** that spec 87 WVR-005 introduces, plus visible "locked" badges on mutating actions. Full role management is a later cycle.

9. **Is this one big plan or decomposed?**
   **Recommendation:** treat as a **program** with five tracks (IA, Weaver Console, Cluster panel, Auth panel, Command palette + visual system). Each track ships independently. Spec 90 + plan 91 sequence them.

## 6. Scope boundaries

**In scope for this program**
- Mission Control IA (renames + hero + attention lanes)
- Weaver Console (bi-directional; dispatch lanes; audit log)
- Cluster panel (node/pod/auth/daemon rollup)
- Auth panel + rotate UX for `cluster-agent-api-keys` / `cluster-agent-auth` + refresher health
- Command palette evolution (Go/Do tabs + real commands)
- Visual + interaction system tokens (density, motion, empty/error/loading, keyboard model, color palette, a11y baseline)
- Freshness meter primitive + store consolidation pass
- Mobile scope: approvals, alerts, spawn kill-switch, cluster-health tile

**Out of scope**
- Full RBAC / multi-user HUD
- Mobile authoring (domain edit, auth rotation, workflow definition)
- Non-K8s backend visibility (only K3s matters here)
- New telemetry events beyond what spec 87 / OBS-* introduces
- Anything that requires a new vendor integration (Vault, SPIFFE, HashiCorp stack)
- Offline/local-first HUD (still live-connected)

## 7. Sources

- `internal/hud/frontend/src/App.svelte:1-60` — root component, panel imports, onMount bootstrap
- `internal/hud/frontend/src/lib/components/WeaverPanel.svelte:14-50` — 5s polling, no SSE, no mutations
- `internal/hud/domain/weaver/handlers.go:9-151` — read-only status/domains/history/metrics
- `internal/hud/spawn.go:641-661, 1117-1163` — agent routing + secret plumbing; referenced for AUTH-* impact
- `internal/daemon/session.go:22-255`, `session_handlers.go:12-116` — session lease foundation
- `cmd/loom/proxy_session.go:18-119` — proxy-side lease keepalive
- `pkg/weaver/domain.go:8-21` — `SubAgent` struct (no `Backend` yet)
- `pkg/agentcontext/svc_sessions.go`, `svc_presence.go` — orthogonal session + presence layer
- `internal/hud/routes.go:14-91` — full API surface inventory
- `.loom/86-research-session-spawning-weaver-integration-2026-04-19.md` — session + cluster-auth + weaver-bridge research
- `.loom/87-product-spec-session-spawning-weaver-2026-04-19.md` — SESS-*, AUTH-*, WVR-*, OBS-* in-flight
- `.loom/88-implementation-plan-session-spawning-weaver-2026-04-19.md` — Slice-1..Slice-6 sequencing
- `.loom/84-plan-hud-ux-polish-2026-04-11.md` — ten HUD gaps, Waves 1–2 shipped
- `.loom/76-implementation-plan-weaver-hardening-2026-04-04.md` — weaver `query_id` telemetry
- External pattern review: [builderz mission-control](https://github.com/builderz-labs/mission-control), [GitHub blog mission control](https://github.blog/ai-and-ml/github-copilot/how-to-orchestrate-agents-using-mission-control/), [Smashing agentic UX](https://www.smashingmagazine.com/2026/02/designing-agentic-ai-practical-ux-patterns/), [Fiddler control plane](https://www.fiddler.ai/control-plane), [Microsoft Agent 365](https://blog.admindroid.com/microsoft-agent-365-unified-control-plane-to-manage-ai-agents/)
