package vendorsessions

import (
	"encoding/json"
	"strings"
	"time"
)

// lineDoc is the superset of both vendors' transcript line shapes needed to
// pull human-readable text out of a record. Claude Code lines carry
// message.role/content (content is a string or an array of typed blocks);
// Codex rollout lines wrap everything in payload with per-type fields.
type lineDoc struct {
	Type      string          `json:"type"`
	Timestamp string          `json:"timestamp"`
	Summary   string          `json:"summary"`
	Content   json.RawMessage `json:"content"` // claude queue-operation: string
	Message   struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
	Payload struct {
		Type    string          `json:"type"`
		Role    string          `json:"role"`
		Message string          `json:"message"`
		Text    string          `json:"text"`
		Content json.RawMessage `json:"content"`
	} `json:"payload"`
}

// contentBlock is one element of a structured content array. Claude uses
// {type:"text",text}, tool blocks, and nested tool_result content; Codex
// response items use {type:"input_text"/"output_text",text}.
type contentBlock struct {
	Type    string          `json:"type"`
	Text    string          `json:"text"`
	Content json.RawMessage `json:"content"`
}

// extractedRecord is the human-readable view of one transcript line.
// Role is a short lowercase hint ("user", "assistant", "summary",
// "thinking"); Timestamp is the record's RFC3339 timestamp when it carried
// a parseable one.
type extractedRecord struct {
	Role      string
	Text      string
	Timestamp string
}

// extractRecord pulls the conversational text out of one transcript line of
// either vendor, whitespace-normalized. ok=false means the record has no
// human-readable content (tool traffic, file snapshots, token counts,
// settings churn) and should be skipped by text-oriented consumers (search
// matching, federated tails, titles).
func extractRecord(line []byte) (extractedRecord, bool) {
	rec, ok := extractRawRecord(line)
	if !ok {
		return extractedRecord{}, false
	}
	rec.Text = normalizeText(rec.Text)
	if rec.Text == "" {
		return extractedRecord{}, false
	}
	return rec, true
}

// extractRawRecord is extractRecord before whitespace normalization — the
// title path needs the original line structure so a multi-paragraph prompt
// can be titled by its opening line alone.
func extractRawRecord(line []byte) (extractedRecord, bool) {
	var doc lineDoc
	if json.Unmarshal(line, &doc) != nil {
		return extractedRecord{}, false
	}
	ts := ""
	if doc.Timestamp != "" {
		if t, err := time.Parse(time.RFC3339Nano, doc.Timestamp); err == nil {
			ts = t.Format(time.RFC3339)
		}
	}

	// Claude: standalone summary records title the conversation.
	if doc.Type == "summary" && doc.Summary != "" {
		return extractedRecord{Role: "summary", Text: doc.Summary, Timestamp: ts}, true
	}

	// Claude: queued user prompts ride queue-operation records; only the
	// enqueue carries content (dequeue is bookkeeping).
	if doc.Type == "queue-operation" {
		if s := rawString(doc.Content); s != "" {
			return extractedRecord{Role: "user", Text: s, Timestamp: ts}, true
		}
		return extractedRecord{}, false
	}

	// Claude: user/assistant turns under message.{role,content}.
	if doc.Message.Role != "" {
		if s := contentText(doc.Message.Content); s != "" {
			return extractedRecord{Role: strings.ToLower(doc.Message.Role), Text: s, Timestamp: ts}, true
		}
		return extractedRecord{}, false
	}

	// Codex: event_msg / response_item payloads.
	switch doc.Payload.Type {
	case "user_message":
		if doc.Payload.Message != "" {
			return extractedRecord{Role: "user", Text: doc.Payload.Message, Timestamp: ts}, true
		}
	case "agent_message":
		if doc.Payload.Message != "" {
			return extractedRecord{Role: "assistant", Text: doc.Payload.Message, Timestamp: ts}, true
		}
	case "agent_reasoning":
		if doc.Payload.Text != "" {
			return extractedRecord{Role: "thinking", Text: doc.Payload.Text, Timestamp: ts}, true
		}
	case "message":
		if s := contentText(doc.Payload.Content); s != "" {
			role := strings.ToLower(doc.Payload.Role)
			if role == "" {
				role = "assistant"
			}
			return extractedRecord{Role: role, Text: s, Timestamp: ts}, true
		}
	}
	return extractedRecord{}, false
}

// contentText renders a message content value — a plain string or an array
// of typed blocks — as raw text (whitespace preserved; extractText's wrapper
// normalizes). Tool blocks contribute nothing; a tool-only record therefore
// extracts empty and is skipped.
func contentText(raw json.RawMessage) string {
	if s := rawString(raw); s != "" {
		return s
	}
	var blocks []contentBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		switch b.Type {
		case "text", "input_text", "output_text":
			if b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
	}
	return strings.Join(parts, "\n")
}

// rawString unmarshals raw as a JSON string, returning "" for any other shape.
func rawString(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return ""
}

// normalizeText collapses all whitespace runs to single spaces.
func normalizeText(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// titleFromText turns raw extracted user text into a session title: the
// first non-empty line only (a multi-paragraph prompt titles itself by its
// opening line), capped at maxTitleLen with an ellipsis on truncation.
const maxTitleLen = 140

func titleFromText(s string) string {
	title := ""
	for _, line := range strings.Split(s, "\n") {
		if t := normalizeText(line); t != "" {
			title = t
			break
		}
	}
	if title == "" {
		return ""
	}
	if len(title) > maxTitleLen {
		cut := maxTitleLen
		for cut > 0 && !isASCIIBoundary(title[cut]) {
			cut--
		}
		title = strings.TrimRight(title[:cut], " ") + "…"
	}
	return title
}

// titleWorthy filters out non-conversational user text that would make a
// meaningless title: command wrappers and injected reminders start with an
// XML-ish tag (<command-name>, <system-reminder>, <local-command…), and
// Codex injects workspace instructions as a user_message opening
// "# AGENTS.md instructions for <path>".
func titleWorthy(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" || strings.HasPrefix(s, "<") {
		return false
	}
	return !strings.HasPrefix(s, "# AGENTS.md instructions")
}
