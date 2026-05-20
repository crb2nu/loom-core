# Implementation Plan — Per-request-ID multiplexing for shared stdio transports

- **Date**: 2026-05-20
- **Companion**: [brainstorm-stdio-mux-2026-05-20.md](brainstorm-stdio-mux-2026-05-20.md),
  [product-spec-stdio-mux-2026-05-20.md](product-spec-stdio-mux-2026-05-20.md)
- **Status**: S1 kill-test **PASSED 2026-05-20** (commit pending on
  `feat/stdio-mux-killtest`; evidence pasted into the product spec's Status
  line). S2 unblocked. Two findings during the run updated the plan: see
  "S1 kill-test outcome" below and the new S2.5 slice.
- **Cycle**: 2026-05-20 → ~2026-05-27 (5 working days est.)

## Execution order

```
S1   Kill-test harness against real mcp-agent-context  (≤30 min target, ≤0.5 day)
     → GATE: pass criteria met before any other code   [PASSED 2026-05-20]
S2   pkg/transport/muxstdio package (unit + race)       (1 day)
S2.5 (optional, additive) Enable handler-side parallel  (0.25 day)
     dispatch via SetConcurrencyLimit on each server
S3   Wire mux into pool for local stdio + drop callLock (1 day)
     → ship; soak 24h on operator machine
S4   (optional) Upstream the shape into libs/mcp-go     (1.5 days, separate repo)
     → only if S2/S3 shape proves stable
```

Each slice ends with a green local test run + an MR. S1 ships an in-tree
prototype harness (build-tag gated) as evidence rather than a throwaway —
the test is cheap to keep around and serves as a regression guard for the
demuxer shape in S2.

## S1 kill-test outcome (2026-05-20)

- 3-of-3 runs green. Burst wall 8–12 ms vs. 300 ms budget. Per-id routing
  verified: `delivered=13 dropped=0` per run (1 init + 1 warmup + 10 burst +
  1 follow-up). No transport-level errors in captured stderr.
- Original burst payload (`agent_context_recall`) was infeasible because
  each call costs ~300–400 ms (embedder + Qdrant RTT inside the handler).
  Switched the burst to `tools/list`, which exercises the same wire +
  dispatch path with no external IO. See the spec's Status section for the
  detailed narrative.
- `mcp-go.Server.concurrencyLimit` defaults to `1` (server.go:97
  `// Default to sequential for backward compatibility`). Nothing in this
  repo calls `SetConcurrencyLimit`. The demuxer is correct under this
  default (responses route correctly even when the server processes them
  serially), but actual handler-side parallelism needs an additive change —
  S2.5 below.

## Slice 1 — Kill-test (gating, no production code)

**Goal**: prove the riskiest assumption from the spec, in ≤30 minutes, against a real
local stdio server. If the assumption fails here, the rest of the plan is rewritten,
not patched.

**Files to add**

- `pkg/transport/muxstdio/killtest_test.go` (gated by `//go:build killtest`)
  - Prototype demuxer inlined in the test (NOT the production package yet — keep it
    throwaway-shaped so the spec-vs-impl decision can still pivot).
  - `TestKill_TenConcurrentAgentContextRecallCalls`:
    - `t.Helper(); requireBin(t, "mcp-agent-context")` — fails with a clear message
      if the binary isn't built (`go build -o /tmp/mcp-agent-context ./cmd/mcp-agent-context`).
    - Spawn the binary via `exec.CommandContext` with stdin/stdout pipes.
    - Initialize MCP handshake (one `initialize` call) and one warmup
      `agent_context_recall` to hit cache.
    - Wrap stdin/stdout with the prototype demuxer.
    - `sync.WaitGroup`, 10 goroutines each issuing one
      `tools/call agent_context_recall` with distinct ids 1..10 and identical
      params.
    - Assert (a) all 10 responses received within 300 ms wall-clock; (b) every
      response's `id` was routed to the goroutine that owned that id (no
      misrouting); (c) a follow-up 11th call after the burst succeeds within
      150 ms; (d) the spawned process is still alive (`cmd.Process.Signal(0)`).

