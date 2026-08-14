package pipeline

import (
	"context"
	"strings"

	"github.com/crb2nu/loom/pkg/mills/store"
	"github.com/crb2nu/loom/pkg/telemetry"
)

// TransientRequeueClaimer is the durable budget boundary used by the failure
// router. *store.Store satisfies it.
type TransientRequeueClaimer interface {
	ClaimTransientRequeue(context.Context, string, int) (store.TransientRequeueClaim, error)
}

// FailureRoute is the only action a runner may take after classification.
type FailureRoute string

const (
	FailureRouteRequeue  FailureRoute = "requeue"
	FailureRouteEscalate FailureRoute = "escalate"
)

// FailureRouteDecision carries the durable allowance state into runner audit
// and escalation output. PersistenceError is evidence, not permission to
// retry: store failures always fail closed.
type FailureRouteDecision struct {
	Route            FailureRoute
	Identity         string
	FailureClass     FailureClass
	AttemptsUsed     int
	Cap              int
	Exhausted        bool
	Reason           string
	PersistenceError string
}

// DecideTransientRequeue routes explicit transient-class escalations through
// the durable bounded claim. Every decision emits one bounded-cardinality
// eligible or blocked observation. Unknown future classes, terminal records,
// invalid configuration, store errors, and exhausted caps fail closed.
func DecideTransientRequeue(ctx context.Context, claimer TransientRequeueClaimer, backlogID string, failure FailureClassification, cap int) FailureRouteDecision {
	backlogID = strings.TrimSpace(backlogID)
	d := FailureRouteDecision{
		Route: FailureRouteEscalate, Identity: backlogID,
		FailureClass: failure.Class, Cap: cap,
	}
	if !failure.Retryable || failure.Terminal || !admittedTransientFailureClass(failure.Class) {
		d.Reason = "failure classification is not explicitly transient"
		telemetry.RecordEscalationRequeueBlocked(ctx, telemetry.EscalationRequeueBlockClassification)
		return d
	}
	if claimer == nil || cap <= 0 {
		d.Exhausted = true
		d.Reason = "transient requeue budget unavailable"
		telemetry.RecordEscalationRequeueBlocked(ctx, telemetry.EscalationRequeueBlockBudgetUnavailable)
		return d
	}
	claim, err := claimer.ClaimTransientRequeue(ctx, backlogID, cap)
	d.AttemptsUsed = claim.AttemptsUsed
	if err != nil {
		d.Exhausted = true
		d.Reason = "transient requeue budget persistence failed"
		d.PersistenceError = err.Error()
		telemetry.RecordEscalationRequeueBlocked(ctx, telemetry.EscalationRequeueBlockPersistence)
		return d
	}
	if !claim.Claimed {
		d.Exhausted = true
		d.Reason = "transient requeue budget exhausted"
		telemetry.RecordEscalationRequeueBlocked(ctx, telemetry.EscalationRequeueBlockBudgetExhausted)
		return d
	}
	d.Route = FailureRouteRequeue
	d.Reason = "durable transient requeue allowance claimed"
	telemetry.RecordEscalationRequeueEligible(ctx, string(failure.Class))
	return d
}

func admittedTransientFailureClass(class FailureClass) bool {
	switch class {
	case FailureTransient, FailureTransientQuota, FailureInfrastructure:
		return true
	default:
		return false
	}
}
