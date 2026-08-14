package hud

import (
	"encoding/json"
	"log/slog"
	"strings"
	"time"
)

// GeminiJSONLParser parses Gemini CLI's --output-format stream-json JSONL.
// Each line is a JSON object with a "type" field: init, message, tool_use,
// tool_result, error, result.
type GeminiJSONLParser struct {
	sink       SpawnEventSink
	broadcast  SpawnEventBroadcaster
	agentID    string
	spawnID    string
	logger     *slog.Logger
	toolStarts map[string]time.Time // tool_id -> start timestamp
	// fatalError is the last error-severity `error` event message. Kept so a
	// stream that dies without a terminal `result` can name its cause instead
	// of reporting only how many tool calls were left open (see
	// streamAbortReason).
	fatalError string
	// sawResult records that the CLI emitted its terminal `result` event, i.e.
	// it finished its stream. Any earlier error is then something the run
	// carried on past, not the reason it ended.
	sawResult bool
}

// NewGeminiJSONLParser creates a parser for Gemini CLI stream-json output.
func NewGeminiJSONLParser(sink SpawnEventSink, agentID, spawnID string, broadcast SpawnEventBroadcaster, logger *slog.Logger) *GeminiJSONLParser {
	if logger == nil {
		logger = slog.Default()
	}
	return &GeminiJSONLParser{
		sink:       sink,
		broadcast:  broadcast,
		agentID:    agentID,
		spawnID:    spawnID,
		logger:     logger.With("component", "gemini-parser", "agent_id", agentID),
		toolStarts: make(map[string]time.Time),
	}
}

func (p *GeminiJSONLParser) emitTelemetryDelta() {
	if p.broadcast == nil {
		return
	}
	delta := p.sink.TelemetryDeltaSnapshot(p.spawnID, p.agentID)
	p.broadcast(SpawnTelemetryDeltaEvent, p.agentID, delta)
}

// HandleLine processes a single JSONL line from Gemini stdout.
func (p *GeminiJSONLParser) HandleLine(line []byte) {
	if len(line) == 0 {
		return
	}

	var envelope struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(line, &envelope); err != nil {
		return
	}

	switch envelope.Type {
	case "init":
		p.handleInit(line)
	case "message":
		p.handleMessage(line)
	case "tool_use":
		p.handleToolUse(line)
	case "tool_result":
		p.handleToolResult(line)
	case "error":
		p.handleError(line)
	case "result":
		p.handleResult(line)
	default:
		p.logger.Debug("unknown gemini event type", "type", envelope.Type)
	}
}

func (p *GeminiJSONLParser) handleInit(line []byte) {
	var ev struct {
		SessionID string `json:"session_id"`
		Model     string `json:"model"`
	}
	if err := json.Unmarshal(line, &ev); err != nil {
		return
	}
	if ev.SessionID != "" {
		p.sink.SetExternalSessionID(ev.SessionID)
	}
	p.emitTelemetryDelta()
}

func (p *GeminiJSONLParser) handleMessage(line []byte) {
	var ev struct {
		Role    string `json:"role"`
		Content string `json:"content"`
		Delta   bool   `json:"delta"`
	}
	if err := json.Unmarshal(line, &ev); err != nil {
		return
	}

	// Only count complete messages (non-delta) from the assistant as turns.
	if ev.Role == "assistant" && !ev.Delta {
		p.sink.IncrementTurns()
		if ev.Content != "" {
			p.sink.SetLastMessage(ev.Content)
			p.sink.AddMessage("assistant", "text", ev.Content)
			if p.broadcast != nil {
				p.broadcast("agent.spawn.message", p.agentID, map[string]string{
					"text":     ev.Content,
					"spawn_id": p.spawnID,
				})
			}
		}
	}
	p.emitTelemetryDelta()
}

func (p *GeminiJSONLParser) handleToolUse(line []byte) {
	var ev struct {
		ToolName string `json:"tool_name"`
		ToolID   string `json:"tool_id"`
	}
	if err := json.Unmarshal(line, &ev); err != nil {
		return
	}

	p.toolStarts[ev.ToolID] = time.Now()
	p.sink.StartToolCall(ev.ToolID, ev.ToolName, "")

	if p.broadcast != nil {
		p.broadcast("agent.spawn.tool_start", p.agentID, map[string]string{
			"id":       ev.ToolID,
			"name":     ev.ToolName,
			"spawn_id": p.spawnID,
		})
	}
	p.emitTelemetryDelta()
}

