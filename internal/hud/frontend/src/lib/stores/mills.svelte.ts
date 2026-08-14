// Mills store — backlog / pipeline runs / council runs / eval scores from
// the in-cluster loom-mills-operator, proxied through /api/mills/* by the
// HUD's domain/mills package. Each panel owns a slice of this store and
// polls the corresponding read endpoint at 15s.
//
// Empty/disabled state: when the proxy returns 503 ("operator not
// configured") we surface that via the `disabled` flag so panels can
// render a clear "Mills disabled" empty-state instead of a fetch error.

export interface BacklogItem {
  ID: string;
  Title: string;
  State: string;
  Priority: string;
  Labels?: string[];
  CreatedBy?: string;
  CreatedAt?: string;
  UpdatedAt?: string;
  // The operator's list endpoint serializes the full untagged
  // store.BacklogItem, so PlanID is on the wire for every row. Stamped
  // plans embed their pattern slug (`plan-stamp-<slug>-<primary>`) —
  // the Factory pattern shelf derives run→book attribution from it.
  PlanID?: string;
  // TargetProject is on the wire (untagged store.BacklogItem) and routes MR
  // links for runs belonging to repositories other than loom-core.
  TargetProject?: string;
  // Taste signals are stored on the backlog item by the grade endpoint. The
  // terminal-run endpoint returns bare PipelineRun records, so archive fetches
  // project these fields onto their runs for reload persistence.
  Grade?: BoltGrade;
  GradeNote?: string;
}

// BacklogItemDetail is the full item returned by GET /api/mills/backlog/{id}
// (operator handleBacklogGet → writeJSON(item)). The list endpoint omits
// these heavier fields, so they're all optional on the shared base. Nested
// shapes use the operator's snake_case json tags (pkg/mills/store/types.go);
// the top-level BacklogItem fields are PascalCase Go field names (untagged),
// matching BacklogItem above. This is what makes the backlog drawer worth a
// click: the spec, the slice decomposition, the budget, and cross-links to
// the council run that birthed the item and the GitLab issue behind it.
export interface BacklogSlice {
  name: string;
  files?: string[];
  tests?: string[];
  parallel_with?: string[];
}
export interface BacklogSuccessCriteria {
  tests?: string[];
  metrics?: string[];
  manual_check?: string;
}
export interface BacklogBudget {
  max_cost_usd?: number;
  max_turns?: number;
  max_pipeline_minutes?: number;
}
export interface BacklogItemPolicy {
  require_human_review?: boolean;
  auto_merge?: boolean;
  protected_paths_touched?: string[];
  // S7 imperative template selection: when set, the reconciler routes this
  // item through ClaimWorkflowStart (frozen registry template) instead of
  // the DAG pipeline.
  workflow_template?: string;
  workflow_template_version?: string;
  workflow_params?: Record<string, number>;
  workflow_enums?: Record<string, string>;
}
export interface BacklogItemDetail extends BacklogItem {
  GitLabIssueIID?: number | null;
  SpecDoc?: string;
  SpecAnchor?: string;
  Success?: BacklogSuccessCriteria;
  Budget?: BacklogBudget;
  Policy?: BacklogItemPolicy;
  Slices?: BacklogSlice[];
  Dependencies?: string[];
  CouncilRunID?: string | null;
}

type BacklogDetailLoadState =
  | { status: 'idle' }
  | { status: 'loading' }
  | { status: 'loaded'; detail: BacklogItemDetail }
  | { status: 'error'; message: string };

export interface PipelineRun {
  ID: string;
  BacklogID: string;
  Template: string;
  State: string;
  // CurrentStage is the stage id the runner is currently driving
  // (plan_slice, research, implement, tests, pr_self_review, mr, ci_watch,
  // merge, cleanup). Surfaces what a run is "doing right now" without
  // requiring an extra detail fetch.
  CurrentStage?: string;
  Attempts: number;
  StartedAt?: string;
  EndedAt?: string;
  // Phase 6 (bounded recursion): top-level runs have ParentRunID == null
  // and Depth == 0. Subruns created via mills_pipeline_subrun_create
  // carry their parent's ID and depth+1.
  ParentRunID?: string | null;
  Depth?: number;
  // Detail-only fields (populated by GET /api/mills/pipeline/runs/{id});
  // list responses may omit them, so they're optional. WorktreePath +
  // MRIID let the drilldown surface direct links; CostUSD is the
  // aggregate stage spend for the header total.
  WorktreePath?: string;
  MRIID?: number | null;
  CostUSD?: number;
  ParentSessionID?: string;
  // Escalation metadata stamped on escalated runs by the operator
  // (store.PipelineRun, default Go JSON => PascalCase). EscalationClass is
  // the runner's ErrorClass spelling ("infra"/"config"); FailureClass is the
  // policy-facing taxonomy; ExternalDependency names a known upstream
  // incident. EscalationRetryable is *bool on the wire — null/absent means
  // the run predates the columns or carried no class marker, so it's a
  // tri-state, not a plain boolean. PipelineRunDetail renders these as chips.
  EscalationClass?: string;
  FailureClass?: string;
  ExternalDependencyID?: string;
  ExternalDependency?: string;
  EscalationRetryable?: boolean | null;
  Grade?: BoltGrade;
  GradeNote?: string;
}

export type BoltGrade = 'keep' | 'meh' | 'regret';

interface GradeRunResponse {
  run_id: string;
  grade: BoltGrade;
  note?: string;
}

/** One retryable escalated backlog item, normalized from the operator projection. */
export interface RelaunchCandidate {
  backlogId: string;
  title: string;
  escalationClass: string;
  failureClass: string;
  latestRunEndedAt: string | null;
}

interface RelaunchCandidateWire {
  ID?: unknown;
  Title?: unknown;
  EscalationClass?: unknown;
  FailureClass?: unknown;
  EndedAt?: unknown;
}

// StageResult mirrors store.StageResult — one attempt at one stage.
// Outcome is *StageOutcome on the Go side: null/missing while in flight,
// "success" | "gate_fail" | "error" once finalized.
export interface StageResult {
  ID: number;
  PipelineRunID: string;
  Stage: string;
  Attempt: number;
  StartedAt: string;
  EndedAt?: string | null;
  Outcome?: 'success' | 'gate_fail' | 'error' | null;
  SpawnID?: string;
  CostUSD: number;
  Artifacts?: Record<string, unknown>;
  LogTail?: string;
}

// GateOutcome mirrors store.GateOutcome — one evaluated gate after a
// stage. Reasons are surfaced to the user verbatim so they can act on
// gate_fail without diving into the operator logs.
export interface GateOutcome {
  ID: number;
  PipelineRunID: string;
  AfterStage: string;
  GateName: string;
  Outcome: 'pass' | 'fail' | 'skip';
  Reasons?: string[];
  JudgedBy?: string;
  EvaluatedAt: string;
}

// PipelineRunDetail is the shape returned by handlePipelineRunGet —
// run + stages + gates in one round-trip so the drilldown drawer can
// render without per-section fetches.
// One judge verdict as persisted by the runner's gate site (judge.verdict
// event payload + its occurred_at). Retries append rather than replace, so a
// run can carry several verdicts per gate — the chronology is the evidence.
export interface JudgeVerdictEvidence {
  gate?: string;
  role?: string;
  judge_model?: string;
  score?: number;
  threshold?: number;
  pass?: boolean;
  attempt?: number;
  occurred_at?: string;
}

// The run.provenance stamp: the exact configuration that produced the run.
export interface ProvenanceEvidence {
  lane?: string;
  policy_checksum?: string;
  stage_models?: Record<string, string>;
  prompt_hashes?: Record<string, string>;
  occurred_at?: string;
}

// The regression.attributed event for the run's MR, when a later revert
// undid this run's merged work.
export interface RegressionEvidence {
  regressed_mr_iid?: number;
  merged_sha?: string;
  revert_sha?: string;
  revert_title?: string;
  occurred_at?: string;
}

// The run's CURRENT-BELIEF verdict (Trustworthy Verdicts S1, pkg/mills's
// RunVerdict): what we believe now, resolved from the newest superseding
// event on the run's own subject, or derived from the terminal row when
// nothing superseded it. Distinct from `verdicts` above (judge gate scores)
// and from run.State, which is immutable terminal history.
//
// `class` is the only field the operator always emits; `occurred_at` has no
// omitempty on the Go side, so an in-flight run arrives carrying the zero
// time ("0001-01-01T00:00:00Z") rather than omitting the key. Everything
// else is optional so a mixed-version operator type-checks without casts.
export interface RunVerdictEvidence {
  class: string;
  superseded?: boolean;
  source?: string;
  prior_class?: string;
  outcome?: string;
  occurred_at?: string;
}

export interface RunEvidence {
  verdicts: JudgeVerdictEvidence[];
  provenance: ProvenanceEvidence | null;
  regression: RegressionEvidence | null;
  // Older operators predate the field entirely; optional on top of nullable.
  verdict?: RunVerdictEvidence | null;
}

// One demand-log row: a proposal the council suppressed as restating
// recently-merged work (GET /api/mills/demand-log).
export interface DemandLogRow {
  occurred_at: string;
  proposal_title: string;
  merged_title: string;
  merged_url?: string;
  merged_ref?: string;
  score?: number;
  basis?: string;
  dry_run?: boolean;
}

export interface PipelineRunDetail {
  run: PipelineRun;
  // Older operators omit the block; every consumer must null-guard.
  evidence?: RunEvidence;
  stages: StageResult[];
  gates: GateOutcome[];
}

type PipelineDetailLoadState =
  | { status: 'idle' }
  | { status: 'loading' }
  | { status: 'loaded'; detail: PipelineRunDetail }
  | { status: 'error'; message: string };

// --- Requeue action outcome (plan wave-2 W3) ------------------------------
//
// requeuePipelineRun hits the operator's admin start endpoint with ?requeue=1,
// which returns pipelineStartResponse
// ({run_id?, backlog_id, decision, state?, reason?, blockers?}) and maps
// outcomes onto HTTP status: 201 started, 409 conflict (the one-way
// terminal-state guard — e.g. "state is merged", the ghost-spark already-done
// case), 403 policy disabled / autonomy blocked / missing admin token.
// normalizeRequeueResponse is a pure status+body → outcome mapper so the drawer
// can render nuanced inline feedback and be unit-tested without a DOM.

/** Wire shape of the operator's pipeline start/requeue response. */
export interface RequeueResponseBody {
  run_id?: string;
  backlog_id?: string;
  decision?: string;
  state?: string;
  reason?: string;
  blockers?: string[];
}

export type RequeueOutcomeKind = 'started' | 'conflict' | 'forbidden' | 'error';

export interface RequeueOutcome {
  kind: RequeueOutcomeKind;
  /** run_id from a 201 started response, when the operator returned one. */
  runId?: string;
  /** operator reason (409/403) or normalized error text. */
  reason?: string;
  /** autonomy blockers the operator returned (403 autonomy-gate case). */
  blockers?: string[];
  /** true when a 409 reason means the item already reached a terminal state
   *  (e.g. "state is merged"): the ghost-spark case reads as already-done,
   *  not a failure the operator must chase. */
  alreadyCompleted?: boolean;
  /** human-facing message the drawer can render directly. */
  message: string;
}

