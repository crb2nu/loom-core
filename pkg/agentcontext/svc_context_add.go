package agentcontext

import (
	"context"
	"fmt"
	"strings"
	"time"

	"gitlab.flexinfer.ai/libs/mcp-go"

	"github.com/crb2nu/loom/pkg/validate"
)

// --- Context CRUD ---

func (cs *ContextSvc) Add(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	v := validate.NewArgs(args)
	sessionID := v.Required("session_id")
	entriesRaw := v.RequiredAny("entries")

	if err := v.Validate(); err != nil {
		return mcp.ErrorResult(err), nil
	}

	session, err := cs.getSession(ctx, sessionID)
	if err != nil || session == nil {
		return mcp.ErrorResult(fmt.Errorf("session %s not found", sessionID)), nil
	}

	entriesArr, ok := entriesRaw.([]any)
	if !ok || len(entriesArr) == 0 {
		return mcp.ErrorResult(fmt.Errorf("entries array is required")), nil
	}

	type parsedEntry struct {
		raw            map[string]any
		durability     Durability
		title          string
		content        string
		mirrorToMemory bool
	}
	var parsed []parsedEntry

	for _, raw := range entriesArr {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		title := toString(m["title"])
		content := toString(m["content"])
		if title == "" || content == "" {
			continue
		}
		durabilityRaw := strings.TrimSpace(toString(m["durability"]))
		dur := Durability(durabilityRaw)
		if dur == "" {
			dur = DurabilitySession
		}
		parsed = append(parsed, parsedEntry{
			raw:            m,
			durability:     dur,
			title:          title,
			content:        content,
			mirrorToMemory: durabilityRaw == "" && shouldAutoMirrorToMemory(EntryType(strings.TrimSpace(toString(m["entry_type"])))),
		})
	}

	if len(parsed) == 0 {
		return mcp.ErrorResult(fmt.Errorf("no valid entries provided")), nil
	}

	var allIDs []string
	routedCounts := map[string]int{}

	var contextEntries []ContextEntry
	var embedTexts []string
	for _, p := range parsed {
		if p.durability != DurabilitySession {
			continue
		}
		entry := cs.buildContextEntry(session, p.raw, p.title, p.content)
		contextEntries = append(contextEntries, entry)
		embedTexts = append(embedTexts, entry.Title+"\n"+entry.Content)
	}

	if len(contextEntries) > 0 {
		if strings.TrimSpace(cs.cfg.EmbedAPIKey) == "" {
			return mcp.ErrorResult(fmt.Errorf("AGENT_CONTEXT_EMBED_API_KEY (or MORPH_API_KEY / OPENAI_API_KEY) is not set")), nil
		}
		ids, err := cs.storeContextEntries(ctx, contextEntries, embedTexts)
		if err != nil {
			return mcp.ErrorResult(err), nil
		}
		allIDs = append(allIDs, ids...)
		routedCounts["context"] = len(ids)
	}

	for _, p := range parsed {
		if p.durability != DurabilityPersistent && !p.mirrorToMemory {
			continue
		}
		id, err := cs.routeToMemory(ctx, session, p.raw, p.title, p.content)
		if err != nil {
			return mcp.ErrorResult(fmt.Errorf("memory store: %w", err)), nil
		}
		if !p.mirrorToMemory {
			allIDs = append(allIDs, id)
		}
		routedCounts["memory"]++
	}

	for _, p := range parsed {
		if p.durability != DurabilityGraph {
			continue
		}
		id, err := cs.routeToGraph(session, p.raw, p.title, p.content)
		if err != nil {
			return mcp.ErrorResult(fmt.Errorf("graph store: %w", err)), nil
		}
		allIDs = append(allIDs, id)
		routedCounts["graph"]++
	}

	totalTokens := 0
	for _, p := range parsed {
		totalTokens += EstimateTokens(p.title + " " + p.content)
	}
	if cs.addSessionEntryStats != nil {
		// addSessionEntryStats persists the (entry_count, total_tokens)
		// delta to Qdrant via SetPayload synchronously. We deliberately do
		// not full-upsert the session here: a concurrent EndStale or
		// session_end may have just decoded the session payload from Qdrant
		// and written status=ended; a full upsert with a stale in-memory
		// status would re-activate it.
		cs.addSessionEntryStats(session, len(parsed), totalTokens)
	}

	if cs.cfg.AutoSummarize {
		cs.maybeAutoSummarize(ctx, session)
	}

	return mcp.JSONResult(map[string]any{
		"ok":        true,
		"count":     len(parsed),
		"entry_ids": allIDs,
		"routed":    routedCounts,
	})
}

func shouldAutoMirrorToMemory(entryType EntryType) bool {
	switch entryType {
	case EntryTypeDecision, EntryTypeFinding, EntryTypeQuestion, EntryTypeSummary, EntryTypeError, EntryTypeHandoff:
		return true
	default:
		return false
	}
}

