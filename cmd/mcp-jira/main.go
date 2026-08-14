package main

import (
	"context"
	"fmt"
	"os"
	"sync"

	"github.com/andygrunwald/go-jira"
	"gitlab.flexinfer.ai/libs/mcp-go"

	"go.opentelemetry.io/otel/trace"

	"github.com/crb2nu/loom/internal/loomconcurrency"
	"github.com/crb2nu/loom/pkg/lifecycle"
	"github.com/crb2nu/loom/pkg/mcperror"
	"github.com/crb2nu/loom/pkg/mcplog"
	"github.com/crb2nu/loom/pkg/mcpotel"
	"github.com/crb2nu/loom/pkg/validate"
)

var (
	version = "0.2.0"
)

type jiraServer struct {
	jiraURL  string
	username string
	apiToken string
	// legacySearch selects the pre-Cloud /rest/api/2/search endpoint.
	// Atlassian Cloud removed it in 2025; only Server/DC still serves it.
	legacySearch bool

	mu     sync.Mutex
	client *jira.Client
}

func main() {
	if err := lifecycle.RunWithSignals(context.Background(), run); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	logger := mcplog.NewDefault()
	tp, shutdownTracer, err := mcpotel.InitTracer(ctx, "mcp-jira", logger)
	if err != nil {
		logger.Warn("OTel tracer init failed", "error", err)
	}
	defer func() { _ = shutdownTracer(ctx) }()
	tracer := mcpotel.Tracer(tp, "mcp-jira")

	srv := &jiraServer{
		jiraURL:      os.Getenv("JIRA_URL"),
		username:     os.Getenv("JIRA_USERNAME"),
		apiToken:     os.Getenv("JIRA_API_TOKEN"),
		legacySearch: os.Getenv("JIRA_LEGACY_SEARCH") == "1",
	}
	writesEnabled := os.Getenv("JIRA_MCP_WRITE_ENABLED") == "1"

	logger.Info("starting server", "name", "mcp-jira", "version", version, "url", srv.jiraURL, "writes_enabled", writesEnabled)

	server := mcp.NewServer("mcp-jira", version)
	loomconcurrency.Apply(server)
	server.SetInstructions("Read Jira issues, projects, and the authenticated user (read-only by default; write tools require JIRA_MCP_WRITE_ENABLED=1). Requires JIRA_URL, JIRA_USERNAME, and JIRA_API_TOKEN. Set JIRA_LEGACY_SEARCH=1 only for Jira Server/DC instances.")

	registerTools(server, srv, tracer, writesEnabled)

	return server.Run(ctx)
}

func (s *jiraServer) getClient() (*jira.Client, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.client != nil {
		return s.client, nil
	}

	if s.jiraURL == "" {
		return nil, mcperror.NotConfigured("JIRA_URL", "set JIRA_URL environment variable")
	}
	if s.username == "" {
		return nil, mcperror.NotConfigured("JIRA_USERNAME", "set JIRA_USERNAME environment variable")
	}
	if s.apiToken == "" {
		return nil, mcperror.NotConfigured("JIRA_API_TOKEN", "set JIRA_API_TOKEN environment variable")
	}

	tp := jira.BasicAuthTransport{
		Username: s.username,
		Password: s.apiToken,
	}
	client, err := jira.NewClient(tp.Client(), s.jiraURL)
	if err != nil {
		return nil, mcperror.InvalidParam("JIRA_URL", fmt.Sprintf("invalid base URL: %v", err))
	}

	s.client = client
	return s.client, nil
}

// searchIssues runs a JQL search and returns a simplified issue list.
// Cloud instances only serve /rest/api/2/search/jql; legacySearch keeps
// the removed /rest/api/2/search endpoint available for Server/DC.
func (s *jiraServer) searchIssues(ctx context.Context, jql string, limit int) ([]map[string]any, error) {
	client, err := s.getClient()
	if err != nil {
		return nil, err
	}

	var issues []jira.Issue
	if s.legacySearch {
		issues, _, err = client.Issue.SearchWithContext(ctx, jql, &jira.SearchOptions{
			MaxResults: limit,
		})
	} else {
		issues, _, err = client.Issue.SearchV2JQLWithContext(ctx, jql, &jira.SearchOptionsV2{
			MaxResults: limit,
			// The JQL endpoint returns only ids unless fields are named.
			Fields: []string{"summary", "status", "priority", "assignee"},
		})
	}
	if err != nil {
		return nil, err
	}

	simplified := make([]map[string]any, 0, len(issues))
	for _, i := range issues {
		row := map[string]any{"key": i.Key}
		if i.Fields != nil {
			row["summary"] = i.Fields.Summary
			if i.Fields.Status != nil {
				row["status"] = i.Fields.Status.Name
			}
			if i.Fields.Priority != nil {
				row["priority"] = i.Fields.Priority.Name
			}
			if i.Fields.Assignee != nil {
				row["assignee"] = i.Fields.Assignee.DisplayName
			}
		}
		simplified = append(simplified, row)
	}
	return simplified, nil
}

