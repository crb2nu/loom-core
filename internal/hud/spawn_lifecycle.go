package hud

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/crb2nu/loom/internal/devbox/backend"
	"github.com/crb2nu/loom/internal/hud/bridge"
	"github.com/crb2nu/loom/internal/spawn"
)

// RecoverSpawns delegates recovery to the spawn controller. Previously this
// blindly marked non-terminal spawns as failed ("stale after HUD restart").
// Now the controller recovers from the store and a subsequent Reconcile call
// will check actual pod status — fixing the stale-after-restart bug.
func (o *SpawnOrchestrator) RecoverSpawns() error {
	return o.RecoverSpawnsContext(context.Background())
}

// RecoverSpawnsContext performs the one startup recovery pass. Failures remain
// retryable; callers must not start reconciliation or serve spawn mutations
// until this returns nil, otherwise owned durable rows can be cached as peers.
func (o *SpawnOrchestrator) RecoverSpawnsContext(ctx context.Context) error {
	if o == nil || o.ctrl == nil {
		return nil
	}
	o.recoveryMu.Lock()
	if o.recovered {
		o.recoveryMu.Unlock()
		return nil
	}
	if err := o.ctrl.RecoverFromStore(ctx); err != nil {
		o.logger.Warn("failed to recover spawns", "error", err)
		o.recoveryMu.Unlock()
		return err
	}
	o.recovered = true
	o.recoveryMu.Unlock()
	o.recoverStoppingSpawns()
	o.resumePreRuntimeSpawns()
	o.recoverInterruptedSpawns()
	return nil
}

// SetDegraded marks the spawn backend degraded (true) or healthy (false).
// Degraded means the HUD is serving but the startup spawn-state recovery
// pass has not completed — see the degraded field doc for what is gated.
func (o *SpawnOrchestrator) SetDegraded(v bool) {
	if o == nil {
		return
	}
	o.degraded.Store(v)
}

// Degraded reports whether the spawn backend is degraded because startup
// spawn-state recovery has not completed yet.
func (o *SpawnOrchestrator) Degraded() bool {
	return o != nil && o.degraded.Load()
}

