package sigfp

import "testing"

func TestClassifyCuratedOpenRouter402(t *testing.T) {
	want := Classification{
		Signature: OpenRouter402SignatureID, Class: ExternalDependencyIncident,
		External: "openrouter", Capability: "llm_provider",
		Retryable: false, FreeRetry: false,
	}
	positives := []string{
		`OpenRouter HTTP 402: insufficient credits for this request`,
		`OpenRouter 402: insufficient credits for this request`,
		`OpenRouter Payment Required: insufficient credits for this request`,
		`provider=openrouter status=402: this request requires more credits`,
		`OpenRouter response: {"error":{"code":402,"message":"insufficient credits"}}`,
		`OPENROUTER status_code: 402: request requires more credits`,
		`OpenRouter HTTP/1.1 402: credits exhausted`,
		`provider OpenRouter status: 402 Payment Required`,
	}
	for _, evidence := range positives {
		t.Run(evidence, func(t *testing.T) {
			got, matched := ClassifyCurated(evidence)
			if !matched || got != want {
				t.Fatalf("ClassifyCurated() = %+v, %v; want %+v, true", got, matched, want)
			}
		})
	}
}

func TestClassifyCuratedOpenRouter402NearMisses(t *testing.T) {
	nearMisses := []string{
		`Stripe HTTP 402: insufficient credits for this request`,
		`OpenRouter HTTP 429: insufficient credits for this request`,
		`HTTP 402: insufficient credits for this request`,
		`Stripe status=402: payment required`,
		`OpenRouter HTTP 401: payment required`,
		`OpenRouter HTTP 500: credits exhausted`,
		`payment required`,
		`Payment Required: insufficient credits for this OpenAI request`,
		`OpenRouter Payment Required: card authorization failed`,
		`Documentation mentions OpenRouter in section 402 and requires more credits for examples`,
		`insufficient credits for this request`,
	}
	for _, evidence := range nearMisses {
		t.Run(evidence, func(t *testing.T) {
			if got, matched := ClassifyCurated(evidence); matched {
				t.Fatalf("ClassifyCurated() = %+v, matched = true; want no match", got)
			}
		})
	}
}

func TestEligibleCandidate(t *testing.T) {
	tests := []struct {
		name      string
		candidate string
		want      bool
	}{
		{name: "single stop phrase", candidate: "error occurred"},
		{name: "combined stop phrases", candidate: "an error occurred command failed"},
		{name: "case and punctuation", candidate: "ERROR occurred; COMMAND FAILED!"},
		{name: "placeholders only", candidate: "<path> <num> <uuid>"},
		{name: "specific refusal", candidate: "failed: qdrant connection refused", want: true},
		{name: "specific timeout", candidate: "command failed redis timeout", want: true},
		{name: "empty", candidate: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := EligibleCandidate(tt.candidate); got != tt.want {
				t.Fatalf("EligibleCandidate(%q) = %v, want %v", tt.candidate, got, tt.want)
			}
		})
	}
}

// Two occurrences of the same failure shape with different ids, paths,
// timings, and sizes must converge on one fingerprint; different failures
// must not; evidence too short to name a failure stamps nothing.
func TestFingerprintConvergesAcrossOccurrences(t *testing.T) {
	a := Fingerprint(`FAIL lint:parity (8786ms)
/go/pkg/mod/gitlab.flexinfer.ai/libs/fi-accel/go/fiaccel@v0.0.0-20260702142837-7a20206d3425/diff.go:6:10: fatal error: fi_accel.h: No such file or directory`)
	b := Fingerprint(`FAIL lint:parity (513ms)
/go/pkg/mod/gitlab.flexinfer.ai/libs/fi-accel/go/fiaccel@v0.0.0-20260101000000-aaaaaaaaaaaa/chunker_cgo.go:9:2: fatal error: fi_accel.h: No such file or directory`)
	if a == "" || a != b {
		t.Fatalf("same shape must converge: %q vs %q", a, b)
	}
	c := Fingerprint(`fatal: could not read Username for 'https://gitlab.flexinfer.ai': terminal prompts disabled
Confirm the import path was entered correctly.`)
	if c == "" || c == a {
		t.Fatalf("different shapes must diverge: %q vs %q", a, c)
	}
	if got := Fingerprint("ok"); got != "" {
		t.Fatalf("short evidence must stamp nothing, got %q", got)
	}
	if got := Fingerprint(""); got != "" {
		t.Fatalf("empty evidence must stamp nothing, got %q", got)
	}
}

func TestSharedFingerprintNormalizesLogNoise(t *testing.T) {
	left := []string{"job 123 failed at /tmp/build/a.go after 1.2s: database connection refused"}
	right := []string{"job 987 failed at /workspace/run/b.go after 9.8s: database connection refused"}
	if !SharedFingerprint(left, right) {
		t.Fatal("variable ids, paths, and timings should normalize")
	}
	if SharedFingerprint(left, []string{"compiler reported an undefined symbol in generated package"}) {
		t.Fatal("unrelated failures must not match")
	}
	if SharedFingerprint([]string{"failed"}, []string{"failed"}) {
		t.Fatal("short noise must not match")
	}
}

// Evidence under Fingerprint's shape floor still gets a deterministic
// synthetic identity keyed on the classifier verdict, so an escalation with a
// verdict never lands unstamped. No verdict AND no tokens stays unstamped —
// the safety pin that keeps empty evidence from being invented into a cohort.
func TestSyntheticFingerprintCoversShortEvidence(t *testing.T) {
	a := SyntheticFingerprint("config", "tests failed")
	if a == "" {
		t.Fatalf("short evidence with a verdict must stamp")
	}
	if b := SyntheticFingerprint("config", "tests failed"); b != a {
		t.Fatalf("synthetic fingerprint must be deterministic: %q vs %q", a, b)
	}
	if c := SyntheticFingerprint("infra", "tests failed"); c == a {
		t.Fatalf("distinct verdicts must diverge: %q vs %q", a, c)
	}
	if d := SyntheticFingerprint("config", ""); d == "" || d == a {
		t.Fatalf("verdict alone must stamp its own cohort: %q vs %q", a, d)
	}
	if got := SyntheticFingerprint("", ""); got != "" {
		t.Fatalf("no verdict and no evidence must stamp nothing, got %q", got)
	}
}
