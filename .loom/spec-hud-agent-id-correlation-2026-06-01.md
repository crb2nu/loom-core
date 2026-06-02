# Spec: Make interactive-agent tool activity correlate to its HUD session

**Status**: Draft — design decision required before implementation
**Author**: Claude (Opus 4.8), 2026-06-01
**Depends on / completes**: !583 (audit agent_id), !588 (embedded event bridge),
!591/!592 (mirror tool-call forward), !593 (mirror light-path). All merged. This
spec is the **keystone** that makes their plumbing actually populate the HUD.
**Related memory**: `project_hud_live_session_activity_channel`

## Problem (one line)

An interactive agent's MCP tool calls reach the HUD with an **empty `agent_id`**
and a **proxy-lease `session_id`** that does not match the agent's HUD roster
session id — so the HUD cannot attribute the activity to the session, and every
"Live Sessions" row shows **0 calls** despite all the upstream plumbing working.

## Riskiest assumption + kill-test

**Load-bearing assumption**: The proxy can produce, for a given interactive
agent, an `agent_id` that `eventMatchesSessionTrace`
(`internal/hud/app_routes_operations.go:281-292`) will treat as belonging to
that agent's HUD roster session — *without* the proxy knowing the agent's Claude
session_id. Specifically: the workspace-scoped key `<type>-<WS_HASH>` (which the
proxy CAN compute) is sufficient to correlate, if the HUD matches on that base
rather than the full `<type>-<WS_HASH>-<SESSION_SCOPE>` roster id.

**Kill test** (≤30 min, on the **deployed** HUD, with a REAL agent call — NOT a
hand-supplied session id, which is how !583/!588's kill-tests gave false
confidence): build the proxy + HUD change, run them on one real agent in this
workspace, make it issue one MCP tool call, then `GET /api/sessions/<roster
session id>/trace` and confirm the call appears in `events`. Observable,
unambiguous, end-to-end.

**Failure mode if wrong**: We ship a proxy id change + HUD base-match and the
call still doesn't surface — e.g. because the roster session's `agent_id` is not
actually `<type>-<WS_HASH>-…` for this agent, or the time-bounds gate rejects it,
or base-matching mis-attributes across concurrent sessions in a way that makes
the panel useless.

**Status**: **FAILED 2026-06-02** (deployed HUD, real-agent call). The positive
half (cache file contains the roster format, WS_HASH=552019522 matches roster ids
`claude-code-552019522-*`) is verified below. The negative half FAILED: with
Slices 1+2 merged (`f30b37e8`) and the deployed `mobile-hud` running an image
built after the merge (`registry.harbor.lan/mcp/loom-core:20260602-133612`,
`trace_enabled:true`), a real interactive-agent session still renders **0 calls**.
See "Deployed kill-test 2026-06-02" below.

## Deployed kill-test 2026-06-02 — FAILED (evidence)

Run as this very Claude Code session (a real agent in this workspace), reaching
the deployed HUD API from inside the cluster (`k8s_exec` into pod
`mobile-hud-7d758d74f9-g8zn9`, loom-hub ns; the ingress is CF-Access gated).

- My roster session on the deployed HUD: `097d365da080b4b4`, agent_id
  `claude-code-552019522-3713702947`, ns `services/loom-core/docs/changelog-…`.
- `GET /api/sessions/097d365da080b4b4/trace`: `events[]` had ~30 entries, **all
  `agent.heartbeat` / `agent.context.telemetry`, zero `tool.call`**; `traces[]`
  empty; `entries[]` empty — despite ~15 real MCP calls this session.
- **All 6 active sessions** on the deployed HUD: `tool.call` hits = **0**.
- The central daemon's audit IS being written (`/home/mcp/.config/loom/audit.jsonl`,
  48,252 lines, last entry seconds old) but every entry carries **`agent_id:""`**
  (e.g. `{"server":"agent_context","tool":"agent_context_search","agent_id":"",
  "target":"local"}`).

