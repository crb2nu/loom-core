// Package muxstdio multiplexes JSON-RPC requests over a single mcp.Transport by
// JSON-RPC message id.
//
// The production implementation will ship in slice 2 of the stdio-mux plan
// (.loom/implementation-plan-stdio-mux-2026-05-20.md). Slice 1 is a throwaway
// kill-test prototype that lives in killtest_test.go behind the build tag
// "killtest" and proves the load-bearing assumption from
// .loom/product-spec-stdio-mux-2026-05-20.md before any production code is
// written.
package muxstdio
