package gates

import (
	"context"

	"github.com/crb2nu/loom/pkg/telemetry"
)

// TelemetryGate instruments one gate without changing its evaluation result.
// A single instance may be evaluated concurrently.
type TelemetryGate struct {
	gate Gate
	sink telemetry.GateResultEventSink
}

// GateFlakeDetectedEvent is emitted when two evaluations of the same
// canonical gate input disagree and the pipeline grants its bounded re-check.
type GateFlakeDetectedEvent struct {
	GateID             string                `json:"gate_id"`
	RunID              string                `json:"run_id"`
	InputHash          string                `json:"input_hash"`
	PreviousVerdict    telemetry.GateVerdict `json:"previous_verdict"`
	ConflictingVerdict telemetry.GateVerdict `json:"conflicting_verdict"`
	RecheckAttempt     uint64                `json:"recheck_attempt"`
}

// GateFlakeDetectedEventSink receives structured gate-flake signals.
type GateFlakeDetectedEventSink interface {
	RecordGateFlakeDetected(GateFlakeDetectedEvent)
}

// GateFlakeDetectedEventSinkFunc adapts a function to a flake event sink.
type GateFlakeDetectedEventSinkFunc func(GateFlakeDetectedEvent)

// RecordGateFlakeDetected implements GateFlakeDetectedEventSink.
func (f GateFlakeDetectedEventSinkFunc) RecordGateFlakeDetected(event GateFlakeDetectedEvent) {
	if f != nil {
		f(event)
	}
}

// EmitGateFlakeDetected sends a flake signal without allowing a nil or
// panicking telemetry sink to affect the pipeline decision.
func EmitGateFlakeDetected(sink GateFlakeDetectedEventSink, event GateFlakeDetectedEvent) {
	if sink == nil {
		return
	}
	defer func() {
		_ = recover()
	}()
	sink.RecordGateFlakeDetected(event)
}

// InstrumentGate adds structured evaluation telemetry to gate. The adapter is
// intended for the deterministic docs_guardrail and scope gates.
func InstrumentGate(gate Gate, sink telemetry.GateResultEventSink) *TelemetryGate {
	if gate == nil {
		panic("gates: cannot instrument nil gate")
	}
	switch gate.Name() {
	case "docs_guardrail", "scope":
	default:
		panic("gates: structured evaluation telemetry only supports docs_guardrail and scope")
	}
	return &TelemetryGate{gate: gate, sink: sink}
}

// Name preserves the wrapped gate's stable persisted identifier.
func (g *TelemetryGate) Name() string { return g.gate.Name() }

// Evaluate delegates to the wrapped gate and emits exactly one event for
// pass, fail, skip, and error outcomes. Sink failures are isolated from gate
// behavior because observability must never change a pipeline verdict.
func (g *TelemetryGate) Evaluate(ctx context.Context, in StageInput) (out Outcome, err error) {
	out, err = g.gate.Evaluate(ctx, in)

	event := telemetry.GateResultEvent{
		GateID:      g.Name(),
		Verdict:     telemetryVerdictFor(out),
		ParseStatus: telemetry.GateParseStatusParsed,
	}
	if err != nil {
		event.Verdict = telemetry.GateVerdictError
		event.ParseStatus = telemetry.GateParseStatusParseError
	}
	g.emit(event)
	return out, err
}

func (g *TelemetryGate) emit(event telemetry.GateResultEvent) {
	if g.sink == nil {
		return
	}
	defer func() {
		_ = recover()
	}()
	g.sink.RecordGateResultEvent(event)
}
