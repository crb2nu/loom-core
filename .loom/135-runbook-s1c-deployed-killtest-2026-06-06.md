# Runbook: S1c — Mills deployed dual-crash kill-test

**Date**: 2026-06-06
**Owner**: Cody Blevins
**Type**: Runbook (executable checklist)
**Source design**: `.loom/134 §3–§5` (this is the standalone, copy-pasteable execution form)
**Status**: NOT YET RUN — blocked on S6-min being deployed (see Prerequisites).

## Purpose

Prove **exactly-once agent spawn across an operator-process crash** for the Mills Layer-3 imperative workflow runtime, on the **real k8s substrate** — the deployed gate that authorizes building S6-full. This is the live counterpart to the in-process S1 kill-test (which passed 2026-06-06 but cannot exercise durable cross-process resume).

**Why dual-crash (not operator-only):** the spawn controller that dedupes runs in **`mobile-hud`** (ns `loom-hub`), not the operator. An operator-only crash leaves mobile-hud's in-memory `c.spawns` intact, so dedupe passes trivially and proves nothing. The crash that matters is **mobile-hud** (empties the in-memory map, forcing dedupe onto the durable path / real k8s `AlreadyExists`). Both state-holders must survive.

## Prerequisites (ALL must clear before starting — this is a gate)

> Values marked **[verify-at-runtime]** were not pinnable when this runbook was written (intermittent `kubectl` route from the automation host). Confirm each with the command shown before proceeding.

- [ ] **B1 — Cluster reachable.** `KUBECONFIG=platform/gitops/.kube/k3s.yaml kubectl get ns loom-mills loom-hub devbox` succeeds. (Cluster is healthy; if `kubectl` flaps `EHOSTUNREACH`, retry / drive from an on-LAN host — the API responds: `curl -sk https://192.168.50.200:6443/version` returns a k8s `Status`.)
- [ ] **B2 — Deployed operator carries the S6-min build.** loom-core !658 merged → CI `build:image:loom-mills-operator` (~90m) → Flux IUA bumps the image (polls ~5m) → single-replica `Recreate` pod rolls. Confirm: `kubectl -n loom-mills get deploy loom-mills-operator -o jsonpath='{..image}'` matches the S6-min commit.
- [ ] **B3 — Pod selectors pinned.** **[verify-at-runtime]** operator: `kubectl -n loom-mills get deploy loom-mills-operator -o jsonpath='{.spec.selector.matchLabels}'`; mobile-hud: `kubectl -n loom-hub get deploy -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.spec.selector.matchLabels}{"\n"}{end}' | grep -i hud`. Record both before CRASH steps. (`.loom/134` assumed operator `app.kubernetes.io/name=loom-mills-operator`, mobile-hud `app=mobile-hud` — VERIFY.)
- [ ] **B4 — Spawn-state ConfigMap is in `devbox` ns.** `kubectl -n devbox get cm | grep -i spawn`. All spawn-state probes target `devbox`, even though the spawn host (mobile-hud) runs in `loom-hub`.
- [ ] **B5 — Canary substrate pre-confirmed.** The harness must actually start a turn. codex: `SPAWN_CODEX_MODEL=gpt-5.5` (loom-core!640); or claude-code/gemini with confirmed auth. A model-access 400 at the spawn masks the crash result (the A2 failure mode). **Decision needed** (`.loom/134 §6 Q5`): which harness for the canary (affects the PASS-4 `cost_source` check).
- [ ] **B6 — Spawn state is durable.** Confirm mobile-hud uses `K8sConfigMapStore` (not an ephemeral `FileStore`), so `LoadAll` rehydrates across CRASH B. **[verify-at-runtime]**
- [ ] **B7 — Flag wired + ready to flip.** `workflows:` section is in `k3s/mills/configmap-policy.yaml` (gitops !242, committed `enabled: false`). The S6-min binary defaults it OFF. To activate for the canary window: set `enabled: true` + bump the deployment's pod-checksum annotation, reconcile, verify: `kubectl -n loom-mills exec deploy/loom-mills-operator -- sh -c 'cat /etc/loom-mills/policy.yaml | grep -A2 workflows'` shows `enabled: true`. **Revert after.**
- [ ] **B8 — Substrate is k8s, not harvester-vm** (structural gate — imperative dedupe is k8s-only until harvester-vm name-dedupe is proven).
- [ ] **B9 — (recommended)** S4 HUD step-log panel deployed for live journal visibility; otherwise use the `kubectl exec … sqlite3` SQL probe in the procedure below.

## Procedure

> Single-loomd-lock: do **not** spin an isolated operator/loomd — use the **real deployed pods** (kill + restart). The SQLite store is single-writer (RWO PVC) and dedupe only works against the live mobile-hud ConfigMap.

