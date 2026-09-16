// mcp-mills is the operator's hand on the Loom Mills factory: inspect the
// backlog and pipeline runs, post precision-grounded items, and apply
// revision-safe state transitions — with the operational guardrails learned
// from live burndowns baked in (never requeue over an existing implement
// branch; always echo the stored Revision; retry a 409 exactly once from a
// fresh read).
package main

import (
	"context"
	"strings"

	"gitlab.flexinfer.ai/libs/mcp-go"

	"github.com/crb2nu/loom/pkg/env"
	"github.com/crb2nu/loom/pkg/httpclient"
	"github.com/crb2nu/loom/pkg/lifecycle"
	"github.com/crb2nu/loom/pkg/mcpscaffold"
)

var version = "0.1.0"

// operatorTokenEnvs is the resolution order for the operator admin bearer.
// LOOM_MILLS_OPERATOR_TOKEN is the name the loomd / HUD environment already
// carries for this same credential; before it was accepted here the
// daemon-hosted server had no token and every mutation failed NotConfigured
// while the value sat one process up.
var operatorTokenEnvs = []string{"LOOM_MILLS_TOKEN", "LOOM_MILLS_OPERATOR_TOKEN", "LOOM_MILLS_ADMIN_TOKEN"}

func resolveOperatorToken() string { return env.StringChain(operatorTokenEnvs, "") }

type millsServer struct {
	api      *operatorClient
	brancher branchProber
}

func main() {
	lifecycle.RunWithSignals(context.Background(), run)
}

func run(ctx context.Context) error {
	cfg := httpclient.DefaultConfig()
	s := &millsServer{
		api: &operatorClient{
			base:   strings.TrimRight(env.String("LOOM_MILLS_OPERATOR_URL", "https://mills.flexinfer.ai"), "/"),
			token:  resolveOperatorToken(),
			client: httpclient.New(cfg),
			// Same resolution order as the mills-ops snapshot script.
			cfID:     env.StringChain([]string{"LOOM_MILLS_CF_ACCESS_ID", "CF_ACCESS_CLIENT_ID"}, ""),
			cfSecret: env.StringChain([]string{"LOOM_MILLS_CF_ACCESS_SECRET", "CF_ACCESS_CLIENT_SECRET"}, ""),
		},
		brancher: &gitLsRemoteProber{gitlabBase: strings.TrimRight(env.String("MILLS_GITLAB_BASE", "https://gitlab.flexinfer.ai"), "/")},
	}

	srv, cleanup, err := mcpscaffold.NewServer(ctx, "mcp-mills", version,
		mcpscaffold.WithInstructions("Loom Mills factory operations. Reads are tokenless; mutations need LOOM_MILLS_TOKEN or its aliases LOOM_MILLS_OPERATOR_TOKEN / LOOM_MILLS_ADMIN_TOKEN (the operator's own admin token from k8s secret loom-mills/loom-mills-admin — the HUD admin token is rejected). Requeueing an escalated item is refused while an implement branch exists: finish that branch, ready its MR, and let mills adopt-green-MR settle the item instead. For scope-gate escalations, mills_backlog_amend_scope widens files/tests and can requeue matching scope-rescue drafts."),
	)
	if err != nil {
		return err
	}
	defer func() { _ = cleanup(ctx) }()

	registerTools(srv, s)
	return srv.Run(ctx)
}

