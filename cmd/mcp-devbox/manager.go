package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/crb2nu/loom/internal/devbox/backend"
	"github.com/crb2nu/loom/internal/devbox/detect"
	"github.com/crb2nu/loom/internal/devbox/dockerfile"
	"github.com/crb2nu/loom/internal/devbox/state"
	"github.com/crb2nu/loom/pkg/env"
	"github.com/crb2nu/loom/pkg/lifecycle"
)

type managerConfig struct {
	workspaceRoot string
	cacheDir      string
	backendType   string
	registry      string
	imagePrefix   string
	maxTailLines  int
	idleTimeout   time.Duration
	defaultCPU    float64
	defaultMemMB  int

	// millsIdleTimeout (DEVBOX_MILLS_IDLE_TIMEOUT) bounds how long a Mills per-run
	// sandbox orphaned by a gate that died mid-call keeps holding devbox quota;
	// the gate releases it itself on every verdict. Zero falls back to idleTimeout.
	millsIdleTimeout time.Duration

	// K8s-specific
	kubeconfig                   string
	k8sNamespace                 string
	storageClass                 string
	k8sWorkspacePVC              string
	k8sImagePullSecret           string
	builderImage                 string
	gitCloneImage                string
	buildCPURequest              string
	buildCPULimit                string
	buildMemoryRequest           string
	buildMemoryLimit             string
	buildEphemeralStorageRequest string
	buildEphemeralStorageLimit   string
	buildAvoidNodes              string
	maxConcurrentBuilds          int
	buildTimeout                 time.Duration
	gitCloneMemoryRequest        string
	gitCloneMemoryLimit          string

	// goCachePVC names a shared RWX claim mounted into every K8s sandbox as
	// the Go build + module cache (DEVBOX_K8S_GO_CACHE_PVC). Empty disables
	// the mount. newManager clears it when the claim does not exist so a
	// missing claim degrades to cold caches instead of unschedulable pods.
	goCachePVC string

	// Quality gate per-check budgets in seconds; zero means the defaults
	// (300 for fmt/lint style checks, 900 for the test commands).
	gateCheckTimeoutSec int
	gateTestTimeoutSec  int

	// NFS cache flush before each exec (default true for K8s backend)
	nfsFlush bool

	// Git-clone mode: populate workspace via git clone instead of NFS PVC
	gitBaseURL string // base git URL (e.g., "https://gitlab.blevins.dev/homelab")
	gitSecret  string // K8s secret name with git token (key: "token")
	// gitToken overrides the secret-sourced token for git host API calls
	// (DEVBOX_GIT_TOKEN); remoteManifestTTL bounds manifest cache reuse.
	gitToken          string
	remoteManifestTTL time.Duration

	// Tar-pipe sync: stream local files into pods via SPDY exec
	syncMode     string   // "tar-pipe", "git-clone", "nfs"
	syncExcludes []string // additional exclude patterns
	maxSyncSize  int64    // max uncompressed tar bytes

	// Warm pool: pre-provision pods for these projects on startup
	warmProjects []string

	// Harvester-VM-specific (used when backendType == "harvester-vm").
	harvesterKubeconfig       string
	harvesterBaseImage        string
	harvesterNamespace        string
	harvesterStorageClass     string
	harvesterNetworkAttachDef string
	harvesterDefaultVCPUs     int
	harvesterDefaultMemMi     int
	harvesterDefaultDiskGi    int
	harvesterSSHUser          string
}

type manager struct {
	drain          *lifecycle.Drain
	cfg            managerConfig
	backend        backend.Backend
	backends       map[string]backend.Backend
	defaultBackend string
	store          *state.Store
	logger         *slog.Logger
	metrics        *metrics
	events         *eventEmitter

	// Async exec tracking
	asyncExecs *asyncRegistry

	// Async image-build tracking. Cold sandbox builds can take several
	// minutes; the tracker lets ensureRunning return "build in progress"
	// immediately instead of blocking the caller until the MCP/proxy
	// call times out.
	builds *buildTracker

	// Remote manifest fingerprinting (git-clone mode): manifests fetches the
	// dependency manifests the hub has no local copy of, so a sandbox image
	// is keyed by real content instead of the empty-input hash. nil disables
	// hydration (fingerprints degrade to the generic image as before).
	manifests   manifestSource
	manifestTTL time.Duration
	gitTokenMu  sync.Mutex
	gitTokenVal string
	gitTokenAt  time.Time

	// buildWg tracks async build goroutines. Shutdown does NOT wait on it:
	// the underlying build runs detached cluster-side (the K8s backend
	// re-derives its own background context), so gating daemon shutdown on a
	// multi-minute build would stall restarts. The wg exists for test
	// synchronization and future bounded draining.
	buildWg sync.WaitGroup

	// Per-project lifecycle lock prevents concurrent ensureRunning races (TOCTOU).
	projectMu           sync.Map // map[string]*sync.Mutex
	gateCleanupFailures sync.Map // map[string]error; cleared only by confirmed stop
	gateCancels         sync.Map // map[string]context.CancelFunc

	// Active exec counter per project — reaper skips projects with active execs.
	activeExecs sync.Map // map[string]*atomic.Int32

	// Cumulative counters for HUD summary.
	startedAt   time.Time
	totalExecs  atomic.Int64
	totalBuilds atomic.Int64

	// asyncWg tracks running async goroutines for graceful shutdown.
	asyncWg sync.WaitGroup
}

// stringMapArg normalizes a string map received either directly from an
// in-process caller or through JSON decoding at the MCP boundary.
func stringMapArg(value any) map[string]string {
	result := make(map[string]string)
	switch values := value.(type) {
	case map[string]string:
		for key, val := range values {
			result[key] = val
		}
	case map[string]any:
		for key, val := range values {
			if text, ok := val.(string); ok {
				result[key] = text
			}
		}
	}
	return result
}

