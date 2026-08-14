package crossrepo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Loader watches a single `repos.yaml` file, validates it, and reflects
// each successful parse into an in-memory snapshot atomically. It mirrors
// the fsnotify hot-reload pattern in `pkg/mills/policy_manager.go`: a single
// background goroutine watches the parent directory, treats every event as a
// hint, debounces event bursts with a maximum latency, and re-reads the target
// after the burst or deadline. This survives atomic replacements whose
// decisive target event is unnamed or absent on some fsnotify backends.
//
// Bad YAML during reload is non-destructive: the previous good snapshot
// survives, OnError fires, and the watcher keeps running. The first parse
// (in NewLoader) is fatal — a misconfigured registry should fail loud at
// startup rather than silently routing every cross-repo run to fallback.
type Loader struct {
	path    string // absolute, resolved
	dir     string // parent dir of path; what fsnotify actually watches
	opts    LoaderOptions
	log     *slog.Logger
	current atomic.Pointer[Registry]
	watcher *fsnotify.Watcher
	cancel  context.CancelFunc
	wg      sync.WaitGroup

	mu       sync.Mutex // guards onChange registration
	onChange []func(*Registry)

	// lastRaw is the content the watch loop last acted on. The initial load
	// seeds it before the watcher starts; afterwards only the watch goroutine
	// mutates it. Invalid changed bytes are retained here so unrelated parent
	// events do not create an error storm, while a later corrected write still
	// differs and can recover.
	lastRaw []byte
	// lastReadErr deduplicates a persistent read failure until the target is
	// readable again. Like lastRaw, only the watch goroutine mutates it.
	lastReadErr string

	// watchDebounce, watchMaxDebounce, and watchAttempted are internal test
	// seams. Production uses the registry debounce constants and leaves
	// watchAttempted nil.
	watchDebounce    time.Duration
	watchMaxDebounce time.Duration
	watchAttempted   func()
}

// LoaderOptions tunes loader construction. All fields are optional.
type LoaderOptions struct {
	// SkipWatch disables fsnotify; callers must invoke Reload manually.
	// Useful for tests that drive reload synchronously.
	SkipWatch bool

	// OnError is called with reload errors (parse/validate). nil drops
	// errors silently. The constructor's first-parse error is *not*
	// routed through OnError — it returns synchronously instead.
	OnError func(error)
}

// NewLoader opens the registry file, performs an initial parse + validate,
// and (unless SkipWatch is set) starts a background fsnotify loop on the
// parent directory. The first parse must succeed: a missing file or
// invalid YAML at startup returns an error and starts no watcher.
//
// Subsequent reload errors are non-fatal: the prior good snapshot is
// retained, OnError fires, and the watcher keeps running.
func NewLoader(ctx context.Context, path string, log *slog.Logger, opts LoaderOptions) (*Loader, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("crossrepo: path required")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("crossrepo: abs %q: %w", path, err)
	}
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	l := &Loader{
		path: abs,
		dir:  filepath.Dir(abs),
		opts: opts,
		log:  log,
	}
	// Initial parse must succeed.
	raw, reg, err := l.readRegistry()
	if err != nil {
		return nil, err
	}
	l.current.Store(reg)
	l.lastRaw = append([]byte(nil), raw...)
	l.log.Info("crossrepo registry loaded", "path", abs, "repos", len(reg.Spec.Repos))

	if opts.SkipWatch {
		return l, nil
	}
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("crossrepo: fsnotify: %w", err)
	}
	if err := w.Add(l.dir); err != nil {
		_ = w.Close()
		return nil, fmt.Errorf("crossrepo: watch %s: %w", l.dir, err)
	}
	l.watcher = w
	watchCtx, cancel := context.WithCancel(ctx)
	l.cancel = cancel
	l.wg.Add(1)
	go l.watchLoop(watchCtx)
	return l, nil
}

// Snapshot returns a defensive copy of the current registry's repo list.
// Lock-free; safe to call from hot paths.
func (l *Loader) Snapshot() []RepoEntry {
	if l == nil {
		return nil
	}
	reg := l.current.Load()
	return reg.Repos()
}

// Current returns a pointer to the most recently loaded Registry. The
// returned pointer is safe to read (Repos slice is shared, treat as
// immutable). For mutation callers should prefer Snapshot.
func (l *Loader) Current() *Registry {
	if l == nil {
		return nil
	}
	return l.current.Load()
}

// Find looks up a repo by name in the current snapshot.
func (l *Loader) Find(name string) (RepoEntry, bool) {
	if l == nil {
		return RepoEntry{}, false
	}
	reg := l.current.Load()
	return reg.Find(name)
}

// Subscribe registers a callback fired after every successful reload (not
// on initial load). The callback runs on the watcher goroutine; it must
// not block long.
func (l *Loader) Subscribe(f func(*Registry)) {
	if f == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.onChange = append(l.onChange, f)
}

// Reload re-reads + validates the registry. On success it atomically
// replaces the current snapshot and fires subscribers. On parse/validate
// error it returns the error and leaves the prior snapshot intact —
// callers (and the watcher loop) treat that as the "last good config"
// behavior.
func (l *Loader) Reload() error {
	reg, err := l.parse()
	if err != nil {
		return err
	}
	l.apply(reg)
	return nil
}

