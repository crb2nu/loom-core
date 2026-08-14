package eventpub

import (
	"reflect"
	"testing"

	"github.com/crb2nu/loom/pkg/telemetry"
)

type recordingPublisher struct {
	eventType string
	payload   any
}

func (p *recordingPublisher) Publish(eventType string, payload any) {
	p.eventType = eventType
	p.payload = payload
}

func TestGateResultPublisherEmitsStructuredEvent(t *testing.T) {
	publisher := &recordingPublisher{}
	sink := GateResultPublisher{Publisher: publisher}
	want := telemetry.GateResultEvent{
		GateID:      "docs_guardrail",
		Verdict:     telemetry.GateVerdictFail,
		ParseStatus: telemetry.GateParseStatusParsed,
	}

	sink.RecordGateResultEvent(want)

	if publisher.eventType != GateResultEventType {
		t.Fatalf("event type = %q, want %q", publisher.eventType, GateResultEventType)
	}
	if !reflect.DeepEqual(publisher.payload, want) {
		t.Fatalf("payload = %#v, want %#v", publisher.payload, want)
	}
}

func TestGateResultPublisherAllowsNilPublisher(t *testing.T) {
	GateResultPublisher{}.RecordGateResultEvent(telemetry.GateResultEvent{})
}
