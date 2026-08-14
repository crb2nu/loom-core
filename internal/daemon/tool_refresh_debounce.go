package daemon

import (
	"context"
	"sync"
	"time"

	"gitlab.flexinfer.ai/libs/mcp-go"
)

// toolRefreshDebounce coalesces rapid schedule calls into a single refresh that
// fires after the quiet period elapses. It exists so upstream pod flapping
// (disconnect / reconnect / disconnect / ...) does not thrash the tool cache.
type toolRefreshDebounce struct {
	mu         sync.Mutex
	timer      *time.Timer
	interval   time.Duration
	generation uint64
	stopped    bool
	running    map[*toolRefreshRun]struct{}
	// onFire runs when the debounced timer finally fires. Kept as a field so
	// tests can swap in a counter.
	onFire func(context.Context)
}

type toolRefreshRun struct {
	cancel context.CancelFunc
	done   chan struct{}
}

func newToolRefreshDebounce(interval time.Duration, onFire func(context.Context)) *toolRefreshDebounce {
	return &toolRefreshDebounce{
		interval: interval,
		onFire:   onFire,
		running:  make(map[*toolRefreshRun]struct{}),
	}
}

// schedule resets the debounce window. If no call arrives within interval,
// onFire runs exactly once.
func (t *toolRefreshDebounce) schedule() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stopped {
		return
	}
	t.generation++
	generation := t.generation
	if t.timer != nil {
		t.timer.Stop()
	}
	t.timer = time.AfterFunc(t.interval, func() {
		t.fire(generation)
	})
}

func (t *toolRefreshDebounce) fire(generation uint64) {
	t.mu.Lock()
	if t.stopped || generation != t.generation {
		t.mu.Unlock()
		return
	}
	t.timer = nil
	ctx, cancel := context.WithCancel(context.Background())
	run := &toolRefreshRun{cancel: cancel, done: make(chan struct{})}
	t.running[run] = struct{}{}
	t.mu.Unlock()

	defer func() {
		cancel()
		t.mu.Lock()
		delete(t.running, run)
		close(run.done)
		t.mu.Unlock()
	}()
	t.onFire(ctx)
}

// stop cancels any pending or running refresh and waits for running callbacks
// to return. Used during daemon shutdown.
func (t *toolRefreshDebounce) stop() {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.stopped = true
	t.generation++
	if t.timer != nil {
		t.timer.Stop()
		t.timer = nil
	}
	running := make([]*toolRefreshRun, 0, len(t.running))
	for run := range t.running {
		running = append(running, run)
	}
	t.mu.Unlock()

	for _, run := range running {
		run.cancel()
	}
	for _, run := range running {
		<-run.done
	}
}

// toolRefreshDebounceInterval is the quiet period after the last upstream
// disconnect/reconnect event before the daemon refreshes the tool cache.
const toolRefreshDebounceInterval = 3 * time.Second

// ensureToolRefresh constructs and returns the daemon's debouncer. All reads of
// toolRefresh enter the same sync.Once, so shutdown cannot race lazy creation.
func (d *Daemon) ensureToolRefresh() *toolRefreshDebounce {
	if d == nil {
		return nil
	}
	d.toolRefreshOnce.Do(func() {
		d.toolRefresh = newToolRefreshDebounce(toolRefreshDebounceInterval, func(ctx context.Context) {
			// Guard against firing before the daemon is fully initialized
			// (e.g., unit tests construct a minimal Daemon and call
			// transportFailure directly).
			if d.currentRegistry() == nil {
				return
			}
			// The original request context is long dead by the time the timer
			// fires, but daemon shutdown must still cancel an active refresh.
			refreshCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			if _, err := d.refreshToolCacheDeduplicated(refreshCtx); err != nil && d.logger != nil {
				d.logger.Debug("debounced tool cache refresh failed", "error", err)
			}
		})
	})
	return d.toolRefresh
}

// scheduleToolRefresh debounces tool-cache refreshes triggered by upstream
// reconnects. Once shutdown stops the debouncer, later notifications are
// ignored instead of recreating a timer.
func (d *Daemon) scheduleToolRefresh() {
	if refresh := d.ensureToolRefresh(); refresh != nil {
		refresh.schedule()
	}
}

func (d *Daemon) stopToolRefresh() {
	if refresh := d.ensureToolRefresh(); refresh != nil {
		refresh.stop()
	}
	d.stopCacheRefreshWork()
}

// handleToolsReload is a manual escape hatch: forces a synchronous refresh of
// the daemon's tool cache. Useful after redeploying an upstream MCP server
// without having to restart every connected client.
func (d *Daemon) handleToolsReload(ctx context.Context, msg *mcp.Message) (*mcp.Message, error) {
	refreshCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	tools, err := d.refreshToolCacheDeduplicated(refreshCtx)
	if err != nil {
		return mcp.NewErrorResponse(msg.ID, mcp.InternalError, err.Error()), nil
	}
	return mcp.NewResponse(msg.ID, map[string]any{
		"ok":         true,
		"tool_count": len(tools),
	})
}
