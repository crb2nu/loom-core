package backend

import "strings"

// ExecResult holds the structured output of a command execution.
type ExecResult struct {
	ExitCode    int    `json:"exit_code"`
	StdoutLines int    `json:"stdout_lines"`
	StderrLines int    `json:"stderr_lines"`
	StdoutTail  string `json:"stdout_tail"`
	StderrHead  string `json:"stderr_head,omitempty"`
	StderrTail  string `json:"stderr_tail,omitempty"`
	DurationMs  int64  `json:"duration_ms"`
	Truncated   bool   `json:"truncated"`
	OOMKilled   bool   `json:"oom_killed,omitempty"`
}

// TruncateOutput keeps only the last maxLines lines from output.
// Returns the truncated string, total line count, and whether truncation occurred.
func TruncateOutput(output string, maxLines int) (string, int, bool) {
	if output == "" {
		return "", 0, false
	}

	lines := strings.Split(output, "\n")
	total := len(lines)

	// Remove trailing empty line from Split
	if total > 0 && lines[total-1] == "" {
		lines = lines[:total-1]
		total = len(lines)
	}

	if maxLines <= 0 || total <= maxLines {
		return strings.Join(lines, "\n"), total, false
	}

	tail := lines[total-maxLines:]
	return strings.Join(tail, "\n"), total, true
}

// stderrHead returns at most the first 20 lines / 8 KiB, excluding the
// terminating newline just like TruncateOutput. The byte limit may split a line.
func stderrHead(output string) string {
	if len(output) > 8*1024 {
		output = output[:8*1024]
	}
	start := 0
	for i := 0; i < 20; i++ {
		n := strings.IndexByte(output[start:], '\n')
		if n < 0 {
			break
		}
		start += n + 1
		if i == 19 {
			output = output[:start]
			break
		}
	}
	// Do not let a short head keep a large non-streaming stderr string alive.
	return strings.Clone(strings.TrimSuffix(output, "\n"))
}
