// Vendor transcript federation — the second stream on the mirror loop.
//
// The presence heartbeat makes the workstation's agents visible on the
// cluster HUD; this sync makes their vendor CLI transcripts (Claude Code +
// Codex session JSONL) visible too. Every VendorInterval the mirror lists
// recent local sessions via pkg/vendorsessions and POSTs metadata — plus a
// bounded tail of extracted lines for sessions that changed — to the remote
// HUD's /api/vendor-sessions/mirror ingest. The remote vendorsessions
// domain merges the pushed snapshot into its list/search responses, tagged
// with this host's name, so the web Operator Deck and the iOS companion
// can browse and grep workstation transcripts from anywhere.
//
// Raw JSONL never leaves the host: entries are whitespace-normalized,
// per-line-capped extracts (pkg/vendorsessions.Tail), so a full push is a
// few hundred KB, and steady-state cycles ship metadata only.
package mirror

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"time"

	"github.com/crb2nu/loom/pkg/vendorsessions"
)

const (
	defaultVendorInterval = 60 * time.Second
	minVendorInterval     = 15 * time.Second

	// vendorSyncSessionLimit bounds how many recent sessions are
	// federated; vendorSyncWindow is the modified-at floor.
	vendorSyncSessionLimit = 16
	vendorSyncWindow       = 7 * 24 * time.Hour
)

// vendorPushBudgetBytes caps the entry payload of a single push. Ingress
// edges default to small body limits (nginx: 1m — a full 16-session tail
// ship 413'd against exactly that on first rollout), so stay well under:
// sessions whose tails don't fit are deferred, keep their stale cursors,
// and ship on an immediate follow-up cycle. Var, not const, as a test seam.
var vendorPushBudgetBytes = 640 << 10

// entriesCost approximates the JSON weight of a session's entry tail.
func entriesCost(entries []vendorsessions.Entry) int {
	n := 0
	for _, e := range entries {
		n += len(e.Text) + len(e.Role) + len(e.Timestamp) + 48
	}
	return n
}

// vendorCursor remembers the last federated state of one transcript so
// unchanged sessions ship metadata-only (no tail re-extraction, no bytes).
type vendorCursor struct {
	modTime time.Time
	size    int64
}

// defaultVendorStore resolves the transcript roots, honoring the same env
// overrides as the agent-context server (pkg/agentcontext/config.go) so the
// mirror and the local bridge always read the same directories.
func defaultVendorStore() vendorsessions.Store {
	store := vendorsessions.DefaultStore()
	if v := strings.TrimSpace(os.Getenv("AGENT_CONTEXT_CLAUDE_SESSIONS_DIR")); v != "" {
		store.ClaudeRoot = v
	}
	if v := strings.TrimSpace(os.Getenv("AGENT_CONTEXT_CODEX_SESSIONS_DIR")); v != "" {
		store.CodexRoot = v
	}
	return store
}

// SetVendorStore overrides the transcript roots and host label. Test seam;
// call before Start. A nil store disables the sync.
func (s *Service) SetVendorStore(store *vendorsessions.Store, host string) {
	s.vendorStore = store
	s.vendorHost = host
}

func (s *Service) vendorInterval() time.Duration {
	d := s.cfg.VendorInterval
	if d <= 0 {
		d = defaultVendorInterval
	}
	if d < minVendorInterval {
		d = minVendorInterval
	}
	return d
}

// maybeVendorSync runs a vendor sync when one is due. Called from the
// mirror loop after each presence cycle so the whole service stays a
// single goroutine.
func (s *Service) maybeVendorSync(ctx context.Context) {
	if s.vendorStore == nil {
		return
	}
	if !s.lastVendorSync.IsZero() && time.Since(s.lastVendorSync) < s.vendorInterval() {
		return
	}
	s.vendorSyncOnce(ctx)
}

// VendorSyncOnce runs a single vendor transcript sync synchronously.
// Exposed for tests.
func (s *Service) VendorSyncOnce(ctx context.Context) { s.vendorSyncOnce(ctx) }

