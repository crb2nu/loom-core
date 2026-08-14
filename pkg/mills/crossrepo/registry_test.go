package crossrepo

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
)

const validThreeRepos = `
apiVersion: mills.loom.dev/v1
kind: RepoRegistry
metadata:
  name: workspace
spec:
  repos:
    - name: loom-core
      url: git@gitlab.flexinfer.ai:services/loom-core.git
      project_id: 47
      default_branch: main
      auto_merge: true
      protected_paths:
        - "platform/gitops/**"
    - name: loom
      url: git@gitlab.flexinfer.ai:services/loom.git
      project_id: 51
      default_branch: main
      auto_merge: true
    - name: flexdeck
      url: git@gitlab.flexinfer.ai:services/flexdeck.git
      project_id: 53
      auto_merge: false
`

const validTwoRepos = `
apiVersion: mills.loom.dev/v1
kind: RepoRegistry
metadata:
  name: workspace
spec:
  repos:
    - name: loom-core
      url: git@gitlab.flexinfer.ai:services/loom-core.git
      project_id: 47
      default_branch: main
      auto_merge: true
    - name: loom
      url: git@gitlab.flexinfer.ai:services/loom.git
      project_id: 51
      default_branch: main
      auto_merge: true
`

const validUpdatedThreeRepos = `
apiVersion: mills.loom.dev/v1
kind: RepoRegistry
metadata:
  name: workspace
spec:
  repos:
    - name: loom-core
      url: git@gitlab.flexinfer.ai:services/loom-core.git
      project_id: 47
      default_branch: main
      auto_merge: true
    - name: loom
      url: git@gitlab.flexinfer.ai:services/loom.git
      project_id: 51
      default_branch: main
      auto_merge: true
    - name: flexinfer
      url: git@gitlab.flexinfer.ai:services/flexinfer.git
      project_id: 71
      default_branch: main
      auto_merge: false
`

const invalidBadAPIVersion = `
apiVersion: mills.loom.dev/v0
kind: RepoRegistry
metadata: { name: workspace }
spec:
  repos:
    - name: loom-core
      url: git@gitlab.flexinfer.ai:services/loom-core.git
`

const invalidEmptyName = `
apiVersion: mills.loom.dev/v1
kind: RepoRegistry
metadata: { name: workspace }
spec:
  repos:
    - name: ""
      url: git@gitlab.flexinfer.ai:services/loom-core.git
`

const invalidEmptyURL = `
apiVersion: mills.loom.dev/v1
kind: RepoRegistry
metadata: { name: workspace }
spec:
  repos:
    - name: loom-core
      url: ""
`

const invalidDuplicateNames = `
apiVersion: mills.loom.dev/v1
kind: RepoRegistry
metadata: { name: workspace }
spec:
  repos:
    - name: loom-core
      url: git@gitlab.flexinfer.ai:services/loom-core.git
    - name: loom-core
      url: git@gitlab.flexinfer.ai:services/loom-core-fork.git
`

const invalidBadGlob = `
apiVersion: mills.loom.dev/v1
kind: RepoRegistry
metadata: { name: workspace }
spec:
  repos:
    - name: loom-core
      url: git@gitlab.flexinfer.ai:services/loom-core.git
      protected_paths: ["[unterminated"]
`

const malformedYAML = `not: yaml: at all: [`

func writeRegistry(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// writeRegistryAtomic does a tempfile + rename so fsnotify sees a
// Create/Rename instead of a partial Write — closer to how kubectl
// ConfigMap mounts and most editors save.
func writeRegistryAtomic(t *testing.T, path, body string) {
	t.Helper()
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".repos-*.yaml.tmp")
	if err != nil {
		t.Fatalf("tempfile: %v", err)
	}
	if _, err := tmp.WriteString(body); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		t.Fatalf("write tmp: %v", err)
	}
	if err := tmp.Close(); err != nil {
		t.Fatalf("close tmp: %v", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		t.Fatalf("rename: %v", err)
	}
}

func startTestWatchEvents(t *testing.T, l *Loader) (chan<- fsnotify.Event, <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan fsnotify.Event, 64)
	errs := make(chan error, 1)
	attempted := make(chan struct{}, 128)
	done := make(chan struct{})
	l.watchDebounce = 10 * time.Millisecond
	l.watchMaxDebounce = 50 * time.Millisecond
	l.watchAttempted = func() { attempted <- struct{}{} }
	go func() {
		defer close(done)
		l.watchEvents(ctx, events, errs)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("test watch loop did not stop")
		}
	})
	return events, attempted
}

