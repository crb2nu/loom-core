package hud

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crb2nu/loom/internal/hud/bridge"
	"github.com/crb2nu/loom/internal/spawn"
)

func TestWrapAgentCommandWithCompletionHoldZeroIsUnchanged(t *testing.T) {
	const command = `printf '%s\n' agent`
	if got := wrapAgentCommandWithCompletionHold(command, 0); got != command {
		t.Fatalf("zero hold changed command:\n got: %q\nwant: %q", got, command)
	}
}

func TestWrapAgentCommandWithCompletionHoldRunsForegroundSleepAfterSuccess(t *testing.T) {
	binDir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "order.log")
	sleepStub := "#!/bin/sh\nprintf 'sleep:%s\\n' \"$1\" >> " + shellQuote(logPath) + "\n"
	if err := os.WriteFile(filepath.Join(binDir, "sleep"), []byte(sleepStub), 0o755); err != nil {
		t.Fatalf("write sleep stub: %v", err)
	}

	command := `printf 'agent\n' >> ` + shellQuote(logPath)
	wrapped := wrapAgentCommandWithCompletionHold(command, 17)
	if !strings.Contains(wrapped, "; sleep 17;") {
		t.Fatalf("wrapper missing exact foreground sleep: %q", wrapped)
	}

	cmd := exec.CommandContext(context.Background(), "sh", "-c", wrapped)
	cmd.Env = append(os.Environ(), "PATH="+binDir+":"+os.Getenv("PATH"))
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("run wrapped command: %v\n%s", err, output)
	}
	got, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read order log: %v", err)
	}
	if want := "agent\nsleep:17\n"; string(got) != want {
		t.Fatalf("execution order = %q, want %q", got, want)
	}
}

func TestWrapAgentCommandWithCompletionHoldDoesNotMaskAgentFailure(t *testing.T) {
	binDir := t.TempDir()
	marker := filepath.Join(t.TempDir(), "sleep-ran")
	sleepStub := "#!/bin/sh\n: > " + shellQuote(marker) + "\n"
	if err := os.WriteFile(filepath.Join(binDir, "sleep"), []byte(sleepStub), 0o755); err != nil {
		t.Fatalf("write sleep stub: %v", err)
	}

	cmd := exec.CommandContext(context.Background(), "sh", "-c", wrapAgentCommandWithCompletionHold("sh -c 'exit 23'", 17))
	cmd.Env = append(os.Environ(), "PATH="+binDir+":"+os.Getenv("PATH"))
	err := cmd.Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 23 {
		t.Fatalf("wrapped failure = %v, want exit 23", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("sleep ran after agent failure; stat error = %v", err)
	}
}

func TestCompletionHoldWrapsSDKDriverCommand(t *testing.T) {
	driver := buildSDKDriverCommand(
		"codex",
		"finish the task",
		"agent-1",
		"spawn-1",
		"/workspace/loom-core",
		"",
		0,
		0,
	)
	if strings.HasPrefix(strings.TrimSpace(driver), "exec ") {
		t.Fatalf("SDK driver replaces the shell and would bypass the hold: %q", driver)
	}
	wrapped := wrapAgentCommandWithCompletionHold(driver, 90)
	if !strings.HasPrefix(wrapped, driver+";") {
		t.Fatalf("SDK driver is not the first command in wrapper: %q", wrapped)
	}
	if !strings.Contains(wrapped, "; sleep 90;") {
		t.Fatalf("SDK wrapper missing exact foreground hold: %q", wrapped)
	}
}

func TestCompletionHoldBounds(t *testing.T) {
	for _, seconds := range []int{0, spawn.MaxCompletionHoldSeconds} {
		if err := validateCompletionHoldSeconds(seconds); err != nil {
			t.Errorf("validateCompletionHoldSeconds(%d): %v", seconds, err)
		}
	}
	for _, seconds := range []int{-1, spawn.MaxCompletionHoldSeconds + 1} {
		if err := validateCompletionHoldSeconds(seconds); err == nil {
			t.Errorf("validateCompletionHoldSeconds(%d) unexpectedly succeeded", seconds)
		}
	}
}

func TestCompletionHoldJSONRoundTripAndOmitZero(t *testing.T) {
	encoded, err := json.Marshal(SpawnRequest{CompletionHoldSeconds: 90})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	if !strings.Contains(string(encoded), `"completion_hold_seconds":90`) {
		t.Fatalf("nonzero completion hold not persisted: %s", encoded)
	}
	var decoded SpawnRequest
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}
	if decoded.CompletionHoldSeconds != 90 {
		t.Fatalf("round-trip completion hold = %d, want 90", decoded.CompletionHoldSeconds)
	}

	zero, err := json.Marshal(SpawnRequest{})
	if err != nil {
		t.Fatalf("marshal zero request: %v", err)
	}
	if strings.Contains(string(zero), "completion_hold_seconds") {
		t.Fatalf("zero completion hold should be omitted: %s", zero)
	}
}

