package bridge

import "encoding/json"

// EngramProof describes the proof attached to an engram.
type EngramProof struct {
	Kind string   `json:"kind,omitempty"`
	Refs []string `json:"refs"`
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
}

func (w engramWire) info() EngramInfo {
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
		LastVerifiedAt: w.LastVerifiedAt, Proof: w.Proof}
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

// EngramGraph returns the prerequisite graph. It accepts both the current
// URI-only node payload and the richer object payload used by newer servers.
func (a *AgentBridge) EngramGraph() (*EngramGraphResult, error) {
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
