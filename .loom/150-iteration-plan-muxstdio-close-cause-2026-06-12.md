# Iteration Plan: muxstdio close-cause observability (Slice 1 of .loom/149)

**Date**: 2026-06-12
**Branch**: `feat/muxstdio-close-cause`
**Parent plan**: `.loom/149-plan-backend-hub-transport-stability-2026-06-12.md` (Slice 1)

## Scope

**In**:
- `pkg/transport/muxstdio/transport.go`: capture the first `inner.Recv` error in `readLoop` as `closeCause`; log it once at WARN with the count of pending calls it failed; annotate `ErrClosed` returns from `Send`/`Recv` with the cause (`%w` wrap preserves `errors.Is` and the `"transport closed"` substring used by `callpipeline_routing.go:119` / `callpipeline_timeout.go:208`).
- Suppress cause/warn for deliberate `Close()` (reader sees context.Canceled after `readerCancel`) so normal shutdown stays quiet.
- Regression tests: cause propagation, deliberate-close silence, existing `errors.Is` contract.

**Out** (follow-ups per parent plan):
- Gateway-side disconnect-reason logging (lives in `libs/fi-mcp-kit/pkg/gateway/hub.go:511` — separate repo/MR).
- libs/mcp-go WS keepalive/liveness (Slice 2; gorilla close errors already stringify the close code, so client-side cause logging may identify the closer without touching mcp-go).

## Acceptance criteria
1. When the hub WS dies, daemon.err shows the underlying error (incl. WS close code) both in a one-time `muxstdio: inner transport closed` WARN and in every existing `hub transport failed` log line (which prints `error=%v` of the now-annotated ErrClosed).
2. `errors.Is(err, ErrClosed)` still true on all paths; `strings.Contains(err.Error(), "transport closed")` still true.
3. `go test ./pkg/transport/muxstdio/ -race` green; daemon package tests green.

## Risks
- Wrapped error strings flow into agent-visible -32603 messages — longer but more diagnostic; no parsing depends on exact suffix (verified: detection is substring/errors.Is only).

## Test plan
- New: `TestReadLoopError_AnnotatesErrClosedWithCause`, `TestClose_NoCauseOnDeliberateClose`.
- Existing muxstdio suite + `go test ./internal/daemon/ -run TransportFailure`.

## Status
- [x] implemented
- [x] tests green
- [ ] MR merged
