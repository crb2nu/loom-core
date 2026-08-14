package daemon

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"gitlab.flexinfer.ai/libs/mcp-go"

	loomtransport "github.com/crb2nu/loom/pkg/transport"
)

var errHubTransportRegistryClosed = errors.New("hub transport registry closed")

// hubTransportRegistry owns every physical hub transport created for the pool.
// The pool closes idle transports; the registry closes any transports still
// checked out by active calls during daemon shutdown.
type hubTransportRegistry struct {
	mu         sync.Mutex
	closed     bool
	transports map[*ownedHubTransport]struct{}
	next       atomic.Uint64
}

func newHubTransportRegistry() *hubTransportRegistry {
	return &hubTransportRegistry{transports: make(map[*ownedHubTransport]struct{})}
}

// Track wraps one independently dialed physical transport. There is no
// server-name cache: each pool dial remains independently owned and closed.
func (r *hubTransportRegistry) Track(
	serverName string,
	inner mcp.Transport,
	onNotification func(string, *mcp.Message),
) (mcp.Transport, error) {
	if inner == nil {
		return nil, fmt.Errorf("track hub transport for %s: nil transport", serverName)
	}

	tracked := &ownedHubTransport{
		serverName:     serverName,
		generation:     r.next.Add(1),
		inner:          inner,
		registry:       r,
		onNotification: onNotification,
	}

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		_ = inner.Close()
		return nil, errHubTransportRegistryClosed
	}
	r.transports[tracked] = struct{}{}
	r.mu.Unlock()
	return tracked, nil
}

func (r *hubTransportRegistry) remove(transport *ownedHubTransport) {
	if r == nil || transport == nil {
		return
	}
	r.mu.Lock()
	delete(r.transports, transport)
	r.mu.Unlock()
}

// CloseAll rejects late registrations and closes every physical transport
// still owned by the daemon. Close is idempotent at the wrapper boundary, so
// transports already closed by the pool are harmless here.
func (r *hubTransportRegistry) CloseAll() error {
	if r == nil {
		return nil
	}

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	transports := make([]*ownedHubTransport, 0, len(r.transports))
	for transport := range r.transports {
		transports = append(transports, transport)
	}
	r.mu.Unlock()

	var closeErrs []error
	for _, transport := range transports {
		if err := transport.Close(); err != nil {
			closeErrs = append(closeErrs, err)
		}
	}
	return errors.Join(closeErrs...)
}

// ownedHubTransport preserves the one-physical-connection-per-pool-entry
// contract while keeping unsolicited notifications out of the response stream.
// A pool entry is checked out by at most one caller, so no shared response mux
// is needed; the call pipeline still validates the returned response ID.
type ownedHubTransport struct {
	serverName     string
	generation     uint64
	inner          mcp.Transport
	registry       *hubTransportRegistry
	onNotification func(string, *mcp.Message)

	closeOnce sync.Once
	closeErr  error
	liveness  loomtransport.Liveness
}

// serverGeneration identifies this independently owned physical WebSocket.
// It is intentionally per transport rather than per server name: hub pool
// entries do not share a socket, so one stale failure must never retire a
// sibling connection.
func (t *ownedHubTransport) serverGeneration() uint64 {
	if t == nil {
		return 0
	}
	return t.generation
}

func (t *ownedHubTransport) Send(ctx context.Context, message *mcp.Message) error {
	return t.inner.Send(ctx, message)
}

func (t *ownedHubTransport) Recv(ctx context.Context) (*mcp.Message, error) {
	for {
		message, err := t.inner.Recv(ctx)
		if err != nil {
			return nil, err
		}
		if message == nil {
			return nil, fmt.Errorf("hub transport for %s received nil message", t.serverName)
		}
		if message.ID != nil {
			return message, nil
		}
		if t.onNotification != nil {
			t.onNotification(t.serverName, message)
		}
	}
}

func (t *ownedHubTransport) Close() error {
	t.closeOnce.Do(func() {
		if t.registry != nil {
			t.registry.remove(t)
		}
		t.closeErr = t.inner.Close()
	})
	return t.closeErr
}

// handleHubNotification routes only broadcast-safe MCP list-change
// notifications through the daemon's existing EventBus notification path.
// Request-scoped notifications (for example progress) need an originating
// proxy/session channel and are deliberately not broadcast fleet-wide.
func (d *Daemon) handleHubNotification(serverName string, message *mcp.Message) {
	if d == nil || message == nil {
		return
	}

	data := map[string]any{
		"server": serverName,
		"method": message.Method,
	}
	switch message.Method {
	case "notifications/tools/list_changed":
		d.scheduleToolRefresh()
		if d.eventBus != nil {
			d.eventBus.Publish(EventToolsChanged, data)
		}
	case "notifications/resources/list_changed":
		if d.eventBus != nil {
			d.eventBus.Publish(EventResourcesChanged, data)
		}
	default:
		if d.logger != nil {
			d.logger.Debug("dropping unsupported hub notification",
				"server", serverName,
				"method", message.Method)
		}
	}
}
