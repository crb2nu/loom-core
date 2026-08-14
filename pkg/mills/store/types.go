package store

import (
	"strings"
	"time"
)

// BacklogState is the lifecycle state of a backlog item.
type BacklogState string

const (
	BacklogQueued    BacklogState = "queued"
	BacklogRunning   BacklogState = "running"
	BacklogMerged    BacklogState = "merged"
	BacklogEscalated BacklogState = "escalated"
	BacklogPaused    BacklogState = "paused"
	// BacklogRetired is the overseer groomer's terminal "closed without work"
	// state: duplicates and LLM-judged-obsolete items land here. Distinct from
	// merged (which success KPIs and HUD bolts count) and from escalated/paused
	// (which imply a human is coming back). Every state-specific query
	// (admission, ranker, sweeps, quiescence) selects other states, so a
	// retired item simply drops out of circulation; the transition's audit
	// event names why (and, for duplicates, the surviving canonical item).
	BacklogRetired BacklogState = "retired"
)

// Priority is the human-readable priority bucket for a backlog item.
type Priority string

const (
	P0 Priority = "P0"
	P1 Priority = "P1"
	P2 Priority = "P2"
	P3 Priority = "P3"
)

// Slice is one independent unit of work within a backlog item.
type Slice struct {
	Name         string   `json:"name"`
	Files        []string `json:"files"`
	Tests        []string `json:"tests"`
	ParallelWith []string `json:"parallel_with,omitempty"`
	// MissingFiles is the subset of Files that did not exist in the target
	// repo's tree at GroundingRevision when the item was emitted. A missing
	// file is not by itself an error — slices legitimately create new files —
	// but the set is what lets the post-implement fabricated_slice gate tell
	// a planned new file from a fabricated path after the implement run has
	// already created it (at which point repo existence proves nothing).
	MissingFiles []string `json:"missing_files,omitempty"`
	// Fabricated marks a slice whose EVERY declared non-glob file was absent
	// from the target repo at emit time: the plan cites files as if they exist,
	// but none do. Set by the plan-slice emitter's grounding hook; consumed by
	// the fabricated_slice gate to make an all-new-files diff a terminal
	// escalation (a retry re-implements the same fabricated plan and cannot
	// change the outcome).
	Fabricated bool `json:"fabricated,omitempty"`
	// GroundingRevision is the git revision the emit-time existence check ran
	// against, so a later audit can re-resolve exactly what tree the verdict
	// was based on (current-main checks are blind once the run mints the file).
	GroundingRevision string `json:"grounding_revision,omitempty"`
}

// SuccessCriteria captures the machine-checkable acceptance for a backlog item.
type SuccessCriteria struct {
	Tests       []string `json:"tests,omitempty"`
	Metrics     []string `json:"metrics,omitempty"`
	ManualCheck string   `json:"manual_check,omitempty"`
}

// Budget bounds the cost/turn/wall-clock for a backlog item's pipeline run.
type Budget struct {
	MaxCostUSD         float64 `json:"max_cost_usd,omitempty"`
	MaxTurns           int     `json:"max_turns,omitempty"`
	MaxPipelineMinutes int     `json:"max_pipeline_minutes,omitempty"`
}

// ItemPolicy carries per-item override of the global policy.
type ItemPolicy struct {
	RequireHumanReview    bool     `json:"require_human_review,omitempty"`
	AutoMerge             bool     `json:"auto_merge,omitempty"`
	ProtectedPathsTouched []string `json:"protected_paths_touched,omitempty"`

	// WorkflowTemplate / WorkflowTemplateVersion select a named imperative
	// workflow template from the CLOSED registry (S7,
	// pkg/mills/workflow/registry.go) instead of the default DAG pipeline.
	// Empty means DAG (unchanged pre-S7 behavior). An unknown name/version
	// fails closed at admission — it never falls back to a default program.
	// Selection is consulted ONLY when a run is created; runs execute their
	// frozen identity and are never re-routed by later edits to these fields.
	WorkflowTemplate        string `json:"workflow_template,omitempty"`
	WorkflowTemplateVersion string `json:"workflow_template_version,omitempty"`
	// WorkflowParams / WorkflowEnums are raw template parameters. Numerics are
	// clamped to the template's spec; enums must be in the template's closed
	// vocabulary. The clamped/validated results are frozen onto the run.
	WorkflowParams map[string]float64 `json:"workflow_params,omitempty"`
	WorkflowEnums  map[string]string  `json:"workflow_enums,omitempty"`
}

