package journalengine

import (
	"math"
	"testing"
)

func TestNewTokenLedgerClampsItsPrior(t *testing.T) {
	tests := []struct {
		name string
		in   float64
		want float64
	}{
		{name: "zero takes the default prior", in: 0, want: defaultCharsPerToken},
		{name: "in-band value is kept", in: 3.1, want: 3.1},
		{name: "absurdly high is clamped down", in: 99, want: maxCharsPerToken},
		{name: "absurdly low is clamped up", in: 0.1, want: minCharsPerToken},
		{name: "negative is clamped up", in: -5, want: minCharsPerToken},
		{name: "NaN is clamped up", in: math.NaN(), want: minCharsPerToken},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NewTokenLedger(tt.in).CharsPerToken(); got != tt.want {
				t.Errorf("CharsPerToken() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestEstimateNeverReturnsZero(t *testing.T) {
	l := NewTokenLedger(4.0)
	if got := l.Estimate(""); got != 1 {
		t.Errorf("Estimate(%q) = %d, want 1", "", got)
	}
	if got := l.Estimate("abcd"); got != 2 {
		t.Errorf("Estimate(4 chars at 4.0) = %d, want 2", got)
	}
	if got := l.Estimate(string(make([]byte, 400))); got != 101 {
		t.Errorf("Estimate(400 chars at 4.0) = %d, want 101", got)
	}
}

func TestCalibrateConvergesOnObservations(t *testing.T) {
	l := NewTokenLedger(4.0)
	// An observed ratio of 2.0 chars/token pulls the estimate down.
	for i := 0; i < 20; i++ {
		l.Calibrate(2000, 1000)
	}
	if got := l.CharsPerToken(); math.Abs(got-2.0) > 0.05 {
		t.Errorf("CharsPerToken() = %v, want ~2.0", got)
	}
	if got := l.Observations(); got != 20 {
		t.Errorf("Observations() = %d, want 20", got)
	}
}

func TestCalibrateRejectsBogusObservations(t *testing.T) {
	// An out-of-band ratio means the caller measured the wrong thing. Averaging
	// it in would poison an otherwise good estimate, so it is dropped, not
	// clamped.
	tests := []struct {
		name         string
		chars        int
		promptTokens int
	}{
		{name: "100 chars per token is nonsense", chars: 100, promptTokens: 1},
		{name: "1 char per token is nonsense", chars: 50, promptTokens: 50},
		{name: "zero chars", chars: 0, promptTokens: 50},
		{name: "zero tokens", chars: 50, promptTokens: 0},
		{name: "negative chars", chars: -100, promptTokens: 25},
		{name: "negative tokens", chars: 100, promptTokens: -25},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l := NewTokenLedger(4.0)
			l.Calibrate(tt.chars, tt.promptTokens)
			if got := l.CharsPerToken(); got != 4.0 {
				t.Errorf("CharsPerToken() = %v, want 4.0 (observation should be ignored)", got)
			}
			if got := l.Observations(); got != 0 {
				t.Errorf("Observations() = %d, want 0", got)
			}
		})
	}
}

func TestNeedsConsolidationUsesBudgetThreshold(t *testing.T) {
	j := New("agent", NewTokenLedger(4.0))
	j.RecordTurn(1, "s", nil, string(make([]byte, 4000))) // ~1000 tokens

	if !j.NeedsConsolidation(1000, 0.9) {
		t.Errorf("NeedsConsolidation(1000, 0.9) = false, want true (estimate %d)", j.TokenEstimate())
	}
	if j.NeedsConsolidation(100000, 0.9) {
		t.Error("NeedsConsolidation(100000, 0.9) = true, want false")
	}
}

func TestLedgerIsSharedWithItsJournal(t *testing.T) {
	// The journal exposes its ledger so a caller can fold the completion's
	// reported prompt_tokens back in after each turn.
	j := New("agent", nil)
	j.Ledger().Calibrate(3000, 1000)
	if got := j.Ledger().Observations(); got != 1 {
		t.Errorf("Observations() = %d, want 1", got)
	}
	if got := j.Ledger().CharsPerToken(); got >= 4.0 {
		t.Errorf("CharsPerToken() = %v, want < 4.0 after a 3.0 observation", got)
	}
}
