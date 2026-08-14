package hud

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/crb2nu/loom/internal/devbox/backend"
)

// DefaultSpawnConfig returns sensible defaults.
func DefaultSpawnConfig() SpawnOrchestratorConfig {
	wsRoot := "/workspace"
	if home, err := os.UserHomeDir(); err == nil {
		wsRoot = home + "/workspace"
	}
	return SpawnOrchestratorConfig{
		MaxConcurrent:        3,
		MaxConcurrentBuilds:  1,
		DefaultTimeout:       60 * time.Minute,
		DefaultMemory:        4096,
		DefaultCPUs:          2.0,
		WorkspaceRoot:        wsRoot,
		LivenessStallTimeout: defaultLivenessStallTimeout,
		ControllerID:         defaultSpawnControllerID(),
		RecoveryAuthority:    spawnRecoveryAuthorityFromEnv(),
		SupervisedExecution:  supervisedExecutionFromEnv(),
	}
}

const defaultSupervisedExecution = true

func supervisedExecutionFromEnv() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("LOOM_SPAWN_SUPERVISED_EXECUTION"))) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return defaultSupervisedExecution
	}
}

func defaultSpawnControllerID() string { return scopedDefaultSpawnControllerID() }

func scopedDefaultSpawnControllerID(scope ...string) string {
	if configured := strings.TrimSpace(os.Getenv("SPAWN_CONTROLLER_ID")); configured != "" {
		return configured
	}
	host, _ := os.Hostname()
	home, _ := os.UserHomeDir()
	executable, _ := os.Executable()
	return localSpawnControllerID(host, home, strings.TrimSpace(filepath.Base(executable)), scope...)
}

func localSpawnControllerID(host, home, role string, scope ...string) string {
	role = strings.TrimSpace(filepath.Base(role))
	parts := []string{strings.TrimSpace(host), strings.TrimSpace(home), role}
	for _, item := range scope {
		parts = append(parts, strings.TrimSpace(item))
	}
	seed := strings.Join(parts, "\x00")
	if seed == "\x00\x00" {
		seed = "unknown-local-controller"
	}
	sum := sha256.Sum256([]byte(seed))
	if role == "" || role == "." {
		role = "unknown"
	}
	return fmt.Sprintf("local/%s/%x", role, sum[:8])
}

