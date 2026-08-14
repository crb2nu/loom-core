// Package textsim is the one title-similarity implementation shared by the
// mill-staff lanes: the council's proposal dedup (hard, gray-band, and
// plan-lane), the overseer groomer's duplicate scan, and the overseer policy
// validation bounds. One tokenizer, one stopword set, one gray-band floor —
// previously these were mirrored across three files with "keep in lockstep"
// comments because pkg/mills cannot import pkg/mills/council. The package is
// a dependency-free leaf so every staff lane can import it.
package textsim

import "strings"

// GrayBandFloor is the lower Jaccard bound for gray-band dedup: pairs
// scoring in [GrayBandFloor, threshold) are too dissimilar for a
// deterministic dedup but similar enough to be the same theme reworded —
// the council routes them to recency-gated blocking, the groomer to an LLM
// verdict. Chosen from the live miss: the !970/!978 title pair scores
// exactly 0.6 with the default stopword set.
const GrayBandFloor = 0.55

// TitleJaccard reports the Jaccard similarity of two backlog-item titles
// using the same normalization (lowercase, alphanumeric tokens, stopword
// drop) everywhere — the council's dedup and the overseer groomer's
// duplicate scan stay behaviorally identical by construction.
func TitleJaccard(a, b string) float64 {
	ta := NormalizeTitleTokens(a)
	if len(ta) == 0 {
		return 0
	}
	return Jaccard(ta, NormalizeTitleTokens(b))
}

// sliceTitleSeparator is what the plan-slice emitter joins a plan title and a
// slice name with, and therefore what rides into the merge request the slice
// lands as ("Wire config-gated OTel trace export into the daemon — daemon-otel-export").
const sliceTitleSeparator = " — "

// titlePrefixMaxLen bounds how far into a title NormalizeWorkTitle looks for
// the ':' of a conventional-commit prefix. Long enough for
// "refactor(mills/council)!:", short enough that a sentence with a mid-title
// colon ("Fail closed: the classifier must not guess") keeps its clause.
const titlePrefixMaxLen = 40

// itemSlugPrefixes are the id decorations mills stamps onto the work it
// derives from a council proposal: the plan-slice emitter's backlog id
// ("psl-plan-council-<slugified-proposal-title>-<N>") and the pipeline run id
// built from it ("PIPE-psl-…"). Either can ride into a title; as tokens they
// are pure noise against the proposal title they were slugified FROM.
var itemSlugPrefixes = []string{"pipe-psl-", "psl-"}

// NormalizeWorkTitle strips the decorations mills' own machinery adds between
// a council proposal and the merge request that lands it, so the two compare
// as the work they describe rather than as differently dressed strings. Three
// layers come off, outermost first:
//
//   - bracketed and draft leads: "[scope-escalated] ", "Draft: ", "WIP: ";
//   - a conventional-commit prefix: "feat(mills): ", "docs: ";
//   - the plan-slice decoration: a trailing " — <slice-slug>" and any bare
//     psl-/PIPE-psl- item-id token.
//
// Without it a merged MR titled "feat(mills): council grounds proposals —
// merged-work-grounding" scores well under GrayBandFloor against the very
// proposal it shipped, and grounding never fires on the case it exists for.
func NormalizeWorkTitle(title string) string {
	s := strings.TrimSpace(title)
	for {
		next := strings.TrimSpace(stripTitleLead(s))
		if next == s || next == "" {
			break
		}
		s = next
	}
	s = stripSliceDecoration(s)
	return strings.TrimSpace(stripItemSlugTokens(s))
}

// NormalizeWorkTitleTokens is NormalizeTitleTokens over a NormalizeWorkTitle'd
// title — the token form the merged-work comparators feed to Jaccard.
func NormalizeWorkTitleTokens(title string) []string {
	return NormalizeTitleTokens(NormalizeWorkTitle(title))
}

// WorkTitleJaccard is TitleJaccard with both sides run through
// NormalizeWorkTitle first. Same similarity math, decoration-blind inputs.
func WorkTitleJaccard(a, b string) float64 {
	ta := NormalizeWorkTitleTokens(a)
	if len(ta) == 0 {
		return 0
	}
	return Jaccard(ta, NormalizeWorkTitleTokens(b))
}

