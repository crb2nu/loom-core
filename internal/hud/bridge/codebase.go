package bridge

import (
	"fmt"
)

// --- Codebase DTOs ---

// CodebaseStatsInfo describes the current state of the codebase index, as
// returned by the codebase-memory `codebase_stats` tool. When Aggregate is
// true the counts summarize every indexed repo in the collection (fleet
// mode); otherwise they describe the single repo named by RepoID.
type CodebaseStatsInfo struct {
	RepoID      string         `json:"repo_id"`
	Collection  string         `json:"collection"`
	Aggregate   bool           `json:"aggregate"`
	TotalChunks int            `json:"total_chunks"`
	ByLanguage  map[string]int `json:"by_language"`
	ByChunkType map[string]int `json:"by_chunk_type"`
}

// CodebaseSearchResult represents a single search hit from the codebase index.
type CodebaseSearchResult struct {
	FilePath string  `json:"file_path"`
	Symbol   string  `json:"symbol"`
	Kind     string  `json:"kind"`
	Line     int     `json:"line"`
	Score    float64 `json:"score"`
	Snippet  string  `json:"snippet"`
}

// CodebaseIndexJob represents an in-progress or completed indexing job.
type CodebaseIndexJob struct {
	JobID  string `json:"job_id"`
	Status string `json:"status"`
}

// --- Codebase bridge methods ---

// CodebaseStats fetches the current codebase index statistics from the
// codebase-memory MCP server. It intentionally passes no repo_id: on a
// fleet server (no CODEBASE_REPO_ID configured) this yields an aggregate
// summary across every indexed repo; on a single-repo server it yields that
// repo's stats. Either way the call succeeds — the HUD codebase panel is a
// fleet view, so aggregate is the desired default.
func (a *AgentBridge) CodebaseStats() (*CodebaseStatsInfo, error) {
	raw, err := a.client.CallTool("codebase_memory__codebase_stats", map[string]any{})
	if err != nil {
		return nil, fmt.Errorf("codebase stats: %w", err)
	}
	var stats CodebaseStatsInfo
	if err := UnmarshalToolResult(raw, &stats); err != nil {
		return nil, fmt.Errorf("unmarshal codebase stats: %w", err)
	}
	return &stats, nil
}

// CodebaseSearch performs a semantic search across the codebase index.
func (a *AgentBridge) CodebaseSearch(query string, limit int) ([]CodebaseSearchResult, error) {
	if limit <= 0 {
		limit = 20
	}
	raw, err := a.client.CallTool("codebase_memory__codebase_search", map[string]any{
		"query": query,
		"limit": limit,
	})
	if err != nil {
		return nil, fmt.Errorf("codebase search: %w", err)
	}
	var result struct {
		Results []CodebaseSearchResult `json:"results"`
	}
	if err := UnmarshalToolResult(raw, &result); err != nil {
		return nil, fmt.Errorf("unmarshal codebase search: %w", err)
	}
	return result.Results, nil
}

// CodebaseTextSearch performs a text (grep-like) search across the codebase index.
func (a *AgentBridge) CodebaseTextSearch(query string, limit int) ([]CodebaseSearchResult, error) {
	if limit <= 0 {
		limit = 20
	}
	raw, err := a.client.CallTool("codebase_memory__codebase_text_search", map[string]any{
		"query": query,
		"limit": limit,
	})
	if err != nil {
		return nil, fmt.Errorf("codebase text search: %w", err)
	}
	var result struct {
		Results []CodebaseSearchResult `json:"results"`
	}
	if err := UnmarshalToolResult(raw, &result); err != nil {
		return nil, fmt.Errorf("unmarshal codebase text search: %w", err)
	}
	return result.Results, nil
}

// CodebaseIndexStart triggers a new indexing job for the given path.
func (a *AgentBridge) CodebaseIndexStart(path string) (*CodebaseIndexJob, error) {
	raw, err := a.client.CallTool("codebase_memory__codebase_index_start", map[string]any{
		"path": path,
	})
	if err != nil {
		return nil, fmt.Errorf("codebase index start: %w", err)
	}
	var job CodebaseIndexJob
	if err := UnmarshalToolResult(raw, &job); err != nil {
		return nil, fmt.Errorf("unmarshal codebase index start: %w", err)
	}
	return &job, nil
}

// CodebaseIndexPoll checks the status of an in-progress indexing job.
func (a *AgentBridge) CodebaseIndexPoll(jobID string) (*CodebaseIndexJob, error) {
	raw, err := a.client.CallTool("codebase_memory__codebase_index_poll", map[string]any{
		"job_id": jobID,
	})
	if err != nil {
		return nil, fmt.Errorf("codebase index poll: %w", err)
	}
	var job CodebaseIndexJob
	if err := UnmarshalToolResult(raw, &job); err != nil {
		return nil, fmt.Errorf("unmarshal codebase index poll: %w", err)
	}
	return &job, nil
}
