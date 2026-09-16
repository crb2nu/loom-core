package main

import (
	"context"
	"io"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/crb2nu/loom/internal/devbox/backend"
	"github.com/crb2nu/loom/internal/devbox/detect"
	"github.com/crb2nu/loom/internal/devbox/state"
)

// stateEntry is an alias for convenience in tests.
type stateEntry = state.Entry

func newTestStore(cacheDir string) (*state.Store, error) {
	return state.NewStore(cacheDir)
}

// fakeStatus holds per-container status for the fake backend.
type fakeStatus struct {
	running bool
	status  string
}

// fakeBackend implements backend.Backend for testing.
type fakeBackend struct {
	statuses map[string]*fakeStatus

	// Configurable responses for handler tests.
	buildResult     *backend.BuildResult
	buildErr        error
	buildOpts       []backend.BuildOpts
	readFileContent []byte
	readFileErr     error
	writeFileErr    error
}

func (f *fakeBackend) Build(_ context.Context, opts backend.BuildOpts) (*backend.BuildResult, error) {
	f.buildOpts = append(f.buildOpts, opts)
	if f.buildErr != nil {
		return nil, f.buildErr
	}
	if f.buildResult != nil {
		return f.buildResult, nil
	}
	return &backend.BuildResult{}, nil
}
func (f *fakeBackend) Start(_ context.Context, opts backend.StartOpts) (*backend.StartResult, error) {
	return &backend.StartResult{ContainerID: opts.Name}, nil
}
func (f *fakeBackend) Exec(_ context.Context, _ backend.ExecOpts) (*backend.ExecResult, error) {
	return &backend.ExecResult{}, nil
}
func (f *fakeBackend) Stop(_ context.Context, _ string) error { return nil }
func (f *fakeBackend) Status(_ context.Context, id string) (*backend.StatusResult, error) {
	if s, ok := f.statuses[id]; ok {
		return &backend.StatusResult{Running: s.running, Status: s.status}, nil
	}
	return &backend.StatusResult{Running: false, Status: "not_found"}, nil
}
func (f *fakeBackend) Health(_ context.Context) error           { return nil }
func (f *fakeBackend) Pause(_ context.Context, _ string) error  { return backend.ErrNotSupported }
func (f *fakeBackend) Resume(_ context.Context, _ string) error { return backend.ErrNotSupported }
func (f *fakeBackend) ReadFile(_ context.Context, _, _ string) ([]byte, error) {
	if f.readFileErr != nil {
		return nil, f.readFileErr
	}
	return f.readFileContent, nil
}
func (f *fakeBackend) WriteFile(_ context.Context, _, _ string, _ []byte, _ string) error {
	if f.writeFileErr != nil {
		return f.writeFileErr
	}
	return nil
}
func (f *fakeBackend) CleanupBuilds(_ context.Context, _ time.Duration) (int, error) {
	return 0, nil
}

func TestCheckBackendHealth_Timeout(t *testing.T) {
	orig := backendHealthTimeout
	backendHealthTimeout = 50 * time.Millisecond
	defer func() {
		backendHealthTimeout = orig
	}()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	start := time.Now()

	checkBackendHealth(context.Background(), logger, func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	})

	elapsed := time.Since(start)
	if elapsed > 500*time.Millisecond {
		t.Fatalf("health check took too long: %v", elapsed)
	}
}

