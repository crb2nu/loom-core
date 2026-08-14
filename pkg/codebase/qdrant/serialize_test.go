package qdrant

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestFilterAcrossRepos_MatchAll(t *testing.T) {
	t.Parallel()

	if f := FilterAcrossRepos(nil, nil); f != nil {
		t.Fatalf("expected nil (match-all) filter, got %v", f)
	}
	if f := FilterAcrossRepos([]string{}, []string{}); f != nil {
		t.Fatalf("expected nil (match-all) filter for empty slices, got %v", f)
	}
}

func TestFilterAcrossRepos_NeverScopesRepo(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		languages  []string
		chunkTypes []string
	}{
		{"language only", []string{"go"}, nil},
		{"chunk type only", nil, []string{"function"}},
		{"both", []string{"go", "typescript"}, []string{"function", "method"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := FilterAcrossRepos(tc.languages, tc.chunkTypes)
			b, err := json.Marshal(f)
			if err != nil {
				t.Fatalf("marshal filter: %v", err)
			}
			// The whole point of the aggregate path: no repo_id condition, so
			// Qdrant counts across every repo instead of the empty-string repo.
			if strings.Contains(string(b), "repo_id") {
				t.Fatalf("FilterAcrossRepos must not scope by repo_id, got: %s", b)
			}
			for _, l := range tc.languages {
				if !strings.Contains(string(b), `"`+l+`"`) {
					t.Fatalf("expected language %q in filter %s", l, b)
				}
			}
			for _, ct := range tc.chunkTypes {
				if !strings.Contains(string(b), `"`+ct+`"`) {
					t.Fatalf("expected chunk_type %q in filter %s", ct, b)
				}
			}
		})
	}
}
