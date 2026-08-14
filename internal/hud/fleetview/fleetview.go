// Package fleetview is the single source of truth for correlating agent
// presence with agent sessions into the unified rows rendered by the HUD
// Fleet panel, the mobile /agents endpoint, and anywhere else that needs to
// answer "which agents have an active session right now?".
//
// Design rules:
//
//  1. HasSession is a derived flag, never stored. It is true only when a
//     presence row joins to a session whose Status == "active" (by SessionID
//     first, then by AgentID).
//
//  2. The join resets any session-derived fields (HasSession, SessionID,
//     SessionStatus, SessionStartedAt, SessionAgeSeconds) on each call so
//     stale state carried in the incoming slice cannot leak through.
//
//  3. A session without a matching presence produces a synthetic
//     "session-only" row so the UI still surfaces the session. A presence
//     without a matching session produces a "presence-only" row with
//     HasSession=false.
//
//  4. Callers feed in the raw snapshots from the agent bridge; this package
//     never mutates the input slices.
package fleetview

import (
	"strings"
	"time"

	"github.com/crb2nu/loom/internal/hud/bridge"
	"github.com/crb2nu/loom/internal/visibility/contracts/presence"
)

// JoinOptions carries cross-refresh evidence a single pure Join call cannot
// derive on its own.
type JoinOptions struct {
	// LastSessionSeen maps agent_id → the last wall-clock at which that agent
	// was observed joined to an active session (or otherwise adjudicated
	// non-orphan by an authoritative join). markOrphan uses it as a grace
	// anchor: an agent whose session was seen within OrphanStaleAfter is not
	// an orphan, even if THIS call's session list happens to miss it.
	//
	// Rationale (the "day one" orphan flap): markOrphan's only in-call anchor
	// is RegisteredAt, which measures time since the agent first registered —
	// not time since the presence/session divergence began. For any agent
	// registered longer than OrphanStaleAfter ago, a single session-list miss
	// (slow bridge read, truncated projection, races with lease renewal)
	// therefore flipped it to orphan INSTANTLY, and the next successful poll
	// flipped it back — a classification flap the UI cannot hide. With this
	// evidence, orphanhood requires the divergence to PERSIST for the full
	// OrphanStaleAfter window, which is what the threshold always meant.
	LastSessionSeen map[string]time.Time
}

// Join correlates the given presence and session records into a single
// enriched slice. See the package doc for the rules it enforces.
//
// The input slices are not mutated. The returned slice contains enriched
// copies of each presence row, plus synthetic rows for sessions that have no
// matching presence.
//
// Stateless form: orphanhood is judged from this call's inputs only. Callers
// that hold cross-refresh session evidence (the fleet monitor) or re-join an
// already-adjudicated snapshot (the mobile handlers) should use JoinOpts so
// a transient session-list miss cannot flap long-registered agents straight
// to orphan.
func Join(presences []presence.PresenceInfo, sessions []bridge.SessionInfo, now time.Time) []presence.PresenceInfo {
	return JoinOpts(presences, sessions, now, JoinOptions{})
}