func TestSpawnRejectsInvalidCompletionHoldBeforeDependencies(t *testing.T) {
	o := &SpawnOrchestrator{}
	for _, seconds := range []int{-1, spawn.MaxCompletionHoldSeconds + 1} {
		_, err := o.Spawn(context.Background(), SpawnRequest{CompletionHoldSeconds: seconds})
		if err == nil || !strings.Contains(err.Error(), "completion_hold_seconds") {
			t.Errorf("Spawn(completion hold %d) error = %v, want bounds error", seconds, err)
		}
	}
}

func TestCompletionGuardFailsClosedOnUnmatchedCodexToolCall(t *testing.T) {
	acc := bridge.NewSpawnTelemetryAccumulator()
	guard := newCompletionGuardSink(acc)
	parser := NewCodexJSONLParser(guard, "agent-1", "spawn-1", nil, nil)

	parser.HandleLine([]byte(`{"type":"item.started","item":{"id":"cmd-1","type":"command_execution","command":"go test ./..."}}`))
	if got := guard.openToolCallCount(); got != 1 {
		t.Fatalf("open tool calls after item.started = %d, want 1", got)
	}
	if err := guard.completionError(); err == nil || !strings.Contains(err.Error(), "1 tool call(s) still open") {
		t.Fatalf("completion error with unmatched item = %v, want fail-closed error", err)
	}

	parser.HandleLine([]byte(`{"type":"item.completed","item":{"id":"cmd-1","type":"command_execution","command":"go test ./...","exit_code":0}}`))
	if got := guard.openToolCallCount(); got != 0 {
		t.Fatalf("open tool calls after item.completed = %d, want 0", got)
	}
	if err := guard.completionError(); err != nil {
		t.Fatalf("completion error after matched item: %v", err)
	}
}

func TestSpawnCompletionErrorFailsClosedOnCodexTurnFailed(t *testing.T) {
	acc := bridge.NewSpawnTelemetryAccumulator()
	guard := newCompletionGuardSink(acc)
	parser := NewCodexJSONLParser(guard, "agent-1", "spawn-1", nil, nil)

	// Fixture captured from codex 0.143.0 while gpt-5.6-sol was rejected by
	// the API. codex exec itself exits 0 for this sequence, so completion must
	// be rejected from the structured terminal event instead.
	for _, line := range []string{
		`{"type":"thread.started","thread_id":"019-test"}`,
		`{"type":"turn.started"}`,
		`{"type":"turn.failed","error":{"message":"HTTP 400: invalid_request_error: The model gpt-5.6-sol is not supported for this account.","type":"invalid_request_error"}}`,
	} {
		parser.HandleLine([]byte(line))
	}

	err := spawnCompletionError(guard, parser)
	if err == nil {
		t.Fatal("clean Codex process exit after turn.failed would mark the spawn completed")
	}
	if !strings.Contains(err.Error(), "HTTP 400: invalid_request_error") {
		t.Fatalf("spawn failure = %q, want embedded API error text", err)
	}
}

