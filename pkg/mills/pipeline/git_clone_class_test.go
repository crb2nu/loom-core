package pipeline

import (
	"strings"
	"testing"
)

// The error strings below are representative of what the capture layer
// (internal/devbox/backend: failedInitContainerDetail) now appends to a spawn
// or build failure — the opaque k8s terminated line followed by the real git
// stderr tail. git exit 128 renders identically for every cause, so the tail is
// what distinguishes them. Message formats are cited against git's / GitLab's
// actual output:
//   - repo not found:  `fatal: repository '<url>' not found`
//   - GitLab 404 body: `remote: The project you were looking for could not be found …`
//   - bad ref:         `fatal: couldn't find remote ref <ref>` /
//                      `fatal: Remote branch <ref> not found in upstream origin`
//   - auth:            `fatal: Authentication failed for '<url>'` /
//                      `remote: HTTP Basic: Access denied` / `error: … 403`
//   - dns/network:     `fatal: unable to access '<url>': Could not resolve host: <host>`
//                      `ssh: connect to host <host> port 22: Connection timed out`

func TestClassifyGitCloneError_Classes(t *testing.T) {
	cases := []struct {
		name        string
		msg         string
		wantClass   ErrorClass
		wantFound   bool
		wantMsgSubs []string // all must appear in Message
	}{
		{
			name:        "repository not found (git fatal, tonight's familyforge case)",
			msg:         "image build failed: buildah build failed: container git-clone terminated exit_code=128 reason=Error — git-clone log: Cloning into 'familyforge'... | fatal: repository 'https://token:glpat-REDACT@gitlab.blevins.dev/services/familyforge.git/' not found",
			wantClass:   ClassConfig,
			wantFound:   true,
			wantMsgSubs: []string{"services/familyforge", "does not exist", "create the GitLab repo", "bootstrap"},
		},
		{
			name:        "GitLab 404 project could not be found body",
			msg:         "pod not ready: container git-clone terminated exit_code=128 reason=Error — git-clone log: remote: The project you were looking for could not be found or you don't have permission to view it. | fatal: repository 'https://***@gitlab.blevins.dev/services/newthing.git/' not found",
			wantClass:   ClassConfig,
			wantFound:   true,
			wantMsgSubs: []string{"does not exist", "requeue"},
		},
		{
			name:        "http 404 returned error form",
			msg:         "buildah build failed: container git-clone terminated exit_code=128 — git-clone log: fatal: unable to access 'https://gitlab.blevins.dev/services/ghostrepo.git/': The requested URL returned error: 404",
			wantClass:   ClassConfig,
			wantFound:   true,
			wantMsgSubs: []string{"ghostrepo", "does not exist"},
		},
		{
			name:        "bad ref couldn't find remote ref",
			msg:         "image build failed: buildah build failed: container git-clone terminated exit_code=128 reason=Error — git-clone log: fatal: couldn't find remote ref refs/heads/feature-x",
			wantClass:   ClassConfig,
			wantFound:   true,
			wantMsgSubs: []string{"feature-x", "not found", "requeue"},
		},
		{
			name:        "bad ref remote branch not found",
			msg:         "pod not ready: container git-clone terminated exit_code=128 reason=Error — git-clone log: fatal: Remote branch release-9.9 not found in upstream origin",
			wantClass:   ClassConfig,
			wantFound:   true,
			wantMsgSubs: []string{"release-9.9", "not found"},
		},
		{
			name:        "auth failed authentication",
			msg:         "image build failed: buildah build failed: container git-clone terminated exit_code=128 reason=Error — git-clone log: fatal: Authentication failed for 'https://***@gitlab.blevins.dev/services/loom-core.git/'",
			wantClass:   ClassConfig,
			wantFound:   true,
			wantMsgSubs: []string{"git auth failed", "loom-core", "spawn git secret"},
		},
		{
			name:        "auth HTTP Basic access denied 403",
			msg:         "pod not ready: container git-clone terminated exit_code=128 — git-clone log: remote: HTTP Basic: Access denied | fatal: Authentication failed for 'https://gitlab.blevins.dev/services/loom-core.git/'",
			wantClass:   ClassConfig,
			wantFound:   true,
			wantMsgSubs: []string{"git auth failed", "token"},
		},
		{
			name:        "auth could not read Username (no credential)",
			msg:         "buildah build failed: container git-clone terminated exit_code=128 — git-clone log: fatal: could not read Username for 'https://gitlab.blevins.dev': No such device or address",
			wantClass:   ClassConfig,
			wantFound:   true,
			wantMsgSubs: []string{"git auth failed"},
		},
		{
			name:        "network could not resolve host (DNS blip) → transient",
			msg:         "image build failed: buildah build failed: container git-clone terminated exit_code=128 reason=Error — git-clone log: fatal: unable to access 'https://gitlab.blevins.dev/services/loom-core.git/': Could not resolve host: gitlab.blevins.dev",
			wantClass:   ClassTransient,
			wantFound:   true,
			wantMsgSubs: []string{"network error", "loom-core", "retry"},
		},
		{
			name:        "network connection timed out → transient",
			msg:         "pod not ready: container git-clone terminated exit_code=128 — git-clone log: fatal: unable to access 'https://gitlab.blevins.dev/services/loom-core.git/': Failed to connect to gitlab.blevins.dev port 443: Connection timed out",
			wantClass:   ClassTransient,
			wantFound:   true,
			wantMsgSubs: []string{"network error", "retry"},
		},
		{
			// The exact captured tail from PIPE-psl-plan-council-split-internal-
			// hud-spawn-… (2026-09-13): it used to fall through to the exit-128
			// default and escalate terminal config as "no git message was captured".
			name:        "GitLab HTTP 502 mid-clone (RPC failed + expected flush) → transient transport",
			msg:         "pod not ready: container git-clone terminated exit_code=128 reason=Error — git-clone log: Cloning into 'loom-core'... | error: RPC failed; HTTP 502 curl 22 The requested URL returned error: 502 | fatal: expected flush after ref listing",
			wantClass:   ClassTransient,
			wantFound:   true,
			wantMsgSubs: []string{"transport error", "loom-core", "retry"},
		},
		{
			name:        "early EOF in the pack stream → transient transport",
			msg:         "image build failed: buildah build failed: container git-clone terminated exit_code=128 reason=Error — git-clone log: Cloning into 'familyforge'... | fatal: early EOF | fatal: fetch-pack: invalid index-pack output",
			wantClass:   ClassTransient,
			wantFound:   true,
			wantMsgSubs: []string{"transport error", "familyforge", "retry"},
		},
		{
			name:        "remote end hung up → transient transport",
			msg:         "pod not ready: container git-clone terminated exit_code=128 — git-clone log: fatal: the remote end hung up unexpectedly",
			wantClass:   ClassTransient,
			wantFound:   true,
			wantMsgSubs: []string{"transport error", "retry"},
		},
		{
			name:      "a non-git early EOF must NOT be claimed as a clone transport failure",
			msg:       "stage tests errored: devbox exec stream: early EOF",
			wantFound: false,
		},
		{
			name:      "a gRPC rpc error is not git's RPC failed",
			msg:       "spawn: rpc error: code = Unavailable desc = transport is closing",
			wantFound: false,
		},
		{
			name:        "exit 128 with empty/unrecognized stderr → safe generic terminal-config",
			msg:         "image build failed: buildah build failed: container git-clone terminated exit_code=128 reason=Error",
			wantClass:   ClassConfig,
			wantFound:   true,
			wantMsgSubs: []string{"exit 128", "enable clone-error capture", "requeue"},
		},
		{
			name:      "non-git-clone error is not matched",
			msg:       "stage tests errored: devbox quality_gate: websocket: close 1006 (abnormal closure): unexpected EOF",
			wantFound: false,
		},
		{
			name:      "k8s pod-not-found transient must NOT match git-clone not-found",
			msg:       "pod not found during reconciliation",
			wantFound: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gc, ok := ClassifyGitCloneError(tc.msg)
			if ok != tc.wantFound {
				t.Fatalf("ClassifyGitCloneError found = %v, want %v (msg=%q)", ok, tc.wantFound, tc.msg)
			}
			if !tc.wantFound {
				return
			}
			if gc.Class != tc.wantClass {
				t.Errorf("class = %q, want %q (msg=%q)", gc.Class, tc.wantClass, tc.msg)
			}
			for _, sub := range tc.wantMsgSubs {
				if !strings.Contains(gc.Message, sub) {
					t.Errorf("Message %q missing substring %q", gc.Message, sub)
				}
			}
			// The spawn git token must never survive into the actionable
			// message that becomes a GitLab issue body.
			if strings.Contains(gc.Message, "glpat-") {
				t.Errorf("Message leaked a credential: %q", gc.Message)
			}
		})
	}
}

