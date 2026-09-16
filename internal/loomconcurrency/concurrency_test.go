package loomconcurrency

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestConcurrencyClampsZeroToOne(t *testing.T) {
	if got := NewConcurrency(0).Limit(); got != MinLimit {
		t.Fatalf("limit = %d, want %d", got, MinLimit)
	}
}

func TestConcurrencyBoundsAdmissions(t *testing.T) {
	const limit = 3
	gate := NewConcurrency(limit)
	release := make(chan struct{})
	acquired := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := gate.Acquire(context.Background()); err != nil {
				t.Errorf("Acquire: %v", err)
				return
			}
			acquired <- struct{}{}
			<-release
			gate.Release()
		}()
	}
	for range limit {
		<-acquired
	}
	select {
	case <-acquired:
		t.Fatalf("more than %d admissions acquired concurrently", limit)
	default:
	}
	close(release)
	wg.Wait()
}

func TestConcurrencySetLimitPreservesInflightAdmissions(t *testing.T) {
	gate := NewConcurrency(2)
	if err := gate.Acquire(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := gate.Acquire(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := gate.SetLimit(1); err != nil {
		t.Fatal(err)
	}
	gate.Release()

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if err := gate.Acquire(ctx); err == nil {
		t.Fatal("lower limit admitted work while one holder remained")
	}
	gate.Release()
}
