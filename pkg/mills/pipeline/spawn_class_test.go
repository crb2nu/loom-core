package pipeline

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// realSpawnTimeoutReason is the exact escalation Reason string from GitLab
// issues #356/#357/#358/#359 (services/loom-core, 2026-07-18): plan_slice
// spawns whose agent CLI was killed by the exec deadline. Captured verbatim so
// the classifier is tested against the real producer bytes, not a paraphrase.
const realSpawnTimeoutReason = "hud spawn spawn-3453cf7947a1 status=failed: agent CLI exited 124 (no stderr; stdout: command timed out): spawn: terminal non-completed status"

// realSpawnStdinReason is the exact escalation Reason string from GitLab issue
// #351: a plan_slice spawn started without a usable stdin/prompt.
const realSpawnStdinReason = "hud spawn spawn-10840877a7fe status=failed: agent CLI exited 1: Reading additional input from stdin...: spawn: terminal non-completed status"

// realSpawnAuthMissingReason is an abridged form of the escalation Reason from
// GitLab issue #368 (services/loom-core, 2026-07-22): a fresh spawn pod during
// the 01:54Z fleet-rollout window ran codex with no usable credential. The
// real tail contains BOTH the always-printed "Reading additional input from
// stdin..." line AND the 401 missing-auth-header spew — the auth reason must
// win over the stdin catch-all. Captured with the real producer bytes (raw
// codex text, pre-log-redaction).
const realSpawnAuthMissingReason = "hud spawn spawn-25af388c7906 status=failed: agent CLI exited 1 (no stderr; stdout: Reading additional input from stdin...\n" +
	`{"type":"thread.started","thread_id":"019f878b-23e4-7012-9c8b-c77b72acfd39"}` + "\n" +
	`{"type":"turn.started"}` + "\n" +
	"2026-07-22T01:57:51.478705Z ERROR codex_api::endpoint::responses_websocket: failed to connect to websocket: HTTP error: 401 Unauthorized, url: wss://api.openai.com/v1/responses\n" +
	`{"type":"error","message":"Reconnecting... 2/5 (unexpected status 401 Unauthorized: Missing bearer or basic authentication in header, url: wss://api.openai.com/v1/responses, cf-ray: a1eed64dc89065ef-ATL)"}` + "\n" +
	"): spawn: terminal non-completed status"

// realSpawnDriverLostReason is the exact spawn-failed reason mobile-hud
// records when a controller restart severs an unkeyed spawn's turn driver
// (internal/hud/spawn.go; observed live as #368's implement attempt 2, which
// mis-classified as code).
const realSpawnDriverLostReason = "hud spawn spawn-ce675d97d355 status=failed: agent turn driver lost across mobile-hud restart; unkeyed spawn cannot be re-driven: spawn: terminal non-completed status"

// realSpawnPodLifecycleReason / realSpawnIdentityConflictReason are the two
// error shapes from the live 2026-08-01 18:42Z fleet-roll collision on
// PIPE-pattern-stamp-loom-runbook-loom-runbook-019fbea2, whose plan_slice stage
// escalated class=code. Both are assembled from the real producer chain rather
// than a paraphrase:
//
//   - attempt 1 — a mobile-hud fleet roll evicted the spawn pod mid-start.
//     internal/devbox/backend/k8s_wait.go reports "watch closed for pod …",
//     k8s_runtime.go wraps it as "pod not ready: …", internal/hud/spawn.go
//     failSpawn records "pod creation failed: …", and pkg/mills/clients/spawn.go
//     wraps the terminal state with ErrSpawnTerminalFailure;
//   - attempts 2-4 — the re-dispatch hit the keyed-spawn runtime preflight
//     (internal/hud/spawn.go preflightKeyedRuntime) while the prior pod was
//     still terminating. ProbeStartIdentity joins backend.ErrRuntimeIdentityConflict
//     with "pod … is terminating" (k8s_runtime.go), the mobile spawn handler
//     returns it as a 400 spawn_error envelope, and the Mills client wraps the
//     body verbatim.
const realSpawnPodLifecycleReason = "hud spawn spawn-2ddf8d6cd4ee status=failed: pod creation failed: pod not ready: watch closed for pod spawn-2ddf8d6cd4ee: spawn: terminal non-completed status"

const realSpawnIdentityConflictReason = `hud spawn: POST status 400: {"ok":false,"error":{"code":"spawn_error","message":"preflight keyed spawn 2ddf8d6cd4ee runtime: runtime identity conflict\npod spawn-2ddf8d6cd4ee is terminating"},"meta":{"request_id":"req-8f21c0","timestamp":"2026-08-01T18:42:57Z"}}`