function asRequeueBody(body: unknown): RequeueResponseBody | null {
  if (!body || typeof body !== 'object') return null;
  return body as RequeueResponseBody;
}

// normalizeRequeueResponse maps the operator's status + (parsed) body onto a
// RequeueOutcome. Pure: no store/DOM access, so it is unit-tested directly.
// `body` is the parsed JSON object for 2xx/4xx responses, or the raw text for
// the plain-text http.Error bodies the operator returns on 404/500/503.
export function normalizeRequeueResponse(status: number, body: unknown): RequeueOutcome {
  const parsed = asRequeueBody(body);
  // The operator's JSON carries `reason`; the HUD admin gate rejects a
  // present-but-wrong token with 401 {"error":"invalid admin token"} — read
  // that shape too so the drawer can show the admin-token hint instead of a
  // bare status code.
  const gateError =
    body && typeof body === 'object' && typeof (body as { error?: unknown }).error === 'string'
      ? ((body as { error: string }).error ?? '').trim()
      : '';
  const reason =
    parsed?.reason?.trim() || gateError || (typeof body === 'string' ? body.trim() : '');

  if (status === 401) {
    return {
      kind: 'forbidden',
      reason: reason || undefined,
      message: reason
        ? `Requeue forbidden: ${reason} — set the admin token in the Labs access bar`
        : 'Requeue forbidden — set the admin token in the Labs access bar',
    };
  }

  if (status === 201) {
    const runId = parsed?.run_id?.trim() || undefined;
    return {
      kind: 'started',
      runId,
      message: runId ? `Requeued as ${runId}` : 'Requeued',
    };
  }

  if (status === 409) {
    // The one-way terminal-state guard refuses to requeue an item that already
    // reached merged/done — the MWPS-merged-later "ghost spark". Phrase that as
    // already-completed rather than an error the operator must chase.
    const alreadyCompleted = /\bstate is (merged|done)\b/i.test(reason);
    return {
      kind: 'conflict',
      reason: reason || undefined,
      alreadyCompleted,
      message: alreadyCompleted
        ? `Already completed${reason ? ` (${reason})` : ''} — nothing to requeue`
        : `Can't requeue${reason ? `: ${reason}` : ' — item is no longer requeueable'}`,
    };
  }

  if (status === 403) {
    const blockers =
      parsed?.blockers && parsed.blockers.length > 0 ? parsed.blockers : undefined;
    const detail = blockers
      ? `${reason || 'autonomy blocked'} (${blockers.join(', ')})`
      : reason;
    return {
      kind: 'forbidden',
      reason: reason || undefined,
      blockers,
      message: detail
        ? `Requeue forbidden: ${detail} — set the admin token in the Labs access bar`
        : 'Requeue forbidden — set the admin token in the Labs access bar',
    };
  }

  return {
    kind: 'error',
    reason: reason || undefined,
    message: reason ? `Requeue failed (${status}): ${reason}` : `Requeue failed (${status})`,
  };
}

// --- Durable workflow step-log (plan .loom/134 §S4) -----------------------
//
// These mirror the operator's S4a read endpoints (handlers_workflow.go).
// Unlike the pipeline endpoints (which use Go default PascalCase encoding),
// the workflow handlers carry explicit snake_case json tags, so the field
// names here are snake_case to match the wire shape verbatim. The contract
// is pinned operator-side by handlers_workflow_test.go — if a tag changes
// there, change it here too.

// WorkflowRun is the flat per-row summary returned by
// GET /api/mills/workflow/runs (and embedded as `run` in the detail
// response). step_count is only populated on the detail endpoint, so it's
// optional. cost_usd is the aggregate spend the runtime attributed to the
// run; its provenance is per-step (see WorkflowStep.cost_source).
export interface WorkflowRun {
  id: string;
  backlog_id?: string;
  // agent_type is populated for canary runs (the immutable harness choice);
  // S7 registry runs carry their harness inside workflow_params instead.
  agent_type?: string;
  engine: string;
  template: string;
  // template_version + interpreter_version are the S7 frozen identity — a
  // run replays under exactly this pin or fails closed.
  template_version?: string;
  interpreter_version?: string;
  state: string;
  started_at?: string;
  ended_at?: string;
  cost_usd: number;
  step_count?: number;
}

// TERMINAL_WORKFLOW_STATES mirrors the store's terminal set. A claim-started
// run in one of these has ALREADY settled: reservation released, item
// escalated, work product parked on its mills-wf/<run-id> branch.
export const TERMINAL_WORKFLOW_STATES = new Set([
  'done',
  'error',
  'escalated',
  'quarantined',
]);

// workflowRunBranch derives the deterministic work-product branch for an
// imperative run (store.WorkflowRunBranchPrefix + run id).
export function workflowRunBranch(runID: string): string {
  return `mills-wf/${runID}`;
}

// WorkflowStep is one row of the durable replay log returned by
// GET /api/mills/workflow/runs/{id}. `badge` is server-derived
// (deriveStepBadge) so the UI never re-implements the cache-hit heuristic;
// `cost_source` is the provenance of cost_usd and must NEVER be blended —
// an "estimated" cost is not a "real" one. effect_count == 0 on a success
// is what the operator's badge logic reads as a replay (cache_hit).
export type WorkflowStepBadge =
  | 'quarantined'
  | 'cache_hit'
  | 'live'
  | 'pending'
  | 'failed';

export type WorkflowCostSource = 'real' | 'estimated' | 'unavailable' | string;

export interface WorkflowStep {
  id: number;
  step_key: string;
  event_type: string;
  status: string;
  spawn_id?: string;
  call_hash: string;
  cost_usd: number;
  cost_source?: WorkflowCostSource;
  effect_count: number;
  started_at?: string;
  ended_at?: string;
  badge: WorkflowStepBadge | string;
}

// WorkflowRunDetail is the {run, steps} payload from the detail endpoint —
// run summary (with step_count) plus the append-ordered step log.
export interface WorkflowRunDetail {
  run: WorkflowRun;
  steps: WorkflowStep[];
  // workflow_params is the run's frozen selection blob (S7 registry runs) or
  // canary params — opaque JSON, detail-only, pretty-printed in the drawer.
  workflow_params?: string;
}

type WorkflowDetailLoadState =
  | { status: 'idle' }
  | { status: 'loading' }
  | { status: 'loaded'; detail: WorkflowRunDetail }
  | { status: 'error'; message: string };

export interface CouncilRun {
  ID: string;
  Trigger: string;
  Outcome: string;
  StartedAt?: string;
  EndedAt?: string;
  CostUSD?: number;
}

// CouncilDebateRound mirrors the Go store.CouncilDebateRound shape.
// Used by the Council panel's "Debate Rounds" expander (Phase 5 slice
// 5.3) to render the per-round transcript persisted by slice 5.2.
export interface CouncilDebateRound {
  ID: number;
  CouncilRunID: string;
  RoundIndex: number;
  // editor_proposes | reviewer_critiques | moderator_decision | editor_revises
  Role: string;
  CostUSD: number;
  Summary?: string;
  ArtifactDeltas?: Array<{ path?: string; line_range?: string; action?: string }>;
  CreatedAt?: string;
}

// DebateLoadState tracks the lazy fetch lifecycle per council run.
// Stored in the millsStore so the panel can render a spinner / error
// / cached transcript without re-fetching on every poll tick.
type DebateLoadState =
  | { status: 'idle' }
  | { status: 'loading' }
  | { status: 'loaded'; rounds: CouncilDebateRound[] }
  | { status: 'error'; message: string };

// EvalScore mirrors pkg/mills/store.EvalScore as JSON-encoded by
// /api/mills/eval/scores. Field names are the Go struct names because
// the operator uses default encoding/json (no tags). The contract is
// pinned by TestHandleEvalScores_JSONShape in handlers_test.go — if
// you change a field here, update both sides at once.
export interface EvalScore {
  ID: number;
  SubjectKind: 'council_run' | 'pipeline_run' | 'cross_run' | string;
  SubjectID: string;
  Rubric: string;
  Score: number;
  Breakdown?: Record<string, unknown>;
  JudgedBy: string;
  EvaluatedAt: string;
  Notes?: string;
}

// loopLetterFor derives the "A | B | C | ?" loop tag from the eval
// score's attribution. The recording sites in pkg/mills/eval pin:
//   Loop A — council artifact judgments (SubjectKind=council_run)
//   Loop B — pipeline outcome attribution (SubjectKind=pipeline_run)
//   Loop C — weekly cross-run consistency (JudgedBy=loop_c_cross_run)
// Anything else is unknown.
export function loopLetterFor(s: Pick<EvalScore, 'SubjectKind' | 'JudgedBy'>): string {
  if (s.JudgedBy && s.JudgedBy.startsWith('loop_c_')) return 'C';
  if (s.SubjectKind === 'pipeline_run') return 'B';
  if (s.SubjectKind === 'council_run') return 'A';
  if (s.SubjectKind === 'cross_run') return 'C';
  return '?';
}

// CouncilAgent mirrors pkg/mills/policy.go:CouncilAgent. Used by the
// council ensemble inspection in CouncilPanel so operators can see
// the editor/reviewer pool the next run will use, with live model
// availability badges from flexinfer_models.svelte.ts.
export interface CouncilAgent {
  name?: string;
  model?: string;
  backend?: string;
}

export interface CouncilEnsemble {
  editor?: CouncilAgent;
  reviewers?: CouncilAgent[];
  judge?: CouncilAgent;
}

export interface PolicyView {
  enabled: boolean;
  version: number;
  raw?: unknown;
  // Optional decoded shape — the operator returns the full Policy
  // struct on /api/mills/policy, but only enabled+version are needed
  // by most panels. CouncilPanel extracts the ensemble lazily so
  // unrelated surfaces don't carry the payload weight.
  council?: {
    ensemble?: CouncilEnsemble;
  };
}

export interface MillsCapabilityRow {
  id: string;
  status?: 'green' | 'yellow' | 'red' | string;
  mode?: string;
  required_for_autonomy?: boolean;
  last_checked_at?: string;
  message?: string;
  source?: string;
  config_key?: string;
}

// BudgetWindowUsage mirrors pkg/mills.WindowUsage — one tier's rolling-24h
// spend/runs against the active policy caps. Zero caps mean "not
// configured" (an uncapped tank), not an empty one.
export interface BudgetWindowUsage {
  spent_usd: number;
  cap_usd: number;
  runs: number;
  runs_cap: number;
}

export interface MillsStatus {
  ok?: boolean;
  service?: string;
  time?: string;
  policy_enabled?: boolean;
  policy_version?: number;
  // Rolling-24h fuel for the Factory gauge; a tier is omitted when its
  // read failed (the gauge renders an em dash, never a guessed level).
  budget?: { pipeline?: BudgetWindowUsage; council?: BudgetWindowUsage };
  autonomy_ready?: boolean;
  autonomy_blockers?: string[];
  capabilities?: MillsCapabilityRow[];
  queue_depth?: number;
  active_pipeline_runs?: number;
  last_council_at?: string | null;
  // All-time most-recent autonomous merge (operator-sourced). The health
  // banner can't derive this from the active-only pipelineRuns list, which
  // never holds terminal `done` runs — see systemHealth `lastMergeAt`.
  last_merge_at?: string | null;
}