// stripTitleLead removes ONE leading decoration — a bracketed tag or a
// "type(scope)!:" prefix — and returns title unchanged when there is none.
// NormalizeWorkTitle loops it so "Draft: feat(mills): x" unwraps fully.
func stripTitleLead(title string) string {
	if rest, ok := strings.CutPrefix(title, "["); ok {
		if end := strings.IndexByte(rest, ']'); end >= 0 {
			return rest[end+1:]
		}
		return title
	}
	colon := strings.IndexByte(title, ':')
	if colon <= 0 || colon > titlePrefixMaxLen {
		return title
	}
	head := strings.TrimSuffix(title[:colon], "!")
	if open := strings.IndexByte(head, '('); open >= 0 {
		if !strings.HasSuffix(head, ")") {
			return title
		}
		scope := head[open+1 : len(head)-1]
		if scope == "" || strings.ContainsAny(scope, " \t") {
			return title
		}
		head = head[:open]
	}
	if head == "" || !isAlphaWord(head) {
		return title
	}
	return title[colon+1:]
}

// stripSliceDecoration drops a trailing " — <slug>" segment. Only a slug tail
// is decoration: a tail carrying whitespace is a real clause the author wrote,
// and dropping it would discard the tokens that distinguish two proposals.
func stripSliceDecoration(title string) string {
	idx := strings.LastIndex(title, sliceTitleSeparator)
	if idx <= 0 {
		return title
	}
	head := strings.TrimSpace(title[:idx])
	tail := strings.TrimSpace(title[idx+len(sliceTitleSeparator):])
	if head == "" || tail == "" || strings.ContainsAny(tail, " \t") {
		return title
	}
	return head
}

// stripItemSlugTokens drops whitespace-delimited tokens that are mills item
// ids rather than words. Surrounding punctuation is ignored so "(psl-…-1)"
// is recognised too.
func stripItemSlugTokens(title string) string {
	fields := strings.Fields(title)
	kept := make([]string, 0, len(fields))
	for _, f := range fields {
		bare := strings.ToLower(strings.Trim(f, "()[]{}.,;:"))
		slug := false
		for _, prefix := range itemSlugPrefixes {
			if strings.HasPrefix(bare, prefix) {
				slug = true
				break
			}
		}
		if !slug {
			kept = append(kept, f)
		}
	}
	return strings.Join(kept, " ")
}

// isAlphaWord reports whether s is a non-empty run of ASCII letters — the
// shape a conventional-commit type has ("feat", "docs", "refactor").
func isAlphaWord(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') {
			return false
		}
	}
	return true
}

// stopwords are the English filler tokens we drop before computing
// Jaccard so that "Add the new HUD panel" and "Add new HUD panel"
// produce identical token sets. Kept tiny by design — bigger lists
// risk hiding genuine differences (e.g. "before" vs "after").
var stopwords = map[string]struct{}{
	"a": {}, "an": {}, "the": {},
	"to": {}, "of": {}, "for": {}, "and": {}, "or": {},
	"with": {}, "in": {}, "on": {}, "at": {}, "by": {},
	"is": {}, "are": {},
}

// NormalizeTitleTokens lowercases title and splits on non-alphanumeric
// characters, then drops stopwords and tokens shorter than 2 chars.
// Returns a deduplicated slice in original token order so the function
// is also useful for displays/tests that want stable output.
func NormalizeTitleTokens(title string) []string {
	if title == "" {
		return nil
	}
	lower := strings.ToLower(title)
	out := make([]string, 0, 8)
	seen := make(map[string]struct{}, 8)
	start := -1
	for i, r := range lower {
		alnum := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if alnum {
			if start < 0 {
				start = i
			}
			continue
		}
		if start >= 0 {
			tok := lower[start:i]
			start = -1
			if len(tok) < 2 {
				continue
			}
			if _, skip := stopwords[tok]; skip {
				continue
			}
			if _, dup := seen[tok]; dup {
				continue
			}
			seen[tok] = struct{}{}
			out = append(out, tok)
		}
	}
	if start >= 0 {
		tok := lower[start:]
		if len(tok) >= 2 {
			if _, skip := stopwords[tok]; !skip {
				if _, dup := seen[tok]; !dup {
					out = append(out, tok)
				}
			}
		}
	}
	return out
}

// Jaccard returns |A ∩ B| / |A ∪ B|. Both empty → 0 (we only ever
// compare non-empty token slices, but defensive zero is the right
// answer either way: empty titles aren't "duplicates" of each other).
func Jaccard(a, b []string) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	setA := make(map[string]struct{}, len(a))
	for _, t := range a {
		setA[t] = struct{}{}
	}
	inter := 0
	setB := make(map[string]struct{}, len(b))
	for _, t := range b {
		if _, dup := setB[t]; dup {
			continue
		}
		setB[t] = struct{}{}
		if _, ok := setA[t]; ok {
			inter++
		}
	}
	union := len(setA) + len(setB) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}
