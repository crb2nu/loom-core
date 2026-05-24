package pipeline

import (
	"errors"
	"io"
	"strings"
	"time"
)

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
)

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
	// Net layer eofs always wrap up to a transport class.
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return ClassTransient
	}
	s := err.Error()
	lower := strings.ToLower(s)

	// Quota first — a "429" can be embedded inside a transport-shaped
	// error so check it before the transport patterns.
	for _, needle := range []string{
		"429",
		"too many requests",
		"rate limit",
		"rate_limit",
		"quota exceeded",
	} {
		if strings.Contains(lower, needle) {
			return ClassTransientQuota
		}
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
	} {
		if strings.Contains(lower, needle) {
			return ClassTransient
		}
	}

	// Upstream 5xx — return a transient_quota so we back off without
	// burning attempts. Matches `status 500`..`status 599` and
	// `status 502: …` shapes used by the GitLab and devbox clients.
	for _, needle := range []string{
		"status 500", "status 501", "status 502", "status 503", "status 504",
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
