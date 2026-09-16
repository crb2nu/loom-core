package workflow

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestProveQueuedRequeue(t *testing.T) {
	base := []QueuedProofEvent{
		{ItemID: "item-1", Kind: QueuedProofSeeded},
		{ItemID: "item-1", Kind: QueuedProofInterrupted},
		{ItemID: "item-1", Kind: QueuedProofRequeued},
		{ItemID: "item-1", Kind: QueuedProofExecuted, AttemptID: "run-1"},
	}
	tests := []struct {
		name   string
		events []QueuedProofEvent
		want   string
	}{
		{name: "success", events: base},
		{name: "missing requeue", events: append(append([]QueuedProofEvent{}, base[:2]...), base[3]), want: "requeue"},
		{name: "duplicate requeue", events: append(append([]QueuedProofEvent{}, base[:3]...), base[2:]...), want: "requeued 2"},
		{name: "duplicate execution", events: append(append([]QueuedProofEvent{}, base...), base[3]), want: "duplicates execution"},
		{name: "contradictory identity", events: func() []QueuedProofEvent {
			e := append([]QueuedProofEvent{}, base...)
			e[2].ItemID = "item-2"
			return e
		}(), want: "contradictory item identity"},
		{name: "contradictory state", events: []QueuedProofEvent{{ItemID: "item-1", Kind: QueuedProofSeeded}, {ItemID: "item-1", Kind: QueuedProofRequeued}}, want: "before interruption"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			proof, err := ProveQueuedRequeue("item-1", tt.events)
			if tt.want == "" {
				if err != nil || !proof.Passed || proof.Requeues != 1 || proof.Executions != 1 {
					t.Fatalf("proof=%+v err=%v", proof, err)
				}
				return
			}
			if err == nil || proof.Passed || !strings.Contains(err.Error(), tt.want) || proof.Failure == "" {
				t.Fatalf("proof=%+v err=%v, want %q", proof, err, tt.want)
			}
			encoded, marshalErr := json.Marshal(proof)
			if marshalErr != nil || !json.Valid(encoded) {
				t.Fatalf("failure result is not JSON: %s (%v)", encoded, marshalErr)
			}
		})
	}
}

func TestPlanQueuedRequeueProofIsSideEffectFree(t *testing.T) {
	proof, err := PlanQueuedRequeueProof("item-1")
	if err != nil || !proof.DryRun || !proof.Planned || proof.Passed || len(proof.Events) != 0 {
		t.Fatalf("proof=%+v err=%v", proof, err)
	}
	if encoded, err := json.Marshal(proof); err != nil || !json.Valid(encoded) {
		t.Fatalf("dry-run JSON=%s err=%v", encoded, err)
	}
}