func spawnRecoveryAuthorityFromEnv() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("SPAWN_RECOVERY_AUTHORITY"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// buildSpawnPodEnv returns the env-var map the orchestrator passes to
// backend.StartOpts.Env when creating the spawn pod. Extracted from
// runSpawn so the routing logic is unit-testable without spinning up
// the full orchestrator.
//
// Substrate → DEVBOX_BACKEND is the Slice 2c hop: Mills selects a
// per-stage devbox backend in policy.SubstrateForStage, propagates it
// to pipeline.SpawnRequest.Substrate (Slice 2b), HUDSpawnClient sends
// it on the spawn POST body (Slice 2c, this slice), and the in-pod
// mcp-devbox reads DEVBOX_BACKEND at startup to route subsequent
// devbox_* MCP calls. The pod itself still runs on the orchestrator's
// single backend; Slice 2d will add per-spawn backend selection.
func buildSpawnPodEnv(req SpawnRequest, agentID, spawnID string) map[string]string {
	env := map[string]string{
		"AGENT_ID":  agentID,
		"SPAWN_ID":  spawnID,
		"NAMESPACE": req.Namespace,
	}
	if req.ParentSessionID != "" {
		env["LOOM_PARENT_SESSION_ID"] = req.ParentSessionID
	}
	// Gemini picks up service-account auth via this standard Google env
	// var, which the Google Auth Library reads to find the SA JSON file.
	// Harmless when the SA JSON isn't present — Gemini falls back to
	// GEMINI_API_KEY from cluster-agent-api-keys.
	if req.AgentType == "gemini" {
		env["GOOGLE_APPLICATION_CREDENTIALS"] = GeminiSAMountPath + "/" + GeminiSAFilename
	}
	if req.Substrate != "" {
		env["DEVBOX_BACKEND"] = req.Substrate
	}
	// Go toolchain env so the spawned agent can `go build`/`go test` a
	// single-repo clone (git-clone mode) of a flexinfer Go service whose
	// checked-in go.work `use`s private sibling modules the pod does not have on
	// disk. See spawnGoModuleEnv. Inert for non-Go spawns.
	for k, v := range spawnGoModuleEnv() {
		env[k] = v
	}
	return env
}

// defaultSpawnGitPrivateHost is the GitLab host whose modules are private to
// this workspace (gitlab.flexinfer.ai/libs/*). loom-core itself is hosted here
// too, so the git-clone token the spawn pod already carries authorizes these
// sibling modules as well.
const defaultSpawnGitPrivateHost = "gitlab.flexinfer.ai"

// resolveSpawnGitPrivateHost returns the host treated as a private Go module
// source (GOPRIVATE) and credentialed via git url.insteadOf inside the spawn
// pod. SPAWN_GIT_PRIVATE_HOST overrides it; setting it empty disables the
// private-module wiring (GOWORK/CGO/GOFLAGS are still applied — they are the
// correct defaults for any single-repo Go clone).
func resolveSpawnGitPrivateHost() string {
	if v, ok := os.LookupEnv("SPAWN_GIT_PRIVATE_HOST"); ok {
		return strings.TrimSpace(v)
	}
	return defaultSpawnGitPrivateHost
}

// spawnGoModuleEnv returns the Go toolchain env vars that let a spawned agent
// `go build`/`go test` a single-repo clone of a flexinfer Go service.
//
// The implement spawn pod clones ONE repo (git-clone mode, emptyDir): the
// ../../libs/* siblings the checked-in go.work references are absent, and the
// gitlab.flexinfer.ai/libs/* modules are private. Without this env the agent
// cannot self-verify its changes and ships unbuilt code — Mills run
// PIPE-MILLS-2026-06-29-001-1782734575 saw three implement attempts each die on
// a different toolchain gap (no private-module auth, missing sibling modules,
// broken go.work overlay).
//
// These mirror services/loom-core/Dockerfile + .gitlab-ci.yml exactly:
//   - GOWORK=off              ignore the sibling-overlay go.work; resolve the
//     pinned go.mod versions instead.
//   - GOPRIVATE/GONOSUMDB/GONOPROXY=<host>/*  fetch the private modules
//     directly over git (no proxy/sumdb), authenticated by the url.insteadOf
//     rule injectAgentConfig writes from $GIT_TOKEN.
//   - CGO_ENABLED=0           the lean agent image has no fi-accel C headers;
//     the pure-Go fallback is what CI builds/tests with.
//   - GOFLAGS=-buildvcs=false match the production build and avoid VCS-stamp
//     failures on the spawn clone.
//
// Set globally (not gated on agent type) because the spawn base image is always
// golang and these are inert for non-Go work. The <host> comes from
// resolveSpawnGitPrivateHost; an empty host drops the private-module trio.
func spawnGoModuleEnv() map[string]string {
	env := map[string]string{
		"GOWORK":      "off",
		"GOFLAGS":     "-buildvcs=false",
		"CGO_ENABLED": "0",
	}
	if host := resolveSpawnGitPrivateHost(); host != "" {
		glob := host + "/*"
		env["GOPRIVATE"] = glob
		env["GONOSUMDB"] = glob
		env["GONOPROXY"] = glob
	}
	return env
}

// spawnGoCacheMountPath is where the shared Go cache claim surfaces inside a
// spawn pod when SPAWN_GO_CACHE_PVC is set.
const spawnGoCacheMountPath = "/gocache"

// spawnGoCachePVC names the shared RWX PersistentVolumeClaim mounted into
// every k8s spawn pod as the fleet's Go build + module cache. Empty (the
// default) disables the mount entirely — byte-identical legacy pods.
//
// WHY: every spawn clones fresh into an emptyDir, so `go build`/`go test`
// recompiles the entire dependency tree from scratch. On 2026-07-26 that cold
// compile ate 20+ minutes of 25-minute spawn deadlines — 17 of 73 failed
// stage-attempts were deadline kills (exit 143 / "spawn deadline exceeded"),
// most AFTER the agent had finished authoring its change. A shared cache pays
// that compile once per fleet instead of once per spawn. Both cache types are
// concurrency-safe by design (content-addressed build cache; lock-filed
// module cache); the claim must be RWX (e.g. NFS-backed) so concurrent
// spawns can mount it together.
func spawnGoCachePVC() string {
	return strings.TrimSpace(os.Getenv("SPAWN_GO_CACHE_PVC"))
}

// applySpawnGoCache points the pod's Go toolchain at the shared cache claim:
// it sets GOCACHE/GOMODCACHE under spawnGoCacheMountPath in env (the same map
// passed as StartOpts.Env) and returns the CachePVCMount for StartOpts.
// A nil return with untouched env when claim is empty keeps the legacy pod
// spec byte-identical.
func applySpawnGoCache(env map[string]string, claim string) []backend.CachePVCMount {
	if claim == "" {
		return nil
	}
	env["GOCACHE"] = spawnGoCacheMountPath + "/go-build"
	env["GOMODCACHE"] = spawnGoCacheMountPath + "/gomod"
	return []backend.CachePVCMount{{ClaimName: claim, MountPath: spawnGoCacheMountPath}}
}

// SpawnOrchestratorConfig holds configuration for the spawn orchestrator.
type SpawnOrchestratorConfig struct {
	MaxConcurrent       int
	MaxConcurrentBuilds int
	DefaultTimeout      time.Duration
	DefaultMemory       int // MB
	DefaultCPUs         float64
	WorkspaceRoot       string   // local path to workspace mount (for project detection)
	SyncMode            string   // backend workspace sync mode: "git-clone", "nfs", "tar-pipe" (from SPAWN_SYNC_MODE)
	Projects            []string // available projects for spawn picker (from SPAWN_PROJECTS env)
	// ControllerID is the stable logical owner of spawn drivers persisted in a
	// shared K8s ConfigMap. Replacement restarts reuse it; concurrently active
	// controllers must use distinct IDs unless protected by leader election.
	ControllerID string
	// RecoveryAuthority permits this controller to claim pre-ownership legacy
	// rows and genuinely rowless orphan pods. Exactly one controller sharing a
	// ConfigMap may enable it.
	RecoveryAuthority bool
	// LivenessStallTimeout bounds how long a streaming (K8s) spawn may run
	// without producing any agent output before the orchestrator declares it
	// stalled and fails it. Guards against the zombie-pod wedge: a container
	// stuck in Phase=Running while the codex process inside is dead never goes
	// terminal, so the Mills operator's poll loop waits out its full deadline
	// and only an operator restart re-spawns. Zero falls back to
	// defaultLivenessStallTimeout. The buffered (harvester-vm) path is not
	// watched — it has no mid-flight telemetry, so its TimeoutSec bounds it.
	LivenessStallTimeout time.Duration
	// SupervisedExecution enables the S4 pod-owned execution supervisor for new
	// spawns on the streaming (k8s) substrate. Default derives from
	// LOOM_SPAWN_SUPERVISED_EXECUTION (see supervisedExecutionFromEnv). When on,
	// a controller restart re-attaches to the in-pod reaper instead of
	// re-driving; when off, the legacy exec+re-drive path is used unchanged.
	SupervisedExecution bool
}

// defaultLivenessStallTimeout is the fallback stall window for the spawn
// liveness watchdog when none is configured. 15 minutes is comfortably longer
// than any healthy implement spawn's gap between streamed JSONL lines, while
// recovering a zombie pod far sooner than the 30-minute operator poll deadline
// that previously required a manual restart. Override per deployment via
// LOOM_SPAWN_LIVENESS_STALL_TIMEOUT (a Go duration, e.g. "20m").
const defaultLivenessStallTimeout = 15 * time.Minute

// injectAgentConfig writes platform-specific config files into the pod.
