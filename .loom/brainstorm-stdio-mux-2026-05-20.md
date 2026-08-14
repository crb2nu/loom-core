# Brainstorm — Per-request-ID multiplexing for shared stdio transports

- **Date**: 2026-05-20
- **Author**: claude-code (main, post-!456)
- **Trigger**: MR !456 (`fix(daemon): skip per-server callLock for hub-routed calls`) merged
  to main 2026-05-19. Hub-routed calls now bypass the per-server mutex (each pool Conn owns
  its own `websocket.Conn`). Local stdio still serializes via the call lock — pool
  `maxOpen=25` gives 25 logical handles to ONE process / ONE stdin/stdout pipe / ONE
  `msgCh`. The lock was originally added (commit `6785663d`, reverted-then-restored
  `a6bb44e2`/`6785663d`) to stop ID-interleaved Recvs from corrupting the wire under
  concurrent callers.
- **Predecessor**: [callpipeline_routing.go:229-269](../internal/daemon/callpipeline_routing.go)
  documents the lock semantics post-!456.
- **Goal of this doc**: surface multiple framings for unblocking local-stdio parallelism, find
  combinations and tensions, converge on ONE recommendation for the spec.

## Problem

After !456, the daemon's local-stdio call path looks like:

```
caller A ──► acquireCallLock("foo") ──► pool.Get("foo") ──► tx.Send(req) ──► tx.Recv()
caller B ──► acquireCallLock("foo") ──blocks until A releases──►
caller C ──► acquireCallLock("foo") ──blocks until A releases──►
```

`tx` is the SAME `*mcp.StdioTransport` for every caller of `"foo"` because
`kitprocess.Manager.Dial` returns the same `*Process` per `serverName`. Pool size 25
buys nothing — 25 logical handles, one wire.

The lock acquire has a 15s budget (`acquireCallLock` ctx) and tool-RPC timeout is 30s.
Under heartbeat load (`agent_context_*`, presence, stream monitors) one slow Recv against
a 30s tool can deadline-exceed every other concurrent caller through the same per-server
mutex — exactly the cascade !456 fixed for hub.

