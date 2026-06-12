package muxstdio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"

	mcp "gitlab.flexinfer.ai/libs/mcp-go"
)

// Sentinel errors.
var (
	// ErrClosed is returned by Send and Recv after Close has been called or the
	// inner transport's reader has reached EOF.
	ErrClosed = errors.New("muxstdio: transport closed")

	// ErrNoID is returned by Send when the message has a nil JSON-RPC id.
	// Notifications (id-less messages) cannot be routed back to a caller and
	// must be sent through the underlying transport directly.
	ErrNoID = errors.New("muxstdio: cannot Send message with nil id; use the inner transport for notifications")

	// ErrDuplicateID is returned by Send when another in-flight call is already
	// registered for the same id. Callers must pick unique ids for concurrent
	// requests.
	ErrDuplicateID = errors.New("muxstdio: duplicate in-flight id")
)

// notifyChanBuffer bounds the unsolicited-notification channel. Larger than 1
// because callers may not poll NotificationCh tightly; small enough that
// runaway servers do not blow memory before the drop-and-meter path kicks in.
const notifyChanBuffer = 16

// Transport multiplexes JSON-RPC requests over a single underlying mcp.Transport
// by routing inbound messages to per-id channels.
//
// Construct with [New]. Concurrent use of Send is safe; concurrent use of Recv
// for the same id is not (Recv removes the pending entry on return, so a
// second Recv for the same id after the first returns is a programming
// error). NotificationCh is safe to read from a single goroutine.
type Transport struct {
	inner mcp.Transport

	mu         sync.Mutex
	pending    map[string]chan *mcp.Message
	closed     bool
	closeCause error // first inner.Recv error observed by readLoop; nil on deliberate Close

	notifyCh chan *mcp.Message

	readerCtx    context.Context
	readerCancel context.CancelFunc
	done         chan struct{}
	closeOnce    sync.Once
	readerWg     sync.WaitGroup

	metrics Metrics
	logger  *slog.Logger
}

// New wraps inner in a per-id demuxing layer. The returned Transport spawns
// one background reader goroutine that lives until [Transport.Close] is
// called or inner.Recv returns an error.
func New(inner mcp.Transport, opts ...Option) *Transport {
	ctx, cancel := context.WithCancel(context.Background())
	t := &Transport{
		inner:        inner,
		pending:      make(map[string]chan *mcp.Message),
		notifyCh:     make(chan *mcp.Message, notifyChanBuffer),
		readerCtx:    ctx,
		readerCancel: cancel,
		done:         make(chan struct{}),
		metrics:      nopMetrics{},
		logger:       slog.Default(),
	}
	for _, opt := range opts {
		opt(t)
	}
	t.readerWg.Add(1)
	go t.readLoop()
	return t
}

// Send registers a pending channel for msg.ID and then forwards the message
// to the inner transport. The caller must invoke [Transport.Recv] with the
// same id to receive the response (or to clean up if the call is cancelled).
//
// If inner.Send fails the pending registration is removed before returning.
func (t *Transport) Send(ctx context.Context, msg *mcp.Message) error {
	if msg == nil {
		return fmt.Errorf("muxstdio: nil message")
	}
	key := idKey(msg.ID)
	if key == "" {
		return ErrNoID
	}

	ch := make(chan *mcp.Message, 1)

	t.mu.Lock()
	if t.closed {
		err := t.closedErrLocked()
		t.mu.Unlock()
		return err
	}
	if _, dup := t.pending[key]; dup {
		t.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrDuplicateID, key)
	}
	t.pending[key] = ch
	t.mu.Unlock()

	if err := t.inner.Send(ctx, msg); err != nil {
		t.mu.Lock()
		delete(t.pending, key)
		t.mu.Unlock()
		return err
	}
	return nil
}

// Recv blocks until the response for id arrives, ctx is cancelled, or the
// transport is closed. The pending registration is removed before Recv
// returns, regardless of outcome.
func (t *Transport) Recv(ctx context.Context, id any) (*mcp.Message, error) {
	key := idKey(id)
	if key == "" {
		return nil, ErrNoID
	}

	t.mu.Lock()
	ch, ok := t.pending[key]
	t.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("muxstdio: no pending Send for id %s", key)
	}

	defer func() {
		t.mu.Lock()
		delete(t.pending, key)
		t.mu.Unlock()
	}()

	select {
	case msg, open := <-ch:
		if !open || msg == nil {
			return nil, t.closedErr()
		}
		return msg, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-t.done:
		return nil, t.closedErr()
	}
}

// closedErr returns ErrClosed annotated with the underlying read error when
// the close was caused by the inner transport (e.g. a WebSocket close code),
// so callers and logs can see WHY the transport died. errors.Is(err, ErrClosed)
// holds on both paths, and the "transport closed" substring used by the
// daemon's transport-failure detection is preserved by the %w wrap.
func (t *Transport) closedErr() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.closedErrLocked()
}

func (t *Transport) closedErrLocked() error {
	if t.closeCause != nil {
		return fmt.Errorf("%w (cause: %v)", ErrClosed, t.closeCause)
	}
	return ErrClosed
}

