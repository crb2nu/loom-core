package pipeline

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// CIPipelineTerminalError preserves the failed-job reasons observed for a
// terminal pipeline while continuing to match ErrCIPipelineTerminal.
type CIPipelineTerminalError struct {
	Status           string
	MRIID            int64
	FailedJobReasons []string
	FailedJobs       []FailedJob
	FirstFailedJobs  []FailedJob
	AutoRetried      bool
}

func (e *CIPipelineTerminalError) Error() string {
	if e.AutoRetried && len(e.FirstFailedJobs) > 0 {
		second := failedJobNames(e.FailedJobs)
		if second == "" {
			second = "unknown job"
		}
		return fmt.Sprintf("ci pipeline %s for mr %d: %s failed, auto-retried once, failed again (%s): %v", e.Status, e.MRIID, failedJobNames(e.FirstFailedJobs), second, ErrCIPipelineTerminal)
	}
	return fmt.Sprintf("ci pipeline %s for mr %d: %v", e.Status, e.MRIID, ErrCIPipelineTerminal)
}

func (e *CIPipelineTerminalError) Unwrap() error { return ErrCIPipelineTerminal }

func (e *CIPipelineTerminalError) allRunnerSystemFailures() bool {
	if e == nil || len(e.FailedJobReasons) == 0 {
		return false
	}
	for _, reason := range e.FailedJobReasons {
		if strings.TrimSpace(reason) != "runner_system_failure" {
			return false
		}
	}
	return true
}

// ErrorClass categorises a stage error so the runner can decide whether
// to spend the MaxAttempts budget on the retry. Set explicitly by
// Classify; the zero value is ClassCode (treat-as-real-bug) so an
// unrecognized failure can never accidentally retry forever.
type ErrorClass string

const (
	// ClassTransient: a flaky-but-not-terminal failure. Free retry —
	// does not consume MaxAttempts budget. Cap retries via the
	// runner's transientRetryCap to bound permanent transients.
	// Sourced from the 2026-05-24 kill-test (.loom/local/handoffs/
	// mills-autonomy-killtest-2026-05-24.md): k8s pod GC race, MCP
	// websocket close 1000/1006, broken pipe, flexinfer chat context
	// deadline, devbox initialize 1000 "Backend unavailable".
	// Combined ~62% of failing stage_results.
	ClassTransient ErrorClass = "transient"

	// ClassTransientQuota: a rate-limit or quota response from an
	// upstream. Free retry like ClassTransient, but the runner adds
	// exponential backoff so we don't immediately burn the next quota
	// budget. Examples: flexinfer 429, GitLab 429, generic "rate limit".
	ClassTransientQuota ErrorClass = "transient_quota"

	// ClassInfra: persistent infrastructure misconfig that retry can't
	// fix on its own. Counts against MaxAttempts (so we don't sit on a
	// truly broken sandbox forever) but is reported separately so the
	// operator knows the fix is at the cluster/image layer, not the
	// pipeline. Examples from kill-test (~22% of failing stage_results):
	// buildah pod name conflicts ("pods … already exists"), dockerfile
	// generation failures, persistent buildah exit codes.
	// Slice 2e is the durable fix; until then, treating these as code-
	// class would conflate them with real code bugs in the metric.
	ClassInfra ErrorClass = "infra"

	// ClassCode: a real code-side failure that retrying the same input
	// won't fix. Counts against MaxAttempts. Examples: gate fails,
	// build failures, test failures, spec-conformance violations.
	// Default when no other class matches.
	ClassCode ErrorClass = "code"

	// ClassConfig: a terminal project/MR configuration error that no
	// amount of retrying the same call can fix — the fix is a human (or
	// policy) change in GitLab, not in the pipeline. Escalates
	// immediately without consuming retries. Canonical case: the merge
	// stage's 405 Method Not Allowed (escalations #148/#150), which
	// GitLab returns when the MR can't be merged as requested
	// (merge-when-pipeline-succeeds unavailable, approvals unmet, wrong
	// merge method). Retrying verbatim burned 3 attempts per run before
	// escalating with no extra signal.
	ClassConfig ErrorClass = "config"
)

