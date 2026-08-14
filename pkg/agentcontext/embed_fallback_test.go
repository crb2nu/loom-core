package agentcontext

import (
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestPrepareFallbackBackfillInput(t *testing.T) {
	tests := []struct {
		name          string
		input         string
		want          string
		wantTruncated bool
		wantEmpty     bool
	}{
		{name: "short unchanged", input: "useful context", want: "useful context"},
		{name: "whitespace only", input: " \n\t ", wantEmpty: true},
		{
			name:          "overlong utf8",
			input:         strings.Repeat("界", maxFallbackBackfillInputBytes),
			want:          strings.Repeat("界", maxFallbackBackfillInputBytes/len("界")),
			wantTruncated: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, truncated, empty := prepareFallbackBackfillInput(tt.input)
			if got != tt.want || truncated != tt.wantTruncated || empty != tt.wantEmpty {
				t.Fatalf("prepareFallbackBackfillInput() = (%q, %v, %v), want (%q, %v, %v)", got, truncated, empty, tt.want, tt.wantTruncated, tt.wantEmpty)
			}
			if !utf8.ValidString(got) {
				t.Fatal("prepared input is not valid UTF-8")
			}
		})
	}
}

func TestKeywordTokens(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"a b c", nil}, // all 1-char, dropped
		{"Hub WS transport storm", []string{"hub", "ws", "transport", "storm"}},
		{"morph morph Morph", []string{"morph"}},                  // dedup, case-fold
		{"agent_context-v1!", []string{"agent", "context", "v1"}}, // split on non-alnum
	}
	for _, c := range cases {
		got := keywordTokens(c.in)
		if len(got) == 0 && len(c.want) == 0 {
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("keywordTokens(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestKeywordScore(t *testing.T) {
	tokens := keywordTokens("hub transport storm")

	full := keywordScore(tokens, "the hub transport storm root cause")
	if full != 1.0 {
		t.Errorf("full match score = %v, want 1.0", full)
	}

	partial := keywordScore(tokens, "transport only mentioned here")
	if partial <= 0 || partial >= 1 {
		t.Errorf("partial score = %v, want in (0,1)", partial)
	}

	none := keywordScore(tokens, "completely unrelated text")
	if none != 0 {
		t.Errorf("no-overlap score = %v, want 0", none)
	}

	// Case-insensitive on the haystack side.
	if keywordScore(tokens, "HUB TRANSPORT STORM") != 1.0 {
		t.Error("expected case-insensitive full match")
	}

	// Empty token set never matches.
	if keywordScore(nil, "anything") != 0 {
		t.Error("empty tokens should score 0")
	}
}

func TestKeywordScore_RanksMoreOverlapHigher(t *testing.T) {
	tokens := keywordTokens("circuit breaker embedding timeout")
	strong := keywordScore(tokens, "circuit breaker for the embedding timeout path")
	weak := keywordScore(tokens, "embedding only")
	if strong <= weak {
		t.Errorf("expected strong(%v) > weak(%v)", strong, weak)
	}
}
