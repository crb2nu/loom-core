package textsim

import (
	"context"
	"math"
	"time"

	"github.com/crb2nu/loom/pkg/codebase/embed"
)

const semanticEmbedTimeout = 5 * time.Second

// Scorer compares two work titles. Implementations must return a score in
// [0,1]; callers can inspect SemanticAvailable to observe lexical fallback.
type Scorer interface {
	Score(ctx context.Context, a, b string) Similarity
}

// Similarity describes both inputs to the combined score. Semantic is zero
// when SemanticAvailable is false and Combined is then exactly Lexical.
type Similarity struct {
	Lexical           float64
	Semantic          float64
	Combined          float64
	SemanticAvailable bool
}

// SemanticScorer combines decoration-blind lexical Jaccard and embedding
// cosine similarity with equal weight. A nil or failing backend, invalid
// vectors, and empty titles all degrade to the unchanged lexical score.
type SemanticScorer struct {
	backend embed.DocumentEmbedder
}

// NewSemanticScorer returns a scorer backed by backend. backend may be nil to
// make the lexical-only fallback explicit at configuration time.
func NewSemanticScorer(backend embed.DocumentEmbedder) *SemanticScorer {
	return &SemanticScorer{backend: backend}
}

// Score computes the combined title similarity. Both normalized titles are
// embedded in one batch so remote providers need at most one request per pair.
func (s *SemanticScorer) Score(ctx context.Context, a, b string) Similarity {
	lexical := WorkTitleJaccard(a, b)
	fallback := Similarity{Lexical: lexical, Combined: lexical}
	if s == nil || s.backend == nil {
		return fallback
	}

	a = NormalizeWorkTitle(a)
	b = NormalizeWorkTitle(b)
	if a == "" || b == "" {
		return fallback
	}

	// Grounding is a best-effort signal. Keep an unhealthy embedding backend
	// from stalling duplicate detection even when the caller supplied no
	// deadline of its own.
	embedCtx, cancel := context.WithTimeout(ctx, semanticEmbedTimeout)
	defer cancel()
	vectors, err := s.backend.EmbedDocuments(embedCtx, []string{a, b})
	if err != nil || len(vectors) != 2 {
		return fallback
	}
	semantic, ok := CosineSimilarity(vectors[0], vectors[1])
	if !ok {
		return fallback
	}
	return Similarity{
		Lexical:           lexical,
		Semantic:          semantic,
		Combined:          (lexical + semantic) / 2,
		SemanticAvailable: true,
	}
}

// CosineSimilarity returns cosine similarity mapped from [-1,1] to [0,1].
// The boolean is false for empty, mismatched, zero-norm, NaN, or infinite
// vectors, preventing invalid backend data from contaminating grounding.
func CosineSimilarity(a, b []float64) (float64, bool) {
	if len(a) == 0 || len(a) != len(b) {
		return 0, false
	}
	var normA, normB float64
	for i := range a {
		if math.IsNaN(a[i]) || math.IsInf(a[i], 0) || math.IsNaN(b[i]) || math.IsInf(b[i], 0) {
			return 0, false
		}
		// Hypot accumulates the norm without overflowing or underflowing when
		// otherwise valid vectors contain components near float64's limits.
		normA = math.Hypot(normA, a[i])
		normB = math.Hypot(normB, b[i])
	}
	if normA == 0 || normB == 0 || math.IsInf(normA, 0) || math.IsInf(normB, 0) {
		return 0, false
	}
	var cosine float64
	for i := range a {
		cosine += (a[i] / normA) * (b[i] / normB)
	}
	if math.IsNaN(cosine) || math.IsInf(cosine, 0) {
		return 0, false
	}
	// Floating-point accumulation can stray just outside the cosine range.
	cosine = math.Max(-1, math.Min(1, cosine))
	return (cosine + 1) / 2, true
}
