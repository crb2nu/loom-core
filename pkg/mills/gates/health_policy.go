package gates

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/crb2nu/loom/pkg/telemetry"
)

const (
	TelemetryHealthCodeOK              = "ok"
	TelemetryHealthCodeStaleTelemetry  = "stale_telemetry"
	TelemetryHealthCodeThresholdBreach = "threshold_breach"
	TelemetryHealthCodeBackendIncident = "backend_incident"
)

// IncidentClass is the bounded ownership taxonomy shared by health policies
// and the gates that consume their results. It deliberately describes the
// first meaningful failure rather than every downstream symptom.
type IncidentClass string

const (
	IncidentClassNone               IncidentClass = "none"
	IncidentClassStorage            IncidentClass = "storage_incident"
	IncidentClassLocalConfiguration IncidentClass = "local_configuration"
	IncidentClassExternalDependency IncidentClass = "external_dependency"
	IncidentClassRepository         IncidentClass = "repository_regression"
	IncidentClassUnknown            IncidentClass = "unknown"
)

// IncidentSeverity is a stable urgency vocabulary for policy output.
type IncidentSeverity string

const (
	IncidentSeverityNone     IncidentSeverity = "none"
	IncidentSeverityWarning  IncidentSeverity = "warning"
	IncidentSeverityCritical IncidentSeverity = "critical"
)

// IncidentClassification is a reusable, wire-safe incident contract. The
// fields remain intentionally small so later gates can add their own evidence
// without redefining ownership, severity, or retry semantics.
type IncidentClassification struct {
	Class                  IncidentClass    `json:"class"`
	Severity               IncidentSeverity `json:"severity"`
	Dependency             string           `json:"dependency,omitempty"`
	Reason                 string           `json:"reason,omitempty"`
	RetryAllowed           bool             `json:"retry_allowed"`
	RequiresManualRecovery bool             `json:"requires_manual_recovery"`
}

// StorageHealthState is the policy state for storage capacity and integrity.
// Its values match the operator thresholds documented in incident-hardening.
type StorageHealthState string

const (
	StorageHealthStateNormal    StorageHealthState = "normal"
	StorageHealthStateWarning   StorageHealthState = "warning"
	StorageHealthStateCritical  StorageHealthState = "critical"
	StorageHealthStateExhausted StorageHealthState = "exhausted"
)

// StorageHealthPolicy configures deterministic capacity thresholds. Both byte
// and inode usage are evaluated and the more severe result always wins.
type StorageHealthPolicy struct {
	WarningUsedPercent  float64 `json:"warning_used_percent"`
	CriticalUsedPercent float64 `json:"critical_used_percent"`
}

// StorageHealthSnapshot is the minimum evidence needed to evaluate storage
// health. Write, remount, and SQLite failures are exhausted regardless of
// reported capacity because mutations are no longer safe.
type StorageHealthSnapshot struct {
	CapacityUsedPercent float64 `json:"capacity_used_percent"`
	InodeUsedPercent    float64 `json:"inode_used_percent"`
	WriteError          string  `json:"write_error,omitempty"`
	ReadOnly            bool    `json:"read_only"`
	SQLiteError         string  `json:"sqlite_error,omitempty"`
}

// StorageHealthVerdict is the deterministic result consumed by future
// admission gates. Autonomous writes are allowed only in the normal state.
type StorageHealthVerdict struct {
	State                   StorageHealthState     `json:"state"`
	AutonomousWritesAllowed bool                   `json:"autonomous_writes_allowed"`
	UsedPercent             float64                `json:"used_percent"`
	Classification          IncidentClassification `json:"classification"`
}

// DefaultStorageHealthPolicy returns the documented 80% warning and 90%
// critical thresholds. Keeping these defaults in code makes all consumers
// classify the same evidence identically.
func DefaultStorageHealthPolicy() StorageHealthPolicy {
	return StorageHealthPolicy{WarningUsedPercent: 80, CriticalUsedPercent: 90}
}

