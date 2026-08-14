package daemon

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/crb2nu/loom/internal/daemon/generation"
	"github.com/crb2nu/loom/internal/pool"
)

// localPoolCheckout is the only production ownership path for a checked-out
// local pool connection. A generation lease protects the physical process and
// mux for the entire Send/Recv interval, while Release fences stale logical
// views before returning them to the pool.
type localPoolCheckout struct {
	daemon     *Daemon
	ctx        context.Context
	serverName string
	conn       *pool.Conn
	lease      *generation.Lease
	generation uint64
	release    sync.Once
}

// checkoutLocalConnection gets a local logical transport and, when the daemon
// has a generation supervisor, leases the exact physical generation named by
// that transport. A stale idle view is discarded and replaced once.
func (d *Daemon) checkoutLocalConnection(ctx context.Context, serverName string) (*localPoolCheckout, error) {
	if d == nil || d.pool == nil {
		return nil, fmt.Errorf("local pool not configured")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	conn, err := d.pool.Get(ctx, serverName)
	if err != nil {
		return nil, err
	}
	if threshold := d.poolStaleThreshold(); threshold > 0 && time.Since(conn.LastUsed) > threshold {
		idle := time.Since(conn.LastUsed)
		if d.logger != nil {
			d.logger.Debug("discarding stale local pool connection",
				"server", serverName, "idle", idle.Round(time.Second), "threshold", threshold)
		}
		conn.Healthy = false
		d.pool.Put(conn)
		conn, err = d.pool.Get(ctx, serverName)
		if err != nil {
			return nil, fmt.Errorf("replace idle-stale local connection for %s: %w", serverName, err)
		}
	}
	checkout, err := d.bindLocalConnection(ctx, serverName, conn)
	if err == nil {
		return checkout, nil
	}

	// A pool can retain non-owning logical views after their physical
	// generation has retired. Reject the observed view, drop its idle peers,
	// and let the pool dial exactly one view of the current generation.
	conn.Healthy = false
	d.pool.Put(conn)
	d.pool.ClearServer(serverName)

	fresh, dialErr := d.pool.Get(ctx, serverName)
	if dialErr != nil {
		return nil, fmt.Errorf("replace stale local generation for %s: %w", serverName, dialErr)
	}
	checkout, leaseErr := d.bindLocalConnection(ctx, serverName, fresh)
	if leaseErr != nil {
		fresh.Healthy = false
		d.pool.Put(fresh)
		return nil, fmt.Errorf("lease replacement local generation for %s: %w", serverName, leaseErr)
	}
	return checkout, nil
}

func (d *Daemon) bindLocalConnection(ctx context.Context, serverName string, conn *pool.Conn) (*localPoolCheckout, error) {
	checkout := &localPoolCheckout{
		daemon:     d,
		ctx:        ctx,
		serverName: serverName,
		conn:       conn,
	}
	if d.serverSupervisor == nil || conn == nil {
		return checkout, nil
	}

	generationID, tagged := transportGeneration(conn.Transport)
	if !tagged {
		// Compatibility for manually assembled test daemons. Production local
		// transports are always generation tagged by serverSupervisor.
		return checkout, nil
	}
	lease, err := d.serverSupervisor.acquireLease(serverName, generationID)
	if err != nil {
		return nil, fmt.Errorf("lease local generation %d: %w", generationID, err)
	}
	checkout.lease = lease
	checkout.generation = generationID
	return checkout, nil
}

func (c *localPoolCheckout) markUnhealthy() {
	if c != nil && c.conn != nil {
		c.conn.Healthy = false
	}
}

// failObservedGeneration marks the logical view unhealthy and retires only
// the physical generation leased by this checkout. Mux caller cancellation is
// call-local because the request registration is removed and late responses
// are discarded; every other send/recv failure means the observed physical
// transport is no longer safe to reuse.
func (c *localPoolCheckout) failObservedGeneration(cause error) {
	if c == nil {
		return
	}
	c.markUnhealthy()
	if c.daemon == nil || c.daemon.serverSupervisor == nil || c.generation == 0 {
		return
	}
	if c.daemon.muxStdio && c.ctx != nil && c.ctx.Err() != nil {
		return
	}
	_, err := c.daemon.failServerGeneration(c.serverName, c.generation, cause)
	if err != nil && c.daemon.logger != nil {
		c.daemon.logger.Warn("failed to retire probed local generation",
			"server", c.serverName, "generation", c.generation, "error", err)
	}
}

// close returns the logical view before releasing its generation lease. That
// ordering prevents idle retirement between the current-generation fence and
// pool.Put. A cancelled call or retired generation is never returned healthy.
func (c *localPoolCheckout) close() {
	if c == nil {
		return
	}
	c.release.Do(func() {
		if c.conn != nil {
			if c.ctx != nil && c.ctx.Err() != nil {
				c.conn.Healthy = false
			}
			if c.generation != 0 && c.daemon != nil && c.daemon.serverSupervisor != nil &&
				!c.daemon.serverSupervisor.currentReady(c.serverName, c.generation) {
				c.conn.Healthy = false
			}
			if c.daemon != nil && c.daemon.pool != nil {
				c.daemon.pool.Put(c.conn)
			} else if c.conn.Transport != nil {
				_ = c.conn.Transport.Close()
			}
			c.conn = nil
		}
		if c.lease != nil {
			c.lease.Release()
			c.lease = nil
		}
	})
}
