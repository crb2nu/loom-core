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

func TestIsFreeRetry(t *testing.T) {
	cases := []struct {
		c    ErrorClass
		want bool
	}{
		{ClassTransient, true},
		{ClassTransientQuota, true},
		{ClassInfra, false},
		{ClassCode, false},
		{"", false},
	}
	for _, tc := range cases {
		if got := IsFreeRetry(tc.c); got != tc.want {
			t.Errorf("IsFreeRetry(%s) = %v, want %v", tc.c, got, tc.want)
		}
	}
}
