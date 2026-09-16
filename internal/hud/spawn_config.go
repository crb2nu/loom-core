package hud

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/crb2nu/loom/internal/devbox/backend"
	"github.com/crb2nu/loom/internal/hud/bridge"
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

// spawnProjectPaths records exceptions to the services/<name> workspace
// convention. Path and clone resolution consume this shared metadata instead
// of embedding group-specific branches in the orchestrator.
var spawnProjectPaths = map[string]string{
	"mcp-go":  "libs/mcp-go",
	"fi-fhir": "libs/fi-fhir",
	"edilint": "libs/edilint",
}

func spawnProjectPath(project string) string {
	project = strings.TrimSpace(project)
	if path, ok := spawnProjectPaths[project]; ok {
		return path
	}
	return project
}

// defaultLivenessStallTimeout is the fallback stall window for the spawn
// liveness watchdog when none is configured. 15 minutes is comfortably longer
// than any healthy implement spawn's gap between streamed JSONL lines, while
// recovering a zombie pod far sooner than the 30-minute operator poll deadline
// that previously required a manual restart. Override per deployment via
// LOOM_SPAWN_LIVENESS_STALL_TIMEOUT (a Go duration, e.g. "20m").
const defaultLivenessStallTimeout = 15 * time.Minute

// injectAgentConfig writes platform-specific config files into the pod.
// Uses Exec (stdout-only SPDY) instead of WriteFile (stdin SPDY) to avoid
// in-cluster SPDY stdin stream hangs observed on K3s.
//
// projectDir is the pod-internal absolute path to the project root (e.g.
// "/workspace/services/loom-core"), already resolved by the caller via
// resolveProjectPath so this function does not need to know about
// workspace bucket layouts.
//
// be is the substrate-specific backend chosen by the caller (Slice 2d):
// runSpawn picks via o.substrateBackend(req.Substrate) once per spawn,
// then threads it through to keep all backend calls on one impl.
func (o *SpawnOrchestrator) injectAgentConfig(ctx context.Context, be backend.Backend, containerID, agentType, projectDir string) error {
	writeCmd := func(dir, file, content string) error {
		encoded := base64.StdEncoding.EncodeToString([]byte(content))
		cmd := fmt.Sprintf("mkdir -p %s && echo '%s' | base64 -d > %s/%s", dir, encoded, dir, file)
		_, err := be.Exec(ctx, backend.ExecOpts{
			ContainerID: containerID,
			Command:     cmd,
			TimeoutSec:  30,
		})
		return err
	}

	// Plan Store MCP wiring: spawn pods have no loom daemon, but they can reach
	// the in-cluster agent-context WebSocket endpoint. The bundled loom binary
	// (loomBinaryCopyLines) runs `loom proxy --ws-backend <url>` as a stdio MCP
	// server so the spawned agent can call agent_plan_* (and the rest of
	// agent-context) directly against the Plan Store. Empty url ⇒ feature off.
	planStoreWSURL := resolvePlanStoreWSURL()

	gitAuthorName := strings.TrimSpace(os.Getenv("SPAWN_GIT_AUTHOR_NAME"))
	if gitAuthorName == "" {
		gitAuthorName = "loom-spawn"
	}
	gitAuthorEmail := strings.TrimSpace(os.Getenv("SPAWN_GIT_AUTHOR_EMAIL"))
	if gitAuthorEmail == "" {
		gitAuthorEmail = "loom-spawn@loom.local"
	}
	for _, author := range []struct{ key, value string }{
		{"user.name", gitAuthorName},
		{"user.email", gitAuthorEmail},
	} {
		cmd := fmt.Sprintf("git config --global %s %s", author.key, shellQuote(author.value))
		if _, err := be.Exec(ctx, backend.ExecOpts{ContainerID: containerID, Command: cmd, TimeoutSec: 30}); err != nil {
			return fmt.Errorf("configure git author %s: %w", author.key, err)
		}
	}

	// Authenticate direct git fetches of private modules so the agent can
	// `go build`/`go test` the single-repo clone without the ../../libs
	// siblings its go.work references. $GIT_TOKEN is the same token the
	// git-clone init container used (the k8s backend also exposes it on the
	// runtime container in git-clone mode); the url.insteadOf rule mirrors
	// services/loom-core/Dockerfile + .gitlab-ci.yml. Guarded on $GIT_TOKEN so
	// it is a clean no-op when git-clone mode / the token is absent (NFS-mode
	// devbox or the harvester-vm substrate). Paired with spawnGoModuleEnv,
	// which sets GOPRIVATE for the same host.
	if host := resolveSpawnGitPrivateHost(); host != "" {
		gitCred := fmt.Sprintf(
			`if [ -n "${GIT_TOKEN:-}" ]; then git config --global url."https://token:${GIT_TOKEN}@%s/".insteadOf "https://%s/"; fi`,
			host, host,
		)
		if _, err := be.Exec(ctx, backend.ExecOpts{
			ContainerID: containerID,
			Command:     gitCred,
			TimeoutSec:  30,
		}); err != nil {
			return fmt.Errorf("configure git private-module auth: %w", err)
		}
	}

	switch agentType {
	case "claude-code":
		// Claude Code reads project-level .claude/settings.json for permissions.
		// Modern Claude Code natively reads CLAUDE_CODE_OAUTH_TOKEN, which is
		// injected from the cluster setup-token secret. Do not configure an
		// apiKeyHelper here: the historical helper read a stale oauth.json mount
		// and silently fell back to API billing.
		// enableAllProjectMcpServers trusts the project .mcp.json below without
		// an interactive approval prompt (headless spawn).
		settings := `{"permissions":{"allow":["Bash","Read","Write","Edit","Glob","Grep"]},"enableAllProjectMcpServers":true}`
		if err := writeCmd(projectDir+"/.claude", "settings.json", settings); err != nil {
			return fmt.Errorf("write claude settings: %w", err)
		}
		// Project-root .mcp.json is auto-loaded by claude-code; the loom proxy
		// bridges to the in-cluster Plan Store over WebSocket.
		if planStoreWSURL != "" {
			mcpJSON := loomMCPServerJSON(planStoreWSURL)
			if err := writeCmd(projectDir, ".mcp.json", mcpJSON); err != nil {
				return fmt.Errorf("write claude .mcp.json: %w", err)
			}
		}
	case "codex":
		// Codex reads ~/.codex/config.toml for sandbox + multi-agent features
		// and ~/.codex/auth.json for OAuth (falling back to $OPENAI_API_KEY).
		// Because the auth.json is a read-only secret volume mount staged at
		// /home/agent/.codex.auth/, we symlink it into the writable
		// /home/agent/.codex/ directory so Codex CLI can read it at its
		// native path. The symlink transparently reflects kubelet-propagated
		// secret updates (e.g., refreshed OAuth tokens written by
		// mcp-auth-refresher).
		//
		// The loom binary is bundled into the spawn image (loomBinaryCopyLines),
		// so the [mcp_servers.loom] block below gives the codex agent a stdio
		// MCP server that bridges to the in-cluster Plan Store over WebSocket —
		// closing the long-standing TODO. `loom proxy --ws-backend` exposes
		// agent_plan_* (and the rest of agent-context) with un-namespaced names.
		config := `[agent]
approval = "auto-edit"

[sandbox]
mode = "workspace-write"
network_access = true

[features]
multi_agent = true
collaboration_modes = true
unified_exec = true
`
		if planStoreWSURL != "" {
			config += "\n" + loomMCPServerTOML(planStoreWSURL)
		}
		if err := writeCmd(AgentHomeDir+"/.codex", "config.toml", config); err != nil {
			return fmt.Errorf("write codex config: %w", err)
		}
		// Best-effort symlink; pipe "true" at the end so the exec doesn't
		// fail when the auth mount is absent (API-key-only operators).
		linkCmd := "ln -sf " + AgentHomeDir + "/.codex.auth/auth.json " + AgentHomeDir + "/.codex/auth.json 2>/dev/null || true"
		if _, err := be.Exec(ctx, backend.ExecOpts{
			ContainerID: containerID,
			Command:     linkCmd,
			TimeoutSec:  10,
		}); err != nil {
			return fmt.Errorf("link codex auth.json: %w", err)
		}
	case "gemini":
		// Gemini reads ~/.gemini/settings.json for permissions. The Google
		// Auth Library auto-detects GOOGLE_APPLICATION_CREDENTIALS; that env
		// var is set at pod-start time in runSpawn(), pointing at the
		// service-account JSON mounted from the cluster secret. If the SA
		// JSON key is absent, the file is missing and Gemini falls back to
		// GEMINI_API_KEY env.
		// Gemini reads ~/.gemini/settings.json mcpServers for MCP servers; the
		// loom proxy bridges to the in-cluster Plan Store over WebSocket.
		settings := `{"permissions":{"allow_all":true}}`
		if planStoreWSURL != "" {
			settings = `{"permissions":{"allow_all":true},` + loomMCPServerJSONInner(planStoreWSURL) + `}`
		}
		if err := writeCmd(AgentHomeDir+"/.gemini", "settings.json", settings); err != nil {
			return fmt.Errorf("write gemini settings: %w", err)
		}
	}
	return nil
}