func registerTools(server *mcp.Server, srv *jiraServer, tracer trace.Tracer, writesEnabled bool) {
	wrap := func(name string, h mcp.ToolHandler) mcp.ToolHandler {
		return mcpotel.TracedToolHandler(tracer, name, h)
	}

	registerReadTools(server, srv, wrap)
	if writesEnabled {
		registerWriteTools(server, srv, wrap)
	}
}

func registerReadTools(server *mcp.Server, srv *jiraServer, wrap func(string, mcp.ToolHandler) mcp.ToolHandler) {
	server.AddTool(mcp.Tool{
		Name:        "jira_get_issue",
		Description: "Get details of a Jira issue",
		InputSchema: mcp.InputSchema{
			Type: "object",
			Properties: map[string]any{
				"issue_key": map[string]any{"type": "string", "description": "The issue key (e.g. PROJ-123)"},
			},
			Required: []string{"issue_key"},
		},
	}, wrap("jira_get_issue", func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		v := validate.NewArgs(args)
		key := v.Required("issue_key")
		if err := v.Validate(); err != nil {
			return mcp.ErrorResult(err), nil
		}

		client, err := srv.getClient()
		if err != nil {
			return mcp.ErrorResult(err), nil
		}

		issue, _, err := client.Issue.GetWithContext(ctx, key, nil)
		if err != nil {
			return mcp.ErrorResult(err), nil
		}

		return mcp.JSONResult(issue)
	}))

	server.AddTool(mcp.Tool{
		Name:        "jira_search",
		Description: "Search Jira issues using JQL",
		InputSchema: mcp.InputSchema{
			Type: "object",
			Properties: map[string]any{
				"jql":   map[string]any{"type": "string", "description": "JQL query string"},
				"limit": map[string]any{"type": "integer", "description": "Max results (default 50)"},
			},
			Required: []string{"jql"},
		},
	}, wrap("jira_search", func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		v := validate.NewArgs(args)
		jql := v.Required("jql")
		limit := validate.NormalizePerPage(v.Int("limit", 50), 50, 200)
		if err := v.Validate(); err != nil {
			return mcp.ErrorResult(err), nil
		}

		simplified, err := srv.searchIssues(ctx, jql, limit)
		if err != nil {
			return mcp.ErrorResult(err), nil
		}

		return mcp.JSONResult(simplified)
	}))

	server.AddTool(mcp.Tool{
		Name:        "jira_list_projects",
		Description: "List Jira projects visible to the authenticated user",
		InputSchema: mcp.InputSchema{
			Type:       "object",
			Properties: map[string]any{},
		},
	}, wrap("jira_list_projects", func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		client, err := srv.getClient()
		if err != nil {
			return mcp.ErrorResult(err), nil
		}

		projects, _, err := client.Project.GetListWithContext(ctx)
		if err != nil {
			return mcp.ErrorResult(err), nil
		}

		var simplified []map[string]any
		if projects != nil {
			for _, p := range *projects {
				simplified = append(simplified, map[string]any{
					"id":   p.ID,
					"key":  p.Key,
					"name": p.Name,
					"type": p.ProjectTypeKey,
				})
			}
		}

		return mcp.JSONResult(simplified)
	}))

	server.AddTool(mcp.Tool{
		Name:        "jira_get_project",
		Description: "Get details of a Jira project",
		InputSchema: mcp.InputSchema{
			Type: "object",
			Properties: map[string]any{
				"project_key": map[string]any{"type": "string", "description": "Project key or ID (e.g. PROJ)"},
			},
			Required: []string{"project_key"},
		},
	}, wrap("jira_get_project", func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		v := validate.NewArgs(args)
		key := v.Required("project_key")
		if err := v.Validate(); err != nil {
			return mcp.ErrorResult(err), nil
		}

		client, err := srv.getClient()
		if err != nil {
			return mcp.ErrorResult(err), nil
		}

		project, _, err := client.Project.GetWithContext(ctx, key)
		if err != nil {
			return mcp.ErrorResult(err), nil
		}

		return mcp.JSONResult(project)
	}))

	server.AddTool(mcp.Tool{
		Name:        "jira_myself",
		Description: "Get the authenticated Jira user (auth/connectivity probe)",
		InputSchema: mcp.InputSchema{
			Type:       "object",
			Properties: map[string]any{},
		},
	}, wrap("jira_myself", func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		client, err := srv.getClient()
		if err != nil {
			return mcp.ErrorResult(err), nil
		}

		user, _, err := client.User.GetSelfWithContext(ctx)
		if err != nil {
			return mcp.ErrorResult(err), nil
		}

		return mcp.JSONResult(map[string]any{
			"account_id":   user.AccountID,
			"display_name": user.DisplayName,
			"email":        user.EmailAddress,
			"time_zone":    user.TimeZone,
			"active":       user.Active,
		})
	}))

	server.AddTool(mcp.Tool{
		Name:        "jira_get_transitions",
		Description: "Get available transitions for an issue",
		InputSchema: mcp.InputSchema{
			Type: "object",
			Properties: map[string]any{
				"issue_key": map[string]any{"type": "string"},
			},
			Required: []string{"issue_key"},
		},
	}, wrap("jira_get_transitions", func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		v := validate.NewArgs(args)
		key := v.Required("issue_key")
		if err := v.Validate(); err != nil {
			return mcp.ErrorResult(err), nil
		}

		client, err := srv.getClient()
		if err != nil {
			return mcp.ErrorResult(err), nil
		}

		transitions, _, err := client.Issue.GetTransitionsWithContext(ctx, key)
		if err != nil {
			return mcp.ErrorResult(err), nil
		}

		return mcp.JSONResult(transitions)
	}))
}

