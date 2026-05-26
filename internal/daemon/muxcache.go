package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"

	mcp "gitlab.flexinfer.ai/libs/mcp-go"

	"github.com/crb2nu/loom/pkg/transport/muxstdio"
)

// perConnNextInternalID is the daemon-wide monotonic source for the
// per-conn internal JSON-RPC ids the perConnTransport stamps on outgoing
// messages before they reach the shared muxstdio.Transport. Allocating
// a unique id per Send guarantees concurrent perConnTransports cannot
// collide on the shared mux's pending map (which would surface as
// muxstdio.ErrDuplicateID and trigger an unnecessary subprocess
// restart cascade).
var perConnNextInternalID atomic.Int64

// muxCache owns one *muxstdio.Transport per local serverName. The cache is
// the authoritative owner of each mux's lifetime: every pool.Conn for the
// same serverName wraps the same shared mux behind a perConnTransport, so a
// pool.Put(Healthy=false) or an idle-pool overflow Close does not tear down
// the demuxer for other in-flight callers. The mux is closed only via
// Evict (from Daemon.stopServerProc) or CloseAll (from daemon shutdown).
//
// muxCache is internal to the daemon and is only consulted when the daemon
// was constructed with muxStdio enabled (LOOM_MUX_STDIO=1).
type muxCache struct {
	mu      sync.Mutex
	entries map[string]*muxstdio.Transport
	logger  *slog.Logger
}

func newMuxCache(logger *slog.Logger) *muxCache {
	if logger == nil {
		logger = slog.Default()
	}
	return &muxCache{
		entries: make(map[string]*muxstdio.Transport),
		logger:  logger,
	}
}

// GetOrCreate returns the cached mux for serverName, constructing one via
// muxstdio.New(inner, opts...) on first call. The inner transport is only
// captured on first call; subsequent callers receive the existing mux.
// Callers must pass the same shared *StdioTransport that kitprocess.Manager
// returns for serverName.
func (c *muxCache) GetOrCreate(serverName string, inner mcp.Transport, opts ...muxstdio.Option) *muxstdio.Transport {
	c.mu.Lock()
	defer c.mu.Unlock()
	if m, ok := c.entries[serverName]; ok {
		return m
	}
	m := muxstdio.New(inner, opts...)
	c.entries[serverName] = m
	return m
}

// Evict removes the cached mux for serverName and closes it. Safe to call
// for an unknown serverName (no-op). Closing the mux drains pending Recv
// waiters with ErrClosed and closes the inner stdio transport, which
// propagates EOF to the spawned process.
func (c *muxCache) Evict(serverName string) {
	c.mu.Lock()
	m, ok := c.entries[serverName]
	if ok {
		delete(c.entries, serverName)
	}
	c.mu.Unlock()
	if ok && m != nil {
		if err := m.Close(); err != nil {
			c.logger.Debug("muxcache: evict close error", "server", serverName, "err", err)
		}
	}
}

// CloseAll evicts every cached mux. Called during daemon shutdown.
func (c *muxCache) CloseAll() {
	c.mu.Lock()
	snapshot := make(map[string]*muxstdio.Transport, len(c.entries))
	for k, v := range c.entries {
		snapshot[k] = v
		delete(c.entries, k)
	}
	c.mu.Unlock()
	for name, m := range snapshot {
		if err := m.Close(); err != nil {
			c.logger.Debug("muxcache: shutdown close error", "server", name, "err", err)
		}
	}
}

// perConnTransport gives each pool.Conn a "single-call" view over the
// shared muxstdio.Transport for one serverName. The pipeline's call shape
// is Send-then-Recv on the same Conn.
//
// To prevent concurrent perConnTransports from colliding on the shared
// mux's pending-id map (which would surface as muxstdio.ErrDuplicateID
// and amplify a transient outage into a subprocess restart cascade), Send
// rewrites the outgoing message id to a process-unique value drawn from
// perConnNextInternalID and remembers the caller's original id. Recv
// waits on the internal id and restores the caller's id on the response
// so the upstream callpipeline's request/response id match still holds
// (see callpipeline_stages.go:433).
//
// Why this wrapper exists:
//   - muxstdio.Transport.Recv is id-aware (Recv(ctx, id)), but mcp.Transport
//     and the daemon pipeline are not — they call Transport.Recv(ctx).
//   - kitpool calls Transport.Close on unhealthy Put and on idle-pool
//     overflow; we cannot let those calls tear down the shared mux. Close
//     on the perConnTransport is a no-op; the shared mux is owned by
//     muxCache and closed via Evict.
//
// Concurrency: the daemon pipeline serializes Send→Recv per Conn (one in
// flight at a time on this wrapper). The shared mux underneath fans out
// across many perConnTransport instances, which is where the parallelism
// comes from.
type perConnTransport struct {
	mux *muxstdio.Transport

	mu         sync.Mutex
	callerID   any   // original id from the upstream caller; restored on Recv
	internalID int64 // unique id stamped onto the shared mux for routing
}

func newPerConnTransport(mux *muxstdio.Transport) *perConnTransport {
	return &perConnTransport{mux: mux}
}

func (p *perConnTransport) Send(ctx context.Context, msg *mcp.Message) error {
	if msg == nil {
		return fmt.Errorf("muxstdio: nil message")
	}

	internal := perConnNextInternalID.Add(1)
	// Shallow copy so we don't mutate the caller's message; only the ID
	// field is rewritten. Params/Result/Error byte slices and pointer
	// fields remain shared, which is safe because they are not mutated.
	rewritten := *msg
	rewritten.ID = internal

	p.mu.Lock()
	p.callerID = msg.ID
	p.internalID = internal
	p.mu.Unlock()

	if err := p.mux.Send(ctx, &rewritten); err != nil {
		// Send failed, no pending entry to wait on; clear state so a
		// follow-up Recv on this Conn does not block on a phantom id.
		p.mu.Lock()
		p.callerID = nil
		p.internalID = 0
		p.mu.Unlock()
		return err
	}
	return nil
}

func (p *perConnTransport) Recv(ctx context.Context) (*mcp.Message, error) {
	p.mu.Lock()
	callerID := p.callerID
	internalID := p.internalID
	p.callerID = nil
	p.internalID = 0
	p.mu.Unlock()
	if callerID == nil {
		return nil, fmt.Errorf("muxstdio: Recv without preceding Send on this conn")
	}
	resp, err := p.mux.Recv(ctx, internalID)
	if err != nil {
		return nil, err
	}
	if resp != nil {
		// Restore the caller's original id; the upstream callpipeline
		// asserts resp.ID == req.ID for transport-corruption defense.
		resp.ID = callerID
	}
	return resp, nil
}

func (p *perConnTransport) Close() error { return nil }