// defaultPlanStoreWSURL is the in-cluster WebSocket MCP endpoint of the
// agent-context server (the Loom Plan Store backend). Spawn pods have no local
// loom daemon and no Unix socket, but they CAN reach this ClusterIP service.
// The agent-context server speaks plain MCP over WebSocket at /ws (the
// deployment sets MCP_TRANSPORT=websocket on :8080); a `loom proxy
// --ws-backend <url>` bridge in the pod exposes its tools (incl. agent_plan_*)
// to the spawned agent over stdio. Overridable via SPAWN_PLAN_STORE_WS_URL so
// a cluster/namespace move is an env flip, not a rebuild.
const defaultPlanStoreWSURL = "ws://mcp-agent-context.loom-hub.svc.cluster.local:8080/ws"

// defaultSpawnLoomImage is the image the generated spawn Dockerfile copies the
// `loom` binary from (loom-core ships it at /usr/local/bin/loom). It is a
// build-time COPY --from source for the ephemeral spawn-runtime image, NOT a
// running workload, so the `:latest` fallback here is acceptable; the operator
// deployment SHOULD set SPAWN_LOOM_IMAGE to its own Flux-pinned loom-core tag
// (e.g. registry.harbor.lan/mcp/loom-core:20260625-013914) so the bundled loom
// stays version-aligned with the daemon.
const defaultSpawnLoomImage = "registry.harbor.lan/mcp/loom-core:latest"