// TestClassifyGitCloneError_FeedsErrorClass locks in that the git-clone classes
// flow through the top-level Classify so budget accounting + the [class=…]
// escalation marker agree with the taxonomy — and, critically, that a
// repo-not-found is NOT swallowed by the generic "buildah build failed"/"image
// build failed" infra needles (the original bug).
func TestClassifyGitCloneError_FeedsErrorClass(t *testing.T) {
	cases := []struct {
		name string
		msg  string
		want ErrorClass
	}{
		{
			name: "repo-not-found wrapped in image build failed is CONFIG not INFRA",
			msg:  "image build failed: buildah build failed: container git-clone terminated exit_code=128 reason=Error — git-clone log: fatal: repository 'https://gitlab.blevins.dev/services/familyforge.git/' not found",
			want: ClassConfig,
		},
		{
			name: "auth wrapped in image build failed is CONFIG not INFRA",
			msg:  "image build failed: buildah build failed: container git-clone terminated exit_code=128 reason=Error — git-clone log: fatal: Authentication failed for 'https://gitlab.blevins.dev/services/loom-core.git/'",
			want: ClassConfig,
		},
		{
			name: "network clone blip is TRANSIENT",
			msg:  "image build failed: buildah build failed: container git-clone terminated exit_code=128 reason=Error — git-clone log: fatal: unable to access 'https://gitlab.blevins.dev/services/loom-core.git/': Could not resolve host: gitlab.blevins.dev",
			want: ClassTransient,
		},
		{
			name: "GitLab 502 clone transport blip is TRANSIENT, not the exit-128 config default",
			msg:  "image build failed: buildah build failed: container git-clone terminated exit_code=128 reason=Error — git-clone log: error: RPC failed; HTTP 502 curl 22 The requested URL returned error: 502 | fatal: expected flush after ref listing",
			want: ClassTransient,
		},
		{
			name: "exit-128 empty stderr fallback is CONFIG not INFRA",
			msg:  "image build failed: buildah build failed: container git-clone terminated exit_code=128 reason=Error",
			want: ClassConfig,
		},
		{
			name: "a genuine buildah build failure (no git-clone) stays INFRA",
			msg:  "image build failed: buildah build failed: container buildah terminated exit_code=1 reason=Error\nSTEP 5/9: RUN go build ./...\n./main.go:3:8: undefined: foo",
			want: ClassInfra,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(errString(tc.msg)); got != tc.want {
				t.Errorf("Classify() = %q, want %q (msg=%q)", got, tc.want, tc.msg)
			}
		})
	}
}

