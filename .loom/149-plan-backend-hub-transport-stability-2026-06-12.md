# Plan: Backend Hub-Transport Stability (HUD warnings/delays + agent tool-call failures)

**Date**: 2026-06-12
**Status**: Slice 0 executed + kill-test passed 2026-06-12; Slices 1–5 planned
**Symptoms addressed**: HUD frontend shows stale-data warnings and delayed panels; agents intermittently fail tool calls (`daemon error (-32603)`) and context-management operations (`agent_session_list`, `agent_context_search`, `agent_presence_list`, heartbeats).

## Riskiest assumption + kill-test

**Load-bearing assumption**: The chronic `muxstdio: transport closed` storm is specific to the hub-routed path (`--hub-prefer` over `wss://mcp.flexinfer.ai/ws`), and pinning `agent_context` to `local-only` in the local daemon's `routing.preferences` eliminates ≥90% of the failures without breaking anything the local HUD/agents depend on (local agents and the embedded HUD already read/write the *local* agent-context store; hub-side agent_context serves cluster agents).

**Kill test** (≤30 min): Add to `~/.config/loom/config.yaml`:
```yaml
routing:
  preferences:
    agent_context: "local-only"
```
then `launchctl kickstart -k gui/$UID/com.loom.daemon`. Compare 30-minute windows of
`grep -c 'transport closed' ~/.config/loom/logs/daemon.err` before/after, filtered by `server=agent_context`. Pass = agent_context transport-closed count drops to ~0, HUD fleet panel stays fresh (no stale banner), and `agent_session_list` via the proxy returns without error.

**Failure mode if wrong**: If failures persist with local-only routing, the wedge is in the local muxstdio/pool layer (not hub), and Slices 2–3 are aimed at the wrong layer — re-run root-cause capture (Slice 1) before any transport changes.

**Status**: PASSED 2026-06-12 (initial window). Applied `agent_context: local-only` + `gitlab: prefer-local` to `~/.config/loom/config.yaml`, kickstarted daemon 10:03:12. Evidence:
- Before: 665 `transport closed` in 30 min (468 agent_context), ~32s burst sawtooth.
- After (10:03:30 → 10:06:00): **0** transport-closed events of any server (old rate predicts ~55; ~5 sawtooth bursts missed zero times).
- `agent_context__agent_session_list` via proxy: 47ms wall, ok. `/api/health`: agent_context `target=local`, healthy, avgLatency 15ms.
- Routing precedence confirmed in code: explicit config prefs win over legacy `--hub-prefer` conversion (`internal/daemon/daemon_new.go:297-303`).
Re-check the 24h window before closing Slice 0 (the storm historically ramps with fleet load mid-morning).

## Evidence (all verified 2026-06-12)

### Scale and shape of the failure
- ~800–1,340 `transport closed` log lines/hour during active hours on 2026-06-11, resuming 2026-06-12 (1,163 in hour 09). Command: `grep 'transport closed' ~/.config/loom/logs/daemon.err | grep -oE '^time=2026-06-[0-9]{2}T[0-9]{2}' | sort | uniq -c`.
- Per-server breakdown (last 2,000 events): `agent_context` 1,410, `gitlab` 173, long tail ~10 each across all hub-capable servers.
- agent_context failures arrive in bursts every **~32s** (e.g. 09:50:06 → 09:50:38 → 09:51:10 → 09:51:45), each burst failing 8–10 in-flight calls. The 32s period = the 30s prefer-hub backoff (`preferHubBackoffDuration`, `internal/daemon/routing.go:12`) + retry latency: backoff expires → next call re-dials hub → connection dies → all concurrent monitor/agent calls fail at once → backoff re-arms.
- Idle servers (e.g. `time`) fail in pairs on first call after an idle gap — dead cached connection discovered at call time.
- Session auto-prune is healthy and NOT the cause this time (launchd `com.loom.session-prune`, 6h cadence, pruning 68–367/run; one rc=137 on 2026-06-10 16:05).
- Hub-side: all `loom-hub` pods Running (mass restart 13h ago on k3s-w-5, stable since). Gateway logs show our redial cycle (`Client proxy connected: agent_context` 2–4x every ~32s) with no close-reason logging.

