package gates

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/crb2nu/loom/pkg/mills/store"
	"github.com/crb2nu/loom/pkg/telemetry"
)

type gateEvaluationEventCollector struct {
	mu     sync.Mutex
	events []telemetry.GateResultEvent
}

func (c *gateEvaluationEventCollector) RecordGateResultEvent(event telemetry.GateResultEvent) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, event)
}

func (c *gateEvaluationEventCollector) snapshot() []telemetry.GateResultEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]telemetry.GateResultEvent(nil), c.events...)
}

type errorTelemetryGate struct {
	name string
}

func (g errorTelemetryGate) Name() string { return g.name }
func (g errorTelemetryGate) Evaluate(context.Context, StageInput) (Outcome, error) {
	return Outcome{}, errors.New("evaluation failed")
}

func TestTelemetryGateEmitsStructuredEventForEveryVerdict(t *testing.T) {
	tests := []struct {
		name        string
		gate        Gate
		in          StageInput
		want        telemetry.GateVerdict
		parseStatus telemetry.GateParseStatus
		wantError   bool
	}{
		{
			name: "docs guardrail pass",
			gate: &DocsGuardrail{},
			in: StageInput{
				RunID:        "docs-pass",
				FilesChanged: []string{"pkg/example.go", "changelog.d/example.added.md"},
			},
			want:        telemetry.GateVerdictPass,
			parseStatus: telemetry.GateParseStatusParsed,
		},
		{
			name: "docs guardrail fail",
			gate: &DocsGuardrail{},
			in: StageInput{
				RunID:        "docs-fail",
				FilesChanged: []string{"pkg/example.go"},
			},
			want:        telemetry.GateVerdictFail,
			parseStatus: telemetry.GateParseStatusParsed,
		},
		{
			name: "scope pass",
			gate: &Scope{},
			in: StageInput{
				RunID:        "scope-pass",
				Item:         fixtureItem(store.Slice{Name: "code", Files: []string{"pkg/example.go"}}),
				FilesChanged: []string{"pkg/example.go"},
			},
			want:        telemetry.GateVerdictPass,
			parseStatus: telemetry.GateParseStatusParsed,
		},
		{
			name: "scope fail",
			gate: &Scope{},
			in: StageInput{
				RunID:        "scope-fail",
				Item:         fixtureItem(store.Slice{Name: "code", Files: []string{"pkg/allowed.go"}}),
				FilesChanged: []string{"cmd/example/main.go"},
			},
			want:        telemetry.GateVerdictFail,
			parseStatus: telemetry.GateParseStatusParsed,
		},
		{
			name: "scope skip",
			gate: &Scope{},
			in: StageInput{
				RunID:        "scope-skip",
				Item:         fixtureItem(),
				FilesChanged: []string{"pkg/example.go"},
			},
			want:        telemetry.GateVerdictSkip,
			parseStatus: telemetry.GateParseStatusParsed,
		},
		{
			name:        "evaluation error",
			gate:        errorTelemetryGate{name: "scope"},
			in:          StageInput{RunID: "scope-error"},
			want:        telemetry.GateVerdictError,
			parseStatus: telemetry.GateParseStatusParseError,
			wantError:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			collector := &gateEvaluationEventCollector{}
			gate := InstrumentGate(tt.gate, collector)

			_, err := gate.Evaluate(context.Background(), tt.in)
			if (err != nil) != tt.wantError {
				t.Fatalf("Evaluate error = %v, wantError = %v", err, tt.wantError)
			}

			events := collector.snapshot()
			if len(events) != 1 {
				t.Fatalf("events = %+v, want exactly one", events)
			}
			got := events[0]
			if got.GateID != tt.gate.Name() || got.Verdict != tt.want ||
				got.ParseStatus != tt.parseStatus {
				t.Errorf("event = %+v", got)
			}
		})
	}
}

func TestTelemetryGateEmitsExactlyOnceConcurrently(t *testing.T) {
	collector := &gateEvaluationEventCollector{}
	gate := InstrumentGate(&DocsGuardrail{}, collector)
	in := StageInput{RunID: "concurrent-run"}

	const evaluations = 32
	var wg sync.WaitGroup
	wg.Add(evaluations)
	for range evaluations {
		go func() {
			defer wg.Done()
			if _, err := gate.Evaluate(context.Background(), in); err != nil {
				t.Errorf("Evaluate: %v", err)
			}
		}()
	}
	wg.Wait()

	if got := len(collector.snapshot()); got != evaluations {
		t.Errorf("events = %d, want %d", got, evaluations)
	}
}

func TestTelemetryGateIsolatesSinkPanics(t *testing.T) {
	sink := telemetry.GateResultEventSinkFunc(func(telemetry.GateResultEvent) {
		panic("sink unavailable")
	})
	gate := InstrumentGate(&DocsGuardrail{}, sink)

	out, err := gate.Evaluate(context.Background(), StageInput{RunID: "sink-panic"})
	if err != nil || !out.Pass {
		t.Fatalf("sink changed gate result: out=%+v err=%v", out, err)
	}
}
