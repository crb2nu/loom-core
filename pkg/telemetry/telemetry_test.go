package telemetry

import (
	"context"
	"sync"
	"testing"
)

func TestCouncilIntentsMissingCounterConcurrent(t *testing.T) {
	const calls = 100
	before := CouncilIntentsMissingTotal()

	var wg sync.WaitGroup
	wg.Add(calls)
	for range calls {
		go func() {
			defer wg.Done()
			RecordCouncilIntentsMissing()
		}()
	}
	wg.Wait()

	if got := CouncilIntentsMissingTotal() - before; got != calls {
		t.Fatalf("counter delta = %d, want %d", got, calls)
	}
}

func TestRecordIntakeFailclosedRejectionIncrementsOnce(t *testing.T) {
	before := IntakeFailclosedRejectionsTotal()
	RecordIntakeFailclosedRejection(context.Background())
	if got := IntakeFailclosedRejectionsTotal(); got != before+1 {
		t.Fatalf("IntakeFailclosedRejectionsTotal() = %d, want %d", got, before+1)
	}
}

type escalationRequeueCapture struct {
	eligible []string
	blocked  []string
}

func (r *escalationRequeueCapture) RecordEscalationRequeueEligible(_ context.Context, class string) {
	r.eligible = append(r.eligible, class)
}
func (r *escalationRequeueCapture) RecordEscalationRequeueBlocked(_ context.Context, reason string) {
	r.blocked = append(r.blocked, reason)
}

func TestEscalationRequeueLabelsAreBounded(t *testing.T) {
	recorder := &escalationRequeueCapture{}
	restore := SetEscalationRequeueRecorderForTest(recorder)
	defer restore()

	RecordEscalationRequeueEligible(context.Background(), "raw future class")
	RecordEscalationRequeueBlocked(context.Background(), "sqlite unavailable: secret detail")

	if len(recorder.eligible) != 1 || recorder.eligible[0] != EscalationRequeueClassUnknown {
		t.Fatalf("eligible labels = %v", recorder.eligible)
	}
	if len(recorder.blocked) != 1 || recorder.blocked[0] != EscalationRequeueBlockUnknown {
		t.Fatalf("blocked labels = %v", recorder.blocked)
	}
}