func TestSanitizeContainerName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input string
		want  string
	}{
		{"loom-core", "loom-core"},
		{"my_project", "my-project"},
		{"hello world!", "hello-world"},
		{"a/b/c.d", "a-b-c-d"},
		{"ALL_CAPS_123", "all-caps-123"},
		{"---", "sandbox"},
	}

	for _, tt := range tests {
		got := sanitizeContainerName(tt.input)
		if got != tt.want {
			t.Errorf("sanitizeContainerName(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestContainerName(t *testing.T) {
	t.Parallel()

	m := &manager{}

	tests := []struct {
		project string
		agentID string
		want    string
	}{
		{"loom-core", "", "devbox-loom-core"},
		{"my_app", "", "devbox-my-app"},
		{"hello world", "", "devbox-hello-world"},
		{"loom-core", "claude-code", "devbox-loom-core-claude-code"},
		{"loom-core", "codex", "devbox-loom-core-codex"},
	}

	for _, tt := range tests {
		got := m.containerName(tt.project, tt.agentID)
		if got != tt.want {
			t.Errorf("containerName(%q, %q) = %q, want %q", tt.project, tt.agentID, got, tt.want)
		}
	}
}

// TestContainerNameLongAgentIDsStayDistinct pins the fix for the shared
// Mills sandbox: every pipeline run's agent id starts with
// "loom-mills-operator-", so truncating to twelve characters mapped all of
// them onto one pod. Long ids must keep a readable prefix, stay within the
// twelve-character agent budget, be deterministic, and differ per id.
func TestContainerNameLongAgentIDsStayDistinct(t *testing.T) {
	t.Parallel()

	m := &manager{}
	hexDigest := regexp.MustCompile(`^[0-9a-f]{5}$`)

	long := []struct {
		project, agentID, wantPrefix string
	}{
		{"loom-core", "loom-mills-operator-8b916b47cece", "devbox-loom-core-loom-m-"},
		{"loom-core", "loom-mills-operator-373090d6b8f2", "devbox-loom-core-loom-m-"},
		{"loom-core", "codex-mills-verify", "devbox-loom-core-codex-"},
		{"flexdeck", "very-long-agent-name-here", "devbox-flexdeck-very-l-"},
	}
	seen := map[string]string{}
	for _, tt := range long {
		got := m.containerName(tt.project, tt.agentID)
		if !strings.HasPrefix(got, tt.wantPrefix) {
			t.Fatalf("containerName(%q, %q) = %q, want prefix %q", tt.project, tt.agentID, got, tt.wantPrefix)
		}
		if digest := strings.TrimPrefix(got, tt.wantPrefix); !hexDigest.MatchString(digest) {
			t.Fatalf("containerName(%q, %q) = %q, want a five-hex digest after the prefix", tt.project, tt.agentID, got)
		}
		if suffix := strings.TrimPrefix(got, "devbox-"+sanitizeContainerName(tt.project)+"-"); len(suffix) > agentSuffixBudget {
			t.Fatalf("agent suffix %q exceeds the %d-character budget", suffix, agentSuffixBudget)
		}
		if again := m.containerName(tt.project, tt.agentID); again != got {
			t.Fatalf("containerName is not deterministic: %q then %q", got, again)
		}
		if prev, dup := seen[got]; dup {
			t.Fatalf("agent ids %q and %q collide on sandbox name %q", prev, tt.agentID, got)
		}
		seen[got] = tt.agentID
	}
}

// TestSandboxGoCache pins the shared-cache wiring: no claim keeps the pod
// spec byte-identical (same env map, no mounts); a claim yields a COPY of the
// env pointing every Go cache at the mount, with the fingerprint's own map
// untouched.
func TestSandboxGoCache(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name      string
		claim     string
		env       map[string]string
		wantDebug string
	}{
		{name: "no claim nil"},
		{name: "no claim unchanged", env: map[string]string{"GODEBUG": "goindex=1,gctrace=1", "GOCACHE": "/original"}},
		{name: "fresh", claim: "cache"},
		{name: "preserve environment", claim: "cache", env: map[string]string{"FOO": "bar", "GOCACHE": "/old"}},
		{name: "empty", claim: "cache", env: map[string]string{"GODEBUG": ""}},
		{name: "append", claim: "cache", env: map[string]string{"GODEBUG": "gctrace=1"}, wantDebug: "gctrace=1,goindex=0"},
		{name: "replace", claim: "cache", env: map[string]string{"GODEBUG": "goindex=1"}},
		{name: "already disabled", claim: "cache", env: map[string]string{"GODEBUG": "goindex=0"}},
		{name: "duplicates", claim: "cache", env: map[string]string{"GODEBUG": "goindex=1,gctrace=1,goindex=0,asyncpreemptoff=1,goindex=1"}, wantDebug: "gctrace=1,asyncpreemptoff=1,goindex=0"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			before := maps.Clone(tt.env)
			env, mounts := sandboxGoCache(tt.env, tt.claim)
			if !maps.Equal(tt.env, before) {
				t.Fatalf("input mutated: got %v, want %v", tt.env, before)
			}
			if tt.claim == "" {
				if mounts != nil || !maps.Equal(env, before) || (env == nil) != (tt.env == nil) {
					t.Fatalf("no claim: env=%v mounts=%v", env, mounts)
				}
				if env != nil {
					env["probe"] = "same map"
					if tt.env["probe"] != "same map" {
						t.Fatal("no claim must return the original map")
					}
				}
				return
			}
			if len(mounts) != 1 || mounts[0].ClaimName != tt.claim || mounts[0].MountPath != sandboxGoCacheMountPath {
				t.Fatalf("unexpected mounts: %+v", mounts)
			}
			want := maps.Clone(before)
			if want == nil {
				want = make(map[string]string)
			}
			want["GOCACHE"] = sandboxGoCacheMountPath + "/go-build"
			want["GOMODCACHE"] = sandboxGoCacheMountPath + "/gomod"
			want["GOLANGCI_LINT_CACHE"] = sandboxGoCacheMountPath + "/golangci-lint"
			want["GODEBUG"] = tt.wantDebug
			if want["GODEBUG"] == "" {
				want["GODEBUG"] = "goindex=0"
			}
			if !maps.Equal(env, want) {
				t.Fatalf("env = %v, want %v", env, want)
			}
			env["probe"] = "copy"
			if !maps.Equal(tt.env, before) {
				t.Fatal("returned map aliases input")
			}
		})
	}
}