// resolvePlanStoreWSURL returns the agent-context WebSocket MCP URL injected
// into spawn agent configs. Empty SPAWN_PLAN_STORE_WS_URL falls back to the
// in-cluster default; an explicit value of "disabled" (case-insensitive)
// suppresses plan-store MCP wiring entirely (returns "").
func resolvePlanStoreWSURL() string {
	v := strings.TrimSpace(os.Getenv("SPAWN_PLAN_STORE_WS_URL"))
	if v == "" {
		return defaultPlanStoreWSURL
	}
	if strings.EqualFold(v, "disabled") || strings.EqualFold(v, "off") {
		return ""
	}
	return v
}

// resolveSpawnLoomImage returns the image to COPY the `loom` binary from when
// building the spawn-runtime image. SPAWN_LOOM_IMAGE overrides the default so
// the operator can pin its own loom-core tag without a code change.
func resolveSpawnLoomImage() string {
	if v := strings.TrimSpace(os.Getenv("SPAWN_LOOM_IMAGE")); v != "" {
		return v
	}
	return defaultSpawnLoomImage
}

// defaultCodexModel is the codex model used for spawn exec when
// SPAWN_CODEX_MODEL is unset. codex 0.120.0+ defaults to gpt-5.3-codex, which
// OpenAI DEPRECATED for ChatGPT-account (sign-in-with-ChatGPT) auth — so
// `codex exec` with no --model fails with HTTP 400 "The 'gpt-5.3-codex' model
// is not supported when using Codex with a ChatGPT account." (Mills A2
// kill-test, 2026-06-06: codex authenticated and started a turn, then the API
// rejected the default model). gpt-5.5 is the current model available with
// ChatGPT sign-in per https://developers.openai.com/codex/models (fetched
// 2026-06-06; supported there: gpt-5.5, gpt-5.4, gpt-5.4-mini; ChatGPT-Pro
// only: gpt-5.3-codex-spark; deprecated: gpt-5.2, gpt-5.3-codex).
//
// Aliased to bridge.DefaultCodexModel so the model we PIN and the model we
// BILL a metadata-less turn as are one constant: the Codex parser prices a
// turn at this model when thread.started omits one, and at this model's rate
// when it names one the price snapshot does not know.
const defaultCodexModel = bridge.DefaultCodexModel

