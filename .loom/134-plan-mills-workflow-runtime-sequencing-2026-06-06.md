# Mills Workflow Runtime — S1c/S6 Sequencing Addendum

**Date**: 2026-06-06
**Status**: Design resolved. S1c gated on the **build chain** (S0 → S6-min) + **two code prerequisites** (AlreadyExists backstop, pre-merge canary scoping) — NOT on the cluster (see Correction below). See §4 and §3.

> ## ⚠️ Correction (2026-06-06, supersedes the "infra-blocked / cluster unreachable" framing below)
> **The k3s cluster is healthy and reachable.** The "cluster unreachable / hard blocker" claim in §4 B1 and the risks section was a **false conclusion from a sandboxed/cold-route probe environment**, not a real outage: the planning subagent's `kubectl` returned `EHOSTUNREACH` to `192.168.50.200:6443`, but a direct `curl -sk https://192.168.50.200:6443/version` returns a valid Kubernetes API `Status`, and `nc` shows `:6443` OPEN. The operator owner confirmed the cluster is up. The `EHOSTUNREACH` is transient/intermittent routing to the Harvester-VM-hosted API from the automation host (ARP cold-miss / flaky route), **not** a cluster-down condition.
> **Corrected gating:** S1c is **not** blocked on the cluster. It is gated on (a) building + deploying **S6-min** (the crash subject), (b) the **§5 AlreadyExists backstop** code fix, and (c) having a **stable cluster-access vantage** to drive the kill-test (the live `kubectl`/pod-kill steps must run from a context with a reliable route — an on-LAN host, the in-cluster operator path, or the loom daemon once its route is stable; flaky access from one automation host is an operational nuisance, not a project blocker). All `[live-unverified]` facts below remain to be confirmed with a `kubectl` probe from a stable vantage, but the cluster itself is live.
**Supersedes**: the open S1c/S6 ordering question in `.loom/133` (plan DAG).
**Lineage**: `130-brainstorm` → `131-research` → `132-product-spec` → `133-implementation-plan` → this addendum. Produced via an 8-agent research + design + adversarial-verify workflow; all three verdicts `sound-with-caveats`, every must-change folded in.
**Implementation status (2026-06-06)**: MERGE-LAYERS **complete** — S2 (!649), S3 (!648), S2b (!651) all merged to `main` (S2b rebased clean off main after a stacked-conflict). S1 in-process kill-test PASSED. **Next executable now: S0** (port spike into the main module) **+ the §5 AlreadyExists backstop** — both buildable on clean main without the cluster. S1c remains the deployed gate (cluster is healthy; see Correction).

> **Facts vs. assumptions.** Code/manifest claims below cite `file:line` and were verified in-repo on 2026-06-06. Claims about the *live cluster* (operator image tag, pod liveness, ConfigMap contents) are **assumptions** until a `kubectl` probe runs — the cluster is currently unreachable (§4, B1). Each such claim is marked **[live-unverified]**.

---

## 1. The chicken-and-egg, and the chosen resolution

### 1.1 The cycle, stated precisely

The plan DAG (`.loom/133`) gates the full Layer-3 runtime (S6) on a **deployed** crash kill-test (S1c): S6 must not expand until exactly-once-across-crash holds on the real substrate. But S1c needs *something deployed to crash*. The in-process S1 spike (`pkg/mills/workflow/spike/`, commit `0fe0e8d3`) proves journal replay against an in-memory map — it cannot be killed on k3s because it is a standalone Go module (its own `go.mod` requiring `go.starlark.net`) that `cmd/loom-mills-operator` cannot import. So:

