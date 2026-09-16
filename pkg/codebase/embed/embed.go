// Package embed provides embedding interfaces and implementations for codebase indexing.
package embed

import (
	"context"
	"math"
)

// CosineSimilarity returns the cosine similarity of two embedding vectors in
// [-1, 1]. ok is false when the vectors cannot represent a valid similarity:
// they are empty, differ in dimension, contain non-finite components, or have
// a zero or non-finite norm.
//
// Norms are accumulated with Hypot and components are divided before the dot
// product. This keeps valid vectors near float64's limits from overflowing or
// underflowing during intermediate calculations.
func CosineSimilarity(a, b []float64) (similarity float64, ok bool) {
	if len(a) == 0 || len(a) != len(b) {
		return 0, false
	}

	var normA, normB float64
	for i := range a {
		if !isFinite(a[i]) || !isFinite(b[i]) {
			return 0, false
		}
		normA = math.Hypot(normA, a[i])
		normB = math.Hypot(normB, b[i])
	}
	if normA == 0 || normB == 0 || !isFinite(normA) || !isFinite(normB) {
		return 0, false
	}

	for i := range a {
		similarity += (a[i] / normA) * (b[i] / normB)
	}
	if !isFinite(similarity) {
		return 0, false
	}
	// Floating-point accumulation may stray fractionally outside the range.
	return math.Max(-1, math.Min(1, similarity)), true
}

func isFinite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

// QueryEmbedder is the smallest interface needed by semantic query callers.
type QueryEmbedder interface {
	// EmbedQuery embeds a single query string.
	EmbedQuery(ctx context.Context, query string) ([]float64, error)
}

// DocumentEmbedder is the batch interface used by callers that need comparable
// vectors for more than one text. Keeping it separate lets lightweight
// consumers depend on only the capability they need.
type DocumentEmbedder interface {
	// EmbedDocuments embeds multiple documents in a batch.
	EmbedDocuments(ctx context.Context, texts []string) ([][]float64, error)
}

// Embedder is the full interface for embedding providers.
type Embedder interface {
	QueryEmbedder
	DocumentEmbedder

	// Name returns the embedder name (for logging/debugging).
	Name() string

	// Model returns the model identifier being used.
	Model() string
}

// DummyEmbedder returns zero vectors with a specified dimension.
// Useful for indexing without embeddings (structure-only mode).
type DummyEmbedder struct {
	dimension int
}

// NewDummyEmbedder creates a dummy embedder that returns zero vectors.
func NewDummyEmbedder(dimension int) *DummyEmbedder {
	if dimension <= 0 {
		dimension = 1
	}
	return &DummyEmbedder{dimension: dimension}
}

func (d *DummyEmbedder) EmbedQuery(ctx context.Context, query string) ([]float64, error) {
	return d.makeVector(), nil
}

func (d *DummyEmbedder) EmbedDocuments(ctx context.Context, texts []string) ([][]float64, error) {
	result := make([][]float64, len(texts))
	for i := range texts {
		result[i] = d.makeVector()
	}
	return result, nil
}

func (d *DummyEmbedder) Name() string {
	return "dummy"
}

func (d *DummyEmbedder) Model() string {
	return "none"
}

func (d *DummyEmbedder) makeVector() []float64 {
	v := make([]float64, d.dimension)
	if len(v) > 0 {
		v[0] = 1 // Non-zero for Qdrant compatibility
	}
	return v
}

// Dimension returns the vector dimension.
func (d *DummyEmbedder) Dimension() int {
	return d.dimension
}
