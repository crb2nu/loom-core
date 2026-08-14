#!/usr/bin/env bash
# Fail CI when a job reaches a public package/image endpoint directly instead of
# the in-cluster cache that already fronts it.
#
# Every direct-internet fetch inside a job is a coin flip during a LAN or WAN
# disturbance, and it fails as `script_failure` — the same signal a real code
# bug produces. The ci namespace already runs athens (Go), verdaccio (npm),
# devpi (pip), apt-cacher-ng (deb) and Harbor's dockerhub-cache proxy project;
# this check keeps CI YAML pointed at them.
#
# Usage:
#   scripts/ci/check_ci_external_endpoints.sh [file...]
# With no arguments it scans this repo's CI YAML.
#
# Deliberate exceptions live in ci/external-endpoints-allowlist.txt, one
# `<path> <endpoint> # reason` per line. A reason is mandatory.

set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
ALLOWLIST="${CI_ENDPOINT_ALLOWLIST:-${ROOT}/ci/external-endpoints-allowlist.txt}"
DOC="docs/ci-external-endpoints.md"

# endpoint-key <TAB> extended-regex <TAB> replacement guidance
RULES=$(
  cat <<'EOF'
docker.io	docker\.io	pull through Harbor: ${PUBLIC_IMAGE_REGISTRY}/... (registry.harbor.lan/dockerhub-cache)
gcr.io	gcr\.io	no in-cluster mirror — vendor the image into Harbor, or allowlist with a reason
quay.io	quay\.io	no in-cluster mirror — vendor the image into Harbor, or allowlist with a reason
ghcr.io	ghcr\.io	pull through Harbor's ghcr-cache proxy project, or allowlist with a reason
mcr.microsoft.com	mcr\.microsoft\.com	no in-cluster mirror — vendor the image into Harbor, or allowlist with a reason
registry.gitlab.com	registry\.gitlab\.com	no in-cluster mirror — vendor the image into Harbor, or allowlist with a reason
proxy.golang.org	proxy\.golang\.org	GOPROXY=http://athens.ci.svc.cluster.local:3000 (athens may keep it as a `|` fallback)
sum.golang.org	sum\.golang\.org	athens serves the checksum DB; set GONOSUMDB=* for private modules
go.dev	go\.dev/dl	pin the toolchain via the job image instead of downloading a tarball
registry.npmjs.org	registry\.npmjs\.org	npm_config_registry=http://verdaccio.ci.svc.cluster.local:4873
pypi.org	pypi\.org	PIP_INDEX_URL=http://devpi.ci.svc.cluster.local:3141/root/pypi/+simple/
files.pythonhosted.org	files\.pythonhosted\.org	devpi proxies the file host too; do not pin it directly
deb.debian.org	deb\.debian\.org	Acquire::http::Proxy "${CI_APT_CACHE_URL}" (apt-cacher-ng)
security.debian.org	security\.debian\.org	Acquire::http::Proxy "${CI_APT_CACHE_URL}" (apt-cacher-ng)
archive.ubuntu.com	archive\.ubuntu\.com	Acquire::http::Proxy "${CI_APT_CACHE_URL}" (apt-cacher-ng)
dl-cdn.alpinelinux.org	dl-cdn\.alpinelinux\.org	no alpine mirror in cluster — prefer a prebuilt image, or allowlist with a reason
raw.githubusercontent.com	raw\.githubusercontent\.com	vendor the file into the repo or an image, or allowlist with a reason
github-release-download	github\.com/[^ "']*/releases/download	vendor the asset into Harbor or the repo, or allowlist with a reason
get.docker.com	get\.docker\.com	install from the distro mirror or a prebuilt image
sh.rustup.rs	sh\.rustup\.rs	use a rust toolchain image instead of a curl|sh installer
EOF
)

# ---------------------------------------------------------------- file set ---
FILES=()
if [ "$#" -gt 0 ]; then
  FILES=("$@")
else
  [ -f "${ROOT}/.gitlab-ci.yml" ] && FILES+=("${ROOT}/.gitlab-ci.yml")
  if [ -d "${ROOT}/ci" ]; then
    while IFS= read -r f; do FILES+=("$f"); done \
      < <(find "${ROOT}/ci" -type f \( -name '*.yml' -o -name '*.yaml' \) | sort)
  fi
  if [ -d "${ROOT}/k3s/ci" ]; then
    while IFS= read -r f; do FILES+=("$f"); done \
      < <(find "${ROOT}/k3s/ci" -type f \( -name '*.yml' -o -name '*.yaml' \) | sort)
  fi
fi

if [ "${#FILES[@]}" -eq 0 ]; then
  echo "ci-endpoints: no CI YAML found under ${ROOT}" >&2
  exit 1
fi

# --------------------------------------------------------------- allowlist ---
# Each entry is "<repo-relative-path>|<endpoint-key>".
declare -A ALLOWED=()
allowlist_errors=0
if [ -f "$ALLOWLIST" ]; then
  lineno=0
  while IFS= read -r raw || [ -n "$raw" ]; do
    lineno=$((lineno + 1))
    case "$raw" in
      ''|'#'*) continue ;;
    esac
    entry="${raw%%#*}"
    reason="${raw#*#}"
    read -r a_path a_key _rest <<<"$entry"
    if [ -z "${a_path:-}" ] || [ -z "${a_key:-}" ]; then
      echo "${ALLOWLIST}:${lineno}: malformed entry (want '<path> <endpoint> # reason')" >&2
      allowlist_errors=$((allowlist_errors + 1))
      continue
    fi
    if [ "$reason" = "$raw" ] || [ -z "$(printf '%s' "$reason" | tr -d '[:space:]')" ]; then
      echo "${ALLOWLIST}:${lineno}: allowlist entries must carry a '# reason'" >&2
      allowlist_errors=$((allowlist_errors + 1))
      continue
    fi
    ALLOWED["${a_path}|${a_key}"]=1
  done <"$ALLOWLIST"
