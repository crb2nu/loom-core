package pipeline

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

// realClaudeInvalidAuthResult is the final Claude Code JSON-stream output
// recorded for spawn-b23f63aa88b4 during the 2026-07-25 dual-fleet canary.
// It has no HTTP status, so it must be classified from the producer payload.
const realClaudeInvalidAuthResult = `{"type":"assistant","message":{"id":"b4faa318-bb1f-409e-a4bc-08b40aa01d97","model":"<synthetic>","role":"assistant","stop_reason":"stop_sequence","stop_sequence":"","type":"message","usage":{"input_tokens":0,"output_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"server_tool_use":{"web_search_requests":0}},"content":[{"type":"text","text":"Invalid API key · Fix external API key"}]},"parent_tool_use_id":null,"session_id":"6964592c-927b-4dd1-848d-3cbcbce17ded"}` + "\n" +
	`{"type":"result","subtype":"success","is_error":true,"duration_ms":1404,"duration_api_ms":0,"num_turns":1,"result":"Invalid API key · Fix external API key","session_id":"6964592c-927b-4dd1-848d-3cbcbce17ded","total_cost_usd":0,"usage":{"input_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":0,"server_tool_use":{"web_search_requests":0}}}`

func TestErrorClass_ValidAndAll(t *testing.T) {
	all := AllErrorClasses()
	if len(all) != 5 {
		t.Fatalf("AllErrorClasses len = %d, want 5", len(all))
	}
	for _, c := range all {
		if !c.Valid() {
			t.Errorf("AllErrorClasses returned %q but Valid() is false", c)
		}
	}
	// A returned slice must be a copy — mutating it must not corrupt the
	// package-level taxonomy the metric label helper depends on.
	all[0] = ErrorClass("mutated")
	if AllErrorClasses()[0] == ErrorClass("mutated") {
		t.Fatal("AllErrorClasses leaked a mutable reference to the taxonomy")
	}
	// Invalid spellings must not validate — notably the FailureClass
	// spellings, which differ from the ErrorClass wire values.
	for _, c := range []ErrorClass{"", "infrastructure", "configuration", "unclassified", "wat"} {
		if c.Valid() {
			t.Errorf("ErrorClass(%q).Valid() = true, want false", c)
		}
	}
}

