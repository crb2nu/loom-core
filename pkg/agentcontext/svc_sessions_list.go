package agentcontext

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"gitlab.flexinfer.ai/libs/mcp-go"

	"github.com/crb2nu/loom/pkg/validate"
)

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
		// For sessions not in memory with 0 entry count, recompute from
		// the context collection (covers HUD-restart data loss).
		if !inMem && sess.EntryCount == 0 && ss.countContextEntries != nil {
			entries, tokens := ss.countContextEntries(ctx, sess.ID)
			if entries > 0 {
				sess.EntryCount = entries
				sess.TotalTokens = tokens
			}
		}
		sessions = append(sessions, *sess)
	}

	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].StartedAt.After(sessions[j].StartedAt)
	})

	if len(sessions) > limit {
		sessions = sessions[:limit]
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
