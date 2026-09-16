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

type scopeFailureRecorder struct {
	mu      sync.Mutex
	classes []telemetry.ScopeFailureClass
}

func (r *scopeFailureRecorder) RecordScopeFailure(_ context.Context, class telemetry.ScopeFailureClass) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.classes = append(r.classes, class)
}

func (r *scopeFailureRecorder) snapshot() []telemetry.ScopeFailureClass {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]telemetry.ScopeFailureClass(nil), r.classes...)
}

func TestRegistryClassifiesEachScopeFailureExactlyOnce(t *testing.T) {
	tests := []struct {
		name     string
		declared string
		want     telemetry.ScopeFailureClass
	}{
		{"missing directory", "not-a-real-scope-directory/file.go", telemetry.ScopeFailureMissingDirectory},
		{"wrong basename", "pkg/mills/gates/not-the-real-basename.go", telemetry.ScopeFailureWrongBasename},
		{"genuine detour", "pkg/mills/gates/scope.go", telemetry.ScopeFailureGenuineDetour},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := &scopeFailureRecorder{}
			restore := telemetry.SetScopeFailureRecorderForTest(recorder)
			defer restore()
			collector := &telemetry.GateDeterminismHarness{}
			registry := NewRegistry()
			registry.Register(&Scope{})
			registry.SetTelemetrySink(collector)

			_, passed, err := registry.EvaluateAll(context.Background(), []string{"scope"}, StageInput{
				RunID:        "scope-classification",
				Item:         fixtureItem(store.Slice{Name: "code", Files: []string{tt.declared}}),
				FilesChanged: []string{"cmd/mcp-git/main.go"},
			})
			if err != nil || passed {
				t.Fatalf("EvaluateAll passed=%v err=%v", passed, err)
			}
			classes := recorder.snapshot()
			if len(classes) != 1 || classes[0] != tt.want {
				t.Fatalf("counter classes = %v, want exactly [%s]", classes, tt.want)
			}
			records := collector.Records()
			if len(records) != 1 || records[0].ScopeFailureClass != tt.want {
				t.Fatalf("structured records = %+v, want class %s", records, tt.want)
			}
		})
	}
}

func TestRegistryDoesNotClassifyScopePassOrSkip(t *testing.T) {
	recorder := &scopeFailureRecorder{}
	restore := telemetry.SetScopeFailureRecorderForTest(recorder)
	defer restore()
	collector := &telemetry.GateDeterminismHarness{}
	registry := NewRegistry()
	registry.Register(&Scope{})
	registry.SetTelemetrySink(collector)

	inputs := []StageInput{
		{Item: fixtureItem(store.Slice{Name: "code", Files: []string{"pkg/mills/gates/scope.go"}}), FilesChanged: []string{"pkg/mills/gates/scope.go"}},
		{Item: fixtureItem(), FilesChanged: []string{"pkg/mills/gates/scope.go"}},
	}
	for _, in := range inputs {
		if _, passed, err := registry.EvaluateAll(context.Background(), []string{"scope"}, in); err != nil || !passed {
			t.Fatalf("EvaluateAll passed=%v err=%v", passed, err)
		}
	}
	if got := recorder.snapshot(); len(got) != 0 {
		t.Fatalf("counter classes = %v, want none", got)
	}
	for _, record := range collector.Records() {
		if record.ScopeFailureClass != "" {
			t.Fatalf("pass/skip record has scope failure class: %+v", record)
		}
	}
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