// JoinOpts is Join with cross-refresh evidence. See JoinOptions.
func JoinOpts(presences []presence.PresenceInfo, sessions []bridge.SessionInfo, now time.Time, opts JoinOptions) []presence.PresenceInfo {
	if now.IsZero() {
		now = time.Now()
	}

	liveByID := make(map[string]bridge.SessionInfo)
	liveByAgent := make(map[string]bridge.SessionInfo)
	for _, s := range sessions {
		if !sessionIsActive(s) {
			continue
		}
		liveByID[s.ID] = s
		if current, ok := liveByAgent[s.AgentID]; !ok || parseTime(s.StartedAt).After(parseTime(current.StartedAt)) {
			liveByAgent[s.AgentID] = s
		}
	}

	result := make([]presence.PresenceInfo, 0, len(presences)+len(liveByAgent))
	seen := make(map[string]struct{}, len(presences))
	for _, agent := range presences {
		agentID := strings.TrimSpace(agent.AgentID)
		if agentID == "" {
			continue
		}
		// Copy, then reset derived session fields so stale state cannot leak.
		row := agent
		resetSessionFields(&row)
		row.HasPresence = true
		row.Source = "presence"
		if row.AgentType == "" || strings.EqualFold(row.AgentType, "unknown") {
			row.AgentType = InferAgentType(agentID)
		}

		// Join: SessionID first (most precise), then exact AgentID, then a
		// directional workspace-base match. The base match reconciles a
		// scopeless proxy/mirror presence ("<type>-<WS_HASH>") to the scoped
		// session the CLI hooks own ("<type>-<WS_HASH>-<SCOPE>") so the
		// background-heartbeat identity stops surfacing as a bogus orphan.
		if s, ok := liveByID[agent.SessionID]; ok {
			applySession(&row, s, now)
		} else if s, ok := liveByAgent[agentID]; ok {
			applySession(&row, s, now)
		} else if s, ok := baseMatchSession(agentID, sessions); ok {
			applySession(&row, s, now)
		}

		row.HeartbeatAgeSeconds = AgeSeconds(row.LastHeartbeat, now)
		markOrphan(&row, now, opts.LastSessionSeen[agentID])
		downgradeStalePresence(&row)
		row.TelemetryStatus = TelemetryStatus(row)
		result = append(result, row)
		seen[agentID] = struct{}{}
	}

	// Synthetic rows for sessions with no matching presence.
	for _, s := range liveByAgent {
		agentID := strings.TrimSpace(s.AgentID)
		if agentID == "" {
			continue
		}
		if _, ok := seen[agentID]; ok {
			continue
		}
		row := presence.PresenceInfo{
			AgentID:       agentID,
			Status:        "active",
			AgentType:     InferAgentType(agentID),
			Description:   s.Description,
			LastHeartbeat: s.StartedAt,
			RegisteredAt:  s.StartedAt,
			Source:        "session",
		}
		applySession(&row, s, now)
		row.TelemetryStatus = TelemetryStatus(row)
		result = append(result, row)
	}

	return result
}

// baseMatchSession returns the most-recently-started ACTIVE session whose
// agent_id is a scoped extension of the given workspace-base agentID (e.g.
// presence "codex-4188162495" -> session "codex-4188162495-2303882182"). Exact
// matches are intentionally excluded — those are handled by the liveByAgent
// lookup before this is reached. Returns false when nothing extends the base.
func baseMatchSession(agentID string, sessions []bridge.SessionInfo) (bridge.SessionInfo, bool) {
	var best bridge.SessionInfo
	found := false
	for _, s := range sessions {
		if !sessionIsActive(s) {
			continue
		}
		sid := strings.TrimSpace(s.AgentID)
		if sid == agentID || !IsBaseOf(agentID, sid) {
			continue
		}
		if !found || parseTime(s.StartedAt).After(parseTime(best.StartedAt)) {
			best = s
			found = true
		}
	}
	return best, found
}

// LivePresenceStaleAfter is the heartbeat-age past which an active/idle
// presence row is no longer counted as "live work", regardless of what the
// upstream MCP presence registry reports as its Status.
//
// Background: agent_presence_list keeps a row in status="active" until
// roughly 10 minutes after its last heartbeat (the registry's own age
// horizon), and a vendor CLI keepalive process may keep firing heartbeats
// even after its session has ended. The Fleet view's job is to answer
// "which agents are doing work right now," not "which agents have ever
// heartbeated in the last 10 minutes" — heartbeats older than this
// threshold are downgraded to "offline" so the live counter and the
// visible row list both reflect actual active work.
//
// 90s is one full heartbeat interval plus a generous retry window: daemon
// heartbeats fire on a 30s cadence (see CLI hook config) and the MCP
// transport has a 5s call cap (see PresenceHeartbeat in
// internal/hud/bridge/agent_session.go), so a live agent will rarely
// report >60s of heartbeat age and never >90s without something being
// wrong. Synthetic session-only rows (Source="session", no presence at
// all) are exempt because they have no heartbeat to age out.
//
// This is the single source of truth for per-agent staleness: the web
// frontend stores mirror it (90s staleAfter), the downgrade in Join keys
// off it, and TelemetryStatus's "stale" label uses the same horizon so
// the web HUD and the mobile API can never disagree about whether an
// agent is live. The full threshold ladder is
// LivePresenceStaleAfter (90s) < OrphanStaleAfter (120s) < the monitor's
// reap horizons (10m).
const LivePresenceStaleAfter = 90 * time.Second

