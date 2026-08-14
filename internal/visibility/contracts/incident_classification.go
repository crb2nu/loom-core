package contracts

import corecontracts "github.com/crb2nu/loom/internal/contracts"

// CIIncidentClass is the stable incident taxonomy exposed by visibility
// consumers. In particular, CIIncidentExternalDependency distinguishes a
// dependency outage from a repository regression.
type CIIncidentClass = corecontracts.CouncilCIIncidentClass

const (
	CIIncidentRepositoryRegression = corecontracts.CouncilCIIncidentRepositoryRegression
	CIIncidentCIConfiguration      = corecontracts.CouncilCIIncidentCIConfiguration
	CIIncidentRunnerInfrastructure = corecontracts.CouncilCIIncidentRunnerInfrastructure
	CIIncidentExternalDependency   = corecontracts.CouncilCIIncidentExternalDependency
	CIIncidentDependencyUpdate     = corecontracts.CouncilCIIncidentDependencyUpdate
	CIIncidentFlakeOrTransient     = corecontracts.CouncilCIIncidentFlakeOrTransient
	CIIncidentBranchOrPlanHygiene  = corecontracts.CouncilCIIncidentBranchOrPlanHygiene
	CIIncidentUnclassified         = corecontracts.CouncilCIIncidentUnclassified
)

// CIIncidentDisposition is the remediation decision paired with an incident
// classification in visibility responses.
type CIIncidentDisposition = corecontracts.CouncilCIIncidentDisposition

const (
	CIIncidentDispositionFixBranch        = corecontracts.CouncilCIIncidentDispositionFixBranch
	CIIncidentDispositionFixCIConfig      = corecontracts.CouncilCIIncidentDispositionFixCIConfig
	CIIncidentDispositionEscalateRunner   = corecontracts.CouncilCIIncidentDispositionEscalateRunner
	CIIncidentDispositionWaitDependency   = corecontracts.CouncilCIIncidentDispositionWaitDependency
	CIIncidentDispositionFixDependency    = corecontracts.CouncilCIIncidentDispositionFixDependency
	CIIncidentDispositionRetryOnce        = corecontracts.CouncilCIIncidentDispositionRetryOnce
	CIIncidentDispositionFixBranchHygiene = corecontracts.CouncilCIIncidentDispositionFixBranchHygiene
	CIIncidentDispositionEscalateHuman    = corecontracts.CouncilCIIncidentDispositionEscalateHuman
)

// CIIncidentClassification is the structured incident output shared with
// Loom visibility consumers. It preserves the source classification's class,
// disposition, evidence, confidence, and retry decision rather than reducing
// an external dependency incident to a repository regression.
type CIIncidentClassification = corecontracts.CouncilCIIncidentClassification
