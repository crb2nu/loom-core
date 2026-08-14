package mills

import (
	"strings"
	"testing"
)

// TestNormalizeEvidenceTokens pins the collapse contract from both sides: the
// parts that differ between two occurrences of the SAME failure disappear, and
// the words that name the failure survive. Both halves matter — a normalizer
// that collapses too little never clusters, and one that collapses too much
// merges unrelated failures into a single bogus signature.
func TestNormalizeEvidenceTokens(t *testing.T) {
	cases := []struct {
		name string
		text string
		want string
	}{
		{
			name: "sha collapses",
			text: "build 4f3a2b1c9d8e7f60 failed",
			want: "build <hex> failed",
		},
		{
			name: "uuid collapses before hex",
			text: "run 3f2a1b0c-9d8e-4f60-8a1b-2c3d4e5f6071 aborted",
			want: "run <uuid> aborted",
		},
		{
			name: "path collapses",
			text: "cannot stat /builds/loom-core/pkg/mills/store.go",
			want: "cannot stat <path>",
		},
		{
			name: "duration collapses before number",
			text: "context deadline exceeded after 30s",
			want: "context deadline exceeded after <dur>",
		},
		{
			name: "fractional duration collapses",
			text: "stage took 1.5h",
			want: "stage took <dur>",
		},
		{
			name: "bare number collapses",
			text: "exit status 137",
			want: "exit status <num>",
		},
		{
			name: "case is folded",
			text: "ERROR: Connection Refused",
			want: "error connection refused",
		},
		{
			name: "hex-shaped word stays a word",
			text: "the interface was defaced",
			want: "the interface was defaced",
		},
		{
			name: "identifier with digits survives",
			text: "sha256 mismatch",
			want: "sha256 mismatch",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := strings.Join(normalizeEvidenceTokens(tc.text), " ")
			if got != tc.want {
				t.Errorf("normalize(%q) = %q, want %q", tc.text, got, tc.want)
			}
		})
	}
}

// TestNormalizeEvidenceTokensCollapsesSameShape: two occurrences of one failure
// differing only in ids, paths, sizes, and timings normalize identically.
func TestNormalizeEvidenceTokensCollapsesSameShape(t *testing.T) {
	a := "knitter: shard 7 refused token 4f3a2b1c9d8e7f60 after 30s at /builds/a/x.go"
	b := "knitter: shard 41 refused token 90ab12cd34ef5678 after 2m at /builds/b/y.go"
	if got, want := normalizeEvidenceTokens(a), normalizeEvidenceTokens(b); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("same-shape texts normalized differently:\n a: %v\n b: %v", got, want)
	}
}

// TestNormalizeEvidenceTokensKeepsDistinctErrorsApart: the collapse must never
// erase the words that make two failures different. This is the property that
// stops the miner proposing one signature for the whole factory.
func TestNormalizeEvidenceTokensKeepsDistinctErrorsApart(t *testing.T) {
	texts := []string{
		"connection refused dialing postgres at 10.0.0.4:5432",
		"disk quota exceeded on runner 10.0.0.4:5432",
		"context deadline exceeded after 30s waiting for lock",
	}
	seen := map[string]int{}
	for i, text := range texts {
		key := strings.Join(normalizeEvidenceTokens(text), " ")
		if prev, ok := seen[key]; ok {
			t.Fatalf("texts %d and %d collapsed to the same tokens %q", prev, i, key)
		}
		seen[key] = i
	}
}

// TestNormalizeEvidenceTokensKeepsTail: a log tail longer than the cap is
// truncated from the FRONT, because the failure that ended the run is at the
// end of the tail.
func TestNormalizeEvidenceTokensKeepsTail(t *testing.T) {
	noise := strings.Repeat("compiling package ", signatureMaxTokens)
	tokens := normalizeEvidenceTokens(noise + " fatal linker error")
	if len(tokens) != signatureMaxTokens {
		t.Fatalf("tokens = %d, want %d", len(tokens), signatureMaxTokens)
	}
	if got := strings.Join(tokens[len(tokens)-3:], " "); got != "fatal linker error" {
		t.Errorf("tail = %q, want the trailing failure text", got)
	}
}