**Validation**

- Build the bin: `go build -o /tmp/mcp-agent-context ./cmd/mcp-agent-context`.
- Run: `go test ./pkg/transport/muxstdio/ -tags=killtest -run TestKill -v -count=3`.
- Three repeats to flush out timing flakes. All three must pass.

**Pass criteria** (record in the spec's "Status" line):

- All 4 assertions green on 3-of-3 runs.
- Wall-clock for the 10-call burst < 300 ms on this machine.
- No "transport closed", "broken pipe", or "EOF" logs from stderr.

**Fail handling**:

- If (a) fails but (b) succeeds → bottleneck is elsewhere (kitprocess startup,
  initialization). Re-time without spawn cost; if still slow, the assumption that
  `mcp-go.Server` parallelizes is wrong for this binary — investigate per-handler
  serialization. Update the spec accordingly.
- If (b) fails (id misrouting) → demuxer design bug. Likely culprits: races
  between Send's pending registration and Recv's dispatch, or
  json.Number-vs-int id-key mismatch. Fix and re-run; if root cause is in
  upstream `StdioTransport`, the spec pivots to F1 (upstream change).
- If (c) or (d) fails → stdio pipe got corrupted by the burst (transport-level
  bug). Stop. The spec's assumption is wrong; F2 (in-loom wrapper) is not viable;
  escalate to F6 (Dispatcher pattern with separate processes per concurrency
  tier) or F4 (TCP migration).

**Out of scope for this slice**:

- Any change to production code paths.
- Adding the prototype to `pkg/transport/muxstdio/` as non-test code (that's
  slice 2 with a different design budget).
- Metrics, observability, structured logging.

**Done when**: spec's "Status" line updated with the recorded run output (paste
the test output as a fenced block + the commit SHA that captured it).

---

## Slice 2 — Production `pkg/transport/muxstdio` package

**Pre-condition**: Slice 1 passed.

**Goal**: ship a real, race-clean, well-tested demuxing transport in
`pkg/transport/muxstdio/`. No call-site changes — package compiles, tests pass,
nothing in the daemon uses it yet.

**Files to add**

- `pkg/transport/muxstdio/transport.go`:
  - `Transport` struct per the spec's "New package" section.
  - `New(inner mcp.Transport, opts ...Option) *Transport`.
  - `Send(ctx, msg)`: extract id (must be non-nil for a request; return error if
    nil — notifications go a different path), pre-register pending chan
    (buffered 1), call `inner.Send`, on failure delete pending entry, return.
  - `Recv(ctx, id)`: look up pending chan; wait on `chan-recv | ctx.Done() |
    done`; delete pending entry on return.
  - Background `readLoop` goroutine: `inner.Recv(bg ctx)`, route by parsed id.
    Non-blocking send to per-id chan; on full chan, log + bump metric, drop
    message. id-less messages → `notifyCh` (also drop-on-full).
  - `NotificationCh() <-chan *mcp.Message`.
  - `Close()`: idempotent via `sync.Once`; closes `done`, waits for reader,
    closes inner. Drains pending chans on close (delivers `errClosed` to any
    waiter).
- `pkg/transport/muxstdio/options.go`:
  - `Option func(*Transport)`.
  - `WithMetrics(m Metrics) Option`.
  - `WithLogger(l *slog.Logger) Option`.
- `pkg/transport/muxstdio/metrics.go`:
  - `Metrics` interface (nil-safe).
  - Counters: `MuxDispatches`, `MuxDropsFullChan`, `MuxDropsNoPending`,
    `MuxNotifications`.

**Files to add (tests)**

