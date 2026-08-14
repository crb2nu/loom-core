package vendorsessions

import (
	"bufio"
	"io"
	"os"
)

// Entry is one extracted transcript line — the unit the HUD federation
// mirror ships to a remote HUD so off-workstation viewers can search
// recent activity without the multi-MB raw JSONL ever leaving the host.
type Entry struct {
	// Line is the 1-based line number in the transcript, or 0 when the
	// tail scan seeked past the file prefix and absolute numbering is
	// unknown.
	Line      int    `json:"line,omitempty"`
	Role      string `json:"role,omitempty"`
	Timestamp string `json:"timestamp,omitempty"`
	// Text is the whitespace-normalized line content, capped at
	// TailOptions.MaxTextBytes (with a trailing ellipsis when truncated).
	Text string `json:"text"`
}

// TailOptions bounds a Tail extraction. Zero values fall back to defaults
// sized for the federation mirror's per-cycle budget.
type TailOptions struct {
	// MaxEntries caps how many trailing entries are kept (default 200, max 500).
	MaxEntries int
	// MaxTailBytes is the scan window measured from EOF (default 2MB).
	// Files larger than this are seeked, which forfeits line numbers.
	MaxTailBytes int64
	// MaxTextBytes caps each entry's normalized text (default 600).
	MaxTextBytes int
}

func (o *TailOptions) normalize() {
	if o.MaxEntries <= 0 {
		o.MaxEntries = 200
	}
	if o.MaxEntries > 500 {
		o.MaxEntries = 500
	}
	if o.MaxTailBytes <= 0 {
		o.MaxTailBytes = 2 << 20
	}
	if o.MaxTextBytes <= 0 {
		o.MaxTextBytes = 600
	}
}

// Tail extracts the newest entries of one session transcript, oldest first.
// Read errors degrade to "whatever was extracted so far" — a transcript
// being appended to mid-read must never fail the whole sync cycle.
func Tail(sess Session, opts TailOptions) []Entry {
	opts.normalize()

	f, err := os.Open(sess.Path)
	if err != nil {
		return nil
	}
	defer f.Close()

	lineNum := 0
	seeked := false
	if info, serr := f.Stat(); serr == nil && info.Size() > opts.MaxTailBytes {
		if _, serr := f.Seek(info.Size()-opts.MaxTailBytes, io.SeekStart); serr == nil {
			seeked = true
		}
	}

	r := bufio.NewReaderSize(f, 256*1024)
	if seeked {
		// Discard the partial line the seek landed inside.
		_, _ = readLimitedLine(r, 4<<20)
	}

	// Ring buffer over the last MaxEntries extracted lines: append, and
	// compact once the slice doubles so memory stays bounded.
	var entries []Entry
	for {
		line, rerr := readLimitedLine(r, 4<<20)
		if len(line) > 0 {
			lineNum++
			if e, ok := extractEntry(line, opts.MaxTextBytes); ok {
				if !seeked {
					e.Line = lineNum
				}
				entries = append(entries, e)
				if len(entries) >= opts.MaxEntries*2 {
					entries = append(entries[:0:0], entries[len(entries)-opts.MaxEntries:]...)
				}
			}
		}
		if rerr != nil {
			break
		}
	}
	if len(entries) > opts.MaxEntries {
		entries = append(entries[:0:0], entries[len(entries)-opts.MaxEntries:]...)
	}
	return entries
}

// extractEntry normalizes one transcript line into an Entry via the shared
// extractor: only records with conversational text survive (tool traffic,
// snapshots, and settings churn are skipped), and the shipped text is what
// was said — not the raw JSONL — so federated search reads clean.
func extractEntry(line []byte, maxText int) (Entry, bool) {
	rec, ok := extractRecord(line)
	if !ok {
		return Entry{}, false
	}
	text := rec.Text
	if len(text) > maxText {
		cut := maxText
		for cut > 0 && !isASCIIBoundary(text[cut]) {
			cut--
		}
		text = text[:cut] + "…"
	}
	return Entry{Text: text, Role: rec.Role, Timestamp: rec.Timestamp}, true
}