// KillSwitchResult mirrors the operator's POST /api/mills/policy/kill-switch
// response. When `changed` is false the desired state already matched gitops
// and no MR was opened. Otherwise mr_url/mr_iid link the auto-PR that, once
// merged + reconciled, applies the flip.
export interface KillSwitchResult {
  changed: boolean;
  previous_enabled: boolean;
  desired_enabled: boolean;
  mr_url?: string;
  mr_iid?: number;
  branch?: string;
  message: string;
}

// PolicyProposal mirrors the operator's proposals row. Field casing is
// PascalCase because the proposals handler relies on Go's default JSON
// marshalling (no explicit json: tags). Phase 7 slice 7.1/7.2 own the
// emission + apply/reject endpoints; this UI consumes them read-only
// plus the two POST mutations below.
export interface PolicyProposal {
  ID: number;
  ProposalDate: string; // YYYY-MM-DD
  Kind: 'relax' | 'tighten' | 'rotate_ensemble';
  Target: string;
  Diff: string;
  Rationale: string;
  State: 'pending' | 'applied_human' | 'applied_auto' | 'rejected' | 'reverted';
  AppliedAt?: string;
  RevertDeadline?: string;
  CreatedAt: string;
}

// CostEstimate mirrors the slice 7.3 /api/mills/cost-preview response.
// Field casing is snake_case because that handler sets explicit
// json:"…" tags. Confidence + sample_size let the UI render a band
// pill (low/med/high) so users can read past-data quality at a glance.
export interface CostEstimate {
  backlog_id: string;
  path_class: string;
  median_historical_usd: number;
  sidecar_slice_count: number;
  sidecar_overhead_usd: number;
  recursion_overhead_usd: number;
  estimate_usd: number;
  ensemble_cap_usd: number;
  capped_by_policy: boolean;
  confidence: 'low' | 'medium' | 'high';
  sample_size: number;
  source: string; // "estimator/v1"
}

// MillsKPISnapshot mirrors the operator's `kpi_snapshots.metrics_json`
// rollup. Fields are optional because the recorder only emits keys it has
// data for; missing keys render as "—" placeholders. Field names here are
// the contract the (future) snapshot recorder must honor.
export interface MillsKPISnapshot {
  snapshot_at?: string;
  window_seconds?: number;
  metrics?: {
    cost_per_merged_change_usd?: number;
    cost_per_merged_pipeline_usd?: number;
    slice_to_merge_p50_seconds?: number;
    gate_pass_rate?: number;        // 0..1
    auto_merge_rate?: number;       // 0..1
    regression_rate?: number;       // 0..1
    council_roi?: number;           // merged-changes-per-council-USD
    // Absolute window counts the operator already emits but the UI did
    // not previously surface. pipeline_merged_runs over the 1d window is
    // the north-star: autonomous merges in the last 24h.
    pipeline_merged_runs?: number;
    pipeline_escalated_runs?: number;
    escalation_rate?: number;       // 0..1
    // Escalated-run breakdown by terminal fault class (code/config/infra/
    // transient/transient_quota/unclassified) over the KPI window. Sums to
    // pipeline_escalated_runs. Absent until the operator emits the key.
    escalations_by_class?: Record<string, number>;
    council_runs?: number;
    pipeline_runs?: number;
  };
}

// Operator returns snapshots with PascalCase fields (Go struct json tags
// follow Go naming). Accept both casings so a recorder that emits
// snake_case `metrics_json` payloads also works.
interface RawKPISnapshot {
  ID?: number;
  SnapshotAt?: string;
  WindowSeconds?: number;
  Metrics?: MillsKPISnapshot['metrics'];
  snapshot_at?: string;
  window_seconds?: number;
  metrics?: MillsKPISnapshot['metrics'];
}

const KPI_HISTORY_MAX = 24;

// normalizeKPISnapshot folds the operator's dual-cased KPI payload
// (PascalCase Go tags OR snake_case recorder output) into the one shape the
// UI reads. Shared by the 1d snapshot path (applyKPISnapshot) and the
// Telemetry panel's windowed fetch so the casing fallback can't drift.
function normalizeKPISnapshot(raw: RawKPISnapshot): MillsKPISnapshot {
  return {
    snapshot_at: raw.SnapshotAt ?? raw.snapshot_at,
    window_seconds: raw.WindowSeconds ?? raw.window_seconds,
    metrics: raw.Metrics ?? raw.metrics ?? {},
  };
}

// Bound every Mills proxy fetch so a stalled operator surfaces as a clear,
// retryable error instead of an indefinitely-pending request. The HUD proxy's
// own ResponseHeaderTimeout is 30s and only covers time-to-first-byte — with no
// client deadline the pipeline-detail drawer would pin itself in "Loading…"
// with no way out (operator slow/unreachable). Kept under the 15s poll interval
// so a timed-out tick is simply replaced by the next one rather than piling up.
const FETCH_TIMEOUT_MS = 12_000;

// SystemHealth + computeSystemHealth live in a rune-free sibling module
// so fixtures / SSR can exercise them without a Svelte runtime. Re-export
// here so consumers keep `from './mills.svelte.ts'` ergonomics.
export type { SystemHealth, SystemHealthState } from './mills.systemHealth.ts';
import { computeSystemHealth } from './mills.systemHealth.ts';
import type { SystemHealth } from './mills.systemHealth.ts';
// SSE + staleness (plan-117 cohort). The `hud.mills` event is a refresh signal
// (the store fans out to many operator endpoints, so it re-runs fetchAll rather
// than applying a partial snapshot); the watchdog poll drops to 60s.
import { eventStore } from './events.svelte.ts';
import { untrack } from 'svelte';
import { isStaleFromTimestamp, stalenessStore } from './staleness.svelte.ts';
import { createPoller } from '../utils/poller.ts';
import type { StageTelemetryReport, TelemetryWindow } from '../utils/telemetryHelpers.ts';
import { normalizeWiring, type MillsWiring } from '../utils/millsWiringHelpers.ts';
// Mill-floor lineage (spec .loom/product-spec-mill-floor-views-2026-07-18 S0).
// The pure segment builders live in the shared module so they unit-test
// without a DOM; the store methods below delegate to them. This is a
// type-only cycle back into this file, so there is no runtime import loop.
import {
  lineageFor as buildLineage,
  spineSegments,
  isBoltState,
  type LineageSegment,
  type WarpBucket,
  type WarpPriority,
} from '../components/mills/shared/lineage.ts';
// Mills mutations are double-gated: the HUD's own requireAdminToken gate runs
// BEFORE the proxy injects the operator's admin bearer, so a browser mutation
// must present the HUD admin token (X-Admin-Token) itself. adminFetch attaches
// it from the Labs access bar.
import { adminFetch } from './labsAuth.svelte.ts';

// Guarded document access — the store is imported by vitest's node environment.
function documentHidden(): boolean {
  return typeof document !== 'undefined' && document.hidden;
}

class MillsStore {
  // Per-panel data
  backlog = $state<BacklogItem[]>([]);
  pipelineRuns = $state<PipelineRun[]>([]);
  councilRuns = $state<CouncilRun[]>([]);
  evalScores = $state<EvalScore[]>([]);
  // Sparks owns this projection and its 60s poller. Keep its state separate
  // from the shared run poll so a queue outage cannot red-flag the whole panel.
  relaunchCandidates = $state<RelaunchCandidate[]>([]);
  relaunchCandidatesLoading = $state(false);
  relaunchCandidatesError = $state<string | null>(null);
  policy = $state<PolicyView | null>(null);
  status = $state<MillsStatus | null>(null);

  // KPI snapshot for the rolling 1d window plus a small in-memory history
  // for sparkline trends. The history is only de-duped on snapshot_at so
  // repeated polls of the same snapshot don't pad the trend.
  kpis = $state<MillsKPISnapshot | null>(null);
  kpisHistory = $state<MillsKPISnapshot[]>([]);

  // Stage/gate telemetry rollup (plan S6) from
  // GET /api/mills/telemetry/stages?window=. Owned by the Telemetry panel,
  // which drives its own window-scoped poller — kept OFF fetchAll so panels
  // that never open it pay nothing. telemetryUnavailable flags that the
  // endpoint answered 404 (or the SPA catch-all): the endpoint IS live on
  // current HUD + operator builds, so this now means "this deployment is
  // older than the route" and the panel renders an explicit empty state
  // rather than a committed sample. telemetryKpis is the KPI snapshot for the
  // panel's selected window, kept separate from `kpis` (pinned to window=1d
  // for the Factory/Overview 24h gauges) so a 7d/30d selection here never
  // clobbers those.
  telemetryReport = $state<StageTelemetryReport | null>(null);
  telemetryLoading = $state(false);
  telemetryError = $state<string | null>(null);
  telemetryUnavailable = $state(false);
  telemetryKpis = $state<MillsKPISnapshot | null>(null);

  // Model-routing wiring (Overview "Loom wiring" card) from
  // GET /api/mills/wiring. Owned by the Overview panel, which drives its own
  // SLOW poller (wiring only changes on operator restart) — kept OFF fetchAll
  // so panels that never open it pay nothing. wiringUnavailable flags that the
  // endpoint answered 404 (or the SPA catch-all); the route is live on current
  // builds, so the panel treats it as "this operator predates the route" and
  // says so instead of rendering a sample.
  wiring = $state<MillsWiring | null>(null);
  wiringLoading = $state(false);
  wiringError = $state<string | null>(null);
  wiringUnavailable = $state(false);

  // Per-run debate transcripts, keyed by CouncilRun.ID. Populated
  // lazily by loadDebate() so the council list itself stays cheap.
  // Phase 5 slice 5.3 — feeds the CouncilPanel's "Debate Rounds"
  // expander.
  debateByRun = $state<Record<string, DebateLoadState>>({});

  // Pipeline-run drilldown state. selectedRunID drives whether the
  // PipelineRunDetail drawer is open; pipelineDetailByRun caches the
  // {run, stages, gates} payload so re-opening the same run is
  // instant. The 15s background poll refreshes the cached entry only
  // when the drawer is currently open (see refreshOpenPipelineDetail).
  selectedRunID = $state<string | null>(null);
  pipelineDetailByRun = $state<Record<string, PipelineDetailLoadState>>({});
  // Non-reactive: the abort controller for the open run's detail fetch.
  // Lets closeRunDetail() cancel an in-flight (possibly stalled) request so
  // the drawer dismisses instantly instead of waiting on the network.
  private detailAbort: AbortController | null = null;

  // Backlog drilldown state — mirrors the pipeline-run drawer pattern.
  // selectedBacklogID drives the BacklogDetail drawer; backlogDetailByID
  // caches the full item so re-opening is instant, and the 15s poll
  // refreshes only the open item (see refreshOpenBacklogDetail).
  selectedBacklogID = $state<string | null>(null);
  backlogDetailByID = $state<Record<string, BacklogDetailLoadState>>({});
  // Non-reactive abort controller for the open backlog item's detail fetch
  // (same close-cancels-fetch contract as detailAbort above).
  private backlogAbort: AbortController | null = null;

