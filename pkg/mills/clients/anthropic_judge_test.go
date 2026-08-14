package clients

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/crb2nu/loom/pkg/mills/gates"
)

func TestAnthropicRubricJudge_ParsesEnvelope(t *testing.T) {
	fake := &fakeAnthropicMessenger{res: anthropicMessageResult{
		Text: "```json\n{\"score\": 0.85, \"reasons\": [\"looks correct\"]}\n```",
	}}
	j := &AnthropicRubricJudge{Client: fake, Model: "claude-sonnet-5"}
	v, err := j.Judge(context.Background(), gates.PRSelfReviewRubricName, gates.StageInput{
		DiffPatch: []byte("diff --git a/a.go b/a.go\n+x\n"),
	})
	if err != nil {
		t.Fatalf("judge: %v", err)
	}
	if v.Score != 0.85 || v.Model != "claude-sonnet-5" {
		t.Errorf("verdict = %+v, want score 0.85 model claude-sonnet-5", v)
	}
	// The tiebreaker must grade the SAME composed prompt shape as the
	// primary judge so scores are comparable: rubric body + grounding +
	// the diff.
	if !strings.Contains(fake.gotPrompt, "diff --git a/a.go") {
		t.Error("composed prompt missing the diff")
	}
	if !strings.Contains(fake.gotPrompt, "Score ONLY defects that exist in the diff AS WRITTEN") {
		t.Error("composed prompt missing the shared grounding instructions")
	}
	if fake.gotModel != "claude-sonnet-5" {
		t.Errorf("request model = %q", fake.gotModel)
	}
	// Thinking must be disabled: adaptive thinking shares MaxTokens with the
	// envelope and can consume all of it on a complex diff, returning an
	// empty body (live 2026-08-09 "tiebreaker unavailable; raw=\"\"").
	if !fake.gotDisabled {
		t.Error("judge request must set DisableThinking")
	}
}

func TestAnthropicRubricJudge_ErrorAndRefusal(t *testing.T) {
	j := &AnthropicRubricJudge{Client: &fakeAnthropicMessenger{err: errors.New("529")}, Model: "claude-sonnet-5"}
	if _, err := j.Judge(context.Background(), gates.PRSelfReviewRubricName, gates.StageInput{}); err == nil {
		t.Fatal("transport error must propagate")
	}
	j = &AnthropicRubricJudge{Client: &fakeAnthropicMessenger{res: anthropicMessageResult{Refusal: true}}, Model: "claude-sonnet-5"}
	if _, err := j.Judge(context.Background(), gates.PRSelfReviewRubricName, gates.StageInput{}); err == nil {
		t.Fatal("refusal must be an error, not a silent verdict")
	}
	j = &AnthropicRubricJudge{Client: &fakeAnthropicMessenger{res: anthropicMessageResult{Text: "no json here"}}, Model: "claude-sonnet-5"}
	if _, err := j.Judge(context.Background(), gates.PRSelfReviewRubricName, gates.StageInput{}); err == nil {
		t.Fatal("unparseable envelope must be an error")
	}
}
