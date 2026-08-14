# Vendor Spec Authority

**Before modifying this package, fetch the current vendor config docs and cite the URL in code + tests.** Commit messages, prior code comments, and memory entries are unreliable anchors — regressions have been caused by trusting claims without verifying. Always re-verify.

Companion to `.agents/skills/mcp-config.md` (workspace skill) and the loom-core memory entries `feedback_vendor_specs_first.md` + `reference_vendor_config_docs.md`.

## Why this file exists

On 2026-04-18, every Codex call was prompting for approval because commit `848be7ef` (2026-04-09) emitted `approval_mode = "always"` in the Codex `[mcp_servers.<name>]` stanza. Neither `approval_mode = "always"` nor the previously-claimed `always_allow = ["*"]` are valid Codex keys. The correct key — `default_tools_approval_mode = "approve"` — was documented at [openai/codex#16501](https://github.com/openai/codex/issues/16501) but never consulted. This file is the durable anchor so future edits to the generator always have a doc URL to check.

## Pinned authoritative URLs

### Codex (OpenAI)
- **Docs host migrated (~2026-07):** `developers.openai.com/codex/*` now 308-redirects to `learn.chatgpt.com`. Canonical pins: https://learn.chatgpt.com/docs/extend/mcp?surface=cli (MCP), https://learn.chatgpt.com/docs/config-file/config-reference (config), https://learn.chatgpt.com/docs/hooks (hooks). Machine-readable schema: https://learn.chatgpt.com/codex/config-schema.json — good CI diff target.
- **Issues / schema discussions:** https://github.com/openai/codex/issues
- **Config file path:** `~/.codex/config.toml` (TOML)
- **Removed key value (2026-07-14):** `approval_policy = "on-failure"` was deleted in the 0.143 window (openai/codex#28418). Valid: `untrusted | on-request | never` (we emit `never`).
- **Hooks are default-on** since ~0.124; `[features] hooks = true` is a harmless no-op. Event set is now 10 (added `SubagentStart`, `SubagentStop`, `PreCompact`, `PostCompact`). Non-managed command hooks are trust-gated per definition hash — every regen that changes a hook re-triggers `/hooks` review (see `codex_hooks_trust.go`).
- **Known-good server-level auto-approval:**
  ```toml
  [mcp_servers.<name>]
  default_tools_approval_mode = "approve"   # valid: "approve" | "prompt"
  ```
  Per-tool override:
  ```toml
  [mcp_servers.<name>.tools.<tool>]
  approval_mode = "approve"   # or "prompt"
  ```
- **INVALID keys (do not emit):**
  - `approval_mode = "always"` — not a recognized value.
  - `always_allow = [...]` — not a Codex key. (Legacy Kilocode pre-1.0 key; still emitted for the Gemini TOML profile only.)

### Claude Code (Anthropic)
- **Docs:** https://docs.claude.com/claude-code
- **Config files:** `.claude/mcp.json` (JSON) + `.claude/settings.json` (hooks, permissions).
- **Tool allowlist** lives in `settings.json` `"permissions"` block, not `mcp.json`.

### Gemini CLI (Google)
- **Repo:** https://github.com/google-gemini/gemini-cli
- **Config files:** `.gemini/config.toml` + `.gemini/settings.json`.
- **Settings schema (2026-07-14):** top-level keys grew to 26 with `policyPaths` + `adminPolicyPaths`; root is still `additionalProperties: false`, so unknown keys (e.g. `agentConfig`) remain invalid. Hook events unchanged at 11. Schema: https://raw.githubusercontent.com/google-gemini/gemini-cli/main/schemas/settings.schema.json

### Kilocode
- **Docs:** https://kilo.ai/docs/automate/mcp/using-in-kilo-code and https://kilo.ai/docs/automate/mcp/using-in-cli (the kilocode.ai-era docs moved; legacy repo is `Kilo-Org/kilocode-legacy`, live monorepo `Kilo-Org/kilo`).
- **Config files:** Kilo 1.0 (extension v7.x, rebuilt on the OpenCode engine) reads `kilo.json(c)` — global `~/.config/kilo/kilo.json`, project `<root>/kilo.json` or `.kilo/kilo.json`. Legacy `opencode.json(c)` names are read as fallback.
- **MCP shape:** top-level `mcp` map; local servers use `type: "local"` + array `command` + `environment`; remote servers use `type: "remote"` + `url`. Timeouts in milliseconds. `$schema` is `https://app.kilo.ai/config.json`.
- **Permissions:** top-level `permission` map keyed by namespaced tool name `{server}_{tool}` with glob support (e.g. `loom_*: allow`). Replaces the legacy `always_allow` array.
- **INVALID keys (do not emit):** `always_allow` — legacy pre-1.0 surface; absent from current docs.
- **Runtime evidence (2026-07-14):** `kilocode.kilo-code-7.4.5` extension bundle — `globalConfigDir()` = `$XDG_CONFIG_HOME/kilo`, `MODERN=["kilo.jsonc","kilo.json"]`, `normalizeMcpEntry` accepts `local`/`remote`, `readLegacyMcpSettings` only migrates globalStorage `mcp_settings.json`; zero references to `config.toml` (the old `~/.kilocode/config.toml` emission was read by nothing). This resolved the 2026-07-14 core-tooling-audit DRIFT WARNING: the generator now emits `~/.config/kilo/kilo.json`.

### Antigravity (Google)
- **Docs:** https://antigravity.google/docs/mcp and https://antigravity.google/docs/hooks
- **About:** Google Antigravity 2.0 is the standalone agent-first desktop app; legacy IDE behavior should not be used as the generator authority.
- **MCP config files:** workspace `.agents/mcp_config.json`; home `~/.gemini/config/mcp_config.json` (the Antigravity 2.0 *shared* central config, read by IDE + CLI + SDK). The pre-2.0 path `~/.gemini/antigravity/mcp_config.json` is legacy — on the reference machine it survives only as a symlink to the shared config. Note: per google-antigravity/antigravity-cli#60, the CLI discovers workspace-local MCP config but silently ignores its `mcpServers` — only the home-level config actually loads servers.
- **MCP shape:** JSON with top-level `mcpServers`. Local servers use `command`/`args`/`env`; remote Streamable HTTP servers use `serverUrl` rather than `url`/`httpUrl`.
- **Hooks:** Antigravity 2.0 uses `hooks.json` under a customization directory such as `.agents/` or `~/.gemini/config/`. Loom emits an Antigravity-specific `hooks.json` wrapper (`{"loom": {"PreInvocation": ...}}`) because Claude/Gemini `settings.json` hook emission has the wrong top-level schema and stdout contract.
- **Permissions:** Loom MCP tools are auto-allowed through a native `PreToolUse` hook for `ask_permission`. The hook returns `decision=allow` plus `permissionOverrides=["mcp(loom/*)"]` when Antigravity asks about `mcp(loom/...)`.

### Zed
- **Docs:** https://github.com/zed-industries/zed/blob/main/docs/src/ai/mcp.md (drift check fetches the raw URL: https://raw.githubusercontent.com/zed-industries/zed/main/docs/src/ai/mcp.md — zed.dev serves 404s to non-browser fetchers, browser path is https://zed.dev/docs/ai/mcp; the old /docs/assistant/mcp path is legacy).
- **Config file:** `~/.config/zed/settings.json`, key `context_servers`. Zed core has NO mcp.json — the pre-2026-07 generator emission of `mcp.json` (root `mcpServers`) into `~/Library/Application Support/Zed/` was inert and has been removed. The generator now stages a `.zed/context_servers.json` fragment which `loom sync zed` merges into the user settings.json (hujson patch: comments, foreign context servers, and all other keys preserved).
- **Local (stdio) shape (verified 2026-07-14 against `crates/settings_content/src/project.rs`):** flat object. `ContextServerCommand.path` is serde-renamed to `"command"`; `timeout` is in seconds.
  ```json
  "context_servers": {
    "loom": { "command": "/path/to/loom", "args": ["proxy"], "env": {}, "timeout": 600 }
  }
  ```
- **Remote shape:** `{ "url": "...", "headers": { ... } }` (OAuth prompt when no Authorization header).
- **Approvals:** `agent.tool_permissions.default` (`"confirm"` | `"allow"` | `"deny"`), per-tool keys `mcp:<server>:<tool_name>` (Zed v0.224.0+). Loom relies on proxy enforcement instead.
- **Legacy nested shape is DEAD:** `{"command": {"path": ..., "arguments": [...]}}` no longer parses as a stdio server — `ContextServerSettingsContent` is `#[serde(untagged)]` and the Stdio variant requires a string `"command"`. An entry carrying a `settings` key deserializes as the Extension variant (for extension-provided servers like the dormant `services/loom-zed`), with any nested `command` object silently ignored. `loom sync zed` migrates such entries to the flat shape.
- **Loom extension (dormant, Tier 3):** `services/loom-zed` receives its config via the Zed extension API (`context_servers.<id>.settings`), never via mcp.json; documents fallback polling when upstream doesn't advertise `tools.listChanged`.

### VS Code
- **Docs:** https://code.visualstudio.com/docs/copilot/customization/mcp-servers
- **Config file:** `.vscode/mcp.json` (workspace) + user-profile `mcp.json`.
- **Root key is `servers`** — `mcpServers` is silently ignored by VS Code. The generator emitted `mcpServers` until 2026-07-14; locked by `TestGeneratedJSONConfig_VSCodeIncludesPolicyMetadata` and the `vscode` entry in `vendor_specs.yaml`.

### MCP spec (vendor-neutral)
- **Spec site:** https://spec.modelcontextprotocol.io
- **tools list-changed notification:** https://spec.modelcontextprotocol.io/specification/server/tools/
- Server advertises `tools.listChanged: true` in its `initialize` response if it *may* send `notifications/tools/list_changed`. Clients honoring the notification re-fetch `tools/list` on receipt.

## Workflow when editing `configs_formats.go`

1. Open the vendor doc URL above in a WebFetch *before* writing.
2. Add a doc-URL comment above the new key emission, e.g.:
   ```go
   // Codex server-level auto-approval: default_tools_approval_mode = "approve"
   // See https://developers.openai.com/codex/mcp + openai/codex#16501.
   ```
3. Add the same URL to the test assertion in `configs_test.go` so drift from the spec fails loudly at CI.
4. Commit message must cite the vendor doc URL. "Aligns with CLI standards" is not sufficient.

## Hook lifecycle surface

The generator wires a small set of *loom canonical* lifecycle events into each
vendor's native hook system. The table below is the authoritative mapping —
when adding or moving an event, update it and the corresponding emitter at the
same time.

### Loom canonical events

| Canonical | What it triggers | Loom CLI invocation |
|-----------|------------------|---------------------|
| `session-start` | Agent session begins or resumes | `loom agent session-start --namespace … --agent-id … --auto-recall` |
| `session-end` | Agent session terminates | `loom agent session-end --agent-id … --summarize` |
| `heartbeat` | Per-tool keepalive ping | `loom agent heartbeat --agent-id … --status active` |
| `task-sync` | TodoWrite / TaskCreate / TaskUpdate | `loom agent task-sync --agent-id …` |
| `pre-tool-use telemetry` | Optional pre-tool emit | `loom agent event-emit --hook pre-tool-use --platform …` |
| `post-tool-use telemetry` | Optional post-tool emit | `loom agent event-emit --hook post-tool-use --platform …` |
| `keepalive-wrap` (codex-style) | Background session wrapper | `loom agent keepalive-wrap …` |

### Vendor surface mapping

Verified May 2026 against the URLs above plus:
- Claude Code hooks: <https://code.claude.com/docs/en/hooks>
- Gemini CLI hooks: <https://github.com/google-gemini/gemini-cli/blob/main/docs/hooks/reference.md>
- Codex hooks: <https://developers.openai.com/codex/hooks>
- OpenCode plugin: <https://opencode.ai/docs/plugins>
- Kilocode plugin: <https://github.com/Kilo-Org/kilocode/blob/main/packages/plugin/src/index.ts>
- Antigravity hooks: <https://antigravity.google/docs/hooks>

| Canonical | Claude Code | Codex | Gemini CLI | OpenCode | Antigravity | Kilocode / VSCode / Zed |
|-----------|-------------|-------|------------|----------|-------------|-------------------------|
| session-start | `SessionStart` | `notify` (turn-end keepalive) + `[[hooks.SessionStart]]` (hooks.json, GA 2026-05-07) | `SessionStart` | TS `sessionCreated` | `PreInvocation` | — |
| session-end | `SessionEnd` *(per-session — was incorrectly `Stop`, fixed 2026-05-12)* | `notify` + keepalive-wrap deregister-on-exit (no `SessionEnd` event exists) | `SessionEnd` | TS `sessionDeleted` | `Stop` only when `fullyIdle=true` | — |
| heartbeat | `PostToolUse` matcher `Bash\|Task` | `[[hooks.PostToolUse]]` (hooks.json) + `notify` (rate-limited stamp file) | `AfterTool` matcher `run_shell_command` | TS `toolExecuteAfter` | `PostToolUse` matcher `run_command\|manage_task\|invoke_subagent` | — |
| task-sync | `PostToolUse` matcher `TaskCreate\|TaskUpdate\|TodoWrite` | `[[hooks.PostToolUse]]` matcher `update_plan` (hooks.json) → `loom agent task-sync` (bridges Codex's native plan tool; reconciled by session+title) | — | — | — |
| pre-tool-use telemetry | `PreToolUse` | — (use `[[hooks.PreToolUse]]` once we emit it) | `BeforeTool` | — | `PreToolUse` | — |
| post-tool-use telemetry | `PostToolUse` (extras) | `notify` (telemetry_eventEmit extra) | `AfterTool` (extras) | TS `toolExecuteAfter` | `PostToolUse` | — |
| GitOps policy guardrail | `PreToolUse` matcher `Bash` (block kubectl mutations) | — (proxy-enforced) | `BeforeTool` matcher `run_shell_command` | (plugin) | `PreToolUse` matcher `run_command` | (proxy-enforced) |

Notes on naming asymmetry (read before adding an event name):
- Claude uses `PreToolUse` / `PostToolUse`. Gemini uses `BeforeTool` / `AfterTool`. **Do not unify them.**
- **`Stop` is per-turn on both Claude and Codex** — fires every time the model finishes responding to a single prompt. It is **not** session-end. Use `SessionEnd` (Claude/Gemini) or notify+keepalive (Codex) for terminal session signals. Mapping session-end to `Stop` was a bug that fired `loom agent session-end --summarize` every turn (fixed 2026-05-12).
- Codex has **no `SessionEnd` event**. Terminal session signal comes from the keepalive-wrap background process exiting when the codex CLI dies; deregister-on-exit handles presence cleanup.
- Claude has `PreCompact` (NOT `PreCompress`). Gemini has `PreCompress`. They are distinct vendor names and not interchangeable.
- Codex `PreToolUse` event exists but image-gen tools do not yet fire it ([openai/codex#20616](https://github.com/openai/codex/issues/20616)).
- Codex `Stop` runs at turn scope (verbatim from docs: "PreToolUse, PermissionRequest, PostToolUse, UserPromptSubmit, and Stop run at turn scope").

### Codex `[hooks]` block

Codex shipped a Claude-style `[hooks]` block in v0.129.0 (2026-05-07). The
generator emits **both** surfaces:

- **`config.toml`** retains `notify = [...]` (still supported; deprecation
  attempt PR #20524 was reverted in #21152) — covers turn-end keepalive
  via the `keepalive-wrap` background process (deregister-on-exit).
- **`hooks.json`** (generated alongside `config.toml`, copied to
  `~/.codex/hooks.json`) carries `SessionStart` (mapped to
  `loom agent session-start --auto-recall`) and `PostToolUse` (mapped to
  `loom agent heartbeat --ensure-session`).
- Codex loads `hooks.json` because the generator writes
  `[features] hooks = true` in the rendered `config.toml`.

`Stop` is **intentionally absent** from the emitted `hooks.json`. Codex
docs say "PreToolUse, PermissionRequest, PostToolUse, UserPromptSubmit,
and Stop run at turn scope" — so `Stop` would fire every turn. Codex has
no `SessionEnd` event; true session termination is handled by the
keepalive-wrap process exiting with the codex CLI and calling
`/api/agent/deregister`. See `pkg/generator/configs_hooks.go` for the
`hp.SessionEndEvent != ""` gate that suppresses the session-end block
when the profile sets `session_end_event: ""`.

Tests: `TestGenerateHooksConfig_CodexEmitsHooksJSON` (asserts SessionStart
+ PostToolUse present, Stop absent) and
`TestVendorCapabilities_CodexHasNotifyAndHooks`.

### Codex execpolicy rules (`~/.codex/rules/default.rules`)

Verified 2026-07-25 against
<https://learn.chatgpt.com/docs/agent-configuration/rules.md>:

- Codex scans `rules/` directories next to active config layers at startup;
  the user layer is `~/.codex/rules/*.rules` (Starlark syntax).
- `prefix_rule(pattern, decision, justification, match, not_match)` —
  `pattern` is a list whose elements are literals (`"git"`) or unions
  (`["view", "list"]`); `decision` is `"allow"` (run **outside the sandbox**
  without prompting), `"prompt"`, or `"forbidden"`, defaulting to allow;
  `match`/`not_match` rows are execpolicy self-test examples.
- Validate with
  `codex execpolicy check --pretty --rules ~/.codex/rules/default.rules -- git commit -m msg`.

Why the generator owns a block here: `sandbox_mode = "workspace-write"`
force-mounts `.git` (including resolved worktree gitdirs) **read-only by
design** with no config override
([openai/codex#15505](https://github.com/openai/codex/issues/15505)), and our
`approval_policy = "never"` removes the escalation prompt — so `git
commit`/`push` wedge with "cannot lock ref ... Operation not permitted". An
allow-rule for `git` is the vendor-supported escape hatch.

Generator/sync contract (`pkg/generator/codex_rules.go`,
`pkg/sync/ops_codex.go`):

- Rules come from `platform_permissions.codex.settings.rules` in the registry
  and render as a block delimited by `CodexRulesBeginMarker`/`EndMarker`.
- **The Codex TUI auto-appends approval rules to the same file**, so sync
  merges the managed block in place (`MergeMarkerBlock`) — everything outside
  the markers survives every `loom sync codex --regen`.
- Deliberately NOT in the sync profile's `ExtraGeneratedFiles`: extras are
  plain-copied (would clobber appended rules) and swept from workspace
  projects by `CleanAllProjectsGenerated` (would delete hand-authored
  repo-local `.codex/rules/` files).

Tests: `TestGenerateConfigs_RealRegistryCodexRules`,
`TestRenderCodexRulesBlock`, `TestMergeMarkerBlock`,
`TestSyncCodexRulesGenerated`.

### When updating this section

1. Re-fetch each vendor docs URL above before changing the table.
2. If you add a new event, also:
   - Update `pkg/generator/platform_profiles.yaml` (`hooks.events` for the affected platform)
   - Update `pkg/generator/configs_hooks.go` (`canonicalTelemetryHookForEvent`)
   - Add a `must_contain` / `emitted_keys` assertion to `pkg/generator/vendor_specs.yaml`
   - Run `loom vendor-specs check` to confirm the assertion holds
3. Keep `notify` and `[hooks]` in sync for Codex during the transition window.

## Future work (tracked as proposal)

A scheduled task that periodically fetches each vendor doc + parses the canonical config-key names + diffs against generator test fixture strings would surface vendor drift before it hits production. Proposed but not yet built — see `.loom/` planning addendums if/when picked up.