func TestClassify_KillTestFixtures(t *testing.T) {
	// All these error strings are real log_tail rows pulled from the
	// live operator's state.db on 2026-05-24 (kill-test report). If a
	// new failure mode shows up in stage_results.log_tail, add it here
	// as the first step before adjusting the classifier.
	cases := []struct {
		name string
		msg  string
		want ErrorClass
	}{
		{
			name: "k8s pod GC race (top transient cause)",
			msg:  "pod not found during reconciliation",
			want: ClassTransient,
		},
		{
			name: "devbox quality_gate mcphub close 1006",
			msg:  "stage=tests attempt=1: devbox quality_gate: mcphub: recv devbox/devbox_quality_gate: read message: websocket: close 1006 (abnormal closure): unexpected EOF",
			want: ClassTransient,
		},
		{
			name: "devbox quality_gate mcphub close 1000 backend unavailable",
			msg:  "stage=tests attempt=2: devbox quality_gate: mcphub: initialize devbox: read message: websocket: close 1000 (normal): Backend unavailable",
			want: ClassTransient,
		},
		{
			name: "broken pipe on send",
			msg:  "devbox quality_gate: mcphub: send devbox/devbox_quality_gate: write message: write tcp 10.42.4.85:45000->10.43.248.41:80: write: broken pipe",
			want: ClassTransient,
		},
		{
			name: "flexinfer context deadline exceeded",
			msg:  "flexinfer chat: Post \"http://flexinfer-proxy.flexinfer-system.svc.cluster.local/v1/chat/completions\": context deadline exceeded",
			want: ClassTransient,
		},
		{
			name: "buildah pod name conflict",
			msg:  "stage=plan_slice attempt=1 spawn=spawn-29009d2ffe87: image build failed: create buildah pod: pods \"buildah-build-spawn-runtime-codex-62269499de1bedeb\" already exists",
			want: ClassInfra,
		},
		{
			name: "buildah build failure",
			msg:  "image build failed: buildah build failed: build pod failed: exit_code=243 reason=Error",
			want: ClassInfra,
		},
		{
			name: "sandbox dockerfile generation",
			msg:  "devbox: decode body: invalid character 'e' looking for beginning of value; raw=\"ensure sandbox: generate dockerfile: no language detected",
			want: ClassInfra,
		},
		{
			name: "gate fail (real code issue)",
			msg:  "gate diff_size: diff exceeds 200 lines",
			want: ClassCode,
		},
		{
			name: "test failure (real code issue)",
			msg:  "go test FAIL: TestFoo not equal to bar",
			want: ClassCode,
		},
		{
			name: "rate limit",
			msg:  "flexinfer chat: status 429: too many requests",
			want: ClassTransientQuota,
		},
		{
			name: "gitlab 503",
			msg:  "gitlab: GET /projects/47/merge_requests/12: status 503: service unavailable",
			want: ClassTransient,
		},
		{
			name: "gitlab 502",
			msg:  "gitlab: POST /projects/47/merge_requests: status 502: Bad Gateway",
			want: ClassTransient,
		},
		{
			// HUD spawn-pool saturation must back off + retry, not escalate as code.
			name: "spawn pool saturated",
			msg:  `hud spawn: POST status 400: {"ok":false,"error":{"code":"spawn_error","message":"max concurrent spawns reached (3)"}}`,
			want: ClassTransientQuota,
		},
		{
			// Escalation #306 (2026-07-10): implement burned attempts 3–4 on a
			// spawn pod that never became ready, classified as code. The pod
			// never ran an agent, so a fresh spawn loses nothing — transient.
			name: "spawn pod never ready (watch closed)",
			msg:  "hud spawn spawn-e54c3222da24 status=failed: pod creation failed: pod not ready: watch closed for pod spawn-spawn-e54c3222da24: spawn: terminal non-completed status",
			want: ClassTransient,
		},
		{
			name: "spawn pod deleted before Running (GC race)",
			msg:  "pod creation failed: pod not ready: pod spawn-abc123 was deleted before reaching Running",
			want: ClassTransient,
		},
		{
			// Image-pull failure is persistent registry/image misconfig — the
			// "pod not ready" wrapper must not classify it as a free retry.
			name: "spawn pod image pull backoff",
			msg:  "pod creation failed: pod not ready: image pull error in devbox: ImagePullBackOff — Back-off pulling image \"registry.harbor.lan/library/devbox:abc\"",
			want: ClassInfra,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Classify(errors.New(tc.msg))
			if got != tc.want {
				t.Errorf("Classify(%q) = %s, want %s", tc.msg, got, tc.want)
			}
		})
	}
}

