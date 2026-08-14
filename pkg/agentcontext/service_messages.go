package agentcontext

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"gitlab.flexinfer.ai/libs/mcp-go"

	"github.com/crb2nu/loom/pkg/validate"
)

// MessageStatus defines direct-message states.
type MessageStatus string

const (
	MessageStatusUnread MessageStatus = "unread"
	MessageStatusRead   MessageStatus = "read"
)

// Message is a lightweight directed note between agents. Unlike a Handoff it
// carries no session context package — it is the "send a line to another
// agent" primitive that works across vendors because both ends talk to the
// same agent-context store.
type Message struct {
	ID          string        `json:"id"`
	FromAgentID string        `json:"from_agent_id"`
	ToAgentID   string        `json:"to_agent_id"`
	Subject     string        `json:"subject,omitempty"`
	Body        string        `json:"body"`
	SessionRef  string        `json:"session_ref,omitempty"`
	Status      MessageStatus `json:"status"`
	CreatedAt   time.Time     `json:"created_at"`
	ReadAt      *time.Time    `json:"read_at,omitempty"`
	ExpiresAt   *time.Time    `json:"expires_at,omitempty"`
	// NudgeDelivered records whether the best-effort HUD nudge enqueue
	// succeeded at send time (live-delivery hint only; the durable copy in
	// the store is the source of truth either way).
	NudgeDelivered bool `json:"nudge_delivered"`
}

type MessageSvc struct {
	*Service
	// nudgeClient posts best-effort live-delivery nudges to the HUD.
	// Swappable in tests.
	nudgeClient *http.Client
}

const messageMaxBodyBytes = 32 * 1024

// HandleMessageSend stores a directed message for another agent and
// best-effort enqueues a HUD nudge so a heartbeating recipient hears about
// it before its next inbox poll.
func (s *MessageSvc) HandleMessageSend(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	v := validate.NewArgs(args)
	toAgentID := v.Required("to_agent_id")
	body := v.Required("body")
	fromAgentID := v.String("from_agent_id", s.cfg.DefaultAgentID)
	subject := v.String("subject", "")
	sessionRef := v.String("session_ref", "")
	expiresHours := v.Int("expires_hours", s.cfg.MessageExpirationHours)

	if err := v.Validate(); err != nil {
		return mcp.ErrorResult(err), nil
	}
	if fromAgentID == "" {
		return mcp.ErrorResult(fmt.Errorf("from_agent_id is required when no default agent id is configured")), nil
	}
	if len(body) > messageMaxBodyBytes {
		return mcp.ErrorResult(fmt.Errorf("body exceeds %d bytes; send a handoff with entries instead", messageMaxBodyBytes)), nil
	}

	now := time.Now()
	msg := Message{
		ID:          GenerateID(fromAgentID, toAgentID, body, now),
		FromAgentID: fromAgentID,
		ToAgentID:   toAgentID,
		Subject:     subject,
		Body:        body,
		SessionRef:  sessionRef,
		Status:      MessageStatusUnread,
		CreatedAt:   now,
	}
	if expiresHours > 0 {
		expires := now.Add(time.Duration(expiresHours) * time.Hour)
		msg.ExpiresAt = &expires
	}

	// Live-delivery hints, both best-effort: the in-process nudge queue
	// covers hub-shared deployments where the recipient heartbeats this
	// same service instance; the HUD HTTP nudge covers split processes.
	s.AddNudge(msg.ToAgentID, &Nudge{
		ID:        msg.ID,
		Type:      NudgeTypeMessage,
		Content:   nudgeContent(msg),
		CreatedAt: now,
		FromAgent: msg.FromAgentID,
	})
	msg.NudgeDelivered = s.tryNudge(ctx, msg)

	if err := s.qdrant.Get(CollMessages).EnsureCollection(ctx, sessionsVectorSize); err != nil {
		return mcp.ErrorResult(fmt.Errorf("ensure collection: %w", err)), nil
	}
	point := Point{
		ID:      msg.ID,
		Vector:  make([]float64, sessionsVectorSize),
		Payload: messageToPayload(msg),
	}
	if err := s.qdrant.Get(CollMessages).Upsert(ctx, []Point{point}, true); err != nil {
		return mcp.ErrorResult(fmt.Errorf("store message: %w", err)), nil
	}

	return mcp.JSONResult(map[string]any{
		"ok":              true,
		"message_id":      msg.ID,
		"to_agent_id":     msg.ToAgentID,
		"from_agent_id":   msg.FromAgentID,
		"nudge_delivered": msg.NudgeDelivered,
	})
}

