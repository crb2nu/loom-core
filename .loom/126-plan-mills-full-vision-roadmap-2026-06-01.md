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

> ## ⟳ STATUS RECONCILED — 2026-06-26 (supersedes the "Current value: 0" snapshot below)
>
> **The north-star has ticked. The empty-MR gap is CLOSED.** Verified live
> against the persisted operator backlog (`GET /api/mills/backlog`, port 8090)
> and the GitLab MR record: the mills bot has autonomously authored **and
> merged** non-empty MRs **fully unattended** (`merge_user` = pipeline service
> account `project_47_bot_*`, i.e. `merged_via=auto`, no human in the loop):
>
> | Date | MR(s) | Change | Diff | merge_user |
> |------|-------|--------|------|-----------|
> | 2026-06-24 | [!774](https://gitlab.flexinfer.ai/services/loom-core/-/merge_requests/774), [!779](https://gitlab.flexinfer.ai/services/loom-core/-/merge_requests/779), [!780](https://gitlab.flexinfer.ai/services/loom-core/-/merge_requests/780) | `testdata/mills-canary/heartbeat.md` | non-empty | **bot** (unattended) |
> | 2026-06-25 | [!788](https://gitlab.flexinfer.ai/services/loom-core/-/merge_requests/788) | **`pkg/mills/tick_outcome_label_test.go`** — a real Go test, not a fixture | non-empty | **bot** (unattended) |
>
> Preceding bot-authored canary MRs on 2026-06-22 ([!768](https://gitlab.flexinfer.ai/services/loom-core/-/merge_requests/768)) and
> 2026-06-23 ([!770](https://gitlab.flexinfer.ai/services/loom-core/-/merge_requests/770)) merged with `merge_user=root` —
> human-**assisted**, during the operator-hardening push.
>
> **First fully-unattended merge: !774 @ 2026-06-24T01:00Z. First unattended
> _real-code_ merge: !788 @ 2026-06-25.** ⇒ the load-bearing assumption below
> ("a real canary produces a non-empty MR that auto-merges unattended") is
> **PROVEN** — but it was closed on the **default (k8s) substrate** via the
> spawn-execution + pipeline-orchestration fix chain
> (!762/!764/!766/!769/!773/!777/!790/!791), **not** via the harvester-vm bet
> this plan was organized around. The harvester-vm path remains a separate,
> still-open hardening track (see ROADMAP.md "Mills harvester-vm substrate",
> Slice 2 acceptance still pending).
>
> **Phase status reconciled**: **A2 (first autonomous merge) PASSED 2026-06-24.**
> **A3 (sustain ≥7 consecutive green/day) NOT yet met** — cadence was 06-24 ×3,
> 06-25 ×1, 06-26 ×0 (intermittent; one debt item escalated:
> `MILLS-DEBT-TICKLABEL-20260624-191214`, retried green 06-25 as !788). **The
> frontier is now A3 reliability hardening, not A1/A2.**
>
> _Caveat for future loops_: the Prometheus counter
> `mills_pipeline_runs_total{state="done"}` reads **0** — but only because the
> operator pod resets it on every roll (last roll 2026-06-26T13:51Z). The
> persisted backlog + the GitLab MR record (above) are the authoritative
> source; do **not** trust the live counter alone for the north-star.

## North-star metric (unchanged from `.loom/43`)

```
autonomous_merges_24h = count(pipeline_runs WHERE
  terminal_state = "merged"
  AND attempts_total = 1
  AND escalated = false
  AND merged_via = "auto"
  AND last_24h)
```

**Current value (as written 2026-06-01 — now SUPERSEDED, see the STATUS
RECONCILED banner above): 0.** It had been 0 every day since at least
2026-05-17 (`.loom/44` §kill-test: 0 of 56 runs ever reached `done`). **As of
2026-06-24 the metric ticks ≥1 on merge days** (06-24 ×3, 06-25 ×1; 06-26 ×0
so far). Every other metric (auto_merge_rate, escalation_rate, slice-to-merge
p50) is a drag term on this one. This plan is organized strictly by leverage on
this number.

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

1. **No mills canary has ever merged through harvester-vm.** _(Reconciled
   2026-06-26: still true for **harvester-vm specifically** — but canaries DO
   now merge unattended on the **default k8s substrate** as of 2026-06-24; see
   the STATUS RECONCILED banner. The harvester-vm path itself remains
   unproven.)_ Prod policy
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

**Status**: **FAILED 2026-06-02** — empty diff. North-star still 0.
Slice A1 wiring DONE 2026-06-01
(`.loom/local/handoffs/mills-harvester-vm-slice-a1-canary-2026-06-01.md`).
A2 ran live (`PIPE-MILLS-CANARY-20260602-000708`, agent=codex) and reached the
`mr` stage, but the substrate fix did **not** close the gap: codex executed
**`turn_count=0`** on the VM (`spawn-ae6f26eb2085`: execute→complete in 0.3s;
`spawn-bb92dc191161`: reaped `failed`), so the branch was empty (0 commits, MR
`loom-core!598` closed). The failure is the `.loom/44` **agent-execution gap**
reproduced on the VM path — NOT substrate routing or scoped-SA creds (those
worked: VMs booted, zero forbidden, cascade-cleaned). Worktree-visibility is
moot when the agent makes no changes. Evidence + next steps:
`.loom/local/handoffs/mills-harvester-vm-slice-a2-killtest-2026-06-01.md`.
**Phases B/C/D remain correctly gated.** Next investigation is the
spawn-execution path (reopen `.loom/119` against the VM path), not the
substrate plumbing.

**Update 2026-06-05 — A2 re-run is now code-ready (live run not yet done).**
All three agent-execution blockers from the 06-02 + 06-04 attempts have
landed on `main` with regression tests, each peeling back one layer of the
codex-on-VM execution chain:
1. **Workspace + CLI absent on the stock VM** → `b4a0485d`
   (`fix(mills): provision workspace + agent CLI on harvester-vm at Start`).
   A direct 06-04 canary exited **127 in 176ms** with
   `cd: /workspace/...: No such file or directory` + `codex: command not
   found`; `HarvesterVMBackend.Start` now git-clones the repo into the
   WorkDir and runs a guarded, pinned agent-CLI install over SSH before the
   agent exec. Tests: `harvester_vm_provision_test.go`, `spawn_cli_install_test.go`.
2. **codex stdin hang** → `75a89996`
   (`fix(spawn): codex exec reads from /dev/null to avoid stdin hang`).
   After (1), the 06-04 re-run `spawn-ced0192f6540` got *further*: codex
   had the CLI + workspace, **authenticated, and emitted
   `thread.started`/`turn.started`** — then printed `Reading additional
   input from stdin...` on stderr and **exit 1** (codex 0.120.0+ inspects
   stdin even with a prompt arg; the harvester-vm SSH session leaves
   `session.Stdin` nil). `buildAgentCommand` now appends `< /dev/null`; the
   redirect binds to the final `codex exec` simple command through the VM's
   login shell (verified: harvester-vm takes the buffered `be.Exec` path →
   `execOverSSH` → `session.Run`). Test: `TestBuildAgentCommand`.
3. **Telemetry-persist blind spot** (`session_id: is required`) → `5fc4dd75`
   (`fix(hud): persist spawn telemetry under the spawn's session_id`). Spawn
   turn detail is now captured under a resolvable session, so the next
   canary debug is not blind. Test: `spawn_telemetry_persist_test.go`.

Plus the defensive backstop `4bb853a7` (`nonempty_diff` gate, `.loom/128`):
if codex *still* produces no diff, the run **escalates instead of opening a
0-commit MR** — the `!598`/`!518`/`!520`/`!522` false-positive can no longer
recur. **Residual risk (live-only):** whether codex, now able to run a full
turn, produces a *non-empty* diff for the canary task — the genuine A2
question, unprovable in code. The `< /dev/null` fix has **never been
exercised live**. A2 re-run readiness + the live procedure:
`.loom/129-iteration-plan-mills-a2-rerun-readiness-2026-06-05.md`. **Status:
READY FOR LIVE RE-RUN (human-gated; flips prod `stage_substrate`).**

**Update 2026-06-06 — A2 live re-run RAN and FAILED on a NEW blocker (codex
model access). North-star still 0.** The re-run executed end-to-end
(`gitops!230` flip → canary `MILLS-CANARY-A2-20260606-011720` on codex →
`gitops!231` revert; window ≈6 min, system restored clean — no orphaned
VMs/pods, no junk MR). The four verified fixes all **worked**: codex had the
CLI + workspace, did not hang on stdin, authenticated (OAuth `chatgpt`), and
**started a turn**. It then failed at the *first* spawn (`plan_slice`, on
k8s — `implement` never reached a VM) with **HTTP 400: "The 'gpt-5.3-codex'
model is not supported when using Codex with a ChatGPT account."** Root
cause: `buildAgentCommand` runs `codex exec` with **no `--model`**, so codex
defaults to `gpt-5.3-codex`, which the mounted ChatGPT-account OAuth does not
grant. Substrate-independent. The `nonempty_diff` gate **escalated cleanly —
no 0-commit MR** (the `!598` failure mode is now genuinely closed). Next
fix (small, needs one live re-run): **run the first canary on `gemini`**
(clean auth — recommended) or **pin a ChatGPT-supported `--model`** on the
codex exec. Evidence:
`.loom/local/handoffs/mills-harvester-vm-slice-a2-killtest-2026-06-05.md`.
**Phases B/C/D remain correctly gated.**

> **✅ RESOLVED 2026-06-24 — kill-test PASSED** (supersedes the 06-02/06-06
> FAILED status above). The first unattended merge closed the loop on the
> **default k8s substrate** via the spawn-execution + pipeline-orchestration fix
> chain (`resolveCodexModel` pinned a supported model; `!762`…`!791`) — **not**
> via the harvester-vm path this section's kill-test was scoped to. The gating
> rule below is satisfied; the Phase A/B/C/D framing it protected is now
> superseded by the re-sequenced **Next waves** section. Original note, kept for
> lineage:
>
> > This kill-test gates the entire rest of the plan. Phases B, C, D do not
> > start until Phase A produces one real autonomous merge.

---

## Next waves (re-sequenced 2026-06-26 — supersedes "The phased roadmap" below)

> **Why re-sequenced:** the STATUS RECONCILED banner records that A2 passed —
> but on the **default k8s substrate**, not the harvester-vm bet that Phase
> A→B→D1 below was organized around. The original critical path (*wire
> harvester-vm → flip defaults to harvester-vm*) is therefore obsolete. The loop
> works on k8s; the job now is to make it **reliable** (Wave 1), **observable**
> (Wave 2), then **scale demand/quality and repos** (Waves 3–4). Harvester-vm
> drops to an optional, non-blocking hardening track.

**Done — no longer the frontier:** A1 (substrate wiring), A2 (first unattended
merge on k8s, 2026-06-24). **Plan Store unification (S7b)** — Mills backlog items
now born-link to first-class Plans via `agent_plan_create` over the hub —
**closed out + live-verified 2026-06-26**
([gitops!291](https://gitlab.flexinfer.ai/platform/gitops/-/merge_requests/291);
loom-core `.loom/161`).

### Wave 1 — A3 sustain: make the k8s loop reliable 🔴 CURRENT FRONTIER
The loop merges intermittently (06-24 ×3, 06-25 ×1, 06-26 ×0) with occasional
escalations. Wave 1 carries the 06-19→06-25 reliability-fix cadence to a
**7-consecutive-green-day** bar (the original A3 gate).
- **W1.1 — Durable north-star metric (do FIRST).** `mills_pipeline_runs_total{state="done"}`
  resets to 0 on every operator roll — the counter-trap that mis-framed this
  doc as 0/56. Persist it (DB-backed gauge, or recompute from backlog
  `State=merged` + GitLab `merge_user=bot`) so A3 is measurable without manually
  cross-checking two sources. **Done when:** a Grafana panel shows an
  `autonomous_merges_24h` that survives an operator restart.
- **W1.2 — Escalation burndown.** Each live escalation → one regression-tested
  fix (continuing merge-stage-405 recovery, zombie-spawn watchdog, scope-gate,
  stalled-spawn re-spawn, pipeline-race deflake). **Done when:** a 7-day window
  shows ≥7 unattended merges, 0 regressions, escalation rate trending down.
  - **2026-06-27 — root cause of the intermittent cadence: no demand source.**
    A live audit found the loop merges 0/day whenever nobody hand-runs
    `loom mills pipelines canary`: the operator boots council (6h), eval
    (Sunday), and the GitLab importer (5m, 0 eligible) schedulers, but **no
    canary scheduler** — `pkg/mills/workflow/scheduler_min.go` explicitly notes
    canaries launch only via the manual admin path. So 06-26 ×0 and 06-27-start
    ×0 weren't a *reliability* regression; the loop was simply idle (queue
    depth 0). A live canary kicked on today's HEAD (`e3de78b9`) confirmed the
    loop is **green** through every spawn stage (implement non-empty diff,
    tests, pr_self_review) + all gates + opened MR + ci_watch
    (`PIPE-MILLS-CANARY-20260627-172429-…`).
  - **Shipped: daily canary-autopilot scheduler (default-off).**
    `mills.CanaryScheduler` + `policy.intake.canary_autopilot` enqueue+start one
    deterministic heartbeat canary per `schedule_cron` (default daily 09:00 UTC),
    honoring the 24h dedupe — so `autonomous_merges_24h` ticks ≥1/day unattended
    once enabled. Default-OFF; **flip sequence: merge → operator image builds +
    deploys → gitops `configmap-policy.yaml` set `intake.canary_autopilot.enabled:
    true`.** This is the automation that makes the 7-green-day bar reachable
    without a daily human poke (the dedupe means it never piles on a real-work
    merge day). Code/tests: `pkg/mills/canary_scheduler{,_test}.go`,
    `pkg/mills/store/canary_item{,_test}.go`.

### Wave 2 — Observability & notify (parallel with W1; cheap, high-ratio)
- **W2.1 — Notify webhook (`.loom/43` 3a; code complete, inert).** Prod policy
  `notify.webhook_url` is still `""`. Set it (Slack incoming webhook or the
  `agent_context` handoff inbox). **Done when:** a real unattended merge posts a
  webhook within 30s.
- **W2.2 — Autonomy dashboard** on W1.1's durable metric: merges/day, escalation
  rate, slice→merge p50, broken out by repo + agent.

### Wave 3 — Demand & quality (old Phase C; substrate-agnostic, still valid)
- **W3.1 — Workspace-signals council brief (`.loom/43` 1b):** feed last-24h Loki
  errors + GitLab CI failure clusters into the council brief so it proposes items
  grounded in real workspace pain instead of synthetic canaries.
- **W3.2 — LLM-ranked dispatch + outcome feedback (`.loom/43` 1c+3c):** rank
  queued items by expected merge probability; FIFO fallback on ranker outage;
  outcome writeback the ranker reads.

### Wave 4 — Scale across repos (the v2 swarm flips, now Plan-backed)
Prod policy state: **squads ON, audit advisory ON, debate incident-only ON**
(Phase-8.3 steps 1–3 done); **cross_repo OFF, adaptive_policy OFF** (steps 4–5
pending). S7b makes work units repo-addressed Plans, so cross-repo is now
tractable.
- **W4.1 — `cross_repo.enabled`** flip after ≥3 dogfood successes, one repo at a
  time, `MILLS_V2_ROLLBACK.md` ready.
- **W4.2 — `audit.advisory_only: false`** once audit survival rate >0.85.
- **W4.3 — `adaptive_policy`** manual-apply. Each flip: one/week, soak + rollback,
  per `docs/MILLS.md` §"v2 acceptance".

### Decoupled track — harvester-vm substrate (NO LONGER critical path)
The empty-MR gap closed on k8s, so harvester-vm is now **optional** hardening
(stronger isolation / heavier workloads), not a gate. Slice 2 acceptance remains
open (ROADMAP.md "Mills harvester-vm substrate"). **Reversed from the original
plan:** old **Phase B1/B2** (curated base image, warm pool) and **Phase D1**
(flip defaults *to* harvester-vm) are **demoted / cancelled** — k8s is the
working default; do not flip away from it. Resume this track only if a concrete
k8s isolation limit actually bites.

### Sequencing (re-sequenced)
```
Wave 1 (A3 reliability) ── W1.1 durable metric ─┬─► W1.2 escalation burndown ─► 7 green days
                                                 └─► Wave 2 (notify + dashboard) ‖ parallel
   │ 7 green days
   ▼
Wave 3 (demand/quality)  ─────────────────────────►  Wave 4 (cross_repo → adaptive flips)
Harvester-vm: optional, off the critical path — no wave depends on it.
```

---

## The phased roadmap (2026-06-01 original — SUPERSEDED by "Next waves" above; kept for lineage)

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