func TestClassify_ClaudeCLIJSONStreamFailures(t *testing.T) {
	// These mirror Claude Code --output-format stream-json terminal events. The
	// quota cases pin the producer's machine-readable error fields; the auth
	// case below is the captured final JSON stream from the dual-fleet canary.
	cases := []struct {
		name string
		msg  string
		want ErrorClass
	}{
		{
			name: "rate-limit error event",
			msg:  `{"type":"result","subtype":"error_during_execution","is_error":true,"errors":["API Error: rate_limit_error"]}`,
			want: ClassTransientQuota,
		},
		{
			name: "usage-limit result event",
			msg:  `{"type":"result","subtype":"error_during_execution","is_error":true,"result":"Claude usage limit reached"}`,
			want: ClassTransientQuota,
		},
		{
			name: "overloaded error event",
			msg:  `{"type":"result","subtype":"error_during_execution","is_error":true,"errors":["API Error: overloaded_error"]}`,
			want: ClassTransientQuota,
		},
		{
			name: "captured invalid auth event (spawn-b23f63aa88b4)",
			msg:  realClaudeInvalidAuthResult,
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

func TestClassify_ClaudeRateLimitEscalationRoundTrip(t *testing.T) {
	// Mirrors the runner's persisted escalation reason shape from GitLab issue
	// #386. The issue retained the HUD wrapper but truncated stdout before the
	// terminal quota event, so the stdout fixture stays focused on the Claude
	// stream-json producer field this classifier consumes.
	stdout := `{"type":"result","subtype":"error_during_execution","is_error":true,"errors":["API Error: rate_limit_error"]}`
	if got := Classify(errors.New(stdout)); got != ClassTransientQuota {
		t.Fatalf("Classify(Claude rate-limit stdout) = %q, want %q", got, ClassTransientQuota)
	}
	// The class now travels as an argument, not as prose the runner re-parses,
	// so what matters is that the label the escalation records equals the class
	// the call site classified.
	reason := fmt.Sprintf("stage plan_slice errored after 3 attempts [class=%s]: hud spawn spawn-d27380fcae08 status=failed: agent CLI exited 1 (stdout: %s)", Classify(errors.New(stdout)), stdout)
	if got := escalationClassLabel(Classify(errors.New(stdout)), reason, ""); got != string(ClassTransientQuota) {
		t.Fatalf("escalationClassLabel = %q, want %q", got, ClassTransientQuota)
	}
}

func TestClassify_ModelUnavailableParked503(t *testing.T) {
	// FlexInfer shared-GPU proxy parks a model behind a higher-priority primary
	// and returns 503 service_unavailable. These must classify TRANSIENT
	// (retryable free), never code — a still-parked model is not a code defect
	// (live 2026-07-16: 24 research 503s in 7d, one run 8× against one parked
	// model). Covers the string surface forms AND the wrapped sentinel.
	cases := []struct {
		name string
		err  error
	}{
		{
			// Exact live shape from the plan doc: status 503 + parked + underscore code.
			name: "live parked 503 body",
			err:  errors.New(`flexinfer chat: status 503: {"error":{"message":"model 'qwen35-35b-clean-gptq-workhorse' is parked behind a higher-priority primary","type":"service_unavailable"}}`),
		},
		{
			// Aggregate error with no "status 503" prefix — only the underscore
			// code + "parked behind" carry the signal.
			name: "underscore code without status prefix",
			err:  errors.New("flexinfer chat: model 'x' parked behind a higher-priority primary (service_unavailable)"),
		},
		{
			name: "wrapped ErrModelUnavailable sentinel",
			err:  fmt.Errorf("flexinfer chat: all candidate models unavailable (tried [a b]): %w", ErrModelUnavailable),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(tc.err); got != ClassTransient {
				t.Errorf("Classify(%v) = %s, want transient", tc.err, got)
			}
			// Transient is a free retry — so any residual escalation stays
			// retryable, not code-class (S2 requirement 4).
			if !IsFreeRetry(Classify(tc.err)) {
				t.Errorf("model-unavailable must be a free retry: %v", tc.err)
			}
			if fc := FailureClassFromErrorClass(Classify(tc.err)); !fc.Retryable() {
				t.Errorf("model-unavailable failure class %s must be retryable", fc)
			}
		})
	}
}

// TestClassify_FlexInferRawUpstream5xx pins the exact log_tail shapes the
// 2026-07-26 audit found on 3 research escalations — a raw upstream 5xx from the
// LiteLLM/flexinfer proxy with no structured body. These must classify TRANSIENT
// (free retry, infra-side), never code: the FlexInfer client now parks the model
// and walks the candidate chain on these, and the residual all-candidates error
// wraps ErrModelUnavailable, so both the bare and aggregate forms are covered.
func TestClassify_FlexInferRawUpstream5xx(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{
			// Verbatim live log_tail (escalations, 2026-07-26).
			name: "live raw 500",
			err:  errors.New("stage=research attempt=1: flexinfer chat: status 500: Internal Server Error"),
		},
		{
			name: "raw 502 bad gateway",
			err:  errors.New("flexinfer chat: status 502: Bad Gateway"),
		},
		{
			name: "raw 504 gateway timeout",
			err:  errors.New("flexinfer chat: status 504: Gateway Timeout"),
		},
		{
			// What the client now returns once every candidate 5xxs: the
			// sentinel wrap carrying the last real proxy body.
			name: "all candidates 5xx wraps sentinel",
			err: fmt.Errorf("flexinfer chat: all candidate models unavailable (tried [a b]): %w (last: %v)",
				ErrModelUnavailable, errors.New("flexinfer chat: status 500: Internal Server Error")),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(tc.err); got != ClassTransient {
				t.Errorf("Classify(%v) = %s, want transient", tc.err, got)
			}
			if !IsFreeRetry(Classify(tc.err)) {
				t.Errorf("raw upstream 5xx must be a free retry: %v", tc.err)
			}
		})
	}
}

func TestClassify_NilAndEOF(t *testing.T) {
	if got := Classify(nil); got != "" {
		t.Errorf("Classify(nil) = %q, want empty", got)
	}
	if got := Classify(io.EOF); got != ClassTransient {
		t.Errorf("Classify(io.EOF) = %s, want transient", got)
	}
	if got := Classify(io.ErrUnexpectedEOF); got != ClassTransient {
		t.Errorf("Classify(io.ErrUnexpectedEOF) = %s, want transient", got)
	}
}

func TestClassify_DefaultsToCode(t *testing.T) {
	// Unrecognized errors must be conservative: classify as ClassCode
	// so they consume the attempt budget and the operator gets pulled
	// in, never accidentally a free infinite retry.
	got := Classify(errors.New("some bizarre unrecognized failure"))
	if got != ClassCode {
		t.Errorf("Classify(unknown) = %s, want code", got)
	}
}

