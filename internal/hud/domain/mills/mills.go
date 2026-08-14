package mills

import (
	"net/http"
	"net/url"
	"strings"
)

// Domain registers the mills-proxy routes.
type Domain struct {
	deps  Deps
	proxy *operatorProxy
}

// New creates a Domain. The proxy is constructed lazily on the first
// request; if Deps.MillsConfig().BaseURL is unset the proxy stays nil and
// every handler returns 503.
func New(deps Deps) *Domain {
	d := &Domain{deps: deps}
	if cfg := deps.MillsConfig(); cfg.BaseURL != "" {
		if u, err := url.Parse(strings.TrimRight(cfg.BaseURL, "/")); err == nil {
			d.proxy = newOperatorProxy(u, cfg.AdminToken, deps.Logger())
		}
	}
	return d
}

// Name satisfies domain.Domain.
func (d *Domain) Name() string { return "mills" }

// RegisterRoutes wires the mills endpoints to the ServeMux. We register
// each route by exact path/method rather than a single subtree so the
// frontend gets the right HTTP semantics (GET reads stay open; POST
// mutations require the HUD admin token).
//
// The proxy itself rewrites the URL to the operator's path and adds the
// operator's admin bearer; the HUD admin gate above just authenticates
// the caller against the HUD.
func (d *Domain) RegisterRoutes(mux *http.ServeMux, mw func(http.HandlerFunc) http.HandlerFunc) {
	// Reads — no admin gate.
	mux.HandleFunc("GET /api/mills/status", mw(d.handleProxyGet))
	mux.HandleFunc("GET /api/mills/policy", mw(d.handleProxyGet))
	mux.HandleFunc("GET /api/mills/kpis", mw(d.handleProxyGet))
	// Stage/gate/run telemetry roll-up (plan mills-telemetry-optimization S5).
	// Read-only aggregation feeding the HUD "Mill Telemetry" panel; proxied
	// without an admin gate like the other read endpoints. Without this the SPA
	// fallback serves HTML the frontend then fails to JSON.parse.
	mux.HandleFunc("GET /api/mills/telemetry/stages", mw(d.handleProxyGet))
	// Resolved model-wiring snapshot (which model/backend the judge, weaver,
	// council lenses, and each spawn stage use). Read-only non-secret config
	// feeding the HUD Mills Overview; proxied without an admin gate like the
	// other reads. Without this the SPA fallback serves HTML the frontend then
	// fails to JSON.parse.
	mux.HandleFunc("GET /api/mills/wiring", mw(d.handleProxyGet))
	// Overseers status (Mills Overseers slice 4): policy gates + per-agent
	// harness snapshots (groomer/sentinel/foreman) + each agent's 24h audit
	// trail and any live admission-suppression lease. Read-only, proxied
	// without an admin gate like the other reads so the HUD's Overseers panel
	// can poll it from a browser. Without this the SPA fallback serves HTML the
	// frontend then fails to JSON.parse.
	mux.HandleFunc("GET /api/mills/overseers", mw(d.handleProxyGet))
	// Mill Staff evidence reports. Five window-bounded read-only aggregations
	// over the durable events table, feeding the HUD's "Mill Staff" panel:
	// promotion (dry-run vs executed per guarded actor), judge calibration
	// (per-gate merged-vs-escalated score discrimination), post-merge
	// regressions, config outcomes (which configuration ships work), and mined
	// classifier-signature candidates. Open reads like the overseers status
	// they aggregate; each accepts ?window= (and the promotion report ?actor=),
	// so the query must survive the proxy. Without the explicit registration
	// the SPA fallback serves HTML the frontend then fails to JSON.parse.
	mux.HandleFunc("GET /api/mills/promotion-report", mw(d.handleProxyGet))
	mux.HandleFunc("GET /api/mills/judge-calibration", mw(d.handleProxyGet))
	mux.HandleFunc("GET /api/mills/regressions", mw(d.handleProxyGet))
	mux.HandleFunc("GET /api/mills/config-outcomes", mw(d.handleProxyGet))
	mux.HandleFunc("GET /api/mills/signature-candidates", mw(d.handleProxyGet))
	mux.HandleFunc("GET /api/mills/demand-log", mw(d.handleProxyGet))
	mux.HandleFunc("GET /api/mills/council/runs", mw(d.handleProxyGet))
	mux.HandleFunc("GET /api/mills/council/runs/{id}", mw(d.handleProxyGet))
	// Per-round debate transcript for a council run. Without this proxy
	// the SPA fallback served HTML to /runs/{id}/debate, which the HUD
	// frontend then tried to JSON.parse and surfaced as
	// "Unrecognized token '<'" on the Council tab.
	mux.HandleFunc("GET /api/mills/council/runs/{id}/debate", mw(d.handleProxyGet))
	mux.HandleFunc("GET /api/mills/pipeline/runs", mw(d.handleProxyGet))
	mux.HandleFunc("GET /api/mills/pipeline/runs/{id}", mw(d.handleProxyGet))
	// Durable workflow step-log (plan .loom/134 §S4). Read-only journal
	// views proxied through without an admin gate so the HUD's Workflows
	// panel can poll them; they back the S1c step-timeline observation
	// surface. Without these the SPA fallback serves HTML to
	// /workflow/runs, which the frontend then fails to JSON.parse.
	mux.HandleFunc("GET /api/mills/workflow/runs", mw(d.handleProxyGet))
	mux.HandleFunc("GET /api/mills/workflow/runs/{id}", mw(d.handleProxyGet))
	mux.HandleFunc("GET /api/mills/backlog", mw(d.handleProxyGet))
	mux.HandleFunc("GET /api/mills/backlog/{id}", mw(d.handleProxyGet))
	// Serial merge queue: active entries + per-lane depth (open read, feeds
	// the HUD merge-queue panel and lane-pressure checks). Without the
	// explicit registration the SPA fallback serves HTML the frontend then
	// fails to JSON.parse.
	mux.HandleFunc("GET /api/mills/merge-queue", mw(d.handleProxyGet))
	// Escalation relaunch candidates: escalated backlog items whose latest
	// pipeline run is policy-retryable. Read-only projection proxied without
	// an admin gate like the other reads. Without this the SPA fallback
	// serves HTML the frontend then fails to JSON.parse.
	mux.HandleFunc("GET /api/mills/escalations/relaunch-candidates", mw(d.handleProxyGet))
	mux.HandleFunc("GET /api/mills/eval/scores", mw(d.handleProxyGet))
	// Per-backlog-item cost estimate (slice 7.3). Read-only projection the HUD
	// shows before starting a pipeline run; the operator implements it at
	// GET /api/mills/cost-preview?backlog_id=... Proxied without an admin gate
	// like the other reads. Without this the SPA fallback serves HTML the
	// frontend then fails to JSON.parse.
	mux.HandleFunc("GET /api/mills/cost-preview", mw(d.handleProxyGet))
	// Spinning Room frame list (Live Beam slice 3). Read-only policy view
	// proxied through without an admin gate so the HUD's "Spin a plan"
	// dialog can populate its frame selector; without this the SPA fallback
	// serves HTML the frontend then fails to JSON.parse.
	mux.HandleFunc("GET /api/mills/spinning-room/frames", mw(d.handleProxyGet))
	// Async spins (plan .loom/166): the HUD's spin dialog fires-and-forgets
	// against POST /api/mills/spin/async and polls these status reads until the
	// draft lands — a frontier frame runs minutes, past the client-facing proxy
	// timeout, so the connection can't be held open. Reads are open; without the
	// proxy the SPA fallback serves HTML the frontend then fails to JSON.parse.
	mux.HandleFunc("GET /api/mills/spin/runs", mw(d.handleProxyGet))
	mux.HandleFunc("GET /api/mills/spin/runs/{id}", mw(d.handleProxyGet))
	// Bootstrapped-project registry (plan→repo bootstrap). Read-only list of
	// repos minted from a Spinning Room plan; feeds the Plans panel's
	// "spin up a repo" action so it can show what's already minted and
	// whether the two-key policy gate is on. Open read like the spin runs.
	mux.HandleFunc("GET /api/mills/projects/bootstrapped", mw(d.handleProxyGet))

	// Squads (Phase 2 slice 2.5). Read endpoints proxy through without
	// an admin gate so the HUD's Squads panel can poll them. The
	// route-test path is admin-gated to mirror the operator's own gate.
	mux.HandleFunc("GET /api/mills/squads", mw(d.handleProxyGet))
	mux.HandleFunc("GET /api/mills/squads/{name}", mw(d.handleProxyGet))
	mux.HandleFunc("GET /api/mills/squads/{name}/memory", mw(d.handleProxyGet))
	mux.HandleFunc("GET /api/mills/squads/{name}/outcomes", mw(d.handleProxyGet))

	// Audit (Phase 3 slice 3.5). Read endpoints feed the HUD Audit
	// panel; admin POST /run is gated identically to the operator's
	// own admin gate.
	mux.HandleFunc("GET /api/mills/audit/findings", mw(d.handleProxyGet))
	mux.HandleFunc("GET /api/mills/audit/findings/{id}", mw(d.handleProxyGet))

	// Cross-repo (Phase 4 slice 4.4). Reads proxy through; abort is
	// HUD-admin-gated before reaching the operator's own admin gate.
	mux.HandleFunc("GET /api/mills/cross-repo/runs", mw(d.handleProxyGet))
	mux.HandleFunc("GET /api/mills/cross-repo/runs/{id}", mw(d.handleProxyGet))

	// Mutations — gated by the HUD's existing admin-token check before
	// the operator's own admin gate. Two layers of auth keep stray
	// browser tabs from triggering the autonomy loop.
	mux.HandleFunc("POST /api/mills/council/run", mw(d.handleProxyAdminPost))
	mux.HandleFunc("POST /api/mills/council/dryrun", mw(d.handleProxyAdminPost))
	// Spinning Room (Live Beam slice 3): spin a draft plan from a brief on a
	// chosen model frame. Mutates the Plan Store, so it is double-gated
	// through the HUD admin token before the operator's own admin gate.
	mux.HandleFunc("POST /api/mills/spin", mw(d.handleProxyAdminPost))
	// Async spin (plan .loom/166): returns 202 + a spin_id immediately and runs
	// the spin in the background. Same double-gate as the synchronous /spin.
	mux.HandleFunc("POST /api/mills/spin/async", mw(d.handleProxyAdminPost))
	// Plan→repo bootstrap: mint a new GitLab project from a Spinning Room
	// plan. Creates a repo AND re-scopes the plan, so it is double-gated
	// through the HUD admin token before the operator's own admin gate.
	mux.HandleFunc("POST /api/mills/projects/bootstrap", mw(d.handleProxyAdminPost))
	// Global autonomy kill-switch (plan 42 Slice 1b). Opens a GitOps
	// auto-PR flipping policy `enabled:`; double-gated through the HUD
	// admin token before the operator's own admin gate.
	mux.HandleFunc("POST /api/mills/policy/kill-switch", mw(d.handleProxyAdminPost))
	mux.HandleFunc("POST /api/mills/pipeline/runs/{backlog_id}/start", mw(d.handleProxyAdminPost))
	mux.HandleFunc("POST /api/mills/pipeline/runs/{id}/pause", mw(d.handleProxyAdminPost))
	mux.HandleFunc("POST /api/mills/pipeline/runs/{id}/resume", mw(d.handleProxyAdminPost))
	mux.HandleFunc("POST /api/mills/pipeline/runs/{id}/escalate", mw(d.handleProxyAdminPost))
	mux.HandleFunc("POST /api/mills/pipeline/runs/{id}/grade", mw(d.handleProxyAdminPost))
	mux.HandleFunc("POST /api/mills/backlog", mw(d.handleProxyAdminPost))
	mux.HandleFunc("POST /api/mills/backlog/sync", mw(d.handleProxyAdminPost))
	mux.HandleFunc("POST /api/mills/eval/run-cross", mw(d.handleProxyAdminPost))
	mux.HandleFunc("POST /api/mills/squads/{name}/route-test", mw(d.handleProxyAdminPost))
	mux.HandleFunc("POST /api/mills/audit/run", mw(d.handleProxyAdminPost))
	mux.HandleFunc("POST /api/mills/cross-repo/runs/{id}/abort", mw(d.handleProxyAdminPost))
	// Serial merge-queue enqueue for LOCAL agents (the "via loomd" path):
	// mcp-gitlab's merge tool posts here with the HUD admin credential and
	// the proxy injects the operator bearer, so the cluster-wide operator
	// token never lands in per-agent env files. Same double-gate as every
	// other mutation.
	mux.HandleFunc("POST /api/mills/merge-queue/enqueue", mw(d.handleProxyAdminPost))

	// Adaptive policy proposals (Phase 7 slice 7.2). Read endpoint feeds
	// the HUD's Adaptive panel; apply/reject double-gate through both
	// HUD and operator admin tokens.
	mux.HandleFunc("GET /api/mills/policy/proposals", mw(d.handleProxyGet))
	mux.HandleFunc("POST /api/mills/policy/proposals/{id}/apply", mw(d.handleProxyAdminPost))
	mux.HandleFunc("POST /api/mills/policy/proposals/{id}/reject", mw(d.handleProxyAdminPost))
}

func (d *Domain) handleProxyGet(w http.ResponseWriter, r *http.Request) {
	if d.proxy == nil {
		d.deps.WriteError(w, http.StatusServiceUnavailable, "loom-mills operator not configured", nil)
		return
	}
	d.proxy.ServeHTTP(w, r)
}

func (d *Domain) handleProxyAdminPost(w http.ResponseWriter, r *http.Request) {
	if d.proxy == nil {
		d.deps.WriteError(w, http.StatusServiceUnavailable, "loom-mills operator not configured", nil)
		return
	}
	if !d.deps.RequireAdminToken(w, r) {
		return
	}
	d.proxy.ServeHTTP(w, r)
}
