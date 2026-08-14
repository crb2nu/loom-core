package pipeline

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/crb2nu/loom/pkg/mills/gates"
	"github.com/crb2nu/loom/pkg/telemetry"
)

type flakeEventRecorder struct {
	mu     sync.Mutex
	events []gates.GateFlakeDetectedEvent
}

func (r *flakeEventRecorder) RecordGateFlakeDetected(event gates.GateFlakeDetectedEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
}

func (r *flakeEventRecorder) snapshot() []gates.GateFlakeDetectedEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]gates.GateFlakeDetectedEvent(nil), r.events...)
}

func TestGateRecheckPolicy(t *testing.T) {
	tests := []struct {
		name         string
		observations []GateRecheckObservation
		wantRechecks []bool
		wantEvents   int
	}{
		{
			name: "stable verdicts",
			observations: []GateRecheckObservation{
				{GateID: "scope", RunID: "run", InputHash: "same", Verdict: telemetry.GateVerdictPass},
				{GateID: "scope", RunID: "run", InputHash: "same", Verdict: telemetry.GateVerdictPass},
			},
			wantRechecks: []bool{false, false},
		},
		{
			name: "pass to fail flip",
			observations: []GateRecheckObservation{
				{GateID: "docs_guardrail", RunID: "run", InputHash: "same", Verdict: telemetry.GateVerdictPass},
				{GateID: "docs_guardrail", RunID: "run", InputHash: "same", Verdict: telemetry.GateVerdictFail},
			},
			wantRechecks: []bool{false, true},
			wantEvents:   1,
		},
		{
			name: "fail to pass flip",
			observations: []GateRecheckObservation{
				{GateID: "scope", RunID: "run", InputHash: "same", Verdict: telemetry.GateVerdictFail},
				{GateID: "scope", RunID: "run", InputHash: "same", Verdict: telemetry.GateVerdictPass},
			},
			wantRechecks: []bool{false, true},
			wantEvents:   1,
		},
		{
			name: "changed hash does not qualify",
			observations: []GateRecheckObservation{
				{GateID: "scope", RunID: "run", InputHash: "first", Verdict: telemetry.GateVerdictPass},
				{GateID: "scope", RunID: "run", InputHash: "changed", Verdict: telemetry.GateVerdictFail},
			},
			wantRechecks: []bool{false, false},
		},
		{
			name: "unsupported gate does not qualify",
			observations: []GateRecheckObservation{
				{GateID: "diff_size", RunID: "run", InputHash: "same", Verdict: telemetry.GateVerdictPass},
				{GateID: "diff_size", RunID: "run", InputHash: "same", Verdict: telemetry.GateVerdictFail},
			},
			wantRechecks: []bool{false, false},
		},
		{
			name: "only one free recheck",
			observations: []GateRecheckObservation{
				{GateID: "scope", RunID: "run", InputHash: "same", Verdict: telemetry.GateVerdictPass},
				{GateID: "scope", RunID: "run", InputHash: "same", Verdict: telemetry.GateVerdictFail},
				{GateID: "scope", RunID: "run", InputHash: "same", Verdict: telemetry.GateVerdictPass},
				{GateID: "scope", RunID: "run", InputHash: "same", Verdict: telemetry.GateVerdictFail},
			},
			wantRechecks: []bool{false, true, false, false},
			wantEvents:   1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := &flakeEventRecorder{}
			policy := NewGateRecheckPolicy(recorder)
			for i, observation := range tt.observations {
				got := policy.Observe(observation)
				if got.Recheck != tt.wantRechecks[i] {
					t.Fatalf("observation %d decision = %+v, want recheck %v", i, got, tt.wantRechecks[i])
				}
				if got.Recheck && (!got.GateOnly || got.ConsumesRetry || got.RecheckAttempt != 1) {
					t.Fatalf("free recheck decision = %+v", got)
				}
			}
			if got := len(recorder.snapshot()); got != tt.wantEvents {
				t.Fatalf("events = %d, want %d", got, tt.wantEvents)
			}
		})
	}
}