func TestClassify_KubeletExecDial502(t *testing.T) {
	// Real log_tail from the 2026-06-05 A2 kill-test: a plan_slice codex
	// spawn on k3s-w-10 failed because the apiserver could not reach the
	// node's kubelet (:10250) for exec streaming. Before this fix it fell
	// through to ClassCode and escalated after the attempt cap (turn_count=0,
	// $0 cost), blocking the first harvester-vm canary. It is a transient
	// konnectivity blip — the node was Ready before and after — so it must
	// be a free retry.
	cases := []struct {
		name string
		msg  string
		want ErrorClass
	}{
		{
			name: "kubelet exec dial 502 bad gateway (the A2 fixture)",
			msg:  "agent CLI exited 1: exec error: error dialing backend: proxy error from 127.0.0.1:6443 while dialing 192.168.50.213:10250, code 502: 502 Bad Gateway",
			want: ClassTransient,
		},
		{
			name: "error dialing backend without a status prefix",
			msg:  "stage=implement attempt=1 spawn=spawn-abc: exec error: error dialing backend: EOF",
			want: ClassTransient,
		},
		{
			name: "worded 503 service unavailable",
			msg:  "spawn poll: 503 Service Unavailable",
			want: ClassTransient,
		},
		{
			name: "worded 504 gateway timeout",
			msg:  "spawn stream: 504 Gateway Timeout",
			want: ClassTransient,
		},
		{
			// Real escalation 2026-07-05: the tests-stage fmt exec hung ~3m17s
			// then was SIGKILLed (exit 137). Classified ClassCode (default), it
			// burned all attempts and escalated as a fake test failure.
			name: "sandbox exec SIGKILL (exit 137, k8s remotecommand form)",
			msg:  "stage=tests attempt=1: FAIL fmt (3495ms): exec error: command terminated with exit code 137",
			want: ClassTransient,
		},
		{
			name: "sandbox exec killed (os/exec form)",
			msg:  "stage=tests attempt=1: go test: signal: killed",
			want: ClassTransient,
		},
		{
			name: "pod OOMKilled",
			msg:  "spawn poll: container tests terminated: reason=OOMKilled exit_code=137",
			want: ClassTransient,
		},
		{
			// Guard: a real test failure that merely mentions 137 in its output
			// must NOT be reclassified as infra — only the exit-code signatures do.
			name: "real test failure mentioning 137 stays code",
			msg:  "stage=tests attempt=1: FAIL: TestQueue got 137 want 42",
			want: ClassCode,
		},
		{
			// Guard: an in-process Go OOM ("runtime: out of memory") is not the
			// exec-kill signature — it stays code (could be a real memory leak).
			name: "runtime out of memory stays code",
			msg:  "stage=tests attempt=1: fatal error: runtime: out of memory",
			want: ClassCode,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(errors.New(tc.msg)); got != tc.want {
				t.Errorf("Classify(%q) = %s, want %s", tc.msg, got, tc.want)
			}
		})
	}
}

func TestIsFreeRetry(t *testing.T) {
	cases := []struct {
		c    ErrorClass
		want bool
	}{
		{ClassTransient, true},
		{ClassTransientQuota, true},
		{ClassInfra, false},
		{ClassCode, false},
		{ClassConfig, false},
		{"", false},
	}
	for _, tc := range cases {
		if got := IsFreeRetry(tc.c); got != tc.want {
			t.Errorf("IsFreeRetry(%s) = %v, want %v", tc.c, got, tc.want)
		}
	}
}

// TestClassify_Merge405IsConfig pins the DEBT-073(b) contract: GitLab's
// 405 Method Not Allowed on the merge stage is a terminal config error
// (merge method / MWPS / approvals), never a retryable class. The first
// fixture is the GitLab client's real error shape
// (pkg/mills/clients/gitlab.go doRequest); escalations #148/#150
// retried it verbatim 3× before this.
func TestClassify_Merge405IsConfig(t *testing.T) {
	cases := []struct {
		name string
		msg  string
	}{
		{
			name: "gitlab client status-line shape",
			msg:  `gitlab: PUT /projects/services%2Floom-core/merge_requests/598/merge: status 405: {"message":"405 Method Not Allowed"}`,
		},
		{
			name: "worded form without status prefix",
			msg:  "merge mr 598: method not allowed",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(errors.New(tc.msg)); got != ClassConfig {
				t.Errorf("Classify(%q) = %s, want %s", tc.msg, got, ClassConfig)
			}
		})
	}
}