func (cs *ContextSvc) buildContextEntry(session *Session, m map[string]any, title, content string) ContextEntry {
	visibility := Visibility(toString(m["visibility"]))
	if visibility == "" {
		visibility = cs.cfg.DefaultVisibility
	}
	ts := time.Now()
	entry := ContextEntry{
		ID:            GenerateID(session.AgentID, session.ID, title+"\n"+content, ts),
		SchemaVersion: SchemaVersion,
		AgentID:       session.AgentID,
		SessionID:     session.ID,
		Namespace:     session.Namespace,
		Project:       canonicalProject(session.Project, session.Namespace, session.PipelineRef),
		EntryType:     EntryType(toString(m["entry_type"])),
		Timestamp:     ts,
		Title:         title,
		Content:       content,
		ContentHash:   ContentHashFunc(content),
		FilePath:      toString(m["file_path"]),
		LineStart:     toInt(m["line_start"]),
		LineEnd:       toInt(m["line_end"]),
		Tags:          toStringSlice(m["tags"]),
		TokenCount:    EstimateTokens(title + " " + content),
		Visibility:    visibility,
		SharedWith:    toStringSlice(m["shared_with"]),
	}
	if meta, ok := m["metadata"].(map[string]any); ok {
		entry.Metadata = meta
	}
	return entry
}

func (cs *ContextSvc) storeContextEntries(ctx context.Context, entries []ContextEntry, embedTexts []string) ([]string, error) {
	coll := cs.qdrant.Get(CollContext)

	// Generate embeddings — best-effort, but bounded. A sporadically failing
	// embedder must never block the context write: embedding is enrichment for
	// semantic search, not a correctness gate for persistence. This mirrors
	// pkg/agentcontext/svc_tasks.go and the pkg/pm risk store, which persist a
	// deterministic fallback vector on embed failure. Without this, an embedder
	// outage (Morph 522 / gte-qwen2-1.5b HTTP 500 / circuit breaker open)
	// hard-fails agent_context_add and agents lose the ability to record any
	// decision or finding.
	//
	// The bound: once fallback usage is systemic (ratio/continuous-failure
	// thresholds in embed_degradation.go), the write fails closed with
	// ErrEmbedderDegraded instead of silently filling the collection with
	// unsearchable hash vectors.
	vectors, err := cs.embed.EmbedDocuments(ctx, embedTexts)
	if err != nil {
		if cs.metrics != nil {
			cs.metrics.EmbeddingErrors.Add(1)
		}
		if derr := recordEmbedWriteOutcome(cs.embedDegradation, cs.metrics, true); derr != nil {
			if cs.logger != nil {
				cs.logger.Warn("context embed degraded past fail-closed thresholds; rejecting write",
					"error", err, "degraded", derr, "count", len(entries))
			}
			return nil, fmt.Errorf("context embed degraded (last error: %v): %w", err, derr)
		}
		if cs.logger != nil {
			cs.logger.Warn("context embed failed; persisting with fallback vectors",
				"error", err, "count", len(entries))
		}
		vectors = nil
	} else if len(vectors) != len(entries) {
		if derr := recordEmbedWriteOutcome(cs.embedDegradation, cs.metrics, true); derr != nil {
			if cs.logger != nil {
				cs.logger.Warn("context embed degraded past fail-closed thresholds; rejecting write",
					"got", len(vectors), "want", len(entries), "degraded", derr)
			}
			return nil, fmt.Errorf("context embed degraded (count mismatch %d/%d): %w", len(vectors), len(entries), derr)
		}
		if cs.logger != nil {
			cs.logger.Warn("context embed count mismatch; persisting with fallback vectors",
				"got", len(vectors), "want", len(entries))
		}
		vectors = nil
	} else {
		_ = recordEmbedWriteOutcome(cs.embedDegradation, cs.metrics, false)
	}

	// Determine the vector size: a real embedding wins, then the shared
	// discovered size, then the live collection, then the embedder default.
	for _, v := range vectors {
		if len(v) > 0 {
			*cs.vectorSize = len(v)
			break
		}
	}
	if *cs.vectorSize <= 0 {
		if exists, size, gerr := coll.GetCollectionVectorSize(ctx); gerr == nil && exists && size > 0 {
			*cs.vectorSize = size
		}
	}
	if *cs.vectorSize <= 0 {
		*cs.vectorSize = defaultEmbedVectorSize
	}

	if err := coll.EnsureCollection(ctx, *cs.vectorSize); err != nil {
		return nil, fmt.Errorf("ensure collection: %w", err)
	}
	for i, vector := range vectors {
		if len(vector) > 0 && len(vector) != *cs.vectorSize {
			return nil, fmt.Errorf("context embedding dimension mismatch at index %d: got %d, collection requires %d", i, len(vector), *cs.vectorSize)
		}
	}

	// Build points. Any entry lacking a usable, correctly-sized embedding gets a
	// deterministic non-zero fallback vector (Qdrant rejects all-zero vectors
	// under cosine distance) so the entry still persists and stays filterable by
	// payload (project/session/entry_type).
	points := make([]Point, 0, len(entries))
	for i, entry := range entries {
		var vector []float64
		if i < len(vectors) && len(vectors[i]) == *cs.vectorSize {
			vector = vectors[i]
		} else {
			vector = fallbackEmbedVector(*cs.vectorSize)
		}
		points = append(points, Point{
			ID:     entry.ID,
			Vector: vector,
			Payload: func() map[string]any {
				payload := EntryToPayload(entry, cs.cfg.EmbedModel)
				payload[embeddingFallbackPayloadKey] = i >= len(vectors) || len(vectors[i]) != *cs.vectorSize
				return payload
			}(),
		})
	}

	if err := cs.upsertBatched(ctx, coll, points); err != nil {
		return nil, fmt.Errorf("upsert entries: %w", err)
	}

	ids := make([]string, len(entries))
	for i, e := range entries {
		ids[i] = e.ID
	}
	return ids, nil
}

