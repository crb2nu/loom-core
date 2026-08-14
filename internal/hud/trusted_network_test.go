package hud

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParseCIDRs(t *testing.T) {
	nets := parseCIDRs("192.168.50.0/24, 10.0.0.5 ;bogus/xx  fe80::/10")
	// 192.168.50.0/24, 10.0.0.5 (→/32), fe80::/10 — "bogus/xx" dropped.
	if len(nets) != 3 {
		t.Fatalf("got %d nets, want 3: %v", len(nets), nets)
	}
	if len(parseCIDRs("   ")) != 0 {
		t.Fatal("blank → no nets")
	}
}

func reqFrom(remoteAddr, xRealIP, xff string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/api/labs/auth-check", nil)
	r.RemoteAddr = remoteAddr
	if xRealIP != "" {
		r.Header.Set("X-Real-IP", xRealIP)
	}
	if xff != "" {
		r.Header.Set("X-Forwarded-For", xff)
	}
	return r
}

func TestClientIP_Precedence(t *testing.T) {
	// X-Real-IP wins.
	if ip := clientIP(reqFrom("10.42.33.174:5000", "192.168.50.153", "1.2.3.4")); ip.String() != "192.168.50.153" {
		t.Fatalf("X-Real-IP should win, got %v", ip)
	}
	// Falls back to leftmost XFF.
	if ip := clientIP(reqFrom("10.42.33.174:5000", "", "192.168.50.153, 10.42.0.1")); ip.String() != "192.168.50.153" {
		t.Fatalf("leftmost XFF, got %v", ip)
	}
	// Falls back to RemoteAddr (host:port).
	if ip := clientIP(reqFrom("192.168.50.153:44321", "", "")); ip.String() != "192.168.50.153" {
		t.Fatalf("RemoteAddr fallback, got %v", ip)
	}
}

func newTrustedApp(cidrs string) *App {
	return &App{adminTrustedNets: parseCIDRs(cidrs)}
}

func TestTrustedNetworkAdmin(t *testing.T) {
	app := newTrustedApp("192.168.50.0/24")

	// LAN client via ingress (X-Real-IP set) → trusted.
	if !app.trustedNetworkAdmin(reqFrom("10.42.33.174:5000", "192.168.50.153", "")) {
		t.Fatal("LAN client should be trusted")
	}
	// A client outside the LAN subnet → not trusted.
	if app.trustedNetworkAdmin(reqFrom("10.42.33.174:5000", "192.168.99.7", "")) {
		t.Fatal("off-subnet client must not be trusted")
	}
	// A direct in-cluster pod hitting the ClusterIP (no forwarding headers) →
	// its own pod IP, not in the LAN CIDR → not trusted.
	if app.trustedNetworkAdmin(reqFrom("10.42.29.182:5000", "", "")) {
		t.Fatal("in-cluster pod IP must not be trusted")
	}
	// Feature off (no CIDRs) → never trusted, even for a LAN IP.
	if newTrustedApp("").trustedNetworkAdmin(reqFrom("10.42.33.174:5000", "192.168.50.153", "")) {
		t.Fatal("empty allowlist disables the trusted-network path")
	}
}

func TestAdminIdentity_TrustedNetwork(t *testing.T) {
	// No token, no Access — only the trusted network authorizes.
	app := newTrustedApp("192.168.50.0/24")
	email, via, ok := app.adminIdentity(reqFrom("10.42.33.174:5000", "192.168.50.153", ""))
	if !ok || via != "network" || email != "" {
		t.Fatalf("want (\"\",\"network\",true), got (%q,%q,%v)", email, via, ok)
	}
	// Off-subnet with no token → not admin.
	if _, _, ok := app.adminIdentity(reqFrom("10.42.33.174:5000", "203.0.113.9", "")); ok {
		t.Fatal("non-LAN request with no token must not be admin")
	}
}

func TestRequireAdminToken_TrustedNetworkPasses(t *testing.T) {
	app := newTrustedApp("192.168.50.0/24")
	rec := httptest.NewRecorder()
	if !app.requireAdminToken(rec, reqFrom("10.42.33.174:5000", "192.168.50.153", "")) {
		t.Fatalf("LAN request should pass the gate; got %d", rec.Code)
	}
	// A non-LAN request with no token configured → 401 (gate IS configured via
	// the trusted-net path, so it's "invalid" not "not configured").
	rec2 := httptest.NewRecorder()
	if app.requireAdminToken(rec2, reqFrom("10.42.33.174:5000", "203.0.113.9", "")) {
		t.Fatal("non-LAN request must be rejected")
	}
	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rec2.Code)
	}
}
