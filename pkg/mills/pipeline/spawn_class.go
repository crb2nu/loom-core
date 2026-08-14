package pipeline

import "strings"

// Spawn-infrastructure failure reasons. These name deterministic spawn-layer
// defects that a plain re-dispatch of the SAME work cannot fix on its own, but
// that a fresh spawn frequently can — so they are RETRYABLE INFRASTRUCTURE, not
// code. The agent CLI never produced a verdict on the diff, so blaming the diff
// (class=code) both mis-attributes the escalation-class metric and buries a
// cluster/spawn-layer defect among genuine build/test breaks. Same reasoning as
// ErrPipelinePollTimeout → ClassInfra for ci_watch (escalations #149/#153).
//
// A reason's ErrorClass is NOT uniform: spawnReasonErrorClass maps each token to
// either ClassInfra (budgeted — the defect needs the spawn/cluster layer fixed)
// or ClassTransient (free retry — a rollout-window collision that clears on its
// own). Every token is "spawn-"-prefixed so the spawn-transport breaker
// (pkg/mills/spawn_breaker.go) picks up new reasons without a copy of this set.
const (
	// SpawnReasonAgentTimeout: the agent CLI was killed by the spawn's exec
	// deadline. The pod was healthy enough to start the CLI, but the run
	// exceeded the command timeout. Three deterministic shapes:
	//   - exit 124 with StdoutTail "command timed out" (internal/devbox
	//     backends), rendered by internal/hud/spawn.go bufferedExecFailure as
	//     "agent CLI exited 124 (no stderr; stdout: command timed out)";
	//   - exit 143: the spawn command's own `timeout -k 10 <secs>` wrapper
	//     delivered SIGTERM at the deadline (128+15). Live 2026-07-26: nine
	//     stage attempts died at "agent CLI exited 143" mid cold-cache
	//     `go test` compile — each fell through to class=code and burned a
	//     budgeted attempt on work the agent had already finished authoring;
	//   - "spawn deadline exceeded" — the HUD reconciler's ~65-minute
	//     backstop failing a spawn whose driver never returned.
	// A fresh spawn often lands on a faster / less-loaded node (and a warmed
	// build cache). Live: escalations #356-#359, where plan_slice burned up
	// to 1h per attempt at $0.00 while mis-labeled class=code.
	SpawnReasonAgentTimeout = "spawn-agent-timeout"

	// SpawnReasonStdinMisconfig: the agent CLI exited because it was started
	// without a usable stdin/prompt and blocked reading stdin, surfacing as
	// "agent CLI exited 1: Reading additional input from stdin...". The prompt
	// was not wired to the CLI — a spawn-infra defect, not a code failure of the
	// diff. Live: escalation #351.
	//
	// CAUTION: codex 0.144 prints "Reading additional input from stdin..." on
	// EVERY headless exec (stdin is a non-TTY /dev/null), so this needle can
	// appear in the stdout tail of ANY codex failure. It is therefore the
	// catch-all checked LAST — every more specific spawn reason below must be
	// matched first, or it swallows their diagnostics (escalation #368: a 401
	// missing-credential outage was reported as stdin-misconfig even though
	// the 401 text was in the same tail).
	SpawnReasonStdinMisconfig = "spawn-stdin-misconfig"

	// SpawnReasonAuthMissing: the agent CLI ran with NO usable credential, or
	// Claude Code rejected a revoked/invalid OAuth credential before producing
	// a verdict. Both are retryable spawn-auth delivery failures, rather than
	// code failures in the diff.
	// Producers: (a) the in-pod codex auth preflight (internal/hud/spawn.go
	// codexAuthPreflight) failing fast when ~/.codex/auth.json is a dangling
	// symlink / empty and OPENAI_API_KEY is unset; (b) codex reaching the API
	// anyway and spewing `401 Unauthorized: Missing bearer or basic
	// authentication in header` before exiting 1 at $0.00. The credential
	// mount is k8s-Optional by design, so a transient unpopulated mount (the
	// 2026-07-22 01:54Z fleet-rollout window, escalation #368) starts the pod
	// credential-less instead of failing it — a spawn-infra defect that a
	// fresh spawn fixes once the window passes, hence retryable infra with a
	// rollout-window backoff (retryBackoff → authOutageBackoff).
	SpawnReasonAuthMissing = "spawn-auth-missing"

	// SpawnReasonDriverLost: the controller restarted mid-turn and the spawn's
	// in-memory turn driver could not be re-attached or re-driven ("agent turn
	// driver lost across mobile-hud restart; unkeyed spawn cannot be
	// re-driven", internal/hud/spawn.go). No verdict was produced on the diff;
	// a fresh dispatch against the already-restarted controller succeeds. Left
	// unmatched this was class=code (escalation #368 attempt 2), burning a
	// budgeted attempt on a controller rollout.
	SpawnReasonDriverLost = "spawn-driver-lost"

	// SpawnReasonStateBudget: the HUD could not persist the owned spawn before
	// dispatch because the shared spawn-state ConfigMap reached its safety
	// budget. This is fleet capacity pressure, never a defect in the diff.
	SpawnReasonStateBudget = "spawn-state-budget"

	// SpawnReasonRuntimeIdentityConflict: the HUD's keyed-spawn runtime
	// preflight (internal/hud/spawn.go preflightKeyedRuntime) refused to
	// dispatch because the deterministic pod for this spawn ID still exists and
	// is not attachable — it carries a DeletionTimestamp ("pod spawn-… is
	// terminating"), sits in a non-Running phase, or has no durable row yet.
	// The producer is backend.ErrRuntimeIdentityConflict joined with the
	// concrete cause (internal/devbox/backend/k8s_runtime.go ProbeStartIdentity)
	// and it reaches Mills as an HTTP 400 from the mobile spawn API
	// (`hud spawn: POST status 400: {"error":{"code":"spawn_error",…}}`), so the
	// agent CLI never started and no verdict on the diff exists.
	//
	// This is a ROLLOUT-WINDOW COLLISION, not a cluster misconfig: a fleet roll
	// (or any controller restart) leaves the prior keyed pod terminating while
	// the stage re-dispatches under the same key. It clears on its own once the
	// kubelet reaps the pod, so it is a FREE transient retry — but only paired
	// with identityConflictBackoff, because an immediate re-attempt can only
	// re-observe the same terminating pod. Live 2026-08-01 18:42Z
	// (PIPE-pattern-stamp-loom-runbook-loom-runbook-019fbea2): a mobile-hud
	// fleet roll killed a plan_slice spawn, then attempts 2-4 burned the whole
	// MaxAttempts budget in ~40s against the terminating pod and escalated
	// class=code.
	SpawnReasonRuntimeIdentityConflict = "spawn-runtime-identity-conflict"

	// SpawnReasonPodLifecycle: the spawn pod never reached Running, so the agent
	// CLI never started. Producers: internal/hud/spawn.go ("pod creation
	// failed: …"), internal/devbox/backend/k8s_runtime.go ("pod not ready: …"),
	// internal/devbox/backend/k8s_wait.go ("watch closed for pod spawn-…", "was
	// deleted before reaching Running"). A fleet roll evicting the pod mid-start
	// produces exactly this shape — it was attempt 1 of the 2026-08-01 run above.
	//
	// Already ClassTransient before this token existed; the token is what makes
	// the escalation legible as a spawn-transport defect (the runner stamps
	// "[reason=…]", which is also what the spawn breaker tallies) instead of an
	// anonymous transient. Deliberately does NOT cover the image-pull shape:
	// "pod not ready: image pull error in …: ImagePullBackOff" is a persistent
	// registry/image misconfig that a fresh pod hits identically, so it stays
	// unmatched here and falls through to ClassInfra in Classify.
	SpawnReasonPodLifecycle = "spawn-pod-lifecycle"
)

