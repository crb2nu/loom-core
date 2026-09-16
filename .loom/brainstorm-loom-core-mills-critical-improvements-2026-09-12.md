# Brainstorm: critical improvements and feature enhancements for loom-core and Mills

**Date**: 2026-09-12
**Triggered by**: Operator's open-ended `/brainstorm critical improvements and feature enhancements for loom-core and mills`. Fifth epic-selection brainstorm in the series (2026-08-12 taste-gated demand → 2026-08-13 immune system → 2026-08-25 MCP fleet campaign → 2026-08-29 factory reliability → 2026-09-10 J5 Finishing House). Asked the morning after a week in which the merge queue, the mrwatch shepherd, the runner cache, and the Harvester host all failed in turn, and the overseer foreman has flagged `escalation_storm` and `budget_burn` anomalies every day since 2026-09-06.
**Constraints noted**: none stated. Inferred from live state and standing feedback:

- One operator; attention is the scarce resource. Standing instruction: well-specified slices go through the Mills backlog, humans only see novel failures ([[feedback-route-work-through-mills]]).
- Pipeline budget is a policy cap: $75/day, 4 concurrent runs, 60 runs/day; council $50/day. Do not raise budgets when CI is saturated (pacing rule from the 09-10 incident).
- GitLab CE (no merge trains); ~12 runner slots; a loom-core pipeline is ~150 job-minutes; queue latency 60–90 min at peak.
- The plan-slice emitter has no dependency awareness; sequenced epics stay `draft` with staged enqueue.
- Operator grading policy: best-instance-per-theme = keep; duplicate re-proposals = regret; behavior-preserving refactors = meh.

## Grounding — what the numbers say today

All figures pulled live 2026-09-12 ~14:35Z from `GET /api/mills/{status,kpis,backlog,merge-queue,promotion-report,taste/aggregates}`, Prometheus `mills_*` over 14d, GitLab open MRs/issues, and `git log` over 30d on `main`.

### Yield and cost

| Signal | Value | Source |
|---|---|---|
| Escalation rate, last 24h | 82% (14 of 18 runs) — code 10, config 2, infra 2 | `/api/mills/kpis` |
| Real pipeline merges, last 24h | 3; external (hand-fed) merges 9 | `/api/mills/kpis` |
| Pipeline spend, last 24h | $65.31 of the $75 cap by 14:37Z; retries $17.58 (27%) | `/api/mills/status`, `/kpis` |
| Cost per merged pipeline, 24h | $21.77 | `/api/mills/kpis` |
| Escalations, 14d | 50 — code 30, infra 14, config 6; external_dependency/transient 0 | `mills_pipeline_escalation_class_total` |
| Stage errors, 14d | **tests 44**, ci_watch 15, implement 3, plan_slice 1 | `mills_pipeline_stage_error_class_total` |
| Stage attempts, 14d | tests 84, implement 53, plan_slice 46, research 44, pr_self_review 30, mr 22, ci_watch 17 | `mills_pipeline_stage_attempts_total` |
| Auto-requeues, 14d | 1 | `mills_auto_requeues_total` |
| p50 stage wall-clock, 14d | ci_watch 26m, merge 20m, tests 9m, implement 7m, pr_self_review 4m, plan_slice 3m, research 1m | `mills_pipeline_stage_duration_seconds` |
| Slice-to-merge p50 | 67 min (46 of them waiting on CI) | `/api/mills/kpis` |
| Merge-queue evictions, 14d | 45 — ci_red 23, head_moved 13, ci_timeout 8, rebase_conflict 1; merged 26 | `mills_mergequeue_*` |
| Pipeline spend, 14d | $144 | `mills_pipeline_cost_usd_total` |
| Judge discrimination (keep vs regret) | pr_self_review 0.006, spec_conformance 0.117 | `mills_judge_calibration_discrimination` |
| Gate evaluations, 14d | ~500, pass rate 95%, eval average 0.83 | `mills_gate_evaluations_total`, `/kpis` |
| Taste grade coverage, rolling 14d | 12.8% (gate for S5/S6 is 60%) | `mills_taste_overall_grade_coverage` |

Reading: **59 of 63 stage errors in 14 days happened in the verification substrate** (devbox `tests` + `ci_watch`), not in implementation. The judges cannot tell keep-graded work from regret-graded work. Retries eat a quarter of the budget, and the budget is gone by mid-afternoon.

### Demand — who feeds the mill and what it eats

