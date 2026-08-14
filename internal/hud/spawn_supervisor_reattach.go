package hud

import (
	"context"
	"fmt"
	"sync"

	"github.com/crb2nu/loom/internal/devbox/backend"
	"github.com/crb2nu/loom/internal/hud/bridge"
	"github.com/crb2nu/loom/internal/spawn"
)

// reattachTimeoutFloorSec bounds the reattach launcher exec when the request
// carries no usable timeout. The reaper enforces the real agent budget in-pod;
// this only keeps the launcher's StreamExec alive long enough to observe the
// durable outcome.
const reattachTimeoutFloorSec = 3600

// redriveOrRun dispatches the legacy re-drive path, honoring the redriveSpawn
// test seam. It is the liveness fallback when a supervised spawn cannot be
// reattached (reaper gone with no outcome, or the pod disappeared).
func (o *SpawnOrchestrator) redriveOrRun(spawnID string, req SpawnRequest) {
	if o.redriveSpawn != nil {
		o.redriveSpawn(spawnID, req)
		return
	}
	go o.runSpawn(spawnID, req)
}

// finishSupervisedOutcome terminalizes a supervised spawn from the reaper's
// recorded exit code. It routes through completeSpawn/failSpawn, so the !1025
// terminal-freeze fence (UpdateUnlessStoppingOrTerminal) and outcome
// exactly-once (deterministic IdempotencyKey + journal readThrough) are
// preserved — the supervisor adds continuity, never a second outcome path.
func (o *SpawnOrchestrator) finishSupervisedOutcome(ctx context.Context, state *SpawnState, exitCode int) {
	if exitCode == 0 {
		o.completeSpawn(ctx, state)
		return
	}
	o.failSpawn(ctx, state, fmt.Sprintf(
		"agent turn exited nonzero after supervised reattach (exit %d)", exitCode))
}

// runSpawnReattach RE-ATTACHES to a supervised spawn's in-pod reaper after a
// controller restart instead of re-driving a fresh agent turn. It tails the
// reaper's durable log for HUD telemetry and blocks on the reaper's recorded
// outcome, then delivers that outcome exactly once. The original
// completion-wrapper/hold process pair is never touched, so its (PID, starttime)
// survives the crash — the S1c process-continuity contract.
//
// It answers the three reattach questions:
//   - still-running  → tail + wait, then collect the outcome;
//   - finished       → collect the already-recorded outcome (exactly once);
//   - died mid-flight → fall back to re-drive (liveness; the attach launcher's
//     out-of-band "orphan" marker — never an exit-code sentinel, so a real
//     agent outcome can take any value without being misrouted).
func (o *SpawnOrchestrator) runSpawnReattach(spawnID string, req SpawnRequest) {
	owner, ok := o.acquireSpawnDriver(spawnID)
	if !ok {
		return
	}
	released := false
	release := func() {
		if !released {
			released = true
			o.releaseSpawnDriver(spawnID, owner)
		}
	}
	defer release()

	state, ok := o.ctrl.Get(spawnID)
	if !ok || state == nil {
		return
	}

	be := o.substrateBackend(req.Substrate)
	sec, streamable := be.(streamExecCapable)
	if !streamable || sec == nil {
		// Only the streaming (k8s) substrate can be reattached; anything else
		// (harvester-vm) falls back to re-drive for liveness.
		release()
		o.redriveOrRun(spawnID, req)
		return
	}

	podName := state.PodName
	if podName == "" {
		podName = "spawn-" + spawnID
	}

	// PASS-1 durable-path dedupe signal (mobile-hud side): prove the replacement
	// controller re-attached to the existing supervised pod rather than creating
	// a second incarnation. The message is verbatim an accepted killtest
	// mobile-hud dedupe phrase (killtest/evidence.go dedupePhrases["mobile-hud"])
	// so no contract change is needed; keep it and the spawn_id on one line.
	o.logger.Info("idempotent spawn re-attach (already exists)",
		"spawn_id", spawnID,
		"pod", podName,
		"mode", "supervised",
		"agent_type", req.AgentType,
	)

	// Best-effort live telemetry: tail-fed parser so the HUD sees turns/cost
	// during a reattached run. The durable outcome file — not the parser — is
	// the terminal source of truth.
	acc := bridge.NewSpawnTelemetryAccumulatorWithPublisher(
		newSpawnTelemetryPublisher(o.sseHub), state.SessionID, state.AgentID)
	o.telemetry.Store(spawnID, acc)
	broadcaster := SpawnEventBroadcaster(func(eventType, agentID string, data any) {
		o.broadcastTelemetryEvent(eventType, agentID, data)
	})
	parser := newSpawnParser(req.AgentType, acc, state.AgentID, spawnID, broadcaster, o.logger)

	execTimeoutSec := req.TimeoutMinutes * 60
	if execTimeoutSec <= 0 {
		execTimeoutSec = reattachTimeoutFloorSec
	}
	execTimeoutSec += req.CompletionHoldSeconds + 60

	attachCmd := supervisorLaunchCommand(supervisorStateDir(spawnID), supervisorModeAttach)
	reattachExec := o.reattachExecFn
	if reattachExec == nil {
		reattachExec = defaultReattachExec
	}
	// Attach-mode status arrives OUT-OF-BAND as supervisorMarkerPrefix stdout
	// lines intercepted here, never via the launcher exit code — so a genuinely
	// recorded agent outcome of 231/232 is collected as an outcome, not
	// misrouted to the orphan/malformed paths (they were previously in-band
	// sentinels on the same exit-code channel). Marker lines are consumed and
	// NOT fed to the telemetry parser. The mutex covers the (theoretical)
	// callback-vs-return goroutine handoff inside the stream transport.
	var markerMu sync.Mutex
	markerKind := supervisorMarkerNone
	markerCode := 0
	execResult, execErr := reattachExec(owner, sec, podName, attachCmd, execTimeoutSec, func(line []byte) {
		if kind, code, ok := parseSupervisorMarkerLine(string(line)); ok {
			markerMu.Lock()
			markerKind, markerCode = kind, code
			markerMu.Unlock()
			return
		}
		acc.Touch()
		parser.HandleLine(line)
	})

	// A stop/terminal transition may have raced in while we tailed; never
	// resurrect it (acquire/UpdateUnlessStoppingOrTerminal both fence it).
	if !o.spawnDriverActive(spawnID, owner) {
		return
	}

	if execErr != nil {
		o.resolveReattachFromDurableState(spawnID, req, state, release, "attach exec error")
		return
	}

	markerMu.Lock()
	kind, code := markerKind, markerCode
	markerMu.Unlock()
	switch kind {
	case supervisorMarkerOutcome:
		o.telemetry.Delete(spawnID)
		o.finishSupervisedOutcome(context.Background(), state, code)
	case supervisorMarkerOrphan:
		// Reaper died before recording an outcome — died mid-flight. Re-drive
		// for liveness (the continuity gate cannot pass on this path).
		o.logger.Warn("supervised reattach found orphaned reaper (no outcome); re-driving for liveness",
			"spawn_id", spawnID, "pod", podName)
		release()
		o.redriveOrRun(spawnID, req)
	case supervisorMarkerMalformed:
		o.telemetry.Delete(spawnID)
		o.failSpawn(context.Background(), state,
			"supervised spawn recorded a malformed outcome; failing after reattach")
	default:
		// The stream ended without any marker — launcher exec timeout (the
		// stream layer's synthesized 124), a truncated stream, or an
		// interrupted launcher. The durable supervisor state, not this
		// stream, is the source of truth; re-probe it.
		o.logger.Warn("supervised reattach stream ended without a status marker; resolving from durable supervisor state",
			"spawn_id", spawnID, "pod", podName, "exec_exit_code", execResult.ExitCode)
		o.resolveReattachFromDurableState(spawnID, req, state, release, "stream ended without marker")
	}
}