func TestClassify_SpawnInfraFailures(t *testing.T) {
	cases := []struct {
		name string
		msg  string
		want ErrorClass
	}{
		{
			name: "agent CLI exited 124 command timed out (#356/#357/#358/#359)",
			msg:  realSpawnTimeoutReason,
			want: ClassInfra,
		},
		{
			name: "agent CLI reading additional input from stdin (#351)",
			msg:  realSpawnStdinReason,
			want: ClassInfra,
		},
		{
			// The bare log_tail form the runner records
			// (stage=plan_slice attempt=N spawn=…: error=…) must classify
			// the same way as the wrapped escalation Reason.
			name: "log_tail form of the timeout",
			msg:  "stage=plan_slice attempt=3 spawn=spawn-3453cf7947a1: error=agent CLI exited 124 (no stderr; stdout: command timed out)",
			want: ClassInfra,
		},
		{
			name: "credential-less codex 401 spew (#368)",
			msg:  realSpawnAuthMissingReason,
			want: ClassInfra,
		},
		{
			name: "turn driver lost across controller restart (#368 attempt 2)",
			msg:  realSpawnDriverLostReason,
			want: ClassInfra,
		},
		{
			name: "spawn-state ConfigMap safety budget",
			msg:  "persist owned spawn spawn-1 before dispatch: save devbox/loom-spawn-state would serialize to 917505 bytes, exceeding the 917504-byte spawn-state safety budget; prune retained terminal rows",
			want: ClassInfra,
		},
		{
			// The spawn command's own `timeout -k 10 <secs>` wrapper SIGTERMs
			// the agent CLI at the deadline → exit 143. Live 2026-07-26: nine
			// stage attempts died this way mid cold-cache `go test` compile and
			// were mis-classed code.
			name: "agent CLI exited 143 at the spawn deadline",
			msg:  "stage=pr_self_review attempt=1 spawn=spawn-0749d31e0dcf: last_message=The compile is still running and is close to the outer spawn timeout. error=agent CLI exited 143 (no stderr; stdout: {\"type\":\"item.completed\"})",
			want: ClassInfra,
		},
		{
			name: "reconciler spawn deadline backstop",
			msg:  "stage=plan_slice attempt=1 spawn=spawn-4be5d79bdbfa: spawn deadline exceeded during reconciliation",
			want: ClassInfra,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(errors.New(tc.msg)); got != tc.want {
				t.Fatalf("Classify(%q) = %q, want %q", tc.msg, got, tc.want)
			}
		})
	}
}

// TestClassify_SpawnInfraSurvivesSentinelWrap guards the real production error
// shape: pkg/mills/clients/spawn.go wraps the agent-CLI message with
// ErrSpawnTerminalFailure. Classification must survive the wrap.
func TestClassify_SpawnInfraSurvivesSentinelWrap(t *testing.T) {
	wrapped := fmt.Errorf("hud spawn spawn-ffccd53c4bd6 status=failed: agent CLI exited 124 (no stderr; stdout: command timed out): %w", ErrSpawnTerminalFailure)
	if got := Classify(wrapped); got != ClassInfra {
		t.Fatalf("Classify(wrapped terminal timeout) = %q, want %q", got, ClassInfra)
	}
}

func TestClassify_SpawnInfraIsRetryableInfra(t *testing.T) {
	// The whole point of the fix: these spawn defects are retryable
	// infrastructure (bounded by MaxAttempts, off the free-transient budget),
	// NOT terminal config and NOT a free transient.
	for _, msg := range []string{realSpawnTimeoutReason, realSpawnStdinReason, realSpawnAuthMissingReason, realSpawnDriverLostReason} {
		fc := ClassifyFailureRecord(errors.New(msg))
		if fc.Class != FailureInfrastructure {
			t.Errorf("ClassifyFailureRecord(%q).Class = %q, want %q", msg, fc.Class, FailureInfrastructure)
		}
		if !fc.Retryable {
			t.Errorf("ClassifyFailureRecord(%q).Retryable = false, want true", msg)
		}
		if fc.FreeRetry {
			t.Errorf("ClassifyFailureRecord(%q).FreeRetry = true, want false (must burn the bounded MaxAttempts budget, not the free-transient cap)", msg)
		}
		if fc.Terminal {
			t.Errorf("ClassifyFailureRecord(%q).Terminal = true, want false", msg)
		}
	}
}