  // Pending adaptive policy proposals (Phase 7 slice 7.1/7.2). Refreshed
  // alongside the rest of fetchAll so the card stays in sync with the
  // 15s poll cadence used elsewhere.
  policyProposals = $state<PolicyProposal[]>([]);

  // Cost previews keyed by backlog_id (Phase 7 slice 7.3). Lazily
  // populated by BacklogPanel rows; never auto-polled because the
  // estimate is stable for a given backlog item until policy changes.
  costPreviews = $state<Record<string, CostEstimate>>({});

  // Run history (terminal runs: done / escalated / paused) for the
  // Pipelines "History" view. Kept separate from the live active-run list
  // so the default panel stays in-flight-only and cheap. Only refreshed on
  // the 15s tick while historyActive is true (the panel sets it when the
  // History view is showing), so an operator who never opens history pays
  // nothing. Errors are local to the history view, not the whole panel.
  pipelineHistory = $state<PipelineRun[]>([]);
  // Demand-side decision log (suppressed proposals) for the factory board.
  demandLog = $state<DemandLogRow[]>([]);
  historyLoading = $state(false);
  historyError = $state<string | null>(null);
  historyActive = $state(false);

  // Durable workflow step-log (plan .loom/134 §S4b). The workflow_runs /
  // workflow_steps journal is a separate surface from the DAG pipeline
  // runs, so it has its own list + drilldown state and its own loaders
  // (loadWorkflowRuns / loadWorkflowRunDetail). The Workflows panel owns
  // the poll cadence; the detail drawer renders off selectedWorkflowID +
  // workflowDetailByID, mirroring the pipeline drawer pattern. Errors are
  // local to the workflow surface so a journal hiccup never red-flags the
  // rest of the Mills tabs.
  workflowRuns = $state<WorkflowRun[]>([]);
  workflowLoading = $state(false);
  workflowError = $state<string | null>(null);
  selectedWorkflowID = $state<string | null>(null);
  workflowDetailByID = $state<Record<string, WorkflowDetailLoadState>>({});

  // Connection state
  loading = $state(false);
  error = $state<string | null>(null);
  disabled = $state(false); // operator URL unset → 503 from proxy
  lastUpdated = $state<Date | null>(null);
  // Transient-outage smoothing (the Deck's "502 gremlins"): the operator
  // redeploys with strategy Recreate on every merged MR, so a ~30–60s
  // unreachable window is ROUTINE, not an outage. One failed tick sets
  // `reconnecting` (quiet UI: keep last-known data, muted notice) and arms a
  // fast retry; `error` — which seven panels render red — is only set after
  // two consecutive fully-failed ticks, i.e. a real outage. A tick where only
  // some endpoints failed keeps the fresh slices, keeps the stale rest, and
  // reports the failures via `degraded` instead of redding the whole surface.
  reconnecting = $state(false);
  degraded = $state<string | null>(null);
  private failedTicks = 0;
  private recoveryTimer: ReturnType<typeof setTimeout> | null = null;

  // Staleness (plan-117 cohort). Suppressed while `disabled` so the
  // ConnectionBanner stale pill stays quiet on laptops where the operator
  // isn't configured (no hud.mills events ever arrive there by design).
  staleAfter = 90_000;
  get isStale(): boolean {
    if (this.disabled) return false;
    // Staleness only applies while polling is active (page mounted). An
    // unmounted page's store keeps a frozen lastUpdated forever; reporting
    // it stale would pin the global "Stale data" banner permanently.
    if (!this.poller.running) return false;
    return isStaleFromTimestamp(this.lastUpdated, this.staleAfter);
  }

  // 60s watchdog poll — fires only when SSE is down OR the store has gone
  // stale. A healthy stream drives refreshes via the `hud.mills` tick.
  // The watchdog gate lives in shouldTick so the SSE-invalidation path
  // (refreshCoalesced) is not suppressed by it.
  private poller = createPoller(() => this.fetchAll(), 60000, {
    shouldTick: () => !eventStore.connected || this.isStale,
  });
  private eventUnsubs: Array<() => void> = [];

  get pipelinesByState(): Record<string, number> {
    const out: Record<string, number> = {};
    for (const r of this.pipelineRuns) {
      out[r.State] = (out[r.State] ?? 0) + 1;
    }
    return out;
  }

  get backlogByState(): Record<string, number> {
    const out: Record<string, number> = {};
    for (const i of this.backlog) {
      out[i.State] = (out[i.State] ?? 0) + 1;
    }
    return out;
  }

  get autonomyReady(): boolean | null {
    return this.status?.autonomy_ready ?? null;
  }

  get autonomyBlocked(): boolean {
    return this.status?.autonomy_ready === false && (this.status?.autonomy_blockers?.length ?? 0) > 0;
  }

  get autonomyBlockers(): string[] {
    return this.status?.autonomy_blockers ?? [];
  }

  // systemHealth is the input that drives the Overview "System Health"
  // banner. Recomputed on every access (cheap; bounded by the pipeline run
  // page) so it stays in lock-step with the 15s poll.
  get systemHealth(): SystemHealth {
    return computeSystemHealth({
      pipelineRuns: this.pipelineRuns,
      status: this.status,
      councilRuns: this.councilRuns,
      backlog: this.backlog,
      // Authoritative 24h terminal counts from the rolling-1d KPI snapshot
      // (this.kpis is the window=1d snapshot). Without these the health
      // banner is blind to merged/escalated runs — they never appear in
      // the active-only pipelineRuns list.
      mergedRuns24h: this.kpis?.metrics?.pipeline_merged_runs,
      escalatedRuns24h: this.kpis?.metrics?.pipeline_escalated_runs,
      // All-time last merge from the operator status. Lets the `broken`
      // banner say "Last successful merge: <time>" honestly instead of
      // falsely claiming "No successful merge on record" — the active-only
      // run list can never carry a terminal merged run.
      lastMergeAt: this.status?.last_merge_at,
    });
  }

  async fetchAll(): Promise<void> {
    this.loading = true;
    try {
      const results = await Promise.allSettled([
        this.getJSON<MillsStatus>('/api/mills/status'),
        this.getJSON<PolicyView>('/api/mills/policy'),
        this.getJSON<BacklogItem[]>('/api/mills/backlog'),
        this.getJSON<PipelineRun[]>('/api/mills/pipeline/runs'),
        this.getJSON<CouncilRun[]>('/api/mills/council/runs'),
        this.getJSON<EvalScore[]>('/api/mills/eval/scores'),
        // KPI endpoint returns 404 until the snapshot recorder ships.
        // Tolerate that by passing { tolerate404: true }; null is fine.
        this.getJSON<RawKPISnapshot>('/api/mills/kpis?window=1d', { tolerate404: true }),
      ]);
      const failures: string[] = [];
      const take = <T,>(r: PromiseSettledResult<T | null>, name: string): T | null | undefined => {
        if (r.status === 'fulfilled') return r.value;
        failures.push(`${name}: ${r.reason instanceof Error ? r.reason.message : String(r.reason)}`);
        return undefined; // undefined ⇒ keep whatever we already had
      };
      const [status, policy, backlog, pipelines, council, scores, kpis] = [
        take<MillsStatus>(results[0] as PromiseSettledResult<MillsStatus | null>, 'status'),
        take<PolicyView>(results[1] as PromiseSettledResult<PolicyView | null>, 'policy'),
        take<BacklogItem[]>(results[2] as PromiseSettledResult<BacklogItem[] | null>, 'backlog'),
        take<PipelineRun[]>(results[3] as PromiseSettledResult<PipelineRun[] | null>, 'pipelines'),
        take<CouncilRun[]>(results[4] as PromiseSettledResult<CouncilRun[] | null>, 'council'),
        take<EvalScore[]>(results[5] as PromiseSettledResult<EvalScore[] | null>, 'eval'),
        take<RawKPISnapshot>(results[6] as PromiseSettledResult<RawKPISnapshot | null>, 'kpis'),
      ] as const;

      // Treat 503 from the proxy as "Mills disabled" rather than an error,
      // so the empty-state UX is calm, not red.
      if (failures.some((f) => f.includes('503') || f.toLowerCase().includes('not configured'))) {
        this.disabled = true;
        this.status = null;
        this.error = null;
        this.reconnecting = false;
        this.degraded = null;
        this.failedTicks = 0;
        return;
      }

      if (status !== undefined) this.status = status;
      if (policy !== undefined) this.policy = policy;
      if (backlog !== undefined) this.backlog = backlog ?? [];
      if (pipelines !== undefined) this.pipelineRuns = pipelines ?? [];
      if (council !== undefined) this.councilRuns = council ?? [];
      if (scores !== undefined) this.evalScores = scores ?? [];
      if (kpis !== undefined) this.applyKPISnapshot(kpis ?? null);

      // Core = the three sources the Deck and mill-floor views are built on.
      // Only a tick where ALL of them failed counts toward the outage
      // debounce; anything less keeps the surface green with a degraded note.
      const coreFailed =
        status === undefined && backlog === undefined && pipelines === undefined;
      if (coreFailed) {
        this.failedTicks += 1;
        if (this.failedTicks >= 2) {
          this.error = failures[0] ?? 'Mills operator unreachable';
          this.reconnecting = false;
        } else {
          // First failed tick: routine operator redeploy window. Quiet UI —
          // do not red-flag seven panels for a 30s roll. NO recovery probe
          // here: a fast re-poll during the outage would just count a second
          // failure at +5s and defeat this debounce. Full-outage recovery
          // rides the next scheduled tick and the operator's `hud.mills`
          // push (refreshCoalesced) the moment it comes back.
          this.reconnecting = true;
        }
        this.degraded = null;
        return;
      }

      this.disabled = false;
      this.error = null;
      this.reconnecting = false;
      this.failedTicks = 0;
      this.degraded = failures.length > 0
        ? failures.map((f) => f.split(':')[0]).join(', ') + ' unreachable'
        : null;
      if (failures.length > 0) this.scheduleRecoveryProbe();
      // lastUpdated feeds isStale — only a fully-fresh core resets it, so a
      // partially-failed tick still ages toward the staleness banner.
      if (status !== undefined && backlog !== undefined && pipelines !== undefined) {
        this.lastUpdated = new Date();
      }
      // Refresh adaptive policy proposals on the same tick (Phase 7
      // slice 7.4). Awaited but never throws; its own try/catch
      // silences errors so the rest of the panel stays green.
      void this.fetchPolicyProposals();
      // The drawer/history follow-ups only matter to something on screen, so
      // a hidden tab skips them; the next visible fetchAll picks them back up.
      if (!documentHidden()) {
        // Keep an open pipeline-detail drawer in sync with the 15s
        // cadence — the drawer otherwise frozen at open-time would
        // misrepresent in-flight runs as their stages advance.
        void this.refreshOpenPipelineDetail();
        // Same for an open backlog drawer — state/labels can change as the
        // council re-prioritises, so keep it live rather than frozen.
        void this.refreshOpenBacklogDetail();
        // Keep run history fresh on the same cadence, but only while the
        // Pipelines panel is actually showing the History view.
        if (this.historyActive) void this.fetchPipelineHistory();
      }
      // Prime the terminal archive once so millFloorSpine's bolt/spark
      // tallies are populated on every mill-floor view, not only the two
      // that refresh the archive themselves. One extra fetch per session.
      if (!this.archivePrimed) void this.refreshArchiveRuns();
    } finally {
      this.loading = false;
    }
  }

