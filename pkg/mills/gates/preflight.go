package gates

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	DependencyPreflightGateName = "dependency_preflight"
	defaultDependencyMaxAge     = 5 * time.Minute
)

// DependencyState is the bounded state vocabulary returned by health probes.
type DependencyState string

const (
	DependencyHealthy  DependencyState = "healthy"
	DependencyDegraded DependencyState = "degraded"
	DependencyDown     DependencyState = "down"
	DependencyUnknown  DependencyState = "unknown"
	DependencyMissing  DependencyState = "missing"
)

// Dependency identifies an external service that must be healthy before a
// destructive or expensive live test is admitted.
type Dependency struct {
	Name string `json:"name"`
}

// DependencyEvidence is the health assertion returned by a Prober.
type DependencyEvidence struct {
	State      DependencyState `json:"state"`
	ObservedAt time.Time       `json:"observed_at"`
}

// DependencyProber obtains health evidence. Transport failures are represented
// in the verdict as probe_error and do not escape Evaluate as infrastructure
// errors, preserving fail-closed behavior.
type DependencyProber interface {
	Probe(context.Context, Dependency) (DependencyEvidence, error)
}

type DependencyReasonCode string

const (
	ReasonHealthy    DependencyReasonCode = "dependency.healthy"
	ReasonDegraded   DependencyReasonCode = "dependency.degraded"
	ReasonDown       DependencyReasonCode = "dependency.down"
	ReasonUnknown    DependencyReasonCode = "dependency.unknown"
	ReasonMissing    DependencyReasonCode = "dependency.missing"
	ReasonStale      DependencyReasonCode = "dependency.stale"
	ReasonProbeError DependencyReasonCode = "dependency.probe_error"
)

// DependencyVerdict is one dependency's stable machine-readable result.
type DependencyVerdict struct {
	Name        string               `json:"name"`
	State       DependencyState      `json:"state"`
	ReasonCode  DependencyReasonCode `json:"reason_code"`
	ObservedAt  string               `json:"observed_at,omitempty"`
	Remediation string               `json:"remediation"`
}

// DependencyPreflightVerdict is emitted before a live test is admitted.
type DependencyPreflightVerdict struct {
	SchemaVersion int                 `json:"schema_version"`
	Gate          string              `json:"gate"`
	Status        string              `json:"status"`
	Admitted      bool                `json:"admitted"`
	Dependencies  []DependencyVerdict `json:"dependencies"`
}

// DependencyPreflight checks declared dependencies using an injected prober
// and clock. Zero MaxAge uses a conservative five-minute freshness window.
type DependencyPreflight struct {
	Dependencies []Dependency
	Prober       DependencyProber
	Now          func() time.Time
	MaxAge       time.Duration
}

func (g *DependencyPreflight) Name() string { return DependencyPreflightGateName }

// Check returns the structured verdict used by Evaluate and direct callers.
func (g *DependencyPreflight) Check(ctx context.Context) (DependencyPreflightVerdict, error) {
	verdict := DependencyPreflightVerdict{
		SchemaVersion: 1,
		Gate:          DependencyPreflightGateName,
		Status:        "admitted",
		Admitted:      true,
		Dependencies:  []DependencyVerdict{},
	}
	deps := append([]Dependency(nil), g.Dependencies...)
	for _, dep := range deps {
		if strings.TrimSpace(dep.Name) == "" {
			return verdict, fmt.Errorf("dependency declaration has an empty name")
		}
	}
	sort.Slice(deps, func(i, j int) bool { return deps[i].Name < deps[j].Name })
	for i := 1; i < len(deps); i++ {
		if deps[i-1].Name == deps[i].Name {
			return verdict, fmt.Errorf("duplicate dependency declaration %q", deps[i].Name)
		}
	}
	if len(deps) == 0 {
		verdict.Admitted = false
		verdict.Status = "degraded"
		verdict.Dependencies = append(verdict.Dependencies, DependencyVerdict{
			Name: "declarations", State: DependencyMissing, ReasonCode: ReasonMissing,
			Remediation: "declare every external dependency and provide fresh healthy probe evidence",
		})
		return verdict, nil
	}

	now := time.Now().UTC()
	if g.Now != nil {
		now = g.Now().UTC()
	}
	maxAge := g.MaxAge
	if maxAge <= 0 {
		maxAge = defaultDependencyMaxAge
	}
	for _, dep := range deps {
		result := g.checkOne(ctx, dep, now, maxAge)
		if result.ReasonCode != ReasonHealthy {
			verdict.Admitted = false
			verdict.Status = "degraded"
		}
		verdict.Dependencies = append(verdict.Dependencies, result)
	}
	return verdict, nil
}

func (g *DependencyPreflight) checkOne(ctx context.Context, dep Dependency, now time.Time, maxAge time.Duration) DependencyVerdict {
	if g.Prober == nil {
		return dependencyFailure(dep.Name, DependencyUnknown, ReasonProbeError)
	}
	evidence, err := g.Prober.Probe(ctx, dep)
	if err != nil {
		return dependencyFailure(dep.Name, DependencyUnknown, ReasonProbeError)
	}
	result := DependencyVerdict{Name: dep.Name, State: evidence.State}
	if !evidence.ObservedAt.IsZero() {
		result.ObservedAt = evidence.ObservedAt.UTC().Format(time.RFC3339Nano)
	}
	if evidence.ObservedAt.IsZero() || evidence.ObservedAt.After(now) || now.Sub(evidence.ObservedAt) > maxAge {
		result.ReasonCode = ReasonStale
		result.Remediation = remediation(ReasonStale)
		return result
	}
	switch evidence.State {
	case DependencyHealthy:
		result.ReasonCode = ReasonHealthy
	case DependencyDegraded:
		result.ReasonCode = ReasonDegraded
	case DependencyDown:
		result.ReasonCode = ReasonDown
	case DependencyMissing, "":
		result.State = DependencyMissing
		result.ReasonCode = ReasonMissing
	default:
		result.State = DependencyUnknown
		result.ReasonCode = ReasonUnknown
	}
	result.Remediation = remediation(result.ReasonCode)
	return result
}

func dependencyFailure(name string, state DependencyState, code DependencyReasonCode) DependencyVerdict {
	return DependencyVerdict{Name: name, State: state, ReasonCode: code, Remediation: remediation(code)}
}

func remediation(code DependencyReasonCode) string {
	switch code {
	case ReasonHealthy:
		return "none"
	case ReasonDegraded:
		return "restore the dependency to healthy and rerun the probe"
	case ReasonDown:
		return "restore dependency availability and rerun the probe"
	case ReasonStale:
		return "collect fresh health evidence within the configured maximum age"
	case ReasonProbeError:
		return "repair probe connectivity or credentials, then rerun the probe"
	case ReasonMissing:
		return "configure the dependency and collect fresh healthy evidence"
	default:
		return "resolve the unknown health state and collect fresh healthy evidence"
	}
}

func (g *DependencyPreflight) Evaluate(ctx context.Context, _ StageInput) (Outcome, error) {
	verdict, err := g.Check(ctx)
	if err != nil {
		return Outcome{}, err
	}
	payload, err := json.Marshal(verdict)
	if err != nil {
		return Outcome{}, fmt.Errorf("marshal dependency preflight verdict: %w", err)
	}
	if verdict.Admitted {
		return Outcome{Pass: true, Reasons: []string{string(payload)}, JudgedBy: "go"}, nil
	}
	return Outcome{Pass: false, Reasons: []string{string(payload)}, JudgedBy: "go"}, nil
}
