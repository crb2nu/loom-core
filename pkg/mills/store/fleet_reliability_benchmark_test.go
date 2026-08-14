package store

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
)

func BenchmarkFleetMillsEventAppend(b *testing.B) {
	ctx := context.Background()
	store, err := Open(ctx, Options{Path: filepath.Join(b.TempDir(), "mills.db")})
	if err != nil {
		b.Fatalf("open store: %v", err)
	}
	b.Cleanup(func() { _ = store.Close() })
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		event := &Event{
			Actor:       "fleet-benchmark",
			Kind:        "benchmark.append",
			SubjectKind: "iteration",
			SubjectID:   fmt.Sprintf("%d", i),
		}
		if err := store.Events.Append(ctx, event); err != nil {
			b.Fatalf("append event: %v", err)
		}
	}
}