  // scheduleRecoveryProbe arms a single fast re-poll (5s) after a tick that
  // saw failures, so recovery from the routine operator-redeploy window is
  // quick instead of waiting out the full poll interval. Singleton + gated on
  // an active poller and a visible tab; coalesced through the poller so it
  // can never stack extra full fetch storms.
  private scheduleRecoveryProbe(): void {
    if (this.recoveryTimer || !this.poller.running || documentHidden()) return;
    this.recoveryTimer = setTimeout(() => {
      this.recoveryTimer = null;
      this.poller.refreshCoalesced();
    }, 5000);
  }

  private async getJSON<T>(
    path: string,
    opts: { tolerate404?: boolean; signal?: AbortSignal; timeoutMs?: number } = {},
  ): Promise<T | null> {
    // Every request gets its own controller so we can enforce a deadline and
    // relay an external cancel (e.g. the drawer closing) onto the live fetch.
    const controller = new AbortController();
    const timeoutMs = opts.timeoutMs ?? FETCH_TIMEOUT_MS;
    let timedOut = false;
    const timer = setTimeout(() => {
      timedOut = true;
      controller.abort();
    }, timeoutMs);
    const external = opts.signal;
    const relayAbort = () => controller.abort();
    if (external) {
      if (external.aborted) controller.abort();
      else external.addEventListener('abort', relayAbort, { once: true });
    }
    try {
      const res = await globalThis.fetch(path, { signal: controller.signal });
      if (res.status === 503) {
        // Surface to fetchAll so it can flip the disabled flag.
        throw new Error(`mills proxy: 503 (operator not configured)`);
      }
      if (res.status === 404 && opts.tolerate404) {
        return null;
      }
      if (!res.ok) {
        throw new Error(`${path}: ${res.status}`);
      }
      // Some routes may return null body; tolerate it.
      const text = await res.text();
      if (!text) return null;
      return JSON.parse(text) as T;
    } catch (e) {
      // Our own deadline tripped — report it as a timeout the UI can explain,
      // not the opaque "AbortError" the browser throws.
      if (timedOut) {
        throw new Error(`request timed out after ${Math.round(timeoutMs / 1000)}s`);
      }
      // External cancel (caller closed the drawer): re-tag as a plain
      // AbortError so the caller can swallow it without flagging an error.
      if (external?.aborted) {
        throw new DOMException('aborted', 'AbortError');
      }
      throw e;
    } finally {
      clearTimeout(timer);
      if (external) external.removeEventListener('abort', relayAbort);
    }
  }

  private applyKPISnapshot(raw: RawKPISnapshot | null): void {
    if (!raw) {
      this.kpis = null;
      return;
    }
    const snap = normalizeKPISnapshot(raw);
    this.kpis = snap;
    // Append to history only when snapshot_at advances; otherwise the
    // sparkline would just plot the same point N times.
    const last = this.kpisHistory[this.kpisHistory.length - 1];
    if (!last || last.snapshot_at !== snap.snapshot_at) {
      const next = [...this.kpisHistory, snap];
      this.kpisHistory = next.slice(-KPI_HISTORY_MAX);
    }
  }

  // Pull a single KPI metric series from the in-memory history. Missing
  // values are skipped (no zero-filling) so the sparkline reflects only
  // observed data points.
  metricSeries(key: keyof NonNullable<MillsKPISnapshot['metrics']>): number[] {
    const out: number[] = [];
    for (const snap of this.kpisHistory) {
      const v = snap.metrics?.[key];
      if (typeof v === 'number' && Number.isFinite(v)) out.push(v);
    }
    return out;
  }

  // --- Stage/gate telemetry (plan S6) ------------------------------------

  // Monotonic token for telemetry fetches: rapid window switches (7d→30d)
  // must not let the older window's slower response resolve last and render
  // under the new selection. Each refreshTelemetry bumps the generation;
  // stale responses see a newer token and drop their writes.
  private telemetryGen = 0;

  // fetchStageTelemetry loads the stage/gate rollup for `window`. The route is
  // live on current HUD + operator builds; a deployment older than the route
  // answers 404, and an older HUD hands the request to the SPA catch-all,
  // which returns 200 + index.html and makes JSON.parse throw SyntaxError.
  // Treat BOTH as "route absent on this deployment" (telemetryUnavailable) —
  // distinct from a genuine fetch error, so the panel can name the cause. A
  // 503 (operator unconfigured) flips `disabled`, matching the rest of the
  // store.
  async fetchStageTelemetry(window: TelemetryWindow, gen = this.telemetryGen): Promise<void> {
    this.telemetryLoading = true;
    try {
      const report = await this.getJSON<StageTelemetryReport>(
        `/api/mills/telemetry/stages?window=${encodeURIComponent(window)}`,
        { tolerate404: true },
      );
      if (gen !== this.telemetryGen) return;
      if (report === null) {
        // 404 / empty body: route absent on this deployment. Clear the live
        // report and flag it so the panel can name the cause.
        this.telemetryReport = null;
        this.telemetryUnavailable = true;
        this.telemetryError = null;
      } else {
        // Normalise Go nil slices / a missing runs block to empty defaults at
        // the boundary. A null array reaching a panel $derived (stageWaterfall
        // etc.) would throw and kill the whole effect tree — the same wedge
        // the pipeline-detail drawer hit with `gates:null`. Coerce here so the
        // operator's encoding can never freeze the panel.
        this.telemetryReport = {
          ...report,
          runs: report.runs ?? {
            total: 0,
            done: 0,
            escalated: 0,
            retry_burn_cost_usd: 0,
            retry_burn_seconds: 0,
          },
          stages: report.stages ?? [],
          gates: report.gates ?? [],
          escalation_funnel: report.escalation_funnel ?? [],
          failure_classes: report.failure_classes ?? [],
          model_economics: report.model_economics ?? [],
        };
        this.telemetryUnavailable = false;
        this.telemetryError = null;
      }
    } catch (e) {
      if (gen !== this.telemetryGen) return;
      const msg = e instanceof Error ? e.message : String(e);
      if (msg.includes('503') || msg.toLowerCase().includes('not configured')) {
        this.disabled = true;
        this.telemetryError = null;
      } else if (e instanceof SyntaxError) {
        // 200 + index.html from the SPA catch-all (route not registered on
        // this HUD build) — endpoint not live yet, same handling as 404.
        this.telemetryReport = null;
        this.telemetryUnavailable = true;
        this.telemetryError = null;
      } else {
        this.telemetryError = msg;
      }
    } finally {
      if (gen === this.telemetryGen) this.telemetryLoading = false;
    }
  }

  // fetchTelemetryKPIs pulls the KPI snapshot for the Telemetry panel's
  // selected window into its own slot so it never overwrites the 1d snapshot
  // the Factory/Overview 24h gauges read. Tolerates 404 (recorder not yet
  // emitting a windowed snapshot) by leaving telemetryKpis null.
  async fetchTelemetryKPIs(window: TelemetryWindow, gen = this.telemetryGen): Promise<void> {
    try {
      const raw = await this.getJSON<RawKPISnapshot>(
        `/api/mills/kpis?window=${encodeURIComponent(window)}`,
        { tolerate404: true },
      );
      if (gen !== this.telemetryGen) return;
      this.telemetryKpis = raw ? normalizeKPISnapshot(raw) : null;
    } catch (e) {
      // A KPI hiccup shouldn't blank the telemetry surface — the stage
      // rollup (or fixture) still renders. Console is enough for triage.
      // eslint-disable-next-line no-console
      console.warn('fetchTelemetryKPIs failed', e);
    }
  }

  // refreshTelemetry drives both telemetry fetches for one window in
  // parallel. The Telemetry panel calls this on mount, on window change, and
  // on each of its own poll ticks. No-op while disabled (the panel shows the
  // shared "Mills disabled" empty-state instead).
  async refreshTelemetry(window: TelemetryWindow): Promise<void> {
    // Untracked `disabled` gates here and in the fetchers below: these run
    // synchronously inside panel $effects, and fetchAll writes `disabled`
    // — a tracked read couples every caller effect to operator outage
    // transitions (the mills_staff pre-await-read class, MR !1474).
    if (untrack(() => this.disabled)) return;
    const gen = ++this.telemetryGen;
    await Promise.all([
      this.fetchStageTelemetry(window, gen),
      this.fetchTelemetryKPIs(window, gen),
    ]);
  }

  // --- Model-routing wiring (Overview "Loom wiring" card) ----------------

  // fetchWiring loads the operator's resolved model routing from
  // GET /api/mills/wiring. The endpoint is live against the exact contract
  // normalizeWiring expects; an operator older than the route 404s — treated
  // as "endpoint absent" (wiringUnavailable) so the panel says so rather than
  // rendering a blank card. The SPA catch-all answers a route an older HUD
  // build doesn't register with 200 + index.html, which JSON.parse throws on
  // (SyntaxError) — handled the same as 404. A 503
  // (operator unconfigured) flips `disabled`, matching the rest of the store.
  // Normalisation runs at THIS boundary so every array/nested field is
  // defaulted before any panel `$derived` reads it (the `gates:null` wedge).
  // On a hard error we keep any previously-loaded wiring rather than blanking
  // the card — the routing shown a tick ago is still the best guess.
  async fetchWiring(): Promise<void> {
    if (untrack(() => this.disabled)) return;
    this.wiringLoading = true;
    try {
      const raw = await this.getJSON<unknown>('/api/mills/wiring', { tolerate404: true });
      if (raw === null) {
        // 404 / empty body: endpoint not live yet on this operator.
        this.wiring = null;
        this.wiringUnavailable = true;
        this.wiringError = null;
      } else {
        this.wiring = normalizeWiring(raw);
        this.wiringUnavailable = false;
        this.wiringError = null;
      }
    } catch (e) {
      const msg = e instanceof Error ? e.message : String(e);
      if (msg.includes('503') || msg.toLowerCase().includes('not configured')) {
        this.disabled = true;
        this.wiringError = null;
      } else if (e instanceof SyntaxError) {
        // 200 + index.html from the SPA catch-all (route not registered on
        // this HUD build) — endpoint not live yet, same handling as 404.
        this.wiring = null;
        this.wiringUnavailable = true;
        this.wiringError = null;
      } else {
        this.wiringError = msg;
      }
    } finally {
      this.wiringLoading = false;
    }
  }

  // openPipelineDetail is the derived load-state for the currently
  // selected run id. Returns null when no run is selected so the
  // drawer component can render purely off this one signal.
  get openPipelineDetail(): PipelineDetailLoadState | null {
    if (!this.selectedRunID) return null;
    return this.pipelineDetailByRun[this.selectedRunID] ?? { status: 'idle' };
  }

