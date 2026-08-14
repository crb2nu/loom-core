// Package spawn provides a standalone controller for managing headless agent
// spawn lifecycles. It extracts spawn orchestration out of the HUD package,
// adding K8s-native state reconciliation so pod labels become the source of
// truth instead of local JSON files.
package spawn

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/crb2nu/loom/internal/hud/bridge"
)

// ControlCommand is the wire format the spawn-driver consumes over its JSONL
// control file. The Go orchestrator serializes one of these per line. The
// REST layer (admin + mobile) accepts the same shape as its request body so
// web and mobile clients can push follow-up turns or cancellations into a
// long-lived multi-turn spawn.
//
// Type discriminates payload semantics:
//
//   - "message"   : push a follow-up user turn (Text required)
//   - "interrupt" : abort the in-flight generation (no payload)
//   - "shutdown"  : graceful exit after the current turn completes
type ControlCommand struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// Control command type discriminators. Keep in sync with the driver-side
// ControlCommand union in tools/spawn-driver/src/control-file.ts.
const (
	ControlCommandMessage   = "message"
	ControlCommandInterrupt = "interrupt"
	ControlCommandShutdown  = "shutdown"
)

// Control plane sentinel errors. Handlers map these to HTTP status codes so
// the HUD web UI and mobile client can surface precise failure reasons.
var (
	// ErrSpawnNotFound indicates the spawn ID is unknown to the controller.
	ErrSpawnNotFound = errors.New("spawn not found")
	// ErrSpawnNotRunning indicates the spawn exists but is in a terminal
	// state (completed/failed/stopped) and cannot receive control commands.
	ErrSpawnNotRunning = errors.New("spawn is not running")
	// ErrSpawnNotMultiTurn indicates the spawn was created without the
	// multi_turn flag and therefore has no control file to append to.
	ErrSpawnNotMultiTurn = errors.New("spawn is not multi-turn")
	// ErrInvalidControlCommand indicates the command failed validation
	// (missing type, empty message text, or unknown type).
	ErrInvalidControlCommand = errors.New("invalid control command")
)

// Status tracks the lifecycle state of a spawned agent.
type Status string

const (
	StatusPending  Status = "creating"
	StatusBuilding Status = "building"
	StatusRunning  Status = "running"
	StatusStopped  Status = "stopped"
	StatusFailed   Status = "failed"
	StatusUnknown  Status = "unknown"

	// StatusCompleted indicates the agent finished its task successfully.
	StatusCompleted Status = "completed"
)

// Agent type discriminators for Request.AgentType. The mentatlab-node value
// tags a spawn that originates from an autonomous MentatLab DAG node (F7).
// v1 is a stub path validated by DispatchDAGNode; full engine integration is
// a follow-up.
const (
	AgentTypeMentatLabNode = "mentatlab-node"

	// MaxCompletionHoldSeconds bounds the optional post-agent completion hold.
	// The hold exists for bounded lifecycle/crash validation; keeping it short
	// prevents a caller from turning a completed agent turn into an unbounded
	// spawn-slot reservation.
	MaxCompletionHoldSeconds = 300
)

