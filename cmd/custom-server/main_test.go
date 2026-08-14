package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeWSWriter struct {
	active    atomic.Int32
	maxActive atomic.Int32
	writes    atomic.Int32
}

func (f *fakeWSWriter) WriteMessage(_ int, _ []byte) error {
	active := f.active.Add(1)
	for {
		currentMax := f.maxActive.Load()
		if active <= currentMax {
			break
		}
		if f.maxActive.CompareAndSwap(currentMax, active) {
			break
		}
	}
	time.Sleep(5 * time.Millisecond)
	f.writes.Add(1)
	f.active.Add(-1)
	return nil
}

func TestWriteWSSerializesConcurrentWriters(t *testing.T) {
	var mu sync.Mutex
	writer := &fakeWSWriter{}

	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := writeWS(&mu, writer, 1, []byte("x")); err != nil {
				t.Errorf("writeWS() error = %v", err)
			}
		}()
	}
	wg.Wait()

	if got := writer.writes.Load(); got != 32 {
		t.Fatalf("writes = %d, want 32", got)
	}
	if got := writer.maxActive.Load(); got != 1 {
		t.Fatalf("max concurrent writes = %d, want 1", got)
	}
}

func TestReadyHandlerFailsWhileDraining(t *testing.T) {
	shuttingDown.Store(false)
	t.Cleanup(func() { shuttingDown.Store(false) })

	rec := httptest.NewRecorder()
	readyHandler(rec, httptest.NewRequest(http.MethodGet, "/ready", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("ready before drain = %d, want %d", rec.Code, http.StatusOK)
	}

	shuttingDown.Store(true)
	rec = httptest.NewRecorder()
	readyHandler(rec, httptest.NewRequest(http.MethodGet, "/ready", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("ready during drain = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}

func TestDrainRegistryDrainAllInvokesEachOnce(t *testing.T) {
	d := newDrainRegistry()
	var count atomic.Int32
	for i := range 8 {
		d.Add(fmt.Sprintf("c%d", i), func() { count.Add(1) })
	}
	if got := d.Len(); got != 8 {
		t.Fatalf("Len() = %d, want 8", got)
	}
	d.DrainAll()
	if got := count.Load(); got != 8 {
		t.Fatalf("DrainAll invoked %d fns, want 8", got)
	}
}

func TestDrainRegistryRemoveSkipsClosed(t *testing.T) {
	d := newDrainRegistry()
	var invoked atomic.Bool
	d.Add("x", func() { invoked.Store(true) })
	d.Remove("x")
	if got := d.Len(); got != 0 {
		t.Fatalf("Len() after Remove = %d, want 0", got)
	}
	d.DrainAll()
	if invoked.Load() {
		t.Fatal("removed closeFn must not be invoked by DrainAll")
	}
}

func TestEnvDurationParsesAndFallsBack(t *testing.T) {
	if got := envDuration("MCP_TEST_DUR_UNSET_XYZ", 3*time.Second); got != 3*time.Second {
		t.Fatalf("unset = %s, want 3s", got)
	}
	t.Setenv("MCP_TEST_DUR", "250ms")
	if got := envDuration("MCP_TEST_DUR", time.Second); got != 250*time.Millisecond {
		t.Fatalf("set = %s, want 250ms", got)
	}
	t.Setenv("MCP_TEST_DUR", "not-a-duration")
	if got := envDuration("MCP_TEST_DUR", time.Second); got != time.Second {
		t.Fatalf("invalid = %s, want 1s fallback", got)
	}
}