  // openRunDetail opens the drilldown drawer for a pipeline run.
  // Cache-on-success: subsequent opens for the same id render
  // immediately from cache while a fresh fetch refreshes in the
  // background. The 'loading' state is only set when there's no
  // cached payload yet, so re-opening doesn't flash an empty drawer.
  openRunDetail(runID: string): void {
    if (!runID) return;
    // Cancel any fetch tied to a previously-open run so a slow request for
    // the old run can't land in the drawer after the user switched rows.
    this.detailAbort?.abort();
    this.detailAbort = new AbortController();
    this.selectedRunID = runID;
    // Untracked: openRunDetail runs synchronously inside tracking $effects
    // (InspectDock opens the drawer on selection). A tracked read of the
    // cache it writes on fetch completion re-runs the effect per finished
    // round — an infinite refetch loop at network round-trip cadence
    // (the mills_staff class, MR !1474).
    untrack(() => {
      const cached = this.pipelineDetailByRun[runID];
      if (!cached || cached.status === 'idle' || cached.status === 'error') {
        this.pipelineDetailByRun = {
          ...this.pipelineDetailByRun,
          [runID]: { status: 'loading' },
        };
      }
    });
    void this.fetchPipelineDetail(runID, this.detailAbort.signal);
  }

  closeRunDetail(): void {
    // Abort the in-flight detail fetch immediately so closing the drawer is
    // instant even when the operator is stalled — the request no longer has
    // to run to completion (or the proxy's 30s header timeout) first.
    this.detailAbort?.abort();
    this.detailAbort = null;
    this.selectedRunID = null;
  }

  // refreshOpenPipelineDetail re-fetches the currently open run on
  // each background poll tick so the drawer stays live without
  // forcing the user to close+reopen. No-op when nothing is open.
  // Called from fetchAll() so it shares the 15s cadence. Reuses the
  // open run's abort controller so closing mid-refresh cancels it too.
  async refreshOpenPipelineDetail(): Promise<void> {
    const id = this.selectedRunID;
    if (!id) return;
    await this.fetchPipelineDetail(id, this.detailAbort?.signal);
  }

  private async fetchPipelineDetail(runID: string, signal?: AbortSignal): Promise<void> {
    try {
      const detail = await this.getJSON<PipelineRunDetail>(
        `/api/mills/pipeline/runs/${encodeURIComponent(runID)}`,
        { signal },
      );
      if (!detail) {
        // Treat a null body as "not found" — surface as error so the
        // drawer can show a retry instead of an empty pane.
        this.pipelineDetailByRun = {
          ...this.pipelineDetailByRun,
          [runID]: { status: 'error', message: 'run not found' },
        };
        return;
      }
      // Older operators encode empty stage/gate sets as `null` (Go nil
      // slice); a live run has no gate outcomes until its first stage
      // completes, so that's the COMMON case, and `[...detail.gates]` on
      // null threw inside a $derived — killing the drawer's whole effect
      // tree ("Loading…" forever, close button dead). Normalise here so
      // the operator's encoding can never wedge the UI again.
      detail.stages ??= [];
      detail.gates ??= [];
      this.pipelineDetailByRun = {
        ...this.pipelineDetailByRun,
        [runID]: { status: 'loaded', detail },
      };
    } catch (e) {
      // A deliberate cancel (drawer closed / row switched) is not an error —
      // the drawer is already gone, so leave the cache untouched.
      if (e instanceof DOMException && e.name === 'AbortError') return;
      const message = e instanceof Error ? e.message : String(e);
      this.pipelineDetailByRun = {
        ...this.pipelineDetailByRun,
        [runID]: { status: 'error', message },
      };
    }
  }

  // --- Backlog drilldown (mirrors the pipeline-run drawer above) ---------

  // currentBacklogDetail is the derived load-state for the selected backlog
  // item; null when nothing is open so the drawer renders off one signal.
  // (Named distinctly from the openBacklogDetail() method below — a getter
  // and method sharing a name would collide as duplicate class members.)
  get currentBacklogDetail(): BacklogDetailLoadState | null {
    if (!this.selectedBacklogID) return null;
    return this.backlogDetailByID[this.selectedBacklogID] ?? { status: 'idle' };
  }

  openBacklogDetail(id: string): void {
    if (!id) return;
    this.backlogAbort?.abort();
    this.backlogAbort = new AbortController();
    this.selectedBacklogID = id;
    // Untracked for the same reason as openRunDetail above — callers open
    // this drawer from tracking $effects (WarpsPanel's router sync).
    untrack(() => {
      const cached = this.backlogDetailByID[id];
      if (!cached || cached.status === 'idle' || cached.status === 'error') {
        this.backlogDetailByID = {
          ...this.backlogDetailByID,
          [id]: { status: 'loading' },
        };
      }
    });
    void this.fetchBacklogDetail(id, this.backlogAbort.signal);
  }

  closeBacklogDetail(): void {
    this.backlogAbort?.abort();
    this.backlogAbort = null;
    this.selectedBacklogID = null;
  }

  async refreshOpenBacklogDetail(): Promise<void> {
    const id = this.selectedBacklogID;
    if (!id) return;
    await this.fetchBacklogDetail(id, this.backlogAbort?.signal);
  }

  // pipelineRunsForBacklog returns every known run (active + history)
  // spawned for a backlog item, newest-first. This is the load-bearing
  // cross-link in the drawer: "why is this item escalated?" → its runs.
  pipelineRunsForBacklog(backlogID: string): PipelineRun[] {
    if (!backlogID) return [];
    const seen = new Set<string>();
    const out: PipelineRun[] = [];
    for (const r of [...this.pipelineRuns, ...this.pipelineHistory]) {
      if (r.BacklogID !== backlogID || seen.has(r.ID)) continue;
      seen.add(r.ID);
      out.push(r);
    }
    out.sort((a, b) => (b.StartedAt ?? '').localeCompare(a.StartedAt ?? ''));
    return out;
  }

  // --- Mill-floor views (Warps · Shuttles · Sparks · Bolts) --------------
  //
  // Additive scaffolding for the mill-floor spine (spec S0). Every member
  // here is a pure, []-safe derivation over already-fetched state — the four
  // views ship with ZERO new backend. Kept as one contiguous additive block
  // so it rebases cleanly against concurrent poll-handler changes.

  // Terminal runs (done/merged/escalated) shared by Sparks + Bolts so those
  // two views don't run independent archive fetch loops. Off fetchAll — an
  // operator who never opens either view pays nothing.
  archiveRuns = $state<PipelineRun[]>([]);

  // True once an archive pull has landed. The spine's bolt/spark tallies are
  // derived from archiveRuns, so a view that never refreshes the archive
  // (Warps, Shuttles) would render "0 bolts / 0 sparks" beside a Sparks view
  // reading the same store. fetchAll primes the archive once off this flag so
  // the spine can't drift; Sparks/Bolts keep their own 30s refresh cadence.
  private archivePrimed = false;

  // refreshArchiveRuns pulls the terminal-run window via the existing
  // fetchArchiveRuns (which already returns `?? []`) and assigns it. On a
  // transient failure it leaves the last-good archive in place; the owning
  // view surfaces its own error, so this never blanks good data mid-poll.
  async refreshArchiveRuns(limit = 500): Promise<void> {
    try {
      this.archiveRuns = (await this.fetchArchiveRuns(limit)) ?? [];
      this.archivePrimed = true;
    } catch {
      // Keep the last-good archive; Sparks/Bolts own error display.
    }
  }

  async fetchRelaunchCandidates(): Promise<void> {
    this.relaunchCandidatesLoading = true;
    try {
      const raw = await this.getJSON<unknown>('/api/mills/escalations/relaunch-candidates');
      if (!Array.isArray(raw)) throw new Error('invalid relaunch-candidates response');
      this.relaunchCandidates = raw.map((value) => {
        const row = (value && typeof value === 'object' ? value : {}) as RelaunchCandidateWire;
        return {
          backlogId: typeof row.ID === 'string' ? row.ID : '',
          title: typeof row.Title === 'string' ? row.Title : '',
          escalationClass: typeof row.EscalationClass === 'string' ? row.EscalationClass : '',
          failureClass: typeof row.FailureClass === 'string' ? row.FailureClass : '',
          latestRunEndedAt: typeof row.EndedAt === 'string' ? row.EndedAt : null,
        };
      });
      this.relaunchCandidatesError = null;
    } catch (e) {
      this.relaunchCandidatesError = e instanceof Error ? e.message : String(e);
    } finally {
      this.relaunchCandidatesLoading = false;
    }
  }

  // backlogByPriority buckets the beam into warp lanes P0..P3 (+ other),
  // keeping only items actually strung and waiting (queued/paused) — a
  // running item already has a shuttle. Each bucket is always an array.
  get backlogByPriority(): Record<WarpBucket, BacklogItem[]> {
    const out: Record<WarpBucket, BacklogItem[]> = {
      P0: [],
      P1: [],
      P2: [],
      P3: [],
      other: [],
    };
    for (const item of this.backlog ?? []) {
      const state = (item.State ?? '').toLowerCase();
      if (state !== 'queued' && state !== 'paused') continue;
      const p = (item.Priority ?? '').toUpperCase();
      if (p === 'P0' || p === 'P1' || p === 'P2' || p === 'P3') {
        out[p as WarpPriority].push(item);
      } else {
        out.other.push(item);
      }
    }
    return out;
  }

  // strungCount is how many items are actually on the beam — the sum of the
  // warp buckets, not how long the raw backlog array is. The Warps header and
  // the mills sub-tab badge both read it so the number always describes the
  // bands rendered beneath it (an all-merged backlog is a bare beam).
  get strungCount(): number {
    let n = 0;
    for (const items of Object.values(this.backlogByPriority)) n += items.length;
    return n;
  }

  // activeShuttleCount is a shuttle in flight: a run that is neither terminal
  // nor waiting on a human. Single definition shared by the spine node, the
  // Shuttles header, and the sub-tab badge so the three can never disagree.
  get activeShuttleCount(): number {
    return (this.pipelineRuns ?? []).filter((r) => {
      const s = (r.State ?? '').toLowerCase();
      return s !== 'escalated' && s !== 'paused' && s !== 'done' && s !== 'merged';
    }).length;
  }

  // escalatedRuns unions active escalations/holds (escalated/paused) with
  // archived terminal holds, de-duped by ID (active wins). Feeds the Sparks
  // view.
  get escalatedRuns(): PipelineRun[] {
    const seen = new Set<string>();
    const out: PipelineRun[] = [];
    for (const r of this.pipelineRuns ?? []) {
      const s = (r.State ?? '').toLowerCase();
      if ((s === 'escalated' || s === 'paused') && !seen.has(r.ID)) {
        seen.add(r.ID);
        out.push(r);
      }
    }
    for (const r of this.archiveRuns ?? []) {
      const s = (r.State ?? '').toLowerCase();
      if ((s === 'escalated' || s === 'paused') && !seen.has(r.ID)) {
        seen.add(r.ID);
        out.push(r);
      }
    }
    return out;
  }

  // boltRuns are the archived terminal runs that wound onto the take-up roll
  // (done/merged). Feeds the Bolts view.
  get boltRuns(): PipelineRun[] {
    return (this.archiveRuns ?? []).filter((r) => isBoltState(r.State));
  }

  // lineageFor threads one run's strand (warp → stages → terminal bolt/spark)
  // by delegating to the pure builder with the co-fetched backlog for the
  // warp join. `reasons` optionally enriches a terminal spark when the caller
  // already has the run's failing gates.
  lineageFor(run: PipelineRun, reasons?: string[]): LineageSegment[] {
    return buildLineage(run, this.backlog, reasons);
  }