// allErrorClasses is the closed taxonomy. Kept beside the constants so a
// new class can't be added without deciding whether it is a valid metric
// label (see ErrorClass.Valid, consumed by the escalation-class counter).
var allErrorClasses = []ErrorClass{
	ClassTransient,
	ClassTransientQuota,
	ClassInfra,
	ClassCode,
	ClassConfig,
}

// AllErrorClasses returns every value in the closed error taxonomy.
func AllErrorClasses() []ErrorClass {
	return append([]ErrorClass(nil), allErrorClasses...)
}

// Valid reports whether c is one of the closed ErrorClass values. Used to
// bound the cardinality of the escalation-class metric label: an unknown
// class parsed out of an escalation reason maps to "unclassified" rather
// than minting an unbounded new Prometheus series.
func (c ErrorClass) Valid() bool {
	for _, allowed := range allErrorClasses {
		if c == allowed {
			return true
		}
	}
	return false
}

// Classify maps an error returned from a stage dispatcher to one of the
// ErrorClass values. Conservative on purpose — unknown errors stay
// ClassCode so they consume the attempt budget and the operator gets
// pulled in. Pattern source is the live operator state.db pulled in
// the 2026-05-24 kill-test; new failure modes get added here as the
// operator surfaces them.
func Classify(err error) ErrorClass {
	if err == nil {
		return ""
	}
	if errors.Is(err, ErrMergeRequestLocked) {
		return ClassTransient
	}
	var terminalCI *CIPipelineTerminalError
	if errors.As(err, &terminalCI) && terminalCI.allRunnerSystemFailures() {
		return ClassTransient
	}
	if errors.Is(err, ErrMergeRequestClosed) ||
		errors.Is(err, ErrMergeAuthorizationStale) ||
		errors.Is(err, ErrMergeRecoveryConfig) {
		return ClassConfig
	}
	// An MR that never reports a head SHA is a terminal GitLab state (conflicts,
	// a closed MR, a missing source branch), not a slow pipeline: re-polling
	// observes the identical response. Checked BEFORE ErrPipelinePollTimeout so
	// this can never be laundered into the retryable infra bucket and burn
	// MaxAttempts × the ci_watch watch cap. See ErrMRHeadSHAUnavailable.
	if errors.Is(err, ErrMRHeadSHAUnavailable) {
		return ClassConfig
	}
	// Same reasoning one step later in the MR's life: the head SHA exists but
	// the project never builds a push pipeline for it. Also checked BEFORE
	// ErrPipelinePollTimeout so it cannot be laundered into the retryable infra
	// bucket. See ErrBranchPipelineUnavailable.
	if errors.Is(err, ErrBranchPipelineUnavailable) {
		return ClassConfig
	}
	// A spawn that exhausted its poll deadline without reaching a terminal
	// status is transient: a fresh re-spawn frequently lands on a healthy
	// pod (the prior one was hung-but-alive). Free retry — does not burn the
	// MaxAttempts budget — but every attempt still counts toward the
	// transient hard cap so a persistently hung substrate escalates instead
	// of looping pending forever. See Runner.runStage's stall conversion.
	if errors.Is(err, ErrSpawnPollTimeout) {
		return ClassTransient
	}
	// A ci_watch pipeline-poll timeout is infra, not code: the branch
	// pipeline never reached a terminal state within PollDeadline, so the
	// fix is at the CI/cluster layer (a stuck or genuinely slow pipeline),
	// not in the diff. Unlike a spawn poll timeout, re-polling re-attaches to
	// the SAME pipeline, so it is NOT a free transient retry — Infra counts
	// against MaxAttempts, bounding total wall-clock at MaxAttempts ×
	// PollDeadline while keeping the escalation-class metric honest
	// (escalations #149/#153 previously mis-classed these as code). Checked
	// via errors.Is (before the string matching) so the wrapped web_url in
	// the message can't accidentally match a different needle.
	if errors.Is(err, ErrPipelinePollTimeout) {
		return ClassInfra
	}
	// A model-unavailable failure (503 "service_unavailable" / "parked behind a
	// higher-priority primary" from the shared-GPU proxy, exhausted across the
	// whole fallback chain) is transient: the model is deployed but temporarily
	// unservable, so a free retry after the park window frequently lands. The
	// research stage soft-skips this before it reaches Classify; this covers the
	// judge/other paths where the error still propagates. Checked via errors.Is
	// before the string matching so a wrapped chain is recognized regardless of
	// the surface message form.
	if errors.Is(err, ErrModelUnavailable) {
		return ClassTransient
	}
	// A devbox quality gate that reports not-passed with zero executed checks
	// produced no verdict — an infrastructure contract violation (recycled/
	// evicted sandbox), not a test failure. Transient so a fresh sandbox retries
	// free rather than the empty result burning the implement budget as a
	// phantom code failure (live 2026-07-16). Checked via errors.Is before the
	// string matching so the gate JSON tail wrapped in the message can't match
	// an unrelated needle.
	if errors.Is(err, ErrDevboxGateNoChecks) {
		return ClassTransient
	}
	// Net layer eofs always wrap up to a transport class.
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return ClassTransient
	}
	s := err.Error()
	lower := strings.ToLower(s)

	// Terminal config errors first — these must never fall through to a
	// retryable class. "status 405" matches the GitLab client's error
	// shape ("gitlab: PUT /projects/.../merge: status 405: ...",
	// pkg/mills/clients/gitlab.go); "method not allowed" catches the
	// worded form when the body is included. "status 422" /
	// "cannot be merged" is GitLab's merge-conflict rejection ("Branch
	// cannot be merged") — the branch needs a rebase, so an identical
	// retry can only return the identical error (escalation #287,
	// 2026-07-07: 3 merge attempts in ~1.5s total before escalating).
	for _, needle := range []string{
		"status 405",
		"method not allowed",
		"status 422",
		"cannot be merged",
		// devbox_quality_gate refusing to mint a verdict because the
		// requested checks selector resolved to zero executable commands
		// (cmd/mcp-devbox/quality_gate.go). The selector comes from pipeline
		// policy, so an identical retry can only fail identically — the fix
		// is the checks list, not the diff.
		"quality gate executed no checks",
	} {
		if strings.Contains(lower, needle) {
			return ClassConfig
		}
	}

	// Git-clone init/build failures — MUST be classified before the generic
	// "buildah build failed"/"image build failed" infra needles at the bottom,
	// because the captured git stderr is wrapped INSIDE those messages (e.g.
	// "image build failed: buildah build failed: container git-clone terminated
	// exit_code=128 … fatal: repository … not found"). Left to fall through, a
	// missing target repo (config, terminal) mis-classed as infra and burned
	// the retry budget on a call that can only fail identically. A DNS/network
	// clone blip is the one transient case and re-enters the retry path here.
	// Live 2026-07-16: a plan targeting services/familyforge (a repo that did
	// not exist) escalated as opaque infra with the git 404 invisible.
	if gc, ok := ClassifyGitCloneError(s); ok {
		return gc.Class
	}

	// Spawn-infrastructure defects: the agent CLI was killed by the exec
	// timeout (exit 124 / "command timed out"), ran with no usable credential
	// (auth preflight / 401 missing-auth-header, escalation #368), lost its
	// turn driver to a controller restart, started without a usable
	// stdin/prompt ("Reading additional input from stdin"), was refused by the
	// keyed-spawn runtime preflight over a terminating pod, or never got a pod
	// into Running at all. No verdict was produced on the diff, so these are
	// infrastructure, not code, and the escalation is attributed at the
	// spawn/cluster layer with a distinct reason (SpawnInfraReason). Left in the
	// default ClassCode, escalations #356-#359 (timeout, up to 1h/attempt at
	// $0.00), #351 (stdin) and the 2026-08-01 fleet-roll collision were retried
	// and reported as code bugs.
	//
	// The class is per-reason (spawnReasonErrorClass), not uniform: a defect
	// needing the spawn layer repaired is budgeted ClassInfra (never the
	// free-transient budget that let runs balloon to the cap-8 total), while a
	// self-clearing rollout collision is a free ClassTransient bounded by
	// transientRetryCap. Checked before the generic infra needles so the specific
	// spawn reason is recognizable — and the pod-lifecycle needles live inside
	// spawnInfraReasonFromString (which excludes the image-pull shape) so this
	// branch cannot launder a persistent registry misconfig into a free retry.
	// Note: a spawn POLL timeout (ErrSpawnPollTimeout, "poll deadline exceeded")
	// is a distinct, free-transient case handled above — this only catches a
	// TERMINAL spawn-layer failure.
	if reason, ok := spawnInfraReasonFromString(s); ok {
		return spawnReasonErrorClass(reason)
	}

	// Quota first — a "429" can be embedded inside a transport-shaped
	// error so check it before the transport patterns.
	for _, needle := range []string{
		"429",
		"too many requests",
		"rate limit",
		"rate_limit",
		// Claude Code's --output-format stream-json terminal events preserve
		// Anthropic's machine-readable error type, rather than spelling out
		// "rate limit". These are upstream capacity/quota conditions: retry
		// with quotaBackoff, never blame the diff. Captured from the
		// 2026-07-25 dual-fleet canary (spawn-d27380fcae08).
		"rate_limit_error",
		"usage limit",
		"overloaded_error",
		"quota exceeded",
		// HUD spawn-pool saturation: the spawn API rejects with a 400
		// `{"code":"spawn_error","message":"max concurrent spawns reached (N)"}`
		// when its concurrency limit is hit. This is a CAPACITY limit, not a
		// code defect — the slot frees as soon as another spawn finishes — so
		// it must back off and retry rather than escalate. Before this, a busy
		// spawn pool (e.g. overlapping council + canary + manual runs) made a
		// legitimate item escalate instantly with class=code, cost $0, no
		// stage progress (observed live 2026-06-30 on MILLS-VERIFY-CODE-…2002,
		// which the doc verify item earlier merged through the same path when
		// the pool was free). Backoff spacing is saturationBackoff (minutes,
		// slot-release scale), selected by retryBackoff via isSpawnSaturation.
		spawnSaturationNeedle,
	} {
		if strings.Contains(lower, needle) {
			return ClassTransientQuota
		}
	}

	// Spawn pod never became ready, image-pull variant. The devbox k8s backend
	// wraps every readiness failure as "pod not ready: <cause>"
	// (internal/devbox/backend/k8s_runtime.go), and a cause of "image pull
	// error in …: ErrImagePull/ImagePullBackOff" (k8s_wait.go
	// podEarlyContainerError) is a persistent registry/image-layer misconfig
	// that a fresh pod hits identically — so it must never share the
	// free-retry class of the pod-lifecycle deaths that use the same wrapper.
	// Reaching this line at all depends on spawnInfraReasonFromString declining
	// to claim the image-pull shape as SpawnReasonPodLifecycle; that exclusion
	// is what keeps the two apart now that the pod-lifecycle needles are matched
	// EARLIER, by the spawn-reason branch above (→ ClassTransient, the same
	// free-retry class they always had — escalation #306, 2026-07-10 — now with
	// a spawn reason token so the escalation reads as a spawn-transport defect
	// instead of an anonymous transient).
	if strings.Contains(lower, "image pull error") {
		return ClassInfra
	}

	// Transport / k8s GC / flexinfer timeout patterns. Cross-checked
	// against the kill-test buckets:
	//   transient:k8s-spawn-pod-gc       — "pod not found during reconciliation"
	//   transient:mcp-ws-1006            — "websocket: close 1006"
	//   transient:mcp-backend-unavailable — "websocket: close 1000" + "backend unavailable"
	//   transient:mcp-broken-pipe        — "broken pipe"
	//   transient:flexinfer-timeout      — "context deadline exceeded" on flexinfer chat
	for _, needle := range []string{
		"pod not found during reconciliation",
		"websocket: close",
		"backend unavailable",
		"broken pipe",
		"connection reset by peer",
		"use of closed network connection",
		"transport closed",
		"i/o timeout",
		"unexpected eof",
		"context deadline exceeded",
		"no such host",
		"connection refused",
		// k8s exec-stream konnectivity blip: the apiserver can't reach the
		// target node's kubelet (:10250), so spawn/`kubectl exec` streaming
		// fails with "error dialing backend ... 502 Bad Gateway". Transient —
		// the node is typically reachable again on the next attempt.
		// 2026-06-05 A2 kill-test: a plan_slice codex spawn on k3s-w-10
		// escalated on this (turn_count=0, $0 cost), blocking the first
		// harvester-vm canary before it could reach the implement stage.
		"error dialing backend",
		// k8s exec against a recycled/evicted sandbox pod: the kubelet
		// rejects the exec upgrade with `unable to upgrade connection:
		// container not found ("devbox")`. A fresh exec lands on the
		// replacement pod. Left in ClassCode, a devbox tests stage burned
		// all 3 attempts in ~4s and escalated as a code failure
		// (escalation #289, 2026-07-08).
		"container not found",
		"unable to upgrade connection",
	} {
		if strings.Contains(lower, needle) {
			return ClassTransient
		}
	}

	// Upstream 5xx — transient so we retry instead of escalating. Matches
	// the `status 5xx` shape used by the GitLab and devbox clients AND the
	// worded gateway forms emitted by the k8s apiserver proxy / ingress
	// ("502 Bad Gateway", "503 Service Unavailable", "504 Gateway Timeout"),
	// which arrive without a "status NNN" prefix (e.g. "code 502: 502 Bad
	// Gateway" from the exec dialer).
	for _, needle := range []string{
		"status 500", "status 501", "status 502", "status 503", "status 504",
		"bad gateway", "service unavailable", "gateway timeout",
		// FlexInfer shared-GPU proxy parks a model behind a higher-priority
		// primary and returns 503 service_unavailable; the underscore code form
		// and the worded "parked behind" don't match "service unavailable"
		// above. Live: 24 research 503s in 7d against a pinned parked model.
		"service_unavailable", "parked behind",
	} {
		if strings.Contains(lower, needle) {
			return ClassTransient
		}
	}

	// Sandbox process killed — SIGKILL (exit 137 = 128+9) or an OOMKill. A
	// devbox exec whose process is killed by the cgroup memory limit, a
	// deadline, or a pod eviction is a transient sandbox death (same family as
	// the k8s pod-GC / "backend unavailable" cases above): the check produced
	// NO verdict, and a fresh exec/pod frequently lands healthy. Left in
	// ClassCode (the default), a killed `fmt`/test exec was treated as a real
	// test failure — it burned all MaxAttempts and escalated with cost $0 and
	// no verdict (PIPE-psl-plan-council-add-a-mills-incident-triage-runbook-
	// …-1783261156, 2026-07-05: fmt hung ~3m17s then exit 137 ×3 → escalated).
	// ClassTransient so it retries free (off the code/MaxAttempts budget); the
	// transient hard cap still escalates a sandbox that is killed every time.
	// "exit code 137" is the k8s remotecommand form; "exit status 137" the
	// os/exec form.
	for _, needle := range []string{
		"exit code 137",
		"exit status 137",
		"signal: killed",
		"oomkilled",
	} {
		if strings.Contains(lower, needle) {
			return ClassTransient
		}
	}

	// Buildah / sandbox infrastructure failures. Persistent — counts
	// against attempts, but reported as ClassInfra so the operator
	// sees that the fix is at the image/k8s layer not the pipeline.
	for _, needle := range []string{
		"create buildah pod",
		"buildah build failed",
		"ensure sandbox: generate dockerfile",
		"sandbox image",
		"pull image",
		"image build failed",
		"already exists", // "pods ... already exists"
		"is forbidden",   // namespace/role denial
	} {
		if strings.Contains(lower, needle) {
			return ClassInfra
		}
	}

	return ClassCode
}

