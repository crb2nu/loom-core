# Plan — Mills full-vision roadmap: from substrate-proven to autonomous-merges-at-scale (2026-06-01)

Multi-phase plan to carry Loom Mills from "every execution primitive is
built and unit-proven, but zero pipelines have ever merged unattended"
to the north-star: **sustained merged-MRs/day with no human touch, at
quality, across repos.**

- **Date**: 2026-06-01
- **Lineage** — converges three tracks that are now ready to meet:
  - `.loom/43-plan-mills-autonomy-2026-05-24.md` — autonomy loop
    plumbing (auto-merge, retry classification, intake, loop closure).
    8 slices shipped; 3 deferred (1b, 1c, 3c).
  - `.loom/44-mills-autonomy-retro-2026-05-25.md` — the retro that found
    the autonomy infra is sound but **end-to-end never closed**: the
    spawn agent produced empty MRs because the pod-local shallow clone
    is invisible to the operator's worktree (`.loom/119-…`).
  - `.loom/45-product-spec-mills-harvester-vm-substrate-2026-05-25.md` —
    the KubeVirt VM substrate that structurally fixes the empty-MR gap
    (the VM's disk IS the worktree). Shipped through **Slice 2d.5d**;
    codex home-parity kill-test **PASSED 2026-06-01**.
  - `docs/MILLS.md` §"Mills v2" — hierarchical-swarm features (squads,
    audit, cross-repo, debate, recursion, adaptive policy) all shipped
    as code, all default-OFF awaiting sequential Phase-8.3 flips.

---

## North-star metric (unchanged from `.loom/43`)

```
autonomous_merges_24h = count(pipeline_runs WHERE
  terminal_state = "merged"
  AND attempts_total = 1
  AND escalated = false
  AND merged_via = "auto"
  AND last_24h)
```

**Current value: 0. It has been 0 every day since at least 2026-05-17**
(`.loom/44` §kill-test: 0 of 56 runs ever reached `done`). Every other
metric (auto_merge_rate, escalation_rate, slice-to-merge p50) is a drag
term on this one. This plan is organized strictly by leverage on this
number.

---

## Current state — the honest one-screen snapshot

What is **built and proven**:

- Autonomy plumbing live in cluster (operator image `20260525-112914`):
  transient-vs-code retry classification, buildah eviction-race fix,
  `merge_when_pipeline_succeeds` wiring, webhook notify (inert — no
  URL), tick-on-merge, bounded escalation auto-retry, canary GC (swept
  56 stuck items), GitLab issue importer (polling, 0 eligible issues).
  [`.loom/44` §"What shipped"]
- The spawn-execution gap chain fixed across `!525`/`!527`/`!531`/
  `!535`/`!543`/`!545`: init container checks out `req.Branch`, chowns
  the workspace to uid 1000, codex runs with bypass-approvals, codex
  JSONL is parsed, and `OPENAI_API_KEY=PLACEHOLDER` no longer overrides
  `auth.json`. [`.loom/44` §"Post-merge verification log"]
- harvester-vm substrate: full `Backend` implementation
  (`internal/devbox/backend/harvester_vm*.go`) with env + SecretEnv +
  SecretMount + home-dir parity + out-of-line cloud-init + ownerRef-race
  retry. Spawn path threads per-stage substrate from policy →
  `SpawnRequest.Substrate` → pod `DEVBOX_BACKEND` → per-spawn backend
  resolution (Slices 2b/2c/2d). **Kill-test
  `TestHarvesterVMBackend_Integration_CodexHomeParity` PASSED on-LAN
  2026-06-01 (110s)**: VM boots, SSH as `agent`, codex auth.json is
  byte-for-byte correct, `~/.codex/auth.json` symlink resolves.

What is **NOT done** (verified 2026-06-01):

1. **No mills canary has ever merged through harvester-vm.** Prod policy
   `platform/gitops/k3s/mills/configmap-policy.yaml:104` is
   `stage_substrate: {}` (k8s default). A per-item `mills-canary-
   harvester-vm` label exists to route one canary, and the inline
   comment gates the global flip on *"at least one auto-merge."* That
   merge is the whole ballgame and it has not happened.
2. **Curated base image never built.** No
   `platform/gitops/harvester/mills-devbox-base/`. The kill-test ran on
   stock `longhorn-image-mc9ph` with a self-healing
   `apt-get install qemu-guest-agent` first-runcmd — adds ~70s to boot
   and a network dependency at VM-create. (Spec §Slice 1.5 "remaining
   work".)
3. **Slice 2e (mcp-devbox multi-backend) not wired.**
   `cmd/mcp-devbox/main.go` still dispatches only `docker`/`k8s`; the
   harvester config fields exist but there is no `harvester-vm`
   constructor case, and `harvesterSSHUser` still defaults to `ubuntu`
   (stale vs. Slice 2d.5c's `agent`).
4. **Operator spawn path may not construct the harvester backend in
   prod.** Per Slice 2d, `initSpawnOrchestrator` builds
   `HarvesterVMBackend` only when `cfg.SpawnHarvesterKubeconfig != ""`.
   No `--spawn-harvester-*` / `$SPAWN_HARVESTER_*` wiring was found in
   the HUD/operator deployment manifests — so a `harvester-vm` spawn
   today likely **silently falls back to k8s**. This must be the first
   thing the Phase-A kill-test confirms or fixes.
5. **Deferred demand/quality work** from `.loom/43`: workspace-signals
   council brief (1b), LLM-ranked dispatch (1c), outcome→ranker
   feedback (3c); notify webhook has no URL (3a inert).
6. **All v2 swarm features default-OFF** awaiting Phase-8.3 flips.

---

## Riskiest assumption + kill-test

**Load-bearing assumption**: Routing a real `mills-canary` pipeline's
`implement` + `tests` stages to the harvester-vm substrate closes the
empty-MR gap that blocked all 56 historical runs — i.e. the agent runs
on the VM's own disk, commits, and pushes a **non-empty** branch, so the
`mr` stage opens an MR with a real `head_sha`, CI runs against real
commits, and `merge_when_pipeline_succeeds=true` lands it unattended.
This is the convergence bet of all three tracks: it assumes the
substrate fix (proven at the *backend* kill-test level) actually
propagates through the *pipeline integration* level to a merge.

**Kill test** (≤30 min once prerequisites are wired; this IS Slice A2):
1. Confirm the prod operator constructs the harvester backend
   (`SpawnHarvesterKubeconfig` set; `/api/mills/capabilities` shows the
   harvester substrate row green). Fix the deployment wiring if not.
2. Enqueue one backlog item labelled `mills-canary-harvester-vm` (or set
   `stage_substrate: {implement: harvester-vm, tests: harvester-vm}` for
   a single canary tick) with a trivial, real file change
   (e.g. append a line to `testdata/mills-canary/heartbeat.md`).
3. Watch it traverse `implement → tests → mr → ci_watch → merge`.
4. Assert, from `stage_results` + the GitLab MR:
   - implement `diff_patch` is **non-empty** and `files_changed ≥ 1`
   - the MR has a real `head_sha` and a running `head_pipeline`
   - `merge_when_pipeline_succeeds=true` was set
   - terminal state `merged`, `merged_via=auto`, `attempts_total` low.

**Pass criteria**: `autonomous_merges_24h` ticks from 0 → ≥1 with a
non-empty diff. (A merge of an empty branch does NOT count — that was
the `!518`/`!520`/`!522` false-positive.)

**Failure mode if wrong**: if the canary still produces an empty MR on
harvester-vm, the substrate did not actually change the agent's working
directory in the spawn path — meaning the `SpawnRequest.Substrate` →
backend-resolution wiring has a prod gap the unit tests don't cover, and
Phase B/C/D are all premature. We'd reopen the spawn-execution
investigation (`.loom/119`) against the VM path specifically.

**Status**: not run (Slice A1 wiring **DONE 2026-06-01**; A2 kill-test not yet
run). Evidence: `.loom/local/handoffs/mills-harvester-vm-slice-a1-canary-2026-06-01.md`.

> This kill-test gates the entire rest of the plan. Phases B, C, D do not
> start until Phase A produces one real autonomous merge.

---

## The phased roadmap

### Phase A — Close the loop ONCE (the convergence milestone) 🔴 CRITICAL PATH

The single highest-leverage work. Until one canary merges unattended
through harvester-vm, every other improvement is speculative.

**Slice A1 — Wire harvester-vm into the prod spawn path. ✅ DONE 2026-06-01.**
Evidence: `.loom/local/handoffs/mills-harvester-vm-slice-a1-canary-2026-06-01.md`.
Shipped via [loom-core!587](https://gitlab.flexinfer.ai/services/loom-core/-/merge_requests/587)
(mobile-hud mounts the scoped kubeconfig + `SPAWN_HARVESTER_KUBECONFIG`) on top of
the least-privilege SA work ([gitops!199](https://gitlab.flexinfer.ai/platform/gitops/-/merge_requests/199)
+ [gitops!201](https://gitlab.flexinfer.ai/platform/gitops/-/merge_requests/201)).
Confirmed live: `available="[k8s harvester-vm]"`, no RBAC errors; an
operator-path canary (`PIPE-MILLS-CANARY-20260601-233435`) routed its implement
stage to harvester-vm and provably spawned VMI `spawn-spawn-64e203df77f5` (+ Bound
20Gi PVC) via the scoped `mills-spawn` SA, zero forbidden. VM cascade-cleaned on
Stop. **Caveat for A2**: routing was reverted right after the spawn, so only the
implement stage ran on the VM — no end-to-end merge yet.
- Set `--spawn-harvester-kubeconfig` (+ base-image, namespace,
  storage-class, network-attach-def) on the operator/HUD deployment from
  the existing `harvester-kubeconfig` secret
  (`platform/gitops/clusters/k3s/flux-system/secret-harvester-
  kubeconfig.yaml`).
- Verify `initSpawnOrchestrator` registers the `harvester-vm` backend
  (warn-log absent today = silent k8s fallback).
- Reconcile the SSH-user default to `agent` everywhere (close the
  `cmd/mcp-devbox/main.go` `ubuntu` drift).
- **Done when**: `/api/mills/capabilities` shows a green harvester
  substrate row; a `harvester-vm`-labelled spawn provably starts a VMI
  (not a pod), confirmed in operator logs.
- Files: `platform/gitops/k3s/mills/deployment.yaml` (or HUD
  deployment), `cmd/loom/hud.go` flag plumbing audit,
  `cmd/mcp-devbox/main.go` SSH-user default.

**Slice A2 — First end-to-end autonomous merge (the kill-test above).**
- Run the riskiest-assumption kill-test against a single
  `mills-canary-harvester-vm` item with a real diff.
- **Done when**: north-star metric 0 → ≥1, non-empty diff, `merged_via=
  auto`. Evidence memo in `.loom/local/handoffs/`.
- **Decision point — which agent**: `.loom/44` flagged codex OAuth
  tokens in `cluster-agent-auth.codex-auth-json` as stale. The
  2026-06-01 kill-test proved the secret *mounts* correctly, not that
  codex makes a live LLM call. **Recommend running the first canary on
  the agent with known-good cluster auth** (gemini was end-to-end-clean
  per Slice 2d.5; claude-code via `ANTHROPIC_API_KEY`). Refresh codex
  OAuth as a parallel operational task, not a blocker.

**Slice A3 — Sustain: 7 consecutive green canaries on harvester-vm.**
- Let the canary loop run with `mills-canary-harvester-vm` enqueued
  on a schedule; observe escalation rate + regression rate.
- **Done when**: 7-day window shows ≥7 autonomous merges, 0
  regressions, harvester-vm escalation rate < k8s baseline. This is the
  gate the policy comment names for flipping global defaults (Phase D1).

### Phase B — Make it reliable & fast (parallelizable with A3)

**Slice B1 — Curated base image build pipeline.**
- `platform/gitops/harvester/mills-devbox-base/`: virt-customize a
  24.04 qcow2 with Go 1.24 / Python 3.12+uv / Node 22+pnpm / buildah /
  git / kubectl / glab, **qemu-guest-agent enabled at boot**, openssh
  hardened. Publish as `VirtualMachineImage mills-devbox-base-YYYYMMDD`;
  point per-VM PVCs at `longhorn-image-mills-devbox-base`.
- Removes the self-healing apt-get path → boot ≤60s, no VM-create
  network dependency.
- **Done when**: a cold canary boots from the curated image in ≤60s with
  no `apt-get` in cloud-init logs. (Spec §Slice 1.5 remaining.)

**Slice B2 — Warm pool (spec §Slice 3).**
- Operator controller maintains `N=2` paused VMIs of the base image; on
  `Start`, unpause + hot-plug a fresh cloud-init seed disk.
- **Done when**: `Start` p50 ≤30s, p95 ≤60s (spec acceptance).

**Slice B3 — Substrate health fallback (spec §Slice 4, resilience half).**
- When harvester-vm `Health` fails, the spawn dispatcher falls back to
  `k8s` for that spawn and logs a degraded-substrate event, so a
  Harvester outage degrades rather than halts autonomy.
- **Done when**: a fault-injected Harvester-API failure routes the next
  canary to k8s with an audit event; autonomy continues.

### Phase C — Scale demand & quality (the deferred `.loom/43` slices)

Only worth doing once Phase A proves the loop closes — these improve the
*rate and quality* of an already-working loop.

**Slice C1 — Notify webhook gets a real destination (`.loom/43` 3a).**
- Smallest, highest-ratio: code is complete and inert. Set
  `policy.notify.webhook_url` (Slack incoming webhook or the
  `agent_context` handoff inbox per Open Question 3 in `.loom/43`).
- **Done when**: a real autonomous merge posts a webhook within 30s.

**Slice C2 — Workspace-signals council brief (`.loom/43` 1b).**
- Feed last-24h Loki errors + GitLab CI failure clusters + open canary
  failures into the council brief so the council proposes backlog items
  grounded in real workspace pain instead of synthetic canaries.
- Files: `pkg/mills/council/brief.go` (`WorkspaceSignals` field),
  `pkg/mills/clients/loki.go` (new), `pkg/mills/council/sidecar.go`.
- **Done when**: one council run visibly proposes an item grounded in a
  real Loki/CI error.

**Slice C3 — LLM-ranked dispatch + outcome feedback (`.loom/43` 1c+3c,
bundled).**
- Replace FIFO-within-priority with a small-model ranker scoring queued
  items by expected merge probability (item age/priority, recent
  merge history of the agent+repo pair, whether the item touches
  actively-escalating files). FIFO fallback on ranker outage. Pair with
  the outcome writeback (`pkg/mills/dispatch/outcomes.go`) the ranker
  reads. Heaviest R&D; needs the model-selection decision from
  `.loom/43` Open Question 5.
- **Done when**: ranker decisions logged on 10 consecutive dispatches;
  FIFO fallback unit-tested; one outcome→ranker round-trip in the audit
  log.

### Phase D — Flip to the full vision

**Slice D1 — Flip implement+tests defaults to harvester-vm (spec §Slice 4).**
- After Phase A3's 7 green days, uncomment
  `stage_substrate: {implement: harvester-vm, tests: harvester-vm}` in
  `configmap-policy.yaml`. k8s stays as Phase-B3 fallback.
- **Done when**: 7-day prod escalation rate at `tests` < 30% (was 93%
  per `.loom/44` kill-test).

**Slice D2 — Sequential v2 swarm flips (MILLS.md Phase 8.3).**
- One flip per week with a soak, in the documented order, each with the
  `MILLS_V2_ROLLBACK.md` playbook ready:
  1. `squads.enabled` (route ≥30% of items end-to-end)
  2. `audit.enabled: true, advisory_only: true`
  3. `council.debate` incident-only
  4. `cross_repo.enabled` (after 3 dogfood successes)
  5. `adaptive_policy` manual-apply
- **Done when**: each flip meets its `docs/MILLS.md` §"v2 acceptance
  criteria" with no regression-count spike over the soak.

---

## Sequencing

```
  Phase A (CRITICAL PATH — nothing else starts until A2 passes)
    A1 wire prod spawn path ─► A2 first autonomous merge (KILL-TEST)
                                   │ pass
                                   ▼
        ┌──────────────────────────┴───────────────────────────┐
        │                                                       │
   A3 sustain 7 green ◄─── runs concurrently with ───► Phase B (B1→B2, B3)
        │  (base image B1 feeds faster/cleaner canaries)
        ▼
   Phase D1 flip defaults  ◄─ gated on A3 + B3 fallback
        │
        ├─► Phase C (C1 cheap-now; C2, C3 improve rate/quality)
        │     C1 can ship anytime after A2 (it just needs one real merge)
        ▼
   Phase D2 v2 sequential flips (one/week, soak + rollback ready)
```

Why this order:
1. **A is forced**: the program has built every primitive and proven
   none of them in series. One real merge converts ~6 months of
   default-OFF code from "theoretically works" to "works", and the
   north-star from 0 to non-zero. It also de-risks everything: if A2
   fails, B/C/D were the wrong investment.
2. **B parallelizes with A3**: the base image and warm pool make the
   sustain phase faster and cleaner but aren't prerequisites for the
   first merge (the kill-test already ran on stock Ubuntu).
3. **C is rate/quality, not existence**: a smarter ranker on a loop that
   merges 0/day is worth 0. C1 (webhook URL) is the exception — trivial
   and useful the moment A2 lands.
4. **D is the payoff**: flip defaults + flip v2 features only on a
   demonstrably-green loop, sequentially, each behind its rollback.

---

## Non-goals (this round)

- **Not building a second Harvester host** — Slice 0 confirmed capacity
  (4 VMs at 72% CPU / 78% MEM).
- **Not migrating local-dev devbox** — claude-code on dev machines keeps
  `docker`.
- **Not adding new gate *types*** — promote/flip existing ones; don't add
  a security-scanner gate this round (`.loom/43` non-goal).
- **Not introducing Whereabouts IPAM** — DHCP + qemu-guest-agent is the
  IP story (spec §refinements).
- **Not flipping v2 features before D2** — they stay OFF until the loop
  is green and the flips are sequenced with soaks.

---

## Open questions (decide before the named slice)

1. **(A1)** Where do the `--spawn-harvester-*` flags belong — the
   `loom-mills-operator` Deployment or the HUD Deployment that owns the
   spawn orchestrator? Confirm which process calls
   `initSpawnOrchestrator` in prod.
2. **(A2)** First-canary agent: gemini (clean) vs codex (needs OAuth
   refresh) vs claude-code (`ANTHROPIC_API_KEY`). Recommend gemini or
   claude-code to avoid coupling the kill-test to an operational token
   refresh.
3. **(A2 parallel)** Refresh codex OAuth in `cluster-agent-auth.codex-
   auth-json` (operational: `codex login` on a workstation with the
   Mills service account → re-encrypt into SOPS). Tracked but not
   blocking.
4. **(C1)** Notify destination: Slack incoming webhook vs `agent_context`
   handoff inbox vs both.
5. **(C3)** Ranker model: gemma3:4b vs qwen3:8b vs flexinfer default —
   cost + latency comparison (`.loom/43` OQ5).

---

## Verification per phase (kill criteria)

- **A**: `autonomous_merges_24h ≥ 1` with a non-empty diff via
  harvester-vm; memo in `.loom/local/handoffs/`.
- **B**: cold boot ≤60s from curated image (B1); `Start` p50 ≤30s (B2);
  fault-injected Harvester failure routes to k8s with an audit event
  (B3).
- **C**: webhook fires on a real merge (C1); council proposes a
  signal-grounded item (C2); ranker decisions + one outcome round-trip
  logged (C3).
- **D**: `tests`-stage escalation < 30% over 7 days post-flip (D1); each
  v2 flip meets its `docs/MILLS.md` acceptance with no regression spike
  (D2).

---

## Sources

- North-star + autonomy plumbing: `.loom/43-plan-mills-autonomy-2026-05-24.md`
- Retro / empty-MR finding: `.loom/44-mills-autonomy-retro-2026-05-25.md`
- Spawn no-diff root cause: `.loom/119-diagnosis-mills-spawn-no-diff-2026-05-25.md`
- Substrate spec + slice status: `.loom/45-product-spec-mills-harvester-vm-substrate-2026-05-25.md`
- Kill-test evidence: `.loom/local/handoffs/mills-harvester-home-parity-killtest-2026-05-30.md`
- v2 swarm state + acceptance: `docs/MILLS.md` §"Mills v2", `.loom/93/94`
- Prod policy default-OFF + flip gate: `platform/gitops/k3s/mills/configmap-policy.yaml:94-109`
- Backend dispatch gap: `cmd/mcp-devbox/main.go` (no harvester-vm case)
- Spawn backend construction gate: Slice 2d note in spec §188 (`initSpawnOrchestrator` requires `SpawnHarvesterKubeconfig != ""`)