// embedQueryBestEffort returns an embedding for text, falling back to a
// deterministic non-zero vector when the embedder fails or returns a
// wrong-sized result. A failed embedder must never block a context/annotation
// write — embedding is enrichment for semantic search, not a correctness gate
// for persistence (mirrors storeContextEntries and pkg/agentcontext/svc_tasks.go).
// The shared *cs.vectorSize is updated as a side effect. A non-empty vector
// with the wrong size is returned as an error so callers persist nothing.
func (cs *ContextSvc) embedQueryBestEffort(ctx context.Context, text string, coll *QdrantClient) ([]float64, error) {
	vector, err := cs.embed.EmbedQuery(ctx, text)
	if err != nil {
		if cs.metrics != nil {
			cs.metrics.EmbeddingErrors.Add(1)
		}
		if cs.logger != nil {
			cs.logger.Warn("context embed failed; using fallback vector", "error", err)
		}
		vector = nil
	}
	if len(vector) > 0 {
		*cs.vectorSize = len(vector)
	}
	if *cs.vectorSize <= 0 {
		if exists, size, gerr := coll.GetCollectionVectorSize(ctx); gerr == nil && exists && size > 0 {
			*cs.vectorSize = size
		}
	}
	if *cs.vectorSize <= 0 {
		*cs.vectorSize = defaultEmbedVectorSize
	}
	if len(vector) > 0 && len(vector) != *cs.vectorSize {
		return nil, fmt.Errorf("context embedding dimension mismatch: got %d, collection requires %d", len(vector), *cs.vectorSize)
	}
	if len(vector) == 0 {
		vector = fallbackEmbedVector(*cs.vectorSize)
	}
	return vector, nil
}

func (cs *ContextSvc) routeToMemory(ctx context.Context, session *Session, m map[string]any, title, content string) (string, error) {
	if cs.persistedMemoryHierarchy == nil {
		return "", fmt.Errorf("memory hierarchy not available")
	}

	category := toString(m["entry_type"])
	if category == "" {
		category = "finding"
	}

	item := &MemoryItem{
		Tier:       MemoryTierShortTerm,
		Importance: ImportanceLevelMedium,
		Title:      title,
		Content:    content,
		Category:   category,
		Namespace:  session.Namespace,
		SessionID:  session.ID,
		AgentID:    session.AgentID,
		Tags:       toStringSlice(m["tags"]),
	}
	if metadata, ok := m["metadata"].(map[string]any); ok {
		item.Metadata = metadata
	}
	item.OriginalTokens = EstimateTokens(title + " " + content)

	if err := cs.persistedMemoryHierarchy.AddItemWithPersistence(ctx, item, nil); err != nil {
		return "", err
	}

	cs.metrics.ShortTermMemoryItems.Add(1)
	cs.metrics.ShortTermMemoryTokens.Add(int64(item.OriginalTokens))

	return item.ID, nil
}

func (cs *ContextSvc) routeToGraph(session *Session, m map[string]any, title, content string) (string, error) {
	if cs.knowledgeGraph == nil {
		return "", fmt.Errorf("knowledge graph not available")
	}

	entityType := EntityType(toString(m["entry_type"]))
	if entityType == "" {
		entityType = EntityTypeConcept
	}

	entity := &Entity{
		Type:        entityType,
		Name:        title,
		Description: content,
		Namespace:   session.Namespace,
		FilePath:    toString(m["file_path"]),
		LineStart:   toInt(m["line_start"]),
		LineEnd:     toInt(m["line_end"]),
		Language:    toString(m["language"]),
		SessionID:   session.ID,
		AgentID:     session.AgentID,
		Tags:        toStringSlice(m["tags"]),
	}

	if props, ok := m["metadata"].(map[string]any); ok {
		entity.Properties = props
	}

	if err := cs.knowledgeGraph.AddEntity(entity); err != nil {
		return "", err
	}

	cs.metrics.GraphEntitiesAdded.Add(1)
	return entity.ID, nil
}
