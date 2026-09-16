package sigfp

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
)

// Placeholder tokens replace the parts of a failure message that differ between
// two occurrences of the SAME failure: identifiers, sizes, timings, and file
// locations. They are written with angle brackets so a mined phrase reads as an
// obvious template in the operator UI rather than as a literal log line.
const (
	signaturePlaceholderUUID = "<uuid>"
	signaturePlaceholderPath = "<path>"
	signaturePlaceholderDur  = "<dur>"
	signaturePlaceholderHex  = "<hex>"
	signaturePlaceholderNum  = "<num>"
)

// The collapse order is load-bearing and runs most-specific first: a UUID is
// also a run of hex and digits, a path may contain both, and a duration is a
// number with a unit suffix. Reversing any pair would shred the more specific
// form into fragments of the more general one.
var (
	signatureUUIDPattern = regexp.MustCompile(`\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)
	// A path is any run containing a slash. Deliberately greedy: a build path,
	// a URL's host+path, and a repo-relative file all collapse to one token,
	// because none of them distinguishes one occurrence of a failure shape
	// from another.
	signaturePathPattern = regexp.MustCompile(`[a-z0-9._~+-]*/[a-z0-9._~+/-]*`)
	// Go-style durations, including the bare-unit forms log lines use
	// ("timed out after 30s", "took 1.5h").
	signatureDurationPattern = regexp.MustCompile(`\b\d+(?:\.\d+)?(?:ns|us|ms|s|m|h)\b`)
	// Commit ids, container ids, and other hex blobs. Seven characters is the
	// short-SHA floor; the digit requirement is enforced in code (RE2 has no
	// lookahead) so all-letter words like "defaced" stay words.
	signatureHexPattern    = regexp.MustCompile(`\b[0-9a-f]{7,}\b`)
	signatureNumberPattern = regexp.MustCompile(`\b\d+(?:\.\d+)?\b`)
	// Tokenizer: a placeholder, an identifier-ish word, or a leftover digit
	// run. The placeholder alternative comes first so "<uuid>" survives as one
	// token instead of being split into "uuid".
	signatureTokenPattern    = regexp.MustCompile(`<[a-z]+>|[a-z][a-z0-9_]*|[0-9]+`)
	openRouterHTTP402Pattern = regexp.MustCompile(`(?:\bhttp(?:/\d(?:\.\d)?)?\s+402\b|\bstatus(?:[ _-]?code)?\s*["']?\s*[:=]?\s*402\b|\bcode\s*["']?\s*[:=]\s*402\b)`)
	openRouterCreditsPattern = regexp.MustCompile(`\b(?:insufficient\s+credits?|requires?\s+more\s+credits?|credits?\s+exhausted)\b`)
	// "OpenRouter 402" with no HTTP/status framing, and the reason phrase
	// "Payment Required" (HTTP 402's canonical text), are weaker status
	// evidence: each only counts alongside a credits phrase or a real 402
	// code, so "OpenRouter HTTP 401: payment required" stays unmatched.
	openRouterBare402Pattern         = regexp.MustCompile(`\bopenrouter\s+402\b`)
	openRouterPaymentRequiredPattern = regexp.MustCompile(`\bpayment\s+required\b`)

	// These tokens describe only the fact that an operation failed, not the
	// failure itself. Candidate admission rejects a phrase only when every
	// normalized token is generic (or a variable placeholder).
	signatureStopTokens = map[string]struct{}{
		"an": {}, "command": {}, "error": {}, "failed": {}, "failure": {},
		"occurred": {}, "the": {}, "was": {},
	}
)

const (
	OpenRouter402SignatureID   = "openrouter_402_credit_exhausted"
	ExternalDependencyIncident = "external_dependency_incident"
)

// Classification is the policy metadata emitted by a curated signature.
type Classification struct {
	Signature  string `json:"signature"`
	Class      string `json:"class"`
	External   string `json:"external"`
	Capability string `json:"capability"`
	Retryable  bool   `json:"retryable"`
	FreeRetry  bool   `json:"free_retry"`
}

// ClassifyCurated matches promoted signatures. OpenRouter credit exhaustion
// requires provider attribution, HTTP 402 evidence, and narrow credit wording.
func ClassifyCurated(evidence string) (Classification, bool) {
	normalized := strings.ToLower(evidence)
	if !strings.Contains(normalized, "openrouter") {
		return Classification{}, false
	}
	status402 := openRouterHTTP402Pattern.MatchString(normalized) || openRouterBare402Pattern.MatchString(normalized)
	paymentRequired := openRouterPaymentRequiredPattern.MatchString(normalized)
	credits := openRouterCreditsPattern.MatchString(normalized)
	matched := (status402 && (credits || paymentRequired)) || (paymentRequired && credits)
	if !matched {
		return Classification{}, false
	}
	return Classification{
		Signature: OpenRouter402SignatureID, Class: ExternalDependencyIncident,
		External: "openrouter", Capability: "llm_provider",
		Retryable: false, FreeRetry: false,
	}, true
}

// MaxTokens bounds how much of one evidence text is mined. Log tails
// run to thousands of tokens while the failure that ended the run is at the
// END, so the cap keeps the TAIL: it bounds the n-gram work per sweep without
// discarding the part that carries the signature.
const MaxTokens = 200

// normalizeEvidenceTokens collapses one raw failure text to the token sequence
// the miner clusters on. Two occurrences of the same failure with different
// ids, paths, sizes, and timings normalize to the same sequence; two different
// failures do not, because every word that names the failure survives intact.
func NormalizeEvidenceTokens(text string) []string {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	lower := strings.ToLower(text)
	lower = signatureUUIDPattern.ReplaceAllString(lower, " "+signaturePlaceholderUUID+" ")
	lower = signaturePathPattern.ReplaceAllString(lower, " "+signaturePlaceholderPath+" ")
	lower = signatureDurationPattern.ReplaceAllString(lower, " "+signaturePlaceholderDur+" ")
	lower = signatureHexPattern.ReplaceAllStringFunc(lower, func(match string) string {
		if !strings.ContainsAny(match, "0123456789") {
			return match // an all-letter word that happens to be hex-shaped
		}
		return " " + signaturePlaceholderHex + " "
	})
	lower = signatureNumberPattern.ReplaceAllString(lower, " "+signaturePlaceholderNum+" ")

	tokens := signatureTokenPattern.FindAllString(lower, -1)
	if len(tokens) > MaxTokens {
		tokens = tokens[len(tokens)-MaxTokens:]
	}
	return tokens
}

// IsPlaceholder reports whether a token is one of the collapse placeholders.
func IsPlaceholder(token string) bool {
	switch token {
	case signaturePlaceholderUUID, signaturePlaceholderPath, signaturePlaceholderDur,
		signaturePlaceholderHex, signaturePlaceholderNum:
		return true
	default:
		return false
	}
}

// EligibleCandidate reports whether a mined signature candidate contains a
// distinguishing failure term. Input may be raw or already normalized; using
// the normalizer here makes case, punctuation, and whitespace variants obey
// the same rule as mining evidence.
func EligibleCandidate(candidate string) bool {
	tokens := NormalizeEvidenceTokens(candidate)
	for _, token := range tokens {
		if IsPlaceholder(token) {
			continue
		}
		if _, generic := signatureStopTokens[token]; !generic {
			return true
		}
	}
	return false
}

// Fingerprint is the durable identity of a failure SHAPE: the sha256 (first
// 16 hex chars) of the normalized token tail. Two escalations with different
// ids, paths, sizes, and timings converge on one fingerprint; two different
// failures do not. Stamped onto escalated runs at classification time and
// joined by the shepherd's environment-delta predicate — internal consistency
// between stamps is the contract, so any change to the normalization or the
// tail bounds silently splits cohorts and must be treated as a migration.
func Fingerprint(text string) string {
	tokens := NormalizeEvidenceTokens(text)
	if len(tokens) < fingerprintMinTokens {
		return ""
	}
	if len(tokens) > fingerprintTailTokens {
		tokens = tokens[len(tokens)-fingerprintTailTokens:]
	}
	sum := sha256.Sum256([]byte(strings.Join(tokens, " ")))
	return hex.EncodeToString(sum[:])[:16]
}

// SharedFingerprint reports whether two evidence sets contain the same
// normalized failure shape. Empty/short evidence is deliberately ignored so
// generic GitLab failure reasons cannot create false baseline matches.
func SharedFingerprint(left, right []string) bool {
	seen := make(map[string]struct{}, len(left))
	for _, evidence := range left {
		if fp := Fingerprint(evidence); fp != "" {
			seen[fp] = struct{}{}
		}
	}
	for _, evidence := range right {
		if fp := Fingerprint(evidence); fp != "" {
			if _, ok := seen[fp]; ok {
				return true
			}
		}
	}
	return false
}

// fingerprintTailTokens keeps the failure-carrying TAIL of the normalized
// text, mirroring MaxTokens' rationale at a tighter bound so one
// unrelated leading line cannot split a cohort.
const fingerprintTailTokens = 64

// fingerprintMinTokens rejects texts too short to name a failure; an empty
// fingerprint means "unstamped" everywhere downstream.
const fingerprintMinTokens = 4

// SyntheticFingerprint is the fallback identity for evidence below
// Fingerprint's shape floor (bl-honest-verdicts-s1). It keys on the
// classifier's verdict plus whatever normalized tokens the evidence does
// carry, so a CLASSIFIED escalation never lands unstamped — the 2026-08-20
// storm left 19 escalations with empty signatures the miner and the
// shepherd's cohort join could not see. The "synthetic" seed prefix keeps
// these coarse identities from ever colliding with a real Fingerprint of
// the same words. No verdict and no tokens stamps nothing: empty evidence
// must not be invented into a cohort.
func SyntheticFingerprint(class, text string) string {
	tokens := NormalizeEvidenceTokens(text)
	if len(tokens) > fingerprintTailTokens {
		tokens = tokens[len(tokens)-fingerprintTailTokens:]
	}
	class = strings.TrimSpace(class)
	if class == "" && len(tokens) == 0 {
		return ""
	}
	seed := "synthetic " + class + " " + strings.Join(tokens, " ")
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:])[:16]
}