// EvaluateStorageHealthPolicy classifies capacity and integrity evidence. A
// write or SQLite error, read-only remount, or full filesystem is exhausted;
// otherwise capacity and inode percentages select the more severe threshold.
func EvaluateStorageHealthPolicy(policy StorageHealthPolicy, snapshot StorageHealthSnapshot) StorageHealthVerdict {
	policy = normalizeStorageHealthPolicy(policy)
	used := maxStorageUsedPercent(snapshot.CapacityUsedPercent, snapshot.InodeUsedPercent)
	state := StorageHealthStateNormal
	reason := ""

	switch {
	case strings.TrimSpace(snapshot.WriteError) != "":
		state, reason = StorageHealthStateExhausted, "storage write error: "+strings.TrimSpace(snapshot.WriteError)
	case snapshot.ReadOnly:
		state, reason = StorageHealthStateExhausted, "storage is mounted read-only"
	case strings.TrimSpace(snapshot.SQLiteError) != "":
		state, reason = StorageHealthStateExhausted, "sqlite storage error: "+strings.TrimSpace(snapshot.SQLiteError)
	case used >= 100:
		state, reason = StorageHealthStateExhausted, "storage capacity or inode usage is exhausted"
	case used >= policy.CriticalUsedPercent:
		state, reason = StorageHealthStateCritical, "storage capacity or inode usage exceeds critical threshold"
	case used >= policy.WarningUsedPercent:
		state, reason = StorageHealthStateWarning, "storage capacity or inode usage exceeds warning threshold"
	}

	verdict := StorageHealthVerdict{
		State:                   state,
		AutonomousWritesAllowed: state == StorageHealthStateNormal,
		UsedPercent:             used,
		Classification: IncidentClassification{
			Class:    IncidentClassNone,
			Severity: IncidentSeverityNone,
		},
	}
	if state == StorageHealthStateNormal {
		return verdict
	}
	verdict.Classification = IncidentClassification{
		Class:                  IncidentClassStorage,
		Severity:               storageIncidentSeverity(state),
		Dependency:             "storage",
		Reason:                 reason,
		RetryAllowed:           false,
		RequiresManualRecovery: state == StorageHealthStateExhausted,
	}
	return verdict
}

func normalizeStorageHealthPolicy(policy StorageHealthPolicy) StorageHealthPolicy {
	defaults := DefaultStorageHealthPolicy()
	if policy.WarningUsedPercent <= 0 || policy.WarningUsedPercent >= 100 {
		policy.WarningUsedPercent = defaults.WarningUsedPercent
	}
	if policy.CriticalUsedPercent <= 0 || policy.CriticalUsedPercent >= 100 {
		policy.CriticalUsedPercent = defaults.CriticalUsedPercent
	}
	if policy.CriticalUsedPercent <= policy.WarningUsedPercent {
		return defaults
	}
	return policy
}

func maxStorageUsedPercent(capacity, inodes float64) float64 {
	if capacity < 0 {
		capacity = 0
	}
	if inodes < 0 {
		inodes = 0
	}
	if inodes > capacity {
		return inodes
	}
	return capacity
}

func storageIncidentSeverity(state StorageHealthState) IncidentSeverity {
	if state == StorageHealthStateWarning {
		return IncidentSeverityWarning
	}
	return IncidentSeverityCritical
}

// TelemetryHealthSeverity is the bounded severity vocabulary for telemetry
// degraded-mode policy output.
type TelemetryHealthSeverity string

const (
	TelemetryHealthSeverityOK       TelemetryHealthSeverity = "ok"
	TelemetryHealthSeverityWarning  TelemetryHealthSeverity = "warning"
	TelemetryHealthSeverityCritical TelemetryHealthSeverity = "critical"
)

// TelemetryHealthPolicy configures deterministic degraded-mode thresholds for
// telemetry signals consumed by council and pipeline decisioning.
type TelemetryHealthPolicy struct {
	MaxTelemetryAge         time.Duration
	MaxPipelineFailureRate  float64
	MaxGateFlakeRate        float64
	MaxJudgeUnparseableRate float64
	MaxRetryBurnRate        float64
	MaxQueueDepth           int
}

// TelemetryHealthSnapshot is the telemetry evidence bundle evaluated by
// TelemetryHealthPolicy.
type TelemetryHealthSnapshot struct {
	ObservedAt           time.Time
	PipelineFailureRate  float64
	GateFlakeRate        float64
	JudgeUnparseableRate float64
	RetryBurnRate        float64
	QueueDepth           int
	BackendIncidents     []TelemetryBackendIncident
}

// TelemetryBackendIncident is an active backend incident that should put Mills
// telemetry consumers into degraded mode.
type TelemetryBackendIncident struct {
	ID      string `json:"id,omitempty"`
	Backend string `json:"backend"`
	Summary string `json:"summary,omitempty"`
	Active  bool   `json:"active"`
}

// TelemetryHealthCheck reports one checked telemetry signal.
type TelemetryHealthCheck struct {
	Name      string                  `json:"name"`
	Pass      bool                    `json:"pass"`
	Value     float64                 `json:"value"`
	Threshold float64                 `json:"threshold"`
	Severity  TelemetryHealthSeverity `json:"severity"`
	Reason    string                  `json:"reason,omitempty"`
}

