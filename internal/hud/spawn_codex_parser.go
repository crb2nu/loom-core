package hud

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/crb2nu/loom/internal/hud/bridge"
)

// CodexJSONLParser parses Codex's --json JSONL output.
// Each line is a JSON object with a "type" field: thread.started, turn.started,
// turn.completed, item.started, item.completed, turn.failed, error.
type CodexJSONLParser struct {
	sink           SpawnEventSink
	broadcast      SpawnEventBroadcaster
	agentID        string
	spawnID        string
	model          string
	logger         *slog.Logger
	completedTurns int
	turnFailure    string
	// fatalError is the last `error` event message Codex emitted. Kept
	// separately from turnFailure: an error event is not by itself a turn
	// verdict, but when the stream ends with tool calls still open it is the
	// only account of why.
	fatalError string
	// openTools tracks started-but-not-completed tool item ids (id → tool
	// name) so turn.completed can sweep stragglers: Codex may start commands
	// in a parallel batch and end the turn without ever emitting
	// item.completed for some of them (captured stream spawn-d9889e05e2e9,
	// 2026-07-21). completedTools makes completion idempotent — the
	// accumulator matches a completion to the most recent open entry, so a
	// duplicate CompleteToolCall would clobber an unrelated in-flight call.
	openTools      map[string]string
	completedTools map[string]struct{}
}

// NewCodexJSONLParser creates a parser that writes structured events to sink.
// broadcast may be nil if real-time SSE is not needed. spawnID is used to
// stamp agent.spawn.telemetry.delta events with the owning spawn; it may be
// empty in unit tests that do not exercise delta broadcasts.
func NewCodexJSONLParser(sink SpawnEventSink, agentID, spawnID string, broadcast SpawnEventBroadcaster, logger *slog.Logger) *CodexJSONLParser {
	if logger == nil {
		logger = slog.Default()
	}
	return &CodexJSONLParser{
		sink:           sink,
		broadcast:      broadcast,
		agentID:        agentID,
		spawnID:        spawnID,
		model:          bridge.DefaultCodexModel,
		logger:         logger.With("component", "codex-parser", "agent_id", agentID),
		openTools:      make(map[string]string),
		completedTools: make(map[string]struct{}),
	}
}

// emitTelemetryDelta snapshots the current accumulator state and broadcasts
// an agent.spawn.telemetry.delta SSE event so web HUD and iOS clients can
// render live cost / token / tool counts without polling the full
// /api/agent/spawn/{id}/telemetry endpoint. No-op when the broadcaster is
// nil (unit tests or buffered-exec fallback).
func (p *CodexJSONLParser) emitTelemetryDelta() {
	if p.broadcast == nil {
		return
	}
	delta := p.sink.TelemetryDeltaSnapshot(p.spawnID, p.agentID)
	p.broadcast(SpawnTelemetryDeltaEvent, p.agentID, delta)
}

// HandleLine processes a single JSONL line from Codex stdout.
func (p *CodexJSONLParser) HandleLine(line []byte) {
	if len(line) == 0 {
		return
	}

	var envelope struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(line, &envelope); err != nil {
		p.logger.Debug("skipping non-JSON line", "error", err)
		return
	}
	switch envelope.Type {
	case "thread.started":
		p.handleThreadStarted(line)
	case "turn.started":
		p.handleTurnStarted()
	case "turn.completed":
		p.handleTurnCompleted(line)
	case "item.started":
		p.handleItemStarted(line)
	case "item.updated":
		p.handleItemUpdated(line)
	case "item.completed":
		p.handleItemCompleted(line)
	case "turn.failed":
		p.handleTurnFailed(line)
	case "error":
		p.handleError(line)
	default:
		p.logger.Debug("skipping unknown event type", "type", envelope.Type)
	}
}

// ---------- thread.started ----------

