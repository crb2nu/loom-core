// mcp-devbox is an MCP server providing persistent, project-aware container
// sandboxes for AI coding agents. Each project gets an auto-built container
// with the right runtimes and dependencies.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	mcp "gitlab.flexinfer.ai/libs/mcp-go"

	"github.com/crb2nu/loom/internal/devbox/backend"
	"github.com/crb2nu/loom/internal/devbox/baseimage"
	"github.com/crb2nu/loom/internal/devbox/state"
	"github.com/crb2nu/loom/pkg/env"
	"github.com/crb2nu/loom/pkg/lifecycle"
	"github.com/crb2nu/loom/pkg/mcperror"
	"github.com/crb2nu/loom/pkg/mcpscaffold"
)

var version = "0.2.0"

func main() {
	if err := lifecycle.RunWithSignals(context.Background(), run); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	if !isK8sBackendType(env.String("DEVBOX_BACKEND", "docker")) {
		srv, cleanup, err := newDevboxServer(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = cleanup(context.Background()) }()
		return srv.Run(ctx)
	}
	workCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	defer cancel()
	drain := &lifecycle.Drain{}
	workCtx = context.WithValue(workCtx, drainContextKey{}, drain)
	srv, cleanup, err := newDevboxServer(workCtx)
	if err != nil {
		return err
	}
	defer func() {
		cancel()
		cleanupCtx, stop := context.WithTimeout(context.Background(), 20*time.Second)
		defer stop()
		done := make(chan struct{})
		go func() { _ = cleanup(cleanupCtx); close(done) }()
		select {
		case <-done:
		case <-cleanupCtx.Done():
		}
	}()
	transport := &drainTransport{Transport: mcp.NewStdioTransport(os.Stdin, os.Stdout), drain: drain, pending: make(map[string]int)}
	finished := make(chan error, 1)
	go func() { finished <- srv.RunWithTransport(workCtx, transport) }()
	select {
	case err := <-finished:
		return err
	case <-ctx.Done():
	}
	waitDevboxDrain(drain, env.Duration("DEVBOX_DRAIN_TIMEOUT", 30*time.Minute), srv.Logger)
	return nil
}

func waitDevboxDrain(drain *lifecycle.Drain, timeout time.Duration, logger *slog.Logger) {
	n, done := drain.Begin()
	logger.Info(fmt.Sprintf("devbox: draining %d in-flight calls", n))
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
	}
	logger.Info("devbox: drain complete")
}

