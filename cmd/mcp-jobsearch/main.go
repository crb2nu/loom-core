package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/crb2nu/loom/pkg/lifecycle"
	"github.com/crb2nu/loom/pkg/mcperror"
	"github.com/crb2nu/loom/pkg/mcpscaffold"
)

var version = "0.1.0"

func main() {
	if err := lifecycle.RunWithSignals(context.Background(), run); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	srv, cleanup, err := newJobsearchServer(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = cleanup(ctx) }()
	return srv.Run(ctx)
}

// newJobsearchServer builds the MCP server without running it, so tests can
// drive initialize, tools/list and tool calls in-process. A missing
// JOBSEARCH_API_URL is not fatal: the server starts degraded and every tool
// call answers NotConfigured until it is set. Any other client
// configuration error still refuses to start. The returned cleanup flushes
// the tracer.
func newJobsearchServer(ctx context.Context) (*mcpscaffold.Server, func(context.Context) error, error) {
	srv, cleanup, err := mcpscaffold.NewServer(ctx, "mcp-jobsearch", version,
		mcpscaffold.WithInstructions("JobSearch backend integration MCP server. Includes explicit workflow/CRM tools and a guarded generic API passthrough."),
	)
	if err != nil {
		return nil, nil, err
	}

	client, clientErr := newJobsearchClientFromEnv(srv.Logger)
	var caller apiCaller = client
	maxResponseBytes := 2 * 1024 * 1024
	if clientErr != nil {
		if strings.TrimSpace(os.Getenv("JOBSEARCH_API_URL")) != "" {
			_ = cleanup(ctx)
			return nil, nil, clientErr
		}
		srv.Logger.Warn("JobSearch backend is not configured; tool calls return NotConfigured", "missing_env", "JOBSEARCH_API_URL")
		caller = unconfiguredJobsearchClient{}
	} else {
		maxResponseBytes = client.maxResponseBytes
	}

	s := &jobsearchServer{
		logger:                  srv.Logger,
		client:                  caller,
		defaultMaxResponseBytes: maxResponseBytes,
		tracer:                  srv.Tracer,
	}

	apiURL := ""
	hasCloudflareAccess := false
	if client != nil {
		apiURL = client.baseURL
		hasCloudflareAccess = client.hasCloudflareAccess
	}
	srv.Logger.Info("starting server", "name", "mcp-jobsearch", "version", version, "api_url", apiURL, "cloudflare_access", hasCloudflareAccess)

	registerCoreTools(srv.Server, s)
	registerResumeTools(srv.Server, s)
	registerWorkflowCRMTools(srv.Server, s)
	registerPassthroughTools(srv.Server, s)

	return srv, cleanup, nil
}

type unconfiguredJobsearchClient struct{}

func (unconfiguredJobsearchClient) Request(context.Context, requestOptions) (*jobsearchResponse, error) {
	return nil, mcperror.NotConfigured("JOBSEARCH_API_URL", "set JOBSEARCH_API_URL environment variable")
}
