package muxstdio_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	mcp "gitlab.flexinfer.ai/libs/mcp-go"

	"github.com/crb2nu/loom/pkg/transport/muxstdio"
)

// failingRecvTransport blocks Recv until gate closes, then returns recvErr.
// Simulates a remote peer (hub WS, subprocess stdio) dying with a specific
// error while calls are in flight.
type failingRecvTransport struct {
	recvErr error
	gate    chan struct{}
}

func (f *failingRecvTransport) Send(ctx context.Context, msg *mcp.Message) error { return nil }

func (f *failingRecvTransport) Recv(ctx context.Context) (*mcp.Message, error) {
	select {
	case <-f.gate:
		return nil, f.recvErr
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (f *failingRecvTransport) Close() error { return nil }

func TestReadLoopError_AnnotatesErrClosedWithCause(t *testing.T) {
	cause := errors.New("websocket: close 1006 (abnormal closure): unexpected EOF")
	inner := &failingRecvTransport{recvErr: cause, gate: make(chan struct{})}
	tr := muxstdio.New(inner)
	t.Cleanup(func() { _ = tr.Close() })

	// Register an in-flight call, then fail the reader.
	if err := tr.Send(context.Background(), newRequest(1, "tools/call")); err != nil {
		t.Fatalf("Send: %v", err)
	}
	close(inner.gate)

	_, err := tr.Recv(context.Background(), 1)
	if !errors.Is(err, muxstdio.ErrClosed) {
		t.Fatalf("Recv after reader failure: got err=%v, want errors.Is ErrClosed", err)
	}
	if !strings.Contains(err.Error(), "transport closed") {
		t.Fatalf("Recv error %q lost the transport-closed substring the daemon matches on", err)
	}
	if !strings.Contains(err.Error(), "close 1006") {
		t.Fatalf("Recv error %q does not carry the underlying close cause", err)
	}

	// Subsequent Sends carry the cause too.
	err = tr.Send(context.Background(), newRequest(2, "tools/call"))
	if !errors.Is(err, muxstdio.ErrClosed) || !strings.Contains(err.Error(), "close 1006") {
		t.Fatalf("Send after reader failure: got err=%v, want annotated ErrClosed", err)
	}
}

func TestClose_NoCauseOnDeliberateClose(t *testing.T) {
	client, _ := newPair(t)
	if err := client.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	err := client.Send(context.Background(), newRequest(1, "tools/call"))
	if !errors.Is(err, muxstdio.ErrClosed) {
		t.Fatalf("Send after Close: got err=%v, want ErrClosed", err)
	}
	if strings.Contains(err.Error(), "cause:") {
		t.Fatalf("deliberate Close must not be annotated with a cause, got %q", err)
	}
}
