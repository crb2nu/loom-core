package daemon

import (
	"errors"
	"fmt"
	"io"
	"net"
	"syscall"
	"testing"
)

func TestIsClientGoneErr(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"epipe wrapped", fmt.Errorf("write body: %w", syscall.EPIPE), true},
		{"econnreset wrapped", fmt.Errorf("send: %w", syscall.ECONNRESET), true},
		{"net closed", fmt.Errorf("x: %w", net.ErrClosed), true},
		{"closed pipe", io.ErrClosedPipe, true},
		// The daemon's stdio transport wraps the OS error as text; this is the
		// exact shape from the 2026-09-13 log.
		{"broken pipe text", errors.New("write body: write unix /Users/x/.config/loom/loom.sock->: write: broken pipe"), true},
		{"connection reset text", errors.New("write tcp 127.0.0.1:1->127.0.0.1:2: write: connection reset by peer"), true},
		{"closed network text", errors.New("use of closed network connection"), true},
		{"unrelated", errors.New("json: unsupported type"), false},
		{"timeout is not gone", errors.New("i/o timeout"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := isClientGoneErr(tc.err); got != tc.want {
				t.Fatalf("isClientGoneErr(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
