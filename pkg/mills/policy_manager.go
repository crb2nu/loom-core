package mills

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"
)

// PolicyManager owns the active Policy and supports atomic hot-reload from a
// YAML file. Readers call Current() with no lock; the value is replaced
// atomically so in-flight runs always see a coherent snapshot. A bad reload
// (parse or validate error) keeps the previous policy active and surfaces the
// error via the OnError callback.
type PolicyManager struct {
	path     string
	current  atomic.Pointer[policySnapshot]
	watcher  *fsnotify.Watcher
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	onChange []func(old, new *Policy)
	onError  func(error)
	mu       sync.Mutex // guards onChange registration only

	// lastRaw is the raw bytes of the policy file as of the last time the
	// watch loop acted on it. Only the watch goroutine touches it (after the
	// initial seed), so it needs no lock. It lets the watcher skip reloads
	// when an event fires but the resolved content is unchanged.
	lastRaw []byte
}

// policySnapshot pairs the active policy with the checksum of the exact bytes
// it was parsed from. The two are swapped as one pointer so a provenance
// reader can never observe a new policy under a stale checksum (or the
// reverse) across a hot reload.
type policySnapshot struct {
	policy   *Policy
	checksum string
}

// PolicyManagerOptions tunes manager construction. All fields are optional.
type PolicyManagerOptions struct {
	// OnError is invoked with reload errors. If nil, errors are dropped.
	OnError func(error)
	// SkipWatch disables fsnotify entirely; Reload() must be called manually.
	// Useful for tests and for ConfigMap mounts that re-inject the file by
	// path replacement (which fsnotify can miss without watch_root tricks).
	SkipWatch bool
}

// NewPolicyManager loads the policy at path, validates it, and (unless
// SkipWatch is set) installs an fsnotify watch on the file's parent directory
// so it survives ConfigMap remounts.
func NewPolicyManager(ctx context.Context, path string, opts PolicyManagerOptions) (*PolicyManager, error) {
	if path == "" {
		return nil, errors.New("mills: policy path required")
	}
	p, checksum, err := LoadPolicyWithChecksum(path)
	if err != nil {
		return nil, err
	}
	m := &PolicyManager{path: path, onError: opts.OnError}
	m.current.Store(&policySnapshot{policy: p, checksum: checksum})

	if opts.SkipWatch {
		return m, nil
	}

	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("mills: fsnotify: %w", err)
	}
	// Watch the parent directory so atomic-rename and ConfigMap-style mounts
	// (which replace the symlink chain) trigger reloads.
	if err := w.Add(filepath.Dir(path)); err != nil {
		_ = w.Close()
		return nil, fmt.Errorf("mills: watch %s: %w", path, err)
	}
	m.watcher = w

	// Seed lastRaw with the current file contents so the first unrelated
	// event does not trigger a redundant reload. Best-effort: a read failure
	// just means the first event reloads, which is harmless.
	m.lastRaw, _ = os.ReadFile(path)

	watchCtx, cancel := context.WithCancel(ctx)
	m.cancel = cancel
	m.wg.Add(1)
	go m.watchLoop(watchCtx)
	return m, nil
}

// Current returns the active policy. Lock-free; safe to call from hot paths.
func (m *PolicyManager) Current() *Policy {
	if m == nil {
		return nil
	}
	snap := m.current.Load()
	if snap == nil {
		return nil
	}
	return snap.policy
}

// CurrentChecksum returns the checksum of the bytes the active policy was
// parsed from (PolicyChecksum format), or the empty string when no policy is
// loaded. Run provenance stamps this so a merged run can be joined back to the
// exact policy revision that produced it.
func (m *PolicyManager) CurrentChecksum() string {
	if m == nil {
		return ""
	}
	snap := m.current.Load()
	if snap == nil {
		return ""
	}
	return snap.checksum
}