  // millFloorSpine assembles the floor-nav ribbon from the same derivations
  // the four views read, so the spine can never drift from them.
  get millFloorSpine(): LineageSegment[] {
    return spineSegments({
      backlogByPriority: this.backlogByPriority,
      activeShuttles: this.activeShuttleCount,
      bolts: this.boltRuns.length,
      sparks: this.escalatedRuns.length,
    });
  }

  private async fetchBacklogDetail(id: string, signal?: AbortSignal): Promise<void> {
    try {
      const detail = await this.getJSON<BacklogItemDetail>(
        `/api/mills/backlog/${encodeURIComponent(id)}`,
        { signal },
      );
      if (!detail) {
        this.backlogDetailByID = {
          ...this.backlogDetailByID,
          [id]: { status: 'error', message: 'backlog item not found' },
        };
        return;
      }
      this.backlogDetailByID = {
        ...this.backlogDetailByID,
        [id]: { status: 'loaded', detail },
      };
    } catch (e) {
      if (e instanceof DOMException && e.name === 'AbortError') return;
      const message = e instanceof Error ? e.message : String(e);
      this.backlogDetailByID = {
        ...this.backlogDetailByID,
        [id]: { status: 'error', message },
      };
    }
  }

  // loadDebate fetches the per-round transcript for one council run.
  // Cache-on-success: subsequent calls for the same id return without
  // network. Errors are surfaced via debateByRun[id].status === 'error'
  // so the panel can show a retry affordance instead of a silent fail.
  // The 'idle' / 'loading' transitions are explicit so the panel can
  // distinguish "never tried" from "in flight".
  async loadDebate(runID: string): Promise<void> {
    if (!runID) return;
    const cached = this.debateByRun[runID];
    if (cached && (cached.status === 'loaded' || cached.status === 'loading')) {
      return;
    }
    this.debateByRun = { ...this.debateByRun, [runID]: { status: 'loading' } };
    try {
      const rounds =
        (await this.getJSON<CouncilDebateRound[]>(
          `/api/mills/council/runs/${encodeURIComponent(runID)}/debate`,
        )) ?? [];
      this.debateByRun = {
        ...this.debateByRun,
        [runID]: { status: 'loaded', rounds },
      };
    } catch (e) {
      const message = e instanceof Error ? e.message : String(e);
      this.debateByRun = {
        ...this.debateByRun,
        [runID]: { status: 'error', message },
      };
    }
  }

