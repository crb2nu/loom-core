# Loom skills: reliable delivery and behavioral evaluation sprint

- **Plan ID**: `plan-loom-skills-reliable-delivery-and-behavioral-evaluation-spri-04bf28`
- **Phase**: in_progress
- **Priority**: P1
- **Project**: services/loom-core
- **Namespace**: loom-core/skills-enhancement-2026-09
- **Created by**: codex-skills-research
- **Created**: 2026-09-13T14:39:54Z
- **Updated**: 2026-09-13T15:43:12Z

> Rendered from the Loom plan store (canonical). Edit via `agent_plan_*` tools, not this file.

## Success criteria

- Test: go test ./pkg/skills ./pkg/sync
- Test: make changelog-check
- Metric: Held-out selection precision >=90% and recall >=85% per measured host, provisional until S0
- Metric: Zero observed boundary violations; no critical outcome regressions
- Metric: Verified install identity and failed-copy/retired-file regression coverage
- Manual: Complete S0 host probe before downstream behavior rollout; review current/candidate evidence and two-host canary/rollback results.

## Phase history

| From | To | At | Actor | Note |
|---|---|---|---|---|
| draft | planned | 2026-09-13T15:32:48Z | codex-skills-sprint | User authorized implementation with Get started; executing locally from existing worktree. |
| planned | in_progress | 2026-09-13T15:32:49Z | codex-skills-sprint | Begin S0 host probe; prepare S1 conformance changes concurrently under feature-dev workflow. |

## Spec

## Objective and scope

Make the existing Loom skill library dependable at the point of use: the intended revision reaches the host, the right workflow activates, and completion is supported by observable evidence. The user authorized implementation on 2026-09-13. Begin with host feasibility, authoring conformance and delivery integrity; behavioral pilot rollout remains gated on measured evidence.

Research baseline: [Loom skills review, 2026-09-13](research-skills-enhancement-2026-09-13.md), checkout `96e63c4602f2984b08e528fc26ef46224b45b1ab`.

## Riskiest assumption + kill-test

**Load-bearing assumption:** Our supported Codex and Claude clients can run isolated current/candidate skill bundles with comparable task inputs, and expose enough trace/artifact evidence to distinguish skill selection from successful execution.

**Kill test:** In at most 30 minutes, record installed client/model versions and run one tiny local task plus one near-neighbor negative prompt on each host using temporary skill roots. Use a synthetic skill with a distinctive required artifact. Record which root/revision was loaded, the tool/file-read trace when available, output state, duration and available usage. Repeat once with the skill absent. Pass only if the output grader detects the missing artifact, the negative prompt remains in scope, and each host's loading evidence can be labeled observed, inferred or unavailable without pretending these are equivalent. Activation-based release metrics require observed loading on both hosts; otherwise restrict that metric to supported hosts and record the limitation before continuing.

**Failure mode:** We build a benchmark around forced or self-reported invocation and mistake harness/model differences for skill improvements.

**Status:** failed feasibility gate on 2026-09-13. Codex CLI 0.144.6 produced observed positive loading and a correct negative artifact, but the absent control read the neighboring positive receipt and copied it. Separate working directories did not isolate reads. Claude Code 2.1.205 reports loggedIn=false; no Claude model run was attempted. Actual Codex model identity was not exposed in retained JSONL. Evidence: `mcp/skills/_eval/baseline/2026-09-13.json` and normalized raw traces. Narrow this increment to deterministic S1/S2 repairs. Before S3 live comparisons, enforce read-isolated case environments, demonstrate a valid absent control, record model identity and restore Claude authentication. No behavioral pilot rollout or activation accuracy claim is supported by this probe.

