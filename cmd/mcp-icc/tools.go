package main

import (
	"fmt"

	"gitlab.flexinfer.ai/libs/mcp-go"

	"github.com/crb2nu/loom/internal/iccclient"
	"github.com/crb2nu/loom/pkg/mcpscaffold"
)

// registerTools wires up every tool exposed by mcp-icc. Read tools
// are unconditionally registered. Write tools are registered with the
// withWriteGate wrapper so they return "writes_disabled" when
// ICC_MCP_WRITE_ENABLED=1 is not set — the registration happens
// either way so the tool list stays stable across the gate setting.
//
// Tool counts to maintain in sync with README + registry entry:
//   - 20 read tools
//   - 16 write tools (10 create + 4 update + 5 transition + others)
//   - 1 convenience tool (icc_quick_capture)
func registerTools(srv *mcpscaffold.Server, icc *iccclient.Client, writesEnabled bool) {
	registerReadTools(srv, icc)
	registerWriteTools(srv, icc, writesEnabled)
	registerConvenienceTools(srv, icc, writesEnabled)
}

// registerReadTools wires up the ~20 read tools. Each one is a
// thin wrapper around an ICC GET endpoint that already exists; the
// MCP tool's job is to surface a typed contract and let the MCP
// caller stop hand-rolling HTTP.
func registerReadTools(srv *mcpscaffold.Server, icc *iccclient.Client) {
	// Project-level reads.
	srv.AddTracedTool(mcp.Tool{
		Name:        "icc_project_list",
		Description: "List all ICC projects with per-project rollups (artifact counts, open action items, blocked, milestones at risk, deliverables in review, last activity). Backed by /api/projects/overview.",
		InputSchema: projectListSchema(),
	}, makeProjectListHandler(icc))

	srv.AddTracedTool(mcp.Tool{
		Name:        "icc_project_brief",
		Description: "Single-project brief view: synthesized project narrative + headline metrics. Backed by /api/project-brief.",
		InputSchema: projectBriefSchema(),
	}, makeProjectBriefHandler(icc))

	srv.AddTracedTool(mcp.Tool{
		Name:        "icc_project_kanban",
		Description: "Kanban-shape view of a project's action items grouped by status. Backed by /api/projects/<id>/kanban.",
		InputSchema: projectScopedSchema(nil),
	}, makeProjectScopedReader(icc, "project_kanban", "kanban", "include_done"))

	srv.AddTracedTool(mcp.Tool{
		Name:        "icc_project_calendar",
		Description: "Calendar-shape view of milestones + deliverables due dates for a project. Backed by /api/projects/<id>/calendar.",
		InputSchema: projectScopedSchema(map[string]any{
			"from": map[string]any{"type": "string", "description": "ISO date lower bound"},
			"to":   map[string]any{"type": "string", "description": "ISO date upper bound"},
		}),
	}, makeProjectScopedReader(icc, "project_calendar", "calendar", "from", "to"))

	srv.AddTracedTool(mcp.Tool{
		Name:        "icc_project_gantt",
		Description: "Gantt-shape view of milestones + deliverables for a project (start/end ranges, dependency edges). Backed by /api/projects/<id>/gantt.",
		InputSchema: projectScopedSchema(nil),
	}, makeProjectScopedReader(icc, "project_gantt", "gantt"))

	srv.AddTracedTool(mcp.Tool{
		Name:        "icc_project_status",
		Description: "Single-project status panel: RAG, last status update, executive summary. Backed by /api/projects/<id>/status.",
		InputSchema: projectScopedSchema(nil),
	}, makeProjectScopedReader(icc, "project_status", "status"))

	srv.AddTracedTool(mcp.Tool{
		Name:        "icc_project_changes",
		Description: "Time-ordered audit feed of changes for a project (mutations to artifacts, action items, decisions, etc). Backed by /api/projects/<id>/changes.",
		InputSchema: projectScopedSchema(map[string]any{
			"since": map[string]any{"type": "string", "description": "ISO timestamp lower bound"},
			"limit": map[string]any{"type": "string", "description": "Max rows (string-encoded int)"},
		}),
	}, makeProjectScopedReader(icc, "project_changes", "changes", "since", "limit"))

	srv.AddTracedTool(mcp.Tool{
		Name:        "icc_project_blocked",
		Description: "Blocker rollup for a project (blocked action items + at-risk milestones + waiting deliverables). Backed by /api/projects/<id>/blocked.",
		InputSchema: projectScopedSchema(nil),
	}, makeProjectScopedReader(icc, "project_blocked", "blocked"))

	// Entity list reads.
	srv.AddTracedTool(mcp.Tool{
		Name:        "icc_action_item_list",
		Description: "List action items with optional filters (project_id, vendor_id, status, owner, include_done). Backed by /api/action-items.",
		InputSchema: listToolSchema("project_id", "vendor_id", "status", "owner", "include_done"),
	}, makeListReader(icc, "action_item_list", "/api/action-items",
		"project_id", "vendor_id", "status", "owner", "include_done"))

	srv.AddTracedTool(mcp.Tool{
		Name:        "icc_decision_list",
		Description: "List decisions with optional filters (project_id, vendor_id). Backed by /api/decisions.",
		InputSchema: listToolSchema("project_id", "vendor_id"),
	}, makeListReader(icc, "decision_list", "/api/decisions", "project_id", "vendor_id"))

	srv.AddTracedTool(mcp.Tool{
		Name:        "icc_risk_list",
		Description: "List risks with optional filters (project_id, vendor_id, status, severity). Backed by /api/risks.",
		InputSchema: listToolSchema("project_id", "vendor_id", "status", "severity"),
	}, makeListReader(icc, "risk_list", "/api/risks",
		"project_id", "vendor_id", "status", "severity"))

	srv.AddTracedTool(mcp.Tool{
		Name:        "icc_milestone_list",
		Description: "List milestones with optional filters (project_id, status, risk). Backed by /api/milestones.",
		InputSchema: listToolSchema("project_id", "status", "risk"),
	}, makeListReader(icc, "milestone_list", "/api/milestones", "project_id", "status", "risk"))

	srv.AddTracedTool(mcp.Tool{
		Name:        "icc_deliverable_list",
		Description: "List deliverables with optional filters (project_id, status, owner). Backed by /api/deliverables.",
		InputSchema: listToolSchema("project_id", "status", "owner"),
	}, makeListReader(icc, "deliverable_list", "/api/deliverables", "project_id", "status", "owner"))

	srv.AddTracedTool(mcp.Tool{
		Name:        "icc_dependency_list",
		Description: "List dependencies (cross-entity dependency graph edges) with optional filter on source/target id. Backed by /api/dependencies.",
		InputSchema: listToolSchema("project_id", "source_id", "target_id"),
	}, makeListReader(icc, "dependency_list", "/api/dependencies",
		"project_id", "source_id", "target_id"))

	srv.AddTracedTool(mcp.Tool{
		Name:        "icc_workstream_list",
		Description: "List workstreams with optional project_id filter. Backed by /api/workstreams.",
		InputSchema: listToolSchema("project_id"),
	}, makeListReader(icc, "workstream_list", "/api/workstreams", "project_id"))

	srv.AddTracedTool(mcp.Tool{
		Name:        "icc_code_ref_list",
		Description: "List code_refs (raw notes / file pointers) with optional filters. Backed by /api/code/refs.",
		InputSchema: listToolSchema("project_id", "kind", "classification", "limit"),
	}, makeListReader(icc, "code_ref_list", "/api/code/refs",
		"project_id", "kind", "classification", "limit"))

	srv.AddTracedTool(mcp.Tool{
		Name:        "icc_session_link_list",
		Description: "List session_links (agent session → artifact/ref bindings). Backed by /api/sessions.",
		InputSchema: listToolSchema("project_id", "agent_id", "session_id", "limit"),
	}, makeListReader(icc, "session_link_list", "/api/sessions",
		"project_id", "agent_id", "session_id", "limit"))

	// Single-entity GET.
	srv.AddTracedTool(mcp.Tool{
		Name:        "icc_artifact_get",
		Description: "Fetch full detail for a single artifact (with links and provenance). Backed by /api/artifacts/<id>.",
		InputSchema: mcp.InputSchema{
			Type:     "object",
			Required: []string{"artifact_id"},
			Properties: map[string]any{
				"artifact_id": map[string]any{"type": "string"},
			},
		},
	}, makeGetByIDReader(icc, "artifact_get", "/api/artifacts", "artifact_id"))

	srv.AddTracedTool(mcp.Tool{
		Name:        "icc_artifact_links_list",
		Description: "List the links attached to a given artifact (URLs, code_refs, etc). Backed by /api/artifacts/<id>/links.",
		InputSchema: artifactLinksSchema(),
	}, makeArtifactLinksHandler(icc))

	// Search + needs-attention.
	srv.AddTracedTool(mcp.Tool{
		Name:        "icc_search",
		Description: "Search artifacts (FTS5 lexical or hybrid mode). PHI excluded unless include_phi=1 with a non-empty reason. Backed by /api/search.",
		InputSchema: searchSchema(),
	}, makeSearchHandler(icc))

	srv.AddTracedTool(mcp.Tool{
		Name:        "icc_needs_attention",
		Description: "Workspace-wide rollup of items that need a human (overdue action items, at-risk milestones, etc). Backed by /api/needs-attention.",
		InputSchema: mcp.InputSchema{Type: "object", Properties: map[string]any{}},
	}, makeNeedsAttentionHandler(icc))
}

