# Brainstorm — VMs-for-agents lessons applied to devbox, mills, flexinfer

- **Date**: 2026-05-25
- **Author**: claude-code (Opus 4.7)
- **Trigger**: User asked "what can we learn / be inspired by" from
  [agents need VMs, not containers (YT, 57min)](https://www.youtube.com/watch?v=1GX18UGoJRw) —
  David Crawshaw interview on exe.dev (purpose-built cloud for AI agents).
- **Goal**: surface multiple framings for how the video's claims should
  reshape `devbox`, `mills`, `flexinfer`; converge on the highest-leverage
  next slice.
- **Status**: kill-test passed → spec 45 spawned → Slice 0 + Slice 1.5
  shipped same day. See related handoff memos in `.loom/local/handoffs/`.

## Source distillation — Crawshaw's load-bearing claims

1. **Containers are the wrong unit of isolation for agents.** Every customer
   test broke Sketch's container model — DinD, k3s-in-pod, root-required
   tests, network-interface tests. Agents need VMs.
2. **Agent-friendly == developer-friendly.** Models trained on developer
   transcripts. Cloud-API ceremony eats context-window intelligence.
3. **Marginal cost of starting a VM should be zero.** Buy a CPU/RAM pool;
   slice arbitrary VMs from it. 10 ideas → 10 VMs.
4. **Dev-as-prod is back** with live agents. Set of software you want to
   develop in-place is no longer zero.
5. **Batteries-included edges by default**: TLS, IAM proxy, browser-SSO.
6. **Test as fast as possible**: exe.dev's main suite spins up ~thousands
   of VMs in 100s per push.
7. **Local NVMe + RAID-5 hot-swap > remote disk.** Laptop = 500k IOPS,
   EC2-remote = 200k IOPS at $20k/mo.
8. **Token efficiency is the dominant currency** — not dollars, not CPUs.

## Phase 1 — Diverge (8 framings)

- **F1.** Microvm devbox (Firecracker / Kata on harvester). Replaces
  container sandbox with real VMs.
- **F2.** Marginal-zero VM pool for mills runs (per-council-run VMs).
- **F3.** Dev-as-prod long-running VMs for HUD / mills artifacts.
- **F4.** NVMe-local + RAID-5 storage for flexinfer model weights.
- **F5.** Token-efficient MCP surface audit across devbox/mills/flexinfer.
- **F6.** Auto-batteries edges (TLS + IAM proxy + browser SSO) for every
  spawned env.
- **F7.** 100-second sharded test suite for loom-core itself.
- **F8.** Anti-anchor: don't build a cloud, buy exe.dev. Strip ~30% of
  platform maintenance.

(Each framing was developed with one paragraph + Bet + Risk in the original
draft. The convergence below is what survived re-prioritization after the
Slice 0 kill-test.)

## Phase 2 — Cross-pollinate

- **Combo A**: F1 + F6 — microvm + auto-batteries belong together (shared
  networking primitive).
- **Combo B**: F2 + F7 — per-run mills VMs + fast sharded tests collapse
  council cycle time minutes → seconds.
- **Tension**: F1 (build) vs F8 (buy exe.dev) is the actual build-vs-buy
  fork. The decision axis is "is isolation our core competency?"
- **Tension**: F3 (dev-as-prod) ↔ workspace GitOps-everywhere policy.

## Phase 3 — Converge

**Recommended**: Combo B (per-run mills VMs + 100s sharded tests) with F5
(MCP token audit) as a zero-risk precursor.

**Runner-up**: F1 + F6 (microvm devbox + auto-batteries edges) if the
F1-vs-F8 fork lands on "build."

**Riskiest assumption + kill-test (PASSED 2026-05-25)**: Mills' container
sandbox is meaningfully limiting agent capability today. Threshold: >8/20
historical runs had container-attributable failures.

## Kill-test results (Slice 0 — PASSED)

Reused 14d production telemetry from
`.loom/local/handoffs/mills-autonomy-killtest-2026-05-24.md` — 56 escalated
pipeline_runs, 125 classified failing stage_results.

| evidence | result |
|---|---|
| Strict container-attributable (buildah-DinD + pod-GC + dockerfile-gen) | 54/125 events (43%) |
| Including MCP-on-pod-restart cluster | 89/125 events (71%) |
| **Runs escalated at buildah/devbox infra stages (plan_slice or tests)** | **46/56 → ~16/20** |
| Threshold for "Combo B decisively justified" | >8/20 |

**Plot twist**: Mills isn't hitting "agent skipped a test" — it's hitting
"container infra failed before the agent could run at all." Even stronger
case for VMs.

**Plus structural bug** (MR !523, merged 2026-05-25): operator's worktree
path doesn't cross pod boundary; every `implement` spawn writes diff into
pod-local clone the operator never sees. That class of bug has no foothold
in a VM-as-dev-environment model.

## Decision: take the build (F1) path

User chose "def our harvester setup" after the brainstorm presented the
F1-vs-F8 fork. Spawned `.loom/45-product-spec-mills-harvester-vm-substrate-2026-05-25.md`
to design implementation.

## Lineage / outcomes from this brainstorm

- **Spec**: `.loom/45-product-spec-mills-harvester-vm-substrate-2026-05-25.md`
- **Slice 0 kill-test memo** (PASS):
  `.loom/local/handoffs/mills-harvester-vm-killtest-2026-05-25.md`
- **Slice 1.5 base-VM memo** (PROVISIONAL PASS):
  `.loom/local/handoffs/mills-harvester-vm-slice15-2026-05-25.md`
- **Platform MRs landed today**:
  - !178 — fix Whereabouts CIDR exclude parse error
  - !179 — deprecate `default/lan10g-whereabouts` NAD entirely
    (overlapped LAN gateway, allocated `.1` to a test VM)
- **F5 (MCP token audit)** — still queued, zero-risk parallel work
- **F3, F4, F7, F8** — explicitly NOT pursued; documented as non-goals in
  spec 45.
