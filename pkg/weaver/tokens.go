package weaver

// estimateTokens returns a rough token count for a string using the
// chars/4 heuristic — the same fallback pkg/openairesponses uses when an
// exact tokenizer isn't available (fallbackCharsToTokens). It is good
// enough for the F8 token-economics ratios (compression, token-savings,
// context-waste), which only need relative magnitudes, and avoids pulling
// a tokenizer dependency into the weaver router's hot path.
func estimateTokens(s string) int {
	if len(s) == 0 {
		return 0
	}
	return (len(s) + 3) / 4
}