// registerWriteTools wires up the ~16 write tools. Every handler is
// wrapped with withWriteGate(writesEnabled, ...) so the gate is the
// single source of truth — handler files don't have to re-check.
func registerWriteTools(srv *mcpscaffold.Server, icc *iccclient.Client, writesEnabled bool) {
	gated := func(h iccToolHandler) iccToolHandler {
		return withWriteGate(writesEnabled, h)
	}

	// --- canonical create set (10 tools) -------------------------------

	srv.AddTracedTool(mcp.Tool{
		Name:        "icc_project_create",
		Description: "Create a new ICC project. Payload follows the backend create_project schema (name, slug, kind, vendor_id, ...). Gated behind ICC_MCP_WRITE_ENABLED.",
		InputSchema: payloadSchema("Project create payload"),
	}, gated(makeCreateHandler(icc, "project_create", "/api/projects")))

	srv.AddTracedTool(mcp.Tool{
		Name:        "icc_artifact_create",
		Description: "Create a new artifact. Payload follows the backend create_artifact schema (project_id, title, summary, classification, kind, ...). Prefer icc_quick_capture for the code-snapshot workflow. Gated behind ICC_MCP_WRITE_ENABLED.",
		InputSchema: payloadSchema("Artifact create payload"),
	}, gated(makeCreateHandler(icc, "artifact_create", "/api/artifacts")))

	srv.AddTracedTool(mcp.Tool{
		Name:        "icc_action_item_create",
		Description: "Create a new action item. Payload follows the backend create_action_item schema (project_id, title, owner, due_date, status, ...). Gated behind ICC_MCP_WRITE_ENABLED.",
		InputSchema: payloadSchema("Action item create payload"),
	}, gated(makeCreateHandler(icc, "action_item_create", "/api/action-items")))

	srv.AddTracedTool(mcp.Tool{
		Name:        "icc_decision_create",
		Description: "Create a new decision. Payload follows the backend create_decision schema (project_id, title, rationale, decided_by, decided_at, ...). Gated behind ICC_MCP_WRITE_ENABLED.",
		InputSchema: payloadSchema("Decision create payload"),
	}, gated(makeCreateHandler(icc, "decision_create", "/api/decisions")))

	srv.AddTracedTool(mcp.Tool{
		Name:        "icc_risk_create",
		Description: "Create a new risk. Payload follows the backend create_risk schema (project_id, title, severity, likelihood, mitigation, ...). Gated behind ICC_MCP_WRITE_ENABLED.",
		InputSchema: payloadSchema("Risk create payload"),
	}, gated(makeCreateHandler(icc, "risk_create", "/api/risks")))

	srv.AddTracedTool(mcp.Tool{
		Name:        "icc_milestone_create",
		Description: "Create a new milestone. Payload follows the backend create_milestone schema (project_id, name, due_date, status, ...). Gated behind ICC_MCP_WRITE_ENABLED.",
		InputSchema: payloadSchema("Milestone create payload"),
	}, gated(makeCreateHandler(icc, "milestone_create", "/api/milestones")))

	srv.AddTracedTool(mcp.Tool{
		Name:        "icc_deliverable_create",
		Description: "Create a new deliverable. Payload follows the backend create_deliverable schema (project_id, title, owner, due_date, status, ...). Gated behind ICC_MCP_WRITE_ENABLED.",
		InputSchema: payloadSchema("Deliverable create payload"),
	}, gated(makeCreateHandler(icc, "deliverable_create", "/api/deliverables")))

	srv.AddTracedTool(mcp.Tool{
		Name:        "icc_dependency_create",
		Description: "Create a new cross-entity dependency edge. Payload follows the backend create_dependency schema (source_kind, source_id, target_kind, target_id, kind, ...). Gated behind ICC_MCP_WRITE_ENABLED.",
		InputSchema: payloadSchema("Dependency create payload"),
	}, gated(makeCreateHandler(icc, "dependency_create", "/api/dependencies")))

	srv.AddTracedTool(mcp.Tool{
		Name:        "icc_code_ref_create",
		Description: "Create a new code_ref (raw file/section pointer). Payload follows the backend create_code_ref schema (project_id, path, kind, classification, ...). Gated behind ICC_MCP_WRITE_ENABLED.",
		InputSchema: payloadSchema("Code ref create payload"),
	}, gated(makeCreateHandler(icc, "code_ref_create", "/api/code/refs")))

	srv.AddTracedTool(mcp.Tool{
		Name:        "icc_session_link_create",
		Description: "Create a new session_link (agent session → artifact/code_ref binding). Payload follows the backend create_session_link schema (session_id, agent_id, artifact_id|code_ref_id, ...). Gated behind ICC_MCP_WRITE_ENABLED.",
		InputSchema: payloadSchema("Session link create payload"),
	}, gated(makeCreateHandler(icc, "session_link_create", "/api/sessions")))

	// --- updates (4 highest-traffic) -----------------------------------

	srv.AddTracedTool(mcp.Tool{
		Name:        "icc_project_update",
		Description: "Update a project. Provide id (top-level required) + any fields to change. Gated behind ICC_MCP_WRITE_ENABLED.",
		InputSchema: payloadSchema("Project update payload", "id"),
	}, gated(makeIDPayloadHandler(icc, "project_update", "/api/projects/update", "id")))

	srv.AddTracedTool(mcp.Tool{
		Name:        "icc_artifact_update",
		Description: "Update an artifact. Provide id + fields to change. Gated behind ICC_MCP_WRITE_ENABLED.",
		InputSchema: payloadSchema("Artifact update payload", "id"),
	}, gated(makeIDPayloadHandler(icc, "artifact_update", "/api/artifacts/update", "id")))

	srv.AddTracedTool(mcp.Tool{
		Name:        "icc_action_item_update",
		Description: "Update an action item. Provide id + fields to change. Gated behind ICC_MCP_WRITE_ENABLED.",
		InputSchema: payloadSchema("Action item update payload", "id"),
	}, gated(makeIDPayloadHandler(icc, "action_item_update", "/api/action-items/update", "id")))

	srv.AddTracedTool(mcp.Tool{
		Name:        "icc_risk_update",
		Description: "Update a risk. Provide id + fields to change. Gated behind ICC_MCP_WRITE_ENABLED.",
		InputSchema: payloadSchema("Risk update payload", "id"),
	}, gated(makeIDPayloadHandler(icc, "risk_update", "/api/risks/update", "id")))

	// --- transitions (5 status-change endpoints) -----------------------

	srv.AddTracedTool(mcp.Tool{
		Name:        "icc_action_item_transition",
		Description: "Transition an action item between statuses. Payload: id, new_status, note (optional). Gated behind ICC_MCP_WRITE_ENABLED.",
		InputSchema: payloadSchema("Transition payload (id + new_status + optional note)", "id"),
	}, gated(makeIDPayloadHandler(icc, "action_item_transition", "/api/action-items/transition", "id")))

	srv.AddTracedTool(mcp.Tool{
		Name:        "icc_risk_transition",
		Description: "Transition a risk between statuses. Payload: id, new_status, note (optional). Gated behind ICC_MCP_WRITE_ENABLED.",
		InputSchema: payloadSchema("Transition payload (id + new_status + optional note)", "id"),
	}, gated(makeIDPayloadHandler(icc, "risk_transition", "/api/risks/transition", "id")))

	srv.AddTracedTool(mcp.Tool{
		Name:        "icc_milestone_transition",
		Description: "Transition a milestone between statuses. Payload: id, new_status, note (optional). Gated behind ICC_MCP_WRITE_ENABLED.",
		InputSchema: payloadSchema("Transition payload (id + new_status + optional note)", "id"),
	}, gated(makeIDPayloadHandler(icc, "milestone_transition", "/api/milestones/transition", "id")))

	srv.AddTracedTool(mcp.Tool{
		Name:        "icc_deliverable_transition",
		Description: "Transition a deliverable between statuses. Payload: id, new_status, note (optional). Gated behind ICC_MCP_WRITE_ENABLED.",
		InputSchema: payloadSchema("Transition payload (id + new_status + optional note)", "id"),
	}, gated(makeIDPayloadHandler(icc, "deliverable_transition", "/api/deliverables/transition", "id")))

	srv.AddTracedTool(mcp.Tool{
		Name:        "icc_extraction_transition",
		Description: "Transition an extraction run between statuses (pending → running → ok|failed). Payload: id, new_status. Gated behind ICC_MCP_WRITE_ENABLED.",
		InputSchema: payloadSchema("Extraction transition payload", "id"),
	}, gated(makeIDPayloadHandler(icc, "extraction_transition", "/api/extractions/transition", "id")))

	// --- artifact links (2) --------------------------------------------

	srv.AddTracedTool(mcp.Tool{
		Name:        "icc_artifact_link_add",
		Description: "Add a link to an existing artifact. Payload: artifact_id, link_kind, target (URL or code_ref id), label (optional). Gated behind ICC_MCP_WRITE_ENABLED.",
		InputSchema: payloadSchema("Artifact link add payload"),
	}, gated(makeCreateHandler(icc, "artifact_link_add", "/api/artifacts/links/add")))

	srv.AddTracedTool(mcp.Tool{
		Name:        "icc_artifact_link_remove",
		Description: "Remove a link from an artifact. Payload: artifact_id, link_id. Gated behind ICC_MCP_WRITE_ENABLED.",
		InputSchema: payloadSchema("Artifact link remove payload"),
	}, gated(makeCreateHandler(icc, "artifact_link_remove", "/api/artifacts/links/remove")))

	// --- artifact delete + reclassify ----------------------------------

	srv.AddTracedTool(mcp.Tool{
		Name:        "icc_artifact_delete",
		Description: "Soft-delete an artifact. Payload: id, reason (required by backend). Gated behind ICC_MCP_WRITE_ENABLED.",
		InputSchema: payloadSchema("Artifact delete payload"),
	}, gated(makeCreateHandler(icc, "artifact_delete", "/api/artifacts/delete")))

	srv.AddTracedTool(mcp.Tool{
		Name:        "icc_artifact_reclassify",
		Description: "Change an artifact's classification floor (e.g. possible_phi → confirmed_phi). Payload: id, classification, reason. Gated behind ICC_MCP_WRITE_ENABLED.",
		InputSchema: payloadSchema("Artifact reclassify payload"),
	}, gated(makeCreateHandler(icc, "artifact_reclassify", "/api/artifacts/reclassify")))

	// --- deliverable + dependency delete -------------------------------

	srv.AddTracedTool(mcp.Tool{
		Name:        "icc_deliverable_delete",
		Description: "Delete a deliverable. Payload: id. Gated behind ICC_MCP_WRITE_ENABLED.",
		InputSchema: payloadSchema("Deliverable delete payload"),
	}, gated(makeCreateHandler(icc, "deliverable_delete", "/api/deliverables/delete")))

	srv.AddTracedTool(mcp.Tool{
		Name:        "icc_dependency_delete",
		Description: "Delete a dependency edge. Payload: id. Gated behind ICC_MCP_WRITE_ENABLED.",
		InputSchema: payloadSchema("Dependency delete payload"),
	}, gated(makeCreateHandler(icc, "dependency_delete", "/api/dependencies/delete")))

	// --- code_ref + session_link updates/deletes -----------------------

	srv.AddTracedTool(mcp.Tool{
		Name:        "icc_code_ref_update",
		Description: "Update a code_ref. Provide id + fields to change. Gated behind ICC_MCP_WRITE_ENABLED.",
		InputSchema: payloadSchema("Code ref update payload", "id"),
	}, gated(makeIDPayloadHandler(icc, "code_ref_update", "/api/code/refs/update", "id")))

	srv.AddTracedTool(mcp.Tool{
		Name:        "icc_code_ref_delete",
		Description: "Delete a code_ref. Payload: id. Gated behind ICC_MCP_WRITE_ENABLED.",
		InputSchema: payloadSchema("Code ref delete payload"),
	}, gated(makeCreateHandler(icc, "code_ref_delete", "/api/code/refs/delete")))

	srv.AddTracedTool(mcp.Tool{
		Name:        "icc_session_link_update",
		Description: "Update a session_link. Provide id + fields to change. Gated behind ICC_MCP_WRITE_ENABLED.",
		InputSchema: payloadSchema("Session link update payload", "id"),
	}, gated(makeIDPayloadHandler(icc, "session_link_update", "/api/sessions/update", "id")))

	srv.AddTracedTool(mcp.Tool{
		Name:        "icc_session_link_delete",
		Description: "Delete a session_link. Payload: id. Gated behind ICC_MCP_WRITE_ENABLED.",
		InputSchema: payloadSchema("Session link delete payload"),
	}, gated(makeCreateHandler(icc, "session_link_delete", "/api/sessions/delete")))

	// --- artifact demote (URL-templated id) ----------------------------

	srv.AddTracedTool(mcp.Tool{
		Name:        "icc_artifact_demote",
		Description: "Soft-delete an artifact via the dedicated demote route (more audit-friendly than artifact_delete). Provide artifact_id + payload {reason, keep_code_ref}. Gated behind ICC_MCP_WRITE_ENABLED.",
		InputSchema: mcp.InputSchema{
			Type:     "object",
			Required: []string{"artifact_id", "payload"},
			Properties: map[string]any{
				"artifact_id": map[string]any{"type": "string"},
				"payload": map[string]any{
					"type":        "object",
					"description": "{reason, keep_code_ref}",
				},
			},
		},
	}, gated(makeIDInURLHandler(icc, "artifact_demote", "artifact_id",
		func(id string) string { return fmt.Sprintf("/api/artifacts/%s/demote", id) })))
}

// registerConvenienceTools wires up the small set of cross-cutting
// helpers — currently just icc_quick_capture (M53.5).
func registerConvenienceTools(srv *mcpscaffold.Server, icc *iccclient.Client, writesEnabled bool) {
	srv.AddTracedTool(mcp.Tool{
		Name:        "icc_quick_capture",
		Description: "Create an artifact with code_path + session_id populated from the caller's current context — pairs with Slice 5's artifact form fields. One tool call instead of artifact_create + link_add. Gated behind ICC_MCP_WRITE_ENABLED.",
		InputSchema: quickCaptureSchema(),
	}, withWriteGate(writesEnabled, makeQuickCaptureHandler(icc)))
}