| Signal | Value | Source |
|---|---|---|
| Backlog items (all time) | 1030 — retired 495, merged 491, escalated 42, queued 1, running 1 | `/api/mills/backlog` |
| Created by | plan-slice emitter 436, mrwatch shepherd 213, claude-code 135, merge-queue evictor 67, operator_manual 46, canary 29, council 15 | `/api/mills/backlog` |
| `mills-fabricated-suspect` items | 36 — all emitter-minted (slices citing files that do not exist) | `/api/mills/backlog` |
| Escalated now | 42 — 31 emitter-minted, 7 fabricated | `/api/mills/backlog` |
| Target repos | loom-core ~989; all other repos 41 (procmodel 9, flexdeck 8, fi-fhir 7, mcp-go 6, flexinfer 5, edilint 4, housemd 1, familyforge 1) | `/api/mills/backlog` |
| Merged items, 30d, by origin | emitter 161, claude-code 108, canary 17, api 12, operator 11 | `/api/mills/backlog` |
| Merge commits on main, 30d | 298 — council `psl-*` 74 (25%), operator-filed `bl-*` 85 (29%), canary 18, direct human/agent branches 126 (42%) | `git log --merges` |
| Grades on merged items (all time) | keep 63, meh 29, regret 62, ungraded 337 | `/api/mills/backlog` |

Reading: every one of the 37 council-minted items merged in the last 14 days is Mills hardening (CI classification, signature mining, soak reports, kill-test modes, spawn-file splits, audit-digest sweeps). The council re-proposes the same theme as `-1/-2/-3` siblings ("fail closed to lexical grounding" three times in a week, "split internal/hud/spawn.go" four times, "audit-advisory digest" five times), and the operator's grading policy calls duplicates regret. Product work in the last 30 days (fi-fhir, edilint, Finishing House, Cloth Hall, merge-queue proofs) came from the operator's own filings.

### Landing — the human is still the merge path

| Signal | Value | Source |
|---|---|---|
| Open MRs | 15 — 6 `[scope-escalated]` drafts awaiting widen-or-close, 5 with conflicts, 3 human-authored | GitLab |
| Escalation issues opened, 14d | 29 (265 open in total, label `mills-escalation`) | `mills_escalation_issues_created_total`, GitLab |
| External (hand-fed) merge-queue candidates | 213 shepherd placeholders + 67 evictor re-adoptions in the backlog | `/api/mills/backlog` |
| Ad-hoc operator recipes in memory | branch rescue, gate-reasons hand-finish, rebase + `-o ci.skip`, cancel same-sha duplicates, retry red pipelines + new idempotency key, state upsert after hand-merge | memory index |

Reading: the interventions-per-week north star proposed on 2026-08-29 was never instrumented; the hand-fed path (shepherd/evictor rows) is now the largest producer of backlog rows after the council.

### Platform (loom-core outside Mills)

| Signal | Value | Source |
|---|---|---|
| Engineering attention, 30d | pkg/mills + operator 46% of file touches; HUD 14%; MCP servers + daemon + CLI + agent-context 6% | `git log --name-only` |
| MCP servers | 71 binaries; 52 with zero commits since 2026-06-01; `loomd` last touched 2026-05-08 | platform survey |
| Gateway load | 13 clients connected; 2.6M messages relayed in 7d; Mills alone makes 201k hub calls in 7d | `mcp_gateway_*`, `mills_mcphub_calls_total` |
| Tool surface | `llm-core` profile cap 167, hand-bumped six times; comment-marked "SILENT-DISPLACEMENT HAZARD" (`cmd/loom/proxy_tool_filter.go:30`) | platform survey |
| Release hygiene | 682 unreleased `changelog.d` fragments (~50% Mills); `ci-lint` is warnings-only; CI coverage gate 35% vs 60.6% actual; `docs/IMPLEMENTATION_STATUS.md` "canonical" but dated 2026-04-14 | platform survey |
| Store | Mills `state.db` ~500MB; retention only fixed 2026-09-08; agent-context is Qdrant-only with unlimited worktree TTL | memory, platform survey |

Reading: the surface every agent (and every Mills spawn) talks through gets the least care, and nothing has been *released* in months — the factory ships merges, not versions.

## Phase 1 — Framings

The lazy default, named so it can be passed over: "harden Mills some more" — the council already proposes exactly that six times a day, and it is what the operator grades as regret. Also passed over: "add HUD panels" and "raise the daily budget".

### F1 — The weakest machine is the verification substrate, not the agents

