package vendorsessions

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// claudeHeadLine is the subset of a Claude Code transcript record needed for
// session metadata. Records are heterogeneous; cwd/sessionId/timestamp appear
// on user/assistant turns but not on summary records, so the head scan reads
// a few lines until it finds them.
type claudeHeadLine struct {
	CWD         string `json:"cwd"`
	SessionID   string `json:"sessionId"`
	Timestamp   string `json:"timestamp"`
	IsSidechain *bool  `json:"isSidechain"`
}

// claudeHeadInfo is everything the head scan of one Claude transcript yields.
type claudeHeadInfo struct {
	cwd     string
	started time.Time
	title   string
	kind    string
}

// listClaude enumerates ~/.claude/projects/<flattened-cwd>/<uuid>.jsonl.
func listClaude(root string, opts ListOptions) []Session {
	if root == "" {
		return nil
	}
	dirs, err := os.ReadDir(root)
	if err != nil {
		return nil
	}

	// Cheap pre-filter: the project dir name is the session cwd with path
	// separators flattened to '-'. A cwd fragment filter can therefore skip
	// whole directories before any file IO. The authoritative cwd check
	// still happens against the transcript head below.
	dirFilter := strings.ReplaceAll(strings.Trim(opts.CwdContains, "/"), "/", "-")

	var cands []candidate
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		if dirFilter != "" && !strings.Contains(d.Name(), dirFilter) {
			continue
		}
		entries, err := os.ReadDir(filepath.Join(root, d.Name()))
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			if !opts.Since.IsZero() && info.ModTime().Before(opts.Since) {
				continue
			}
			cands = append(cands, candidate{
				path:    filepath.Join(root, d.Name(), e.Name()),
				modTime: info.ModTime(),
				size:    info.Size(),
			})
		}
	}
	sortCandidates(cands)

	var sessions []Session
	for _, c := range cands {
		if len(sessions) >= opts.Limit {
			break
		}
		head := claudeHeadMeta(c.path)
		if opts.CwdContains != "" && head.cwd != "" && !strings.Contains(head.cwd, opts.CwdContains) {
			continue
		}
		sessions = append(sessions, Session{
			Vendor:     VendorClaude,
			ID:         strings.TrimSuffix(filepath.Base(c.path), ".jsonl"),
			Path:       c.path,
			CWD:        head.cwd,
			Title:      head.title,
			Kind:       head.kind,
			StartedAt:  head.started,
			ModifiedAt: c.modTime,
			SizeBytes:  c.size,
		})
	}
	return sessions
}

// claudeHeadMeta scans the first records of a Claude transcript for cwd,
// the earliest timestamp, a session title, and the sidechain marker.
// Best-effort: returns zero values on any read or parse trouble.
//
// Title: the first titleable record wins — a leading summary record (the
// conversation's own compaction title, always line 1 on resumed sessions)
// or the first real user prompt. Command wrappers and injected reminders
// (text starting "<") never title a session.
func claudeHeadMeta(path string) claudeHeadInfo {
	var info claudeHeadInfo
	f, err := os.Open(path)
	if err != nil {
		return info
	}
	defer f.Close()

	r := bufio.NewReaderSize(f, 64*1024)
	sidechainKnown := false
	for i := 0; i < 40; i++ {
		line, err := readLimitedLine(r, 1<<20)
		if len(line) > 0 {
			var head claudeHeadLine
			if json.Unmarshal(line, &head) == nil {
				if info.cwd == "" {
					info.cwd = head.CWD
				}
				if info.started.IsZero() && head.Timestamp != "" {
					if t, terr := time.Parse(time.RFC3339Nano, head.Timestamp); terr == nil {
						info.started = t
					}
				}
				// The first record that declares sidechain-ness decides the
				// session kind: a subagent transcript is sidechain from
				// record one.
				if !sidechainKnown && head.IsSidechain != nil {
					sidechainKnown = true
					if *head.IsSidechain {
						info.kind = KindSidechain
					}
				}
			}
			if info.title == "" {
				if rec, ok := extractRawRecord(line); ok {
					if rec.Role == "summary" || (rec.Role == "user" && titleWorthy(rec.Text)) {
						info.title = titleFromText(rec.Text)
					}
				}
			}
			if info.cwd != "" && !info.started.IsZero() && info.title != "" && sidechainKnown {
				break
			}
		}
		if err != nil {
			break
		}
	}
	return info
}

// readLimitedLine reads one newline-terminated line, returning at most max
// bytes of it and discarding any overflow so the next read starts at the
// following line. The returned error is io.EOF at end of input.
func readLimitedLine(r *bufio.Reader, max int) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := r.ReadSlice('\n')
		if len(buf)+len(chunk) <= max {
			buf = append(buf, chunk...)
		} else if len(buf) < max {
			buf = append(buf, chunk[:max-len(buf)]...)
		}
		switch err {
		case nil:
			return trimEOL(buf), nil
		case bufio.ErrBufferFull:
			continue
		case io.EOF:
			return trimEOL(buf), io.EOF
		default:
			return trimEOL(buf), err
		}
	}
}

func trimEOL(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}