// stringSliceArg normalizes a string slice received either directly from an
// in-process caller or through JSON decoding at the MCP boundary.
func stringSliceArg(value any) []string {
	switch values := value.(type) {
	case []string:
		return append([]string(nil), values...)
	case []any:
		result := make([]string, 0, len(values))
		for _, value := range values {
			if text, ok := value.(string); ok {
				result = append(result, text)
			}
		}
		return result
	default:
		return nil
	}
}

// backendHealthTimeout bounds startup probing so MCP init is never blocked by a hung runtime.
var backendHealthTimeout = 3 * time.Second

func checkBackendHealth(ctx context.Context, logger *slog.Logger, health func(context.Context) error) {
	healthCtx, cancel := context.WithTimeout(ctx, backendHealthTimeout)
	defer cancel()

	if err := health(healthCtx); err != nil {
		logger.Warn("backend health check failed", "error", err)
	}
}

func canonicalBackendType(backendType string) string {
	switch strings.TrimSpace(strings.ToLower(backendType)) {
	case "", "docker":
		return "docker"
	case "k8s", "kubernetes":
		return "k8s"
	case "harvester-vm":
		return "harvester-vm"
	default:
		return backendType
	}
}

func isK8sBackendType(backendType string) bool {
	return canonicalBackendType(backendType) == "k8s"
}

func (m *manager) backendFor(name string) backend.Backend {
	if m == nil {
		return nil
	}
	key := canonicalBackendType(name)
	if strings.TrimSpace(name) == "" {
		key = m.defaultBackend
	}
	if m.backends != nil {
		if b := m.backends[key]; b != nil {
			return b
		}
	}
	return m.backend
}

