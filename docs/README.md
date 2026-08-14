# Loom Core Documentation Hub

This is the main docs index for users, contributors, and operators.

## Current Status First

If you need to know what is shipped vs still in progress:

- [Implementation status](IMPLEMENTATION_STATUS.md)
- [Roadmap](../ROADMAP.md)

## Quick Paths By Audience

- New user/operator setup: [User guide](USER_GUIDE.md)
- Contributor/developer workflows: [Developer guide](DEVELOPER_GUIDE.md)
- Runtime/system model: [Architecture](ARCHITECTURE.md)

## Task-Based Navigation

- Install, sync configs, run daemon/HUD: [User guide](USER_GUIDE.md)
- Build/reload loop for local development: [Development build lifecycle](DEV_BUILD_LIFECYCLE.md)
- Add or update MCP servers safely: [Developer guide](DEVELOPER_GUIDE.md)
- Manage repository branding through MCP: `mcp-brand-kit` provides repository listing, inspection, lint, preview, render, and fix tools.
- Understand compatibility commitments: [API stability](API_STABILITY.md)
- Follow MCP error-handling standards: [Error handling](ERROR_HANDLING.md)
- Configure enterprise controls: [Enterprise security](ENTERPRISE_SECURITY.md)
- Configure remote transport: [Streamable HTTP](STREAMABLE_HTTP.md)
- Mobile companion API contract (draft): [Mobile companion API](MOBILE_COMPANION_API.md)
- Mobile companion security model (draft): [Mobile companion security](MOBILE_COMPANION_SECURITY.md)
- Mobile companion iPhone test runbook: [iPhone testing](MOBILE_COMPANION_IPHONE_TESTING.md)
- Mobile companion signing and CI secrets setup: [Mobile companion signing setup](MOBILE_COMPANION_SIGNING_SETUP.md)
- Follow docs ownership and update cadence: [Documentation maintenance](DOCS_MAINTENANCE.md)
- Publish docs to flexinfer.ai: [Flexinfer site integration](FLEXINFER_SITE_INTEGRATION.md)

## Planning and Technical Direction

- Active roadmap and priorities: [Roadmap](../ROADMAP.md)
- Planning index and historical notes: [Planning index](planning/README.md)

## Documentation Checks

Run the standalone documentation lint before opening a change that touches
documentation or contributor-facing scripts:

```bash
scripts/docs-lint.sh
```

The lint runs the same changed-files documentation policy used in CI and checks
that every status shown in the planning index matches that plan's
`> **Status:** ...` annotation. Keep planning documents as the source of truth:
when a plan status changes, update its index row in the same change. The output
identifies the index file and line, plus the mismatched status, so stale entries
can be corrected directly.

## Diagrams

- Diagram sources and regeneration notes: [Diagrams](diagrams/README.md)

## Suggested Reading Order

1. [Project README](../README.md)
2. [Implementation status](IMPLEMENTATION_STATUS.md)
3. [User guide](USER_GUIDE.md)
4. [Architecture](ARCHITECTURE.md)
5. [Developer guide](DEVELOPER_GUIDE.md)
