# Renovate And Image-Build Gate Guardrails

Repo-local guardrails for handling Renovate merge requests and image-build
gate failures without changing external systems. Use this document when a
Renovate branch, `build:image:*` job, or Mills `ci_watch` escalation needs a
deterministic operator decision.

This document complements `docs/ci-triage-runbook.md` and
`docs/ci-incident-triage.md`. Those runbooks classify repeated CI incidents;
this one defines the required branch checks, retry rules, and closeout notes
for the two CI families that most often look external while still needing
repo-local evidence.

## Operating Rule

Do not retry, rebase, merge, or regenerate a Renovate or image-build branch
until the first failure has been classified as one of:

- `repo-fix`: the branch diff caused the failure and needs a repository change.
- `ci-config-fix`: loom-core CI configuration or guardrail logic caused the
  failure.
- `external-dependency-incident`: GitLab, registry, package proxy, checksum,
  DNS, TLS, runner, or Kubernetes infrastructure caused the failure.
- `flake-retried`: one bounded retry passed and no matching recurrence exists.
- `operator-escalated`: Mills or Renovate automation needs human disposition
  before another attempt.

The first failed job is authoritative. Later skipped, blocked, or downstream
jobs are follow-on signals unless they contain the first meaningful error.

## Required Preflight

Run these checks before changing pipeline state:

```bash
git status --short --branch
git diff --name-only origin/main...HEAD
git log --oneline origin/main..HEAD
```

For Mills-generated work, resolve the live plan from the shared store and
compare the slice files with the branch diff:

```text
agent_plan_get{plan_id:"<plan-id>"}
```

For local repository checks, use the narrowest guardrail that matches the
branch first:

```bash
bash scripts/ci/check_docs_guardrails.sh
bash scripts/ci/check_docs_guardrails_test.sh
make ci-contracts
go test ./...
```

If the branch has no commits, the MR is empty, the plan slice does not match
the diff, or required docs are missing for code-facing changes, stop and fix
branch hygiene before retrying CI.

## Renovate Gate Handling

Renovate branches are repository-owned until evidence proves the package
infrastructure failed. A dependency update that changes compile behavior,
generated metadata, lockfiles, module graph resolution, or tests is a
repo-fix even when Renovate created the branch.

Classify a Renovate failure with this decision table:

| Signal | Classification | Operator action |
| --- | --- | --- |
| Compile, lint, contract, docs guardrail, or deterministic test failure points at the updated dependency or changed lockfile | `repo-fix` | Patch the branch, pin or adjust the update, add tests when behavior changed, then rerun the failed job |
| `go mod`, package download, checksum, tarball, or package metadata returns repeatable 404 for the target version only | `repo-fix` or dependency policy issue | Confirm the version exists upstream; if invalid, close/reconfigure the update rather than retrying |
| Registry, package proxy, checksum DB, DNS, TLS, or GitLab API returns 429/5xx or connection failures across unrelated Renovate branches | `external-dependency-incident` | Record the shared incident and retry only after recovery or bounded backoff |
| Renovate cannot update the branch because GitLab rejects pushes, API calls, or MR metadata updates | `external-dependency-incident` if shared, otherwise `operator-escalated` | Capture GitLab response and stop automation until auth/rate-limit state is known |
| One timeout or connection reset passes on a single clean retry and does not recur elsewhere | `flake-retried` | Record the retry job ID and close the loop |

Required Renovate evidence:

- Package name, manager, current version, target version, and Renovate branch.
- Changed lockfile or module metadata paths.
- First failing job, job ID, and first meaningful error line.
- Registry, checksum, or package-proxy response when downloads fail.
- Whether the same signature appears on unrelated Renovate branches or `main`.
- Retry job ID and result, if a single retry was allowed.

Do not close a Renovate failure as external solely because Renovate authored
the MR. Do not keep retrying a dependency update that deterministically breaks
repository tests; either adapt the repo, constrain the update, or close the MR
with the evidence note.

## Image-Build Gate Handling

Image-build gates publish deployable images for `loom-core`, `custom-server`,
and `loom-mills-operator`. The `loom-core` and `custom-server` jobs are gated
by changed image inputs so unrelated default-branch merges do not roll shared
MCP server fleets.

Current image jobs:

