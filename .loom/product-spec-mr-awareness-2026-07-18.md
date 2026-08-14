# Product spec — Branch-MR awareness for agents ("no MR left behind")

Date: 2026-07-18. Owner: claude-code (session vigorous-faraday). Roadmap:
`.loom/190-roadmap-core-refinement-2026-07-18.md` (inserts as Wave 1.5).

## Riskiest assumption + kill-test

**Load-bearing assumption**: The two delivery channels work on current tooling:
(a) a Claude Code `UserPromptSubmit` hook whose stdout emits
`{"hookSpecificOutput":{"hookEventName":"UserPromptSubmit","additionalContext":"…"}}`
reliably lands that context in the model's next turn (current CLI version, our
generated settings.json), and (b) the loom proxy can append a short trailer to
proxied tool results without breaking any client (Claude, Codex, Gemini,
Antigravity).

**Kill test** (≤30 min): (a) scratch project with a hand-written
`UserPromptSubmit` hook emitting sentinel `MR-AWARENESS-KILLTEST-4711`; run
`claude -p "repeat any bracketed sentinel present in your context"`; PASS iff the
sentinel is echoed. (b) is already proven in production: the proxy's
`[loom] output truncated to 48000 bytes` trailer IS result mutation across all
four clients — cite `LOOM_PROXY_MAX_TOOL_RESULT_BYTES` handling in
`cmd/loom/proxy*.go`. Negative search: check Claude Code changelog/docs that
`additionalContext` was not removed/renamed for UserPromptSubmit.

**Failure mode if wrong**: (a) fails → drop hook channel, rely on proxy trailer +
inbox (still covers all vendors at point-of-action); (b) fails → hook channel +
inbox only. The registry (M1) is channel-agnostic and survives either outcome.

**Status**: (a) passed 2026-07-18 — headless `claude -p` (haiku) with a scratch
`UserPromptSubmit` hook echoed sentinel `MR-AWARENESS-KILLTEST-4711` + full MR
state string verbatim (this session; scratchpad `mr-killtest/`). (b) passed
2026-07-18 (production truncation-trailer evidence). All slices unblocked.

## Problem

Agent-created MRs stall silently: conflicts after main moves, red CI from
transient flakes, MWPS never armed / dropped on push, skipped head pipelines,
Renovate "unchecked" states. Sessions end; nothing owns the MR. Today (2026-07-18)
five MRs sat stalled for up to 11 days until a manual sweep. Prior fixes tried
hooks (unreliable at the time — unknown-event validation killed whole hook sets,
since fixed by probe-validated generation !686) and MCP Apps widgets (dead end:
Claude Code does not render them — see
`.loom/brainstorm-widget-rendering-breakdown-2026-05-17.md`).

## Solution — three layers

### L1 — Branch→MR status registry (source of truth) [M1, M2]

New HUD daemon component `internal/hud/mrwatch`:
- Polls GitLab group MRs (reuse the mills GitLab client; group token
  `loom-mills-gitlab-group`; 60–120s interval, jittered; bounded to repos with
  recent agent activity).
- Classifies every open MR into a stall taxonomy:
  `ok | awaiting_pipeline | ci_running | ci_failed_flaky | ci_failed_deterministic
  | conflict | automerge_unarmed | pipeline_skipped | stale_branch | draft_idle`.
  Flaky-vs-deterministic reuses `pkg/mills/audit.ClassifyCIFailureMessage`.
- Joins MR source branch → owning agent/session via the coordination snapshot +
  agent-context worktree/presence registry (branch name is the join key).
- Surfaces:
  - `GET /api/agent/mr-status?branch=&repo=` and `GET /api/mrwatch/summary`
    (HUD REST :3333).
  - MCP tool `agent_mr_status` in mcp-agent-context (add to llm-core priority
    tail — bump the cap per memory `project_cross_vendor_session_bridge`).
  - `loom agent mr-status --branch <b> [--repo <r>] [--json|--brief]` CLI.
  - Attention-lane `merge` events on unhealthy transitions (existing lane
    contract, classify by type first).

### L2 — Delivery into agent context [M3a, M3b]

- **Proxy trailer (all vendors, point-of-action)** [M3b]: when a session calls
  `git_push`, `git_status`, `git_commit`, or any `gitlab` MR tool through the
  loom proxy, the proxy consults L1 for the current repo/branch and appends
  `\n[loom] MR !NNN (<branch>): <state> — <one-line action>` to the tool result.
  Delta-gated: emit only on state change or unhealthy state (per-session
  last-seen hash). Same mechanism as the existing truncation trailer.
- **Hook additionalContext (Claude/Gemini)** [M3a, gated on kill-test (a)]:
  generated `UserPromptSubmit` (Claude) / `BeforeModel` (Gemini) hook runs
  `loom agent mr-status --brief --delta`; emits context only when unhealthy or
  changed (state cached under `~/.loom/mrwatch/<session>.state`). Codex has no
  context-injection hook — covered by proxy trailer + notify.
- **Durable inbox** : on transition into any unhealthy state, mrwatch sends
  `agent_message_send` to the owning agent (bridge !1083) so even a future
  session recalls it; plus HUD nudge.

### L3 — Shepherd (bounded autonomy) [M4]

Reconciler loop in the HUD daemon (NOT mills; interactive-agent MRs are out of
mills' remit): per unhealthy MR, bounded auto-actions with a per-MR daily budget
(default 2 actions):
- `ci_failed_flaky` → retry pipeline once (flaky classes: golangci schema fetch,
  gitconfig lock, runner pod, bench-gate — from the audit classifier).
- `pipeline_skipped`/missing head pipeline → create pipeline for the ref.
- `automerge_unarmed` + green/running CI + MR open >30 min → arm MWPS
  (re-arm after pushes; memory `feedback_automerge_drops_on_push`).
- mills escalation issue whose MR merged → close (extends ghost-spark sweep).
- NEVER: auto-rebase, force-push, or merge without CI. Conflicts and
  deterministic failures → escalate only (inbox + attention lane + issue label).
All actions audit-logged (`/api/mrwatch/actions`), kill-switchable via env
`LOOM_MRWATCH_SHEPHERD=off`.

## Non-goals

- MCP Apps widget rendering (dead on Claude Code).
- Cross-forge (GitHub PRs) in v1.
- Auto-resolving merge conflicts.

## Slices

| ID | Scope | Gate |
|---|---|---|
| M1 | `internal/hud/mrwatch` registry + classification + REST + tests | — |
| M2 | `agent_mr_status` MCP tool + `loom agent mr-status` CLI (+ tool-cap bump) | M1 |
| M3a | Hook generator: UserPromptSubmit/BeforeModel delta-gated injection | kill-test (a) |
| M3b | Proxy trailer injection on git/gitlab tool results | M1 |
| M4 | Shepherd bounded auto-actions + audit log + kill switch | M1 |
| M5 | Inbox nudges + attention-lane wiring + iOS surfacing | M1 |

## Evidence

- Stall inventory 2026-07-18: !982 (11 days, skipped pipeline), !1097/!1126
  (flaky lint schema fetch, 2 days), !1098/!1128 (conflicts after main moved).
- Proxy result mutation in prod: `[loom] output truncated to 48000 bytes`
  trailer observed on `k8s_get`/`list_issues` results this session.
- Hook-death root cause + fix: memory `project_claude_hook_event_validation`
  (!686 probe-validated generation).
- Widget dead-end: `.loom/brainstorm-widget-rendering-breakdown-2026-05-17.md`.