// spawnTransientReasons are the spawn reasons whose defect clears on its own
// (a rollout window closing, a pod being reaped) rather than needing the
// spawn/cluster layer repaired. They get a FREE retry off the MaxAttempts
// budget, still bounded by the runner's transientRetryCap.
var spawnTransientReasons = map[string]struct{}{
	SpawnReasonRuntimeIdentityConflict: {},
	SpawnReasonPodLifecycle:            {},
}

// spawnReasonErrorClass maps a spawn reason token to the ErrorClass Classify
// returns for it. Self-clearing rollout collisions are free transients; every
// other spawn defect stays budgeted ClassInfra so a genuinely broken spawn layer
// still escalates within MaxAttempts instead of looping on the transient cap.
func spawnReasonErrorClass(reason string) ErrorClass {
	if _, ok := spawnTransientReasons[reason]; ok {
		return ClassTransient
	}
	return ClassInfra
}

// SpawnInfraReason returns the distinct spawn-infrastructure reason token for a
// stage error whose underlying message is a recognized spawn-layer defect, and
// true. It returns ("", false) for any other error. Matching is on the wrapped
// message (pkg/mills/clients/spawn.go wraps the agent CLI message with
// ErrSpawnTerminalFailure) so it works regardless of the sentinel chain, and is
// intentionally narrow: only the timeout (exit 124 / "command timed out"),
// missing-credential (preflight / 401 missing-auth-header), driver-lost,
// state-budget, keyed-runtime-identity-conflict, pod-lifecycle, and
// stdin-misconfig shapes match, so a genuine nonzero agent exit with a real
// diagnostic still falls through to ClassCode.
//
// ORDER MATTERS twice over:
//   - the stdin-misconfig needle is a line codex prints on every headless exec,
//     so it is the catch-all and must stay LAST — any specific reason it would
//     otherwise swallow (the #368 401 outage) is checked first;
//   - the identity-conflict needle precedes the pod-lifecycle needles: a
//     preflight rejection for a terminating pod can quote both, and the
//     collision (which needs identityConflictBackoff) is the actionable one.
func SpawnInfraReason(err error) (string, bool) {
	if err == nil {
		return "", false
	}
	return spawnInfraReasonFromString(err.Error())
}