// newDevboxServer builds the MCP server and its sandbox manager without
// running the stdio loop, so tests can drive initialize, tools/list and tool
// calls in-process. A missing docker CLI is not fatal: the docker backend
// degrades to NotConfigured on every sandbox call, exactly like a missing
// socket, while the transport stays up. The returned cleanup stops every
// sandbox the manager started and flushes the tracer.
func newDevboxServer(ctx context.Context) (*mcpscaffold.Server, func(context.Context) error, error) {
	srv, shutdownTracer, err := mcpscaffold.NewServer(ctx, "mcp-devbox", version,
		mcpscaffold.WithInstructions("Project-aware dev sandbox executor. Builds isolated containers with auto-detected runtimes and deps. "+
			"Tools: devbox_exec, devbox_build, devbox_status, devbox_stop, devbox_detect, "+
			"devbox_read_file, devbox_write_file, devbox_exec_async, devbox_exec_poll, "+
			"devbox_metrics, devbox_summary"),
	)
	if err != nil {
		return nil, nil, err
	}

	logger := srv.Logger
	logger.Info("starting server", "name", "mcp-devbox", "version", version,
		"backend", env.String("DEVBOX_BACKEND", "docker"), "build_timeout", buildTimeout())

	workspaceRoot := env.String("DEVBOX_WORKSPACE_ROOT", "")
	if workspaceRoot == "" {
		home, _ := os.UserHomeDir()
		workspaceRoot = home + "/workspace"
	}

	cacheDir := env.String("DEVBOX_CACHE_DIR", "")
	if cacheDir == "" {
		home, _ := os.UserHomeDir()
		cacheDir = home + "/.cache/loom/devbox"
	}

	backendType := env.String("DEVBOX_BACKEND", "docker")

	// K8s backend defaults to 2h idle timeout (sleeping pods are nearly free).
	defaultIdleTimeout := 30 * 60 * time.Second // 30m for Docker
	if backendType == "k8s" || backendType == "kubernetes" {
		defaultIdleTimeout = 2 * time.Hour
	}

	// Sync mode: tar-pipe (default for local), git-clone, nfs.
	// Auto-detect: if workspace root exists on disk, default to tar-pipe (local mode).
	// Otherwise default to git-clone (hub/remote mode).
	syncMode := env.String("DEVBOX_SYNC_MODE", "")
	if syncMode == "" {
		if _, err := os.Stat(workspaceRoot); err == nil {
			syncMode = "tar-pipe"
		} else {
			syncMode = "git-clone"
		}
	}

	// Parse extra sync excludes from comma-separated env var.
	var syncExcludes []string
	if se := env.String("DEVBOX_SYNC_EXCLUDES", ""); se != "" {
		for _, e := range strings.Split(se, ",") {
			e = strings.TrimSpace(e)
			if e != "" {
				syncExcludes = append(syncExcludes, e)
			}
		}
	}

	maxSyncSizeMB := env.Int("DEVBOX_MAX_SYNC_SIZE_MB", 200)

	// NFS cache flush: only for NFS sync mode.
	isK8s := backendType == "k8s" || backendType == "kubernetes"
	nfsFlush := env.Bool("DEVBOX_NFS_FLUSH", isK8s && syncMode == "nfs")

	// Parse warm projects from comma-separated env var
	var warmProjects []string
	if wp := env.String("DEVBOX_WARM_PROJECTS", ""); wp != "" {
		for _, p := range strings.Split(wp, ",") {
			p = strings.TrimSpace(p)
			if p != "" {
				warmProjects = append(warmProjects, p)
			}
		}
	}

	logger.Info("workspace sync", "mode", syncMode, "workspace", workspaceRoot)
	cfg := managerConfig{
		workspaceRoot:                workspaceRoot,
		cacheDir:                     cacheDir,
		backendType:                  backendType,
		registry:                     env.String("DEVBOX_REGISTRY", "registry.harbor.lan"),
		imagePrefix:                  env.String("DEVBOX_IMAGE_PREFIX", "mcp/devbox"),
		maxTailLines:                 env.Int("DEVBOX_MAX_TAIL_LINES", 20),
		idleTimeout:                  env.Duration("DEVBOX_IDLE_TIMEOUT", defaultIdleTimeout),
		millsIdleTimeout:             env.Duration("DEVBOX_MILLS_IDLE_TIMEOUT", defaultMillsIdleTimeout),
		defaultCPU:                   env.Float("DEVBOX_DEFAULT_CPU", 0.5),
		defaultMemMB:                 env.Int("DEVBOX_DEFAULT_MEMORY_MB", 2048),
		kubeconfig:                   env.String("DEVBOX_KUBECONFIG", ""),
		k8sNamespace:                 env.String("DEVBOX_K8S_NAMESPACE", "devbox"),
		storageClass:                 env.String("DEVBOX_K8S_STORAGE_CLASS", "longhorn"),
		k8sWorkspacePVC:              env.String("DEVBOX_K8S_WORKSPACE_PVC", "devbox-workspace-nfs"),
		k8sImagePullSecret:           env.String("DEVBOX_K8S_IMAGE_PULL_SECRET", "harbor-creds"),
		builderImage:                 builderImage(),
		gitCloneImage:                env.String("DEVBOX_K8S_GIT_CLONE_IMAGE", ""),
		buildCPURequest:              env.String("DEVBOX_K8S_BUILD_CPU_REQUEST", ""),
		buildCPULimit:                env.String("DEVBOX_K8S_BUILD_CPU_LIMIT", ""),
		buildMemoryRequest:           env.String("DEVBOX_K8S_BUILD_MEMORY_REQUEST", ""),
		buildMemoryLimit:             env.String("DEVBOX_K8S_BUILD_MEMORY_LIMIT", ""),
		buildEphemeralStorageRequest: env.String("DEVBOX_K8S_BUILD_EPHEMERAL_STORAGE_REQUEST", ""),
		buildEphemeralStorageLimit:   env.String("DEVBOX_K8S_BUILD_EPHEMERAL_STORAGE_LIMIT", ""),
		buildAvoidNodes:              env.String("DEVBOX_K8S_BUILD_AVOID_NODES", ""),
		maxConcurrentBuilds:          env.Int("DEVBOX_K8S_MAX_CONCURRENT_BUILDS", 1),
		buildTimeout:                 buildTimeout(),
		gitCloneMemoryRequest:        env.String("DEVBOX_K8S_GIT_CLONE_MEMORY_REQUEST", ""),
		gitCloneMemoryLimit:          env.String("DEVBOX_K8S_GIT_CLONE_MEMORY_LIMIT", ""),
		nfsFlush:                     nfsFlush,
		gitBaseURL:                   env.String("DEVBOX_K8S_GIT_BASE_URL", ""),
		gitSecret:                    env.String("DEVBOX_K8S_GIT_SECRET", ""),
		gitToken:                     env.String("DEVBOX_GIT_TOKEN", ""),
		remoteManifestTTL:            env.Duration("DEVBOX_REMOTE_MANIFEST_TTL", defaultRemoteManifestTTL),
		syncMode:                     syncMode,
		syncExcludes:                 syncExcludes,
		maxSyncSize:                  int64(maxSyncSizeMB) * 1024 * 1024,
		warmProjects:                 warmProjects,
		harvesterKubeconfig:          env.String("DEVBOX_HARVESTER_KUBECONFIG", ""),
		harvesterBaseImage:           env.String("DEVBOX_HARVESTER_BASE_IMAGE", ""),
		harvesterNamespace:           env.String("DEVBOX_HARVESTER_NAMESPACE", "default"),
		harvesterStorageClass:        env.String("DEVBOX_HARVESTER_STORAGE_CLASS", ""),
		harvesterNetworkAttachDef:    env.String("DEVBOX_HARVESTER_NETWORK_ATTACH_DEF", "default/lan10g"),
		harvesterDefaultVCPUs:        env.Int("DEVBOX_HARVESTER_DEFAULT_VCPUS", 2),
		harvesterDefaultMemMi:        env.Int("DEVBOX_HARVESTER_DEFAULT_MEM_MI", 4096),
		harvesterDefaultDiskGi:       env.Int("DEVBOX_HARVESTER_DEFAULT_DISK_GI", 20),
		// Empty default on purpose: NewHarvesterVMBackend applies the canonical
		// "agent" user (backend.defaultHarvesterSSHUser) when SSHUser is unset.
		// Slice 2d.5c moved the VM's cloud-init login + auth-file home to the
		// uid-1000 "agent" user for home-parity with the K8s spawn pod; the old
		// hardcoded "ubuntu" fallback here made devbox-launched harvester-vm
		// sandboxes SSH as the wrong user, so mounted agent auth files
		// (~/.codex/auth.json etc.) would not resolve. Let the backend be the
		// single source of truth, matching the operator/HUD spawn path.
		harvesterSSHUser: env.String("DEVBOX_HARVESTER_SSH_USER", ""),
		// Shared Go cache claim for K8s sandboxes (verified at manager init).
		goCachePVC: env.String("DEVBOX_K8S_GO_CACHE_PVC", ""),
		// Quality gate budgets: the checks that compile (go test, golangci-lint)
		// are CPU-bound, so operators size these to the sandbox's CPU rather
		// than living with the compiled-in defaults.
		gateCheckTimeoutSec: env.Int("DEVBOX_QUALITY_GATE_CHECK_TIMEOUT_SEC", 0),
		gateTestTimeoutSec:  env.Int("DEVBOX_QUALITY_GATE_TEST_TIMEOUT_SEC", 0),
	}
	mgr, err := newManager(ctx, logger, cfg)
	if err != nil && canonicalBackendType(backendType) == "docker" {
		if _, lookupErr := exec.LookPath("docker"); lookupErr != nil {
			logger.Warn("Docker backend is not configured; sandbox tool calls return NotConfigured", "missing_dependency", "docker CLI")
			mgr, err = newDegradedDockerManager(cfg, logger)
		}
	}
	if err != nil {
		_ = shutdownTracer(ctx)
		return nil, nil, fmt.Errorf("init manager: %w", err)
	}

	// Set startup time for uptime tracking.
	mgr.startedAt = time.Now()
	mgr.drain, _ = ctx.Value(drainContextKey{}).(*lifecycle.Drain)

	// Initialize metrics
	mgr.metrics = newMetrics()
	baseImageProbe := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "loom", Subsystem: "devbox", Name: "base_image_probe",
		Help: "Last startup registry probe result for each registered base image",
	}, []string{"language", "version", "outcome"})
	mgr.metrics.registry.MustRegister(baseImageProbe)

	probeClient, clientErr := backend.RegistryClient(time.Second)
	credentials := baseimage.RegistryCredentials{}
	if kb, ok := mgr.backends["k8s"].(*backend.K8sBackend); ok {
		credentials.Username, credentials.Password = kb.RegistryCredentials(ctx, mgr.cfg.registry)
	}
	if clientErr != nil {
		for _, item := range baseimage.Languages() {
			for _, outcome := range baseimage.ProbeOutcomes() {
				baseImageProbe.WithLabelValues(item.Language, item.Version, string(outcome)).Set(0)
			}
			baseImageProbe.WithLabelValues(item.Language, item.Version, string(baseimage.ProbeTransport)).Set(1)
			logger.Warn("devbox registered base image probe", "language", item.Language, "version", item.Version, "image", item.Image, "outcome", baseimage.ProbeTransport, "reason", clientErr)
		}
	} else {
		probeCtx, cancelProbe := context.WithTimeout(ctx, 5*time.Second)
		for _, result := range baseimage.ProbeRegistry(probeCtx, probeClient, env.String("DEVBOX_BASE_REGISTRY_URL", ""), credentials) {
			for _, outcome := range baseimage.ProbeOutcomes() {
				baseImageProbe.WithLabelValues(result.Language, result.Version, string(outcome)).Set(0)
			}
			baseImageProbe.WithLabelValues(result.Language, result.Version, string(result.Outcome)).Set(1)
			args := []any{"language", result.Language, "version", result.Version, "image", result.Image, "outcome", result.Outcome, "reason", result.Reason}
			if result.Outcome == baseimage.ProbeAvailable {
				logger.Info("devbox registered base image probe", args...)
			} else {
				logger.Warn("devbox registered base image probe", args...)
			}
		}
		cancelProbe()
	}

	// Initialize event emitter (optional — enabled via DEVBOX_HUD_ADDR)
	mgr.events = newEventEmitter(env.String("DEVBOX_HUD_ADDR", ""), logger)

	// Initialize async exec registry
	mgr.asyncExecs = newAsyncRegistry()
	go mgr.asyncExecs.cleanupLoop(ctx)

	registerTools(srv.Server, mgr, srv.Tracer)

	// Reconcile stale state entries (pods evicted, node reboots)
	mgr.reconcileState(ctx)

	// Start idle reaper
	go mgr.reapLoop(ctx)

	// Start warm pool if configured
	if len(mgr.cfg.warmProjects) > 0 {
		logger.Info("warm pool enabled", "projects", mgr.cfg.warmProjects)
		go mgr.warmPool(ctx)
	}

	cleanup := func(cleanupCtx context.Context) error {
		// The caller supplies a fresh cleanup context after canceling work.
		// Kubernetes shutdown bounds this context independently of the drain.
		mgr.shutdownAll(cleanupCtx)
		return shutdownTracer(cleanupCtx)
	}
	return srv, cleanup, nil
}