func registerTools(srv *mcpscaffold.Server, s *millsServer) {
	str := func(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
	integer := func(desc string) map[string]any { return map[string]any{"type": "integer", "description": desc} }
	boolean := func(desc string) map[string]any { return map[string]any{"type": "boolean", "description": desc} }

	srv.AddTracedTool(mcp.Tool{
		Name:        "mills_status",
		Description: "Operator posture: autonomy readiness + blockers, budgets, active runs, and any non-green capabilities.",
		InputSchema: mcp.InputSchema{Type: "object", Properties: map[string]any{}},
	}, s.handleStatus)

	srv.AddTracedTool(mcp.Tool{
		Name:        "mills_backlog_list",
		Description: "List backlog items as compact rows (id, state, priority, title, updated). Filter by state and/or id substring.",
		InputSchema: mcp.InputSchema{
			Type: "object",
			Properties: map[string]any{
				"state":       str("Filter: queued|running|escalated|merged|retired (empty = all)"),
				"id_contains": str("Filter: substring of the item ID"),
				"limit":       integer("Max rows returned (default 50, cap 200)"),
			},
		},
	}, s.handleBacklogList)

	srv.AddTracedTool(mcp.Tool{
		Name:        "mills_backlog_get",
		Description: "Full record for one backlog item (PascalCase wire form, including Revision, SpecDoc, and Slices).",
		InputSchema: mcp.InputSchema{
			Type:       "object",
			Properties: map[string]any{"id": str("Backlog item ID")},
			Required:   []string{"id"},
		},
	}, s.handleBacklogGet)

	srv.AddTracedTool(mcp.Tool{
		Name:        "mills_runs",
		Description: "Pipeline runs as compact rows (id, backlog, state, stage, failure/escalation class, MR iid, attempts). scope=active shows non-terminal runs; scope=terminal shows finished ones.",
		InputSchema: mcp.InputSchema{
			Type: "object",
			Properties: map[string]any{
				"scope":            str("active (default) or terminal"),
				"backlog_contains": str("Filter: substring of the run's backlog item ID"),
				"limit":            integer("Max rows returned (default 25, cap 200)"),
			},
		},
	}, s.handleRuns)

	srv.AddTracedTool(mcp.Tool{
		Name:        "mills_kpis",
		Description: "Headline KPI snapshot for a window (merge rate, escalations, gate pass rate, costs).",
		InputSchema: mcp.InputSchema{
			Type:       "object",
			Properties: map[string]any{"window": str("Rollup window, e.g. 1d (default) or 7d")},
		},
	}, s.handleKPIs)

	srv.AddTracedTool(mcp.Tool{
		Name:        "mills_escalation_diagnose",
		Description: "One-shot escalation triage for a backlog item: latest terminal run's failure fields plus a remote implement-branch check, ending in a requeue-vs-rescue verdict.",
		InputSchema: mcp.InputSchema{
			Type:       "object",
			Properties: map[string]any{"id": str("Backlog item ID")},
			Required:   []string{"id"},
		},
	}, s.handleDiagnose)

	srv.AddTracedTool(mcp.Tool{
		Name:        "mills_backlog_post",
		Description: "Create a backlog item (mutating; needs LOOM_MILLS_TOKEN). Slices files must name paths that exist on the target's main branch — fabricated grounding is the factory's top escalation cause. 409 means the ID already exists. Do not list changelog fragments; root-level files serialize the whole tree. Fragments are dropped and reported in dropped_files; broad envelopes are reported in warnings. Bare directories and trailing slashes are refused.",
		InputSchema: mcp.InputSchema{
			Type: "object",
			Properties: map[string]any{
				"id":             str("Deterministic item ID (convention: bl-<topic>-<yyyymm>)"),
				"title":          str("One-line title"),
				"spec_doc":       str("Markdown spec with Goal, Founding evidence, and Scope sections"),
				"priority":       str("P1|P2|P3 (default P2)"),
				"target_project": str("GitLab project path; empty = operator home repo (services/loom-core)"),
				"slice_name":     str("Slice name (default: the item id)"),
				"files":          map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Slice grounding: files the implementation touches (required, non-empty)"},
				"tests":          map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Test commands for the slice (optional)"},
				"labels":         map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Labels (optional)"},
			},
			Required: []string{"id", "title", "spec_doc", "files"},
		},
	}, s.handleBacklogPost)

	srv.AddTracedTool(mcp.Tool{
		Name:        "mills_backlog_amend_scope",
		Description: "Widen slice files and Success.tests with revision echo and one fresh retry on 409. Do not list changelog fragments; root-level files serialize the whole tree. Fragments are dropped and reported in dropped_files; merged-scope broad envelopes are reported in warnings. Bare directories and trailing slashes are refused. Optionally requeue after saving; existing implement branches require matching open scope-rescue draft MRs or force=true. Mutating; needs LOOM_MILLS_TOKEN.",
		InputSchema: mcp.InputSchema{Type: "object", Properties: map[string]any{
			"id":         str("Backlog item ID"),
			"add_files":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Files or file patterns to append; bare directories are refused"},
			"add_tests":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Test commands to append; at least one file or test is required"},
			"slice_name": str("Slice name; defaults to the first slice"),
			"requeue":    boolean("Start with requeue=1 after saving (default false)"),
			"force":      boolean("Override the implement-branch guard (default false)"),
		}, Required: []string{"id"}},
	}, s.handleAmendScope)

	srv.AddTracedTool(mcp.Tool{
		Name:        "mills_backlog_update_state",
		Description: "Revision-safe state transition (mutating; needs LOOM_MILLS_TOKEN): fresh GET, echo Revision, set State, POST; one retry from a re-read on 409. Requeue (state=queued) is REFUSED while an implement branch exists remotely unless force=true — finish the branch and let mills adopt the green MR instead.",
		InputSchema: mcp.InputSchema{
			Type: "object",
			Properties: map[string]any{
				"id":    str("Backlog item ID"),
				"state": str("Target state: queued|retired|merged"),
				"force": boolean("Override the implement-branch requeue guard (default false)"),
			},
			Required: []string{"id", "state"},
		},
	}, s.handleUpdateState)
}