func TestGateTimeoutsFallBackToDefaults(t *testing.T) {
	t.Parallel()

	m := &manager{}
	if got := m.gateCheckTimeout(); got != defaultGateCheckTimeoutSec {
		t.Fatalf("check timeout = %d, want default %d", got, defaultGateCheckTimeoutSec)
	}
	if got := m.gateTestTimeout(); got != defaultGateTestTimeoutSec {
		t.Fatalf("test timeout = %d, want default %d", got, defaultGateTestTimeoutSec)
	}
	m.cfg.gateCheckTimeoutSec, m.cfg.gateTestTimeoutSec = 120, 1200
	if m.gateCheckTimeout() != 120 || m.gateTestTimeout() != 1200 {
		t.Fatalf("configured timeouts = %d/%d, want 120/1200", m.gateCheckTimeout(), m.gateTestTimeout())
	}
}

func TestStoreKey(t *testing.T) {
	t.Parallel()

	tests := []struct {
		project string
		agentID string
		want    string
	}{
		{"loom-core", "", "loom-core"},
		{"loom-core", "claude-code", "loom-core/claude-code"},
		{"flexdeck", "codex", "flexdeck/codex"},
	}

	for _, tt := range tests {
		got := storeKey(tt.project, tt.agentID)
		if got != tt.want {
			t.Errorf("storeKey(%q, %q) = %q, want %q", tt.project, tt.agentID, got, tt.want)
		}
	}
}

func TestParseStoreKey(t *testing.T) {
	t.Parallel()

	tests := []struct {
		key     string
		project string
		agentID string
	}{
		{"loom-core", "loom-core", ""},
		{"loom-core/claude-code", "loom-core", "claude-code"},
		{"flexdeck/codex", "flexdeck", "codex"},
	}

	for _, tt := range tests {
		project, agentID := parseStoreKey(tt.key)
		if project != tt.project || agentID != tt.agentID {
			t.Errorf("parseStoreKey(%q) = (%q, %q), want (%q, %q)",
				tt.key, project, agentID, tt.project, tt.agentID)
		}
	}
}

