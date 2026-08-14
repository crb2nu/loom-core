// Package vendorsessions reads the on-disk session transcripts written by
// vendor coding agents (Claude Code and Codex) and exposes a unified
// list/search surface over them.
//
// Claude Code stores one JSONL file per session under
// ~/.claude/projects/<flattened-cwd>/<session-uuid>.jsonl; each line carries
// the record's cwd/sessionId/timestamp. Codex stores
// ~/.codex/sessions/YYYY/MM/DD/rollout-<ts>-<uuid>.jsonl whose first line is
// a session_meta record with id/cwd/originator/source.
//
// Neither store is indexed here: listing stats files newest-first and reads
// only the head of each candidate for metadata, and search streams line by
// line under per-file and total caps. This keeps the tools safe to run
// against multi-GB session dirs.
package vendorsessions

import (
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Vendor identifiers accepted by ListOptions.Vendor.
const (
	VendorClaude = "claude"
	VendorCodex  = "codex"
)

// Session kinds beyond a plain interactive chat, derived from transcript
// head metadata. Empty means interactive.
const (
	// KindAutomation marks a Codex scheduled-automation run (session_meta
	// thread_source "automation") — recurring background work that would
	// otherwise be indistinguishable from the user's own chats.
	KindAutomation = "automation"
	// KindSidechain marks a Claude Code subagent transcript (records carry
	// isSidechain: true) — machine-spawned side work, not a user chat.
	KindSidechain = "sidechain"
)

// Session is unified metadata for one vendor session transcript.
type Session struct {
	Vendor string `json:"vendor"`
	ID     string `json:"id"`
	Path   string `json:"path"`
	CWD    string `json:"cwd,omitempty"`
	Source string `json:"source,omitempty"`
	// Title is the human handle for the session: Claude's conversation
	// summary when present, else the opening line of the first real user
	// prompt. Best-effort; empty when the head scan found neither.
	Title string `json:"title,omitempty"`
	// Kind distinguishes non-interactive transcripts ("automation",
	// "sidechain"); empty for a normal user chat.
	Kind       string    `json:"kind,omitempty"`
	StartedAt  time.Time `json:"started_at,omitempty"`
	ModifiedAt time.Time `json:"modified_at"`
	SizeBytes  int64     `json:"size_bytes"`
}

// ListOptions filters session listing.
type ListOptions struct {
	// Vendor restricts to "claude" or "codex"; empty means both.
	Vendor string
	// CwdContains keeps sessions whose working directory contains this
	// substring (case-sensitive path fragment, e.g. "services/loom-core").
	CwdContains string
	// Since keeps sessions modified at or after this time (zero = no floor).
	Since time.Time
	// Limit caps the number of sessions returned (default 50, max 500).
	Limit int
}

// Store locates the vendor session roots. Zero-value roots are filled from
// the current user's home directory.
type Store struct {
	ClaudeRoot string
	CodexRoot  string
}

// DefaultStore resolves the conventional vendor session roots under $HOME.
func DefaultStore() Store {
	home, err := os.UserHomeDir()
	if err != nil {
		return Store{}
	}
	return Store{
		ClaudeRoot: filepath.Join(home, ".claude", "projects"),
		CodexRoot:  filepath.Join(home, ".codex", "sessions"),
	}
}

func (o *ListOptions) normalize() {
	if o.Limit <= 0 {
		o.Limit = 50
	}
	if o.Limit > 500 {
		o.Limit = 500
	}
}

// List returns unified session metadata across the configured vendors,
// newest-modified first. Missing roots are skipped, not errors: an agent
// host that has never run one of the vendors simply contributes nothing.
func (s Store) List(opts ListOptions) ([]Session, error) {
	opts.normalize()

	var sessions []Session
	if opts.Vendor == "" || opts.Vendor == VendorClaude {
		sessions = append(sessions, listClaude(s.ClaudeRoot, opts)...)
	}
	if opts.Vendor == "" || opts.Vendor == VendorCodex {
		sessions = append(sessions, listCodex(s.CodexRoot, opts)...)
	}

	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].ModifiedAt.After(sessions[j].ModifiedAt)
	})
	if len(sessions) > opts.Limit {
		sessions = sessions[:opts.Limit]
	}
	return sessions, nil
}

// candidateFiles collects transcript file paths with stat info, newest
// first, bounded by mtime floor. maxFiles bounds how many survive.
type candidate struct {
	path    string
	modTime time.Time
	size    int64
}

func sortCandidates(cands []candidate) {
	sort.Slice(cands, func(i, j int) bool {
		return cands[i].modTime.After(cands[j].modTime)
	})
}
