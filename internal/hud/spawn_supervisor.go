package hud

// S4 durable execution: a pod-owned execution supervisor/reaper.
//
// Problem (loom-core#300 / S1c process-continuity contract): the legacy path
// has mobile-hud EXEC into the spawn pod and drive the agent CLI + a foreground
// `sleep N` completion hold. That exec session is a child of mobile-hud's
// streaming connection, so force-deleting mobile-hud severs it; recovery then
// RE-DRIVES a fresh agent turn (new completion-wrapper + hold PID/starttime).
// Outcome-exactly-once still holds (deterministic IdempotencyKey + journal
// readThrough), but PROCESS continuity does not — the original pair dies and any
// partial side effects belong to a dead lineage. The S1c runbook's stronger
// contract requires the SAME `(hold, wrapper)` `(PID, starttime)` to survive
// BOTH crashes, so the re-drive path is expected to FAIL it.
//
// Fix (this file): move execution ownership INTO the pod. A detached reaper
// (launched via `setsid`, reparented to PID 1, independent of the exec/
// controller lifecycle) owns the completion-wrapper/hold pair, records the turn
// outcome durably on the pod filesystem, and lingers as a live, non-matching
// process so exiting cannot leave a zombie under the pod's non-reaping
// `sleep infinity` PID 1. mobile-hud only LAUNCHES the reaper and TAILS its log;
// when mobile-hud dies the launcher dies but the reaper+wrapper+hold survive. A
// restarted mobile-hud RE-ATTACHES (tails status, collects the outcome exactly
// once) instead of re-driving.
//
// Delivery mechanism: no new image dependency. The wrapped agent command
// (unchanged wrapAgentCommandWithCompletionHold output — the exact
// completion-wrapper the S1c killtest matches on) and the reaper body are
// written into the pod as files via the existing base64 injection pattern, so
// neither string ever appears in a long-lived process argv. The launcher (the
// mobile-hud streaming exec) launches `setsid sh <reaper.sh>` and tails the log.

import (
	"context"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"

	"github.com/crb2nu/loom/internal/devbox/backend"
)

const (
	// supervisorBaseDir is the pod-local root that holds per-spawn supervisor
	// state. The per-spawn subdirectory is created lazily by the launcher,
	// which runs as the unprivileged agent user — so /opt/loom MUST be
	// agent-owned in the runtime image (agentRuntimeDockerfile). It is NOT
	// safe to assume injectSDKDriver created it: the legacy CLI path (Mills
	// spawns) never injects the SDK driver, and on images without an
	// agent-owned /opt/loom the launcher's mkdir fails with EACCES. Before
	// the launcher guard below, that failure was silently swallowed and the
	// outcome wait-loop spun for the full exec deadline with ZERO output —
	// the 2026-07-18/19 plan_slice wedge (20-60min per attempt at $0).
	supervisorBaseDir = "/opt/loom/supervisor"

	// supervisorReaperOrphanExit is the LAUNCH-mode launcher exit code meaning
	// "the reaper died before writing an outcome" (died mid-flight). It is only
	// meaningful on the initial runSpawn path, whose finalizer treats ANY
	// nonzero exit as an honest failure — there is no sentinel switch there, so
	// an agent whose real exit code happens to equal this value is still simply
	// a failure. The ATTACH (reattach) path deliberately does NOT use exit-code
	// sentinels: a recorded agent outcome could legitimately be any value
	// (including 231/232), so attach mode reports out-of-band via
	// supervisorMarkerPrefix stdout markers and always exits 0. See
	// supervisorLaunchCommand.
	supervisorReaperOrphanExit = 231
	// supervisorMalformedOutcomeExit is the LAUNCH-mode launcher exit code for
	// "the outcome file existed but did not parse as an integer". Launch-mode
	// only, same rationale as supervisorReaperOrphanExit.
	supervisorMalformedOutcomeExit = 232
	// supervisorStateDirExit is the LAUNCH-mode launcher exit code for "the
	// per-spawn state dir could not be created" (EACCES on a runtime image
	// whose /opt/loom is not agent-owned, read-only fs, disk full). Failing
	// fast here — with the mkdir error on stderr — is what turns that
	// misconfiguration into an instant classified failure instead of the
	// silent full-deadline wedge described on supervisorBaseDir.
	supervisorStateDirExit = 233
)

