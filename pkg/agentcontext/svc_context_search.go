package agentcontext

import (
	"context"
	"fmt"
	"time"

	"gitlab.flexinfer.ai/libs/mcp-go"

	"github.com/crb2nu/loom/pkg/validate"
)

// --- Search & Recall ---

func (cs *ContextSvc) Search(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	v := validate.NewArgs(args)
	query := v.Required("query")
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

// --- Sharing ---