func sendWatchHint(t *testing.T, events chan<- fsnotify.Event, name string) {
	t.Helper()
	select {
	case events <- fsnotify.Event{Name: name, Op: fsnotify.Write}:
	case <-time.After(time.Second):
		t.Fatal("test watch loop did not accept event")
	}
}

func waitForWatchAttempt(t *testing.T, attempted <-chan struct{}) {
	t.Helper()
	select {
	case <-attempted:
	case <-time.After(time.Second):
		t.Fatal("test watch loop did not attempt a reload")
	}
}

func assertNoWatchAttempt(t *testing.T, attempted <-chan struct{}, wait time.Duration) {
	t.Helper()
	select {
	case <-attempted:
		t.Fatal("test watch loop attempted more than one reload for an event burst")
	case <-time.After(wait):
	}
}

func TestParse_TableTests(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantErr string // substring; "" means no error
		wantN   int
	}{
		{name: "valid_three_repos", body: validThreeRepos, wantN: 3},
		{name: "valid_two_repos", body: validTwoRepos, wantN: 2},
		{name: "bad_apiversion", body: invalidBadAPIVersion, wantErr: "apiVersion"},
		{name: "empty_name", body: invalidEmptyName, wantErr: "name is required"},
		{name: "empty_url", body: invalidEmptyURL, wantErr: "url is required"},
		{name: "duplicate_names", body: invalidDuplicateNames, wantErr: "duplicated"},
		{name: "bad_glob", body: invalidBadGlob, wantErr: "glob"},
		{name: "malformed_yaml", body: malformedYAML, wantErr: "parse"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			reg, err := Parse([]byte(tc.body))
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if got := len(reg.Spec.Repos); got != tc.wantN {
					t.Fatalf("repo count: got %d want %d", got, tc.wantN)
				}
				// All repos must have a default_branch after defaults.
				for i, r := range reg.Spec.Repos {
					if r.DefaultBranch == "" {
						t.Errorf("repo[%d] %s: default_branch unset after defaults", i, r.Name)
					}
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestParse_DefaultBranch(t *testing.T) {
	body := `
apiVersion: mills.loom.dev/v1
kind: RepoRegistry
metadata: { name: workspace }
spec:
  repos:
    - name: loom-core
      url: git@example.com:loom-core.git
`
	reg, err := Parse([]byte(body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if reg.Spec.Repos[0].DefaultBranch != "main" {
		t.Errorf("default_branch: got %q want %q", reg.Spec.Repos[0].DefaultBranch, "main")
	}
}

func TestRegistry_FindAndRepos(t *testing.T) {
	reg, err := Parse([]byte(validThreeRepos))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got, ok := reg.Find("loom")
	if !ok {
		t.Fatalf("Find(loom): not found")
	}
	if got.ProjectID != 51 {
		t.Errorf("Find(loom).ProjectID: got %d want 51", got.ProjectID)
	}
	if _, ok := reg.Find("absent"); ok {
		t.Errorf("Find(absent): expected !ok")
	}
	all := reg.Repos()
	if len(all) != 3 {
		t.Errorf("Repos(): got %d want 3", len(all))
	}
	// Defensive copy: mutating the returned slice must not affect later reads.
	all[0].Name = "tampered"
	all[0].ProtectedPaths = append(all[0].ProtectedPaths, "tampered/**")
	again := reg.Repos()
	if again[0].Name == "tampered" {
		t.Errorf("Repos returned a shared slice — mutation leaked")
	}
}

func TestNewLoader_RejectsMissingFile(t *testing.T) {
	dir := t.TempDir()
	_, err := NewLoader(context.Background(), filepath.Join(dir, "nope.yaml"), nil, LoaderOptions{SkipWatch: true})
	if err == nil {
		t.Fatal("expected error for missing file")
	}
	if !strings.Contains(err.Error(), "read") {
		t.Errorf("error should reference read: %v", err)
	}
}

func TestNewLoader_RejectsInvalidInitialFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "repos.yaml")
	writeRegistry(t, path, invalidDuplicateNames)
	_, err := NewLoader(context.Background(), path, nil, LoaderOptions{SkipWatch: true})
	if err == nil {
		t.Fatal("expected error for invalid initial registry")
	}
	if !strings.Contains(err.Error(), "duplicated") {
		t.Errorf("error should reference duplicated: %v", err)
	}
}

func TestLoader_LoadsValidRegistry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "repos.yaml")
	writeRegistry(t, path, validThreeRepos)

	l, err := NewLoader(context.Background(), path, nil, LoaderOptions{SkipWatch: true})
	if err != nil {
		t.Fatalf("new loader: %v", err)
	}
	defer func() { _ = l.Close() }()

	snap := l.Snapshot()
	if len(snap) != 3 {
		t.Fatalf("snapshot count: got %d want 3", len(snap))
	}
	names := map[string]bool{}
	for _, r := range snap {
		names[r.Name] = true
	}
	for _, want := range []string{"loom-core", "loom", "flexdeck"} {
		if !names[want] {
			t.Errorf("missing %q in snapshot: %v", want, namesOf(snap))
		}
	}
	got, ok := l.Find("loom-core")
	if !ok || got.ProjectID != 47 {
		t.Errorf("Find(loom-core): ok=%v entry=%+v", ok, got)
	}
}

func TestLoader_HotReloadAddsNewEntry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "repos.yaml")
	writeRegistry(t, path, validTwoRepos)

	var (
		mu       sync.Mutex
		captured []*Registry
	)
	updated := make(chan struct{}, 8)
	l, err := NewLoader(context.Background(), path, nil, LoaderOptions{})
	if err != nil {
		t.Fatalf("new loader: %v", err)
	}
	defer func() { _ = l.Close() }()

	if got := len(l.Snapshot()); got != 2 {
		t.Fatalf("initial snapshot: got %d want 2", got)
	}

	l.Subscribe(func(r *Registry) {
		mu.Lock()
		captured = append(captured, r)
		mu.Unlock()
		select {
		case updated <- struct{}{}:
		default:
		}
	})

	// Atomic rewrite — fsnotify fires Create/Rename on the destination path.
	writeRegistryAtomic(t, path, validUpdatedThreeRepos)

	if !waitForSnapshot(t, l, 3, 3*time.Second) {
		mu.Lock()
		caps := len(captured)
		mu.Unlock()
		t.Fatalf("hot reload did not pick up new entry within 3s; subscribe fired %d times; got %v",
			caps, namesOf(l.Snapshot()))
	}

	// Drain the channel; we already polled the snapshot above.
	select {
	case <-updated:
	default:
	}

	got, ok := l.Find("flexinfer")
	if !ok {
		t.Fatalf("Find(flexinfer): not found after hot reload; have %v", namesOf(l.Snapshot()))
	}
	if got.ProjectID != 71 {
		t.Errorf("Find(flexinfer).ProjectID: got %d want 71", got.ProjectID)
	}
}

