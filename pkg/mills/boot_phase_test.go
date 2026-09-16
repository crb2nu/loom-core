package mills

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestBootPhase_ReleaseUnblocksWaitAndIsIdempotent(t *testing.T) {
	p := NewBootPhase("warmup", nil)
	select {
	case <-p.Done():
		t.Fatal("phase released before Release")
	default:
	}
	p.Release()
	p.Release() // idempotent
	if !p.Wait(context.Background(), time.Second) {
		t.Fatal("Wait after Release should report released")
	}
	select {
	case <-p.Done():
	default:
		t.Fatal("Done channel not closed after Release")
	}
}

func TestBootPhase_WaitBudgetExpiryProceeds(t *testing.T) {
	p := NewBootPhase("wedged", nil)
	started := time.Now()
	if p.Wait(context.Background(), 20*time.Millisecond) {
		t.Fatal("unreleased phase reported released")
	}
	if time.Since(started) < 20*time.Millisecond {
		t.Fatal("Wait returned before its budget elapsed")
	}
}

func TestBootPhase_WaitReturnsOnCancel(t *testing.T) {
	p := NewBootPhase("cancelled", nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if p.Wait(ctx, time.Hour) {
		t.Fatal("cancelled wait reported released")
	}
}

// TestBootPhase_GateOrdersLoops is the boot-ordering contract: a gated loop
// does not start until the phase it waits on is released, and once released
// the loop runs with the caller's context.
func TestBootPhase_GateOrdersLoops(t *testing.T) {
	p := NewBootPhase("reconciler_boot_tick", nil)
	var order atomic.Int32
	loopStarted := make(chan int32, 1)
	loopErr := errors.New("loop done")
	done := make(chan error, 1)
	go func() {
		done <- p.Gate(context.Background(), time.Hour, func(context.Context) error {
			loopStarted <- order.Add(1)
			return loopErr
		})
	}()
	select {
	case <-loopStarted:
		t.Fatal("gated loop started before the phase was released")
	case <-time.After(30 * time.Millisecond):
	}
	// The owning loop finishes its first pass, then the gated loop runs.
	first := order.Add(1)
	p.Release()
	select {
	case got := <-loopStarted:
		if got <= first {
			t.Fatalf("gated loop ordered %d, want after owner %d", got, first)
		}
	case <-time.After(time.Second):
		t.Fatal("gated loop did not start after release")
	}
	if err := <-done; !errors.Is(err, loopErr) {
		t.Fatalf("Gate returned %v, want the loop's error", err)
	}
}

func TestBootPhase_GateSkipsLoopOnCancelledContext(t *testing.T) {
	p := NewBootPhase("never", nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ran := false
	err := p.Gate(ctx, time.Hour, func(context.Context) error { ran = true; return errors.New("should not run") })
	if err != nil || ran {
		t.Fatalf("Gate on cancelled ctx: err=%v ran=%v; want nil and not run", err, ran)
	}
}

func TestBootPhase_NilIsReleased(t *testing.T) {
	var p *BootPhase
	p.Release()
	if !p.Wait(context.Background(), time.Millisecond) {
		t.Fatal("nil phase should read as released")
	}
	ran := false
	if err := p.Gate(context.Background(), time.Millisecond, func(context.Context) error { ran = true; return nil }); err != nil || !ran {
		t.Fatalf("nil Gate: err=%v ran=%v", err, ran)
	}
}
