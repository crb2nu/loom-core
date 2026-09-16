# MRTR elicitation for loom: Phase 2 seed

- **Date:** 2026-08-29
- **Status:** research / backlog seed; no implementation in this change
- **Roadmap parent:** [`.loom/195-research-mcp-2026-07-28-gap-roadmap-2026-08-15.md` §Phase 2, item 10](195-research-mcp-2026-07-28-gap-roadmap-2026-08-15.md#phase-2--value-adoption-next-quarter)
- **Upstream basis:** [MCP 2026-07-28 tools: Input Required Tool Results](https://modelcontextprotocol.io/specification/2026-07-28/server/tools#input-required-tool-results), [schema reference](https://modelcontextprotocol.io/specification/2026-07-28/schema), and [elicitation](https://modelcontextprotocol.io/specification/2026-07-28/client/elicitation).

## Decision and boundary

Use multi-round-trip requests (MRTR) for the small set of tool calls that need
additional human input after invocation.  A server returns a normal `tools/call`
result with `resultType: "input_required"`; the client obtains input and issues a
new `tools/call`, including `inputResponses` and the opaque `requestState` when
provided.  The retry has a **different JSON-RPC id**.  This is not the old
server-initiated `elicitation/create` request pattern: the elicitation-shaped
object is data inside a tool result, so it needs no server-to-client request
channel.  The upstream tools specification gives both the response and retry
shape and says that `resultType` is `"complete"` for ordinary results.

That distinction is why this is viable despite the hub constraint recorded in
the roadmap: `internal/daemon/hub_transport.go:158-186` only forwards the two
broadcast-safe list-change notifications and drops all other notifications.
MRTR must never be implemented by opening a held server-to-client RPC through
that path.

This seed deliberately does not turn ordinary invalid arguments into prompts.
Missing immutable identifiers and malformed values should remain actionable
`isError`/invalid-input results.  Use MRTR only where the missing value is
human choice, confirmation, or credentials/consent that the client can present
safely.

## 1. Candidates: today they fail on absent arguments

The fleet has a common validation idiom rather than a single interactive
mechanism: handlers call `validate.NewArgs(...).Required(...)` or return
`mcperror.RequiredParam(...)`.  `pkg/mcperror/error.go:168-174` renders the
current outcome as `missing required parameter: <name>`;
`pkg/mcpscaffold/scaffold.go:35-76` installs tracing and server construction,
but no elicitation wrapper.  The inventory below is representative and
high-value, not a claim that every required field should be elicited.

| Server / representative tool area | Current missing-input evidence | MRTR fit |
| --- | --- | --- |
| `mcp-devbox` — command execution and file writes | `cmd/mcp-devbox/handlers.go:22-23` requires `project` and `command`; write paths later require `path` and `content` | **Pilot candidate:** ask for a project only when the caller has several permitted projects; never prompt for an arbitrary shell command or file content. |
| `mcp-mills` — destructive workflow/approval actions | The shared `mcperror.RequiredParam` path is used by tool handlers, and the roadmap explicitly identifies destructive Mills actions as an MRTR case (`.loom/195…:142-145`) | **Strong candidate:** return a confirmation/choice request before an authorized destructive action, preserving the proposed action in opaque state. |
| `mcp-gitlab` / `mcp-github` — project/repository-scoped mutations | Required-field validation is pervasive in the `cmd/mcp-*` fleet; `cmd/mcp-argocd/main.go:639` requires `repo`, while adjacent Git hosting tools follow the same `validate.Required` convention | **Conditional:** offer a selector when a safe, already-authorized repository choice is ambiguous; do not elicit a missing target that expands access. |
| `mcp-k8s`, `mcp-alertmanager`, and `mcp-argocd` — operational changes | `cmd/mcp-alertmanager/main.go:299,316` requires `comment`/`matchers`; `cmd/mcp-argocd/main.go:388-675` repeatedly requires `name`, `repo`, and `server` | **Confirmation only:** a user-facing approval or bounded environment choice can fit. Names, selectors, and matchers remain validation failures when no safe candidate set exists. |
| `mcp-browserkit`, `mcp-filesystem`, and `mcp-aws` — side-effecting I/O | `cmd/mcp-browserkit/main.go:172` requires `url`; `cmd/mcp-filesystem/main.go:118` requires `path`; `cmd/mcp-aws/main.go:372-373` requires `bucket` and `key` | **Usually no:** preserve fail-on-missing behavior; only a previously enumerated, authorized choice can be elicited. Never use MRTR to collect secrets or unrestricted paths/URLs. |

Before filing a server slice, its owner must write down: (a) the exact state at
which execution stops, (b) the finite, authorization-filtered choices or form
schema, (c) which response actions are accepted, and (d) expiry, replay, and
audit behavior.  A validation error remains the default when those conditions
are not true.

## 2. Required `libs/mcp-go` surface

`libs/mcp-go` is a sibling module (as recorded by `.loom/195…:40-47`), not a
directory in this worktree.  The following is an API contract for that module,
not a proposed loom-core path.

1. Model all result-bearing protocol responses with an explicit `resultType`.
   `CallToolResult` must encode/decode `complete` and `input_required` without
   losing unknown compatible fields.  Existing ordinary tool helpers should
   emit `complete` by default once the protocol-version work permits it.
2. Add an input-required result representation with `InputRequests` and opaque
   `RequestState`.  An input request is keyed by a stable request key and holds
   the requested client interaction (`method: "elicitation/create"` plus its
   form/url params); do not make it a server RPC type.
3. Extend call parameters with `InputResponses` and `RequestState`.  They must
   round-trip as raw/typed JSON values, retain response `action` values (such as
   accept/decline/cancel), and be available to the tool handler or an MRTR
   continuation wrapper.
4. Provide client retry semantics, not hidden automatic retries: expose the
   input-required result to the client; let the client collect input; submit a
   new `tools/call` with a new JSON-RPC id, same tool and arguments,
   `inputResponses`, and returned `requestState`.  Bound the interaction count
   and total deadline, reject state that is expired or belongs to another
   principal, and terminate cleanly on decline/cancel.
5. Preserve ordinary error semantics.  `input_required` is neither a JSON-RPC
   error nor `CallToolResult.IsError`; middleware and error adapters must not
   convert it into an invalid-parameter failure.

The upstream schema remains the authority for exact field optionality and
action shapes.  The SDK slice should pin a specification fixture for the
published response/retry examples instead of copying a locally invented schema.

## 3. Proxy and hub trace: pass-through contract and current swallow points

The intended flow is:

```text
vendor client
  -> stdio `loom proxy` tools/call
  -> daemon `loom/call`
  -> local server or hub transport
  -> CallToolResult(resultType=input_required)
  -> same correlated response back to the client
  -> new tools/call with inputResponses + requestState
```

MRTR responses are correlated responses, not notifications.  `pkg/transport/muxstdio/transport.go:234-290` routes messages with an id to a per-id waiter;
only id-less messages use the bounded notification channel.  Hub envelopes also
keep their payload as `json.RawMessage` (`internal/hubproto/envelope.go:29-45`),
so the envelope itself has no result schema that strips MRTR fields.  The hub
notification allowlist cited above therefore does not apply to a result.

The existing code is **not yet an end-to-end pass-through**.  These are the
verified changes/tests required before declaring it one:

| Location | Finding / required hardening |
| --- | --- |
| `cmd/loom/proxy_handlers.go:128-140` | The proxy reconstructs downstream parameters from only `name` and `arguments`; it currently drops retry `inputResponses` and `requestState`. Preserve those fields verbatim (or use a versioned typed request) before forwarding. |
| `internal/daemon/callpipeline_stages.go:399-429` | The daemon preserves `callParams.Params` raw when supplied, including on hub egress; its fallback builder creates only name/arguments. Ensure the proxy supplies full params and add a retry fixture across local and hub routes. |
| `cmd/loom/proxy_handlers.go:177-192` | The oversize guard unmarshals every result into the current `mcp.CallToolResult` and re-marshals it when it changes. Until the SDK knows MRTR fields, that is a field-loss risk. Do not truncate or append trailers to `input_required`; otherwise preserve its raw result exactly. |
| `cmd/loom/proxy_mrtrailer.go:246-338` | The MR-status trailer mutates a typed tool result after truncation. It must bypass non-`complete` results so an interaction payload cannot be changed. |
| `pkg/toolexec/client.go:173-225` | This convenience parser turns a `CallToolResult` into a map/text and treats `IsError` specially. It is unsuitable for an MRTR client path; either surface the envelope or explicitly return an input-required signal. |
| `pkg/mcpotel/middleware.go:18-50`, `pkg/mcperror/error.go:168-174` | Neither re-marshals results today; tracing only observes `IsError`, and `RequiredParam` only builds an error. Keep this distinction in tests so input-required is not marked as a tool failure. |
| `internal/daemon/hub_transport.go:158-186` | Notification filtering is intentionally unchanged and irrelevant to correlated responses. A regression test must prove an MRTR response does not enter this notification path. |

`pkg/telemetry/redact/` contains telemetry policies, not a transport response
marshaller in the reviewed path; it is a privacy review point for request-state
logging, not a confirmed wire swallow point.  Likewise `cmd/mcp-hub-wrapper`
bridges `mcp.Message` values; add a raw-result round-trip test rather than
assuming its reconnect/replay logic is safe for a new result variant.

## 4. One-tool kill-test and compatibility fallback

Build a purpose-made stdio fixture server (the process-hosting pattern in
`cmd/custom-server/main.go:44-95` is relevant) with exactly one tool,
`mrtr_echo`.  Its first call returns a stable `input_required` form request for
`message` plus signed/expiring `requestState`; its retry accepts that response
and returns `complete` with the selected message.  It records attempt count,
JSON-RPC ids, and received retry fields to a test-only log.

Run it through `loom proxy` over stdio separately with the newest available
Claude Code and Codex builds.  The harness should follow the evidence-oriented
shape of `cmd/mills-workflow-killtest/main.go:1-70`: bounded deadlines, machine-
readable evidence, explicit PASS/FAIL predicates, and a non-zero exit for a
failed predicate.  This is a new focused harness, not reuse of the Mills
cluster crash scenario.

Pass criteria:

1. Both clients discover `mrtr_echo` through the proxy and make the first call.
2. The client renders/collects the requested input and sends exactly one retry
   with a different JSON-RPC id, original arguments, `inputResponses`, and the
   exact `requestState`.
3. The fixture returns `resultType: "complete"`; the client displays the final
   result.  Proxy/hub logs show no notification drop and no extra side effect.
4. Negative cases prove decline/cancel, expired state, altered state, and a
   retry-budget breach perform no side effect and terminate deterministically.

Vendor implementation is an external dependency.  Until a particular client
has demonstrated the loop, capability-detect per connection/client version and
gate MRTR per tool.  Unsupported or unknown clients retain today’s
fail-on-missing-parameters behavior; they must not receive a result they render
as success or silently discard.  This must be per connection, rather than a
global switch, because loom has version-skewed local clients across the fleet
(`internal/fleetgate/`, `cmd/fleet-reliability-gate/`, and
`internal/hud/fleetview/`).  Record the capability decision and fallback reason
in audit/telemetry without logging form answers or opaque request state.

## 5. Backlog-ready implementation slices

1. **`mcpgo-mrtr-result-surface`** — `files=libs/mcp-go/{types.go,server.go,client*.go,*_test.go}`; add result variants, call retry fields, raw-compatible encoding, and spec fixtures. `tests=go test ./...`.
2. **`loom-mrtr-proxy-daemon-pass-through`** — `files=cmd/loom/proxy_handlers.go,cmd/loom/proxy_truncate.go,cmd/loom/proxy_mrtrailer.go,internal/daemon/callpipeline_stages.go,internal/daemon/*mrtr*_test.go`; preserve retry parameters and raw input-required results through local/hub routes. `tests=go test ./cmd/loom/... ./internal/daemon/...`.
3. **`loom-mrtr-scaffold-continuation`** — `files=pkg/mcpscaffold/,pkg/mcpotel/,pkg/mcperror/,pkg/validate/`; provide an opt-in continuation helper, state/signature/expiry rules, and metrics semantics while retaining validation errors by default. `tests=go test ./pkg/mcpscaffold/... ./pkg/mcpotel/... ./pkg/mcperror/... ./pkg/validate/...`.
4. **`mcp-mills-mrtr-confirmation-pilot`** — `files=cmd/mcp-mills/,pkg/mcpscaffold/`; protect one explicitly destructive, already-authorized action with a bounded confirmation request and an audit-safe continuation. `tests=go test ./cmd/mcp-mills/...; targeted integration test`.
5. **`loom-mrtr-vendor-stdio-killtest`** — `files=cmd/mcp-mrtr-killtest/ (new),cmd/custom-server/ (fixture if needed),docs/runbooks/`; automate the Claude Code/Codex proxy evidence described above, including unsupported-client evidence. `tests=go test ./cmd/mcp-mrtr-killtest/...; manual newest-Claude-Code-and-Codex stdio runs`.
6. **`loom-mrtr-hub-observability-hardening`** — `files=internal/hubproto/,internal/daemon/hub_transport.go,cmd/mcp-hub-wrapper/,pkg/telemetry/redact/`; pin raw-result round trips, assert notification filtering cannot affect results, and redact state/input values. `tests=go test ./internal/hubproto/... ./internal/daemon/... ./cmd/mcp-hub-wrapper/... ./pkg/telemetry/redact/...`.

The SDK slice precedes every loom-core implementation slice.  The pilot and
vendor kill-test must remain gated until the pass-through tests prove that both
MRTR request legs and the non-complete result survive the proxy and hub.
