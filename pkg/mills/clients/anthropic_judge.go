package clients

import (
	"context"
	"errors"
	"fmt"

	"github.com/crb2nu/loom/pkg/llmusage"
	"github.com/crb2nu/loom/pkg/mills/gates"
)

// AnthropicRubricJudge satisfies gates.RubricJudge against the Anthropic
// Messages API. It exists as the DISSENT TIEBREAKER for the LLM-judged
// gates: when the local FlexInfer judge fails a gate whose deterministic
// tests stage passed, the gate asks this judge for a second opinion from a
// different model family (escalations #309/#310 — the local judge
// deterministically failed a correct dynamic-WHERE SQL idiom across three
// runs and nine review attempts; same-model retries and prompt clauses
// cannot cure a model fixation, diversity can).
//
// It composes the SAME rubric prompt as the FlexInfer judge (composePrompt +
// the canonical rubric bodies) and parses the same JSON envelope, so the two
// verdicts are comparable score-for-score.
type AnthropicRubricJudge struct {
	Client anthropicMessenger
	Model  string
	// MaxTokens caps the completion. Thinking is disabled on judge calls
	// (see Judge), so the full budget goes to the score envelope, which is
	// tiny. Default 2048 keeps generous headroom for verbose reasons lists.
	MaxTokens int64
	// RubricBody maps rubric name → prompt body. Nil falls back to the
	// canonical bodies in pkg/mills/gates (same as the FlexInfer judge).
	RubricBody func(rubric string) string
}

// Judge implements gates.RubricJudge.
func (j *AnthropicRubricJudge) Judge(ctx context.Context, rubric string, in gates.StageInput) (gates.RubricVerdict, error) {
	if j == nil || j.Client == nil {
		return gates.RubricVerdict{}, errors.New("anthropic rubric judge: client not configured")
	}
	// Attribute this call's token accounting to the judge rather than to the
	// shared Anthropic client — same tagging as the FlexInfer judge.
	// Instrumentation only; nothing reads it back.
	ctx = llmusage.WithComponent(ctx, ComponentJudge)
	body := j.RubricBody
	if body == nil {
		body = defaultRubricBody
	}
	maxTokens := j.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 2048
	}
	// DisableThinking: adaptive thinking shares MaxTokens with the text body,
	// and on complex diffs claude-sonnet-5 thought through the entire budget
	// and returned an empty envelope (live 2026-08-09: "tiebreaker unavailable
	// ... raw=\"\""), leaving near-miss primary fails unresolved. A structured
	// score needs no extended thinking; the local judge scores without it too.
	res, err := j.Client.CreateMessage(ctx, anthropicMessageRequest{
		Model:           j.Model,
		Prompt:          composePrompt(body(rubric), in),
		MaxTokens:       maxTokens,
		DisableThinking: true,
	})
	if err != nil {
		return gates.RubricVerdict{}, fmt.Errorf("anthropic rubric judge: %w", err)
	}
	if res.Refusal {
		return gates.RubricVerdict{}, errors.New("anthropic rubric judge: request refused")
	}
	score, reasons, perr := parseRubricEnvelope(res.Text)
	if perr != nil {
		return gates.RubricVerdict{Model: j.Model}, fmt.Errorf("anthropic rubric judge: parse: %w; raw=%q", perr, truncateForLog(res.Text, 400))
	}
	return gates.RubricVerdict{Score: score, Reasons: reasons, Model: j.Model}, nil
}

// truncateForLog bounds raw model output embedded in an error message.
func truncateForLog(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