func (o *SpawnOrchestrator) acquireSpawnDriver(spawnID string) (*spawnDriverOwner, bool) {
	if o == nil || o.ctrl == nil {
		return nil, false
	}
	o.driversMu.Lock()
	defer o.driversMu.Unlock()
	if o.drivers == nil {
		o.drivers = make(map[string]*spawnDriverOwner)
	}
	if _, exists := o.drivers[spawnID]; exists {
		if o.logger != nil {
			o.logger.Warn("spawn lifecycle already has an active driver", "spawn_id", spawnID)
		}
		return nil, false
	}
	state, ok := o.ctrl.Get(spawnID)
	if !ok || state == nil || spawn.IsTerminal(state.Status) || state.StopRequestedAt != nil {
		return nil, false
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	owner := &spawnDriverOwner{ctx: ctx, cancel: cancel, done: make(chan struct{})}
	o.drivers[spawnID] = owner
	return owner, true
}

func (o *SpawnOrchestrator) spawnDriverActive(spawnID string, owner *spawnDriverOwner) bool {
	if o == nil || owner == nil || o.ctrl == nil {
		return false
	}
	o.driversMu.Lock()
	defer o.driversMu.Unlock()
	if o.drivers[spawnID] != owner || owner.stopRequested || owner.ctx.Err() != nil {
		return false
	}
	state, ok := o.ctrl.Get(spawnID)
	return ok && state != nil && !spawn.IsTerminal(state.Status) && state.StopRequestedAt == nil
}

// updateSpawnFromDriver is the only non-terminal controller transition used
// by runSpawn. StopSpawn takes the same lock while requesting cancellation,
// making the terminal fence atomic with respect to late driver writes.
func (o *SpawnOrchestrator) updateSpawnFromDriver(
	ctx context.Context,
	spawnID string,
	owner *spawnDriverOwner,
	update func(*SpawnState),
) (*SpawnState, bool, error) {
	if o == nil || owner == nil || o.ctrl == nil {
		return nil, false, nil
	}
	o.driversMu.Lock()
	defer o.driversMu.Unlock()
	if o.drivers[spawnID] != owner || owner.stopRequested || owner.ctx.Err() != nil {
		return nil, false, nil
	}
	updated, ok, err := o.ctrl.UpdateUnlessStoppingOrTerminal(ctx, spawnID, update)
	if err != nil {
		return &updated, false, err
	}
	if !ok {
		return nil, false, nil
	}
	return &updated, true, nil
}

func (o *SpawnOrchestrator) setStopCleanupError(spawnID string, owner *spawnDriverOwner, err error) {
	if err == nil || owner == nil {
		return
	}
	o.driversMu.Lock()
	if o.drivers[spawnID] == owner {
		owner.stopCleanupErr = errors.Join(owner.stopCleanupErr, err)
	}
	o.driversMu.Unlock()
}

func (o *SpawnOrchestrator) releaseSpawnDriver(spawnID string, owner *spawnDriverOwner) {
	if owner == nil {
		return
	}
	defer owner.cancel(nil)
	if o == nil {
		close(owner.done)
		return
	}
	o.driversMu.Lock()
	if o.drivers[spawnID] != owner {
		o.driversMu.Unlock()
		close(owner.done)
		return
	}
	if !owner.stopRequested {
		delete(o.drivers, spawnID)
		o.driversMu.Unlock()
		close(owner.done)
		return
	}
	cleanupDone := owner.stopCleanupDone
	o.driversMu.Unlock()

	// The stop caller owns cleanup of the pod known at cancellation time.
	// A pod returned late from Start is cleaned by runSpawn before it exits.
	// Waiting for both makes the stopped state a truthful cleanup barrier.
	if cleanupDone != nil {
		<-cleanupDone
	}

	var cleanupErr error
	o.driversMu.Lock()
	if o.drivers[spawnID] == owner {
		delete(o.drivers, spawnID)
		cleanupErr = owner.stopCleanupErr
	}
	o.driversMu.Unlock()
	if cleanupErr == nil {
		if stopped, ok, err := o.ctrl.CompleteStop(context.Background(), spawnID); err != nil {
			owner.stopCleanupErr = err
		} else if ok {
			o.finishStoppedSpawn(context.Background(), &stopped)
		}
	}
	close(owner.done)
}

// StopSpawn stops a running spawned agent.
func (o *SpawnOrchestrator) Spawn(ctx context.Context, req SpawnRequest) (string, error) {
	// While the backend is degraded (startup recovery pending because the
	// spawn-state store was unreachable at boot), refuse mutations: a fresh
	// record registered now could collide with a durable keyed row that
	// recovery has not loaded yet.
	if o.Degraded() {
		return "", errSpawnBackendDegraded
	}

	builder := o.launchSpecBuilder
	if builder == nil {
		builder = productionSpawnLaunchSpecBuilder{orchestrator: o}
	}
	var err error
	req, err = builder.Build(ctx, req)
	if err != nil {
		return "", err
	}

	if existing := o.existingActiveSpawnForRequest(req); existing != "" {
		o.logger.Info("returning existing active spawn for idempotent request",
			"spawn_id", existing,
			"run_id", req.Metadata["LOOM_MILLS_RUN_ID"],
			"stage", req.Metadata["LOOM_MILLS_STAGE"],
		)
		return existing, nil
	}

	// Check concurrent limit.
	if o.ctrl.ActiveCount() >= o.maxConcurrent {
		return "", fmt.Errorf("max concurrent spawns reached (%d)", o.maxConcurrent)
	}

	// Delegate validation and ID generation to the controller.
	spawnID, dispatch, err := o.ctrl.Register(ctx, req)
	if err != nil {
		return "", err
	}
	if !dispatch {
		o.logger.Info("returning existing keyed spawn without duplicate dispatch",
			"spawn_id", spawnID, "idempotency_key", req.IdempotencyKey)
		return spawnID, nil
	}

	if o.metrics != nil {
		o.metrics.AgentSpawnTotal.Add(ctx, 1,
			metric.WithAttributes(
				attribute.String("agent_type", req.AgentType),
				attribute.String("outcome", "initiated"),
			),
		)
		o.metrics.SpawnedAgentActive.Add(ctx, 1)
		o.activeSpawnMetrics.Store(spawnID, struct{}{})
	}

	launcher := o.launcher
	if launcher == nil {
		launcher = productionSpawnLauncher{orchestrator: o}
	}
	launcher.Launch(spawnID, req)

	return spawnID, nil
}

// runSpawn executes the full spawn lifecycle in a background goroutine.
func (o *SpawnOrchestrator) runSpawn(spawnID string, req SpawnRequest) {
	owner, ok := o.acquireSpawnDriver(spawnID)
	if !ok {
		return
	}
	defer o.releaseSpawnDriver(spawnID, owner)

	ctx, span := o.tracer.Start(owner.ctx, "agent.spawn",
		trace.WithAttributes(
			attribute.String("agent.type", req.AgentType),
			attribute.String("project", req.Project),
			attribute.String("namespace", req.Namespace),
			attribute.String("spawn_id", spawnID),
		),
	)
	defer span.End()

	state, _ := o.ctrl.Get(spawnID)
	// Spawn validates new requests before registration. Validate again for
	// recovered rows so corrupt or hand-authored persisted state cannot create
	// an unexpectedly long hold when the driver is re-attached after restart.
	if err := validateCompletionHoldSeconds(req.CompletionHoldSeconds); err != nil {
		o.failSpawn(ctx, state, err.Error())
		return
	}

	// Pick the substrate backend once per spawn so Build/Start/Exec/Stop
	// in this goroutine all hit the same impl. Slice 2d.
	be := o.substrateBackend(req.Substrate)
	if be == nil {
		o.failSpawn(ctx, state, fmt.Sprintf("no backend registered for substrate %q (default %q)", req.Substrate, o.defaultSubstrate))
		return
	}

	if state.AuthRetryPending {
		cleanupCtx, cancel := context.WithTimeout(ctx, spawnDriverStopTimeout)
		err := o.stopSpawnRuntime(cleanupCtx, be, state, state.PodName)
		cancel()
		if err != nil {
			o.failSpawn(ctx, state, fmt.Sprintf("replace runtime after auth failure: %v", err))
			return
		}
	}

	// Resolve which cluster auth path this spawn will use. Populated on
	// the state so HUD detail endpoints can surface "cluster_api_key" vs
	// "cluster_service_account" without introspecting the pod. In Slice 2a
	// the resolver returns a default based on agent type; Slice 2b layers
	// in cluster OAuth detection.
	authAccount := o.pickAuthAccount(req.AgentType)
	state, ok, transitionErr := o.updateSpawnFromDriver(ctx, spawnID, owner, func(current *SpawnState) {
		if len(current.AuthFailures) == 0 {
			current.AuthMode = resolveAuthMode(req.AgentType)
			current.AuthAccount = authAccount
			if req.AgentType == "claude-code" && claudeAuthPolicy() == "api-then-oauth" {
				current.AuthMode = spawn.AuthModeClusterAPIKey
				current.AuthAccount = ""
			}
		} else if current.AuthRetryPending {
			current.PodName = ""
			current.Supervised = false
			current.Status = spawn.StatusPending
			current.AuthRetryPending = false
		}
		// Which credential in the vendor's pool this spawn runs under; the
		// secret env below is built from the same value so state and pod agree.
	})
	if transitionErr != nil {
		o.failSpawn(ctx, state, fmt.Sprintf("persist spawn auth mode: %v", transitionErr))
		return
	}
	if !ok {
		return
	}

	if req.AgentType == "claude-code" && state.AuthMode == spawn.AuthModeClusterOAuth && state.AuthAccount == "" {
		o.failSpawn(ctx, state, "no healthy Claude OAuth account; wait for exclusion expiry or clear repaired credentials")
		return
	}

	// Resolve the project name to an on-disk location plus the
	// workspace-relative path used inside the spawned pod. Bare names like
	// "loom-core" are searched under the standard buckets (services/, libs/,
	// ...) so monorepo repos resolve without explicit registration.
	projectPath := spawnProjectPath(req.Project)
	projectDir, projectRel, resolveErr := resolveProjectPath(o.workspaceRoot, projectPath)
	if resolveErr != nil {
		// In git-clone mode the on-disk workspace copy is used ONLY to
		// fingerprint the repo for Dockerfile generation; the spawn pod's init
		// container clones the repo fresh as its source of truth. So a repo that
		// is not staged on the workspace PVC need not be a hard failure — fall
		// back to a lexical path and let generateDockerfile degrade to the
		// generic agent-runtime image. This lets Mills target additional
		// (services-group) repos without pre-staging them on the workspace
		// volume. For nfs/tar-pipe modes the local copy IS the source of truth,
		// so keep the hard failure there.
		lexRel, lexErr := lexicalProjectRel(projectPath)
		if !o.gitCloneMode() || lexErr != nil {
			msg := fmt.Sprintf("project resolution failed: %v", resolveErr)
			if lexErr != nil {
				msg = fmt.Sprintf("%s (lexical fallback: %v)", msg, lexErr)
			}
			o.failSpawn(ctx, state, msg)
			return
		}
		o.logger.Warn("spawn project not staged on workspace; using git-clone fallback with generic runtime image",
			"project", req.Project, "resolved_rel", lexRel, "resolve_error", resolveErr)
		projectDir = filepath.Join(o.workspaceRoot, lexRel)
		projectRel = lexRel
	}
	podProjectDir := "/workspace/" + projectRel

	// Step 1: Detect project environment and generate Dockerfile.
	state, ok, transitionErr = o.updateSpawnFromDriver(ctx, spawnID, owner, func(current *SpawnState) {
		current.Status = SpawnStatusBuilding
	})
	if transitionErr != nil {
		o.failSpawn(ctx, state, fmt.Sprintf("persist spawn building transition: %v", transitionErr))
		return
	}
	if !ok {
		return
	}
	o.broadcastSpawnEvent("agent.spawn.building", state)

	_, buildSpan := o.tracer.Start(ctx, "agent.spawn.image_build")

	df, dfErr := o.generateDockerfile(projectDir, req.AgentType)
	if dfErr != nil {
		buildSpan.End()
		if !o.spawnDriverActive(spawnID, owner) {
			return
		}
		o.failSpawn(ctx, state, fmt.Sprintf("dockerfile generation failed: %v", dfErr))
		return
	}
	if !o.spawnDriverActive(spawnID, owner) {
		buildSpan.End()
		return
	}

	// ContextDir is used by the K8s backend for filepath.Rel (string-only,
	// no local filesystem access needed). In git-clone mode the backend
	// derives the project name from the path and clones the repo.
	buildTag := agentRuntimeBuildTag(req.AgentType, df)
	releaseBuildSlot, slotErr := o.acquireBuildSlot(ctx)
	if slotErr != nil {
		buildSpan.End()
		if !o.spawnDriverActive(spawnID, owner) {
			return
		}
		o.failSpawn(ctx, state, fmt.Sprintf("image build queue failed: %v", slotErr))
		return
	}
	buildResult, err := be.Build(ctx, backend.BuildOpts{
		Tag:            buildTag,
		Dockerfile:     df,
		ContextDir:     projectDir,
		PreferExisting: true,
	})
	releaseBuildSlot()
	buildSpan.End()
	if err != nil {
		if !o.spawnDriverActive(spawnID, owner) {
			return
		}
		o.failSpawn(ctx, state, fmt.Sprintf("image build failed: %v", err))
		return
	}
	if !o.spawnDriverActive(spawnID, owner) {
		return
	}

	// Step 2: Start K8s pod.
	o.logger.Info("build completed, starting pod", "spawn_id", spawnID, "image", buildResult.ImageTag)
	_, podSpan := o.tracer.Start(ctx, "agent.spawn.pod_create")
	env := buildSpawnPodEnv(req, state.AgentID, spawnID)
	startOpts := spawnRuntimeStartIdentityOpts(
		spawnID, state.AgentID, state.DriverOwnerID, state.StartedAt, false,
	)
	startOpts.ExtraLabels[spawn.AgentTypeLabel] = spawn.KubernetesLabelValue(req.AgentType)
	startOpts.ExtraLabels[spawn.ProjectLabel] = spawn.KubernetesLabelValue(req.Project)
	startOpts.ImageTag = buildResult.ImageTag
	startOpts.WorkDir = podProjectDir
	startOpts.GitProjectPath = projectRel
	startOpts.Env = env
	startOpts.SecretEnv = agentSecretEnvVarsForMode(req.AgentType, state.AuthAccount, state.AuthMode)
	startOpts.SecretMounts = agentSecretMounts(req.AgentType)
	startOpts.MemoryMB = req.MemoryMB
	startOpts.CPUs = req.CPUs
	startOpts.Network = true
	startOpts.Branch = req.Branch
	startOpts.BaseBranch = req.BaseBranch
	startOpts.AgentCLIInstallCmd = agentCLIInstallShell(req.AgentType)
	startOpts.CachePVCs = applySpawnGoCache(env, spawnGoCachePVC())
	startResult, err := o.startSpawnPod(ctx, be, req, spawnID, startOpts)
	podSpan.End()
	if err != nil {
		if !o.spawnDriverActive(spawnID, owner) {
			return
		}
		o.failSpawn(ctx, state, fmt.Sprintf("pod creation failed: %v", err))
		return
	}

	state, ok, transitionErr = o.updateSpawnFromDriver(ctx, spawnID, owner, func(current *SpawnState) {
		current.PodName = startResult.ContainerID
	})
	if transitionErr != nil {
		o.cleanupLateSpawn(be, spawnID, startResult.ContainerID, owner)
		o.failSpawn(ctx, state, fmt.Sprintf("persist spawn pod handle: %v", transitionErr))
		return
	}
	if !ok {
		o.cleanupLateSpawn(be, spawnID, startResult.ContainerID, owner)
		return
	}

	// Step 3: Inject pre-authed configs (with a short timeout to avoid hanging on SPDY issues).
	_, cfgSpan := o.tracer.Start(ctx, "agent.spawn.config_inject")
	cfgCtx, cfgCancel := context.WithTimeout(ctx, 30*time.Second)
	o.logger.Info("injecting agent config", "spawn_id", spawnID, "pod", startResult.ContainerID, "agent_type", req.AgentType)
	if err := o.injectAgentConfig(cfgCtx, be, startResult.ContainerID, req.AgentType, podProjectDir); err != nil {
		cfgCancel()
		cfgSpan.End()
		if !o.spawnDriverActive(spawnID, owner) {
			return
		}
		o.failSpawn(ctx, state, fmt.Sprintf("config injection failed: %v", err))
		return
	}
	cfgCancel()
	cfgSpan.End()

	// Step 4: Register agent session (before exec so the agent has session context).
	// Capture sessionID so per-tool-call events emitted by the accumulator below
	// can stamp the correct session and land in the Live Sessions panel row for
	// this spawn. StartSession is idempotent — repeat spawns under the same
	// namespace reuse the same session.
	_, sessSpan := o.tracer.Start(ctx, "agent.spawn.session_register")
	// Persist the session ID on the durable state so terminal transitions
	// (completeSpawn/failSpawn → persistTelemetrySummary) can write the
	// telemetry summary under a real, existing session. The state is flushed
	// to the controller below when we mark the spawn running. A failed
	// registration is logged inside registerSpawnSession (not swallowed): an
	// empty session id here makes persistTelemetrySummary skip the write,
	// silently dropping the spawn's turn-level telemetry — the exact blind
	// spot that hid the in-VM codex failure during the Mills A2 kill-test.
	if !o.spawnDriverActive(spawnID, owner) {
		sessSpan.End()
		return
	}
	sessionID := o.registerSpawnSession(req, state.AgentID)
	sessSpan.End()

	// Mark running and broadcast event.
	state, ok, transitionErr = o.updateSpawnFromDriver(ctx, spawnID, owner, func(current *SpawnState) {
		current.SessionID = sessionID
		current.Status = SpawnStatusRunning
	})
	if transitionErr != nil {
		o.cleanupLateSpawn(be, spawnID, startResult.ContainerID, owner)
		o.failSpawn(ctx, state, fmt.Sprintf("persist spawn running transition: %v", transitionErr))
		return
	}
	if !ok {
		o.cleanupLateSpawn(be, spawnID, startResult.ContainerID, owner)
		return
	}
	o.broadcastSpawnEvent("agent.spawn.running", state)

	// Step 5: Start heartbeat loop for spawn visibility.
	heartbeatCtx, heartbeatCancel := context.WithCancel(ctx)
	go o.runHeartbeatLoop(heartbeatCtx, state)

	// Step 6: Execute agent CLI (or SDK driver) with real-time JSONL telemetry parsing.
	o.logger.Info("executing agent",
		"spawn_id", spawnID,
		"agent_type", req.AgentType,
		"pod", startResult.ContainerID,
		"use_sdk_driver", req.UseSDKDriver,
		"multi_turn", req.MultiTurn,
	)
	_, execSpan := o.tracer.Start(ctx, "agent.spawn.agent_exec")

	// Choose between the legacy CLI path and the embedded loom-spawn-driver
	// Node.js sidecar. When UseSDKDriver is set we inject the bundled driver
	// into the pod and invoke it instead of the raw agent CLI.
	var agentCmd string
	if req.UseSDKDriver {
		injectCtx, injectCancel := context.WithTimeout(ctx, 30*time.Second)
		if err := o.injectSDKDriver(injectCtx, be, startResult.ContainerID); err != nil {
			injectCancel()
			execSpan.End()
			heartbeatCancel()
			if !o.spawnDriverActive(spawnID, owner) {
				return
			}
			o.failSpawn(ctx, state, fmt.Sprintf("inject spawn driver: %v", err))
			return
		}

		// In multi-turn mode pre-create the control file so the driver's
		// fs.watch fires immediately on the first REST-driven append. The
		// REST handlers (slice 8c) call injectControlMessage to push
		// `{type:"message"|"interrupt"|"shutdown"}` lines into this file.
		var controlFilePath string
		if req.MultiTurn {
			if err := o.injectControlFile(injectCtx, be, startResult.ContainerID, spawnID); err != nil {
				injectCancel()
				execSpan.End()
				heartbeatCancel()
				if !o.spawnDriverActive(spawnID, owner) {
					return
				}
				o.failSpawn(ctx, state, fmt.Sprintf("inject control file: %v", err))
				return
			}
			controlFilePath = controlFilePathForSpawn(spawnID)
		}
		injectCancel()

		agentCmd = buildSDKDriverCommand(
			req.AgentType,
			req.TaskDescription,
			state.AgentID,
			spawnID,
			podProjectDir,
			controlFilePath,
			spawnRequestModel(req.AgentType, req.Model),
			req.MaxTurns,
			req.MaxCostUSD,
		)
	} else {
		// Claude Code and Codex both accept a model override in their headless
		// modes. Gemini has no equivalent flag in this invocation.
		if m := strings.TrimSpace(req.Model); m != "" && req.AgentType != "codex" && req.AgentType != "claude-code" {
			o.logger.Info("spawn model override ignored: agent has no headless model knob",
				"spawn_id", spawnID, "agent_type", req.AgentType, "model", m)
		}
		agentCmd = buildAgentCommand(req.AgentType, req.TaskDescription, state.AgentID, req.Model, req.MaxTurns)
	}
	agentCmd = wrapAgentCommandWithCompletionHold(agentCmd, req.CompletionHoldSeconds)

	// S4 pod-owned execution supervisor. On the streaming (k8s) substrate, run
	// the wrapped agent+hold under a detached, PID-1-reparented in-pod reaper
	// that records the outcome durably, and have this exec only LAUNCH + TAIL it.
	// A controller crash then leaves the original completion-wrapper/hold process
	// pair intact and a restarted controller RE-ATTACHES (see runSpawnReattach)
	// instead of re-driving — the S1c process-continuity contract. Persist
	// State.Supervised BEFORE the exec so a crash mid-turn recovers via the
	// supervised path. Gated + streaming-only; the buffered harvester-vm path
	// keeps the direct exec (out of scope: k8s substrate only).
	supervised := false
	if _, streamCapable := be.(streamExecCapable); o.supervisedExecution && streamCapable {
		if err := o.injectSupervisorAssets(ctx, be, startResult.ContainerID, spawnID, agentCmd, req.TimeoutMinutes*60); err != nil {
			execSpan.End()
			heartbeatCancel()
			if !o.spawnDriverActive(spawnID, owner) {
				return
			}
			o.failSpawn(ctx, state, fmt.Sprintf("inject execution supervisor: %v", err))
			return
		}
		state, ok, transitionErr = o.updateSpawnFromDriver(ctx, spawnID, owner, func(current *SpawnState) {
			current.Supervised = true
		})
		if transitionErr != nil {
			execSpan.End()
			heartbeatCancel()
			o.failSpawn(ctx, state, fmt.Sprintf("persist supervised spawn flag: %v", transitionErr))
			return
		}
		if !ok {
			execSpan.End()
			heartbeatCancel()
			return
		}
		agentCmd = supervisorLaunchCommand(supervisorStateDir(spawnID), supervisorModeLaunch)
		supervised = true
		o.logger.Info("launched pod-owned execution supervisor",
			"spawn_id", spawnID, "pod", startResult.ContainerID)
	}

	// Create telemetry accumulator and JSONL parser for real-time parsing.
	// Wire it to the SSE hub via spawnTelemetryPublisher so per-tool-call
	// events (tool.call.start/end) reach /api/events. Without the publisher
	// the accumulator silently dropped them and the Live Sessions panel
	// only ever showed empty session rows for in-cluster spawn agents.
	acc := bridge.NewSpawnTelemetryAccumulatorWithPublisher(
		newSpawnTelemetryPublisher(o.sseHub),
		state.SessionID,
		state.AgentID,
	)
	o.telemetry.Store(spawnID, acc)

	broadcaster := SpawnEventBroadcaster(func(eventType string, agentID string, data any) {
		o.broadcastTelemetryEvent(eventType, agentID, data)
	})
	completionGuard := newCompletionGuardSink(acc)
	parser := newSpawnParser(req.AgentType, completionGuard, state.AgentID, spawnID, broadcaster, o.logger)

	var execResult *backend.ExecResult
	var execErr error
	usedStreaming := false

	// Cancellable exec context so the budget + liveness watchers can abort the
	// run. WithCancelCause lets the liveness watchdog attach errSpawnStalled so
	// the finalize path can distinguish a stalled-zombie cancellation from a
	// normal one (first cancel call wins, so the cleanup cancel(nil) below
	// never clobbers the watchdog's cause).
	execCtx, execCancel := context.WithCancelCause(ctx)

	// Budget watcher: polls the telemetry accumulator every 5s and cancels the
	// exec context when a configured budget is exceeded. The watcher terminates
	// when the exec returns via the done channel.
	watcherDone := make(chan struct{})
	if req.MaxCostUSD > 0 || req.MaxTurns > 0 {
		go o.runBudgetWatcher(execCtx, spawnID, req, acc, execCancel, watcherDone)
	} else {
		close(watcherDone)
	}

	// Liveness watchdog: streaming (K8s) path only. A zombie pod stuck in
	// Phase=Running with a dead agent process emits no further JSONL lines, so
	// acc.LastActivity() stops advancing. When it goes stale beyond the stall
	// timeout the watcher cancels exec with errSpawnStalled → failSpawn cleans
	// the pod → the Mills operator's poll sees a terminal failure and
	// auto-retries, instead of waiting out its 30-minute deadline pending a
	// manual operator restart. The buffered (harvester-vm) path is excluded:
	// it has no mid-flight telemetry, so freshness is not a valid signal there;
	// its Exec TimeoutSec bounds the run instead.
	_, streamCapable := be.(streamExecCapable)
	runLiveness := streamCapable && parser != nil && o.livenessStallTimeout > 0
	livenessDone := make(chan struct{})
	if runLiveness {
		go o.runLivenessWatcher(execCtx, spawnID, acc, o.livenessStallTimeout, execCancel, livenessDone)
	} else {
		close(livenessDone)
	}

	// On the supervised path the launcher must OUTLIVE the in-pod reaper (agent
	// timeout enforced in-pod by the reaper, plus the completion hold), so give
	// the launcher exec extra slack; otherwise it could time out and fail the
	// spawn while the reaper is still finishing the hold. Zero = unbounded.
	execTimeoutSec := req.TimeoutMinutes * 60
	if supervised && execTimeoutSec > 0 {
		execTimeoutSec += req.CompletionHoldSeconds + 60
	}

	// Use streaming exec if the substrate's backend supports it (K8s only)
	// and we have a parser; otherwise fall back to buffered Exec. The
	// harvester-vm backend doesn't implement streamExecCapable so its
	// spawns always take the buffered path.
	if sec, ok := be.(streamExecCapable); ok && parser != nil {
		usedStreaming = true
		execResult, execErr = backend.StreamExec(execCtx,
			sec.Clientset(), sec.RestConfig(), sec.Namespace(), sec.NFSFlush(),
			backend.StreamExecOpts{
				ContainerID: startResult.ContainerID,
				Command:     agentCmd,
				WorkDir:     podProjectDir,
				TimeoutSec:  execTimeoutSec,
				OnLine: func(line []byte) {
					// Stamp liveness on every streamed line: receiving output
					// proves the agent process is alive, which the parser's
					// telemetry mutations alone would not capture for lines that
					// carry no structured telemetry.
					acc.Touch()
					parser.HandleLine(line)
				},
			},
		)
	} else {
		// Fallback: buffered exec (no real-time telemetry). Pass the same
		// env that landed on the substrate at Start time. For K8s pods env
		// is already on the container so this is redundant; for harvester-vm
		// the SSH shell prefix is the delivery channel — without this the
		// agent CLI runs without spawn env (DEVBOX_BACKEND, LOOM_HUD_URL,
		// resolved SecretEnv API keys, etc.).
		execResult, execErr = be.Exec(execCtx, backend.ExecOpts{
			ContainerID: startResult.ContainerID,
			Command:     agentCmd,
			WorkDir:     podProjectDir,
			Env:         env,
			TimeoutSec:  req.TimeoutMinutes * 60,
		})
	}
	// Stop the watchers and release the exec context. cancel(nil) is a no-op
	// if a watcher already cancelled with a cause (first call wins), so the
	// liveness watchdog's errSpawnStalled survives for the finalize path.
	if req.MaxCostUSD > 0 || req.MaxTurns > 0 {
		close(watcherDone)
	}
	if runLiveness {
		close(livenessDone)
	}
	execCancel(nil)
	execSpan.End()
	heartbeatCancel()

	// Buffered (non-streaming) path — harvester-vm and any other backend that
	// is not streamExecCapable. The parser never saw the agent's output here,
	// so without this the spawn shows turn_count=0 regardless of what the agent
	// actually did, and a nonzero exit / stderr is invisible. This buffered
	// path silently swallowing the result is what hid the in-VM codex failure
	// during the Mills A2 first-autonomous-merge kill-test (empty diff,
	// turn_count=0, spawn marked "completed"). Make it observable:
	//   1. feed the captured stdout tail through the parser for best-effort
	//      telemetry (full real-time telemetry still needs a stream-capable
	//      backend — harvester-vm is not one yet),
	//   2. log a full exec summary (exit code, durations, stdout/stderr tails).
	if !usedStreaming && execResult != nil {
		if parser != nil && execResult.StdoutTail != "" {
			for _, line := range strings.Split(execResult.StdoutTail, "\n") {
				if strings.TrimSpace(line) != "" {
					parser.HandleLine([]byte(line))
				}
			}
		}
		o.logger.Info("buffered agent exec finished",
			"spawn_id", state.SpawnID,
			"agent_type", req.AgentType,
			"exit_code", execResult.ExitCode,
			"duration_ms", execResult.DurationMs,
			"stdout_lines", execResult.StdoutLines,
			"stderr_lines", execResult.StderrLines,
			"stderr_tail", execResult.StderrTail)
	}

	// Capture agent output as a context entry for session visibility. Include
	// the stderr tail when present — on a failing agent CLI that is where the
	// actionable diagnostic lives.
	if execResult != nil && (execResult.StdoutTail != "" || execResult.StderrTail != "") {
		go func() {
			truncated := execResult.StdoutTail
			if execResult.StderrTail != "" {
				truncated += "\n--- stderr ---\n" + execResult.StderrTail
			}
			if len(truncated) > 8000 {
				truncated = truncated[:8000] + "\n... (truncated)"
			}
			_ = o.agentBridge.ContextAdd("", []map[string]any{{
				"entry_type": "finding",
				"title":      fmt.Sprintf("Agent output (%s)", state.SpawnID),
				"content":    truncated,
			}})
		}()
	}

	// Step 7: Finalize based on exec result.
	if execErr != nil {
		if !o.spawnDriverActive(spawnID, owner) {
			return
		}
		// Prefer the cancellation cause when the liveness watchdog tripped, so
		// the failure reads as a diagnosable stall rather than a generic
		// "context canceled" (the streaming exec surfaces watchdog cancellation
		// as a context error).
		reason := fmt.Sprintf("agent execution failed: %v", execErr)
		if cause := context.Cause(execCtx); cause != nil && errors.Is(cause, errSpawnStalled) {
			reason = cause.Error()
		}
		o.failSpawn(ctx, state, reason)
		return
	}
	if execResult != nil && o.spawnDriverActive(spawnID, owner) {
		var evidence *claudeParseResult
		if cp, ok := parser.(*ClaudeJSONLParser); ok {
			evidence = cp.authSnapshot()
		}
		if o.handleClaudeAuthCompletion(ctx, state, evidence, execResult.ExitCode, strings.Split(execResult.StderrTail, "\n")) {
			return
		}
	}
	// A nonzero exit from the agent CLI is a failure even though Backend.Exec
	// returns a nil Go error for command-level nonzero exits. Previously only
	// execErr was checked, so a failing agent on the buffered path fell through
	// to completeSpawn and the spawn showed as "completed" with no signal.
	if msg, failed := bufferedExecFailure(execResult); failed {
		if !o.spawnDriverActive(spawnID, owner) {
			return
		}
		o.logger.Warn("agent CLI exited nonzero",
			"spawn_id", state.SpawnID,
			"agent_type", req.AgentType,
			"exit_code", execResult.ExitCode,
			"stderr_tail", execResult.StderrTail,
			"stdout_tail", execResult.StdoutTail)
		o.failSpawn(ctx, state, msg)
		return
	}
	// A clean process exit is not sufficient when the structured stream ended
	// mid-tool. In particular, Codex can report exit 0 after item.started while
	// omitting item.completed if its transport closes abruptly. Completing that
	// spawn would publish a false success and let Mills advance. The guard wraps
	// every parser through SpawnEventSink, so this is model-independent.
	if guardErr := spawnCompletionError(completionGuard, parser); guardErr != nil {
		if !o.spawnDriverActive(spawnID, owner) {
			return
		}
		o.failSpawn(ctx, state, guardErr.Error())
		return
	}
	if !o.spawnDriverActive(spawnID, owner) {
		return
	}
	o.completeSpawn(ctx, state)
}

// spawnCompletionError combines model-independent incomplete-tool detection
// with parser-specific terminal failures. Codex exec can exit 0 after emitting
// turn.failed, so its parser must get an opportunity to veto completion.
//
// The parser's terminal error is consulted FIRST, because the open-tool-call
// count is a SYMPTOM of an aborted turn, never its cause. A turn that fails
// mid-batch leaves every in-flight call open, and reporting "18 tool call(s)
// still open after agent process exit" as the verdict buried the actual reason
// (2026-07-26: plan_slice spawn-26e4557605de escalated with 18 open calls and
// no trace of why the turn died). Real exit reasons that arrive OUTSIDE the
// parser — a nonzero agent CLI exit, the exec deadline, the liveness watchdog —
// already win earlier in the finalizer, before this is reached.
//
// A clean turn end still sweeps as !1180 intended: turn.completed closes the
// stragglers, so the guard has nothing to report and this returns nil.
//
// A recorded stream abort is a VERDICT, not merely corroborating detail. It was
// previously consulted only when tool calls happened to be left open, so a
// stream that emitted a fatal error, completed no turn, and had nothing in
// flight reached completeSpawn as a success — the exact case the codex error
// handler already describes as "a spawn completed without producing work". The
// veto requires positive evidence: a parser reports a reason only when it
// recorded a fatal diagnostic AND nothing in the run recovered from it (codex:
// no turn completed; gemini: no terminal result). Absence of a terminal event
// on its own is NOT enough to fail here — that case keeps the older open-tool
// rule below, so this mints no failure for a quiet stream.
func spawnCompletionError(guard *completionGuardSink, parser SpawnLineParser) error {
	if reporter, ok := parser.(interface{ terminalError() error }); ok {
		if err := reporter.terminalError(); err != nil {
			return err
		}
	}
	open := guard.openToolCallCount()
	// The stream carried a fatal error and nothing recovered from it. Lead with
	// that cause, and keep the open-call count as corroborating detail when
	// there is one, instead of emitting the count alone (or nothing at all).
	if reporter, ok := parser.(interface{ streamAbortReason() string }); ok {
		if reason := strings.TrimSpace(reporter.streamAbortReason()); reason != "" {
			if open == 0 {
				return fmt.Errorf("agent stream aborted: %s", reason)
			}
			return fmt.Errorf(
				"agent stream aborted: %s (%d tool call(s) still open at stream end)",
				reason, open)
		}
	}
	if open == 0 {
		return nil
	}
	// Genuinely unexplained truncation: the process ended mid-turn with no
	// terminal event and no diagnostic. Still fails closed — completing here
	// would publish a false success and let Mills advance on partial work.
	return guard.completionError()
}

// bufferedExecFailure reports whether a buffered ExecResult represents an agent
// CLI failure (nonzero exit) and builds an operator-facing message that prefers
// the stderr tail (where the actionable diagnostic lives), falling back to the
// stdout tail. Backend.Exec returns a nil Go error for command-level nonzero
// exits, so the spawn finalizer must inspect ExitCode explicitly — not doing so
// is what let failing harvester-vm spawns report "completed" during the A2
// kill-test. Returns ("", false) for a nil result or a clean (exit 0) run.
func bufferedExecFailure(execResult *backend.ExecResult) (string, bool) {
	if execResult == nil || execResult.ExitCode == 0 {
		return "", false
	}
	msg := fmt.Sprintf("agent CLI exited %d", execResult.ExitCode)
	if s := strings.TrimSpace(execResult.StderrTail); s != "" {
		msg += ": " + s
	} else if s := strings.TrimSpace(execResult.StdoutTail); s != "" {
		msg += " (no stderr; stdout: " + s + ")"
	}
	return msg, true
}

// completionGuardSink tracks tool starts that have not received a matching
// completion while delegating the full telemetry interface to the canonical
// accumulator. Embedding keeps this guard model-independent: Claude, Codex,
// Gemini, and the SDK driver all pass through the same SpawnEventSink contract.
type completionGuardSink struct {
	SpawnEventSink

	mu       sync.Mutex
	openTool map[string]string // id → tool name, for actionable failure messages
}

func newCompletionGuardSink(delegate SpawnEventSink) *completionGuardSink {
	return &completionGuardSink{
		SpawnEventSink: delegate,
		openTool:       make(map[string]string),
	}
}

func (s *completionGuardSink) StartToolCall(id, name, serverName string) {
	s.mu.Lock()
	s.openTool[id] = name
	s.mu.Unlock()
	s.SpawnEventSink.StartToolCall(id, name, serverName)
}

func (s *completionGuardSink) CompleteToolCall(id string, durationMs int, exitCode *int, errMsg string) {
	s.SpawnEventSink.CompleteToolCall(id, durationMs, exitCode, errMsg)
	s.mu.Lock()
	delete(s.openTool, id)
	s.mu.Unlock()
}

func (s *completionGuardSink) openToolCallCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.openTool)
}

