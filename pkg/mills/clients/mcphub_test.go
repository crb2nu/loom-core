package clients

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	mcp "gitlab.flexinfer.ai/libs/mcp-go"

	"github.com/crb2nu/loom/pkg/mills/pipeline"
)

// fakeTransport is a deterministic in-memory mcp.Transport stand-in.
// Tests pre-load `responses` keyed on inbound method name; the
// transport echoes them back with the matching ID. Sent messages are
// recorded for assertion.
type fakeTransport struct {
	mu        sync.Mutex
	sent      []mcp.Message
	responses map[string][]byte // method → result JSON to send back
	failOn    map[string]*mcp.Error
	closed    bool
	pending   []mcp.Message
}

// concurrencyProbeTransport withholds tools/call responses until release is
// closed. It makes concurrent Send calls observable without relying on goroutine
// scheduling or response order. A single MCP transport is a request/response
// stream: more than one tools/call in flight lets independent Recv loops steal
// and discard each other's JSON-RPC responses.
type concurrencyProbeTransport struct {
	mu          sync.Mutex
	pending     []mcp.Message
	release     chan struct{}
	started     chan string
	inFlight    int
	maxInFlight int
	closed      bool
}

func newConcurrencyProbeTransport() *concurrencyProbeTransport {
	return &concurrencyProbeTransport{
		release: make(chan struct{}),
		started: make(chan string, 16),
	}
}

func (f *concurrencyProbeTransport) Send(_ context.Context, msg *mcp.Message) error {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return errors.New("transport closed")
	}
	if msg.Method == "notifications/initialized" {
		f.mu.Unlock()
		return nil
	}
	if msg.Method == "initialize" {
		f.pending = append(f.pending, mcp.Message{JSONRPC: "2.0", ID: msg.ID, Result: []byte(`{}`)})
		f.mu.Unlock()
		return nil
	}
	var params mcp.CallToolParams
	if err := json.Unmarshal(msg.Params, &params); err != nil {
		f.mu.Unlock()
		return err
	}
	marker, _ := params.Arguments["marker"].(string)
	if marker == "" {
		marker, _ = params.Arguments["agent_id"].(string)
	}
	f.inFlight++
	if f.inFlight > f.maxInFlight {
		f.maxInFlight = f.inFlight
	}
	f.mu.Unlock()
	f.started <- marker

	go func(id any, value string) {
		<-f.release
		body := makeCallToolResultForProbe(value)
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.closed {
			return
		}
		f.pending = append(f.pending, mcp.Message{JSONRPC: "2.0", ID: id, Result: body})
		f.inFlight--
	}(msg.ID, marker)
	return nil
}

func makeCallToolResultForProbe(marker string) []byte {
	body, _ := json.Marshal(map[string]any{"marker": marker, "passed": true, "tested_sha": marker})
	result, _ := json.Marshal(mcp.CallToolResult{Content: []mcp.Content{{Type: "text", Text: string(body)}}})
	return result
}

func (f *concurrencyProbeTransport) Recv(ctx context.Context) (*mcp.Message, error) {
	for {
		f.mu.Lock()
		if f.closed {
			f.mu.Unlock()
			return nil, errors.New("transport closed")
		}
		if len(f.pending) > 0 {
			msg := f.pending[0]
			f.pending = f.pending[1:]
			f.mu.Unlock()
			return &msg, nil
		}
		f.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Millisecond):
		}
	}
}

func (f *concurrencyProbeTransport) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

func (f *concurrencyProbeTransport) maxConcurrent() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.maxInFlight
}

func (f *fakeTransport) Send(_ context.Context, msg *mcp.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return errors.New("transport closed")
	}
	f.sent = append(f.sent, *msg)
	if msg.Method == "notifications/initialized" {
		// Notification — no response.
		return nil
	}
	resp := mcp.Message{JSONRPC: "2.0", ID: msg.ID}
	if errPayload, bad := f.failOn[msg.Method]; bad {
		resp.Error = errPayload
	} else if body, ok := f.responses[msg.Method]; ok {
		resp.Result = body
	} else {
		// Default: empty success.
		resp.Result = []byte(`{}`)
	}
	f.pending = append(f.pending, resp)
	return nil
}