func TestImageTag(t *testing.T) {
	t.Parallel()

	m := &manager{
		cfg: managerConfig{
			imagePrefix: "registry.local/devbox",
		},
	}

	tag := m.imageTag("loom-core", "abc1234567890")
	expected := "registry.local/devbox/loom-core:abc1234"
	if tag != expected {
		t.Errorf("imageTag = %q, want %q", tag, expected)
	}
}

func TestActiveExecs(t *testing.T) {
	t.Parallel()

	m := &manager{}

	if m.hasActiveExecs("test-project") {
		t.Error("expected no active execs initially")
	}

	m.incActiveExecs("test-project")
	if !m.hasActiveExecs("test-project") {
		t.Error("expected active execs after inc")
	}

	m.decActiveExecs("test-project")
	if m.hasActiveExecs("test-project") {
		t.Error("expected no active execs after dec")
	}
}

func TestCanonicalBackendType(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input string
		want  string
	}{
		{"", "docker"},
		{"docker", "docker"},
		{"k8s", "k8s"},
		{"kubernetes", "k8s"},
		{"harvester-vm", "harvester-vm"},
		{"custom", "custom"},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.input, func(t *testing.T) {
			t.Parallel()
			if got := canonicalBackendType(tt.input); got != tt.want {
				t.Fatalf("canonicalBackendType(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestBackendFor(t *testing.T) {
	t.Parallel()

	k8sBackend := &fakeBackend{}
	harvesterBackend := &fakeBackend{}
	m := &manager{
		backend:        k8sBackend,
		defaultBackend: "k8s",
		backends: map[string]backend.Backend{
			"k8s":          k8sBackend,
			"harvester-vm": harvesterBackend,
		},
	}

	if got := m.backendFor(""); got != k8sBackend {
		t.Fatalf("backendFor(empty) = %#v, want default k8s backend", got)
	}
	if got := m.backendFor("kubernetes"); got != k8sBackend {
		t.Fatalf("backendFor(kubernetes) = %#v, want k8s backend", got)
	}
	if got := m.backendFor("harvester-vm"); got != harvesterBackend {
		t.Fatalf("backendFor(harvester-vm) = %#v, want harvester backend", got)
	}
	if got := m.backendFor("missing"); got != k8sBackend {
		t.Fatalf("backendFor(missing) = %#v, want singleton fallback", got)
	}
}

func TestGenerateSandboxDockerfile_GitCloneAllowsNoLocalLanguages(t *testing.T) {
	m := &manager{
		cfg:    managerConfig{syncMode: "git-clone"},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	fp := &detect.EnvFingerprint{
		ProjectDir:  "/app/services/loom-core",
		ProjectName: "loom-core",
		Hash:        "abc123456789",
	}

	df, err := m.generateSandboxDockerfile(fp)
	if err != nil {
		t.Fatalf("generateSandboxDockerfile returned error: %v", err)
	}
	got := string(df)
	for _, want := range []string{"ARG DEVBOX_BASE_IMAGE=registry.harbor.lan/mcp/devbox-base/go:1.25", "FROM ${DEVBOX_BASE_IMAGE}", `ENV PATH="/usr/local/go/bin:${PATH}"`, "nodejs npm python3", "WORKDIR /workspace"} {
		if !strings.Contains(got, want) {
			t.Fatalf("fallback Dockerfile missing %q:\n%s", want, got)
		}
	}
}

func TestGenerateSandboxDockerfile_NFSStillRejectsNoLocalLanguages(t *testing.T) {
	m := &manager{cfg: managerConfig{syncMode: "nfs"}}
	fp := &detect.EnvFingerprint{
		ProjectDir:  "/app/services/loom-core",
		ProjectName: "loom-core",
		Hash:        "abc123456789",
	}

	_, err := m.generateSandboxDockerfile(fp)
	if err == nil || !strings.Contains(err.Error(), "no languages detected") {
		t.Fatalf("expected no-languages error, got %v", err)
	}
}

func TestResolveProject_GitCloneFallbackForUnstagedRepo(t *testing.T) {
	ws := t.TempDir()
	// A staged repo still resolves to its real on-disk path.
	if err := os.MkdirAll(filepath.Join(ws, "services", "loom-core"), 0o755); err != nil {
		t.Fatal(err)
	}
	m := &manager{
		cfg:    managerConfig{syncMode: "git-clone", workspaceRoot: ws},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	cases := []struct {
		name     string
		project  string
		wantDir  string
		wantName string
	}{
		{"staged repo resolves on disk", "loom-core", filepath.Join(ws, "services", "loom-core"), "loom-core"},
		{"unstaged bare name -> services bucket", "flexdeck", filepath.Join(ws, "services", "flexdeck"), "flexdeck"},
		{"unstaged bucket-qualified verbatim", "libs/svg-sdk", filepath.Join(ws, "libs", "svg-sdk"), "svg-sdk"},
		{"unstaged services-qualified", "services/loom-flightdeck", filepath.Join(ws, "services", "loom-flightdeck"), "loom-flightdeck"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, name, err := m.resolveProject(tc.project)
			if err != nil {
				t.Fatalf("resolveProject(%q) errored: %v", tc.project, err)
			}
			if dir != tc.wantDir || name != tc.wantName {
				t.Errorf("resolveProject(%q) = (%q, %q), want (%q, %q)", tc.project, dir, name, tc.wantDir, tc.wantName)
			}
		})
	}

	// Unsafe inputs are rejected even in git-clone mode.
	for _, bad := range []string{"../escape", "../../etc/passwd", "/etc/passwd"} {
		if _, _, err := m.resolveProject(bad); err == nil {
			t.Errorf("resolveProject(%q) expected error, got nil", bad)
		}
	}
}

func TestResolveProject_NonGitCloneStillHardFails(t *testing.T) {
	ws := t.TempDir()
	for _, mode := range []string{"tar-pipe", "nfs"} {
		m := &manager{cfg: managerConfig{syncMode: mode, workspaceRoot: ws}}
		if _, _, err := m.resolveProject("flexdeck"); err == nil {
			t.Errorf("syncMode=%s: resolveProject(unstaged) should hard-fail, got nil", mode)
		}
	}
}

func TestBuildMounts_K8sBackendReturnsEmpty(t *testing.T) {
	t.Parallel()

	m := &manager{
		cfg: managerConfig{
			backendType:   "k8s",
			workspaceRoot: "/home/user/workspace",
		},
	}

	mounts := m.buildMounts("/home/user/workspace/services/app")
	if len(mounts) != 0 {
		t.Errorf("expected empty mounts for k8s backend, got %d", len(mounts))
	}
}

func TestBuildMounts_DockerMonorepoMountsWorkspaceRoot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	workspace := filepath.Join(home, "workspace")
	projectDir := filepath.Join(workspace, "services", "loom-core")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatalf("mkdir project dir: %v", err)
	}

	m := &manager{
		cfg: managerConfig{
			backendType:   "docker",
			workspaceRoot: workspace,
		},
	}
	mounts := m.buildMounts(projectDir)
	if len(mounts) == 0 {
		t.Fatal("expected at least one mount")
	}

	if mounts[0].Host != workspace || mounts[0].Container != "/workspace" {
		t.Fatalf("expected workspace root mount, got %#v", mounts[0])
	}

	for _, mount := range mounts {
		if mount.Host == projectDir && mount.Container == "/workspace" {
			t.Fatalf("unexpected direct project mount for monorepo project: %#v", mount)
		}
	}
}

func TestBuildMounts_DockerOutsideWorkspaceMountsProjectDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	workspace := filepath.Join(home, "workspace")
	projectDir := filepath.Join(home, "external", "other-repo")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatalf("mkdir project dir: %v", err)
	}

	m := &manager{
		cfg: managerConfig{
			backendType:   "docker",
			workspaceRoot: workspace,
		},
	}
	mounts := m.buildMounts(projectDir)
	if len(mounts) == 0 {
		t.Fatal("expected at least one mount")
	}

	if mounts[0].Host != projectDir || mounts[0].Container != "/workspace" {
		t.Fatalf("expected direct project mount for outside-workspace project, got %#v", mounts[0])
	}
}