// IsFreeRetry reports whether a class should be retried without
// consuming the MaxAttempts budget. Transient + TransientQuota qualify;
// Infra + Code count against the budget.
func IsFreeRetry(c ErrorClass) bool {
	return c == ClassTransient || c == ClassTransientQuota
}

// IsTerminal reports whether a class should skip the retry loop
// entirely and escalate on first sight. Only Config qualifies: the
// failure is in GitLab project/MR configuration, so an identical retry
// can only return the identical error.
func IsTerminal(c ErrorClass) bool {
	return c == ClassConfig
}

// spawnSaturationNeedle identifies the HUD spawn API's pool-saturation
// rejection ("max concurrent spawns reached (N)"). Shared by Classify
// (→ ClassTransientQuota) and isSpawnSaturation (→ saturationBackoff)
// so the two can never disagree about what counts as saturation.
const spawnSaturationNeedle = "concurrent spawns"

// isSpawnSaturation reports whether err is the HUD spawn-pool saturation
// rejection, which clears on a slot-release timescale (a running spawn
// finishing, 10–30 min) rather than a rate-limit timescale (seconds).
func isSpawnSaturation(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), spawnSaturationNeedle)
}

// retryBackoff returns how long Drive should wait before re-dispatching a
// failed stage. Zero means retry immediately (Drive logs and loops).
// ClassTransientQuota waits (spawn-pool saturation gets the slot-release
// schedule, everything else the rate-limit schedule), and two spawn reasons
// carry their own schedule regardless of class: the missing-credential
// rollout window and the keyed-runtime identity collision.
func retryBackoff(cls ErrorClass, err error, attempt int) time.Duration {
	if reason, ok := SpawnInfraReason(err); ok {
		switch reason {
		// A credential-less spawn (spawn-auth-missing) is a delivery-window
		// outage: escalation #368 burned attempts 3+4 ~90s apart INSIDE the
		// 2026-07-22 01:54Z rollout window while the same secret+image+
		// invocation worked minutes before and after it. Immediate retries can
		// only re-hit the same window, so space them on the minutes scale it
		// actually clears.
		case SpawnReasonAuthMissing:
			return authOutageBackoff(attempt)
		// A keyed-spawn identity collision is blocked on the kubelet actually
		// reaping the prior pod, which an immediate retry cannot hasten — it
		// only re-observes the same DeletionTimestamp.
		case SpawnReasonRuntimeIdentityConflict:
			return identityConflictBackoff(attempt)
		}
	}
	if cls != ClassTransientQuota {
		return 0
	}
	if isSpawnSaturation(err) {
		return saturationBackoff(attempt)
	}
	return quotaBackoff(attempt)
}

