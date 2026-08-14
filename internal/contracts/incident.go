package contracts

// IncidentClassifier names the deterministic classifier that produced a
// structured incident record.
type IncidentClassifier string

const (
	IncidentClassifierExternalDependency IncidentClassifier = "incident-classifier"
)

// IncidentCategory is the bounded top-level incident taxonomy. The initial
// classifier only emits external_dependency; unknown messages return Matched
// false instead of minting an unbounded category.
type IncidentCategory string

const (
	IncidentCategoryExternalDependency IncidentCategory = "external_dependency"
)

// IncidentDependency names the external system implicated by a recurring
// failure pattern.
type IncidentDependency string

const (
	IncidentDependencyFlexInfer  IncidentDependency = "flexinfer"
	IncidentDependencyGitLab     IncidentDependency = "gitlab"
	IncidentDependencyKubernetes IncidentDependency = "kubernetes"
	IncidentDependencyMCPGateway IncidentDependency = "mcp_gateway"
	IncidentDependencyNetwork    IncidentDependency = "network"
	IncidentDependencySandbox    IncidentDependency = "sandbox"
	IncidentDependencySpawnPool  IncidentDependency = "spawn_pool"
)

// IncidentClassification is the wire-safe incident record consumed by audit
// follow-up and downstream reporting. Labels are deterministic and suitable for
// GitLab issue labels, metrics dimensions, or dashboard grouping.
type IncidentClassification struct {
	Classifier IncidentClassifier `json:"classifier"`
	Matched    bool               `json:"matched"`
	Category   IncidentCategory   `json:"category,omitempty"`
	Dependency IncidentDependency `json:"dependency,omitempty"`
	Reason     string             `json:"reason,omitempty"`
	Retryable  bool               `json:"retryable"`
	Labels     []string           `json:"labels,omitempty"`
}
