package vendorsessions

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func tailFixtureSession(t *testing.T, lines []string) Session {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tail.jsonl")
	writeFixture(t, path, strings.Join(lines, "\n")+"\n", time.Now())
	return Session{Vendor: VendorClaude, ID: "tail-test", Path: path}
}

func TestTailExtractsEntriesWithLineNumbers(t *testing.T) {
	t.Parallel()
	sess := tailFixtureSession(t, []string{
		`{"type":"user","timestamp":"2026-07-26T10:00:00.000Z","message":{"role":"user","content":"start the marmalade run"}}`,
		`{"type":"assistant","timestamp":"2026-07-26T10:00:05.500Z","message":{"role":"assistant","content":[{"type":"text","text":"running it now"}]}}`,
	})

	entries := Tail(sess, TailOptions{})
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(entries))
	}
	if entries[0].Line != 1 || entries[1].Line != 2 {
		t.Fatalf("line numbers = %d,%d, want 1,2", entries[0].Line, entries[1].Line)
	}
	if entries[0].Role != "user" || entries[1].Role != "assistant" {
		t.Fatalf("roles = %q,%q", entries[0].Role, entries[1].Role)
	}
	if entries[0].Timestamp != "2026-07-26T10:00:00Z" {
		t.Fatalf("timestamp = %q, want RFC3339-normalized", entries[0].Timestamp)
	}
	if !strings.Contains(entries[0].Text, "marmalade") {
		t.Fatalf("text lost content: %q", entries[0].Text)
	}
}

func TestTailKeepsOnlyNewestEntries(t *testing.T) {
	t.Parallel()
	var lines []string
	for i := 1; i <= 30; i++ {
		lines = append(lines, fmt.Sprintf(`{"type":"user","message":{"role":"user","content":"line number %03d"}}`, i))
	}
	sess := tailFixtureSession(t, lines)

	entries := Tail(sess, TailOptions{MaxEntries: 10})
	if len(entries) != 10 {
		t.Fatalf("entries = %d, want 10", len(entries))
	}
	if !strings.Contains(entries[0].Text, "021") || !strings.Contains(entries[9].Text, "030") {
		t.Fatalf("window wrong: first=%q last=%q", entries[0].Text, entries[9].Text)
	}
	if entries[0].Line != 21 {
		t.Fatalf("first kept line = %d, want 21", entries[0].Line)
	}
}

func TestTailCapsTextWithEllipsis(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", 500)
	sess := tailFixtureSession(t, []string{
		`{"type":"user","message":{"role":"user","content":"` + long + `"}}`,
	})

	entries := Tail(sess, TailOptions{MaxTextBytes: 80})
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	if got := len(entries[0].Text); got > 80+len("…") {
		t.Fatalf("text length = %d, want <= %d", got, 80+len("…"))
	}
	if !strings.HasSuffix(entries[0].Text, "…") {
		t.Fatalf("capped text missing ellipsis: %q", entries[0].Text)
	}
}

func TestTailSeekedLargeFileDropsLineNumbers(t *testing.T) {
	t.Parallel()
	var lines []string
	for i := 1; i <= 50; i++ {
		lines = append(lines, fmt.Sprintf(`{"type":"user","message":{"role":"user","content":"padding padding padding %03d"}}`, i))
	}
	sess := tailFixtureSession(t, lines)

	// A window far smaller than the file forces the seek path: the partial
	// first line is discarded and absolute numbering is unknown.
	entries := Tail(sess, TailOptions{MaxTailBytes: 256})
	if len(entries) == 0 {
		t.Fatal("no entries from seeked tail")
	}
	for _, e := range entries {
		if e.Line != 0 {
			t.Fatalf("seeked tail should have line=0, got %d (%q)", e.Line, e.Text)
		}
	}
	if !strings.Contains(entries[len(entries)-1].Text, "050") {
		t.Fatalf("seeked tail should end at the newest line, got %q", entries[len(entries)-1].Text)
	}
}

func TestTailMissingFileIsNil(t *testing.T) {
	t.Parallel()
	entries := Tail(Session{Vendor: VendorClaude, ID: "gone", Path: filepath.Join(t.TempDir(), "missing.jsonl")}, TailOptions{})
	if entries != nil {
		t.Fatalf("entries = %v, want nil", entries)
	}
}