func TestSpawnCompletionErrorAllowsCompletedCodexTurn(t *testing.T) {
	acc := bridge.NewSpawnTelemetryAccumulator()
	guard := newCompletionGuardSink(acc)
	parser := NewCodexJSONLParser(guard, "agent-1", "spawn-1", nil, nil)

	parser.HandleLine([]byte(`{"type":"turn.completed","usage":{}}`))
	parser.HandleLine([]byte(`{"type":"turn.failed","message":"later cleanup failed"}`))

	if err := spawnCompletionError(guard, parser); err != nil {
		t.Fatalf("completed Codex turn rejected: %v", err)
	}
}

// TestSpawnCompletionErrorPrefersTurnFailureOverOpenToolCalls guards the
// 2026-07-26 regression: codex fired a batch of MCP calls, the turn failed
// mid-batch, and the verdict reported only "18 tool call(s) still open after
// agent process exit" — the count, never the cause. The open calls are a
// symptom of the aborted turn, so the parser's terminal reason must win.
func TestSpawnCompletionErrorPrefersTurnFailureOverOpenToolCalls(t *testing.T) {
	acc := bridge.NewSpawnTelemetryAccumulator()
	guard := newCompletionGuardSink(acc)
	parser := NewCodexJSONLParser(guard, "agent-1", "spawn-1", nil, nil)

	parser.HandleLine([]byte(`{"type":"turn.started"}`))
	for _, id := range []string{"item_1", "item_2", "item_3"} {
		parser.HandleLine([]byte(`{"type":"item.started","item":{"id":"` + id + `","type":"mcp_tool_call","tool":"agent_plan_slice_update","server":"agent-context"}}`))
	}
	parser.HandleLine([]byte(`{"type":"turn.failed","error":{"message":"stream disconnected before completion"}}`))

	if got := guard.openToolCallCount(); got != 3 {
		t.Fatalf("open tool calls = %d, want 3 (turn.failed must not sweep)", got)
	}
	err := spawnCompletionError(guard, parser)
	if err == nil {
		t.Fatal("turn.failed with open calls must still fail the spawn")
	}
	if !strings.Contains(err.Error(), "stream disconnected before completion") {
		t.Fatalf("verdict = %q, want the turn.failed reason", err)
	}
	if strings.Contains(err.Error(), "still open after agent process exit") {
		t.Fatalf("verdict = %q, want the cause, not the open-call symptom", err)
	}
}

// TestSpawnCompletionErrorSurvivesEventsAfterTurnFailed: codex keeps emitting
// after it gives up (a fatal `error` event, late item completions from the
// failed batch), and the buffered path replays only a stdout tail. Anchoring
// the terminal reason on the LAST event dropped it in exactly those cases.
func TestSpawnCompletionErrorSurvivesEventsAfterTurnFailed(t *testing.T) {
	acc := bridge.NewSpawnTelemetryAccumulator()
	guard := newCompletionGuardSink(acc)
	parser := NewCodexJSONLParser(guard, "agent-1", "spawn-1", nil, nil)

	parser.HandleLine([]byte(`{"type":"turn.started"}`))
	parser.HandleLine([]byte(`{"type":"item.started","item":{"id":"cmd-1","type":"command_execution","command":"go test ./..."}}`))
	parser.HandleLine([]byte(`{"type":"turn.failed","error":{"message":"context window exceeded"}}`))
	parser.HandleLine([]byte(`{"type":"item.completed","item":{"id":"cmd-1","type":"command_execution","command":"go test ./...","exit_code":0}}`))
	parser.HandleLine([]byte(`{"type":"error","message":"session aborted"}`))

	err := spawnCompletionError(guard, parser)
	if err == nil || !strings.Contains(err.Error(), "context window exceeded") {
		t.Fatalf("verdict = %v, want the turn.failed reason despite trailing events", err)
	}
}