- **S6 is gated on S1c** (don't build the engine until the contract holds live).
- **S1c needs a deployable runtime** (can't crash-test nothing).
- **The only deployable runtime is S6.**

That is the cycle.

### 1.2 Resolution: incremental runtime (S6-min is the deployable seed of S6-full)

Split S6 into **S6-min** (the minimum mechanism — journal + keyed-spawn + resume — behind a default-OFF flag) and **S6-full** (parallel/loop, budget envelope, scheduler, version-pin). **S1c sits between them as the gate.** S6-min is *not* a throwaway stub; it is the literal first commit of S6-full and grows into it. The cycle breaks because the thing S1c crashes is the smallest slice of the thing S1c gates — not the whole engine.

```
Layers MERGED → S0 (port) → S6-min (deploy) → [S1c gate] → S6-full → S7
```

### 1.3 Reuse-Starlark decision (decided)

**Reuse the spike's Starlark code as the seed; port it into the main module — do not vendor the standalone spike module, and do not write a Go-only stub.**

Rationale, all verified:

- **A Go stub would make S1c vacuous.** The exact machinery S1c crash-tests is read-through-journal + structured drift-tolerant step keys + record-before-effect. A stub hardcoding `spawn → done` exercises none of it and would prove nothing about the runtime S6-full ships.
- **The spike is deliberately a separate module** (header `journal.go:8-11`) to dodge `go.work` breakage from a worktree, so it is *not importable as-is*. "Reuse" therefore means **port** the pure logic (`buildUniverse`, `ScopeStack`/`Frame`/`LeafKey`, `canonicalCallHash`, clamp helpers, `QuarantineError`/`VersionDriftError`, the interpreter-version pin) into `pkg/mills/workflow/{interp,host}.go` under the main module, add `go.starlark.net v0.0.0-20260522144826-ec58d4b459e2` to the **main** `go.mod`, and **replace** the spike's in-memory `Journal` with an adapter backed by the merged `store.WorkflowDAO`.
- **The DAO already provides the exact primitives** the read-through loop needs: `GetStep` (replay), `AppendStep` (idempotent pending→terminal upsert; `ErrStepCallHashMismatch` == the spike's `QuarantineError`), `ListByRun`/`ListPending` (resume). The spike's in-memory `effectCounter` becomes a deployed side-effect assertion: a count of `workflow_steps` success rows.
- **Nothing is discarded.** S6-full *adds* WorkerRunner wiring of `agent()`, the budget envelope, parallel/loop, the scheduler, and the template registry (S7). The spike's own `go.mod` is deleted after the port (it served its S1 purpose).

**Rejected**: (a) vendoring the spike module verbatim — un-importable, and dual `go.starlark.net` pins risk drift that the version-pin tripwire would then falsely fire on; (b) a Go-only stub — vacuous gate, throwaway.

---

## 2. Updated slice sequence

> **Every runtime slice requires Layers 1/2/2b MERGED TO MAIN first** (the deployed operator must carry migration 004 + WorkflowDAO + worker contract + spawn-idempotency). Layers are implemented but unmerged today: !648 (`feat/mills-workflow-step-journal`, `3def61b8`), !649 (`feat/mills-worker-contract`, `34896293`), !651 (`wt-s2b-spawn-idempotency`).

```
!648 (S3 journal) ─┐
!649 (S2 contract) ├─ all MERGED to main
!651 (S2b idem)   ─┘   (merge !649 → !651 stacked → !648 independent)
       │
       ▼
S0  port spike → main module (no operator wiring)
       │
       ▼
S6-min  minimal deployed runtime, flag default-OFF        [dep: S0, layers]
       │      S4 (HUD step-log) runs in parallel off layers — recommended, not gating
       ▼  [INFRA GATE — §4]
S1c  DEPLOYED dual-crash kill-test                         [dep: S6-min deployed]
       │  [GATE: S1c PASS, recorded with live evidence]
       ▼
S6-full  full Layer-3 runtime                              [killTestGated, dep: S1c PASS]
       ▼
S7  council template registry + clamp                      [dep: S6-full]
```

### MERGE-LAYERS — land Layers 1/2/2b (prerequisite, not new work)
- **dependsOn**: none.
- **Deliverables**: merge !649 → !651 (stacked) → !648 (independent). Confirm `WorkflowDAO` is exported from `pkg/mills/store` (it is, public). Confirm `DeriveSpawnID` parity between `pkg/mills/worker` and `internal/spawn` (`internal/spawn/controller.go:590`).
- **filesAffected**: `pkg/mills/store/migrations/004_workflow_steps.sql`, `pkg/mills/store/dao_workflow.go`, `pkg/mills/store/types.go`, `pkg/mills/worker/worker.go`, `pkg/mills/worker/adapter.go`, `internal/spawn/controller.go`, `internal/spawn/types.go`, `pkg/mills/clients/spawn.go`.
- **Verification**: on main, `go test ./pkg/mills/store/ -run Workflow`, `go test ./pkg/mills/worker/ ./internal/spawn/`; migration 004 applies on operator startup.
- **Effort**: S (merge coordination only).

### S0 — port the spike into the main module (no operator wiring)
- **dependsOn**: MERGE-LAYERS.
- **Deliverables**: add `go.starlark.net` pseudo-version to main `go.mod` (+ `go.work` per the worktree-build gotcha); delete the spike `go.mod` after port. Port `interp.go` (6-builtin whitelist, `fileOptions{While,TopLevelControl}`, `thread.Load=nil`, up-front `checkVersion`) and `host.go` pure logic. New `journal_dao.go` adapter delegating `Get→GetStep`, `Append→AppendStep`, mapping `ErrStepCallHashMismatch → QuarantineError`. **Fold in the S1-retro carry-forwards** (`.loom/133:36-40`): (1) recursive arg canonicalizer for non-scalar Starlark args (the spike used `.String()` on lists/dicts — `host.go:55-71`); (2) explicit **sibling-insert AND sibling-delete** drift test in one scope frame (the 7 spike scenarios only had a flat global counter); (3) a focused **durable `UNIQUE(run_id,step_key)` pending→success upsert test against the real `WorkflowDAO`** (the spike only exercised a map) — **this is verify:test-validity mustChange #4 and is mandatory in S0**.
- **filesAffected**: `go.mod`, `go.work`, `pkg/mills/workflow/{interp,host,journal_dao,workflow_test}.go` (new), `pkg/mills/workflow/spike/*` (delete).
- **Verification**: `go test ./pkg/mills/workflow/ -race -v` — 7 ported scenarios + the 3 carry-forwards green, using a real temp-file `store.Store` with migration 004 (not `MemJournal`). `go vet` import-ban confirms `net`/`os`/`time`/random are unreachable from the universe.
- **Effort**: M (2–4 days). Risk: `go.work` relative-path breakage from a worktree (MEMORY: `reference_go_build_from_worktree`) — build on main or with an absolute-path temp `go.work`.

### S6-min — minimal deployed runtime (the S1c crash subject)
- **dependsOn**: S0, MERGE-LAYERS.
- **Deliverables**:
  - `pkg/mills/workflow/runtime.go`: `WorkflowInterpreter{dao, runner WorkerRunner}.Run(ctx, run)` — `ListByRun` replay; `agent()` → `WorkerRequest{AgentType, IdempotencyKey: deriveStepKey(run_id, step_key, call_hash)}`; **`AppendStep(pending)` before `WorkerRunner.Run`, `AppendStep(terminal)` after**, recording `SpawnID`+`CostSource`.
  - `pkg/mills/workflow/scheduler_min.go`: tick goroutine scanning `workflow_runs WHERE state='running' AND engine='imperative'`; `ListPending` reconciles interrupted spawns; resume re-runs read-through (short-circuit recorded steps; for a pending-with-spawn-id step, `WorkerResumer.Resume(idempotencyKey)`). **Must honor a between-step `paused_at` check** even in the minimal version (see §5 — `policy.IsEnabled` cannot abort an in-flight spawn).
  - `pkg/mills/policy.go`: `WorkflowsPolicy{Enabled bool, SubstrateK8sOnly bool}` as `Policy.Workflows` (mirror the `Intake`/`Squads` section pattern); **default OFF**; k8s-only.
  - `cmd/loom-mills-operator/main.go`: construct interpreter + scheduler-min in the errgroup **alongside** the existing reconciler (~`main.go:338-391`/`419-425`), active only when `policy.workflows.enabled`; reuse the **same** spawn-client adapter the DAG pipeline uses — zero new pods/services.
  - **Hardcoded canary script** in Go: `agent('implement', model='claude-code', budget_usd=1.0) ; gate('trivial') ; done` — not a template, not parameterized (registry is S7). **Scope the canary to STOP at the gate, pre-merge** (verify:test-validity + verify:safety mustChange — merge idempotency is deferred to S6-full, so a merging canary cannot satisfy PASS-3).
  - Engine discriminator: imperative runs never enter the DAG runner and vice-versa.
  - A small admin/test entrypoint to create a `workflow_run` (engine=imperative, state=running) for a backlog item (S7 selection does not exist yet).
- **filesAffected**: `pkg/mills/workflow/{runtime,scheduler_min}.go` (new), `pkg/mills/policy.go`, `cmd/loom-mills-operator/main.go`, `platform/gitops/k3s/mills/configmap-policy.yaml` (add `workflows:` section, **`enabled: false`** committed).
- **Verification (in-process, not the deployed gate)**: real `store.Store` + migration 004 + a fake `WorkerRunner` counting live invocations; drive an imperative run; **drop the interpreter (simulate crash)**; re-create sharing the same store; re-run the tick — assert live-invocation count unchanged (replay short-circuits) and the run reaches `done` exactly once. `go test ./pkg/mills/workflow/ ./cmd/loom-mills-operator/ -race`. Operator boots with flag OFF and logs the capability as ready-but-disabled. **State plainly: this in-process test proves only journal short-circuit (same property as the spike) and CANNOT substitute for the deployed S1c** (verify:test-validity mustChange #3).
- **killTestGated**: false (ships behind OFF flag).
- **Effort**: M-L (4–6 days).

### S4 — HUD step-log replay panel (parallel; recommended before S1c)
- **dependsOn**: MERGE-LAYERS (only). Runs concurrently with the S0→S1c chain; **not a hard gate** — a `kubectl exec … sqlite3` SQL probe substitutes during the crash test.
- **Deliverables**: `GET /api/mills/workflow/runs` + `/{id}` (run + nested steps, proxied like existing mills detail endpoints); SSE timeline with cache-hit/live/quarantine badges; KPI counters (`workflow_quarantined_runs`, cost branched on `CostSource`); **`dist/` rebuilt via `make hud-frontend` and COMMITTED** (the `go:embed` gotcha — CI does not rebuild the frontend).
- **filesAffected**: `cmd/loom-mills-operator/main.go`, `internal/hud/monitor/mills.go`, `pkg/mills/kpi_writer.go`, `internal/hud/embed.go` + frontend + `dist/`.
- **Verification**: endpoint tests `go test ./internal/hud/...`; visual check of a seeded imperative run.
- **Effort**: L (1.5–2 weeks incl. frontend).

### S1c — DEPLOYED dual-crash kill-test (the gate)
- **dependsOn**: S6-min deployed. **Plus the infra prerequisites in §4** (cluster currently unreachable — hard blocker).
- **Deliverables**: `pkg/mills/workflow/killtest/` harness + runbook (the §3 `kubectl` sequence); S6-min image deployed via Flux IUA; `workflows.enabled=true` flipped in `configmap-policy.yaml` + pod-checksum bump **for the canary window only, then reverted**; a canary backlog item + imperative `workflow_run` targeting **k8s** (not harvester-vm); recorded live evidence; flip the riskiest-assumption status to `passed YYYY-MM-DD` (or `FAILED` with the observed divergence).
- **Verification**: see §3. Net: exactly one spawn pod across both crashes; one `workflow_steps` success row; no double-MR.
- **killTestGated**: true (this IS the gate).
- **Effort**: L. Requires cluster connectivity (HARD infra blocker, §4) and a pre-confirmed canary substrate (§4, B5).

### S6-full — full capability-confined Starlark runtime (KILL-TEST-GATED on S1c)
- **dependsOn**: **S1c PASS**.
- **Deliverables**: `parallel()`/`loop_until_dry()` promoted from S0-ported logic; host resource envelope (wall-clock deadline, concurrent-spawn semaphore, total-spawn cap, loop ceiling, mem limit — council params can only LOWER); pre-flight budget reservation + 5s watcher + turn/time fallback when `CostSource != real`; **merge idempotency at `GitLabClient.Merge`**; `WorkflowScheduler` honoring `AutonomyGate`+`paused_at`; interpreter/template-version pinning with hard resume-abort-and-escalate on drift; CI deploy-safety gate (drain in-flight imperative runs before bumping `go.starlark.net` or any EffectHost builtin signature); test-time fake EffectHost fails on un-recorded IO + import-ban vet check.
- **filesAffected**: `pkg/mills/workflow/{interp,host,scheduler,budget,parallel}.go`, `cmd/loom-mills-operator/main.go:338-391`, `pkg/mills/policy.go`, `internal/hud/clients/gitlab.go:271-284`.
- **Verification**: S1c crash test re-run on a `loop_until_dry` workflow + a **merging** canary (now that merge idempotency exists) + hostile-param clamp + import-ban vet. `go test ./pkg/mills/workflow/ -race`.
- **Effort**: XL (2.5–3 weeks).

### S7 — council template selection + closed registry + clamping
- **dependsOn**: S6-full.
- **Deliverables**: `TemplateRegistry` (named template → Starlark source + params schema + defaults; content-hashed, immutable; startup fail-fast on unknown pinned version); `ResolveWorkflowTemplate` in `reconciler.tryStart` after squad routing, before `run.Put`, freezing immutable engine + `template_version` + pinned `interpreter_version` + clamped params; numeric clamp fuzz test; engine-discriminator guard so selection never re-routes started runs.
- **filesAffected**: `pkg/mills/workflow/registry.go` (new), `pkg/mills/reconciler.go:608-649`, `pkg/mills/council/backlog_mutator.go`, `pkg/mills/squads/types.go`, `pkg/mills/store/types.go`.
- **Verification**: clamp fuzz; unknown-template rejection; in-flight-run-not-re-routed. `go test ./pkg/mills/...`.
- **Effort**: L (1–1.5 weeks).

---

## 3. The EXACT S1c deployed kill-test procedure

> **Test-validity (verdict: sound-with-caveats).** The gate is valid *as designed* because S6-min wires the three crash-critical pieces **real** (DAO journal, keyed spawn, real resume re-running read-through) — it does not stub what S1c must prove. But two things must be real for the test to mean anything: **(a) the test must be DUAL-crash** (operator-only is a trivial pass — see below), and **(b) PASS-1 must assert dedupe fired via the durable path, not merely "one pod exists"** (a *failed* racing Create also leaves one pod).

### 3.1 Two-state-holder architecture (verified) — why dual-crash is mandatory

The spawn controller that does the dedupe does **not** run in the operator pod. There are **two** state-holders, and exactly-once requires **both** to survive a crash:

- **`loom-mills-operator`** (ns `loom-mills`): owns the `workflow_runs`/`workflow_steps` journal (SQLite on RWO PVC, `replicas: 1`, `strategy: Recreate` — `deployment.yaml:10,15-17`).
- **`mobile-hud`** (ns `loom-hub`): owns the spawn controller. The operator calls it over HTTP at `http://mobile-hud.loom-hub.svc.cluster.local` (`deployment.yaml:177`). The controller's dedupe map `c.spawns` is **in-memory only** (`controller.go:169`); durable state is a `K8sConfigMapStore` that `LoadAll` rehydrates on restart (`controller.go:442`).

**An operator-only crash is a trivial pass that proves nothing**: `mobile-hud`'s `c.spawns` stays intact, so the in-memory lookup at `controller.go:169` dedupes without ever exercising durable rehydration. The crash that matters is **CRASH B (mobile-hud)**, which empties `c.spawns` and forces the dedupe onto either the persistent store or a real k8s `AlreadyExists`.

### 3.2 Preconditions (assert before starting)
- **P1. Cluster reachable** **[live-unverified — currently FAILS, §4 B1]**: `KUBECONFIG=platform/gitops/.kube/k3s.yaml kubectl get ns loom-mills loom-hub` succeeds.
- **P2. Operator image is the S6-min build** **[live-unverified]**: `kubectl -n loom-mills get deploy loom-mills-operator -o jsonpath='{..image}'` matches the S6-min commit (Flux IUA bumps `deployment.yaml:75` post-merge).
- **P3. Flag hot-reloaded ON** **[live-unverified]**: `kubectl -n loom-mills exec deploy/loom-mills-operator -- cat /etc/loom-mills/policy.yaml | grep -A2 workflows` shows `enabled: true`; operator log shows the workflow capability enabled.
- **P4. Canary substrate pre-confirmed working** (§4 B5): codex pinned via `SPAWN_CODEX_MODEL=gpt-5.5` (loom-core!640) **or** claude-code/gemini with confirmed auth — a model-access 400 at `plan_slice` would mask the crash result (the A2 failure mode).
- **P5. Substrate is k8s, not harvester-vm** (structural gate — §5).
- **P6. Spawn-state ConfigMap namespace is `devbox`** (verified: `store_test.go:173,206` construct `K8sConfigMapStore` with namespace `"devbox"`). **All ConfigMap probes target `devbox`, not `loom-hub`** (verify:infra-reality mustChange #1).

### 3.3 Step-by-step
1. **Launch** the canary: create a `workflow_run` (engine=imperative, state=running) for a canary backlog item via the S6-min admin entrypoint. Record `run_id`.
2. **Advance to the first spawn**: poll `kubectl -n loom-mills exec deploy/loom-mills-operator -- sqlite3 /var/lib/loom-mills/state.db "SELECT step_key,status,spawn_id FROM workflow_steps WHERE run_id='<run_id>';"` until a `spawn_requested` row shows `status=pending` with non-null `spawn_id` (this is the record-before-dispatch row; `spawn_id == DeriveSpawnID(idempotency_key)`).
3. **Confirm exactly one pod**: `kubectl get pods -A -l loom/spawn-id=<spawn_id>` (or by name `spawn-<spawn_id>` — the pod name is `"spawn-"+spawnID`, `internal/hud/spawn.go:662`) returns exactly 1. Record its name.
4. **CRASH A (operator)**: `kubectl -n loom-mills delete pod -l app.kubernetes.io/name=loom-mills-operator --grace-period=0 --force`. The `Recreate` Deployment restarts it on the RWO PVC. Wait for Ready.
5. **CRASH B (mobile-hud), interleaved before the operator's resume completes**: `kubectl -n loom-hub delete pod -l app=mobile-hud --grace-period=0 --force`. **Pin the actual selector from the loom-hub manifest before the run** (verify:infra-reality mustChange #4) — `app=mobile-hud` is the assumed label and **[live-unverified]**. mobile-hud restarts and runs `LoadAll`, rehydrating `c.spawns` from the `devbox` ConfigMap.
6. **Let both restart.** The operator's scheduler-min tick finds the run still `running`, replays `workflow_steps`: the pending `spawn_requested` step re-derives the **same** `step_key`+`call_hash` (deterministic), so read-through either (a) short-circuits on a recorded success, or (b) for a still-pending step calls `WorkerResumer.Resume(idempotency_key)` → `DeriveSpawnID` → same id → `spawnWithKey` sees the existing entry and returns AlreadyExists (no second pod).
7. **Run to terminal.** For S6-min, the canary **stops at the gate (pre-merge)** — do **not** let it reach GitLab merge (merge idempotency is S6-full).

### 3.4 PASS (ALL must hold)
- **PASS-1 (no double-spawn, via the durable path)**: exactly ONE pod for `<spawn_id>` across both crashes — never two — **AND** the mobile-hud log shows `"idempotent spawn re-attach (already exists)"` (`controller.go:173`) **or** a real k8s `AlreadyExists` on the derived name. "One pod" alone is insufficient: a failed racing Create also yields one pod (verify:test-validity mustChange #3).
- **PASS-2 (journal exactly-once)**: `SELECT count(*) FROM workflow_steps WHERE run_id='<run_id>' AND step_key='<spawn step_key>' AND status='success'` == 1; no duplicate `(run_id,step_key)` rows (`UNIQUE` enforces); no spurious quarantine.
- **PASS-3 (no double-merge)**: not exercised by the S6-min canary (it stops pre-merge); asserted in S6-full's merging re-run.
- **PASS-4 (cost provenance)**: the success row's `cost_source` matches the harness (claude-code=real, codex=estimated, gemini=unavailable); an empty-telemetry + exit≠0 run is recorded as a **failure** step, not silently dropped.
- **PASS-5 (counter exact)**: distinct spawn pods (or success rows) == number of `agent()` calls in the script (1 for the minimal workflow), unchanged by the crashes.

### 3.5 FAIL (any one)
- A second pod for the same logical step (idempotency did not dedupe — most likely mobile-hud `LoadAll` did not rehydrate before the operator re-dispatched, i.e. ordering or `devbox` ConfigMap persistence is broken).
- Two success rows / a quarantine on deterministic replay (step-key derivation drifted across restart).
- Operator crash-loops on resume (version-pin or migration issue) instead of resuming.

A **FAILED** status means the Layer-3 imperative bet does not hold on the real substrate → fall back to declarative-DAG (`.loom/132 §7` option b). Record the divergence with the `kubectl`/SQL evidence.

### 3.6 Single-loomd-lock gotcha
Do **not** attempt an isolated operator or isolated loomd — the SQLite RWO PVC is single-writer and the spawn dedupe only works against the production mobile-hud's live ConfigMap/cluster state. Use the **real deployed pods** (kill + restart), never a side instance.

---

## 4. INFRA PREREQUISITES (gating pre-slice)

> **Infra-reality (verdict: sound-with-caveats).** The plan correctly treats S1c as infra-blocked. The operator **is** deployed; the blocker is connectivity + merge timing, not feasibility. The mustChanges (devbox-namespace probes, codex model pin, named connectivity prereq with a verify command, pinned mobile-hud selector) are folded in below.

S1c **cannot start** until all of these clear. Treat this as a **gating pre-slice** before scheduling S1c.

- **B1 — Cluster connectivity (HARD BLOCKER; currently FAILING).** kubeconfig `192.168.50.200:6443` is unreachable. **Verify command (must succeed before scheduling S1c):**
  ```
  KUBECONFIG=platform/gitops/.kube/k3s.yaml kubectl get ns loom-mills loom-hub
  ```
  No code change unblocks this. **Provisioning steps**: restore VPN/LAN route to `192.168.50.200`; re-run the verify command; if the API server itself is down, see the k3s DiskPressure runbook (MEMORY: `reference_k3s_disk_pressure_runbook`). This is the single gating dependency for the whole S1c→S6-full chain.
- **B2 — Deployed operator carrying the S6-min build.** After MERGE-LAYERS + S6-min land on main, CI `build:image:loom-mills-operator` runs automatically (~90m), Flux IUA polls every 5m and bumps the image tag, the single-replica `Recreate` pod rolls (~5–15m merge-to-running). Confirm the live tag matches the S6-min commit (`deployment.yaml:75`) before S1c. **[live-unverified]**
- **B3 — Two pod surfaces identified.** `loom-mills-operator` (ns `loom-mills`, RWO PVC `/var/lib/loom-mills`) AND `mobile-hud` (ns `loom-hub`). **Pin the mobile-hud selector** from the loom-hub manifest before CRASH B — `app=mobile-hud` is assumed. Operator selector `app.kubernetes.io/name=loom-mills-operator` is also **[live-unverified]**.
- **B4 — Spawn-state ConfigMap namespace is `devbox`.** Verified in code (`store_test.go:173,206`). **All spawn-state ConfigMap probes target `devbox`**, even though the spawn *host* (mobile-hud) runs in `loom-hub`. Do not conflate the two.
- **B5 — Canary substrate pre-confirmed.** The harness must actually start a turn: codex with `SPAWN_CODEX_MODEL=gpt-5.5` (loom-core!640) or claude-code/gemini with confirmed auth — else a model-access 400 at `plan_slice` masks the crash result (A2 failure mode).
- **B6 — Spawn-state persistence is durable (not ephemeral).** Confirm mobile-hud uses `K8sConfigMapStore` (`controller.go:442`, `store.go`), not a `FileStore` on an ephemeral volume — otherwise CRASH B loses the dedupe state and S1c fails for an infra reason, not a code one. **[live-unverified]**
- **B7 — Policy flag wired + flipped.** `policy.workflows.enabled` exists in `pkg/mills/policy.go` (added in S6-min) and is set true in `configmap-policy.yaml` with a pod-checksum bump (ConfigMap is fsnotify hot-reloaded; no image rebuild). Default OFF; flipped only for the canary window, then reverted.
- **B8 — Canary item + run row.** A real backlog item + an imperative `workflow_run` (state=running) via the S6-min admin entrypoint, k8s substrate only.
- **B9 — Recommended (not hard).** S4 HUD step-log panel deployed for live journal visibility; otherwise the `kubectl exec … sqlite3 state.db` SQL probe substitutes.

---

## 5. Safety

> **Safety (verdict: sound-with-caveats).** The mustChanges below are folded in. The highest-risk seam — the dual-crash race on an in-memory-first dedupe — is upgraded from "verify which layer enforces dedupe" to a **required code change** because the live Create path was verified to lack `AlreadyExists` handling.

- **Default-OFF flag.** `policy.workflows.enabled` is **false** in the committed `configmap-policy.yaml`. S6-min can merge to main *without activating anything*. Only the S1c canary window flips it ON (then reverts). S6-full stays hard-gated on S1c PASS. **Mitigation against a premature prod flip**: keep the flag OFF in git; the canary flip is an out-of-band, reverted-immediately action.
- **Canary isolation, no risk to real merges.** The S6-min canary **stops at the gate, pre-merge** (verify:test-validity + verify:safety mustChange). It never invokes `GitLabClient.Merge`, so it cannot double-merge or touch real MRs. A merging canary is deferred to S6-full's re-run, after merge idempotency lands.
- **REQUIRED CODE CHANGE before S1c — close the dedupe seam to one hard invariant** (verify:test-validity #2, verify:safety #1). Today dedupe is **in-memory only**: `spawnWithKey` checks `c.spawns[spawnID]` (`controller.go:169`), and the live pod Create at `internal/hud/spawn.go:660-691` does **not** handle `apierrors.IsAlreadyExists` — line 691 `failSpawn`s on any create error. Because the pod name is already deterministic (`"spawn-"+DeriveSpawnID(key)`, `spawn.go:662`), the fix is small and high-leverage: **on the Create path, treat `IsAlreadyExists` on the derived name as a re-attach (no-op), not a failure.** Optionally also have `spawnWithKey` consult the persistent `devbox` ConfigMap on a cold create. Without this, a cold mobile-hud (empty `c.spawns` after CRASH B) that re-dispatches before `LoadAll` completes will either (a) double-spawn if the name weren't unique, or (b) `failSpawn` on the real `AlreadyExists`. The deterministic name + `IsAlreadyExists` re-attach is the durable backstop that makes the dual-crash race safe. **This change must land in or before S6-min and be exercised by CRASH B.**
- **Kill-switch cannot abort an in-flight spawn — `paused_at` is the fast stop** (verify:safety #3). `policy.IsEnabled` (`reconciler.go:158,250`) is eventually-consistent (GitOps→Flux→ConfigMap-poll) and only blocks *new* ticks. The scheduler-min **must honor a between-step `paused_at` check** so an emergency stop during the canary takes effect between steps. Make this a **tested S6-min criterion**, not a S6-full deferral.
- **Test the record-to-dispatch crash window, not just pod-exists** (verify:safety #4). Add an S1c arm that crashes in the window between `AppendStep(pending)` and the dispatch returning (`controller.go:161-162,191-192`) — assert resume does **not** double-dispatch a pending-with-no-spawn-id step.
- **Run multiple times / inject timing** (verify:safety #5). One green run on a race is not exactly-once proof. Run S1c ≥3 times, and if feasible inject a delay so the operator resumes *before* mobile-hud's `LoadAll` completes — the worst-case ordering the durable backstop must survive.
- **Version-pin tripwire freeze.** `HostInterpreterVersion` is pinned to the exact `go.starlark.net` pseudo-version; any bump forces resume-abort-and-escalate on every in-flight imperative run. **Freeze the version through the S1c canary window** so a bump doesn't abort the canary.
- **Cleanup.** After the canary: revert `workflows.enabled` to false in git + bump the checksum back; delete the canary backlog item and `workflow_run`; remove the canary spawn pod if it lingers; if the run reached a gate, cancel it. Record the evidence (logs + SQL counts) in the S1c handoff before tearing down.

---

## 6. Open questions / decisions still needed from the operator owner

1. **Cluster connectivity ETA.** When will the route to `192.168.50.200:6443` be restored? The entire S1c→S6-full chain is blocked on it (§4 B1). Is there an alternate reachable kubeconfig/jump host?
2. **mobile-hud pod selector.** Confirm the exact label selector for the mobile-hud Deployment in `loom-hub` (CRASH B step 5 assumes `app=mobile-hud`). **[live-unverified]**
3. **Harvester-vm dedupe — permanent k8s-only?** (`.loom/132 Open Question 4`) Does harvester-vm have any name-based create-dedupe primitive, or is exactly-once structurally impossible there? If none, imperative workflows stay k8s-only indefinitely and a separate (out-of-plan) VM kill-test is required before any VM workflow. **Decision needed**: accept k8s-only for the foreseeable future, or fund the VM dedupe investigation?
4. **AlreadyExists backstop placement.** Confirm the §5 required change lands in S6-min (preferred) vs. a stacked pre-S1c fix. Also: should `spawnWithKey` additionally consult the persistent `devbox` ConfigMap on a cold create, or is the deterministic-name + `IsAlreadyExists` re-attach sufficient as the sole durable invariant?
5. **Canary substrate choice.** claude-code (real cost), codex (`gpt-5.5`, estimated cost), or gemini (unavailable cost) for the S1c canary? Affects the PASS-4 `cost_source` assertion (§3.4).
6. **S4 before S1c?** Deploy the HUD step-log panel for live journal visibility during the crash, or accept the `kubectl exec … sqlite3` SQL probe and defer S4? (Recommended: deploy S4 first if the connectivity-restore timeline allows.)

---

**Verdict reconciliation note**: no verdict was `broken`. All three were `sound-with-caveats`; every mustChange is incorporated. The two namespace facts that appeared contradictory across verdicts are both true and now disambiguated (§3.1, §4 B4): the spawn **host** is `mobile-hud` in `loom-hub` (`deployment.yaml:177`); the spawn-**state ConfigMap** that `LoadAll` rehydrates is in `devbox` (`store_test.go:173,206`). The single substantive escalation beyond the original design is promoting the AlreadyExists backstop from a "verify which layer enforces dedupe" risk note to a **required code change before S1c** (§5), justified by the verified gap at `internal/hud/spawn.go:691` (no `IsAlreadyExists` handling on the live Create).
