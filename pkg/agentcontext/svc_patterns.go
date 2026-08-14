// svc_patterns.go -- PatternSvc: CRUD + semantic search for the first-class
// Pattern entity (the "pattern library" / textile-pattern catalog).
//
// SCOPING INVARIANT (same as Plan): pattern reads are scoped by pattern_id /
// makes / status and are NEVER filtered by agent_id. The catalog is deliberately
// cross-agent — any agent or Mills pod resolves the live library by id; writes
// are attributed (created_by) but not gated by identity.
//
// Qdrant is the source of truth (so patterns are visible across processes /
// worktrees / agents); the in-memory map is a write-through cache consulted only
// as a fallback when Qdrant is unavailable. SeedBuiltins upserts the curated
// builtin catalog (currently pattern-go-rest-service) idempotently on startup.
package agentcontext

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"gitlab.flexinfer.ai/libs/mcp-go"

	"github.com/crb2nu/loom/pkg/codebase/embed"
	"github.com/crb2nu/loom/pkg/validate"
)

// PatternSvc manages Pattern records.
type PatternSvc struct {
	mu       sync.RWMutex
	patterns map[string]*Pattern

	patternsQ  *QdrantClient // CollPatterns
	embedr     embed.Embedder
	vectorSize *int
	logger     *slog.Logger
}

// NewPatternSvc constructs a PatternSvc. embedr/vectorSize may be nil in tests.
func NewPatternSvc(patternsQ *QdrantClient, embedr embed.Embedder, vectorSize *int, logger *slog.Logger) *PatternSvc {
	if vectorSize == nil {
		vs := 0
		vectorSize = &vs
	}
	return &PatternSvc{
		patterns:   make(map[string]*Pattern),
		patternsQ:  patternsQ,
		embedr:     embedr,
		vectorSize: vectorSize,
		logger:     logger,
	}
}

// ---- Service delegates -----------------------------------------------------

func (s *Service) HandlePatternAdd(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	return s.patterns.Add(ctx, args)
}
func (s *Service) HandlePatternGet(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	return s.patterns.Get(ctx, args)
}
func (s *Service) HandlePatternList(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	return s.patterns.List(ctx, args)
}
func (s *Service) HandlePatternSearch(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	return s.patterns.Search(ctx, args)
}

// ---- Add -------------------------------------------------------------------

