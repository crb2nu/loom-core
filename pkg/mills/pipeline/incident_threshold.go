package pipeline

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/crb2nu/loom/pkg/mills/council"
	"github.com/crb2nu/loom/pkg/mills/store"
	sharedpolicy "github.com/crb2nu/loom/pkg/policy"
)

const (
	ExternalIncidentWindow          = 24 * time.Hour
	ExternalIncidentThresholdOKCode = "external_incident_threshold_ok"
	ExternalIncidentSuppressedCode  = "external_incident_threshold_exceeded"
	ExternalIncidentSuppressionsKPI = "external_incident_suppressions"
)

// ExternalIncidentClusterCounter is the durable per-ref incident query used by
// the auto-merge guardrail.
type ExternalIncidentClusterCounter interface {
	CountExternalDependencyIncidentClusters(context.Context, string, time.Time) (int, error)
}

// ExternalIncidentKPIRecorder persists the suppression signal for operational
// reporting.
type ExternalIncidentKPIRecorder interface {
	RecordSnapshot(context.Context, *store.KPISnapshot) error
}

// ExternalIncidentThresholdGuard evaluates the rolling per-ref threshold.
// Now is injectable so the window boundary and emitted snapshot are stable in
// tests.
type ExternalIncidentThresholdGuard struct {
	Counter ExternalIncidentClusterCounter
	KPI     ExternalIncidentKPIRecorder
	Policy  sharedpolicy.ExternalIncidentPolicy
	Now     func() time.Time
}

// Evaluate returns a machine-readable council verdict. The configured
// threshold is inclusive; auto-merge is suppressed only when the count exceeds
// it.
func (g ExternalIncidentThresholdGuard) Evaluate(ctx context.Context, ref string) (council.PolicyVerdict, error) {
	if g.Counter == nil {
		return council.PolicyVerdict{}, fmt.Errorf("external incident threshold: counter required")
	}
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return council.PolicyVerdict{}, fmt.Errorf("external incident threshold: ref required")
	}
	now := time.Now().UTC()
	if g.Now != nil {
		now = g.Now().UTC()
	}
	threshold := g.Policy.ExternalIncidentThreshold()
	count, err := g.Counter.CountExternalDependencyIncidentClusters(ctx, ref, now.Add(-ExternalIncidentWindow))
	if err != nil {
		return council.PolicyVerdict{}, fmt.Errorf("external incident threshold count for ref %q: %w", ref, err)
	}

	metrics := map[string]float64{
		"external_incident_clusters":  float64(count),
		"external_incident_threshold": float64(threshold),
	}
	if count <= threshold {
		return council.PolicyVerdict{
			Pass:     true,
			Code:     ExternalIncidentThresholdOKCode,
			Severity: council.PolicySeverityOK,
			Action:   council.PolicyActionNone,
			Metrics:  metrics,
		}, nil
	}

	reason := fmt.Sprintf(
		"ref=%q count=%d window=%s threshold=%d",
		ref, count, ExternalIncidentWindow, threshold,
	)
	if g.KPI != nil {
		if err := g.KPI.RecordSnapshot(ctx, &store.KPISnapshot{
			SnapshotAt:    now,
			WindowSeconds: int(ExternalIncidentWindow.Seconds()),
			Metrics: map[string]any{
				ExternalIncidentSuppressionsKPI: 1,
				"ref":                           ref,
				"incident_count":                count,
				"incident_threshold":            threshold,
			},
		}); err != nil {
			return council.PolicyVerdict{}, fmt.Errorf("external incident threshold KPI: %w", err)
		}
	}
	return council.PolicyVerdict{
		Pass:     false,
		Code:     ExternalIncidentSuppressedCode,
		Severity: council.PolicySeverityWarning,
		Action:   council.PolicyActionEscalate,
		Reasons:  []string{reason},
		Metrics:  metrics,
	}, nil
}