1. **Flip the flag ON** for the canary window (B7) and confirm the operator hot-reloaded it.
2. **Launch the canary**: create an imperative `workflow_run` (engine=imperative, state=running) for a throwaway `mills-canary` backlog item via the S6-min admin entrypoint. Record `RUN_ID`.
3. **Advance to the first spawn**: poll the journal until the `spawn_requested` step is `pending` with a non-null `spawn_id`:
   ```
   kubectl -n loom-mills exec deploy/loom-mills-operator -- \
     sqlite3 /var/lib/loom-mills/state.db \
     "SELECT step_key,status,spawn_id FROM workflow_steps WHERE run_id='$RUN_ID';"
   ```
   (`spawn_id` == `DeriveSpawnID(idempotency_key)`; pod name == `spawn-$spawn_id`, `internal/hud/spawn.go:662`.)
4. **Confirm exactly one pod**: `kubectl get pods -A | grep spawn-$SPAWN_ID` → exactly 1. Record it.
5. **CRASH A (operator)**: `kubectl -n loom-mills delete pod -l <operator-selector from B3> --grace-period=0 --force`. The `Recreate` Deployment restarts it on the RWO PVC. Wait for Ready.
6. **CRASH B (mobile-hud), interleaved before the operator's resume completes**: `kubectl -n loom-hub delete pod -l <mobile-hud-selector from B3> --grace-period=0 --force`. mobile-hud restarts, runs `LoadAll`, rehydrates `c.spawns` from the `devbox` ConfigMap.
7. **Let both restart.** The operator's `WorkflowScheduler` tick finds the run still `running`, replays `workflow_steps`: the `pending` `spawn_requested` step re-derives the **same** `step_key`+`call_hash`, so `readThrough` re-runs it via the executor → `WorkerResumer.Resume(idempotency_key)` → same `DeriveSpawnID` → `spawnWithKey` / the **S2c AlreadyExists backstop** re-attaches to the existing pod (no second pod).
8. **Run to terminal.** The S6-min canary **stops at the gate (pre-merge)** — do NOT let it reach GitLab merge (merge idempotency is S6-full).

## PASS criteria (ALL must hold)

- [ ] **PASS-1 (no double-spawn, via the durable path):** exactly ONE pod for `$SPAWN_ID` across both crashes — never two — AND the mobile-hud log shows `"idempotent spawn re-attach (already exists)"` (controller.go) **or** the operator log shows `"k8s AlreadyExists on derived spawn name — re-attaching"` (S2c, `internal/hud/spawn.go`). "One pod" alone is insufficient — a *failed* racing create also yields one pod.
- [ ] **PASS-2 (journal exactly-once):** `SELECT count(*) FROM workflow_steps WHERE run_id='$RUN_ID' AND step_key='<spawn step_key>' AND status='success'` == 1; no duplicate `(run_id,step_key)` rows; no spurious quarantine.
- [ ] **PASS-3 (no double-merge):** N/A for the S6-min canary (stops pre-merge); asserted in S6-full's merging re-run.
- [ ] **PASS-4 (cost provenance):** the success row's `cost_source` matches the harness (claude-code=real, codex=estimated, gemini=unavailable); an empty-telemetry + `exit!=0` run is recorded as a **failure** step, not silently dropped.
- [ ] **PASS-5 (counter exact):** distinct spawn pods (or success rows) == number of `agent()` calls in the canary (1), unchanged by the crashes.

**Extra arms** (`.loom/134 §5`): also run a crash in the **record→dispatch window** (between `AppendStep(pending)` and dispatch return) and assert no double-dispatch; and run the whole test **≥3 times** (ideally injecting a delay so the operator resumes *before* mobile-hud `LoadAll` completes — the worst-case ordering the backstop must survive). One green run on a race is not exactly-once proof.

## FAIL (any one) → record + fall back

- A second pod for the same logical step (dedupe didn't fire — most likely mobile-hud `LoadAll` didn't rehydrate before the operator re-dispatched, or `devbox` ConfigMap persistence is broken).
- Two `success` rows / a quarantine on deterministic replay (step-key derivation drifted across restart).
- Operator crash-loops on resume (version-pin/migration issue).

A **FAILED** status means the Layer-3 imperative bet doesn't hold on the real substrate → fall back to the declarative-DAG model (`.loom/132 §7` option b). Record the divergence with the `kubectl`/SQL evidence and flip the riskiest-assumption status (`.loom/132 §3`) to `FAILED YYYY-MM-DD`.

## Rollback / cleanup (ALWAYS, after PASS or FAIL)

1. Revert `workflows.enabled` → `false` in `k3s/mills/configmap-policy.yaml` + bump the pod-checksum back; reconcile.
2. Delete the canary backlog item and its `workflow_run`; if the run reached the gate, cancel it; remove the canary spawn pod if it lingers.
3. Record evidence (operator + mobile-hud logs, the SQL counts) in the S1c handoff before tearing down.
4. On PASS: flip `.loom/132 §3` riskiest-assumption status to `passed YYYY-MM-DD` with the live evidence, and unblock S6-full.

## Contacts / notes

- Cluster: k3s on Harvester VMs; API `192.168.50.200:6443` (intermittent route from the automation host — drive from a stable/on-LAN vantage). DiskPressure runbook: memory `reference_k3s_disk_pressure_runbook`.
- Stuck CI runner during the build: memory `CI: Stuck Runner Diagnostic`.
- Open decisions before running (`.loom/134 §6`): canary harness choice (Q5); whether harvester-vm stays permanently k8s-only (Q3).