func TestLoader_InvalidReloadKeepsLastGood(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "repos.yaml")
	writeRegistry(t, path, validThreeRepos)

	var (
		mu      sync.Mutex
		errCh   = make(chan error, 8)
		gotErrs []error
	)
	l, err := NewLoader(context.Background(), path, nil, LoaderOptions{
		OnError: func(e error) {
			mu.Lock()
			gotErrs = append(gotErrs, e)
			mu.Unlock()
			select {
			case errCh <- e:
			default:
			}
		},
	})
	if err != nil {
		t.Fatalf("new loader: %v", err)
	}
	defer func() { _ = l.Close() }()

	pre := l.Snapshot()
	if len(pre) != 3 {
		t.Fatalf("pre-corruption count: got %d want 3", len(pre))
	}

	// Atomically corrupt — must not destroy the last-good snapshot.
	writeRegistryAtomic(t, path, malformedYAML)

	select {
	case <-errCh:
	case <-time.After(3 * time.Second):
		t.Fatal("OnError did not fire for malformed reload within 3s")
	}

	post := l.Snapshot()
	if len(post) != 3 {
		t.Errorf("last-good snapshot dropped on bad reload; got %d want 3 (%v)", len(post), namesOf(post))
	}

	// Now restore a valid file and confirm the loader recovers.
	writeRegistryAtomic(t, path, validTwoRepos)
	if !waitForSnapshot(t, l, 2, 3*time.Second) {
		t.Fatalf("loader did not recover after bad-then-good reload; got %v", namesOf(l.Snapshot()))
	}
}

