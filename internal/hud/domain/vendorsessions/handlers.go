package vendorsessions

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/crb2nu/loom/internal/hud/bridge"
)

// listParamsFromQuery parses the shared list filters. Malformed integers are
// treated as unset so a sloppy client degrades to server defaults instead of
// erroring.
func listParamsFromQuery(r *http.Request) bridge.VendorSessionListParams {
	q := r.URL.Query()
	return bridge.VendorSessionListParams{
		Vendor:      strings.TrimSpace(q.Get("vendor")),
		CwdContains: strings.TrimSpace(q.Get("cwd_contains")),
		SinceHours:  atoiOrZero(q.Get("since_hours")),
		Limit:       atoiOrZero(q.Get("limit")),
	}
}

func atoiOrZero(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// validVendor mirrors the tool-side vendor check so a typo'd vendor filter
// comes back as a 400 here instead of a 502 wrapping the tool error.
func validVendor(vendor string) bool {
	return vendor == "" || vendor == "claude" || vendor == "codex"
}

// handleList returns vendor CLI session transcripts, newest-modified first:
// the local bridge's sessions merged with any federated per-host snapshots
// (mirror pushes), the latter tagged with "host".
//
// Response shape:
//
//	{"sessions": [...], "count": <int>, "degraded": <bool>}
//
// degraded=true means NO transcript source is available — no bridge AND no
// live federated host — so clients can tell "nothing can answer" apart from
// "no sessions anywhere".
func (d *Domain) handleList(w http.ResponseWriter, r *http.Request) {
	p := listParamsFromQuery(r)
	if !validVendor(p.Vendor) {
		d.deps.WriteError(w, http.StatusBadRequest, "vendor must be \"claude\" or \"codex\"", nil)
		return
	}

	var local []bridge.VendorSessionInfo
	ops := d.deps.VendorSessions()
	if ops != nil {
		sessions, err := ops.VendorSessionList(p)
		if err != nil {
			// A broken bridge is only fatal when the mirror can't cover
			// for it; with federated data the read degrades to mirror-only.
			if !d.mirror.HasLiveHosts() {
				d.deps.WriteError(w, http.StatusBadGateway, "vendor session list", err)
				return
			}
		} else {
			local = sessions
		}
	}

	merged := mergeSessions(local, d.mirror.Sessions(p), p.Limit)
	d.deps.WriteJSON(w, http.StatusOK, map[string]any{
		"sessions": merged,
		"count":    len(merged),
		"degraded": ops == nil && !d.mirror.HasLiveHosts(),
	})
}

// mergeSessions combines local bridge rows with federated rows: local rows
// win on (vendor,id) collisions (the local file is the authoritative copy),
// and the union is newest-modified first, trimmed to the request limit
// (server default 50, max 500 — pkg/vendorsessions' bounds).
func mergeSessions(local []bridge.VendorSessionInfo, mirrored []SessionOut, limit int) []SessionOut {
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	out := make([]SessionOut, 0, len(local)+len(mirrored))
	seen := make(map[string]bool, len(local))
	for _, s := range local {
		seen[s.Vendor+":"+s.ID] = true
		out = append(out, SessionOut{VendorSessionInfo: s})
	}
	for _, m := range mirrored {
		if seen[m.Vendor+":"+m.ID] {
			continue
		}
		out = append(out, m)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return parseWhen(out[i].ModifiedAt).After(parseWhen(out[j].ModifiedAt))
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// handleSearch greps vendor CLI transcripts for a substring: the local
// bridge's full-file grep merged with a scan of the federated entry tails
// (bounded windows of remote transcripts — a federated match means the
// substring appeared in that session's recent activity).
//
// Response shape:
//
//	{"query": <string>, "matches": [...], "count": <int>, "degraded": <bool>}
func (d *Domain) handleSearch(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("query"))
	if query == "" {
		d.deps.WriteError(w, http.StatusBadRequest, "query parameter is required", nil)
		return
	}

	p := bridge.VendorSessionSearchParams{
		VendorSessionListParams: listParamsFromQuery(r),
		Query:                   query,
		MaxResults:              atoiOrZero(r.URL.Query().Get("max_results")),
		MaxPerSession:           atoiOrZero(r.URL.Query().Get("max_per_session")),
	}
	if !validVendor(p.Vendor) {
		d.deps.WriteError(w, http.StatusBadRequest, "vendor must be \"claude\" or \"codex\"", nil)
		return
	}

	var local []bridge.VendorSessionMatch
	ops := d.deps.VendorSessions()
	if ops != nil {
		matches, err := ops.VendorSessionSearch(p)
		if err != nil {
			if !d.mirror.HasLiveHosts() {
				d.deps.WriteError(w, http.StatusBadGateway, fmt.Sprintf("vendor session search %q", query), err)
				return
			}
		} else {
			local = matches
		}
	}

	maxResults := p.MaxResults
	if maxResults <= 0 {
		maxResults = 30
	}
	if maxResults > 200 {
		maxResults = 200
	}
	merged := make([]MatchOut, 0, len(local))
	for _, m := range local {
		merged = append(merged, MatchOut{VendorSessionMatch: m})
	}
	for _, m := range d.mirror.Search(p) {
		if len(merged) >= maxResults {
			break
		}
		merged = append(merged, m)
	}
	if len(merged) > maxResults {
		merged = merged[:maxResults]
	}
	d.deps.WriteJSON(w, http.StatusOK, map[string]any{
		"query":    query,
		"matches":  merged,
		"count":    len(merged),
		"degraded": ops == nil && !d.mirror.HasLiveHosts(),
	})
}

func (d *Domain) handleTail(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	vendor, id := strings.TrimSpace(q.Get("vendor")), strings.TrimSpace(q.Get("id"))
	if vendor == "" || id == "" {
		d.deps.WriteError(w, http.StatusBadRequest, "vendor and id parameters are required", nil)
		return
	}
	if !validVendor(vendor) {
		d.deps.WriteError(w, http.StatusBadRequest, "vendor must be \"claude\" or \"codex\"", nil)
		return
	}
	lines := 0
	if raw := strings.TrimSpace(q.Get("lines")); raw != "" {
		var err error
		lines, err = strconv.Atoi(raw)
		if err != nil {
			d.deps.WriteError(w, http.StatusBadRequest, "lines must be an integer", err)
			return
		}
	}
	ops := d.deps.VendorSessions()
	if ops != nil {
		result, err := ops.VendorSessionTail(vendor, id, lines)
		if err == nil {
			if result.Lines == nil {
				result.Lines = []bridge.VendorSessionTailLine{}
			}
			d.deps.WriteJSON(w, http.StatusOK, result)
			return
		}
		if result, ok := d.mirror.Tail(vendor, id, lines); ok {
			d.deps.WriteJSON(w, http.StatusOK, result)
			return
		}
		d.deps.WriteError(w, http.StatusBadGateway, "vendor session tail", err)
		return
	}
	if result, ok := d.mirror.Tail(vendor, id, lines); ok {
		d.deps.WriteJSON(w, http.StatusOK, result)
		return
	}
	d.deps.WriteJSON(w, http.StatusOK, bridge.VendorSessionTailResult{
		Lines: []bridge.VendorSessionTailLine{}, Degraded: true,
	})
}

// mirrorIngestMaxBytes caps a federation push. A full snapshot is ~16
// sessions × 200 entries × ~600B ≈ 2MB; 8MB leaves generous headroom
// while keeping a misbehaving sender from ballooning HUD memory.
const mirrorIngestMaxBytes = 8 << 20

// handleMirrorIngest accepts a federated transcript snapshot from a remote
// HUD mirror (internal/hud/mirror vendorsync). The push is a full
// replacement for that host; see MirrorStore.Ingest for the entry
// carry-forward contract.
func (d *Domain) handleMirrorIngest(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		Host     string          `json:"host"`
		Sessions []IngestSession `json:"sessions"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, mirrorIngestMaxBytes))
	if err := dec.Decode(&payload); err != nil {
		d.deps.WriteError(w, http.StatusBadRequest, "decode mirror payload", err)
		return
	}
	host := strings.TrimSpace(payload.Host)
	if host == "" {
		d.deps.WriteError(w, http.StatusBadRequest, "host is required", nil)
		return
	}
	for _, s := range payload.Sessions {
		if s.Vendor != "claude" && s.Vendor != "codex" {
			d.deps.WriteError(w, http.StatusBadRequest, "vendor must be \"claude\" or \"codex\"", nil)
			return
		}
	}
	d.mirror.Ingest(host, payload.Sessions)
	d.deps.WriteJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"host":     host,
		"sessions": len(payload.Sessions),
		// Senders compare this across pushes: a change means the store
		// restarted empty and cursors must reset (full re-ship).
		"epoch": d.mirror.Epoch(),
	})
}
