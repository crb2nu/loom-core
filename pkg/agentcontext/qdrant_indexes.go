// qdrant_indexes.go -- Keyword payload indexes for agent-context collections.
//
// Without these indexes, every filtered list/scroll query (e.g. by session_id,
// status, agent_id) is a brute-force scan over the entire collection. Once a
// collection grows past a few hundred points the daemon's call lock starts
// timing out, which wedges the entire fleet (heartbeats, presence, task list).
//
// We register the canonical filter fields per collection kind here so that
// EnsureCollection can apply them idempotently on every startup.
package agentcontext

import (
	"context"
	"fmt"
	"net/http"
)

// keywordIndexesByKind lists the payload fields that need keyword indexes per
// collection kind. The keys are Coll* constants from qdrant_registry.go.
var keywordIndexesByKind = map[string][]string{
	CollContext:    {"session_id", "agent_id", "entry_type", "visibility", "file_path", "namespace", "project"},
	CollSessions:   {"agent_id", "namespace", "status"},
	CollTasks:      {"session_id", "agent_id", "status", "namespace", "project", "file_path"},
	CollWorkflows:  {"status", "agent_id", "session_id"},
	CollHandoffs:   {"target_agent_id", "agent_id", "status"},
	CollMemory:     {"agent_id", "namespace", "tier"},
	CollPresence:   {"agent_id", "status"},
	CollFileClaims: {"agent_id", "status", "file_path"},
	CollPlans:      {"id", "project", "namespace", "status", "slug", "created_by"},
}

// datetimeIndexesByKind lists payload fields stored as RFC3339 strings that
// need range-filter support. Sessions: started_at/ended_at let the reaper
// query "older than X" via Qdrant's range filter instead of post-filtering
// in Go after a full scroll.
var datetimeIndexesByKind = map[string][]string{
	CollSessions: {"started_at", "ended_at"},
}

// EnsureKeywordIndex idempotently creates a keyword payload index on the named
// field. Qdrant's PUT /collections/{name}/index is idempotent: it succeeds if
// the index already exists with the same schema.
func (c *QdrantClient) EnsureKeywordIndex(ctx context.Context, field string) error {
	body := map[string]any{
		"field_name":   field,
		"field_schema": "keyword",
	}
	if err := c.doJSON(ctx, http.MethodPut, "/collections/"+c.collection+"/index", body, nil); err != nil {
		return fmt.Errorf("ensure index %s.%s: %w", c.collection, field, err)
	}
	return nil
}

// EnsureKeywordIndexes applies a list of keyword payload indexes. Stops at the
// first error.
func (c *QdrantClient) EnsureKeywordIndexes(ctx context.Context, fields ...string) error {
	for _, f := range fields {
		if err := c.EnsureKeywordIndex(ctx, f); err != nil {
			return err
		}
	}
	return nil
}

// EnsureDatetimeIndex idempotently creates a datetime payload index.
// Required for range filters like {"key":"started_at","range":{"lt":"..."}}.
func (c *QdrantClient) EnsureDatetimeIndex(ctx context.Context, field string) error {
	body := map[string]any{
		"field_name":   field,
		"field_schema": "datetime",
	}
	if err := c.doJSON(ctx, http.MethodPut, "/collections/"+c.collection+"/index", body, nil); err != nil {
		return fmt.Errorf("ensure datetime index %s.%s: %w", c.collection, field, err)
	}
	return nil
}

// EnsureDatetimeIndexes applies a list of datetime payload indexes.
func (c *QdrantClient) EnsureDatetimeIndexes(ctx context.Context, fields ...string) error {
	for _, f := range fields {
		if err := c.EnsureDatetimeIndex(ctx, f); err != nil {
			return err
		}
	}
	return nil
}

// ensureRegisteredIndexes applies the canonical keyword + datetime index sets
// for the client's kind. No-op when kind is empty or unregistered.
func (c *QdrantClient) ensureRegisteredIndexes(ctx context.Context) error {
	if c == nil || c.kind == "" {
		return nil
	}
	if fields, ok := keywordIndexesByKind[c.kind]; ok && len(fields) > 0 {
		if err := c.EnsureKeywordIndexes(ctx, fields...); err != nil {
			return err
		}
	}
	if fields, ok := datetimeIndexesByKind[c.kind]; ok && len(fields) > 0 {
		if err := c.EnsureDatetimeIndexes(ctx, fields...); err != nil {
			return err
		}
	}
	return nil
}