func TestLoader_ParentOnlyHintRecoversInvalidThenValid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "repos.yaml")
	writeRegistry(t, path, validThreeRepos)

	errCh := make(chan error, 4)
	changed := make(chan struct{}, 4)
	l, err := NewLoader(context.Background(), path, nil, LoaderOptions{
		SkipWatch: true,
		OnError: func(err error) {
			select {
			case errCh <- err:
			default:
			}
		},
	})
	if err != nil {
		t.Fatalf("new loader: %v", err)
	}
	defer func() { _ = l.Close() }()
	l.Subscribe(func(*Registry) {
		select {
		case changed <- struct{}{}:
		default:
		}
	})
	events, attempted := startTestWatchEvents(t, l)

	writeRegistryAtomic(t, path, malformedYAML)
	sendWatchHint(t, events, path)
	waitForWatchAttempt(t, attempted)
	select {
	case <-errCh:
	default:
		t.Fatal("processed malformed registry hint did not report an error")
	}
	if got := len(l.Snapshot()); got != 3 {
		t.Fatalf("invalid reload replaced last-good snapshot: got %d repos, want 3", got)
	}
	select {
	case <-changed:
		t.Fatal("invalid reload fired a change callback")
	default:
	}
	sendWatchHint(t, events, filepath.Join(dir, "unrelated.tmp"))
	waitForWatchAttempt(t, attempted)
	select {
	case err := <-errCh:
		t.Fatalf("unchanged invalid bytes reported repeatedly: %v", err)
	default:
	}

	writeRegistryAtomic(t, path, validTwoRepos)
	// Reproduce the macOS/kqueue shape: the target replacement itself is not
	// named; only the watched parent directory reports a write hint.
	sendWatchHint(t, events, dir)
	waitForWatchAttempt(t, attempted)
	if got := len(l.Snapshot()); got != 2 {
		t.Fatalf("parent-only hint did not recover valid registry; got %v", namesOf(l.Snapshot()))
	}
	select {
	case <-changed:
	default:
		t.Fatal("processed valid recovery did not fire a change callback")
	}
}

func TestLoader_ParentOnlyHintRecoversAfterTransientReadFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "repos.yaml")
	writeRegistry(t, path, validThreeRepos)

	errCh := make(chan error, 1)
	changed := make(chan struct{}, 1)
	l, err := NewLoader(context.Background(), path, nil, LoaderOptions{
		SkipWatch: true,
		OnError:   func(err error) { errCh <- err },
	})
	if err != nil {
		t.Fatalf("new loader: %v", err)
	}
	defer func() { _ = l.Close() }()
	l.Subscribe(func(*Registry) { changed <- struct{}{} })
	events, attempted := startTestWatchEvents(t, l)

	if err := os.Remove(path); err != nil {
		t.Fatalf("remove registry: %v", err)
	}
	sendWatchHint(t, events, dir)
	waitForWatchAttempt(t, attempted)
	select {
	case <-errCh:
	default:
		t.Fatal("processed missing registry hint did not report a read error")
	}
	if got := len(l.Snapshot()); got != 3 {
		t.Fatalf("read failure replaced last-good snapshot: got %d repos, want 3", got)
	}
	sendWatchHint(t, events, filepath.Join(dir, "unrelated.tmp"))
	waitForWatchAttempt(t, attempted)
	select {
	case err := <-errCh:
		t.Fatalf("persistent read failure reported repeatedly: %v", err)
	default:
	}

	writeRegistry(t, path, validTwoRepos)
	sendWatchHint(t, events, dir)
	waitForWatchAttempt(t, attempted)
	if got := len(l.Snapshot()); got != 2 {
		t.Fatalf("parent-only hint did not recover after read failure; got %v", namesOf(l.Snapshot()))
	}
	select {
	case <-changed:
	default:
		t.Fatal("processed recovery after read failure did not fire a change callback")
	}
}