Fifty-nine of sixty-three stage errors in 14 days are `tests` (devbox sandbox) and `ci_watch`. The sandbox has failed this month on lint-parity exit codes with zero findings, empty output tails, exit 137 on fresh checkouts, missing docker CLI, buildah caches never populated, a constant image tag for every repo, and TLS to Harbor. Each was fixed one at a time by hand, none with a drill. This framing says: stop treating those as incidents and make the tests stage a deterministic machine — one gate script shared byte-for-byte with CI, findings captured or the verdict is `infra`, a warm pinned sandbox per repo, and a daily fault drill that proves it. Only then does an escalation mean "the code is wrong".

- **Bet**: at least half of code-class escalations are substrate false-negatives; fixing the machine halves the escalation rate and the retry spend without touching a prompt.
- **Risk**: the substrate is a moving target (Longhorn, Harbor, runner cache, PVC attach) and every fix so far regressed silently; without the drill the work evaporates, and the first slices must be hand-finished because they run through the very stage they fix.

### F2 — Demand diet: the council must stop eating its own exhaust

436 of 1030 backlog items come from the plan-slice emitter, 36 of them cite files that do not exist, and every council item merged in the last two weeks hardens Mills. The mill is starved of *product* demand while gorging on meta-work it will later regret. This framing changes what the council is allowed to want: a hard share cap on items scoped under `pkg/mills`/`cmd/loom-mills-operator` (say 25%, exempting open incidents), roadmap intents read from every Tier-1 repo's `ROADMAP.md` rather than only loom-core's, GitLab-issue intake turned on for the satellite repos already onboarded (41 items proved the lane works), and a duplicate-theme brake keyed on the grading corpus (a theme with a `regret` sibling cannot be re-proposed for 30 days).

- **Bet**: the factory's marginal value is product work landing across the portfolio; the machinery is already good enough to carry it, and the meta-work loop is what keeps it idle.
- **Risk**: satellite repos re-open the wedge catalog (base images, CI variance, missing sandbox toolchains) against one operator, and product specs need richer materials than the council's briefs carry today — fabricated slices at product scale.

### F3 — Close the loop inside the run: attempt N+1 must read attempt N's verdicts

The single most reliable hand-finish recipe in memory is "the gate reasons are the fix list": read `/api/mills/pipeline/runs/{id}` `.gates[]`, apply exactly those reasons, push, done. Cloth Hall S2 escalated nine times across three runs with the *same* three spec-conformance reasons unaddressed; the editor never saw them. This framing wires the loop: every retry attempt is conditioned on the previous attempt's gate verdicts, lint findings, and CI job trace; scope-escalations with a shared-ancestor reach auto-widen once; a conflicted MR gets one autonomous rebase before it becomes a human's problem; a superseded sibling (same theme already merged) is retired, not rescued. The pieces exist and are not joined: gate verdicts persist per attempt (`pkg/mills/store/gate_verdicts.go`) while the runner rewinds to `RetryFrom` (`pkg/mills/pipeline/runner.go`) — whether the next spawn ever reads them is exactly what Cloth Hall's nine identical attempts suggest it does not; the scope gate already computes `shared-ancestor` admissibility for every rescue MR it drafts; and the rebase trigger is comment-marked "lands in a later slice behind a default-off policy flag" (`pkg/mills/pipeline/dispatcher.go:302`).

- **Bet**: most of what the operator does after an escalation is mechanical and already written down; automating three recipes cuts interventions by half and lifts autonomous merges without new models.
- **Risk**: auto-widening scope and auto-rebasing are exactly how a bad diff launders past review; some escalations are the correct outcome (the 7b SSE refusal), and a loop that resubmits forever converts escalations into budget burn.

### F4 — Retire the judges to advisory; make acceptance executable

The calibration metric says the pr_self_review judge scores keep and regret work within 0.006 of each other, and spec_conformance within 0.12, yet every run pays research + judge stages and ~500 gate evaluations a fortnight, with a 95% pass rate that predicts nothing. This is not a weak-model problem: since 2026-07-25 the `spec_conformance` and `pr_self_review` gates dial `oa/gpt-5.6-luna` through LiteLLM with a second-family tiebreaker (`platform/gitops/k3s/mills/deployment.yaml:159-175`, `pkg/mills/gates/llm_judge.go:255`), after a bake-off proved the previous fallback a rubber stamp that passed an absurd-spec mutant; `docs/weaver.md:33` still claims `qwen3-8b`, which is drift. A frontier judge with a rubric still cannot tell keep from regret, so the signal is the problem. This framing moves the quality signal to something that can be run: each backlog item ships with an executable acceptance (a failing test, a kill-test command, a golden), the tests stage runs it, and LLM verdicts become annotations that never block. The `spec-riskiest-assumption` discipline the operator already applies to specs is pushed down to slices.