// BacklogItem is the canonical record for a unit of work in the mills.
type BacklogItem struct {
	ID             string
	GitLabIssueIID *int64
	Title          string
	Labels         []string
	State          BacklogState
	// ClaimVersion is the monotonic compare-and-swap version used when a queued
	// item is claimed for pipeline execution. It identifies aggregate pipeline
	// transitions; only the transactional pipeline-start kernel advances it.
	ClaimVersion int64
	// Revision is the monotonic row version used by every backlog mutation.
	// BacklogDAO.Put and ClaimPipelineStart both compare-and-swap it, preventing
	// stale metadata or policy writes from racing a pipeline admission.
	Revision     int64
	Priority     Priority
	SpecDoc      string
	SpecAnchor   string
	Success      SuccessCriteria
	Budget       Budget
	Policy       ItemPolicy
	Slices       []Slice
	Dependencies []string
	CouncilRunID *string
	// PlanID links this backlog item to a first-class Plan in the agent-context
	// store (plan store convergence). When set, the Mills agent resolves the
	// live plan + slices via agent_plan_get rather than re-reading SpecDoc.
	PlanID string
	// TargetProject is the repo this item's pipeline executes against
	// (implement + tests + mr/merge). Empty targets the operator's home repo
	// (backward compatible). A non-home value routes the run cross-repo and is
	// gated by CrossRepoPolicy.Enabled — the reconciler skips a cross-repo item
	// fail-closed while cross-repo execution is disabled, so it can never land
	// changes in the wrong repo. A bare name ("loom-flightdeck") or a
	// bucket-qualified path ("services/loom-flightdeck") both resolve.
	TargetProject string
	// Grade is the current operator taste signal. Grade history remains in
	// append-only bolt.graded events on the pipeline run subject.
	Grade      string
	GradeNote  string
	GradeActor string
	GradedAt   *time.Time
	CreatedBy  string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// PlanTasteAggregate is the merged-work taste rollup for one plan.
type PlanTasteAggregate struct {
	PlanID        string  `json:"plan_id"`
	Keep          int     `json:"keep"`
	Meh           int     `json:"meh"`
	Regret        int     `json:"regret"`
	Graded        int     `json:"graded"`
	Merged        int     `json:"merged"`
	RegretRate    float64 `json:"regret_rate"`
	GradeCoverage float64 `json:"grade_coverage"`
}

// TasteAggregates is the operator's single readout for plan taste and the
// epic's rolling coverage kill-test.
type TasteAggregates struct {
	Plans              []PlanTasteAggregate `json:"plans"`
	OverallGraded14d   int                  `json:"overall_graded_14d"`
	OverallMerged14d   int                  `json:"overall_merged_14d"`
	OverallCoverage14d float64              `json:"overall_coverage_14d"`
}

// RepoBase returns the canonical bare repo name for a project identifier:
// the last path segment, lowercased. "services/loom-core", "loom-core", and
// "LOOM-CORE" all normalize to "loom-core". Used to compare a per-item
// TargetProject against the operator's home project regardless of whether
// either is bucket-qualified.
func RepoBase(project string) string {
	p := strings.Trim(strings.TrimSpace(project), "/")
	if i := strings.LastIndex(p, "/"); i >= 0 {
		p = p[i+1:]
	}
	return strings.ToLower(p)
}

// SameRepo reports whether two project identifiers name the same repo,
// ignoring bucket prefix and case. Empty identifiers never match.
func SameRepo(a, b string) bool {
	ba, bb := RepoBase(a), RepoBase(b)
	return ba != "" && ba == bb
}

// CouncilTrigger identifies what caused a council run.
type CouncilTrigger string

const (
	CouncilTriggerCron     CouncilTrigger = "cron"
	CouncilTriggerRoadmap  CouncilTrigger = "roadmap"
	CouncilTriggerIncident CouncilTrigger = "incident"
	CouncilTriggerManual   CouncilTrigger = "manual"
)

// CouncilOutcome reports the final status of a council run.
type CouncilOutcome string

const (
	CouncilOutcomeRunning  CouncilOutcome = "running"
	CouncilOutcomeSuccess  CouncilOutcome = "success"
	CouncilOutcomePartial  CouncilOutcome = "partial"
	CouncilOutcomeError    CouncilOutcome = "error"
	CouncilOutcomeConflict CouncilOutcome = "conflict"
)

// ArtifactRef identifies one artifact emitted by a council run.
type ArtifactRef struct {
	Kind string `json:"kind"`
	Path string `json:"path,omitempty"`
	ID   string `json:"id,omitempty"`
}

// BacklogDeltas summarises the backlog mutations a council run intends.
type BacklogDeltas struct {
	Created []string `json:"created,omitempty"`
	Updated []string `json:"updated,omitempty"`
	Closed  []string `json:"closed,omitempty"`
}

// CouncilRun is one execution of the council ensemble.
type CouncilRun struct {
	ID              string
	Trigger         CouncilTrigger
	StartedAt       time.Time
	EndedAt         *time.Time
	Outcome         CouncilOutcome
	CostFrontierUSD float64
	CostLocalUSD    float64
	Artifacts       []ArtifactRef
	BacklogDeltas   BacklogDeltas
	Sidecar         map[string]any
	BranchName      string
	CommitSHA       string
	Notes           string
}

// CouncilStartLimits is the transactional budget/admission snapshot applied
// while claiming a Council run. Zero values are uncapped, matching policy
// budget semantics.
type CouncilStartLimits struct {
	MaxUSDPerRun      float64
	MaxUSDPerDay      float64
	MaxRunsPerDay     int
	MaxConcurrentRuns int
}

// CouncilBudgetReservation protects a conservative estimate between Council
// admission and terminal finalization. FinalizeCouncilRun releases it in the
// same transaction that replaces the estimate with the run's actual cost.
type CouncilBudgetReservation struct {
	ID          int64
	RunID       string
	ReservedUSD float64
	State       string
	CreatedAt   time.Time
	ExpiresAt   time.Time
	ReleasedAt  *time.Time
}

// PipelineState is the lifecycle state of a pipeline run.
type PipelineState string

const (
	PipelineQueued       PipelineState = "queued"
	PipelinePlanning     PipelineState = "planning"
	PipelineSlicing      PipelineState = "slicing"
	PipelineImplementing PipelineState = "implementing"
	PipelineTesting      PipelineState = "testing"
	PipelineReviewing    PipelineState = "reviewing"
	PipelineMR           PipelineState = "mr"
	PipelineCI           PipelineState = "ci"
	PipelineMerging      PipelineState = "merging"
	PipelineDone         PipelineState = "done"
	PipelineEscalated    PipelineState = "escalated"
	PipelinePaused       PipelineState = "paused"
)

// PipelineRun is one execution of the pipeline DAG for a backlog item.
//
// ParentRunID + Depth implement bounded recursion (Mills v2). Top-level runs
// have ParentRunID == nil and Depth == 0; sub-runs created via
// mills_pipeline_subrun_create increment Depth from their parent. The
// dispatcher rejects creation when Depth would exceed
// policy.pipeline.max_recursion_depth (default 2).
type PipelineRun struct {
	ID        string
	BacklogID string
	// AggregateVersion is the backlog ClaimVersion committed with this run.
	// It correlates the pipeline row, transition ledger, budget reservation,
	// workflow metadata, and dispatch intent born in the same transaction.
	AggregateVersion int64
	// Revision is the pipeline row's mutation compare-and-swap version. It is
	// independent of AggregateVersion and advances on every PutRun stage/state
	// rollup so duplicate runners cannot overwrite newer progress.
	Revision        int64
	Template        string
	State           PipelineState
	CurrentStage    string
	Attempts        int
	WorktreePath    string
	MRIID           *int64
	StartedAt       time.Time
	EndedAt         *time.Time
	CostUSD         float64
	ParentSessionID string
	ParentRunID     *string
	Depth           int
	// EscalationClass is the runner's historical ErrorClass spelling stamped on
	// escalated runs (for example "infra" or "config").
	EscalationClass string
	// FailureClass is the policy-facing failure taxonomy spelling emitted on
	// escalations (for example "infrastructure" or "configuration").
	FailureClass string
	// ExternalDependencyID and ExternalDependency identify a known upstream
	// dependency incident when the escalation evidence matches one.
	ExternalDependencyID string
	ExternalDependency   string
	// EscalationRetryable records whether retrying is policy-allowed for the
	// persisted failure class. Nil means the run predates the metadata columns
	// or escalated without a class marker.
	EscalationRetryable *bool
	// RetryExhausted marks a retryable failure that reached its bounded
	// auto-requeue cap and therefore requires escalation.
	RetryExhausted *bool
}

// PipelineStartLimits is the transactional budget/admission snapshot applied
// while claiming a backlog item. A zero value means uncapped, matching the
// existing policy budget semantics.
type PipelineStartLimits struct {
	MaxUSDPerRun      float64
	MaxUSDPerDay      float64
	MaxRunsPerDay     int
	MaxConcurrentRuns int
}

// PipelineBudgetReservation protects a pipeline estimate between claim and a
// terminal run transition. Active reservations participate in subsequent
// transactional budget checks; terminal PutRun calls release them atomically.
type PipelineBudgetReservation struct {
	ID          int64
	RunID       string
	BacklogID   string
	ReservedUSD float64
	State       string
	CreatedAt   time.Time
	ReleasedAt  *time.Time
}

// DispatchStatus is the delivery state of a committed pipeline-start intent.
type DispatchStatus string

const (
	DispatchPending    DispatchStatus = "pending"
	DispatchDelivered  DispatchStatus = "delivered"
	DispatchDeadLetter DispatchStatus = "dead_letter"
)

// PendingDispatch is a transactional outbox row. A reconciler may only invoke
// the PipelineStarter for rows returned by ListPendingDispatches, ensuring the
// intent is durable before any process-local goroutine is launched.
type PendingDispatch struct {
	ID               int64
	RunID            string
	BacklogID        string
	AggregateVersion int64
	Kind             string
	Status           DispatchStatus
	Attempts         int
	LastError        string
	NextAttemptAt    time.Time
	LeaseToken       string
	LeaseExpiresAt   *time.Time
	CreatedAt        time.Time
	UpdatedAt        time.Time
	DeliveredAt      *time.Time
	DeadLetteredAt   *time.Time
}

// DispatchRetryPolicy bounds start-intent retry pressure. A claimed delivery
// increments Attempts; failures are rescheduled with exponential backoff until
// MaxAttempts, when an untouched queued run is atomically dead-lettered.
type DispatchRetryPolicy struct {
	BaseDelay   time.Duration
	MaxDelay    time.Duration
	MaxAttempts int
}

// DispatchFailureResult describes the durable result of one token-guarded
// delivery failure.
type DispatchFailureResult struct {
	Attempts      int
	DeadLettered  bool
	NextAttemptAt *time.Time
}

// ClassifiedCIFailureSummary is the compact read model surfaced in Mills and
// council summaries for escalated CI-watch failures that carry classification
// metadata.
type ClassifiedCIFailureSummary struct {
	RunID                string    `json:"run_id"`
	BacklogID            string    `json:"backlog_id"`
	BacklogTitle         string    `json:"backlog_title,omitempty"`
	StartedAt            time.Time `json:"started_at"`
	Classifier           string    `json:"classifier,omitempty"`
	EscalationClass      string    `json:"escalation_class,omitempty"`
	FailureClass         string    `json:"failure_class,omitempty"`
	ExternalDependencyID string    `json:"external_dependency_id,omitempty"`
	ExternalDependency   string    `json:"external_dependency,omitempty"`
	Retryable            *bool     `json:"retryable,omitempty"`
	FreeRetry            *bool     `json:"free_retry,omitempty"`
	Terminal             *bool     `json:"terminal,omitempty"`
}

// EscalationEvidence is one escalated run's free-text failure evidence: the
// last non-empty stage log tail recorded for the run. Classified is true when
// the run already carries ANY escalation classification marker, so a reader can
// mine the unclassified rows while still evaluating a proposed signature
// against the whole window.
type EscalationEvidence struct {
	RunID      string    `json:"run_id"`
	BacklogID  string    `json:"backlog_id"`
	StartedAt  time.Time `json:"started_at"`
	Stage      string    `json:"stage,omitempty"`
	Evidence   string    `json:"evidence"`
	Classified bool      `json:"classified"`
}

// RunTerminalOutcome is the (run → what it finally did, at what price, on
// which MR) projection that ground-truth joins read. It stays a handful of
// scalar columns wide: a calibration window spans thousands of runs, and
// ListRecentTerminal's full-row read is hard-capped at 200 rows precisely
// because it rehydrates everything.
type RunTerminalOutcome struct {
	RunID     string        `json:"run_id"`
	BacklogID string        `json:"backlog_id"`
	State     PipelineState `json:"state"`
	// CostUSD is the run-level rollup (PipelineRun.CostUSD), which is what
	// per-configuration economics compares. Per-model attribution lives on
	// StageResult and is deliberately not folded in here.
	CostUSD float64 `json:"cost_usd"`
	// MRIID links the run to the merge request it produced, which is the only
	// reliable bridge from a run to the regression.attributed events keyed on
	// the regressed MR. Nil for runs that never opened one.
	MRIID *int64 `json:"mr_iid,omitempty"`
}

// StageOutcome captures whether one stage attempt succeeded.
type StageOutcome string

const (
	StageOutcomeSuccess  StageOutcome = "success"
	StageOutcomeGateFail StageOutcome = "gate_fail"
	StageOutcomeError    StageOutcome = "error"
)

// StageResult is one attempt to execute one stage of a pipeline run.
type StageResult struct {
	ID            int64
	PipelineRunID string
	Stage         string
	Attempt       int
	StartedAt     time.Time
	EndedAt       *time.Time
	Outcome       *StageOutcome
	SpawnID       string
	CostUSD       float64
	// Model + Backend attribute this attempt's cost to a model tier for the
	// telemetry roll-up (per-model economics). Both are optional: empty means
	// the worker did not surface its identity, and the aggregation buckets such
	// rows under "unknown". Persisted nullable via migration 013.
	Model     string
	Backend   string
	Artifacts map[string]any
	LogTail   string
}

// GateOutcomeKind reports whether a gate passed.
type GateOutcomeKind string

const (
	GateOutcomePass GateOutcomeKind = "pass"
	GateOutcomeFail GateOutcomeKind = "fail"
	GateOutcomeSkip GateOutcomeKind = "skip"
)

// GateOutcome is the persisted record of one gate evaluation.
type GateOutcome struct {
	ID            int64
	PipelineRunID string
	AfterStage    string
	GateName      string
	Outcome       GateOutcomeKind
	Reasons       []string
	JudgedBy      string
	EvaluatedAt   time.Time
}

// KPISnapshot is a rolled-up metric set persisted per reconcile tick.
type KPISnapshot struct {
	ID            int64
	SnapshotAt    time.Time
	WindowSeconds int
	Metrics       map[string]any
}

// EvalSubjectKind names what an EvalScore is judging.
type EvalSubjectKind string

const (
	EvalSubjectCouncilRun  EvalSubjectKind = "council_run"
	EvalSubjectPipelineRun EvalSubjectKind = "pipeline_run"
	EvalSubjectCrossRun    EvalSubjectKind = "cross_run"
)

// EvalScore is one judgment of a council run, pipeline run, or cross-run window.
type EvalScore struct {
	ID          int64
	SubjectKind EvalSubjectKind
	SubjectID   string
	Rubric      string
	Score       float64
	Breakdown   map[string]any
	JudgedBy    string
	EvaluatedAt time.Time
	Notes       string
}

// Event is an append-only audit / debug record.
type Event struct {
	ID          int64
	OccurredAt  time.Time
	Actor       string
	Kind        string
	SubjectKind string
	SubjectID   string
	Payload     map[string]any
}

// RoadmapIntent is one extracted theme/priority/constraint from ROADMAP.md.
type RoadmapIntent struct {
	ID                   int64
	Theme                string
	Priority             int
	Summary              string
	Constraints          map[string]any
	LastSeenInRoadmapSHA string
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

// ----- Mills v2 — Hierarchical Swarm types ---------------------------------

// Squad is the persistence-side mirror of a squad manifest YAML
// (`platform/gitops/k3s/mills/squads/<name>.yaml`). The squad loader
// writes into this table on boot + on fsnotify change.
type Squad struct {
	ID               string // PK = Name; kept as ID for symmetry with other DAOs
	Name             string
	Paths            []string
	Tests            []string
	Gates            map[string]any // {required:[…], advisory:[…]}
	Ensemble         map[string]any // editor / reviewers / judge
	BudgetShare      float64
	RecursionEnabled bool
	Enabled          bool
	LastLoadedSHA    string
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// SquadMemoryKind classifies a working-memory entry.
type SquadMemoryKind string

const (
	SquadMemoryMerge      SquadMemoryKind = "merge"
	SquadMemoryTechDebt   SquadMemoryKind = "tech_debt"
	SquadMemoryConvention SquadMemoryKind = "convention"
	SquadMemoryFollowup   SquadMemoryKind = "followup"
)

// SquadMemory is one append-on-merge working-memory entry. The weekly
// pruner drops rows with importance < 0.3 older than 30 days.
type SquadMemory struct {
	ID         int64
	SquadName  string
	Kind       SquadMemoryKind
	Title      string
	Body       string
	Refs       []string
	Importance float64
	CreatedAt  time.Time
	LastSeenAt time.Time
}

// SquadOutcomeKind reports how a squad-routed pipeline run resolved.
type SquadOutcomeKind string

const (
	SquadOutcomeMergedClean SquadOutcomeKind = "merged_clean"
	SquadOutcomeFailed      SquadOutcomeKind = "failed"
	SquadOutcomeSelfVetoed  SquadOutcomeKind = "self_vetoed"
)

// SquadOutcome is the per-run record the router consults to compute
// rolling success rate per (squad, path_class).
type SquadOutcome struct {
	ID              int64
	SquadName       string
	PathClass       string
	PipelineRunID   string
	Outcome         SquadOutcomeKind
	Grade           string
	CostUSD         float64
	DurationSeconds int64
	CreatedAt       time.Time
}

// AuditSubjectKind names what an AuditFinding row is judging.
type AuditSubjectKind string

const (
	AuditSubjectCouncilArtifact AuditSubjectKind = "council_artifact"
	AuditSubjectPipelineMerge   AuditSubjectKind = "pipeline_merge"
)

// AuditSeverity is the categorical severity emitted by the audit rubric.
type AuditSeverity string

const (
	AuditSeverityInfo     AuditSeverity = "info"
	AuditSeverityWarn     AuditSeverity = "warn"
	AuditSeverityCritical AuditSeverity = "critical"
)

// AuditFinding is one independent adversarial verdict on a council artifact
// or a pipeline merge. The auditor pool is captured per row so policy
// rotations are auditable in retrospect.
type AuditFinding struct {
	ID            int64
	SubjectKind   AuditSubjectKind
	SubjectID     string
	Severity      AuditSeverity
	RubricID      string
	SurvivalScore float64
	Findings      []map[string]any
	AuditorPool   []map[string]any
	CostUSD       float64
	CreatedAt     time.Time
}

// CrossRepoState is the lifecycle state of an atomic cross-repo run.
type CrossRepoState string

const (
	CrossRepoPlanning   CrossRepoState = "planning"
	CrossRepoOpen       CrossRepoState = "open"
	CrossRepoGatesGreen CrossRepoState = "gates_green"
	CrossRepoMerging    CrossRepoState = "merging"
	CrossRepoMerged     CrossRepoState = "merged"
	CrossRepoReverted   CrossRepoState = "reverted"
	CrossRepoFailed     CrossRepoState = "failed"
)

// CrossRepoRepoEntry is one repo's slice of an atomic cross-repo run.
type CrossRepoRepoEntry struct {
	ProjectID  int64  `json:"project_id"`
	RepoName   string `json:"repo_name,omitempty"`
	Branch     string `json:"branch"`
	MRIID      *int64 `json:"mr_iid,omitempty"`
	CIStatus   string `json:"ci_status,omitempty"`
	GateStatus string `json:"gate_status,omitempty"`
}

// CrossRepoRun coordinates a backlog item that spans multiple repos.
type CrossRepoRun struct {
	ID                string
	BacklogItemID     string
	Repos             []CrossRepoRepoEntry
	State             CrossRepoState
	AtomicityStrategy string
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// DebateRole names which step in a Council Debate round emitted this row.
type DebateRole string

const (
	DebateRoleEditorProposes    DebateRole = "editor_proposes"
	DebateRoleReviewerCritiques DebateRole = "reviewer_critiques"
	DebateRoleModeratorDecision DebateRole = "moderator_decision"
	DebateRoleEditorRevises     DebateRole = "editor_revises"
)

// CouncilDebateRound is one entry in a Council Debate transcript.
// ArtifactDeltas references path + line range pairs so the sidecar
// stays small even on long debates.
type CouncilDebateRound struct {
	ID             int64
	CouncilRunID   string
	RoundIndex     int
	Role           DebateRole
	CostUSD        float64
	Summary        string
	ArtifactDeltas []map[string]any
	CreatedAt      time.Time
}

// PolicyProposalKind classifies an adaptive-policy suggestion.
type PolicyProposalKind string

const (
	PolicyProposalRelax          PolicyProposalKind = "relax"
	PolicyProposalTighten        PolicyProposalKind = "tighten"
	PolicyProposalRotateEnsemble PolicyProposalKind = "rotate_ensemble"
)

// PolicyProposalState tracks the lifecycle of one adaptive proposal.
type PolicyProposalState string

const (
	PolicyProposalPending      PolicyProposalState = "pending"
	PolicyProposalAppliedHuman PolicyProposalState = "applied_human"
	PolicyProposalAppliedAuto  PolicyProposalState = "applied_auto"
	PolicyProposalRejected     PolicyProposalState = "rejected"
	PolicyProposalReverted     PolicyProposalState = "reverted"
)

// PolicyProposal is one machine-emitted suggestion to relax, tighten, or
// rotate a policy element. Rationale cites kpi_snapshots / eval_scores /
// audit_findings / gate_outcomes; the .loom/mills/policy_proposals/<date>.md
// markdown is the human-facing copy.
type PolicyProposal struct {
	ID             int64
	ProposalDate   string // YYYY-MM-DD
	Kind           PolicyProposalKind
	Target         string
	Diff           string
	Rationale      string
	State          PolicyProposalState
	AppliedAt      *time.Time
	RevertDeadline *time.Time
	CreatedAt      time.Time
}

// ----- Layer-2 durable workflow step/event journal -------------------------
//
// These types back migration 004 (workflow_runs + workflow_steps). They are
// the stable persistence layer for the Mills durable workflow engine; the
// imperative runtime that writes them ships in a later slice.
//
// DUAL SOURCE-OF-TRUTH: legacy `dag` runs do NOT write workflow_steps; only
// `imperative` runs do. workflow_steps is the source of truth for imperative
// resume — the runtime replays the append-only step log. The generic Event
// log (events table) stays advisory (audit/debug), never used for resume.

// WorkflowEngine is the IMMUTABLE discriminator for how a workflow run
// executes. It is fixed at creation and must never change for a given run id.
type WorkflowEngine string

const (
	// WorkflowEngineDAG is the legacy pipeline DAG. Runs with this engine do
	// NOT write workflow_steps; they exist for symmetry / cross-reference.
	WorkflowEngineDAG WorkflowEngine = "dag"
	// WorkflowEngineImperative is the durable imperative runtime. Only these
	// runs append to workflow_steps.
	WorkflowEngineImperative WorkflowEngine = "imperative"
)

// WorkflowRunState is the lifecycle state of a workflow run.
type WorkflowRunState string

const (
	WorkflowRunRunning     WorkflowRunState = "running"
	WorkflowRunPaused      WorkflowRunState = "paused"
	WorkflowRunDone        WorkflowRunState = "done"
	WorkflowRunEscalated   WorkflowRunState = "escalated"
	WorkflowRunError       WorkflowRunState = "error"
	WorkflowRunQuarantined WorkflowRunState = "quarantined"
)

// WorkflowStepStatus is the lifecycle status of one journaled step.
//
// Record-before-result invariant: a step is appended Pending BEFORE its
// effect, then updated to a terminal status (Success/Error/GateFail/Skipped)
// once the effect resolves.
type WorkflowStepStatus string

const (
	WorkflowStepPending  WorkflowStepStatus = "pending"
	WorkflowStepSuccess  WorkflowStepStatus = "success"
	WorkflowStepError    WorkflowStepStatus = "error"
	WorkflowStepGateFail WorkflowStepStatus = "gate_fail"
	WorkflowStepSkipped  WorkflowStepStatus = "skipped"
)

// IsTerminal reports whether s is a final (non-pending) step status.
func (s WorkflowStepStatus) IsTerminal() bool {
	return s != "" && s != WorkflowStepPending
}

// WorkflowCostSource records the provenance of a step's cost figure.
type WorkflowCostSource string

const (
	WorkflowCostReal        WorkflowCostSource = "real"
	WorkflowCostEstimated   WorkflowCostSource = "estimated"
	WorkflowCostUnavailable WorkflowCostSource = "unavailable"
)

// WorkflowEventType enumerates the kinds of effect/event a journaled step
// records. The store treats these as opaque strings; the constants exist so
// the runtime and tests share a single source of truth.
type WorkflowEventType string

const (
	WorkflowEventSpawnRequested               WorkflowEventType = "spawn_requested"
	WorkflowEventSpawnResult                  WorkflowEventType = "spawn_result"
	WorkflowEventSpawnResumed                 WorkflowEventType = "spawn_resumed"
	WorkflowEventGateEval                     WorkflowEventType = "gate_eval"
	WorkflowEventBudgetReserved               WorkflowEventType = "budget_reserved"
	WorkflowEventBudgetDebit                  WorkflowEventType = "budget_debit"
	WorkflowEventToolCall                     WorkflowEventType = "tool_call"
	WorkflowEventCtxNow                       WorkflowEventType = "ctx_now"
	WorkflowEventCtxUUID                      WorkflowEventType = "ctx_uuid"
	WorkflowEventParallelBranch               WorkflowEventType = "parallel_branch"
	WorkflowEventParallelJoin                 WorkflowEventType = "parallel_join"
	WorkflowEventLoopIter                     WorkflowEventType = "loop_iter"
	WorkflowEventStepCacheHit                 WorkflowEventType = "step_cache_hit"
	WorkflowEventStepBudgetExhausted          WorkflowEventType = "step_budget_exhausted"
	WorkflowEventStepPaused                   WorkflowEventType = "step_paused"
	WorkflowEventStepResumed                  WorkflowEventType = "step_resumed"
	WorkflowEventStepNondeterminismQuarantine WorkflowEventType = "step_nondeterminism_quarantine"
	WorkflowEventWorkflowDone                 WorkflowEventType = "workflow_done"
)

// WorkflowRunBranchPrefix is the deterministic branch namespace every
// imperative run's agent() spawns work on ("mills-wf/" + run ID). Defined in
// the store so both the runtime's branch derivation and the terminal-settle
// escalation event (which points the human reviewer at the work product)
// share one source of truth.
const WorkflowRunBranchPrefix = "mills-wf/"

// WorkflowSelection is a FROZEN imperative template selection (S7): the
// immutable identity ClaimWorkflowStart stamps onto a new run. Produced by
// pkg/mills/workflow.ResolveItemSelection; defined here so the reconciler
// (pkg/mills) and the resolver (pkg/mills/workflow) can share it without an
// import cycle.
type WorkflowSelection struct {
	Engine             WorkflowEngine
	Template           string
	TemplateVersion    string
	InterpreterVersion string
	ParamsJSON         string
}

// WorkflowRun is one execution of a durable workflow. Engine is fixed at
// creation (see WorkflowEngine). WorkflowParams carries an opaque JSON params
// blob; the store never parses it.
type WorkflowRun struct {
	ID                 string
	BacklogID          string
	Engine             WorkflowEngine
	Template           string
	TemplateVersion    string
	InterpreterVersion string
	WorkflowParams     string // opaque JSON string ("" => NULL)
	State              WorkflowRunState
	PausedAt           *time.Time
	ResumedAt          *time.Time
	StartedAt          *time.Time
	EndedAt            *time.Time
	CostUSD            float64
	ParentSessionID    string
}

// ----- Async Spinning-Room spins -------------------------------------------
//
// These back migration 007 (spin_runs). An async spin (POST
// /api/mills/spin/async) returns 202 + a spin id immediately and runs in a
// detached operator goroutine; this row is the pollable status record so a
// client never holds a connection open for a minutes-long frontier spin.

// SpinStatus is the lifecycle state of an async spin run.
type SpinStatus string

const (
	// SpinPending: accepted, queued behind the concurrency semaphore, not
	// yet started. This is also the state a row is inserted in.
	SpinPending SpinStatus = "pending"
	// SpinRunning: the goroutine acquired a slot and is spinning.
	SpinRunning SpinStatus = "running"
	// SpinSucceeded: at least one draft plan was authored (PlanIDs non-empty).
	SpinSucceeded SpinStatus = "succeeded"
	// SpinFailed: the spin errored (disabled room, unknown frame, editor/author
	// failure, or operator shutdown). Error carries the reason.
	SpinFailed SpinStatus = "failed"
	// SpinTimeout: the spin exceeded its per-request budget (context deadline).
	// Split from SpinFailed so a client can distinguish "took too long"
	// (retryable) from a hard error.
	SpinTimeout SpinStatus = "timeout"
)

// IsTerminal reports whether s is a final (non-pending, non-running) status.
func (s SpinStatus) IsTerminal() bool {
	return s == SpinSucceeded || s == SpinFailed || s == SpinTimeout
}

// SpinRun is one async Spinning-Room spin. Frames is the requested frame set
// (one entry for a plain spin, N for a competitive frames[] spin); PlanIDs is
// the 0..N draft plan ids the spin authored. Error carries the failure reason
// on a failed/timeout row, or a partial-failure summary on a competitive spin
// that succeeded on some frames but not others.
type SpinRun struct {
	ID          string     `json:"id"`
	Brief       string     `json:"brief"`
	Frames      []string   `json:"frames"`
	Priority    string     `json:"priority,omitempty"`
	Project     string     `json:"project,omitempty"`
	Namespace   string     `json:"namespace,omitempty"`
	Status      SpinStatus `json:"status"`
	PlanIDs     []string   `json:"plan_ids"`
	Error       string     `json:"error,omitempty"`
	Competitive bool       `json:"competitive"`
	StartedAt   time.Time  `json:"started_at"`
	EndedAt     *time.Time `json:"ended_at,omitempty"`
}

// WorkflowStep is one append-only journal entry within a workflow run.
//
// StepKey is an opaque structured key string minted by the runtime; the store
// treats it as a bare string and UNIQUE(run_id, step_key) gives idempotent
// append + replay. CallHash fingerprints the recorded call so a mismatch on an
// existing step_key (nondeterminism) is detectable by the caller. ResultBlob
// is an opaque JSON result set on completion ("" => NULL).
type WorkflowStep struct {
	ID             int64
	RunID          string
	StepKey        string
	EventType      WorkflowEventType
	CallHash       string
	IdempotencyKey string // "" => NULL
	Status         WorkflowStepStatus
	SpawnID        string // "" => NULL
	StartedAt      *time.Time
	EndedAt        *time.Time
	ResultBlob     string // opaque JSON ("" => NULL)
	CostUSD        float64
	CostSource     WorkflowCostSource // "" => NULL
	EffectCount    int
}