**Why Slices 1+2 are inert on this path** (⚠️ PARTIALLY SUPERSEDED — see
"Correction 2026-06-02" below: the local proxy→daemon path is **not** inert; it
stamps the stable id correctly. The empty-`agent_id` entries are two distinct
populations, and the real cross-host fix is relocated to `buildForwardRequest`).
`emitAudit`
(`internal/daemon/daemon_call.go:219`) publishes `EventToolCall` only
`if d.eventBus != nil && params.SessionID != ""`, and stamps the audit with
`agentID`. For interactive-agent calls reaching the **central** daemon via the
proxy/hub (`target:local`), **both `params.SessionID` and `agentID` are empty**.
So: (a) no `tool.call` event is ever published → eventLog channel empty; (b) the
audit entry's `agent_id` is "" → `filterSessionTraceAuditEntries` /
`eventMatchesSessionTrace` base-match guards against empty (`app_routes_operations.go:292`)
→ no match. The proxy's `<type>-<WS_HASH>` derivation (Slice 1) and the HUD
base-match (Slice 2) never get a non-empty agent_id to work with on this path.

**Verdict.** The keystone is upstream of Slices 1+2: the proxy/hub→central-daemon
call path must propagate the agent's **session_id AND agent_id** into the daemon
`params` so `emitAudit` stamps them. This matches memory
`project_hud_live_session_activity_channel` item #5 (the "empty agent_id" keystone),
now CONFIRMED on the deployed central daemon — not just locally.

**Next slice (unblocks both specs):** thread a stable identity into the daemon
call params for proxy-routed calls — set `LOOM_AGENT_ID` (read in
`cmd/loom/proxy_session.go:36`) / `--agent-hint` to the SAME id the CLI session
hooks register, and ensure the central daemon receives `params.SessionID` +
`params.AgentID` for `target:local` interactive calls. Re-run THIS kill-test
against the deployed HUD before any further downstream work.

## Correction 2026-06-02 (local rebuild + isolated kill-test)

The "rebuild local HUD + retest" path was run with a fresh isolated daemon built
from current `main` (`b0129e86`). Two claims in the deployed-kill-test verdict
above are corrected by **live evidence** (raw audit kept at `/tmp/kt/audit.jsonl`):

**1. The merged correlation code is NOT inert — the proxy→daemon path stamps the
stable id correctly.** Driving one `loom proxy --agent-hint claude-code` tool call
(`time__get_current_time`) through an isolated current-`main` daemon with
`LOOM_AUDIT_ENABLED=true` produced this audit entry:

```json
{"agent_id":"claude-code-2236829634","server":"time","tool":"get_current_time","status":"success"}
```

`claude-code-<cksum>` is exactly the workspace-scoped base the HUD base-matches
(`agentIDMatchesSession`). So Slice 1 (proxy derivation) + Slice 2 (HUD base-match)
DO work end-to-end for an interactive call that reaches the daemon **the proxy is
connected to**. The cksum differs from 552019522 only because the isolated daemon's
git-toplevel differed; the form is identical.

**2. The empty-`agent_id` entries are TWO distinct populations, not one.**
- (a) **The HUD's own background fleet-monitor polls.** The same isolated run wrote
  **28** `agent_id:""` entries — all `agent_session_list` / `agent_memory_stats` /
  `agent_context_search` / `devbox_summary` / `codebase_stats`, i.e. the embedded
  HUD's monitor polling its own backends. These legitimately have no agent identity
  (they originate from the daemon/HUD, not a proxy). The deployed central daemon
  runs the same embedded HUD → most of its 48k `agent_id:""` lines are this noise,
  **not** interactive calls. The prior verdict mistook this noise for the dev
  session's calls.