### Architecture facts (file:line)
1. **Local daemon runs `--hub-prefer`** (`~/Library/LaunchAgents/com.loom.daemon.plist` ProgramArguments) against `wss://mcp.flexinfer.ai/ws` (Cloudflare-fronted; `~/.config/loom/config.yaml` hub block).
2. **One shared WebSocket per server name**, demuxed via muxstdio: `internal/daemon/daemon_new.go:249-266` — `hubClient.Dial` → `GetConnection` returns the same cached `*WebSocketTransport` for all 25 pool conns. When it closes, every in-flight call fails simultaneously (`muxstdio` readLoop closes all pending channels).
3. **No WS keepalive**: `libs/mcp-go/websocket.go:181` defines `Ping()` but **nothing calls it** (verified: zero callers in libs/mcp-go and loom-core). No read deadline, no ping loop.
4. **No liveness gating on reuse**: `libs/mcp-go/websocket.go:203-215` — `GetConnection` returns the cached conn if `IsInitialized()` (a flag, not a probe). A remotely-closed conn is handed out until a send/recv fails.
5. **No health gating on reconnect**: `internal/daemon/callpipeline_routing.go:319-360` (`retryHubAfterHubFailure`) re-dials and immediately re-executes; 25 concurrent callers thundering-herd the same endpoint.
6. **Flat 30s backoff, prefer-hub only**: `internal/daemon/routing.go:114-124` + `callpipeline_stages.go:243-248`. No exponential growth; doesn't apply to hub-only/hub-delegate routing.
7. **Health-monitor false positives**: `internal/daemon/health.go:188-321` deep probe spawns a fresh subprocess with a **10s** timeout (`daemon_tools_fetch.go:27`); under retry-storm load, subprocess start exceeds 10s → `start: context deadline exceeded` → unhealthy → restart of a healthy local server (observed for `gitlab`, which then makes the *local fallback* path slow too).
8. **Hub spawns a full server instance per WS connection**: `kubectl logs deploy/agent-context -n loom-hub` shows `starting server` + `restored active sessions` + `starting background services` (compaction, task/worktree reconcilers, session reaper) **every ~1s** under the redial storm. N connections = N competing background-service instances against shared state.
9. **Frontend warning path**: monitors carry over the previous snapshot on partial failure (`internal/hud/monitor/fleet.go:692-715`), so failures are invisible until `lastUpdated` exceeds `staleAfter=90s` (`frontend/src/lib/stores/fleet.svelte.ts:229`), at which point the stale banner appears — that is the user-visible "warning/delay".
10. **Agent error path**: agents get `daemon error (-32603): tools/call failed during send: muxstdio: transport closed` with one transport-level retry (`internal/hud/bridge/daemon.go:227-243`); no tool-level retry — that is the user-visible "failed tool calls / context management".
11. **Routing preferences are config-supported**: `internal/daemon/config.go:476-480` (`routing.preferences`, values `local-only|hub-only|prefer-local|prefer-hub|health-based`).

### Why hub-prefer existed
Added 2026-05-25 for fleet-wide shared context (plist backup `com.loom.daemon.plist.bak.preHubPrefer...`). Known amplifier: any hub transport stall surfaces as cross-server "flaky MCP" drops (memory: `project_hud_no_agents_session_list_timeout`, RECURRED 2026-06-03 entry). Reverting hub-prefer is the documented escape hatch; this plan makes the surgical version of that revert durable and fixes the transport underneath.

## Slices

### Slice 0 — Surgical mitigation (config-only, today)
Pin the hot, latency-sensitive servers local on the local daemon:
```yaml
routing:
  preferences:
    agent_context: "local-only"
    gitlab: "prefer-local"
```
Restart daemon; run the kill-test above. **This is the riskiest-assumption kill-test.** Document before/after counts here. Tradeoff to verify during the window: nothing on this Mac actually needs hub-side agent_context (cluster agents have their own daemons; mobile-hud runs in-cluster).

### Slice 1 — Close-reason observability (small code, loom-core + libs/mcp-go)
We still don't know **who** closes the WS (Cloudflare idle/limits, gateway, per-conn process exit, read-limit). Add:
- muxstdio readLoop: log the underlying `inner.Recv()` error (close code/reason), not just "transport closed".
- `WebSocketTransport`: log close code from gorilla `CloseError`.
- Gateway: log client/upstream disconnects with reason.
Capture one full ~32s cycle with the logging in place; record the verdict in this doc. Gate Slice 2's design on it.

### Slice 2 — Transport durability (libs/mcp-go, gated on Slice 1)
- Keepalive: background ping loop per cached connection (the unused `Ping()` at `websocket.go:181`), close-and-evict on pong timeout so dead conns are discovered proactively, not at call time.
- Liveness gating: `GetConnection` pings (or checks last-traffic age) before returning a cached conn idle > ~30s.
- Singleflight reconnect per server name to kill the thundering herd (`retryHubAfterHubFailure` currently lets all 25 pool conns re-dial concurrently).
- Exponential backoff (30s → 60s → 120s, cap ~5min) replacing flat `preferHubBackoffDuration`, applied to hub-only/hub-delegate as well.
- Audit gorilla read limits vs. large `agent_session_list`/TOON payloads.
Regression tests in libs/mcp-go (fake closing server) + daemon-level test that a hub close fails ≤1 call per pool, not all.

