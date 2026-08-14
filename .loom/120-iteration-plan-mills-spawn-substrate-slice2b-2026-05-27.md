# RALPH Iteration Plan — Mills SpawnWorker substrate plumbing (Slice 2b)

- **Date**: 2026-05-27
- **Lineage**: `.loom/45-product-spec-mills-harvester-vm-substrate-2026-05-25.md` (Slice 2). Slice 2a (policy field) shipped 2026-05-26 as commit `2a9c6c3a`. Slice 2b is the next reviewable increment; Slice 2c (HUD spawn client + spawn server consuming the substrate as `DEVBOX_BACKEND`) follows.

## Review

- Roadmap milestone: `.loom/45-…`, Slice 2 "Mills opts in for `mills-canary-*` items"
- Spec sections:
  - "Mills integration" (§Per-stage substrate selection via new `policy.yaml` field)
  - "Implementation plan → Slice 2" (acceptance = 1 successful end-to-end auto-merge of a canary item via `harvester-vm`; Slice 2b is one rung below that acceptance)
- Prior decisions to preserve:
  - `pkg/mills/pipeline` already imports `pkg/mills` (no new dep direction)
  - `SpawnWorker` already follows the closure injection pattern (`PromptFor func(jc JobContext) string`) — `SubstrateFor func(stage string) string` mirrors it
  - `SpawnRequest.Env` is the canonical channel for `LOOM_MILLS_*` env-vars but substrate is a routing decision not an env-var, so it gets a top-level field

## Align

- Slice name: **Mills SpawnWorker reads `policy.SubstrateForStage` and emits `SpawnRequest.Substrate`**
- Scope in:
  - Add `Substrate string` field to `pipeline.SpawnRequest` (`pkg/mills/pipeline/dispatcher.go:139`)
  - Add `SubstrateFor func(stage string) string` field to `pipeline.SpawnWorker` (`pkg/mills/pipeline/dispatcher.go:178`)
  - In `SpawnWorker.Run`, populate `req.Substrate = w.SubstrateFor(jc.Stage.ID)` when `SubstrateFor != nil` — nil-safe fallback to `""` (empty = caller's default)
  - Wire `SubstrateFor` from `pm.Current().SubstrateForStage` at the 3 SpawnWorker construction sites in `cmd/loom-mills-operator/main.go:824/831/839`
  - Update `DefaultRoutes` test helper signature (single internal caller)
- Scope out:
  - HUD spawn client metadata propagation (Slice 2c)
  - Spawn server reading metadata to set `DEVBOX_BACKEND` (Slice 2c / HUD-side)
  - Canary-label opt-in gating (Slice 2 acceptance criterion 2)
  - Any `harvester-vm` runtime change
- Acceptance criteria:
  1. Policy with `pipeline.stage_substrate.implement: harvester-vm` causes `SpawnWorker.Run` to emit `req.Substrate = "harvester-vm"` for the `implement` stage; default policy emits `req.Substrate = "k8s"`.
  2. Nil-SubstrateFor SpawnWorker emits `req.Substrate = ""` (no regression for existing test fixtures).
  3. All existing `go test ./pkg/mills/...` and `go test ./cmd/loom-mills-operator/...` stay green.
  4. `go vet ./...` clean; `golangci-lint` clean.
- Dependencies/blockers: none. The HUD spawn server's `DEVBOX_BACKEND` interpretation is a downstream concern; passing the value in the request body is recorded auditably regardless.

## Land

- Planned file areas:
  - `pkg/mills/pipeline/dispatcher.go` (SpawnRequest + SpawnWorker + Run + DefaultRoutes)
  - `pkg/mills/pipeline/dispatcher_test.go` (new SpawnWorker substrate test + update DefaultRoutes test)
  - `cmd/loom-mills-operator/main.go` (3 SpawnWorker construction sites, +1 closure)
  - `.loom/120-iteration-plan-…` (this doc)
- Implementation steps:
  1. Worktree allocate `feat/mills-spawn-substrate-wiring` from `main`.
  2. Edit `dispatcher.go`: add field to `SpawnRequest`, field to `SpawnWorker`, populate in `Run`, extend `DefaultRoutes`.
  3. Edit `cmd/loom-mills-operator/main.go`: add `subFor := func(s string) string { return pm.Current().SubstrateForStage(s) }` near the existing policy wiring; pass to all 3 SpawnWorker literals.
  4. Edit `dispatcher_test.go`: new `TestSpawnWorker_Substrate_FromPolicy` covering nil/default/explicit cases; update `TestDefaultRoutes_WiresAllStages` signature.
  5. Build + test inside devbox (or local).
  6. Commit, push, MR, auto-merge.

## Prove

- Tests to run:
  - `go test ./pkg/mills/pipeline/... -run Substrate -v`
  - `go test ./pkg/mills/... ./cmd/loom-mills-operator/...`
  - `go build ./...`
- Lint/static checks: `go vet ./...`, `golangci-lint run ./pkg/mills/... ./cmd/loom-mills-operator/...`
- CI checks: GitLab pipeline must reach green; specifically the `test` + `lint` + `security:govulncheck` stages.

## Handoff/Harvest

- Docs to update:
  - `.loom/00-index.md` — add a one-liner under "Mills VM substrate" pointing at this plan + commit SHA
  - `.loom/45-…` Slice 2 status line: append "Slice 2b shipped YYYY-MM-DD as <commit>"
- Agent-context entries to add:
  - decision: "Substrate is propagated via a top-level `SpawnRequest.Substrate` field plus a `SpawnWorker.SubstrateFor` closure, not via `Env`. Rationale: substrate is a routing decision the spawn server must see at pod-spec build time, not at process-env time."
  - finding: "Slice 2a (policy field) was already shipped via commit `2a9c6c3a` between roadmap reconciliation and this session start — RALPH discovered it during the Read phase."
- Next-slice candidates:
  - **Slice 2c**: HUD `HUDSpawnClient.Run` forwards `req.Substrate` as `Metadata["DEVBOX_BACKEND"]`; spawn server reads it when constructing the spawn pod's env (out of this repo if HUD spawn server lives in `internal/hud/spawn/`).
  - **Slice 2 acceptance**: canary-label opt-in gating in the GitLabWorker so only `mills-canary-*` items flip to harvester-vm.
  - **Slice 1.5 closeout**: pre-baked image pipeline at `platform/gitops/harvester/mills-devbox-base/` (platform repo, not this one).
