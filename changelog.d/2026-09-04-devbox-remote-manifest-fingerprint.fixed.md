Stop rebuilding the git-clone sandbox image on every Mills run. In `git-clone`
sync mode the hub has no checkout to fingerprint, so every sandbox was the
"generic" image keyed by the empty-input hash (`e3b0c44`), and because its base
image could only be chosen inside the build pod the registry was never trusted
as a cache: each fresh `mcp-devbox` process (one per hub websocket session)
rebuilt and re-pushed the identical 450MB image, 24–28 minutes under load. The
quality gate waits 8 minutes for a build, so a run burned two or three "sandbox
image still building" attempts before it could test anything — 137 of the 447
tests-stage attempts in the week to 2026-09-04 (22.7 hours of wall-clock), and
several runs hit the transient retry cap and escalated as infrastructure.

`mcp-devbox` now fetches the project's dependency manifests (`go.mod`,
`package.json`, `pyproject.toml`, lockfiles, `.devbox.yaml`) from the git host
through the GitLab files API with the sandbox git token, and fingerprints that
copy: the image tag is a content hash, the Dockerfile is the language template
on the registered base image, and the existing registry-hit short-circuits
apply — the image is rebuilt only when a manifest changes. Manifests are cached
under `DEVBOX_CACHE_DIR/remote-manifests` for `DEVBOX_REMOTE_MANIFEST_TTL`
(default 10m); a failed refresh serves the cached copy, and a repo with no
reachable manifests degrades to the previous generic image. The token comes
from `DEVBOX_GIT_TOKEN` or the `token` key of `DEVBOX_K8S_GIT_SECRET` (the hub
service account needs `get` on that secret). `DEVBOX_REMOTE_MANIFESTS=0`
disables hydration.
