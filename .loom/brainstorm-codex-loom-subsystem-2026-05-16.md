# Brainstorm — Codex/loom hook + MCP proxy + daemon subsystem

**Date:** 2026-05-16
**Trigger:** Investigating Codex GUI tool-call line bouncing mid-response; suspected related to hooks. Investigation showed notify is fire-and-forget and per-tool hooks were already removed, so the visible bug is most likely `notifications/tools/list_changed` flap from the daemon's tool-cache refresh diffing against a flapping backend MCP server set. User asked for higher-leverage architectural and performance options across the subsystem, not just a patch for the bounce.

**Constraints surfaced:**
- Must keep Codex/Claude/Gemini/HUD all working off the same daemon.
- Hook surface is constrained by vendor schemas (Codex notify/[hooks], Claude `settings.json`, Gemini settings) — can't rewrite the agent CLIs.
- `~/.cache/loom/`, `~/.config/loom/loom.sock`, and `loom-agent-hooks.log` are already in place; greenfield is not required.
- Operationally: the daemon currently shows recurring 5s timeouts and HUD 502s — instability is real and recurring, not a one-off.

---

## Phase 1 — Diverge (7 framings)

### A. Capability-negotiate `listChanged: false` for Codex GUI
Loom proxy advertises `tools.listChanged: true` to every client (`cmd/loom/proxy_handlers.go:17`). Codex GUI reacts to the notification by re-fetching `tools/list` and redrawing the tool-call UI. Per-client capability negotiation: respond to the GUI's `initialize` with `listChanged: false` while leaving it `true` for clients that handle it gracefully (Claude, HUD).
- **Bet**: Bouncing root cause is the GUI's re-fetch reaction, not the underlying tool-set churn.
- **Risk**: Codex GUI never picks up legitimate within-session tool-list changes; has to be solved by session restart or a soft re-`initialize` path.

### B. Stable manifest with backend-health overlay
Today the tool set = union of currently-reachable backends; a flapping backend drops and restores its tools, so the set flaps, so `list_changed` fires (`daemon_tools_cache.go:191`). Instead: tool set = union of *registered* backends (registry-derived, stable). Backend health is independent overlay metadata. Tools from an unhealthy backend still appear; calls fast-fail with a typed error result.
- **Bet**: Tool-set flap is the class bug; transient backend health should never touch the advertised list.
- **Risk**: Tools that "look available" but fail to call can confuse agents and operators; needs surfaced "degraded" state in clients that care.

### C. Daemon-as-socket-sidecar, kill HTTP in the hot path
Hooks log (`/var/folders/.../loom-agent-hooks.log`) shows recurring HTTP 502s and 5s timeouts on heartbeat/session-start; hooks pay this every turn because they shell out → connect → POST → wait. Replace with UNIX-domain-socket transport on `~/.config/loom/loom.sock` (already exists for some calls). Hook becomes one `printf | nc -U` or a tiny `loom hook-write` binary. Daemon buffers in-memory; if it's down, hook still returns instantly.
- **Bet**: Network/HTTP transport overhead and daemon-process flakiness are the actual cost drivers, not the per-hook logic.
- **Risk**: Larger ops surface (socket lifecycle, permissions, daemon restart semantics); migrating existing hooks is intrusive.

### D. Declarative hook DSL → typed compilation
The generated notify command (`codexNotifyCommand` at `pkg/generator/configs_codex.go:337`) is a ~2000-character TOML-embedded shell blob mixing rate-limit logic, jq parsing, file I/O, nohup background, and a foreground CLI call. Nobody can read or test it as a unit. Replace with a typed Go spec — `HookSpec{Steps: []HookStep{Name,Stdin,Cmd,Bg,Timeout}}` — compiled to TOML/JSON at sync time. Each step is unit-testable; vendor schema constraints enforced in the compile step.
- **Bet**: The biggest operational risk isn't performance — it's that nobody can reason about or test what we're shipping in the shell blob.
- **Risk**: Another generator layer to maintain; on its own doesn't reduce notification churn or daemon load.

