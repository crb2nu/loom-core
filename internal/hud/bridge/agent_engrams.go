package bridge

import (
	"encoding/json"
	"fmt"
	"time"
)

// EngramProof describes the proof attached to an engram.
type EngramProof struct {
	Kind string   `json:"kind,omitempty"`
	Refs []string `json:"refs"`
}

// UnmarshalJSON accepts both proof shapes on the wire: the structured
// {kind, refs} object and the bare proof STRING agent_engram_list emits
// (engramItemToMap's "proof" field). The typed decode used to fail on the
// string form with a type error, which 502'd /api/engrams and /api/engrams/graph
// for every catalog that had any engram with a proof — i.e. always.
func (p *EngramProof) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		p.Kind = ""
		p.Refs = nil
		if s != "" {
			p.Refs = []string{s}
		}
		return nil
	}
	type plain EngramProof
	var obj plain
	if err := json.Unmarshal(b, &obj); err != nil {
		return err
	}
	*p = EngramProof(obj)
	return nil
}

// EngramInfo is the HUD-facing representation of an engram.
type EngramInfo struct {
	ID             string      `json:"id"`
	Name           string      `json:"name"`
	Tier           int         `json:"tier"`
	ProofStatus    string      `json:"proof_status"`
	Description    string      `json:"description,omitempty"`
	Prerequisites  []string    `json:"prerequisites"`
	LastVerifiedAt string      `json:"last_verified_at,omitempty"`
	Proof          EngramProof `json:"proof"`
	// Stub marks a placeholder node the full-catalog graph emits for a
	// prerequisite URI that has no engram behind it (fullEngramGraph in
	// pkg/agentcontext). The tree still renders it as a gap, so it stays in
	// EngramGraphResult.Nodes, but it is not an engram and EngramSummary does
	// not count it. Deliberately kept off the wire: the graph shape the HUD
	// consumes is unchanged.
	Stub bool `json:"-"`
}

// EngramGraphEdge connects an engram to one of its prerequisites.
type EngramGraphEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// EngramGraphResult is the complete engram prerequisite graph.
type EngramGraphResult struct {
	Nodes    []EngramInfo      `json:"nodes"`
	Edges    []EngramGraphEdge `json:"edges"`
	Degraded bool              `json:"degraded"`
}

type engramWire struct {
	ID             string      `json:"id"`
	URI            string      `json:"uri"`
	Name           string      `json:"name"`
	Title          string      `json:"title"`
	Tier           int         `json:"tier"`
	ProofStatus    string      `json:"proof_status"`
	Description    string      `json:"description"`
	Content        string      `json:"content"`
	Prerequisites  []string    `json:"prerequisites"`
	LastVerifiedAt string      `json:"last_verified_at"`
	LastVerified   string      `json:"last_verified"`
	Proof          EngramProof `json:"proof"`
	ProofKind      string      `json:"proof_kind"`
	ProofRefs      []string    `json:"proof_refs"`
	// Stub is the marker fullEngramGraph sets on prerequisite placeholders.
	// A server predating the marker emits stubs without it; they then count
	// as unverified engrams in EngramSummary until that server is redeployed.
	Stub bool `json:"stub"`
}

func (w engramWire) info() EngramInfo {
	// Since 2026-08-30 the service serves id == uri on every surface (the
	// storage id rides separately as memory_id), so this fallback is only
	// load-bearing against an OLDER agent-context whose graph nodes carried
	// no id at all — keep it until no such server can appear behind this HUD.
	if w.ID == "" {
		w.ID = w.URI
	}
	if w.Name == "" {
		w.Name = w.Title
	}
	if w.Description == "" {
		w.Description = w.Content
	}
	if w.LastVerifiedAt == "" {
		w.LastVerifiedAt = w.LastVerified
	}
	if w.ProofStatus == "" {
		w.ProofStatus = "unverified"
	}
	if w.Prerequisites == nil {
		w.Prerequisites = []string{}
	}
	if w.Proof.Kind == "" {
		w.Proof.Kind = w.ProofKind
	}
	if w.Proof.Refs == nil {
		w.Proof.Refs = w.ProofRefs
	}
	if w.Proof.Refs == nil {
		w.Proof.Refs = []string{}
	}
	return EngramInfo{ID: w.ID, Name: w.Name, Tier: w.Tier, ProofStatus: w.ProofStatus,
		Description: w.Description, Prerequisites: w.Prerequisites,
		LastVerifiedAt: w.LastVerifiedAt, Proof: w.Proof, Stub: w.Stub}
}

// EngramList returns the full engram catalog.
func (a *AgentBridge) EngramList() ([]EngramInfo, error) {
	var result struct {
		Items []engramWire `json:"items"`
	}
	if err := a.callAgentTool("agent_engram_list", map[string]any{"limit": 1000}, &result); err != nil {
		return nil, err
	}
	out := make([]EngramInfo, 0, len(result.Items))
	for _, item := range result.Items {
		out = append(out, item.info())
	}
	return out, nil
}