// Request contains the parameters for spawning a headless agent.
type Request struct {
	// AgentType is one of "claude-code", "codex", "gemini", or
	// AgentTypeMentatLabNode for autonomous DAG-originated spawns.
	AgentType string `json:"agent_type"`
	// Model is the vendor-native LLM model id (e.g. "gpt-5.6-terra") the agent
	// CLI should run. Optional: empty means "use the vendor default"
	// (resolveCodexModel's SPAWN_CODEX_MODEL env / compiled default for codex).
	// Only the codex path consumes it today — buildAgentCommand pins it on
	// `codex exec --model`; claude-code and gemini have no CLI model knob, so a
	// set value is ignored with a wiring log rather than an error. Mills
	// populates this from pipeline.SpawnRequest.AgentModel, derived from
	// policy.ModelForStage / LOOM_MILLS_SPAWN_MODEL.
	Model           string  `json:"model,omitempty"`
	Namespace       string  `json:"namespace"`        // Agent context namespace.
	Branch          string  `json:"branch"`           // Git branch to work on.
	BaseBranch      string  `json:"base_branch"`      // Base branch for worktree.
	TaskDescription string  `json:"task_description"` // Task to execute.
	Project         string  `json:"project"`          // Project/repo name.
	MemoryMB        int     `json:"memory_mb"`        // Container memory limit.
	CPUs            float64 `json:"cpus"`             // Container CPU limit.
	TimeoutMinutes  int     `json:"timeout_minutes"`  // Max runtime before reap.
	// CompletionHoldSeconds keeps the agent exec session and spawn lifecycle
	// open for this many seconds after a successful agent command. Zero keeps
	// legacy behavior. The HUD rejects values outside
	// [0, MaxCompletionHoldSeconds].
	CompletionHoldSeconds int `json:"completion_hold_seconds,omitempty"`
	// MaxCostUSD caps total spawn cost in USD. The budget watcher cancels the
	// exec when the accumulated cost meets or exceeds this value. 0 = unlimited.
	MaxCostUSD float64 `json:"max_cost_usd,omitempty"`
	// MaxTurns caps total agent turns. The budget watcher cancels the exec
	// when the accumulated turn count meets or exceeds this value. 0 = unlimited.
	MaxTurns int `json:"max_turns,omitempty"`
	// UseSDKDriver routes the spawn through the embedded loom-spawn-driver
	// Node.js sidecar instead of invoking the agent CLI directly. The driver
	// is injected into the pod via injectSDKDriver and emits parser-compatible
	// JSONL on stdout. Slice 7a/7b ship a hand-written stub bundle; Slice 7c
	// will swap in a real SDK-backed bundle. Defaults to false (legacy CLI path).
	UseSDKDriver bool `json:"use_sdk_driver,omitempty"`
	// MultiTurn enables long-lived conversational mode for the spawn driver.
	// When set, the orchestrator pre-creates an empty JSONL control file in
	// the pod and passes its path to the driver via --control-file. The
	// driver tails the file for `{type:"message"|"interrupt"|"shutdown"}`
	// commands so the HUD/mobile REST endpoints (slice 8c) can push
	// follow-up turns and cancellations into a running session. Requires
	// UseSDKDriver=true; ignored on the legacy CLI path. Defaults to false
	// for full backwards compatibility with single-shot spawns.
	MultiTurn bool `json:"multi_turn,omitempty"`
	// ParentSessionID is the daemon proxy session.ID that originated this
	// spawn. When non-empty it is exposed to the pod as
	// LOOM_PARENT_SESSION_ID so CLI session hooks can stitch the spawn's
	// agent-context session to the caller's proxy session. Optional; leave
	// empty for standalone spawns or when no proxy session exists (e.g.,
	// direct K8s jobs, MentatLab DAG nodes).
	ParentSessionID string `json:"parent_session_id,omitempty"`
	// Metadata carries caller-supplied key/value tags on the spawn for
	// HUD correlation. The weaver spawn bridge populates weaver_query_id
	// and weaver_domain here so the HUD can render "spawn X came from
	// weaver query Y". Keys are free-form; consumers should namespace
	// their own keys (e.g. weaver_*).
	Metadata map[string]string `json:"metadata,omitempty"`
	// Substrate selects the devbox backend the spawn pod's in-pod
	// mcp-devbox should route subsequent devbox_* MCP calls to. Mills
	// populates this via pipeline.SpawnRequest.Substrate, which is
	// derived from policy.SubstrateForStage (see
	// .loom/45-product-spec-mills-harvester-vm-substrate-2026-05-25.md).
	// Empty means "use the in-pod mcp-devbox's compiled-in default
	// backend" — current behavior pre-Slice-2c. The HUD spawn
	// orchestrator translates this to DEVBOX_BACKEND on the pod env.
	// Slice 2d will add a per-spawn backend lookup so the pod itself
	// runs on the named substrate; today the pod still lives on the
	// orchestrator's single backend.
	Substrate string `json:"substrate,omitempty"`
	// IdempotencyKey is an OPT-IN caller-supplied replay key (Slice 2b).
	// When non-empty, the controller derives a deterministic spawn id from
	// it and makes registration idempotent: a duplicate create with the
	// same key re-attaches to the existing spawn (AlreadyExists no-op)
	// instead of minting a second pod, closing the pre-existing
	// double-spawn window. Empty preserves legacy behavior exactly — the
	// server mints the id via crypto/rand NewSpawnID(). Set by the Mills
	// durable workflow runtime (spec: .loom/130-133) and by every Mills
	// pipeline stage spawn (pipeline.stageIdempotencyKey), whose keyed-ness
	// is what lets restart recovery re-drive/re-attach the turn instead of
	// fail-fasting it.
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

// AuthMode describes which cluster credential path the spawned agent was
// configured to use. Surfaced in HUD spawn detail and used to fail fast
// before pod start when no credentials are available.
type AuthMode string

const (
	// AuthModeClusterOAuth means the agent uses a cluster-owned OAuth
	// token from ClusterAgentAuthSecret (Slice 2b).
	AuthModeClusterOAuth AuthMode = "cluster_oauth"

	// AuthModeClusterAPIKey means the agent uses a vendor API key from
	// ClusterAgentAPIKeysSecret. Default for Slice 2a.
	AuthModeClusterAPIKey AuthMode = "cluster_api_key"

	// AuthModeClusterServiceAccount means the agent uses a service
	// account JSON (Gemini path).
	AuthModeClusterServiceAccount AuthMode = "cluster_service_account"

	// AuthModeMissing means no cluster credentials were resolvable at
	// spawn time. The spawn fails fast with an actionable error pointing
	// the operator at `loom auth cluster-set-key`.
	AuthModeMissing AuthMode = "missing"
)

// State holds the state of a spawned agent.
type State struct {
	SpawnID string `json:"spawn_id"`
	AgentID string `json:"agent_id"`
	// DriverOwnerID is the stable logical identity of the HUD/controller that
	// owns this spawn's agent-turn driver. It survives a controller restart so
	// the replacement may resume keyed work, while peer controllers sharing the
	// same ConfigMap keep the row read-only. Concurrent processes must not share
	// one owner ID without an external leader lease.
	DriverOwnerID string `json:"driver_owner_id,omitempty"`
	// SessionID is the agent-context session created for this spawn at
	// spawn-start (via AgentBridge.StartSession). It is the durable home for
	// the spawn's telemetry summary and error entries. Without it, terminal
	// transitions call agent_context_add with an empty session_id, which the
	// store rejects with "session_id: is required" — silently dropping the
	// agent's turn-level telemetry. Empty when the spawn failed before the
	// session was registered.
	SessionID string                 `json:"session_id,omitempty"`
	PodName   string                 `json:"pod_name"`
	Status    Status                 `json:"status"`
	Request   Request                `json:"request"`
	StartedAt time.Time              `json:"started_at"`
	EndedAt   *time.Time             `json:"ended_at,omitempty"`
	Error     string                 `json:"error,omitempty"`
	Telemetry *bridge.SpawnTelemetry `json:"telemetry,omitempty"`
	// AuthMode records which cluster credential path the pod was
	// configured to use. Populated by the orchestrator before pod start.
	AuthMode AuthMode `json:"auth_mode,omitempty"`
	// CleanupAt records when the terminal hook ran for this spawn. Used
	// by the reconciler to fire the hook at most once per spawn — without
	// this, every Reconcile tick after termination would re-attempt
	// presence/pod cleanup.
	CleanupAt *time.Time `json:"cleanup_at,omitempty"`
	// StopRequestedAt is a durable, non-terminal cancellation intent. While it
	// is set, lifecycle drivers and Reconcile must not advance Status; cleanup
	// owns the record until it compare-and-sets the state to stopped. Keeping the
	// prior active Status makes unfinished cleanup visible to admission gates.
	StopRequestedAt *time.Time `json:"stop_requested_at,omitempty"`
	// Supervised records that this spawn's agent turn was launched under the
	// pod-owned execution supervisor (S4): a detached, PID-1-reparented reaper
	// owns the completion-wrapper/hold process pair and records the outcome
	// durably in the pod. When set, restart recovery RE-ATTACHES to the running
	// supervisor (observe status, collect outcome) instead of re-driving a fresh
	// agent turn — preserving the original process pair across a controller
	// crash (the S1c process-continuity contract). Empty/false preserves the
	// legacy re-drive recovery path exactly, so pods launched by an older
	// controller (no supervisor) still recover. Set by the HUD orchestrator
	// before the agent exec; never by external callers.
	Supervised bool `json:"supervised,omitempty"`
}

// IsTerminal returns true if the status represents a terminal spawn state.
func IsTerminal(status Status) bool {
	switch status {
	case StatusCompleted, StatusFailed, StatusStopped:
		return true
	default:
		return false
	}
}

// ManagedByLabel is the Kubernetes label used to identify pods managed by the
// spawn controller.
const ManagedByLabel = "app.kubernetes.io/managed-by"

// ManagedByValue is the label value applied to spawn-managed pods.
const ManagedByValue = "loom-spawn"

// SpawnIDLabel is the label key for the spawn ID on managed pods.
const SpawnIDLabel = "loom.dev/spawn-id"

// AgentIDLabel is the label key for the agent ID on managed pods.
const AgentIDLabel = "loom.dev/agent-id"

// AgentTypeLabel is the label key for the agent CLI vocabulary. Reconcile uses
// it when reconstructing a rowless legacy pod after an operator restart.
const AgentTypeLabel = "loom.dev/agent-type"

// ProjectLabel is the label key for the project name on managed pods.
const ProjectLabel = "loom.dev/project"

// KubernetesLabelValue converts external metadata into a deterministic label
// value. Kubernetes limits values to 63 ASCII characters and rejects project
// paths such as "services/loom-core" because slash is not allowed.
func KubernetesLabelValue(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}

	var normalized strings.Builder
	normalized.Grow(len(value))
	lastWasReplacement := false
	for i := 0; i < len(value); i++ {
		c := value[i]
		if isKubernetesLabelValueChar(c) {
			normalized.WriteByte(c)
			lastWasReplacement = false
			continue
		}
		if !lastWasReplacement {
			normalized.WriteByte('-')
			lastWasReplacement = true
		}
	}

	result := strings.Trim(normalized.String(), "-_.")
	sum := sha256.Sum256([]byte(value))
	hash := hex.EncodeToString(sum[:8])
	if result == "" {
		return hash
	}
	if len(result) <= 63 {
		return result
	}

	const suffixLength = 1 + 16 // separator plus the encoded hash
	prefix := strings.TrimRight(result[:63-suffixLength], "-_.")
	if prefix == "" {
		return hash
	}
	return prefix + "-" + hash
}

