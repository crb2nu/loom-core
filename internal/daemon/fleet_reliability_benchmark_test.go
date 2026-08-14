package daemon

import (
	"log/slog"
	"testing"
)

func BenchmarkFleetDaemonEventPublish(b *testing.B) {
	bus := NewEventBus(slog.New(slog.DiscardHandler))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		bus.Publish(EventType("fleet.benchmark"), i)
	}
}