// defaultEngramGraphTTL bounds how long one agent_engram_graph result keeps
// serving EngramGraph and EngramSummary. The HUD engrams store fires
// GET /api/engrams/graph and GET /api/engrams/summary in the same Promise.all
// every 60s: callers that overlap coalesce on the singleflight group, and this
// window covers the second request landing a few milliseconds after the first
// already completed. It sits far below the poll interval, so every cycle still
// observes a fresh catalog, and the tree and its summary strip always describe
// the same snapshot.
const defaultEngramGraphTTL = 5 * time.Second

// engramGraphCacheKey is the singleflight and cache key for the shared graph.
const engramGraphCacheKey = "engram_graph"

// EngramGraph returns the prerequisite graph. It accepts both the current
// URI-only node payload and the richer object payload used by newer servers.
//
// One agent_engram_graph fetch serves both this and EngramSummary: callers
// that overlap share a single in-flight call, and for engramGraphTTL after it
// completes they share its result. The returned value is that shared result
// and must be treated as read-only. Failures are never cached, so the next
// caller retries.
func (a *AgentBridge) EngramGraph() (*EngramGraphResult, error) {
	if cached, ok := a.cachedEngramGraph(); ok {
		return cached, nil
	}
	v, err, _ := a.engramGraphFlight.Do(engramGraphCacheKey, func() (any, error) {
		// A caller that missed the cache just before the previous flight
		// landed must reuse that result rather than start another fetch.
		if cached, ok := a.cachedEngramGraph(); ok {
			return cached, nil
		}
		graph, err := a.fetchEngramGraph()
		if err != nil {
			return nil, err
		}
		if a.engramGraphTTL > 0 {
			a.cache.Set(engramGraphCacheKey, graph, a.engramGraphTTL)
		}
		return graph, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(*EngramGraphResult), nil
}

func (a *AgentBridge) cachedEngramGraph() (*EngramGraphResult, bool) {
	cached, ok := a.cache.Get(engramGraphCacheKey)
	if !ok {
		return nil, false
	}
	graph, ok := cached.(*EngramGraphResult)
	return graph, ok
}

// fetchEngramGraph is the uncached agent_engram_graph call behind EngramGraph.
func (a *AgentBridge) fetchEngramGraph() (*EngramGraphResult, error) {
	var result struct {
		Nodes []json.RawMessage `json:"nodes"`
		Edges []EngramGraphEdge `json:"edges"`
	}
	if err := a.callAgentTool("agent_engram_graph", map[string]any{}, &result); err != nil {
		return nil, err
	}
	out := &EngramGraphResult{Nodes: []EngramInfo{}, Edges: result.Edges}
	if out.Edges == nil {
		out.Edges = []EngramGraphEdge{}
	}
	for _, raw := range result.Nodes {
		var uri string
		if json.Unmarshal(raw, &uri) == nil {
			out.Nodes = append(out.Nodes, engramWire{URI: uri}.info())
			continue
		}
		var node engramWire
		if err := json.Unmarshal(raw, &node); err != nil {
			return nil, err
		}
		out.Nodes = append(out.Nodes, node.info())
	}
	return out, nil
}

// EngramSummary returns aggregate counts of engrams by proof_status and tier.
// Powers the HUD catalog summary line.
//
// It is a rollup of the same full-catalog graph EngramGraph serves, so the
// HUD's paired graph+summary poll costs one agent_engram_graph call and the
// tree and the summary strip describe one snapshot. Stub nodes (prerequisite
// placeholders, see EngramInfo.Stub) are not engrams and are not counted.
// Like the tree, the rollup is bounded by the server's full-graph node cap.
func (a *AgentBridge) EngramSummary() (*EngramSummaryResult, error) {
	graph, err := a.EngramGraph()
	if err != nil {
		return nil, err
	}
	return summarizeEngrams(graph.Nodes), nil
}

// summarizeEngrams counts the non-stub nodes by proof_status and tier. Every
// proof_status key the strip renders is present even at zero; a missing or
// non-positive tier counts as tier 1, the server's DefaultEngramTier.
func summarizeEngrams(nodes []EngramInfo) *EngramSummaryResult {
	summary := &EngramSummaryResult{
		ByStatus: map[string]int{
			"unverified": 0,
			"verified":   0,
			"stale":      0,
			"failing":    0,
		},
		ByTier: map[string]int{},
	}
	for _, node := range nodes {
		if node.Stub {
			continue
		}
		summary.Total++

		status := node.ProofStatus
		if status == "" {
			status = "unverified"
		}
		summary.ByStatus[status]++

		tier := node.Tier
		if tier < 1 {
			tier = 1
		}
		summary.ByTier[fmt.Sprintf("tier:%d", tier)]++
	}
	return summary
}

// EngramSummaryResult is the shape returned by EngramSummary.
type EngramSummaryResult struct {
	Total    int            `json:"total"`
	ByStatus map[string]int `json:"by_status"`
	ByTier   map[string]int `json:"by_tier"`
	// Degraded is always false on this real (bridge-backed) result. It exists
	// so the wire shape matches the placeholder the HUD returns when the agent
	// bridge is unconfigured, letting clients distinguish "no data available"
	// from a genuinely empty catalog.
	Degraded bool `json:"degraded"`
}
