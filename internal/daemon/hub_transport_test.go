package daemon

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gitlab.flexinfer.ai/libs/mcp-go"
)

type scriptedOwnedHubInner struct {
	mu       sync.Mutex
	messages []*mcp.Message
	closed   atomic.Int64
}

func (s *scriptedOwnedHubInner) Send(context.Context, *mcp.Message) error { return nil }

func (s *scriptedOwnedHubInner) Recv(context.Context) (*mcp.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.messages) == 0 {
		return nil, io.EOF
	}
	message := s.messages[0]
	s.messages = s.messages[1:]
	return message, nil
}

func (s *scriptedOwnedHubInner) Close() error {
	s.closed.Add(1)
	return nil
}

func TestOwnedHubTransport_DivertsNotificationsAndReturnsResponse(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	d := &Daemon{
		logger:   logger,
		eventBus: NewEventBus(logger),
	}
	subscriptionID, events := d.eventBus.Subscribe()
	t.Cleanup(func() { d.eventBus.Unsubscribe(subscriptionID) })
	t.Cleanup(d.stopToolRefresh)

	inner := &scriptedOwnedHubInner{messages: []*mcp.Message{
		{JSONRPC: mcp.JSONRPCVersion, Method: "notifications/tools/list_changed"},
		{JSONRPC: mcp.JSONRPCVersion, Method: "notifications/progress"},
		{JSONRPC: mcp.JSONRPCVersion, ID: "call-1", Result: []byte(`{"ok":true}`)},
	}}
	registry := newHubTransportRegistry()
	transport, err := registry.Track("agent_context", inner, d.handleHubNotification)
	if err != nil {
		t.Fatalf("track transport: %v", err)
	}
	t.Cleanup(func() { _ = registry.CloseAll() })

	response, err := transport.Recv(context.Background())
	if err != nil {
		t.Fatalf("recv response: %v", err)
	}
	if response.ID != "call-1" || string(response.Result) != `{"ok":true}` {
		t.Fatalf("response = id:%v result:%s, want call-1 ok", response.ID, response.Result)
	}

	select {
	case event := <-events:
		if event.Type != EventToolsChanged {
			t.Fatalf("event type = %q, want %q", event.Type, EventToolsChanged)
		}
		notification := eventToMCPNotification(event)
		if notification == nil || notification.Method != "notifications/tools/list_changed" {
			t.Fatalf("forwarded notification = %+v", notification)
		}
	case <-time.After(time.Second):
		t.Fatal("tools/list_changed notification was not delivered through EventBus")
	}

	select {
	case unexpected := <-events:
		t.Fatalf("unsupported request-scoped notification was broadcast: %+v", unexpected)
	default:
	}
}

func TestHubTransportRegistry_AssignsMonotonicPhysicalGenerations(t *testing.T) {
	registry := newHubTransportRegistry()
	first, err := registry.Track("agent_context", &scriptedOwnedHubInner{}, nil)
	if err != nil {
		t.Fatalf("track first transport: %v", err)
	}
	second, err := registry.Track("agent_context", &scriptedOwnedHubInner{}, nil)
	if err != nil {
		t.Fatalf("track second transport: %v", err)
	}
	t.Cleanup(func() { _ = registry.CloseAll() })

	firstGeneration := first.(*ownedHubTransport).serverGeneration()
	secondGeneration := second.(*ownedHubTransport).serverGeneration()
	if firstGeneration == 0 || secondGeneration <= firstGeneration {
		t.Fatalf("physical generations = (%d, %d), want positive monotonic values", firstGeneration, secondGeneration)
	}
}

type blockingOwnedHubInner struct {
	started   chan struct{}
	closed    chan struct{}
	startOnce sync.Once
	closeOnce sync.Once
	closes    atomic.Int64
}

func newBlockingOwnedHubInner() *blockingOwnedHubInner {
	return &blockingOwnedHubInner{
		started: make(chan struct{}),
		closed:  make(chan struct{}),
	}
}

func (b *blockingOwnedHubInner) Send(context.Context, *mcp.Message) error { return nil }

func (b *blockingOwnedHubInner) Recv(ctx context.Context) (*mcp.Message, error) {
	b.startOnce.Do(func() { close(b.started) })
	select {
	case <-b.closed:
		return nil, io.ErrClosedPipe
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (b *blockingOwnedHubInner) Close() error {
	b.closes.Add(1)
	b.closeOnce.Do(func() { close(b.closed) })
	return nil
}

func TestDaemonStop_ClosesActiveHubTransportBeforeWait(t *testing.T) {
	registry := newHubTransportRegistry()
	inner := newBlockingOwnedHubInner()
	transport, err := registry.Track("agent_context", inner, nil)
	if err != nil {
		t.Fatalf("track transport: %v", err)
	}

	d := &Daemon{
		done:          make(chan struct{}),
		logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		hubTransports: registry,
	}
	recvDone := make(chan error, 1)
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		_, recvErr := transport.Recv(context.Background())
		recvDone <- recvErr
	}()
	<-inner.started

	stopDone := make(chan error, 1)
	go func() { stopDone <- d.Stop() }()
	select {
	case stopErr := <-stopDone:
		if stopErr != nil {
			t.Fatalf("stop daemon: %v", stopErr)
		}
	case <-time.After(time.Second):
		t.Fatal("daemon Stop waited on an active hub Recv instead of closing it")
	}

	select {
	case recvErr := <-recvDone:
		if !errors.Is(recvErr, io.ErrClosedPipe) {
			t.Fatalf("active Recv error = %v, want closed pipe", recvErr)
		}
	default:
		t.Fatal("active hub Recv did not unblock during Stop")
	}
	if got := inner.closes.Load(); got != 1 {
		t.Fatalf("physical transport close count = %d, want 1", got)
	}

	late := newBlockingOwnedHubInner()
	if _, trackErr := registry.Track("late", late, nil); !errors.Is(trackErr, errHubTransportRegistryClosed) {
		t.Fatalf("late Track error = %v, want registry closed", trackErr)
	}
	if got := late.closes.Load(); got != 1 {
		t.Fatalf("late physical transport close count = %d, want 1", got)
	}
}
