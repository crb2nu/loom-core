package webhookbus

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestMatchingIsolationAndUnsubscribe(t *testing.T) {
	b := New(2)
	ch, stop := b.Subscribe("services/loom-core", "abc", 42)
	b.Publish(Event{Project: "other", SHA: "abc", MRIID: 42})
	b.Publish(Event{Project: "services/loom-core", SHA: "abc"})
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("matching event not delivered")
	}
	stop()
	stop()
	if got := b.SubscriberCount(); got != 0 {
		t.Fatalf("subscribers=%d, want 0", got)
	}
}

func TestOverflowDropsOldestAndCounts(t *testing.T) {
	b := New(1)
	ch, stop := b.Subscribe("p", "s", 0)
	defer stop()
	before := testutil.ToFloat64(EventsDropped)
	b.Publish(Event{Project: "p", SHA: "s", MRIID: 1})
	b.Publish(Event{Project: "p", SHA: "s", MRIID: 2})
	if got := (<-ch).MRIID; got != 2 {
		t.Fatalf("retained event=%d, want newest 2", got)
	}
	if got := testutil.ToFloat64(EventsDropped); got != before+1 {
		t.Fatalf("drops=%v, want %v", got, before+1)
	}
}
