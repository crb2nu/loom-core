package embed

import "context"

// FallbackEmbedder routes embedding calls to a primary embedder and, when the
// primary fails, to a secondary embedder for the WRITE path only.
//
// Rationale: a same-dimension secondary (e.g. a self-hosted gte/bge model
// standing in for a down hosted provider) lives in a DIFFERENT vector space
// than the primary. Falling back for queries would compare a secondary-space
// query vector against a primarily primary-space corpus — noise. So EmbedQuery
// never falls back; the caller degrades to keyword search instead. EmbedDocuments
// (the write path) DOES fall back, so new entries are still stored with a
// usable, dimension-valid vector during a primary outage instead of erroring.
//
// The secondary MUST emit the same vector dimension as the collection (and thus
// the primary); a mismatch surfaces as a downstream upsert error by design.
type FallbackEmbedder struct {
	primary   Embedder
	secondary Embedder
}

// Ensure FallbackEmbedder implements Embedder.
var _ Embedder = (*FallbackEmbedder)(nil)

// NewFallbackEmbedder wraps primary with a write-path fallback to secondary.
// If secondary is nil the result behaves exactly like primary.
func NewFallbackEmbedder(primary, secondary Embedder) *FallbackEmbedder {
	return &FallbackEmbedder{primary: primary, secondary: secondary}
}

// EmbedQuery delegates to the primary only — no cross-space fallback for search.
func (f *FallbackEmbedder) EmbedQuery(ctx context.Context, query string) ([]float64, error) {
	return f.primary.EmbedQuery(ctx, query)
}

// EmbedDocuments tries the primary, then the secondary on any primary error.
func (f *FallbackEmbedder) EmbedDocuments(ctx context.Context, texts []string) ([][]float64, error) {
	vecs, err := f.primary.EmbedDocuments(ctx, texts)
	if err == nil {
		return vecs, nil
	}
	if f.secondary == nil {
		return nil, err
	}
	return f.secondary.EmbedDocuments(ctx, texts)
}

// Name reports the primary's name (the secondary is an outage-only stand-in).
func (f *FallbackEmbedder) Name() string { return f.primary.Name() }

// Model reports the primary's model identifier.
func (f *FallbackEmbedder) Model() string { return f.primary.Model() }
