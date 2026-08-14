package agentcontext

import (
	"context"
	"sort"
	"strings"
	"unicode/utf8"
)

// The production gte lane has a 512-token context. Agent-context does not own
// that model's tokenizer, so keep a conservative byte budget below the usual
// three-bytes-per-token estimate. Bounding bytes rather than runes keeps
// multibyte-heavy input from defeating the safety margin.
const maxFallbackBackfillInputBytes = 1400

func prepareFallbackBackfillInput(text string) (prepared string, truncated bool, empty bool) {
	prepared = strings.TrimSpace(text)
	if prepared == "" {
		return "", false, true
	}
	if len(prepared) <= maxFallbackBackfillInputBytes {
		return prepared, false, false
	}
	prepared = prepared[:maxFallbackBackfillInputBytes]
	for !utf8.ValidString(prepared) {
		prepared = prepared[:len(prepared)-1]
	}
	return prepared, true, false
}

// keywordSearch is a vector-free fallback used when the embedding provider is
// unavailable (timeout / circuit breaker open / upstream 5xx). It scrolls
// candidate entries matching the structured filter and ranks them by simple
// keyword overlap with the query, so search/recall keep returning useful
// results during an embedding-provider outage instead of hard-failing.
//
// It deliberately uses Scroll (no vector required) plus in-process ranking so
// it works without a Qdrant full-text payload index.
func keywordSearch(ctx context.Context, qc *QdrantClient, query string, filter map[string]any, limit int) ([]SearchResult, error) {
	if limit <= 0 {
		limit = 10
	}
	tokens := keywordTokens(query)
	if len(tokens) == 0 {
		return []SearchResult{}, nil
	}

	// Pull a generous candidate pool; ranking happens in-process. Bounded so a
	// huge collection cannot blow up the fallback.
	pool := limit * 20
	if pool < 100 {
		pool = 100
	}
	if pool > 2000 {
		pool = 2000
	}

	entries, err := qc.Scroll(ctx, filter, pool)
	if err != nil {
		return nil, err
	}

	scored := make([]SearchResult, 0, len(entries))
	for _, e := range entries {
		score := keywordScore(tokens, e.Title+" "+e.Content)
		if score <= 0 {
			continue
		}
		scored = append(scored, SearchResult{Score: score, Entry: e})
	}

	sort.SliceStable(scored, func(i, j int) bool {
		return scored[i].Score > scored[j].Score
	})
	if len(scored) > limit {
		scored = scored[:limit]
	}
	return scored, nil
}

// keywordTokens lowercases s, splits on non-alphanumeric runes, drops 1-char
// tokens, and de-duplicates while preserving order. (Distinct from the
// compression-package tokenize, which keeps duplicates and short tokens.)
func keywordTokens(s string) []string {
	fields := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9')
	})
	seen := make(map[string]struct{}, len(fields))
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if len(f) < 2 {
			continue
		}
		if _, ok := seen[f]; ok {
			continue
		}
		seen[f] = struct{}{}
		out = append(out, f)
	}
	return out
}

// keywordScore returns the fraction of distinct query tokens present in text,
// in (0,1]. 0 means no overlap.
func keywordScore(tokens []string, text string) float64 {
	if len(tokens) == 0 {
		return 0
	}
	hay := strings.ToLower(text)
	hits := 0
	for _, t := range tokens {
		if strings.Contains(hay, t) {
			hits++
		}
	}
	return float64(hits) / float64(len(tokens))
}
