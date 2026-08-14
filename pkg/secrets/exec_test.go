package secrets

import (
	"context"
	"os/exec"
	"testing"
	"time"
)

func TestRealCommandExecutorRunContextKillsBlockingCommand(t *testing.T) {
	sleepPath, err := exec.LookPath("sleep")
	if err != nil {
		t.Skipf("sleep command unavailable: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, _, runErr := (&RealCommandExecutor{}).RunContext(ctx, sleepPath, "30")
		done <- runErr
	}()

	// The command must still be blocked before cancellation.
	select {
	case runErr := <-done:
		t.Fatalf("blocking command returned before cancellation: %v", runErr)
	case <-time.After(100 * time.Millisecond):
	}

	cancel()
	select {
	case runErr := <-done:
		if runErr == nil {
			t.Fatal("canceled command returned nil error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("canceled command was not killed promptly")
	}
}
