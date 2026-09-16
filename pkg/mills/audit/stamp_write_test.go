package audit

import (
	"context"
	"testing"
)

func TestStampWriteEmitterFuncPreservesStructuredEvent(t *testing.T) {
	want := StampWriteEvent{
		SourceProject: "services/loom-core",
		TargetProject: "services/flexdeck",
		StampID:       "stamp-1",
		Decision:      StampWriteRejected,
		Reason:        "policy_denied",
	}
	var got StampWriteEvent
	StampWriteEmitterFunc(func(_ context.Context, event StampWriteEvent) { got = event }).EmitStampWrite(context.Background(), want)
	if got != want {
		t.Fatalf("event = %+v, want %+v", got, want)
	}
}