- `pkg/transport/muxstdio/transport_test.go`:
  - `TestSend_RegistersPendingThenForwards` — uses `mcp.NewPipeTransport`.
  - `TestRecv_DemuxesByID_TwoConcurrentCalls` — two goroutines, two ids,
    cross-ordered response delivery; assert each goroutine got its own id.
  - `TestRecv_ContextCancel_RemovesPending` — assert the pending map is empty
    after ctx cancel.
  - `TestRecv_AfterClose_ReturnsClosedError`.
  - `TestSlowDrainer_DoesNotBlockOtherCallers` — caller A registers but never
    Recvs; caller B's Send→Recv must complete within 100 ms.
  - `TestNotificationFanOut_DropsOnFullChan` — fill `notifyCh`, send N+1
    notifications, assert metric increment + no goroutine block.
  - `TestClose_Idempotent`.
  - `TestClose_DeliversErrorToPendingWaiters`.
- `pkg/transport/muxstdio/transport_race_test.go`:
  - 100 goroutines, 1000 random Send/Recv interleavings, no race detector hits.

**Validation**

- `go test ./pkg/transport/muxstdio/ -race -count=10` — green.
- `go vet ./pkg/transport/muxstdio/` — clean.
- `golangci-lint run ./pkg/transport/muxstdio/...` — clean.

**Done when**:

- Package compiles, tests pass with `-race`, MR opened with the recorded
  metric names + the spec's API surface matching what shipped (or spec
  amended to match shipped surface if a small deviation was needed).

**MR title**: `feat(transport/muxstdio): per-id demuxing wrapper for shared stdio`

---

## Slice 2.5 — Server-side concurrency (optional, additive)

**Pre-condition**: Slice 2 merged.

**Goal**: enable handler-side parallel dispatch on every loom-owned MCP
server so the wire-level demuxer from S2 actually translates into reduced
end-to-end latency for callers under load.

**Why this is its own slice**: discovered during S1's kill-test that
`mcp-go.Server.concurrencyLimit` defaults to `1`. The demuxer is correct
without this change (it just demuxes responses arriving serially from a
sequential server), so S3 can ship without S2.5 if needed. But callers
that pipeline requests through the demuxer see no throughput improvement
unless the server is also processing concurrently.

**Files to change**

- Each `cmd/mcp-*` entry-point that constructs an `mcp.NewServer(...)`
  (grep `mcp.NewServer\|NewServer(` to enumerate). Add:
  ```go
  server.SetConcurrencyLimit(loomconcurrency.Default()) // honors LOOM_MCP_CONCURRENCY env, default 8
  ```
- New helper `internal/loomconcurrency/limit.go` reading `LOOM_MCP_CONCURRENCY`
  (default `8`, validated `1..256`, `1` keeps backward-compat sequential
  mode). Keeps the per-binary change to one line.

**Risks**

- Handlers that share mutable state without internal locking will race. Audit
  before merging. Most loom handlers are read-mostly or use `sync.Map` /
  per-key locks already; verify per binary.
- Bumping concurrency to N=8 means each server can run up to 8 goroutines
  per stdio process. Memory/file-descriptor budgets should be sanity
  checked; defaults look comfortable but make this explicit in the MR.

**Validation**

- Re-run the slice-1 kill-test with payload swapped back to a real handler
  call that doesn't depend on the FlexInfer embedder (pick one that's
  cheap and IO-free) — burst wall should drop ~Nx with N=8.
- Existing per-binary unit tests still pass.

**Done when**: every loom MCP server entrypoint sets concurrency, helper
package merged, kill-test variant with a parallel-friendly handler shows
~Nx speedup.

**MR title**: `feat(mcp): enable handler-side parallel dispatch (LOOM_MCP_CONCURRENCY)`

---

## Slice 3 — Wire mux into local-stdio pool + drop the callLock

**Pre-condition**: Slice 2 merged.

**Goal**: make every local-stdio `pool.Conn` use the mux'd transport. Delete the
`acquireCallLock` site for `TargetLocal`. Prove the change with a regression
test mirroring !456's hub-parallelism test.