// supervisorMarkerPrefix opens every out-of-band status line the ATTACH-mode
// launcher prints on stdout. The reattach driver intercepts these lines before
// the telemetry parser; the launcher's exit code carries no outcome in attach
// mode (always 0 on a delivered marker), so the sentinel channel and the
// recorded agent outcome channel are disjoint — a genuinely recorded outcome of
// 231/232 is collected as an outcome, never misread as orphan/malformed.
//
// Marker bodies:
//   - "outcome <n>" — the reaper durably recorded agent exit <n>; collect it.
//   - "orphan"      — reaper died with no outcome (died mid-flight); re-drive.
//   - "malformed"   — outcome file exists but is not a canonical integer; fail.
//
// Trust note: the tailed agent log shares this stdout stream, so an agent
// could in principle print a spoofed marker. That grants nothing — the agent
// already controls its own exit code and could write the outcome file
// directly; the marker is a transport, not an authenticator.
const supervisorMarkerPrefix = "LOOM_SUPERVISOR_V1 "

// supervisorMarkerKind discriminates the parsed attach-mode marker.
type supervisorMarkerKind int

const (
	supervisorMarkerNone supervisorMarkerKind = iota
	supervisorMarkerOutcome
	supervisorMarkerOrphan
	supervisorMarkerMalformed
)

// parseSupervisorMarkerLine parses one streamed line as an attach-mode marker.
// ok=false means the line is not a marker (forward it to the telemetry
// parser). A marker-prefixed line with a non-canonical outcome is classified
// malformed (fail closed) rather than silently dropped.
func parseSupervisorMarkerLine(line string) (kind supervisorMarkerKind, exitCode int, ok bool) {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, supervisorMarkerPrefix) {
		return supervisorMarkerNone, 0, false
	}
	body := strings.TrimSpace(strings.TrimPrefix(trimmed, supervisorMarkerPrefix))
	switch {
	case body == "orphan":
		return supervisorMarkerOrphan, 0, true
	case body == "malformed":
		return supervisorMarkerMalformed, 0, true
	case strings.HasPrefix(body, "outcome "):
		raw := strings.TrimSpace(strings.TrimPrefix(body, "outcome "))
		code, err := strconv.Atoi(raw)
		if err != nil || code < 0 || raw != strconv.Itoa(code) {
			return supervisorMarkerMalformed, 0, true
		}
		return supervisorMarkerOutcome, code, true
	default:
		// Unknown marker body (e.g. the reaper's diagnostic log lines use a
		// different prefix precisely to avoid landing here) — not a marker.
		return supervisorMarkerNone, 0, false
	}
}

// supervisorMode selects whether the launcher script starts the reaper (initial
// spawn) or only attaches to an already-running one (restart recovery).
type supervisorMode int

const (
	// supervisorModeLaunch starts the detached reaper (idempotently) and then
	// tails from the start of the log so the live parser sees every JSONL line.
	supervisorModeLaunch supervisorMode = iota
	// supervisorModeAttach never starts a reaper; it tails from the END of the
	// log (a reattach must not replay already-parsed telemetry) and waits for
	// the durable outcome the reaper records.
	supervisorModeAttach
)

// supervisorStateDir returns the pod-local per-spawn supervisor state directory.
// Spawn IDs are sanitized to a quote-free shell subset so the path is always
// safe to embed unquoted in the generated scripts.
func supervisorStateDir(spawnID string) string {
	return supervisorBaseDir + "/" + sanitizeSpawnID(spawnID)
}