// TelemetryHealthVerdict is the stable machine-readable output for telemetry
// degraded-mode policy decisions.
type TelemetryHealthVerdict struct {
	Pass                 bool                            `json:"pass"`
	Degraded             bool                            `json:"degraded"`
	Code                 string                          `json:"code"`
	Severity             TelemetryHealthSeverity         `json:"severity"`
	OperationalState     telemetry.MillsOperationalState `json:"operational_state"`
	Reasons              []string                        `json:"reasons,omitempty"`
	Checks               []TelemetryHealthCheck          `json:"checks"`
	DegradedDependencies []string                        `json:"degraded_dependencies,omitempty"`
	ActiveIncidents      []telemetry.DependencyIncident  `json:"active_incidents,omitempty"`
	Metrics              map[string]float64              `json:"metrics,omitempty"`
}

// DefaultTelemetryHealthPolicy returns conservative thresholds for deciding
// when telemetry is too unhealthy to trust as a normal operating signal.
func DefaultTelemetryHealthPolicy() TelemetryHealthPolicy {
	return TelemetryHealthPolicy{
		MaxTelemetryAge:         10 * time.Minute,
		MaxPipelineFailureRate:  0.25,
		MaxGateFlakeRate:        0.10,
		MaxJudgeUnparseableRate: 0.05,
		MaxRetryBurnRate:        0.35,
		MaxQueueDepth:           25,
	}
}

// EvaluateTelemetryHealthPolicy evaluates telemetry health and marks degraded
// mode when telemetry is stale, policy thresholds are breached, or backend
// incident inputs are active.
func EvaluateTelemetryHealthPolicy(policy TelemetryHealthPolicy, snapshot TelemetryHealthSnapshot, now time.Time) TelemetryHealthVerdict {
	policy = normalizeTelemetryHealthPolicy(policy)
	if now.IsZero() {
		now = time.Now().UTC()
	}

	checks := []TelemetryHealthCheck{
		telemetryMaxCheck("pipeline_failure_rate", snapshot.PipelineFailureRate, policy.MaxPipelineFailureRate, TelemetryHealthSeverityCritical, "pipeline failure rate exceeds telemetry health threshold"),
		telemetryMaxCheck("gate_flake_rate", snapshot.GateFlakeRate, policy.MaxGateFlakeRate, TelemetryHealthSeverityWarning, "gate flake rate exceeds telemetry health threshold"),
		telemetryMaxCheck("judge_unparseable_rate", snapshot.JudgeUnparseableRate, policy.MaxJudgeUnparseableRate, TelemetryHealthSeverityWarning, "judge unparseable rate exceeds telemetry health threshold"),
		telemetryMaxCheck("retry_burn_rate", snapshot.RetryBurnRate, policy.MaxRetryBurnRate, TelemetryHealthSeverityWarning, "retry burn rate exceeds telemetry health threshold"),
		telemetryMaxCheck("queue_depth", float64(snapshot.QueueDepth), float64(policy.MaxQueueDepth), TelemetryHealthSeverityWarning, "telemetry queue depth exceeds threshold"),
	}

	verdict := TelemetryHealthVerdict{
		Pass:     true,
		Code:     TelemetryHealthCodeOK,
		Severity: TelemetryHealthSeverityOK,
		Checks:   checks,
		Metrics: map[string]float64{
			"max_telemetry_age_seconds": policy.MaxTelemetryAge.Seconds(),
		},
	}
	for _, c := range checks {
		verdict.Metrics[c.Name] = c.Value
		verdict.Metrics[c.Name+".threshold"] = c.Threshold
	}

	if snapshot.ObservedAt.IsZero() {
		verdict.markDegraded(TelemetryHealthCodeStaleTelemetry, TelemetryHealthSeverityCritical, "telemetry health evidence has no timestamp")
	} else if age := now.Sub(snapshot.ObservedAt); age < 0 || age > policy.MaxTelemetryAge {
		verdict.Metrics["telemetry_age_seconds"] = age.Seconds()
		verdict.markDegraded(TelemetryHealthCodeStaleTelemetry, TelemetryHealthSeverityCritical, fmt.Sprintf("telemetry health evidence is stale: age %s exceeds %s", age.Truncate(time.Second), policy.MaxTelemetryAge))
	} else {
		verdict.Metrics["telemetry_age_seconds"] = now.Sub(snapshot.ObservedAt).Seconds()
	}

	for _, c := range checks {
		if c.Pass {
			continue
		}
		verdict.markDegraded(TelemetryHealthCodeThresholdBreach, c.Severity, c.Reason)
	}

	incidents := normalizeTelemetryBackendIncidents(snapshot.BackendIncidents)
	if len(incidents) > 0 {
		verdict.ActiveIncidents = incidents
		for _, incident := range incidents {
			verdict.DegradedDependencies = append(verdict.DegradedDependencies, incident.Dependency)
			reason := "backend incident active: " + incident.Dependency
			if incident.Summary != "" {
				reason += ": " + incident.Summary
			}
			verdict.markDegraded(TelemetryHealthCodeBackendIncident, TelemetryHealthSeverityCritical, reason)
		}
	}

	if verdict.Degraded && len(verdict.DegradedDependencies) == 0 {
		verdict.DegradedDependencies = []string{"telemetry"}
	}
	verdict.applyTelemetryOperationalState()
	return verdict
}