  // postJSON is the mutation counterpart to getJSON. It surfaces 503 the
  // same way (so the disabled flag flips) but otherwise treats any
  // non-2xx as an error to surface in the UI. Body is JSON-encoded; pass
  // {} when the endpoint takes no payload.
  //
  // Every mutation route it targets is HUD-admin-gated (handleProxyAdminPost →
  // requireAdminToken) BEFORE the proxy forwards to the operator, so the request
  // must carry the HUD admin token. It goes through adminFetch (X-Admin-Token
  // from the Labs access bar) with requireToken:true, which fails fast with a
  // clear "requires an admin token" message when the bar is empty instead of
  // firing a tokenless request that the HUD gate 401s — the "handoff of slices
  // to mill returned 401 on the backlog endpoint" bug.
  private async postJSON<T>(path: string, body: unknown): Promise<T | null> {
    const res = await adminFetch(path, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify(body ?? {}),
      requireToken: true,
      action: 'This Mills action',
    });
    if (res.status === 503) {
      this.disabled = true;
      throw new Error('mills proxy: 503 (operator not configured)');
    }
    if (res.status === 401 || res.status === 403) {
      throw new Error(
        `${path}: ${res.status} (admin token missing or invalid — set it in the Labs access bar)`,
      );
    }
    if (!res.ok) {
      throw new Error(`${path}: ${res.status}`);
    }
    const text = await res.text();
    if (!text) return null;
    return JSON.parse(text) as T;
  }

  // fetchPolicyProposals refreshes the pending-proposals list. Called
  // from fetchAll() so the panel piggybacks on the existing 15s poll.
  async fetchPolicyProposals(state: string = 'pending'): Promise<void> {
    if (untrack(() => this.disabled)) return;
    try {
      const list = await this.getJSON<PolicyProposal[]>(
        `/api/mills/policy/proposals?state=${encodeURIComponent(state)}`,
      );
      this.policyProposals = list ?? [];
    } catch (e) {
      // Don't pollute store.error; proposals failures shouldn't blank
      // the rest of the Mills UI. Console is enough for triage.
      // eslint-disable-next-line no-console
      console.warn('fetchPolicyProposals failed', e);
    }
  }

  // fetchCostPreview is fire-and-store. Returns the estimate for callers
  // that want it inline; also caches into costPreviews keyed by
  // backlog_id so derived views can render without their own state.
  async fetchCostPreview(backlogID: string): Promise<CostEstimate | null> {
    if (untrack(() => this.disabled) || !backlogID) return null;
    try {
      const est = await this.getJSON<CostEstimate>(
        `/api/mills/cost-preview?backlog_id=${encodeURIComponent(backlogID)}`,
      );
      if (est) {
        this.costPreviews = { ...this.costPreviews, [backlogID]: est };
      }
      return est ?? null;
    } catch {
      return null;
    }
  }

  async applyPolicyProposal(id: number): Promise<void> {
    await this.postJSON(`/api/mills/policy/proposals/${id}/apply`, {});
    await this.fetchPolicyProposals();
  }

  async rejectPolicyProposal(id: number): Promise<void> {
    await this.postJSON(`/api/mills/policy/proposals/${id}/reject`, {});
    await this.fetchPolicyProposals();
  }

  // runCouncil fires an ad-hoc council pass via the operator's
  // POST /api/mills/council/run endpoint. The HUD proxy attaches the
  // admin bearer before forwarding so the browser never sees the token.
  // Returns true on success so callers can drive their own UI feedback.
  async runCouncil(reason: string = 'hud'): Promise<boolean> {
    await this.postJSON(`/api/mills/council/run`, { trigger: 'manual', reason });
    await this.fetchAll();
    return true;
  }

  // dryrunCouncil runs a council pass against a scratch DB (no commits,
  // no backlog writes) via POST /api/mills/council/dryrun. Used by the
  // Council panel to preview what the current ensemble would propose
  // before committing to a real run. Does not refresh the live stores.
  async dryrunCouncil(reason: string = 'hud'): Promise<boolean> {
    await this.postJSON(`/api/mills/council/dryrun`, { trigger: 'manual', reason });
    return true;
  }

  // setKillSwitch flips global autonomy via a GitOps auto-PR. The operator
  // opens an MR against platform/gitops flipping policy `enabled:` (it does
  // NOT write through — Flux owns the ConfigMap). action is
  // 'pause' | 'resume' | 'toggle'. Returns the MR info so the caller can
  // link it. Does not refresh stores: the change only takes effect after
  // the MR is merged and Flux reconciles. The HUD proxy injects the admin
  // bearer; the browser never handles a token.
  async setKillSwitch(
    action: 'pause' | 'resume' | 'toggle',
    reason: string = 'hud-overview',
  ): Promise<KillSwitchResult | null> {
    return this.postJSON<KillSwitchResult>('/api/mills/policy/kill-switch', { action, reason });
  }

  // escalateRun force-escalates a stuck pipeline run via
  // POST /api/mills/pipeline/runs/{id}/escalate. The reconciler does not
  // auto-retry escalated items, so the operator owns the next move.
  async escalateRun(runID: string, reason: string = 'manual escalation'): Promise<boolean> {
    await this.postJSON(`/api/mills/pipeline/runs/${encodeURIComponent(runID)}/escalate`, { reason });
    await this.fetchAll();
    return true;
  }

  async pauseRun(runID: string, reason: string): Promise<boolean> {
    await this.postJSON(`/api/mills/pipeline/runs/${encodeURIComponent(runID)}/pause`, { reason });
    await this.fetchAll();
    return true;
  }

  // startPipeline kicks off a pipeline run for a backlog item via
  // POST /api/mills/pipeline/runs/{backlog_id}/start. Surfaced from the
  // backlog drawer so an operator can act on an item without leaving the
  // detail view. Refreshes so the new run appears in the cross-link list.
  //
  // opts.requeue appends ?requeue=1: the operator flips an escalated item
  // back to queued before starting, so an explicit human re-run of a
  // previously escalated item (e.g. re-handing a plan to Mills) doesn't
  // dead-end on 409 "state is escalated".
  async startPipeline(backlogID: string, opts?: { requeue?: boolean }): Promise<boolean> {
    const qs = opts?.requeue ? '?requeue=1' : '';
    await this.postJSON(`/api/mills/pipeline/runs/${encodeURIComponent(backlogID)}/start${qs}`, {});
    await this.fetchAll();
    return true;
  }

  // requeuePipelineRun re-runs an escalated backlog item via the operator's
  // admin start endpoint with ?requeue=1 (POST
  // /api/mills/pipeline/runs/{backlog_id}/start?requeue=1). Unlike
  // startPipeline it returns a normalized RequeueOutcome instead of throwing,
  // so the run drawer can render nuanced inline feedback (started / conflict /
  // forbidden / error) — notably the ghost-spark "state is merged" 409 that
  // reads as already-done. Refreshes the run list on a successful start.
  //
  // Token attachment mirrors postJSON: adminFetch carries the HUD admin token
  // (X-Admin-Token) and fails fast — before firing — when the Labs bar is empty
  // and there's no Cloudflare Access identity; that pre-flight throw is mapped
  // to the same admin-token hint a 403 reports.
  async requeuePipelineRun(backlogID: string): Promise<RequeueOutcome> {
    const id = backlogID.trim();
    if (!id) {
      return { kind: 'error', message: 'Requeue failed: missing backlog id' };
    }
    let res: Response;
    try {
      res = await adminFetch(
        `/api/mills/pipeline/runs/${encodeURIComponent(id)}/start?requeue=1`,
        {
          method: 'POST',
          headers: { 'content-type': 'application/json' },
          body: '{}',
          requireToken: true,
          action: 'Requeue',
        },
      );
    } catch (e) {
      const message = e instanceof Error ? e.message : String(e);
      // adminFetch throws before firing when the Labs bar has no token and
      // there's no Cloudflare Access identity — the same admin-token gate a 403
      // would report, so surface it as forbidden with the shared hint.
      if (/requires an admin token/i.test(message)) {
        return { kind: 'forbidden', message };
      }
      return { kind: 'error', message: `Requeue failed: ${message}` };
    }
    if (res.status === 503) {
      this.disabled = true;
    }
    // 2xx/4xx bodies are JSON (pipelineStartResponse); 404/500/503 come back as
    // plain-text http.Error bodies, so fall back to the raw text. The body read
    // itself can reject (network drop mid-stream) — that must resolve to an
    // error outcome, not break the no-throw contract and wedge the drawer's
    // "Requeuing…" state.
    let parsedBody: unknown = '';
    try {
      const text = await res.text();
      parsedBody = text;
      if (text) {
        try {
          parsedBody = JSON.parse(text);
        } catch {
          parsedBody = text;
        }
      }
    } catch (e) {
      const message = e instanceof Error ? e.message : String(e);
      return { kind: 'error', message: `Requeue failed: ${message}` };
    }
    const outcome = normalizeRequeueResponse(res.status, parsedBody);
    if (outcome.kind === 'started') {
      await this.fetchAll();
    }
    return outcome;
  }

  // createBacklog upserts a backlog item via POST /api/mills/backlog. Used to
  // run a Plan in Mills when it isn't already born-linked to a backlog item:
  // the HUD builds an item from the plan (deterministic id => idempotent upsert)
  // and then starts its pipeline. Returns the persisted item (carrying its id).
  async createBacklog(item: Record<string, unknown>): Promise<{ id?: string } | null> {
    const res = await this.postJSON<{ id?: string }>('/api/mills/backlog', item);
    await this.fetchAll();
    return res;
  }

  // fetchPipelineHistory loads finished runs (done / escalated / paused),
  // newest-first, for the Pipelines "History" view via
  // GET /api/mills/pipeline/runs?state=terminal. Separate from the live
  // active-run poll; a failure is held in historyError (local to the
  // history view) so it doesn't red-flag the whole panel. A 503 means the
  // operator is unconfigured — fetchAll already surfaces that via disabled,
  // so we swallow it here to avoid a duplicate error line.
  // fetchDemandLog loads the council's merged-work suppressions — the
  // demand-side "declined with reasons" rows the factory board renders.
  // Best-effort like the history fetch: an unconfigured operator empties
  // the log instead of erroring the floor.
  async fetchDemandLog(window = '24h'): Promise<void> {
    try {
      const resp = await this.getJSON<{ rows: DemandLogRow[] }>(
        `/api/mills/demand-log?window=${encodeURIComponent(window)}`,
      );
      this.demandLog = resp?.rows ?? [];
    } catch {
      this.demandLog = [];
    }
  }

  async fetchPipelineHistory(limit = 50): Promise<void> {
    this.historyLoading = true;
    this.historyError = null;
    try {
      const runs = await this.getJSON<PipelineRun[]>(
        `/api/mills/pipeline/runs?state=terminal&limit=${encodeURIComponent(String(limit))}`,
      );
      this.pipelineHistory = runs ?? [];
    } catch (e) {
      const msg = e instanceof Error ? e.message : String(e);
      if (msg.includes('503') || msg.toLowerCase().includes('not configured')) {
        this.pipelineHistory = [];
        this.historyError = null;
      } else {
        this.historyError = msg;
      }
    } finally {
      this.historyLoading = false;
    }
  }

  // fetchArchiveRuns returns up to `limit` terminal runs from the
  // operator's 7-day default window WITHOUT storing them. The Factory
  // loom diffs pipelineHistory into weave events, so parking a large
  // archive fetch there would flood the shuttle with hundreds of phantom
  // rows — the caller (the bolt archive) owns the result instead.
  async fetchArchiveRuns(limit = 500): Promise<PipelineRun[]> {
    const [runs, backlog] = await Promise.all([
      this.getJSON<PipelineRun[]>(
        `/api/mills/pipeline/runs?state=terminal&limit=${encodeURIComponent(String(limit))}`,
      ),
      this.getJSON<BacklogItem[]>('/api/mills/backlog'),
    ]);
    const grades = new Map((backlog ?? []).map((item) => [item.ID, item]));
    return (runs ?? []).map((run) => {
      const item = grades.get(run.BacklogID);
      return item?.Grade
        ? { ...run, Grade: item.Grade, GradeNote: item.GradeNote ?? '' }
        : run;
    });
  }

  // gradeRun records a supervised taste signal and immediately reflects it in
  // the shared archive projection. The caller's modal snapshot performs the
  // same optimistic update; both restore their exact prior values on failure.
  async gradeRun(runID: string, grade: BoltGrade, note = ''): Promise<GradeRunResponse | null> {
    const index = this.archiveRuns.findIndex((run) => run.ID === runID);
    const previous = index >= 0 ? this.archiveRuns[index] : undefined;
    if (previous) {
      this.archiveRuns = this.archiveRuns.map((run) =>
        run.ID === runID ? { ...run, Grade: grade, GradeNote: note } : run,
      );
    }
    try {
      return await this.postJSON<GradeRunResponse>(
        `/api/mills/pipeline/runs/${encodeURIComponent(runID)}/grade`,
        { grade, note },
      );
    } catch (error) {
      if (previous) {
        this.archiveRuns = this.archiveRuns.map((run) => run.ID === runID ? previous : run);
      }
      throw error;
    }
  }

  // fetchArchiveRunDetail loads one run's stages+gates WITHOUT touching
  // the drilldown drawer cache — the shift report resolves failing gates
  // for its sparks in the background, and parking those fetches in
  // pipelineDetailByRun would make the drawer think those runs are open.
  // Returns null on any failure; the report degrades to "no gate detail".
  async fetchArchiveRunDetail(runID: string): Promise<PipelineRunDetail | null> {
    try {
      return await this.getJSON<PipelineRunDetail>(
        `/api/mills/pipeline/runs/${encodeURIComponent(runID)}`,
      );
    } catch {
      return null;
    }
  }

  // auditByMRIID looks up the pipeline run(s) that produced a given
  // merged MR (Loop B attribution) via GET /api/mills/pipeline/runs?mr_iid=.
  // Returns [] when no run matches the iid.
  async auditByMRIID(mrIID: number): Promise<PipelineRun[]> {
    const runs = await this.getJSON<PipelineRun[]>(
      `/api/mills/pipeline/runs?mr_iid=${encodeURIComponent(String(mrIID))}`,
    );
    return runs ?? [];
  }

  // --- Durable workflow step-log loaders (plan .loom/134 §S4b) -----------

  // loadWorkflowRuns pulls the most-recent workflow runs (summary shape),
  // newest-first, from GET /api/mills/workflow/runs. Bounded by the
  // operator's own limit (default 50, max 200). A 503 means the operator
  // is unconfigured — fetchAll already flips `disabled` for that, so we
  // swallow it here to keep the workflow surface calm rather than red. Any
  // open detail drawer is refreshed on the same call so it tracks in-flight
  // steps without a close+reopen.
  async loadWorkflowRuns(limit = 50): Promise<void> {
    this.workflowLoading = true;
    this.workflowError = null;
    try {
      const res = await this.getJSON<{ runs: WorkflowRun[] }>(
        `/api/mills/workflow/runs?limit=${encodeURIComponent(String(limit))}`,
      );
      this.workflowRuns = res?.runs ?? [];
      // Keep an open workflow-detail drawer live on the same cadence.
      if (this.selectedWorkflowID) {
        void this.loadWorkflowRunDetail(this.selectedWorkflowID);
      }
    } catch (e) {
      const msg = e instanceof Error ? e.message : String(e);
      if (msg.includes('503') || msg.toLowerCase().includes('not configured')) {
        this.workflowRuns = [];
        this.workflowError = null;
      } else {
        this.workflowError = msg;
      }
    } finally {
      this.workflowLoading = false;
    }
  }

  // workflowRunsForBacklog returns the imperative-lane runs claimed for one
  // backlog item, newest-first — the S7 analog of pipelineRunsForBacklog and
  // the BacklogDetail drawer's "why is this escalated?" cross-link for items
  // routed through ClaimWorkflowStart (which creates NO pipeline run).
  workflowRunsForBacklog(backlogID: string): WorkflowRun[] {
    if (!backlogID) return [];
    return this.workflowRuns
      .filter((r) => r.backlog_id === backlogID)
      .sort((a, b) => (b.started_at ?? '').localeCompare(a.started_at ?? ''));
  }

  // ensureWorkflowRunsLoaded lazily populates the workflow-runs list for
  // surfaces outside the Workflows panel (the BacklogDetail drawer). One
  // fetch per need — the panel's own poller keeps it fresh when visible.
  ensureWorkflowRunsLoaded(): void {
    // Untracked: BacklogDetail calls this from its open-drawer $effect. A
    // tracked read of workflowRuns/workflowLoading loops when the mill has
    // zero workflow runs — every completion writes a fresh empty array,
    // re-arming the length===0 guard at network round-trip cadence.
    const needsLoad = untrack(() => this.workflowRuns.length === 0 && !this.workflowLoading);
    if (needsLoad) {
      void this.loadWorkflowRuns();
    }
  }

  // openWorkflowDetail is the derived load-state for the selected run id;
  // null when nothing is open so the drawer renders off one signal.
  get openWorkflowDetail(): WorkflowDetailLoadState | null {
    if (!this.selectedWorkflowID) return null;
    return this.workflowDetailByID[this.selectedWorkflowID] ?? { status: 'idle' };
  }

  // openWorkflowRunDetail opens the step-timeline drawer for a run.
  // Cache-on-success: re-opening renders instantly from cache while a
  // fresh fetch refreshes in the background; 'loading' is only set when
  // there's no cached payload yet, so re-open doesn't flash empty.
  openWorkflowRunDetail(runID: string): void {
    if (!runID) return;
    this.selectedWorkflowID = runID;
    const cached = this.workflowDetailByID[runID];
    if (!cached || cached.status === 'idle' || cached.status === 'error') {
      this.workflowDetailByID = {
        ...this.workflowDetailByID,
        [runID]: { status: 'loading' },
      };
    }
    void this.loadWorkflowRunDetail(runID);
  }

  closeWorkflowDetail(): void {
    this.selectedWorkflowID = null;
  }

  // loadWorkflowRunDetail fetches one run + its step log from
  // GET /api/mills/workflow/runs/{id} (the {run, steps} payload) and caches
  // it keyed by id. A null body is treated as not-found so the drawer shows
  // a retry rather than an empty pane.
  async loadWorkflowRunDetail(runID: string): Promise<void> {
    if (!runID) return;
    try {
      const detail = await this.getJSON<WorkflowRunDetail>(
        `/api/mills/workflow/runs/${encodeURIComponent(runID)}`,
      );
      if (!detail || !detail.run) {
        this.workflowDetailByID = {
          ...this.workflowDetailByID,
          [runID]: { status: 'error', message: 'workflow run not found' },
        };
        return;
      }
      this.workflowDetailByID = {
        ...this.workflowDetailByID,
        [runID]: { status: 'loaded', detail },
      };
    } catch (e) {
      const message = e instanceof Error ? e.message : String(e);
      this.workflowDetailByID = {
        ...this.workflowDetailByID,
        [runID]: { status: 'error', message },
      };
    }
  }

  startPolling(intervalMs = 60000): void {
    this.stopPolling();
    void this.fetchAll();
    this.poller.start(intervalMs);
    // The operator status monitor pushes a `hud.mills` tick (~15s); treat it as
    // a refresh signal so all browsers re-pull on push without each polling.
    // Coalesced through the poller: fetchAll is 7 parallel GETs plus up to 5
    // follow-ups, and it used to run on every push forever, hidden tab or not.
    this.eventUnsubs.push(
      eventStore.on('hud.mills', () => this.poller.refreshCoalesced()),
    );
  }

  stopPolling(): void {
    this.poller.stop();
    if (this.recoveryTimer) {
      clearTimeout(this.recoveryTimer);
      this.recoveryTimer = null;
    }
    for (const unsub of this.eventUnsubs) unsub();
    this.eventUnsubs = [];
  }
}

export const millsStore = new MillsStore();
stalenessStore.register('mills', () => millsStore.isStale);
