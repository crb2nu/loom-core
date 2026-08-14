package journalengine

import "math"

// Token accounting bounds. English prose sits near 4 chars/token; an observation
// outside this band means the caller measured the wrong thing, not that the
// register drifted.
const (
	minCharsPerToken     = 1.5
	maxCharsPerToken     = 8.0
	defaultCharsPerToken = 4.0
	emaAlpha             = 0.3
)

// TokenLedger estimates token counts from characters and corrects itself.
//
// Exact tokenization needs the model's tokenizer, which this package cannot
// carry and does not need: what a long-context memory engine needs is a budget
// estimate good to a few percent, and every OpenAI-compatible completion hands
// back an exact prompt_tokens that can be folded back in. Calibrate closes that
// loop, so the estimate converges on whatever the caller's traffic actually
// looks like — dense code, prose, and structured logs all sit at different
// ratios, and a fixed chars/4 is wrong for at least two of them.
//
// A TokenLedger is not safe for concurrent use on its own; the Journal that owns
// one serializes access to it.
type TokenLedger struct {
	charsPerToken float64
	observations  int
}

// NewTokenLedger returns a ledger starting from charsPerToken, clamped into the
// sane band. Pass 0 for the default prior of 4.0.
func NewTokenLedger(charsPerToken float64) *TokenLedger {
	if charsPerToken == 0 {
		charsPerToken = defaultCharsPerToken
	}
	return &TokenLedger{charsPerToken: clampCPT(charsPerToken)}
}

// Estimate returns the estimated token count for s. It never returns 0, so a
// caller dividing by it cannot trip over an empty string.
func (l *TokenLedger) Estimate(s string) int {
	return int(float64(len(s))/l.charsPerToken) + 1
}

// Calibrate folds an observed (chars, promptTokens) pair into the ratio via an
// exponential moving average.
//
// Non-positive or out-of-band observations are ignored rather than clamped: a
// ratio of 100 chars/token means the caller measured the wrong thing, and
// averaging that in would poison an otherwise good estimate.
func (l *TokenLedger) Calibrate(chars, promptTokens int) {
	if chars <= 0 || promptTokens <= 0 {
		return
	}
	observed := float64(chars) / float64(promptTokens)
	if observed < minCharsPerToken || observed > maxCharsPerToken {
		return
	}
	if math.IsNaN(observed) || math.IsInf(observed, 0) {
		return
	}
	l.charsPerToken += emaAlpha * (observed - l.charsPerToken)
	l.observations++
}

// CharsPerToken reports the current ratio.
func (l *TokenLedger) CharsPerToken() float64 { return l.charsPerToken }

// Observations reports how many calibration samples have been accepted. A
// ledger with zero observations is still running on its prior.
func (l *TokenLedger) Observations() int { return l.observations }

func clampCPT(v float64) float64 {
	if math.IsNaN(v) || v < minCharsPerToken {
		return minCharsPerToken
	}
	if v > maxCharsPerToken {
		return maxCharsPerToken
	}
	return v
}
