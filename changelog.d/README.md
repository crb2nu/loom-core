# Changelog fragments (`changelog.d/`)

Every merge request that needs a changelog entry drops a **fragment file** here
instead of editing `CHANGELOG.md` directly. CI assembles the fragments into
`CHANGELOG.md` at release time. This removes the constant merge collisions that
came from many MRs all editing the same `## [Unreleased]` section (GitLab flags
those as server-side conflicts and drops auto-merge — the union merge driver
only exists client-side).

## How to add an entry

Create one file per change:

```
changelog.d/<slug>.<category>.md
```

- **`<slug>`** — unique per MR. Use your branch name (minus the `feat/`/`fix/`
  prefix) or a `date-topic` string, e.g. `changelog-fragments`,
  `2026-07-17-auth-timeout`. The slug is what keeps two MRs from colliding, so
  it must not clash with another open MR's slug.
- **`<category>`** — one of the [Keep a Changelog](https://keepachangelog.com/en/1.0.0/)
  categories: `added`, `changed`, `deprecated`, `removed`, `fixed`, `security`.
- **file body** — the markdown entry exactly as it should appear as a
  `CHANGELOG.md` bullet. Start with `- `; multi-line bullets are fine.

`README.md` (this file) is never folded.

### Example

`changelog.d/auth-timeout.fixed.md`:

```markdown
- **Login no longer times out under load** (`internal/auth/session.go`): the
  session refresh used a fixed 2s deadline that expired during cold-cache
  fan-out. It now scales with the configured upstream timeout.
```

That becomes a bullet under `### Fixed` in `## [Unreleased]` when folded.

## Multiple entries in one MR

Add multiple files, each with a **distinct slug** (the slug must be unique even
across categories), e.g. `myfeature-api.added.md` and `myfeature-cli.changed.md`.

## Validating locally

```bash
make changelog-check          # or: go run ./scripts/changelog --check
```

CI runs the same check in the `guardrails:docs-cli` lint job. A fragment counts
as the documentation update the docs guardrail requires — a code-facing MR that
adds a `changelog.d/*.md` fragment satisfies both `scripts/ci/check_docs_guardrails.sh`
and the Mills `docs_guardrail` gate.

## Folding into `CHANGELOG.md` (release time)

Fragments are folded at **release time**, not on every merge to `main`
(per-merge bot commits would race merge-when-pipeline-succeeds and burn
pipelines). When cutting a release:

```bash
# Fold everything under changelog.d/ into CHANGELOG.md's [Unreleased] section
make changelog-fold

# …or fold AND cut a versioned section in one step:
go run ./scripts/changelog --fold --version v0.10.0 --date 2026-07-17
```

`--fold` inserts each fragment under its matching `### Category` heading (creating
headings in canonical order as needed), then deletes the folded fragment files.
`--version` additionally renames `## [Unreleased]` to the version and adds a
fresh empty `## [Unreleased]` above it. See `docs/DOCS_MAINTENANCE.md` for the
full release procedure.

The assembler lives in `scripts/changelog/` (`go run ./scripts/changelog`).
