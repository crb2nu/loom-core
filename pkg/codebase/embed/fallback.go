package embed

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/crb2nu/loom/pkg/telemetry"
)

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
	primary        Embedder
	secondary      Embedder
	primaryName    string
	primaryModel   string
	secondaryName  string
	secondaryModel string
}

// Ensure FallbackEmbedder implements Embedder.
var _ Embedder = (*FallbackEmbedder)(nil)

// NewFallbackEmbedder wraps primary with a write-path fallback to secondary.
// If secondary is nil the result behaves exactly like primary.
func NewFallbackEmbedder(primary, secondary Embedder) *FallbackEmbedder {
	f := &FallbackEmbedder{primary: primary, secondary: secondary}
	f.primaryName, f.primaryModel = embedderNameModel(primary)
	f.secondaryName, f.secondaryModel = embedderNameModel(secondary)
	return f
}

// EmbedQuery delegates to the primary only — no cross-space fallback for search.
func (f *FallbackEmbedder) EmbedQuery(ctx context.Context, query string) ([]float64, error) {
	vec, err := f.primary.EmbedQuery(ctx, query)
	if err != nil {
		telemetry.RecordEmbeddingFallback(ctx, telemetry.EmbeddingFallbackEvent{
			Path:              telemetry.EmbeddingPathQuery,
			Outcome:           telemetry.EmbeddingOutcomeDegraded,
			Reason:            queryReason(err),
			PrimaryProvider:   f.primaryName,
			PrimaryModel:      f.primaryModel,
			SecondaryProvider: f.secondaryName,
			SecondaryModel:    f.secondaryModel,
			BatchSize:         1,
		})
	}
	return vec, err
}

// EmbedDocuments tries the primary, then the secondary on any primary error.
func (f *FallbackEmbedder) EmbedDocuments(ctx context.Context, texts []string) ([][]float64, error) {
	vecs, err := f.primary.EmbedDocuments(ctx, texts)
	if err == nil {
		return vecs, nil
	}
	if f.secondary == nil {
		telemetry.RecordEmbeddingFallback(ctx, telemetry.EmbeddingFallbackEvent{
			Path:              telemetry.EmbeddingPathDocuments,
			Outcome:           telemetry.EmbeddingOutcomeDegraded,
			Reason:            telemetry.EmbeddingReasonNoSecondary,
			PrimaryProvider:   f.primaryName,
			PrimaryModel:      f.primaryModel,
			SecondaryProvider: f.secondaryName,
			SecondaryModel:    f.secondaryModel,
			BatchSize:         len(texts),
		})
		return nil, err
	}
	fallbackVecs, fallbackErr := f.secondary.EmbedDocuments(ctx, texts)
	event := telemetry.EmbeddingFallbackEvent{
		Path:              telemetry.EmbeddingPathDocuments,
		Outcome:           telemetry.EmbeddingOutcomeFallbackSuccess,
		Reason:            documentsReason(err),
		PrimaryProvider:   f.primaryName,
		PrimaryModel:      f.primaryModel,
		SecondaryProvider: f.secondaryName,
		SecondaryModel:    f.secondaryModel,
		BatchSize:         len(texts),
	}
	if fallbackErr != nil {
		event.Outcome = telemetry.EmbeddingOutcomeFallbackError
		event.Reason = secondaryReason(fallbackErr)
	}
	telemetry.RecordEmbeddingFallback(ctx, event)
	return fallbackVecs, fallbackErr
}

// Name reports the primary's name (the secondary is an outage-only stand-in).
func (f *FallbackEmbedder) Name() string { return f.primary.Name() }

// Model reports the primary's model identifier.
func (f *FallbackEmbedder) Model() string { return f.primary.Model() }

func embedderNameModel(e Embedder) (string, string) {
	if e == nil {
		return "", ""
	}
	return e.Name(), e.Model()
}

// HTTPStatusError is returned by HTTP-backed embedders when the provider
// responds with an error status. It lets fallback telemetry classify overload
// without parsing provider strings at every call site.
type HTTPStatusError struct {
	Provider   string
	StatusCode int
	Body       string
}

func (e *HTTPStatusError) Error() string {
	if e == nil {
		return ""
	}
	if e.Body == "" {
		return fmt.Sprintf("%s API HTTP %d", e.Provider, e.StatusCode)
	}
	return fmt.Sprintf("%s API HTTP %d: %s", e.Provider, e.StatusCode, e.Body)
}

func queryReason(err error) string {
	if errors.Is(err, ErrEmbedderUnavailable) {
		return telemetry.EmbeddingReasonCircuitOpen
	}
	if isProviderOverload(err) {
		return telemetry.EmbeddingReasonProviderOverload
	}
	return telemetry.EmbeddingReasonQueryPrimaryError
}

func documentsReason(err error) string {
	if errors.Is(err, ErrEmbedderUnavailable) {
		return telemetry.EmbeddingReasonCircuitOpen
	}
	if isProviderOverload(err) {
		return telemetry.EmbeddingReasonProviderOverload
	}
	return telemetry.EmbeddingReasonPrimaryError
}

func secondaryReason(err error) string {
	if errors.Is(err, ErrEmbedderUnavailable) {
		return telemetry.EmbeddingReasonCircuitOpen
	}
	if isProviderOverload(err) {
		return telemetry.EmbeddingReasonProviderOverload
	}
	return telemetry.EmbeddingReasonSecondaryError
}

func isProviderOverload(err error) bool {
	if err == nil {
		return false
	}
	var statusErr *HTTPStatusError
	if errors.As(err, &statusErr) {
		return isOverloadStatus(statusErr.StatusCode) || containsOverloadSignal(statusErr.Body)
	}
	return containsOverloadSignal(err.Error())
}

func isOverloadStatus(status int) bool {
	switch status {
	case 429, 502, 503, 504, 522:
		return true
	default:
		return false
	}
}

func containsOverloadSignal(s string) bool {
	lower := strings.ToLower(s)
	return strings.Contains(lower, "overload") ||
		strings.Contains(lower, "rate limit") ||
		strings.Contains(lower, "too many requests") ||
		strings.Contains(lower, "capacity") ||
		strings.Contains(lower, "temporarily unavailable")
}