// projectLock returns (or creates) a per-project mutex for lifecycle serialization.
func (m *manager) projectLock(name string) *sync.Mutex {
	v, _ := m.projectMu.LoadOrStore(name, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// incActiveExecs increments the active exec counter for a project.
func (m *manager) incActiveExecs(name string) {
	v, _ := m.activeExecs.LoadOrStore(name, &atomic.Int32{})
	v.(*atomic.Int32).Add(1)
}

// decActiveExecs decrements the active exec counter for a project.
func (m *manager) decActiveExecs(name string) {
	if v, ok := m.activeExecs.Load(name); ok {
		v.(*atomic.Int32).Add(-1)
	}
}

// hasActiveExecs returns true if a project has exec calls in flight.
func (m *manager) hasActiveExecs(name string) bool {
	if v, ok := m.activeExecs.Load(name); ok {
		return v.(*atomic.Int32).Load() > 0
	}
	return false
}

// sanitizeContainerName returns a DNS-1123-safe name fragment that is also safe
// for Docker container names and image repository components.
func sanitizeContainerName(name string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteRune('-')
			lastDash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "sandbox"
	}
	return out
}

// validateMountPath ensures a host path is under an allowed directory.
// Resolves symlinks before validation so symlinked paths are correctly matched.
func (m *manager) validateMountPath(hostPath string) error {
	resolved, err := filepath.EvalSymlinks(hostPath)
	if err != nil {
		// Path doesn't exist yet — fall back to Abs only.
		resolved = hostPath
	}
	abs, err := filepath.Abs(resolved)
	if err != nil {
		return fmt.Errorf("invalid mount path %q: %w", hostPath, err)
	}
	home, _ := os.UserHomeDir()
	allowed := []string{
		m.cfg.workspaceRoot,
		filepath.Join(home, ".cache"),
		filepath.Join(home, ".local"),
	}
	for _, prefix := range allowed {
		if strings.HasPrefix(abs, prefix+string(filepath.Separator)) || abs == prefix {
			return nil
		}
	}
	return fmt.Errorf("mount path %q not under allowed directories (%s)", hostPath, strings.Join(allowed, ", "))
}

func newManager(ctx context.Context, logger *slog.Logger, cfg managerConfig) (*manager, error) {
	backends, defaultBackend, err := initBackends(cfg, logger)
	if err != nil {
		return nil, err
	}
	cfg.backendType = defaultBackend
	b := backends[defaultBackend]

	for name, be := range backends {
		checkBackendHealth(ctx, logger.With("backend", name), be.Health)
	}

	store, err := state.NewStore(cfg.cacheDir)
	if err != nil {
		return nil, fmt.Errorf("init state store: %w", err)
	}

	m := &manager{
		cfg:            cfg,
		backend:        b,
		backends:       backends,
		defaultBackend: defaultBackend,
		store:          store,
		logger:         logger,
		builds:         newBuildTracker(),
		manifestTTL:    cfg.remoteManifestTTL,
	}
	// The shared Go cache is K8s-only and must exist before a pod references
	// it: a missing claim makes every sandbox unschedulable, so verify once
	// here and fall back to cold caches with a WARN.
	if cfg.goCachePVC != "" {
		kb, ok := backends["k8s"].(*backend.K8sBackend)
		switch {
		case !ok:
			logger.Warn("sandbox Go cache claim configured without a K8s backend; ignoring", "claim", cfg.goCachePVC)
			m.cfg.goCachePVC = ""
		default:
			present, err := kb.HasClaim(ctx, cfg.goCachePVC)
			if err != nil || !present {
				logger.Warn("sandbox Go cache claim unavailable; sandboxes run with cold caches", "claim", cfg.goCachePVC, "namespace", cfg.k8sNamespace, "error", err)
				m.cfg.goCachePVC = ""
			} else {
				logger.Info("sandbox Go cache enabled", "claim", cfg.goCachePVC, "mount", sandboxGoCacheMountPath)
			}
		}
	}
	// git-clone mode has no local checkout to fingerprint; fetch the
	// dependency manifests from the git host instead (see
	// remote_fingerprint.go). DEVBOX_REMOTE_MANIFESTS=0 restores the
	// generic-image behavior.
	if cfg.syncMode == "git-clone" && strings.TrimSpace(cfg.gitBaseURL) != "" && env.Bool("DEVBOX_REMOTE_MANIFESTS", true) {
		m.manifests = newGitLabManifestSource(cfg.gitBaseURL, m.gitToken)
		logger.Info("remote manifest fingerprinting enabled", "git_base_url", cfg.gitBaseURL, "ttl", m.manifestTTL)
	}
	return m, nil
}

func initBackends(cfg managerConfig, logger *slog.Logger) (map[string]backend.Backend, string, error) {
	defaultBackend := canonicalBackendType(cfg.backendType)
	switch defaultBackend {
	case "docker":
		db, err := backend.NewDockerBackend()
		if err != nil {
			return nil, "", err
		}
		return map[string]backend.Backend{"docker": db}, "docker", nil
	case "k8s":
		kb, err := newK8sBackend(cfg)
		if err != nil {
			return nil, "", err
		}
		return map[string]backend.Backend{"k8s": kb}, "k8s", nil
	case "harvester-vm":
		backends := make(map[string]backend.Backend, 2)
		var resolver backend.SecretResolver
		if kb, err := newK8sBackend(cfg); err != nil {
			if logger != nil {
				logger.Warn("k8s backend unavailable for harvester-vm secret resolver", "error", err)
			}
		} else {
			backends["k8s"] = kb
			resolver = kb
		}

		hb, err := newHarvesterVMBackend(cfg, resolver)
		if err != nil {
			return nil, "", err
		}
		backends["harvester-vm"] = hb
		return backends, "harvester-vm", nil
	default:
		return nil, "", fmt.Errorf("unsupported backend: %s (use 'docker', 'k8s', or 'harvester-vm')", cfg.backendType)
	}
}

func newK8sBackend(cfg managerConfig) (*backend.K8sBackend, error) {
	return backend.NewK8sBackend(backend.K8sBackendConfig{
		Kubeconfig:                   cfg.kubeconfig,
		Namespace:                    cfg.k8sNamespace,
		Registry:                     cfg.registry,
		WorkspacePVC:                 cfg.k8sWorkspacePVC,
		ImagePullSecret:              cfg.k8sImagePullSecret,
		WorkspaceRoot:                cfg.workspaceRoot,
		BuilderImage:                 cfg.builderImage,
		GitCloneImage:                cfg.gitCloneImage,
		NFSFlush:                     cfg.nfsFlush,
		GitBaseURL:                   cfg.gitBaseURL,
		GitSecret:                    cfg.gitSecret,
		BuildCPURequest:              cfg.buildCPURequest,
		BuildCPULimit:                cfg.buildCPULimit,
		BuildMemoryRequest:           cfg.buildMemoryRequest,
		BuildMemoryLimit:             cfg.buildMemoryLimit,
		BuildEphemeralStorageRequest: cfg.buildEphemeralStorageRequest,
		BuildEphemeralStorageLimit:   cfg.buildEphemeralStorageLimit,
		BuildAvoidNodes:              cfg.buildAvoidNodes,
		MaxConcurrentBuilds:          cfg.maxConcurrentBuilds,
		BuildTimeout:                 cfg.buildTimeout,
		GitCloneMemoryRequest:        cfg.gitCloneMemoryRequest,
		GitCloneMemoryLimit:          cfg.gitCloneMemoryLimit,
		SyncMode:                     cfg.syncMode,
		SyncExcludes:                 cfg.syncExcludes,
		MaxSyncSize:                  cfg.maxSyncSize,
	})
}

func newHarvesterVMBackend(cfg managerConfig, resolver backend.SecretResolver) (*backend.HarvesterVMBackend, error) {
	return backend.NewHarvesterVMBackend(backend.HarvesterVMBackendConfig{
		KubeconfigPath:       cfg.harvesterKubeconfig,
		Namespace:            cfg.harvesterNamespace,
		BaseImageName:        cfg.harvesterBaseImage,
		StorageClassName:     cfg.harvesterStorageClass,
		NetworkAttachmentDef: cfg.harvesterNetworkAttachDef,
		DefaultVCPUs:         cfg.harvesterDefaultVCPUs,
		DefaultMemMi:         cfg.harvesterDefaultMemMi,
		DefaultDiskGi:        cfg.harvesterDefaultDiskGi,
		SSHUser:              cfg.harvesterSSHUser,
		SecretResolver:       resolver,
	})
}

// resolveProject finds the absolute path for a project name.
// Searches: workspace/<name>, workspace/services/<name>, workspace/libs/<name>, workspace/platform/<name>
func (m *manager) resolveProject(project string) (string, string, error) {
	// If it's already an absolute path
	if filepath.IsAbs(project) {
		if info, err := os.Stat(project); err == nil && info.IsDir() {
			return project, filepath.Base(project), nil
		}
		return "", "", fmt.Errorf("project directory not found: %s", project)
	}

	candidates := []string{
		filepath.Join(m.cfg.workspaceRoot, project),
		filepath.Join(m.cfg.workspaceRoot, "services", project),
		filepath.Join(m.cfg.workspaceRoot, "libs", project),
		filepath.Join(m.cfg.workspaceRoot, "platform", project),
	}

	for _, path := range candidates {
		if info, err := os.Stat(path); err == nil && info.IsDir() {
			name := filepath.Base(path)
			return path, name, nil
		}
	}

	// git-clone fallback: the sandbox's git-clone init container is the source
	// of truth, so a repo that is not staged in the local workspace need not be
	// a hard failure. The on-disk copy is used only to fingerprint the sandbox
	// toolchain image, and Fingerprint + generateSandboxDockerfile already
	// degrade a missing/empty dir to the generic git-clone image (Go + Node +
	// Python). Resolving lexically lets the tests stage run against any
	// services-group repo without pre-staging it (mirrors the spawn
	// orchestrator, loom-core !941). For tar-pipe/nfs the local copy IS the
	// source, so keep the hard failure there.
	if m.cfg.syncMode == "git-clone" {
		if dir, name, ok := m.lexicalProjectDir(project); ok {
			if m.logger != nil {
				m.logger.Warn("project not staged in local workspace; using git-clone fallback (generic sandbox image; the sandbox clones the repo)",
					"project", project, "resolved_dir", dir, "workspace", m.cfg.workspaceRoot)
			}
			return dir, name, nil
		}
	}

	return "", "", fmt.Errorf("project '%s' not found under %s", project, m.cfg.workspaceRoot)
}

// lexicalProjectDir derives an on-disk project path from a name WITHOUT
// requiring it to exist, for the git-clone fallback in resolveProject. A bare
// name resolves under the "services" bucket (matching the git-clone base URL
// group, so `flexdeck` clones from services/flexdeck.git); a bucket-qualified
// name ("services/foo", "libs/bar") is used verbatim. Absolute paths and paths
// that escape the workspace root are rejected (ok=false).
func (m *manager) lexicalProjectDir(project string) (dir, name string, ok bool) {
	p := strings.TrimSpace(project)
	if p == "" || filepath.IsAbs(p) {
		return "", "", false
	}
	p = filepath.Clean(p)
	if p == "." || strings.HasPrefix(p, "..") {
		return "", "", false
	}
	if !strings.ContainsRune(p, filepath.Separator) {
		p = filepath.Join("services", p)
	}
	full := filepath.Join(m.cfg.workspaceRoot, p)
	return full, filepath.Base(full), true
}

// imageTag returns the Docker image tag for a project fingerprint.
func (m *manager) imageTag(projectName, hash string) string {
	return fmt.Sprintf("%s/%s:%s", m.cfg.imagePrefix, sanitizeContainerName(projectName), hash[:7])
}

// containerName returns the Docker container/pod name for a project.
// When agentID is provided, the pod name includes a truncated agent suffix
// for per-agent isolation: devbox-<project>-<agent>.
func (m *manager) containerName(projectName, agentID string) string {
	base := "devbox-" + sanitizeContainerName(projectName)
	if agentID != "" {
		return base + "-" + agentSandboxSuffix(agentID)
	}
	return base
}

// agentSuffixBudget caps the agent part of a sandbox name so the whole name
// stays inside the K8s name budget alongside the project.
const agentSuffixBudget = 12

// agentSandboxSuffix renders the agent part of a sandbox name. Short ids stay
// readable (claude-code, codex). A longer id becomes its first characters
// plus a five-hex digest of the WHOLE id: plain truncation collapsed every
// Mills run (loom-mills-operator-<run hash>) onto one pod name, so four
// concurrent runs shared one sandbox and each "restart" of it killed the
// other runs' quality gates mid-flight (2026-09-13). The digest is
// deterministic, so a restarted devbox server maps the same agent to the
// same pod.
func agentSandboxSuffix(agentID string) string {
	id := sanitizeContainerName(agentID)
	if len(id) <= agentSuffixBudget {
		return id
	}
	sum := sha256.Sum256([]byte(agentID))
	digest := hex.EncodeToString(sum[:])[:5]
	prefix := strings.Trim(id[:agentSuffixBudget-len(digest)-1], "-")
	return prefix + "-" + digest
}

// sandboxGoCacheMountPath is where the shared Go cache claim surfaces inside
// a sandbox pod. Same layout as the HUD spawn fleet's /gocache (go-build,
// gomod) but a SEPARATE claim: sandboxes run as root while spawn pods run as
// uid 1000 under fsGroup 1000, and root-created cache directories (0755)
// would be unwritable for the spawns.
const sandboxGoCacheMountPath = "/gocache"

// sandboxGoCache returns the pod env and cache mounts for a sandbox. With a
// claim it points GOCACHE, GOMODCACHE and GOLANGCI_LINT_CACHE at the shared
// mount so a quality gate no longer recompiles the world in every fresh pod
// (2026-09-13: 1.7 GB of caches lived on the pod's ephemeral disk and every
// replaced pod rebuilt them; the 900s test check died at 887s). Without a
// claim the input env is returned untouched and no mount is added, so the
// legacy pod spec stays byte-identical. The input map is never mutated: it
// belongs to the cached fingerprint.
func sandboxGoCache(env map[string]string, claim string) (map[string]string, []backend.CachePVCMount) {
	if claim == "" {
		return env, nil
	}
	out := make(map[string]string, len(env)+4)
	for k, v := range env {
		out[k] = v
	}
	out["GOCACHE"] = sandboxGoCacheMountPath + "/go-build"
	out["GOMODCACHE"] = sandboxGoCacheMountPath + "/gomod"
	out["GOLANGCI_LINT_CACHE"] = sandboxGoCacheMountPath + "/golangci-lint"
	// A module index built during concurrent extraction can persist file-open
	// errors on the shared cache even after the files become readable.
	settings := make([]string, 0)
	for _, setting := range strings.Split(out["GODEBUG"], ",") {
		if setting != "" && !strings.HasPrefix(setting, "goindex=") {
			settings = append(settings, setting)
		}
	}
	out["GODEBUG"] = strings.Join(append(settings, "goindex=0"), ",")
	return out, []backend.CachePVCMount{{ClaimName: claim, MountPath: sandboxGoCacheMountPath}}
}

// storeKey returns the state store key for a project+agent combination.
// When agentID is set, returns "project/agentID" for per-agent state isolation.
func storeKey(projectName, agentID string) string {
	if agentID != "" {
		return projectName + "/" + agentID
	}
	return projectName
}

// ensureRunning ensures a sandbox is built and running for a project.
// Returns the container ID. When agentID is provided, each agent gets
// its own isolated pod for the project.
func (m *manager) ensureRunning(ctx context.Context, projectDir, projectName, agentID string) (string, error) {
	// Fingerprint the project (git-clone mode hydrates from the git host).
	fp, err := m.fingerprintProject(ctx, projectDir)
	if err != nil {
		return "", fmt.Errorf("fingerprint: %w", err)
	}

	key := storeKey(projectName, agentID)
	entry := m.store.Get(key)
	tag := m.imageTag(projectName, fp.Hash)
	containerID := m.containerName(projectName, agentID)

	// Fast path: container exists with matching hash
	if entry != nil && entry.FingerprintHash == fp.Hash && entry.Status == "running" {
		status, err := m.backend.Status(ctx, containerID)
		if err == nil && status.Running {
			return containerID, nil
		}
	}

	// Warm resume: paused container with matching hash — unpause instead of rebuild
	if entry != nil && entry.FingerprintHash == fp.Hash && entry.Status == "paused" {
		if err := m.backend.Resume(ctx, containerID); err == nil {
			entry.Status = "running"
			entry.LastUsed = time.Now()
			_ = m.store.Set(key, entry)
			m.logger.Info("resumed paused sandbox", "project", projectName, "agent", agentID)
			return containerID, nil
		}
		// Resume failed — fall through to rebuild
		m.logger.Warn("resume failed, rebuilding", "project", projectName, "agent", agentID)
	}

	// Stopped container with matching hash — try to restart without rebuild.
	// For K8s backend, Start() reuses existing running pods or creates new ones.
	if entry != nil && entry.FingerprintHash == fp.Hash && entry.Status == "stopped" {
		m.logger.Info("restarting stopped sandbox (hash match)", "project", projectName, "agent", agentID)
		// Skip build, go straight to Start below
	} else if entry == nil || entry.FingerprintHash != fp.Hash {
		// Stale or missing: image build required (cold start or fingerprint
		// changed). Cold builds run `go mod download`, apk installs, and an
		// image push that routinely take several minutes — far longer than
		// the MCP/proxy call timeout. For the K8s backend (where the build
		// pod is already detached from the request context) run the build in
		// a background goroutine and return buildInProgressError so the
		// caller gets an immediate, actionable "retry shortly" response
		// instead of a hung call. Synchronous backends (docker, harvester-vm)
		// keep the original inline build — their builds are fast or no-ops.
		if m.asyncBuildEnabled() {
			if id, err := m.ensureAsyncBuild(projectDir, projectName, tag, fp); err != nil || id == "" {
				return "", err
			}
			// Build finished successfully on a prior call — fall through to Start.
		} else {
			m.logger.Info("building sandbox image", "project", projectName, "hash", fp.Hash[:7])

			dockerfileContent, err := m.generateSandboxDockerfile(fp)
			if err != nil {
				return "", fmt.Errorf("generate dockerfile: %w", err)
			}

			result, err := m.backend.Build(ctx, backend.BuildOpts{
				Tag:             tag,
				Dockerfile:      dockerfileContent,
				ContextDir:      projectDir,
				DetectBaseImage: m.detectBaseImageAfterClone(fp),
			})
			if err != nil {
				return "", fmt.Errorf("build image: %w", err)
			}
			m.recordBaseImageFallback(projectName, result)
		}
	}

	// Stop existing container if running
	_ = m.backend.Stop(ctx, containerID)

	// Start new container
	mounts := m.buildMounts(projectDir)

	memMB := m.sandboxMemoryMB(fp)
	cpu := m.cfg.defaultCPU
	network := true
	if fp.Overrides != nil {
		if fp.Overrides.Limits != nil {
			if fp.Overrides.Limits.CPU > 0 {
				cpu = fp.Overrides.Limits.CPU
			}
		}
		if fp.Overrides.Network != nil {
			network = *fp.Overrides.Network
		}
		for _, extra := range fp.Overrides.Mounts {
			if err := m.validateMountPath(extra.Host); err != nil {
				return "", fmt.Errorf("invalid override mount: %w", err)
			}
			mounts = append(mounts, backend.Mount{
				Host:      extra.Host,
				Container: extra.Container,
				ReadOnly:  extra.ReadOnly,
			})
		}
	}

	workDir := m.projectWorkDir(projectDir)
	startEnv, cachePVCs := sandboxGoCache(fp.EnvVars, m.cfg.goCachePVC)
	m.logger.Info("starting sandbox", "project", projectName, "agent", agentID, "image", tag, "workdir", workDir, "go_cache_claim", m.cfg.goCachePVC)
	result, err := m.backend.Start(ctx, backend.StartOpts{
		Name:      containerID,
		ImageTag:  tag,
		WorkDir:   workDir,
		Mounts:    mounts,
		Env:       startEnv,
		CachePVCs: cachePVCs,
		MemoryMB:  memMB,
		CPUs:      cpu,
		Network:   network,
		AgentID:   agentID,
	})
	if err != nil {
		return "", fmt.Errorf("start container: %w", err)
	}

	// Tar-pipe sync: stream local source into the pod after it starts.
	if err := m.syncIfNeeded(ctx, containerID, projectDir); err != nil {
		return "", fmt.Errorf("sync workspace: %w", err)
	}

	// Persist state
	now := time.Now()
	if err := m.store.Set(key, &state.Entry{
		ProjectDir:      projectDir,
		ContainerID:     result.ContainerID,
		ImageTag:        tag,
		FingerprintHash: fp.Hash,
		Backend:         m.cfg.backendType,
		Status:          "running",
		LastUsed:        now,
		CreatedAt:       now,
	}); err != nil {
		m.logger.Warn("failed to persist state", "error", err)
	}

	return containerID, nil
}

// asyncBuildEnabled reports whether cold image builds should run in a detached
// goroutine (returning "build in progress" to the caller) rather than blocking.
// Only the K8s backend benefits: its builds are minutes-long and already
// detach the build pod from the request context. Docker and harvester-vm keep
// the synchronous path (fast local builds / no-op builds respectively).
// Set LOOM_DEVBOX_ASYNC_BUILD=0 to force the legacy synchronous behavior.
func (m *manager) asyncBuildEnabled() bool {
	if !env.Bool("LOOM_DEVBOX_ASYNC_BUILD", true) {
		return false
	}
	return m.cfg.backendType == "k8s" || m.cfg.backendType == "kubernetes"
}

// ensureAsyncBuild drives the asynchronous build state machine for tag.
//
//   - Build still running (or just kicked off): returns ("", buildInProgressError).
//   - Build finished with an error: returns ("", wrapped error). The tracker
//     retains it briefly so concurrent callers share the verdict, then permits a retry.
//   - Build finished successfully: retains the immutable-tag result and returns
//     ("ready", nil) so every caller falls through to Start.
func (m *manager) ensureAsyncBuild(projectDir, projectName, tag string, fp *detect.EnvFingerprint) (string, error) {
	if bi := m.builds.lookup(tag); bi != nil && bi.done {
		if bi.err != nil {
			return "", fmt.Errorf("sandbox image build failed: %w", bi.err)
		}
		m.logger.Info("sandbox image build complete", "project", projectName, "tag", tag)
		return "ready", nil
	}

	dockerfileContent, err := m.generateSandboxDockerfile(fp)
	if err != nil {
		return "", fmt.Errorf("generate dockerfile: %w", err)
	}

	bi, started := m.builds.startOrJoin(tag, &m.buildWg, func() error {
		result, berr := m.backend.Build(context.Background(), backend.BuildOpts{
			Tag:             tag,
			Dockerfile:      dockerfileContent,
			ContextDir:      projectDir,
			PreferExisting:  true,
			DetectBaseImage: m.detectBaseImageAfterClone(fp),
		})
		if berr != nil {
			m.logger.Warn("sandbox image build failed", "project", projectName, "tag", tag, "error", berr)
			return berr
		}
		m.recordBaseImageFallback(projectName, result)
		m.totalBuilds.Add(1)
		return nil
	})
	if started {
		m.logger.Info("building sandbox image (async)", "project", projectName, "hash", fp.Hash[:7], "tag", tag)
	}
	return "", &buildInProgressError{tag: tag, project: projectName, elapsed: time.Since(bi.startedAt), started: started}
}

func (m *manager) detectBaseImageAfterClone(fp *detect.EnvFingerprint) bool {
	return m.cfg.syncMode == "git-clone" && fp != nil && len(fp.Languages) == 0
}

func (m *manager) recordBaseImageFallback(project string, result *backend.BuildResult) {
	if result == nil || result.BaseImageFallback == nil {
		return
	}
	f := result.BaseImageFallback
	if m.metrics != nil {
		m.metrics.baseImageFallbacks.WithLabelValues(project, f.Language, f.Version, f.Reason).Inc()
	}
	if m.events != nil {
		m.events.EmitBaseImageFallback(context.Background(), project, f.Language, f.Version, f.Reason)
	}
	if m.logger != nil {
		m.logger.Warn("devbox base image registry fallback", "project", project, "language", f.Language, "version", f.Version, "reason", f.Reason)
	}
}

func (m *manager) generateSandboxDockerfile(fp *detect.EnvFingerprint) ([]byte, error) {
	df, err := dockerfile.Generate(fp)
	if err == nil {
		return df, nil
	}
	if fp != nil && len(fp.Languages) == 0 && m.cfg.syncMode == "git-clone" {
		if m.logger != nil {
			m.logger.Warn("using generic git-clone sandbox image; local project source is unavailable for language detection",
				"project", fp.ProjectName,
				"dir", fp.ProjectDir,
				"error", err,
			)
		}
		return genericGitCloneDockerfile(), nil
	}
	return nil, err
}

func genericGitCloneDockerfile() []byte {
	return []byte(`# Auto-generated by mcp-devbox for git-clone source hydration.
ARG DEVBOX_BASE_IMAGE=registry.harbor.lan/mcp/devbox-base/go:1.25
FROM ${DEVBOX_BASE_IMAGE}
ENV PATH="/usr/local/go/bin:${PATH}"
RUN apk add --no-cache nodejs npm python3 py3-pip
WORKDIR /workspace
CMD ["sleep", "infinity"]
`)
}

// sandboxMemoryMB is shared by sandbox creation and gate process budgeting.
func (m *manager) sandboxMemoryMB(fp *detect.EnvFingerprint) int {
	if fp != nil && fp.Overrides != nil && fp.Overrides.Limits != nil && fp.Overrides.Limits.MemoryMB > 0 {
		return fp.Overrides.Limits.MemoryMB
	}
	return m.cfg.defaultMemMB
}

// projectWorkDir returns the working directory inside the container for a project.
// If the project is under workspaceRoot, we mount the root and use a subdirectory.
// Otherwise, we mount the project directly at /workspace.
func (m *manager) projectWorkDir(projectDir string) string {
	rel, err := filepath.Rel(m.cfg.workspaceRoot, projectDir)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "/workspace"
	}
	return filepath.Join("/workspace", rel)
}