// quotaBackoff returns an exponential backoff for the n-th attempt of a
// ClassTransientQuota retry. Caps at 32s so a rate-limited upstream
// can't keep the pipeline worker pinned for >30s. The cap matters more
// than the curve — quota errors are usually cleared within seconds.
func quotaBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	const maxBackoff = 32 * time.Second
	shift := attempt - 1
	if shift > 5 { // 1<<5 = 32s
		shift = 5
	}
	d := time.Duration(1<<shift) * time.Second
	if d > maxBackoff {
		d = maxBackoff
	}
	return d
}

// saturationBackoff is the retry spacing for HUD spawn-pool saturation.
// Unlike a 429 rate limit, a saturated pool frees a slot only when a
// running spawn FINISHES — typically 10–30 minutes — so the quotaBackoff
// schedule (32s cap, ~95s total across the 7 free retries of a run)
// burned the whole attempt cap in under two minutes and escalated while
// the pool was still busy; even with escalation_auto_retry_cap 2 the
// item human-escalated within minutes (escalations #220, #235).
// 1m·2^(n-1) capped at 5m spaces one run's free retries across ~27
// minutes (1+2+4+5+5+5+5) — long enough for a slot to actually free.
// Drive blocks between attempts for the wait, which is deliberate: an
// in-flight run idling is cheaper than converting it into an escalation,
// and it holds a MaxConcurrentRuns slot so the reconciler doesn't pile
// more runs onto the saturated pool.
func saturationBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	const maxBackoff = 5 * time.Minute
	shift := attempt - 1
	if shift > 3 { // 1<<3 = 8m, above the cap
		shift = 3
	}
	d := time.Duration(1<<shift) * time.Minute
	if d > maxBackoff {
		d = maxBackoff
	}
	return d
}