func (v *TelemetryHealthVerdict) markDegraded(code string, severity TelemetryHealthSeverity, reason string) {
	v.Pass = false
	v.Degraded = true
	v.Reasons = append(v.Reasons, reason)
	switch {
	case code == TelemetryHealthCodeBackendIncident:
		v.Code = code
	case code == TelemetryHealthCodeStaleTelemetry && v.Code != TelemetryHealthCodeBackendIncident:
		v.Code = code
	case code == TelemetryHealthCodeThresholdBreach && v.Code == TelemetryHealthCodeOK:
		v.Code = code
	}
	if severity == TelemetryHealthSeverityCritical {
		v.Severity = TelemetryHealthSeverityCritical
	} else if v.Severity == TelemetryHealthSeverityOK {
		v.Severity = TelemetryHealthSeverityWarning
	}
}

func (v *TelemetryHealthVerdict) applyTelemetryOperationalState() {
	report := telemetry.EvaluateMillsOperationalState(telemetry.MillsOperationalStateInput{
		AutonomyAllowed:           true,
		HealthAllowed:             true,
		HealthReasons:             v.Reasons,
		DegradedDependencies:      v.DegradedDependencies,
		ActiveDependencyIncidents: v.ActiveIncidents,
	})
	v.OperationalState = report.State
	v.DegradedDependencies = report.DegradedDependencies
	v.ActiveIncidents = report.ActiveIncidents
}

func normalizeTelemetryHealthPolicy(policy TelemetryHealthPolicy) TelemetryHealthPolicy {
	def := DefaultTelemetryHealthPolicy()
	if policy.MaxTelemetryAge <= 0 {
		policy.MaxTelemetryAge = def.MaxTelemetryAge
	}
	if policy.MaxPipelineFailureRate <= 0 || policy.MaxPipelineFailureRate >= 1 {
		policy.MaxPipelineFailureRate = def.MaxPipelineFailureRate
	}
	if policy.MaxGateFlakeRate <= 0 || policy.MaxGateFlakeRate >= 1 {
		policy.MaxGateFlakeRate = def.MaxGateFlakeRate
	}
	if policy.MaxJudgeUnparseableRate <= 0 || policy.MaxJudgeUnparseableRate >= 1 {
		policy.MaxJudgeUnparseableRate = def.MaxJudgeUnparseableRate
	}
	if policy.MaxRetryBurnRate <= 0 || policy.MaxRetryBurnRate >= 1 {
		policy.MaxRetryBurnRate = def.MaxRetryBurnRate
	}
	if policy.MaxQueueDepth <= 0 {
		policy.MaxQueueDepth = def.MaxQueueDepth
	}
	return policy
}

func telemetryMaxCheck(name string, value, threshold float64, severity TelemetryHealthSeverity, reason string) TelemetryHealthCheck {
	check := TelemetryHealthCheck{Name: name, Pass: value <= threshold, Value: value, Threshold: threshold, Severity: severity}
	if !check.Pass {
		check.Reason = reason
	}
	return check
}

func normalizeTelemetryBackendIncidents(values []TelemetryBackendIncident) []telemetry.DependencyIncident {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(values))
	out := make([]telemetry.DependencyIncident, 0, len(values))
	for _, value := range values {
		if !value.Active {
			continue
		}
		id := strings.TrimSpace(value.ID)
		backend := strings.TrimSpace(value.Backend)
		summary := strings.TrimSpace(value.Summary)
		if backend == "" && id == "" {
			continue
		}
		if backend == "" {
			backend = "unknown"
		}
		key := id + "\x00" + backend
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, telemetry.DependencyIncident{ID: id, Dependency: backend, Summary: summary})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Dependency == out[j].Dependency {
			return out[i].ID < out[j].ID
		}
		return out[i].Dependency < out[j].Dependency
	})
	return out
}
