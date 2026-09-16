package backend

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestTruncateOutput_EmptyString(t *testing.T) {
	out, lines, truncated := TruncateOutput("", 10)
	if out != "" {
		t.Fatalf("expected empty string, got %q", out)
	}
	if lines != 0 {
		t.Fatalf("expected 0 lines, got %d", lines)
	}
	if truncated {
		t.Fatal("expected truncated=false for empty input")
	}
}

func TestTruncateOutput_SingleLineWithinLimit(t *testing.T) {
	out, lines, truncated := TruncateOutput("hello world", 5)
	if out != "hello world" {
		t.Fatalf("expected %q, got %q", "hello world", out)
	}
	if lines != 1 {
		t.Fatalf("expected 1 line, got %d", lines)
	}
	if truncated {
		t.Fatal("expected truncated=false for single line within limit")
	}
}

func TestTruncateOutput_MultipleLinesWithinLimit(t *testing.T) {
	input := "line1\nline2\nline3"
	out, lines, truncated := TruncateOutput(input, 5)
	if out != "line1\nline2\nline3" {
		t.Fatalf("expected %q, got %q", "line1\nline2\nline3", out)
	}
	if lines != 3 {
		t.Fatalf("expected 3 lines, got %d", lines)
	}
	if truncated {
		t.Fatal("expected truncated=false when lines < maxLines")
	}
}

func TestTruncateOutput_ExceedsMaxLines(t *testing.T) {
	input := "line1\nline2\nline3\nline4\nline5"
	out, lines, truncated := TruncateOutput(input, 3)
	if out != "line3\nline4\nline5" {
		t.Fatalf("expected %q, got %q", "line3\nline4\nline5", out)
	}
	if lines != 5 {
		t.Fatalf("expected 5 total lines, got %d", lines)
	}
	if !truncated {
		t.Fatal("expected truncated=true when lines > maxLines")
	}
}

func TestTruncateOutput_ExactBoundary(t *testing.T) {
	input := "a\nb\nc"
	out, lines, truncated := TruncateOutput(input, 3)
	if out != "a\nb\nc" {
		t.Fatalf("expected %q, got %q", "a\nb\nc", out)
	}
	if lines != 3 {
		t.Fatalf("expected 3 lines, got %d", lines)
	}
	if truncated {
		t.Fatal("expected truncated=false when lines == maxLines")
	}
}

func TestTruncateOutput_TrailingNewline(t *testing.T) {
	// strings.Split("a\nb\n", "\n") produces ["a","b",""], so the trailing
	// empty element is stripped, leaving 2 lines.
	input := "a\nb\n"
	out, lines, truncated := TruncateOutput(input, 5)
	if out != "a\nb" {
		t.Fatalf("expected %q, got %q", "a\nb", out)
	}
	if lines != 2 {
		t.Fatalf("expected 2 lines, got %d", lines)
	}
	if truncated {
		t.Fatal("expected truncated=false")
	}
}

func TestTruncateOutput_MaxLinesZero(t *testing.T) {
	// maxLines <= 0 means no truncation
	input := "a\nb\nc"
	out, lines, truncated := TruncateOutput(input, 0)
	if out != "a\nb\nc" {
		t.Fatalf("expected %q, got %q", "a\nb\nc", out)
	}
	if lines != 3 {
		t.Fatalf("expected 3 lines, got %d", lines)
	}
	if truncated {
		t.Fatal("expected truncated=false when maxLines <= 0")
	}
}

func TestTruncateOutput_TruncateToOne(t *testing.T) {
	input := "first\nsecond\nthird"
	out, lines, truncated := TruncateOutput(input, 1)
	if out != "third" {
		t.Fatalf("expected %q, got %q", "third", out)
	}
	if lines != 3 {
		t.Fatalf("expected 3 total lines, got %d", lines)
	}
	if !truncated {
		t.Fatal("expected truncated=true")
	}
}

func TestResultStderrHeadJSON(t *testing.T) {
	var result ExecResult
	if err := json.Unmarshal([]byte(`{"exit_code":1,"stderr_tail":"old","truncated":false}`), &result); err != nil {
		t.Fatal(err)
	}
	if result.StderrHead != "" || result.StderrTail != "old" {
		t.Fatal("legacy JSON changed")
	}
	data, err := json.Marshal(result)
	if err != nil || strings.Contains(string(data), "stderr_head") {
		t.Fatalf("empty head: %s, %v", data, err)
	}
	result.StderrHead = "first"
	data, err = json.Marshal(result)
	if err != nil || !strings.Contains(string(data), `"stderr_head":"first"`) {
		t.Fatalf("head JSON: %s, %v", data, err)
	}
}

func TestResultStderrHead(t *testing.T) {
	for _, tt := range []struct{ input, want string }{
		{"", ""}, {"\n", ""}, {"a\n\n", "a\n"}, {"a\nb", "a\nb"},
		{strings.Repeat("x\n", 100), strings.TrimSuffix(strings.Repeat("x\n", 20), "\n")},
		{strings.Repeat("a", 9000), strings.Repeat("a", 8192)},
	} {
		if got := stderrHead(tt.input); got != tt.want {
			t.Fatalf("head length %d, want %d", len(got), len(tt.want))
		}
	}
}
