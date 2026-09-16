package clients

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/crb2nu/loom/pkg/llmusage"
	"github.com/crb2nu/loom/pkg/mills/gates"
	"github.com/crb2nu/loom/pkg/openairesponses"
)

// OpenAIRubricJudge uses the same prompt and parser as the other rubric judges.
// The configured Responses client emits usage through llmusage.
type OpenAIRubricJudge struct {
	Client     responsesClient
	Model      string
	Breaker    *VendorBreaker
	RubricBody func(string) string
}

func (j *OpenAIRubricJudge) Judge(ctx context.Context, rubric string, in gates.StageInput) (gates.RubricVerdict, error) {
	if j == nil || j.Client == nil {
		return gates.RubricVerdict{}, errors.New("openai rubric judge: client not configured")
	}
	b := vendorBreaker(j.Breaker)
	if err := b.check("openai"); err != nil {
		return gates.RubricVerdict{}, err
	}
	body := j.RubricBody
	if body == nil {
		body = defaultRubricBody
	}
	resp, err := j.Client.Create(llmusage.WithComponent(ctx, ComponentJudge), openairesponses.TurnRequest{
		Model: j.Model, Input: composePrompt(body(rubric), in),
		Context:        openairesponses.ContextStrategy{Mode: openairesponses.ContextModeStateless},
		PromptCacheKey: "loom-mills-rubric-judge",
	})
	if err != nil {
		return gates.RubricVerdict{}, fmt.Errorf("openai rubric judge: %w", b.failure("openai", err))
	}
	b.Reset("openai")
	// TurnResponse normalizes text but retains the original payload. Inspect
	// typed refusal items instead of guessing from the model's prose.
	var payload struct {
		Output []struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
			} `json:"content"`
		} `json:"output"`
	}
	if json.Unmarshal(resp.RawPayload, &payload) == nil {
		for _, item := range payload.Output {
			refused := item.Type == "refusal"
			for _, c := range item.Content {
				refused = refused || c.Type == "refusal"
			}
			if refused {
				return gates.RubricVerdict{}, &VendorError{Vendor: "openai", Kind: VendorRefusal, Err: errors.New("openai rubric judge: request refused")}
			}
		}
	}
	score, reasons, err := parseRubricEnvelope(resp.OutputText)
	if err != nil {
		return gates.RubricVerdict{Model: j.Model}, fmt.Errorf("openai rubric judge: parse: %w", err)
	}
	return gates.RubricVerdict{Score: score, Reasons: reasons, Model: j.Model}, nil
}