- **Bet**: an executable acceptance criterion is the only judge that discriminates, and it doubles as the spec — items without one are the fabricated/duplicated ones anyway.
- **Risk**: authoring executable acceptance is the expensive half of spec-writing; the council cannot do it reliably today, so throughput drops until it learns; the calibration sample (graded runs) is small enough that the discrimination number could itself be noise; and demoting the judges removes the one gate that has stopped absurd specs in the past.

### F5 — The platform is the product; Mills is one customer

Six percent of engineering attention goes to the surface that relays 2.6 million messages a week to 13 clients and 201k Mills hub calls. The tool-profile cap has been hand-bumped six times with a comment-marked silent-displacement hazard; 52 of 71 servers are dormant; `loomd` has not changed since May; 682 changelog fragments have never been folded into a release; lint is soft and the coverage gate sits at 35 against 60. This framing spends the next epic on the platform every agent stands on: a tool-surface diet (per-agent profiles under ~80 tools with measured context cost), a monthly release train that folds fragments and tags, hard lint, dormant-server pruning or archival, and the agent-context store on a relational base for the tables that are not vectors.

- **Bet**: the highest-leverage improvement to every agent's work, Mills spawns included, is the tool surface and platform they use; it compounds across all repos rather than one factory.
- **Risk**: no KPI in the shift report moves for weeks; the council cannot self-propose this work (its demand is Mills exhaust), so it costs operator attention up front; pruning the wrong "dormant" server breaks a live consumer, as the SSE-removal premise did.

### F6 — Verification throughput: manage CI as the bottleneck instead of suffering it

Forty-six of the 67-minute p50 cycle is waiting on CI; the queue evicted 45 heads in 14 days; each rebase, speculation, and shepherd mint costs ~150 job-minutes on 12 slots; the budget clock and the runner queue both run out by mid-afternoon. Proof-by-tree and speculation shipped this week and helped. This framing goes further: an MR-scoped fast lane (test only the packages the diff reaches, full suite on main and on the merge commit), a queue-minted `MILLS_MERGE_QUEUE=1` lane on bigger runners (!1914 in flight), cancel-superseded-pipelines as a rule, and a budget model that charges CI minutes as well as dollars so the mill self-paces.

- **Bet**: doubling merges per day is cheaper through CI minutes than through model spend; the machinery is already half-built.
- **Risk**: partial test selection lets regressions reach main (regression rate is 0 today only because the full suite runs everywhere); bigger runner lanes shift cost to the Harvester host that reset three times last week.

### F7 — Subtract: shrink Mills to the machines that earn keeps

Twenty-nine subpackages under `pkg/mills`, twenty Mills docs, twenty-two incident runbooks (six of them overlapping external-dependency runbooks nobody links), Debate Mode parked at ~600 LOC, squads advisory-only, the pattern catalog covering 5% of demand, the workflow engine with one template, weaver auto-compose off everywhere. The code survey found the same shape inside: the workflow merge executor is "NOT wired … merging canaries will fail closed" (`cmd/loom-mills-operator/main.go:3606`), `load()` is "not implemented" (`pkg/mills/workflow/interp.go:282`) and the host's live effects are "the default stub" (`pkg/mills/workflow/host.go:233-311`); squad manifest fields are "parsed, persisted, and displayed but NOT yet enforced anywhere" (`pkg/mills/squads/types.go:14-19`); audit findings are advisory pending a v2.1 that has no date (`pkg/mills/audit/types.go:4-5`); item and council memory consolidation is "deliberately NOT wired" (`pkg/mills/store/dao_item_memory.go:31`, `dao_council_memory.go:38`); the Debate `Moderator` is an interface with no implementation (`pkg/mills/council/moderator.go:18`); and the ranker, tests-baseline oracle, and auto-revert all sit default-off (`pkg/mills/policy.go:645,655,753`). Forty percent of graded merges are regret. This framing deletes before it adds: every subsystem gets a keep/regret verdict against the grading corpus and the 30-day usage counters; unwired and regretted machinery is removed with its docs; the council's demand sources shrink accordingly.

- **Bet**: the maintenance tax on unused machinery is what the council's entire output currently services; a smaller mill fails less and proposes less meta-work.
- **Risk**: deleting the wrong thing (the "SSE unused" claim was false for the loom surface); the factory metaphor's machines are also the product story, and pruning it reads as retreat.

### F8 — An interventions ledger with a decision cockpit