### Slice 3 — Hub-side: stop spawning a server per connection (loom-core hub deployment)
Per-WS-connection `mcp-agent-context` instances each start compaction/reaper/reconciler background services — N redials = N reapers racing on shared Qdrant state. Either:
- (a) single long-lived upstream process, gateway multiplexes WS clients onto it (preferred), or
- (b) per-conn instances start with background services disabled (env gate), only a designated singleton runs them.
Even with (b) the spawn cost (~1s session restore per conn) goes away under steady state once Slice 2 stops the churn.

### Slice 4 — Health-monitor hardening (internal/daemon)
- [x] Make the 10s deep-probe subprocess-start timeout configurable; default 30s (matches `defaultDaemonControlRPCTimeout`). — Slice 4.1, `.loom/152`, branch `feat/health-deep-probe-timeout` (`HealthConfig.DeepProbeTimeoutSeconds` → `HealthMonitorConfig.DeepProbeTimeout`; `fetchServerToolsWithTimeout`; `checkAllServers` budget widened).
- [x] Don't count hub-stage transport failures toward local-server restart decisions (the gitlab restarts were collateral). — **Architecturally already satisfied** (no code change): the health monitor only probes *local* processes (`fetchServerToolsViaPool` → `d.pool`=`connPool`→`procMgr.Dial` local stdio; deep probe spawns a dedicated local subprocess). The hub transport lives in a separate `hubPool`/`hubClient` the monitor never touches, so a hub `transport closed` never reaches the restart counter. The `gitlab` collateral restarts came from slow local subprocess *start* under storm load, which Slice 4.1 fixed. See `.loom/154` for the full code trace. — Slice 4.2.
- [x] Add restart hysteresis: skip auto-restart when system-wide retry pressure is high. — Slice 4.3, `.loom/154`, branch `feat/health-restart-hysteresis`. Signal = count of distinct servers whose last probe failed within `restartPressureWindow` (default 60s); when ≥ `restartPressureThreshold` (default 3) the auto-restart is suppressed (server stays marked unhealthy; next sweep re-evaluates). Config: `health.restart_pressure_threshold` / `health.restart_pressure_window_seconds` (negative threshold disables). Measured entirely inside the monitor — no cross-module coupling.

### Slice 5 — Honest degraded-state surfacing (HUD, lower priority)
- [x] **5a (backend)**: explicit `Degraded`/`DegradedReason`/`DegradedSince` fields on `FleetSnapshot`, populated in `refresh()`'s partial-failure branch with onset-time hysteresis; flows to the API/SSE payload. `.loom/155`, branch `feat/hud-degraded-state-surfacing`. The carry-over is no longer silent — the degraded state is machine-readable in the snapshot.
- [ ] **5b (frontend, follow-up)**: `fleet.svelte.ts`/`ConnectionBanner` consume `degraded` to render "degraded since HH:MM (sessions fetch failing)" instead of the generic stale pill; surface circuit-open state. Needs the `go:embed`'d HUD dist rebuild (`make hud-frontend` + commit; CI does not rebuild it).

## Sequencing & gates
1. Slice 0 now (config + restart + kill-test) — restores agent/HUD reliability immediately.
2. Slice 1 next (observability) — cheap, unblocks correct Slice 2 design.
3. Slice 2 and 4 in parallel branches (different modules; libs/mcp-go change needs a loom-core go.mod bump + redeploy per `project_hud_toon_dashless_decode` precedent).
4. Slice 3 after Slice 1 confirms/denies per-conn process churn as a close cause (it may BE the closer: wrapper killing conns under resource pressure).
5. Slice 5 opportunistic.

## Sources
- `~/.config/loom/logs/daemon.err` (commands inline above)
- `~/Library/LaunchAgents/com.loom.daemon.plist` (`plutil -p`)
- `~/.config/loom/config.yaml` hub block
- `internal/daemon/callpipeline_routing.go:319-404`, `callpipeline_errors.go:21-125`, `callpipeline_stages.go:200-460`, `routing.go:12,83-124`, `health.go:154-394`, `daemon_tools_fetch.go:15-94`, `daemon_new.go:249-266`, `config.go:476-480`
- `libs/mcp-go/websocket.go:129-262`
- `internal/hud/monitor/fleet.go:458-715`, `internal/hud/bridge/daemon.go:195-316`, `internal/hud/bridge/agent_session.go:441-580`, `frontend/src/lib/stores/staleness.svelte.ts`, `fleet.svelte.ts:214-236`
- `kubectl get pods -n loom-hub`; `kubectl logs deploy/loom-gateway -n loom-hub --since=2h`; `kubectl logs deploy/agent-context -n loom-hub --since=30m`
- Memory: `project_hud_no_agents_session_list_timeout` (hub-prefer escape hatch, 2026-06-03 recurrence)