func TestIsK8sBackend(t *testing.T) {
	t.Parallel()

	tests := []struct {
		backendType string
		want        bool
	}{
		{"k8s", true},
		{"kubernetes", true},
		{"docker", false},
		{"", false},
	}
	for _, tt := range tests {
		m := &manager{cfg: managerConfig{backendType: tt.backendType}}
		if got := m.isK8sBackend(); got != tt.want {
			t.Errorf("isK8sBackend(%q) = %v, want %v", tt.backendType, got, tt.want)
		}
	}
}

func TestReconcileState(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cacheDir := filepath.Join(t.TempDir(), "cache")

	store, err := newTestStore(cacheDir)
	if err != nil {
		t.Fatalf("create store: %v", err)
	}

	fb := &fakeBackend{statuses: map[string]*fakeStatus{
		"devbox-alive":  {running: true, status: "running"},
		"devbox-dead":   {running: false, status: "not_found"},
		"devbox-failed": {running: false, status: "failed"},
	}}

	now := time.Now()
	_ = store.Set("alive", &stateEntry{
		Status:    "running",
		LastUsed:  now,
		CreatedAt: now,
	})
	_ = store.Set("dead", &stateEntry{
		Status:    "running",
		LastUsed:  now,
		CreatedAt: now,
	})
	_ = store.Set("failed", &stateEntry{
		Status:    "paused",
		LastUsed:  now,
		CreatedAt: now,
	})
	_ = store.Set("already-stopped", &stateEntry{
		Status:    "stopped",
		LastUsed:  now,
		CreatedAt: now,
	})

	m := &manager{
		cfg:     managerConfig{backendType: "k8s"},
		backend: fb,
		store:   store,
		logger:  logger,
	}

	m.reconcileState(context.Background())

	// "alive" should stay running
	if e := store.Get("alive"); e == nil || e.Status != "running" {
		t.Errorf("alive entry should stay running, got: %v", e)
	}

	// "dead" should be marked stopped
	if e := store.Get("dead"); e == nil || e.Status != "stopped" {
		t.Errorf("dead entry should be stopped, got: %v", e)
	}

	// "failed" should be marked stopped
	if e := store.Get("failed"); e == nil || e.Status != "stopped" {
		t.Errorf("failed entry should be stopped, got: %v", e)
	}

	// "already-stopped" should remain stopped (not touched)
	if e := store.Get("already-stopped"); e == nil || e.Status != "stopped" {
		t.Errorf("already-stopped entry should remain stopped, got: %v", e)
	}
}

