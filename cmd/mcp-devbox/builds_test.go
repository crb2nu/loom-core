package main

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// TestBuildTracker_StartOrJoinDedupes verifies that a second caller for the
// same tag joins the in-flight build instead of starting a duplicate.
func TestBuildTracker_StartOrJoinDedupes(t *testing.T) {
	tr := newBuildTracker()
	var wg sync.WaitGroup

	release := make(chan struct{})
	var runs int
	var mu sync.Mutex
	run := func() error {
		mu.Lock()
		runs++
		mu.Unlock()
		<-release // block so the build stays "in flight"
		return nil
	}

	bi1, started1 := tr.startOrJoin("img:abc", &wg, run)
	if !started1 {
		t.Fatal("first startOrJoin should report started=true")
	}
	if bi1.done {
		t.Fatal("first build should not be done yet")
	}

	bi2, started2 := tr.startOrJoin("img:abc", &wg, run)
	if started2 {
		t.Fatal("second startOrJoin for same tag should join (started=false)")
	}
	if bi2.done {
		t.Fatal("joined build should still be in flight")
	}

	close(release)
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if runs != 1 {
		t.Fatalf("run executed %d times, want exactly 1 (dedup)", runs)
	}
}

// TestBuildTracker_DoneTransition verifies a build's terminal state and error
// are observable via lookup after completion.
func TestBuildTracker_DoneTransition(t *testing.T) {
	tr := newBuildTracker()
	var wg sync.WaitGroup
	wantErr := errors.New("boom")

	tr.startOrJoin("img:fail", &wg, func() error { return wantErr })
	wg.Wait()

	bi := tr.lookup("img:fail")
	if bi == nil {
		t.Fatal("lookup returned nil after build finished")
	}
	if !bi.done {
		t.Fatal("build should be marked done")
	}
	if !errors.Is(bi.err, wantErr) {
		t.Fatalf("build err = %v, want %v", bi.err, wantErr)
	}

	tr.clear("img:fail")
	if tr.lookup("img:fail") != nil {
		t.Fatal("clear should remove the entry")
	}
}

// TestBuildTracker_RestartsAfterDone verifies that once a build is done, a new
// startOrJoin for the same tag starts a fresh build rather than joining the
// stale finished one.
func TestBuildTracker_RestartsAfterDone(t *testing.T) {
	tr := newBuildTracker()
	var wg sync.WaitGroup

	tr.startOrJoin("img:x", &wg, func() error { return nil })
	wg.Wait()

	_, started := tr.startOrJoin("img:x", &wg, func() error { return nil })
	if !started {
		t.Fatal("startOrJoin after a finished build should start a new one")
	}
	wg.Wait()
}

// TestAsBuildInProgress verifies the sentinel is detectable when wrapped.
func TestAsBuildInProgress(t *testing.T) {
	base := &buildInProgressError{tag: "t", project: "p", elapsed: 2 * time.Second, started: true}
	wrapped := fmt.Errorf("ensure sandbox: %w", base)

	got, ok := asBuildInProgress(wrapped)
	if !ok {
		t.Fatal("asBuildInProgress should detect a wrapped buildInProgressError")
	}
	if got.project != "p" {
		t.Fatalf("project = %q, want p", got.project)
	}

	if _, ok := asBuildInProgress(errors.New("unrelated")); ok {
		t.Fatal("asBuildInProgress should not match an unrelated error")
	}
}

// TestBuildingResult renders a structured, non-error building payload.
func TestBuildingResult(t *testing.T) {
	res, err := buildingResult(&buildInProgressError{project: "loom-core", elapsed: 5 * time.Second})
	if err != nil {
		t.Fatalf("buildingResult returned err: %v", err)
	}
	if res == nil || res.IsError {
		t.Fatal("building result must be a non-error tool result")
	}
}