// reaperScript is the body of reaper.sh, run detached as `setsid sh reaper.sh`.
// It runs the completion-wrapper (hold.sh) with stdio redirected off the exec
// session, waits for it (reaping it so it never becomes a zombie), records the
// exit code atomically, then lingers as `sleep infinity` — a live, non-matching
// process — so its own exit cannot orphan a zombie under the pod's non-reaping
// PID 1. timeoutSec > 0 bounds the agent turn in-pod (the reaper part of
// "supervisor/reaper") so an over-running turn is reaped here, not only by the
// controller-side deadline backstop.
//
// The reaper body deliberately contains NEITHER of the two substrings the S1c
// killtest uses to identify the completion wrapper (`loom_agent_exit=$?;` and
// `; sleep <N>; loom_hold_exit=$?;`); those live only in hold.sh, read at
// runtime via command substitution, so the killtest sees exactly one wrapper.
func reaperScript(stateDir string, timeoutSec int) string {
	dir := shellQuote(stateDir)
	var launch string
	if timeoutSec > 0 {
		launch = fmt.Sprintf(
			`if command -v timeout >/dev/null 2>&1; then `+
				`timeout -k 10 %d sh -c "$(cat %s/hold.sh)" >%s/agent.log 2>&1 </dev/null & `+
				`else sh -c "$(cat %s/hold.sh)" >%s/agent.log 2>&1 </dev/null & fi`,
			timeoutSec, dir, dir, dir, dir)
	} else {
		launch = fmt.Sprintf(
			`sh -c "$(cat %s/hold.sh)" >%s/agent.log 2>&1 </dev/null &`, dir, dir)
	}
	return strings.Join([]string{
		"set -u",
		"D=" + dir,
		"mkdir -p \"$D\"",
		// The reaper records its OWN pid authoritatively, as its first action,
		// rather than letting the launcher guess it from `$!`. `$!` is fragile
		// across the setsid/timeout process topology: on a setsid variant that
		// forks, or on the timeout-wrapped path, the launcher's `$!` can name a
		// short-lived parent, not the reaper shell — the launcher would then read
		// that pid as dead and misfire the orphan check on a SUCCESSFUL run. `$$`
		// here is the reaper shell itself, which is also the same pid that lingers
		// after `exec sleep infinity`, so the launcher's liveness probe stays
		// correct from launch through linger. Written atomically (tmp+rename).
		`printf '%s\n' "$$" > "$D/reaper.pid.tmp" 2>/dev/null && mv "$D/reaper.pid.tmp" "$D/reaper.pid" 2>/dev/null`,
		launch,
		"child=$!",
		`wait "$child"`,
		"code=$?",
		// Atomic outcome record. A write failure (e.g. disk full) must NOT be
		// silent: without an outcome file the attach launcher blocks until its
		// exec timeout and then resolves via the durable probe, so surface the
		// cause in the reaper's own log for the operator. The diagnostic line
		// deliberately uses a prefix DIFFERENT from supervisorMarkerPrefix so a
		// tailing attach launcher never parses it as a status marker.
		`if ! { printf '%s\n' "$code" > "$D/outcome.tmp" && mv "$D/outcome.tmp" "$D/outcome"; } 2>>"$D/agent.log"; then`,
		`	printf '%s\n' "LOOM-SUPERVISOR-DIAG outcome write failed (agent exit $code); attach will resolve via probe/timeout" >> "$D/agent.log" 2>/dev/null`,
		"fi",
		// Linger; the pod teardown SIGKILLs us. Never `exit` — that would orphan
		// a zombie under the pod's `sleep infinity` PID 1.
		"exec sleep infinity",
	}, "\n") + "\n"
}