- (b) **Genuinely hub-forwarded interactive calls that lose identity in transit.**
  `agent_context` + `devbox` are hub-delegated by default
  (`internal/daemon/hub_delegate.go:18-20`). This session's `--hub-prefer` daemon
  forwards those calls over the hub to the **central** daemon, where they execute
  `target:local` and are re-audited. But the forwarded request is built by
  `buildForwardRequest` (`internal/daemon/callpipeline_stages.go:302-334`) as a
  **bare standard `tools/call`** (`{name, arguments}`) — it does NOT carry
  `p.params.AgentID` / `p.params.SessionID`. So the central daemon's `callParams`
  parse them as empty → central audit stamps `agent_id:""`.

**3. The prior "local test was stale code" diagnosis was also wrong.** The running
local daemon is `~/.local/bin/loomd` (started `Jun 1 18:54`, **after** `f30b37e8`
merged `18:51`) — not the repo `bin/loomd` (26 May) the prior session checked. The
local kill-test showed `trace_enabled:false` simply because the local daemon has
**audit disabled** (no `~/.config/loom/audit.jsonl`). With audit enabled, traces
render.

**Relocated keystone fix.** The cross-host (central-HUD) gap is NOT "the central
daemon's call params" in isolation, and NOT `--agent-hint`/`LOOM_AGENT_ID` (already
set; local stamping already works). It is the **forwarding** daemon's hub egress:
`buildForwardRequest` must inject `agent_id` + `session_id` into `forwardParams`
when `p.target == hub`, so the receiving central daemon stamps them.

**UNVERIFIED sub-assumption (gate before shipping the fix):** that the central
daemon's hub ingress parses `agent_id`/`session_id` out of an incoming `tools/call`
params blob into its own `callParams`. If it does not, the egress injection is
inert and the ingress must be patched too. Verify this (read the central daemon's
hub-server request handling, or add the injection + a deployed re-test) BEFORE
declaring the fix done.

**Same-host path (free win):** for the LOCAL HUD to show an interactive session's
calls, only **enable audit on the local daemon** — no code change. The correlation
already works there (proven above).

## Verified facts (2026-06-01, this machine)

1. **Hook-registered id** (`pkg/generator/configs_hooks.go:456-480`,
   `hookAgentIDBootstrap`): `AGENT_ID = <type>-<WS_HASH>[-<SESSION_SCOPE>]`,
   where `WS_HASH = cksum(git toplevel)` and `SESSION_SCOPE = cksum(Claude
   session_id)` when hook JSON carries one. Cached in
   `~/.cache/loom/agent-id-<type>-<WS_HASH>[-<SESSION_SCOPE>]`. The CLI
   (`cmd/loom/cmd_agent_session.go`) uses `--agent-id` verbatim — no defaulting.
2. **Roster ids for this workspace** are `claude-code-552019522-<SESSION_SCOPE>`
   (WS_HASH 552019522 = `cksum(/Users/cblevins/workspace/services/loom-core)`).
   **Three concurrent** observed: `…-665755975`, `…-753541032`, `…-2598570833`.
3. **Proxy-derived id** (`cmd/loom/proxy_heartbeat.go:65-126`,
   `resolveProxyIdentity`): for **codex** it is the deterministic
   `codex-<WS_HASH>` (`stableWorkspaceProxyAgentID`) — which DOES match the hook
   base. For **claude-code / gemini / kilocode / …** it is the process-scoped
   `claude-code-<host>-<pid>[-<nshash>]` — does NOT match, and changes per run.
4. **Proxy tool-call `agent_id`** (`cmd/loom/proxy_handlers.go:125-128,157`):
   `resolvedAgentID = resolveProxyIdentity(agentHintGlobal)` only when
   `agentHintGlobal` (`--agent-hint`, set from the profile in
   `pkg/generator/configs_targets.go:24`) is non-empty. Observed empty in
   practice → tool.call `agent_id=""`.
5. **`LOOM_AGENT_ID`** is READ only (`cmd/loom/proxy_session.go:36`, sent as
   `presence_agent_id`); it is **never SET/exported** by any hook, mcp.json env,
   or launch script. The hook writes its id to a cache file but nothing reads it
   back into the proxy.