func (s *Service) vendorSyncOnce(ctx context.Context) {
	// Mark the attempt up front: a failing remote must not turn the
	// mirror loop into a hot retry of multi-KB pushes every 15s.
	s.lastVendorSync = time.Now()

	sessions, err := s.vendorStore.List(vendorsessions.ListOptions{
		Since: time.Now().Add(-vendorSyncWindow),
		Limit: vendorSyncSessionLimit,
	})
	if err != nil {
		s.logErr("vendor_list", err)
		return
	}

	type sessionPayload struct {
		Vendor     string                 `json:"vendor"`
		ID         string                 `json:"id"`
		Path       string                 `json:"path"`
		CWD        string                 `json:"cwd,omitempty"`
		Source     string                 `json:"source,omitempty"`
		Title      string                 `json:"title,omitempty"`
		Kind       string                 `json:"kind,omitempty"`
		StartedAt  string                 `json:"started_at,omitempty"`
		ModifiedAt string                 `json:"modified_at"`
		SizeBytes  int64                  `json:"size_bytes"`
		Entries    []vendorsessions.Entry `json:"entries,omitempty"`
	}

	out := make([]sessionPayload, 0, len(sessions))
	next := make(map[string]vendorCursor, len(sessions))
	budget := vendorPushBudgetBytes
	deferred := false
	for _, sess := range sessions {
		key := sess.Vendor + ":" + sess.ID

		p := sessionPayload{
			Vendor:     sess.Vendor,
			ID:         sess.ID,
			Path:       sess.Path,
			CWD:        sess.CWD,
			Source:     sess.Source,
			Title:      sess.Title,
			Kind:       sess.Kind,
			ModifiedAt: sess.ModifiedAt.Format(time.RFC3339),
			SizeBytes:  sess.SizeBytes,
		}
		if !sess.StartedAt.IsZero() {
			p.StartedAt = sess.StartedAt.Format(time.RFC3339)
		}
		cur, seen := s.vendorCursors[key]
		changed := !seen || !cur.modTime.Equal(sess.ModifiedAt) || cur.size != sess.SizeBytes
		switch {
		case !changed:
			next[key] = cur
		default:
			entries := vendorsessions.Tail(sess, vendorsessions.TailOptions{})
			if cost := entriesCost(entries); cost <= budget {
				p.Entries = entries
				budget -= cost
				next[key] = vendorCursor{modTime: sess.ModifiedAt, size: sess.SizeBytes}
			} else {
				// Over budget this cycle: ship metadata only, keep the
				// stale cursor (absent stays absent) so this session
				// still reads as changed, and follow up immediately.
				deferred = true
				if seen {
					next[key] = cur
				}
			}
		}
		out = append(out, p)
	}

	// A push built with no prior cursors and no budget deferrals carried
	// entries for every session — the receiver now has everything
	// regardless of its epoch.
	fullShip := len(s.vendorCursors) == 0 && !deferred

	body := map[string]any{
		"host":     s.vendorHost,
		"sessions": out,
	}
	respBody, err := s.postJSON(ctx, "/api/vendor-sessions/mirror", body)
	if err != nil {
		// Cursors stay uncommitted so the next cycle re-ships the tails
		// the remote never received.
		s.logErr("vendor_push", err)
		return
	}

	// Epoch handshake: the receiver's store identifier changes when it
	// restarts empty (deploy, pod roll). A change on a delta push means
	// our cursors describe entries the receiver no longer has — reset so
	// the next cycle re-ships full tails. A receiver predating the ingest
	// route (SPA catch-all body) parses to "" and never triggers a reset
	// until a real epoch appears.
	var ack struct {
		Epoch string `json:"epoch"`
	}
	_ = json.Unmarshal(respBody, &ack)
	if ack.Epoch != s.vendorEpoch {
		s.vendorEpoch = ack.Epoch
		if !fullShip {
			s.vendorCursors = make(map[string]vendorCursor)
			// Re-sync on the next loop tick instead of waiting a full
			// vendor interval with a knowingly empty receiver.
			s.lastVendorSync = time.Time{}
			return
		}
	}
	s.vendorCursors = next
	if deferred {
		// Budget-deferred tails catch up on the next loop tick rather
		// than a full vendor interval.
		s.lastVendorSync = time.Time{}
	}
}