// resolveCodexModel returns the codex model id to pin on `codex exec --model`.
// Precedence (highest first):
//
//  1. reqModel — the per-spawn model from spawn.Request.Model (Mills wires this
//     from policy stage_models / LOOM_MILLS_SPAWN_MODEL). This is how the
//     implement stage runs gpt-5.6-terra while plan_slice runs gpt-5.6-sol
//     without a global env flip.
//  2. SPAWN_CODEX_MODEL env — the orchestrator-wide override, so operators can
//     retune WITHOUT a rebuild the next time OpenAI shifts the ChatGPT-supported
//     model set (the failure class that broke the A2 kill-test).
//  3. defaultCodexModel — the compiled-in ChatGPT-account-safe fallback.
func resolveCodexModel(reqModel string) string {
	if m := strings.TrimSpace(reqModel); m != "" {
		return m
	}
	if m := strings.TrimSpace(os.Getenv("SPAWN_CODEX_MODEL")); m != "" {
		return m
	}
	return defaultCodexModel
}

// defaultClaudeModel is the model a claude-code spawn runs when neither the
// request nor SPAWN_CLAUDE_MODEL names one. Pinned rather than left to the
// CLI's own default so a harness upgrade cannot silently move the fleet to a
// different tier. 2026-09-13: Opus 5 for every Claude Code spawn — the Mills
// agent_routing rule already named it for UI work; label-routed spawns
// (`agent/claude-code`) previously fell through to the CLI default.
const defaultClaudeModel = "claude-opus-5"

// resolveClaudeModel mirrors resolveCodexModel for claude-code: the request
// model (Mills agent_routing / stage_models) wins, then the SPAWN_CLAUDE_MODEL
// env, then defaultClaudeModel. Always non-empty, so `claude -p` always
// carries --model.
func resolveClaudeModel(reqModel string) string {
	if m := strings.TrimSpace(reqModel); m != "" {
		return m
	}
	if m := strings.TrimSpace(os.Getenv("SPAWN_CLAUDE_MODEL")); m != "" {
		return m
	}
	return defaultClaudeModel
}

// spawnRequestModel applies the vendor default-model policy before the SDK
// driver sees the request, matching what buildAgentCommand does on the CLI
// path. Vendors without a headless model knob pass the request through.
func spawnRequestModel(agentType, model string) string {
	switch agentType {
	case "claude-code":
		return resolveClaudeModel(model)
	case "codex":
		return resolveCodexModel(model)
	}
	return model
}

