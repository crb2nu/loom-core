package main

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"gitlab.flexinfer.ai/libs/mcp-go"
)

// buildInfo tracks one in-flight (or just-finished) sandbox image build.
// Builds are keyed by image tag so concurrent callers for the same project
// fingerprint join a single build instead of each spawning their own.
type buildInfo struct {
	tag       string
	startedAt time.Time
	done      bool
	err       error
}

// buildTracker records asynchronous sandbox image builds. It lets
// ensureRunning return an immediate "build in progress" signal instead of
// blocking the caller through a multi-minute image build (the cold-build
// case dominated by `go mod download` / apk installs / image push).
type buildTracker struct {
	mu     sync.Mutex
	builds map[string]*buildInfo
}

func newBuildTracker() *buildTracker {
	return &buildTracker{builds: make(map[string]*buildInfo)}
}

// lookup returns a snapshot of the tracked build for tag, or nil if none.
func (t *buildTracker) lookup(tag string) *buildInfo {
	t.mu.Lock()
	defer t.mu.Unlock()
	if b, ok := t.builds[tag]; ok {
		cp := *b
		return &cp
	}
	return nil
}

// startOrJoin registers an async build for tag and launches run in a detached
// goroutine, unless a build for tag is already in flight. It returns a snapshot
// of the build info and whether this call started it (false means it joined an
// existing in-flight build). wg tracks the goroutine for graceful shutdown.
func (t *buildTracker) startOrJoin(tag string, wg *sync.WaitGroup, run func() error) (*buildInfo, bool) {
	t.mu.Lock()
	if b, ok := t.builds[tag]; ok && !b.done {
		cp := *b
		t.mu.Unlock()
		return &cp, false
	}
	b := &buildInfo{tag: tag, startedAt: time.Now()}
	t.builds[tag] = b
	t.mu.Unlock()

	wg.Add(1)
	go func() {
		defer wg.Done()
		err := run()
		t.mu.Lock()
		b.done = true
		b.err = err
		t.mu.Unlock()
	}()

	cp := *b
	return &cp, true
}

// clear removes a finished build entry so a later fingerprint change rebuilds.
func (t *buildTracker) clear(tag string) {
	t.mu.Lock()
	delete(t.builds, tag)
	t.mu.Unlock()
}

// buildInProgressError signals that a sandbox image build was started (or is
// still running) asynchronously. Callers translate it into a non-error
// "retry shortly" tool result rather than a hard failure.
type buildInProgressError struct {
	tag     string
	project string
	elapsed time.Duration
	started bool
}

func (e *buildInProgressError) Error() string {
	verb := "in progress"
	if e.started {
		verb = "started"
	}
	return fmt.Sprintf("sandbox image build %s for %s (elapsed %s)", verb, e.project, e.elapsed.Round(time.Second))
}

// asBuildInProgress reports whether err is (or wraps) a buildInProgressError.
func asBuildInProgress(err error) (*buildInProgressError, bool) {
	var b *buildInProgressError
	if errors.As(err, &b) {
		return b, true
	}
	return nil, false
}

// buildingResult renders a buildInProgressError as a structured, non-error
// tool result so the agent gets actionable "retry shortly" feedback instead
// of a hung call that eventually times out.
func buildingResult(e *buildInProgressError) (*mcp.CallToolResult, error) {
	return mcp.JSONResult(map[string]any{
		"status":          "building",
		"project":         e.project,
		"elapsed_seconds": int(e.elapsed.Round(time.Second).Seconds()),
		"message": fmt.Sprintf(
			"Sandbox image is building (elapsed %s). Re-run the same call in ~30-60s; "+
				"it will execute once the image is ready. First builds can take several "+
				"minutes (downloading Go/Node dependencies, installing packages).",
			e.elapsed.Round(time.Second)),
	})
}