// authOutageBackoff is the retry spacing for spawn-auth-missing failures: the
// spawn ran with no usable credential because the Optional auth secret mount
// materialized absent/empty. These windows are rollout-churn scale (minutes —
// kubelet secret sync + fleet restart settling), not rate-limit scale:
// escalation #368's attempts 3+4 failed ~90s apart inside one such window
// while the identical secret+image+invocation succeeded at 01:43Z and again
// on a post-incident probe. 1m·2^(n-1) capped at 4m spaces a run's budgeted
// attempts across ~7 minutes of wall clock (plus pod build time), long enough
// to ride out the observed window without materially delaying a genuine
// persistent outage's escalation. Drive blocks between attempts for the wait
// (deliberate — same slot-holding rationale as saturationBackoff).
func authOutageBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	const maxBackoff = 4 * time.Minute
	shift := attempt - 1
	if shift > 2 { // 1<<2 = 4m = cap
		shift = 2
	}
	d := time.Duration(1<<shift) * time.Minute
	if d > maxBackoff {
		d = maxBackoff
	}
	return d
}

// identityConflictBackoff is the retry spacing for a keyed-spawn runtime
// identity collision (SpawnReasonRuntimeIdentityConflict). The blocker is a pod
// the kubelet has not finished reaping: the spawn preflight probes the SAME
// deterministic pod name every attempt, so until that pod is gone an immediate
// retry can only re-read the same DeletionTimestamp and return the identical
// HTTP 400. Live 2026-08-01 18:42Z: attempts 2-4 of a plan_slice stage failed
// inside ~40 seconds against one terminating pod, exhausting MaxAttempts before
// the pod had cleared, and escalated class=code.
//
// 30s·2^(n-1) capped at 60s. The floor is 3× the spawn pods' 10s termination
// grace period (internal/devbox/backend/k8s_objects.go), which is what a normal
// reap costs; the cap keeps a run from idling minutes per attempt on what is
// usually a seconds-scale window. The total is bounded by the stage budget
// rather than by this schedule — the collision classifies ClassTransient, so
// retries are free but still counted against the runner's
// maxAttempts+transientRetryCap hard cap (8 by default), giving ~6.5 minutes of
// worst-case waiting before the run escalates as a spawn-transport failure.
// Drive blocks between attempts for the wait (deliberate — same slot-holding
// rationale as saturationBackoff).
func identityConflictBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	const (
		baseBackoff = 30 * time.Second
		maxBackoff  = 60 * time.Second
	)
	shift := attempt - 1
	if shift > 1 { // 30s<<1 = 60s = cap
		shift = 1
	}
	d := baseBackoff << shift
	if d > maxBackoff {
		d = maxBackoff
	}
	return d
}