// Add persists a new (or updated) Pattern and returns its id. Idempotent by id:
// the same name maps to the same pattern-<slug> id, so re-adding upserts.
func (ps *PatternSvc) Add(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	v := validate.NewArgs(args)
	name := v.Required("name")
	makes := v.Required("makes")
	if err := v.Validate(); err != nil {
		return mcp.ErrorResult(err), nil
	}
	status := v.String("status", PatternStatusCandidate)
	if !patternStatusValid(status) {
		return mcp.ErrorResult(fmt.Errorf("invalid status %q", status)), nil
	}
	now := time.Now().UTC()
	p := &Pattern{
		ID:             v.String("id", ""),
		Name:           name,
		Makes:          makes,
		Description:    v.String("description", ""),
		Version:        v.String("version", "0.1"),
		Status:         status,
		DeployContract: v.String("deploy_contract", ""),
		Engrams:        v.StringSlice("engrams"),
		Tags:           v.StringSlice("tags"),
		CreatedBy:      v.String("agent_id", ""),
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if p.ID == "" {
		p.ID = GeneratePatternID(name)
	}
	p.Slug = patternSlug(name)
	remarshalInto(args["materials_schema"], &p.MaterialsSchema)
	remarshalInto(args["tools_manifest"], &p.ToolsManifest)
	remarshalInto(args["pins"], &p.Pins)
	remarshalInto(args["slice_template"], &p.SliceTemplate)
	if args["gauge"] != nil {
		var g PatternGauge
		remarshalInto(args["gauge"], &g)
		p.Gauge = &g
	}
	if args["provenance"] != nil {
		var pr PatternProvenance
		remarshalInto(args["provenance"], &pr)
		p.Provenance = &pr
	}

	if err := ps.persist(ctx, p); err != nil {
		return mcp.ErrorResult(fmt.Errorf("persist pattern: %w", err)), nil
	}
	ps.mu.Lock()
	ps.patterns[p.ID] = p
	ps.mu.Unlock()

	return mcp.JSONResult(map[string]any{
		"ok":         true,
		"pattern_id": p.ID,
		"slug":       p.Slug,
		"makes":      p.Makes,
		"status":     p.Status,
	})
}

// ---- Get -------------------------------------------------------------------

// Get returns a Pattern by id. Qdrant-first, cache fallback. NOT agent-scoped.
func (ps *PatternSvc) Get(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	v := validate.NewArgs(args)
	patternID := v.Required("pattern_id")
	if err := v.Validate(); err != nil {
		return mcp.ErrorResult(err), nil
	}
	p, err := ps.fetch(ctx, patternID)
	if err != nil {
		return mcp.ErrorResult(fmt.Errorf("get pattern: %w", err)), nil
	}
	if p == nil {
		return mcp.ErrorResult(fmt.Errorf("pattern %q not found", patternID)), nil
	}
	return mcp.JSONResult(map[string]any{"ok": true, "pattern": p})
}

// fetch resolves a pattern by id, Qdrant-first.
func (ps *PatternSvc) fetch(ctx context.Context, patternID string) (*Pattern, error) {
	if ps.patternsQ != nil {
		raw, err := ps.patternsQ.GetPoint(ctx, patternID, false)
		switch {
		case err == nil:
			if p := payloadToPattern(raw.Payload); p != nil {
				ps.mu.Lock()
				ps.patterns[p.ID] = p
				ps.mu.Unlock()
				return p, nil
			}
		case errors.Is(err, ErrCollectionNotFound):
		default:
			ps.logger.Debug("pattern fetch from qdrant failed; trying cache", "pattern_id", patternID, "error", err)
		}
	}
	ps.mu.RLock()
	cached := ps.patterns[patternID]
	ps.mu.RUnlock()
	return cached, nil
}

// ---- List ------------------------------------------------------------------

// List returns patterns filtered by makes and/or status. NOT agent-scoped.
func (ps *PatternSvc) List(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	v := validate.NewArgs(args)
	makes := v.String("makes", "")
	status := v.String("status", "")
	limit := v.Int("limit", 100)

	patterns, err := ps.list(ctx, makes, status, limit)
	if err != nil {
		return mcp.ErrorResult(fmt.Errorf("list patterns: %w", err)), nil
	}
	return mcp.JSONResult(map[string]any{"ok": true, "count": len(patterns), "patterns": patterns})
}

func (ps *PatternSvc) list(ctx context.Context, makes, status string, limit int) ([]*Pattern, error) {
	if ps.patternsQ == nil {
		return ps.listFromCache(makes, status), nil
	}
	var conds []any
	if makes != "" {
		conds = append(conds, Match("makes", makes))
	}
	if status != "" {
		conds = append(conds, Match("status", status))
	}
	var filter map[string]any
	if len(conds) > 0 {
		filter = FilterMust(conds...)
	}
	points, err := ps.patternsQ.ScrollPoints(ctx, filter, limit, false)
	if err != nil {
		return ps.listFromCache(makes, status), nil
	}
	out := make([]*Pattern, 0, len(points))
	for _, pt := range points {
		if p := payloadToPattern(pt.Payload); p != nil {
			out = append(out, p)
		}
	}
	return out, nil
}

func (ps *PatternSvc) listFromCache(makes, status string) []*Pattern {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	out := make([]*Pattern, 0, len(ps.patterns))
	for _, p := range ps.patterns {
		if makes != "" && p.Makes != makes {
			continue
		}
		if status != "" && p.Status != status {
			continue
		}
		out = append(out, p)
	}
	return out
}

// ---- Search ----------------------------------------------------------------

// Search runs a semantic search over pattern name+makes+description. Falls back
// to a keyword list when no embedder/Qdrant is available.
func (ps *PatternSvc) Search(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	v := validate.NewArgs(args)
	query := v.Required("query")
	if err := v.Validate(); err != nil {
		return mcp.ErrorResult(err), nil
	}
	limit := v.Int("limit", 20)

	if ps.patternsQ == nil || ps.embedr == nil {
		patterns, _ := ps.list(ctx, "", "", limit)
		return mcp.JSONResult(map[string]any{"ok": true, "count": len(patterns), "patterns": patterns, "mode": "fallback-list"})
	}
	vec, err := ps.embedr.EmbedQuery(ctx, query)
	if err != nil || len(vec) == 0 {
		ps.logger.Warn("pattern search embed failed; falling back to list", "error", err)
		patterns, _ := ps.list(ctx, "", "", limit)
		return mcp.JSONResult(map[string]any{"ok": true, "count": len(patterns), "patterns": patterns, "mode": "fallback-list"})
	}
	points, err := ps.patternsQ.SearchRaw(ctx, vec, nil, limit)
	if err != nil {
		return mcp.ErrorResult(fmt.Errorf("pattern search: %w", err)), nil
	}
	out := make([]*Pattern, 0, len(points))
	for _, pt := range points {
		if p := payloadToPattern(pt.Payload); p != nil {
			out = append(out, p)
		}
	}
	return mcp.JSONResult(map[string]any{"ok": true, "count": len(out), "patterns": out, "mode": "semantic"})
}

// ---- Seed ------------------------------------------------------------------

// SeedBuiltins upserts the curated builtin catalog idempotently. Best-effort:
// called on service start; a failure is logged, not fatal. Existing patterns
// with the same id are overwritten with the builtin definition (the code is the
// source of truth for builtins).
func (ps *PatternSvc) SeedBuiltins(ctx context.Context) {
	now := time.Now().UTC()
	for _, p := range BuiltinPatterns() {
		if p.CreatedAt.IsZero() {
			p.CreatedAt = now
		}
		p.UpdatedAt = now
		if err := ps.persist(ctx, p); err != nil {
			ps.logger.Warn("seed pattern failed", "pattern_id", p.ID, "error", err)
			continue
		}
		ps.mu.Lock()
		ps.patterns[p.ID] = p
		ps.mu.Unlock()
		ps.logger.Debug("seeded builtin pattern", "pattern_id", p.ID)
	}
}

// ---- persistence -----------------------------------------------------------

// persist writes a pattern to Qdrant, embedding name+makes+description best-
// effort (a failed embedder must NEVER block the write).
func (ps *PatternSvc) persist(ctx context.Context, p *Pattern) error {
	if ps.patternsQ == nil {
		return nil
	}
	vec := ps.embedText(ctx, p.Name+" "+p.Makes+" "+p.Description)
	degraded := len(vec) == 0
	size := ps.resolveVectorSize(ctx, vec, ps.patternsQ)
	if err := ps.patternsQ.EnsureCollection(ctx, size); err != nil {
		return err
	}
	if len(vec) > 0 && len(vec) != size {
		return fmt.Errorf("pattern embedding dimension mismatch: got %d, collection requires %d", len(vec), size)
	}
	if len(vec) == 0 {
		degraded = true
		vec = fallbackEmbedVector(size)
	}
	payload := patternToPayload(p)
	payload[embeddingFallbackPayloadKey] = degraded
	return ps.patternsQ.Upsert(ctx, []Point{{ID: p.ID, Vector: vec, Payload: payload}}, true)
}

// BackfillFallbackVectors repairs a bounded page of patterns explicitly marked
// as having been written with a fallback vector. Cursor is Qdrant's opaque
// scroll offset and may be passed back unchanged to resume the sweep.
func (ps *PatternSvc) BackfillFallbackVectors(ctx context.Context, limit int, cursor any) (BackfillResult, error) {
	return backfillFallbackPoints(ctx, ps.patternsQ, ps.embedr, ps.vectorSize, ps.logger, limit, cursor, func(payload map[string]any) string {
		return toString(payload["name"]) + " " + toString(payload["makes"]) + " " + toString(payload["description"])
	})
}

// embedText returns an embedding for text, or nil on any failure / no embedder.
func (ps *PatternSvc) embedText(ctx context.Context, text string) []float64 {
	if ps.embedr == nil {
		return nil
	}
	vecs, err := ps.embedr.EmbedDocuments(ctx, []string{text})
	if err != nil || len(vecs) != 1 || len(vecs[0]) == 0 {
		ps.logger.Warn("pattern embed failed; using fallback vector", "error", err)
		return nil
	}
	return vecs[0]
}

// resolveVectorSize picks the embedding dimension (mirrors PlanSvc).
func (ps *PatternSvc) resolveVectorSize(ctx context.Context, vec []float64, q *QdrantClient) int {
	if len(vec) > 0 {
		*ps.vectorSize = len(vec)
	}
	if *ps.vectorSize <= 0 {
		if exists, size, err := q.GetCollectionVectorSize(ctx); err == nil && exists && size > 0 {
			*ps.vectorSize = size
		}
	}
	if *ps.vectorSize <= 0 {
		*ps.vectorSize = defaultEmbedVectorSize
	}
	return *ps.vectorSize
}

// remarshalInto round-trips an arbitrary decoded-JSON arg (map/slice from MCP
// args) into a typed destination via JSON. Best-effort: leaves dst unchanged on
// any marshal/unmarshal error.
func remarshalInto(raw any, dst any) {
	if raw == nil {
		return
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return
	}
	_ = json.Unmarshal(b, dst)
}
