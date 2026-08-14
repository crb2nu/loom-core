package hud

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The mobile operator token is scope-restricted to /api/mobile/v1 by
// withCORS → mobileTokenOutsideMobileAPI. The companion app also renders the
// Mills / Plan Store / Weaver read+board surfaces off the HUD proxy, so those
// prefixes must be reachable with the mobile token or the phone shows
// "Couldn't reach Mills". These tests pin exactly which extra paths open up
// and which stay denied.

func mobileReq(path string) *http.Request {
	req := httptest.NewRequest("GET", path, nil)
	req.Header.Set("Authorization", "Bearer mobile-secret")
	return req
}

func TestMobileTokenOutsideMobileAPI_AllowsCompanionSurfaces(t *testing.T) {
	app, _ := newTestApp(t)
	app.config.MobileOperatorToken = "mobile-secret"

	allowed := []string{
		"/api/mobile/v1/ping",
		"/api/mobile/v1/sessions",
		"/api/mills/pipeline/runs",
		"/api/mills/kpis",
		"/api/mills/spinning-room/frames",
		"/api/mills/spin/runs",
		"/api/mills/spin/runs/spin-123",
		"/api/mills/spin/async",
		"/api/mills/pipeline/runs/run-1/escalate",
		"/api/plans",
		"/api/plans/plan-1",
		"/api/plans/plan-1/advance",
		"/api/plans/plan-1/priority",
		"/api/weaver/status",
		"/api/weaver/history",
		"/api/aimodels/roles",
		// Pattern Loom catalog — the iOS shift report reads
		// GET /api/patterns?status=approved. Query strings never reach the
		// allowlist (it matches on r.URL.Path), so the bare path is the check.
		"/api/patterns",
		// Cross-vendor transcript surface (GET-only): the bare collection
		// plus the search subpath.
		"/api/vendor-sessions",
		"/api/vendor-sessions/search",
		"/api/vendor-sessions/tail",
	}
	for _, path := range allowed {
		if app.mobileTokenOutsideMobileAPI(mobileReq(path)) {
			t.Errorf("mobile token should be ALLOWED to reach %s, but it was blocked", path)
		}
	}
}

func TestMobileTokenOutsideMobileAPI_DeniesAdminSurfaces(t *testing.T) {
	app, _ := newTestApp(t)
	app.config.MobileOperatorToken = "mobile-secret"

	denied := []string{
		"/api/agent/session-start",
		"/api/labs/auth-check",
		"/api/spawn",
		"/api/mills",  // no trailing slash — the /api/mills/ prefix must not over-match
		"/api/plansx", // must not be caught by the /api/plans exact/subpath allow
		"/api/weaver", // no trailing slash
		"/api/config",
		// /api/patterns is allowed as an EXACT path only; the mutating
		// stamp subpath must stay out of reach of the mobile token.
		"/api/patterns/stamp",
		"/api/patternsx",        // must not be caught by the /api/patterns exact allow
		"/api/vendor-sessionsx", // must not be caught by the /api/vendor-sessions exact allow
	}
	for _, path := range denied {
		if !app.mobileTokenOutsideMobileAPI(mobileReq(path)) {
			t.Errorf("mobile token should be DENIED on %s, but it was allowed", path)
		}
	}
}

func TestMobileTokenOutsideMobileAPI_NonMobileTokenUnaffected(t *testing.T) {
	app, _ := newTestApp(t)
	app.config.MobileOperatorToken = "mobile-secret"

	// A request without the mobile token (e.g. an admin bearer or SSO) is never
	// subject to this restriction — the gate is mobile-token-specific.
	req := httptest.NewRequest("GET", "/api/agent/session-start", nil)
	req.Header.Set("Authorization", "Bearer some-other-token")
	if app.mobileTokenOutsideMobileAPI(req) {
		t.Fatal("non-mobile token must not be restricted by the mobile-token gate")
	}

	// And with no mobile token configured at all, the gate never fires.
	app.config.MobileOperatorToken = ""
	if app.mobileTokenOutsideMobileAPI(mobileReq("/api/mills/pipeline/runs")) {
		t.Fatal("gate must not fire when no mobile operator token is configured")
	}
}