func TestGateRecheckPolicyEventFields(t *testing.T) {
	recorder := &flakeEventRecorder{}
	policy := NewGateRecheckPolicy(recorder)
	policy.Observe(GateRecheckObservation{
		GateID: "scope", RunID: "run-42", InputHash: "sha256:abc",
		Verdict: telemetry.GateVerdictPass,
	})
	policy.Observe(GateRecheckObservation{
		GateID: "scope", RunID: "run-42", InputHash: "sha256:abc",
		Verdict: telemetry.GateVerdictFail,
	})

	events := recorder.snapshot()
	if len(events) != 1 {
		t.Fatalf("events = %+v, want one", events)
	}
	want := gates.GateFlakeDetectedEvent{
		GateID: "scope", RunID: "run-42", InputHash: "sha256:abc",
		PreviousVerdict:    telemetry.GateVerdictPass,
		ConflictingVerdict: telemetry.GateVerdictFail, RecheckAttempt: 1,
	}
	if events[0] != want {
		t.Fatalf("event = %+v, want %+v", events[0], want)
	}
}

func TestGateRecheckPolicyIsolatesRunsAndGates(t *testing.T) {
	policy := NewGateRecheckPolicy(nil)
	for _, observation := range []GateRecheckObservation{
		{GateID: "scope", RunID: "run-a", InputHash: "hash", Verdict: telemetry.GateVerdictPass},
		{GateID: "docs_guardrail", RunID: "run-a", InputHash: "hash", Verdict: telemetry.GateVerdictFail},
		{GateID: "scope", RunID: "run-b", InputHash: "hash", Verdict: telemetry.GateVerdictFail},
	} {
		if got := policy.Observe(observation); got.Recheck {
			t.Fatalf("isolated first observation qualified: %+v", observation)
		}
	}
	if got := policy.Observe(GateRecheckObservation{
		GateID: "scope", RunID: "run-a", InputHash: "hash",
		Verdict: telemetry.GateVerdictFail,
	}); !got.Recheck {
		t.Fatalf("matching run/gate/hash flip did not qualify: %+v", got)
	}
}

func TestGateRecheckPolicyTelemetryCannotChangeDecision(t *testing.T) {
	panicSink := gates.GateFlakeDetectedEventSinkFunc(func(gates.GateFlakeDetectedEvent) {
		panic("telemetry unavailable")
	})
	for _, sink := range []gates.GateFlakeDetectedEventSink{nil, panicSink} {
		policy := NewGateRecheckPolicy(sink)
		policy.Observe(GateRecheckObservation{
			GateID: "scope", RunID: "run", InputHash: "hash",
			Verdict: telemetry.GateVerdictPass,
		})
		got := policy.Observe(GateRecheckObservation{
			GateID: "scope", RunID: "run", InputHash: "hash",
			Verdict: telemetry.GateVerdictFail,
		})
		if !got.Recheck || got.ConsumesRetry {
			t.Fatalf("sink changed decision: %+v", got)
		}
	}
}

func TestGateRecheckPolicyConcurrentCap(t *testing.T) {
	recorder := &flakeEventRecorder{}
	policy := NewGateRecheckPolicy(recorder)
	observation := GateRecheckObservation{
		GateID: "scope", RunID: "run", InputHash: "hash",
		Verdict: telemetry.GateVerdictPass,
	}
	policy.Observe(observation)
	observation.Verdict = telemetry.GateVerdictFail

	const workers = 64
	var grants atomic.Int64
	var wg sync.WaitGroup
	wg.Add(workers)
	for range workers {
		go func() {
			defer wg.Done()
			if policy.Observe(observation).Recheck {
				grants.Add(1)
			}
		}()
	}
	wg.Wait()

	if got := grants.Load(); got != 1 {
		t.Fatalf("free recheck grants = %d, want 1", got)
	}
	if got := len(recorder.snapshot()); got != 1 {
		t.Fatalf("flake events = %d, want 1", got)
	}
}