// --- reapIdle tests ---

func TestReapIdle_SkipsActiveExecs(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	store, _ := newTestStore(filepath.Join(t.TempDir(), "cache"))

	fb := &fakeBackend{statuses: map[string]*fakeStatus{}}

	// Set up an idle entry
	old := time.Now().Add(-10 * time.Minute)
	_ = store.Set("proj-a", &stateEntry{
		Status:   "running",
		LastUsed: old,
	})

	m := &manager{
		cfg:     managerConfig{backendType: "docker", idleTimeout: 5 * time.Minute},
		backend: fb,
		store:   store,
		logger:  logger,
	}

	// Mark active execs — reap should skip
	m.incActiveExecs("proj-a")
	m.reapIdle(context.Background())

	if e := store.Get("proj-a"); e == nil || e.Status != "running" {
		t.Error("entry with active execs should stay running")
	}
}

func TestReapIdle_DockerPauseFallsBackToStop(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	store, _ := newTestStore(filepath.Join(t.TempDir(), "cache"))

	fb := &fakeBackend{statuses: map[string]*fakeStatus{}}

	old := time.Now().Add(-10 * time.Minute)
	_ = store.Set("proj-a", &stateEntry{
		Status:   "running",
		LastUsed: old,
	})

	m := &manager{
		cfg:     managerConfig{backendType: "docker", idleTimeout: 5 * time.Minute},
		backend: fb,
		store:   store,
		logger:  logger,
	}

	m.reapIdle(context.Background())

	// fakeBackend.Pause returns ErrNotSupported, so it should fall back to Stop
	e := store.Get("proj-a")
	if e == nil || e.Status != "stopped" {
		t.Errorf("expected stopped after pause fallback, got: %v", e)
	}
}

