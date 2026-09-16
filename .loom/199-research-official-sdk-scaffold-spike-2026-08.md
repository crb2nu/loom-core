# Official Go SDK at `mcpscaffold`: Phase 3 spike decision seed

- **Date:** 2026-08-29
- **Status:** ready to file as one implementation spike; research only
- **Predecessor:** `.loom/195-research-mcp-2026-07-28-gap-roadmap-2026-08-15.md`, Phase 3 item 13

## Decision question and scope

`pkg/mcpscaffold/scaffold.go` is the leaf-server choke point: it creates the MCP server, applies instructions, initialises logging and OpenTelemetry, and wraps every registered tool with tracing. The Phase 3 question is whether leaf servers should use the official Go SDK through an equivalent scaffold, while the loom proxy and hub keep their in-house transport plumbing.

This is deliberately a **one-server** experiment. It neither replaces the WebSocket hub fabric nor changes the existing `gitlab.flexinfer.ai/libs/mcp-go` dependency. Although the earlier prose calls the dependency `libs/mcp-go`, this checkout consumes it as a Go module (see `go.mod`); there is no authoritative top-level `libs/` source tree in this repository.

## Upstream pin and protocol/transport coverage

Pin the spike to **`github.com/modelcontextprotocol/go-sdk v1.7.0`**. The official release was published **2026-07-28 13:09:53 UTC** and describes itself as full support for MCP `2026-07-28` [release record](https://github.com/modelcontextprotocol/go-sdk/releases/tag/v1.7.0). The SDK compatibility table lists `2026-07-28`, `2025-11-25`, `2025-06-18`, `2025-03-26`, and `2024-11-05` for v1.7.0+ [SDK README](https://github.com/modelcontextprotocol/go-sdk#version-compatibility). The spike must use this exact version (not a later v1.7 pre-release or `main`) so that the decision is reproducible.

| Concern | v1.7.0 evidence and implication |
| --- | --- |
| `2026-07-28` lifecycle | The SDK implements `server/discover`, carries client identity/capabilities and version per request in `_meta`, and falls back to legacy `initialize` when discovery is unavailable [protocol lifecycle](https://github.com/modelcontextprotocol/go-sdk/blob/v1.7.0/docs/protocol.md#lifecycle). |
| Stateless Streamable HTTP | `StreamableHTTPHandler` supports it, but `StreamableHTTPOptions.Stateless=true` is required for `2026-07-28`; a stateful handler negotiates clients down to `2025-11-25` [v1.7.0 release](https://github.com/modelcontextprotocol/go-sdk/releases/tag/v1.7.0), [transport docs](https://github.com/modelcontextprotocol/go-sdk/blob/v1.7.0/docs/protocol.md#stateless-mode). Stateless mode cannot make server-to-client requests. |
| Stdio | Server-side `StdioTransport` uses process stdin/stdout; client-side `CommandTransport` starts and speaks to a subprocess [transport docs](https://github.com/modelcontextprotocol/go-sdk/blob/v1.7.0/docs/protocol.md#stdio-transport). This is the direct fit for `loom proxy`. |
| Streamable HTTP API | The SDK exposes `StreamableHTTPHandler`, `StreamableServerTransport`, and `StreamableClientTransport` [transport docs](https://github.com/modelcontextprotocol/go-sdk/blob/v1.7.0/docs/protocol.md#streamable-transport). It validates modern MCP HTTP headers for negotiated `2026-07-28` sessions [HTTP headers](https://github.com/modelcontextprotocol/go-sdk/blob/v1.7.0/docs/protocol.md#http-headers). |

## Scaffold API mapping

| Existing scaffold contract | Current in-house use | Official SDK equivalent / spike requirement |
| --- | --- | --- |
| `NewServer(name, version, ...)` | Creates `mcp.Server`, logger, tracer, and lifecycle cleanup. | `mcp.NewServer(&mcp.Implementation{Name: name, Version: version}, &mcp.ServerOptions{...})`; retain lifecycle and cleanup in a small official-SDK-only scaffold adapter. |
| `WithInstructions` | Calls `SetInstructions` before serving. | `mcp.ServerOptions.Instructions`; assert it appears in `server/discover` and legacy initialization as appropriate. |
| `AddTracedTool` | Wraps `mcp.Tool` and a map-based handler with `mcpotel.TracedToolHandler`. | Register with `mcp.AddTool` (typed `ToolHandlerFor` where practical, or low-level `Server.AddTool` for a wrapper); adapter must start an OTel span, preserve `agent_id`/`session_id`/`namespace` attributes, and record handler errors. The official SDK does not replace loom's `pkg/mcpotel` policy. |
| `mcp.InputSchema` | Hand-authored `type`, properties, and required fields. | Prefer typed input structs plus `jsonschema` tags, because `mcp.AddTool` derives and validates schema; compare emitted schema to today’s `tools/list`. Use the low-level `Tool` form only if exact schema parity requires it. |
| `mcp.JSONResult` / errors | Produces current text/TOON-oriented result shape and `mcp.ErrorResult`. | Typed `AddTool` produces `StructuredContent` and JSON text content; error behavior must be explicitly mapped and compared. This is a parity risk, not an assumed improvement. |

The official API provides useful schema validation and structured-output defaults, but it does not supply loom-specific OTel setup, result formatting policy, `mcperror` conventions, or bounded concurrency configuration. Those remain adapter responsibilities and are part of the maintenance comparison.

## Spike target: `mcp-time`

Choose **`cmd/mcp-time`**, rather than `mcp-youtube`. It is small, has no network dependency in its principal behavior, and is already scaffolded. Its five deterministic or easily controlled tool shapes make protocol diffs easier to attribute than a transcript provider’s external HTTP and caption fallbacks.

It exercises:

- `NewServer`, a non-empty `WithInstructions`, and shutdown cleanup;
- `AddTracedTool` for five tools, including success and tool-error paths;
- hand-authored `InputSchema` with optional and required string fields;
- `mcpotel` spans around each handler; and
- JSON object results, validation failures, and a deliberately long-running `wait` tool for cancellation/timeout observation.

It cannot establish parity for resources, prompts, sampling, roots, elicitation, tasks, MCP Apps `_meta`, OAuth, hub WebSocket transport, binary dependencies used by other servers, or complex array-heavy results. It also does not prove that the official SDK supports loom’s TOON policy. A pass therefore supports a leaf tools-only migration decision only; it is not evidence to migrate the hub or `mcp-loom-widget`.

## Evidence bar and decision rule

The implementation must produce an artifact (command transcripts and checked-in test fixtures) for every row below. “Compiles” is not a passing result.

| Evidence | Pass criterion | Fails / blocks adoption when |
| --- | --- | --- |
| Feature-parity table | Side-by-side `tools/list` definitions and success/error `tools/call` payloads for all five tools; instructions visible; OTel span name/attributes/error status equal to existing binary; documented intentional differences only. | A client-visible field, validation rule, error convention, instructions, or tracing attribute regresses without a bounded adapter fix. |
| Binary size | Reproducible stripped Linux-amd64 builds of existing and official binaries, with commands, Go version, and byte counts recorded. | Official binary grows by more than **15% or 1 MiB** (whichever is larger) without a concrete fleet-level benefit. |
| Handshake matrix | For each row below, record selected protocol version, `server/discover` or `initialize` path, `tools/list`, one `tools/call`, and failure logs. | Any required existing client path breaks, or `2026-07-28` cannot complete the documented stateless HTTP path. |
| Maintenance argument | Compare adapter LOC, tests, dependency/update cadence, upstream issue ownership, and in-house work avoided for Phase 0/1 gaps. State costs and benefits in both directions. | The official SDK merely moves protocol maintenance into a larger unowned adapter, or the in-house SDK has a funded, time-bounded conformance path that is cheaper. |

Required handshake matrix:

| Client/path | Transport | Required outcome |
| --- | --- | --- |
| `loom proxy` → official `mcp-time` | stdio | Existing legacy client behavior continues: startup, `tools/list`, and `get_current_time` round trip. Capture negotiated legacy version. |
| SDK test client → official `mcp-time` | stdio | `server/discover` selects `2026-07-28`, followed by `tools/list` and `wait`. |
| `mcpo` → official `mcp-time` endpoint | Streamable HTTP, stateless | `2026-07-28` modern headers and `_meta` accepted; discovery, list, and call succeed through the `mcpo` streamable-http route. |
| Legacy streamable client → official endpoint | Streamable HTTP, stateful or downgrade path | Negotiates `2025-11-25` (or highest mutual legacy version) and completes list/call; this establishes compatibility boundary. |

## Filable next slice

**Title:** `spike(mcp): port mcp-time through an official-Go-SDK scaffold adapter`

**Scope:** add the pinned official SDK dependency; add an isolated adapter (do not change `pkg/mcpscaffold` or other servers); port only `cmd/mcp-time`; add golden/protocol tests and a reproducible size/handshake evidence script or document. Keep the current in-house `mcp-time` implementation buildable until the decision is recorded. No hub, WebSocket, registry, or fleet-wide migration.

**Acceptance tests:** `GOWORK=off go test ./cmd/mcp-time/... ./pkg/...` (with the adapter package named in the final command), `GOWORK=off go build ./cmd/mcp-time`, the four-row handshake matrix, OTel span assertion, and binary-size comparison. Add a changelog fragment because that implementation slice changes Go code.

**Decision after the slice:** adopt at the scaffold boundary only if every required parity and handshake row passes, the size threshold holds, and the maintenance argument identifies a net reduction in protocol-conformance work. Otherwise retain the in-house SDK and re-evaluate when the official SDK adds a needed loom-specific integration point or when the in-house SDK has not closed the Phase 0/1 `2026-07-28` conformance items within one quarter.