// defaultReattachExec is the production reattach launcher exec: a detached
// streaming exec (StreamExec deliberately ignores its ctx so a controller-side
// timeout cannot sever a healthy reattach) that tails the reaper's log and
// blocks until the attach launcher prints its out-of-band status marker and
// exits (always 0 once a marker is delivered; the marker carries the outcome).
func defaultReattachExec(owner *spawnDriverOwner, sec streamExecCapable, podName, attachCmd string, timeoutSec int, onLine func([]byte)) (*backend.ExecResult, error) {
	return backend.StreamExec(owner.ctx,
		sec.Clientset(), sec.RestConfig(), sec.Namespace(), sec.NFSFlush(),
		backend.StreamExecOpts{
			ContainerID: podName,
			Command:     attachCmd,
			TimeoutSec:  timeoutSec,
			OnLine:      onLine,
		},
	)
}

// resolveReattachFromDurableState decides recovery when the reattach launcher
// stream could not deliver a status marker — the exec itself errored (transport
// drop, pod disappeared) or the stream ended marker-less (launcher exec
// timeout, truncated stream). It re-probes the DURABLE supervisor state so a
// still-live turn is never wrongly terminalized:
//
//   - outcome recorded → collect it (exactly once, via finishSupervisedOutcome);
//   - reaper alive     → leave the spawn non-terminal and recoverable (the next
//     recovery pass / reconciler deadline owns it) — never fail a live turn;
//   - gone / probe err → re-drive for liveness (continuity cannot pass here).
func (o *SpawnOrchestrator) resolveReattachFromDurableState(spawnID string, req SpawnRequest, state *spawn.State, release func(), reason string) {
	probeFn := o.probeSupervisorFn
	if probeFn == nil {
		probeFn = o.probeSupervisor
	}
	probe, perr := probeFn(context.Background(), req.Substrate, spawnID)
	switch {
	case perr == nil && probe.OutcomePresent:
		o.telemetry.Delete(spawnID)
		o.finishSupervisedOutcome(context.Background(), state, probe.OutcomeExit)
	case perr == nil && probe.ReaperAlive:
		// Turn is still running; leave the durable record non-terminal so the
		// next recovery pass / reconciler deadline handles it. Do NOT fail it —
		// that would delete the pod and kill a live turn.
		o.logger.Warn("supervised reattach could not observe outcome but supervisor is alive; leaving spawn recoverable",
			"spawn_id", spawnID, "reason", reason)
	default:
		o.logger.Warn("supervised reattach could not observe outcome and supervisor is gone; re-driving for liveness",
			"spawn_id", spawnID, "reason", reason, "probe_error", perr)
		release()
		o.redriveOrRun(spawnID, req)
	}
}
