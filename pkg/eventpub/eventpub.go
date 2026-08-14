package eventpub

import "github.com/crb2nu/loom/pkg/telemetry"

// GateResultEventType is the stable EventBus type for gate-result telemetry.
const GateResultEventType = "mills.gate_result"

// Publisher is the minimal event contract used by in-process producers.
// HTTPPublisher satisfies it; tests and embedders may provide synchronous
// implementations.
type Publisher interface {
	Publish(eventType string, payload any)
}

// GateResultPublisher adapts a generic Publisher to the gate telemetry sink.
// Delivery remains best-effort according to the wrapped publisher.
type GateResultPublisher struct {
	Publisher Publisher
}

// RecordGateResultEvent publishes one structured gate-result event.
func (p GateResultPublisher) RecordGateResultEvent(event telemetry.GateResultEvent) {
	if p.Publisher != nil {
		p.Publisher.Publish(GateResultEventType, event)
	}
}
