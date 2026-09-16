package clients

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	mcp "gitlab.flexinfer.ai/libs/mcp-go"

	"github.com/crb2nu/loom/pkg/mills/pipeline"
)

// slowToolTransport answers initialize immediately and every tools/call only
// after delay — the shape of a devbox quality gate that legitimately runs
// longer than the hub client's default per-call cap.
type slowToolTransport struct {
	mu       sync.Mutex
	pending  []mcp.Message
	closed   bool
	delay    time.Duration
	toolBody []byte
}

func (s *slowToolTransport) Send(_ context.Context, msg *mcp.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("transport closed")
	}
	if msg.Method == "notifications/initialized" {
		return nil
	}
	resp := mcp.Message{JSONRPC: "2.0", ID: msg.ID, Result: []byte(`{}`)}
	switch msg.Method {
	case "initialize":
		resp.Result = []byte(`{"protocolVersion":"2024-11-05","serverInfo":{"name":"x","version":"1"}}`)
		s.pending = append(s.pending, resp)
	case "tools/call":
		resp.Result = s.toolBody
		time.AfterFunc(s.delay, func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			if !s.closed {
				s.pending = append(s.pending, resp)
			}
		})
	default:
		s.pending = append(s.pending, resp)
	}
	return nil
}

func (s *slowToolTransport) Recv(ctx context.Context) (*mcp.Message, error) {
	for {
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return nil, errors.New("transport closed")
		}
		if len(s.pending) > 0 {
			msg := s.pending[0]
			s.pending = s.pending[1:]
			s.mu.Unlock()
			return &msg, nil
		}
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func (s *slowToolTransport) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

func newSlowHubClient(t *testing.T, delay time.Duration, toolBody []byte) *MCPHubClient {
	t.Helper()
	return newMCPHubClientWithDefaults(MCPHubConfig{
		HubURL:         "wss://stub",
		ConnectTimeout: 100 * time.Millisecond,
		CallTimeout:    50 * time.Millisecond,
	}, func(ctx context.Context, _ string) (mcp.Transport, error) {
		return &slowToolTransport{delay: delay, toolBody: toolBody}, nil
	})
}

// A tool slower than the client default dies under CallTool and survives under
// CallToolWithTimeout with a cap that covers it.
func TestMCPHubClient_CallToolWithTimeoutOverridesDefault(t *testing.T) {
	hub := newSlowHubClient(t, 300*time.Millisecond, []byte(`{"content":[{"type":"text","text":"slow ok"}]}`))
	_, err := hub.CallTool(context.Background(), "devbox", "slow_tool", nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("CallTool under the 50ms default: err=%v, want context.DeadlineExceeded", err)
	}
	body, err := hub.CallToolWithTimeout(context.Background(), "devbox", "slow_tool", nil, 3*time.Second)
	if err != nil {
		t.Fatalf("CallToolWithTimeout(3s): %v", err)
	}
	if body != "slow ok" {
		t.Fatalf("body = %q, want the tool's text", body)
	}
}

// The devbox gate must run under GateTimeout, not the hub default.
func TestDevboxClient_GateRunsUnderGateTimeout(t *testing.T) {
	body := devboxQualityGateResult{Language: "go", Passed: true, TotalDurationMs: 250_000}
	hub := newSlowHubClient(t, 300*time.Millisecond, devboxStubResult(t, body))
	dc := NewDevboxClient(hub)
	if dc.GateTimeout != DefaultDevboxGateTimeout {
		t.Fatalf("default GateTimeout = %v, want %v", dc.GateTimeout, DefaultDevboxGateTimeout)
	}
	dc.GateTimeout = 3 * time.Second
	resp, err := dc.QualityGate(context.Background(), pipeline.DevboxRequest{Project: "loom-core", AgentID: "mills"})
	if err != nil {
		t.Fatalf("quality gate under GateTimeout: %v", err)
	}
	if !resp.Passed {
		t.Fatalf("expected the slow gate's result to be delivered (Passed=true)")
	}

	// And with GateTimeout unset it falls back to the hub default, which the
	// slow gate exceeds — the pre-fix behaviour, kept explicit.
	dc.GateTimeout = 0
	if _, err := dc.QualityGate(context.Background(), pipeline.DevboxRequest{Project: "loom-core", AgentID: "mills"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("gate with GateTimeout=0 under a 50ms default: err=%v, want context.DeadlineExceeded", err)
	}
}
