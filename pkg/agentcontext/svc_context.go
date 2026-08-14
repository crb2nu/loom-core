package agentcontext

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/crb2nu/loom/pkg/codebase/embed"
)

const embeddingFallbackPayloadKey = "_embedding_fallback"

// BackfillResult reports one bounded, resumable fallback-vector repair page.
type BackfillResult struct {
	Scanned        int            `json:"scanned"`
	Corrected      int            `json:"corrected"`
	Failed         int            `json:"failed"`
	Truncated      int            `json:"truncated,omitempty"`
	FailureReasons map[string]int `json:"failure_reasons"`
	Cursor         any            `json:"cursor,omitempty"`
}

// HandleBackfillFallbackVectors exposes the bounded repair sweep to the MCP
// maintenance surface. collection must be "context" or "patterns".
func (s *Service) HandleBackfillFallbackVectors(ctx context.Context, collection string, limit int, cursor any) (BackfillResult, error) {
	switch collection {
	case "context":
		return s.ctxSvc.BackfillFallbackVectors(ctx, limit, cursor)
	case "patterns":
		return s.patterns.BackfillFallbackVectors(ctx, limit, cursor)
	default:
		return BackfillResult{}, fmt.Errorf("invalid collection %q: must be context or patterns", collection)
	}
}

// BackfillFallbackVectors repairs context rows marked by the degraded write
// path. Failed rows retain their marker and remain eligible for a later sweep.
func (cs *ContextSvc) BackfillFallbackVectors(ctx context.Context, limit int, cursor any) (BackfillResult, error) {
	return backfillFallbackPoints(ctx, cs.qdrant.Get(CollContext), cs.embed, cs.vectorSize, cs.logger, limit, cursor, func(payload map[string]any) string {
		return toString(payload["title"]) + " " + toString(payload["content"])
	})
}

func backfillFallbackPoints(ctx context.Context, q *QdrantClient, e embed.Embedder, vectorSize *int, logger *slog.Logger, limit int, cursor any, text func(map[string]any) string) (BackfillResult, error) {
	result := BackfillResult{FailureReasons: make(map[string]int)}
	if q == nil || e == nil {
		return result, fmt.Errorf("qdrant and embedder are required")
	}
	if limit <= 0 || limit > 256 {
		limit = 100
	}
	// Scan vectors as well as payloads so rows written before the marker was
	// introduced remain repairable. The degraded write path has always used the
	// deterministic [1, 0, ...] vector, which is safe to recognize exactly.
	points, next, err := q.ScrollPointsPage(ctx, nil, limit, cursor, true)
	if err != nil {
		return result, err
	}
	result.Scanned, result.Cursor = len(points), next
	if logger == nil {
		logger = slog.Default()
	}
	const maxRowFailureLogs = 10
	loggedFailures := 0
	recordFailure := func(pointID, reason string, err error) {
		result.Failed++
		result.FailureReasons[reason]++
		if loggedFailures < maxRowFailureLogs {
			logger.Warn("fallback embedding backfill row failed", "row_id", pointID, "reason", reason, "error", err)
			loggedFailures++
		}
	}
	for _, point := range points {
		marked, _ := point.Payload[embeddingFallbackPayloadKey].(bool)
		if !marked && !isFallbackEmbedVector(point.Vector) {
			continue
		}
		input, truncated, empty := prepareFallbackBackfillInput(text(point.Payload))
		if empty {
			recordFailure(point.ID, "empty_input", fmt.Errorf("embedding input is empty or whitespace"))
			continue
		}
		if truncated {
			result.Truncated++
		}
		vectors, embedErr := e.EmbedDocuments(ctx, []string{input})
		if embedErr != nil {
			recordFailure(point.ID, "embed_error", embedErr)
			continue
		}
		if len(vectors) != 1 || len(vectors[0]) == 0 {
			recordFailure(point.ID, "invalid_vector", fmt.Errorf("embedder returned %d vectors", len(vectors)))
			continue
		}
		if *vectorSize <= 0 {
			if exists, size, sizeErr := q.GetCollectionVectorSize(ctx); sizeErr != nil {
				return result, sizeErr
			} else if exists {
				*vectorSize = size
			}
		}
		if *vectorSize <= 0 || len(vectors[0]) != *vectorSize {
			recordFailure(point.ID, "dimension_mismatch", fmt.Errorf("embedding dimension %d does not match collection dimension %d", len(vectors[0]), *vectorSize))
			continue
		}
		point.Payload[embeddingFallbackPayloadKey] = false
		logicalID := toString(point.Payload["id"])
		if logicalID == "" {
			// Older test fixtures and any non-standard rows may not duplicate the
			// logical ID in payload. Retain the existing behavior for those rows.
			logicalID = point.ID
		}
		if upsertErr := q.Upsert(ctx, []Point{{ID: logicalID, Vector: vectors[0], Payload: point.Payload}}, true); upsertErr != nil {
			recordFailure(point.ID, "upsert_error", upsertErr)
			continue
		}
		result.Corrected++
	}
	if result.Failed > loggedFailures {
		logger.Warn("fallback embedding backfill row failure logs capped", "logged", loggedFailures, "suppressed", result.Failed-loggedFailures, "failure_reasons", result.FailureReasons)
	}
	return result, nil
}

func isFallbackEmbedVector(vector []float64) bool {
	if len(vector) == 0 || vector[0] != 1 {
		return false
	}
	for _, value := range vector[1:] {
		if value != 0 {
			return false
		}
	}
	return true
}

// ContextSvc manages context entries, annotations, search, and summary generation.
type ContextSvc struct {
	qdrant                   *QdrantRegistry
	embed                    embed.Embedder
	vectorSize               *int // shared mutable — pointer to Service.vectorSize
	cfg                      Config
	logger                   *slog.Logger
	metrics                  *Metrics
	persistedMemoryHierarchy *persistedMemoryHierarchy
	knowledgeGraph           *KnowledgeGraph

	// Fail-closed gate for the write-path embed fallback (wired by Service;
	// nil keeps the unbounded best-effort behavior for direct constructions).
	embedDegradation *EmbedDegradationTracker

	// Cross-domain callbacks (wired by Service).
	getSession func(ctx context.Context, sessionID string) (*Session, error)

	// Session state callbacks — SessionSvc owns the mutex.
	addSessionEntryStats  func(session *Session, entries int, tokens int)
	readSessionStats      func(session *Session) (entryCount, totalTokens int, lastSummary *time.Time)
	markSessionSummarized func(session *Session, t time.Time)
}

// NewContextSvc creates a new ContextSvc.
func NewContextSvc(qdrant *QdrantRegistry, embedr embed.Embedder, vectorSize *int, cfg Config, logger *slog.Logger, metrics *Metrics) *ContextSvc {
	return &ContextSvc{
		qdrant:     qdrant,
		embed:      embedr,
		vectorSize: vectorSize,
		cfg:        cfg,
		logger:     logger,
		metrics:    metrics,
	}
}
