# CI external endpoints and in-cluster caches

Every network fetch a CI job performs inline is a coin flip during a LAN or WAN
disturbance — and it fails as `script_failure`, the same signal a real code bug
produces. The `ci` namespace already runs a cache in front of each package
ecosystem CI uses; this page records which endpoint maps to which cache, and
what is deliberately still external.

Enforced by `scripts/ci/check_ci_external_endpoints.sh` (job `lint:ci-endpoints`).
Exceptions live in `ci/external-endpoints-allowlist.txt` and must carry a reason.

## In-cluster caches

| Ecosystem | Service | Endpoint | Set via |
|---|---|---|---|
| Go modules | athens | `http://athens.ci.svc.cluster.local:3000` | `GOPROXY` |
| npm | verdaccio | `http://verdaccio.ci.svc.cluster.local:4873` | `npm_config_registry` / `NPM_CONFIG_REGISTRY` |
| pip | devpi | `http://devpi.ci.svc.cluster.local:3141/root/pypi/+simple/` | `PIP_INDEX_URL` |
| deb | apt-cacher-ng | `http://apt-cache.ci.svc.cluster.local:3142` | `CI_APT_CACHE_URL` → `Acquire::http::Proxy` |
| dnf | dnf-cache | `http://dnf-cache.ci.svc.cluster.local:8080` | `CI_DNF_CACHE_URL` |
| Docker Hub images | Harbor proxy-cache project | `registry.harbor.lan/dockerhub-cache` | `PUBLIC_IMAGE_REGISTRY`, `PUBLIC_BASE_REGISTRY` |

The variables (except the two registry ones) arrive from
`platform/gitops:/k3s/ci/caches/gitlab-ci-cache.yml`, which this repo's
`.gitlab-ci.yml` includes, and from the runner chart's `environment` block.

## Audit — 2026-08-02 CI-hardening sweep

| Job / location | External endpoint | Replacement |
|---|---|---|
| global `variables:` | `GOPROXY=https://proxy.golang.org\|direct` | `http://athens.ci.svc.cluster.local:3000\|https://proxy.golang.org\|direct` — the comment already claimed Athens-first; the value did not |
| `.go-template` (all Go jobs) | `image: docker.io/library/golang:$GO_VERSION` | `${PUBLIC_IMAGE_REGISTRY}/library/golang:$GO_VERSION` |
| `build:frontend`, `lint:mcp-godot` | `image: docker.io/library/node:20` | `${PUBLIC_IMAGE_REGISTRY}/library/node:20` |
| `deploy:homebrew` | `image: docker.io/library/alpine:3.24` | `${PUBLIC_IMAGE_REGISTRY}/library/alpine:3.24` |
| `build:image:*` | `BUILDKIT_CLI_IMAGE=docker.io/moby/buildkit:v0.12.5` | `${PUBLIC_IMAGE_REGISTRY}/moby/buildkit:v0.12.5` |
| `Dockerfile`, `Dockerfile.custom-server`, `Dockerfile.loom-mills-operator` | `ARG PUBLIC_BASE_REGISTRY=docker.io` — never overridden, so buildkit pulled `golang:1.26.5-alpine`, `node:20-alpine`, `alpine:3.24` from Docker Hub | `scripts/ci/buildkit-build.sh` now passes `build-arg:PUBLIC_BASE_REGISTRY`, set to the Harbor mirror in CI |
| `.go-template` `apt-get` fallback | `deb.debian.org` (only when the image lacks git/curl) | `Acquire::http::Proxy "$CI_APT_CACHE_URL"` written before `apt-get update` |
| `deploy:homebrew` | `apk add git curl` → `dl-cdn.alpinelinux.org` | removed — the job is an `echo` placeholder and called neither tool |
| `release` | `image: registry.gitlab.com/gitlab-org/release-cli:latest` | **allowlisted** — no Harbor mirror for `registry.gitlab.com`; tag-only job, never on the merge path |
| global `variables:` | `proxy.golang.org` as a GOPROXY fallback | **allowlisted** — reached only when Athens errors |
| `build:frontend` (`npm install -g pnpm@10`), `lint:mcp-godot` (`npm ci`) | — | already on verdaccio via `npm_config_registry` from the runner env; no change |
| `go install …@version` (golangci-lint, gosec, govulncheck, gocover-cobertura) | — | follows `GOPROXY`, so now athens-first; no change |

All Harbor-mirrored tags above were verified to resolve through
`registry.harbor.lan/dockerhub-cache` before the cutover.

## Known gaps

- **Bare image references** (`image: someorg/someimage:tag`, no host) resolve to
  Docker Hub but contain no literal `docker.io`, so the lint cannot see them.
  Write the registry explicitly.
- **`registry.gitlab.com`, `gcr.io`, `quay.io`, `mcr.microsoft.com`** have no
  Harbor proxy-cache project. Only `dockerhub-cache` exists today. Adding one is
  a `platform/gitops` change (`scripts/harbor/setup-proxy-cache.sh`).
- The lint scans **CI YAML only**, not shell scripts or Dockerfiles. Dockerfile
  base images are covered by the `PUBLIC_BASE_REGISTRY` ARG instead.

## Adding a new image tag

Harbor's proxy cache returns 404 in runner *prepare* for a tag that does not
exist upstream — before any CI script can run, so the failure is opaque. When
bumping `GO_VERSION` or any other pinned tag, confirm it resolves first:

```bash
docker --context 7900xtx manifest inspect --insecure registry.harbor.lan/dockerhub-cache/library/golang:<new-tag>
```

## Source-fetch hardening

Related but separate: `.clone_repo` in `.gitlab-ci.yml` fetches this repo's own
sources over the LAN (`host_aliases` pins `gitlab.flexinfer.ai` to a LAN IP), and
died on an HTTP/2 stream reset in job #212512. It now retries
HTTP/2 → HTTP/1.1 → in-cluster `gitlab-vm.gitlab.svc.cluster.local`, with
`GIT_HTTP_LOW_SPEED_LIMIT`/`TIME` stall detection, and prints
`CI-INFRA-FAILURE: source-fetch` as its last line when all three fail so triage
can separate infrastructure from code. `diag:source-fetch-probe`
(`RUN_FETCH_PROBE=true`) measures the failure rate of that path on demand.
