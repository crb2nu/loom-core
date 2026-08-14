package pipeline

import (
	"fmt"
	"regexp"
	"strings"
)

// GitCloneFailure is the classification of a spawn/build git-clone failure. It
// carries both the ErrorClass (so budget accounting + escalation metadata agree
// with the rest of the taxonomy) and a human-actionable Message that names the
// concrete remediation the operator (or a requeue) must perform.
//
// The whole point of this classifier is to turn the opaque
//
//	image build failed: buildah build failed: container git-clone terminated exit_code=128 reason=Error
//
// — which git exit 128 always renders the same way regardless of cause — into
// the RIGHT class and the RIGHT instruction. A missing target repo, a bad ref,
// and a bad token are all TERMINAL config errors (a human must create the repo,
// fix the branch, or fix the credential — an identical retry can only fail
// identically), while a DNS/connectivity blip is TRANSIENT and must retry.
// Before this, all four escalated as ClassInfra via the generic
// "buildah build failed"/"image build failed" needles in Classify, which both
// mis-attributed the fault (cluster vs. config) AND burned the retry budget on
// the terminal cases.
type GitCloneFailure struct {
	// Class is the taxonomy bucket. Terminal cases (repo-not-found, bad-ref,
	// auth) map to ClassConfig; a network/DNS blip maps to ClassTransient.
	Class ErrorClass
	// Message is the actionable escalation reason (no trailing period; the
	// runner wraps it in a "stage X terminal git-clone error …" envelope).
	Message string
	// Project is the parsed target repo (path or bare name), best-effort;
	// "the target repo" when it could not be extracted.
	Project string
	// Ref is the parsed branch/ref for bad-ref failures, best-effort.
	Ref string
}

// gitCloneCredentialsRE matches the userinfo (`user:pass@` / `token:xxx@`)
// segment of a clone URL so a captured `fatal: repository 'https://token:…@…'`
// line never carries the spawn git token into an escalation issue. Mirrors the
// backend-side redaction (internal/devbox/backend) as a defense-in-depth second
// pass on the classifier's own message inputs.
var gitCloneCredentialsRE = regexp.MustCompile(`(?i)(https?://)[^/@\s'"]*@`)

// gitCloneURLProjectRE pulls the `<group>/<repo>` path out of a clone URL,
// stopping at the `.git` suffix. Credential userinfo (already redacted) and the
// host are excluded from the capture group.
var gitCloneURLProjectRE = regexp.MustCompile(`(?i)https?://(?:[^/@\s'"]*@)?[^/\s'"]+/([^\s'"]+?)\.git`)

// gitCloneCloningIntoRE is git's `Cloning into '<dest>'...` progress line, used
// as a fallback project name when no URL is present in the captured tail.
var gitCloneCloningIntoRE = regexp.MustCompile(`(?i)cloning into ['"]([^'"]+)['"]`)

// gitCloneRemoteRefRE / gitCloneRemoteBranchRE extract the missing ref name from
// git's two bad-ref phrasings.
var gitCloneRemoteRefRE = regexp.MustCompile(`(?i)couldn'?t find remote ref (?:refs/heads/)?(\S+)`)
var gitCloneRemoteBranchRE = regexp.MustCompile(`(?i)remote branch (\S+) not found`)

// looksLikeGitClone gates the whole classifier: only errors that carry a git
// clone signal are considered, so generic needles like "not found" below cannot
// mis-classify an unrelated stage error (e.g. the k8s "pod not found during
// reconciliation" transient) as a git-clone config failure. The captured
// backend error always includes the `git-clone` container name; the raw git
// fatal phrases cover callers that pass only the git stderr tail.
func looksLikeGitClone(lower string) bool {
	for _, s := range []string{
		"git-clone", // k8s container name in the captured error
		"git clone",
		"cloning into",
		".git'", ".git/", ".git\"",
		"fatal: repository",
		"fatal: could not read",
		"fatal: unable to access",
		"couldn't find remote ref",
		"couldnt find remote ref",
		"remote branch",
		"the project you were looking for", // GitLab not-found/no-permission body
	} {
		if strings.Contains(lower, s) {
			return true
		}
	}
	return false
}

// authNeedles: credential/permission failures. GitLab returns 401/403 (and the
// "HTTP Basic: Access denied" body) when the token is wrong or lacks scope; git
// prompts for a username when no credential is present.
var gitCloneAuthNeedles = []string{
	"authentication failed",
	"could not read username",
	"could not read password",
	"invalid username or password",
	"http basic: access denied",
	"access denied",
	"returned error: 401",
	"returned error: 403",
	"error: 401",
	"error: 403",
	"401 unauthorized",
	"403 forbidden",
	"permission denied",
}

// badRefNeedles: the requested branch/ref does not exist on the remote. Checked
// before the not-found needles so "Remote branch X not found" is a bad-ref, not
// a missing-repo.
var gitCloneBadRefNeedles = []string{
	"couldn't find remote ref",
	"couldnt find remote ref",
	"remote branch", // paired with "not found" — see classify switch
	"did not match any file(s) known to git",
}

// notFoundNeedles: the target repository does not exist. Safe within the
// looksLikeGitClone guard.
var gitCloneNotFoundNeedles = []string{
	"repository not found",
	"returned error: 404",
	"error: 404",
	"project not found",
	"could not be found", // GitLab's conflated not-found/no-permission phrasing
	"the project you were looking for",
	"not found", // git's `fatal: repository '…' not found` (guarded)
}

