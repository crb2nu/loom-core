package hud

import (
	"strings"
	"testing"

	"github.com/crb2nu/loom/internal/devbox/backend"
)

// TestBufferedExecFailure guards the harvester-vm (buffered, non-streaming)
// spawn finalizer: Backend.Exec returns a nil Go error for command-level
// nonzero exits, so the spawn path must inspect ExitCode explicitly. Failing to
// do so is what let failing harvester-vm spawns report "completed" with
// turn_count=0 and no diagnostics during the Mills A2 kill-test.
func TestBufferedExecFailure(t *testing.T) {
	tests := []struct {
		name       string
		result     *backend.ExecResult
		wantFailed bool
		wantSubstr []string
	}{
		{
			name:       "nil result is not a failure",
			result:     nil,
			wantFailed: false,
		},
		{
			name:       "clean exit is not a failure",
			result:     &backend.ExecResult{ExitCode: 0, StdoutTail: "done"},
			wantFailed: false,
		},
		{
			name:       "nonzero exit with stderr surfaces the stderr",
			result:     &backend.ExecResult{ExitCode: 1, StderrTail: "codex: not authenticated"},
			wantFailed: true,
			wantSubstr: []string{"exited 1", "codex: not authenticated"},
		},
		{
			name:       "nonzero exit without stderr falls back to stdout",
			result:     &backend.ExecResult{ExitCode: 127, StdoutTail: "codex: command not found"},
			wantFailed: true,
			wantSubstr: []string{"exited 127", "no stderr", "command not found"},
		},
		{
			name:       "nonzero exit with no output still reports the code",
			result:     &backend.ExecResult{ExitCode: 2},
			wantFailed: true,
			wantSubstr: []string{"exited 2"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg, failed := bufferedExecFailure(tt.result)
			if failed != tt.wantFailed {
				t.Fatalf("bufferedExecFailure() failed = %v, want %v (msg=%q)", failed, tt.wantFailed, msg)
			}
			for _, sub := range tt.wantSubstr {
				if !strings.Contains(msg, sub) {
					t.Fatalf("message %q missing expected substring %q", msg, sub)
				}
			}
			if !failed && msg != "" {
				t.Fatalf("expected empty message when not failed, got %q", msg)
			}
		})
	}
}
