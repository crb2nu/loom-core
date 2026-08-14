// Package backend provides container runtime backends for devbox sandboxes.
package backend

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrNotSupported is returned when a backend doesn't support an operation.
var ErrNotSupported = errors.New("operation not supported by this backend")

// ErrRuntimeIdentityConflict is returned when an existing runtime resource
// does not carry the identity labels expected by the caller. Callers must
// treat this as an ownership fence: never reuse, replace, or delete the
// resource based on its deterministic name alone.
var ErrRuntimeIdentityConflict = errors.New("runtime identity conflict")

// StartIdentityProber is an optional capability implemented by shared-cluster
// backends. It reports whether the deterministic runtime name already exists
// and validates its labels without mutating it. Orchestrators use this before
// durable registration so a rowless foreign runtime cannot be claimed by
// name.
type StartIdentityProber interface {
	ProbeStartIdentity(ctx context.Context, opts StartOpts) (exists bool, err error)
}

// IdentityStopper is an optional capability implemented by shared-cluster
// backends. It deletes a runtime only when its current labels match every
// expected identity label. Implementations also use the resource UID as a
// delete precondition when available to close the check/delete race.
type IdentityStopper interface {
	StopIfIdentity(ctx context.Context, id string, expectedLabels map[string]string) error
}

// Backend defines the interface for container runtimes (Docker, K8s).
type Backend interface {
	// Build builds a container image from a generated Dockerfile.
	Build(ctx context.Context, opts BuildOpts) (*BuildResult, error)

	// Start starts a persistent sandbox container for a project.
	Start(ctx context.Context, opts StartOpts) (*StartResult, error)

	// Exec runs a command in a running sandbox container.
	Exec(ctx context.Context, opts ExecOpts) (*ExecResult, error)

	// Stop stops and removes a sandbox container.
	Stop(ctx context.Context, id string) error

	// Status returns the status of a sandbox container.
	Status(ctx context.Context, id string) (*StatusResult, error)

	// Health checks if the backend runtime is available.
	Health(ctx context.Context) error

	// Pause freezes a running container for instant resume later.
	// Returns ErrNotSupported if the backend doesn't support pausing.
	Pause(ctx context.Context, id string) error

	// Resume unfreezes a paused container (~5ms for Docker).
	// Returns ErrNotSupported if the backend doesn't support resuming.
	Resume(ctx context.Context, id string) error

	// ReadFile reads a file from inside a running container.
	ReadFile(ctx context.Context, id, path string) ([]byte, error)

	// WriteFile writes content to a file inside a running container.
	WriteFile(ctx context.Context, id, path string, content []byte, mode string) error

	// CleanupBuilds deletes completed build pods and associated ConfigMaps
	// older than maxAge. Returns the number of resources cleaned up.
	CleanupBuilds(ctx context.Context, maxAge time.Duration) (int, error)
}

// BuildOpts configures an image build.
type BuildOpts struct {
	Tag            string // image tag (e.g., "mcp/devbox/loom-core:a3b9c1d")
	Dockerfile     []byte // generated Dockerfile content
	ContextDir     string // build context directory (project dir)
	PreferExisting bool   // when true, return/reuse Tag if it already exists
}

// BuildResult describes the outcome of an image build.
type BuildResult struct {
	ImageTag string `json:"image_tag"`
	Cached   bool   `json:"cached"`
}

// SecretEnvVar describes an environment variable sourced from a K8s Secret.
type SecretEnvVar struct {
	Name       string // env var name (e.g., "ANTHROPIC_API_KEY")
	SecretName string // K8s secret name (e.g., "agent-api-keys")
	SecretKey  string // key within the secret
}

// SecretResolver translates Secret references into the concrete artifacts
// a spawn backend needs to bake into its sandbox payload (env values for
// SecretEnv; file contents for SecretMount). K8sBackend implements both
// methods natively via its existing Clientset; non-K8s backends (e.g.,
// HarvesterVMBackend) accept it as an optional dependency so they can
// flatten Secret-backed env + files into the per-VM cloud-init payload at
// Start time. Missing Secrets or missing keys are treated as no-ops
// (matching the Optional semantics K8s SecretKeyRef and SecretVolumeSource
// apply on the pod-spec side) so a partially-populated cluster Secret does
// not break Start.
type SecretResolver interface {
	ResolveSecretEnv(ctx context.Context, secrets []SecretEnvVar) (map[string]string, error)
	ResolveSecretMounts(ctx context.Context, mounts []SecretMount) ([]ResolvedSecretFile, error)
}

// ResolvedSecretFile is one file's worth of Secret content resolved out of
// a SecretMount. Path is the absolute destination inside the sandbox,
// already computed by the resolver as filepath.Join(mount.MountPath,
// item.Path). Content is the raw bytes (binary-safe). Mode is the POSIX
// file mode (default "0600" — matches K8s SecretVolumeSource.DefaultMode).
type ResolvedSecretFile struct {
	Path    string
	Content []byte
	Mode    string
}

// SecretMount mounts individual keys from a K8s Secret as files in the container.
type SecretMount struct {
	SecretName string // K8s secret name (e.g., "agent-auth-tokens")
	MountPath  string // container directory to mount into (e.g., "/root/.codex")
	Items      []SecretMountItem
}

