# Research Brief: Antigravity 2.0 Support

## Question

What does loom-core need to change so the `antigravity` sync target supports Google Antigravity 2.0 rather than the legacy VS Code-fork assumptions?

## Findings

- Google Antigravity 2.0 expects MCP servers in a dedicated `mcp_config.json` file with a top-level `mcpServers` object. Official docs list the home path as `~/.gemini/antigravity/mcp_config.json`.
- Antigravity 2.0's workspace customization surface is `.agents/`; third-party and migration docs consistently point workspace MCP config at `.agents/mcp_config.json`.
- Remote MCP entries use `serverUrl` for Streamable HTTP. Loom's current Antigravity path emits local stdio proxy config, so no remote-field conversion is needed in this slice.
- Antigravity 2.0 supports JSON hooks, but the contract is not Claude/Gemini-compatible: `hooks.json` maps hook names to event config, and hook commands must return event-specific JSON on stdout. Reusing the existing `settings.json` hook/stub path would be misleading.
- Antigravity's `ask_permission` hook can grant `permissionOverrides`; Loom should use that native path to auto-allow `mcp(loom/...)` tools instead of inventing unsupported MCP-config permission keys.
- Global skills remain under `~/.gemini/antigravity/skills/`; workspace skills now default to `.agents/skills` with `.agent/skills` kept as backwards compatibility by Antigravity.

## Implications

- The platform profile should generate `mcp_config.json` for the `antigravity` target.
- The sync profile should use `.agents` as its repo/workspace directory, sync MCP home config to `~/.gemini/antigravity/mcp_config.json`, and sync hooks to `~/.gemini/config/hooks.json`.
- The old Antigravity `settings.json` hooks stub should be removed from sync expectations and replaced with an Antigravity-specific `hooks.json` generator.
- Doctor/status logic should look for `.agents/mcp_config.json` + `.agents/hooks.json` first, then home `~/.gemini/antigravity/mcp_config.json` + `~/.gemini/config/hooks.json`.

## Recommended Slice

1. Update `pkg/generator/platform_profiles.yaml` and `pkg/sync/manager.go` for Antigravity 2.0 paths.
2. Update doctor detection and docs/vendor-spec notes.
3. Add a native `hooks.json` generator for `PreInvocation`, `PreToolUse`, `PostToolUse`, and fully-idle `Stop`, including JSON stdout decisions.
4. Add regression tests pinning the new paths, hook shape, telemetry normalization, GitOps guardrails, and Loom MCP autoallow.

## Sources

- Google Antigravity MCP docs: https://antigravity.google/docs/mcp
- Google Antigravity hooks docs: https://antigravity.google/docs/hooks
- Google Antigravity skills docs: https://antigravity.google/docs/skills
- Google Antigravity 2.0 overview/features docs: https://antigravity.google/docs and https://antigravity.google/docs/features
- Local source: `pkg/sync/manager.go`, `pkg/generator/platform_profiles.yaml`, `pkg/generator/doctor.go`, `pkg/generator/VENDOR_SPECS.md`