func (s *completionGuardSink) completionError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.openTool) == 0 {
		return nil
	}
	// Name the open calls so the escalation issue points at the actual
	// stragglers instead of forcing a Loki dig (spawn-d9889e05e2e9 took an
	// item-id diff across parser logs to identify). Sorted for determinism,
	// capped so a pathological stream cannot bloat the failure message.
	open := make([]string, 0, len(s.openTool))
	for id, name := range s.openTool {
		open = append(open, fmt.Sprintf("%s[%s]", name, id))
	}
	sort.Strings(open)
	if len(open) > 5 {
		open = append(open[:5], "…")
	}
	return fmt.Errorf(
		"agent execution incomplete: %d tool call(s) still open after agent process exit (open: %s)",
		len(s.openTool), strings.Join(open, ", "))
}

func validateCompletionHoldSeconds(seconds int) error {
	if seconds < 0 || seconds > spawn.MaxCompletionHoldSeconds {
		return fmt.Errorf(
			"completion_hold_seconds must be between 0 and %d", spawn.MaxCompletionHoldSeconds)
	}
	return nil
}

// wrapAgentCommandWithCompletionHold appends a bounded, foreground `sleep N`
// after a successful agent command. A nonzero agent status exits immediately,
// so a failed agent is never masked or held. The explicit final exit preserves
// the successful agent status after the hold; an interrupted/failed hold stays
// a failure rather than publishing a false completion.
func wrapAgentCommandWithCompletionHold(command string, seconds int) string {
	if seconds == 0 {
		return command
	}
	duration := strconv.Itoa(seconds)
	return command +
		`; loom_agent_exit=$?; ` +
		`if [ "$loom_agent_exit" -ne 0 ]; then exit "$loom_agent_exit"; fi; ` +
		`sleep ` + duration + `; loom_hold_exit=$?; ` +
		`if [ "$loom_hold_exit" -ne 0 ]; then exit "$loom_hold_exit"; fi; ` +
		`exit "$loom_agent_exit"`
}