func TestReapIdle_K8sKeepsWarmThenHardReaps(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	store, _ := newTestStore(filepath.Join(t.TempDir(), "cache"))

	fb := &fakeBackend{statuses: map[string]*fakeStatus{}}

	idleTimeout := 5 * time.Minute

	// Pod idle for 1.5× timeout — should be kept warm (under 2×)
	warmIdle := time.Now().Add(-time.Duration(float64(idleTimeout) * 1.5))
	_ = store.Set("warm-pod", &stateEntry{
		Status:   "running",
		LastUsed: warmIdle,
	})

	// Pod idle for 3× timeout — should be hard-reaped
	staleIdle := time.Now().Add(-3 * idleTimeout)
	_ = store.Set("stale-pod", &stateEntry{
		Status:   "running",
		LastUsed: staleIdle,
	})

	m := &manager{
		cfg:     managerConfig{backendType: "k8s", idleTimeout: idleTimeout},
		backend: fb,
		store:   store,
		logger:  logger,
	}

	m.reapIdle(context.Background())

	// warm-pod: should still be running (kept warm)
	if e := store.Get("warm-pod"); e == nil || e.Status != "running" {
		t.Errorf("warm pod should stay running, got: %v", e)
	}

	// stale-pod: should be stopped (hard-reaped)
	if e := store.Get("stale-pod"); e == nil || e.Status != "stopped" {
		t.Errorf("stale pod should be stopped, got: %v", e)
	}
}

// TestReapIdle_MillsBackstop pins bl-devbox-sandbox-quota-headroom-20260913: under
// DEVBOX_MILLS_IDLE_TIMEOUT a Mills per-run sandbox (main or baseline) is hard-reaped
// as soon as it passes the backstop — no 2× keep-warm grace — while every other
// sandbox keeps the global timeout; with the backstop unset, Mills sandboxes follow
// the global policy, warm grace included.
func TestReapIdle_MillsBackstop(t *testing.T) {
	const (
		mills    = "loom-core/loom-mills-operator-698e4c672a6c"
		baseline = mills + "-baseline"
		fresh    = "loom-core/loom-mills-operator-0a8a7c27a051"
		other    = "loom-core/claude-code" // another agent's sandbox
		shared   = "loom-core"             // the shared per-repo sandbox
	)
	for _, tc := range []struct {
		name                          string
		idleTimeout, millsIdleTimeout time.Duration
		active                        []string
		idle                          map[string]time.Duration // key -> idle span
		want                          map[string]string        // key -> status after one reap
	}{
		{
			name: "backstop set", idleTimeout: 2 * time.Hour, millsIdleTimeout: 5 * time.Minute,
			idle: map[string]time.Duration{mills: 6 * time.Minute, baseline: 6 * time.Minute, fresh: 4 * time.Minute, other: 6 * time.Minute, shared: 6 * time.Minute},
			want: map[string]string{mills: "stopped", baseline: "stopped", fresh: "running", other: "running", shared: "running"},
		},
		{
			name: "active Mills gate", idleTimeout: 2 * time.Hour, millsIdleTimeout: 5 * time.Minute,
			idle:   map[string]time.Duration{mills: 90 * time.Minute},
			active: []string{mills}, want: map[string]string{mills: "running"},
		},
		{
			name: "backstop unset", idleTimeout: 5 * time.Minute,
			idle: map[string]time.Duration{mills: 7 * time.Minute, baseline: 15 * time.Minute},
			want: map[string]string{mills: "running", baseline: "stopped"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, _ := newTestStore(filepath.Join(t.TempDir(), "cache"))
			for key, span := range tc.idle {
				_ = store.Set(key, &stateEntry{Status: "running", LastUsed: time.Now().Add(-span)})
			}
			m := &manager{
				cfg:     managerConfig{backendType: "k8s", idleTimeout: tc.idleTimeout, millsIdleTimeout: tc.millsIdleTimeout},
				backend: &fakeBackend{statuses: map[string]*fakeStatus{}},
				store:   store,
				logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
			}
			for _, key := range tc.active {
				m.incActiveExecs(key)
			}
			m.reapIdle(context.Background())
			for key, want := range tc.want {
				if e := store.Get(key); e == nil || e.Status != want {
					t.Errorf("%s: status = %v, want %s", key, e, want)
				}
			}
		})
	}
}

