# Mills Intake — product spec (2026-09-04)

**Goal.** From the HUD, an operator can (1) see which repos Mills can weave in
and exactly why any repo cannot, (2) onboard an existing GitLab or GitHub repo
into Mills, and (3) create a new project the workspace way — GitLab primary
with a GitHub mirror — and have it onboarded in the same motion. Provider
identity comes from the operator's own `glab` / `gh` logins where the HUD
daemon runs, or from env tokens in-cluster.

**Riskiest assumption + kill-test.** That runtime registration is enough for
Mills to weave in a repo without a policy edit. Kill-test: register a repo via
`POST /api/mills/projects/onboard`, then `GET /api/mills/projects` must report
`ready: true` with `protected_paths: global` (not `unknown`) while
`cross_repo.enabled` + `allow_bootstrapped` are on. This was false before this
slice (unknown targets fell back to match-all `**`), and is fixed by
`mills.SetRuntimeKnownProjects`.

## Surfaces

| Surface | Where | What |
|---|---|---|
| Projects panel | Work → Projects | Registry of every project Mills knows (home, demand list, intake list, runtime registry) merged with the plan/task/session rollup. Each card carries a **Mills readiness** chip: `weaving` / `registered` / `not onboarded` / `blocked`. Header actions: **⊕ Onboard repo**, **＋ New project**. Detail aside shows the readiness checklist with blockers named and an **Open policy MR** action. |
| Provider bar | Projects panel header | GitLab and GitHub identity: who, from where (`env` / `glab` / `gh`), with the connect command when missing. Sits beside the HUD admin-token bar so a 401 is never a dead end. |
| Repo picker | dialog | Provider tabs, search, last-activity order, visibility + default branch, per-row Mills status. Select → Onboard. |
| Onboard dialog | dialog | GitLab repo: register now (runtime) + optional gitops policy MR (demand list, issue intake, protected paths, budget caps, checksum bump). GitHub repo: import into GitLab under a chosen group, keep GitHub as the push mirror, then the same onboarding. |
| New project dialog | dialog | Name, group (from GitLab groups the token can create in), visibility defaulted by bucket rule (`libs/*` public, else private), description, template (`generic` / `go`), **Mirror to GitHub** (default on), **Onboard into Mills** (default on). Dry-run preview before commit. |

## Honesty rules

- A readiness chip is derived from the operator's live answer, never from
  what the HUD just did. After onboarding, the panel re-fetches.
- Snapshot-at-start policy keys (`cross_repo.demand_projects`,
  `intake.gitlab.projects`) are labelled *takes effect after the operator
  restarts* in the checklist; the runtime registry path is labelled *live now*.
- Provider mutations require the HUD admin token AND a provider credential;
  every mutation supports `dry_run` and returns a per-step ledger.
- Nothing is deleted or overwritten: an existing GitLab project, GitHub repo,
  or push mirror is reported as `exists`, not recreated.

## API

Operator (`loom-mills-operator`):
- `GET /api/mills/projects` — registry + readiness.
- `POST /api/mills/projects/onboard` — runtime registration (admin).
- `POST /api/mills/projects/policy-mr` — anchored gitops edit + MR (admin).

HUD daemon (`internal/hud/domain/forge`):
- `GET /api/forge/auth`, `GET /api/forge/groups`, `GET /api/forge/repos`
- `POST /api/forge/projects` (create + seed + mirror), `POST /api/forge/projects/import` (GitHub → GitLab + mirror) — admin, `dry_run` supported.

## Out of scope (named)

- Interactive `glab auth login` / `gh auth login` from the browser (device
  flows need a terminal); the bar shows the exact command instead.
- A native Anthropic council reviewer; branding assets (`banner-kit`) in the
  seed; a `loom project` CLI. Each is a follow-up, not a hidden gap.
