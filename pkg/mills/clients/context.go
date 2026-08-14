package clients

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/crb2nu/loom/pkg/mills/pipeline"
)

// defaultContextContentLimit bounds the content of a single recorded entry.
// The operator's decisions are short structured records; capping them keeps
// one pathological failure record (a giant log tail) from ballooning the
// session's token count and the handoff package built from it.
const defaultContextContentLimit = 2048

// ContextRecorder writes single entries into the operator's long-lived
// agent-context session via agent_context_add on mcp-agent-context, called
// through MCPHubClient.
//
// Why this exists: the operator has held an agent-context session since boot
// (agent_session_start) and stamps its id on every handoff and worktree
// allocation, but never wrote a single entry to it. Handoffs therefore shipped
// with entry_count: 0 — a session-shaped envelope with nothing inside. This
// client is the write path that makes the operator a real citizen of the shared
// context store: escalation decisions and merge findings land in the same
// session that handoffs package.
//
// Session model mirrors HandoffClient: SessionID is the boot-time value and
// SessionIDFunc (when set) supplies the CURRENT id, so a session re-established
// after a hub outage is picked up without rebuilding the recorder.
type ContextRecorder struct {
	Hub           *MCPHubClient
	ServerName    string
	SessionID     string
	SessionIDFunc func() string
	// Durability routes the entry inside agent-context: "session" (default)
	// keeps it in the context store, "persistent" promotes to long-term memory.
	Durability string
	// Visibility defaults to "shared" — operator decisions are fleet-wide
	// signal, not private notes.
	Visibility string
	// MaxContentBytes caps entry content. Default defaultContextContentLimit.
	MaxContentBytes int
}

// NewContextRecorder returns a ContextRecorder bound to hub. sessionID is the
// operator's boot-time agent-context session; set SessionIDFunc afterwards so
// a re-established session is picked up live.
func NewContextRecorder(hub *MCPHubClient, sessionID string) *ContextRecorder {
	return &ContextRecorder{
		Hub:             hub,
		ServerName:      AgentContextServerName,
		SessionID:       sessionID,
		Durability:      "session",
		Visibility:      "shared",
		MaxContentBytes: defaultContextContentLimit,
	}
}

// contextAddResponse mirrors the agent_context_add payload. Only ok/count are
// consulted; entry_ids is kept for diagnostics.
type contextAddResponse struct {
	OK       bool     `json:"ok"`
	Count    int      `json:"count"`
	EntryIDs []string `json:"entry_ids"`
}

// AddContextEntry records exactly one entry in the operator's session.
//
// sessionID may be empty, which means "the recorder's current operator
// session" — the escalation and merge call sites do not carry a session id of
// their own, they just want the entry to land wherever the operator's handoffs
// are packaged from. Pass an explicit id only to target a different session.
//
// Callers treat a returned error as advisory: recording context must never
// fail an escalation or a merge.
func (c *ContextRecorder) AddContextEntry(ctx context.Context, sessionID, entryType, title, content string, tags []string) error {
	if c == nil || c.Hub == nil {
		return errors.New("context recorder: client not configured")
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		sessionID = c.sessionID()
	}
	if sessionID == "" {
		return errors.New("context recorder: session id required (start an operator session at boot)")
	}
	entryType = strings.TrimSpace(entryType)
	if entryType == "" {
		return errors.New("context recorder: entry_type required")
	}
	title = strings.TrimSpace(title)
	if title == "" {
		return errors.New("context recorder: title required")
	}

	entry := map[string]any{
		"entry_type": entryType,
		"title":      title,
		"content":    capBytes(content, c.contentLimit()),
		"durability": orDefault(c.Durability, "session"),
		"visibility": orDefault(c.Visibility, "shared"),
	}
	if len(tags) > 0 {
		entry["tags"] = tags
	}
	args := map[string]any{
		"session_id": sessionID,
		"entries":    []any{entry},
	}

	body, err := c.Hub.CallTool(ctx, c.serverName(), "agent_context_add", args)
	if err != nil && body == "" {
		return fmt.Errorf("context recorder: add: %w", err)
	}
	var parsed contextAddResponse
	if derr := decodeListBody(body, &parsed); derr != nil {
		if err != nil {
			return fmt.Errorf("context recorder: add: %w; raw=%q", err, truncateBody(body, 240))
		}
		return fmt.Errorf("context recorder: decode: %w; raw=%q", derr, truncateBody(body, 240))
	}
	if !parsed.OK {
		return fmt.Errorf("context recorder: service reported failure: %s", truncateBody(body, 240))
	}
	return nil
}

func (c *ContextRecorder) sessionID() string {
	if c == nil {
		return ""
	}
	if c.SessionIDFunc != nil {
		if id := strings.TrimSpace(c.SessionIDFunc()); id != "" {
			return id
		}
	}
	return strings.TrimSpace(c.SessionID)
}

func (c *ContextRecorder) serverName() string {
	if c.ServerName != "" {
		return c.ServerName
	}
	return AgentContextServerName
}

func (c *ContextRecorder) contentLimit() int {
	if c.MaxContentBytes > 0 {
		return c.MaxContentBytes
	}
	return defaultContextContentLimit
}

// capBytes truncates s to at most n bytes, appending an elision marker so a
// reader can tell the record was cut.
func capBytes(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	return s[:n] + "\n…(truncated)"
}

func orDefault(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

// Compile-time interface assertion. notify.ContextRecorder has the identical
// shape, so satisfying this satisfies both call sites.
var _ pipeline.ContextRecorder = (*ContextRecorder)(nil)
