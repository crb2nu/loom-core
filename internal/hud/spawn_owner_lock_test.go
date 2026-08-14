package hud

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSpawnControllerOwnerLockIsNonBlockingAndReleases(t *testing.T) {
	lockDir := t.TempDir()
	controllerID := "local/loomd/same-endpoint"
	first, err := acquireSpawnControllerOwnerLockAt(lockDir, controllerID)
	if err != nil {
		t.Fatalf("acquire first owner lock: %v", err)
	}
	defer first.Close()

	// Process startup can take several seconds under the race detector. The
	// helper measures the flock call itself; this outer deadline only catches a
	// genuinely wedged child process.
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSpawnControllerOwnerLockHelperProcess$")
	cmd.Env = append(os.Environ(),
		"LOOM_TEST_SPAWN_OWNER_LOCK_HELPER=1",
		"LOOM_TEST_SPAWN_OWNER_LOCK_DIR="+lockDir,
		"LOOM_TEST_SPAWN_OWNER_LOCK_ID="+controllerID,
		fmt.Sprintf("LOOM_TEST_SPAWN_OWNER_LOCK_PID=%d", os.Getpid()),
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			t.Fatal("second process owner claim blocked instead of failing immediately")
		}
		t.Fatalf("second process owner claim: %v\n%s", err, output)
	}

	if err := first.Close(); err != nil {
		t.Fatalf("release first owner lock: %v", err)
	}
	next, err := acquireSpawnControllerOwnerLockAt(lockDir, controllerID)
	if err != nil {
		t.Fatalf("reacquire owner lock after release: %v", err)
	}
	defer next.Close()
}

func TestSpawnControllerOwnerLockHelperProcess(t *testing.T) {
	if os.Getenv("LOOM_TEST_SPAWN_OWNER_LOCK_HELPER") != "1" {
		return
	}
	started := time.Now()
	lock, err := acquireSpawnControllerOwnerLockAt(
		os.Getenv("LOOM_TEST_SPAWN_OWNER_LOCK_DIR"),
		os.Getenv("LOOM_TEST_SPAWN_OWNER_LOCK_ID"),
	)
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("nonblocking owner claim took %s", elapsed)
	}
	if lock != nil {
		_ = lock.Close()
		t.Fatal("helper process unexpectedly acquired the parent's owner lock")
	}
	if !errors.Is(err, errSpawnControllerOwnerLockHeld) {
		t.Fatalf("helper process error = %v, want lock-held sentinel", err)
	}
	if !strings.Contains(err.Error(), "pid="+os.Getenv("LOOM_TEST_SPAWN_OWNER_LOCK_PID")) {
		t.Fatalf("contention error omitted parent owner metadata: %v", err)
	}
}

func TestInitSpawnOrchestratorReleasesGeneratedOwnerLockOnFailure(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SPAWN_CONTROLLER_ID", "")

	app := &App{
		config: Config{
			SocketPath:           filepath.Join(home, "loom.sock"),
			BindAddress:          "127.0.0.1",
			Port:                 3333,
			SpawnBuildCPURequest: "not-a-kubernetes-quantity",
		},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if err := app.initSpawnOrchestrator(t.Context()); err == nil {
		t.Fatal("initSpawnOrchestrator unexpectedly accepted invalid build CPU request")
	}
	if app.spawnControllerOwnerLock == nil {
		t.Fatal("generated controller startup failed before exercising owner lock")
	}

	reacquired, err := acquireSpawnControllerOwnerLockAt(
		filepath.Dir(app.spawnControllerOwnerLock.path),
		app.spawnControllerOwnerLock.controllerID,
	)
	if err != nil {
		t.Fatalf("startup failure did not release generated owner lock: %v", err)
	}
	defer reacquired.Close()
}

func TestExplicitSpawnControllerIDBypassesLocalOwnerLock(t *testing.T) {
	tests := []struct {
		name     string
		configID string
		envID    string
		wantID   string
	}{
		{name: "config", configID: "loom-hub/mobile-hud", wantID: "loom-hub/mobile-hud"},
		{name: "environment", envID: "loom-hub/mobile-hud", wantID: "loom-hub/mobile-hud"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("SPAWN_CONTROLLER_ID", tt.envID)
			cfg := Config{
				SocketPath:           "/tmp/loom.sock",
				BindAddress:          "127.0.0.1",
				Port:                 3333,
				SpawnControllerID:    tt.configID,
				SpawnBuildCPURequest: "not-a-kubernetes-quantity",
			}
			controllerID, generated := resolveSpawnControllerIdentity(cfg)
			if generated || controllerID != tt.wantID {
				t.Fatalf("resolved identity = %q, generated=%v; want explicit %q", controllerID, generated, tt.wantID)
			}

			app := &App{config: cfg, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
			if err := app.initSpawnOrchestrator(t.Context()); err == nil {
				t.Fatal("initSpawnOrchestrator unexpectedly accepted invalid build CPU request")
			}
			if app.spawnControllerOwnerLock != nil {
				t.Fatal("explicit cluster controller ID acquired a host-local owner lock")
			}
		})
	}
}

func TestStopMonitorsReleasesSpawnControllerOwnerLock(t *testing.T) {
	lockDir := t.TempDir()
	controllerID := "local/loom/stop-release"
	lock, err := acquireSpawnControllerOwnerLockAt(lockDir, controllerID)
	if err != nil {
		t.Fatal(err)
	}
	app := &App{
		logger:                   slog.New(slog.NewTextHandler(io.Discard, nil)),
		spawnControllerOwnerLock: lock,
	}
	app.StopMonitors()
	app.StopMonitors() // Idempotent cleanup must not re-close or panic.

	next, err := acquireSpawnControllerOwnerLockAt(lockDir, controllerID)
	if err != nil {
		t.Fatalf("StopMonitors did not release owner lock: %v", err)
	}
	defer next.Close()
}
