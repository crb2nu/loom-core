package monitor

import (
	"context"
	"log/slog"
	"time"

	"github.com/crb2nu/loom/internal/hud/bridge"
)

// CodebaseSnapshot captures the current state of the codebase index. When
// Aggregate is true the counts summarize every indexed repo in the collection
// (fleet mode, the HUD panel's default); otherwise they describe the single
// repo named by RepoID.
type CodebaseSnapshot struct {
	RepoID      string         `json:"repo_id"`
	Collection  string         `json:"collection"`
	Aggregate   bool           `json:"aggregate"`
	TotalChunks int            `json:"total_chunks"`
	ByLanguage  map[string]int `json:"by_language"`
	ByChunkType map[string]int `json:"by_chunk_type"`
	UpdatedAt   time.Time      `json:"updated_at"`
}

// CodebaseMonitor tracks codebase index statistics by periodically polling
// the codebase-memory MCP server through the agent bridge.
type CodebaseMonitor struct {
	BaseMonitor[CodebaseSnapshot]
	agent *bridge.AgentBridge
}

// NewCodebaseMonitor creates a CodebaseMonitor backed by the given agent bridge.
func NewCodebaseMonitor(agent *bridge.AgentBridge, logger *slog.Logger) *CodebaseMonitor {
	m := &CodebaseMonitor{agent: agent}
	m.InitBase(logger, nil, "codebase-monitor")
	return m
}

// Start begins the background polling goroutine at the given interval.
func (m *CodebaseMonitor) Start(interval time.Duration) {
	m.BaseMonitor.Start(interval, m.refresh)
}

// Status returns the current cached codebase snapshot.
func (m *CodebaseMonitor) Status() CodebaseSnapshot {
	return m.Snapshot()
}

// refresh fetches the latest codebase stats from the agent bridge.
func (m *CodebaseMonitor) refresh(_ context.Context) (CodebaseSnapshot, error) {
	stats, err := m.agent.CodebaseStats()
	if err != nil {
		return CodebaseSnapshot{}, err
	}
	return CodebaseSnapshot{
		RepoID:      stats.RepoID,
		Collection:  stats.Collection,
		Aggregate:   stats.Aggregate,
		TotalChunks: stats.TotalChunks,
		ByLanguage:  stats.ByLanguage,
		ByChunkType: stats.ByChunkType,
		UpdatedAt:   time.Now(),
	}, nil
}

// Refresh forces an immediate refresh. Exposed for external callers.
func (m *CodebaseMonitor) Refresh() error {
	snap, err := m.refresh(context.Background())
	if err != nil {
		return err
	}
	m.Update(snap)
	return nil
}
