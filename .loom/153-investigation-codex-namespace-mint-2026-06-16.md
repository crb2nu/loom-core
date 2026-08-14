# Investigation: Codex mobile sessions show malformed namespaces (`3ef2/...`, `////main`)

**Date**: 2026-06-16
**Trigger**: User report — "codex sessions are still a little broken in mobile" (iOS Roster groups a codex agent under "3ef2 / 3ef2" instead of a repo/branch like claude's "fi-fhir / libs/fi-fhir").
**Status**: Root cause CONFIRMED (data-driven). Fix IMPLEMENTED 2026-06-16 (branch `fix/codex-namespace-remote-derivation`): `inferGitNamespace` prefers git-remote `group/repo` (covers `3ef2/…`); `isMalformedNamespace` guard at the keepalive/heartbeat chokepoints discards `////main` before storing. Both in the `loom` CLI → self-healing on binary deploy. Tests: `TestProjectFromRemoteURL`, `TestIsMalformedNamespace`.

## Evidence (live `agent_session_list`, all minted 2026-06-16 20:19–20:35)

| agent | stored `namespace` | `project` | description |
|---|---|---|---|
| `codex-2197351258` | `3ef2/conspiracy-files` | `3ef2` | **Heartbeat bootstrap session** |
| `codex-1569945794` | `41fc/news-analyzer` | `41fc` | **Heartbeat bootstrap session** |
| `codex-1713039686` ×3 | `////main` | — | Codex keepalive wrapper session / "Codex CLI · ///" |
| `claude-code-…` (same fleet) | `services/flexinfer/master` ✓ | `services/flexinfer` ✓ | — |

The iOS app renders the stored value faithfully — this is a **mint-time** bug, not a rendering bug.

## Two codex minting paths, one shared root cause

Both derive the namespace from a **physical filesystem path** in a context where that path is not the canonical repo checkout.

### Path A — "Heartbeat bootstrap session" → `3ef2/conspiracy-files`
- Codex proxy heartbeat sets `proxyNamespace = inferGitNamespace()` (`cmd/loom/proxy_heartbeat.go:24-25`) and posts it; with no live session, `internal/hud/domain/fleet/handler_session.go:438` bootstraps one with that namespace + description "Heartbeat bootstrap session".
- `inferGitNamespace()` (`cmd/loom/cmd_agent_session.go:399-435`) returns `basename(dirname(repoRoot))/basename(repoRoot)/branch`.
- The codex agent runs in a **clone/spawn checkout** at `.../3ef2/conspiracy-files` (parent dir = a 4-hex hash). So `basename(repoRoot)`=`conspiracy-files` (correct), `basename(dirname)`=`3ef2` (the clone-dir hash), and `git branch --show-current` is empty (detached/fresh clone) → branch segment dropped → `3ef2/conspiracy-files`.
- **Proof it's path-derived, not remote-derived**: the canonical repo exists at `~/workspace/private/conspiracy-files` with remote `https://gitlab.flexinfer.ai/services/xfiles.git`. The stored name is the *local dir basename* `conspiracy-files`, NOT the remote name `xfiles` — so the value comes from the filesystem path, not git metadata.
- `3ef2`/`41fc` are NOT generated anywhere in loom-core (grep-confirmed: no `%04x`/short-hash on the namespace path). They are real parent-directory names of hash-named clone/spawn workspaces.

### Path B — "Codex keepalive wrapper session" → `////main`
- Codex `notify` hook (`pkg/generator/configs_codex.go:368`) computes `$NS_PROJECT/$NS_BRANCH` via `hookNamespaceVars()` in the **detached** keepalive-wrap process where `WS_ROOT=$(git rev-parse --show-toplevel || $PWD)` is unresolvable → all-empty path segments → `////main`.
- This is the **same bug `ca61b14f` (2026-06-14, MR !711/!712) claimed to fix** — that change moved the computation from server-side `--infer-namespace` to the hook's `$NS_PROJECT/$NS_BRANCH`, but did not fix the unreliable *context*. `inferGitNamespace`'s degenerate-segment guard (`cmd_agent_session.go:420`) only protects the inferred path, NOT the explicitly-passed `--namespace` (stored verbatim by keepalive-wrap, `cmd_agent_presence.go:228`).

### Why claude is unaffected
Claude runs in canonical workspace paths (`services/flexinfer`, `libs/fi-fhir`) where the parent is a real bucket (`services`/`libs`), and its SessionStart hook resolves `WS_ROOT` in the interactive context (+ `session-cwd` stamp).

## Recommended fix (unified — covers both)

1. **Prefer git remote identity over physical path** in namespace derivation: parse `git config --get remote.origin.url` → `group/repo` (e.g. `services/xfiles`), falling back to the path-based `dirname/basename` only when no remote. Stable across clone/spawn locations → fixes Path A. (Caveat: maps `conspiracy-files`→`xfiles`; that is the *correct* canonical identity.)
2. **Validate namespace at the storage chokepoints** (`keepalive-wrap` in `cmd_agent_presence.go`, heartbeat-bootstrap in `handler_session.go`): reject/repair namespaces containing empty path segments (`////main`, `///`) — fall back to inferred-from-remote or a muted "unknown" rather than storing garbage. Covers Path B (no-git detached context where even remote lookup fails).
3. (Optional, low value) iOS cosmetic guard: if `project`/`namespace` is a bare hash, show muted "—". Masks, doesn't fix — skip in favor of 1+2.

**Deploy note**: fixes 1–2 are in the `loom` CLI (`cmd/loom`) — the single binary chokepoint all codex hooks flow through; once the binary updates on each host, malformed namespaces are sanitized regardless of stale generated hooks. Hook-generator changes additionally need `loom sync codex --regen`.

## Open question
Exact provenance of the `3ef2`/`41fc` parent dir (which spawn mechanism — Mills `git-clone` pod, HUD spawn, or ephemeral codex clone). Not required for the fix (remote-based derivation is path-location-independent), but worth confirming for the deploy/rollout story.

## Sources
- `cmd/loom/proxy_heartbeat.go:24-25,97-110`; `cmd/loom/cmd_agent_session.go:399-447`; `cmd/loom/cmd_agent_presence.go:228-239`
- `internal/hud/domain/fleet/handler_session.go:438`
- `pkg/generator/configs_codex.go:360-372`; `pkg/generator/configs_hooks.go:34-44`
- Live: `agent_session_list` (status=active), `agent_presence_list`; remotes of `~/workspace/{private/conspiracy-files,services/news-analyzer}`
- Prior: CHANGELOG `[Unreleased]` codex `////main` entry; MRs !706/!708/!711/!712; memory `project_agent_namespace_minting`