// buildAgentCommand constructs the CLI command to run the agent headlessly.
//
// The returned string is executed via `sh -c` (see backend.StreamExec /
// K8sBackend.Exec), so the prompt MUST be shell-quoted, not Go-quoted. Go's
// %q yields a *Go* string literal, which leaves shell metacharacters live:
// backticks and $(...) inside the prompt are evaluated by the shell as
// command substitution before the agent CLI ever sees them. The Mills canary
// SpecDoc wraps the fixture path and backlog id in backticks
// (`testdata/mills-canary/heartbeat.md`, `MILLS-CANARY-...`), so a %q prompt
// produced `sh: testdata/mills-canary/heartbeat.md: Permission denied` /
// `sh: MILLS-CANARY-...: not found` and a prompt with those spans silently
// stripped — the plan_slice stage that blocked the first autonomous merge.
// shellQuote wraps the prompt in single quotes (with '\” escaping) so every
// metacharacter is passed through literally. The buildSDKDriverCommand path
// already uses shellQuote; this is the legacy CLI path catching up.
// defaultClaudeCodeMaxTurns caps a claude-code spawn when the request carries
// no turn budget. The previous hard-coded 50 was exhausted by review-stage
// agents that launch long test suites in the background and then poll with
// cheap one-command turns until the cap kills them mid-wait (2026-08-08:
// three pr_self_review spawns died at error_max_turns ~$1.5-1.9 apiece,
// the entire claude-code backend error count for that day).
const defaultClaudeCodeMaxTurns = 100

