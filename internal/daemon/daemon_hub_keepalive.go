package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"gitlab.flexinfer.ai/libs/mcp-go"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/crb2nu/loom/internal/hubproto"
	loomtransport "github.com/crb2nu/loom/pkg/transport"
)

// hubKeepaliveLoop periodically probes the hub WebSocket connection to detect
// dead connections before agents encounter them during tool calls.
// It uses the configured PingIntervalSeconds (default 30s) as the probe interval.
func (d *Daemon) hubKeepaliveLoop() {
	defer d.wg.Done()

	interval := time.Duration(d.fileCfg.Hub.PingIntervalSeconds) * time.Second
	if interval <= 0 {
		interval = 30 * time.Second
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-d.done:
			return
		case <-ticker.C:
			d.hubKeepalivePing()
		}
	}
}

// hubKeepalivePing borrows a connection from the hub pool, sends a lightweight
// tools/list probe wrapped in a DomainControl envelope, and observes the result.
// On failure, only the independently owned connection is marked unhealthy.
// For backward compatibility, it gracefully handles raw (non-envelope) pong responses.
func (d *Daemon) hubKeepalivePing() {
	if d.hubPool == nil || d.hubClient == nil {
		return
	}

	// Use a well-known server name from the hub pool stats to probe.
	// If the pool has no idle connections, skip — nothing to keep alive.
	stats := d.hubPool.Stats()
	if stats.IdleConns == 0 {
		return
	}

	// Pick any hub-capable server from the registry to probe.
	serverName := d.pickHubServer()
	if serverName == "" {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, span := d.daemonTracer().Start(ctx, "daemon.hub.keepalive",
		trace.WithAttributes(attribute.String("mcp.server", serverName)),
	)
	defer span.End()

	conn, err := d.hubPool.Get(ctx, serverName)
	if err != nil {
		span.AddEvent("daemon.hub.keepalive.skip", trace.WithAttributes(
			attribute.String("reason", "no connection available"),
		))
		d.logger.Debug("hub keepalive: no connection available", "server", serverName, "error", err)
		return
	}

	owned, ok := conn.Transport.(*ownedHubTransport)
	if !ok {
		d.logger.Warn("hub keepalive: transport lacks correlated liveness state; using compatibility downgrade",
			"server", serverName)
		d.hubPool.Put(conn)
		return
	}
	maxMissed := d.fileCfg.Hub.MaxMissedPongs
	if maxMissed <= 0 {
		maxMissed = 2
	}
	pingID, healthy := owned.liveness.Ping(maxMissed)
	if !healthy {
		span.AddEvent("daemon.hub.keepalive.missed_pong_threshold")
		d.logger.Warn("hub keepalive: missed pong threshold reached, retiring owning connection",
			"server", serverName)
		conn.Healthy = false
		d.hubPool.Put(conn)
		return
	}

	pingEnv := d.buildControlPing(pingID)

	sendCtx, sendCancel := context.WithTimeout(ctx, 5*time.Second)
	sendErr := conn.Transport.Send(sendCtx, pingEnv)
	sendCancel()
	if sendErr != nil {
		span.AddEvent("daemon.hub.keepalive.send_fail", trace.WithAttributes(
			attribute.String("error", sendErr.Error()),
		))
		d.logger.Warn("hub keepalive: send failed, retiring owning connection",
			"server", serverName, "error", sendErr)
		conn.Healthy = false
		d.hubPool.Put(conn)
		return
	}

	recvCtx, recvCancel := context.WithTimeout(ctx, 5*time.Second)
	resp, recvErr := conn.Transport.Recv(recvCtx)
	recvCancel()
	if recvErr != nil {
		if errors.Is(recvErr, context.DeadlineExceeded) || errors.Is(recvErr, context.Canceled) {
			span.AddEvent("daemon.hub.keepalive.pong_absent")
			d.logger.Warn("hub keepalive: pong absent; retaining connection until miss threshold",
				"server", serverName, "ping_id", pingID)
			d.hubPool.Put(conn)
			return
		}
		span.AddEvent("daemon.hub.keepalive.recv_fail", trace.WithAttributes(
			attribute.String("error", recvErr.Error()),
		))
		d.logger.Warn("hub keepalive: recv failed, retiring owning connection",
			"server", serverName, "error", recvErr)
		conn.Healthy = false
		d.hubPool.Put(conn)
		return
	}

	// Backward compat is retained as an explicit, observable downgrade.
	correlated, downgraded := d.handlePongResponse(resp, &owned.liveness)
	if !correlated && !downgraded {
		span.AddEvent("daemon.hub.keepalive.pong_mismatch")
		d.logger.Warn("hub keepalive: response did not match outstanding ping",
			"server", serverName, "ping_id", pingID)
		d.hubPool.Put(conn)
		return
	}

	// Success: return healthy connection to pool (keeps it warm).
	span.AddEvent("daemon.hub.keepalive.success")
	d.hubPool.Put(conn)
	d.logger.Debug("hub keepalive: probe succeeded", "server", serverName)
}

// buildControlPing creates a uniquely correlated control ping and wraps it in
// the MCP message shape used by the existing WebSocket transport.
func (d *Daemon) buildControlPing(pingID string) *mcp.Message {
	envBytes, err := hubproto.Encode(hubproto.NewPing(pingID, "daemon", time.Now()))
	if err != nil {
		d.logger.Error("hub keepalive: failed to encode ping envelope", "error", err)
		return nil
	}

	// Wrap the envelope JSON as the params of a synthetic MCP message so it
	// can be sent over the existing MCP transport layer.
	msg, _ := mcp.NewRequest(0, "hub/envelope", json.RawMessage(envBytes))
	return msg
}

// handlePongResponse processes a keepalive response, accepting both
// envelope-wrapped pongs and raw MCP responses for backward compatibility.
func (d *Daemon) handlePongResponse(resp *mcp.Message, liveness *loomtransport.Liveness) (correlated, downgraded bool) {
	if resp == nil {
		return false, false
	}

	// Try to decode as an envelope response. If the hub supports envelopes,
	// the response will be a hub/envelope method with an envelope payload.
	if (resp.Method == "" || resp.Method == "hub/envelope") && resp.Result != nil {
		env, err := hubproto.Decode(resp.Result)
		if err == nil && env.Domain == hubproto.DomainControl {
			pongID, parseErr := hubproto.ParsePong(env)
			if parseErr != nil || liveness == nil || !liveness.Pong(pongID) {
				return false, false
			}
			d.logger.Debug("hub keepalive: received envelope pong",
				"method", env.Method, "request_id", env.RequestID)
			return true, false
		}
	}

	// Old hubs cannot correlate raw responses. Reset explicitly and warn so this
	// weaker compatibility mode is never mistaken for correlated health.
	if liveness != nil {
		liveness.Reset()
	}
	d.logger.Warn("hub keepalive: accepting uncorrelated raw response (compatibility downgrade)")
	return false, true
}

// pickHubServer returns the name of a hub-capable server from the registry.
// Returns "" if no hub servers exist.
func (d *Daemon) pickHubServer() string {
	reg := d.currentRegistry()
	if reg == nil {
		return ""
	}
	for _, srv := range reg.Servers {
		if srv == nil || srv.IsLocalOnly() {
			continue
		}
		return srv.Name
	}
	return ""
}