// unavailableDockerBackend keeps the MCP transport available when the host has
// no docker executable. Existing handlers convert these errors to MCP errors.
type unavailableDockerBackend struct{ backend.Backend }

func (unavailableDockerBackend) unavailable() error {
	return mcperror.NotConfigured("docker CLI", "install docker and ensure it is available on PATH, or set DEVBOX_BACKEND=k8s")
}
func (b unavailableDockerBackend) Build(context.Context, backend.BuildOpts) (*backend.BuildResult, error) {
	return nil, b.unavailable()
}
func (b unavailableDockerBackend) Start(context.Context, backend.StartOpts) (*backend.StartResult, error) {
	return nil, b.unavailable()
}
func (b unavailableDockerBackend) Exec(context.Context, backend.ExecOpts) (*backend.ExecResult, error) {
	return nil, b.unavailable()
}
func (b unavailableDockerBackend) Stop(context.Context, string) error { return b.unavailable() }
func (b unavailableDockerBackend) Status(context.Context, string) (*backend.StatusResult, error) {
	return nil, b.unavailable()
}
func (b unavailableDockerBackend) Health(context.Context) error         { return b.unavailable() }
func (b unavailableDockerBackend) Pause(context.Context, string) error  { return b.unavailable() }
func (b unavailableDockerBackend) Resume(context.Context, string) error { return b.unavailable() }
func (b unavailableDockerBackend) ReadFile(context.Context, string, string) ([]byte, error) {
	return nil, b.unavailable()
}
func (b unavailableDockerBackend) WriteFile(context.Context, string, string, []byte, string) error {
	return b.unavailable()
}
func (b unavailableDockerBackend) CleanupBuilds(context.Context, time.Duration) (int, error) {
	return 0, b.unavailable()
}