// TestSpawnCompletionErrorNamesStreamAbortReason: no terminal turn event at
// all, but codex emitted a fatal error before the stream ended. Lead with that
// cause and keep the open-call count as corroboration.
func TestSpawnCompletionErrorNamesStreamAbortReason(t *testing.T) {
	acc := bridge.NewSpawnTelemetryAccumulator()
	guard := newCompletionGuardSink(acc)
	parser := NewCodexJSONLParser(guard, "agent-1", "spawn-1", nil, nil)

	parser.HandleLine([]byte(`{"type":"turn.started"}`))
	parser.HandleLine([]byte(`{"type":"item.started","item":{"id":"cmd-1","type":"command_execution","command":"go build ./..."}}`))
	parser.HandleLine([]byte(`{"type":"error","message":"mcp transport closed: 1006"}`))

	err := spawnCompletionError(guard, parser)
	if err == nil {
		t.Fatal("aborted stream with an open call must fail the spawn")
	}
	if !strings.Contains(err.Error(), "mcp transport closed: 1006") {
		t.Fatalf("verdict = %q, want the fatal error message", err)
	}
	if !strings.Contains(err.Error(), "1 tool call(s) still open") {
		t.Fatalf("verdict = %q, want the open-call count retained as detail", err)
	}
}

// TestSpawnCompletionErrorFailsAbortedStreamWithNothingOpen is the round-2
// policy call: a codex stream that emitted a fatal `error`, completed no turn,
// and left NOTHING in flight used to reach completeSpawn as a success, because
// the abort reason was consulted only after the open-tool-call gate. Zero
// completed turns means no finished work regardless of what happened to be in
// flight when the stream died.
func TestSpawnCompletionErrorFailsAbortedStreamWithNothingOpen(t *testing.T) {
	acc := bridge.NewSpawnTelemetryAccumulator()
	guard := newCompletionGuardSink(acc)
	parser := NewCodexJSONLParser(guard, "agent-1", "spawn-1", nil, nil)

	parser.HandleLine([]byte(`{"type":"turn.started"}`))
	parser.HandleLine([]byte(`{"type":"item.started","item":{"id":"cmd-1","type":"command_execution","command":"go build ./..."}}`))
	parser.HandleLine([]byte(`{"type":"item.completed","item":{"id":"cmd-1","type":"command_execution","command":"go build ./...","exit_code":0}}`))
	parser.HandleLine([]byte(`{"type":"error","message":"stream error: exceeded retry limit"}`))

	if got := guard.openToolCallCount(); got != 0 {
		t.Fatalf("open tool calls = %d, want 0 (this is the nothing-in-flight case)", got)
	}
	err := spawnCompletionError(guard, parser)
	if err == nil {
		t.Fatal("a fatal codex error with zero completed turns must not complete as a success")
	}
	if !strings.Contains(err.Error(), "stream error: exceeded retry limit") {
		t.Fatalf("verdict = %q, want the fatal error message", err)
	}
	if strings.Contains(err.Error(), "tool call(s) still open") {
		t.Fatalf("verdict = %q, must not claim open calls when there are none", err)
	}
}

// TestSpawnCompletionErrorAllowsRecoveredErrorWithNothingOpen is the other half
// of that policy: the veto requires that NOTHING recovered from the error. A
// completed turn after the error means the agentic loop carried on, so the run
// still completes.
func TestSpawnCompletionErrorAllowsRecoveredErrorWithNothingOpen(t *testing.T) {
	acc := bridge.NewSpawnTelemetryAccumulator()
	guard := newCompletionGuardSink(acc)
	parser := NewCodexJSONLParser(guard, "agent-1", "spawn-1", nil, nil)

	parser.HandleLine([]byte(`{"type":"turn.started"}`))
	parser.HandleLine([]byte(`{"type":"error","message":"transient tool error"}`))
	parser.HandleLine([]byte(`{"type":"turn.completed","usage":{"input_tokens":10,"output_tokens":5}}`))

	if err := spawnCompletionError(guard, parser); err != nil {
		t.Fatalf("recovered error rejected: %v", err)
	}
}