func (p *CodexJSONLParser) handleThreadStarted(line []byte) {
	var ev struct {
		ThreadID string `json:"thread_id"`
		Model    string `json:"model"`
		Thread   struct {
			Model string `json:"model"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(line, &ev); err != nil {
		p.logger.Debug("failed to parse thread.started", "error", err)
		return
	}
	if ev.ThreadID != "" {
		p.sink.SetExternalSessionID(ev.ThreadID)
	}
	if model := firstNonEmpty(ev.Model, ev.Thread.Model); model != "" {
		p.model = model
	}
}

// ---------- turn.started ----------

func (p *CodexJSONLParser) handleTurnStarted() {
	p.sink.IncrementTurns()
	p.emitTelemetryDelta()
}

// ---------- turn.completed ----------

func (p *CodexJSONLParser) handleTurnCompleted(line []byte) {
	var ev struct {
		Usage struct {
			InputTokens       int `json:"input_tokens"`
			CachedInputTokens int `json:"cached_input_tokens"`
			OutputTokens      int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(line, &ev); err != nil {
		p.logger.Debug("failed to parse turn.completed", "error", err)
		return
	}
	p.completedTurns++
	p.sweepOpenTools()

	// Codex's `usage.input_tokens` follows the OpenAI Responses API convention:
	// it is the TOTAL input tokens including the cached portion, and
	// `cached_input_tokens` is a subset already included in that total (see
	// https://platform.openai.com/docs/guides/prompt-caching).
	//
	// Loom's canonical SpawnTokenUsage treats InputTokens and CacheReadTokens
	// as additive (total = input + output + cacheCreate + cacheRead), so we
	// must subtract the cached portion before forwarding to the sink.
	// Otherwise the HUD double-counts cached tokens and over-reports billable
	// input usage for every Codex turn.
	freshInputTokens := ev.Usage.InputTokens - ev.Usage.CachedInputTokens
	if freshInputTokens < 0 {
		// Defensive: if Codex ever emits cached > input (shouldn't happen per
		// the OpenAI contract), prefer under-reporting fresh input to negative
		// counts. Log for observability.
		p.logger.Warn("codex usage: cached_input_tokens > input_tokens",
			"input", ev.Usage.InputTokens,
			"cached", ev.Usage.CachedInputTokens)
		freshInputTokens = 0
	}

	p.sink.AddTokens(
		freshInputTokens,
		ev.Usage.OutputTokens,
		0,
		ev.Usage.CachedInputTokens,
	)

	// Codex's SDK does not emit per-turn cost (unlike Claude's `result`
	// event with `total_cost_usd`), so SpawnTelemetry.TotalCostUSD has been
	// 0 for every Codex spawn. Estimate the cost in-process using the model
	// metadata from thread.started when available, and fall back to the
	// canonical Codex model when the SDK omits it.
	estimatedCost := bridge.EstimateCodexCost(
		p.model,
		freshInputTokens,
		ev.Usage.CachedInputTokens,
		ev.Usage.OutputTokens,
	)
	if estimatedCost > 0 {
		p.sink.AddEstimatedCost(estimatedCost)
	}

	// turn.completed always mutates the accumulator (AddTokens), so emit
	// a telemetry delta so clients land on the new token / cost totals.
	p.emitTelemetryDelta()
}

// ---------- item.started ----------

type codexItemStartedEvent struct {
	Item codexItem `json:"item"`
}

type codexItem struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Command  string `json:"command,omitempty"`
	Tool     string `json:"tool,omitempty"`
	Server   string `json:"server,omitempty"`
	Status   string `json:"status,omitempty"`
	ExitCode *int   `json:"exit_code,omitempty"`
	Text     string `json:"text,omitempty"`
	Message  string `json:"message,omitempty"`
	Error    string `json:"error,omitempty"`
	Stderr   string `json:"stderr,omitempty"`
	Changes  []struct {
		Path string `json:"path"`
		Kind string `json:"kind"`
	} `json:"changes,omitempty"`
}

func (p *CodexJSONLParser) handleItemStarted(line []byte) {
	var ev codexItemStartedEvent
	if err := json.Unmarshal(line, &ev); err != nil {
		p.logger.Debug("failed to parse item.started", "error", err)
		return
	}

	item := ev.Item
	changed := false
	switch item.Type {
	case "command_execution":
		// Log every shell command codex starts. Without this, headless
		// debugging of "implement spawn produced no diff" had to rely on
		// post-mortem git state since the spawn pod is reaped immediately
		// on completion. The command text is truncated to keep log lines
		// reasonable while still showing the actual git push / git commit
		// invocations we care about.
		cmd := item.Command
		if len(cmd) > 200 {
			cmd = cmd[:200] + "...(truncated)"
		}
		p.logger.Info("codex item.started: command_execution",
			"item_id", item.ID,
			"command", cmd,
		)
		p.openTools[item.ID] = "Bash"
		p.sink.StartToolCall(item.ID, "Bash", "")
		changed = true
	case "mcp_tool_call":
		name := item.Tool
		if name == "" {
			name = "unknown"
		}
		p.logger.Info("codex item.started: mcp_tool_call",
			"item_id", item.ID, "tool", name, "server", item.Server)
		p.openTools[item.ID] = name
		p.sink.StartToolCall(item.ID, name, item.Server)
		changed = true
	default:
		// Surface unhandled item types so the parser doesn't silently drop
		// events that might explain "codex finished but did nothing".
		p.logger.Info("codex item.started: other",
			"item_id", item.ID, "type", item.Type)
	}
	if changed {
		p.emitTelemetryDelta()
	}
}

// ---------- item.updated ----------

// handleItemUpdated routes item.updated events carrying a terminal status to
// the same completion handlers as item.completed. Codex emits item.updated
// for status transitions; a terminal item.updated may be the only completion
// signal the parser receives for a tool item, and markToolCompleted keeps a
// later item.completed for the same id from double-completing it.
func (p *CodexJSONLParser) handleItemUpdated(line []byte) {
	var ev struct {
		Item codexItem `json:"item"`
	}
	if err := json.Unmarshal(line, &ev); err != nil {
		p.logger.Debug("failed to parse item.updated", "error", err)
		return
	}
	item := ev.Item
	if item.Status != "completed" && item.Status != "failed" {
		return
	}
	switch item.Type {
	case "command_execution":
		p.handleCommandExecution(item)
	case "mcp_tool_call":
		p.handleMCPToolCall(item)
	}
}

// markToolCompleted transitions an item id to completed and reports whether
// this is the first terminal event for it. The accumulator matches a
// completion to the most recent open entry, so a second CompleteToolCall for
// an already-completed id would clobber an unrelated in-flight tool call.
func (p *CodexJSONLParser) markToolCompleted(id string) bool {
	if _, done := p.completedTools[id]; done {
		return false
	}
	p.completedTools[id] = struct{}{}
	delete(p.openTools, id)
	return true
}

// sweepOpenTools closes tool calls whose terminal item event never arrived.
// Codex can start commands in a parallel batch and end the turn without
// emitting item.completed for stragglers: captured stream spawn-d9889e05e2e9
// (2026-07-21) started `make changelog-check` and `go test` alongside a third
// command, completed only the third, then committed, pushed, and exited
// cleanly — and the completion guard failed the spawn ("2 tool call(s) still
// open"), burning three Mills implement attempts on a run whose work landed.
// A clean turn.completed means codex will never complete these items, so they
// are protocol noise, not truncation. A process that dies mid-turn emits no
// turn.completed, leaves its calls open, and still fails closed.
func (p *CodexJSONLParser) sweepOpenTools() {
	for id, name := range p.openTools {
		p.logger.Info("codex tool call left open at turn end; closing",
			"item_id", id, "tool", name)
		p.completedTools[id] = struct{}{}
		p.sink.CompleteToolCall(id, 0, nil, "")
		if p.broadcast != nil {
			p.broadcast("agent.spawn.tool_complete", p.agentID, map[string]any{
				"id":    id,
				"swept": true,
			})
		}
	}
	clear(p.openTools)
}

// ---------- item.completed ----------

func (p *CodexJSONLParser) handleItemCompleted(line []byte) {
	var ev struct {
		Item codexItem `json:"item"`
	}
	if err := json.Unmarshal(line, &ev); err != nil {
		p.logger.Debug("failed to parse item.completed", "error", err)
		return
	}

	item := ev.Item
	switch item.Type {
	case "command_execution":
		p.handleCommandExecution(item)
	case "file_change":
		p.handleFileChange(item)
	case "agent_message":
		p.handleAgentMessage(item)
	case "mcp_tool_call":
		p.handleMCPToolCall(item)
	case "reasoning":
		if item.Text != "" {
			p.sink.AddMessage("assistant", "reasoning", item.Text)
			p.emitTelemetryDelta()
		}
		if p.broadcast != nil {
			p.broadcast("agent.spawn.reasoning", p.agentID, map[string]string{
				"text": item.Text,
			})
		}
	case "error":
		msg := item.Message
		if msg == "" {
			msg = item.Error
		}
		p.sink.AddError("execution", msg)
		p.emitTelemetryDelta()
	case "todo_list":
		if item.Text != "" {
			p.sink.AddMessage("assistant", "todo", item.Text)
			p.emitTelemetryDelta()
		}
		if p.broadcast != nil {
			p.broadcast("agent.spawn.todo", p.agentID, map[string]string{
				"text": item.Text,
			})
		}
	default:
		p.logger.Debug("skipping unknown item type", "item_type", item.Type)
	}
}

func (p *CodexJSONLParser) handleCommandExecution(item codexItem) {
	if !p.markToolCompleted(item.ID) {
		return
	}
	errMsg := ""
	if item.ExitCode != nil && *item.ExitCode != 0 {
		errMsg = item.Stderr
		if errMsg == "" {
			errMsg = item.Error
		}
		p.sink.AddError("tool_failure", errMsg)
	}
	// Always log the result of a shell command. Without this, a failing
	// `git push` (auth, branch protected, etc.) inside the spawn pod is
	// invisible because the pod is reaped on completion. Truncate stderr
	// so a 5MB compile error doesn't bloat the HUD log.
	exit := -1
	if item.ExitCode != nil {
		exit = *item.ExitCode
	}
	stderrTail := errMsg
	if len(stderrTail) > 400 {
		stderrTail = stderrTail[:400] + "...(truncated)"
	}
	cmd := item.Command
	if len(cmd) > 200 {
		cmd = cmd[:200] + "...(truncated)"
	}
	p.logger.Info("codex command_execution complete",
		"item_id", item.ID,
		"command", cmd,
		"exit_code", exit,
		"stderr_tail", stderrTail,
	)
	p.sink.CompleteToolCall(item.ID, 0, item.ExitCode, errMsg)

	if p.broadcast != nil {
		p.broadcast("agent.spawn.tool_complete", p.agentID, map[string]any{
			"id":        item.ID,
			"command":   item.Command,
			"exit_code": item.ExitCode,
		})
	}
	p.emitTelemetryDelta()
}

func (p *CodexJSONLParser) handleFileChange(item codexItem) {
	for _, ch := range item.Changes {
		p.sink.AddFileChange(ch.Path, ch.Kind, 0, 0)
		// One line per actual file modification. Combined with the
		// command_execution log this makes "codex modified the file but
		// failed to commit/push" distinguishable from "codex never
		// touched the file".
		p.logger.Info("codex file_change",
			"item_id", item.ID, "path", ch.Path, "kind", ch.Kind)
	}
	if p.broadcast != nil {
		p.broadcast("agent.spawn.file_change", p.agentID, map[string]any{
			"changes": item.Changes,
		})
	}
	if len(item.Changes) > 0 {
		p.emitTelemetryDelta()
	}
}

func (p *CodexJSONLParser) handleAgentMessage(item codexItem) {
	text := item.Text
	if text == "" {
		text = item.Message
	}
	if text != "" {
		p.sink.SetLastMessage(text)
		p.sink.AddMessage("assistant", "text", text)
	}
	if p.broadcast != nil {
		p.broadcast("agent.spawn.message", p.agentID, map[string]string{
			"text": text,
		})
	}
	if text != "" {
		p.emitTelemetryDelta()
	}
}

func (p *CodexJSONLParser) handleMCPToolCall(item codexItem) {
	// Defensive: ensure a tool call entry exists with the server name even
	// if item.started was not emitted. The Codex SDK is allowed to skip the
	// started event for synchronous mcp_tool_call items, in which case
	// handleItemStarted never ran and the entry that CompleteToolCall is
	// about to update has no ServerName. EnsureToolCall is idempotent — when
	// item.started did fire it is a no-op (the entry name already matches).
	name := item.Tool
	if name == "" {
		name = "unknown"
	}
	if !p.markToolCompleted(item.ID) {
		return
	}
	p.sink.EnsureToolCall(item.ID, name, item.Server)

	errMsg := ""
	if item.Error != "" {
		errMsg = item.Error
		p.sink.AddError("tool_failure", errMsg)
	}
	// Log completion symmetrically with command_execution: without this,
	// diagnosing which tool items never completed (the completion-guard
	// failure class) required guessing which side of an item pair was MCP.
	p.logger.Info("codex mcp_tool_call complete",
		"item_id", item.ID, "tool", name, "server", item.Server, "error", errMsg)
	p.sink.CompleteToolCall(item.ID, 0, nil, errMsg)

	if p.broadcast != nil {
		p.broadcast("agent.spawn.tool_complete", p.agentID, map[string]any{
			"id":          item.ID,
			"tool":        item.Tool,
			"server_name": item.Server,
			"error":       errMsg,
		})
	}
	p.emitTelemetryDelta()
}

// ---------- turn.failed ----------

func (p *CodexJSONLParser) handleTurnFailed(line []byte) {
	var ev struct {
		Message string          `json:"message"`
		Error   json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(line, &ev); err != nil {
		p.logger.Debug("failed to parse turn.failed", "error", err)
		return
	}
	p.turnFailure = codexFailureMessage(ev.Error, ev.Message)
	p.logger.Warn("codex turn.failed", "message", p.turnFailure)
	p.sink.AddError("execution", p.turnFailure)
	p.emitTelemetryDelta()
}

// terminalError reports the specific Codex failure that a clean CLI exit can
// otherwise mask. TurnCount telemetry is incremented at turn.started, so use
// completedTurns here: a turn.failed before any turn.completed means no agent
// turn actually finished and must not advance a Mills stage.
//
// The trigger is "a turn.failed was recorded", NOT "turn.failed was the last
// line". Codex routinely emits trailing lines after it gives up — a fatal
// `error` event, a late item.completed from a straggler in the failed batch —
// and the buffered path replays only a stdout TAIL, so the previous last-event
// anchor silently dropped the real reason and left the open-tool-call guard as
// the only (and opaque) verdict.
func (p *CodexJSONLParser) terminalError() error {
	if p.turnFailure == "" || p.completedTurns != 0 {
		return nil
	}
	return fmt.Errorf("codex turn failed: %s", p.turnFailure)
}

// streamAbortReason returns the last fatal `error` event Codex emitted when no
// turn ever completed. Empty once any turn completes: the run recovered, so the
// error is not the reason it ended.
//
// This is a completion VETO, not only a label for a stream that ended mid-tool.
// Codex emits `error` for failures its agentic loop gave up on; with zero
// completed turns the spawn published no finished work, whether or not a tool
// call happened to be in flight when the stream died. Gating the veto on open
// calls (as the finalizer originally did) let the no-tools-in-flight variant
// complete as a success.
func (p *CodexJSONLParser) streamAbortReason() string {
	if p.completedTurns != 0 {
		return ""
	}
	return p.fatalError
}

func codexFailureMessage(raw json.RawMessage, fallback string) string {
	var structured struct {
		Message string `json:"message"`
	}
	if len(raw) > 0 && json.Unmarshal(raw, &structured) == nil && strings.TrimSpace(structured.Message) != "" {
		return structured.Message
	}
	var text string
	if len(raw) > 0 && json.Unmarshal(raw, &text) == nil && strings.TrimSpace(text) != "" {
		return text
	}
	if strings.TrimSpace(fallback) != "" {
		return fallback
	}
	return "turn failed"
}

// ---------- error ----------

func (p *CodexJSONLParser) handleError(line []byte) {
	var ev struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(line, &ev); err != nil {
		p.logger.Debug("failed to parse error event", "error", err)
		return
	}
	// Codex emits `{"type":"error",…}` for hard failures the agentic loop
	// gave up on. Log at error so operators can grep `level=ERROR` and
	// see exactly why a spawn completed without producing work.
	p.logger.Error("codex error event", "message", ev.Message)
	// Retained so a stream that ends mid-batch can report WHY (see
	// streamAbortReason). Recorded, never itself a completion veto: an error
	// event that the agentic loop recovered from is followed by a completed
	// turn, and streamAbortReason ignores it in that case.
	if msg := strings.TrimSpace(ev.Message); msg != "" {
		p.fatalError = msg
	}
	p.sink.AddError("fatal", ev.Message)
	p.emitTelemetryDelta()
}