// buildAgentCommand builds the headless CLI command. model is the vendor-native
// LLM model id from spawn.Request.Model; empty means "use the vendor default".
// Codex and Claude Code consume it using their native headless --model flags;
// Gemini currently has no model flag in this invocation. maxTurns caps the
// claude-code agent loop; <=0 applies defaultClaudeCodeMaxTurns.
func buildAgentCommand(agentType, task, agentID, model string, maxTurns int) string {
	switch agentType {
	case "claude-code":
		// stream-json emits one JSONL event per line for real-time telemetry parsing.
		// --verbose is mandatory: claude-code 1.x rejects `-p` + `--output-format
		// stream-json` without it ("Error: When using --print, --output-format=
		// stream-json requires --verbose"). Without --verbose the CLI prints
		// that one line and exits 0 *without making any API call*, which is
		// why every Mills spawn showed turn_count=0 / cost=$0 / file_changes=0.
		// --model is always pinned: request > SPAWN_CLAUDE_MODEL > defaultClaudeModel.
		modelArg := " --model " + shellQuote(resolveClaudeModel(model))
		if maxTurns <= 0 {
			maxTurns = defaultClaudeCodeMaxTurns
		}
		return fmt.Sprintf(`claude -p %s --dangerously-skip-permissions --output-format stream-json --verbose --max-turns %d%s`, shellQuote(task), maxTurns, modelArg)
	case "codex":
		// Wrap with EXIT trap so loom session-end fires even without a native hook.
		// The trap is best-effort: if the loom binary is not in the pod PATH,
		// stderr is suppressed via 2>/dev/null and the HUD-side completeSpawn /
		// failSpawn will still call EndSession as a fallback.
		//
		// --dangerously-bypass-approvals-and-sandbox is the codex equivalent of
		// claude-code's --dangerously-skip-permissions: it bypasses BOTH the
		// approval prompts (default approval="auto-edit" auto-approves file
		// edits but still asks for every shell command — git add/commit/push,
		// go test, etc.) AND the network-blocking workspace-write sandbox.
		// Without both bypasses, Mills' implement stage produced empty MRs:
		//   - --sandbox workspace-write alone: file edits land but `git push`
		//     hangs/fails because shell commands need approval AND network.
		//   - --sandbox danger-full-access alone: sandbox is open but codex
		//     still pauses for approval on every shell command in a headless
		//     pod where no human is around to type "yes".
		// The pod itself is already isolated by Kubernetes and runs with a
		// project-scoped GIT_TOKEN; the "EXTREMELY DANGEROUS" warning in the
		// flag's help text refers to running codex on a developer workstation,
		// not in an ephemeral spawn pod. See ab6f8446 / 5742ae07 / 46418c9f
		// for the earlier coupled fixes.
		// --skip-git-repo-check lets codex run in the /workspace clone where
		// the .git directory might be at the working dir rather than a parent.
		//
		// `< /dev/null` is REQUIRED. codex 0.120.0+ `exec` with a prompt arg
		// still inspects stdin: when stdin is a non-TTY pipe that is open but
		// never written/closed, codex reports "Reading additional input from
		// stdin..." and reads it as additional prompt input — then hangs (or,
		// under a session whose stdin EOFs oddly, exits 1) instead of running
		// the single turn. BOTH spawn exec paths leave stdin in exactly that
		// state — the K8s StreamExec sets PodExecOptions{Stdin:false} and the
		// harvester-vm SSH session leaves session.Stdin nil — so without this
		// redirect codex starts a thread + turn then dies with turn_count=1 and
		// no diff. This is the agent-execution gap the Mills A2 kill-test hit on
		// the VM path (and the latent cause of codex's empty implements on k8s).
		// Refs openai/codex#20919; the prompt is already passed as an arg, so no
		// real stdin is needed.
		//
		// `--model` is REQUIRED. codex 0.120.0+ `exec` with no --model uses its
		// CLI default (gpt-5.3-codex), which OpenAI deprecated for ChatGPT-account
		// auth: the Mills A2 kill-test (2026-06-06) saw codex authenticate and
		// start a turn, then fail with HTTP 400 "The 'gpt-5.3-codex' model is not
		// supported when using Codex with a ChatGPT account." resolveCodexModel
		// pins a ChatGPT-supported model (default gpt-5.5) with a SPAWN_CODEX_MODEL
		// env override so a future model-set shift is an env flip, not a rebuild.
		//
		// task is shell-quoted (not Go %q): the result runs via `sh -c`, so
		// backticks / $(...) / $VAR in the prompt would otherwise be evaluated by
		// the shell before codex sees them — the canary's backtick-wrapped
		// fixture path + backlog id died as `sh: ...: not found`. agentID stays
		// %q: it sits inside the trap's single-quoted body and is a controlled
		// identifier, never user text. The model is shell-quoted too (defensive;
		// it is a controlled identifier, never user text).
		//
		// codexAuthPreflight runs between the trap and the CLI: codex 0.144
		// with NO usable credential still starts a thread + turn and then spews
		// `401 Unauthorized: Missing bearer or basic authentication in header`
		// retries before exiting 1 at $0 — and because the always-printed
		// "Reading additional input from stdin..." line dominates the stdout
		// tail, the failure was binned as spawn-stdin-misconfig (escalation
		// #368, 2026-07-22: fresh spawn pods during the 01:54Z fleet-rollout
		// window ran codex against a dangling ~/.codex/auth.json while the
		// SAME secret+image+argv worked before and after the window). The
		// guard runs in-pod at turn time — on the direct exec path AND inside
		// the S4 supervisor's hold.sh — so every attempt re-checks the mount.
		return fmt.Sprintf(
			`trap 'loom agent session-end --agent-id %q --summarize --summary-async --quiet 2>/dev/null' EXIT; %s; codex exec --dangerously-bypass-approvals-and-sandbox --skip-git-repo-check --model %s --json %s < /dev/null`,
			agentID, codexAuthPreflight(AgentHomeDir), shellQuote(resolveCodexModel(model)), shellQuote(task),
		)
	case "gemini":
		return fmt.Sprintf(`gemini -p %s --yolo --output-format stream-json`, shellQuote(task))
	default:
		return fmt.Sprintf(`echo "Unsupported agent type: %s"`, agentType)
	}
}