// syncIfNeeded performs tar-pipe workspace sync if configured.
// It discovers the project's sibling deps (Go replace directives) and streams
// all source files into the pod via SPDY exec.
func (m *manager) syncIfNeeded(ctx context.Context, containerID, projectDir string) error {
	if m.cfg.syncMode != "tar-pipe" {
		return nil
	}

	kb, ok := m.backend.(*backend.K8sBackend)
	if !ok {
		return nil // tar-pipe only works with K8s backend
	}

	dirs, err := backend.DiscoverDeps(projectDir, m.cfg.workspaceRoot)
	if err != nil {
		return fmt.Errorf("discover deps: %w", err)
	}

	m.logger.Info("syncing workspace", "project", filepath.Base(projectDir),
		"dirs", len(dirs), "container", containerID)

	return kb.SyncWorkspace(ctx, containerID, dirs, m.cfg.syncExcludes, m.cfg.maxSyncSize) //nolint:staticcheck // intentionally deprecated, migrating to sandbox.Controller
}

// buildMounts creates the standard bind mounts for a sandbox.
// For K8s backend, returns empty slice — NFS PVC handles workspace mounting.
func (m *manager) buildMounts(projectDir string) []backend.Mount {
	// K8s backend uses NFS PVC for workspace; host mounts are not available on cluster nodes.
	if isK8sBackendType(m.cfg.backendType) {
		return nil
	}

	home, _ := os.UserHomeDir()

	// Mount workspace root so sibling projects (Go replace directives) are accessible
	mountHost := m.cfg.workspaceRoot
	rel, err := filepath.Rel(m.cfg.workspaceRoot, projectDir)
	if err != nil || strings.HasPrefix(rel, "..") {
		// Project is outside workspace root — mount project directly
		mountHost = projectDir
	}

	mounts := []backend.Mount{
		{Host: mountHost, Container: "/workspace"},
	}

	// Shared caches (only mount if they exist on host)
	caches := []struct {
		host      string
		container string
	}{
		{filepath.Join(home, ".cache", "go", "mod"), "/go/pkg/mod"},
		{filepath.Join(home, ".cache", "pip"), "/root/.cache/pip"},
		{filepath.Join(home, ".local", "share", "pnpm", "store"), "/root/.local/share/pnpm/store"},
	}

	for _, c := range caches {
		if info, err := os.Stat(c.host); err == nil && info.IsDir() {
			mounts = append(mounts, backend.Mount{Host: c.host, Container: c.container})
		}
	}

	return mounts
}

