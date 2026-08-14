package hud

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// extractBearerToken extracts a bearer token from the Authorization header.
func extractBearerToken(r *http.Request) string {
	if r == nil {
		return ""
	}
	authHeader := strings.TrimSpace(r.Header.Get("Authorization"))
	if authHeader == "" {
		return ""
	}
	parts := strings.SplitN(authHeader, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return ""
	}
	return strings.TrimSpace(parts[1])
}

// isMobileOperatorToken returns true if the request carries the configured
// mobile operator bearer token.
func (a *App) isMobileOperatorToken(r *http.Request) bool {
	expected := strings.TrimSpace(a.config.MobileOperatorToken)
	if expected == "" {
		return false
	}
	actual := extractBearerToken(r)
	if actual == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(expected), []byte(actual)) == 1
}

// mobileCompanionExtraPrefixes are the non-/api/mobile/v1 path prefixes the
// mobile operator token is ALSO allowed to reach: the read/board surfaces the
// companion app renders directly off the HUD proxy (Mills, the Plan Store,
// Weaver, and the AI-model roles Weaver reads). The app hits these paths
// verbatim (see apps/loom-companion-ios .../Networking/Endpoint.swift), so
// without this allowlist every Mills/Plans/Weaver request 403s with
// "restricted to /api/mobile/v1 endpoints" — which surfaces as "Couldn't reach
// Mills" on the phone.
//
// This only lets the token REACH the routes; it does NOT grant admin power.
// Mutations under /api/mills (spin, escalate, backlog, kill-switch, council)
// stay behind their own admin gate (handleProxyAdminPost → requireAdminToken →
// X-Admin-Token / HUD_ADMIN_TOKEN), so a mobile token still cannot mutate
// without a separate admin token. Plan advance/priority are intentionally open
// (the HUD web frontend advances with a bare fetch too). Agent/labs/spawn and
// every other admin surface remain blocked for the mobile token.
var mobileCompanionExtraPrefixes = []string{
	"/api/mobile/v1/", // the primary mobile surface
	"/api/mills/",
	"/api/plans/", // /api/plans/{id}[/advance|/priority]; the bare collection is matched below
	"/api/weaver/",
	"/api/aimodels/",
	// Cross-vendor transcript list/search (GET-only routes; the bare
	// collection is matched below). Lets the companion's operator surface
	// browse claude/codex CLI sessions on the workstation.
	"/api/vendor-sessions/",
}

// mobileCompanionExactPaths are non-prefixed routes (no subpath) the mobile
// token may reach — the Plan Store collection endpoint and the Pattern Loom
// catalog.
//
// /api/patterns is a read-only GET the iOS shift report calls as
// `GET /api/patterns?status=approved`; without it the companion gets a 403
// "mobile_operator token is restricted..." and the shift report renders empty.
// It is deliberately an EXACT path, not a prefix: POST /api/patterns/stamp
// mutates the catalog and stays out of reach of the mobile token.
var mobileCompanionExactPaths = map[string]bool{
	"/api/plans":           true,
	"/api/patterns":        true,
	"/api/vendor-sessions": true,
	// /api/health is served unauthenticated to everyone, so denying it to a
	// scoped token grants no protection — it only breaks callers that present
	// their token on every request. The mills operator's sentinel liveness
	// probe (LOOM_HUD_TOKEN is the mobile operator token) does exactly that
	// and would otherwise trip a permanent "hud" incident on 403s.
	"/api/health": true,
}

// mobileTokenOutsideMobileAPI returns true when a request uses the mobile
// operator token but targets a path the token is not permitted to reach. The
// mobile surface (/api/mobile/v1/*) plus the companion's read/board surfaces
// (mobileCompanionExtraPrefixes/mobileCompanionExactPaths) are allowed;
// everything else is denied.
func (a *App) mobileTokenOutsideMobileAPI(r *http.Request) bool {
	if !a.isMobileOperatorToken(r) {
		return false
	}
	path := r.URL.Path
	if mobileCompanionExactPaths[path] {
		return false
	}
	for _, prefix := range mobileCompanionExtraPrefixes {
		if strings.HasPrefix(path, prefix) {
			return false
		}
	}
	return true
}
