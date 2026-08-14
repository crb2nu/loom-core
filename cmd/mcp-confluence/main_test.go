package main

import (
	"context"
	"testing"

	"github.com/crb2nu/loom/pkg/httpclient"
	"github.com/crb2nu/loom/pkg/mcpscaffold"
)

func registeredToolNames(t *testing.T, writesEnabled bool) map[string]bool {
	t.Helper()

	srv, cleanup, err := mcpscaffold.NewServer(context.Background(), "mcp-confluence-test", "0.0.0")
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() { _ = cleanup(context.Background()) })

	cs := &confluenceServer{
		baseURL:    "https://example.atlassian.net/wiki",
		email:      "u@example.com",
		apiToken:   "token",
		httpClient: httpclient.NewDefault(),
	}
	registerTools(srv, cs, writesEnabled)

	names := map[string]bool{}
	for _, tool := range srv.Tools() {
		names[tool.Name] = true
	}
	return names
}

func TestRegisterTools_ReadOnlyByDefault(t *testing.T) {
	names := registeredToolNames(t, false)

	for _, want := range []string{
		"confluence_search",
		"confluence_get_page",
		"confluence_get_page_by_title",
		"confluence_list_spaces",
		"confluence_get_space",
		"confluence_list_pages",
		"confluence_get_children",
		"confluence_get_ancestors",
	} {
		if !names[want] {
			t.Errorf("expected read tool %q to be registered", want)
		}
	}

	for _, writeTool := range []string{"confluence_create_page", "confluence_update_page"} {
		if names[writeTool] {
			t.Errorf("write tool %q must not be registered without CONFLUENCE_MCP_WRITE_ENABLED", writeTool)
		}
	}
}

func TestRegisterTools_WritesEnabled(t *testing.T) {
	names := registeredToolNames(t, true)

	for _, writeTool := range []string{"confluence_create_page", "confluence_update_page"} {
		if !names[writeTool] {
			t.Errorf("expected write tool %q to be registered when writes are enabled", writeTool)
		}
	}
}
