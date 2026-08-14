package vendorsessions

import (
	"bufio"
	"os"
	"strings"
)

// Match is one search hit inside a session transcript.
type Match struct {
	Vendor    string `json:"vendor"`
	SessionID string `json:"session_id"`
	Path      string `json:"path"`
	CWD       string `json:"cwd,omitempty"`
	Line      int    `json:"line"`
	Role      string `json:"role,omitempty"`
	Timestamp string `json:"timestamp,omitempty"`
	Snippet   string `json:"snippet"`
}

// SearchOptions controls transcript search.
type SearchOptions struct {
	ListOptions
	// Query is the case-insensitive substring to find. Required.
	Query string
	// MaxPerSession caps hits per transcript (default 3, max 20).
	MaxPerSession int
	// MaxResults caps total hits (default 30, max 200).
	MaxResults int
	// MaxScanBytes caps bytes scanned per transcript (default 16MB).
	MaxScanBytes int64
	// SnippetRadius is how many characters of context surround the match
	// on each side (default 120).
	SnippetRadius int
}

func (o *SearchOptions) normalize() {
	o.ListOptions.normalize()
	if o.MaxPerSession <= 0 {
		o.MaxPerSession = 3
	}
	if o.MaxPerSession > 20 {
		o.MaxPerSession = 20
	}
	if o.MaxResults <= 0 {
		o.MaxResults = 30
	}
	if o.MaxResults > 200 {
		o.MaxResults = 200
	}
	if o.MaxScanBytes <= 0 {
		o.MaxScanBytes = 16 << 20
	}
	if o.SnippetRadius <= 0 {
		o.SnippetRadius = 120
	}
}

// Search scans the newest transcripts of both vendors for a substring.
// Candidate sessions honor the embedded ListOptions (vendor, cwd, since,
// limit); the limit bounds how many transcripts are scanned.
//
// Matching runs against the EXTRACTED conversational text of each record
// (extractRecord), not the raw JSONL: tool traffic, file snapshots, and
// structural JSON keys neither match nor pollute snippets. A hit's snippet
// is a readable window of what was actually said.
func (s Store) Search(opts SearchOptions) ([]Match, error) {
	opts.normalize()
	if strings.TrimSpace(opts.Query) == "" {
		return nil, nil
	}
	sessions, err := s.List(opts.ListOptions)
	if err != nil {
		return nil, err
	}

	needle := strings.ToLower(opts.Query)
	var matches []Match
	for _, sess := range sessions {
		if len(matches) >= opts.MaxResults {
			break
		}
		matches = append(matches, searchFile(sess, needle, opts, opts.MaxResults-len(matches))...)
	}
	return matches, nil
}

func searchFile(sess Session, needle string, opts SearchOptions, budget int) []Match {
	f, err := os.Open(sess.Path)
	if err != nil {
		return nil
	}
	defer f.Close()

	perFile := opts.MaxPerSession
	if budget < perFile {
		perFile = budget
	}
	if perFile <= 0 {
		return nil
	}

	r := bufio.NewReaderSize(f, 256*1024)
	var (
		matches []Match
		lineNum int
		scanned int64
	)
	for scanned < opts.MaxScanBytes && len(matches) < perFile {
		line, rerr := readLimitedLine(r, 4<<20)
		if len(line) > 0 {
			lineNum++
			scanned += int64(len(line))
			if rec, ok := extractRecord(line); ok {
				if idx := strings.Index(strings.ToLower(rec.Text), needle); idx >= 0 {
					matches = append(matches, Match{
						Vendor:    sess.Vendor,
						SessionID: sess.ID,
						Path:      sess.Path,
						CWD:       sess.CWD,
						Line:      lineNum,
						Role:      rec.Role,
						Timestamp: rec.Timestamp,
						Snippet:   snippet(rec.Text, idx, len(needle), opts.SnippetRadius),
					})
				}
			}
		}
		if rerr != nil {
			break
		}
	}
	return matches
}

// snippet returns a whitespace-normalized window around the match.
func snippet(line string, idx, matchLen, radius int) string {
	start := idx - radius
	if start < 0 {
		start = 0
	}
	end := idx + matchLen + radius
	if end > len(line) {
		end = len(line)
	}
	// Avoid splitting multi-byte runes at the window edges.
	for start > 0 && start < len(line) && !isASCIIBoundary(line[start]) {
		start--
	}
	for end < len(line) && !isASCIIBoundary(line[end]) {
		end++
	}
	s := strings.Join(strings.Fields(line[start:end]), " ")
	if start > 0 {
		s = "…" + s
	}
	if end < len(line) {
		s += "…"
	}
	return s
}

// isASCIIBoundary reports whether the byte begins a UTF-8 rune (i.e. is not
// a continuation byte), making it a safe slice point.
func isASCIIBoundary(b byte) bool {
	return b&0xC0 != 0x80
}