// reapLoop periodically stops idle containers, prunes stale state, and cleans up build pods.
func (m *manager) reapLoop(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.reapIdle(ctx)
			m.reapCompletedBuilds(ctx)
			m.store.PruneOlderThan(7 * 24 * time.Hour)
		}
	}
}

// reapCompletedBuilds cleans up completed build pods and orphaned ConfigMaps.
func (m *manager) reapCompletedBuilds(ctx context.Context) {
	cleaned, err := m.backend.CleanupBuilds(ctx, 1*time.Hour)
	if err != nil {
		m.logger.Warn("build cleanup failed", "error", err)
		return
	}
	if cleaned > 0 {
		m.logger.Info("cleaned up build resources", "count", cleaned)
	}
}

// isK8sBackend returns true if the backend is Kubernetes-based.
func (m *manager) isK8sBackend() bool {
	return isK8sBackendType(m.cfg.backendType)
}

// isWarmProject returns true if the project is in the warm pool list.
func (m *manager) isWarmProject(projectName string) bool {
	for _, p := range m.cfg.warmProjects {
		if p == projectName {
			return true
		}
	}
	return false
}

// warmPool pre-provisions pods for configured warm projects.
// Runs on startup and re-warms every 30 minutes.
func (m *manager) warmPool(ctx context.Context) {
	m.warmOnce(ctx)

	ticker := time.NewTicker(30 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.warmOnce(ctx)
		}
	}
}