// Call is a convenience wrapper that issues Send and then Recv with the same
// id. It exists so that pool.Conn integration (slice 3) and test code don't
// have to repeat the Send/Recv pair boilerplate.
func (t *Transport) Call(ctx context.Context, msg *mcp.Message) (*mcp.Message, error) {
	if err := t.Send(ctx, msg); err != nil {
		return nil, err
	}
	return t.Recv(ctx, msg.ID)
}

// NotificationCh returns the channel of id-less messages received from the
// inner transport. The channel is buffered; the wrapper drops messages and
// bumps a metric if the channel is full.
//
// The channel is closed after Close completes; readers will see a closed
// channel and should exit.
func (t *Transport) NotificationCh() <-chan *mcp.Message {
	return t.notifyCh
}

// Close stops the reader goroutine, closes the inner transport, and delivers
// ErrClosed to every Recv caller still blocked on a pending response. Safe to
// call multiple times.
func (t *Transport) Close() error {
	var innerErr error
	t.closeOnce.Do(func() {
		t.mu.Lock()
		t.closed = true
		t.mu.Unlock()

		// Cancel the reader context first so inner.Recv unblocks regardless
		// of how the inner transport handles Close-during-Recv (StdioTransport
		// honors Close; PipeTransport does not).
		t.readerCancel()
		close(t.done)
		innerErr = t.inner.Close()
		t.readerWg.Wait()
		t.drainPending()
		close(t.notifyCh)
	})
	return innerErr
}

// drainPending delivers ErrClosed to every caller blocked in Recv. Called
// during Close after the reader has stopped, so no new pending entries can
// be registered.
func (t *Transport) drainPending() {
	t.mu.Lock()
	defer t.mu.Unlock()
	for k, ch := range t.pending {
		close(ch)
		delete(t.pending, k)
	}
}

// readLoop drains the inner transport and dispatches messages to per-id
// channels (or NotificationCh for id-less messages).
func (t *Transport) readLoop() {
	defer t.readerWg.Done()
	for {
		msg, err := t.inner.Recv(t.readerCtx)
		if err != nil {
			// EOF / transport closed. Any Recv waiters are drained by
			// Close.drainPending; if the reader observed err before Close was
			// called, mark the transport closed so future Sends fail fast.
			t.mu.Lock()
			deliberate := t.closed // Close() sets closed before cancelling the reader
			t.closed = true
			if !deliberate && t.closeCause == nil {
				t.closeCause = err
			}
			pendingFailed := len(t.pending)
			// Wake any waiters now, in case Close is never called.
			for k, ch := range t.pending {
				close(ch)
				delete(t.pending, k)
			}
			t.mu.Unlock()
			if !deliberate {
				t.logger.Warn("muxstdio: inner transport closed",
					slog.Any("error", err),
					slog.Int("pending_failed", pendingFailed))
			}
			return
		}
		if msg == nil {
			continue
		}

		key := idKey(msg.ID)
		if key == "" {
			// Notification path: non-blocking send; drop on full.
			select {
			case t.notifyCh <- msg:
				t.metrics.IncMuxNotifications()
			default:
				t.metrics.IncMuxDropsFullChan()
				t.logger.Warn("muxstdio: dropped notification (channel full)",
					slog.String("method", msg.Method))
			}
			continue
		}

		t.mu.Lock()
		ch, ok := t.pending[key]
		t.mu.Unlock()
		if !ok {
			// No registered waiter (caller cancelled before response arrived,
			// or server sent an unsolicited response).
			t.metrics.IncMuxDropsNoPending()
			t.logger.Warn("muxstdio: dropped response (no pending waiter)",
				slog.String("id", key))
			continue
		}
		select {
		case ch <- msg:
			t.metrics.IncMuxDispatches()
		default:
			// Caller pre-registered but never Recv'd; chan is buffer-1 and
			// already holds one message. Should never happen with the
			// Send/Recv contract; defensive guard.
			t.metrics.IncMuxDropsFullChan()
			t.logger.Warn("muxstdio: dropped response (per-id channel full)",
				slog.String("id", key))
		}
	}
}

// idKey normalizes a JSON-RPC id (which is `any` in mcp.Message) to a
// canonical string key. JSON-unmarshaled numbers come back as float64; ids
// sent as int / int64 / json.Number must hash to the same key as the
// matching float64 to route correctly.
func idKey(id any) string {
	if id == nil {
		return ""
	}
	switch v := id.(type) {
	case string:
		return "s:" + v
	case float64:
		if v == float64(int64(v)) {
			return "n:" + strconv.FormatInt(int64(v), 10)
		}
		return "n:" + strconv.FormatFloat(v, 'f', -1, 64)
	case int:
		return "n:" + strconv.Itoa(v)
	case int64:
		return "n:" + strconv.FormatInt(v, 10)
	case json.Number:
		if i, err := v.Int64(); err == nil {
			return "n:" + strconv.FormatInt(i, 10)
		}
		return "n:" + v.String()
	default:
		return fmt.Sprintf("o:%v", v)
	}
}