// TestClassify_QualityGateNoChecksSelectorIsConfig pins the class for
// devbox_quality_gate's zero-executed-checks refusal
// (cmd/mcp-devbox/quality_gate.go): the checks selector comes from
// pipeline policy, so an identical retry can only fail identically.
// Distinct from ErrDevboxGateNoChecks (a not-passed verdict that
// arrived WITH zero checks — infra contract violation, transient).
func TestClassify_QualityGateNoChecksSelectorIsConfig(t *testing.T) {
	msg := "devbox quality_gate: mcphub: devbox/devbox_quality_gate reported error: quality gate executed no checks (requested [bogus], language go); refusing to report a verdict"
	if got := Classify(errors.New(msg)); got != ClassConfig {
		t.Errorf("Classify(%q) = %s, want %s", msg, got, ClassConfig)
	}
}

// TestClassify_SandboxStillBuildingIsInfra guards escalation #322
// (2026-07-16): a cold sandbox image build that outlived the quality
// gate's build wait surfaced as "ensure sandbox: sandbox image still
// building after 8m0s". The client-side strict parse now propagates
// that text instead of fabricating a 0-checks verdict; it must land in
// the infra class, not code.
func TestClassify_SandboxStillBuildingIsInfra(t *testing.T) {
	msg := `devbox quality_gate: mcphub: devbox/devbox_quality_gate reported error: ensure sandbox: sandbox image still building after 8m0s: build in progress; raw="ensure sandbox: sandbox image still building after 8m0s"`
	if got := Classify(errors.New(msg)); got != ClassInfra {
		t.Errorf("Classify(%q) = %s, want %s", msg, got, ClassInfra)
	}
}

// TestClassify_Merge422IsConfig guards escalation #287 (2026-07-07): GitLab
// rejects a conflicted merge with 422 "Branch cannot be merged" — the fix is a
// rebase, so an identical retry can only return the identical error. The old
// default (ClassCode) burned 3 merge attempts in ~1.5s total before escalating.
func TestClassify_Merge422IsConfig(t *testing.T) {
	cases := []struct {
		name string
		msg  string
	}{
		{
			name: "gitlab client status-line shape",
			msg:  `gitlab: PUT /projects/services%2Floom-core/merge_requests/987/merge: status 422: {"message":"Branch cannot be merged"}`,
		},
		{
			name: "worded form without status prefix",
			msg:  "merge mr 987: branch cannot be merged",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(errors.New(tc.msg)); got != ClassConfig {
				t.Errorf("Classify(%q) = %s, want %s", tc.msg, got, ClassConfig)
			}
		})
	}
}

// TestClassify_ContainerNotFoundIsTransient guards escalation #289
// (2026-07-08): a devbox tests exec against a recycled sandbox pod fails with
// `unable to upgrade connection: container not found ("devbox")`. A fresh exec
// lands on the replacement pod, so this is transient — the old default
// (ClassCode) burned all 3 attempts in ~4s and escalated as a code failure.
func TestClassify_ContainerNotFoundIsTransient(t *testing.T) {
	cases := []string{
		`exec error: unable to upgrade connection: container not found ("devbox")`,
		`devbox quality gate failed (1/1 checks failed: fmt[exit=1]: unable to upgrade connection: container not found ("devbox"))`,
	}
	for _, msg := range cases {
		if got := Classify(errors.New(msg)); got != ClassTransient {
			t.Errorf("Classify(%q) = %s, want %s", msg, got, ClassTransient)
		}
	}
}

// TestClassify_PipelinePollTimeoutIsInfra guards DEBT-073 (#167) class a: a
// ci_watch pipeline-poll timeout must classify ClassInfra, not the default
// ClassCode, so the escalation-class metric attributes it to the CI/cluster
// layer (escalations #149/#153) instead of conflating a stuck pipeline with a
// real code bug. Detection is via errors.Is on the wrapped sentinel so the
// embedded pipeline web_url in the message can't shift the result.
func TestClassify_PipelinePollTimeoutIsInfra(t *testing.T) {
	// The exact shape the GitLab client emits (fmt.Errorf("...: %w", ...)): a
	// human-readable prefix, an embedded pipeline URL, and the wrapped sentinel.
	err := fmt.Errorf("gitlab: pipeline poll timed out after 30m0s (pipeline: https://gitlab.example/services/loom-core/-/pipelines/12345): %w", ErrPipelinePollTimeout)
	if got := Classify(err); got != ClassInfra {
		t.Fatalf("Classify(pipeline poll timeout) = %s, want %s", got, ClassInfra)
	}
	// Infra shares Code's retry accounting: it is not a free transient retry
	// (so it counts against MaxAttempts, bounding total wall-clock) and it is
	// not terminal (a genuinely slow pipeline can still go green on a re-poll).
	if IsFreeRetry(ClassInfra) {
		t.Error("ClassInfra must not be a free retry")
	}
	if IsTerminal(ClassInfra) {
		t.Error("ClassInfra must not be terminal")
	}
}