func (m *manager) warmOnce(ctx context.Context) {
	for _, project := range m.cfg.warmProjects {
		projectDir, projectName, err := m.resolveProject(project)
		if err != nil {
			m.logger.Warn("warm pool: project not found", "project", project, "error", err)
			continue
		}

		key := storeKey(projectName, "")
		mu := m.projectLock(key)
		mu.Lock()
		_, err = m.ensureRunning(ctx, projectDir, projectName, "")
		mu.Unlock()
		if bip, ok := asBuildInProgress(err); ok {
			// Expected on the warm path: the async build was kicked off (or is
			// still running). A later tick promotes it to a ready sandbox.
			m.logger.Info("warm pool: building project image", "project", projectName, "elapsed", bip.elapsed.Round(time.Second))
		} else if err != nil {
			m.logger.Warn("warm pool: failed to warm project", "project", projectName, "error", err)
		} else {
			m.logger.Info("warm pool: project ready", "project", projectName)
		}
	}
}

// parseStoreKey splits a compound store key ("project/agent") into its parts.
func parseStoreKey(key string) (projectName, agentID string) {
	if idx := strings.Index(key, "/"); idx >= 0 {
		return key[:idx], key[idx+1:]
	}
	return key, ""
}

// millsAgentIDPrefix identifies the Mills tests stage's per-run sandboxes
// (pkg/mills/pipeline devboxAgentID: operator agent id + run token [+ "-baseline"]).
const millsAgentIDPrefix = "loom-mills-operator-"

