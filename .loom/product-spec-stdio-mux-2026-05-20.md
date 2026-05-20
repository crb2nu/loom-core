# Product Spec — Per-request-ID multiplexing for shared stdio transports

- **Date**: 2026-05-20
- **Brainstorm**: [.loom/brainstorm-stdio-mux-2026-05-20.md](brainstorm-stdio-mux-2026-05-20.md)
- **Predecessor MR**: !456 `fix(daemon): skip per-server callLock for hub-routed calls`
- **Predecessor commit**: `14ece38f`
- **Implementation plan**: [.loom/implementation-plan-stdio-mux-2026-05-20.md](implementation-plan-stdio-mux-2026-05-20.md)
- **Status**: draft — slice 1 (kill-test) blocks slice 2

## Riskiest assumption + kill-test

**Load-bearing assumption**: Wrapping `mcp.StdioTransport` (from
`gitlab.flexinfer.ai/libs/mcp-go@aa57e61f11bd`) in a per-id demuxing layer is
sufficient to safely permit N concurrent `Send` + `Recv` pairs against a real
local MCP stdio server. Specifically: against `cmd/mcp-agent-context/` (built
on `mcp-go.Server`, default concurrent dispatch — verified
[server.go:240-256](file:///Users/cblevins/go/pkg/mod/gitlab.flexinfer.ai/libs/mcp-go@v0.2.1-0.20260520023524-aa57e61f11bd/server.go)),
10 concurrent calls complete in approximately 1/10th the serialized wall-clock,
with no ID-routed mismatches and no transport-closed cascade.

**Kill test** (≤30 minutes, slice 1 deliverable):

1. Build `cmd/mcp-agent-context` at current main:
   `go build -o /tmp/mcp-agent-context ./cmd/mcp-agent-context`.
2. Run the harness (new test, `pkg/transport/muxstdio/killtest_test.go`,
   gated by `-tags=killtest` so it doesn't run in unit CI):
   - Spawn the binary as a child process; wrap its stdio with the prototype
     demuxer.
   - Issue 10 concurrent `agent_context_recall` calls with identical params.
   - Server-side latency observed for this tool on this binary is ~80–120 ms
     under cold cache; warm it with one prior call.
   - Assert: (a) all 10 responses arrive within 300 ms wall-clock (serialized
     would be ~1 s); (b) every response's JSON-RPC id matches the caller that
     sent it; (c) the transport remains usable for a follow-up 11th call after
     the burst; (d) no "transport closed" or "broken pipe" surface in stderr.
3. Record the run as evidence (paste output + duration in this spec under
   **Status** and link the commit that updated the file).

**Failure modes if the assumption is wrong**:

- *Backpressure*: one slow caller's per-id chan fills, the demuxer's reader
  goroutine blocks on the send to that chan, every other caller stalls.
  Mitigation surfaced in the design below (per-id chan buffer = 1; reader uses
  non-blocking send with a slow-drainer eviction path).
- *Server-side serial handler*: a Python/Node script not built on `mcp-go`
  processes requests sequentially. Demuxing the wire gains nothing for that
  server; outer lock removal is still correct (no ID corruption) but throughput
  matches the serial baseline. Acceptable.
- *Send-side ordering bug*: server-side `transport.Send` from concurrent request
  goroutines uses `writeMu` (verified
  [transport.go:51](file:///Users/cblevins/go/pkg/mod/gitlab.flexinfer.ai/libs/mcp-go@v0.2.1-0.20260520023524-aa57e61f11bd/transport.go)).
  If a non-mcp-go server lacks equivalent write serialization, two responses
  could interleave bytes on stdout. Mitigation: parser-level recovery + skip,
  log + metric. Acceptable degradation; the wire was never safe against such
  servers and the outer lock didn't fix it either (lock is on the CLIENT side).

**Status**: not run.

### Pair with negative search (per `~/.claude/rules/spec-riskiest-assumption.md`)

Searches to run before declaring slice 1 passed (record one-line summary per
finding, evidence links, in the slice-1 handoff doc):

1. *"mcp-go StdioTransport thread-safe"* — confirm upstream stance on
   concurrent Recv.
2. *"JSON-RPC pipelining stdio mismatch"* — surface known footguns.
3. *"MCP server NOT concurrent stdio"* — disconfirming search: are there
   documented MCP servers that explicitly process stdin sequentially despite the
   spec? If so, list them in the spec's "Out of scope" section.

## Goal

Eliminate the per-server `acquireCallLock` for `TargetLocal` in
`internal/daemon/callpipeline_routing.go`. After this work, local-stdio call
parallelism matches hub-routed parallelism (just landed in !456): N concurrent
callers against the same `serverName` execute concurrently end-to-end, bounded
only by the server's own `concurrencyLimit` (already concurrent by default for
mcp-go-built servers).

## Non-goals

- **Process-per-caller**: F3 in the brainstorm. Out of scope. Stdio servers
  carry per-process state (DB handles, caches) and spawn cost is non-trivial.
- **Migrating hot servers to TCP**: F4 in the brainstorm. Out of scope for this
  slice plan; revisit if mux'd stdio still doesn't meet the latency budget.
- **Upstream change to libs/mcp-go.StdioTransport**: F1. Out of scope for this
  spec but tracked as a follow-on (slice 4 in the plan, optional).
- **Server-side notification fan-out** (server-pushed `notifications/*`
  messages, no id): out of scope for slice 1–3. The demuxer routes id-less
  messages to a single notification channel; existing callers don't subscribe
  to it. Slice 5+ can model a real subscription API if needed.
- **Streaming responses** (`progress` partials, multi-message responses with
  the same id): out of scope. Single-response-per-id model in this spec.

## Architecture

```
                                Existing (post-!456)
─────────────────────────────────────────────────────────────────────────
caller A ──► acquireCallLock ──► pool.Get ──► tx.Send ──► tx.Recv  ✓
caller B ──► acquireCallLock ──blocks for A───────────────────►
caller C ──► acquireCallLock ──blocks for A───────────────────►

                                    Proposed
─────────────────────────────────────────────────────────────────────────
caller A ──► mux.Send(id=42, req) ──► mux.Recv(id=42) ──► resp_42  ✓
caller B ──► mux.Send(id=43, req) ──► mux.Recv(id=43) ──► resp_43  ✓  (parallel)
caller C ──► mux.Send(id=44, req) ──► mux.Recv(id=44) ──► resp_44  ✓  (parallel)
                                            ▲
                                            │   one reader goroutine
                                            ├── inside *muxstdio.Transport
                                            ▼
                              upstream tx.Recv() ──► dispatch by msg.ID ──► per-id chan
```

### New package: `pkg/transport/muxstdio`

```go
package muxstdio

import (
    "context"
    "sync"

    "gitlab.flexinfer.ai/libs/mcp-go"
)

// Transport multiplexes requests over one underlying mcp.Transport by JSON-RPC
// message id. Send returns once the message is written. Recv blocks until the
// response with the matching id arrives, or ctx is cancelled.
//
// A single reader goroutine drains the underlying transport and dispatches each
// inbound message to the channel registered for its id. Messages with no id
// (notifications, server-pushed events) go to NotificationCh.
type Transport struct {
    inner       mcp.Transport       // underlying *mcp.StdioTransport (or any Transport)
    pending     sync.Map            // id (json.Number-as-string) -> chan *mcp.Message (buf 1)
    notifyCh    chan *mcp.Message   // unsolicited messages (id == nil)
    done        chan struct{}
    closeOnce   sync.Once
    readerWg    sync.WaitGroup
    metrics     Metrics             // optional, nil-safe
}

func New(inner mcp.Transport, opts ...Option) *Transport
func (t *Transport) Send(ctx context.Context, msg *mcp.Message) error // pre-registers id then forwards
func (t *Transport) Recv(ctx context.Context, id string) (*mcp.Message, error) // demuxed
func (t *Transport) NotificationCh() <-chan *mcp.Message
func (t *Transport) Close() error
```

Key behaviors:

- **Pending registration**: `Send` parses `msg.ID`, makes a buffered chan
  (capacity 1), stores it in `pending`, then calls `inner.Send`. If `Send`
  fails, the pending entry is deleted before returning.
- **Reader goroutine**: blocks on `inner.Recv(ctx.Background())`. On message:
  if `msg.ID != nil`, looks up the pending chan and does a non-blocking send
  with a 1 ms grace; if the chan is full (slow drainer) or absent (caller
  cancelled), logs + bumps a metric and drops the message. If `msg.ID ==
  nil`, sends to `notifyCh` (drop if full).
- **Recv**: looks up the chan, waits on `chan-recv | ctx.Done() | done`. On
  return, deletes the pending entry.
- **Close**: closes `done`, waits for reader, then closes `inner`. Safe to
  call multiple times.

### Call-site changes

1. `pkg/pool/conn.go` (or wherever `pool.Conn` holds the underlying transport):
   - Construction path for local stdio wraps the dialed transport in
     `muxstdio.New(...)`.
   - Hub Conns are unaffected — they have per-Conn `*WebSocketTransport`
     already.
2. `internal/daemon/callpipeline_routing.go`:
   - Delete both `acquireCallLock` sites for `TargetLocal`. Keep the
     `MarkActivity` call (no lock needed, `procMgr.MarkActivity` is
     internally safe).
   - The "Re-acquire the lock for the RPC send phase" comment block goes
     away.
3. `internal/daemon/callpipeline_*.go` — update any test that asserts the
   lock is held.

### Pool semantics

Post-mux, `pool.maxOpen=25` for a local stdio server represents the maximum
in-flight RPC count against that one process. The wire stays at 1; the
parallelism is virtual (id-multiplexed). This matches the pool semantics for
hub already.

`pool.Conn.Close()` does NOT close the inner mux transport — `Close` on the
shared transport is owned by `kitprocess.Manager` (process lifecycle), not
the pool. Pool `Close()` returns the logical handle; the demuxer survives.

## Test plan

### Slice 1 — kill-test (this spec's gating evidence)

- `pkg/transport/muxstdio/killtest_test.go` (`-tags=killtest`)
- Spawns the real `mcp-agent-context` binary, wraps stdio with the prototype
  demuxer, issues 10 concurrent calls, asserts wall-clock and id-routing
  correctness.
- Recorded run + duration pasted into this spec.

### Slice 2 — unit + race

- `pkg/transport/muxstdio/transport_test.go`: pipe-transport-backed unit
  tests for Send→Recv id routing, ctx cancellation, slow-drainer
  eviction, close idempotency.
- `go test ./pkg/transport/muxstdio/ -race -count=10`.

### Slice 3 — regression

- New `internal/daemon/callpipeline_test.go` test:
  `TestHandleCall_LocalConcurrentCallsRunInParallel`. Mirror of the hub test
  from !456 but with `target = router.TargetLocal` and a fake DialFunc that
  returns ONE shared stdio-style transport, demuxed via the new wrapper.
  Assert wall-clock < 300 ms for 10 concurrent calls × 100 ms latency.
- The existing `TestHandleCall_LocalConcurrencyNoIDMismatchWithLock` test
  (renamed in !456) STAYS but its assertion flips: with the demuxer, ID
  mismatch can no longer happen. Rename to
  `TestHandleCall_LocalConcurrencyNoIDMismatchWithDemux`.

### Slice 4 (optional) — upstream

- Land equivalent API in `libs/mcp-go.StdioTransport`. Loom-core picks
  it up via go.mod bump. `pkg/transport/muxstdio` becomes a thin
  compatibility shim or deleted entirely.

## Host-support matrix (server families)

| Server family                                           | Concurrent handler? | Mux unlocks throughput? | Notes |
|---------------------------------------------------------|---------------------|-------------------------|-------|
| Built on `mcp-go.Server` (loom-core `cmd/mcp-*`)        | Yes (default)       | Yes                     | server.go:240-256. Most local servers. |
| Built on `fi-mcp-kit` (loom-core wrappers)              | Yes                 | Yes                     | Same dispatch as mcp-go. |
| Python `mcp` package (modelcontextprotocol/python-sdk)  | Yes (asyncio)       | Yes                     | Async by default. |
| Node `@modelcontextprotocol/sdk`                        | Yes                 | Yes                     | Promise-based handlers. |
| Bespoke Python/Node scripts (no SDK)                    | Often no            | No (no regression)      | Removing the outer lock is correct either way; demuxer still demuxes, just one outstanding response at a time. |

## Risk register

- **R1 — Backpressure deadlock**: if Recv goroutine blocks on a per-id chan
  send, all other callers stall. *Mitigation*: chan buffer 1, non-blocking
  send with grace, log+drop on overflow.
- **R2 — Slice 1 kill-test passes locally but doesn't reflect prod scale**:
  10 concurrent calls is small. *Mitigation*: spec slice 3's regression test
  uses the same shape but parameterized N=10/50/100 to catch
  super-linear-slowdown bugs.
- **R3 — Removing the lock exposes a race we haven't seen**: e.g. concurrent
  `procMgr.MarkActivity` against the same server has a side effect we don't
  notice today because the lock serializes it. *Mitigation*: `-race` flag on
  the regression test; pre-merge `go test ./... -race`.
- **R4 — Notification fan-out grows side users**: future code subscribes to
  `NotificationCh` and creates dependencies we don't want. *Mitigation*:
  document the channel as "internal observability hook, not a subscription
  API"; consider gating behind a method that returns nil unless explicitly
  enabled.

## Out of scope (explicit)

- Process-per-caller (brainstorm F3) — semantics break.
- TCP migration of hot servers (F4) — separate project.
- Pool-level Dispatcher pattern (F6) — defer; mux earns the right to that
  refactor only if streaming/notifications need it.
- kitprocess.Manager mux (F8) — wrong layer, worse blast radius.
- Upstream `libs/mcp-go` change (F1) — slice 4, deferred until the API shape
  is proven in loom-core.

## Decision log

- **2026-05-20**: Chose F2 (loom-core wrapper) over F1 (upstream change).
  Rationale: faster to validate, fewer repo coordinations, can upstream the
  proven shape later. Cost: a small amount of duplicate parsing
  (`Message.ID` re-extraction) until upstream lands.
- **2026-05-20**: Rejected F7 (defer + observability only). Observability
  is valuable on its own, but using it as a reason to defer the fix would
  recreate the cascade that !456 fixed for hub. The metric will show
  contention; we already know that.

## Open questions

1. Does any local stdio server send unsolicited messages (id == nil,
   server-pushed)? If yes, the notification channel is load-bearing for
   correctness, not just observability. **Action**: grep loom-core's
   `cmd/mcp-*/main.go` for any `server.Notify`-style calls before slice 2.
2. Pool `maxOpen=25` semantics post-mux: do we want to keep the in-flight
   bound, or let the mux be unbounded? **Recommendation**: keep `maxOpen` as
   an in-flight cap; servers still have finite request-handler concurrency
   and unbounded muxing pushes the bottleneck downstream.
3. Should `NotificationCh` be a Method/Option (subscriber-pull) or a fixed
   channel? **Recommendation**: fixed channel, drop-on-full, optional metric.
   Defer subscriber API to slice 5+ if a real user emerges.