func TestClassify_CIPipelineTerminalJobReasons(t *testing.T) {
	tests := []struct {
		name    string
		reasons []string
		want    ErrorClass
	}{
		{name: "all runner system failures", reasons: []string{"runner_system_failure", "runner_system_failure"}, want: ClassTransient},
		{name: "script failure", reasons: []string{"script_failure"}, want: ClassCode},
		{name: "mixed", reasons: []string{"runner_system_failure", "script_failure"}, want: ClassCode},
		{name: "unknown", reasons: []string{""}, want: ClassCode},
		{name: "missing inspection", reasons: nil, want: ClassCode},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := &CIPipelineTerminalError{Status: "failed", MRIID: 42, FailedJobReasons: tt.reasons}
			if got := Classify(err); got != tt.want {
				t.Fatalf("Classify(%v) = %s, want %s", err, got, tt.want)
			}
			if !errors.Is(err, ErrCIPipelineTerminal) {
				t.Fatal("typed terminal CI error must wrap ErrCIPipelineTerminal")
			}
		})
	}
}

func TestCIPipelineTerminalError_PostRescueNamesBothFailures(t *testing.T) {
	err := &CIPipelineTerminalError{
		Status:          "failed",
		MRIID:           42,
		AutoRetried:     true,
		FirstFailedJobs: []FailedJob{{Name: "test:reliability"}},
		FailedJobs:      []FailedJob{{Name: "test:unit"}},
	}
	want := "test:reliability failed, auto-retried once, failed again (test:unit)"
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("Error() = %q, want %q", err.Error(), want)
	}
}

func TestClassify_MergeAuthorizationSentinelsAreConfig(t *testing.T) {
	for _, sentinel := range []error{
		ErrMergeRequestClosed,
		ErrMergeAuthorizationStale,
		ErrMergeRecoveryConfig,
	} {
		err := fmt.Errorf("gitlab merge stopped: %w", sentinel)
		if got := Classify(err); got != ClassConfig {
			t.Fatalf("Classify(%v) = %s, want %s", err, got, ClassConfig)
		}
	}
}

// TestClassify_UnbuildableMRSentinelsBeatPollTimeout guards the ordering that
// makes both wedge fixes work: an MR that can never produce a pipeline is
// terminal CONFIG, and must be classified before the ErrPipelinePollTimeout arm
// would launder it into the retryable infra bucket (where ci_watch spends its
// full 90m extension budget re-observing the identical state).
func TestClassify_UnbuildableMRSentinelsBeatPollTimeout(t *testing.T) {
	for _, sentinel := range []error{
		ErrMRHeadSHAUnavailable,
		ErrBranchPipelineUnavailable,
	} {
		// Wrapped alongside the poll-timeout sentinel: the config verdict must
		// still win, whichever order the wrapping happens to take.
		err := fmt.Errorf("ci watch: %w (%w)", sentinel, ErrPipelinePollTimeout)
		if got := Classify(err); got != ClassConfig {
			t.Fatalf("Classify(%v) = %s, want %s", err, got, ClassConfig)
		}
		if !IsTerminal(Classify(err)) {
			t.Fatalf("%v must be terminal so the run escalates instead of re-watching", sentinel)
		}
	}
}

func TestClassify_MergeRequestLockedSentinelWinsOver405Text(t *testing.T) {
	err := fmt.Errorf("gitlab merge status 405 while authoritative MR state is locked: %w", ErrMergeRequestLocked)
	if got := Classify(err); got != ClassTransient {
		t.Fatalf("Classify(%v) = %s, want %s", err, got, ClassTransient)
	}
	if !IsFreeRetry(ClassTransient) || IsTerminal(ClassTransient) {
		t.Fatal("locked MR must use the Runner's bounded transient retry path")
	}
}

func TestIsTerminal(t *testing.T) {
	cases := []struct {
		c    ErrorClass
		want bool
	}{
		{ClassConfig, true},
		{ClassTransient, false},
		{ClassTransientQuota, false},
		{ClassInfra, false},
		{ClassCode, false},
		{"", false},
	}
	for _, tc := range cases {
		if got := IsTerminal(tc.c); got != tc.want {
			t.Errorf("IsTerminal(%s) = %v, want %v", tc.c, got, tc.want)
		}
	}
}

