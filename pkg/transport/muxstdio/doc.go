// Package muxstdio multiplexes JSON-RPC requests over a single mcp.Transport by
// JSON-RPC message id.
//
// Wrap any mcp.Transport (typically *mcp.StdioTransport) in a [Transport] and
// then call [Transport.Send] from any number of goroutines. A single reader
// goroutine inside the wrapper drains the underlying transport and dispatches
// each response to the goroutine that registered the matching id via
// [Transport.Recv]. Messages with no id (notifications, server-pushed events)
// are delivered on [Transport.NotificationCh].
//
// Lifecycle: Close is idempotent. On Close the inner transport is closed and
// any callers blocked in Recv receive [ErrClosed]. The wrapper does not own
// the inner transport's process: in pool.Conn integration (slice 3) the
// kitprocess.Manager owns the process and calls Close when the process exits.
//
// References:
//   - Spec: .loom/product-spec-stdio-mux-2026-05-20.md
//   - Plan: .loom/implementation-plan-stdio-mux-2026-05-20.md
package muxstdio
