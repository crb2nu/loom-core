package store

// IncidentClass is the durable incident taxonomy written into Mills records
// and projections. Keep values stable: stored council and pipeline evidence can
// outlive the process that classified it.
type IncidentClass string

const (
	// IncidentClassExternalDependency marks failures whose root cause belongs
	// to infrastructure or a service outside the repository being planned.
	IncidentClassExternalDependency IncidentClass = "external_dependency_incident"
)

// String returns the stable value persisted by store callers.
func (c IncidentClass) String() string {
	return string(c)
}
