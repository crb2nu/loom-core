package daemon

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/crb2nu/loom/internal/daemon/generation"
)

type blockingStopGenerationResource struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (r *blockingStopGenerationResource) Close() error {
	r.once.Do(func() { close(r.entered) })
	<-r.release
	return nil
}

func TestStopAllServerProcsPropagatesBoundedSupervisorTimeout(t *testing.T) {
	resource := &blockingStopGenerationResource{entered: make(chan struct{}), release: make(chan struct{})}
	core := generation.New(func(context.Context, string, uint64) (generation.Resource, error) {
		return resource, nil
	})
	if _, _, err := core.Ensure(context.Background(), "blocked-stop"); err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	d := &Daemon{
		logger:                slog.New(slog.NewTextHandler(io.Discard, nil)),
		serverSupervisor:      &serverSupervisor{core: core},
		serverShutdownTimeout: 20 * time.Millisecond,
	}

	started := time.Now()
	d.stopAllServerProcs()
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("stopAllServerProcs exceeded bound: %s", elapsed)
	}
	if !errors.Is(d.stopErr, context.DeadlineExceeded) {
		t.Fatalf("stopErr = %v, want context.DeadlineExceeded", d.stopErr)
	}
	awaitSignal(t, resource.entered, "blocked generation close")

	close(resource.release)
	if err := core.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown after releasing close: %v", err)
	}
}
