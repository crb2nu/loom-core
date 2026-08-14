package backend

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestStreamExecContext_RelaysWatchdogCancellation(t *testing.T) {
	parent, cancelParent := context.WithCancelCause(context.Background())
	ctx, cancel := streamExecContext(parent, time.Hour)
	defer cancel()

	stallErr := errors.New("liveness watchdog stalled")
	cancelParent(stallErr)
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("stream exec context did not cancel promptly after watchdog cancellation")
	}
	if got := context.Cause(ctx); !errors.Is(got, stallErr) {
		t.Fatalf("stream exec cancellation cause = %v, want %v", got, stallErr)
	}
}

func TestStreamExecContext_IgnoresCallerDeadline(t *testing.T) {
	parent, cancelParent := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancelParent()
	ctx, cancel := streamExecContext(parent, time.Hour)
	defer cancel()

	<-parent.Done()
	select {
	case <-ctx.Done():
		t.Fatal("caller deadline unexpectedly canceled stream exec context")
	case <-time.After(50 * time.Millisecond):
	}
}

// TestTailRing_KeepsLastLinesInOrder is the regression guard for the off-by-one
// that corrupted wrapped stdout tails. Writing at total%size instead of
// (total-1)%size evicted the SECOND line on the first wrap and left the first
// alive, so the reader replayed that stale line inside the tail.
//
// The damage window is exactly the FIRST wrap — line counts in (size, 2*size).
// Past 2*size the stale entry has been overwritten and the old indices happened
// to agree with the correct ones again, so a large-N case proves nothing about
// this bug: the cases below that actually discriminate are the ones marked
// "first wrap". Keep at least one, and do not "simplify" the table down to a
// big-N case.
func TestTailRing_KeepsLastLinesInOrder(t *testing.T) {
	for _, tc := range []struct {
		name  string
		size  int
		lines int
		want  []string
	}{
		{name: "under capacity", size: 4, lines: 3, want: []string{"1", "2", "3"}},
		{name: "exactly capacity", size: 4, lines: 4, want: []string{"1", "2", "3", "4"}},
		// first wrap: old code returned 3,4,1,5 — line 2 lost, line 1 replayed.
		{name: "first wrap start", size: 4, lines: 5, want: []string{"2", "3", "4", "5"}},
		// first wrap: old code returned 1,5,6,7 — the stale line leading the tail.
		{name: "first wrap late", size: 4, lines: 7, want: []string{"4", "5", "6", "7"}},
		{name: "exact second wrap", size: 4, lines: 8, want: []string{"5", "6", "7", "8"}},
		{name: "many wraps", size: 3, lines: 100, want: []string{"98", "99", "100"}},
		{name: "single slot", size: 1, lines: 9, want: []string{"9"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newTailRing(tc.size)
			for i := 1; i <= tc.lines; i++ {
				r.add(strconv.Itoa(i))
			}
			if r.total != tc.lines {
				t.Errorf("total = %d, want %d", r.total, tc.lines)
			}
			got := r.lines()
			if len(got) != len(tc.want) {
				t.Fatalf("lines() = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("lines() = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// TestTailRing_ProductionSizeFirstWrap pins the real shape: StreamExec uses a
// 20-line ring, so the corrupted window was a command emitting 21–39 stdout
// lines — most short command output. At 38 lines the old indices produced a tail
// ending "…, 19, 20, 1, 21": line 1 replayed two slots from the end, presented
// to every reader as if it were adjacent to line 21.
func TestTailRing_ProductionSizeFirstWrap(t *testing.T) {
	const size = 20
	for _, lines := range []int{21, 30, 38, 39} {
		r := newTailRing(size)
		for i := 1; i <= lines; i++ {
			r.add(strconv.Itoa(i))
		}
		got := r.lines()
		if len(got) != size {
			t.Fatalf("lines=%d: tail length %d, want %d", lines, len(got), size)
		}
		// Strictly consecutive and ending on the newest line: no stale entry can
		// survive anywhere in the tail.
		for i, s := range got {
			if want := strconv.Itoa(lines - size + 1 + i); s != want {
				t.Fatalf("lines=%d: tail[%d] = %s, want %s (full tail %v)", lines, i, s, want, got)
			}
		}
	}
}

// TestTailRing_Empty: a command that emits nothing must produce an empty tail,
// not a ring full of blanks.
func TestTailRing_Empty(t *testing.T) {
	r := newTailRing(4)
	if got := r.lines(); len(got) != 0 {
		t.Errorf("lines() = %v, want empty", got)
	}
	if r.total != 0 {
		t.Errorf("total = %d, want 0", r.total)
	}
}

func TestStreamLineCallbackWriter_SingleLines(t *testing.T) {
	var lines []string
	w := &lineCallbackWriter{
		onLine: func(line []byte) {
			lines = append(lines, string(line))
		},
	}

	w.Write([]byte("hello\n"))
	w.Write([]byte("world\n"))

	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2", len(lines))
	}
	if lines[0] != "hello" {
		t.Errorf("line[0]: got %q, want %q", lines[0], "hello")
	}
	if lines[1] != "world" {
		t.Errorf("line[1]: got %q, want %q", lines[1], "world")
	}
}

func TestStreamLineCallbackWriter_PartialBuffering(t *testing.T) {
	var lines []string
	w := &lineCallbackWriter{
		onLine: func(line []byte) {
			lines = append(lines, string(line))
		},
	}

	// Write partial line -- should NOT produce a callback
	w.Write([]byte("hel"))
	if len(lines) != 0 {
		t.Fatalf("expected no lines after partial write, got %d", len(lines))
	}

	// Complete the line
	w.Write([]byte("lo\n"))
	if len(lines) != 1 {
		t.Fatalf("expected 1 line after completing write, got %d", len(lines))
	}
	if lines[0] != "hello" {
		t.Errorf("line[0]: got %q, want %q", lines[0], "hello")
	}
}

func TestStreamLineCallbackWriter_Flush(t *testing.T) {
	var lines []string
	w := &lineCallbackWriter{
		onLine: func(line []byte) {
			lines = append(lines, string(line))
		},
	}

	// Write partial line without newline
	w.Write([]byte("partial"))
	if len(lines) != 0 {
		t.Fatalf("expected no lines before flush, got %d", len(lines))
	}

	// Flush should deliver the buffered content
	w.Flush()
	if len(lines) != 1 {
		t.Fatalf("expected 1 line after flush, got %d", len(lines))
	}
	if lines[0] != "partial" {
		t.Errorf("line[0]: got %q, want %q", lines[0], "partial")
	}

	// Second flush should be a no-op
	w.Flush()
	if len(lines) != 1 {
		t.Fatalf("expected 1 line after second flush, got %d", len(lines))
	}
}

func TestStreamLineCallbackWriter_EmptyLines(t *testing.T) {
	var lines []string
	w := &lineCallbackWriter{
		onLine: func(line []byte) {
			lines = append(lines, string(line))
		},
	}

	w.Write([]byte("\n"))
	w.Write([]byte("data\n"))
	w.Write([]byte("\n"))

	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3", len(lines))
	}
	if lines[0] != "" {
		t.Errorf("line[0]: got %q, want empty", lines[0])
	}
	if lines[1] != "data" {
		t.Errorf("line[1]: got %q, want %q", lines[1], "data")
	}
	if lines[2] != "" {
		t.Errorf("line[2]: got %q, want empty", lines[2])
	}
}

func TestStreamLineCallbackWriter_VeryLongLine(t *testing.T) {
	var lines []string
	w := &lineCallbackWriter{
		onLine: func(line []byte) {
			lines = append(lines, string(line))
		},
	}

	// Create a line > 64KB
	longLine := strings.Repeat("x", 70000)
	w.Write([]byte(longLine + "\n"))

	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1", len(lines))
	}
	if len(lines[0]) != 70000 {
		t.Errorf("line length: got %d, want 70000", len(lines[0]))
	}
}

func TestStreamLineCallbackWriter_MultipleLinesInSingleWrite(t *testing.T) {
	var lines []string
	w := &lineCallbackWriter{
		onLine: func(line []byte) {
			lines = append(lines, string(line))
		},
	}

	w.Write([]byte("alpha\nbeta\ngamma\n"))

	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3", len(lines))
	}
	if lines[0] != "alpha" {
		t.Errorf("line[0]: got %q, want %q", lines[0], "alpha")
	}
	if lines[1] != "beta" {
		t.Errorf("line[1]: got %q, want %q", lines[1], "beta")
	}
	if lines[2] != "gamma" {
		t.Errorf("line[2]: got %q, want %q", lines[2], "gamma")
	}
}

func TestStreamLineCallbackWriter_MultipleLinesWithTrailingPartial(t *testing.T) {
	var lines []string
	w := &lineCallbackWriter{
		onLine: func(line []byte) {
			lines = append(lines, string(line))
		},
	}

	w.Write([]byte("line1\nline2\nparti"))
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2", len(lines))
	}

	w.Write([]byte("al\n"))
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3", len(lines))
	}
	if lines[2] != "partial" {
		t.Errorf("line[2]: got %q, want %q", lines[2], "partial")
	}
}

