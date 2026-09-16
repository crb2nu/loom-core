# Flexinfer Site Integration

This repository (`services/loom-core`) is the source of truth for Loom Core docs that are published on [https://flexinfer.ai](https://flexinfer.ai) via the `services/flexinfer-site` repository.

## Source and Publish Paths

- Source docs in this repo: `docs/`
- Site content target in `services/flexinfer-site`: `content/loom-core-docs/`
- Public route on site: `/docs/loom-core`

The sync mapping is defined in:

- `services/flexinfer-site/scripts/sync-docs.mjs`
  - `name: 'loom-core'`
  - `source: '../loom-core/docs'`
  - `target: 'content/loom-core-docs'`

## Publish Workflow

From `services/flexinfer-site`:

```bash
pnpm sync:loom-core-docs
pnpm lint
pnpm test
pnpm build
```

Then open a PR in `services/flexinfer-site` and deploy through its normal pipeline.

## Authoring Rules in loom-core

When you change user/developer-facing behavior in `services/loom-core`:

1. Update docs under `docs/` and `CHANGELOG.md` in this repo.
2. Run guardrails in this repo:
   - `bash scripts/ci/check_docs_guardrails.sh`
   - `bash scripts/ci/check_flexinfer_site_integration.sh`
3. Sync and publish in `services/flexinfer-site` using `pnpm sync:loom-core-docs`.

## CI and Pre-commit Enforcement

This repo enforces the site integration contract in:

- Pre-commit hooks (`.pre-commit-config.yaml`)
- Local CI guardrails (`make ci-guardrails`)
- GitLab CI guardrails job (`guardrails:docs-cli`)
- GitHub Actions CI guardrails step

The integration guardrail script validates:

- This runbook exists and includes required sync details.
- If a sibling `services/flexinfer-site` checkout exists, its sync mapping for `loom-core` is still correct.

## Drift measurement

The Mills operator compares Git blob IDs from the deployed loom-core build's
`docs/` tree with `content/loom-core-docs/` on the configured site ref. It
counts source files missing from the mirror, files whose blob differs, and
extra mirror files. `nav.yaml`, `roadmap-reconciliation-*.md`, and
`ROADMAP_RECONCILIATION_*.md` are ignored before counting.

The check runs hourly after boot and is exposed at
`GET /api/mills/finishing/docs-mirror`; the cached result also appears in the
shift report. Configure the target with `LOOM_MILLS_DOCS_MIRROR_PROJECT`,
`LOOM_MILLS_DOCS_MIRROR_REF`, and `LOOM_MILLS_DOCS_MIRROR_PATH`. Git or GitLab
failures produce an `unknown` result rather than failing the report.

Drift is advisory: run `pnpm sync:loom-core-docs` in `services/flexinfer-site`,
review the generated content, and publish it through that repository's normal
merge and deployment workflow.
