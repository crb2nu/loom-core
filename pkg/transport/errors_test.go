package transport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
)

func TestIsError(t *testing.T) {
	for _, s := range []string{"dial tcp: lookup hub: i/o timeout", "connect: connection refused", "lookup hub: no such host", "unexpected EOF", "websocket: close 1006", "broken pipe"} {
		if !IsError(errors.New(s)) {
			t.Errorf("not transport: %s", s)
		}
	}
	if !IsError(fmt.Errorf("wrapped: %w", io.EOF)) || IsError(nil) {
		t.Fatal("wrapped EOF/nil")
	}
	for _, s := range []string{"unknown", "401 unauthorized: dial tcp", "403 forbidden: EOF", "missing token: i/o timeout", "policy disabled: EOF", "tool reported error: unexpected EOF", "invalid args (code=-32602): EOF"} {
		if IsError(errors.New(s)) {
			t.Errorf("classified permanent error: %s", s)
		}
	}
}

// TestIsTimeout pins the 2026-09-13 hub-contention shape: the operator's
// agent_context probe timed out waiting for the hub call slot while four runs
// and the reconciler shared the hub. That is a busy peer, not a dead
// connection (IsError must stay false so the client never redials on it) and
// not configuration (permanent text still wins).
func TestIsTimeout(t *testing.T) {
	for _, s := range []string{
		"wait for call slot: context deadline exceeded",
		"mcp_hub_session: MCP hub agent_context unavailable after 1 consecutive failure(s): wait for call slot: context deadline exceeded",
		"mcphub: wait for agent_context call slot: context deadline exceeded",
		"flexinfer: chat: context deadline exceeded",
	} {
		if !IsTimeout(errors.New(s)) {
			t.Errorf("not timeout: %s", s)
		}
		if IsError(errors.New(s)) {
			t.Errorf("timeout classified as a dead connection (would redial): %s", s)
		}
	}
	if !IsTimeout(fmt.Errorf("probe: %w", context.DeadlineExceeded)) || IsTimeout(nil) {
		t.Fatal("wrapped context.DeadlineExceeded/nil")
	}
	for _, s := range []string{"unknown", "connect: connection refused", "401 unauthorized: context deadline exceeded", "missing token: wait for call slot", "policy disabled: context deadline exceeded", "tool reported error: context deadline exceeded", "invalid args (code=-32602): deadline exceeded"} {
		if IsTimeout(errors.New(s)) {
			t.Errorf("classified permanent/non-timeout error as timeout: %s", s)
		}
	}
}