func spawnInfraReasonFromString(msg string) (string, bool) {
	lower := strings.ToLower(msg)
	// Agent CLI killed by the exec timeout: exit 124 / the devbox backends'
	// "command timed out" StdoutTail, exit 143 (the spawn command's own
	// `timeout` wrapper SIGTERM at the deadline), or the HUD reconciler's
	// "spawn deadline exceeded" backstop. All three are the same defect —
	// the turn ran out of wall clock, no verdict on the diff.
	if strings.Contains(lower, "command timed out") || strings.Contains(lower, "exited 124") ||
		strings.Contains(lower, "exited 143") || strings.Contains(lower, "spawn deadline exceeded") {
		return SpawnReasonAgentTimeout, true
	}
	// Agent CLI ran with no usable credential. Two producers: the in-pod
	// preflight message (internal/hud/spawn.go codexAuthPreflight — exact
	// contract string), and the codex missing-auth-header 401 spew. The 401
	// needle pair matches both the raw CLI text ("Missing bearer or basic
	// authentication in header") and its log-redacted form ("Missing
	// [REDACTED] basic authentication in header"); a credentialed-but-rejected
	// 401 (e.g. "Incorrect API key provided") deliberately does NOT match —
	// that is a persistent config defect, not this transient delivery window.
	if strings.Contains(lower, "codex auth preflight failed") ||
		(strings.Contains(lower, "401 unauthorized") && strings.Contains(lower, "authentication in header")) {
		return SpawnReasonAuthMissing, true
	}
	// Claude Code emits its auth rejection as stream-json terminal events. Its
	// captured "Invalid API key · Fix external API key" result (the 2026-07-25
	// dual-fleet canary) has no HTTP status to share the Codex 401 branch above.
	// OAuth-specific wording is equally a spawn credential failure. Keep these
	// needles narrow so an arbitrary application-level "invalid API key" does
	// not stop being a conservative ClassCode failure.
	if strings.Contains(lower, "invalid api key · fix external api key") ||
		strings.Contains(lower, "oauth token has been revoked") ||
		strings.Contains(lower, "invalid oauth token") {
		return SpawnReasonAuthMissing, true
	}
	// Controller restarted mid-turn and the spawn could not be re-driven.
	if strings.Contains(lower, "turn driver lost") {
		return SpawnReasonDriverLost, true
	}
	if strings.Contains(lower, "spawn-state safety budget") {
		return SpawnReasonStateBudget, true
	}
	// Keyed-spawn rollout collision: the HUD runtime preflight found the
	// deterministic pod for this spawn ID still present and unattachable. The
	// sentinel's own text ("runtime identity conflict",
	// backend.ErrRuntimeIdentityConflict) is joined ahead of the concrete cause,
	// so it identifies every ProbeStartIdentity rejection; the preflight-wrapper
	// pairing is kept as a second needle so a future cause that reaches Mills
	// without the sentinel text still matches.
	if strings.Contains(lower, "runtime identity conflict") ||
		(strings.Contains(lower, "preflight keyed spawn") && strings.Contains(lower, "is terminating")) {
		return SpawnReasonRuntimeIdentityConflict, true
	}
	// Spawn pod never reached Running. The image-pull exclusion is load-bearing:
	// the k8s backend wraps EVERY readiness failure as "pod not ready: <cause>",
	// and a cause of "image pull error in …: ErrImagePull/ImagePullBackOff" is a
	// persistent registry/image misconfig a fresh pod hits identically. Leaving
	// it unmatched here keeps it on Classify's ClassInfra path instead of
	// laundering it into a free transient retry.
	if !strings.Contains(lower, "image pull error") {
		for _, needle := range []string{
			"pod creation failed",
			"pod not ready",
			"watch closed for pod",
			"deleted before reaching running",
		} {
			if strings.Contains(lower, needle) {
				return SpawnReasonPodLifecycle, true
			}
		}
	}
	// Agent CLI blocked reading stdin because the spawn was started without a
	// prompt. Catch-all — keep last (see SpawnReasonStdinMisconfig).
	if strings.Contains(lower, "additional input from stdin") {
		return SpawnReasonStdinMisconfig, true
	}
	return "", false
}