func TestStreamLineCallbackWriter_NilCallback(t *testing.T) {
	// Should not panic even with nil onLine.
	w := &lineCallbackWriter{onLine: nil}
	n, err := w.Write([]byte("hello\nworld\n"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 12 {
		t.Errorf("bytes written: got %d, want 12", n)
	}
	// Flush with nil should also not panic
	w.Flush()
}

func TestStreamLineCallbackWriter_WriteReturnsCorrectByteCount(t *testing.T) {
	w := &lineCallbackWriter{
		onLine: func(line []byte) {},
	}

	data := []byte("abc\ndef\ngh")
	n, err := w.Write(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != len(data) {
		t.Errorf("bytes written: got %d, want %d", n, len(data))
	}
}

func TestStreamLineCallbackWriter_CallbackGetsCopy(t *testing.T) {
	// Verify that modifying the callback argument does not affect subsequent calls.
	var captured []byte
	w := &lineCallbackWriter{
		onLine: func(line []byte) {
			captured = line
		},
	}

	w.Write([]byte("first\n"))
	// Mutate the captured slice
	for i := range captured {
		captured[i] = 'Z'
	}

	w.Write([]byte("second\n"))
	if string(captured) != "second" {
		t.Errorf("captured: got %q, want %q", string(captured), "second")
	}
}