func TestLoader_ParentHintsDebounceAndIgnoreUnchanged(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "repos.yaml")
	writeRegistry(t, path, validTwoRepos)

	changed := make(chan struct{}, 64)
	l, err := NewLoader(context.Background(), path, nil, LoaderOptions{SkipWatch: true})
	if err != nil {
		t.Fatalf("new loader: %v", err)
	}
	defer func() { _ = l.Close() }()
	l.Subscribe(func(*Registry) { changed <- struct{}{} })
	events, attempted := startTestWatchEvents(t, l)

	writeRegistryAtomic(t, path, validUpdatedThreeRepos)
	for i := 0; i < 32; i++ {
		name := filepath.Join(dir, "unrelated.tmp")
		if i%2 == 0 {
			name = dir
		}
		sendWatchHint(t, events, name)
	}
	waitForWatchAttempt(t, attempted)
	if got := len(l.Snapshot()); got != 3 {
		t.Fatalf("debounced parent hints did not reload changed registry; got %v", namesOf(l.Snapshot()))
	}
	select {
	case <-changed:
	default:
		t.Fatal("processed changed registry did not fire a callback")
	}
	assertNoWatchAttempt(t, attempted, 5*l.watchDebounce)
	select {
	case <-changed:
		t.Fatal("event burst fired more than one callback")
	default:
	}

	for i := 0; i < 32; i++ {
		sendWatchHint(t, events, filepath.Join(dir, "still-unrelated.tmp"))
	}
	waitForWatchAttempt(t, attempted)
	select {
	case <-changed:
		t.Fatal("unchanged registry fired a callback for unrelated events")
	default:
	}
	assertNoWatchAttempt(t, attempted, 5*l.watchDebounce)
}

func TestLoader_ContinuousParentHintsRespectMaxDebounce(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "repos.yaml")
	writeRegistry(t, path, validTwoRepos)

	changed := make(chan struct{}, 1)
	l, err := NewLoader(context.Background(), path, nil, LoaderOptions{SkipWatch: true})
	if err != nil {
		t.Fatalf("new loader: %v", err)
	}
	defer func() { _ = l.Close() }()
	l.Subscribe(func(*Registry) { changed <- struct{}{} })
	events, attempted := startTestWatchEvents(t, l)

	writeRegistryAtomic(t, path, validUpdatedThreeRepos)
	stop := make(chan struct{})
	pumpDone := make(chan struct{})
	started := time.Now()
	go func() {
		defer close(pumpDone)
		for {
			select {
			case events <- fsnotify.Event{Name: dir, Op: fsnotify.Write}:
			case <-stop:
				return
			}
		}
	}()

	waitForWatchAttempt(t, attempted)
	elapsed := time.Since(started)
	close(stop)
	select {
	case <-pumpDone:
	case <-time.After(time.Second):
		t.Fatal("continuous event pump did not stop")
	}
	if elapsed < l.watchMaxDebounce/2 {
		t.Fatalf("reload happened after %s, before sustained hints could exercise the %s maximum", elapsed, l.watchMaxDebounce)
	}
	if elapsed > 4*l.watchMaxDebounce {
		t.Fatalf("reload happened after %s, well beyond the configured %s maximum", elapsed, l.watchMaxDebounce)
	}
	if got := len(l.Snapshot()); got != 3 {
		t.Fatalf("maximum debounce did not reload changed registry; got %v", namesOf(l.Snapshot()))
	}
	select {
	case <-changed:
	default:
		t.Fatal("maximum-debounce reload did not fire a change callback")
	}
}

func TestLoader_ReloadSyncSwapsSnapshot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "repos.yaml")
	writeRegistry(t, path, validTwoRepos)

	l, err := NewLoader(context.Background(), path, nil, LoaderOptions{SkipWatch: true})
	if err != nil {
		t.Fatalf("new loader: %v", err)
	}
	defer func() { _ = l.Close() }()

	if got := len(l.Snapshot()); got != 2 {
		t.Fatalf("initial: got %d want 2", got)
	}

	writeRegistry(t, path, validThreeRepos)
	if err := l.Reload(); err != nil {
		t.Fatalf("manual reload: %v", err)
	}
	if got := len(l.Snapshot()); got != 3 {
		t.Errorf("post-reload: got %d want 3", got)
	}

	// Bad reload returns an error and leaves the snapshot intact.
	writeRegistry(t, path, invalidDuplicateNames)
	if err := l.Reload(); err == nil {
		t.Fatal("expected error from invalid reload")
	}
	if got := len(l.Snapshot()); got != 3 {
		t.Errorf("post-bad-reload (last good): got %d want 3", got)
	}
}

// waitForSnapshot polls Snapshot() until len matches target or the deadline
// elapses. Returns true on match. Mirrors `eventually`-style assertions in
// pkg/mills/squads/loader_test.go without using time.Sleep as a workaround.
func waitForSnapshot(t *testing.T, l *Loader, target int, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if len(l.Snapshot()) == target {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func namesOf(rs []RepoEntry) []string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.Name)
	}
	return out
}
