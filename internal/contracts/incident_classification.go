package contracts

// CouncilCIIncidentClass is the Loom-owned, stable class taxonomy emitted by
// Mills council incident classification. Values are suitable for persistence,
// metrics, and cross-process JSON contracts.
type CouncilCIIncidentClass string

const (
	CouncilCIIncidentRepositoryRegression CouncilCIIncidentClass = "repository_regression"
	CouncilCIIncidentCIConfiguration      CouncilCIIncidentClass = "ci_configuration_regression"
	CouncilCIIncidentRunnerInfrastructure CouncilCIIncidentClass = "runner_infrastructure_incident"
	CouncilCIIncidentExternalDependency   CouncilCIIncidentClass = "external_dependency_incident"
	CouncilCIIncidentDependencyUpdate     CouncilCIIncidentClass = "dependency_update_ci_incident"
	CouncilCIIncidentFlakeOrTransient     CouncilCIIncidentClass = "flake_or_transient_dependency"
	CouncilCIIncidentBranchOrPlanHygiene  CouncilCIIncidentClass = "branch_or_plan_hygiene"
	CouncilCIIncidentUnclassified         CouncilCIIncidentClass = "unclassified"
)

// CouncilCIIncidentDisposition is the stable remediation decision attached to
// a Mills council incident classification.
type CouncilCIIncidentDisposition string

const (
	CouncilCIIncidentDispositionFixBranch        CouncilCIIncidentDisposition = "fix_branch_before_retry"
	CouncilCIIncidentDispositionFixCIConfig      CouncilCIIncidentDisposition = "fix_ci_config_before_retry"
	CouncilCIIncidentDispositionEscalateRunner   CouncilCIIncidentDisposition = "escalate_runner_owner"
	CouncilCIIncidentDispositionWaitDependency   CouncilCIIncidentDisposition = "wait_for_dependency_recovery"
	CouncilCIIncidentDispositionFixDependency    CouncilCIIncidentDisposition = "fix_dependency_update_before_retry"
	CouncilCIIncidentDispositionRetryOnce        CouncilCIIncidentDisposition = "retry_once"
	CouncilCIIncidentDispositionFixBranchHygiene CouncilCIIncidentDisposition = "fix_branch_or_plan_state"
	CouncilCIIncidentDispositionEscalateHuman    CouncilCIIncidentDisposition = "human_triage_required"
)

// CouncilCIIncidentClassification is the JSON contract consumed by downstream
// Mills planners and operators for council CI incident classification. The
// local follow-up fields are populated for external dependency incidents so a
// consumer can distinguish an upstream outage from work Loom can safely take.
type CouncilCIIncidentClassification struct {
	Class                  CouncilCIIncidentClass       `json:"class"`
	Disposition            CouncilCIIncidentDisposition `json:"disposition"`
	Dependency             string                       `json:"dependency,omitempty"`
	Evidence               string                       `json:"evidence,omitempty"`
	Reason                 string                       `json:"reason"`
	Confidence             float64                      `json:"confidence"`
	RetryAllowed           bool                         `json:"retry_allowed"`
	Label                  string                       `json:"label,omitempty"`
	OmitReason             string                       `json:"omit_reason,omitempty"`
	InRepoFollowUpRequired bool                         `json:"in_repo_follow_up_required"`
}

// CouncilExternalDependencyIncidentLabel is the stable label for follow-up work
// generated from external dependency incidents.
const CouncilExternalDependencyIncidentLabel = "external-dependency-incident"

// CouncilExternalIncidentNoInRepoFollowUpReason is the stable reason emitted
// when an external dependency incident has no safe, repository-owned action.
const CouncilExternalIncidentNoInRepoFollowUpReason = "external dependency incident; no actionable in-repo follow-up"