// networkNeedles: DNS/connectivity blips. These are the only TRANSIENT class —
// a fresh spawn after the blip clears typically clones fine.
var gitCloneNetworkNeedles = []string{
	"could not resolve host",
	"couldn't resolve host",
	"couldnt resolve host",
	"temporary failure in name resolution",
	"connection timed out",
	"operation timed out",
	"connection refused",
	"network is unreachable",
	"no route to host",
	"failed to connect",
	"timed out",
}

// ClassifyGitCloneError inspects a spawn/build error string and, when it
// recognizes a git-clone failure, returns its class + actionable message.
// found=false means the error is not a git-clone failure (the caller falls back
// to the generic Classify path). Deterministic and side-effect free so both the
// runner's escalation branch and Classify can call it.
func ClassifyGitCloneError(errText string) (GitCloneFailure, bool) {
	lower := strings.ToLower(errText)
	if !looksLikeGitClone(lower) {
		return GitCloneFailure{}, false
	}

	project := parseGitCloneProject(errText)
	if project == "" {
		project = "the target repo"
	}

	switch {
	// Auth first: a 403/401 is unambiguous, and GitLab's "could not be found"
	// body (checked under not-found) can also mean no-permission — an explicit
	// auth signal should win.
	case containsAny(lower, gitCloneAuthNeedles):
		return GitCloneFailure{
			Class:   ClassConfig,
			Project: project,
			Message: fmt.Sprintf("git auth failed cloning %s — the spawn git token is missing, wrong, or lacks read scope on the repo; fix the spawn git secret (SPAWN_GIT_SECRET), then requeue", project),
		}, true

	// Bad ref before not-found so "Remote branch X not found" is a ref problem,
	// not a missing repo.
	case isBadRef(lower):
		ref := parseGitCloneRef(errText)
		refLabel := ref
		if refLabel == "" {
			refLabel = "the requested branch"
		}
		return GitCloneFailure{
			Class:   ClassConfig,
			Project: project,
			Ref:     ref,
			Message: fmt.Sprintf("target branch %s not found in %s — create/push the branch or fix the plan's base ref, then requeue", refLabel, project),
		}, true

	case containsAny(lower, gitCloneNotFoundNeedles):
		return GitCloneFailure{
			Class:   ClassConfig,
			Project: project,
			Message: fmt.Sprintf("target repo %s does not exist — create the GitLab repo (or enable plan→repo bootstrap: POST /api/mills/projects/bootstrap) then requeue", project),
		}, true

	case containsAny(lower, gitCloneNetworkNeedles):
		return GitCloneFailure{
			Class:   ClassTransient,
			Project: project,
			Message: fmt.Sprintf("transient git-clone network error cloning %s (DNS/connectivity) — retrying", project),
		}, true

	// Our git-clone init container exited 128 but the captured tail held no
	// recognizable git phrase. Fail closed to a SAFE generic terminal-config
	// (git exit 128 is a fatal config-shaped error far more often than a
	// retryable one, and mis-classing it as infra was the original bug) with a
	// note that clone-error capture should be surfacing the real message.
	case isGitCloneExit128(lower):
		return GitCloneFailure{
			Class:   ClassConfig,
			Project: project,
			Message: fmt.Sprintf("git clone failed (exit 128) for %s — no git message was captured; enable clone-error capture, then verify the repo exists, the branch/base ref is valid, and the spawn git token has access, and requeue", project),
		}, true

	default:
		return GitCloneFailure{}, false
	}
}

// isBadRef reports the bad-ref phrasings. "remote branch" alone is too broad, so
// it must be paired with "not found".
func isBadRef(lower string) bool {
	for _, n := range gitCloneBadRefNeedles {
		if n == "remote branch" {
			if strings.Contains(lower, "remote branch") && strings.Contains(lower, "not found") {
				return true
			}
			continue
		}
		if strings.Contains(lower, n) {
			return true
		}
	}
	return false
}

// isGitCloneExit128 anchors the generic fallback to OUR init container: the
// captured error names the `git-clone` container and reports exit 128. Without
// this anchor a bare "git clone" mention with an unrelated exit code would not
// reach the fallback.
func isGitCloneExit128(lower string) bool {
	if !strings.Contains(lower, "git-clone") {
		return false
	}
	return strings.Contains(lower, "exit_code=128") || strings.Contains(lower, "exit code 128")
}

// parseGitCloneProject extracts a `<group>/<repo>` (or bare repo) name from the
// captured error, preferring the clone URL and falling back to git's
// `Cloning into '<dest>'` line. Any credential userinfo is redacted defensively.
func parseGitCloneProject(errText string) string {
	redacted := gitCloneCredentialsRE.ReplaceAllString(errText, "$1***@")
	if m := gitCloneURLProjectRE.FindStringSubmatch(redacted); len(m) == 2 {
		return strings.Trim(m[1], "/")
	}
	if m := gitCloneCloningIntoRE.FindStringSubmatch(redacted); len(m) == 2 {
		return strings.Trim(m[1], "/")
	}
	return ""
}

// parseGitCloneRef extracts the missing ref name from git's two bad-ref forms.
func parseGitCloneRef(errText string) string {
	if m := gitCloneRemoteRefRE.FindStringSubmatch(errText); len(m) == 2 {
		return strings.TrimRight(m[1], ".,'\"")
	}
	if m := gitCloneRemoteBranchRE.FindStringSubmatch(errText); len(m) == 2 {
		return strings.TrimRight(m[1], ".,'\"")
	}
	return ""
}

func containsAny(lower string, needles []string) bool {
	for _, n := range needles {
		if strings.Contains(lower, n) {
			return true
		}
	}
	return false
}
