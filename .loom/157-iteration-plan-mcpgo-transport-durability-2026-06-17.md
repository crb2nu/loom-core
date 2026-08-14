# Iteration Plan: libs/mcp-go transport durability (Slice 2 of .loom/149)

**Date**: 2026-06-17
**Branch (mcp-go)**: `feat/ws-keepalive-liveness-singleflight`
**Parent plan**: `.loom/149-plan-backend-hub-transport-stability-2026-06-12.md` (Slice 2)
**Repos touched**: `libs/mcp-go` (primary), `services/loom-core` (go.mod bump + redeploy to consume)

## Why now — Slice 1 verdict is in (the gate)

Slice 1 (`.loom/150`, merged 8b07338b) added close-cause logging. The captured verdict
across 29 WARN samples in `~/.config/loom/logs/daemon.err`:

- **Dominant cause: `read message: websocket: close 1006 (abnormal closure): unexpected EOF`**
  (17 of 29; remaining 12 are downstream generic `transport closed`).
  Close code **1006 = no close frame received** — the TCP/WS was severed without a graceful
  close handshake. Consistent with Cloudflare idle-timeout or upstream per-conn process exit,
  **not** an application-level close.
- **Idle conns die silently**: the 10:37–10:38 burst on 2026-06-16 shows 1006 closures with
  `pending_failed=0` — connections dying while idle, discovered only at next call.
- **Shared-WS thundering herd CONFIRMED**: 2026-06-16 10:33:15–16, a single hub WS death
  failed `prompts/list` across **5 distinct servers** (`cloudflare`, `ops_mcp`, `server_mgmt`,
  `git`, `qdrant`) within ~1s, every line carrying the identical 1006 cause
  (`...muxstdio: transport closed (cause: ...close 1006...)`), each followed by a separate
  `prefer-hub override ... backoff_until=+30s`. One dead connection → N concurrent failures →
  N independent 30s backoffs. This is exactly the failure shape Slice 2 targets.

Verdict → **Slice 2's design direction is correct and now evidence-backed**: keepalive to
prevent/early-detect idle 1006 death, liveness gating so a dead conn is never handed out, and
singleflight reconnect so one death fails ≤1 caller, not the whole pool.

## Riskiest assumption + kill-test

**Load-bearing assumption**: The 1006 abnormal closures are driven by **idle / low-traffic
connection reaping** (Cloudflare or gateway dropping a connection with no recent frames), so a
background WS keepalive ping (≤25s interval, under typical 60–100s idle windows) will keep the
shared connections alive and drive the idle-1006 rate to ~0; for closures that still happen,
liveness gating + singleflight reconnect collapse the blast radius to ≤1 failed call per event.

**Kill test** (≤30 min, two tiers):
1. **Unit/integration (in libs/mcp-go, runnable immediately)** — a fake WS server that (a) drops
   any connection idle >2s without a close frame, and (b) records concurrent dial count. Assert:
   - with the keepalive ping loop enabled, a connection idle 10s survives (0 reconnects);
   - when the server force-closes mid-flight, `N=25` concurrent callers trigger **exactly 1**
     redial (singleflight), and ≤1 in-flight call surfaces an error;
   - `GetConnection` on a conn whose last-traffic age > threshold probes/evicts before returning.
   Command: `go test ./... -race -run 'Keepalive|Liveness|Singleflight'` in `libs/mcp-go`.
2. **Live confirmation (after loom-core go.mod bump + local daemon rebuild)** — bump the mcp-go
   pseudo-version, `make build`, `launchctl kickstart -k gui/$UID/com.loom.daemon`, then compare
   30-min windows of `grep -c '1006 (abnormal' ~/.config/loom/logs/daemon.err` before/after on
   the still-hub-routed servers (cloudflare/ops_mcp/server_mgmt/git/qdrant — these are NOT pinned
   local by Slice 0). Pass = idle-1006 rate drops to ~0 and any residual close fails ≤1 call (no
   5-server simultaneous-failure bursts).

