package pipeline

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/crb2nu/loom/pkg/eventpub"
	"github.com/crb2nu/loom/pkg/mills/store"
)

const (
	ClassificationDisagreementEvent = "mills.classification.disagreement"
	ClassificationResolutionVersion = "dual-source-v1"
)

// ClassificationVerdict is the durable policy input for one escalated
// failure. Both source opinions are retained even when resolution fails
// closed, so an operator can audit disagreement without replaying classifiers.
type ClassificationVerdict struct {
	FailureID        string               `json:"failure_id"`
	Primary          SourceClassification `json:"primary"`
	Secondary        SourceClassification `json:"secondary"`
	ResolvedClass    ClassificationClass  `json:"resolved_class"`
	Disagreement     bool                 `json:"disagreement"`
	ResolutionReason string               `json:"resolution_reason"`
	ResolutionRule   string               `json:"resolution_rule"`
	ResolvedAt       time.Time            `json:"resolved_at"`
}

// ResolveClassification applies the deterministic fail-closed rule. Only two
// known, agreeing source classes produce an actionable verdict.
func ResolveClassification(failureID string, primary, secondary SourceClassification) ClassificationVerdict {
	primary = normalizeSourceClassification(primary)
	secondary = normalizeSourceClassification(secondary)
	v := ClassificationVerdict{
		FailureID:      failureID,
		Primary:        primary,
		Secondary:      secondary,
		ResolvedClass:  ClassificationUnknown,
		ResolutionRule: ClassificationResolutionVersion,
		ResolvedAt:     time.Now().UTC(),
	}
	switch {
	case primary.Class == ClassificationUnknown || secondary.Class == ClassificationUnknown:
		v.ResolutionReason = "missing_or_unknown_source"
	case primary.Class != secondary.Class:
		v.Disagreement = true
		v.ResolutionReason = "source_disagreement"
	default:
		v.ResolvedClass = primary.Class
		v.ResolutionReason = "source_agreement"
	}
	return v
}

// VerdictStore is implemented by store.ClassificationVerdictDAO.
type VerdictStore interface {
	PutClassificationVerdict(context.Context, store.ClassificationVerdictRecord) (bool, error)
	GetClassificationVerdict(context.Context, string) (store.ClassificationVerdictRecord, error)
}

// VerdictReconciler resolves, durably records, and publishes classifications.
// Persistence is first-writer stable; duplicate reconciliation therefore emits
// at most one disagreement event for a failure.
type VerdictReconciler struct {
	Store     VerdictStore
	Publisher eventpub.Publisher
	Now       func() time.Time
}

func (r VerdictReconciler) Reconcile(
	ctx context.Context,
	failureID string,
	primary, secondary SourceClassification,
) (ClassificationVerdict, error) {
	if failureID == "" {
		return ClassificationVerdict{}, errors.New("classification verdict: failure ID required")
	}
	if r.Store == nil {
		return ClassificationVerdict{}, errors.New("classification verdict: store required")
	}
	v := ResolveClassification(failureID, primary, secondary)
	if r.Now != nil {
		v.ResolvedAt = r.Now().UTC()
	}
	inserted, err := r.Store.PutClassificationVerdict(ctx, verdictRecord(v))
	if err != nil {
		return ClassificationVerdict{}, fmt.Errorf("persist classification verdict: %w", err)
	}
	if !inserted {
		record, err := r.Store.GetClassificationVerdict(ctx, failureID)
		if err != nil {
			return ClassificationVerdict{}, fmt.Errorf("load classification verdict: %w", err)
		}
		return verdictFromRecord(record), nil
	}
	if v.Disagreement && r.Publisher != nil {
		r.Publisher.Publish(ClassificationDisagreementEvent, v)
	}
	return v, nil
}

func verdictRecord(v ClassificationVerdict) store.ClassificationVerdictRecord {
	return store.ClassificationVerdictRecord{
		FailureID:        v.FailureID,
		PrimarySource:    v.Primary.Source,
		PrimaryClass:     string(v.Primary.Class),
		SecondarySource:  v.Secondary.Source,
		SecondaryClass:   string(v.Secondary.Class),
		ResolvedClass:    string(v.ResolvedClass),
		Disagreement:     v.Disagreement,
		ResolutionReason: v.ResolutionReason,
		ResolutionRule:   v.ResolutionRule,
		ResolvedAt:       v.ResolvedAt,
	}
}

func verdictFromRecord(v store.ClassificationVerdictRecord) ClassificationVerdict {
	return ClassificationVerdict{
		FailureID: v.FailureID,
		Primary: SourceClassification{
			Source: v.PrimarySource,
			Class:  ClassificationClass(v.PrimaryClass),
		},
		Secondary: SourceClassification{
			Source: v.SecondarySource,
			Class:  ClassificationClass(v.SecondaryClass),
		},
		ResolvedClass:    ClassificationClass(v.ResolvedClass),
		Disagreement:     v.Disagreement,
		ResolutionReason: v.ResolutionReason,
		ResolutionRule:   v.ResolutionRule,
		ResolvedAt:       v.ResolvedAt,
	}
}
