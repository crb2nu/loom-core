# Kill-test — ChatGPT Apps SDK widget rendering over remote HTTPS MCP

**Date**: 2026-05-17
**Slice**: 0 (gates Skybridge HTTPS deploy work — slices A/B/C)
**Status**: prepared, pending execution

## Load-bearing assumption

ChatGPT (Plus / Pro / Team / Enterprise / Edu with Developer Mode enabled)
renders an MCP Apps widget when:
- The MCP server is reached over a public HTTPS Streamable-HTTP endpoint
- The server is registered as a custom Connector / App in ChatGPT settings
- The tool response includes the existing `_meta.ui.resourceUri = ui://widget/loom-fleet.html`
  shape `mcp-loom-widget` already emits

## Failure mode if wrong

We'd build a production HTTP transport in `mcp-loom-widget` + k8s Deployment/Ingress +
cert-manager + Cloudflare Access app for `loom-widget.flexinfer.ai`, then discover
ChatGPT doesn't actually render our widget shape for non-obvious reasons (auth
header passthrough, iframe sandbox, MIME-type quirks, Plus-tier limits, etc.).
Net loss ≈ slices A/B/C effort if the path is dead end. Same shape as the
Claude Code MCP Apps mistake from MR #436.

## Positive evidence already gathered (Slice 0 doc review)

- OpenAI Apps SDK officially supports remote MCP servers over Streamable HTTP
  (https://developers.openai.com/apps-sdk/build/mcp-server)
- Workflow: ChatGPT → Settings → Connectors → Create → paste `<https>/mcp`
  (https://developers.openai.com/apps-sdk/deploy/connect-chatgpt)
- Cloudflare Tunnel is the officially recommended HTTPS exposure for testing
  (https://developers.openai.com/apps-sdk/quickstart)
- Widget renders in ChatGPT iframe via MCP Apps UI bridge (same wire format we emit)

## Disconfirming evidence — known gating constraints

- Free ChatGPT cannot register custom connectors
  (https://help.openai.com/en/articles/12584461-developer-mode-apps-and-full-mcp-connectors-in-chatgpt-beta)
- Plus / Pro individuals get **read-only** custom connectors. Our widget is
  read-only → fine.
- Workspace admins gate Developer Mode for Business / Enterprise / Edu
- Custom connectors are per-account scope (no shared-conversation tool reuse)
- No documented "widget fails to render over remote MCP" issues found
  (disconfirming search came up empty — positive sign)

## End-to-end procedure (15 min)

### Pre-flight checks (you confirm)

1. ChatGPT plan tier: Plus / Pro / Team / Enterprise / Edu (NOT Free)
2. Developer Mode enabled in ChatGPT Settings → Apps & Connectors
   (Enterprise/Edu users: admin needs to enable it on the workspace first)
3. `~/go/bin/mcp-loom-widget` exists and runs (already verified — built 2026-05-17)
4. HUD secrets in shell:
   - `LOOM_HUD_TOKEN`
   - `LOOM_HUD_CF_ACCESS_CLIENT_ID`
   - `LOOM_HUD_CF_ACCESS_CLIENT_SECRET`

### Step 1 — Expose the stdio binary over HTTPS (~3 min)

Open three terminals.

**Terminal A — start the stdio↔Streamable-HTTP bridge:**

```sh
uvx --from mcp-proxy mcp-proxy \
  --pass-environment \
  --port 8765 \
  --host 127.0.0.1 \
  --stateless \
  --allow-origin '*' \
  -e LOOM_HUD_URL "https://hud.flexinfer.ai" \
  -- ~/go/bin/mcp-loom-widget
```

Expected log line: `INFO ... Uvicorn running on http://127.0.0.1:8765`.
Endpoints exposed: `/sse` (SSE) and `/mcp` (Streamable HTTP).

**Terminal B — open a public HTTPS tunnel:**

```sh
cloudflared tunnel --url http://127.0.0.1:8765
```

Wait for: `Your quick Tunnel has been created! Visit it at: https://<RANDOM>.trycloudflare.com`.
Copy that hostname — call it `$TUNNEL`.

**Terminal C — sanity-check the bridge from outside:**

```sh
curl -s "https://<RANDOM>.trycloudflare.com/mcp" \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{}}}' \
  | head -30
```

Expected: a JSON-RPC response body (or an event-stream `data: {...}` line)
containing `"serverInfo":{"name":"mcp-loom-widget", ...}`. If you get this,
the proxy + tunnel chain works.

### Step 2 — Register in ChatGPT (~5 min)

1. Open ChatGPT in web (chat.openai.com or chatgpt.com)
2. Settings → **Connectors** (or **Apps**, post-Dec-17 2025 rename)
3. Click **Create** / **Add custom connector**
4. Name: `loom-widget-test`
5. MCP server URL: `https://<RANDOM>.trycloudflare.com/mcp`
6. Authentication: **None** (the binary uses env-var bearer tokens internally for HUD; no ChatGPT-side auth in this test)
7. Click **Connect** / **Save**
8. If it says "discovered tools: loom_fleet_show, loom_fleet_get_dashboard, …" → **wire format check PASS**

### Step 3 — Invoke the tool (~3 min)

1. Start a new ChatGPT conversation
2. Enable the `loom-widget-test` connector in the message composer (toggle / app picker)
3. Send: `Show me the loom fleet using loom-widget-test`
4. **Observe**:
   - **PASS**: An iframe widget renders inline, showing the fleet dashboard
     (agent rows, sessions, server count, etc.) — same data the markdown
     fallback returns
   - **PARTIAL**: Tool gets invoked, markdown text appears, no widget
     (= same failure mode as Claude Code — widget not rendered)
   - **FAIL**: Tool can't be invoked, error message, or ChatGPT hallucinates

### Step 4 — Capture evidence

- Screenshot the rendered widget (or the failure mode)
- Save a copy to: `.loom/kill-test-evidence-2026-05-17/`
- Update this doc's **Status** line at top with `PASSED YYYY-MM-DD` or
  `FAILED YYYY-MM-DD (reason)`

### Step 5 — Tear down

- Terminal A: Ctrl+C the mcp-proxy
- Terminal B: Ctrl+C cloudflared (the trycloudflare URL is ephemeral; it disappears)
- ChatGPT: optionally remove the test connector

## Decision tree post-test

- **PASS** → spawn slices A/B/C in parallel
  - A: production HTTP transport in `mcp-loom-widget` (Go)
  - B: k8s Deployment + Service + Ingress + cert-manager for `loom-widget.flexinfer.ai`
  - C: registry entry + Codex/Claude/Kilo config regen + ChatGPT setup README
- **PARTIAL** (tool invoked, no widget) → STOP. Same failure mode as Claude
  Code. Investigate ChatGPT's iframe sandbox / MIME requirements before
  committing infra.
- **FAIL** (connector setup fails) → likely plan/tier or Developer-Mode gating.
  Capture exact error, revise host-support matrix, defer slice 1b.

## Why this is the right kill-test (per spec-riskiest-assumption rule)

- Total time ≤ 15 min vs. ≥ 2 days for slices A/B/C
- Tests the actual host (ChatGPT web) not a proxy (MCP Inspector)
- Tests the actual wire format the binary already emits (no Go HTTP code change)
- Tests the actual transport (Streamable HTTP) ChatGPT documents
- Captures the auth-tier risk (Plus / Pro / Team / Enterprise gating)
- Reversible — no production state touched, tunnel URL is ephemeral
- Cites positive AND disconfirming evidence (per rule)