// Regression for escalations #220/#235: the HUD spawn API's 400
// "max concurrent spawns reached (N)" is classified transient_quota, but the
// generic quotaBackoff schedule (32s cap, ~95s across the 7 free retries of a
// run) is rate-limit-scaled — a saturated pool frees a slot only when a
// running spawn FINISHES (10–30 min), so #235 burned all 8 attempts and
// human-escalated while the pool was still busy. retryBackoff must route the
// saturation shape to the slot-release schedule and keep real rate limits on
// the seconds schedule.
func TestRetryBackoff_SpawnSaturationUsesSlotReleaseSchedule(t *testing.T) {
	// Exact live error shape from issue #235.
	satErr := errors.New(`hud spawn: POST status 400: {"ok":false,"error":{"code":"spawn_error","message":"max concurrent spawns reached (6)"}}`)
	rateErr := errors.New("flexinfer chat: status 429: too many requests")

	if got := Classify(satErr); got != ClassTransientQuota {
		t.Fatalf("Classify(saturation) = %s, want transient_quota", got)
	}

	// Saturation → minutes schedule (1m, 2m, 4m, then 5m cap).
	wantSat := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 5 * time.Minute, 5 * time.Minute}
	for i, want := range wantSat {
		if got := retryBackoff(ClassTransientQuota, satErr, i+1); got != want {
			t.Errorf("retryBackoff(saturation, attempt %d) = %s, want %s", i+1, got, want)
		}
	}

	// Real rate limit → unchanged seconds schedule.
	wantRate := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}
	for i, want := range wantRate {
		if got := retryBackoff(ClassTransientQuota, rateErr, i+1); got != want {
			t.Errorf("retryBackoff(429, attempt %d) = %s, want %s", i+1, got, want)
		}
	}

	// Non-quota classes never wait.
	if got := retryBackoff(ClassTransient, satErr, 3); got != 0 {
		t.Errorf("retryBackoff(transient) = %s, want 0", got)
	}
	if got := retryBackoff(ClassCode, satErr, 3); got != 0 {
		t.Errorf("retryBackoff(code) = %s, want 0", got)
	}
}

// TestRetryBackoff_SpawnAuthMissingUsesRolloutWindowSchedule pins the #368
// regression: a credential-less spawn (spawn-auth-missing) must NOT retry
// immediately — attempts 3+4 of PIPE-mills-spawn-stall-fast-kill burned ~90s
// apart inside the same 2026-07-22 01:54Z rollout window that broke the
// Optional auth secret mount, while the identical secret+image+invocation
// worked minutes before and after. The minutes-scale schedule rides out the
// window; other infra reasons keep the immediate retry.
func TestRetryBackoff_SpawnAuthMissingUsesRolloutWindowSchedule(t *testing.T) {
	authErr := errors.New(realSpawnAuthMissingReason)

	// The class stays retryable infra (bounded budget), yet gets a backoff —
	// the one exception to "non-quota classes never wait".
	if got := Classify(authErr); got != ClassInfra {
		t.Fatalf("Classify(auth-missing) = %s, want infra", got)
	}
	want := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 4 * time.Minute}
	for i, w := range want {
		if got := retryBackoff(ClassInfra, authErr, i+1); got != w {
			t.Errorf("retryBackoff(auth-missing, attempt %d) = %s, want %s", i+1, got, w)
		}
	}

	// Other spawn-infra reasons (timeout, stdin, driver-lost) retry
	// immediately — a fresh spawn against a healthy layer needs no wait.
	for _, msg := range []string{realSpawnTimeoutReason, realSpawnStdinReason, realSpawnDriverLostReason} {
		if got := retryBackoff(ClassInfra, errors.New(msg), 2); got != 0 {
			t.Errorf("retryBackoff(%q) = %s, want 0", msg, got)
		}
	}

	// Degenerate attempt numbers clamp instead of misbehaving.
	if got := authOutageBackoff(0); got != time.Minute {
		t.Errorf("authOutageBackoff(0) = %s, want 1m", got)
	}
}