// TestClassify_FleetRollCollisionIsFreeTransient pins the 2026-08-01 18:42Z
// regression on PIPE-pattern-stamp-loom-runbook-loom-runbook-019fbea2: a
// mobile-hud fleet roll killed a plan_slice spawn, the re-dispatches collided
// with the still-terminating keyed pod, and the run escalated class=code —
// blaming the diff for a rollout window and making the item ineligible for the
// bounded auto-requeue sweep (which only releases infra/transient/quota).
//
// Both halves must classify ClassTransient: no agent ever ran, so no verdict on
// the diff exists, and the blocker clears itself once the pod is reaped. Free
// retry keeps the MaxAttempts budget for real code failures; the runner's
// transientRetryCap still escalates a substrate that collides every time.
func TestClassify_FleetRollCollisionIsFreeTransient(t *testing.T) {
	for _, tc := range []struct {
		name string
		msg  string
	}{
		{"attempt 1: pod killed mid-start", realSpawnPodLifecycleReason},
		{"attempts 2-4: keyed preflight identity conflict", realSpawnIdentityConflictReason},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(errors.New(tc.msg)); got != ClassTransient {
				t.Fatalf("Classify = %q, want %q (was %q, which blamed the diff and blocked auto-requeue)", got, ClassTransient, ClassCode)
			}
			fc := ClassifyFailureRecord(errors.New(tc.msg))
			if fc.Class != FailureTransient {
				t.Errorf("ClassifyFailureRecord.Class = %q, want %q", fc.Class, FailureTransient)
			}
			if !fc.Retryable {
				t.Error("Retryable = false, want true")
			}
			if !fc.FreeRetry {
				t.Error("FreeRetry = false, want true (must not burn the MaxAttempts budget meant for code failures)")
			}
			if fc.Terminal {
				t.Error("Terminal = true, want false")
			}
			// The runner stamps "[reason=…]" from this token; the spawn
			// breaker tallies the same marker, so it must be spawn-prefixed.
			reason, ok := SpawnInfraReason(errors.New(tc.msg))
			if !ok {
				t.Fatal("SpawnInfraReason did not match; the escalation would read as an anonymous transient")
			}
			if !strings.HasPrefix(reason, "spawn-") {
				t.Errorf("reason %q is not spawn-prefixed; the spawn-transport breaker would ignore it", reason)
			}
		})
	}
}

// TestClassify_SpawnImagePullStaysInfra guards the exclusion that makes the
// pod-lifecycle needle safe: the k8s backend wraps EVERY readiness failure as
// "pod not ready: <cause>", so an ImagePullBackOff would otherwise be laundered
// into a free-retry transient and loop the whole transient cap against a
// registry misconfig a fresh pod hits identically.
func TestClassify_SpawnImagePullStaysInfra(t *testing.T) {
	msg := "hud spawn spawn-2ddf8d6cd4ee status=failed: pod creation failed: pod not ready: image pull error in spawn-2ddf8d6cd4ee: ImagePullBackOff: spawn: terminal non-completed status"
	if got := Classify(errors.New(msg)); got != ClassInfra {
		t.Fatalf("Classify(image pull) = %q, want %q", got, ClassInfra)
	}
	if reason, ok := SpawnInfraReason(errors.New(msg)); ok {
		t.Fatalf("SpawnInfraReason(image pull) = %q, want no match", reason)
	}
}

// TestSpawnReasonErrorClass pins the per-reason split: budgeted ClassInfra for
// defects that need the spawn layer repaired, free ClassTransient only for the
// self-clearing rollout collisions. An unknown token fails closed to infra.
func TestSpawnReasonErrorClass(t *testing.T) {
	infra := []string{
		SpawnReasonAgentTimeout,
		SpawnReasonStdinMisconfig,
		SpawnReasonAuthMissing,
		SpawnReasonDriverLost,
		SpawnReasonStateBudget,
		"spawn-some-future-reason",
	}
	for _, reason := range infra {
		if got := spawnReasonErrorClass(reason); got != ClassInfra {
			t.Errorf("spawnReasonErrorClass(%q) = %q, want %q", reason, got, ClassInfra)
		}
	}
	transient := []string{SpawnReasonRuntimeIdentityConflict, SpawnReasonPodLifecycle}
	for _, reason := range transient {
		if got := spawnReasonErrorClass(reason); got != ClassTransient {
			t.Errorf("spawnReasonErrorClass(%q) = %q, want %q", reason, got, ClassTransient)
		}
	}
}

