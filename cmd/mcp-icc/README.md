# mcp-icc

Broad MCP server for the Integration Command Center (ICC). Exposes the
full entity surface — projects, artifacts, action items, decisions,
risks, milestones, deliverables, dependencies, code refs, session links
— through ~20 read tools and ~25 write tools.

The narrow `mcp-icc-capture` server remains for the note-capture
workflow (Slack/email/meeting pastes); `mcp-icc` covers everything
else.

## Status

M53 (Slice 7) — first cut. Read tools enabled by default; write tools
gated behind `ICC_MCP_WRITE_ENABLED=1` so production deployments
can run with reads-only by default and opt into writes per-instance.

## Configuration

| Env var | Default | Purpose |
|---|---|---|
| `ICC_BASE_URL` | (empty) | Base URL of the ICC backend, e.g. `http://localhost:8765`. Required for all tools — they fail loud at call time when unset. |
| `ICC_API_URL` | (empty) | Historical fallback for `ICC_BASE_URL`. |
| `ICC_TIMEOUT_SECONDS` | `30` | HTTP client timeout. |
| `ICC_MCP_WRITE_ENABLED` | (unset) | Set to `1` to enable the write tools. Any other value (including `true`/`yes`) leaves writes disabled. |

## Tools

### Read tools (always enabled)

| Name | Backed by |
|---|---|
| `icc_project_list` | `GET /api/projects/overview` |
| `icc_project_brief` | `GET /api/project-brief?project_id=` |
| `icc_project_kanban` | `GET /api/projects/<id>/kanban` |
| `icc_project_calendar` | `GET /api/projects/<id>/calendar` |
| `icc_project_gantt` | `GET /api/projects/<id>/gantt` |
| `icc_project_status` | `GET /api/projects/<id>/status` |
| `icc_project_changes` | `GET /api/projects/<id>/changes` |
| `icc_project_blocked` | `GET /api/projects/<id>/blocked` |
| `icc_action_item_list` | `GET /api/action-items` |
| `icc_decision_list` | `GET /api/decisions` |
| `icc_risk_list` | `GET /api/risks` |
| `icc_milestone_list` | `GET /api/milestones` |
| `icc_deliverable_list` | `GET /api/deliverables` |
| `icc_dependency_list` | `GET /api/dependencies` |
| `icc_workstream_list` | `GET /api/workstreams` |
| `icc_code_ref_list` | `GET /api/code/refs` |
| `icc_session_link_list` | `GET /api/sessions` |
| `icc_artifact_get` | `GET /api/artifacts/<id>` |
| `icc_artifact_links_list` | `GET /api/artifacts/<id>/links` |
| `icc_search` | `GET /api/search` |
| `icc_needs_attention` | `GET /api/needs-attention` |

### Write tools (gated)

All write tools register unconditionally but return `writes_disabled`
when `ICC_MCP_WRITE_ENABLED!=1`. The gate fires before any network I/O.

Creates (10):
`icc_project_create`, `icc_artifact_create`, `icc_action_item_create`,
`icc_decision_create`, `icc_risk_create`, `icc_milestone_create`,
`icc_deliverable_create`, `icc_dependency_create`,
`icc_code_ref_create`, `icc_session_link_create`.

Updates (6):
`icc_project_update`, `icc_artifact_update`, `icc_action_item_update`,
`icc_risk_update`, `icc_code_ref_update`, `icc_session_link_update`.

Transitions (5):
`icc_action_item_transition`, `icc_risk_transition`,
`icc_milestone_transition`, `icc_deliverable_transition`,
`icc_extraction_transition`.

Deletes / soft-deletes (5):
`icc_artifact_delete`, `icc_deliverable_delete`,
`icc_dependency_delete`, `icc_code_ref_delete`,
`icc_session_link_delete`.

Artifact lifecycle (3):
`icc_artifact_reclassify`, `icc_artifact_link_add`,
`icc_artifact_link_remove`, `icc_artifact_demote`.

### Convenience

| Name | Purpose |
|---|---|
| `icc_quick_capture` | M53.5: create an artifact with `code_path` + `session_id` populated (pairs with Slice 5's artifact-form fields). One call instead of `artifact_create` + `link_add`. |

## Architecture

```
cmd/mcp-icc/
├── main.go               — server bootstrap, env reads, gate eval
├── tools.go              — Tool definitions + registration
├── tools_reads.go        — schema + handler factories for reads
├── tools_writes.go       — schema + handler factories for writes
├── tools_quick_capture.go — icc_quick_capture
├── helpers.go            — jsonResult, withWriteGate, type aliases
└── tools_test.go         — contract tests (in-process httptest)
```

The shared HTTP wrapper lives at `internal/iccclient/`. Both
`mcp-icc-capture` and `mcp-icc` import it. The wrapper sends the
ICC backend's trusted-context handshake headers (`Content-Type`,
`X-Requested-With: integration-command-center`, `Origin`) on every
request. HMAC signing is a future hardening slice — intentionally
not implemented.

## Build + test

```bash
make mcp-icc                       # build the binary into bin/mcp-icc
go test ./cmd/mcp-icc/...          # unit + contract tests
go test ./internal/iccclient/...   # shared HTTP wrapper
```

## Known gaps (filed as follow-ups)

- HMAC signing (matches `handle_bridge_ingest`). Trusted-context
  headers suffice today; HMAC is hardening, not capability.
- `icc_vendor_*` write tools. Vendors are a lower-traffic entity
  family; deferred to a follow-up slice if a caller asks.
- `icc_workstream_update` / `icc_workstream_delete`. Same rationale.
- `icc_extraction_run` / `icc_extraction_run_batch`. Extractions are
  driven by the SPA today; MCP tooling here would just shadow it.
