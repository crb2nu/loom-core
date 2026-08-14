package main

import (
	"context"

	"gitlab.flexinfer.ai/libs/mcp-go"

	"github.com/crb2nu/loom/pkg/agentcontext"

	"go.opentelemetry.io/otel/trace"
)

// registerBridgeTools registers the cross-vendor bridge: direct messages
// between agents and unified list/search over vendor CLI session
// transcripts (Claude Code + Codex) on this host.
func registerBridgeTools(server *mcp.Server, svc *agentcontext.Service, tracer trace.Tracer) {
	server.AddTool(mcp.Tool{
		Name:        "agent_message_send",
		Description: "Send a direct message to another agent (cross-vendor: claude-code <-> codex). Stored durably for inbox pickup; also nudged live to a heartbeating recipient when possible. For handing off work with session context, use agent_handoff_create instead.",
		InputSchema: mcp.InputSchema{
			Type: "object",
			Properties: map[string]any{
				"to_agent_id": map[string]any{
					"type":        "string",
					"description": "Recipient agent id (e.g. claude-code, codex).",
				},
				"body": map[string]any{
					"type":        "string",
					"description": "Message body (max 32KB).",
				},
				"from_agent_id": map[string]any{
					"type":        "string",
					"description": "Sender agent id (default: this agent's configured id).",
				},
				"subject": map[string]any{
					"type":        "string",
					"description": "Optional short subject line.",
				},
				"session_ref": map[string]any{
					"type":        "string",
					"description": "Optional session reference (agent-context session id or vendor session id) the message is about.",
				},
				"expires_hours": map[string]any{
					"type":        "integer",
					"description": "Hours until the message expires unread (default: 168).",
				},
			},
			Required: []string{"to_agent_id", "body"},
		},
	}, traced(tracer, "agent_message_send", func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		return svc.HandleMessageSend(ctx, args)
	}))

	server.AddTool(mcp.Tool{
		Name:        "agent_message_inbox",
		Description: "List direct messages addressed to an agent (newest first). Returned unread messages are marked read unless mark_read=false.",
		InputSchema: mcp.InputSchema{
			Type: "object",
			Properties: map[string]any{
				"agent_id": map[string]any{
					"type":        "string",
					"description": "Agent id whose inbox to read (default: this agent's configured id).",
				},
				"include_read": map[string]any{
					"type":        "boolean",
					"description": "Include already-read messages (default: false).",
				},
				"mark_read": map[string]any{
					"type":        "boolean",
					"description": "Mark returned unread messages as read (default: true).",
				},
				"limit": map[string]any{
					"type":        "integer",
					"description": "Maximum messages to return (default: 20, max: 100).",
				},
			},
		},
	}, traced(tracer, "agent_message_inbox", func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		return svc.HandleMessageInbox(ctx, args)
	}))

	server.AddTool(mcp.Tool{
		Name:        "agent_vendor_session_list",
		Description: "List local vendor CLI sessions (Claude Code ~/.claude/projects, Codex ~/.codex/sessions) newest first, with cwd/source metadata. Cross-vendor: lets one agent see the other vendor's recent sessions on this host.",
		InputSchema: mcp.InputSchema{
			Type: "object",
			Properties: map[string]any{
				"vendor": map[string]any{
					"type":        "string",
					"enum":        []string{"claude", "codex"},
					"description": "Restrict to one vendor (default: both).",
				},
				"cwd_contains": map[string]any{
					"type":        "string",
					"description": "Keep sessions whose working directory contains this path fragment (e.g. services/loom-core).",
				},
				"since_hours": map[string]any{
					"type":        "integer",
					"description": "Only sessions modified within the last N hours.",
				},
				"limit": map[string]any{
					"type":        "integer",
					"description": "Maximum sessions to return (default: 50, max: 500).",
				},
			},
		},
	}, traced(tracer, "agent_vendor_session_list", func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		return svc.HandleVendorSessionList(ctx, args)
	}))

	server.AddTool(mcp.Tool{
		Name:        "agent_vendor_session_search",
		Description: "Search local vendor CLI session transcripts (Claude Code + Codex) for a substring. Returns per-match session id, vendor, cwd, line, role, and a snippet. Use to find what another agent's session discussed or decided.",
		InputSchema: mcp.InputSchema{
			Type: "object",
			Properties: map[string]any{
				"query": map[string]any{
					"type":        "string",
					"description": "Case-insensitive substring to search for.",
				},
				"vendor": map[string]any{
					"type":        "string",
					"enum":        []string{"claude", "codex"},
					"description": "Restrict to one vendor (default: both).",
				},
				"cwd_contains": map[string]any{
					"type":        "string",
					"description": "Keep sessions whose working directory contains this path fragment.",
				},
				"since_hours": map[string]any{
					"type":        "integer",
					"description": "Only sessions modified within the last N hours.",
				},
				"limit": map[string]any{
					"type":        "integer",
					"description": "Maximum transcripts to scan, newest first (default: 50).",
				},
				"max_results": map[string]any{
					"type":        "integer",
					"description": "Maximum total matches (default: 30, max: 200).",
				},
				"max_per_session": map[string]any{
					"type":        "integer",
					"description": "Maximum matches per transcript (default: 3, max: 20).",
				},
			},
			Required: []string{"query"},
		},
	}, traced(tracer, "agent_vendor_session_search", func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		return svc.HandleVendorSessionSearch(ctx, args)
	}))

	server.AddTool(mcp.Tool{
		Name:        "agent_vendor_session_tail",
		Description: "Read the bounded, parsed tail of one local Claude Code or Codex transcript in chronological order.",
		InputSchema: mcp.InputSchema{
			Type: "object",
			Properties: map[string]any{
				"vendor": map[string]any{"type": "string", "enum": []string{"claude", "codex"}},
				"id":     map[string]any{"type": "string"},
				"lines":  map[string]any{"type": "integer", "description": "Lines to return (default 200, max 500)."},
			},
			Required: []string{"vendor", "id"},
		},
	}, traced(tracer, "agent_vendor_session_tail", func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		return svc.HandleVendorSessionTail(ctx, args)
	}))

	server.AddTool(mcp.Tool{
		Name:        "agent_mr_status",
		Description: "Report the branch->merge-request awareness status from the HUD mrwatch registry: every open MR whose source branch matches, each classified into a stall taxonomy (ok, awaiting_pipeline, ci_running, ci_failed_flaky, ci_failed_deterministic, conflict, automerge_unarmed, pipeline_skipped, stale_branch, draft_idle). Use to check whether an MR you (or another session) opened has stalled. When the HUD is unreachable it returns available=false rather than erroring.",
		InputSchema: mcp.InputSchema{
			Type: "object",
			Properties: map[string]any{
				"branch": map[string]any{
					"type":        "string",
					"description": "Source branch to query (e.g. feat/my-feature).",
				},
				"repo": map[string]any{
					"type":        "string",
					"description": "Optional GitLab project path to narrow to a single repo (e.g. services/loom-core).",
				},
			},
			Required: []string{"branch"},
		},
	}, traced(tracer, "agent_mr_status", func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		return svc.HandleMRStatus(ctx, args)
	}))
}
