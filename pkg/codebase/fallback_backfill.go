package codebase

import (
	"context"
	"fmt"

	"github.com/crb2nu/loom/pkg/codebase/qdrant"
)

// FallbackBackfillResult reports one bounded, cursor-resumable repair page.
type FallbackBackfillResult struct {
	Scanned   int `json:"scanned"`
	Corrected int `json:"corrected"`
	Failed    int `json:"failed"`
	Cursor    any `json:"cursor,omitempty"`
}

// BackfillFallbackVectors repairs marked and legacy [1,0,...] codebase rows.
func (s *Service) BackfillFallbackVectors(ctx context.Context, limit int, cursor any) (FallbackBackfillResult, error) {
	var result FallbackBackfillResult
	if s.qdrant == nil || s.embed == nil {
		return result, fmt.Errorf("qdrant and embedder are required")
	}
	points, next, err := s.qdrant.ScrollPointsPage(ctx, limit, cursor)
	if err != nil {
		return result, err
	}
	result.Scanned, result.Cursor = len(points), next
	exists, size, err := s.qdrant.GetCollectionVectorSize(ctx)
	if err != nil {
		return result, fmt.Errorf("codebase collection vector size: %w", err)
	}
	if !exists || size <= 0 {
		return result, fmt.Errorf("codebase collection vector size unavailable")
	}
	for _, point := range points {
		marked, _ := point.Payload[qdrant.EmbeddingFallbackPayloadKey].(bool)
		if !marked && !legacyFallbackVector(point.Vector) {
			continue
		}
		content, _ := point.Payload["content"].(string)
		vectors, embedErr := s.embed.EmbedDocuments(ctx, []string{content})
		if embedErr != nil || len(vectors) != 1 || len(vectors[0]) != size {
			result.Failed++
			continue
		}
		point.Payload[qdrant.EmbeddingFallbackPayloadKey] = false
		logicalID, _ := point.Payload["id"].(string)
		if logicalID == "" {
			result.Failed++
			continue
		}
		if err := s.qdrant.Upsert(ctx, []qdrant.Point{{ID: logicalID, Vector: vectors[0], Payload: point.Payload}}, true); err != nil {
			result.Failed++
			continue
		}
		result.Corrected++
	}
	return result, nil
}

func legacyFallbackVector(v []float64) bool {
	if len(v) == 0 || v[0] != 1 {
		return false
	}
	for _, n := range v[1:] {
		if n != 0 {
			return false
		}
	}
	return true
}
