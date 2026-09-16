package workflow

import (
	"errors"
	"fmt"
)

// QueuedProofEvent is one durable observation made by the queued-proof
// kill-test. AttemptID is required for execution observations so the proof can
// distinguish a retry from duplicate execution of the same attempt.
type QueuedProofEvent struct {
	ItemID    string `json:"item_id"`
	Kind      string `json:"kind"`
	AttemptID string `json:"attempt_id,omitempty"`
}

const (
	QueuedProofSeeded      = "seeded"
	QueuedProofInterrupted = "interrupted"
	QueuedProofRequeued    = "requeued"
	QueuedProofExecuted    = "executed"
)

// QueuedRequeueProof is the machine-readable exactly-once result embedded in
// the kill-test JSON output.
type QueuedRequeueProof struct {
	ItemID     string             `json:"item_id"`
	DryRun     bool               `json:"dry_run"`
	Planned    bool               `json:"planned"`
	Passed     bool               `json:"passed"`
	Requeues   int                `json:"requeues"`
	Executions int                `json:"executions"`
	Events     []QueuedProofEvent `json:"events,omitempty"`
	Failure    string             `json:"failure,omitempty"`
}

// ProveQueuedRequeue validates the fail-closed queued-proof contract. A live
// proof requires one seed, an interruption, exactly one later requeue and one
// later execution. Reusing an attempt identity is duplicate execution.
func ProveQueuedRequeue(itemID string, events []QueuedProofEvent) (QueuedRequeueProof, error) {
	proof := QueuedRequeueProof{ItemID: itemID, Events: append([]QueuedProofEvent(nil), events...)}
	fail := func(format string, args ...any) (QueuedRequeueProof, error) {
		err := fmt.Errorf(format, args...)
		proof.Failure = err.Error()
		return proof, err
	}
	if itemID == "" {
		return fail("queued proof item identity is required")
	}
	seeded, interrupted, requeued := false, false, false
	attempts := make(map[string]struct{})
	for i, event := range events {
		if event.ItemID != itemID {
			return fail("event %d has contradictory item identity %q (want %q)", i, event.ItemID, itemID)
		}
		switch event.Kind {
		case QueuedProofSeeded:
			if seeded || interrupted || requeued || proof.Executions != 0 {
				return fail("event %d has contradictory seeded state", i)
			}
			seeded = true
		case QueuedProofInterrupted:
			if !seeded || interrupted || requeued {
				return fail("event %d has contradictory interrupted state", i)
			}
			interrupted = true
		case QueuedProofRequeued:
			if !interrupted {
				return fail("event %d requeues before interruption", i)
			}
			proof.Requeues++
			requeued = true
		case QueuedProofExecuted:
			if !requeued || event.AttemptID == "" {
				return fail("event %d executes without a requeue or attempt identity", i)
			}
			if _, exists := attempts[event.AttemptID]; exists {
				return fail("event %d duplicates execution attempt %q", i, event.AttemptID)
			}
			attempts[event.AttemptID] = struct{}{}
			proof.Executions++
		default:
			return fail("event %d has unknown state %q", i, event.Kind)
		}
	}
	if !seeded || !interrupted {
		return fail("proof is missing seeded or interrupted evidence")
	}
	if proof.Requeues != 1 {
		return fail("queued item was requeued %d times (want exactly 1)", proof.Requeues)
	}
	if proof.Executions != 1 {
		return fail("requeued item executed %d times (want exactly 1)", proof.Executions)
	}
	proof.Passed = true
	return proof, nil
}

// PlanQueuedRequeueProof returns the side-effect-free dry-run result.
func PlanQueuedRequeueProof(itemID string) (QueuedRequeueProof, error) {
	if itemID == "" {
		return QueuedRequeueProof{}, errors.New("queued proof item identity is required")
	}
	return QueuedRequeueProof{ItemID: itemID, DryRun: true, Planned: true}, nil
}