// downgradeStalePresence flips an active/idle presence row to "offline"
// when its heartbeat has aged past LivePresenceStaleAfter or it has been
// flagged as an orphan. It runs inside Join after markOrphan and before
// TelemetryStatus is derived, so Status and TelemetryStatus in the same
// row always agree — callers must not re-derive staleness with their own
// thresholds. Synthetic session-only rows (HasPresence=false) are exempt:
// they have no heartbeat clock by construction.
func downgradeStalePresence(row *presence.PresenceInfo) {
	if row == nil || !row.HasPresence {
		return
	}
	if row.Status != "active" && row.Status != "idle" {
		return
	}
	tooStale := row.HeartbeatAgeSeconds >= int(LivePresenceStaleAfter.Seconds())
	if tooStale || row.IsOrphan {
		row.Status = "offline"
	}
}

// OrphanStaleAfter is the age past which a heartbeating presence with no
// matching active session is flagged as an orphan. Short enough to catch
// real divergence, long enough to avoid false positives during normal
// session-start bootstrap (vendor CLIs register presence, then call
// session-start; the window between those is typically <1s). 120s leaves
// generous room for a flaky daemon retry. Intentionally longer than
// LivePresenceStaleAfter (orphanhood measures registered-without-session
// divergence, not heartbeat freshness) and far shorter than the monitor's
// 10m reap horizons.
const OrphanStaleAfter = 120 * time.Second

// markOrphan sets the derived IsOrphan / OrphanAgeSeconds fields on an
// enriched presence row. An orphan is a row with presence evidence but no
// joined active session where that DIVERGENCE has persisted past
// OrphanStaleAfter. Synthetic session-only rows (Source="session") can never
// be orphans by definition.
//
// lastSessionSeen (zero when the caller has no evidence) is the last time
// this agent was observed with an active session; a divergence younger than
// OrphanStaleAfter relative to that moment is bootstrap noise or a transient
// session-list miss, not orphanhood. Without it, the only anchor is
// RegisteredAt — correct for an agent that registered and NEVER produced a
// session, but instantly wrong for a long-registered agent whose session
// dropped out of a single poll.
func markOrphan(row *presence.PresenceInfo, now time.Time, lastSessionSeen time.Time) {
	if row == nil {
		return
	}
	row.IsOrphan = false
	row.OrphanAgeSeconds = 0
	if !row.HasPresence || row.HasSession {
		return
	}
	// Grace: recently seen with a session ⇒ the sessionless reading has not
	// persisted long enough to mean anything yet.
	if !lastSessionSeen.IsZero() && now.Sub(lastSessionSeen) < OrphanStaleAfter {
		return
	}
	// Divergence clock: how long has this agent been sessionless? The best
	// in-call anchor is RegisteredAt (registered without ever joining);
	// cross-refresh evidence supersedes it when newer. Fall back to
	// LastHeartbeat when RegisteredAt is absent, e.g. in older fixtures.
	anchor := parseTime(row.RegisteredAt)
	if anchor.IsZero() {
		anchor = parseTime(row.LastHeartbeat)
	}
	if !lastSessionSeen.IsZero() && lastSessionSeen.After(anchor) {
		anchor = lastSessionSeen
	}
	if anchor.IsZero() {
		return
	}
	age := now.Sub(anchor)
	if age < OrphanStaleAfter {
		return
	}
	row.IsOrphan = true
	row.OrphanAgeSeconds = int(age.Seconds())
}

// SessionEvidence derives a LastSessionSeen map from rows that already went
// through an authoritative (monitor) join, stamped at that join's snapshot
// time. Rows joined to a session obviously count; presence rows the join
// left non-orphan ALSO count, because that verdict may itself rest on grace
// evidence the re-joining caller doesn't hold — without propagating it, a
// mobile re-join of the same snapshot would re-flag the very flap the
// monitor just suppressed. Rows already adjudicated orphan contribute
// nothing, so re-joins reproduce that verdict identically.
func SessionEvidence(rows []presence.PresenceInfo, at time.Time) map[string]time.Time {
	if len(rows) == 0 {
		return nil
	}
	out := make(map[string]time.Time, len(rows))
	for _, row := range rows {
		agentID := strings.TrimSpace(row.AgentID)
		if agentID == "" {
			continue
		}
		if row.HasSession || (row.HasPresence && !row.IsOrphan) {
			out[agentID] = at
		}
	}
	return out
}

