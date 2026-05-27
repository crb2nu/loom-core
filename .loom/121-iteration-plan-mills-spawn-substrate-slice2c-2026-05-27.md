# RALPH Iteration Plan — Mills HUDSpawnClient + spawn pod env propagation (Slice 2c)

- **Date**: 2026-05-27
- **Lineage**: `.loom/45-product-spec-mills-harvester-vm-substrate-2026-05-25.md` (Slice 2). Slice 2a (policy field) shipped 2026-05-26 as `2a9c6c3a`; Slice 2b (SpawnWorker → SpawnRequest.Substrate) shipped 2026-05-27 as `a6979762` ([!564](https://gitlab.flexinfer.ai/services/loom-core/-/merge_requests/564)). Slice 2c carries the value over the wire and onto the spawn pod's env so the substrate becomes observable end-to-end. Backend switching at pod-creation time is **explicitly deferred** to a later slice (2d) — that change has to refactor the HUD spawn orchestrator's single-backend assumption and is too large to bundle with this carrier slice.

## Review

- Roadmap milestone: `.loom/45-…`, Slice 2 "Mills opts in for `mills-canary-*` items"
- Spec sections:
  - "Mills integration" / "Implementation plan → Slice 2 → Slice 2c" — the spec literally describes this hop ("`HUDSpawnClient.Run` forwards `req.Substrate` to spawn-pod env as `DEVBOX_BACKEND`")
  - Deferred per spec: "multi-backend initialization in `cmd/mcp-devbox` + a backend lookup in `internal/hud/spawn.go`" — that's Slice 2d's surface area.
- Prior decisions to preserve:
  - `pipeline.SpawnRequest.Substrate` is a top-level field (Slice 2b's commit), not an `Env` map entry — keep that convention on the wire too.
  - `internal/spawn.Request.Metadata` is reserved for telemetry tags (LOOM_MILLS_*, weaver_query_id) and is NOT auto-propagated to pod env. We must explicitly add the env-var.
  - `internal/hud/spawn.go` builds env at `runSpawn` line ~525 with a small literal map. Add the entry there.
  - The HUD mobile spawn handler at `internal/hud/domain/mobile/handler_spawn.go:205` JSON-decodes `r.Body` straight into `internal/spawn.Request`. Adding a JSON-tagged field to that struct is sufficient — no handler edit needed.

## Riskiest assumption + kill-test

**Load-bearing assumption**: The spawn pod's `DEVBOX_BACKEND` env-var is the only signal `mcp-devbox` (running inside the pod) needs to route subsequent `devbox_exec` calls onto the substrate this stage selected. The pod itself remains on the HUD orchestrator's K8s backend in this slice — but any in-pod `devbox_*` MCP call gates on this env-var.

**Kill test**: After this slice ships, spawn one mills `mills-canary-*` item with `pipeline.stage_substrate.implement: harvester-vm`, then `kubectl exec` into the resulting spawn pod and `env | grep DEVBOX_BACKEND`. Expected: `DEVBOX_BACKEND=harvester-vm`. Run on the live operator after auto-merge; ~3 min.

**Failure mode if wrong**: Slice 2d's pod-substrate switch becomes load-bearing on a different signal (e.g. labels, annotations, a dedicated K8s CRD). We'd ship Slice 2c shipping an unused env-var.

**Status**: not run.

## Align

- Slice name: **HUDSpawnClient sends `substrate`; HUD spawn server promotes it to `DEVBOX_BACKEND` on the pod env**
- Scope in:
  - Add `Substrate string \`json:"substrate,omitempty"\`` field to `internal/spawn.Request` (mirrors the pipeline.SpawnRequest field; JSON tag is `substrate` to match the policy YAML vocabulary).
  - Add `Substrate string \`json:"substrate,omitempty"\`` to `pkg/mills/clients.hudSpawnRequestBody`; populate it from `req.Substrate` in `HUDSpawnClient.Run` (single-line edit next to the existing `Metadata: buildSpawnMetadata(req)`).
  - In `internal/hud/spawn.go runSpawn`, when `req.Substrate != ""`, set `env["DEVBOX_BACKEND"] = req.Substrate` in the `env` map literal so it lands on the pod via `backend.StartOpts.Env`.
  - Extract that env-building into a small unbound helper (`buildSpawnPodEnv(req spawn.Request, agentID, spawnID string) map[string]string`) so it's unit-testable without spinning up the full orchestrator.
  - Tests:
    1. `pkg/mills/clients/spawn_test.go`: extend `TestRun_PostsCorrectRequestAndAuth` (or new `TestRun_PropagatesSubstrate`) to set `req.Substrate = "harvester-vm"` and assert the decoded POST body contains `"substrate":"harvester-vm"`. Also add a "default empty substrate" case asserting the field is omitted.
    2. `internal/hud/spawn_test.go`: new `TestBuildSpawnPodEnv_Substrate` covering empty / "k8s" / "harvester-vm" inputs against the extracted helper.
- Scope out (Slice 2d candidates):
  - Multi-backend init in `internal/hud/embed.go initSpawnOrchestrator` — `SpawnOrchestrator.backend` stays single.
  - Backend lookup in `runSpawn` so the pod itself is created on harvester-vm.
  - Multi-backend init in `cmd/mcp-devbox/main.go` — `cmd/mcp-devbox` still reads a single `DEVBOX_BACKEND` env at startup.
  - End-to-end auto-merge of a canary item via harvester-vm — that's Slice 2's full acceptance criterion (we're carrying the signal, not flipping the substrate).
- Acceptance criteria:
  1. `HUDSpawnClient.Run` with `pipeline.SpawnRequest{Substrate: "harvester-vm"}` sends a JSON body whose `substrate` field is `"harvester-vm"`.
  2. Empty `req.Substrate` produces a JSON body with NO `substrate` field (omitempty).
  3. JSON-decoding `{"substrate":"harvester-vm",…}` into `internal/spawn.Request` yields `Request.Substrate == "harvester-vm"`.
  4. `buildSpawnPodEnv(spawn.Request{Substrate: "harvester-vm"}, …)` returns a map with `DEVBOX_BACKEND=harvester-vm`. Empty substrate produces a map without that key.
  5. All existing `go test ./...` stay green, `go vet ./...` clean, `golangci-lint` clean on the touched packages.
- Dependencies/blockers: none.

## Land

- Planned file areas:
  - `internal/spawn/types.go` (one struct field)
  - `pkg/mills/clients/spawn.go` (struct field + one-line populate in `Run`)
  - `internal/hud/spawn.go` (extract `buildSpawnPodEnv`, call it in `runSpawn`)
  - `pkg/mills/clients/spawn_test.go` (POST-body substrate assertion)
  - `internal/hud/spawn_test.go` (helper unit test)
  - `.loom/121-iteration-plan-…` (this doc)
- Implementation steps:
  1. Worktree allocate `feat/mills-hud-spawn-substrate-env` from `main`.
  2. Edit `internal/spawn/types.go`: add `Substrate string \`json:"substrate,omitempty"\`` near the other routing fields.
  3. Edit `pkg/mills/clients/spawn.go`: add `Substrate string \`json:"substrate,omitempty"\`` to `hudSpawnRequestBody`; populate it in `HUDSpawnClient.Run` (next to `Metadata: buildSpawnMetadata(req)`).
  4. Edit `internal/hud/spawn.go`: extract `buildSpawnPodEnv` (pure func), call it from `runSpawn`, branch on `req.Substrate`.
  5. Add tests: substrate POST-body test in `spawn_test.go`; helper unit test in `internal/hud/spawn_test.go`.
  6. Build + targeted test inside devbox.
  7. Commit, push, MR, auto-merge.

## Prove

- Tests to run:
  - `go test ./pkg/mills/clients/... -run Substrate -v`
  - `go test ./internal/hud/... -run BuildSpawnPodEnv -v`
  - `go test ./pkg/mills/... ./internal/hud/... ./internal/spawn/...`
  - `go build ./...`
- Lint/static checks: `go vet ./...`, `golangci-lint run ./pkg/mills/clients/... ./internal/hud/... ./internal/spawn/...`
- CI checks: GitLab pipeline reaches green (test + lint + security stages).

## Handoff/Harvest

- Docs to update on land:
  - `.loom/00-index.md` — update the Mills VM substrate one-liner to "Slice 2c shipped YYYY-MM-DD as `<sha>`; next: Slice 2d (HUD multi-backend init + per-spawn backend lookup)".
  - `.loom/45-…` Slice 2c status line: append "Slice 2c shipped YYYY-MM-DD as `<sha>` ([!MR](https://gitlab.flexinfer.ai/services/loom-core/-/merge_requests/MR))".
- Agent-context entries (post-merge):
  - decision: "Substrate travels as a typed JSON field (`internal/spawn.Request.Substrate`) on the spawn POST, not via `Request.Metadata`. Rationale: Metadata is reserved for telemetry tags that don't auto-flow to pod env; a typed field gives Slice 2d a clean switch surface for backend lookup."
  - finding: "`internal/hud/spawn.go runSpawn` builds pod env inline at line ~525; extracting `buildSpawnPodEnv` makes future env additions (e.g. weaver query id) unit-testable without exercising the K8s backend."
- Next-slice candidates (after Slice 2c lands):
  - **Slice 2d**: multi-backend init in `internal/hud/embed.go initSpawnOrchestrator` (construct optional `HarvesterVMBackend` alongside the existing `K8sBackend`); refactor `SpawnOrchestrator.backend` → `backends map[string]backend.Backend`; in `runSpawn` look up `req.Substrate` to pick the backend (default: k8s).
  - **Slice 2e**: multi-backend init in `cmd/mcp-devbox/main.go` so an in-pod `devbox_*` MCP call can use the env-supplied backend (this is the consumer of Slice 2c's env-var when the pod's own mcp-devbox starts up).
  - **Slice 2 acceptance**: canary-label opt-in gating in `GitLabWorker` so only `mills-canary-*` items receive `pipeline.stage_substrate.implement: harvester-vm`; then run the kill-test for one end-to-end auto-merge.

## Sources

- Spec: `.loom/45-product-spec-mills-harvester-vm-substrate-2026-05-25.md` § "Slice 2c (next)"
- Slice 2b plan + outcome: `.loom/120-iteration-plan-mills-spawn-substrate-slice2b-2026-05-27.md`, commit `a6979762`
- `pipeline.SpawnRequest.Substrate` (Slice 2b): `pkg/mills/pipeline/dispatcher.go:160-168`
- `HUDSpawnClient.Run` request-body construction: `pkg/mills/clients/spawn.go:185-211`
- HUD spawn env build site: `internal/hud/spawn.go:525-539`
- HUD mobile spawn handler JSON decode: `internal/hud/domain/mobile/handler_spawn.go:205-211`
- `internal/spawn.Request` definition: `internal/spawn/types.go:77-124`