// supervisorLaunchCommand returns the inline shell command the mobile-hud
// streaming exec runs. In launch mode it idempotently starts the detached
// reaper (an mkdir lock makes a double-launch impossible even under a racing
// recovery); the reaper records its own pid. Both modes then tail agent.log and
// block until the reaper records the outcome. A reaper observed dead is only an
// orphan if the outcome is still absent (the reaper writes the outcome before
// exiting), so a completed run is never misreported as orphaned.
//
// Outcome delivery differs by mode, deliberately:
//
//   - LAUNCH (initial runSpawn): the launcher exits WITH the recorded outcome
//     code, and 231/232 flag orphan/malformed. That is safe in-band because
//     runSpawn's finalizer has no sentinel switch — every nonzero exit is an
//     honest failure, so an agent that really exits 231 is still just a
//     failure, never misrouted.
//   - ATTACH (restart reattach): the outcome channel must carry ANY recorded
//     agent exit (including 231/232), so status is reported OUT-OF-BAND via
//     supervisorMarkerPrefix stdout markers and the launcher always exits 0
//     once a marker is printed. runSpawnReattach switches on the marker, not
//     the exit code, so sentinel and outcome channels are disjoint.
//
// This command's argv also contains neither killtest wrapper substring — the
// reaper body lives in reaper.sh, referenced only by path here.
func supervisorLaunchCommand(stateDir string, mode supervisorMode) string {
	dir := shellQuote(stateDir)
	var b strings.Builder
	b.WriteString("set -u\n")
	b.WriteString("D=" + dir + "\n")
	// Fail fast if the state dir cannot be created: without it the reaper
	// never launches and no outcome can ever appear, so entering the wait
	// loop below would spin silently until the exec deadline (the 2026-07-19
	// /opt/loom EACCES wedge). Attach mode reports via the orphan marker —
	// no state dir means no reaper to re-attach to.
	b.WriteString("if ! mkdir -p \"$D\"; then\n")
	if mode == supervisorModeAttach {
		b.WriteString("  printf '%s\\n' '" + supervisorMarkerPrefix + "orphan'\n")
		b.WriteString("  exit 0\n")
	} else {
		b.WriteString("  echo \"supervisor: cannot create state dir $D (is /opt/loom agent-owned in the runtime image?)\" >&2\n")
		fmt.Fprintf(&b, "  exit %d\n", supervisorStateDirExit)
	}
	b.WriteString("fi\n")
	b.WriteString("touch \"$D/agent.log\" 2>/dev/null\n")

	tailFrom := "0"
	if mode == supervisorModeLaunch {
		tailFrom = "+1"
		// Idempotent, race-free reaper launch: mkdir is atomic, so only one
		// launcher ever starts the reaper for a given state dir. The launcher
		// does NOT record reaper.pid — the reaper writes its own `$$` (see
		// reaperScript), which is authoritative regardless of whether setsid or
		// the timeout wrapper forked. `$!` here would be fragile across that
		// topology, so it is deliberately not used for liveness.
		b.WriteString("if mkdir \"$D/launch.lock\" 2>/dev/null; then\n")
		b.WriteString("  setsid sh \"$D/reaper.sh\" >/dev/null 2>&1 </dev/null &\n")
		b.WriteString("fi\n")
	}

	b.WriteString("tail -n " + tailFrom + " -f \"$D/agent.log\" 2>/dev/null &\n")
	b.WriteString("tailpid=$!\n")
	b.WriteString("while [ ! -f \"$D/outcome\" ]; do\n")
	b.WriteString("  if [ -f \"$D/reaper.pid\" ]; then\n")
	b.WriteString("    rp=$(cat \"$D/reaper.pid\" 2>/dev/null)\n")
	b.WriteString("    if [ -n \"$rp\" ] && ! kill -0 \"$rp\" 2>/dev/null; then\n")
	// TOCTOU close: the reaper writes the outcome BEFORE it exits, so observing
	// the reaper dead does NOT imply orphan — re-check for the outcome first. A
	// present outcome means the reaper finished (deliver it); only a still-absent
	// outcome after the reaper is gone is a genuine died-mid-flight orphan. Since
	// the write happens-before the exit, once `kill -0` reports dead this
	// re-read is authoritative. Without this, a successful run whose reaper exits
	// right after writing the outcome is falsely reported as an orphan (the
	// timeout-wrapper false-231 the CI Linux run caught).
	b.WriteString("      if [ -f \"$D/outcome\" ]; then break; fi\n")
	b.WriteString("      kill \"$tailpid\" 2>/dev/null\n")
	if mode == supervisorModeAttach {
		b.WriteString("      printf '%s\\n' '" + supervisorMarkerPrefix + "orphan'\n")
		b.WriteString("      exit 0\n")
	} else {
		fmt.Fprintf(&b, "      exit %d\n", supervisorReaperOrphanExit)
	}
	b.WriteString("    fi\n")
	b.WriteString("  fi\n")
	b.WriteString("  sleep 0.25\n")
	b.WriteString("done\n")
	b.WriteString("kill \"$tailpid\" 2>/dev/null\n")
	b.WriteString("code=$(cat \"$D/outcome\" 2>/dev/null)\n")
	if mode == supervisorModeAttach {
		b.WriteString("case \"$code\" in ''|*[!0-9]*) printf '%s\\n' '" + supervisorMarkerPrefix + "malformed'; exit 0 ;; esac\n")
		b.WriteString("printf '%s\\n' \"" + supervisorMarkerPrefix + "outcome $code\"\n")
		b.WriteString("exit 0\n")
	} else {
		fmt.Fprintf(&b, "case \"$code\" in ''|*[!0-9]*) exit %d ;; esac\n", supervisorMalformedOutcomeExit)
		b.WriteString("exit \"$code\"\n")
	}
	return b.String()
}