The operator's touches are invisible: hand-fed enqueues show up as `mrwatch_shepherd` rows, widen-or-close decisions live in Draft MR descriptions, rebases and pipeline retries leave no trace, and 265 escalation issues sit open. This framing records every intervention with its cause (a `loom mills touch` verb the recipes call, plus automatic attribution from admin-API mutations and GitLab actions by the operator's identity), renders them as a weekly ledger, and turns the top recurring causes into the backlog — with the remaining human decisions (grade, widen, promote, adopt) batched into one queue with context cards.

- **Bet**: the factory cannot optimize what it cannot see; the ledger *is* the reliability backlog, and batching decisions makes each one several times cheaper.
- **Risk**: Goodhart — pressure to automate judgment calls that should stay human, or to stop touching the factory to keep the number pretty; one more derived surface to keep honest.

## Phase 2 — Cross-Pollinations & Tensions

### Combinations

- **C1 = F1 + F4 + F3 → executable verification, one loop.** The tests stage becomes the acceptance gate (the item's own kill-test runs there, findings captured, empty output classed `infra`); the judges are demoted to advisory annotations; and the retry attempt is conditioned on the previous attempt's captured findings. Neither alone gets there: F1 makes the sandbox honest but keeps paying for non-discriminating judges; F4 changes the signal but has nowhere reliable to run it; F3 feeds verdicts back but today's verdicts are noise. Together, "escalated" starts meaning "the code is wrong", and retries stop being $17/day of the same mistake.
- **C2 = F2 + F7 → the demand diet.** Cap the meta-work share and prune the unwired machinery in the same motion: the council's exhaust shrinks because there is less machinery to harden, and the freed capacity is pointed at satellite `ROADMAP.md` intents and issue intake. F2 without F7 leaves the council a maintenance tax to keep proposing; F7 without F2 leaves the mill idle.
- **C3 = F8 inside F3.** Every automated recipe lands with its ledger row: the intervention it retires is counted before and after, which is the promotion evidence the guard/overseer discipline already demands. Skip the standalone cockpit; ship the ledger as the measurement half of the night shift.

### Tensions

- **T1 = F6 vs F1/F4 — verification budget per merge.** More verification per run (executable acceptance, full-suite parity) costs CI minutes the queue does not have; faster lanes cost coverage. The real decision is the target cost of a merged bolt: roughly $2.50 and 30 minutes with partial verification, or $20 and 70 minutes with full parity. Today the mill pays the second price and gets the first quality.
- **T2 = F5 vs everything Mills — where the operator's one weekly brainstorm goes.** The council can only propose what it sees, and it sees Mills. Platform work needs the operator to author demand by hand, which is precisely the attention every Mills framing promises to save. Pick Mills first and the platform stays at 6%; pick the platform first and the shift report flatlines for a month.
- **T3 = F2 vs F7 — grow scope or shrink surface.** Both end Mills-on-Mills; one widens the mill to the portfolio, the other narrows it to what earns keeps. Doing both at once is a re-org; the order matters and is a taste call.

## Phase 3 — Convergence

### Recommended: C1 — executable verification, one loop (F1 + F4 + F3)

This wins on the arithmetic. Fifty-nine of sixty-three stage errors in a fortnight sit in the verification substrate; the judges that gate ~500 evaluations cannot separate keep from regret; a quarter of the daily budget is retries of the same unaddressed reasons; and the hand-finish recipe that works every time is "read the gate reasons and apply them" — which is a plumbing gap, not a research problem. C1 attacks the 82%/47% escalation rate directly, needs no new model spend, and every slice is measurable against numbers already exported (`stage_error_class_total{stage="tests"}`, retry cost, escalation class, judge discrimination). It is also the only framing that makes every *other* framing cheaper: demand diet, throughput lanes, and the night shift all assume an escalation is a true signal, and today it is not. Sequence: (1) the kill-test below, (2) sandbox honesty — shared gate script, captured findings, `infra` on empty output, per-repo warm pin, daily drill; (3) judges to advisory behind the existing policy flags — the judge is already a frontier model with a tiebreaker, so there is no cheaper model-swap experiment left to run first; (4) verdict feedback into retries; (5) per-item executable acceptance, council-authored, enforced at enqueue by the existing `fabricated_slice` gate's sibling. The first two slices are hand-finished by the operator by design, because they run through the stage they repair.

### Runner-up: C2 — the demand diet (F2 + F7)

C2 tips ahead if the kill-test shows the tests-stage failures are mostly *real* — fabricated or duplicated slices producing genuinely failing code. In that world the substrate is honest and the problem is what the council feeds it; capping the meta-work share, braking duplicate themes on the grading corpus, and pointing intake at the satellite roadmaps would raise keep-rate faster than any sandbox work. C2 is also the cheaper program (policy and pruning, little new code) and the one that most directly answers the standing frustration that the factory works on itself. Its cost is that it does nothing for yield per run, so a starved-but-honest mill would still burn a quarter of its budget on retries.

### Open question

**Is the next quarter's purpose "reliable on loom-core first" or "product across the portfolio now"?** C1 assumes the former (fix the machine where it lives, then widen); C2 assumes the latter (widen now, accept per-repo wedges). The answer also settles T2: if the portfolio is the goal, the platform framing (F5) becomes the epic after C2 rather than an indefinite deferral, because satellite repos consume the same proxy, profiles, and sandbox images.

## Riskiest assumption + kill-test

> Every brainstorm-derived plan must surface its riskiest load-bearing
> assumption explicitly. See the `spec-riskiest-assumption` skill.

**Load-bearing assumption**: The 44 `tests`-stage errors recorded in the last 14 days (`mills_pipeline_stage_error_class_total{stage="tests"}`) are predominantly devbox-sandbox faults — empty or unparseable gate output, checkout/infra failures, image or cache defects — rather than genuine test or lint failures in the agent's diff. If most are genuine, C1's sandbox slices repair a machine that is already honest and the runner-up (C2, demand quality) is the right program.

**Kill test** (≤30 min, one operator, read-only): on the operator pod, query the store in place — `sqlite3 /var/lib/loom-mills/state.db` — for every `stage_results` row with `stage='tests'`, non-success outcome, created in the last 14 days; pull each row's `artifacts_json` quality-gate output and `log_tail`, and classify: **(a) substrate** — empty `output_tail`, `exit=7` with `0 issues`, exit 137, `tests checkout infra`, image pull/TLS/cache errors; **(b) mechanical-real** — golangci findings the agent could have fixed (noctx, imports, staticcheck); **(c) genuine** — failing tests or build errors in the diff. Unambiguous outcome: **(a) ≥ 50% ⇒ C1 confirmed**; (c) ≥ 50% ⇒ C2 takes the epic slot and F1 shrinks to the empty-output classification fix already filed (`bl-mills-lintparity-empty-output-tail-20260910`); a plurality of (b) ⇒ F3 (verdict feedback) leads and sandbox work follows. Cross-check the same rows' `EscalationClass` — an (a) row classed `code` is the false-negative C1 exists to end. Disconfirming search before trusting the classification: grep operator logs (Loki, `app="loom-mills-operator"`) for `lint:parity` lines over the same window and confirm the store's `output_tail` matches what the devbox actually returned; the S1 J5 escalation showed the store dropping finding text the sandbox had produced.

**Failure mode if wrong**: two hand-finished slices of sandbox plumbing land, the escalation rate does not move because the diffs were genuinely broken, and the real lever (demand quality) waits another cycle while the budget keeps burning on retries.

**Status**: **RUN 2026-09-12 15:10Z — PASSED (C1 confirmed).** Method: the run-detail endpoint (`GET /api/mills/pipeline/runs/{id}`) serves terminal runs with per-attempt `LogTail` and a `checks` artifact carrying the devbox's own per-check `Output`/`ExitCode`, so the store was queried through the API rather than `sqlite3` on the pod. Corpus: 116 run ids (escalation issues opened since 2026-08-29 ∪ the 24h shift ledger; the ledger ignores `window_seconds`), 113 fetched, **59 runs with ≥1 errored tests attempt, 209 errored attempts**. Classifier: `scratchpad/classify2.py`, results in `kill_test_result.json`.

| Bucket | Attempts | Share |
|---|---|---|
| (a) substrate | 185 (+5 exec-in-container failures at the 4Gi limit first tagged unknown) | 88.5% → 91% |
| (b) mechanical-real (lint findings the agent could fix) | 13 | 6.2% |
| (c) genuine (test/build failure in the diff) | 6 | 2.9% |

Per run: 46 of 59 (78%) had *only* substrate failures; 10 (17%) had at least one real failure. Spend on runs whose every tests error was substrate: $104 of $152 (69%). **14 runs whose final attempt was substrate were classed `code`/`config`** — the false negative C1 exists to end. Retries after a real failure repeated the identical signature in 9 of 13 pairs (69%) — the F3 thesis, measured.

Substrate families by day (attempt start): `sandbox image still building` (47) + `buildah build timed out/watch closed` (27) + `golangci-lint v1/v2 config mismatch` (18) all stop after the 09-04/09-05 devbox fixes (fingerprint, buildah cache, tag-by-recipe) — **do not re-fix**. Live since 09-06: **`lint:parity` exit 3 "parallel golangci-lint is running"** (21 attempts, 09-06→09-12; concurrent runs share one sandbox pod and golangci-lint takes a file lock), `wait to replace non-running pod: wait pod gone` (10, 09-08→09-10), exit 137 / "could not start the exec process (container memory limit 4Gi)" on `pkg/mills` tests (~12), checkout failures (8), network/TLS (7). Disconfirming check: the `checks` artifact's `Output` matches the devbox's real message for every `lint_fail_no_findings` row (0 rows lacked it), so no Loki cross-check was needed (the Loki probe returned HTTP 400 anyway).

> Plan unblocked → `.loom/200-plan-executable-verification-2026-09-12.md`.

## Handoff

- If chosen → next step is: `plan-loom-core` — draft the C1 plan in the Plan Store under a non-emitter namespace (`mills/executable-verification`, phase `draft`) so nothing auto-enqueues before the kill-test; slices S1–S2 (sandbox honesty + drill) are operator-lane by design, S3–S5 route through the backlog as `bl-verify-s{3,4,5}-*` with explicit `Slices` and `Dependencies`.
- If C2 chosen instead → `plan-loom-core` for the demand-policy slices (meta-work share cap, duplicate-theme brake, satellite roadmap intents) plus a `tech-debt-planning` pass to produce the F7 prune list from the grading corpus and 30-day usage counters.
- Linked spec/plan doc (fill in once it exists): `<.loom/NNN-product-spec-executable-verification-...md>`
- Lineage: `.loom/brainstorm-factory-reliability-2026-08-29.md` (F1 work-conservation, F7 interventions north star — both still unbuilt), `.loom/brainstorm-next-features-unlock-2026-08-25.md` (F4 multi-repo, F5 cockpit, F7 shepherd graduation), `docs/FACTORY_MODEL.md` §4–§5 (junction sequence), `.loom/product-spec-mills-intake-2026-09-04.md` (repo onboarding surface C2 would lean on).

## Appendix — code-level gap inventory (surveys run 2026-09-12)

Two read-only surveys of this worktree at `ec73f9d1`. Cited so the framings above can be checked against the tree rather than remembered.

### Mills (`pkg/mills`, `cmd/loom-mills-operator`, `internal/spawn`, `internal/hud`)

| Theme | Marker | Where |
|---|---|---|
| Pipeline shape | `plan_slice → research → implement → post_implement_gate → tests(shell) → post_tests_gate → pr_self_review → post_review_gate → mr → post_mr_gate → ci_watch → post_ci_gate → merge → post_merge_gate → cleanup`; gates rewind to `RetryFrom`; `RetryPolicy{MaxAttempts:3, CooldownSeconds:300}` | `pkg/mills/pipeline/runner.go:62-111`, `pkg/mills/policy.go:1380-1382` |
| Judge gates | `spec_conformance` + `pr_self_review` on `post_review_gate`; unparseable → soft rewind; canary short-circuit; second-family tiebreaker | `pkg/mills/gates/llm_judge.go:14-38,255` |
| Workflow lane | merge executor not wired; `load()` not implemented; host effects stubbed; merge idempotency deferred to S6-full; `SubstrateK8sOnly` reserved | `cmd/loom-mills-operator/main.go:3606`, `pkg/mills/workflow/interp.go:282`, `host.go:233-311`, `runtime.go:41`, `pkg/mills/policy.go:226-236` |
| Squads | manifest `tests/gates/ensemble/budget_share/recursion_enabled` never enforced; workflow-lane runs record no squad outcome; `Fallback` reserved | `pkg/mills/squads/types.go:14-19`, `outcome_recorder.go:24`, `pkg/mills/policy.go:1414-1417` |
| Council / audit / overseers | audit advisory-only until v2.1; groomer priority promotions not implemented; triage judge may be unwired; council artifact/merge-diff loaders not wired; Debate `Moderator` interface only | `pkg/mills/audit/types.go:4-5`, `pkg/mills/overseer/groomer.go:490-493`, `triage.go:26-28`, `cmd/loom-mills-operator/handlers_audit.go:258,279`, `pkg/mills/council/moderator.go:18` |
| Store | item/council memory consolidation not wired and default-off; migrate-down not implemented | `pkg/mills/store/dao_item_memory.go:31`, `dao_council_memory.go:38`, `store.go:90,96`, `migrate.go:65` |
| Default-off policy sections | Squads, Debate, Recursion, AdaptivePolicy (`AutoApply` false), Overseer allow flags, Workflows, SpinningRoom, `RankerEnabled`, `TestsBaselineOracle`, `AutoRevertOnRegression`, `SpawnBreakerPolicy`, `NotifyPolicy` sinks, storage-evaluator blocking | `pkg/mills/policy.go:26-42,645,655,753,1029,1408-1539`, `policy_overseers.go:17-200`, `gates/storage_evaluator.go:140` |
| Dispatcher | rebase trigger "lands in a later slice behind a default-off policy flag"; stale comment says KPI writer not wired | `pkg/mills/pipeline/dispatcher.go:302`, `cmd/loom-mills-operator/capabilities.go:294` |
| Spawn substrate | Pod-based (not Job), per-agent-type generated Dockerfile, non-root `agent` user, vendors claude-code/codex/gemini, only codex takes `--model`; pod-metadata reconstruction lossy (`TimeoutMinutes == 0`); terminal-status predicate duplicated | `internal/devbox/backend/k8s_objects.go:13`, `internal/hud/spawn_runtime_image.go:19-50`, `internal/spawn/types.go:90-98`, `controller.go:1162-1165`, `pkg/mills/pipeline/STATES.md:171-213` |
| Taste consumers | ranker gate `MinimumTasteCoverage = 0.6`, `tasteRegretPenalty = 500`; squads blend weight 0.2; grade endpoint run-level only | `pkg/mills/ranker.go:18-80`, `pkg/mills/squads/router.go:214-226`, `pkg/mills/run_grade.go` |

### Platform (everything else)

| Theme | Finding | Where |
|---|---|---|
| Inventory | 78 binaries, 71 `mcp-*`; 52 servers with zero commits since 2026-06-01; `loomd` last touched 2026-05-08; 20 dormant `pkg/`, 7 dormant `internal/` (incl. a 45-file `tui`) | `cmd/`, `git log --since=2026-06-01` |
| CLI | 35 top-level `loom` commands; `agent` has 22 subcommands; no `hub` command (hub is a `sync --hub-mode` flag) | `cmd/loom/main.go:49-119`, `cmd_agent.go:49` |
| Proxy | three tool profiles, `llm-core` cap 167 hand-bumped six times, "SILENT-DISPLACEMENT HAZARD"; MR trailer fail-open; truncation 48KB/1.5MB/64KB; no Gemini transcript reader in the cross-vendor bridge | `cmd/loom/proxy_tool_filter.go:11-39,227,265`, `proxy_mrtrailer.go:15-21`, `proxy_truncate.go:17-25`, `pkg/vendorsessions/vendorsessions.go:26-28` |
| Agent-context | Qdrant-only (16+ collections); embed provider default flexinfer, morph retired and force-normalized; fail-closed degradation gate; session reaper 24h for "active" sessions; `WorktreeMaxTTLHours` 0 = unlimited; compaction audit log-only | `pkg/agentcontext/qdrant_registry.go:48-67`, `config.go:21-44,139-148,320-325`, `embed_degradation.go:12-32`, `compaction_audit.go:44` |
| Stubs | mentatlab DAG dispatch stub; ICC HMAC not implemented; loom-widget hard-coded placeholder and hand-rolled JSON-RPC; Streamable HTTP `GET /mcp` 405 (server-initiated messages not implemented); legacy tar-pipe sync + SPDY exec still shipped | `internal/spawn/mentatlab_adapter.go:21-30`, `internal/iccclient/client.go:7`, `cmd/mcp-loom-widget/main.go:8-12`, `docs/STREAMABLE_HTTP.md:186`, `internal/devbox/backend/k8s_sync.go:82`, `internal/sandbox/ws_exec.go:25` |
| Quality gates | `ci-lint` = `ci-lint-soft` (warnings only); `COVERAGE_THRESHOLD` 35 in CI vs 60.6% actual (comment admits the gate "could never fail" before a parsing fix); lint job 45-minute timeout; 37 `t.Skip` in `internal/integration`; 11 `allow_failure` jobs | `Makefile:661,669-690`, `.gitlab-ci.yml:169-179,517-569` |
| Docs | `docs/IMPLEMENTATION_STATUS.md` "canonical" but dated 2026-04-14; `docs/weaver.md:33` judge model stale; `docs/PLAN_STORE.md:66` dependencies parsed but not sequenced; `docs/JOURNAL_ENGINE.md:7` step 1 "deferred, not implementable as written"; 682 unreleased `changelog.d` fragments (~50% Mills) | `docs/` |
