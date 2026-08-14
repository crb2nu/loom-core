package journalengine

import "fmt"

// prefixContextWindow is how many characters of surrounding text a divergence
// report shows on each side. Enough to recognize the offending token; short
// enough to read in a test failure.
const prefixContextWindow = 60

// CheckPrefixExtension reports whether later is a strict prefix-extension of
// earlier, i.e. later starts with earlier and is longer. A nil error means the
// prefix cache would hit; a non-nil error names the divergence point.
//
// This is the assertion to put in your own tests. This package can prove
// Journal.Render is append-only, but only your test can prove that *your* prompt
// assembly did not put a timestamp, a token count, a retrieved memory, or a
// "turn 14 of 30" above the now-block boundary. Compare the full cacheable
// prefix — system prompt plus journal render — across successive turns:
//
//	for i := 1; i < len(prompts); i++ {
//		if err := journalengine.CheckPrefixExtension(prompts[i-1], prompts[i]); err != nil {
//			t.Fatalf("prefix cache contract broken: %v", err)
//		}
//	}
//
// The failure this catches is silent in production: the hit rate drops to
// roughly zero and every turn pays a full cold prefill of the entire history.
// Nothing errors, it just gets slow and expensive.
func CheckPrefixExtension(earlier, later string) error {
	if earlier == later {
		return fmt.Errorf("render did not grow (both %d bytes)", len(earlier))
	}
	if len(later) < len(earlier) {
		return fmt.Errorf(
			"render shrank from %d to %d bytes; only a consolidation may rewrite the prefix",
			len(earlier), len(later),
		)
	}
	if idx := FirstDivergence(earlier, later); idx >= 0 {
		return fmt.Errorf(
			"prefix diverges at byte %d:\n  before: ...%q\n  after:  ...%q\n"+
				"the stable prefix must not change between consolidations; "+
				"move whatever changed into the ephemeral tail",
			idx,
			window(earlier, idx),
			window(later, idx),
		)
	}
	return nil
}

// FirstDivergence returns the index of the first byte at which earlier and later
// differ, or -1 when later extends earlier without modifying it.
func FirstDivergence(earlier, later string) int {
	limit := len(earlier)
	if len(later) < limit {
		limit = len(later)
	}
	for i := 0; i < limit; i++ {
		if earlier[i] != later[i] {
			return i
		}
	}
	if len(later) >= len(earlier) {
		return -1
	}
	return limit
}

func window(s string, idx int) string {
	start := idx - prefixContextWindow
	if start < 0 {
		start = 0
	}
	end := idx + prefixContextWindow
	if end > len(s) {
		end = len(s)
	}
	return s[start:end]
}