func newDegradedDockerManager(cfg managerConfig, logger *slog.Logger) (*manager, error) {
	store, err := state.NewStore(cfg.cacheDir)
	if err != nil {
		return nil, fmt.Errorf("init state store: %w", err)
	}
	b := unavailableDockerBackend{}
	return &manager{
		cfg: cfg, backend: b, backends: map[string]backend.Backend{"docker": b},
		defaultBackend: "docker", store: store, logger: logger,
		builds: newBuildTracker(), manifestTTL: cfg.remoteManifestTTL,
	}, nil
}

// builderImage returns the builder image, checking DEVBOX_BUILDER_IMAGE first,
// then falling back to the deprecated DEVBOX_KANIKO_IMAGE for backward compatibility.
func builderImage() string {
	if img := env.String("DEVBOX_BUILDER_IMAGE", ""); img != "" {
		return img
	}
	return env.String("DEVBOX_KANIKO_IMAGE", "")
}

// Admission happens at receive, before the SDK dispatches a handler; release
// happens only after the response has been written to the stdio pipe.
type drainContextKey struct{}
type drainTransport struct {
	mcp.Transport
	drain   *lifecycle.Drain
	mu      sync.Mutex
	pending map[string]int
}

func (t *drainTransport) Recv(ctx context.Context) (*mcp.Message, error) {
	for {
		msg, err := t.Transport.Recv(ctx)
		if err != nil {
			return nil, err
		}
		if msg.Method != "tools/call" {
			return msg, nil
		}
		// tools/call requires a request ID.
		if msg.ID == nil {
			continue
		}
		var params struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(msg.Params, &params)
		admitted := t.drain.Admit()
		// Polling is continuation of previously admitted async work.
		if !admitted && params.Name == "devbox_exec_poll" {
			admitted = t.drain.Retain()
		}
		if !admitted {
			resp := &mcp.Message{JSONRPC: mcp.JSONRPCVersion, ID: msg.ID, Error: &mcp.Error{Code: -32000, Message: "devbox is draining; retry on another server", Data: map[string]any{"retryable": true}}}
			if err := t.Transport.Send(ctx, resp); err != nil {
				return nil, err
			}
			continue
		}
		key, _ := json.Marshal(msg.ID)
		t.mu.Lock()
		t.pending[string(key)]++
		t.mu.Unlock()
		return msg, nil
	}
}
func (t *drainTransport) Send(ctx context.Context, msg *mcp.Message) error {
	err := t.Transport.Send(ctx, msg)
	if msg.Method == "" && msg.ID != nil {
		key, _ := json.Marshal(msg.ID)
		t.mu.Lock()
		if t.pending[string(key)] > 0 {
			t.pending[string(key)]--
			if t.pending[string(key)] == 0 {
				delete(t.pending, string(key))
			}
			t.drain.Release()
		}
		t.mu.Unlock()
	}
	return err
}

// buildTimeout returns a positive pod budget; invalid or disabled values use the default.
func buildTimeout() time.Duration {
	timeout := env.Duration("DEVBOX_K8S_BUILD_TIMEOUT", backend.DefaultBuildTimeout)
	if timeout <= 0 {
		return backend.DefaultBuildTimeout
	}
	return timeout
}