// TestRetryBackoff_IdentityConflictWaitsForPodReap pins the second half of the
// 2026-08-01 18:42Z regression: attempts 2, 3 and 4 of the plan_slice stage
// re-dispatched inside ~40 seconds total, each one probing the SAME
// deterministic pod that was still terminating, so all three could only re-read
// the same DeletionTimestamp and return the identical HTTP 400. The budget was
// gone before the kubelet had finished reaping the pod.
//
// The collision must now space its retries on the reap timescale. Everything
// else keeps its existing behaviour: the pod-lifecycle half of the same
// incident retries immediately (its pod is already gone — waiting buys nothing).
func TestRetryBackoff_IdentityConflictWaitsForPodReap(t *testing.T) {
	conflictErr := errors.New(realSpawnIdentityConflictReason)

	// Free-retry class AND a backoff — the second exception to "non-quota
	// classes never wait" (spawn-auth-missing is the first).
	if got := Classify(conflictErr); got != ClassTransient {
		t.Fatalf("Classify(identity conflict) = %s, want transient", got)
	}
	want := []time.Duration{30 * time.Second, time.Minute, time.Minute, time.Minute}
	for i, w := range want {
		if got := retryBackoff(ClassTransient, conflictErr, i+1); got != w {
			t.Errorf("retryBackoff(identity conflict, attempt %d) = %s, want %s", i+1, got, w)
		}
	}

	// The ~40s that attempts 2-4 burned live must no longer fit the same
	// three retries: the first three waits alone have to exceed it.
	var firstThree time.Duration
	for attempt := 1; attempt <= 3; attempt++ {
		firstThree += identityConflictBackoff(attempt)
	}
	if firstThree <= 40*time.Second {
		t.Errorf("backoff across attempts 1-3 = %s, want > 40s (the live burn window)", firstThree)
	}

	// Bounded by the stage budget, not by this schedule: with the default
	// max_attempts 3 + transient_retry_cap 5 the runner waits before attempts
	// 1..7 and escalates at the cap-8 total, so the worst case stays minutes.
	var total time.Duration
	for attempt := 1; attempt <= 7; attempt++ {
		d := identityConflictBackoff(attempt)
		if d > time.Minute {
			t.Fatalf("identityConflictBackoff(%d) = %s, want <= 60s cap", attempt, d)
		}
		total += d
	}
	if total > 10*time.Minute {
		t.Errorf("total backoff across 7 free retries = %s, want <= 10m", total)
	}

	// The pod-lifecycle half of the incident has no pod left to wait on.
	if got := retryBackoff(ClassTransient, errors.New(realSpawnPodLifecycleReason), 2); got != 0 {
		t.Errorf("retryBackoff(pod-lifecycle) = %s, want 0", got)
	}

	// Degenerate attempt numbers clamp instead of misbehaving.
	for _, attempt := range []int{0, -3} {
		if got := identityConflictBackoff(attempt); got != 30*time.Second {
			t.Errorf("identityConflictBackoff(%d) = %s, want 30s", attempt, got)
		}
	}
}

// The core invariant behind the fix: one run's free retries must span long
// enough for a typical spawn (10–30 min) to finish and free a slot. With
// max_attempts 3 + transient_retry_cap 5 the runner waits between attempts
// 1..7 before the cap-8 escalation; that total must comfortably exceed the
// old ~95s (which guaranteed escalating into a still-saturated pool).
func TestSaturationBackoff_SpansSlotRelease(t *testing.T) {
	var total time.Duration
	for attempt := 1; attempt <= 7; attempt++ {
		d := saturationBackoff(attempt)
		if d <= 0 {
			t.Fatalf("saturationBackoff(%d) = %s, want > 0", attempt, d)
		}
		if d > 5*time.Minute {
			t.Fatalf("saturationBackoff(%d) = %s, want <= 5m cap", attempt, d)
		}
		total += d
	}
	if total < 20*time.Minute {
		t.Errorf("total backoff across 7 free retries = %s, want >= 20m (slot-release scale)", total)
	}
	// Degenerate attempt numbers clamp instead of misbehaving.
	if got := saturationBackoff(0); got != time.Minute {
		t.Errorf("saturationBackoff(0) = %s, want 1m", got)
	}
	if got := saturationBackoff(-3); got != time.Minute {
		t.Errorf("saturationBackoff(-3) = %s, want 1m", got)
	}
}

func TestIsSpawnSaturation(t *testing.T) {
	if !isSpawnSaturation(errors.New("hud spawn: POST status 400: max concurrent spawns reached (3)")) {
		t.Error("live saturation shape not detected")
	}
	if isSpawnSaturation(errors.New("flexinfer chat: status 429")) {
		t.Error("429 misdetected as saturation")
	}
	if isSpawnSaturation(nil) {
		t.Error("nil misdetected as saturation")
	}
}
