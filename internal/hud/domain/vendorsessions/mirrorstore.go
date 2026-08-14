package vendorsessions

import (
	"crypto/rand"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/crb2nu/loom/internal/hud/bridge"
)

// mirrorHostTTL drops a federated host whose mirror stopped pushing —
// the sender's cadence is ~60s, so five minutes of silence means the
// workstation is gone (asleep, offline, mirror disabled), and stale
// transcripts should not linger in the deck.
const mirrorHostTTL = 5 * time.Minute

// MirroredEntry is one extracted transcript line pushed by a remote
// mirror (pkg/vendorsessions.Entry on the sender side).
type MirroredEntry struct {
	Line      int    `json:"line,omitempty"`
	Role      string `json:"role,omitempty"`
	Timestamp string `json:"timestamp,omitempty"`
	Text      string `json:"text"`
}

// mirroredSession is one federated transcript: bridge-shaped metadata plus
// the searchable tail of extracted entries.
type mirroredSession struct {
	info bridge.VendorSessionInfo
	// modified is the parsed ModifiedAt used for filtering and merge
	// ordering; zero when the sender's string didn't parse.
	modified time.Time
	entries  []MirroredEntry
}

// IngestSession is the wire shape of one session in a mirror push.
// Entries == nil means "unchanged since my last push — keep what you have";
// an empty non-nil slice explicitly clears them.
type IngestSession struct {
	Vendor     string           `json:"vendor"`
	ID         string           `json:"id"`
	Path       string           `json:"path"`
	CWD        string           `json:"cwd,omitempty"`
	Source     string           `json:"source,omitempty"`
	Title      string           `json:"title,omitempty"`
	Kind       string           `json:"kind,omitempty"`
	StartedAt  string           `json:"started_at,omitempty"`
	ModifiedAt string           `json:"modified_at"`
	SizeBytes  int64            `json:"size_bytes"`
	Entries    *[]MirroredEntry `json:"entries,omitempty"`
}

type hostSnapshot struct {
	sessions   map[string]*mirroredSession // key: vendor + ":" + id
	receivedAt time.Time
}

// MirrorStore holds federated transcript snapshots per source host.
// In-memory by design: the sender re-pushes a full snapshot every cycle,
// so a HUD restart repopulates within one mirror interval.
type MirrorStore struct {
	mu    sync.RWMutex
	hosts map[string]*hostSnapshot
	now   func() time.Time
	// epoch identifies this store instance. It rides every ingest response
	// so senders can detect a receiver restart (fresh, empty store) and
	// reset their "already shipped" cursors — otherwise entries for
	// unchanged transcripts would never re-ship and federated search would
	// silently miss them until the file next changed.
	epoch string
}

// NewMirrorStore builds an empty store with a fresh epoch.
func NewMirrorStore() *MirrorStore {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		// Timestamp fallback still changes across restarts, which is the
		// only property the epoch needs.
		return &MirrorStore{hosts: make(map[string]*hostSnapshot), now: time.Now,
			epoch: strconv.FormatInt(time.Now().UnixNano(), 36)}
	}
	return &MirrorStore{hosts: make(map[string]*hostSnapshot), now: time.Now,
		epoch: hex.EncodeToString(buf)}
}

// Epoch returns the store-instance identifier.
func (st *MirrorStore) Epoch() string { return st.epoch }

// Ingest replaces host's session set with the pushed snapshot. Sessions
// whose Entries are nil keep the previously stored entries (the sender
// skips re-extracting unchanged transcripts); sessions absent from the
// push are dropped (the push is a full snapshot of what the host wants
// federated).
func (st *MirrorStore) Ingest(host string, sessions []IngestSession) {
	st.mu.Lock()
	defer st.mu.Unlock()

	prev := st.hosts[host]
	snap := &hostSnapshot{
		sessions:   make(map[string]*mirroredSession, len(sessions)),
		receivedAt: st.now(),
	}
	for _, in := range sessions {
		key := in.Vendor + ":" + in.ID
		ms := &mirroredSession{
			info: bridge.VendorSessionInfo{
				Vendor:     in.Vendor,
				ID:         in.ID,
				Path:       in.Path,
				CWD:        in.CWD,
				Source:     in.Source,
				Title:      in.Title,
				Kind:       in.Kind,
				StartedAt:  in.StartedAt,
				ModifiedAt: in.ModifiedAt,
				SizeBytes:  in.SizeBytes,
			},
			modified: parseWhen(in.ModifiedAt),
		}
		switch {
		case in.Entries != nil:
			ms.entries = *in.Entries
		case prev != nil:
			if old, ok := prev.sessions[key]; ok {
				ms.entries = old.entries
			}
		}
		snap.sessions[key] = ms
	}
	st.hosts[host] = snap
}

// HasLiveHosts reports whether any host pushed within the TTL. Used for
// the degraded flag: mirror data counts as a live transcript source even
// when the local bridge is absent.
func (st *MirrorStore) HasLiveHosts() bool {
	st.mu.RLock()
	defer st.mu.RUnlock()
	cutoff := st.now().Add(-mirrorHostTTL)
	for _, snap := range st.hosts {
		if snap.receivedAt.After(cutoff) {
			return true
		}
	}
	return false
}

// SessionOut is a list-response row: bridge metadata plus the source host
// for federated rows (empty for the HUD's own bridge results). Embedding
// keeps the wire shape flat and byte-compatible for host-less rows.
type SessionOut struct {
	bridge.VendorSessionInfo
	Host string `json:"host,omitempty"`
}

// MatchOut is a search-response row, same host convention as SessionOut.
type MatchOut struct {
	bridge.VendorSessionMatch
	Host string `json:"host,omitempty"`
}

