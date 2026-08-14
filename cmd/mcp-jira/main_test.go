package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/crb2nu/loom/pkg/mcperror"
	"gitlab.flexinfer.ai/libs/mcp-go"
	"go.opentelemetry.io/otel/trace/noop"
)

func TestGetClient_MissingConfig(t *testing.T) {
	tests := []struct {
		name     string
		server   *jiraServer
		wantHint string
	}{
		{
			name:     "missing url",
			server:   &jiraServer{username: "user@example.com", apiToken: "token"},
			wantHint: "JIRA_URL",
		},
		{
			name:     "missing username",
			server:   &jiraServer{jiraURL: "https://example.atlassian.net", apiToken: "token"},
			wantHint: "JIRA_USERNAME",
		},
		{
			name:     "missing token",
			server:   &jiraServer{jiraURL: "https://example.atlassian.net", username: "user@example.com"},
			wantHint: "JIRA_API_TOKEN",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.server.getClient()
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			mcpErr, ok := err.(*mcperror.Error)
			if !ok {
				t.Fatalf("expected *mcperror.Error, got %T", err)
			}
			if mcpErr.Code != mcperror.CodeServerError {
				t.Fatalf("expected code %q, got %q", mcperror.CodeServerError, mcpErr.Code)
			}
			if mcpErr.Details == nil {
				t.Fatal("expected error details")
			}
			details, ok := mcpErr.Details.(map[string]string)
			if !ok {
				t.Fatalf("expected map[string]string details, got %T", mcpErr.Details)
			}
			if details["config"] != tc.wantHint {
				t.Fatalf("expected config detail %q, got %q", tc.wantHint, details["config"])
			}
		})
	}
}

func TestGetClient_InvalidURL(t *testing.T) {
	srv := &jiraServer{
		jiraURL:  "://bad-url",
		username: "user@example.com",
		apiToken: "token",
	}

	_, err := srv.getClient()
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	mcpErr, ok := err.(*mcperror.Error)
	if !ok {
		t.Fatalf("expected *mcperror.Error, got %T", err)
	}
	if mcpErr.Code != mcperror.CodeInvalidInput {
		t.Fatalf("expected code %q, got %q", mcperror.CodeInvalidInput, mcpErr.Code)
	}
}

func registeredToolNames(t *testing.T, writesEnabled bool) map[string]bool {
	t.Helper()
	server := mcp.NewServer("mcp-jira-test", "0.0.0")
	registerTools(server, &jiraServer{}, noop.NewTracerProvider().Tracer("test"), writesEnabled)

	names := map[string]bool{}
	for _, tool := range server.Tools() {
		names[tool.Name] = true
	}
	return names
}

func TestRegisterTools_ReadOnlyByDefault(t *testing.T) {
	names := registeredToolNames(t, false)

	for _, want := range []string{
		"jira_get_issue",
		"jira_search",
		"jira_list_projects",
		"jira_get_project",
		"jira_myself",
		"jira_get_transitions",
	} {
		if !names[want] {
			t.Errorf("expected read tool %q to be registered", want)
		}
	}

	for _, writeTool := range []string{"jira_add_comment", "jira_transition_issue"} {
		if names[writeTool] {
			t.Errorf("write tool %q must not be registered without JIRA_MCP_WRITE_ENABLED", writeTool)
		}
	}
}

func TestRegisterTools_WritesEnabled(t *testing.T) {
	names := registeredToolNames(t, true)

	for _, writeTool := range []string{"jira_add_comment", "jira_transition_issue"} {
		if !names[writeTool] {
			t.Errorf("expected write tool %q to be registered when writes are enabled", writeTool)
		}
	}
}

func TestSearchIssues_UsesJQLEndpointByDefault(t *testing.T) {
	var gotPath, gotFields string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotFields = r.URL.Query().Get("fields")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"issues":[{"key":"PROJ-1","fields":{"summary":"first","status":{"name":"Done"}}}]}`))
	}))
	defer ts.Close()

	srv := &jiraServer{jiraURL: ts.URL, username: "u@example.com", apiToken: "token"}
	rows, err := srv.searchIssues(context.Background(), "ORDER BY updated DESC", 10)
	if err != nil {
		t.Fatalf("searchIssues: %v", err)
	}

	if gotPath != "/rest/api/2/search/jql" {
		t.Fatalf("expected Cloud JQL endpoint, got %q", gotPath)
	}
	if gotFields == "" {
		t.Fatal("expected explicit fields param (JQL endpoint returns only ids by default)")
	}
	if len(rows) != 1 || rows[0]["key"] != "PROJ-1" || rows[0]["status"] != "Done" {
		t.Fatalf("unexpected rows: %#v", rows)
	}
	// The fixture has no priority/assignee — mapping must not panic and
	// must omit the fields rather than fabricate them.
	if _, ok := rows[0]["priority"]; ok {
		t.Fatalf("expected no priority key for issue without priority: %#v", rows[0])
	}
}

func TestSearchIssues_LegacyEnvUsesOldEndpoint(t *testing.T) {
	var gotPath string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"issues":[]}`))
	}))
	defer ts.Close()

	srv := &jiraServer{jiraURL: ts.URL, username: "u@example.com", apiToken: "token", legacySearch: true}
	if _, err := srv.searchIssues(context.Background(), "project = X", 10); err != nil {
		t.Fatalf("searchIssues: %v", err)
	}

	if gotPath != "/rest/api/2/search" {
		t.Fatalf("expected legacy search endpoint, got %q", gotPath)
	}
}

func TestGetClient_CachesClient(t *testing.T) {
	srv := &jiraServer{
		jiraURL:  "https://example.atlassian.net",
		username: "user@example.com",
		apiToken: "token",
	}

	first, err := srv.getClient()
	if err != nil {
		t.Fatalf("getClient first call: %v", err)
	}
	second, err := srv.getClient()
	if err != nil {
		t.Fatalf("getClient second call: %v", err)
	}

	if first != second {
		t.Fatal("expected cached client pointer to be reused")
	}
}