func (p *GeminiJSONLParser) handleToolResult(line []byte) {
	var ev struct {
		ToolID string `json:"tool_id"`
		Status string `json:"status"`
		Output string `json:"output"`
		Error  *struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(line, &ev); err != nil {
		return
	}

	durationMs := 0
	if start, ok := p.toolStarts[ev.ToolID]; ok {
		durationMs = int(time.Since(start).Milliseconds())
		delete(p.toolStarts, ev.ToolID)
	}

	errMsg := ""
	if ev.Error != nil {
		errMsg = ev.Error.Message
	}
	p.sink.CompleteToolCall(ev.ToolID, durationMs, nil, errMsg)

	if p.broadcast != nil {
		p.broadcast("agent.spawn.tool_complete", p.agentID, map[string]any{
			"id":          ev.ToolID,
			"duration_ms": durationMs,
			"is_error":    ev.Status == "error",
			"spawn_id":    p.spawnID,
		})
	}
	p.emitTelemetryDelta()
}

func (p *GeminiJSONLParser) handleError(line []byte) {
	var ev struct {
		Severity string `json:"severity"`
		Message  string `json:"message"`
	}
	if err := json.Unmarshal(line, &ev); err != nil {
		return
	}

	// Retained so a stream that dies before its terminal `result` can report
	// WHY (see streamAbortReason). Only error-grade severities count: Gemini
	// also uses this event for warnings the run continues past, and an
	// abort reason is a completion veto, so it must not fire on those.
	if geminiFatalSeverity(ev.Severity) {
		if msg := strings.TrimSpace(ev.Message); msg != "" {
			p.fatalError = msg
		}
	}
	p.sink.AddError(ev.Severity, ev.Message)
	p.emitTelemetryDelta()
}

// geminiFatalSeverity reports whether an `error` event's severity names a
// failure the run did not continue past. An absent severity counts: the event
// type is already "error", so an unlabeled one is taken at its word.
func geminiFatalSeverity(severity string) bool {
	switch strings.ToLower(strings.TrimSpace(severity)) {
	case "", "error", "fatal", "critical":
		return true
	default:
		return false
	}
}

// streamAbortReason returns the last error-severity `error` event when the CLI
// never emitted its terminal `result`, so a Gemini stream killed mid-run names
// its cause instead of reporting only how many tool calls were left open.
// Empty once a result arrives: the CLI finished its stream, so any earlier
// error is something the run carried on past.
//
// Deliberately requires a RECORDED error, not merely a missing result: this is
// a completion veto (see spawnCompletionError), and a stream that simply ends
// without a diagnostic still falls through to the older open-tool-call rule.
func (p *GeminiJSONLParser) streamAbortReason() string {
	if p.sawResult {
		return ""
	}
	return p.fatalError
}

// geminiStreamStats matches the Gemini CLI stream-json result.stats shape.
type geminiStreamStats struct {
	TotalTokens  int                               `json:"total_tokens"`
	InputTokens  int                               `json:"input_tokens"`
	OutputTokens int                               `json:"output_tokens"`
	Cached       int                               `json:"cached"`
	Input        int                               `json:"input"`
	DurationMs   int                               `json:"duration_ms"`
	ToolCalls    int                               `json:"tool_calls"`
	Models       map[string]geminiModelStreamStats `json:"models"`
}

type geminiModelStreamStats struct {
	TotalTokens  int `json:"total_tokens"`
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	Cached       int `json:"cached"`
	Input        int `json:"input"`
}

func (p *GeminiJSONLParser) handleResult(line []byte) {
	var ev struct {
		Status string `json:"status"`
		Error  *struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
		Stats *geminiStreamStats `json:"stats"`
	}
	if err := json.Unmarshal(line, &ev); err != nil {
		return
	}
	p.sawResult = true

	stopReason := ev.Status
	if ev.Error != nil {
		p.sink.AddError(ev.Error.Type, ev.Error.Message)
		if stopReason == "" {
			stopReason = "error"
		}
	}

	if ev.Stats != nil {
		// Add token counts (net input = input - cached to avoid double-count).
		netInput := ev.Stats.InputTokens - ev.Stats.Cached
		if netInput < 0 {
			netInput = 0
		}
		p.sink.AddTokens(netInput, ev.Stats.OutputTokens, 0, ev.Stats.Cached)

		// Gemini CLI doesn't report cost; estimate is not possible without
		// per-model pricing. Mark as estimated with zero cost for now.
		p.sink.SetResult(0, 0, stopReason)
	} else {
		p.sink.SetResult(0, 0, stopReason)
	}

	if p.broadcast != nil {
		durMs := 0
		turns := 0
		if ev.Stats != nil {
			durMs = ev.Stats.DurationMs
		}
		p.broadcast("agent.spawn.result", p.agentID, map[string]any{
			"stop_reason": stopReason,
			"cost_usd":    0,
			"turns":       turns,
			"duration_ms": durMs,
			"spawn_id":    p.spawnID,
		})
	}
	p.emitTelemetryDelta()
}