func TestReapIdle_SkipsWarmPoolProjects(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	store, _ := newTestStore(filepath.Join(t.TempDir(), "cache"))

	fb := &fakeBackend{statuses: map[string]*fakeStatus{}}

	old := time.Now().Add(-1 * time.Hour)
	_ = store.Set("warm-proj", &stateEntry{
		Status:   "running",
		LastUsed: old,
	})

	m := &manager{
		cfg: managerConfig{
			backendType:  "docker",
			idleTimeout:  5 * time.Minute,
			warmProjects: []string{"warm-proj"},
		},
		backend: fb,
		store:   store,
		logger:  logger,
	}

	m.reapIdle(context.Background())

	// warm-proj has no agent ID and is in warmProjects — should be skipped
	if e := store.Get("warm-proj"); e == nil || e.Status != "running" {
		t.Errorf("warm pool project should stay running, got: %v", e)
	}
}

func TestLangNames(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		fp   *detect.EnvFingerprint
		want string
	}{
		{
			name: "empty languages",
			fp:   &detect.EnvFingerprint{},
			want: "",
		},
		{
			name: "single language",
			fp: &detect.EnvFingerprint{
				Languages: []detect.LanguageSpec{
					{Language: "go"},
				},
			},
			want: "go",
		},
		{
			name: "multiple languages preserve order",
			fp: &detect.EnvFingerprint{
				Languages: []detect.LanguageSpec{
					{Language: "go"},
					{Language: "python"},
					{Language: "node"},
				},
			},
			want: "go, python, node",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := langNames(tt.fp); got != tt.want {
				t.Fatalf("langNames() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestShutdownProtectsActiveK8sSandbox(t *testing.T) {
	for _, kind := range []string{"k8s", "docker"} {
		t.Run(kind, func(t *testing.T) {
			store, err := newTestStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			_ = store.Set("busy", &stateEntry{Status: "running"})
			_ = store.Set("idle", &stateEntry{Status: "running"})
			b := &shutdownRecorder{}
			m := &manager{cfg: managerConfig{backendType: kind}, backend: b, store: store, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
			m.incActiveExecs("busy")
			m.shutdownAll(context.Background())
			entries := store.List()
			want := "stopped"
			wantStops := 2
			if kind == "k8s" {
				want = "running"
				wantStops = 1
			}
			if len(b.stopped) != wantStops {
				t.Fatalf("unexpected sandbox stops: %v", b.stopped)
			}
			if entries["busy"].Status != want || entries["idle"].Status != "stopped" {
				t.Fatalf("wrong cleanup: %+v", entries)
			}
		})
	}
}

func TestShutdownBoundsUncooperativeAsyncWorker(t *testing.T) {
	store, err := newTestStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m := &manager{cfg: managerConfig{backendType: "k8s"}, backend: &fakeBackend{}, store: store, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	m.asyncWg.Add(1)
	defer m.asyncWg.Done()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	m.shutdownAll(ctx)
	if time.Since(start) > time.Second {
		t.Fatal("cleanup ignored deadline")
	}
}

type shutdownRecorder struct {
	fakeBackend
	stopped []string
}

func (b *shutdownRecorder) Stop(_ context.Context, name string) error {
	b.stopped = append(b.stopped, name)
	return nil
}