func (f *fakeTransport) Recv(ctx context.Context) (*mcp.Message, error) {
	for {
		f.mu.Lock()
		if f.closed {
			f.mu.Unlock()
			return nil, errors.New("transport closed")
		}
		if len(f.pending) > 0 {
			msg := f.pending[0]
			f.pending = f.pending[1:]
			f.mu.Unlock()
			return &msg, nil
		}
		f.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func (f *fakeTransport) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

func (f *fakeTransport) sentMessages() []mcp.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]mcp.Message, len(f.sent))
	copy(out, f.sent)
	return out
}

// makeCallToolResult marshals a CallToolResult with one text content
// block whose body is the JSON of body.
func makeCallToolResult(t *testing.T, body any) []byte {
	t.Helper()
	bodyJSON, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	res := mcp.CallToolResult{
		Content: []mcp.Content{{Type: "text", Text: string(bodyJSON)}},
	}
	out, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	return out
}

func newTestHubClient(t *testing.T, ft *fakeTransport) *MCPHubClient {
	t.Helper()
	c := newMCPHubClientWithDefaults(MCPHubConfig{
		HubURL:         "wss://stub",
		ConnectTimeout: 100 * time.Millisecond,
		CallTimeout:    500 * time.Millisecond,
	}, func(ctx context.Context, _ string) (mcp.Transport, error) {
		return ft, nil
	})
	return c
}

// ----- Config validation -----

func TestNewMCPHubClient_RequiresHubURL(t *testing.T) {
	if _, err := NewMCPHubClient(MCPHubConfig{}); err == nil {
		t.Error("expected error for empty HubURL")
	}
}

func TestNewMCPHubClient_AppliesDefaults(t *testing.T) {
	c, err := NewMCPHubClient(MCPHubConfig{HubURL: "wss://x"})
	if err != nil {
		t.Fatal(err)
	}
	if c.cfg.Profile != "loom-mills" {
		t.Errorf("Profile default = %q, want loom-mills", c.cfg.Profile)
	}
	if c.cfg.ConnectTimeout != 10*time.Second {
		t.Errorf("ConnectTimeout default = %v, want 10s", c.cfg.ConnectTimeout)
	}
	if c.cfg.CallTimeout != 10*time.Minute {
		t.Errorf("CallTimeout default = %v, want 10m", c.cfg.CallTimeout)
	}
}

// ----- Initialize handshake -----

func TestCallTool_PerformsInitializeOnFirstCall(t *testing.T) {
	ft := &fakeTransport{
		responses: map[string][]byte{
			"initialize": []byte(`{"protocolVersion":"2024-11-05","serverInfo":{"name":"x","version":"1"}}`),
			"tools/call": makeCallToolResult(t, map[string]any{"ok": true}),
		},
	}
	c := newTestHubClient(t, ft)
	got, err := c.CallTool(context.Background(), "mcp-devbox", "devbox_quality_gate", map[string]any{"project": "loom-core"})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !strings.Contains(got, `"ok":true`) {
		t.Errorf("expected JSON body in result, got %q", got)
	}
	sent := ft.sentMessages()
	if len(sent) != 3 {
		t.Fatalf("expected 3 sends (initialize, initialized notif, tools/call), got %d", len(sent))
	}
	if sent[0].Method != "initialize" {
		t.Errorf("first message = %q, want initialize", sent[0].Method)
	}
	if sent[1].Method != "notifications/initialized" {
		t.Errorf("second message = %q, want notifications/initialized", sent[1].Method)
	}
	if sent[2].Method != "tools/call" {
		t.Errorf("third message = %q, want tools/call", sent[2].Method)
	}
}

