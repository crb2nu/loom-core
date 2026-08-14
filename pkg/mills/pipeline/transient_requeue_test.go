package pipeline

import (
	"context"
	"errors"
	"testing"

	"github.com/crb2nu/loom/pkg/mills/store"
	"github.com/crb2nu/loom/pkg/telemetry"
)

type requeueRecorder struct {
	eligible []string
	blocked  []string
}

func (r *requeueRecorder) RecordEscalationRequeueEligible(_ context.Context, class string) {
	r.eligible = append(r.eligible, class)
}
func (r *requeueRecorder) RecordEscalationRequeueBlocked(_ context.Context, reason string) {
	r.blocked = append(r.blocked, reason)
}

type fakeRequeueClaimer struct {
	claim store.TransientRequeueClaim
	err   error
	calls int
}

func (f *fakeRequeueClaimer) ClaimTransientRequeue(context.Context, string, int) (store.TransientRequeueClaim, error) {
	f.calls++
	return f.claim, f.err
}

func TestDecideTransientRequeueFailsClosed(t *testing.T) {
	transient := func(class FailureClass) FailureClassification {
		return FailureClassification{Class: class, Retryable: true}
	}
	tests := []struct {
		name      string
		failure   FailureClassification
		claim     store.TransientRequeueClaim
		err       error
		want      FailureRoute
		exhausted bool
		calls     int
		metric    string
		cap       int
		nilStore  bool
	}{
		{name: "transient", failure: transient(FailureTransient), claim: store.TransientRequeueClaim{Claimed: true, AttemptsUsed: 1, Cap: 2}, want: FailureRouteRequeue, calls: 1, metric: "transient", cap: 2},
		{name: "transient quota", failure: transient(FailureTransientQuota), claim: store.TransientRequeueClaim{Claimed: true, AttemptsUsed: 1, Cap: 2}, want: FailureRouteRequeue, calls: 1, metric: "transient_quota", cap: 2},
		{name: "runner system infrastructure", failure: transient(FailureInfrastructure), claim: store.TransientRequeueClaim{Claimed: true, AttemptsUsed: 2, Cap: 2}, want: FailureRouteRequeue, calls: 1, metric: "infrastructure", cap: 2},
		{name: "exhausted", failure: transient(FailureTransient), claim: store.TransientRequeueClaim{AttemptsUsed: 2, Cap: 2}, want: FailureRouteEscalate, exhausted: true, calls: 1, metric: "budget_exhausted", cap: 2},
		{name: "persistence error", failure: transient(FailureTransient), err: errors.New("sqlite unavailable"), want: FailureRouteEscalate, exhausted: true, calls: 1, metric: "persistence", cap: 2},
		{name: "nil store", failure: transient(FailureTransient), want: FailureRouteEscalate, exhausted: true, metric: "budget_unavailable", cap: 2, nilStore: true},
		{name: "zero cap", failure: transient(FailureTransient), want: FailureRouteEscalate, exhausted: true, metric: "budget_unavailable"},
		{name: "terminal", failure: FailureClassification{Class: FailureTransient, Retryable: true, Terminal: true}, want: FailureRouteEscalate, metric: "classification", cap: 2},
		{name: "code", failure: FailureClassification{Class: FailureCode, Retryable: true}, want: FailureRouteEscalate, metric: "classification", cap: 2},
		{name: "config", failure: FailureClassification{Class: FailureConfiguration, Retryable: true}, want: FailureRouteEscalate, metric: "classification", cap: 2},
		{name: "unknown", failure: transient(FailureClass("future")), want: FailureRouteEscalate, metric: "classification", cap: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := &requeueRecorder{}
			restore := telemetry.SetEscalationRequeueRecorderForTest(recorder)
			defer restore()
			f := &fakeRequeueClaimer{claim: tt.claim, err: tt.err}
			var claimer TransientRequeueClaimer = f
			if tt.nilStore {
				claimer = nil
			}
			got := DecideTransientRequeue(context.Background(), claimer, "BACKLOG-1", tt.failure, tt.cap)
			if got.Route != tt.want || got.Exhausted != tt.exhausted || f.calls != tt.calls {
				t.Fatalf("decision=%+v calls=%d", got, f.calls)
			}
			if tt.err != nil && got.PersistenceError == "" {
				t.Fatal("persistence error evidence missing")
			}
			if got.Route == FailureRouteRequeue {
				if len(recorder.eligible) != 1 || recorder.eligible[0] != tt.metric || len(recorder.blocked) != 0 {
					t.Fatalf("eligible=%v blocked=%v", recorder.eligible, recorder.blocked)
				}
			} else if len(recorder.blocked) != 1 || recorder.blocked[0] != tt.metric || len(recorder.eligible) != 0 {
				t.Fatalf("eligible=%v blocked=%v", recorder.eligible, recorder.blocked)
			}
		})
	}
}