// codexAuthPreflight returns the POSIX-sh guard that fails a codex spawn fast
// — BEFORE codex starts a turn — when the pod holds no usable credential.
//
// The codex CLI reads ~/.codex/auth.json (injectAgentConfig symlinks it to the
// optional cluster-agent-auth codex-auth-json secret mount) and falls back to
// $OPENAI_API_KEY. Both sources are k8s-Optional by design so a pod always
// starts — but that means an absent/unpopulated mount leaves a DANGLING
// symlink, and codex then runs a full unauthenticated turn: 401 "Missing
// bearer or basic authentication in header" spew, exit 1, $0.00, with the
// misleading "Reading additional input from stdin..." line as the visible
// stdout tail (escalation #368; the 2026-07-22 01:54Z rollout-window pods).
// `[ -s ]` follows the symlink, so a dangling link, a missing file, and an
// empty (unpopulated-key) file all trip the guard. Exit 78 (EX_CONFIG) is an
// honest nonzero for runSpawn's finalizer and does not collide with the S4
// launcher sentinels (231/232/233). The message is the classifier contract:
// pkg/mills/pipeline/spawn_class.go matches "codex auth preflight failed" to
// tag the failure spawn-auth-missing (retryable infra with a rollout-window
// backoff) instead of the stdin-misconfig catch-all.
func codexAuthPreflight(home string) string {
	authPath := home + "/.codex/auth.json"
	return `if [ ! -s ` + authPath + ` ] && [ -z "${OPENAI_API_KEY:-}" ]; then ` +
		`echo "codex auth preflight failed: ` + authPath + ` missing or empty and OPENAI_API_KEY unset ` +
		`(is the cluster-agent-auth secret codex-auth-json key populated and mounted?)" >&2; exit 78; fi`
}

// StopSpawn stops a running spawned agent.
// Pinned CLI versions for reproducible agent container builds.
const (
	claudeCodeVersion = "2.1.270"
	// codexVersion is the @openai/codex npm version baked into the k8s
	// spawn-runtime image (agentCLIInstallLines) and installed onto the
	// harvester-vm substrate (agentCLIInstallShell). It MUST be new enough for
	// every model the orchestrator may pin via resolveCodexModel. OpenAI gates
	// newer model ids behind a minimum Codex CLI version and the API rejects an
	// under-versioned CLI with HTTP 400 "The '<model>' model requires a newer
	// version of Codex. Please upgrade to the latest app or CLI." That 400 broke
	// EVERY per-stage-model spawn on 2026-07-18 (issues #347/#349/#350/#351):
	// Mills wired model=gpt-5.6-sol while the pod ran codex 0.130.0, so codex
	// authenticated, started the turn, then died on the version gate. The
	// operator only saw the misleading stdout tail "Reading additional input
	// from stdin..." because that line follows the failed turn; the true 400 was
	// captured in the spawn record's telemetry.errors.
	//
	// 0.143.0 does NOT clear the gpt-5.6 gate. The earlier "verified locally"
	// claim was run on a newer CLI; a pinned re-test on 2026-07-19 (npx
	// @openai/codex@0.143.0, and in-pod on the deployed
	// spawn-runtime-codex image) reproduces the same 400 for gpt-5.6-sol, so
	// the 0.143.0 pin left every stage_models codex spawn failing at $0 —
	// the plan_slice 64% error rate in /api/mills/telemetry/stages. 0.144.6
	// clears the gate: verified 2026-07-19 in-pod (cluster OAuth,
	// gpt-5.6-sol AND gpt-5.6-terra both complete turns). SPAWN_CODEX_VERSION
	// overrides this at image-build time so a future gate is an env flip, not
	// a rebuild (mirrors SPAWN_CODEX_MODEL) — see resolveCodexVersion.
	//
	// 0.154.0 (2026-09-13): the gpt-6 family. Verified with the PINNED CLI
	// (`npx -y @openai/codex@0.154.0 exec -m gpt-6-astra`, cluster-equivalent
	// ChatGPT OAuth) to complete a turn cleanly; 0.144.6 predates the id.
	codexVersion  = "0.154.0"
	geminiVersion = "0.37.1"
	// goVersion is the Go toolchain baked into the k8s spawn-runtime image
	// (agentRuntimeDockerfile) and installed onto the harvester-vm substrate
	// (agentCLIInstallShell). It matches the repo's go.mod `go` directive and
	// .gitlab-ci.yml GO_VERSION so a spawned agent's `go build`/`go test` never
	// triggers a GOTOOLCHAIN auto-download — and, because the golang images pin
	// GOTOOLCHAIN=local, a runtime image OLDER than the go.mod directive makes
	// every in-spawn `go` command fail outright (the post-!979 1.25.11 image vs
	// the 1.26.4 directive did exactly that).
	goVersion = "1.26.6"
)