// Tail returns the newest mirrored entries for one live session in
// chronological order. The boolean distinguishes an empty transcript from a
// session that is absent or stale.
func (st *MirrorStore) Tail(vendor, id string, maxLines int) (bridge.VendorSessionTailResult, bool) {
	if maxLines <= 0 {
		maxLines = 200
	} else if maxLines > 500 {
		maxLines = 500
	}

	st.mu.RLock()
	defer st.mu.RUnlock()
	cutoff := st.now().Add(-mirrorHostTTL)
	key := vendor + ":" + id
	for _, snap := range st.hosts {
		if !snap.receivedAt.After(cutoff) {
			continue
		}
		ms, ok := snap.sessions[key]
		if !ok {
			continue
		}
		start := len(ms.entries) - maxLines
		if start < 0 {
			start = 0
		}
		lines := make([]bridge.VendorSessionTailLine, 0, len(ms.entries)-start)
		for _, entry := range ms.entries[start:] {
			lines = append(lines, bridge.VendorSessionTailLine{
				Role: entry.Role, Timestamp: entry.Timestamp, Text: entry.Text,
			})
		}
		total := len(ms.entries)
		if len(ms.entries) > 0 && ms.entries[len(ms.entries)-1].Line > 0 {
			total = ms.entries[len(ms.entries)-1].Line
		}
		return bridge.VendorSessionTailResult{
			Lines: lines, TotalLines: total,
			Truncated: total > len(lines) || start > 0,
		}, true
	}
	return bridge.VendorSessionTailResult{}, false
}

// Sessions returns federated sessions passing the shared list filters,
// newest-modified first.
func (st *MirrorStore) Sessions(p bridge.VendorSessionListParams) []SessionOut {
	st.mu.RLock()
	defer st.mu.RUnlock()

	var floor time.Time
	if p.SinceHours > 0 {
		floor = st.now().Add(-time.Duration(p.SinceHours) * time.Hour)
	}
	cutoff := st.now().Add(-mirrorHostTTL)

	var out []SessionOut
	for host, snap := range st.hosts {
		if !snap.receivedAt.After(cutoff) {
			continue
		}
		for _, ms := range snap.sessions {
			if !matchesList(ms, p, floor) {
				continue
			}
			out = append(out, SessionOut{VendorSessionInfo: ms.info, Host: host})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return parseWhen(out[i].ModifiedAt).After(parseWhen(out[j].ModifiedAt))
	})
	return out
}

// Search greps the federated entry tails for a case-insensitive substring,
// honoring the same per-session and total caps as pkg/vendorsessions.
func (st *MirrorStore) Search(p bridge.VendorSessionSearchParams) []MatchOut {
	needle := strings.ToLower(strings.TrimSpace(p.Query))
	if needle == "" {
		return nil
	}
	maxResults := p.MaxResults
	if maxResults <= 0 {
		maxResults = 30
	}
	if maxResults > 200 {
		maxResults = 200
	}
	maxPerSession := p.MaxPerSession
	if maxPerSession <= 0 {
		maxPerSession = 3
	}
	if maxPerSession > 20 {
		maxPerSession = 20
	}

	st.mu.RLock()
	defer st.mu.RUnlock()

	var floor time.Time
	if p.SinceHours > 0 {
		floor = st.now().Add(-time.Duration(p.SinceHours) * time.Hour)
	}
	cutoff := st.now().Add(-mirrorHostTTL)

	// Deterministic host order so paging/caps don't shuffle between polls.
	hosts := make([]string, 0, len(st.hosts))
	for host := range st.hosts {
		hosts = append(hosts, host)
	}
	sort.Strings(hosts)

	var out []MatchOut
	for _, host := range hosts {
		snap := st.hosts[host]
		if !snap.receivedAt.After(cutoff) {
			continue
		}
		// Newest session first, mirroring pkg/vendorsessions.Search.
		sessions := make([]*mirroredSession, 0, len(snap.sessions))
		for _, ms := range snap.sessions {
			sessions = append(sessions, ms)
		}
		sort.Slice(sessions, func(i, j int) bool {
			return sessions[i].modified.After(sessions[j].modified)
		})
		for _, ms := range sessions {
			if len(out) >= maxResults {
				return out
			}
			if !matchesList(ms, p.VendorSessionListParams, floor) {
				continue
			}
			perSession := 0
			for _, e := range ms.entries {
				if perSession >= maxPerSession || len(out) >= maxResults {
					break
				}
				if !strings.Contains(strings.ToLower(e.Text), needle) {
					continue
				}
				out = append(out, MatchOut{
					VendorSessionMatch: bridge.VendorSessionMatch{
						Vendor:    ms.info.Vendor,
						SessionID: ms.info.ID,
						Path:      ms.info.Path,
						CWD:       ms.info.CWD,
						Line:      e.Line,
						Role:      e.Role,
						Timestamp: e.Timestamp,
						// Entries are already normalized and capped at
						// extraction time, so the text is its own snippet.
						Snippet: e.Text,
					},
					Host: host,
				})
				perSession++
			}
		}
	}
	return out
}

func matchesList(ms *mirroredSession, p bridge.VendorSessionListParams, floor time.Time) bool {
	if p.Vendor != "" && ms.info.Vendor != p.Vendor {
		return false
	}
	if p.CwdContains != "" && !strings.Contains(ms.info.CWD, p.CwdContains) {
		return false
	}
	if !floor.IsZero() && ms.modified.Before(floor) {
		return false
	}
	return true
}

// parseWhen parses the RFC3339(Nano) timestamps the sender ships; zero on
// failure so unparseable rows sort last instead of erroring.
func parseWhen(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	return time.Time{}
}
