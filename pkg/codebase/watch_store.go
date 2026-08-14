package codebase

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// watchDescriptor is the durable record of a running watch. It carries
// everything needed to re-launch the watch goroutine after the
// mcp-codebase-memory process restarts (idle-reaper kill, transport-storm
// respawn, crash, or deploy). Root is always stored absolute because the
// respawned process may have a different working directory than the one that
// started the watch.
type watchDescriptor struct {
	WatchID     string    `json:"watch_id"`
	RepoID      string    `json:"repo_id"`
	Root        string    `json:"root"`
	Languages   []string  `json:"languages,omitempty"`
	Exclude     []string  `json:"exclude,omitempty"`
	DebounceMs  int       `json:"debounce_ms"`
	GitMetadata bool      `json:"git_metadata"`
	Embeddings  bool      `json:"embeddings"`
	StartedAt   time.Time `json:"started_at"`
	// LastActiveAt is the last time a client showed interest in this watch
	// (start, reuse, or poll). Watches idle beyond Config.WatchTTL are
	// expired instead of resumed, so an abandoned watch stops burning
	// embedding tokens on every file change forever.
	LastActiveAt time.Time `json:"last_active_at,omitempty"`
}

// watchStore persists watch descriptors as one JSON file per watch under a
// stable directory. A nil store, or one with an empty dir, is a no-op so the
// service still functions (in-memory only) when no durable path is available.
type watchStore struct {
	dir string
	mu  sync.Mutex
}

func newWatchStore(dir string) *watchStore {
	return &watchStore{dir: strings.TrimSpace(dir)}
}

func (w *watchStore) enabled() bool {
	return w != nil && w.dir != ""
}

// fileName maps a watch id to its on-disk path. The id is a hex SHA-256 digest
// (schema.ShortSHA256Hex), so it is a safe filename; the separator guard is
// defense-in-depth against an unexpected id reaching the store.
func (w *watchStore) fileName(watchID string) (string, bool) {
	if watchID == "" || strings.ContainsAny(watchID, `/\`) || watchID == "." || watchID == ".." {
		return "", false
	}
	return filepath.Join(w.dir, watchID+".json"), true
}

// save writes (or overwrites) the descriptor atomically via tempfile+rename so
// a concurrent reader (ResumeWatches in a freshly respawned process) never
// observes a half-written file.
func (w *watchStore) save(d watchDescriptor) error {
	if !w.enabled() {
		return nil
	}
	path, ok := w.fileName(d.WatchID)
	if !ok {
		return fmt.Errorf("invalid watch id %q", d.WatchID)
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	if err := os.MkdirAll(w.dir, 0o755); err != nil {
		return fmt.Errorf("create watch store dir: %w", err)
	}
	payload, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal watch descriptor: %w", err)
	}

	tmp, err := os.CreateTemp(w.dir, d.WatchID+".*.tmp")
	if err != nil {
		return fmt.Errorf("create temp watch file: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(payload); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("write temp watch file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("close temp watch file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("rename watch file: %w", err)
	}
	return nil
}

// remove deletes the descriptor (and any claim file) for watchID. Removing a
// missing file is not an error so stop is idempotent.
func (w *watchStore) remove(watchID string) error {
	if !w.enabled() {
		return nil
	}
	path, ok := w.fileName(watchID)
	if !ok {
		return nil
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	_ = os.Remove(path + claimSuffix)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove watch file: %w", err)
	}
	return nil
}

const claimSuffix = ".claim"

// tryClaim takes the cross-process run-claim for watchID. The state dir is
// shared by every server process on the host (the hub's custom-server spawns
// one child per WebSocket connection, and each child resumes persisted
// watches), so without a claim the same watch runs once per process and every
// file change is re-embedded once per process — pure token waste. The claim is
// an exclusive flock held for the owning process's lifetime; the kernel
// releases it automatically when the process dies, so a crashed owner never
// wedges the watch.
//
// Returns the held claim (release with releaseClaim) and true on success, or
// (nil, false) when another live process already runs this watch. A disabled
// store always claims: with no shared state dir there is exactly one process.
func (w *watchStore) tryClaim(watchID string) (*os.File, bool) {
	if !w.enabled() {
		return nil, true
	}
	path, ok := w.fileName(watchID)
	if !ok {
		return nil, false
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	if err := os.MkdirAll(w.dir, 0o755); err != nil {
		// Cannot arbitrate: run the watch rather than silently dropping it.
		return nil, true
	}
	f, err := os.OpenFile(path+claimSuffix, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, true
	}
	if !flockExclusive(f) {
		_ = f.Close()
		return nil, false
	}
	return f, true
}

// releaseClaim drops a claim taken by tryClaim. Safe on nil.
func releaseClaim(f *os.File) {
	if f != nil {
		_ = f.Close()
	}
}

// load returns every persisted descriptor. Unreadable or malformed files are
// skipped (best-effort) rather than failing the whole resume.
func (w *watchStore) load() ([]watchDescriptor, error) {
	if !w.enabled() {
		return nil, nil
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	entries, err := os.ReadDir(w.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read watch store dir: %w", err)
	}

	descriptors := make([]watchDescriptor, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(w.dir, entry.Name()))
		if err != nil {
			continue
		}
		var d watchDescriptor
		if err := json.Unmarshal(data, &d); err != nil || d.WatchID == "" {
			continue
		}
		descriptors = append(descriptors, d)
	}
	return descriptors, nil
}
