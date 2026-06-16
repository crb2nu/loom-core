package fleetview

import "strings"

// Agent-id identity helpers, ported from the HUD frontend
// (internal/hud/frontend/src/lib/utils/agents.ts) so the Go server side
// (mobile API) groups and dedupes agents the same way the web client does.
//
// Lifecycle hooks mint a distinct agent_id per (workspace, conversation):
// `<base>-<WS_HASH>[-<SESSION_SCOPE>]` (pkg/generator/configs_hooks.go), where
// the base prefix (`claude-code`, `codex`, `gemini-cli`) is never all-numeric,
// WS_HASH is the cksum of the git workspace root (stable per repo/worktree), and
// SESSION_SCOPE is the cksum of the conversation/session id (stable for one chat).

// workspaceAnchoredBases lists bases whose ids are workspace-anchored rather than
// conversation-scoped. Codex's notify hook mints `codex-<WS_HASH>` (one app per
// workspace) and the fleet also sees scoped variants for the same app, so its
// "conversation" is the workspace; Claude/Gemini carry a stable SESSION_SCOPE
// across repos and are keyed by scope. Mirrors WORKSPACE_ANCHORED_BASES in
// agents.ts.
var workspaceAnchoredBases = map[string]struct{}{"codex": {}}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// AgentBase returns the non-numeric prefix of an agent id (everything before the
// first all-numeric segment): `codex-1713039686-2004540290` -> `codex`,
// `claude-code-552019522` -> `claude-code`, `codex-7b28` -> `codex-7b28`.
func AgentBase(agentID string) string {
	parts := strings.Split(agentID, "-")
	for i, p := range parts {
		if isAllDigits(p) {
			return strings.Join(parts[:i], "-")
		}
	}
	return agentID
}

// IsWorkspaceAnchored reports whether an agent id's base is workspace-anchored
// (currently only codex). Such ids fold scopeless + scoped variants together.
func IsWorkspaceAnchored(agentID string) bool {
	_, ok := workspaceAnchoredBases[AgentBase(agentID)]
	return ok
}

// RootAgentID collapses a per-(workspace,conversation) agent_id down to its
// stable workspace-scoped identity: everything up to and including the FIRST
// all-numeric segment (the WS_HASH). Mirrors rootAgentId() in agents.ts.
//
//	claude-code-552019522-2804496862 -> claude-code-552019522
//	codex-4188162495-2303882182      -> codex-4188162495
//	codex-7b28                        -> codex-7b28 (no WS_HASH)
func RootAgentID(agentID string) string {
	id := strings.TrimSpace(agentID)
	if id == "" {
		return ""
	}
	parts := strings.Split(id, "-")
	for i, p := range parts {
		if isAllDigits(p) {
			return strings.Join(parts[:i+1], "-")
		}
	}
	return id
}

// ConversationID collapses a per-(workspace,conversation) agent_id down to the
// conversation it belongs to. For conversation-scoped vendors (claude/gemini) it
// keeps the SESSION_SCOPE and drops the WS_HASH, so one chat that moved across
// repos groups as a single identity. For workspace-anchored vendors (codex) it
// keeps the WS_HASH, folding scopeless + scoped ids for one app. Mirrors
// conversationId() in agents.ts.
//
//	claude-code-3749726816-1105899468 -> claude-code-1105899468
//	claude-code-401508988-1105899468  -> claude-code-1105899468 (same chat)
//	codex-401508988-2992486099        -> codex-401508988 (workspace-anchored)
//	codex-401508988                   -> codex-401508988 (folds with the above)
func ConversationID(agentID string) string {
	id := strings.TrimSpace(agentID)
	if id == "" {
		return ""
	}
	parts := strings.Split(id, "-")
	wsIdx := -1
	for i, p := range parts {
		if isAllDigits(p) {
			wsIdx = i
			break
		}
	}
	if wsIdx < 0 {
		return id // no numeric suffix — already a conversation root
	}
	base := strings.Join(parts[:wsIdx], "-")
	if _, anchored := workspaceAnchoredBases[base]; anchored {
		return strings.Join(parts[:wsIdx+1], "-") // base-WSHASH
	}
	// SESSION_SCOPE is the last all-numeric segment AFTER the WS_HASH.
	scope := ""
	for i := len(parts) - 1; i > wsIdx; i-- {
		if isAllDigits(parts[i]) {
			scope = parts[i]
			break
		}
	}
	if scope == "" {
		return id // base-WSHASH only, no scope
	}
	if base == "" {
		return scope
	}
	return base + "-" + scope
}
