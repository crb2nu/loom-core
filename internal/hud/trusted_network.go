package hud

import (
	"net"
	"net/http"
	"strings"
)

// trusted_network.go — LAN trusted-network admin.
//
// Off-LAN the HUD is reached through Cloudflare Access (verified SSO JWT, see
// internal/hud/cfaccess). On-LAN, split-horizon DNS points hud.flexinfer.ai at
// the internal nginx ingress, so the request never transits Cloudflare and
// carries no Access JWT. For that path we trust the network: a request whose
// real client IP is in an operator-configured CIDR allowlist (the home LAN) is
// admin, no token. The manual token stays the CLI/scripting fallback.
//
// Trust model: this trusts the ingress to set the client-IP forwarding headers
// and assumes a single-tenant cluster (no hostile pods forging X-Forwarded-For
// against the ClusterIP). That holds for a homelab; a multi-tenant deployment
// should leave HUD_ADMIN_TRUSTED_CIDRS empty and use the Access/token paths.
// The allowlist should be the LAN subnet only (e.g. 192.168.50.0/24), never the
// pod/service CIDRs (10.x here) — those are how in-cluster callers reach the
// HUD, and they carry their own tokens.

// parseCIDRs parses a comma/space/semicolon-separated CIDR list (from an env
// var) into networks. Bare IPs are accepted and treated as /32 or /128.
// Unparseable entries are skipped.
func parseCIDRs(s string) []*net.IPNet {
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ';' || r == ' ' || r == '\n' || r == '\t'
	})
	out := make([]*net.IPNet, 0, len(fields))
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		if _, n, err := net.ParseCIDR(f); err == nil {
			out = append(out, n)
			continue
		}
		// Bare IP → host route.
		if ip := net.ParseIP(f); ip != nil {
			bits := 32
			if ip.To4() == nil {
				bits = 128
			}
			out = append(out, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
		}
	}
	return out
}

// clientIP returns the request's real client IP. Behind the nginx ingress the
// true client is in X-Real-IP (canonical) or the leftmost X-Forwarded-For
// entry; a direct connection falls back to RemoteAddr. Returns nil when no IP
// can be determined.
func clientIP(r *http.Request) net.IP {
	if r == nil {
		return nil
	}
	if xr := strings.TrimSpace(r.Header.Get("X-Real-IP")); xr != "" {
		if ip := net.ParseIP(xr); ip != nil {
			return ip
		}
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		// Leftmost entry is the original client (ingress appends downstream).
		first := strings.TrimSpace(strings.Split(xff, ",")[0])
		if ip := net.ParseIP(first); ip != nil {
			return ip
		}
	}
	host := r.RemoteAddr
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		host = h
	}
	return net.ParseIP(strings.TrimSpace(host))
}

// trustedNetworkAdmin reports whether the request's client IP falls in the
// configured trusted-CIDR allowlist. False (feature off) when the allowlist is
// empty.
func (a *App) trustedNetworkAdmin(r *http.Request) bool {
	if a == nil || len(a.adminTrustedNets) == 0 {
		return false
	}
	ip := clientIP(r)
	if ip == nil {
		return false
	}
	for _, n := range a.adminTrustedNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// ClientIP exposes the detected client IP as a string (for /api/labs/auth-check
// diagnostics so an operator can see what IP the HUD attributes to them when
// tuning HUD_ADMIN_TRUSTED_CIDRS). Empty when undetermined.
func (a *App) ClientIP(r *http.Request) string {
	if ip := clientIP(r); ip != nil {
		return ip.String()
	}
	return ""
}