// SecretMountItem maps a single key from a Secret to a file path within the mount.
type SecretMountItem struct {
	Key  string // key in the Secret (e.g., "codex-auth-json")
	Path string // relative filename within MountPath (e.g., "auth.json")
}

// StartOpts configures a sandbox container start.
type StartOpts struct {
	Name         string            // container name (e.g., "devbox-loom-core")
	ImageTag     string            // image to use
	WorkDir      string            // working directory inside container (default: "/workspace")
	Mounts       []Mount           // bind mounts
	Env          map[string]string // environment variables
	SecretEnv    []SecretEnvVar    // env vars sourced from K8s secrets (K8s backend only)
	SecretMounts []SecretMount     // files from K8s secrets mounted into the container
	MemoryMB     int               // memory limit in MB (0 = no limit)
	CPUs         float64           // CPU limit (0 = no limit)
	Network      bool              // enable networking
	AgentID      string            // owning agent ID (used as pod label in K8s backend)

	// ManagedByOverride, if non-empty, replaces the default "mcp-devbox"
	// value for the app.kubernetes.io/managed-by label. Spawn pods set this
	// to "loom-spawn" so the reconciler can discover them.
	ManagedByOverride string

	// ExtraLabels are merged into the pod/container labels after defaults.
	// Caller-provided keys win over defaults if there is a collision.
	ExtraLabels map[string]string

	// AllowMissingIdentityLabels permits the one configured recovery authority
	// to adopt legacy resources that predate identity labels. A non-empty
	// mismatch is never allowed. ProbeStartIdentity remains read-only; Start
	// stamps any permitted missing labels before reuse or replacement.
	AllowMissingIdentityLabels bool

	// Branch, when set in git-clone sync mode, is checked out by the
	// git-clone init container after clone. If the branch exists on
	// origin it is checked out directly; otherwise it is created from
	// BaseBranch. Has no effect in tar-pipe or PVC modes.
	Branch string

	// BaseBranch is the parent branch used to create Branch when Branch
	// does not yet exist on origin. Defaults to "main" when empty.
	// Only consulted in git-clone sync mode.
	BaseBranch string

	// AgentCLIInstallCmd is a guarded, idempotent shell snippet that ensures
	// the agent CLI (codex/claude-code/gemini) is present on the substrate
	// before the agent runs. The K8s backend bakes the CLI into the per-agent
	// runtime image at Build time and ignores this field. The harvester-vm
	// backend has a no-op Build (one shared base image), so it runs this
	// snippet over SSH at Start time to install the CLI on the VM. Written to
	// be a fast no-op (`command -v <cli> || install`) so a future curated base
	// image with the CLI pre-baked makes provisioning instant. Empty means
	// "assume the CLI is already on the substrate".
	AgentCLIInstallCmd string

	// CachePVCs mounts shared persistent volume claims into the container —
	// build/module caches that outlive any single pod. Unlike the workspace
	// (freshly cloned per spawn), a cache mount is deliberately REUSED across
	// pods so cold-start work (Go dependency compiles that were eating 20+
	// of a 25-minute spawn deadline, 2026-07-26) is paid once per fleet
	// instead of once per spawn. K8s backend only; other backends ignore it.
	// The claim must be RWX when spawns run concurrently.
	CachePVCs []CachePVCMount
}

// CachePVCMount names one shared cache claim and where to surface it.
type CachePVCMount struct {
	ClaimName string
	MountPath string
}

func expectedStartIdentityLabels(opts StartOpts) map[string]string {
	expected := make(map[string]string, len(opts.ExtraLabels)+1)
	if value := opts.ManagedByOverride; value != "" {
		expected["app.kubernetes.io/managed-by"] = value
	}
	for key, value := range opts.ExtraLabels {
		if key != "" && value != "" {
			expected[key] = value
		}
	}
	return expected
}

// validateIdentityLabels returns labels that may be stamped by the recovery
// authority. Existing non-empty mismatches always fail closed.
func validateIdentityLabels(resource string, actual, expected map[string]string, allowMissing bool) (map[string]string, error) {
	missing := make(map[string]string)
	for key, want := range expected {
		got := actual[key]
		switch {
		case got == want:
		case got == "" && allowMissing:
			missing[key] = want
		default:
			return nil, errors.Join(
				ErrRuntimeIdentityConflict,
				fmt.Errorf("%s label %s=%q, want %q", resource, key, got, want),
			)
		}
	}
	return missing, nil
}

// Mount describes a bind mount.
type Mount struct {
	Host      string // host path
	Container string // container path
	ReadOnly  bool   // read-only mount
}

// StartResult describes a started container.
type StartResult struct {
	ContainerID string `json:"container_id"`
}

// ExecOpts configures command execution in a sandbox.
type ExecOpts struct {
	ContainerID string            // target container
	Command     string            // shell command to run
	WorkDir     string            // working directory (default: "/workspace")
	Env         map[string]string // additional env vars
	TimeoutSec  int               // execution timeout in seconds
	MaxLines    int               // max tail lines to return
}

// StatusResult describes the current state of a sandbox container.
type StatusResult struct {
	Running bool   `json:"running"`
	Status  string `json:"status"` // "running", "exited", "not_found"
}