### E. Hooks-as-data: disk queue + separate flusher
Hooks should never call the loom daemon directly. Each hook becomes an atomic JSONL append to `~/.cache/loom/queue/<agent>-<hash>.jsonl`. A launchd-managed flusher daemon drains the queue into the daemon (or skips it when offline). The CLI hot path becomes one `echo >> file` per event. Zero daemon coupling, zero foreground latency, zero cascade.
- **Bet**: Decoupling event production from event consumption eliminates an entire class of cascade failure.
- **Risk**: Queue growth without flush daemon; ordering guarantees weaken; loses immediate-action hooks (anything that needs to feed back into the agent's prompt has to happen before the hook returns).

### F. OS-level lifecycle observer, drop turn-end hooks entirely
Codex/Claude/Gemini are OS-managed processes. A launchd/systemd unit can watch process spawn/exit and register sessions/heartbeats by observing the parent PID — no hook needed. Heartbeats become "the process is still in `ps`" rather than "the agent ran a shell command." Per-turn signals get reconstructed from MCP traffic at the proxy.
- **Bet**: We're smuggling lifecycle out of the agent via hooks because we don't think we have another channel — but the OS already has the truth.
- **Risk**: Platform-specific (macOS vs Linux); coarser semantics (no "turn end" without proxy-side reconstruction); harder to scope across machines.

### G. Health-aware push model: backends register, daemon never polls
Current model: daemon refreshes tool list from backends on TTL expiry (5min default), computes diff, fires `list_changed` if the set differs. TTL-driven polling fabricates "change" events from transient connection drops. Inverted: backends push tool-list changes via MCP `notifications/tools/list_changed` themselves; daemon mirrors. No background refresh; no spurious diffs.
- **Bet**: Polling is fundamentally incompatible with stable list semantics — push is the natural fit.
- **Risk**: Many backends don't push; need a poll fallback anyway; backend-side bugs (over-eager notification) leak directly to clients.

---

## Phase 2 — Cross-Pollinate

### Combination — C + E: buffered socket
Combine UNIX-socket sidecar (C) with disk-queue (E). Hook always writes to the socket; daemon-side handler buffers in memory and flushes to disk on overflow / on shutdown. If the daemon isn't accepting connections, the hook falls through to direct disk append. On (re)start the daemon drains disk → in-memory → consumers. Gets us both "no HTTP in hot path" and "daemon outage is invisible to hooks." API stays uniform; backpressure invisible to hook authors.

### Combination — B + G: stable list + push
B's policy ("transient backend health doesn't change the advertised list") is implemented by G's mechanism ("backends push their own list changes"). Together: the daemon never diffs from polling. Tool set changes only on registry edit or on an explicit backend-pushed change. Connection drops are health events, not list events. This is the cleanest single-source-of-truth design.

### Tension — A (surgical, ~1 week) vs C+E (structural, ~1 quarter)
A fixes the visible Codex GUI symptom and does nothing for daemon fragility or hook brittleness; just hides the bounce. C+E rebuilds hook→daemon transport so the same fragility never bites anywhere again, but it's a real lift. **Decision axis**: is the bouncing the actual bug, or is it the cheapest visible signal that the subsystem is structurally fragile?

### Tension — D (keep CLI hooks, make them legible) vs E+F (delete the CLI hot path)
D bets the CLI hook surface has long-term value and just needs better tooling. E+F bets the CLI hook surface is the wrong abstraction — too platform-coupled, too synchronous, too imperative — and we should route around it. You can't really do both: investing in DSL tooling for a surface you're deprecating is wasted.

---

## Phase 3 — Converge

### Recommended: B + A — stable manifest with health overlay, plus capability-negotiate Codex away from `list_changed`

B is the right architectural framing: the tool set should be a function of the registry, not of transient backend health. That single change collapses a class of "agent UI redraws because a backend hiccupped" bugs across every client, not just Codex GUI. It also simplifies the daemon — no diff computation on each refresh, no spurious `list_changed`. A piggybacks as the surgical compatibility patch: even with B, desktop GUIs over-react to MCP notifications, and there's no upside to sending notifications a client can't tolerate. The combination is small enough to ship in a week or two and hits the actual root-cause class, not just the visible symptom. Concretely:
- **B**: change `daemon_tools_cache.go` so the cache's "tool set" is registry-derived; backend unreachability sets a `degraded` flag on the tool entry rather than removing it; `tools/call` for a degraded tool returns a typed error immediately without forwarding. `toolNamesChanged` only triggers on registry-driven changes.
- **A**: in the daemon's `initialize` handler, vary capability advertisement by client-identity (`clientInfo.name`/`clientInfo.version` from the initialize params). Codex desktop GUI → `listChanged: false`. Claude/HUD/Gemini → unchanged.

### Runner-up: C + E — buffered socket sidecar
If the 502s/timeouts/broken-pipes in `loom-agent-hooks.log` represent ongoing operational pain — not a snapshot-in-time artifact — then the structural fix is to take the daemon out of the hook hot path entirely. C+E (always-write socket with disk fallback) gives every hook on every agent platform a uniform, fast, fail-safe transport. Bigger investment but it amortizes across Claude, Gemini, Codex, future agents. **Tip-over**: if you'd otherwise patch daemon connectivity bugs once a month, this pays back. If recent fragility traces to a transient k3s/k8s incident, the smaller B+A fix is enough.

### Open question
Is the bouncing reproducible with the current daemon state, and is `notifications/tools/list_changed` actually the message the Codex GUI is reacting to? Verification: instrument a single Codex GUI session, log every MCP message the loom proxy sends to it, and correlate `list_changed` events with the visual bounce. If confirmed, B+A is the path. If the trigger is something else (periodic proxy reconnects sending re-`initialize` traffic, or some `event-emit` write to a stream the GUI watches), the diagnosis re-routes — and we may end up at C or G instead.

---

## Lineage
- Source investigation: this session's prior conversation (Codex GUI bouncing, daemon fragility, hook surface review).
- Cited files: `pkg/generator/configs_codex.go`, `pkg/generator/configs_hooks.go`, `pkg/generator/platform_profiles.yaml`, `cmd/loom/proxy_handlers.go`, `internal/daemon/daemon_tools_cache.go`, `internal/daemon/daemon_transport.go`, `internal/daemon/daemon_toolcache.go`.
- Cited upstream: `openai/codex` repo, `codex-rs/hooks/src/legacy_notify.rs:56-64` (notify fire-and-forget), `codex-rs/hooks/src/events/session_start.rs` (SessionStartSource variants).
- Next handoff if pursued: `plan-loom-core` for B+A spec; `research` for the verification step in the Open question.