// defaultMillsIdleTimeout is short on purpose: an idle Mills sandbox is an orphan
// holding a sandbox of devbox quota (bl-devbox-sandbox-quota-headroom-20260913).
const defaultMillsIdleTimeout = 5 * time.Minute

// millsBackstop reports whether the Mills idle backstop governs agentID: a Mills
// per-run sandbox while DEVBOX_MILLS_IDLE_TIMEOUT is set (zero = global policy).
func (m *manager) millsBackstop(agentID string) bool {
	return strings.HasPrefix(agentID, millsAgentIDPrefix) && m.cfg.millsIdleTimeout > 0
}

// idleTimeoutFor returns the idle timeout one sandbox is held to.
func (m *manager) idleTimeoutFor(agentID string) time.Duration {
	if m.millsBackstop(agentID) {
		return m.cfg.millsIdleTimeout
	}
	return m.cfg.idleTimeout
}

// idleScanTimeout is the shortest configured timeout, so one store scan covers
// both populations; reapIdle re-checks each entry against its own.
func (m *manager) idleScanTimeout() time.Duration {
	if t := m.cfg.millsIdleTimeout; t > 0 && t < m.cfg.idleTimeout {
		return t
	}
	return m.cfg.idleTimeout
}

// reapIdle pauses containers that have been idle beyond the timeout.
// Paused containers can be resumed instantly (~5ms) vs cold start (~2-5s).
// Falls back to stop if pause is not supported by the backend.
//
// K8s-aware: sleeping K8s pods use ~0 CPU. On first idle timeout, just log
// "keeping warm". Hard-reap (stop) only after 2× idle timeout.
//
// Mills per-run sandboxes under millsBackstop are stopped as soon as they pass
// millsIdleTimeout, with no keep-warm grace: a sleeping pod still holds quota.
func (m *manager) reapIdle(ctx context.Context) {
	idle := m.store.IdleEntries(m.idleScanTimeout())
	for key, entry := range idle {
		// Skip entries with active exec calls
		if m.hasActiveExecs(key) {
			continue
		}

		projectName, agentID := parseStoreKey(key)

		// Skip warm-pool projects — the warm pool goroutine will just recreate them.
		if agentID == "" && m.isWarmProject(projectName) {
			continue
		}

		// The scan used the shortest timeout; hold each entry to its own.
		backstop := m.millsBackstop(agentID)
		idleTimeout := m.idleTimeoutFor(agentID)
		idleDuration := time.Since(entry.LastUsed)
		if idleDuration < idleTimeout {
			continue
		}

		containerName := m.containerName(projectName, agentID)

		// K8s-aware: keep pods warm on first idle, hard-reap at 2× timeout.
		// A Mills sandbox under the backstop gets no warm grace.
		if m.isK8sBackend() {
			if !backstop && idleDuration < 2*idleTimeout {
				m.logger.Debug("keeping K8s pod warm", "key", key,
					"idle_since", entry.LastUsed.Format(time.RFC3339))
				continue
			}
			// Past the warm grace (or a Mills sandbox past its backstop) — hard-reap.
			m.logger.Info("hard-reaping idle K8s pod", "key", key,
				"idle_since", entry.LastUsed.Format(time.RFC3339),
				"idle_timeout", idleTimeout, "mills_backstop", backstop)
			if err := m.backend.Stop(ctx, containerName); err != nil {
				m.logger.Warn("failed to stop idle sandbox", "key", key, "error", err)
				continue
			}
			entry.Status = "stopped"
		} else {
			m.logger.Info("pausing idle sandbox", "key", key,
				"idle_since", entry.LastUsed.Format(time.RFC3339))

			// Try pause first (instant resume); fall back to stop
			if err := m.backend.Pause(ctx, containerName); err != nil {
				// Pause not supported — fall back to stop
				if err := m.backend.Stop(ctx, containerName); err != nil {
					m.logger.Warn("failed to stop idle sandbox", "key", key, "error", err)
					continue
				}
				entry.Status = "stopped"
			} else {
				entry.Status = "paused"
			}
		}

		if m.metrics != nil {
			m.metrics.idleReaps.WithLabelValues(projectName).Inc()
		}
		if err := m.store.Set(key, entry); err != nil {
			m.logger.Warn("failed to update state", "key", key, "error", err)
		}
	}
}