| Job | Dockerfile | Build command | Input gate |
| --- | --- | --- | --- |
| `build:image:loom-core` | `Dockerfile` | `scripts/ci/buildkit-build.sh mcp/loom-core Dockerfile loom-core` | `.gitlab-ci.yml`, `.dockerignore`, `Dockerfile`, build script, `go.mod`, `go.sum`, `cmd/**`, `internal/**`, `pkg/**` |
| `build:image:custom-server` | `Dockerfile.custom-server` | `scripts/ci/buildkit-build.sh "$CUSTOM_SERVER_IMAGE_REPO" Dockerfile.custom-server custom-server` | `.gitlab-ci.yml`, `.dockerignore`, `Dockerfile.custom-server`, build script, `go.mod`, `go.sum`, `cmd/**`, `internal/**`, `pkg/**` |
| `build:image:loom-mills-operator` | `Dockerfile.loom-mills-operator` | `scripts/ci/buildkit-build.sh mcp/loom-mills-operator Dockerfile.loom-mills-operator loom-mills-operator` | Default branch always builds; feature branches build for operator Dockerfile, build script, module files, `cmd/loom-mills-operator/**`, and `pkg/mills/**` |

Classify an image-build failure with this decision table:

| Signal | Classification | Operator action |
| --- | --- | --- |
| Dockerfile command, Go build, frontend build, asset generation, `.dockerignore`, build script, or image tag logic fails inside changed inputs | `repo-fix` or `ci-config-fix` | Patch the repository and rerun the image job |
| Job fails before checkout or before `scripts/ci/buildkit-build.sh` starts | Runner or cluster incident | Capture runner/pod evidence and escalate to the runner owner |
| Base image pull, BuildKit startup, registry login, layer push, DNS, TLS, or registry API returns shared 429/5xx/auth errors | `external-dependency-incident` | Record registry/runner evidence and retry only after recovery or bounded backoff |
| Image job is skipped on a branch that changed deployable inputs | `ci-config-fix` | Patch `.gitlab-ci.yml` rules and verify with a branch diff that exercises the gate |
| Image job runs on a branch that changed only docs or unrelated files | `ci-config-fix` | Tighten `rules:changes` to prevent unnecessary image publication and fleet rolls |

Required image-build evidence:

- Job name, image repository, tag, Dockerfile path, and commit SHA.
- Branch diff against `origin/main`, including whether gated inputs changed.
- First failing log line and the 50 lines before it.
- BuildKit runner or pod identity, Kubernetes events if available, and runner
  system-failure message when the job does not reach user scripts.
- Base-image source, registry endpoint, and push/pull response when layer
  operations fail.
- Whether the same signature appears on unrelated branches or recent `main`.

Do not manually force an image publish to work around an unclassified failure.
For shared images, an unnecessary default-branch publish can roll many MCP
server pods; fix the gate or wait for dependency recovery instead.

## Retry Rules

Allowed retries are intentionally narrow:

- Repository and CI configuration failures require a new commit before retry.
- Renovate download, registry, and image publication failures may be retried
  once only after evidence shows the failure is transient or the dependency has
  recovered.
- Runner or cluster failures may be retried after the runner owner confirms a
  healthy pool or the failing runner is replaced.
- Mills `ci_watch` retries require a non-empty pushed branch, matching live
  plan slice, and a known-good dependency state.
- A second failure with the same signature ends the retry loop and becomes an
  incident or operator escalation.

Every retry note must include the original job ID, retry job ID, classification,
and why the retry condition was satisfied.

## Closeout Note Template

Use this shape in the MR discussion, Mills escalation note, or incident record:

```text
Disposition: <repo-fix|ci-config-fix|external-dependency-incident|flake-retried|operator-escalated>
Family: <renovate|image-build>
First failing job: <job name> <job id>
First error: <one line>
Branch diff: <summary or git diff --stat>
Evidence: <registry/package/runner/GitLab/Kubernetes links or command output>
Retry: <not retried|retry job id + result>
Next action: <commit, wait for dependency recovery, close MR, or escalate>
```

For repo-owned fixes, include the commit SHA that changed loom-core. For
external dependency incidents, include the dependency owner or incident link and
avoid creating additional repo backlog unless it improves classification,
retry behavior, telemetry, configuration, or this documentation.