// SendControlMessage appends a control command to a running multi-turn
// spawn's JSONL control file. The spawn-driver's tail loop picks up the new
// line within ~200ms (fs.watch + poll fallback) and dispatches it to the
// active SDK Query or Codex Thread.
//
// Errors are returned as wrapped sentinels so REST handlers can distinguish
// 404 (not found), 409 (not running), and 400 (not multi-turn / invalid
// command) from 5xx backend failures:
//
//   - spawn.ErrSpawnNotFound         → 404
//   - spawn.ErrSpawnNotRunning       → 409
//   - spawn.ErrSpawnNotMultiTurn     → 400
//   - spawn.ErrInvalidControlCommand → 400
//
// Any other error is a backend/exec failure and should surface as 500.
func (o *SpawnOrchestrator) SendControlMessage(ctx context.Context, spawnID string, cmd spawn.ControlCommand) error {
	if err := validateControlCommand(cmd); err != nil {
		return err
	}

	state, ok := o.ctrl.Get(spawnID)
	if !ok {
		return fmt.Errorf("%w: %s", spawn.ErrSpawnNotFound, spawnID)
	}

	if state.Status != SpawnStatusRunning {
		return fmt.Errorf("%w: %s is %s", spawn.ErrSpawnNotRunning, spawnID, state.Status)
	}

	if !state.Request.MultiTurn || !state.Request.UseSDKDriver {
		return fmt.Errorf("%w: %s was not spawned with multi_turn=true", spawn.ErrSpawnNotMultiTurn, spawnID)
	}

	if state.PodName == "" {
		return fmt.Errorf("spawn %s has no pod name; cannot inject control command", spawnID)
	}

	if err := o.injectControlMessage(ctx, o.substrateBackend(state.Request.Substrate), state.PodName, spawnID, cmd); err != nil {
		return fmt.Errorf("inject control command for %s: %w", spawnID, err)
	}

	o.logger.Info("injected spawn control command",
		"spawn_id", spawnID,
		"agent_id", state.AgentID,
		"command_type", cmd.Type,
	)
	return nil
}

