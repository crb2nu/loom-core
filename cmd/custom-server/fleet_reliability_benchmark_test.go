package main

import (
	"sync"
	"testing"
)

type fleetBenchmarkWSWriter struct{}

func (fleetBenchmarkWSWriter) WriteMessage(_ int, _ []byte) error { return nil }

func BenchmarkFleetCustomServerWrite(b *testing.B) {
	var mu sync.Mutex
	writer := fleetBenchmarkWSWriter{}
	payload := []byte(`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := writeWS(&mu, writer, 1, payload); err != nil {
			b.Fatalf("write: %v", err)
		}
	}
}
