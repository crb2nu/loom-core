package bridge

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"
)

// Light session lists are the HUD's most frequent upstream call. The shuttle
// monitor (3s), the context-health monitor (5s), the fleet monitor (15s, two
// or three fetches per refresh) and the cluster mirror all ask
// agent_session_list for the same one or two projections, and each answer is
// a Qdrant scroll of up to ten thousand points on the agent-context side.
// When the store slows down every one of those calls times out on its own,
// the mux discards the reply that lands a moment later, and the next tick
// repeats the whole scroll — the 2026-09-13 daemon log shows the replies
// arriving inside the same second the 3s budget fired, ~2,300 times a day.
//
// Two mechanisms break that loop:
//
//   - one in-flight fetch per projection (singleflight) plus a short TTL, so
//     overlapping monitors share one scroll instead of stacking several onto
//     a store that is already behind;
//   - a budget that steps up after a timeout (3s → 6s → 12s) and steps back
//     down only after a quiet stretch, so a store that is merely slow gets to
//     answer instead of having its work thrown away every tick.
//
// Callers always receive their own copy of the slice; the fleet monitor
// appends to the list it gets back.
const (
	// sessionListCacheTTL is how long a fetched projection is reused. Short
	// enough that the 3s shuttle poll still refreshes every tick; long enough
	// that a fleet refresh's second and third fetch, and a mirror cycle that
	// lands beside a monitor tick, do not scroll the store again.
	sessionListCacheTTL = 2 * time.Second

	// sessionListBudgetBase is the recv budget for a light list when the
	// store is healthy. It doubles per escalation level.
	sessionListBudgetBase = 3 * time.Second

	// sessionListBudgetMaxLevel caps escalation at base<<level (12s). Beyond
	// that the store is down, not slow, and a longer wait only delays the
	// monitors' degraded signal.
	sessionListBudgetMaxLevel = 2

	// sessionListBudgetDecay is the quiet period (no timeout) after which the
	// budget steps down one level. Long enough that a store that answers in
	// 3.5s under sustained load does not oscillate between 3s (timeout) and
	// 6s (success) on alternating ticks.
	sessionListBudgetDecay = 90 * time.Second

	sessionListKindFleet  = "fleet"  // unfiltered, light projection
	sessionListKindActive = "active" // status=active, light projection
)

// sessionListBudget is the adaptive recv budget shared by every light
// session-list fetch on a bridge.
type sessionListBudget struct {
	mu         sync.Mutex
	level      int
	lastChange time.Time
	now        func() time.Time
}

func (b *sessionListBudget) clock() time.Time {
	if b.now == nil {
		return time.Now()
	}
	return b.now()
}

func budgetForLevel(level int) time.Duration {
	return sessionListBudgetBase << uint(level)
}

// current returns the budget the next fetch should use.
func (b *sessionListBudget) current() time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	return budgetForLevel(b.level)
}

// observe records a fetch outcome. It returns the new budget and true when
// the level changed, so the caller can log the transition once.
func (b *sessionListBudget) observe(err error) (time.Duration, bool) {
	now := b.clock()
	b.mu.Lock()
	defer b.mu.Unlock()
	switch {
	case err == nil:
		if b.level > 0 && now.Sub(b.lastChange) >= sessionListBudgetDecay {
			b.level--
			b.lastChange = now
			return budgetForLevel(b.level), true
		}
	case isSessionListTimeout(err):
		// Every timeout restarts the quiet period, including at the cap.
		b.lastChange = now
		if b.level < sessionListBudgetMaxLevel {
			b.level++
			return budgetForLevel(b.level), true
		}
	}
	return budgetForLevel(b.level), false
}

// isSessionListTimeout recognises the daemon's recv-budget and dial-deadline
// failures as wrapped by callWithSpan ("agent tool agent_session_list: daemon
// error (-32603): tools/call timeout during recv after 3s …", "… dial
// agent_context: context deadline exceeded"). Anything else — a tool error
// envelope from a dead store, a circuit-open bridge — is not a budget signal.
func isSessionListTimeout(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	lower := strings.ToLower(err.Error())
	return strings.Contains(lower, "timeout") || strings.Contains(lower, "deadline exceeded")
}

// lightSessionList serves one light projection through the shared
// singleflight group and TTL cache, applying the adaptive budget to the
// upstream fetch. kind names the projection; params is what the projection
// sends to agent_session_list.
func (a *AgentBridge) lightSessionList(kind string, params map[string]any) ([]SessionInfo, error) {
	key := "sessions:light:" + kind
	if sessions, ok := a.cachedSessionList(key); ok {
		return sessions, nil
	}
	v, err, _ := a.sessionListFlight.Do(key, func() (any, error) {
		// A leader that finished while we queued on the group has already
		// populated the cache; do not scroll the store again.
		if sessions, ok := a.cachedSessionList(key); ok {
			return sessions, nil
		}
		budget := a.sessionListBudget.current()
		sessions, err := a.SessionsWithParams(params, budget)
		if next, changed := a.sessionListBudget.observe(err); changed {
			if err != nil {
				slog.Default().Info("session list budget raised after recv timeout",
					"kind", kind, "from", budget, "to", next)
			} else {
				slog.Default().Info("session list budget decayed after quiet period",
					"kind", kind, "from", budget, "to", next)
			}
		}
		if err != nil {
			return nil, err
		}
		if sessions == nil {
			sessions = []SessionInfo{}
		}
		a.cache.Set(key, sessions, a.sessionListTTL)
		return sessions, nil
	})
	if err != nil {
		return nil, err
	}
	sessions, _ := v.([]SessionInfo)
	return slices.Clone(sessions), nil
}

// cachedSessionList returns a private copy of the cached projection, if any.
func (a *AgentBridge) cachedSessionList(key string) ([]SessionInfo, bool) {
	cached, ok := a.cache.Get(key)
	if !ok {
		return nil, false
	}
	sessions, ok := cached.([]SessionInfo)
	if !ok {
		return nil, false
	}
	return slices.Clone(sessions), true
}

// InvalidateSessionLists drops the cached light projections so the next
// fetch scrolls the store. Session start/end and presence changes that must
// be visible on the very next monitor tick call this.
func (a *AgentBridge) InvalidateSessionLists() {
	a.cache.Invalidate("sessions:light:" + sessionListKindFleet)
	a.cache.Invalidate("sessions:light:" + sessionListKindActive)
}