// TestClassifyGitCloneError_Terminality maps the retry semantics: config classes
// are terminal (fail fast, no retry burn); the network transient is a free
// retry so a DNS blip re-enters the queue instead of escalating a human.
func TestClassifyGitCloneError_Terminality(t *testing.T) {
	notFound, _ := ClassifyGitCloneError("container git-clone terminated exit_code=128 — git-clone log: fatal: repository 'https://h/services/x.git/' not found")
	if !IsTerminal(notFound.Class) {
		t.Errorf("repo-not-found class %q should be terminal", notFound.Class)
	}
	if IsFreeRetry(notFound.Class) {
		t.Errorf("repo-not-found class %q must not be a free retry (fail fast, no repo won't appear on retry)", notFound.Class)
	}
	net, _ := ClassifyGitCloneError("container git-clone terminated exit_code=128 — git-clone log: fatal: unable to access 'https://h/services/x.git/': Could not resolve host: h")
	if IsTerminal(net.Class) {
		t.Errorf("network class %q must not be terminal (a DNS blip should retry)", net.Class)
	}
	if !IsFreeRetry(net.Class) {
		t.Errorf("network class %q should be a free retry", net.Class)
	}
	blip, _ := ClassifyGitCloneError("container git-clone terminated exit_code=128 — git-clone log: error: RPC failed; HTTP 502 curl 22 The requested URL returned error: 502 | fatal: expected flush after ref listing")
	if IsTerminal(blip.Class) || !IsFreeRetry(blip.Class) {
		t.Errorf("transport class %q must be a free, non-terminal retry (a GitLab 5xx blip should retry)", blip.Class)
	}
}