**Files to change**

- `internal/pool/conn.go` (or `internal/pool/dial.go` — locate the local-stdio
  dial path in slice 2's preparation):
  - The local stdio dial path that returns `*pool.Conn` wraps the returned
    transport in `muxstdio.New(...)`.
  - Multiple pool Conns for the same `serverName` share the SAME mux
    instance (kitprocess.Manager already de-dupes the underlying
    `*StdioTransport`; the mux must be de-duped at the same layer).
    Implementation sketch: a `muxCache map[serverName]*muxstdio.Transport`
    guarded by a mutex in the manager that owns kitprocess.
  - Mux instance lifecycle: created when the first Conn for a serverName is
    dialed; closed when kitprocess.Manager stops the process (mux.Close also
    closes inner, propagating to the process). Tie into the same `Stop` path
    that already exists.

- `internal/daemon/callpipeline_routing.go`:
  - Delete the `if target == router.TargetLocal { acquireCallLock(...) ...
    p.callMu.Unlock() }` block (the pre-pool.Get lock).
  - Delete the `if target == router.TargetLocal { p.callMu, _, err =
    acquireCallLock(...) }` block (the post-pool.Get lock).
  - Replace with: if `target == router.TargetLocal && p.daemon.procMgr != nil`
    `p.daemon.procMgr.MarkActivity(p.serverName)` (lock-free — verify
    MarkActivity's safety, add a comment if it relies on internal locking).
  - Update the long lock-justification comment to a brief explanation of
    "why the callLock is gone" + a link to this spec.

- `internal/daemon/callpipeline_*.go`:
  - `TestHandleCall_LocalConcurrencyNoIDMismatchWithLock` (renamed in !456) →
    rename to `TestHandleCall_LocalConcurrencyNoIDMismatchWithDemux`. Change
    the assertion: with the demuxer, IDs cannot misroute, so the test
    becomes a positive parallelism test rather than a serialization test.
  - Find and update any other test that asserts `p.callMu != nil` or
    `p.lockHeld` for the local path.
  - `p.callMu` and `p.lockHeld` fields are now only set on... nothing — the
    pipeline never acquires the lock. Remove the fields if unused; if used
    elsewhere (defer release, error paths), keep them but they're always
    nil/false for local.

**Files to add**

- `internal/daemon/callpipeline_test.go` (additions):
  - `TestHandleCall_LocalConcurrentCallsRunInParallel`: mirror of the
    !456 hub test. 10 concurrent local-stdio callers × 100 ms server-side
    latency. Asserts wall-clock < 300 ms (serialized would be ~1 s).
    Uses a fake transport that the test wraps in `muxstdio.New` so the
    parallelism is exercised end-to-end without spawning a real process.
  - `TestHandleCall_LocalConcurrentCallsHighFanout`: 50 concurrent callers
    × 50 ms latency. Asserts wall-clock < 250 ms. Catches super-linear
    degradation.

**Validation**

- `go test ./internal/daemon/ -race -count=3` — green.
- `go test ./internal/pool/ -race` — green.
- `go vet ./...` — clean.
- `golangci-lint run ./internal/...` — clean.
- Operator-machine soak: bounce the daemon, run a typical fleet for ≥24h
  with heartbeat-heavy callers active (`agent_context_*`, presence, stream
  monitors). Tail `~/.config/loom/logs/daemon.err` — confirm zero "acquire
  call lock for ... after 15s" lines.

**Done when**:

- All tests green with `-race`.
- Daemon-err log clean during the soak window.
- MR description includes a before/after concurrent-call wall-clock
  measurement against a real local server.

**MR title**: `feat(daemon): mux local-stdio transports, drop per-server callLock`

**Backward compatibility**: none required — the daemon process is the only
consumer of the call lock; remote agents calling the daemon see no API
change. `pool.Conn` API is unchanged (the mux is internal to the dial path).

---

## Slice 4 (optional) — Upstream the shape into `libs/mcp-go`

**Pre-condition**: Slice 3 merged + ≥1 week of soak. Skip if no upstream
appetite or if `pkg/transport/muxstdio` proves portable enough that
upstreaming has no net benefit.

**Goal**: move the demuxing logic into `gitlab.flexinfer.ai/libs/mcp-go`'s
`StdioTransport` so every consumer (loom-core, fi-mcp-kit, future projects)
gets the parallelism without the wrapper.

**Files to add/change (upstream repo)**

- `libs/mcp-go/transport.go`:
  - Add `RecvByID(ctx, id) (*Message, error)` to the `Transport` interface
    (default method on `StdioTransport`; pipe + websocket transports return
    "not supported" sentinel).
  - Update `StdioTransport`: replace single `msgCh chan recvResult` with
    `pending sync.Map` + per-id chans. Existing `Recv(ctx)` semantics
    preserved as "give me the next id-less message" for the notifications
    use case.
  - Add tests mirroring the loom-core slice 2 unit tests.

**Files to change (loom-core)**

- Bump `go.mod` pin to the new `libs/mcp-go` version.
- `pkg/transport/muxstdio` becomes a thin shim or is deleted. Decide based on
  whether the upstream API is identical or close-enough.

**MR title** (upstream): `feat(stdio): per-id demuxing for concurrent recv`
**MR title** (loom-core): `chore(deps): bump mcp-go to <ver>, retire local muxstdio`

---

## Risk register

- **R1 — Slice 1 fails**: spec assumption is wrong. Plan pivots. No code shipped
  beyond the throwaway prototype. Cost: ~0.5 day.
- **R2 — Slice 2 ships but Slice 3 wire-up exposes pool/manager ownership
  issues**: e.g. who owns the mux's lifecycle vs the inner stdio. Mitigation:
  slice 2 includes a small "manager-owns-mux-cache" sketch in the package
  doc comment; locate the right hook point during slice 2, not slice 3.
- **R3 — Soak surfaces a regression that unit tests didn't catch**: e.g. a
  heartbeat-cadence-specific race. Mitigation: slice 3 ships behind a daemon
  flag (`LOOM_MUX_STDIO=1`, default off for the first day; flip to default-on
  after 24h soak).
- **R4 — `procMgr.MarkActivity` was load-bearing on the callLock**: removing
  the lock changes a side effect somewhere. Mitigation: read the MarkActivity
  call site in slice 3; if it touches shared state, replace with an explicit
  per-server mutex inside `procMgr` rather than reusing the call lock.

## Verification per slice (kill criteria)

- **S1**: 3-of-3 runs of `TestKill_TenConcurrentAgentContextRecallCalls`
  pass with wall-clock < 300 ms. No transport-closed lines in stderr.
- **S2**: `go test ./pkg/transport/muxstdio/ -race -count=10` green.
- **S3**: `go test ./internal/daemon/ -race -count=3` green +
  `LOOM_MUX_STDIO=1` 24h soak with zero call-lock cascade lines.
- **S4** (optional): same shape as S2 but against upstream's tests; loom-core
  CI green on the bumped pin.

## Effort estimate (rough)

| Slice | Effort   | Notes |
|-------|----------|-------|
| 1     | 0.5 day  | Prototype + 30-min kill-test runbook |
| 2     | 1 day    | Production package + race-clean tests |
| 3     | 1 day    | Wire-up + regression test + soak runbook |
| 4     | 1.5 days | Upstream change, separate repo; OPTIONAL |

## Open follow-ups

- Decision: keep `pool.maxOpen=25` or raise/lower it now that the wire is
  no longer the bound? Defer to post-slice-3 soak data.
- Decision: does the daemon emit a per-server "outstanding-mux-requests"
  metric? Useful for diagnosing future stalls. Defer to slice 3.