Upstream `mcp-go.Server` already handles requests concurrently
([server.go:240-256](file:///Users/cblevins/go/pkg/mod/gitlab.flexinfer.ai/libs/mcp-go@v0.2.1-0.20260520023524-aa57e61f11bd/server.go)):
each request gets its own goroutine (default `concurrencyLimit != 1`). The server-side
parallelism exists — the wire is the bottleneck.

**Where could the parallelism unlock land, and what's the smallest move that proves it
without burning a week on the wrong abstraction?**

## Phase 1 — Diverge (8 framings)

### F1. Per-id demux inside `StdioTransport` (upstream change)
Replace `msgCh chan recvResult` with `pending map[id]chan recvResult` + sync.Map. Add a
`RecvByID(ctx, id)` method. Existing `Recv(ctx)` stays for servers that don't have a
pending id (notifications, server-pushed messages).
- **Bet**: The cleanest place to demux is where the parsing happens. One upstream MR
  in `libs/mcp-go` unblocks every consumer (loom-core, fi-mcp-kit, future projects).
- **Risk**: Cross-repo coordination cost. Need a libs/mcp-go release + version bump
  here. Two repos, two MRs, gated kill-test.

### F2. Demuxing wrapper in loom-core (`pkg/transport/muxstdio`)
Keep upstream `StdioTransport` unchanged. Wrap it: one reader goroutine drains
`upstream.Recv()`, dispatches by parsed `Message.ID` to a per-id chan. Pool `Conn`
becomes a thin handle that registers a pending id, sends via the wrapped transport,
waits on its own chan.
- **Bet**: Localizes the change to one repo, ships in one MR. Can later be upstreamed
  once the API shape is proven.
- **Risk**: Wrapper has to re-parse `Message.ID` (already parsed once upstream).
  Two layers of Recv goroutines. Slightly less elegant; not architecturally where it
  belongs.

### F3. Process-per-caller (drop process reuse)
Make `kitprocess.Manager.Dial` return a NEW `*Process` per caller (no sharing). Each
pool Conn becomes a real OS process with its own stdin/stdout. No demux needed —
caller A and caller B literally do not share a wire.
- **Bet**: Conceptually simplest. Matches what `pool.maxOpen=25` already implies to a
  reader.
- **Risk**: Stdio servers cost real money to spawn — `mcp-agent-context` boot is
  ~150 ms; multiplied across 25 concurrent agents and 30+ servers, you spawn 750
  processes. Most stdio servers don't share session state across processes (DB
  handles, in-memory caches), so user-visible semantics CHANGE.

### F4. Migrate hot local servers off stdio
Run `mcp-agent-context`, `mcp-codebase-memory` (the heartbeat-heavy ones) as long-lived
TCP/HTTP servers on localhost. Each pool Conn dials a fresh TCP connection. Same fix
as hub — independent transports — but without the protocol cost.
- **Bet**: Avoids touching the transport entirely. Reuses the just-shipped hub
  parallelism by making "hub-ish" a thing for local servers too.
- **Risk**: Need a new server lifecycle (systemd-launched? daemon-supervised?) for
  every "hot" server. Operator complexity creep. Doesn't fix the long tail of cold
  stdio servers.

### F5. Bound the lock, don't remove it
Keep the lock but reduce its hold time. Currently we hold it across `Send + Recv`.
What if we hold it only for `Send` and use a per-id channel inside the transport for
Recv dispatch? Same demux as F1/F2 but framed as "shrink the existing lock".
- **Bet**: Smallest semantic change — the lock keeps doing the Send-side serialization
  that `writeMu` already does inside `StdioTransport`. Recv-side becomes parallel via
  demux.
- **Risk**: `StdioTransport.Send` ALREADY has `writeMu`
  ([transport.go:51](file:///Users/cblevins/go/pkg/mod/gitlab.flexinfer.ai/libs/mcp-go@v0.2.1-0.20260520023524-aa57e61f11bd/transport.go)).
  Our outer lock is redundant on the Send side and harmful on the Recv side. F5
  collapses to F1/F2 once you notice that.

### F6. Move the multiplexing out of the transport entirely (pool-level dispatcher)
Add a per-server "Dispatcher" goroutine. `Conn.Call(req)` puts the req on the
dispatcher's inbox; dispatcher serializes Send to the wire, demuxes Recv by id,
fans out to per-call channels. Pool `Conn` becomes a logical handle, not a thread.
- **Bet**: Decouples "transport" (Send/Recv on a wire) from "RPC" (request → response
  with id-tracking). Better separation of concerns. The Dispatcher pattern is also
  what we'd want for streaming responses and server-side notifications.
- **Risk**: Biggest refactor of the four (F1/F2/F6/F8). Touches `internal/pool`,
  `internal/daemon/callpipeline_*`, plus every consumer of `pool.Conn`.

### F7. Defer — observability first, fix only if metric proves it
The cascade !456 fixed was real. But how often does local-stdio lock contention
happen in steady state? We don't have a panel for it. Land observability (Grafana
panel + alert on `CallLockWaitTotal{server=...}`), watch for a week, fix only if the
metric crosses a threshold.
- **Bet**: Don't pay refactor cost until prod tells you to. The just-landed hub fix
  may have already reduced the local-side cascade signal because hub callers no
  longer hold the lock that local callers were contending against.
- **Risk**: Local heartbeat callers (presence, agent_context) DO converge on the same
  small set of servers (mcp-agent-context, mcp-codebase-memory) — the metric will
  almost certainly cross threshold. F7 is a stalling tactic dressed as caution.

### F8. Multiplex inside `kitprocess.Manager`, not the transport
Manager already owns the `*Process` lifecycle. Give Manager a per-id pending map and
a goroutine that reads from `Process.Stdout`, parses the id, and routes. Manager
returns a "logical Conn" (a Sender/Recver pair scoped to one id) per `Dial`. The
transport object disappears as a separate concept.
- **Bet**: Matches the lifecycle reality (Manager already de-dupes processes). Pool
  semantics become "give me N logical conns to process X" instead of "give me N
  handles to process X's transport".
- **Risk**: kitprocess is a more invasive surface to touch than transport. The
  pool/router/daemon layers all already accept `pool.Conn`-wrapped `*Transport`.

---

## Phase 2 — Cross-pollinate

### Combination A: F1 + F2 = "Local first, then upstream the proven shape"
Ship F2 (loom-core wrapper) first; once the API shape is proven against real prod
servers, propose the SAME shape upstream as F1 and migrate. Pays the upstream
coordination cost ONCE, after we know the API works.

### Combination B: F1 + F5 = "Inside-out reframe of the same change"
F1 is "add per-id demux"; F5 is "remove the redundant outer lock". Same change,
two angles. Spec should articulate BOTH so reviewers see the equivalence.

### Combination C: F6 + F4 = "Dispatcher pattern, with hot servers on TCP"
The Dispatcher abstraction (F6) makes the transport substitutable. Once it's in,
running mcp-agent-context as TCP (F4) is a config change, not a refactor. F6 unlocks
F4 without committing to F4 in this slice.

### Combination D: F3 — discard
Process-per-caller fails on stdio semantics (per-process state) AND startup cost.
Not a real option for shared servers. Note for the spec.

### Tension: Where does demux belong?
F1 (upstream transport), F2 (loom-core wrapper), F6 (pool dispatcher), F8 (kitprocess).
All four can host the demux logic. Architectural argument is F1 (transport owns wire
parsing). Pragmatic argument is F2 (one repo, one MR, can upstream later). Both are
defensible.

### Tension: Speed vs verification
The widget-rendering breakdown
([brainstorm-widget-rendering-breakdown-2026-05-17.md](brainstorm-widget-rendering-breakdown-2026-05-17.md))
burned 9 commits / 6,400 lines because slice 1 didn't kill-test the riskiest assumption.
Whatever we pick here MUST have a slice-1 kill-test against a real local stdio server,
not just unit tests against a mock transport. The 2026-05-12 attempt to remove the
lock (commit `a6bb44e2`) was reverted because its unit test used a DialFunc that
returned a fresh Transport per call — passing unit, failing prod.

---

## Phase 3 — Converge

### Recommended path: Combination A (F2 first, then F1)

**Why F2 over F1 as slice 1**: ships in one repo, one MR. Validates the per-id-demux
API shape against real local stdio servers in prod before committing to a libs/mcp-go
release. If the API shape is wrong (e.g. needs server-id-aware routing, or a different
notification fan-out model), we change it in loom-core without an upstream version
bump. Once stable, F1 upstreams the proven shape.

**Why F2 over F6 (Dispatcher)**: smaller surface. F6 is the architecturally cleaner
endpoint but it's a multi-package refactor. F2 contains the change to one new package
(`pkg/transport/muxstdio`). F6 can land later if the Dispatcher pattern earns its
keep on streaming or notification cases.

**Why F2 over F8 (kitprocess mux)**: kitprocess is shared between many call sites
(daemon, custom-server, mcp-hub-wrapper). Adding mux there increases blast radius.

**Discard**: F3 (process-per-caller, breaks semantics), F4 (TCP migration, separate
project), F7 (observability-only deferral, ducks the work).

### Riskiest assumption surfaced by this convergence

> Wrapping `mcp.StdioTransport` in a per-id demuxing layer (`pkg/transport/muxstdio`)
> is sufficient to safely allow N concurrent Send+Recv pairs against a real local MCP
> stdio server (target: `mcp-agent-context`), assuming the server's request handler
> is concurrent (verified for mcp-go-built servers via `server.go:240-256`).

**Failure modes if wrong**:
1. The demuxer's chan-per-id model deadlocks under backpressure (one slow caller's
   chan fills, blocks the reader goroutine, blocks all other callers).
2. Some MCP servers (Python/Node scripts not built on mcp-go) handle requests
   sequentially — demuxing the wire doesn't unlock throughput against them.
3. Server-side `transport.Send(ctx, resp)` from a request goroutine has a race we
   haven't seen because the outer lock has hidden it.

**Kill test (slice 1)**: 30 minutes — wrap `StdioTransport` in a demuxing test
harness, fire 10 concurrent `agent_context_recall` calls at a running
`mcp-agent-context` binary, assert:
1. No ID mismatch / ID interleaving in the dispatch (count of correctly-routed
   responses == 10).
2. Wall-clock under 300 ms (serialized would be ~1s at the 100 ms server-side
   latency we measured for !456's hub test).
3. No "transport closed" cascade after the burst.

Pass → continue to slice 2. Fail → escalate to F1 (upstream change) or F6
(Dispatcher pattern) depending on the failure mode.

## Convergence: ONE recommendation

Build a per-id demuxing wrapper in `loom-core` as `pkg/transport/muxstdio`.
Slice 1 is a kill-test against a real `mcp-agent-context` binary. Slice 2 wires
the wrapper into `pkg/pool` so every pool Conn for a local stdio server uses
the demuxed transport. Slice 3 deletes the local-only `acquireCallLock` site in
`callpipeline_routing.go`. Slice 4 (optional, deferred) upstreams the proven shape
into `libs/mcp-go.StdioTransport`.

Riskiest assumption + kill test live in the product spec; that spec is the unblocker
for the implementation plan.
