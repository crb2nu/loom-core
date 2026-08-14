package muxstdio

import (
	"context"
	"encoding/json"
	"io"
	"sync"
	"testing"

	mcp "gitlab.flexinfer.ai/libs/mcp-go"
)

type fleetBenchmarkTransport struct {
	responses chan *mcp.Message
	done      chan struct{}
	closeOnce sync.Once
}

func newFleetBenchmarkTransport() *fleetBenchmarkTransport {
	return &fleetBenchmarkTransport{
		responses: make(chan *mcp.Message, 1),
		done:      make(chan struct{}),
	}
}

func (t *fleetBenchmarkTransport) Send(ctx context.Context, message *mcp.Message) error {
	response := &mcp.Message{JSONRPC: mcp.JSONRPCVersion, ID: message.ID, Result: json.RawMessage(`{"ok":true}`)}
	select {
	case t.responses <- response:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-t.done:
		return io.EOF
	}
}

func (t *fleetBenchmarkTransport) Recv(ctx context.Context) (*mcp.Message, error) {
	select {
	case response := <-t.responses:
		return response, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-t.done:
		return nil, io.EOF
	}
}

func (t *fleetBenchmarkTransport) Close() error {
	t.closeOnce.Do(func() { close(t.done) })
	return nil
}

func BenchmarkFleetMuxRoundTrip(b *testing.B) {
	inner := newFleetBenchmarkTransport()
	transport := New(inner)
	b.Cleanup(func() { _ = transport.Close() })
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		message := &mcp.Message{JSONRPC: mcp.JSONRPCVersion, ID: i, Method: "tools/list"}
		if _, err := transport.Call(ctx, message); err != nil {
			b.Fatalf("round trip: %v", err)
		}
	}
}
