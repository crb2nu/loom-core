# Dev Build Lifecycle (Agent-Safe)

## Hub devbox fingerprinting and base-image selection

In `git-clone` sync mode the repository does not exist locally when mcp-devbox
fingerprints the project. The manager therefore fetches the project's
dependency manifests (`go.mod`, `package.json`, `pyproject.toml`, lockfiles,
`.devbox.yaml` — the set `detect.DependencyFiles()` names) from the default
branch through the GitLab repository files API and fingerprints that copy.
The result is the same language, version, and content hash a workstation
checkout produces, the Dockerfile is the language template on the registered
`internal/devbox/baseimage` entry, and the registry is trusted as a cache:
the image is rebuilt only when a manifest or the recipe changes. The image
tag (`mcp/devbox/<project>:<hash>`) is `detect.RecipeHash`: the dependency
hash folded with the rendered Dockerfile, so two managers only share a tag
when they would build the same image — a template or base-image change
yields a new tag instead of inheriting whatever an older manager pushed under
the old one. Manifests are
cached under `DEVBOX_CACHE_DIR/remote-manifests/<bucket>/<project>` for
`DEVBOX_REMOTE_MANIFEST_TTL` (default `10m`), a failed refresh serves the
cached copy, and `DEVBOX_REMOTE_MANIFESTS=0` disables hydration. The API call
authenticates with `DEVBOX_GIT_TOKEN` when set, otherwise with the `token` key
of the `DEVBOX_K8S_GIT_SECRET` secret read through the Kubernetes API — the
hub's service account needs `get` on that secret (`platform/gitops`
`k3s/devbox/rbac.yaml`); without it the fetch runs anonymously and private
projects fall back to the generic path below.

When no manifests are reachable (no token, an unreachable host, a repository
with none of the files) the fingerprint is empty and the Kubernetes build pod
inspects the cloned build context before Buildah runs, reads the declared
runtime version, and selects the matching image from
`internal/devbox/baseimage`. For example, a cloned repository declaring Go
1.26 builds from `registry.harbor.lan/mcp/devbox-base/go:1.26`. This
"generic" image is keyed by the empty-input hash, so it cannot be trusted from
the registry and is rebuilt on every cold call — which is why the remote
manifest path above exists.

An empty detection result or a version without a registry mapping retains the
generic fallback image, emits a `base_image_fallback` structured event, and
increments `loom_devbox_base_image_fallbacks_total`. At server startup every
registered tag is also probed through the Registry v2 manifest endpoint with
readable image-pull-secret credentials. The probe uses the build path's
Buildah-compatible insecure TLS posture by default; set `DEVBOX_REGISTRY_CA_FILE`
to use a trusted CA bundle instead. Results are classified as `available`,
`missing`, `unauthorized`, or `transport`, exported through
`loom_devbox_base_image_probe{language,version,outcome}`, and never block startup.

The hub's devbox deployment enables `MCP_SHARED_CHILD=1`, so custom-server
keeps one `mcp-devbox` child alive across WebSocket reconnects. This preserves
the asynchronous build tracker and token cache: a build started by one operator
session remains visible to the next session without launching a duplicate
Buildah pod. The wrapper gives concurrent clients distinct child request IDs
and restores each client's original ID on its response. Child notifications
are broadcast to currently connected clients. A child crash clears this
in-process state and the next request starts a fresh child; successful build
entries otherwise remain retained for the lifetime of the pod.

Goal: iterate quickly on `loom`/`loomd` without breaking agents (Claude Code, Codex, Gemini CLI, VS Code, etc.) that call `~/.local/bin/loom`.

## Principles

- Always reference a stable path in client configs: `~/.local/bin/loom`.
- Never `cp` over an in-use binary on macOS. Install via atomic rename.
- Prefer `--loom-mode` for all client configs so clients only need a single MCP entry (`loom proxy`).
- Avoid daemon restarts while agents have active tool calls.

## Local Dev Upgrade (Recommended)

Use the built-in one-liner:

```bash
make dev-upgrade
```

This does:

1. Build `bin/loom` + `bin/loomd`
2. Atomically install to `~/.local/bin/loom` and `~/.local/bin/loomd` (keeping `*.prev`)
3. `loom sync all --regen --loom-mode --loom-binary ~/.local/bin/loom`
4. Restart daemon only if it is idle (0 active connections)
5. Restart local development HUD if installed/running on port `3333` (launchd-first fallback)
6. Smoke-test `loom proxy` initialization

Options:

- `RESTART_DAEMON=never make dev-upgrade`
- `RESTART_DAEMON=always make dev-upgrade`
- `INSTALL_DIR=/custom/bin make dev-upgrade`

## Rollback

Atomic install writes a single previous copy:

- `~/.local/bin/loom.prev`
- `~/.local/bin/loomd.prev`

Rollback (atomic):

```bash
scripts/install_atomic.sh ~/.local/bin/loom.prev ~/.local/bin/loom --no-backup
scripts/install_atomic.sh ~/.local/bin/loomd.prev ~/.local/bin/loomd --no-backup
```

Then restart daemon when idle:

```bash
loom restart
```

## Dev vs Stable Release

- Dev builds: frequent local upgrades via `make dev-upgrade` (no tags required).
- Stable builds: tag-based releases (`git tag vX.Y.Z`) and CI artifacts (see `.gitlab-ci.yml`).

## Compatibility Contract (Do Not Break)

To keep agents stable across fast iterations:

- `loom proxy` must remain backwards compatible with older generated configs (flags, IO).
- CLI must not require interactive prompts for common agent workflows.
- Daemon socket path should remain stable by default: `~/.config/loom/loom.sock`.