// supervisorProbeScript reports the supervisor's durable state for one spawn:
// whether the state dir exists, whether the reaper is alive, and the recorded
// outcome. Restart recovery runs this before deciding reattach vs collect vs
// re-drive.
func supervisorProbeScript(stateDir string) string {
	dir := shellQuote(stateDir)
	return strings.Join([]string{
		"set -u",
		"D=" + dir,
		"state=absent",
		`[ -d "$D" ] && state=present`,
		"reaper=dead",
		`if [ -f "$D/reaper.pid" ]; then`,
		`  rp=$(cat "$D/reaper.pid" 2>/dev/null)`,
		`  [ -n "$rp" ] && kill -0 "$rp" 2>/dev/null && reaper=alive`,
		"fi",
		"outcome=none",
		`if [ -f "$D/outcome" ]; then`,
		`  c=$(cat "$D/outcome" 2>/dev/null)`,
		`  case "$c" in ''|*[!0-9]*) outcome=malformed ;; *) outcome="$c" ;; esac`,
		"fi",
		`printf 'PROBE\t%s\t%s\t%s\n' "$state" "$reaper" "$outcome"`,
	}, "\n") + "\n"
}

// injectSupervisorAssets writes hold.sh (the wrapped agent command) and
// reaper.sh (the reaper body) into the spawn pod using the same base64
// `cat | base64 -d` pattern injectSDKDriver/injectAgentConfig use. Both are
// delivered as FILES so the killtest wrapper substrings never appear in any
// long-lived process argv. This runs on the buffered Exec path (a transient
// helper exec, not the agent turn) so it finishes long before any process
// sample.
func (o *SpawnOrchestrator) injectSupervisorAssets(ctx context.Context, be backend.Backend, containerID, spawnID, wrappedCmd string, timeoutSec int) error {
	dir := supervisorStateDir(spawnID)
	holdB64 := base64.StdEncoding.EncodeToString([]byte(wrappedCmd + "\n"))
	reaperB64 := base64.StdEncoding.EncodeToString([]byte(reaperScript(dir, timeoutSec)))
	quotedDir := shellQuote(dir)
	cmd := fmt.Sprintf(
		"mkdir -p %s && echo '%s' | base64 -d > %s/hold.sh && echo '%s' | base64 -d > %s/reaper.sh && chmod +x %s/reaper.sh",
		quotedDir, holdB64, quotedDir, reaperB64, quotedDir, quotedDir,
	)
	if _, err := be.Exec(ctx, backend.ExecOpts{
		ContainerID: containerID,
		Command:     cmd,
		TimeoutSec:  30,
	}); err != nil {
		return fmt.Errorf("write supervisor assets for %s: %w", spawnID, err)
	}
	return nil
}

