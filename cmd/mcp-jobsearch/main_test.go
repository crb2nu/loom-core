package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/crb2nu/loom/internal/mcptest"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestUnconfiguredJobsearchClientReturnsNotConfigured(t *testing.T) {
	_, err := (unconfiguredJobsearchClient{}).Request(context.Background(), requestOptions{})
	if err == nil || !strings.Contains(err.Error(), "JOBSEARCH_API_URL") {
		t.Fatalf("expected JOBSEARCH_API_URL error, got %v", err)
	}
}

func TestMain(m *testing.M) {
	_ = os.Setenv("LOOM_MCP_OUTPUT_FORMAT", "json")
	os.Exit(m.Run())
}

func TestNewJobsearchClientFromEnv_MissingURL(t *testing.T) {
	t.Setenv("JOBSEARCH_API_URL", "")
	t.Setenv("JOBSEARCH_API_TOKEN", "test-token")
	t.Setenv("JOBSEARCH_BEARER_TOKEN", "")

	_, err := newJobsearchClientFromEnv(testLogger())
	if err == nil {
		t.Fatal("expected error when JOBSEARCH_API_URL is missing")
	}
}

func TestNewJobsearchClientFromEnv_MissingToken(t *testing.T) {
	t.Setenv("JOBSEARCH_API_URL", "http://localhost:8000")
	t.Setenv("JOBSEARCH_API_TOKEN", "")
	t.Setenv("JOBSEARCH_BEARER_TOKEN", "")
	t.Setenv("JOBSEARCH_CF_ACCESS_CLIENT_ID", "")
	t.Setenv("JOBSEARCH_CF_ACCESS_CLIENT_SECRET", "")
	t.Setenv("CF_ACCESS_CLIENT_ID", "")
	t.Setenv("CF_ACCESS_CLIENT_SECRET", "")

	client, err := newJobsearchClientFromEnv(testLogger())
	if err != nil {
		t.Fatalf("unexpected error when token is missing: %v", err)
	}
	if client.token != "" {
		t.Fatalf("expected empty token when unset, got %q", client.token)
	}
}

func TestNewJobsearchClientFromEnv_UsesFallbackToken(t *testing.T) {
	t.Setenv("JOBSEARCH_API_URL", "http://localhost:8000")
	t.Setenv("JOBSEARCH_API_TOKEN", "")
	t.Setenv("JOBSEARCH_BEARER_TOKEN", "fallback-token")
	t.Setenv("JOBSEARCH_CF_ACCESS_CLIENT_ID", "")
	t.Setenv("JOBSEARCH_CF_ACCESS_CLIENT_SECRET", "")
	t.Setenv("CF_ACCESS_CLIENT_ID", "")
	t.Setenv("CF_ACCESS_CLIENT_SECRET", "")

	client, err := newJobsearchClientFromEnv(testLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if client.token != "fallback-token" {
		t.Fatalf("expected fallback token to be used, got %q", client.token)
	}
}

// TestJobsearchServesHandshakeWithoutBackend pins the degraded-start
// contract at the server level: with no API URL set the server answers
// initialize and tools/list, and a tool call returns an MCP error naming
// JOBSEARCH_API_URL, without exiting the process.
func TestJobsearchServesHandshakeWithoutBackend(t *testing.T) {
	for _, name := range []string{
		"JOBSEARCH_API_URL", "JOBSEARCH_API_TOKEN", "JOBSEARCH_BEARER_TOKEN",
		"JOBSEARCH_CF_ACCESS_CLIENT_ID", "JOBSEARCH_CF_ACCESS_CLIENT_SECRET",
		"CF_ACCESS_CLIENT_ID", "CF_ACCESS_CLIENT_SECRET", "OTEL_EXPORTER_OTLP_ENDPOINT",
	} {
		t.Setenv(name, "")
	}

	srv, cleanup, err := newJobsearchServer(context.Background())
	if err != nil {
		t.Fatalf("server must start without backend configuration: %v", err)
	}
	t.Cleanup(func() { _ = cleanup(context.Background()) })

	res := mcptest.Drive(t, srv.Server, "jobsearch_health_get", map[string]any{})
	if names := mcptest.ToolNames(t, res.ToolsList); len(names) == 0 {
		t.Fatal("tools/list must not be empty while unconfigured")
	}
	result := mcptest.ToolResult(t, res.ToolCall)
	text := mcptest.Text(result)
	if !result.IsError || !strings.Contains(text, "JOBSEARCH_API_URL") {
		t.Fatalf("expected NotConfigured naming JOBSEARCH_API_URL, got isError=%v text=%q", result.IsError, text)
	}
}