func registerWriteTools(server *mcp.Server, srv *jiraServer, wrap func(string, mcp.ToolHandler) mcp.ToolHandler) {
	server.AddTool(mcp.Tool{
		Name:        "jira_add_comment",
		Description: "Add a comment to an issue",
		InputSchema: mcp.InputSchema{
			Type: "object",
			Properties: map[string]any{
				"issue_key": map[string]any{"type": "string"},
				"body":      map[string]any{"type": "string"},
			},
			Required: []string{"issue_key", "body"},
		},
	}, wrap("jira_add_comment", func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		v := validate.NewArgs(args)
		key := v.Required("issue_key")
		body := v.Required("body")
		if err := v.Validate(); err != nil {
			return mcp.ErrorResult(err), nil
		}

		client, err := srv.getClient()
		if err != nil {
			return mcp.ErrorResult(err), nil
		}

		comment := &jira.Comment{
			Body: body,
		}

		added, _, err := client.Issue.AddCommentWithContext(ctx, key, comment)
		if err != nil {
			return mcp.ErrorResult(err), nil
		}

		return mcp.JSONResult(added)
	}))

	server.AddTool(mcp.Tool{
		Name:        "jira_transition_issue",
		Description: "Transition an issue to a new status",
		InputSchema: mcp.InputSchema{
			Type: "object",
			Properties: map[string]any{
				"issue_key":     map[string]any{"type": "string"},
				"transition_id": map[string]any{"type": "string", "description": "ID of the transition to perform"},
			},
			Required: []string{"issue_key", "transition_id"},
		},
	}, wrap("jira_transition_issue", func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		v := validate.NewArgs(args)
		key := v.Required("issue_key")
		transID := v.Required("transition_id")
		if err := v.Validate(); err != nil {
			return mcp.ErrorResult(err), nil
		}

		client, err := srv.getClient()
		if err != nil {
			return mcp.ErrorResult(err), nil
		}

		_, err = client.Issue.DoTransitionWithContext(ctx, key, transID)
		if err != nil {
			return mcp.ErrorResult(err), nil
		}

		return mcp.TextResult(fmt.Sprintf("Transited issue %s", key)), nil
	}))
}