fi

# ------------------------------------------------------------------- scan ----
# Comments are stripped before matching so prose can name an endpoint while
# explaining why it is not used. Line numbers stay those of the original file.
strip_comments() {
  awk '{
    line = $0
    sub(/^[[:space:]]*#.*$/, "", line)
    sub(/[[:space:]]#.*$/, "", line)
    printf "%d:%s\n", NR, line
  }' "$1"
}

findings=0
for file in "${FILES[@]}"; do
  [ -f "$file" ] || continue
  rel="${file#"${ROOT}"/}"
  stripped="$(strip_comments "$file")"
  while IFS=$'\t' read -r key regex guidance; do
    [ -n "$key" ] || continue
    matches="$(printf '%s\n' "$stripped" | grep -E -- "$regex" || true)"
    [ -n "$matches" ] || continue
    if [ -n "${ALLOWED["${rel}|${key}"]:-}" ]; then
      continue
    fi
    while IFS= read -r m; do
      [ -n "$m" ] || continue
      echo "${rel}:${m%%:*}: direct external endpoint '${key}'"
      echo "    -> ${guidance}"
      findings=$((findings + 1))
    done <<<"$matches"
  done <<<"$RULES"
done

# ----------------------------------------------------------------- verdict ---
if [ "$findings" -gt 0 ] || [ "$allowlist_errors" -gt 0 ]; then
  echo
  echo "ci-endpoints: ${findings} direct external endpoint(s), ${allowlist_errors} allowlist error(s)."
  echo "Route the fetch through the in-cluster cache — the job -> endpoint ->"
  echo "replacement table is in ${DOC} — or add a line to"
  echo "${ALLOWLIST#"${ROOT}"/} in the form:"
  echo "    <path> <endpoint> # why this one must stay external"
  exit 1
fi

echo "ci-endpoints: ${#FILES[@]} file(s) clean."