**Failure mode if wrong**: If 1006 persists at the same rate *with* keepalive, the closer is a
**forced periodic kill** (hard max-connection-age at Cloudflare/gateway, or the per-WS hub
process exiting — Slice 3's territory), not idle reaping. Keepalive then only *detects* faster;
it doesn't prevent. In that case the value shifts entirely to liveness-gating + singleflight
(blast-radius reduction) and **Slice 3 (hub stops spawning a process per connection) becomes the
real fix** — re-prioritize before investing further in mcp-go keepalive tuning.

**Status**: tier-1 **PASSED 2026-06-17** (mcp-go `!7`, merged → `origin/main` `18b68e7`,
pipeline 14207 green). `go test -race ./` green incl. the new singleflight/keepalive/liveness
tests and origin's `TestWebSocketClient_DialReturnsFreshTransportPerCall`.

**Tier-2 live: RAN 2026-06-18 — assumption DISPROVEN, failure-mode branch triggered.**
Bumped loom-core (`!724` → `main` `58fc1d2a`), rebuilt local daemon (`make install-core`),
restarted (`launchctl kickstart`, PID 43583 on the keepalive binary). Two windows from
`~/.config/loom/logs/daemon.err`:
- Window-1 (02:02–02:17, fresh conns): 3 close-1006, 0 backoff arms — but this was
  **restart-freshness**; the periodic kill cycle had not resumed yet.
- Window-2 (02:18–03:16, ~57min, aged conns): **60 close-1006, 71 hub-transport failures +
  71 prefer-hub fallbacks (exp backoff arming), 16 agent-visible -32603 over 57min**, BUT
  **0 concurrent-batch failures** (`pending_failed>0` = 0).
- **Smoking gun**: hub-transport failures arrive in **synchronized bursts of ~17 servers at
  once, every ~8 min** (02:49 / 02:57 / 03:05 / 03:15). That is a **periodic forced kill of
  all connections**, not staggered idle reaping — a 22s keepalive cannot prevent it (it only
  detects: 11 app-level `hub keepalive: send failed` clears).

**Verdict**: the load-bearing "idle reaping dominates" assumption is **wrong** for the residual.
Slice 2 still delivered real value — the thundering herd is gone (singleflight: 0 concurrent
pending_failed batches, was 8–10/burst), exponential backoff arms correctly, and raw closures
dropped ~78% (74/15min → ~16/15min) — but the dominant residual closer is **hub-side forced
periodic kills**. Per the failure-mode branch, **Slice 3 (hub stops spawning a server per WS
connection; or fix max-connection-age / pod cycling) is now the priority lever**, not more
keepalive tuning. Next scoping step: confirm the ~8-min cadence source on the hub
(`kubectl logs -n loom-hub` around a burst boundary: Cloudflare max-age vs loom-hub pod/process
cycle vs gateway reaper).

**Mid-flight design correction**: while rebasing onto current `origin/main` I found the
`fix/mcp-core-compat` merge had already decoupled `Dial` to return a **fresh transport per call**
(pool callers keep independent sockets; sharing one caused close-aliasing). This *changes* the
`.loom/149` "one shared WS per server" premise — the pool no longer shares a socket. The three
levers still apply and compose: keepalive now runs on **every** independent pooled transport
(via `NewWebSocketTransport`), so the close-1006 fix covers the pool path; liveness gating +
singleflight cover `GetConnection`'s cached path. Kept origin's fresh `Dial` + its test.

## Scope

**In (`libs/mcp-go/websocket.go`)**:
1. **Keepalive ping loop** — per cached `WebSocketTransport`, a background goroutine calling the
   existing `Ping()` (line 181, currently **zero non-test callers**) every ~20–25s; on
   `WriteControl`/pong failure, close-and-evict from the client map so the dead conn is gone
   before the next `GetConnection`. Stop the loop on `Close()`.
2. **Liveness gating in `GetConnection`** (line 205) — track `lastTraffic time.Time` on the
   transport (updated on successful Send/Recv); when returning a cached conn whose idle age
   exceeds a threshold (~30s), probe via `Ping()` first and reconnect on failure instead of
   handing out a possibly-dead conn on the `IsInitialized()` flag alone.
3. **Singleflight reconnect per server name** — wrap `Reconnect`/the GetConnection recreate path
   in `golang.org/x/sync/singleflight` keyed by `serverName`, so N concurrent callers that hit a
   dead conn share **one** redial. (Today each caller independently `Close()`+recreates.)
4. **Read-limit / deadline audit** — confirm gorilla `SetReadLimit`/`SetReadDeadline` aren't
   silently truncating large `agent_session_list`/TOON payloads and being misread as closures.
   Set a read deadline coupled to the keepalive so a stalled read is detected, not hung.

**In (`services/loom-core`)**: `go get gitlab.flexinfer.ai/libs/mcp-go@<new-commit>` to bump the
pin (currently `v0.2.1-0.20260606235418-cf46de19e009`), `go mod tidy`, rebuild, redeploy.
No `replace`/`go.work` in use — the pseudo-version bump is the only wiring.

**Out (explicitly deferred)**:
- **Exponential backoff (30s→60s→120s, cap 5m) for the daemon's prefer-hub override** — this
  lives in `services/loom-core` (`internal/daemon/routing.go:12,114-124`,
  `callpipeline_stages.go:243-248`), **not** mcp-go. Split into a sibling loom-core-only slice
  (call it **Slice 2b**) so the mcp-go MR stays self-contained and independently testable. The
  flat 30s backoff is visible in the 10:33 burst (`backoff_until=+30s`); singleflight (Slice 2)
  reduces how often it's armed, exponential backoff (2b) reduces churn when it is.
- **Slice 3** (hub stops spawning a server instance per WS connection) — cluster/deploy.
- Two **adjacent, non-transport findings** surfaced while reading the logs — recorded here so
  they aren't conflated into Slice 2:
  - `morph API HTTP 522` (Cloudflare can't reach `embed-v4.morphllm.com`) breaking
    `agent_context_search` embedding queries — an upstream embedding-origin outage, separate
    incident.
  - `local server recv timed out after 900ms` on `agent_context`/`agent_recall` — a tight local
    recv timeout, unrelated to the hub WS. Candidate for its own small slice if it recurs.

## Acceptance criteria
1. `Ping()` is now driven by a keepalive loop; a connection idle past the Cloudflare/gateway
   idle window survives (proven by tier-1 fake-server test).
2. A single mid-flight WS close fails **≤1** in-flight call and triggers **exactly 1** redial
   regardless of concurrent caller count (singleflight test).
3. `GetConnection` never returns a conn that fails a liveness probe.
4. `go test ./... -race` green in libs/mcp-go; existing transport/SSE suites unaffected.
5. After the loom-core bump + redeploy, tier-2 live window shows idle-1006 rate ~0 and no
   multi-server simultaneous-failure bursts (or, if not, the failure-mode branch above is
   triggered and Slice 3 is re-prioritized — recorded in `.loom/149`).

## Risks
- Keepalive interval too aggressive → wasted frames / log noise; too slow → misses the idle
  window. Start ~22s (under a 60s+ Cloudflare idle assumption); make it a `WebSocketConfig` field
  so the live window can tune it without a recompile-and-guess loop.
- `WriteControl` ping contends with concurrent writes — `Ping()` already takes `t.mu`; verify the
  Send path holds the same mutex so a ping can't interleave a partial frame.
- Cross-repo lag: mcp-go MR must merge and its CI must publish the module before the loom-core
  `go get` resolves the new pseudo-version (precedent: `project_hud_toon_dashless_decode`,
  `reference_go_build_from_worktree`).

## Sequencing
1. [x] mcp-go branch: implement (1)(2)(3)(4) + tier-1 kill-test → MR → merge. **DONE** — mcp-go `!7`, `origin/main` `18b68e7`.
2. [ ] loom-core: `go get gitlab.flexinfer.ai/libs/mcp-go@v0.2.1-0.20260617231926-18b68e7e68f6` (or `@main`) → `go mod tidy` → `make build` → redeploy (image bump per `project_hud_toon_dashless_decode`) → tier-2 live window → record verdict in `.loom/149`. **Deploy step — awaiting go-ahead.**
3. [ ] Slice 2b (loom-core exponential backoff in `internal/daemon/routing.go`) — independent, can land in parallel.
4. [ ] Slice 3 — only if tier-2 shows forced-kill (failure-mode branch), else opportunistic.

## Sources
- Close-cause evidence: `~/.config/loom/logs/daemon.err`
  (`grep 'inner transport closed' ... | grep -oE 'error="[^"]*"' | sort | uniq -c`;
  the 2026-06-16 10:33:15–16 5-server burst).
- `libs/mcp-go/websocket.go:175-300` (Ping, GetConnection, Reconnect, retry loop).
- `services/loom-core/go.mod:41` (mcp-go pin).
- Parent plan `.loom/149` Slices 1–3; Slice 1 plan `.loom/150`.
- Memory: `project_hub_ws_transport_storm`, `project_hud_toon_dashless_decode`,
  `reference_go_build_from_worktree`.
