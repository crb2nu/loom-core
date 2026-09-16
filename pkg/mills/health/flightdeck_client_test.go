package health

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

type testCounter struct{ n atomic.Int64 }

func (c *testCounter) Inc() { c.n.Add(1) }

func TestFlightdeckClientDeliversAuthenticatedBatch(t *testing.T) {
	received := make(chan flightdeckBatch, 1)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != flightdeckBatchPath {
			t.Errorf("path=%q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer secret" {
			t.Errorf("auth=%q", got)
		}
		var batch flightdeckBatch
		if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
			t.Error(err)
		}
		received <- batch
		w.WriteHeader(http.StatusOK)
	}))
	defer s.Close()
	c := NewFlightdeckClient(FlightdeckClientConfig{Endpoint: s.URL, Token: "secret", Timeout: time.Second})
	defer c.Close()
	c.Emit(FactoryEvent{Type: "factory.run.started", RunID: "RUN-1", BacklogID: "BACK-1", Stage: "plan_slice", OccurredAt: time.Unix(1, 0).UTC()})
	select {
	case batch := <-received:
		if len(batch.Events) != 1 || batch.Events[0].Platform != "loom-mills" || batch.Events[0].Origin != "factory" || batch.Events[0].EventType != "factory.run.started" {
			t.Fatalf("batch=%+v", batch)
		}
		if batch.Events[0].Payload.RunID != "RUN-1" {
			t.Fatalf("payload=%+v", batch.Events[0].Payload)
		}
		payload, err := json.Marshal(batch.Events[0].Payload)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]any
		if err := json.Unmarshal(payload, &fields); err != nil {
			t.Fatal(err)
		}
		if attempt, ok := fields["attempt"]; !ok || attempt != float64(0) {
			t.Fatalf("zero attempt missing from payload: %s", payload)
		}
	case <-time.After(time.Second):
		t.Fatal("delivery timed out")
	}
}

func TestFlightdeckClientRetriesAndCountsExhaustion(t *testing.T) {
	var requests atomic.Int64
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer s.Close()
	errs := &testCounter{}
	c := NewFlightdeckClient(FlightdeckClientConfig{Endpoint: s.URL, Token: "x", Timeout: time.Second, MaxAttempts: 3, Errors: errs})
	c.Emit(FactoryEvent{Type: "factory.run.terminal", RunID: "R", OccurredAt: time.Now()})
	c.Close()
	if requests.Load() != 3 || errs.n.Load() != 1 {
		t.Fatalf("requests=%d errors=%d", requests.Load(), errs.n.Load())
	}
}

func TestFlightdeckClientEmitIsNonBlockingAndOverflowCounts(t *testing.T) {
	blocked := make(chan struct{})
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { <-blocked; w.WriteHeader(http.StatusOK) }))
	defer s.Close()
	errs := &testCounter{}
	c := NewFlightdeckClient(FlightdeckClientConfig{Endpoint: s.URL, Token: "x", QueueSize: 1, Timeout: 50 * time.Millisecond, MaxAttempts: 1, Errors: errs})
	start := time.Now()
	for i := 0; i < 100; i++ {
		c.Emit(FactoryEvent{Type: "factory.stage.transition", RunID: "R", Attempt: i, OccurredAt: time.Now()})
	}
	if time.Since(start) > 50*time.Millisecond {
		t.Fatal("Emit blocked on transport")
	}
	if errs.n.Load() == 0 {
		t.Fatal("queue overflow was not counted")
	}
	close(blocked)
	c.Close()
}

func TestFlightdeckClientDisabledConstructsNothing(t *testing.T) {
	if got := NewFlightdeckClient(FlightdeckClientConfig{}); got != nil {
		t.Fatalf("got %#v", got)
	}
}
