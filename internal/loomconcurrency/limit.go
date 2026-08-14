// Package loomconcurrency centralizes the per-process handler-concurrency
// limit for every loom-owned MCP server binary. It pairs with the per-id
// stdio mux shipped in pkg/transport/muxstdio + the daemon's local dial
// path: the mux makes the wire safe for concurrent Send+Recv pairs, but
// callers do not actually see throughput improvement unless each server
// processes requests in parallel.
//
// mcp-go.Server defaults to concurrencyLimit=1 (sequential) for backward
// compatibility (see libs/mcp-go server.go:97). Every loom-owned binary
// should call [Apply] (or be constructed via mcpscaffold.NewServer, which
// already does so) to opt into bounded parallel dispatch.
//
// The limit is controlled by LOOM_MCP_CONCURRENCY. Default is 8 — high
// enough to soak up bursty heartbeat-cadence traffic without exhausting
// the typical 1024 file-descriptor budget. The value is clamped to
// [1, 256]; 1 keeps the legacy sequential mode.
package loomconcurrency

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Validate reports whether limit is safe to use as an explicit concurrency
// setting. Unlike ApplyValue, it does not clamp invalid policy input: callers
// loading configuration should reject a typo instead of silently changing it.
func Validate(limit int) error {
	if limit < MinLimit || limit > MaxLimit {
		return fmt.Errorf("concurrency limit must be between %d and %d", MinLimit, MaxLimit)
	}
	return nil
}

const (
	// EnvVar is the environment variable that overrides the default
	// per-server handler-concurrency limit for every loom MCP binary.
	EnvVar = "LOOM_MCP_CONCURRENCY"

	// DefaultLimit is the fallback when LOOM_MCP_CONCURRENCY is unset
	// or unparseable. 8 trades off pipelined throughput against memory
	// and file-descriptor budgets — see package doc.
	DefaultLimit = 8

	// MinLimit clamps the lower bound. 1 keeps the legacy sequential
	// mode (the mcp-go default).
	MinLimit = 1

	// MaxLimit clamps the upper bound. 256 is a sanity ceiling — beyond
	// this, per-process goroutine churn outweighs marginal pipelining
	// gains for the workloads loom MCP servers handle today.
	MaxLimit = 256
)

// limiter exposes the subset of mcp.Server (and any wrapper that embeds
// it, e.g. mcpscaffold.Server) that we need to apply the limit. Using
// an interface lets callers pass either type without an import on
// mcp-go from this package.
type limiter interface {
	SetConcurrencyLimit(int)
}

// Default returns the effective handler-concurrency limit for this
// process, honoring LOOM_MCP_CONCURRENCY with clamping to [1, 256].
// An unset, empty, or unparseable value falls back to DefaultLimit.
//
// Safe to call multiple times; reads the env each call so test harnesses
// using t.Setenv pick up the override.
func Default() int {
	raw := strings.TrimSpace(os.Getenv(EnvVar))
	if raw == "" {
		return DefaultLimit
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return DefaultLimit
	}
	return clamp(n)
}

// Apply sets the handler-concurrency limit on s to Default(). s is
// typically a *mcp.Server or *mcpscaffold.Server. A nil s is a no-op.
func Apply(s limiter) {
	if s == nil {
		return
	}
	s.SetConcurrencyLimit(Default())
}

// ApplyValue sets a specific limit on s, clamped to [1, 256]. Provided
// for tests and for the rare binary that wants to override the env
// for a specific reason — most callers should use [Apply].
func ApplyValue(s limiter, limit int) {
	if s == nil {
		return
	}
	s.SetConcurrencyLimit(clamp(limit))
}

// ApplyValidated applies an explicit limit only when it is within the
// supported range. Validation happens before SetConcurrencyLimit so a rejected
// policy value cannot partially mutate the limiter.
func ApplyValidated(s limiter, limit int) error {
	if err := Validate(limit); err != nil {
		return err
	}
	if s != nil {
		s.SetConcurrencyLimit(limit)
	}
	return nil
}

func clamp(n int) int {
	if n < MinLimit {
		return MinLimit
	}
	if n > MaxLimit {
		return MaxLimit
	}
	return n
}
