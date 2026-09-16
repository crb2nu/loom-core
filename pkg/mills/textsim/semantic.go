package textsim

import (
	"context"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/crb2nu/loom/pkg/codebase/embed"
)

const semanticEmbedTimeout = 5 * time.Second

// MergedWorkSemanticScoresTotal counts whether embedding-backed semantic
// grounding was available when a title comparison resolved. It lives in
// textsim so Score can record every outcome without creating a textsim -> mills
// import cycle; mills re-exports it from its conventional metrics surface.
var MergedWorkSemanticScoresTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "mills_mergedwork_semantic_scores_total",
	Help: "Merged-work semantic title scores by embedding availability.",
}, []string{"available"})

// Band is the decision band a title score occupies. Unavailable is reserved
// for a semantic result that could not be computed; lexical scores never use
// it.
type Band string

const (
	BandNone        Band = "none"
	BandGray        Band = "gray_band"
	BandHard        Band = "hard"
	BandUnavailable Band = "unavailable"
)

// ShadowComparison describes the lexical gate and the embedding-backed
// shadow result for one title pair. Gate is deliberately derived only from
// LexicalBand; CombinedBand is observation data and must not affect callers.
type ShadowComparison struct {
	Similarity
	LexicalBand  Band
	CombinedBand Band
	Gate         bool
	Disagrees    bool
}

// CompareShadow runs scorer beside the lexical gate. grayEligible lets callers
// preserve their own recency rule for the gray band. A nil/failing scorer
// reports semantic unavailability while returning the exact lexical gate.
func CompareShadow(ctx context.Context, scorer Scorer, a, b string, threshold float64, grayEligible bool) ShadowComparison {
	lexical := WorkTitleJaccard(a, b)
	lexicalBand := SimilarityBand(lexical, threshold, grayEligible)
	result := ShadowComparison{
		Similarity:   Similarity{Lexical: lexical, Combined: lexical},
		LexicalBand:  lexicalBand,
		CombinedBand: BandUnavailable,
		Gate:         lexicalBand != BandNone,
	}
	if scorer == nil {
		return result
	}

	result.Similarity = scorer.Score(ctx, a, b)
	// Pin the lexical value locally: even a custom scorer cannot smuggle a
	// semantic decision into the established Jaccard gate.
	result.Lexical = lexical
	if !result.SemanticAvailable {
		result.Combined = lexical
		return result
	}
	result.CombinedBand = SimilarityBand(result.Combined, threshold, grayEligible)
	result.Disagrees = result.CombinedBand != result.LexicalBand
	return result
}

// SimilarityBand maps a score onto the same hard/gray/none bands used by
// merged-work grounding. Invalid thresholds disable every band.
func SimilarityBand(score, threshold float64, grayEligible bool) Band {
	if threshold <= 0 || threshold > 1 {
		return BandNone
	}
	if score >= threshold {
		return BandHard
	}
	if grayEligible && threshold > GrayBandFloor && score >= GrayBandFloor {
		return BandGray
	}
	return BandNone
}

// SemanticScorer combines decoration-blind lexical Jaccard and embedding
// cosine similarity by taking their maximum. A nil or failing backend, invalid
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
func (s *SemanticScorer) Score(ctx context.Context, a, b string) (result Similarity) {
	defer func() {
		MergedWorkSemanticScoresTotal.WithLabelValues(strconv.FormatBool(result.SemanticAvailable)).Inc()
	}()

	fallback := (JaccardScorer{}).Score(ctx, a, b)
	fallback.Fallback = true
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
	combined, semantic, ok := CombineWorkTitleSimilarity(fallback.Lexical, vectors[0], vectors[1])
	if !ok {
		return fallback
	}
	return Similarity{
		Lexical:           fallback.Lexical,
		Semantic:          semantic,
		Combined:          combined,
		SemanticAvailable: true,
	}
}

// CosineSimilarity returns cosine similarity mapped from [-1,1] to [0,1].
// The boolean is false for empty, mismatched, zero-norm, NaN, or infinite
// vectors, preventing invalid backend data from contaminating grounding.
func CosineSimilarity(a, b []float64) (float64, bool) {
	cosine, ok := embed.CosineSimilarity(a, b)
	if !ok {
		return 0, false
	}
	return (cosine + 1) / 2, true
}