// supervisorProbe is the parsed result of supervisorProbeScript. It is the pure
// input to classifySupervisorProbe.
type supervisorProbe struct {
	// Found is true when the per-spawn supervisor state dir exists.
	Found bool
	// ReaperAlive is true when the recorded reaper PID is a live process.
	ReaperAlive bool
	// OutcomePresent is true when the reaper recorded a well-formed exit code.
	OutcomePresent bool
	// OutcomeExit is the recorded exit code (valid only when OutcomePresent).
	OutcomeExit int
}

// supervisedRecoveryAction is the refined restart-recovery decision for a
// supervised spawn, taken from a live supervisorProbe. Pure; unit-tested.
type supervisedRecoveryAction int

const (
	// supervisedReattach: the reaper is still running the turn and no outcome is
	// recorded yet — attach (tail + wait) instead of re-driving, so the original
	// process pair is preserved (the S1c continuity contract).
	supervisedReattach supervisedRecoveryAction = iota
	// supervisedCollect: the reaper recorded an outcome — collect it exactly
	// once (the turn already finished, possibly while the controller was down).
	supervisedCollect
	// supervisedRedrive: the supervisor is gone (no reaper, no outcome — died
	// mid-flight) — fall back to the legacy re-drive path for liveness. The
	// continuity gate cannot pass on this path, but the spawn still progresses.
	supervisedRedrive
)

// classifySupervisorProbe maps a probe to a recovery action. Outcome wins even
// if the reaper still lingers (it records the outcome, then lingers as
// `sleep infinity`), so a completed turn is always collected, never re-driven.
func classifySupervisorProbe(p supervisorProbe) supervisedRecoveryAction {
	switch {
	case p.OutcomePresent:
		return supervisedCollect
	case p.ReaperAlive:
		return supervisedReattach
	default:
		return supervisedRedrive
	}
}

// probeSupervisor execs supervisorProbeScript in the spawn pod and parses it.
// A stream/exec error is returned to the caller (which treats it as
// "supervisor state unknown" and, for liveness, falls back to re-drive).
func (o *SpawnOrchestrator) probeSupervisor(ctx context.Context, substrate, spawnID string) (supervisorProbe, error) {
	be := o.substrateBackend(substrate)
	if be == nil {
		return supervisorProbe{}, fmt.Errorf("probe supervisor %s: no substrate backend", spawnID)
	}
	res, err := be.Exec(ctx, backend.ExecOpts{
		ContainerID: "spawn-" + spawnID,
		Command:     supervisorProbeScript(supervisorStateDir(spawnID)),
		TimeoutSec:  20,
	})
	if err != nil {
		return supervisorProbe{}, fmt.Errorf("probe supervisor %s: %w", spawnID, err)
	}
	return parseSupervisorProbe(res.StdoutTail)
}

// parseSupervisorProbe parses the single tab-separated PROBE line. Malformed
// output fails closed so a garbled probe never masquerades as a live reattach.
func parseSupervisorProbe(raw string) (supervisorProbe, error) {
	line := ""
	for _, candidate := range strings.Split(raw, "\n") {
		candidate = strings.TrimSpace(candidate)
		if strings.HasPrefix(candidate, "PROBE\t") {
			line = candidate
		}
	}
	if line == "" {
		return supervisorProbe{}, fmt.Errorf("supervisor probe output missing PROBE line: %q", raw)
	}
	fields := strings.Split(line, "\t")
	if len(fields) != 4 {
		return supervisorProbe{}, fmt.Errorf("malformed supervisor probe %q", line)
	}
	p := supervisorProbe{
		Found:       fields[1] == "present",
		ReaperAlive: fields[2] == "alive",
	}
	switch outcome := fields[3]; outcome {
	case "none", "malformed":
		// no usable outcome
	default:
		code, err := strconv.Atoi(outcome)
		if err != nil || code < 0 {
			return supervisorProbe{}, fmt.Errorf("malformed supervisor outcome %q", outcome)
		}
		p.OutcomePresent = true
		p.OutcomeExit = code
	}
	return p, nil
}
