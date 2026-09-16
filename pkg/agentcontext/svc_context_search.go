package agentcontext

import (
	"context"
	"fmt"
	"sort"
	"time"

	"gitlab.flexinfer.ai/libs/mcp-go"

	"github.com/crb2nu/loom/pkg/validate"
)

// --- Search & Recall ---

func (cs *ContextSvc) Search(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	v := validate.NewArgs(args)
	// sort=recent lists entries newest-first (optionally since a timestamp)
	// instead of ranking them against the query, which is then optional. The
	// HUD live stream is the consumer: it used to fake this with a
	// "since:<ts>" query text, which a vector/keyword search ranks by
	// similarity to the literal word "since" — months-old entries on the
	// Operator Deck (2026-09-02).
	sortMode := v.String("sort", "relevance")
	sinceRaw := v.String("since", "")
	query := v.String("query", "")
	if sortMode != "recent" && query == "" {
		query = v.Required("query")
	}
	agentID := v.String("agent_id", "")
	sessionID := v.String("session_id", "")
	namespace := v.String("namespace", "")
	entryTypes := v.StringSlice("entry_types")
	tags := v.StringSlice("tags")
	filePath := v.String("file_path", "")
	limit := v.Int("limit", 10)
	includeContent := v.Bool("include_content", true)

	if err := v.Validate(); err != nil {
		return mcp.ErrorResult(err), nil
	}
	var since time.Time
	if sinceRaw != "" {
		parsed, err := time.Parse(time.RFC3339, sinceRaw)
		if err != nil {
			return mcp.ErrorResult(fmt.Errorf("since: %w (want RFC3339)", err)), nil
		}
		since = parsed
	}

	var conds []any
	if agentID != "" {
		conds = append(conds, Match("agent_id", agentID))
	}
	if sessionID != "" {
		conds = append(conds, Match("session_id", sessionID))
	}
	if namespace != "" {
		conds = append(conds, Match("namespace", namespace))
	}
	if filePath != "" {
		conds = append(conds, Match("file_path", filePath))
	}
	if len(entryTypes) > 0 {
		conds = append(conds, FilterShould(Matches("entry_type", entryTypes)...))
	}
	if len(tags) > 0 {
		conds = append(conds, MatchAny("tags", tags))
	}

	var filter map[string]any
	if len(conds) > 0 {
		filter = FilterMust(conds...)
	}

	if sortMode == "recent" {
		results, degraded, err := cs.recentEntries(ctx, conds, since, limit, includeContent)
		if err != nil {
			return mcp.ErrorResult(fmt.Errorf("recent entries: %w", err)), nil
		}
		cs.metrics.RecallRequests.Add(1)
		out := map[string]any{
			"ok":      true,
			"results": results,
			"count":   len(results),
			"sort":    "recent",
		}
		if degraded != "" {
			out["degraded"] = true
			out["degraded_reason"] = degraded
		}
		return mcp.JSONResult(out)
	}

	cs.metrics.EmbeddingRequests.Add(1)
	vector, err := cs.embed.EmbedQuery(ctx, query)
	if err != nil {
		cs.metrics.EmbeddingErrors.Add(1)
		// Embedding provider unavailable — degrade to keyword search so recall
		// keeps working through provider outages instead of hard-failing.
		results, ferr := keywordSearch(ctx, cs.qdrant.Get(CollContext), query, filter, limit)
		if ferr != nil {
			return mcp.ErrorResult(fmt.Errorf("embedding query: %w (keyword fallback also failed: %v)", err, ferr)), nil
		}
		cs.metrics.RecallRequests.Add(1)
		return mcp.JSONResult(map[string]any{
			"ok":              true,
			"results":         results,
			"count":           len(results),
			"degraded":        true,
			"degraded_reason": "embeddings unavailable; keyword fallback",
		})
	}

	searchStart := time.Now()
	results, err := cs.qdrant.Get(CollContext).Search(ctx, vector, filter, limit, includeContent)
	if err != nil {
		return mcp.ErrorResult(fmt.Errorf("search: %w", err)), nil
	}
	cs.metrics.RecordSearchLatency(time.Since(searchStart).Microseconds())
	cs.metrics.RecallRequests.Add(1)

	return mcp.JSONResult(map[string]any{
		"ok":      true,
		"results": results,
		"count":   len(results),
	})
}

// recentEntries lists context entries newest-first, optionally only those at
// or after since. Preferred path: a Qdrant scroll with a datetime range
// filter + order_by on `timestamp` (needs the datetime payload index from
// datetimeIndexesByKind). Fallback (index missing on an older collection,
// or an older Qdrant without order_by): scroll a bounded pool with the
// caller's filters only and range-filter + sort in-process, reporting the
// degradation so the HUD can show it. Score is 0 — recency is the order,
// not a similarity.
func (cs *ContextSvc) recentEntries(ctx context.Context, conds []any, since time.Time, limit int, includeContent bool) ([]SearchResult, string, error) {
	if limit <= 0 {
		limit = 10
	}
	qc := cs.qdrant.Get(CollContext)

	indexedConds := append([]any(nil), conds...)
	if !since.IsZero() {
		indexedConds = append(indexedConds, DatetimeRange("timestamp", since, time.Time{}))
	}
	var indexedFilter map[string]any
	if len(indexedConds) > 0 {
		indexedFilter = FilterMust(indexedConds...)
	}
	entries, err := qc.ScrollOrdered(ctx, indexedFilter, limit, "timestamp")
	degraded := ""
	if err != nil {
		// Older collection without the timestamp index (or an older Qdrant):
		// a bounded pool sorted here. Newest entries can fall outside the
		// pool on a very large collection, hence the degraded flag.
		degraded = "timestamp index unavailable; in-process recency sort over a bounded pool: " + err.Error()
		var plainFilter map[string]any
		if len(conds) > 0 {
			plainFilter = FilterMust(conds...)
		}
		pool := limit * 20
		if pool < 200 {
			pool = 200
		}
		if pool > 2000 {
			pool = 2000
		}
		entries, err = qc.Scroll(ctx, plainFilter, pool)
		if err != nil {
			return nil, "", err
		}
		if !since.IsZero() {
			kept := entries[:0]
			for _, e := range entries {
				if !e.Timestamp.Before(since) {
					kept = append(kept, e)
				}
			}
			entries = kept
		}
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].Timestamp.After(entries[j].Timestamp) })
	if len(entries) > limit {
		entries = entries[:limit]
	}
	results := make([]SearchResult, 0, len(entries))
	for _, e := range entries {
		if !includeContent {
			e.Content = ""
		}
		results = append(results, SearchResult{Entry: e})
	}
	return results, degraded, nil
}

// --- Sharing ---
