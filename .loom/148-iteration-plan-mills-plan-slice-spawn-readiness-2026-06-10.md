# RALPH Iteration Plan - Mills plan_slice Spawn Readiness

## Review

- Roadmap milestone: Mills harvester-vm substrate, Slice 2 acceptance unblocker.
- Spec section(s):
  - `.loom/45-product-spec-mills-harvester-vm-substrate-2026-05-25.md` Slice 2 acceptance.
  - `.loom/126-plan-mills-full-vision-roadmap-2026-06-01.md` Phase A / Slice A2.
  - `.loom/146-iteration-plan-mcp-devbox-multibackend-slice2e-2026-06-10.md` Slice 2e handoff.
  - Sibling-worktree evidence: `.loom/147-iteration-plan-mills-harvester-acceptance-killtest-2026-06-10.md`.
- Prior decisions to preserve:
  - Do not start another Harvester acceptance window until `plan_slice` can reliably leave spawn runtime build readiness.
  - The failed canary `PIPE-MILLS-CANARY-HARVESTER-A2-20260610-215626-1781128618` did not reach Harvester-routed `implement` or `tests`.
  - The visible blocker was a K8s devbox Buildah pod remaining in init/build readiness after git-clone output completed and the runtime image was already present.

## Align

- Slice name: plan_slice spawn/runtime readiness unblocker.
- Scope in:
  - Harden K8s pod wait helpers so already-terminal pods are observed before opening a watch.
  - Surface init-container termination while the pod is still pending instead of waiting for the outer timeout.
  - Remove the misleading `git safe.directory` fatal from the git-clone readiness log by capturing the branch before chowning the repository to uid 1000.
  - Add targeted regressions for already-running, already-succeeded, and failed-init pod states.
- Scope out:
  - Harvester acceptance rerun.
  - Production policy flips.
  - Per-item substrate routing.
  - Curated Harvester base image and warm pool work.
- Acceptance criteria:
  - A cached Buildah runtime pod that reaches `Succeeded` before the watch attaches returns success immediately.
  - A runtime pod already `Running` before the watch attaches returns success immediately.
  - A failed git-clone init container returns an actionable error containing container name, exit code, and message.
  - The git-clone init script no longer runs `git rev-parse` after chowning the repository away from root.
- Dependencies/blockers:
  - Detached Codex worktree requires `GOWORK=off` because `go.work` references workspace-relative sibling libs that are not present under `.codex/worktrees`.
  - `internal/hud` tests require `CGO_ENABLED=0` in this environment because the module-cache copy of `fi-accel` lacks `fi_accel.h`.

## Land

- Planned file areas:
  - `internal/devbox/backend/k8s_wait.go`
  - `internal/devbox/backend/k8s_objects.go`
  - `internal/devbox/backend/k8s_wait_test.go`
  - `internal/devbox/backend/k8s_test.go`
- Implementation steps:
  1. Add current-state checks before pod watches in `waitForPodRunning` and `waitForPodDone`.
  2. Centralize early pod error detection across init and main container statuses.
  3. Capture the git-clone branch before `chown -R 1000:1000`.
  4. Prove with focused backend and HUD spawn tests.

## Prove

- Tests run:
  - `GOWORK=off go test ./internal/devbox/backend`
  - `GOWORK=off CGO_ENABLED=0 go test ./internal/hud -run 'Test.*Spawn|Test.*spawn|TestBuildSpawnPodEnv|TestAgentSecret|TestStartSpawnPod'`
- Lint/static checks:
  - `gofmt` on changed Go files.
- CI checks:
  - Pending after commit/MR.

## Handoff/Harvest

- Docs updated:
  - This iteration note records the blocker fix and local proof.
- Agent-context entries to add:
  - Not available in this Codex session; tool discovery returned no agent-context tools.
- Next-slice candidates:
  - Rebuild/deploy the operator/HUD image containing this fix.
  - Rerun exactly one Harvester acceptance canary with `implement` and `tests` routed to `harvester-vm`.
  - If the canary reaches `implement`, collect VM/VMI/PVC evidence and continue the existing Slice 2 acceptance proof.
