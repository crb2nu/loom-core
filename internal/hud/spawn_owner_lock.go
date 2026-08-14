package hud

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
)

var errSpawnControllerOwnerLockHeld = errors.New("spawn controller owner lock already held")

// spawnControllerOwnerLock is an OS-enforced, process-lifetime claim for one
// generated local spawn controller identity. Keeping the descriptor open is
// what keeps flock held; the kernel releases the claim if the HUD crashes.
type spawnControllerOwnerLock struct {
	mu           sync.Mutex
	file         *os.File
	path         string
	controllerID string
}

func acquireSpawnControllerOwnerLock(controllerID string) (*spawnControllerOwnerLock, error) {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return nil, fmt.Errorf("resolve user config directory for spawn controller lock: %w", err)
	}
	return acquireSpawnControllerOwnerLockAt(
		filepath.Join(configDir, "loom", "locks"),
		controllerID,
	)
}

func acquireSpawnControllerOwnerLockAt(lockDir, controllerID string) (*spawnControllerOwnerLock, error) {
	controllerID = strings.TrimSpace(controllerID)
	if controllerID == "" {
		return nil, errors.New("spawn controller owner lock requires a controller ID")
	}
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		return nil, fmt.Errorf("create spawn controller lock directory %s: %w", lockDir, err)
	}

	sum := sha256.Sum256([]byte(controllerID))
	lockPath := filepath.Join(lockDir, fmt.Sprintf("spawn-controller-%x.lock", sum[:]))
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open spawn controller lock %s: %w", lockPath, err)
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("secure spawn controller lock %s: %w", lockPath, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			owner := "owner metadata unavailable"
			if data, readErr := os.ReadFile(lockPath); readErr == nil {
				if metadata := strings.TrimSpace(string(data)); metadata != "" {
					owner = strings.ReplaceAll(metadata, "\n", ", ")
				}
			}
			return nil, fmt.Errorf(
				"%w: generated controller %q is already active on this host (%s; %s); stop the duplicate HUD or use distinct socket/HTTP endpoints",
				errSpawnControllerOwnerLockHeld, controllerID, lockPath, owner,
			)
		}
		return nil, fmt.Errorf("acquire spawn controller lock %s: %w", lockPath, err)
	}

	// Do not let spawned agent or build processes inherit the lifetime claim.
	syscall.CloseOnExec(int(f.Fd()))
	metadata := fmt.Sprintf("pid=%d\ncontroller_id=%s\n", os.Getpid(), controllerID)
	if err := f.Truncate(0); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("truncate spawn controller lock %s: %w", lockPath, err)
	}
	if _, err := f.WriteAt([]byte(metadata), 0); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("write spawn controller lock %s: %w", lockPath, err)
	}

	return &spawnControllerOwnerLock{
		file:         f,
		path:         lockPath,
		controllerID: controllerID,
	}, nil
}

// Close releases the claim. The lock file intentionally remains in place:
// unlinking it could let a third process lock a new inode while another owner
// still holds the old one. The next successful owner refreshes its metadata.
func (l *spawnControllerOwnerLock) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	err := l.file.Close()
	l.file = nil
	return err
}

func (a *App) releaseSpawnControllerOwnerLock() {
	if a == nil || a.spawnControllerOwnerLock == nil {
		return
	}
	if err := a.spawnControllerOwnerLock.Close(); err != nil && a.logger != nil {
		a.logger.Warn("release local spawn controller owner lock", "error", err)
	}
}