func isKubernetesLabelValueChar(c byte) bool {
	return c >= 'a' && c <= 'z' ||
		c >= 'A' && c <= 'Z' ||
		c >= '0' && c <= '9' ||
		c == '-' || c == '_' || c == '.'
}

// DriverOwnerLabel fingerprints the durable DriverOwnerID onto runtime pods.
// The fingerprint is label-safe and lets a controller distinguish its own
// rowless orphan from a peer pod without exposing or constraining the raw ID.
const DriverOwnerLabel = "loom.dev/driver-owner"

// RuntimeGenerationLabel distinguishes successive durable incarnations that
// reuse the same deterministic spawn name. It prevents a stale controller
// from deleting a newer same-owner/same-key runtime after state recreation.
const RuntimeGenerationLabel = "loom.dev/spawn-generation"

// DriverOwnerLabelValue returns the stable, Kubernetes-label-safe fingerprint
// stored in DriverOwnerLabel. Empty owners intentionally produce no label.
func DriverOwnerLabelValue(ownerID string) string {
	ownerID = strings.TrimSpace(ownerID)
	if ownerID == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(ownerID))
	return hex.EncodeToString(sum[:8])
}

// RuntimeGenerationLabelValue converts the record-before-dispatch timestamp
// into a stable Kubernetes-label-safe generation token.
func RuntimeGenerationLabelValue(startedAt time.Time) string {
	if startedAt.IsZero() {
		return ""
	}
	return strconv.FormatInt(startedAt.UTC().UnixNano(), 36)
}

