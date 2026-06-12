package pipeline

import (
	"errors"
	"io"
	"testing"
)

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