func TestCallTool_PrefersStructuredContentOverText(t *testing.T) {
	res, err := json.Marshal(mcp.CallToolResult{
		Content: []mcp.Content{{Type: "text", Text: `{"source":"text","files":[]}`}},
		StructuredContent: map[string]any{
			"source": "structured",
			"files":  []string{"alpha.go", "beta.go"},
		},
	})
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	ft := &fakeTransport{responses: map[string][]byte{
		"initialize": []byte(`{}`),
		"tools/call": res,
	}}

	body, err := newTestHubClient(t, ft).CallTool(context.Background(), "agent_context", "agent_plan_slice_list", nil)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	var got struct {
		Source string   `json:"source"`
		Files  []string `json:"files"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode preferred body: %v", err)
	}
	if got.Source != "structured" || len(got.Files) != 2 || got.Files[1] != "beta.go" {
		t.Fatalf("preferred body = %#v", got)
	}
}

func TestCallTool_ReusesTransportForSubsequentCalls(t *testing.T) {
	ft := &fakeTransport{
		responses: map[string][]byte{
			"initialize": []byte(`{"protocolVersion":"2024-11-05","serverInfo":{"name":"x","version":"1"}}`),
			"tools/call": makeCallToolResult(t, map[string]any{"ok": true}),
		},
	}
	c := newTestHubClient(t, ft)
	for i := 0; i < 3; i++ {
		if _, err := c.CallTool(context.Background(), "mcp-devbox", "tool", nil); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	sent := ft.sentMessages()
	initCount := 0
	for _, m := range sent {
		if m.Method == "initialize" {
			initCount++
		}
	}
	if initCount != 1 {
		t.Errorf("expected 1 initialize across 3 calls, got %d", initCount)
	}
}

// ----- Tool call args + body -----

func TestCallTool_PassesArgumentsThroughCallToolParams(t *testing.T) {
	ft := &fakeTransport{
		responses: map[string][]byte{
			"initialize": []byte(`{"protocolVersion":"2024-11-05","serverInfo":{"name":"x","version":"1"}}`),
			"tools/call": makeCallToolResult(t, map[string]any{}),
		},
	}
	c := newTestHubClient(t, ft)
	args := map[string]any{
		"project":  "loom-core",
		"agent_id": "claude-code",
		"timeout":  float64(120),
	}
	if _, err := c.CallTool(context.Background(), "mcp-devbox", "devbox_quality_gate", args); err != nil {
		t.Fatalf("call: %v", err)
	}
	sent := ft.sentMessages()
	var callMsg mcp.Message
	for _, m := range sent {
		if m.Method == "tools/call" {
			callMsg = m
			break
		}
	}
	var params mcp.CallToolParams
	if err := json.Unmarshal(callMsg.Params, &params); err != nil {
		t.Fatalf("decode params: %v", err)
	}
	if params.Name != "devbox_quality_gate" {
		t.Errorf("tool name = %q", params.Name)
	}
	if params.Arguments["project"] != "loom-core" {
		t.Errorf("project arg = %v", params.Arguments["project"])
	}
	if params.Arguments["agent_id"] != "claude-code" {
		t.Errorf("agent_id arg = %v", params.Arguments["agent_id"])
	}
}

// ----- Error paths -----

func TestCallTool_JSONRPCErrorBubbles(t *testing.T) {
	ft := &fakeTransport{
		responses: map[string][]byte{
			"initialize": []byte(`{"protocolVersion":"2024-11-05","serverInfo":{"name":"x","version":"1"}}`),
		},
		failOn: map[string]*mcp.Error{
			"tools/call": {Code: -32602, Message: "invalid args"},
		},
	}
	c := newTestHubClient(t, ft)
	_, err := c.CallTool(context.Background(), "mcp-devbox", "tool", nil)
	if err == nil {
		t.Fatal("expected error from JSON-RPC error response")
	}
	if !strings.Contains(err.Error(), "invalid args") {
		t.Errorf("error message missing 'invalid args': %v", err)
	}
}

func TestCallTool_IsErrorOnResultBubblesText(t *testing.T) {
	bodyJSON, _ := json.Marshal(map[string]any{"reason": "no project"})
	res, _ := json.Marshal(mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{{Type: "text", Text: string(bodyJSON)}},
	})
	ft := &fakeTransport{
		responses: map[string][]byte{
			"initialize": []byte(`{"protocolVersion":"2024-11-05","serverInfo":{"name":"x","version":"1"}}`),
			"tools/call": res,
		},
	}
	c := newTestHubClient(t, ft)
	body, err := c.CallTool(context.Background(), "mcp-devbox", "tool", nil)
	if err == nil {
		t.Fatal("expected error when IsError=true")
	}
	if !strings.Contains(body, "no project") {
		t.Errorf("body should still carry the reason: %q", body)
	}
	if health := c.ServerHealth("mcp-devbox"); !health.Known || !health.Healthy {
		t.Fatalf("tool-level rejection marked the live transport unhealthy: %+v", health)
	}
}

func TestCallTool_InitializeFailureClosesTransport(t *testing.T) {
	ft := &fakeTransport{
		failOn: map[string]*mcp.Error{
			"initialize": {Code: -32603, Message: "auth failed"},
		},
	}
	c := newTestHubClient(t, ft)
	if _, err := c.CallTool(context.Background(), "mcp-devbox", "tool", nil); err == nil {
		t.Error("expected error on initialize failure")
	}
	if !ft.closed {
		t.Error("transport should be closed after init failure")
	}
}

func TestCallTool_RequiresServerAndToolName(t *testing.T) {
	c := newTestHubClient(t, &fakeTransport{})
	if _, err := c.CallTool(context.Background(), "", "tool", nil); err == nil {
		t.Error("expected error for empty server")
	}
	if _, err := c.CallTool(context.Background(), "srv", "", nil); err == nil {
		t.Error("expected error for empty tool")
	}
}

// ----- ID-multiplexing / out-of-band messages -----

func TestCallTool_SkipsOutOfBandMessages(t *testing.T) {
	// Inject a notification (no ID) that arrives BETWEEN our send and
	// our own response. The client must skip and keep reading until
	// it finds the matching id.
	ft := &fakeTransport{
		responses: map[string][]byte{
			"initialize": []byte(`{"protocolVersion":"2024-11-05","serverInfo":{"name":"x","version":"1"}}`),
		},
	}
	c := newTestHubClient(t, ft)
	// Drive the initialize so we can hand-craft the tools/call response.
	// Trigger initialize by calling once with a canned response.
	ft.responses["tools/call"] = makeCallToolResult(t, map[string]any{"first": true})
	if _, err := c.CallTool(context.Background(), "mcp-devbox", "tool", nil); err != nil {
		t.Fatalf("first call: %v", err)
	}
	// Now stage: an unrelated notification, then the real response.
	ft.mu.Lock()
	notif := mcp.Message{JSONRPC: "2.0", Method: "log/message", Params: []byte(`{"text":"server status"}`)}
	ft.pending = append(ft.pending, notif)
	ft.responses["tools/call"] = makeCallToolResult(t, map[string]any{"second": true})
	ft.mu.Unlock()
	got, err := c.CallTool(context.Background(), "mcp-devbox", "tool", nil)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if !strings.Contains(got, `"second":true`) {
		t.Errorf("expected to skip notification and find matching response: %q", got)
	}
}

func TestCallTool_SerializesCallsPerServer(t *testing.T) {
	probe := newConcurrencyProbeTransport()
	c := newMCPHubClientWithDefaults(MCPHubConfig{
		HubURL:         "wss://stub",
		ConnectTimeout: 100 * time.Millisecond,
		CallTimeout:    time.Second,
	}, func(context.Context, string) (mcp.Transport, error) { return probe, nil })

	errCh := make(chan error, 2)
	for _, marker := range []string{"first", "second"} {
		marker := marker
		go func() {
			body, err := c.CallTool(context.Background(), "agent_context", "agent_plan_list", map[string]any{"marker": marker})
			if err == nil && !strings.Contains(body, marker) {
				err = fmt.Errorf("response %q did not match caller %q", body, marker)
			}
			errCh <- err
		}()
	}

	select {
	case <-probe.started:
	case <-time.After(time.Second):
		t.Fatal("first call never reached transport")
	}
	concurrent := false
	select {
	case <-probe.started:
		concurrent = true
	case <-time.After(50 * time.Millisecond):
	}
	close(probe.release)
	for range 2 {
		if err := <-errCh; err != nil {
			t.Errorf("CallTool: %v", err)
		}
	}
	if concurrent || probe.maxConcurrent() != 1 {
		t.Fatalf("same-server calls overlapped: observed_second_before_release=%t max_in_flight=%d", concurrent, probe.maxConcurrent())
	}
}

func TestCallTool_QueueWaitHonorsContext(t *testing.T) {
	probe := newConcurrencyProbeTransport()
	c := newMCPHubClientWithDefaults(MCPHubConfig{
		HubURL:         "wss://stub",
		ConnectTimeout: 100 * time.Millisecond,
		CallTimeout:    time.Second,
	}, func(context.Context, string) (mcp.Transport, error) { return probe, nil })

	firstDone := make(chan error, 1)
	go func() {
		_, err := c.CallTool(context.Background(), "agent_context", "agent_plan_list", map[string]any{"marker": "blocking"})
		firstDone <- err
	}()
	select {
	case <-probe.started:
	case <-time.After(time.Second):
		t.Fatal("blocking call never reached transport")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	secondDone := make(chan error, 1)
	go func() {
		_, err := c.CallTool(ctx, "agent_context", "agent_plan_slice_list", map[string]any{"marker": "queued"})
		secondDone <- err
	}()

	var secondErr error
	select {
	case secondErr = <-secondDone:
	case <-time.After(200 * time.Millisecond):
		close(probe.release)
		<-firstDone
		t.Fatal("queued call outlived its context while waiting for the shared transport")
	}
	if !errors.Is(secondErr, context.DeadlineExceeded) {
		t.Fatalf("queued call error = %v, want context deadline exceeded", secondErr)
	}
	health := c.ServerHealth("agent_context")
	if !health.Known || health.Healthy || health.ConsecutiveFailures != 1 {
		t.Fatalf("health after queue timeout = %+v, want one live dependency failure", health)
	}
	close(probe.release)
	if err := <-firstDone; err != nil {
		t.Fatalf("blocking call: %v", err)
	}
	select {
	case marker := <-probe.started:
		t.Fatalf("timed-out queued call reached transport with marker %q", marker)
	default:
	}
}

// ----- Close lifecycle -----

func TestClose_ClosesAllTransports(t *testing.T) {
	ft := &fakeTransport{
		responses: map[string][]byte{
			"initialize": []byte(`{}`),
			"tools/call": makeCallToolResult(t, map[string]any{}),
		},
	}
	c := newTestHubClient(t, ft)
	if _, err := c.CallTool(context.Background(), "srv", "tool", nil); err != nil {
		t.Fatalf("call: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Errorf("close: %v", err)
	}
	if !ft.closed {
		t.Error("transport not closed after Close()")
	}
}

// ----- Transport invalidation + retry -----

// transportErrSequence implements mcp.Transport with a scripted error
// on the Nth Recv call. Used to simulate a half-broken cached
// connection: Send succeeds (or fails) and Recv emits a close-1006 /
// EOF style error to trigger the retry path.
type transportErrSequence struct {
	mu             sync.Mutex
	sendErrOnTry   int // 1-indexed call number on which Send returns sendErr; 0 disables
	sendTries      int
	sendErr        error
	recvErrOnTry   int // 1-indexed call number on which Recv returns recvErr; 0 disables
	recvTries      int
	recvErr        error
	defaultResults map[string][]byte // method → result bytes for successful calls
	failsInit      bool              // when true, return error on initialize
	closed         bool
	sent           []mcp.Message
	pending        []mcp.Message
}

func (f *transportErrSequence) Send(_ context.Context, msg *mcp.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return errors.New("transport closed")
	}
	if msg.Method == "tools/call" {
		f.sendTries++
		if f.sendErrOnTry != 0 && f.sendTries == f.sendErrOnTry {
			return f.sendErr
		}
	}
	f.sent = append(f.sent, *msg)
	if msg.Method == "notifications/initialized" {
		return nil
	}
	resp := mcp.Message{JSONRPC: "2.0", ID: msg.ID}
	if msg.Method == "initialize" && f.failsInit {
		resp.Error = &mcp.Error{Code: -32603, Message: "init refused"}
	} else if body, ok := f.defaultResults[msg.Method]; ok {
		resp.Result = body
	} else {
		resp.Result = []byte(`{}`)
	}
	f.pending = append(f.pending, resp)
	return nil
}

func (f *transportErrSequence) Recv(ctx context.Context) (*mcp.Message, error) {
	for {
		f.mu.Lock()
		if f.closed {
			f.mu.Unlock()
			return nil, errors.New("transport closed")
		}
		if len(f.pending) > 0 {
			// Peek at the next pending message: only count Recv tries on
			// tools/call responses so init handshakes don't burn the
			// scripted error budget.
			next := f.pending[0]
			isToolsCall := next.Result != nil || next.Error != nil
			if isToolsCall {
				f.recvTries++
				if f.recvErrOnTry != 0 && f.recvTries == f.recvErrOnTry {
					f.mu.Unlock()
					return nil, f.recvErr
				}
			}
			f.pending = f.pending[1:]
			f.mu.Unlock()
			return &next, nil
		}
		f.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func (f *transportErrSequence) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

func TestCallTool_RetriesOnceAfterTransportClose1006(t *testing.T) {
	// First Recv returns the gateway's close-1006 error (the exact
	// shape the canary failure log captured). The retry should redial
	// (a fresh transport) and succeed.
	dialCalls := 0
	var firstTransport *transportErrSequence
	successResult := makeCallToolResult(t, map[string]any{"passed": true})

	c := newMCPHubClientWithDefaults(MCPHubConfig{
		HubURL:         "wss://stub",
		ConnectTimeout: 100 * time.Millisecond,
		CallTimeout:    500 * time.Millisecond,
	}, func(_ context.Context, _ string) (mcp.Transport, error) {
		dialCalls++
		if dialCalls == 1 {
			firstTransport = &transportErrSequence{
				recvErrOnTry: 1,
				recvErr:      errors.New("read message: websocket: close 1006 (abnormal closure): unexpected EOF"),
				defaultResults: map[string][]byte{
					"initialize": []byte(`{"protocolVersion":"2024-11-05"}`),
					"tools/call": successResult,
				},
			}
			return firstTransport, nil
		}
		// Second dial: clean transport that succeeds.
		return &transportErrSequence{
			defaultResults: map[string][]byte{
				"initialize": []byte(`{"protocolVersion":"2024-11-05"}`),
				"tools/call": successResult,
			},
		}, nil
	})

	got, err := c.CallTool(context.Background(), "devbox", "devbox_quality_gate", map[string]any{"project": "loom-core"})
	if err != nil {
		t.Fatalf("CallTool after retry: %v", err)
	}
	if !strings.Contains(got, `"passed":true`) {
		t.Errorf("expected retried call to succeed, got body %q", got)
	}
	if dialCalls != 2 {
		t.Errorf("expected 2 dials (initial + retry), got %d", dialCalls)
	}
	if firstTransport == nil || !firstTransport.closed {
		t.Errorf("broken transport should have been closed during invalidation, closed=%v", firstTransport != nil && firstTransport.closed)
	}
}

func TestCallTool_RetriesOnceAfterBrokenPipeOnSend(t *testing.T) {
	// Simulates the third attempt in the canary log: Send returns
	// "broken pipe" on the cached (half-closed) connection. Retry
	// with a fresh dial succeeds.
	dialCalls := 0
	successResult := makeCallToolResult(t, map[string]any{"passed": true})

	c := newMCPHubClientWithDefaults(MCPHubConfig{
		HubURL:         "wss://stub",
		ConnectTimeout: 100 * time.Millisecond,
		CallTimeout:    500 * time.Millisecond,
	}, func(_ context.Context, _ string) (mcp.Transport, error) {
		dialCalls++
		if dialCalls == 1 {
			return &transportErrSequence{
				sendErrOnTry: 1,
				sendErr:      errors.New("write message: write tcp 10.42.7.5:57228->10.43.248.41:80: write: broken pipe"),
				defaultResults: map[string][]byte{
					"initialize": []byte(`{"protocolVersion":"2024-11-05"}`),
					"tools/call": successResult,
				},
			}, nil
		}
		return &transportErrSequence{
			defaultResults: map[string][]byte{
				"initialize": []byte(`{"protocolVersion":"2024-11-05"}`),
				"tools/call": successResult,
			},
		}, nil
	})

	if _, err := c.CallTool(context.Background(), "devbox", "devbox_quality_gate", nil); err != nil {
		t.Fatalf("CallTool after retry: %v", err)
	}
	if dialCalls != 2 {
		t.Errorf("expected 2 dials, got %d", dialCalls)
	}
}

func TestCallTool_DoesNotRetryJSONRPCErrors(t *testing.T) {
	// A tool-reported JSON-RPC error is NOT a transport failure and
	// must bubble immediately without burning a redial.
	dialCalls := 0
	c := newMCPHubClientWithDefaults(MCPHubConfig{
		HubURL:         "wss://stub",
		ConnectTimeout: 100 * time.Millisecond,
		CallTimeout:    500 * time.Millisecond,
	}, func(_ context.Context, _ string) (mcp.Transport, error) {
		dialCalls++
		ft := &fakeTransport{
			responses: map[string][]byte{
				"initialize": []byte(`{"protocolVersion":"2024-11-05"}`),
			},
			failOn: map[string]*mcp.Error{
				"tools/call": {Code: -32602, Message: "invalid args"},
			},
		}
		return ft, nil
	})

	if _, err := c.CallTool(context.Background(), "devbox", "tool", nil); err == nil {
		t.Fatal("expected JSON-RPC error to surface")
	}
	if dialCalls != 1 {
		t.Errorf("expected 1 dial (no retry on app-level error), got %d", dialCalls)
	}
}

func TestCallTool_StopsAfterOneRetry(t *testing.T) {
	// Both dials return broken transports — the second failure must
	// propagate (no infinite retry loop).
	dialCalls := 0
	c := newMCPHubClientWithDefaults(MCPHubConfig{
		HubURL:         "wss://stub",
		ConnectTimeout: 100 * time.Millisecond,
		CallTimeout:    500 * time.Millisecond,
	}, func(_ context.Context, _ string) (mcp.Transport, error) {
		dialCalls++
		return &transportErrSequence{
			recvErrOnTry: 1,
			recvErr:      errors.New("read message: websocket: close 1006 (abnormal closure): unexpected EOF"),
			defaultResults: map[string][]byte{
				"initialize": []byte(`{"protocolVersion":"2024-11-05"}`),
				"tools/call": []byte(`{}`),
			},
		}, nil
	})

	_, err := c.CallTool(context.Background(), "devbox", "tool", nil)
	if err == nil {
		t.Fatal("expected error after both attempts fail")
	}
	if dialCalls != 2 {
		t.Errorf("expected exactly 2 dials, got %d", dialCalls)
	}
}

func TestIsTransportError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"close 1006", errors.New("websocket: close 1006 (abnormal closure): unexpected EOF"), true},
		{"broken pipe", errors.New("write tcp x->y: write: broken pipe"), true},
		{"unexpected EOF", errors.New("read message: unexpected EOF"), true},
		{"connection reset", errors.New("read: connection reset by peer"), true},
		{"closed conn", errors.New("use of closed network connection"), true},
		{"transport closed", errors.New("transport closed"), true},
		{"i/o timeout", errors.New("read tcp: i/o timeout"), true},
		{"raw io.EOF", io.EOF, true},
		{"jsonrpc error", errors.New("mcphub: srv/tool: invalid args (code=-32602)"), false},
		{"tool reported error", errors.New("mcphub: srv/tool reported error: bad project"), false},
		{"dial DNS", errors.New("dial tcp: lookup hub: i/o timeout"), true},
		{"refused", errors.New("connect: connection refused"), true},
		{"auth with transport text", errors.New("401 unauthorized: EOF"), false},
		{"tool quotes transport", errors.New("mcphub: srv/tool reported error: EOF"), false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isTransportError(tc.err); got != tc.want {
				t.Errorf("isTransportError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// ----- Env helper -----

func TestMCPHubConfigFromEnv(t *testing.T) {
	env := map[string]string{
		"LOOM_MCP_HUB_URL":        "wss://hub.example/ws",
		"LOOM_MCP_HUB_PROFILE":    "loom-mills-prod",
		"LOOM_MCP_HUB_TOKEN":      "tok-abc",
		"CF_ACCESS_CLIENT_ID":     "cf-id",
		"CF_ACCESS_CLIENT_SECRET": "cf-secret",
	}
	cfg, ok := MCPHubConfigFromEnv(func(k string) string { return env[k] })
	if !ok {
		t.Fatal("expected ok=true with HubURL set")
	}
	if cfg.HubURL != "wss://hub.example/ws" {
		t.Errorf("HubURL = %q", cfg.HubURL)
	}
	if cfg.Profile != "loom-mills-prod" {
		t.Errorf("Profile = %q", cfg.Profile)
	}
	if cfg.Token != "tok-abc" {
		t.Errorf("Token = %q", cfg.Token)
	}
	if cfg.CFAccessClientID != "cf-id" || cfg.CFAccessClientSecret != "cf-secret" {
		t.Errorf("CF Access creds wrong: %+v", cfg)
	}
}

func TestMCPHubConfigFromEnv_NoURLDisables(t *testing.T) {
	if _, ok := MCPHubConfigFromEnv(func(string) string { return "" }); ok {
		t.Error("ok should be false when HubURL unset")
	}
}

func TestDevboxQualityGatesOverlap(t *testing.T) {
	probes := make(chan *concurrencyProbeTransport, 2)
	hub := newMCPHubClientWithDefaults(MCPHubConfig{}, func(context.Context, string) (mcp.Transport, error) {
		p := newConcurrencyProbeTransport()
		probes <- p
		return p, nil
	})
	defer hub.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	results := make(chan error, 2)
	for _, marker := range []string{"run-a", "run-b"} {
		go func() {
			r, err := NewDevboxClient(hub).QualityGate(ctx, pipeline.DevboxRequest{Project: "repo", AgentID: marker})
			if err == nil && (!r.Passed || r.TestedSHA != marker) {
				err = fmt.Errorf("wrong result: %+v", r)
			}
			results <- err
		}()
	}
	var active []*concurrencyProbeTransport
	defer func() {
		for _, p := range active {
			close(p.release)
		}
	}()
	for range 2 {
		select {
		case p := <-probes:
			active = append(active, p)
			select {
			case <-p.started:
			case <-ctx.Done():
				t.Fatal("gate did not start")
			}
		case <-ctx.Done():
			t.Fatal("gates did not overlap")
		}
	}
	completed := active
	for _, p := range active {
		close(p.release)
	}
	active = nil
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range completed {
		p.mu.Lock()
		if !p.closed {
			t.Error("successful stream leaked")
		}
		p.mu.Unlock()
	}
	hub.mu.Lock()
	defer hub.mu.Unlock()
	if len(hub.dedicated) != 0 || len(hub.transports) != 0 {
		t.Fatal("dedicated streams retained")
	}
}

func TestDedicatedCallFailureIsolatedFromSibling(t *testing.T) {
	for _, mode := range []string{"retry", "retry-exhausted", "timeout", "initialize"} {
		t.Run(mode, func(t *testing.T) {
			sibling := newConcurrencyProbeTransport()
			defer close(sibling.release)
			var streams []*transportErrSequence
			var mu sync.Mutex
			dials := 0
			hub := newMCPHubClientWithDefaults(MCPHubConfig{}, func(context.Context, string) (mcp.Transport, error) {
				mu.Lock()
				defer mu.Unlock()
				dials++
				if dials == 1 {
					return sibling, nil
				}
				if mode == "timeout" {
					return &slowToolTransport{delay: time.Second}, nil
				}
				p := &transportErrSequence{defaultResults: map[string][]byte{"tools/call": makeCallToolResultForProbe("other")}}
				if mode == "initialize" {
					p.failsInit = true
				} else if dials == 2 || mode == "retry-exhausted" {
					p.sendErrOnTry = 1
					p.sendErr = io.ErrUnexpectedEOF
				}
				streams = append(streams, p)
				return p, nil
			})
			defer hub.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				body, err := hub.CallToolDedicatedWithTimeout(ctx, "devbox", "gate", map[string]any{"marker": "sibling"}, time.Second)
				if err == nil && !strings.Contains(body, "sibling") {
					err = fmt.Errorf("wrong sibling result: %s", body)
				}
				done <- err
			}()
			select {
			case <-sibling.started:
			case <-ctx.Done():
				t.Fatal("sibling did not start")
			}
			_, err := hub.CallToolDedicatedWithTimeout(ctx, "devbox", "gate", nil, 20*time.Millisecond)
			if (err == nil) != (mode == "retry") {
				t.Fatalf("mode %s: %v", mode, err)
			}
			if mode == "timeout" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("expected deadline: %v", err)
			}
			mu.Lock()
			wantDials := 3
			if mode == "timeout" || mode == "initialize" {
				wantDials = 2
			}
			if dials != wantDials {
				t.Errorf("dials = %d, want %d", dials, wantDials)
			}
			for _, p := range streams {
				p.mu.Lock()
				if !p.closed {
					t.Error("failed/retried stream not closed")
				}
				p.mu.Unlock()
			}
			mu.Unlock()
			sibling.mu.Lock()
			if sibling.closed {
				t.Error("sibling stream closed by failure")
			}
			sibling.mu.Unlock()
			// Release without closing: the deferred close also cleans up on failure.
			sibling.release <- struct{}{}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDedicatedCallCloseCancelsPendingDial(t *testing.T) {
	started := make(chan struct{})
	hub := newMCPHubClientWithDefaults(MCPHubConfig{}, func(ctx context.Context, _ string) (mcp.Transport, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	done := make(chan error, 1)
	go func() {
		_, err := hub.CallToolDedicatedWithTimeout(context.Background(), "devbox", "gate", nil, time.Second)
		done <- err
	}()
	<-started
	if err := hub.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not cancel dial")
	}
}

func TestDedicatedCallCancellationClosesTransport(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		t.Run(fmt.Sprintf("shutdown=%t", shutdown), func(t *testing.T) {
			probe := newConcurrencyProbeTransport()
			defer close(probe.release)
			hub := newMCPHubClientWithDefaults(MCPHubConfig{}, func(context.Context, string) (mcp.Transport, error) { return probe, nil })
			defer hub.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, err := hub.CallToolDedicatedWithTimeout(ctx, "devbox", "gate", nil, time.Second)
				done <- err
			}()
			select {
			case <-probe.started:
			case <-time.After(time.Second):
				t.Fatal("call did not start")
			}
			if shutdown {
				if err := hub.Close(); err != nil {
					t.Fatal(err)
				}
			} else {
				cancel()
			}
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("cancelled call succeeded")
				}
			case <-time.After(time.Second):
				t.Fatal("call did not stop")
			}
			probe.mu.Lock()
			defer probe.mu.Unlock()
			if !probe.closed {
				t.Fatal("transport leaked")
			}
		})
	}
}