// ParseRuntimeGenerationLabelValue reverses RuntimeGenerationLabelValue and
// rejects non-canonical tokens. A rowless runtime can use this immutable label
// to recover the exact record-before-dispatch generation instead of inventing
// one from the later Kubernetes creation timestamp.
func ParseRuntimeGenerationLabelValue(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, errors.New("runtime generation label is empty")
	}
	nanos, err := strconv.ParseInt(value, 36, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse runtime generation label %q: %w", value, err)
	}
	startedAt := time.Unix(0, nanos).UTC()
	if RuntimeGenerationLabelValue(startedAt) != value {
		return time.Time{}, fmt.Errorf("runtime generation label %q is not canonical", value)
	}
	return startedAt, nil
}

// RuntimeIdentityLabels returns the immutable labels that tie a pod or VM to
// one durable spawn generation. Empty legacy fields are omitted so the sole
// recovery authority can migrate them explicitly.
func RuntimeIdentityLabels(spawnID, agentID, ownerID string, startedAt time.Time) map[string]string {
	labels := make(map[string]string, 4)
	if spawnID != "" {
		labels[SpawnIDLabel] = spawnID
	}
	if agentID != "" {
		labels[AgentIDLabel] = agentID
	}
	if owner := DriverOwnerLabelValue(ownerID); owner != "" {
		labels[DriverOwnerLabel] = owner
	}
	if generation := RuntimeGenerationLabelValue(startedAt); generation != "" {
		labels[RuntimeGenerationLabel] = generation
	}
	return labels
}

// RuntimeIdentityLabelsForState is the State convenience wrapper used by
// reconcile and cleanup paths.
func RuntimeIdentityLabelsForState(state *State) map[string]string {
	if state == nil {
		return nil
	}
	return RuntimeIdentityLabels(state.SpawnID, state.AgentID, state.DriverOwnerID, state.StartedAt)
}