// Subscribe registers a callback fired after every successful reload. The
// callback runs synchronously on the watcher goroutine, so it must not block
// for long.
func (m *PolicyManager) Subscribe(f func(old, new *Policy)) {
	if f == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onChange = append(m.onChange, f)
}

// Reload re-reads the policy file and atomically swaps the active value.
// Safe to call from tests or as a manual fallback when SkipWatch is true.
func (m *PolicyManager) Reload() error {
	p, checksum, err := LoadPolicyWithChecksum(m.path)
	if err != nil {
		return err
	}
	old := m.current.Swap(&policySnapshot{policy: p, checksum: checksum})
	var oldPolicy *Policy
	if old != nil {
		oldPolicy = old.policy
	}
	m.fireOnChange(oldPolicy, p)
	return nil
}

func (m *PolicyManager) fireOnChange(old, new *Policy) {
	m.mu.Lock()
	subs := append([]func(*Policy, *Policy){}, m.onChange...)
	m.mu.Unlock()
	for _, f := range subs {
		f(old, new)
	}
}

// Close stops the watcher goroutine and releases fsnotify resources.
func (m *PolicyManager) Close() error {
	if m == nil {
		return nil
	}
	if m.cancel != nil {
		m.cancel()
	}
	m.wg.Wait()
	if m.watcher != nil {
		return m.watcher.Close()
	}
	return nil
}

// debounceWindow is how long the watch loop waits after the last filesystem
// event before re-reading the policy. It absorbs the burst of events a single
// logical change produces and, critically, gives a Kubernetes ConfigMap swap
// time to complete before we re-resolve the symlink chain (see watchLoop).
const debounceWindow = 200 * time.Millisecond

func (m *PolicyManager) watchLoop(ctx context.Context) {
	defer m.wg.Done()
	// fsnotify cannot reliably name the decisive event for a K8s ConfigMap
	// update, and the two backends disagree on what fires at all. Kubelet
	// stages new content in a timestamped dir and atomically renames the
	// "..data" symlink onto its own existing name:
	//
	//   - Linux/inotify surfaces this as a Create/Rename on "..data".
	//   - macOS/kqueue emits NOTHING for the rename (renaming onto an existing
	//     name adds/removes no directory entry, so the dir-diff sees no
	//     change); only the preceding "..data_tmp"/timestamped-dir creates
	//     fire, and those arrive *before* the swap is visible.
	//
	// So a strict "match this event name + op" filter is inherently fragile.
	// Instead treat any event in the watched directory as a hint, debounce the
	// burst, then re-read the target through the symlink chain and reload only
	// when the bytes actually changed. This is robust across inotify and
	// kqueue and covers both ConfigMap swaps and plain in-place edits. See
	// loom-mills-operator rollout 2026-05-04 (squads flip needed a manual
	// rolling restart because the strict match dropped the ConfigMap event).
	var debounce <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-m.watcher.Events:
			if !ok {
				return
			}
			debounce = time.After(debounceWindow)
		case <-debounce:
			debounce = nil
			m.reloadIfChanged()
		case err, ok := <-m.watcher.Errors:
			if !ok {
				return
			}
			if m.onError != nil {
				m.onError(err)
			}
		}
	}
}

// reloadIfChanged re-reads the policy file (following the symlink chain) and
// reloads only when its bytes differ from what the watcher last acted on. This
// keeps unrelated directory events from firing spurious Subscribe callbacks.
// Runs solely on the watch goroutine, so lastRaw needs no lock.
func (m *PolicyManager) reloadIfChanged() {
	raw, err := os.ReadFile(m.path)
	if err != nil {
		if m.onError != nil {
			m.onError(err)
		}
		return
	}
	if bytes.Equal(raw, m.lastRaw) {
		return
	}
	// Record the bytes we are acting on before reloading so a bad payload is
	// not retried on every subsequent unrelated event; a corrected write has
	// different bytes and will reload. A failed Reload keeps the prior policy.
	m.lastRaw = raw
	if err := m.Reload(); err != nil && m.onError != nil {
		m.onError(err)
	}
}