// TestSpawnCompletionErrorNamesGeminiStreamAbort: gemini streams used to report
// only an open-call count when they died mid-run. The recorded error-severity
// event is the cause and must lead the verdict.
func TestSpawnCompletionErrorNamesGeminiStreamAbort(t *testing.T) {
	acc := bridge.NewSpawnTelemetryAccumulator()
	guard := newCompletionGuardSink(acc)
	parser := NewGeminiJSONLParser(guard, "agent-1", "spawn-1", nil, nil)

	parser.HandleLine([]byte(`{"type":"tool_use","tool_name":"run_shell","tool_id":"t-1"}`))
	parser.HandleLine([]byte(`{"type":"error","severity":"fatal","message":"model backend unavailable"}`))

	err := spawnCompletionError(guard, parser)
	if err == nil {
		t.Fatal("a fatal gemini error with no terminal result must fail the spawn")
	}
	if !strings.Contains(err.Error(), "model backend unavailable") {
		t.Fatalf("verdict = %q, want the gemini error message", err)
	}
	if !strings.Contains(err.Error(), "1 tool call(s) still open") {
		t.Fatalf("verdict = %q, want the open-call count retained as detail", err)
	}
}

// TestSpawnCompletionErrorGeminiWarningsAndResultsDoNotVeto: the veto needs an
// error-grade severity AND no terminal result. A warning the run continued past
// must not fail it, and neither must an error before a terminal result.
func TestSpawnCompletionErrorGeminiWarningsAndResultsDoNotVeto(t *testing.T) {
	for _, tc := range []struct{ name, lines string }{
		{name: "warning only", lines: `{"type":"error","severity":"warning","message":"retrying tool"}`},
		{name: "error then result", lines: `{"type":"error","severity":"error","message":"model hiccup"}` + "\n" + `{"type":"result","status":"success"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			acc := bridge.NewSpawnTelemetryAccumulator()
			guard := newCompletionGuardSink(acc)
			parser := NewGeminiJSONLParser(guard, "agent-1", "spawn-1", nil, nil)
			for _, line := range strings.Split(tc.lines, "\n") {
				parser.HandleLine([]byte(line))
			}
			if err := spawnCompletionError(guard, parser); err != nil {
				t.Fatalf("verdict = %v, want the spawn to complete", err)
			}
		})
	}
}

// TestSpawnCompletionErrorNamesClaudeRetryStormAbort: claude's stream-json has
// no fatal event, so a run the API gives up on stops after a retry storm. That
// storm is the diagnostic; a mid-tool truncation used to report only the count
// of calls left open.
func TestSpawnCompletionErrorNamesClaudeRetryStormAbort(t *testing.T) {
	acc := bridge.NewSpawnTelemetryAccumulator()
	guard := newCompletionGuardSink(acc)
	parser := NewClaudeJSONLParser(guard, "agent-1", "spawn-1", nil, nil)

	parser.HandleLine([]byte(`{"type":"assistant","message":{"id":"msg_1","content":[{"type":"tool_use","id":"tu-1","name":"Bash","input":{}}]}}`))
	parser.HandleLine([]byte(`{"type":"system","subtype":"api_retry","attempt":5,"error_status":529}`))

	err := spawnCompletionError(guard, parser)
	if err == nil {
		t.Fatal("a claude stream that stopped after a retry storm must fail the spawn")
	}
	if !strings.Contains(err.Error(), "status 529") {
		t.Fatalf("verdict = %q, want the retry diagnostic", err)
	}
}

// TestSpawnCompletionErrorClaudeTerminalResultsKeepTheirOutcome pins the
// deliberate limit of the new veto: a terminal `result` — including an
// error-subtype one — keeps whatever outcome it had before, so error_max_turns
// still completes rather than becoming a new spawn failure. And a truncation
// with no retry logged falls back to the old open-call rule.
func TestSpawnCompletionErrorClaudeTerminalResultsKeepTheirOutcome(t *testing.T) {
	acc := bridge.NewSpawnTelemetryAccumulator()
	guard := newCompletionGuardSink(acc)
	parser := NewClaudeJSONLParser(guard, "agent-1", "spawn-1", nil, nil)

	parser.HandleLine([]byte(`{"type":"system","subtype":"api_retry","attempt":2,"error_status":529}`))
	parser.HandleLine([]byte(`{"type":"result","subtype":"error_max_turns","is_error":true,"result":"hit max turns"}`))

	if err := spawnCompletionError(guard, parser); err != nil {
		t.Fatalf("terminal result outcome changed: %v", err)
	}

	acc2 := bridge.NewSpawnTelemetryAccumulator()
	guard2 := newCompletionGuardSink(acc2)
	parser2 := NewClaudeJSONLParser(guard2, "agent-1", "spawn-2", nil, nil)
	parser2.HandleLine([]byte(`{"type":"assistant","message":{"id":"msg_1","content":[{"type":"tool_use","id":"tu-1","name":"Bash","input":{}}]}}`))

	err := spawnCompletionError(guard2, parser2)
	if err == nil || !strings.Contains(err.Error(), "1 tool call(s) still open after agent process exit") {
		t.Fatalf("verdict = %v, want the unchanged fail-closed open-call error", err)
	}
}

// TestSpawnCompletionErrorUnexplainedTruncationStillFailsClosed: with no
// terminal event and no diagnostic, the guard's fail-closed verdict is still
// the right answer — completing here would publish a false success.
func TestSpawnCompletionErrorUnexplainedTruncationStillFailsClosed(t *testing.T) {
	acc := bridge.NewSpawnTelemetryAccumulator()
	guard := newCompletionGuardSink(acc)
	parser := NewCodexJSONLParser(guard, "agent-1", "spawn-1", nil, nil)

	parser.HandleLine([]byte(`{"type":"turn.started"}`))
	parser.HandleLine([]byte(`{"type":"item.started","item":{"id":"cmd-1","type":"command_execution","command":"go build ./..."}}`))

	err := spawnCompletionError(guard, parser)
	if err == nil || !strings.Contains(err.Error(), "1 tool call(s) still open after agent process exit") {
		t.Fatalf("verdict = %v, want the fail-closed open-call error", err)
	}
}

// TestSpawnCompletionErrorCleanTurnEndStillSweeps is the !1180 regression
// guard: a clean turn.completed sweeps the stragglers, so a successful run
// whose work landed is never failed on protocol noise — and a recovered
// `error` event before that clean end must not resurrect a verdict.
func TestSpawnCompletionErrorCleanTurnEndStillSweeps(t *testing.T) {
	acc := bridge.NewSpawnTelemetryAccumulator()
	guard := newCompletionGuardSink(acc)
	parser := NewCodexJSONLParser(guard, "agent-1", "spawn-1", nil, nil)

	parser.HandleLine([]byte(`{"type":"turn.started"}`))
	parser.HandleLine([]byte(`{"type":"item.started","item":{"id":"cmd-1","type":"command_execution","command":"make changelog-check"}}`))
	parser.HandleLine([]byte(`{"type":"error","message":"transient tool error"}`))
	parser.HandleLine([]byte(`{"type":"item.started","item":{"id":"cmd-2","type":"command_execution","command":"git push"}}`))
	parser.HandleLine([]byte(`{"type":"item.completed","item":{"id":"cmd-2","type":"command_execution","command":"git push","exit_code":0}}`))
	parser.HandleLine([]byte(`{"type":"turn.completed","usage":{"input_tokens":10,"cached_input_tokens":0,"output_tokens":5}}`))

	if got := guard.openToolCallCount(); got != 0 {
		t.Fatalf("open tool calls after clean turn end = %d, want 0 (sweep)", got)
	}
	if err := spawnCompletionError(guard, parser); err != nil {
		t.Fatalf("clean turn end rejected: %v", err)
	}
}