// reconcileState checks actual pod/container status on startup and corrects
// stale entries (e.g., pods evicted during daemon downtime, node reboots).
func (m *manager) reconcileState(ctx context.Context) {
	entries := m.store.List()
	for key, entry := range entries {
		if entry.Status != "running" && entry.Status != "paused" {
			continue
		}
		projectName, agentID := parseStoreKey(key)
		containerName := m.containerName(projectName, agentID)
		status, err := m.backend.Status(ctx, containerName)
		if err != nil {
			m.logger.Warn("reconcile: failed to check status", "key", key, "error", err)
			entry.Status = "stopped"
			_ = m.store.Set(key, entry)
			continue
		}
		if !status.Running {
			m.logger.Info("reconcile: marking stale entry as stopped",
				"key", key, "actual_status", status.Status)
			entry.Status = "stopped"
			_ = m.store.Set(key, entry)
		}
	}
}

// shutdownAll stops all managed containers gracefully.
// It cancels running async execs and waits for goroutines to finish
// before stopping containers.
func (m *manager) shutdownAll(ctx context.Context) {
	m.gateCancels.Range(func(key, value any) bool {
		value.(context.CancelFunc)()
		if err := m.lockGateLifecycle(ctx, key.(string), true); err == nil {
			m.projectLock(key.(string)).Unlock()
		}
		return ctx.Err() == nil
	})

	// Cancel all running async execs so goroutines exit promptly.
	if m.asyncExecs != nil {
		m.asyncExecs.mu.RLock()
		for _, ae := range m.asyncExecs.execs {
			if ae.Status == "running" && ae.cancel != nil {
				ae.cancel()
			}
		}
		m.asyncExecs.mu.RUnlock()
	}

	// Wait for all async goroutines to complete.
	done := make(chan struct{})
	go func() { m.asyncWg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	}

	entries := m.store.List()
	for key, entry := range entries {
		if entry.Status == "running" {
			if m.isK8sBackend() && m.hasActiveExecs(key) {
				continue
			}
			if ctx.Err() != nil {
				return
			}
			projectName, agentID := parseStoreKey(key)
			containerName := m.containerName(projectName, agentID)
			m.logger.Info("shutting down sandbox", "key", key)
			if err := m.backend.Stop(ctx, containerName); err != nil {
				m.logger.Warn("failed to stop sandbox on shutdown", "key", key, "error", err)
			}
			entry.Status = "stopped"
			_ = m.store.Set(key, entry)
		}
	}
}

// langNames returns a comma-separated string of detected language names.
func langNames(fp *detect.EnvFingerprint) string {
	names := make([]string, len(fp.Languages))
	for i, l := range fp.Languages {
		names[i] = l.Language
	}
	return strings.Join(names, ", ")
}
