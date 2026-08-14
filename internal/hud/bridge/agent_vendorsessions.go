// agent_vendorsessions.go — cross-vendor session transcript bridge methods.
//
// These wrap the agent-context tools agent_vendor_session_list /
// agent_vendor_session_search (cmd/mcp-agent-context/tools_bridge.go), which
// read the on-disk transcripts written by the vendor CLIs (Claude Code under
// ~/.claude/projects, Codex under ~/.codex/sessions) on the host running the
// agent-context server. The transcripts only exist on the workstation the
// vendor CLIs run on, so the agent_context server must resolve locally
// (`agent_context: prefer-local` in the registry) for these calls to return
// meaningful data — a hub-delegated agent-context pod sees empty roots.
package bridge

import "strings"

// VendorSessionInfo is unified metadata for one vendor CLI session
// transcript. Mirrors pkg/vendorsessions.Session's wire shape; timestamps
// stay RFC3339 strings so the HUD re-serializes them untouched.
type VendorSessionInfo struct {
	Vendor string `json:"vendor"`
	ID     string `json:"id"`
	Path   string `json:"path"`
	CWD    string `json:"cwd,omitempty"`
	Source string `json:"source,omitempty"`
	// Title is the session's human handle (conversation summary or first
	// user prompt); Kind tags non-interactive transcripts ("automation",
	// "sidechain"). Both best-effort, empty on older senders.
	Title      string `json:"title,omitempty"`
	Kind       string `json:"kind,omitempty"`
	StartedAt  string `json:"started_at,omitempty"`
	ModifiedAt string `json:"modified_at"`
	SizeBytes  int64  `json:"size_bytes"`
}

// VendorSessionMatch is one search hit inside a vendor session transcript.
// Mirrors pkg/vendorsessions.Match's wire shape.
type VendorSessionMatch struct {
	Vendor    string `json:"vendor"`
	SessionID string `json:"session_id"`
	Path      string `json:"path"`
	CWD       string `json:"cwd,omitempty"`
	Line      int    `json:"line"`
	Role      string `json:"role,omitempty"`
	Timestamp string `json:"timestamp,omitempty"`
	Snippet   string `json:"snippet"`
}

// VendorSessionTailLine is one parsed conversational transcript line.
type VendorSessionTailLine struct {
	Role      string `json:"role,omitempty"`
	Timestamp string `json:"timestamp,omitempty"`
	Text      string `json:"text"`
}

// VendorSessionTailResult is a bounded, chronological transcript tail.
type VendorSessionTailResult struct {
	Lines      []VendorSessionTailLine `json:"lines"`
	TotalLines int                     `json:"total_lines"`
	Truncated  bool                    `json:"truncated"`
	Degraded   bool                    `json:"degraded"`
}

// VendorSessionListParams filters the transcript listing. Zero values are
// omitted from the tool call so the server-side defaults apply.
type VendorSessionListParams struct {
	// Vendor restricts to "claude" or "codex"; empty means both.
	Vendor string
	// CwdContains keeps sessions whose working directory contains this
	// path fragment (e.g. "services/loom-core").
	CwdContains string
	// SinceHours keeps sessions modified within the last N hours.
	SinceHours int
	// Limit caps returned sessions (server default 50, max 500).
	Limit int
}

func (p VendorSessionListParams) toArgs() map[string]any {
	args := map[string]any{}
	if v := strings.TrimSpace(p.Vendor); v != "" {
		args["vendor"] = v
	}
	if c := strings.TrimSpace(p.CwdContains); c != "" {
		args["cwd_contains"] = c
	}
	if p.SinceHours > 0 {
		args["since_hours"] = p.SinceHours
	}
	if p.Limit > 0 {
		args["limit"] = p.Limit
	}
	return args
}

// VendorSessionSearchParams controls transcript substring search.
type VendorSessionSearchParams struct {
	VendorSessionListParams
	// Query is the case-insensitive substring to find. Required.
	Query string
	// MaxResults caps total hits (server default 30, max 200).
	MaxResults int
	// MaxPerSession caps hits per transcript (server default 3, max 20).
	MaxPerSession int
}

// VendorSessionList lists vendor CLI session transcripts newest-first.
func (a *AgentBridge) VendorSessionList(p VendorSessionListParams) ([]VendorSessionInfo, error) {
	var result struct {
		Sessions []VendorSessionInfo `json:"sessions"`
	}
	if err := a.callAgentTool("agent_vendor_session_list", p.toArgs(), &result); err != nil {
		return nil, err
	}
	return result.Sessions, nil
}

// VendorSessionSearch greps vendor CLI transcripts for a substring.
func (a *AgentBridge) VendorSessionSearch(p VendorSessionSearchParams) ([]VendorSessionMatch, error) {
	args := p.toArgs()
	args["query"] = strings.TrimSpace(p.Query)
	if p.MaxResults > 0 {
		args["max_results"] = p.MaxResults
	}
	if p.MaxPerSession > 0 {
		args["max_per_session"] = p.MaxPerSession
	}
	var result struct {
		Matches []VendorSessionMatch `json:"matches"`
	}
	if err := a.callAgentTool("agent_vendor_session_search", args, &result); err != nil {
		return nil, err
	}
	return result.Matches, nil
}

// VendorSessionTail returns the newest parsed lines of one transcript.
func (a *AgentBridge) VendorSessionTail(vendor, id string, maxLines int) (*VendorSessionTailResult, error) {
	if maxLines <= 0 {
		maxLines = 200
	} else if maxLines > 500 {
		maxLines = 500
	}
	result := &VendorSessionTailResult{Lines: []VendorSessionTailLine{}}
	err := a.callAgentTool("agent_vendor_session_tail", map[string]any{
		"vendor": strings.TrimSpace(vendor), "id": strings.TrimSpace(id), "lines": maxLines,
	}, result)
	return result, err
}
