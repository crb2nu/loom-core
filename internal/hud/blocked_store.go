package hud

import (
	"encoding/json"
	"sort"
	"sync"
	"time"
)

// blockedTTL bounds how long a session stays "blocked" without a matching
// unblock. The flightdeck bridge emits agent.unblocked on the resolving
// PostToolUse/UserPromptSubmit/Stop/SessionEnd, so blocks normally clear
// promptly; the TTL is a safety net for a missed unblock (e.g. the bridge
// restarted mid-wait), so a stale "blocked" never pins the HUD badge forever.
const blockedTTL = 30 * time.Minute

// BlockedSession is one agent session waiting on a human (a permission stall),
// as derived by the flightdeck bridge and pushed via agent.blocked events.
type BlockedSession struct {
	SessionID string
	AgentID   string
	Reason    string
	ToolName  string
	Cwd       string
	Since     time.Time
}

// blockedStore tracks the set of currently-blocked sessions, folded from the
// agent.blocked / agent.unblocked daemon events. It is read by the mobile
// dashboard handler and written from the event-ingest path, so it is
// mutex-guarded.
type blockedStore struct {
	mu  sync.Mutex
	m   map[string]BlockedSession // keyed by session_id
	ttl time.Duration
}

func newBlockedStore() *blockedStore {
	return &blockedStore{m: make(map[string]BlockedSession), ttl: blockedTTL}
}

// blockedFromEvent parses an agent.blocked payload (as emitted by the
// flightdeck bridge: agent_id, session_id, reason, tool_name?, cwd?, since)
// into a BlockedSession. The `since` stamp wins when present; otherwise the
// event arrival time ts is used.
func blockedFromEvent(ts time.Time, data json.RawMessage) BlockedSession {
	sid, _ := jsonStringField(data, "session_id")
	agentID, _ := jsonStringField(data, "agent_id")
	reason, _ := jsonStringField(data, "reason")
	tool, _ := jsonStringField(data, "tool_name")
	cwd, _ := jsonStringField(data, "cwd")
	if reason == "" {
		reason = "permission"
	}
	since := ts
	if s, ok := jsonStringField(data, "since"); ok {
		if parsed, err := time.Parse(time.RFC3339Nano, s); err == nil {
			since = parsed
		}
	}
	return BlockedSession{
		SessionID: sid,
		AgentID:   agentID,
		Reason:    reason,
		ToolName:  tool,
		Cwd:       cwd,
		Since:     since,
	}
}

// block records (or refreshes) a blocked session. A zero Since is stamped now.
func (s *blockedStore) block(b BlockedSession, now time.Time) {
	if s == nil || b.SessionID == "" {
		return
	}
	if b.Since.IsZero() {
		b.Since = now
	}
	s.mu.Lock()
	s.m[b.SessionID] = b
	s.mu.Unlock()
}

// unblock clears a session's blocked state (no-op if it was not blocked).
func (s *blockedStore) unblock(sessionID string) {
	if s == nil || sessionID == "" {
		return
	}
	s.mu.Lock()
	delete(s.m, sessionID)
	s.mu.Unlock()
}

// list returns the currently-blocked sessions, oldest wait first, pruning any
// that have exceeded the TTL (a defensively-dropped missed-unblock).
func (s *blockedStore) list(now time.Time) []BlockedSession {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]BlockedSession, 0, len(s.m))
	for id, b := range s.m {
		if now.Sub(b.Since) > s.ttl {
			delete(s.m, id)
			continue
		}
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Since.Equal(out[j].Since) {
			return out[i].Since.Before(out[j].Since) // longest wait first
		}
		return out[i].SessionID < out[j].SessionID
	})
	return out
}