// HandleMessageInbox lists messages addressed to an agent, newest first,
// marking returned unread messages as read unless mark_read=false.
func (s *MessageSvc) HandleMessageInbox(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	v := validate.NewArgs(args)
	agentID := v.String("agent_id", s.cfg.DefaultAgentID)
	includeRead := v.Bool("include_read", false)
	markRead := v.Bool("mark_read", true)
	limit := v.Int("limit", 20)

	if err := v.Validate(); err != nil {
		return mcp.ErrorResult(err), nil
	}
	if agentID == "" {
		return mcp.ErrorResult(fmt.Errorf("agent_id is required when no default agent id is configured")), nil
	}
	if limit <= 0 || limit > 100 {
		limit = 20
	}

	conds := []any{Match("to_agent_id", agentID)}
	if !includeRead {
		conds = append(conds, Match("status", string(MessageStatusUnread)))
	}
	points, err := s.qdrant.Get(CollMessages).ScrollPoints(ctx, FilterMust(conds...), 200, false)
	if err != nil {
		return mcp.ErrorResult(fmt.Errorf("query message inbox: %w", err)), nil
	}

	now := time.Now()
	msgs := make([]*Message, 0, len(points))
	for _, p := range points {
		m, perr := payloadToMessage(p.Payload)
		if perr != nil || m == nil {
			continue
		}
		if m.ExpiresAt != nil && now.After(*m.ExpiresAt) {
			continue
		}
		msgs = append(msgs, m)
	}
	// Newest first, bounded.
	sortMessagesNewestFirst(msgs)
	if len(msgs) > limit {
		msgs = msgs[:limit]
	}

	out := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		entry := map[string]any{
			"message_id": m.ID,
			"from_agent": m.FromAgentID,
			"subject":    m.Subject,
			"body":       m.Body,
			"status":     string(m.Status),
			"created_at": m.CreatedAt.Format(time.RFC3339),
		}
		if m.SessionRef != "" {
			entry["session_ref"] = m.SessionRef
		}
		out = append(out, entry)

		if markRead && m.Status == MessageStatusUnread {
			readAt := now.Format(time.RFC3339Nano)
			if err := s.qdrant.Get(CollMessages).SetPayload(ctx, []string{m.ID}, map[string]any{
				"status":  string(MessageStatusRead),
				"read_at": readAt,
			}, true); err != nil {
				s.logger.Warn("failed to mark message read", "message_id", m.ID, "error", err)
			}
		}
	}

	return mcp.JSONResult(map[string]any{
		"ok":       true,
		"agent_id": agentID,
		"messages": out,
		"count":    len(out),
	})
}

// tryNudge posts the message to the HUD nudge queue for live delivery on the
// recipient's next heartbeat. Failures are expected (HUD down, off-host) and
// never block the durable store write.
func (s *MessageSvc) tryNudge(ctx context.Context, msg Message) bool {
	if s.cfg.HUDBaseURL == "" {
		return false
	}
	payload, err := json.Marshal(map[string]any{
		"target_agent_id": msg.ToAgentID,
		"type":            "message",
		"content":         nudgeContent(msg),
		"from_agent":      msg.FromAgentID,
	})
	if err != nil {
		return false
	}

	nctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(nctx, http.MethodPost, s.cfg.HUDBaseURL+"/api/agent/nudge", bytes.NewReader(payload))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")

	client := s.nudgeClient
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

// nudgeContent renders the short live-delivery form of a message.
func nudgeContent(msg Message) string {
	content := msg.Body
	if msg.Subject != "" {
		content = msg.Subject + ": " + content
	}
	return content
}

func sortMessagesNewestFirst(msgs []*Message) {
	for i := 1; i < len(msgs); i++ {
		for j := i; j > 0 && msgs[j].CreatedAt.After(msgs[j-1].CreatedAt); j-- {
			msgs[j], msgs[j-1] = msgs[j-1], msgs[j]
		}
	}
}

func messageToPayload(m Message) map[string]any {
	payload := map[string]any{
		"id":              m.ID,
		"from_agent_id":   m.FromAgentID,
		"to_agent_id":     m.ToAgentID,
		"subject":         m.Subject,
		"body":            m.Body,
		"session_ref":     m.SessionRef,
		"status":          string(m.Status),
		"created_at":      m.CreatedAt.Format(time.RFC3339Nano),
		"nudge_delivered": m.NudgeDelivered,
	}
	if m.ReadAt != nil {
		payload["read_at"] = m.ReadAt.Format(time.RFC3339Nano)
	}
	if m.ExpiresAt != nil {
		payload["expires_at"] = m.ExpiresAt.Format(time.RFC3339Nano)
	}
	return payload
}

func payloadToMessage(payload map[string]any) (*Message, error) {
	if payload == nil {
		return nil, fmt.Errorf("nil payload")
	}
	m := &Message{
		ID:          toString(payload["id"]),
		FromAgentID: toString(payload["from_agent_id"]),
		ToAgentID:   toString(payload["to_agent_id"]),
		Subject:     toString(payload["subject"]),
		Body:        toString(payload["body"]),
		SessionRef:  toString(payload["session_ref"]),
		Status:      MessageStatus(toString(payload["status"])),
	}
	if b, ok := payload["nudge_delivered"].(bool); ok {
		m.NudgeDelivered = b
	}
	if ts := toString(payload["created_at"]); ts != "" {
		if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
			m.CreatedAt = t
		}
	}
	if ts := toString(payload["read_at"]); ts != "" {
		if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
			m.ReadAt = &t
		}
	}
	if ts := toString(payload["expires_at"]); ts != "" {
		if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
			m.ExpiresAt = &t
		}
	}
	return m, nil
}