func TestSpawnInfraReason(t *testing.T) {
	cases := []struct {
		name       string
		msg        string
		wantReason string
		wantOK     bool
	}{
		{"timeout", realSpawnTimeoutReason, SpawnReasonAgentTimeout, true},
		{"stdin", realSpawnStdinReason, SpawnReasonStdinMisconfig, true},
		{"exited 124 without stdout tail", "agent CLI exited 124", SpawnReasonAgentTimeout, true},
		{"exited 143 deadline SIGTERM", "agent CLI exited 143 (no stderr; stdout: {\"type\":\"item.started\"})", SpawnReasonAgentTimeout, true},
		{"reconciler deadline backstop", "spawn deadline exceeded during reconciliation", SpawnReasonAgentTimeout, true},
		{"spawn-state safety budget", "persist owned spawn before dispatch: exceeding the 917504-byte spawn-state safety budget", SpawnReasonStateBudget, true},
		{"unrelated build failure stays unmatched", "go build ./...: undefined: Foo", "", false},
		{"genuine nonzero agent exit stays unmatched", "agent CLI exited 2: panic: test failed", "", false},
		// #368 regression: the tail carries BOTH the always-printed stdin
		// marker AND the 401 missing-auth spew — the specific auth reason must
		// win over the stdin catch-all.
		{"auth-missing beats stdin catch-all (#368)", realSpawnAuthMissingReason, SpawnReasonAuthMissing, true},
		// The mobile-hud log redactor rewrites "bearer or" — the redacted form
		// must classify identically to the raw CLI text.
		{
			"auth-missing redacted form",
			"agent CLI exited 1 (no stderr; stdout: unexpected status 401 Unauthorized: Missing [REDACTED] basic authentication in header, url: https://api.openai.com/v1/responses)",
			SpawnReasonAuthMissing, true,
		},
		// The in-pod preflight (internal/hud/spawn.go codexAuthPreflight)
		// producer string is a classifier contract.
		{
			"auth preflight fail-fast",
			"hud spawn spawn-abc status=failed: agent CLI exited 78 (stderr: codex auth preflight failed: /home/agent/.codex/auth.json missing or empty and OPENAI_API_KEY unset (is the cluster-agent-auth secret codex-auth-json key populated and mounted?))",
			SpawnReasonAuthMissing, true,
		},
		// A credentialed-but-rejected 401 is a persistent config defect, not
		// the transient delivery window — it must NOT match auth-missing.
		{
			"incorrect API key 401 stays unmatched",
			"agent CLI exited 1: 401 Unauthorized: Incorrect API key provided: PLACEHOLDER",
			"", false,
		},
		{"driver lost across restart (#368 attempt 2)", realSpawnDriverLostReason, SpawnReasonDriverLost, true},
		// 2026-08-01 fleet-roll collision, both halves.
		{"fleet roll killed the pod mid-start", realSpawnPodLifecycleReason, SpawnReasonPodLifecycle, true},
		{"keyed preflight rejected a terminating pod", realSpawnIdentityConflictReason, SpawnReasonRuntimeIdentityConflict, true},
		// The other ProbeStartIdentity rejections share the joined sentinel
		// text, so they classify identically without a needle each.
		{
			"keyed preflight rejected a non-Running pod",
			`hud spawn: POST status 400: {"ok":false,"error":{"code":"spawn_error","message":"preflight keyed spawn 2ddf8d6cd4ee runtime: runtime identity conflict\npod spawn-2ddf8d6cd4ee phase Failed is not attachable"}}`,
			SpawnReasonRuntimeIdentityConflict, true,
		},
		{
			"runtime exists without its durable row",
			"hud spawn: POST status 400: runtime identity conflict: runtime spawn-2ddf8d6cd4ee exists without its durable spawn row; recovery must reconstruct it before registration",
			SpawnReasonRuntimeIdentityConflict, true,
		},
		// The bare k8s-backend forms (no HUD wrapper) reach Classify from the
		// devbox exec path too.
		{"bare watch-closed", "pod not ready: watch closed for pod spawn-2ddf8d6cd4ee", SpawnReasonPodLifecycle, true},
		{"bare deleted-before-running", "pod not ready: pod spawn-2ddf8d6cd4ee was deleted before reaching Running", SpawnReasonPodLifecycle, true},
		// Load-bearing exclusion: "pod not ready" also wraps ImagePullBackOff,
		// which is a persistent registry/image misconfig. It must NOT be
		// laundered into a free-retry pod-lifecycle transient (see
		// TestClassify_SpawnImagePullStaysInfra).
		{
			"image-pull readiness failure stays unmatched",
			"pod creation failed: pod not ready: image pull error in spawn-2ddf8d6cd4ee: ImagePullBackOff",
			"", false,
		},
		{"Claude invalid auth JSON stream (dual-fleet canary)", realClaudeInvalidAuthResult, SpawnReasonAuthMissing, true},
		{"Claude revoked OAuth token", `{"type":"result","is_error":true,"result":"OAuth token has been revoked. Please log in again."}`, SpawnReasonAuthMissing, true},
		{"Claude invalid OAuth token", `{"type":"result","is_error":true,"result":"Invalid OAuth token"}`, SpawnReasonAuthMissing, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason, ok := SpawnInfraReason(errors.New(tc.msg))
			if ok != tc.wantOK || reason != tc.wantReason {
				t.Fatalf("SpawnInfraReason(%q) = (%q, %v), want (%q, %v)", tc.msg, reason, ok, tc.wantReason, tc.wantOK)
			}
		})
	}
	if reason, ok := SpawnInfraReason(nil); ok || reason != "" {
		t.Fatalf("SpawnInfraReason(nil) = (%q, %v), want (\"\", false)", reason, ok)
	}
}