**Positive evidence:** [Agent Skills evaluation guidance](https://agentskills.io/skill-creation/evaluating-skills) describes baseline comparisons; [Anthropic's eval guidance](https://www.anthropic.com/engineering/demystifying-evals-for-ai-agents) supports grading traces and outcomes.

**Disconfirming evidence:** [Vercel's Next.js experiment](https://vercel.com/blog/agents-md-outperforms-skills-in-our-agent-evals) found frequent non-invocation. Our session exposes duplicate skill names, and a schema can exist in the daemon without appearing in the client's direct tool list.

## Capacity and sequence

Assumption: two engineers, ten working days, fourteen engineer-days of implementation plus two for integration and unexpected host behavior. If staffed by one engineer, retain the order and plan approximately three weeks. Estimates are planning judgments, not measured commitments.

| Slice | Priority | Effort | Dependency | Intended result |
|---|---|---:|---|---|
| S0 — Baseline and host probe | P1 | 1 day | None | Observable benchmark boundary and reproducible baseline |
| S1 — Authoring and API conformance | P1 | 2 days | S0 | Validated schema, current examples and scaffold defaults |
| S2 — Reliable delivery and catalog identity | P1 | 3 days | S1 | Complete installs, correct pruning and useful collision diagnostics |
| S3 — Skill evaluation runner | P1 | 4 days | S0 | Separate routing and outcome comparisons with traceable results |
| S4 — Five pilot workflows | P1 | 3 days | S1, S2, S3 | Measured improvements to selected workflows |
| S5 — Release report and feedback loop | P2 | 1 day | S4 | Reproducible release evidence and regression intake |

Day 1: baseline. Days 2–6: conformance/delivery and evaluation workstreams. Days 7–9: pilot changes and live comparisons. Day 10: report, canary verification and release decision. Serialize edits to `skills-registry.yaml`; S3 owns its separate fixture tree. The feature-dev workflow now assigns independent conformance and delivery work to two workers; shared registry edits remain serialized.

## Pilot content and behavior

| Workflow pack | Pilot | Concrete enhancement | Observable acceptance |
|---|---|---|---|
| Research | `research` | Capture source dates, contrary evidence, local revision and unresolved assumptions; use a bounded fallback when an index/tool is unavailable | Claims link to supporting sources; no invented tool success; negative evidence is represented |
| Technical writing | `plan-loom-core` | Align executable examples with registered schemas; keep store and rendered mirror consistent; provide CLI fallback for tools not directly exposed | A fresh reader retrieves the same plan ID, dependencies and acceptance criteria; a plan-only prompt causes no implementation/landing |
| Testing and delivery | `small-change-loop`, `quality-gate-loop` | Select proportional checks; preserve prior authorization; separate quality evidence from permission; avoid unnecessary worktree/session ceremony | Small edit stays scoped; unrelated dirty file survives; failing proof prevents readiness claims |
| Troubleshooting | Boundary fixtures for `ci-failure-recovery`; `k8s-debug` content rollout deferred | Separate code failure, missing prerequisite and unavailable evidence; bounded retry and next diagnostic action | Broken environment is classified and recorded; no claim that the code passed because the command never ran |
| Coordination | `multi-agent-handoff` | Carry artifact revision, verified state, open decisions, next action and relevant context pointers | A fresh session resumes the intended slice, detects stale evidence and does not repeat completed mutation |

Do not require a model to follow one exact tool sequence when several valid routes produce the correct outcome. Enforce ordering only where it is part of correctness, such as verifying before claiming readiness or re-reading a revised record before retrying a write.

## Evaluation contract

Proposed files: `mcp/skills/_eval/` holds fixtures and a versioned result schema; `scripts/skills/` holds the first runner. Keep the initial runner an adapter over installed hosts. Reuse `pkg/llmusage` conventions and Mills reporting concepts without coupling skill tests to the council-specific `eval.Input`.

Each result records case ID, dataset version, source commit and bundle digest, host/client/model versions, permissions profile, observed skill loads, loading-evidence status, artifact pointers, grader version, assertion outcomes, duration, tool/retry counts, and available input/output/cached token usage. Unknown values stay null/unknown; they never become zero. Redact secrets from stored traces.

Two distinct datasets:

1. **Selection:** 40 realistic labeled prompts across the five pilots, including positive, negative and near-neighbor examples. Split into 20 development and 20 held-out cases before tuning. Allow expected supporting-skill sets rather than requiring a single name. Run three trials per case, per host, per revision. Report both per-host and pooled precision/recall, raw numerator/denominator, and uncertainty; repeated trials of one case are not independent new tasks.
2. **Outcomes:** ten task fixtures, two per pilot. Include dirty-worktree scope, missing tool, incomplete proof, stale handoff and planning-only boundaries. Compare current and candidate bundles on the same host/model/fixture three times. Check filesystem/API state with deterministic graders; use a separately calibrated reviewer only for semantic research/writing quality.

A four-case diagnostic subset also compares a no-skill baseline and a compact routing/index variant. Run it after the base comparison identifies a plausible selection problem. This is an experiment, not a mandate to migrate all hosts to a new loading strategy.

**Budget:** dry-run prints the exact run matrix and estimated cost where pricing is known. Require an explicit batch budget parameter before launching a paid batch; stop at that cap, record partial results, and count provider/infrastructure errors separately. A capped or skipped case is not a pass.

**Provisional release targets:** zero observed unauthorized/out-of-scope mutations or false readiness claims in boundary cases; selection precision at least 90% and recall at least 85% on the held-out set per measured host; no previously passing critical outcome lost. Candidate must improve at least one demonstrated failure class without increasing median successful-task tokens or duration by more than 10%, unless a recorded quality benefit justifies the trade-off. Set final thresholds after S0, before candidate tuning. Small samples provide a release signal, not a universal reliability estimate.

## Delivery and compatibility contract

Keep the registry as the authoring source for this sprint. Add only metadata needed for owner, source identity/digest, compatibility and evaluation references. Do not move all prose to a new registry format as a prerequisite.

All five primary generation targets receive deterministic structural/resource checks. Codex and Claude receive live pilot tests. Gemini's composite size is measured and its routing variant evaluated before demotion; Kilocode, Antigravity, Zed and OpenCode are labeled structurally checked or unverified as appropriate. `AllTargets` currently names five targets while Zed/OpenCode use separate paths; do not imply seven live certifications.

Extend existing manifest/prune behavior rather than replacing it. Missing required resource copies must fail without publishing a successful manifest. Retain the old destination manifest until upgrade completion. Delete only paths proven Loom-managed; unknown or modified ownership requires a diagnostic and preservation. Report retired names and normalized same-name conflicts across host-discovered roots.

A staged bundle digest supports rollback for the pilot. Existing manifests need backward-compatible reading and a clear migration path. Deterministic fixture validation must not accidentally rewrite the registry date or real home profiles.

## Non-goals and follow-on queue

- No new general agent runtime, skill compiler, automatic self-modifying skill loop or mandatory multi-agent architecture.
- No broad rewrite of all 80 skill descriptions and no automatic home cleanup during this first implementation increment.
- No replacement of plan-store, engrams, journalengine, llmusage or the Mills evaluation system.
- Follow-on: immutable SHA-resolved skills.sh imports and hosted content digests; organization-wide skill ownership/freshness policy; live certification of additional hosts; troubleshooting pilot expansion.
- Add imported-skill pinning to this sprint only by dropping an equivalent delivery item, not by silently expanding capacity.

## Validation, rollout and completion

During implementation, run focused `pkg/skills` and relevant `pkg/sync` tests first, generated-fixture conformance across targets, and the bounded live suite for pilot changes. Then run repository-required lint, full tests (or documented sandbox equivalent), contracts when API shapes change, docs guardrails and `make changelog-check` before landing. Use changelog fragments.

Release order: isolated output fixtures → temporary host profiles → one canary installation on each measured host → normal skill sync. Record source/digest before and after. Roll back by restoring the previous verified bundle/manifest, preserving unmanaged neighbors.

S5 is complete only when the release report links the exact source revision, installation evidence, raw case counts, failures, cost/latency comparisons, rollout/rollback evidence and unresolved host limitations. No green aggregate may hide a failed boundary case.

Integrate the result with `session-retro`: a repeated correction produces a candidate regression fixture and proposed skill patch; promotion follows the same checks. It does not directly rewrite installed guidance or enqueue implementation without the normal task scope.

## Decision log

- Build on the July consolidation; measure value before adding more skills.
- Repair delivery first because an improved source skill cannot help a client running an obsolete copy.
- Keep deterministic contracts and model behavior tests separate; both are needed.
- Prefer a small host adapter and artifact report over a new evaluation platform.
- Use a compact index experiment only where activation evidence supports it.

## Slices

### 1. S0 — Baseline and host probe — `implemented`

- **Slice ID**: `plan-loom-skills-reliable-delivery-and-behavioral-evaluation-spri-04bf28#1`
- **Goal**: Establish an observable and reproducible boundary for skill evaluations before behavior changes ship.
- **Files**: mcp/skills/_eval/baseline/, docs/SKILL_EVALUATION.md
- **Branch**: codex/skills-reliability-sprint
- **Acceptance**: Run the <=30-minute two-host kill-test; record client/model versions, fixture revision and observed/inferred/unavailable loading. Capture registry counts and installed discovery roots. Record pass/failure and any narrowed scope before downstream release.
- **Decision**: Completed the bounded feasibility experiment and recorded failure. Codex absent control copied ../positive/receipt.json; read-isolated cases are required. Claude unauthenticated; actual Codex model identity unknown. Plan narrowed to deterministic S1/S2 repairs; benchmark and pilot rollout remain gated.

### 2. S1 — Authoring and API conformance — `implemented`

- **Slice ID**: `plan-loom-skills-reliable-delivery-and-behavioral-evaluation-spri-04bf28#2`
- **Goal**: Catch invalid authoring and stale executable examples; align scaffold defaults and plan schema with implemented behavior.
- **Files**: pkg/skills/registry.go, pkg/skills/generator_validation.go, pkg/skills/registry_test.go, pkg/skills/generator_test.go, mcp/skills/loom-skill-builder/, mcp/context/skills-registry.yaml, cmd/mcp-agent-context/tools_plan.go, docs/PLAN_STORE.md
- **Branch**: codex/skills-reliability-sprint
- **Depends on**: plan-loom-skills-reliable-delivery-and-behavioral-evaluation-spri-04bf28#1
- **Acceptance**: Validate duplicate/invalid names, empty or oversize descriptions and unknown authoring fields/targets with actionable diagnostics. Claude scaffold defaults to a skill bundle. Plan examples use discoverable schema fields; deprecation prose is not falsely flagged as a live call. Source and installed CLI checks are explicitly distinguished.
- **Decision**: Proceed with deterministic repairs under the narrowed S0 scope; two-host behavioral rollout remains gated.
- **Decision**: S1 implemented. Strict registry authoring validation, Claude bundle scaffold, additive optional plan-create schema and executable example checks. Full pkg/skills and cmd/mcp-agent-context tests, source validation for 80 skills, scaffold tests and full-repo lint/tests passed with GOWORK=off and CGO_ENABLED=0.

### 3. S2 — Reliable delivery and catalog identity — `implementing`

- **Slice ID**: `plan-loom-skills-reliable-delivery-and-behavioral-evaluation-spri-04bf28#3`
- **Goal**: Ensure successful sync delivers complete current bundles and preserves enough ownership evidence to remove retired generated files safely.
- **Files**: pkg/skills/manifest.go, pkg/skills/manifest_test.go, pkg/skills/prune.go, pkg/skills/prune_test.go, pkg/skills/generator_bundle.go, pkg/skills/generator_codex_gemini.go, pkg/sync/ops_regen.go, pkg/sync/status.go, pkg/sync/ops_regen_test.go, pkg/sync/status_test.go
- **Branch**: codex/skills-reliability-sprint
- **Depends on**: plan-loom-skills-reliable-delivery-and-behavioral-evaluation-spri-04bf28#2
- **Acceptance**: Failed resource copies return errors and cannot publish success manifests. Destination old/new manifest fixture removes retired Loom-owned files while preserving custom/modified neighbors. Catalog diagnostics distinguish expected target differences from obsolete same-name copies. Source/bundle digest and prior verified state support pilot rollback; old manifests remain readable.
- **Decision**: Proceed with deterministic repairs under the narrowed S0 scope; two-host behavioral rollout remains gated.
- **Decision**: First increment implements fail-fast copies, hashed manifests, ownership-aware retirement, shared skill/profile sync and deferred repo cleanup. Integration review requires pre-write symlink guards, alias-safe cleanup and rejection of incomplete manifest objects. Catalog collision diagnostics and staged whole-bundle rollback remain open; keep this slice implementing.
- **Decision**: Resolved review findings with regressions: pre-write bundle symlink guards, os.SameFile cleanup alias protection, and rejection of incomplete manifest objects. Final affected-package tests and full repository tests pass; source path and verified v1-v2-v1 restore fixtures pass. Remaining catalog diagnostics and staged/canary rollback are outside increment 1.

### 4. S3 — Skill evaluation runner — `pending`

- **Slice ID**: `plan-loom-skills-reliable-delivery-and-behavioral-evaluation-spri-04bf28#4`
- **Goal**: Measure selection and task outcomes separately with controlled comparisons and attributable evidence.
- **Files**: mcp/skills/_eval/schema/, mcp/skills/_eval/cases/, scripts/skills/, docs/SKILL_EVALUATION.md
- **Depends on**: plan-loom-skills-reliable-delivery-and-behavioral-evaluation-spri-04bf28#1
- **Acceptance**: Versioned result schema, 40 split routing prompts and ten task fixtures; three trials per current/candidate arm per measured host. Distinguish observed/inferred/unavailable activation, capped/error/skipped runs and pass/fail. Dry-run run matrix, explicit paid-batch cap, raw counts and uncertainty. Four-case no-skill/index diagnostic is separately reported.

### 5. S4 — Five pilot workflows — `pending`

- **Slice ID**: `plan-loom-skills-reliable-delivery-and-behavioral-evaluation-spri-04bf28#5`
- **Goal**: Improve research, planning, proportional verification and handoff behavior using measured failures.
- **Files**: mcp/context/skills-registry.yaml, mcp/skills/research/, mcp/skills/plan-loom-core/, mcp/skills/small-change-loop/, mcp/skills/quality-gate-loop/, mcp/skills/multi-agent-handoff/, mcp/skills/_shared/
- **Depends on**: plan-loom-skills-reliable-delivery-and-behavioral-evaluation-spri-04bf28#2, plan-loom-skills-reliable-delivery-and-behavioral-evaluation-spri-04bf28#3, plan-loom-skills-reliable-delivery-and-behavioral-evaluation-spri-04bf28#4
- **Acceptance**: Pilot examples and supporting resources resolve from delivered bundles. Planning-only and dirty-worktree boundaries hold; missing proof prevents readiness; handoffs carry revision/proof/next action. Current/candidate report meets predeclared targets or documents failures without rollout. Compact-index and Gemini loading changes remain experiments until their own host evidence passes.

### 6. S5 — Release report and feedback loop — `pending`

- **Slice ID**: `plan-loom-skills-reliable-delivery-and-behavioral-evaluation-spri-04bf28#6`
- **Goal**: Publish reviewable release evidence and turn recurring corrections into regression candidates.
- **Files**: docs/SKILL_EVALUATION.md, mcp/skills/session-retro/, mcp/context/skills-registry.yaml, .gitlab-ci.yml, changelog.d/skills-evaluation-sprint.changed.md
- **Depends on**: plan-loom-skills-reliable-delivery-and-behavioral-evaluation-spri-04bf28#5
- **Acceptance**: Release report identifies revision/digests, host coverage, raw results, boundary failures, cost/latency and canary/rollback evidence. CI runs deterministic conformance without paid evals on unrelated changes. Retrospective guidance proposes fixtures and patches instead of directly changing installed skills. Required repo checks pass before landing.