6. **The proxy cannot learn its Claude session_id**: no `CLAUDE_SESSION_ID`/
   conversation-id env var is exposed to MCP servers; it is not passed via MCP
   initialize. So the proxy cannot compute `SESSION_SCOPE` → cannot reconstruct
   the exact per-session roster id when multiple sessions share a workspace.
7. **Two different session-id namespaces**: tool.call carries the 32-char proxy
   *lease* id (`d.sessions`, `generateSessionID`); the HUD roster session is the
   16-char agent-context id. They never equal — correlation MUST go through
   `agent_id` + time-bounds, not `session_id`.

## Correlation contract (the target)

`eventMatchesSessionTrace`: matches if `Data.session_id == sessionID` (won't fire
— namespaces differ) **OR** `evt.AgentID == agentID` AND within session time
bounds. So the lever is: make the tool.call's `agent_id` equal (or base-match)
the roster session's `agent_id`, and rely on the time-bounds gate.

## Options

| | Mechanism | Exactness | Layers | Risk |
|--|--|--|--|--|
| **A (recommended)** | Proxy derives deterministic `<type>-<WS_HASH>` for ALL platforms (extend codex's `stableWorkspaceProxyAgentID`); uses it as the call `agent_id` + `presence_agent_id`. HUD relaxes `eventMatchesSessionTrace`/`filterSessionTraceAuditEntries` to base-match `agent_id` on the `<type>-<WS_HASH>` prefix (boundary-aware). | Workspace+type grouping. **Over-attributes** across concurrent same-workspace sessions (activity shows under all of them). | proxy + HUD | base-match could mis-group if WS_HASH prefixes collide — mitigate with separator-aware match (`a==b || a+"-" prefixOf b`). |
| B | Proxy globs newest `~/.cache/loom/agent-id-<type>-<WS_HASH>*` and uses it verbatim. | Exact for ONE session/workspace; **wrong** for concurrent (picks one). | proxy only | silent mis-attribution under concurrency (the live case: 3 sessions). |
| C | Claude Code passes its session_id to MCP servers (env/init); proxy computes exact `SESSION_SCOPE`. | Exact, per session. | upstream (Claude) + proxy | not in our control; needs a Claude Code feature. |

## Recommendation

**Option A**, accepting workspace-level grouping as a documented limitation
(perfect per-session attribution is impossible until C). Rationale: deterministic
and stable, no fragile file globbing or cross-process races, single new behavior
reused from the proven codex path, and it turns "0 calls everywhere" into
"calls attributed to the right workspace+agent-type." Over-attribution across
concurrent same-workspace sessions is strictly better than the status quo and is
honest (the panel can note "workspace-grouped").

Pair with the original-issue suggestion (first report) to "relax
filterSessionTraceAuditEntries to match on agent base/prefix" — that defense was
correct after all; it's the HUD half of Option A.

## Slices

1. **Proxy** (`cmd/loom/proxy_heartbeat.go`): make `stableWorkspaceProxyAgentID`
   apply to all agent types (not just codex); ensure `handleProxyToolsCall`
   sends it as `agent_id` even when `--agent-hint` is set but identity resolves
   workspace-scoped. Unit-test the derivation parity with the hook's
   `<type>-<WS_HASH>`.
2. **HUD** (`internal/hud/app_routes_operations.go`): boundary-aware base-match
   in `eventMatchesSessionTrace` + `filterSessionTraceAuditEntries` (`agentID ==
   evt.AgentID || strings.HasPrefix(evt.AgentID, agentID+"-")` and vice-versa).
   Unit-test no cross-workspace/type leakage.
3. **Deployed kill-test** (gating): the riskiest-assumption test above, on one
   real agent, against the deployed HUD. Ship only after it renders.

## Why this wasn't caught earlier (honest retro)

!583/!588/!591/!592 each verified with a **hand-supplied roster session_id**,
which masked that real proxy traffic carries an empty `agent_id` and a
non-matching session id. The lesson (now in memory): the kill-test must drive a
REAL agent call, never a synthetic id that happens to match.