// TestGitCloneEscalationReasonFeedsMetadata proves the end-to-end contract: the
// runner escalates a terminal git-clone failure with a "[class=config]"-marked
// reason carrying the actionable message, and escalationMetadataFromEvidence
// turns that into escalation_class=config / failure_class=configuration /
// retryable=false — so a missing-repo escalation is a config signal a human
// acts on, never an auto-requeued infra flake.
func TestGitCloneEscalationReasonFeedsMetadata(t *testing.T) {
	gc, ok := ClassifyGitCloneError("image build failed: buildah build failed: container git-clone terminated exit_code=128 reason=Error — git-clone log: fatal: repository 'https://gitlab.blevins.dev/services/familyforge.git/' not found")
	if !ok {
		t.Fatal("expected git-clone failure to be recognized")
	}
	// Mirror the runner's escalate envelope (runner.go terminal git-clone
	// branch), which passes gc.Class explicitly — the "[class=…]" text in the
	// reason is operator-facing prose, not the transport.
	reason := "stage plan_slice terminal git-clone error (not retried) [class=" + string(gc.Class) + "]: " + gc.Message

	md := escalationMetadataFromEvidence(gc.Class, reason, "")
	if md.EscalationClass != string(ClassConfig) {
		t.Errorf("EscalationClass = %q, want %q", md.EscalationClass, ClassConfig)
	}
	if md.FailureClass != string(FailureConfiguration) {
		t.Errorf("FailureClass = %q, want %q", md.FailureClass, FailureConfiguration)
	}
	if md.Retryable == nil || *md.Retryable {
		t.Errorf("Retryable = %v, want false (terminal config must not auto-requeue)", md.Retryable)
	}
}

// errString is a tiny error wrapper for the Classify table above.
type errString string

func (e errString) Error() string { return string(e) }

// TestClassifyGitCloneError_TransportFailures covers callers that pass only
// git's stderr tail (no `git-clone` container name in the text): git's own
// transport failure lines are enough to enter the classifier, and every
// HTTP 5xx / pack-stream shape lands in the transient transport family.
func TestClassifyGitCloneError_TransportFailures(t *testing.T) {
	tails := []string{
		"error: RPC failed; HTTP 502 curl 22 The requested URL returned error: 502 | fatal: expected flush after ref listing",
		"error: RPC failed; HTTP 503 curl 22 The requested URL returned error: 503",
		"error: RPC failed; HTTP 504 curl 22 The requested URL returned error: 504",
		"error: RPC failed; curl 22 The requested URL returned error: 500",
		"fatal: early EOF",
		"fatal: the remote end hung up unexpectedly",
		"error: RPC failed; curl 56 GnuTLS recv error (-54): Error in the pull function.",
		"fatal: unexpected disconnect while reading sideband packet",
	}
	for _, tail := range tails {
		t.Run(tail, func(t *testing.T) {
			got, ok := ClassifyGitCloneError(tail)
			if !ok || got.Class != ClassTransient || !strings.Contains(got.Message, "transport error") {
				t.Fatalf("got (%+v, %t), want transient transport", got, ok)
			}
		})
	}
}
