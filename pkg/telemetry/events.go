package telemetry

// GateVerdictError records an evaluation that could not produce a verdict.
const GateVerdictError GateVerdict = "error"

// GateEvaluationEvent is the shared structured event emitted for each
// instrumented gate evaluation, including evaluations that return an error.
type GateEvaluationEvent struct {
	GateID     string      `json:"gate_id"`
	RunID      string      `json:"run_id"`
	Verdict    GateVerdict `json:"verdict"`
	ParseOK    bool        `json:"parse_ok"`
	DurationMS int64       `json:"duration_ms"`
	Attempt    uint64      `json:"attempt"`
}

// GateEvaluationEventSink receives structured gate-evaluation events.
type GateEvaluationEventSink interface {
	RecordGateEvaluationEvent(GateEvaluationEvent)
}

// GateEvaluationEventSinkFunc adapts a function to GateEvaluationEventSink.
type GateEvaluationEventSinkFunc func(GateEvaluationEvent)

// RecordGateEvaluationEvent implements GateEvaluationEventSink.
func (f GateEvaluationEventSinkFunc) RecordGateEvaluationEvent(event GateEvaluationEvent) {
	if f != nil {
		f(event)
	}
}