// validateControlCommand enforces the driver contract: Type must be one of
// the known discriminators and "message" requires non-empty Text.
func validateControlCommand(cmd spawn.ControlCommand) error {
	switch cmd.Type {
	case spawn.ControlCommandMessage:
		if strings.TrimSpace(cmd.Text) == "" {
			return fmt.Errorf("%w: message text is required", spawn.ErrInvalidControlCommand)
		}
		return nil
	case spawn.ControlCommandInterrupt, spawn.ControlCommandShutdown:
		return nil
	case "":
		return fmt.Errorf("%w: type is required", spawn.ErrInvalidControlCommand)
	default:
		return fmt.Errorf("%w: unknown type %q", spawn.ErrInvalidControlCommand, cmd.Type)
	}
}

// failSpawn marks a spawn as failed, cleans up the K8s pod, and broadcasts the event.
func (o *SpawnOrchestrator) failSpawn(ctx context.Context, state *SpawnState, reason string) {
	if o == nil || o.ctrl == nil || state == nil {
		return
	}
	var terminalTelemetry *bridge.SpawnTelemetry
	if accVal, exists := o.telemetry.Load(state.SpawnID); exists {
		acc := accVal.(*bridge.SpawnTelemetryAccumulator)
		snap := acc.Snapshot()
		terminalTelemetry = &snap
	}
	o.driversMu.Lock()
	updated, ok, persistErr := o.ctrl.UpdateUnlessStoppingOrTerminal(ctx, state.SpawnID, func(current *spawn.State) {
		// Attach partial telemetry snapshot (valuable for debugging failures).
		if terminalTelemetry != nil {
			current.Telemetry = terminalTelemetry
		}
		current.Status = SpawnStatusFailed
		current.Error = reason
		now := time.Now()
		current.EndedAt = &now
	})
	o.driversMu.Unlock()
	if persistErr != nil {
		o.logger.Error("failed to persist terminal spawn failure",
			"spawn_id", state.SpawnID, "reason", reason, "error", persistErr)
		return
	}
	if !ok {
		return
	}
	o.telemetry.Delete(state.SpawnID)
	state = &updated
	podName := state.PodName

	// Persist final telemetry summary to the agent-context session.
	o.persistTelemetrySummary(state, string(SpawnStatusFailed))

	// Clean up the pod/VM on the substrate it was created on.
	if podName != "" {
		if err := o.stopSpawnRuntime(ctx, o.substrateBackend(state.Request.Substrate), state, podName); err != nil {
			o.logger.Warn("failed to clean up pod on spawn failure",
				"spawn_id", state.SpawnID, "pod", podName, "error", err)
		}
	}

	if o.metrics != nil {
		if _, owned := o.activeSpawnMetrics.LoadAndDelete(state.SpawnID); owned {
			o.metrics.SpawnedAgentActive.Add(ctx, -1)
		}
		o.metrics.AgentSpawnTotal.Add(ctx, 1,
			metric.WithAttributes(
				attribute.String("agent_type", state.Request.AgentType),
				attribute.String("outcome", "failed"),
			),
		)
	}

	// Record partial spawn telemetry metrics (still valuable for debugging failures).
	if o.metrics != nil && state.Telemetry != nil {
		o.recordSpawnTelemetryMetrics(ctx, state)
	}

	o.logger.Error("spawn failed", "spawn_id", state.SpawnID, "reason", reason)
	o.broadcastSpawnEvent("agent.spawn.failed", state)

	// Record failure and end the agent session. Resolve the spawn's session
	// so agent_context_add lands the error entry instead of being rejected
	// for an empty session_id (skip the write when no session resolves).
	failSessionID := o.resolveSpawnSessionID(state)
	if o.agentBridge != nil {
		go func() {
			if failSessionID != "" {
				_ = o.agentBridge.ContextAdd(failSessionID, []map[string]any{{
					"entry_type": "error",
					"title":      "Spawn failed: " + state.SpawnID,
					"content":    reason,
				}})
			}
			summarize := false
			o.agentBridge.EndSession(bridge.SessionEndParams{AgentID: state.AgentID, Summarize: &summarize})
		}()
	}
}

