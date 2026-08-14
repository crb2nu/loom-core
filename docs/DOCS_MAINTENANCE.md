# Documentation Ownership and Update Cadence

This document defines who updates Loom Core docs and when updates are required.

## Ownership Model

- Feature owner: updates user/developer docs for behavior or workflow changes in the same PR.
- Area maintainer: reviews docs for correctness in their subsystem (`daemon`, `hud`, `devbox`, `mcp-*`).
- Release owner: folds changelog fragments (`make changelog-fold VERSION=...`) and confirms `README.md`, `docs/IMPLEMENTATION_STATUS.md`, and `CHANGELOG.md` are aligned before cutting a release.

## Required Update Triggers

Update documentation when any of these change:

- CLI command names/flags/examples (`loom ...`).
- MCP tool names/args/response semantics.
- Daemon config schema keys or defaults.
- Security/auth flows (token, OIDC, mTLS, OAuth 2.1).
- User-facing operational workflows (bootstrap, sync, upgrade/reload, HUD).

## Minimum Files to Evaluate Per User-Visible Change

1. A changelog fragment (`changelog.d/<slug>.<category>.md`) — see below
2. `docs/IMPLEMENTATION_STATUS.md`
3. At least one of:
   - `README.md`
   - `docs/USER_GUIDE.md`
   - `docs/DEVELOPER_GUIDE.md`
   - feature-specific deep docs under `docs/`

## Changelog Fragments (`changelog.d/`)

Changelog entries are authored as **per-MR fragment files**, not by editing the
shared `## [Unreleased]` section of `CHANGELOG.md` directly. Direct edits collide
across concurrent MRs (GitLab flags a server-side conflict and drops auto-merge;
the union merge driver only exists client-side), which was a recurring source of
rework. A fragment is one isolated file, so parallel MRs never conflict.

- **Author**: add `changelog.d/<slug>.<category>.md` in the same branch as the
  code change. `<category>` is one of `added|changed|deprecated|removed|fixed|
  security` (Keep a Changelog); `<slug>` is unique per MR (branch name or
  `date-topic`); the file body is the markdown bullet exactly as it should
  appear. Full convention: [`changelog.d/README.md`](../changelog.d/README.md).
- **A fragment satisfies the docs guardrail.** `scripts/ci/check_docs_guardrails.sh`
  and the Mills `docs_guardrail` gate both accept a `changelog.d/*.md` fragment
  as the documentation update a code-facing change requires.
- **Validate**: `make changelog-check` (CI runs it in `guardrails:docs-cli`).
- **Assembler**: `scripts/changelog/` (`go run ./scripts/changelog`).

### Folding at release time

Fragments are folded into `CHANGELOG.md` when a release is cut — **not** on every
merge to `main` (a per-merge bot commit would race merge-when-pipeline-succeeds
and burn pipelines). The release owner runs:

```bash
# Fold fragments into [Unreleased] AND cut the version section in one step:
make changelog-fold VERSION=v0.10.0            # DATE= defaults to today (UTC)
# or the raw assembler:
go run ./scripts/changelog --fold --version v0.10.0 --date 2026-07-17
```

`--fold` inserts each fragment under its matching `### Category` heading in
`[Unreleased]` (creating headings in canonical order as needed, folding into the
first heading when duplicates exist), then deletes the folded fragment files.
`--version` additionally renames `## [Unreleased]` to the version and adds a
fresh empty `## [Unreleased]` above it. Commit the folded `CHANGELOG.md` and the
fragment deletions together as part of the release commit. Omit `VERSION=` to
fold into `[Unreleased]` without cutting a version.

## Cadence

- Per PR: add a changelog fragment and update docs in the same branch as the
  code change.
- Weekly: review `docs/IMPLEMENTATION_STATUS.md` against `ROADMAP.md` and active planning notes.
- Per release cut: fold changelog fragments (`make changelog-fold VERSION=...`),
  then run docs/CLI guardrails and resolve drift before tagging.

## Verification Checklist

```bash
scripts/ci/check_docs_guardrails.sh
go run ./scripts/changelog --check
go run ./cmd/loom --help
go run ./cmd/loom agent --help
```

If command behavior changed, also verify related help pages (for example `loom auth --help`, `loom proxy --help`).

## Source of Truth Hierarchy

1. Runtime behavior in code (`cmd/`, `internal/`, `pkg/`)
2. `CHANGELOG.md` for release-facing deltas
3. `docs/IMPLEMENTATION_STATUS.md` for shipped vs in-progress state
4. Top-level and deep docs (`README.md`, `docs/*.md`)
