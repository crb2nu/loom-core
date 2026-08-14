package main

import (
	"context"
	"log/slog"

	"github.com/crb2nu/loom/internal/iccclient"
)

// iccClient is the package-local alias for the shared
// internal/iccclient.Client. The capture binary used to host its own
// http_client.go; M53 extracted that into the shared package so the
// new mcp-icc binary could reuse it. The alias keeps existing call
// sites (handler files, tests) terse and unchanged.
type iccClient = iccclient.Client

// newICCClient is the local wrapper around iccclient.New so existing
// call sites keep their name.
func newICCClient(logger *slog.Logger) *iccClient {
	return iccclient.New(logger)
}

// postJSON re-exports iccclient.PostJSON as a package-local generic so
// the handler files don't need to import iccclient directly. This
// matches the symbol name the handlers use today.
func postJSON[T any](ctx context.Context, c *iccClient, path string, body any) (int, T, error) {
	return iccclient.PostJSON[T](ctx, c, path, body)
}
