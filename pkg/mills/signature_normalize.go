package mills

import (
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
	signatureTokenPattern = regexp.MustCompile(`<[a-z]+>|[a-z][a-z0-9_]*|[0-9]+`)
)

// signatureMaxTokens bounds how much of one evidence text is mined. Log tails
// run to thousands of tokens while the failure that ended the run is at the
// END, so the cap keeps the TAIL: it bounds the n-gram work per sweep without
// discarding the part that carries the signature.
const signatureMaxTokens = 200

// normalizeEvidenceTokens collapses one raw failure text to the token sequence
// the miner clusters on. Two occurrences of the same failure with different
// ids, paths, sizes, and timings normalize to the same sequence; two different
// failures do not, because every word that names the failure survives intact.
func normalizeEvidenceTokens(text string) []string {
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
	if len(tokens) > signatureMaxTokens {
		tokens = tokens[len(tokens)-signatureMaxTokens:]
	}
	return tokens
}

// isSignaturePlaceholder reports whether a token is one of the collapse
// placeholders rather than a word from the failure itself.
func isSignaturePlaceholder(token string) bool {
	switch token {
	case signaturePlaceholderUUID, signaturePlaceholderPath, signaturePlaceholderDur,
		signaturePlaceholderHex, signaturePlaceholderNum:
		return true
	default:
		return false
	}
}
