// Package netmode detects whether the host is on the home LAN or off it, so the
// daemon can route LAN-dependent MCP backends through the Cloudflare-tunnel hub
// when the laptop is remote.
//
// The home LAN exposes services (qdrant, *.lan, k8s) that are unreachable off the
// network. The MCP hub (wss://mcp.flexinfer.ai/ws) runs those backends
// cluster-side and is reachable from anywhere through Cloudflare Access, so
// off-LAN the daemon prefers the hub for otherwise LAN-pinned servers.
package netmode

import (
	"context"
	"net"
	"os"
	"strings"
	"time"
)

// Mode is the resolved network location of the host.
type Mode int

const (
	// LAN means the home LAN is reachable (use local backends).
	LAN Mode = iota
	// Tunnel means the host is off-LAN (route LAN-dependent servers via the hub).
	Tunnel
)

func (m Mode) String() string {
	if m == Tunnel {
		return "tunnel"
	}
	return "lan"
}

const (
	// defaultSentinel is the home nginx ingress VIP — always up on the LAN and
	// not routable from outside, so a fast TCP dial cleanly distinguishes on/off.
	defaultSentinel = "192.168.50.227:443"
	// dialTimeout bounds the sentinel probe so a CLI/daemon start never stalls.
	dialTimeout = 600 * time.Millisecond
)

// Resolve reports the current network mode.
//
// Order:
//  1. LOOM_NETWORK_MODE = lan | tunnel | auto (default auto) — explicit override.
//  2. auto: TCP-dial the LAN sentinel (LOOM_LAN_SENTINEL, default the home
//     ingress VIP). A successful connect within dialTimeout means LAN; any
//     failure (no route / timeout / refused) means Tunnel.
//
// Resolve performs a fresh probe on every call (no caching) so callers decide
// when to evaluate; a long-lived daemon should resolve once at startup and a
// restart re-detects after the laptop moves networks.
func Resolve() Mode {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("LOOM_NETWORK_MODE"))) {
	case "lan", "local", "on", "on-lan":
		return LAN
	case "tunnel", "remote", "off", "off-lan":
		return Tunnel
	case "", "auto":
		// fall through to probe
	default:
		// Unknown value: fall through to probe rather than guess.
	}
	return probe(sentinel())
}

// IsTunnel is a convenience wrapper for Resolve() == Tunnel.
func IsTunnel() bool { return Resolve() == Tunnel }

func sentinel() string {
	if s := strings.TrimSpace(os.Getenv("LOOM_LAN_SENTINEL")); s != "" {
		return s
	}
	return defaultSentinel
}

// probe TCP-dials addr; reachable within dialTimeout => LAN, else Tunnel.
func probe(addr string) Mode {
	ctx, cancel := context.WithTimeout(context.Background(), dialTimeout)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return Tunnel
	}
	_ = conn.Close()
	return LAN
}