// applySession writes session-derived fields onto an enriched presence row.
// Must be called only when the session is active; callers filter first.
func applySession(row *presence.PresenceInfo, s bridge.SessionInfo, now time.Time) {
	if row == nil {
		return
	}
	row.SessionID = s.ID
	row.HasSession = true
	row.SessionStatus = s.Status
	row.SessionStartedAt = s.StartedAt
	row.SessionAgeSeconds = AgeSeconds(s.StartedAt, now)
	if row.Description == "" {
		row.Description = s.Description
	}
	if row.Source == "presence" {
		row.Source = "presence+session"
	}
}

// resetSessionFields clears all session-derived state on a presence row so a
// fresh join cannot inherit stale flags from a prior computation.
func resetSessionFields(row *presence.PresenceInfo) {
	if row == nil {
		return
	}
	row.HasSession = false
	row.SessionStatus = ""
	row.SessionStartedAt = ""
	row.SessionAgeSeconds = 0
	row.IsOrphan = false
	row.OrphanAgeSeconds = 0
	// SessionID stays untouched here: it is an identity hint produced by the
	// presence layer (from registration or heartbeat), and callers further
	// down may use it to match against sessions fetched out-of-band. The
	// HasSession flag is what drives UI correlation, and that is gated on a
	// real active session match.
}

// sessionIsActive returns true when the session status (case-insensitive,
// trimmed) equals "active" and the agent_id is non-empty.
func sessionIsActive(s bridge.SessionInfo) bool {
	if strings.TrimSpace(s.AgentID) == "" {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(s.Status), "active")
}

// InferAgentType maps a well-formed agent ID to an agent type. Used as a
// fallback when the presence/session record doesn't carry one.
func InferAgentType(agentID string) string {
	id := strings.ToLower(strings.TrimSpace(agentID))
	if id == "" {
		return "unknown"
	}
	switch {
	case strings.HasPrefix(id, "claude"):
		return "claude-code"
	case strings.HasPrefix(id, "gemini"):
		return "gemini-cli"
	case strings.HasPrefix(id, "codex"), strings.HasPrefix(id, "zed"), strings.HasPrefix(id, "proxy"):
		return "codex"
	case strings.HasPrefix(id, "copilot"):
		return "copilot"
	case strings.HasPrefix(id, "kilocode"):
		return "kilocode"
	}
	if i := strings.IndexAny(id, "-_"); i > 0 {
		return id[:i]
	}
	return id
}

// AgeSeconds returns the number of seconds between parseable RFC3339 time
// `raw` and `now`, clamping to 0 for zero/unparseable/future values.
func AgeSeconds(raw string, now time.Time) int {
	t := parseTime(raw)
	if t.IsZero() || now.Before(t) {
		return 0
	}
	return int(now.Sub(t).Seconds())
}

// TelemetryStatus derives a rollup label from the enriched presence row. It
// is intentionally derived so two callers computing it over the same row
// always agree.
//
// The stale horizon is LivePresenceStaleAfter — the same threshold the
// downgrade in Join uses to flip aged active/idle rows to "offline". Inside
// Join the downgrade runs first, so those rows surface as "offline" here and
// the "stale" branch only fires for rows with an unusual Status (or for
// callers deriving the label outside Join) — defense in depth, kept aligned
// so no surface can report "live" past the 90s horizon.
func TelemetryStatus(row presence.PresenceInfo) string {
	status := strings.ToLower(strings.TrimSpace(row.Status))
	switch {
	case row.HasSession && !row.HasPresence:
		return "session_only"
	case status == "offline":
		return "offline"
	case row.HasPresence && row.HeartbeatAgeSeconds >= int(LivePresenceStaleAfter.Seconds()):
		return "stale"
	case status == "idle":
		return "idle"
	case status == "active":
		return "live"
	default:
		return "unknown"
	}
}

func parseTime(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return t
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t
	}
	return time.Time{}
}
