package agentcontext

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestMessagePayloadRoundTrip(t *testing.T) {
	now := time.Now().Truncate(time.Millisecond)
	readAt := now.Add(5 * time.Minute)
	expires := now.Add(24 * time.Hour)
	msg := Message{
		ID:             "abc123",
		FromAgentID:    "claude-code",
		ToAgentID:      "codex",
		Subject:        "handoff heads-up",
		Body:           "the debounce fix landed, rebase before continuing",
		SessionRef:     "sess-42",
		Status:         MessageStatusRead,
		CreatedAt:      now,
		ReadAt:         &readAt,
		ExpiresAt:      &expires,
		NudgeDelivered: true,
	}

	got, err := payloadToMessage(messageToPayload(msg))
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != msg.ID || got.FromAgentID != msg.FromAgentID || got.ToAgentID != msg.ToAgentID {
		t.Fatalf("identity fields mangled: %+v", got)
	}
	if got.Subject != msg.Subject || got.Body != msg.Body || got.SessionRef != msg.SessionRef {
		t.Fatalf("content fields mangled: %+v", got)
	}
	if got.Status != MessageStatusRead || !got.NudgeDelivered {
		t.Fatalf("state fields mangled: %+v", got)
	}
	if !got.CreatedAt.Equal(msg.CreatedAt) || got.ReadAt == nil || !got.ReadAt.Equal(readAt) ||
		got.ExpiresAt == nil || !got.ExpiresAt.Equal(expires) {
		t.Fatalf("timestamps mangled: %+v", got)
	}
}

func TestPayloadToMessageNil(t *testing.T) {
	if _, err := payloadToMessage(nil); err == nil {
		t.Fatal("want error for nil payload")
	}
}

func TestNudgeContent(t *testing.T) {
	if got := nudgeContent(Message{Body: "hello"}); got != "hello" {
		t.Fatalf("got %q", got)
	}
	if got := nudgeContent(Message{Subject: "ci", Body: "pipeline red"}); got != "ci: pipeline red" {
		t.Fatalf("got %q", got)
	}
}

func TestSortMessagesNewestFirst(t *testing.T) {
	base := time.Now()
	msgs := []*Message{
		{ID: "old", CreatedAt: base.Add(-2 * time.Hour)},
		{ID: "new", CreatedAt: base},
		{ID: "mid", CreatedAt: base.Add(-1 * time.Hour)},
	}
	sortMessagesNewestFirst(msgs)
	if msgs[0].ID != "new" || msgs[1].ID != "mid" || msgs[2].ID != "old" {
		t.Fatalf("order = %s,%s,%s", msgs[0].ID, msgs[1].ID, msgs[2].ID)
	}
}

func TestTryNudgePostsToHUD(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/agent/nudge" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	svc := &MessageSvc{Service: &Service{cfg: Config{HUDBaseURL: srv.URL}}}
	delivered := svc.tryNudge(context.Background(), Message{
		FromAgentID: "codex",
		ToAgentID:   "claude-code",
		Subject:     "fyi",
		Body:        "check MR 1082",
	})
	if !delivered {
		t.Fatal("want delivered=true")
	}
	if got["target_agent_id"] != "claude-code" || got["from_agent"] != "codex" {
		t.Fatalf("nudge payload = %v", got)
	}
	if got["content"] != "fyi: check MR 1082" || got["type"] != "message" {
		t.Fatalf("nudge payload = %v", got)
	}
}

func TestTryNudgeDisabledAndUnreachable(t *testing.T) {
	svc := &MessageSvc{Service: &Service{cfg: Config{HUDBaseURL: ""}}}
	if svc.tryNudge(context.Background(), Message{ToAgentID: "x", Body: "y"}) {
		t.Fatal("empty HUD URL must disable nudges")
	}

	svc = &MessageSvc{Service: &Service{cfg: Config{HUDBaseURL: "http://127.0.0.1:1"}}}
	if svc.tryNudge(context.Background(), Message{ToAgentID: "x", Body: "y"}) {
		t.Fatal("unreachable HUD must report delivered=false")
	}
}
