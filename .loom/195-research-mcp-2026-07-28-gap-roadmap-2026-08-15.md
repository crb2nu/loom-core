# MCP 2026-07-28: loom-core gap analysis & modernization roadmap

- **Date**: 2026-08-15
- **Status**: research complete; Phase 0 slices filed to Mills backlog
- **Sources**: [2026-07-28 announcement](https://blog.modelcontextprotocol.io/posts/2026-07-28) · [changelog](https://modelcontextprotocol.io/specification/2026-07-28/changelog) · [extensions overview](https://modelcontextprotocol.io/extensions/overview) · [registry charter](https://modelcontextprotocol.io/community/working-groups/registry) · [Go SDK releases](https://github.com/modelcontextprotocol/go-sdk/releases) · in-repo inventory (file:line cites below), corroborating `.loom/182-research-loom-core-fleet-architecture-2026-07-10.md:271`

## 1. What changed upstream

MCP `2026-07-28` (released July 28, 2026) is the largest and first deliberately
breaking revision. Loom-core last tracked `2025-06-18`; the `2025-11-25`
revision was never adopted, so we are two revisions behind a breaking release.

| Change | SEP | Consequence |
|---|---|---|
| Stateless core: `initialize`/`initialized` and `Mcp-Session-Id` removed; per-request `_meta` carries protocolVersion/clientCapabilities/clientInfo | 2575, 2567 | Any request → any instance; new clients eventually stop handshaking |
| `server/discover` RPC (servers MUST implement) | 2575 | Replaces up-front negotiation; also the STDIO back-compat probe |
| MRTR: server-initiated requests (elicitation/sampling/roots) replaced by `resultType:"input_required"` + retry with `inputResponses`; ALL results carry required `resultType` | 2322 | No held-open streams for interactivity |
| `subscriptions/listen` replaces HTTP GET stream + `resources/subscribe`; `ping`, `logging/setLevel`, SSE resumability (`Last-Event-ID`) removed | 2575 | Notification model consolidated |
| Required HTTP headers `Mcp-Method`/`Mcp-Name`; `x-mcp-header` param promotion | 2243 | Gateways route/meter without body parsing |
| `CacheableResult` (`ttlMs`, `cacheScope`) on all list reads + deterministic tool ordering | 2549 | Client-side catalog caching; stable LLM prompt caches |
| Tasks → official extension `io.modelcontextprotocol/tasks`: poll `tasks/get`, `tasks/update`, `requestState` for stateless resumption | 2663 | Standard long-running-work contract |
| Extensions framework formal (`extensions` in capabilities); MCP Apps + Enterprise-Managed Auth are official extensions | 2133 | `ui://` widget pattern is now a standard |
| Auth hardening: RFC 9207 `iss` validation, credentials keyed by issuer, `application_type` in DCR; **DCR deprecated → Client ID Metadata Documents (CIMD)** | 2468, 2352, 837 | Our daemon OAuth needs a CIMD path within the window |
| **Deprecated (12-month clock): Roots, Sampling, Logging, DCR, HTTP+SSE transport** | 2577, 2596 | See §3 — mostly validates our posture |
| Error codes: resource-not-found `-32002`→`-32602`; `-32020..-32099` reserved for spec | — | Audited: in-house SDK uses only standard codes (`mcp-go/types.go:54-58`) — no exposure |
| OTel trace context conventions in `_meta` (`traceparent` etc.) | 414 | Free interop win for our OTel pipeline |

Tier 1 SDKs (TS/Python/**Go**/C#) ship `2026-07-28` today. The official Go SDK
serves the new protocol only with `Stateless=true` and **negotiates older
clients down to `2025-11-25`**; v2 clients fall back to the legacy handshake
against down-level servers. The official MCP Registry remains preview-only and
excludes private servers → our `registry.yaml` remains correct.

## 2. Where loom-core stands (inventory, 2026-08-15)

Backbone is the **in-house SDK** `gitlab.flexinfer.ai/libs/mcp-go`
(go.mod:45, ~8.5k lines) + `fi-mcp-kit`, NOT mark3labs or the official SDK.
`pkg/mcpscaffold/scaffold.go:41` is the single choke point ~30 `cmd/mcp-*`
servers build on — any SDK-level fix lands fleet-wide through it.

**Version split-brain**: proxy and daemon negotiate `2024-11-05|2025-06-18`
(`cmd/loom/proxy_handlers.go:31-51`, `internal/daemon/daemon_dispatch.go:144-158`
— duplicated logic that defaults *forward* on unknown versions), but every leaf
server answers `initialize` with hardcoded `2024-11-05` (`mcp-go/server.go:382`)
and the WS client transport handshakes `2024-11-05` (`mcp-go/websocket.go:290`).
`SupportedProtocolVersions` (`mcp-go/types.go:33`) exists and is **never read**.
No `MCP-Protocol-Version` HTTP header anywhere (a 2025-06-18 requirement).

**Transports**: stdio (both framings), Streamable HTTP w/ sessions (no
resumability — now conformant by removal!), legacy SSE (unused/deprecated
upstream), WebSocket (non-spec, the entire hub fabric: `custom-server`
sidecars at `ws://…/ws`, `loom proxy --ws-backend`). WS server is
unauthenticated in-cluster (`cmd/custom-server/main.go:27` CheckOrigin=true);
network policy + CF Access edge only.

**Feature surface**: tools-only, everywhere. Zero resources/prompts/sampling/
elicitation/roots/completion/logging/tasks/cancellation call sites. Progress
is plumbed but unreachable (`mcp-go/server.go:466-468`) and the hub relay
deliberately drops non-listChanged notifications
(`internal/daemon/hub_transport.go:158-186`) — server→client requests cannot
transit the hub at all.

**Tool results**: no `outputSchema`, no `structuredContent`, no annotations,
no `resource_link` (`mcp-go/types.go:127-131,152-162`). Everything ships as
TOON text via `JSONResult` (default: `mcp-go/output_format.go:31-33`). TOON's
tabular form **drops array-valued columns** — the documented root cause of the
plan-slice `files` loss, zeroed devbox verdicts, and a blanked cluster HUD
(`pkg/mills/clients/plan_reader.go:148,179`, `CHANGELOG.md:319,326,501`).

**Auth**: real OAuth 2.1 + PKCE(S256) + DCR on daemon HTTP only
(`internal/daemon/oauth.go`) — close to 2025-06-18 spec; DCR is now the
deprecated registration path.

**Prior art in-repo**: `cmd/mcp-loom-widget` hand-implements the MCP Apps
pattern (`ui://widget/loom-fleet.html`, `_meta.ui.resourceUri`) because the
SDK "does not yet expose the `_meta` fields that MCP Apps needs"
(`cmd/mcp-loom-widget/main.go:13`).

## 3. What the new spec validates about loom's design

The deprecation list lands almost entirely on features loom never adopted:
sampling, roots, MCP logging, SSE resumability, resources/subscribe — all
skipped here, all now deprecated/removed upstream. Tools-only was the right
bet. Likewise: explicit state handles as tool arguments (now the spec's
recommended pattern), aggressive list caching/truncation (now `CacheableResult`),
and out-of-band safety metadata (annotations remain hints; our registry
`always_allow` + RBAC is the enforcing layer). The stateless core is
*architecturally aligned* with the fleet: per-request routing over dumb LBs is
exactly the shape our k8s hub wants — we just speak WS instead of the now
LB-friendly stateless Streamable HTTP.

## 4. Gap matrix → roadmap

Ordering principle: conformance floor first (cheap, unblocks clients), then
adoptions that fix live bug classes, then strategic transport work.

### Phase 0 — hygiene + conformance floor (days each)
0. **Enable `libs/mcp-go` as a Mills-workable project** — the SDK changes
   below land in the `libs/mcp-go` repo, which is NOT in `SPAWN_PROJECTS`
   (`k8s/base/servers/mobile-hud/deployment.yaml`, env) nor bootstrapped —
   a `TargetProject: libs/mcp-go` item today wedges at the implement spawn.
   Add the project to `SPAWN_PROJECTS`, verify git-clone spawn access via the
   group token (cross-repo keystone pattern, proven for gitops), and smoke it
   with a trivial item. *Filed:* `bl-mills-enable-libs-mcp-go-20260815`.
1. **Leaf version negotiation** — consult `SupportedProtocolVersions` in
   `handleInitialize` (`mcp-go/server.go:346-390`), echo the client's version
   when supported, reply with our highest otherwise (never default-forward);
   dedupe the proxy/daemon copies onto the SDK helper; emit/read
   `MCP-Protocol-Version` header on Streamable HTTP. *Staged behind item 0*
   (target `libs/mcp-go`); spec text lives in this doc — file as
   `bl-mcpgo-leaf-version-negotiation` once enablement lands.
2. **`structuredContent` + `outputSchema`** — add to `Tool`/`CallToolResult`;
   `JSONResult` dual-emits structured JSON + TOON text fallback; hub/bridge/
   mills consumers prefer `structuredContent` when present. Retires the TOON
   tabular data-loss class at the contract level. *Staged behind item 0*:
   SDK half targets `libs/mcp-go`; consumer half is a follow-up loom-core
   item depending on the SDK version bump.
3. Docs drift: `docs/STREAMABLE_HTTP.md:183` cites 2025-03-26; SDK README
   omits its own HTTP transports. Fold into item 1's changelog.

### Phase 1 — 2026-07-28 readiness (this quarter)
4. **`server/discover` + handshake-less acceptance**: accept per-request
   `_meta` protocolVersion/capabilities, treat missing-initialize as valid,
   implement `server/discover` (also the spec's STDIO back-compat probe);
   stamp `resultType:"complete"` on results (harmless to old clients, required
   by new ones).
5. **Deterministic tool ordering + `ttlMs`/`cacheScope`** on list responses —
   cheap, improves vendor-client prompt caching immediately.
6. **CIMD alongside DCR** in daemon OAuth; client-side RFC 9207 `iss`
   validation; keep DCR for the 12-month window.
7. **Kill legacy SSE surfaces** (`mcp-go/sse.go` unused; `custom-server` /sse)
   — deprecated upstream, dead weight here.

### Phase 2 — value adoption (next quarter)
8. **Tasks extension** over our async tool pairs (`devbox_exec_async/_poll`,
   `codebase_index_*`): map to `io.modelcontextprotocol/tasks` (`tasks/get`,
   `tasks/update`, `requestState`) so vendor clients get standard long-running
   semantics; keep the bespoke pairs as aliases.
9. **MCP Apps conformance for loom-widget**: add `_meta` plumbing to mcp-go
   (the documented blocker), move loom-widget onto the SDK, declare
   `io.modelcontextprotocol/ui` via the extensions capability.
10. **MRTR elicitation** where tools genuinely need mid-call input (spawn
    approvals, destructive mills actions) — newly *possible* for us because
    MRTR works over stateless request/response, which our hub relay can carry
    (it's just a result + retry; no server→client channel needed — this
    sidesteps the `hub_transport.go:158-186` structural gap entirely).
11. **Cancellation**: honor `notifications/cancelled` per-request in the SDK
    dispatch; wire to the existing context-cancellation pipeline.

### Phase 3 — strategic (2–3 quarters out; decide, don't drift)
12. **Stateless Streamable HTTP as the hub fabric**: the 2026-07-28 transport
    was built for exactly our fleet shape — replace per-connection WS sidecar
    children with stateless HTTP handlers behind the k8s Service LB;
    `Mcp-Method`/`Mcp-Name` headers give the gateway routing/metering without
    body parsing. WS stays for the interactive daemon paths that want push.
13. **SDK strategy decision**: chase the spec in-house vs adopt the official
    Tier 1 Go SDK at the `mcpscaffold` boundary (leaf servers only; hub
    plumbing stays in-house). Run a one-server spike (port `mcp-tavily`) and
    decide on evidence. The official SDK's downgrade-negotiation to
    `2025-11-25` would buy leaf conformance for free — at the cost of the
    TOON/output-format hooks, which would need a wrapper layer.

## 5. Riskiest assumption + kill-test

**Assumption**: vendor CLIs (Claude Code, Codex, Gemini) keep legacy-handshake
fallback long enough (≥2 quarters) that our stdio `loom proxy` surface keeps
working while we execute Phases 0–1. The C# and Go v2 SDKs both document
automatic fallback, but the vendors' own client builds are the actual risk.

**Kill-test** (cheap, run monthly): against the newest Claude Code + Codex
releases, run `loom proxy --tool-profile core-developer` over stdio and verify
`tools/list` + one `tools/call` round-trip. If either vendor drops fallback,
Phase 1 item 4 becomes a P1 and jumps the queue. First run: pass (2026-08-15,
current session is itself the harness — this conversation runs through the
proxy).

## 6. Explicit non-goals
- Official MCP Registry publication (preview-only; no private servers).
- Adopting Roots/Sampling/MCP-Logging (deprecated at birth for us).
- Rewriting the WS hub before Phase 3's evidence-based decision.
