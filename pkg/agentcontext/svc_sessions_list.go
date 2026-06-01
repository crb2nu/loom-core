package agentcontext

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"gitlab.flexinfer.ai/libs/mcp-go"

	"github.com/crb2nu/loom/pkg/validate"
)

// sessionLite is the trimmed projection returned when a caller passes
// light=true to agent_session_list. It carries only the identity, timing,
// status, and token fields the HUD fleet view needs and drops the heavier
// metadata (description, working_dir, pipeline_ref, last_summary_at).
//
// The fleet monitor polls agent_session_list on a ~5s cadence with no status
// filter. Once thousands of ended/summarized sessions accumulate, the full
// payload exceeds the daemon's 3s tools/call recv budget and every refresh
// times out (see project_hud_no_agents_session_list_timeout). The light
// projection plus the bounded recompute in List keep that call cheap at the
// source. Field json tags match the wire format bridge.SessionInfo decodes.
type sessionLite struct {
	ID              string     `json:"id"`
	AgentID         string     `json:"agent_id"`
	Namespace       string     `json:"namespace,omitempty"`
	Project         string     `json:"project,omitempty"`
	StartedAt       time.Time  `json:"started_at"`
	EndedAt         *time.Time `json:"ended_at,omitempty"`
	Status          string     `json:"status"`
	EntryCount      int        `json:"entry_count"`
	TotalTokens     int        `json:"total_tokens"`
	ParentSessionID string     `json:"parent_session_id,omitempty"`
	RootSessionID   string     `json:"root_session_id,omitempty"`
}

func toSessionLite(s Session) sessionLite {
	return sessionLite{
		ID:              s.ID,
		AgentID:         s.AgentID,
		Namespace:       s.Namespace,
		Project:         s.Project,
		StartedAt:       s.StartedAt,
		EndedAt:         s.EndedAt,
		Status:          s.Status,
		EntryCount:      s.EntryCount,
		TotalTokens:     s.TotalTokens,
		ParentSessionID: s.ParentSessionID,
		RootSessionID:   s.RootSessionID,
	}
}

// sessionListScrollCap bounds how many session points we will scroll from
// Qdrant before sorting and truncating to the caller's limit. Qdrant scroll
// returns points in internal point-ID order, not by StartedAt — so applying
// the caller's limit at the scroll layer would silently drop the most
// recently started sessions (the HUD's Live Sessions panel hit exactly this
// bug: agent_session_list(limit=1000) came back with 1000 ended rows while
// a dozen active sessions were running, because the scroll's ID-ordered
// truncation never reached them). 10000 is well above the realistic working
// set per backend (sessions are pruned at 72h via agent_session_prune) and
// matches the cap used by other "fetch everything then filter" callers
// like workflow_persist.go.
const sessionListScrollCap = 10000

// List returns sessions matching optional filters.
func (ss *SessionSvc) List(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	v := validate.NewArgs(args)
	agentID := strings.TrimSpace(v.String("agent_id", ""))
	namespace := strings.TrimSpace(v.String("namespace", ""))
	status := strings.TrimSpace(v.String("status", ""))
	limit := v.Int("limit", 20)
	light := v.Bool("light", false)

	if err := v.Validate(); err != nil {
		return mcp.ErrorResult(err), nil
	}

	var conds []any
	if agentID != "" {
		conds = append(conds, Match("agent_id", agentID))
	}
	if namespace != "" {
		conds = append(conds, Match("namespace", namespace))
	}
	if status != "" {
		conds = append(conds, Match("status", status))
	}

	var filter map[string]any
	if len(conds) > 0 {
		filter = FilterMust(conds...)
	}

	points, err := ss.qdrant.ScrollPoints(ctx, filter, sessionListScrollCap, false)
	if err != nil {
		return mcp.ErrorResult(fmt.Errorf("list sessions: %w", err)), nil
	}

	sessions := make([]Session, 0, len(points))
	for _, p := range points {
		sess, err := PayloadToSession(p.Payload)
		if err != nil || sess == nil {
			continue
		}
		// Overlay in-memory stats for active sessions (Qdrant has stale 0s
		// because stats are only persisted on session end).
		ss.mu.RLock()
		live, inMem := ss.sessions[sess.ID]
		if inMem {
			sess.EntryCount = live.EntryCount
			sess.TotalTokens = live.TotalTokens
		}
		ss.mu.RUnlock()
		sessions = append(sessions, *sess)
	}

	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].StartedAt.After(sessions[j].StartedAt)
	})

	if len(sessions) > limit {
		sessions = sessions[:limit]
	}

	// Recompute entry/token stats for ended sessions whose persisted counts
	// are 0 (covers HUD-restart data loss). This is deliberately done AFTER
	// the sort+truncate so it runs at most `limit` times, not once per
	// scrolled point. Previously this lived in the scroll loop above and
	// fired for every one of up to sessionListScrollCap points — each
	// EntryCount==0 ended session triggered its own Qdrant scroll, so a large
	// history of short hook-driven sessions turned a single agent_session_list
	// into thousands of serial round-trips and blew the daemon's 3s recv
	// budget. The light fleet path skips the recompute entirely: the fleet
	// view counts live work and tolerates the persisted/in-memory stats.
	if !light && ss.countContextEntries != nil {
		for i := range sessions {
			s := &sessions[i]
			ss.mu.RLock()
			_, inMem := ss.sessions[s.ID]
			ss.mu.RUnlock()
			if inMem || s.EntryCount != 0 {
				continue
			}
			entries, tokens := ss.countContextEntries(ctx, s.ID)
			if entries > 0 {
				s.EntryCount = entries
				s.TotalTokens = tokens
			}
		}
	}

	if light {
		lite := make([]sessionLite, len(sessions))
		for i, s := range sessions {
			lite[i] = toSessionLite(s)
		}
		return mcp.JSONResult(map[string]any{
			"ok":       true,
			"sessions": lite,
			"count":    len(lite),
		})
	}

	return mcp.JSONResult(map[string]any{
		"ok":       true,
		"sessions": sessions,
		"count":    len(sessions),
	})
}

// Delete removes a session by ID.
func (ss *SessionSvc) Delete(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	v := validate.NewArgs(args)
	sessionID := v.Required("session_id")

	if err := v.Validate(); err != nil {
		return mcp.ErrorResult(err), nil
	}

	ss.mu.Lock()
	_, existed := ss.sessions[sessionID]
	delete(ss.sessions, sessionID)
	ss.mu.Unlock()

	if ss.qdrant != nil {
		if err := ss.qdrant.Delete(ctx, []string{sessionID}); err != nil {
			return mcp.ErrorResult(fmt.Errorf("delete session from Qdrant: %w", err)), nil
		}
	}

	return mcp.JSONResult(map[string]any{
		"ok":         true,
		"session_id": sessionID,
		"existed":    existed,
	})
}
