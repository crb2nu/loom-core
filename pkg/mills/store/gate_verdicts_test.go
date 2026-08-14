package store

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/crb2nu/loom/pkg/telemetry"
)

func TestGateVerdictStorePersistsByRun(t *testing.T) {
	st := newTestStore(t)
	sink := NewGateVerdictStore(st)
	for _, verdict := range []telemetry.GateVerdict{telemetry.GateVerdictPass, telemetry.GateVerdictFail, telemetry.GateVerdictSkip, telemetry.GateVerdictError} {
		err := sink.Persist(context.Background(), telemetry.GateEvaluation{
			GateID: "scope", RunID: "run-verdicts", Verdict: verdict,
			Reason: "evidence", DurationMS: -1,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	events, err := st.Events.ListBySubject(context.Background(), gateVerdictSubjectKind, "run-verdicts", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 4 {
		t.Fatalf("events = %d, want 4", len(events))
	}
	for _, event := range events {
		if event.Kind != gateVerdictEventKind || event.Payload["run_id"] != "run-verdicts" || event.Payload["reason"] != "evidence" {
			t.Errorf("event = %+v", event)
		}
		if got := event.Payload["duration_ms"]; got != float64(0) {
			t.Errorf("duration_ms = %#v, want 0", got)
		}
	}
}

func TestGateVerdictStoreConcurrentPersistence(t *testing.T) {
	st := newTestStore(t)
	sink := NewGateVerdictStore(st)
	const count = 32
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sink.RecordGateEvaluation(telemetry.GateEvaluation{
				GateID: fmt.Sprintf("gate-%d", i), RunID: "run-concurrent",
				Verdict: telemetry.GateVerdictPass, DurationMS: int64(i),
			})
		}(i)
	}
	wg.Wait()
	if err := sink.LastError(); err != nil {
		t.Fatal(err)
	}
	events, err := st.Events.ListBySubject(context.Background(), gateVerdictSubjectKind, "run-concurrent", count)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != count {
		t.Fatalf("events = %d, want %d", len(events), count)
	}
}
