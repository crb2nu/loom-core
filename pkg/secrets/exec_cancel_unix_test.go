//go:build darwin || linux

package secrets

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRealCommandExecutorRunContextKillsDescendantHoldingOutputPipes(t *testing.T) {
	shellPath, err := exec.LookPath("sh")
	if err != nil {
		t.Skipf("shell unavailable: %v", err)
	}

	pidPath := t.TempDir() + "/pids"
	script := `
trap 'kill "$child" 2>/dev/null; wait "$child" 2>/dev/null; exit 0' TERM
sleep 30 &
child=$!
printf '%s %s\n' "$$" "$child" > "$1"
wait "$child"
`
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, _, runErr := (&RealCommandExecutor{}).RunContext(ctx, shellPath, "-c", script, "sh", pidPath)
		done <- runErr
	}()

	var groupPID, childPID int
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		data, readErr := os.ReadFile(pidPath)
		if readErr == nil {
			fields := strings.Fields(string(data))
			if len(fields) == 2 {
				groupPID, _ = strconv.Atoi(fields[0])
				childPID, _ = strconv.Atoi(fields[1])
				if groupPID > 0 && childPID > 0 {
					break
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if groupPID == 0 || childPID == 0 {
		t.Fatal("blocking shell did not publish process IDs")
	}

	canceledAt := time.Now()
	cancel()
	select {
	case runErr := <-done:
		if runErr == nil {
			t.Fatal("canceled process group returned nil error")
		}
		if elapsed := time.Since(canceledAt); elapsed > 2*time.Second {
			t.Fatalf("process-group cancellation took %v", elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("descendant-held output pipes blocked command cancellation")
	}

	assertProcessGone(t, childPID, "descendant")
	assertProcessGroupGone(t, groupPID)
}

func assertProcessGone(t *testing.T, pid int, description string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s process %d still exists after cancellation", description, pid)
}

func assertProcessGroupGone(t *testing.T, groupPID int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		err := syscall.Kill(-groupPID, 0)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("process group %d still exists after cancellation", groupPID)
}