// DeleteSpawn removes a terminal spawn from the controller and persistent store.
func (o *SpawnOrchestrator) DeleteSpawn(ctx context.Context, spawnID string) error {
	return o.ctrl.Delete(ctx, spawnID)
}

// ListSpawns returns all spawn states.
func (o *SpawnOrchestrator) ListSpawns() []*SpawnState {
	return o.ctrl.List()
}

// GetSpawn returns a specific spawn state.
func (o *SpawnOrchestrator) GetSpawn(spawnID string) (*SpawnState, bool) {
	return o.ctrl.Get(spawnID)
}

// Wait blocks until the given spawn reaches a terminal state
// (completed / failed / stopped) or ctx is canceled. Returns the terminal
// SpawnState on success; ctx.Err() on cancellation; an error if the spawn
// ID does not exist.
//
// Implemented via polling (spawn.IsTerminal) every waitPollInterval rather
// than subscribing to the SSE hub because the hub's fan-out shape doesn't
// let a single waiter filter by spawn_id without delivery contention with
// browser clients. Polling is cheap — spawn state lookups are O(1) map
// reads — and spawn lifecycles are measured in minutes, so 500ms poll
// granularity is invisible to callers.
func (o *SpawnOrchestrator) Wait(ctx context.Context, spawnID string) (*SpawnState, error) {
	if _, ok := o.ctrl.Get(spawnID); !ok {
		return nil, fmt.Errorf("spawn %s not found", spawnID)
	}

	ticker := time.NewTicker(waitPollInterval)
	defer ticker.Stop()
	for {
		state, ok := o.ctrl.Get(spawnID)
		if !ok {
			return nil, fmt.Errorf("spawn %s disappeared while waiting", spawnID)
		}
		if spawn.IsTerminal(state.Status) {
			return state, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

// waitPollInterval is the cadence at which Wait() re-checks terminal
// state. Tuned for the minute-scale spawn lifecycle; low enough that
// Wait returns quickly after completion, high enough to keep polling
// overhead near zero.
const waitPollInterval = 500 * time.Millisecond

// Projects returns the configured project list for spawn pickers.
func (o *SpawnOrchestrator) Projects() []string { return o.projects }

// gitCloneMode reports whether the spawn backend clones the repo into the pod
// (as opposed to relying on an nfs/tar-pipe workspace mount). In git-clone mode
// the pod's init container is the repo's source of truth, so project resolution
// can fall back to a lexical path when the repo is absent from the workspace
// mount (see runSpawn). Empty syncMode is treated as non-git-clone so tests and
// legacy wirings keep the strict hard-fail behavior.
func (o *SpawnOrchestrator) gitCloneMode() bool { return o.syncMode == "git-clone" }

// projectConfigured reports whether project matches the configured spawn
// allow-list, accepting either the configured name (e.g. "mcp-go") or its
// workspace-path form (e.g. "libs/mcp-go"). Empty allow-lists skip the check
// at the call site so bespoke wirings keep their permissive behavior.
func projectConfigured(project string, configured []string) bool {
	project = strings.TrimSpace(project)
	if project == "" {
		return false
	}
	// Mills' home-lane dispatcher sends the bucket-qualified project
	// ("services/loom-core") while SPAWN_PROJECTS lists bare names
	// ("loom-core"), so a bare candidate must also match its qualified
	// forms. Regression 2026-08-26: the exact-match-only version rejected
	// every home-lane spawn ("project \"services/loom-core\" is not
	// configured for spawning") while libs/mcp-go passed only via its
	// spawnProjectPaths entry.
	projectPath := spawnProjectPath(project)
	for _, candidate := range configured {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		candidatePath := spawnProjectPath(candidate)
		if project == candidate || projectPath == candidatePath {
			return true
		}
		// A bare candidate admits any bucket-qualified form of the same
		// repo name; a bogus bucket still fails later at resolve/clone.
		if !strings.ContainsRune(candidatePath, '/') && path.Base(projectPath) == candidatePath {
			return true
		}
	}
	return false
}

// NewSpawnOrchestratorForTest builds a minimal SpawnOrchestrator backed by
// the given controller. Intended for external-package tests that need
// ListSpawns / GetSpawn / Wait but can't construct a full orchestrator
// because backend/sseHub/etc. fields are unexported. Do not use in
// production code paths.
func NewSpawnOrchestratorForTest(ctrl *spawn.K8sController) *SpawnOrchestrator {
	return &SpawnOrchestrator{ctrl: ctrl, logger: slog.Default()}
}

// Cluster-scoped secret names. These hold credentials tied to the cluster
// identity, decoupled from any developer's Mac Keychain. See
// .loom/87-product-spec-session-spawning-weaver-2026-04-19.md §AUTH.
const (
	// ClusterAgentAPIKeysSecret holds vendor API keys (ANTHROPIC_API_KEY,
	// OPENAI_API_KEY, GEMINI_API_KEY) and the Gemini service-account JSON,
	// all scoped to the cluster's identity.
	ClusterAgentAPIKeysSecret = "cluster-agent-api-keys"

	// ClusterAgentAuthSecret holds cluster-owned OAuth tokens for agents
	// that support subscription auth (Claude, Codex). Populated by
	// `loom auth cluster-login`; refreshed in-cluster by mcp-auth-refresher.
	// Unused in Slice 2a (API-key only); Slice 2b adds the OAuth mounts.
	ClusterAgentAuthSecret = "cluster-agent-auth"

	// GeminiSAKeyName is the Secret key that holds the full Google service
	// account JSON for Gemini. When present it is mounted as a file at
	// GeminiSAMountPath/sa.json so Gemini CLI can pick it up via the
	// standard GOOGLE_APPLICATION_CREDENTIALS env var.
	GeminiSAKeyName   = "GOOGLE_APPLICATION_CREDENTIALS_JSON"
	GeminiSAMountPath = "/home/agent/.gcp"
	GeminiSAFilename  = "sa.json"

	// AgentHomeDir is the writable HOME for spawned agents. The runtime
	// image creates user `agent` (uid 1000) with this as its home; secret
	// mounts and CLI state files all live under here so the non-root
	// process can traverse the path. Stay in sync with
	// agentRuntimeDockerfile().
	AgentHomeDir = "/home/agent"
)
