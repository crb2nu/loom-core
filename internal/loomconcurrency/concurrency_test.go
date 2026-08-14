package loomconcurrency

import (
	"context"
	"sync"
	"testing"
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
