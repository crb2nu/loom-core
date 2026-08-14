package pipeline

import (
	"context"
	"errors"
	"fmt"

	"github.com/crb2nu/loom/pkg/mills/store"
	"github.com/crb2nu/loom/pkg/telemetry"
)

const (
	DefaultExternalIncidentPaidRetryCap       = 2
	RetryDispositionWaitForDependencyRecovery = "wait_for_dependency_recovery"
)

// RetryStateStore supplies the durable classification and retry count used by
// RetryPolicy. PipelineDAO satisfies this interface.
type RetryStateStore interface {
	GetRunRetryState(context.Context, string) (store.RunRetryState, error)
}

// RetryVerdictStore supplies the stable dual-source verdict for a failed run.
type RetryVerdictStore interface {
	GetClassificationVerdict(context.Context, string) (store.ClassificationVerdictRecord, error)
}

// RetryPolicy gates paid retries on the durable dual-source verdict.
type RetryPolicy struct {
	Store                        RetryStateStore
	VerdictStore                 RetryVerdictStore
	ExternalIncidentPaidRetryCap int
	Metrics                      *telemetry.RetryMetrics
}

// RetryDecision is the policy result consumed by a retry scheduler.
type RetryDecision struct {
	Allowed     bool
	Disposition string
	Reason      string
	PaidRetries int
	Cap         int
}

// Decide determines whether a retry may proceed. Free retries always retain
// the existing behavior. Paid retries require a readable, resolved verdict:
// external incidents park without spending a retry and repository regressions
// retain the existing repair path. An absent verdict preserves the legacy
// classification/cap decision; unreadable or unresolved verdicts fail closed.
func (p RetryPolicy) Decide(ctx context.Context, runID string, paid bool) (RetryDecision, error) {
	cap := p.ExternalIncidentPaidRetryCap
	if cap == 0 {
		cap = DefaultExternalIncidentPaidRetryCap
	} else if cap < 0 {
		cap = 0
	}
	decision := RetryDecision{Allowed: true, Cap: cap}
	if !paid {
		return decision, nil
	}
	if p.Store == nil {
		return p.park(decision, telemetry.RetryIncidentClassUnknown,
			"persisted retry state unavailable"), errors.New("retry policy: store not configured")
	}

	state, err := p.Store.GetRunRetryState(ctx, runID)
	if err != nil {
		return p.park(decision, telemetry.RetryIncidentClassUnknown,
			"persisted retry state unavailable"), fmt.Errorf("retry policy: load persisted state: %w", err)
	}
	decision.PaidRetries = state.PaidRetryCount
	if p.VerdictStore == nil {
		return p.park(decision, telemetry.RetryIncidentClassUnknown,
			"persisted classification verdict unavailable"), errors.New("retry policy: verdict store not configured")
	}

	verdict, err := p.VerdictStore.GetClassificationVerdict(ctx, runID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return p.decideLegacy(decision, state), nil
		}
		return p.park(decision, telemetry.RetryIncidentClassUnknown,
				"persisted classification verdict unavailable"),
			fmt.Errorf("retry policy: load persisted verdict: %w", err)
	}
	switch ClassificationClass(verdict.ResolvedClass) {
	case ClassificationRepositoryRegression:
		return decision, nil
	case ClassificationExternalDependencyIncident:
		return p.park(decision, telemetry.RetryIncidentClassExternalDependency,
			"external dependency incident requires recovery before retry"), nil
	default:
		return p.park(decision, telemetry.RetryIncidentClassUnknown,
			"persisted classification verdict is unresolved"), nil
	}
}

func (p RetryPolicy) decideLegacy(decision RetryDecision, state store.RunRetryState) RetryDecision {
	if state.Classification != store.ExternalDependencyIncidentClassification ||
		state.PaidRetryCount < decision.Cap {
		return decision
	}
	return p.park(decision, telemetry.RetryIncidentClassExternalDependency,
		"external dependency incident paid retry cap exhausted")
}

func (p RetryPolicy) park(decision RetryDecision, incidentClass, reason string) RetryDecision {
	decision.Allowed = false
	decision.Disposition = RetryDispositionWaitForDependencyRecovery
	decision.Reason = reason
	p.Metrics.RecordRetryCapRefusal(incidentClass, decision.Disposition)
	return decision
}