func (l *Loader) apply(reg *Registry) {
	l.current.Store(reg)
	l.fireOnChange(reg)
	l.log.Info("crossrepo registry reloaded", "path", l.path, "repos", len(reg.Spec.Repos))
}

// Close stops the watcher and releases fsnotify resources.
func (l *Loader) Close() error {
	if l == nil {
		return nil
	}
	if l.cancel != nil {
		l.cancel()
	}
	l.wg.Wait()
	if l.watcher != nil {
		return l.watcher.Close()
	}
	return nil
}

// parse reads the registry file from disk and validates it. On success
// the registry has its defaults applied. Errors are returned unwrapped
// from this layer so callers can chain with %w.
func (l *Loader) parse() (*Registry, error) {
	_, reg, err := l.readRegistry()
	return reg, err
}

func (l *Loader) readRegistry() ([]byte, *Registry, error) {
	data, err := os.ReadFile(l.path)
	if err != nil {
		return nil, nil, fmt.Errorf("crossrepo: read %s: %w", l.path, err)
	}
	reg, err := Parse(data)
	if err != nil {
		return nil, nil, err
	}
	return data, reg, nil
}

func (l *Loader) fireOnChange(reg *Registry) {
	l.mu.Lock()
	subs := append([]func(*Registry){}, l.onChange...)
	l.mu.Unlock()
	for _, f := range subs {
		f(reg)
	}
}

const (
	registryDebounceWindow    = 200 * time.Millisecond
	registryMaxDebounceWindow = time.Second
)

func (l *Loader) debounceDuration() time.Duration {
	if l.watchDebounce > 0 {
		return l.watchDebounce
	}
	return registryDebounceWindow
}

func (l *Loader) maxDebounceDuration() time.Duration {
	if l.watchMaxDebounce > 0 {
		return l.watchMaxDebounce
	}
	return registryMaxDebounceWindow
}

func (l *Loader) watchLoop(ctx context.Context) {
	defer l.wg.Done()
	l.watchEvents(ctx, l.watcher.Events, l.watcher.Errors)
}

func (l *Loader) watchEvents(ctx context.Context, events <-chan fsnotify.Event, errs <-chan error) {
	var (
		debounce      *time.Timer
		debounceC     <-chan time.Time
		burstDeadline time.Time
	)
	stopDebounce := func() {
		if debounce != nil && !debounce.Stop() {
			select {
			case <-debounce.C:
			default:
			}
		}
		debounceC = nil
	}
	defer stopDebounce()
	resetDebounce := func(delay time.Duration) {
		if debounce == nil {
			debounce = time.NewTimer(delay)
		} else {
			stopDebounce()
			debounce.Reset(delay)
		}
		debounceC = debounce.C
	}

	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-events:
			if !ok {
				return
			}
			now := time.Now()
			if burstDeadline.IsZero() {
				burstDeadline = now.Add(l.maxDebounceDuration())
			}
			remaining := burstDeadline.Sub(now)
			if remaining <= 0 {
				stopDebounce()
				burstDeadline = time.Time{}
				l.reloadIfChanged()
				continue
			}
			delay := l.debounceDuration()
			if remaining < delay {
				delay = remaining
			}
			resetDebounce(delay)
		case <-debounceC:
			debounceC = nil
			burstDeadline = time.Time{}
			l.reloadIfChanged()
		case err, ok := <-errs:
			if !ok {
				return
			}
			if l.opts.OnError != nil {
				l.opts.OnError(err)
			}
			l.log.Warn("crossrepo watcher error", "err", err.Error())
		}
	}
}

// reloadIfChanged resolves the registry target after a debounced parent event
// and reloads only when its bytes changed. It runs on the watch goroutine, so
// lastRaw needs no lock.
func (l *Loader) reloadIfChanged() {
	if l.watchAttempted != nil {
		defer l.watchAttempted()
	}
	raw, err := os.ReadFile(l.path)
	if err != nil {
		reloadErr := fmt.Errorf("crossrepo: read %s: %w", l.path, err)
		if reloadErr.Error() == l.lastReadErr {
			return
		}
		l.lastReadErr = reloadErr.Error()
		l.reportReloadError(reloadErr)
		return
	}
	l.lastReadErr = ""
	if bytes.Equal(raw, l.lastRaw) {
		return
	}
	// Mark changed bytes before parsing. A bad payload retains the last-good
	// registry but is not reparsed on every unrelated directory event; a fixed
	// payload has different bytes and remains recoverable.
	l.lastRaw = append(l.lastRaw[:0], raw...)
	reg, err := Parse(raw)
	if err != nil {
		l.reportReloadError(err)
		return
	}
	l.apply(reg)
}

func (l *Loader) reportReloadError(err error) {
	if l.opts.OnError != nil {
		l.opts.OnError(err)
	}
	l.log.Warn("crossrepo reload error (last-good retained)",
		"path", l.path, "err", err.Error())
}
