// api_engrams.go exposes the engram tech-tree summary to the HUD.
//
// The summary endpoint is a thin aggregator over agent_engram_list — it
// returns counts by proof_status and tier so the catalog view can render a
// single-line "Engrams: N verified · M stale · K failing" badge without the
// frontend having to walk the full library.
package hud

import (
	"net/http"

	"github.com/crb2nu/loom/internal/hud/bridge"
)

// handleEngramList returns the full engram catalog for the tech-tree view.
// A missing bridge returns {"engrams":[],"degraded":true}; bridge failures
// return 502 so unavailable data is never presented as a genuinely empty tree.
func (a *App) handleEngramList(w http.ResponseWriter, r *http.Request) {
	if a.agent == nil {
		a.writeJSON(w, http.StatusOK, map[string]any{"engrams": []bridge.EngramInfo{}, "degraded": true})
		return
	}
	engrams, err := a.agent.EngramList()
	if err != nil {
		a.writeError(w, http.StatusBadGateway, "list engrams", err)
		return
	}
	a.writeJSON(w, http.StatusOK, map[string]any{"engrams": engrams, "degraded": false})
}

// handleEngramGraph returns typed nodes and edges for the engram tech tree.
// A missing bridge returns all collection keys as non-nil empty arrays and
// degraded=true; a bridge call failure returns 502.
func (a *App) handleEngramGraph(w http.ResponseWriter, r *http.Request) {
	if a.agent == nil {
		a.writeJSON(w, http.StatusOK, &bridge.EngramGraphResult{Nodes: []bridge.EngramInfo{}, Edges: []bridge.EngramGraphEdge{}, Degraded: true})
		return
	}
	graph, err := a.agent.EngramGraph()
	if err != nil {
		a.writeError(w, http.StatusBadGateway, "engram graph", err)
		return
	}
	a.writeJSON(w, http.StatusOK, graph)
}

// handleEngramSummary returns aggregate counts of engrams by proof_status
// and tier.
//
// Response shape:
//
//	{
//	  "total":     <int>,
//	  "by_status": {"unverified": int, "verified": int, "stale": int, "failing": int},
//	  "by_tier":   {"tier:1": int, "tier:2": int, "tier:3": int},
//	  "degraded":  <bool>
//	}
//
// The agent bridge is required; if it is not configured (e.g. tests with a
// minimal App), the endpoint returns an empty summary instead of erroring
// so the catalog view can render a "no data yet" state. That placeholder
// carries "degraded": true so a client can tell "the bridge is unavailable"
// apart from "the catalog is genuinely empty" — both otherwise look like a
// 200 with all-zero counts.
func (a *App) handleEngramSummary(w http.ResponseWriter, r *http.Request) {
	if a.agent == nil {
		a.writeJSON(w, http.StatusOK, emptyEngramSummary())
		return
	}

	summary, err := a.agent.EngramSummary()
	if err != nil {
		a.writeError(w, http.StatusBadGateway, "engram summary", err)
		return
	}

	a.writeJSON(w, http.StatusOK, summary)
}

// emptyEngramSummary returns a zero-valued summary with all keys present so
// frontend code can index without nil checks. "degraded" marks it as the
// bridge-unavailable placeholder rather than a real empty catalog.
func emptyEngramSummary() map[string]any {
	return map[string]any{
		"total": 0,
		"by_status": map[string]int{
			"unverified": 0,
			"verified":   0,
			"stale":      0,
			"failing":    0,
		},
		"by_tier":  map[string]int{},
		"degraded": true,
	}
}
